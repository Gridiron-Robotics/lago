package mcp

import (
	"context"
	"fmt"
)

// WriteTools returns the billing-lifecycle tools that MUTATE Lago.
//
// Why these exist: a read-only surface means the agent can report on billing but
// cannot run it, which is no autonomy at all on the module whose job is billing
// subscriptions and metered usage. These four are the lifecycle — meter, start,
// stop, credit — and nothing more.
//
// Every one is marked Destructive, which sets destructiveHint in the Contract A
// catalog and puts a human in front of it via the middleware approval gate. That
// is not a formality on this surface: these calls move money.
//
// Returns nil when the writer is unconfigured, so an unconfigured deployment
// advertises no write tools at all rather than advertising tools that 500.
func WriteTools(w *LagoWriter) []Tool {
	if w == nil {
		return nil
	}
	return []Tool{
		{
			Name: "lago_emit_usage_event",
			Description: "Record a metered usage event against a subscription — the meter reading " +
				"everything else is billed from. Requires a transaction_id that is UNIQUE per real " +
				"usage occurrence: Lago deduplicates on it, so replaying the same transaction_id is " +
				"safe, and reusing it for a DIFFERENT occurrence silently loses that usage. Never " +
				"generate a fresh transaction_id when retrying — a retry with a new id bills the " +
				"customer twice for one unit. DESTRUCTIVE: this changes what the customer will be " +
				"invoiced, and requires approval.",
			InputSchema: objectSchema(map[string]any{
				"transaction_id": stringProp(
					"Idempotency key for this usage occurrence. Reuse it verbatim on retry."),
				"external_subscription_id": stringProp("The subscription's external_id"),
				"code":                     stringProp("The billable metric code (e.g. api_calls, gb_stored)"),
				"properties": map[string]any{
					"type":        "object",
					"description": "Metric properties, e.g. {\"value\": 42}. Shape depends on the billable metric.",
				},
			}, "transaction_id", "external_subscription_id", "code"),
			Destructive: true,
			Handler: func(ctx context.Context, args map[string]any) (string, error) {
				txn, err := requireString(args, "transaction_id")
				if err != nil {
					return "", err
				}
				sub, err := requireString(args, "external_subscription_id")
				if err != nil {
					return "", err
				}
				code, err := requireString(args, "code")
				if err != nil {
					return "", err
				}
				props, err := optionalObject(args, "properties")
				if err != nil {
					return "", err
				}
				return asText(w.EmitUsageEvent(ctx, txn, sub, code, props))
			},
		},
		{
			Name: "lago_create_subscription",
			Description: "Subscribe a customer to a plan, which starts billing them. external_id is " +
				"the caller's own handle for this subscription and must be unique per subscription; " +
				"reusing one that already exists is how a customer ends up billed on two plans. " +
				"billing_time is 'calendar' (bill on period boundaries) or 'anniversary' (bill from " +
				"the start date) — omit it to take Lago's default. DESTRUCTIVE: this starts charging " +
				"a customer, and requires approval.",
			InputSchema: objectSchema(map[string]any{
				"external_customer_id":     stringProp("The customer's external_id"),
				"plan_code":                stringProp("The plan's code"),
				"external_subscription_id": stringProp("A unique external_id for the new subscription"),
				"billing_time":             stringProp("Optional: 'calendar' or 'anniversary'"),
			}, "external_customer_id", "plan_code", "external_subscription_id"),
			Destructive: true,
			Handler: func(ctx context.Context, args map[string]any) (string, error) {
				cust, err := requireString(args, "external_customer_id")
				if err != nil {
					return "", err
				}
				plan, err := requireString(args, "plan_code")
				if err != nil {
					return "", err
				}
				sub, err := requireString(args, "external_subscription_id")
				if err != nil {
					return "", err
				}
				billingTime, _ := args["billing_time"].(string)
				if billingTime != "" && billingTime != "calendar" && billingTime != "anniversary" {
					return "", fmt.Errorf("billing_time must be 'calendar' or 'anniversary', got %q", billingTime)
				}
				return asText(w.CreateSubscription(ctx, cust, plan, sub, billingTime))
			},
		},
		{
			Name: "lago_terminate_subscription",
			Description: "Terminate a subscription, which STOPS billing the customer and triggers a " +
				"final invoice for usage so far. Not reversible: a terminated subscription cannot be " +
				"resumed, only replaced by a new one, and the customer immediately loses whatever the " +
				"plan entitles them to. DESTRUCTIVE: requires approval.",
			InputSchema: objectSchema(map[string]any{
				"external_subscription_id": stringProp("The subscription's external_id"),
			}, "external_subscription_id"),
			Destructive: true,
			Handler: func(ctx context.Context, args map[string]any) (string, error) {
				sub, err := requireString(args, "external_subscription_id")
				if err != nil {
					return "", err
				}
				return asText(w.TerminateSubscription(ctx, sub))
			},
		},
		{
			Name: "lago_top_up_wallet",
			Description: "Add paid credits to a customer's prepaid wallet. paid_credits is a decimal " +
				"STRING in wallet credit units (e.g. \"100.0\"), not cents and not a float — passing a " +
				"float risks a binary-rounding error in a balance. Takes the wallet's lago_id from " +
				"lago_list_wallets, not a customer id. DESTRUCTIVE: this credits a real balance the " +
				"customer can spend, and requires approval.",
			InputSchema: objectSchema(map[string]any{
				"wallet_id":    stringProp("The wallet's lago_id (from lago_list_wallets)"),
				"paid_credits": stringProp("Credits to add, as a decimal string, e.g. \"100.0\""),
			}, "wallet_id", "paid_credits"),
			Destructive: true,
			Handler: func(ctx context.Context, args map[string]any) (string, error) {
				wallet, err := requireString(args, "wallet_id")
				if err != nil {
					return "", err
				}
				credits, err := requireString(args, "paid_credits")
				if err != nil {
					return "", err
				}
				if err := validateDecimalString(credits); err != nil {
					return "", err
				}
				return asText(w.TopUpWallet(ctx, wallet, credits))
			},
		},
	}
}

// optionalObject reads an optional object-valued argument.
func optionalObject(args map[string]any, key string) (map[string]any, error) {
	v, ok := args[key]
	if !ok || v == nil {
		return nil, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("argument %q must be an object", key)
	}
	return m, nil
}

// validateDecimalString rejects anything that is not a plain non-negative decimal.
//
// Lago takes credits as a string precisely so no float rounding happens in
// transit; letting "1e3", "-5", or "100,00" through would defeat that, and a
// negative "top-up" is a silent withdrawal from a customer's balance.
func validateDecimalString(s string) error {
	if s == "" {
		return fmt.Errorf("paid_credits must not be empty")
	}
	dot := false
	for i, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r == '.' && !dot && i != 0 && i != len(s)-1:
			dot = true
		default:
			return fmt.Errorf(
				"paid_credits must be a plain non-negative decimal string like \"100.0\", got %q", s)
		}
	}
	return nil
}
