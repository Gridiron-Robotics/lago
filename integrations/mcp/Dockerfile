# lago-mcp — the MCP sidecar that exposes Lago billing to the ERP brain over the
# uniform Gateway HTTP Contract v1 ("Contract A": GET /tools, POST /invoke, HEAD /).
#
# The estate's billing-gateway dials this container by name as `lago-mcp:8037`
# on erp_shared_network, so it has to exist as a container — the Go module and
# its registration downstream are not enough on their own.
#
# Two stages, modelled on events-processor/Dockerfile minus the Rust stage:
# integrations/mcp is pure Go and stdlib-only (its go.mod declares no requires),
# so there is no CGO and no libexpression_go.so to carry.
#
# Build context is THIS directory, because integrations/mcp carries its own
# go.mod (module github.com/gridiron-robotics/lago/integrations/mcp); the repo
# has no root go.mod to build from.

FROM golang:1.25 AS build

WORKDIR /src

# Separate layer so a future dependency gets cached independently of the source.
COPY go.mod ./
RUN go mod download

COPY . .

# CGO_ENABLED=0: nothing here needs cgo, and a static binary drops the glibc
# coupling between the build and runtime images.
RUN CGO_ENABLED=0 go build -o /out/lago-mcp ./cmd/lago-mcp

FROM debian:13-slim

# ca-certificates: LAGO_API_URL is an in-stack http:// hop in the default compose
# wiring, but points at an https:// Lago in other deployments, and a missing root
# store would fail those at TLS handshake.
# curl: the HEALTHCHECK below — the slim image ships neither.
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*

# This process fronts LAGO_API_KEY, which reads every customer's billing data and
# drives four money-moving writes. It has no reason to be root.
RUN useradd --system --create-home --shell /usr/sbin/nologin lagomcp

WORKDIR /app
COPY --from=build /out/lago-mcp /app/lago-mcp

USER lagomcp

EXPOSE 8037

# The transport is chosen at startup and there is exactly one way to get it wrong:
# with no HTTP address configured, cmd/lago-mcp/main.go serves stdio ONLY, binds
# no port, and — with no stdin in a container — returns immediately on EOF. That
# is a container that builds, starts, exits 0, and leaves the brain connecting to
# nothing. Setting the address here makes the HTTP transport the default; compose
# restates it next to `expose:` so the port is visible where operators look.
#
# Note this must stay an env var rather than an ENTRYPOINT flag: `-http` is a
# STRING flag, so a bare `-http` is not "use the default", it is
# "flag needs an argument: -http" and exit 2 on every start.
ENV LAGO_MCP_HTTP_ADDR=:8037

# GET / is the only route that answers 200 in every auth posture, including the
# fail-closed one a correctly-deployed sidecar sits in until LAGO_MCP_TOKEN is
# issued. Probing /tools would report that healthy-and-fail-closed state as
# unhealthy, because /tools answers 503 there by design (auth.go).
HEALTHCHECK --interval=10s --timeout=5s --start-period=10s --retries=3 \
  CMD curl -fsS http://127.0.0.1:8037/ || exit 1

ENTRYPOINT ["/app/lago-mcp"]
