# DONE.md — the finish line for "production ready"

You can't loop toward "perfect" if nothing defines it. This is the definition.
The repo is production-ready for enterprise billing when **every box is checked
under STRICT mode** (`STRICT=1 ./repo-gates/verify.sh` green) and a fresh-eyes
review surfaces no unresolved critical/high findings.

This list **ratchets**: every time a real bug escapes the gates, add a check here
and a gate for it so it can never escape again.

## Objective gates (must all be green under STRICT)

- [ ] **Pins** — `repo-gates/pins-allow.txt` is empty; no floating versions
      anywhere (`package.json` exact, no `:latest`, no `@latest`).
- [ ] **Go events-processor** — `gofmt` clean, `go vet` clean, `golangci-lint`
      clean, `go build ./...` and `go test ./...` pass **with the CGO Rust lib
      built** (CI, or `GO_TEST_IN_DOCKER=1`).
- [ ] **Connectors** — `connectors/*.yml` pass `redpanda-connect lint`
      (not just YAML parse).
- [ ] **Compose / Docker / shell** — all compose files validate; `hadolint`
      error-level clean; `shellcheck` error-level clean.
- [ ] **Deploy** — Kamal is exactly **2.11.0**; `kamal config` renders;
      `helm lint` and `helm template` succeed.

## Tooling installed so nothing SKIPs

- [ ] `go` + `libexpression_go.so`, `golangci-lint`, `hadolint`, `shellcheck`,
      `kamal` 2.11.0, `helm`, `jq` all present in CI.
- [ ] `STRICT=1 ./repo-gates/verify.sh` is **green** (no SKIPs left).

## Live / runtime (the bug-class the last repo taught us)

- [ ] `make smoke` (or `SMOKE_UP=1 make smoke`) proves the API answers
      `GET /health` → 200, and routing responds at `/api/v1` (not all-404).
- [ ] DB migrations run cleanly (`./scripts/migrate.sh`) before the app serves.

## Deploy artifacts are real, not just valid

- [ ] `config/deploy.yml` has no `<PLACEHOLDER>` left; servers/registry/domain set.
- [ ] `.kamal/secrets` exists locally / in your secret store (never committed).
- [ ] Helm `lago-secrets` Secret documented and created in the target namespace.
- [ ] Every image tag (Kamal accessories + Helm `values.yaml`) is pinned.
- [x] **Headless by default:** production compose starts no human login page — the
      Lago `front` dashboard and `portainer` are opt-in via `--profile dashboard` /
      `--profile portainer`. (Set `LAGO_SIDEKIQ_WEB=false` to drop the last UI.)

## Review & sign-off

- [ ] `make review` run; `review-findings.json` has **0 critical, 0 high** open.
- [ ] `make gate` prints **MERGE-ELIGIBLE**.
- [ ] A human read `git diff` and approved. (Agents do the 80%; you own the merge.)

---

## Planned integrations (future scope — not built yet; gate each when built)

Per the ratchet rule, each integration ships with its own gate the day it's built.

- [ ] **Middleware → Lago (inbound usage):** a new `connectors/<name>.yml` (Redpanda
      Connect). Auto-covered by `connectors-gate.sh`; pinning gate covers its deps.
- [x] **Lago → accounting (outbound) — BUILT (gate-first, all four ERPs).**
      The exactly-once contract is enforced in `integrations/accounting/`
      (`make accounting`): "given usage event X, the **selected** accounting target
      receives entry Y, exactly once", proven under concurrency and
      retry-after-failure. One config-driven selector, **defaulting to the in-house
      Bigcapital module first**.
  - [x] _Four real targets implemented + validated offline (httptest sim) under
        `-race`, each idempotent via its API's native mechanism:_ **Bigcapital**
        (default; `Idempotency-Key` + reference), **QuickBooks** (`requestid`),
        **Xero** (`Idempotency-Key` header), **NetSuite** (externalId upsert,
        OAuth1 TBA). Shared `httptarget.go` for transport + error classification.
  - [x] _Durable store:_ `redis_store.go` (Redis `IdempotencyStore`), validated
        with a fake client; Postgres analog documented.
  - [ ] _Remaining:_ point each target's `*_BASE_URL`/creds at the real tenant,
        back the store with prod Redis/Postgres, and wire the target choice to the
        ERP selector UI (`AvailableTargets()` provides the options).
- [x] **Agentic ERP — MCP server BUILT.** `integrations/mcp/` (`make mcp`): a
      stdlib-only MCP server exposing Lago subscription + usage-based billing to AI
      agents over stdio **and** over the estate's Gateway HTTP Contract v1.
      Validated under `-race`.
  - [x] _Reads are GET-only by construction_ — `LagoClient` has no mutating method
        and its one request path hard-codes `http.MethodGet`.
  - [x] _Action-taking tools behind contract gates:_ 4 write tools (meter a usage
        event, start/stop a subscription, credit a wallet) — the billing lifecycle
        an autonomous operator actually needs. Every mutation goes through one
        audited chokepoint (`LagoWriter.do()`) that checks an explicit
        `allowedWrites` allow-list of (method, path) pairs **before the request
        leaves the process**; the bare collection path does not match the DELETE
        prefix, so a truncated id cannot delete a collection.
  - [x] _Every write is `destructiveHint: true`_ so the middleware puts a human in
        front of it (§3A HITL). An **unknown tool defaults to destructive** — a
        registry miss must not report a mutation as harmless.
  - [x] _Idempotency:_ `Idempotency-Key` replay dedup, **scoped per tenant** so two
        tenants reusing a key never cross-read each other's result. Money amounts
        are validated as plain non-negative decimal **strings**, never floats.
  - [x] _Write tools are registered only when the writer is configured_, so an
        unconfigured deployment advertises no mutations rather than tools that fail
        every call.
  - [x] _Boundary auth is fail-closed._ The bearer is compared against
        `LAGO_MCP_TOKEN` in constant time. Unset token ⇒ **503 on every tool
        route** (the 503 names the variable); `LAGO_MCP_ALLOW_INSECURE=true` is the
        explicit dev-only escape hatch, and a configured token beats it. `HEAD /`
        stays open for liveness and leaks nothing.
  - [x] _OpenObserve self-heal wired._ Failures emit `level=error` records with
        `service=lago` (the stream **is** the incident module). Register with
        `langgraph-agents/deploy/observability/apply-alerts.sh lago`. The rail fires
        from the **handler path**, because Contract A turns every failure into
        structured non-2xx JSON — there is no 5xx or panic for an alert to key off,
        so a crash-based rail would report nothing while the whole surface failed.
        Caller mistakes log at `warn`, not `error`, so malformed tool calls don't
        bury real faults; each record carries `destructive` for triage.
  - [x] _The gate enforces the invariant structurally_, not just by test: `make mcp`
        fails if a non-GET request is constructed outside `lagowriter.go`, or if
        `allowedWrites` stops being an explicit enumeration. Verified by injecting
        a violation and confirming the gate goes red.
  - [ ] _Remaining:_ register `lago` in the middleware `mcp_gateway/tools.py`
        catalog and in the langgraph `INTEGRATIONS.md` manifest (both live in other
        repos), and point `LAGO_MCP_TOKEN` at the real gateway credential.
  - [ ] _Remaining:_ the Rails billing engine itself (`api/`, `front/`) is an empty
        submodule here, so nothing in this repo exercises Lago's own code paths —
        the tool surface is validated against an httptest Lago, not a live one.

### Ratchet log (add a line every time a bug slips through)

- _2026-… — example: "smoke test added after an all-404 ASGI bug shipped green."_
