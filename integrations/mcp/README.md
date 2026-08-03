# integrations/mcp — MCP server (make the ERP agentic)

A [Model Context Protocol](https://modelcontextprotocol.io) server that exposes
Lago **subscription and usage-based billing** to AI agents as tools. Point any MCP
client (Claude Desktop, Claude Code, the langgraph brain) at it and an agent can
both answer billing questions *and* drive the metered-billing lifecycle — with
every mutation enumerated, marked destructive, and idempotency-keyed.

Run the gate: `make mcp`. Pure Go, stdlib only (no deps, no CGO), so it builds and
tests anywhere.

## Scope: this module bills subscriptions and usage, not orders

Lago is the estate's **metering and subscription-billing** engine. Recurring plan
charges and consumption-based charges (per API call, per delivery, per GB) belong
here. Transactional order invoicing and the general ledger live elsewhere
(`bigcapital`, `pos-api-backend`), so the absence of "create an order invoice"
tools is the correct scope, not a gap.

## What an agent can do, and what stops it

The read client is **GET-only by construction** — `LagoClient` has no mutating
method and its one request path hard-codes `http.MethodGet`.

Every write goes through a **single audited chokepoint**, `LagoWriter.do()`, which
checks the request against an explicit allow-list *before it leaves the process*:

| Method | Path |
|---|---|
| `POST` | `/api/v1/events` |
| `POST` | `/api/v1/subscriptions` |
| `DELETE` | `/api/v1/subscriptions/{id}` |
| `POST` | `/api/v1/wallet_transactions` |

Anything else is refused in-process, so a buggy or adversarial tool call cannot
reach an un-enumerated Lago endpoint. Three layers keep that true:

1. **The allow-list itself** — the request is rejected before dispatch, and the
   bare collection path (`/api/v1/subscriptions/` with no id) does not match the
   DELETE prefix, so a truncated id cannot become "delete the collection".
2. **`destructiveHint: true`** on all four write tools, which makes the middleware
   put a human in front of them (`approvals`, per §3A of the estate charter).
3. **The `mcp` gate**, which fails if a non-GET request is constructed in any file
   other than `lagowriter.go`, or if `allowedWrites` stops being an explicit
   enumeration.

Write tools are registered **only when the writer is configured**, so an
unconfigured deployment advertises no mutations at all rather than tools that fail
on every call.

## Tools

Reads (`destructiveHint: false`):

| Tool | Returns |
|---|---|
| `lago_get_customer` | a customer by `external_id` |
| `lago_customer_current_usage` | current uninvoiced usage for a subscription |
| `lago_list_invoices` | invoices (optionally filtered to a customer) |
| `lago_get_invoice` | one invoice by `lago_id` |
| `lago_list_subscriptions` | subscriptions (optionally filtered to a customer) |
| `lago_list_wallets` | prepaid credit wallets + balances for a customer |

Writes (`destructiveHint: true` — each requires human approval upstream):

| Tool | Effect | The trap it warns about |
|---|---|---|
| `lago_emit_usage_event` | meters one billable usage event | `transaction_id` is Lago's dedup key. **Retrying with a new id double-bills.** Reuse the same id to retry safely. |
| `lago_create_subscription` | starts a subscription on a plan | Creates a recurring charge. `billing_time` must be `calendar` or `anniversary`. |
| `lago_terminate_subscription` | stops a subscription | Ends billing and may trigger a final invoice. The id is path-escaped. |
| `lago_top_up_wallet` | credits a prepaid wallet | Moves money. Amounts are plain non-negative decimal **strings** (`"100.0"`), never floats — a float here is a rounding bug on a balance. |

## Boundary authentication (fail-closed)

The bearer token presented on `/tools` and `/invoke` is compared against
`LAGO_MCP_TOKEN` in **constant time**. With no token configured the surface
**refuses to serve** (`503`) rather than accepting any caller:

| `LAGO_MCP_TOKEN` | `LAGO_MCP_ALLOW_INSECURE` | Mode | Behaviour |
|---|---|---|---|
| set | anything | `token` | bearer must match; `401` missing, `403` wrong |
| unset | unset/false | `fail-closed` | **`503` on every tool route** — the 503 names the variable to set |
| unset | `true` | `insecure-explicitly-allowed` | open (local development only) |

`HEAD /` stays open in every mode so a gateway can see liveness without a
credential, and it leaks nothing. A configured token **beats** the insecure flag,
so a leftover `LAGO_MCP_ALLOW_INSECURE` in production cannot silently disable a
credential that is set.

Why fail-closed matters here: this surface fronts `LAGO_API_KEY`, which reads
every customer's billing data and now drives the path that moves money. The
previous behaviour accepted *any* string after `Bearer `, checking only the
header's shape — "a bearer is required" was true and meaningless.

## Run it

```bash
go build -o lago-mcp ./integrations/mcp/cmd/lago-mcp
LAGO_API_URL=https://billing.yourco.com LAGO_API_KEY=*** ./lago-mcp
```

It speaks JSON-RPC 2.0 over stdio (`initialize` → `tools/list` → `tools/call`).
Wire it into an MCP client, e.g. Claude Desktop `claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "lago": {
      "command": "/path/to/lago-mcp",
      "env": { "LAGO_API_URL": "https://billing.yourco.com", "LAGO_API_KEY": "***" }
    }
  }
}
```

## HTTP transport (Gateway "Contract A")

For the langgraph brain (and any network gateway) the server ALSO speaks the
uniform Gateway HTTP Contract v1 — the same three routes every ERP module
exposes. stdio stays available for desktop use.

```bash
LAGO_API_URL=… LAGO_API_KEY=… LAGO_MCP_TOKEN=… ./lago-mcp -http   # default :8037
LAGO_API_URL=… LAGO_API_KEY=… LAGO_MCP_TOKEN=… LAGO_MCP_HTTP_ADDR=:8037 ./lago-mcp
```

| Verb | Shape |
|---|---|
| List | `GET /tools?server=lago` → `{"tools":[{name,description,input_schema,annotations:{destructiveHint,readOnlyHint}}]}` (unknown `server` → empty list, not an error) |
| Invoke | `POST /invoke` `{"server","tool","arguments"}` → `{"tool","result","destructive"}` (+ `"replayed":true` on idempotent replay) |
| Health | `HEAD /` → 200 |

Headers on `/tools` + `/invoke`: `Authorization: Bearer <token>` (matched against
`LAGO_MCP_TOKEN`; `LAGO_API_KEY` remains the downstream Lago credential),
`X-Tenant-Id` (default `default`), `Idempotency-Key` (replay dedup, **scoped per
tenant** so two tenants reusing the same key never cross-read each other's
result). Errors are non-2xx JSON `{"error":…}`: 400 bad body or caller mistake,
401 missing bearer, 403 wrong bearer, 404 unknown tool/server, 422 missing
required argument, 502 upstream Lago failure, 503 unconfigured.

An **unknown tool defaults to destructive** — a registry lookup that misses must
not report a mutation as harmless.

## OpenObserve self-heal

Failures on this surface emit one JSON line per record with `level=error`, which
is the field the estate alert rule matches; `service` is `lago`, which is both the
OpenObserve stream and the incident `module`.

Register the alert once per stream:

```bash
langgraph-agents/deploy/observability/apply-alerts.sh lago
```

**The trap this is built around:** Contract A converts every tool failure into a
structured non-2xx JSON response. There is no unhandled panic and no 5xx for an
alert to key off — so a rail hung off "the process crashed" would report *nothing*
while the entire billing tool surface failed every call. The rail therefore fires
from the **handler path** (`http.go`), not from a crash handler.

Caller mistakes (missing argument, bad decimal, a refused mutation) are logged at
`warn`, deliberately **not** `error`: they are the agent's to fix, and raising a
self-heal incident for every malformed tool call would bury the real faults. The
`isCallerError` classifier draws that line, and each failure record carries
`destructive` so an operator triaging an incident knows immediately whether the
failed call could have moved money.

## Files

| File | Role |
|---|---|
| `server.go` | minimal MCP server: JSON-RPC over stdio (`initialize`/`tools/list`/`tools/call`/`ping`) + destructive lookup |
| `http.go` | Contract A HTTP surface (`GET /tools`, `POST /invoke`, `HEAD /`) reusing the same tools |
| `auth.go` | fail-closed boundary credential (constant-time compare, three modes) |
| `selfheal.go` | OpenObserve rail: `level=error` records + caller-vs-operational classifier |
| `lagoclient.go` | **GET-only** Lago REST client (the read chokepoint) |
| `lagowriter.go` | **allow-listed** Lago write client (the write chokepoint) |
| `tools.go` | the six read tool definitions + handlers |
| `writetools.go` | the four write tool definitions + argument validation |
| `cmd/lago-mcp/main.go` | wires env → client/writer → server → stdio (+ optional HTTP) |
| `*_test.go` | handshake, GET-only invariant, allow-list, auth postures, rail wiring, HTTP contract |

## Extending it

New mutations are **not** a code-only change. Adding one means: a new entry in
`allowedWrites`, a tool with `Destructive: true`, argument validation with a test
per rejection path, and a note here. The gate enforces the first two structurally.

Accounting writes (exactly-once GL posting) live in `integrations/accounting/` and
can be surfaced here as tools when the ledger side is ready.
