# integrations/mcp — read-only MCP server (make the ERP agentic)

A [Model Context Protocol](https://modelcontextprotocol.io) server that exposes
Lago billing data to AI agents as **read-only tools**. Point any MCP client
(Claude Desktop, Claude Code, your own agent) at it and an agent can answer
billing/ERP questions — *without being able to change anything*.

Run the gate: `make mcp`. Pure Go, stdlib only (no deps, no CGO), so it builds and
tests anywhere.

## The read-only guarantee (enforced by a test)

The server is read-only **by construction**: `LagoClient` has no method that
mutates, and its one request path hard-codes `http.MethodGet`. The MCP gate fails
if any tool ever issues a non-GET request to Lago
(`TestReadOnlyTools_DispatchAndStayGET`). So even a buggy or adversarial tool call
can only ever read. Writes (create subscription, issue invoice, post to
accounting) are deliberately *not* here — that's a separate, guard-railed step.

## Tools

| Tool | Returns |
|---|---|
| `lago_get_customer` | a customer by `external_id` |
| `lago_customer_current_usage` | current uninvoiced usage for a subscription |
| `lago_list_invoices` | invoices (optionally filtered to a customer) |
| `lago_get_invoice` | one invoice by `lago_id` |
| `lago_list_subscriptions` | subscriptions (optionally filtered to a customer) |
| `lago_list_wallets` | prepaid credit wallets + balances for a customer |

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
LAGO_API_URL=… LAGO_API_KEY=… ./lago-mcp -http            # default :8037
LAGO_API_URL=… LAGO_API_KEY=… LAGO_MCP_HTTP_ADDR=:8037 ./lago-mcp
```

| Verb | Shape |
|---|---|
| List | `GET /tools?server=lago` → `{"tools":[{name,description,input_schema,annotations:{destructiveHint:false}}]}` (unknown `server` → empty list) |
| Invoke | `POST /invoke` `{"server","tool","arguments"}` → `{"tool","result"}` (+ `"replayed":true` on idempotent replay) |
| Health | `HEAD /` → 200 |

Headers on `/tools` + `/invoke`: `Authorization: Bearer <jwt>` (required at the
boundary; `LAGO_API_KEY` remains the downstream Lago credential), `X-Tenant-Id`
(default `default`), `Idempotency-Key` (replay dedup). Errors are non-2xx JSON
`{"error":…}`: 400 bad body / handler error, 401 missing bearer, 404 unknown
tool/server. All tools are read-only, so `destructiveHint` is always `false`.

## Files

| File | Role |
|---|---|
| `server.go` | minimal MCP server: JSON-RPC over stdio (`initialize`/`tools/list`/`tools/call`/`ping`) |
| `http.go` | Contract A HTTP surface (`GET /tools`, `POST /invoke`, `HEAD /`) reusing the same tools |
| `lagoclient.go` | **GET-only** Lago REST client (the read-only choke point) |
| `tools.go` | the six read tool definitions + handlers |
| `cmd/lago-mcp/main.go` | wires env → client → server → stdio (+ optional HTTP) |
| `*_test.go` | JSON-RPC handshake, GET-only invariant, tool dispatch, HTTP contract |

## Extending it (later, with guard rails)

When you want **action-taking** tools (the "read + guarded writes" path), add them
behind their own contract gates: idempotency keys on every mutation, an allowlist
of permitted actions, and a test per tool. The accounting writes already exist in
`integrations/accounting/` (exactly-once) and can be surfaced here as tools when
you're ready.
