package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The write surface. This is money: a usage event decides what a customer is
// invoiced, a subscription starts or stops charging them, and a wallet top-up
// credits a balance they can spend. Every test below is a way one of those goes
// wrong while still looking like it worked.

// captured is one request the fake Lago received.
type captured struct {
	method string
	path   string // r.URL.Path — DECODED, so %2F appears as /
	wire   string // r.URL.RequestURI() — what actually went on the wire
	body   map[string]any
	auth   string
}

func fakeLago(t *testing.T, status int, reply string) (*LagoWriter, *[]captured) {
	t.Helper()
	var seen []captured
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := captured{
			method: r.Method,
			path:   r.URL.Path,
			wire:   r.URL.RequestURI(),
			auth:   r.Header.Get("Authorization"),
		}
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&c.body)
		}
		seen = append(seen, c)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	return NewLagoWriter(srv.URL, "test-key", srv.Client()), &seen
}

// --------------------------------------------------------------------------- //
// The allow-list — what keeps "we added writes" from meaning "anything"
// --------------------------------------------------------------------------- //
func TestOnlyEnumeratedMutationsArePermitted(t *testing.T) {
	allowed := []struct{ method, path string }{
		{http.MethodPost, "/api/v1/events"},
		{http.MethodPost, "/api/v1/subscriptions"},
		{http.MethodDelete, "/api/v1/subscriptions/sub-1"},
		{http.MethodPost, "/api/v1/wallet_transactions"},
	}
	for _, a := range allowed {
		if !permitted(a.method, a.path) {
			t.Errorf("expected %s %s to be permitted", a.method, a.path)
		}
	}

	// Each of these is a real Lago endpoint an agent must not reach: deleting a
	// customer, rewriting a plan's prices, voiding an invoice.
	refused := []struct{ method, path string }{
		{http.MethodDelete, "/api/v1/customers/acme"},
		{http.MethodPut, "/api/v1/plans/standard"},
		{http.MethodPost, "/api/v1/invoices/inv-1/void"},
		{http.MethodPost, "/api/v1/customers"},
		{http.MethodDelete, "/api/v1/wallet_transactions"},
		// The bare collection path must not satisfy the "/subscriptions/" prefix:
		// a DELETE with an empty id could otherwise read as "delete them all".
		{http.MethodDelete, "/api/v1/subscriptions/"},
		{http.MethodDelete, "/api/v1/subscriptions"},
		// A path that merely starts with an allowed one is not the allowed one.
		{http.MethodPost, "/api/v1/events/replay-all"},
	}
	for _, r := range refused {
		if permitted(r.method, r.path) {
			t.Errorf("%s %s must NOT be permitted", r.method, r.path)
		}
	}
}

func TestAnUnlistedMutationNeverLeavesTheProcess(t *testing.T) {
	w, seen := fakeLago(t, 200, `{}`)
	_, err := w.do(context.Background(), http.MethodDelete, "/api/v1/customers/acme", nil)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "not an allowed Lago mutation") {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(*seen) != 0 {
		t.Fatalf("a refused mutation reached Lago: %+v", *seen)
	}
}

// --------------------------------------------------------------------------- //
// Reads stay read-only
// --------------------------------------------------------------------------- //
func TestReadClientStillIssuesOnlyGETs(t *testing.T) {
	// The invariant the MCP gate exists for. Adding a write path must not have
	// loosened the read path — they are separate types precisely so this holds.
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := NewLagoClient(srv.URL, "k", srv.Client())
	ctx := context.Background()
	_, _ = c.GetCustomer(ctx, "acme")
	_, _ = c.ListInvoices(ctx, "acme")
	_, _ = c.GetInvoice(ctx, "inv-1")
	_, _ = c.ListSubscriptions(ctx, "acme")
	_, _ = c.ListWallets(ctx, "acme")
	_, _ = c.CurrentUsage(ctx, "acme", "sub-1")

	if len(methods) == 0 {
		t.Fatal("no requests were made")
	}
	for _, m := range methods {
		if m != http.MethodGet {
			t.Fatalf("read client issued a %s — the read path must be GET-only", m)
		}
	}
}

// --------------------------------------------------------------------------- //
// Usage events — the double-billing surface
// --------------------------------------------------------------------------- //
func TestUsageEventSendsTheTransactionIDLagoDeduplicatesOn(t *testing.T) {
	w, seen := fakeLago(t, 200, `{"event":{"lago_id":"e1"}}`)
	_, err := w.EmitUsageEvent(context.Background(), "txn-1", "sub-1", "api_calls",
		map[string]any{"value": 42})
	if err != nil {
		t.Fatal(err)
	}
	if len(*seen) != 1 {
		t.Fatalf("expected 1 request, got %d", len(*seen))
	}
	got := (*seen)[0]
	if got.method != http.MethodPost || got.path != "/api/v1/events" {
		t.Fatalf("wrong route: %s %s", got.method, got.path)
	}
	event, _ := got.body["event"].(map[string]any)
	if event["transaction_id"] != "txn-1" {
		t.Errorf("transaction_id not forwarded: %v", event["transaction_id"])
	}
	if event["external_subscription_id"] != "sub-1" || event["code"] != "api_calls" {
		t.Errorf("subscription/code not forwarded: %+v", event)
	}
	props, _ := event["properties"].(map[string]any)
	if props["value"] != float64(42) {
		t.Errorf("properties not forwarded: %+v", event["properties"])
	}
}

func TestUsageEventOmitsEmptyProperties(t *testing.T) {
	// A metric with no properties must not send `"properties": {}` — some
	// billable-metric types reject an empty object.
	w, seen := fakeLago(t, 200, `{}`)
	if _, err := w.EmitUsageEvent(context.Background(), "t", "s", "c", nil); err != nil {
		t.Fatal(err)
	}
	event, _ := (*seen)[0].body["event"].(map[string]any)
	if _, present := event["properties"]; present {
		t.Errorf("empty properties should be omitted, got %+v", event)
	}
}

func TestUsageToolRequiresTheIdempotencyKey(t *testing.T) {
	// Without transaction_id there is no dedupe, so any retry double-bills. It is
	// required at the tool boundary, before a request is built.
	w, seen := fakeLago(t, 200, `{}`)
	tool := toolNamed(t, WriteTools(w), "lago_emit_usage_event")
	_, err := tool.Handler(context.Background(), map[string]any{
		"external_subscription_id": "sub-1", "code": "api_calls",
	})
	if err == nil || !strings.Contains(err.Error(), "transaction_id") {
		t.Fatalf("expected a missing transaction_id error, got %v", err)
	}
	if len(*seen) != 0 {
		t.Fatal("a request was sent without a transaction_id")
	}
}

func TestUsageToolRejectsNonObjectProperties(t *testing.T) {
	w, _ := fakeLago(t, 200, `{}`)
	tool := toolNamed(t, WriteTools(w), "lago_emit_usage_event")
	_, err := tool.Handler(context.Background(), map[string]any{
		"transaction_id": "t", "external_subscription_id": "s", "code": "c",
		"properties": "value=42",
	})
	if err == nil || !strings.Contains(err.Error(), "must be an object") {
		t.Fatalf("expected an object-type error, got %v", err)
	}
}

// --------------------------------------------------------------------------- //
// Subscriptions
// --------------------------------------------------------------------------- //
func TestCreateSubscriptionForwardsThePlanAndExternalID(t *testing.T) {
	w, seen := fakeLago(t, 200, `{"subscription":{}}`)
	if _, err := w.CreateSubscription(context.Background(), "acme", "standard", "sub-9", "calendar"); err != nil {
		t.Fatal(err)
	}
	sub, _ := (*seen)[0].body["subscription"].(map[string]any)
	if sub["external_customer_id"] != "acme" || sub["plan_code"] != "standard" {
		t.Errorf("customer/plan not forwarded: %+v", sub)
	}
	if sub["external_id"] != "sub-9" {
		t.Errorf("external_id not forwarded: %+v", sub)
	}
	if sub["billing_time"] != "calendar" {
		t.Errorf("billing_time not forwarded: %+v", sub)
	}
}

func TestCreateSubscriptionOmitsBillingTimeWhenUnset(t *testing.T) {
	// Sending "" would override Lago's default with an invalid value.
	w, seen := fakeLago(t, 200, `{}`)
	if _, err := w.CreateSubscription(context.Background(), "acme", "standard", "sub-9", ""); err != nil {
		t.Fatal(err)
	}
	sub, _ := (*seen)[0].body["subscription"].(map[string]any)
	if _, present := sub["billing_time"]; present {
		t.Errorf("billing_time should be omitted, got %+v", sub)
	}
}

func TestSubscriptionToolRejectsAnInvalidBillingTime(t *testing.T) {
	w, seen := fakeLago(t, 200, `{}`)
	tool := toolNamed(t, WriteTools(w), "lago_create_subscription")
	_, err := tool.Handler(context.Background(), map[string]any{
		"external_customer_id": "acme", "plan_code": "standard",
		"external_subscription_id": "sub-9", "billing_time": "monthly",
	})
	if err == nil || !strings.Contains(err.Error(), "calendar") {
		t.Fatalf("expected a billing_time validation error, got %v", err)
	}
	if len(*seen) != 0 {
		t.Fatal("an invalid billing_time reached Lago")
	}
}

func TestTerminateSubscriptionRefusesAnEmptyID(t *testing.T) {
	// An empty id would build DELETE /api/v1/subscriptions/ — a collection-level
	// delete. Refused before the request, and refused again by the allow-list.
	w, seen := fakeLago(t, 200, `{}`)
	if _, err := w.TerminateSubscription(context.Background(), "   "); err == nil {
		t.Fatal("expected a refusal for an empty subscription id")
	}
	if len(*seen) != 0 {
		t.Fatal("an empty-id termination reached Lago")
	}
}

func TestTerminateSubscriptionEscapesTheID(t *testing.T) {
	w, seen := fakeLago(t, 200, `{}`)
	if _, err := w.TerminateSubscription(context.Background(), "sub/../../plans"); err != nil {
		t.Fatal(err)
	}
	// The id is percent-escaped, so a crafted value cannot climb out of the
	// subscriptions route into another Lago resource.
	//
	// Assert on the WIRE path, not r.URL.Path: net/http decodes %2F back to / in
	// URL.Path, so a test reading that field sees "/subscriptions/sub/../../plans"
	// and concludes the escaping failed when it worked. The decoded field is the
	// one that hides this either way round, which is why it is named here.
	got := (*seen)[0]
	if !strings.Contains(got.wire, "%2F") {
		t.Fatalf("id was not percent-escaped on the wire: %s", got.wire)
	}
	if strings.HasSuffix(got.wire, "/plans") {
		t.Fatalf("the request escaped its route: %s", got.wire)
	}
}

// --------------------------------------------------------------------------- //
// Wallet top-ups — a balance the customer can spend
// --------------------------------------------------------------------------- //
func TestTopUpSendsCreditsAsAString(t *testing.T) {
	w, seen := fakeLago(t, 200, `{}`)
	if _, err := w.TopUpWallet(context.Background(), "wal-1", "100.50"); err != nil {
		t.Fatal(err)
	}
	txn, _ := (*seen)[0].body["wallet_transaction"].(map[string]any)
	if txn["paid_credits"] != "100.50" {
		t.Errorf("credits must stay a string to avoid float rounding: %#v", txn["paid_credits"])
	}
	if txn["wallet_id"] != "wal-1" {
		t.Errorf("wallet_id not forwarded: %+v", txn)
	}
}

func TestTopUpToolRejectsAnythingButAPlainDecimal(t *testing.T) {
	w, seen := fakeLago(t, 200, `{}`)
	tool := toolNamed(t, WriteTools(w), "lago_top_up_wallet")
	for _, bad := range []string{"-5", "1e3", "100,00", "abc", ".5", "5.", "1.2.3", " 10"} {
		_, err := tool.Handler(context.Background(), map[string]any{
			"wallet_id": "wal-1", "paid_credits": bad,
		})
		if err == nil {
			t.Errorf("paid_credits %q should have been refused", bad)
		}
	}
	if len(*seen) != 0 {
		t.Fatal("a malformed credit amount reached Lago")
	}
}

func TestNegativeTopUpIsRefusedBecauseItIsAWithdrawal(t *testing.T) {
	// The one worth naming: "-50" is not a small top-up, it is taking money off a
	// customer's balance through a tool called top_up.
	if err := validateDecimalString("-50"); err == nil {
		t.Fatal("a negative top-up must be refused")
	}
	if err := validateDecimalString("50"); err != nil {
		t.Fatalf("a plain amount must be accepted: %v", err)
	}
	if err := validateDecimalString("0.01"); err != nil {
		t.Fatalf("a fractional amount must be accepted: %v", err)
	}
}

// --------------------------------------------------------------------------- //
// Configuration + transport
// --------------------------------------------------------------------------- //
func TestUnconfiguredWriterRegistersNoWriteTools(t *testing.T) {
	// An unconfigured deployment must advertise no mutations, rather than
	// advertising tools that fail on every call.
	if w := NewLagoWriter("", "", nil); w != nil {
		t.Fatal("a writer with no base URL must be nil")
	}
	if w := NewLagoWriter("https://billing", "", nil); w != nil {
		t.Fatal("a writer with no API key must be nil")
	}
	if tools := WriteTools(nil); tools != nil {
		t.Fatalf("expected no write tools for a nil writer, got %d", len(tools))
	}
}

func TestWriterForwardsTheDownstreamCredential(t *testing.T) {
	w, seen := fakeLago(t, 200, `{}`)
	if _, err := w.EmitUsageEvent(context.Background(), "t", "s", "c", nil); err != nil {
		t.Fatal(err)
	}
	if (*seen)[0].auth != "Bearer test-key" {
		t.Fatalf("downstream credential not sent: %q", (*seen)[0].auth)
	}
}

func TestWriteFailureSurfacesLagoStatusAndBody(t *testing.T) {
	// A silent failure on a billing write is the worst outcome: the caller
	// believes a customer was charged, or was not, and neither is true.
	w, _ := fakeLago(t, 422, `{"error":"plan_code not found"}`)
	_, err := w.CreateSubscription(context.Background(), "acme", "nope", "sub-1", "")
	if err == nil {
		t.Fatal("expected an error for a 422")
	}
	if !strings.Contains(err.Error(), "422") || !strings.Contains(err.Error(), "plan_code not found") {
		t.Fatalf("error must carry the status and Lago's reason: %v", err)
	}
}

// --------------------------------------------------------------------------- //
// Catalog shape
// --------------------------------------------------------------------------- //
func TestEveryWriteToolIsMarkedDestructive(t *testing.T) {
	// destructiveHint is what fires the middleware approval gate. A write tool
	// that is not marked would move money with no human in the loop.
	w, _ := fakeLago(t, 200, `{}`)
	tools := WriteTools(w)
	if len(tools) != 4 {
		t.Fatalf("expected 4 write tools, got %d", len(tools))
	}
	for _, tool := range tools {
		if !tool.Destructive {
			t.Errorf("%s mutates billing but is not marked Destructive", tool.Name)
		}
		if tool.InputSchema == nil {
			t.Errorf("%s has no input schema", tool.Name)
		}
		if len(tool.Description) < 120 {
			t.Errorf("%s description is too thin for an LLM to use safely", tool.Name)
		}
	}
}

func TestNoReadToolIsMarkedDestructive(t *testing.T) {
	// The mirror: marking a read destructive would put a human in front of every
	// billing question and make the surface useless.
	for _, tool := range ReadOnlyTools(NewLagoClient("https://x", "k", nil)) {
		if tool.Destructive {
			t.Errorf("%s is a read but is marked Destructive", tool.Name)
		}
	}
}

func toolNamed(t *testing.T, tools []Tool, name string) Tool {
	t.Helper()
	for _, tool := range tools {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("tool %q not found", name)
	return Tool{}
}
