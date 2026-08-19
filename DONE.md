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
      `helm lint` and `helm template` succeed; **`Gemfile.lock` is present,
      tracked, un-drifted and resolves kamal 2.11.0**, `.ruby-version` pins the
      Ruby CI installs, and the gate runs `BUNDLE_FROZEN=true` so it cannot
      repair the lockfile it is judging.

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
- [x] **Agent-plane metering (`/agent-usage`) — BOTH HALVES NOW EXIST.** The
      ingest pipeline is `connectors/agent_usage.yml` (this repo). The producer
      shipped in `langgraph-agents` as `agentic_core/lago_usage.py`, hooked into
      `cost_governor.py::governed_record()`: hourly rollup per
      (tenant, subsystem, actor, code) with a deterministic `transaction_id`,
      no client-supplied `organization_id`, and a fire-and-forget POST on a
      daemon thread so an ingest outage cannot fail the metered operation.
      Lago-side setup (create the `agent_tokens` / `agent_tool_calls` billable
      metrics on the organisation) is still a deploy step — see
      `connectors/README.md`.
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
  - [x] _OpenObserve self-heal wired AND shipped._ Failures emit `level=error`
        records with `service.name=lago` (the incident module) to **two**
        destinations: stderr (write-ahead JSON line) and, when the OTEL_* env is
        set, OpenObserve over OTLP/HTTP via `integrations/mcp/observability.go`
        (the same OpenTelemetry-log shipper `ach-payments`/`bigcapital-enterprise`
        use; a unit test with an injected exporter pins `service.name=lago` and
        that env-unset stays stderr-only). Register the estate rule with
        `apply-alerts.sh default` — **not** `apply-alerts.sh lago`: behind a shared
        collector all modules land in one `default` stream and module identity
        comes from `service_name`, so a per-module `lago` rule would watch nothing.
        The rail fires from the **handler path**, because Contract A turns every
        failure into structured non-2xx JSON — there is no 5xx or panic for an
        alert to key off, so a crash-based rail would report nothing while the
        whole surface failed. Caller mistakes log at `warn`, not `error`, so
        malformed tool calls don't bury real faults; each record carries
        `destructive` for triage.
  - [x] _The gate enforces the invariant structurally_, not just by test: `make mcp`
        fails if a non-GET request is constructed outside `lagowriter.go`, or if
        `allowedWrites` stops being an explicit enumeration. Verified by injecting
        a violation and confirming the gate goes red.
  - [x] _Registered downstream._ `langgraph-agents/INTEGRATIONS.md` §19 pins `lago`
        as a **native** Contract-A gateway at `http://lago-mcp:8037` (`kind:
        contract`, native routes at root — no `/mcp` suffix) and
        `specialists/registry.py:416` binds it to a specialist's `servers` tuple.
        The other half of the original line — "register it in the middleware
        `mcp_gateway/tools.py` catalog" — was **misdirected, not merely pending**:
        that module registers handlers for the middleware's *own* Django domain
        apps (catalog, orders, payments, tax, …), and its `registry.Tool.module`
        field is a middleware module name. A standalone module gateway like this
        one is consumed by langgraph directly over Contract A; routing it through
        the middleware's in-process tool table would invert the architecture. No
        middleware change is owed. `LAGO_MCP_TOKEN` is a deploy-time secret and
        stays out of git by design (see "Never put secrets in git").
  - [x] _Containerized + on the estate network._ `integrations/mcp/Dockerfile`
        (two-stage, CGO-free, non-root) and a `lago-mcp` service in
        `deploy/docker-compose.production.yml` (`expose: 8037`, `LAGO_MCP_TOKEN`,
        `LAGO_API_URL`/`LAGO_API_KEY`), joined to `erp_shared_network` — the same
        two-network sidecar shape bigcapital ships. Registration downstream is not
        enough on its own: `langgraph-agents/docker-compose.real.yml` dials
        `http://lago-mcp:8037`, so without this service that name resolves to
        nothing and every billing tool call fails at connect. Structurally covered
        by the `pins` + `compose` gates. Live verification (a `curl -I` from a
        container on `erp_shared_network` returning 200) is a deploy step; the
        consumer side still needs its `GATEWAY_TOKENS` `lago` entry set to the same
        token (a `langgraph-agents` change, tracked cross-repo).
  - [x] IGNORED (charter) — _the Rails billing engine (`api/`, `front/`) is an empty
        submodule here._ That is this repo's **scope**, not a gap in it: it is the
        Lago **deploy** repo (Kamal + Helm + compose + connectors + the Go
        events-processor), and the upstream Rails app is intentionally not vendored.
        The tool surface is therefore validated against an httptest Lago, which is
        the correct boundary for a deploy repo — exercising upstream's own code
        paths belongs to upstream's suite, and pulling the submodule in to chase
        coverage would make this repo a fork it is not meant to be. Recorded rather
        than deleted so the limitation stays visible to whoever reads the numbers.

### Ratchet log (add a line every time a bug slips through)

- _2026-… — example: "smoke test added after an all-404 ASGI bug shipped green."_
- _2026-08 — the MCP block shipped fully checked over a surface that had no
  container and no log shipper: registration downstream was done but no
  `Dockerfile`/compose service existed, and `level=error` records went to stderr
  with nothing shipping them (and the registration command named a per-module
  stream that cannot exist behind a shared collector). Closed by shipping
  `integrations/mcp/Dockerfile` + the `lago-mcp` compose service and the
  `observability.go` OTLP shipper, and corrected `apply-alerts.sh lago` →
  `default`. Both are now their own checked boxes so the gap stays visible._
- _2026-08 — the deploy gate asserted kamal 2.11.0 from `Gemfile` and
  `.kamal/version` but never looked at `Gemfile.lock`, which is what
  `bundle exec kamal` actually runs. Worse, when the lockfile check was first
  added it could not fail: the gate's own `bundle exec` re-resolves and
  REWRITES a missing/drifted `Gemfile.lock` before the checks read it, so
  deleting the lockfile or drifting it to kamal 1.9.0 both stayed GREEN. Closed
  by `export BUNDLE_FROZEN=true` (a gate must never mutate the artifact it
  judges) plus running the static lockfile checks before anything invokes
  bundler. Caught only by mutation-testing the new checks — a check that has
  never been observed to go RED is not a check._
