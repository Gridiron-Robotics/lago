package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// LagoWriter is the deliberately narrow WRITE path to Lago.
//
// It is a separate type from LagoClient on purpose. LagoClient's single request
// method hard-codes GET, which is what makes every read tool read-only by
// construction — that guarantee is worth keeping, so writes do not go through it.
//
// The trade being made here is explicit: a read-only tool surface means the agent
// can report on billing but cannot run it, which is no autonomy at all for the
// module that bills subscriptions and metered usage. So writes are allowed, but
// only these:
//
//   - emit a usage event (the meter reading that everything else is derived from)
//   - create a subscription (put a customer on a plan)
//   - terminate a subscription (stop billing them)
//   - top up a prepaid wallet (credit a balance)
//
// Everything else — deleting customers, editing plans, voiding invoices — stays
// unreachable. Not because those are unimportant, but because an agent does not
// need them to run the billing lifecycle, and each one is irreversible in a way
// a bad tool call should not be able to reach.
//
// Every write is enumerated in allowedWrites below and checked at request time,
// so a future tool cannot quietly acquire a new verb+path by passing a string.
type LagoWriter struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// writeRoute is one permitted (method, path-prefix) pair.
type writeRoute struct {
	method string
	prefix string
}

// allowedWrites is the complete set of mutations this module can perform. A
// request that does not match one is refused BEFORE it leaves the process.
var allowedWrites = []writeRoute{
	{http.MethodPost, "/api/v1/events"},
	{http.MethodPost, "/api/v1/subscriptions"},
	{http.MethodDelete, "/api/v1/subscriptions/"},
	{http.MethodPost, "/api/v1/wallet_transactions"},
}

// NewLagoWriter builds the write client. Returns nil when unconfigured, so the
// caller registers no write tools rather than registering tools that fail.
func NewLagoWriter(baseURL, apiKey string, hc *http.Client) *LagoWriter {
	if strings.TrimSpace(baseURL) == "" || strings.TrimSpace(apiKey) == "" {
		return nil
	}
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	return &LagoWriter{baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, http: hc}
}

// permitted reports whether (method, path) is in the enumerated write set.
func permitted(method, path string) bool {
	for _, r := range allowedWrites {
		if r.method != method {
			continue
		}
		if strings.HasSuffix(r.prefix, "/") {
			if strings.HasPrefix(path, r.prefix) && len(path) > len(r.prefix) {
				return true
			}
			continue
		}
		if path == r.prefix {
			return true
		}
	}
	return false
}

// do performs one enumerated mutation. Every write in this package goes through
// here, which is where the allow-list is enforced.
func (w *LagoWriter) do(ctx context.Context, method, path string, payload any) (json.RawMessage, error) {
	if w == nil || w.baseURL == "" || w.apiKey == "" {
		return nil, fmt.Errorf("lago writer not configured (set LAGO_API_URL and LAGO_API_KEY)")
	}
	if !permitted(method, path) {
		// Reached only by a coding mistake, and it must be loud: the whole point
		// of the allow-list is that a new mutation cannot appear by accident.
		return nil, fmt.Errorf("refused: %s %s is not an allowed Lago mutation", method, path)
	}

	var body io.Reader
	if payload != nil {
		buf, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, w.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+w.apiKey)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := w.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("lago request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("lago %s %s -> HTTP %d: %s", method, path, resp.StatusCode, truncate(string(raw), 300))
	}
	return json.RawMessage(raw), nil
}

// EmitUsageEvent records a metered usage event.
//
// transactionID is Lago's own idempotency key for events: replaying the same
// transaction_id does not double-count. That matters more here than anywhere else
// in the module — a retried usage event is a customer billed twice for one unit.
func (w *LagoWriter) EmitUsageEvent(
	ctx context.Context,
	transactionID, externalSubscriptionID, code string,
	properties map[string]any,
) (json.RawMessage, error) {
	event := map[string]any{
		"transaction_id":           transactionID,
		"external_subscription_id": externalSubscriptionID,
		"code":                     code,
	}
	if len(properties) > 0 {
		event["properties"] = properties
	}
	return w.do(ctx, http.MethodPost, "/api/v1/events", map[string]any{"event": event})
}

// CreateSubscription puts a customer on a plan.
func (w *LagoWriter) CreateSubscription(
	ctx context.Context,
	externalCustomerID, planCode, externalSubscriptionID, billingTime string,
) (json.RawMessage, error) {
	sub := map[string]any{
		"external_customer_id": externalCustomerID,
		"plan_code":            planCode,
		"external_id":          externalSubscriptionID,
	}
	if billingTime != "" {
		sub["billing_time"] = billingTime
	}
	return w.do(ctx, http.MethodPost, "/api/v1/subscriptions", map[string]any{"subscription": sub})
}

// TerminateSubscription stops billing a subscription.
func (w *LagoWriter) TerminateSubscription(ctx context.Context, externalSubscriptionID string) (json.RawMessage, error) {
	if strings.TrimSpace(externalSubscriptionID) == "" {
		return nil, fmt.Errorf("external_subscription_id is required")
	}
	return w.do(ctx, http.MethodDelete, "/api/v1/subscriptions/"+url.PathEscape(externalSubscriptionID), nil)
}

// TopUpWallet credits a prepaid wallet.
func (w *LagoWriter) TopUpWallet(ctx context.Context, walletID, paidCredits string) (json.RawMessage, error) {
	return w.do(ctx, http.MethodPost, "/api/v1/wallet_transactions", map[string]any{
		"wallet_transaction": map[string]any{
			"wallet_id":    walletID,
			"paid_credits": paidCredits,
		},
	})
}
