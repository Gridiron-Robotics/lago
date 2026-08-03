#!/usr/bin/env bash
#
# mcp-gate.sh - the MCP server gate.
#
# Validates integrations/mcp/ (a pure-Go, stdlib-only MCP server exposing Lago
# billing as agent tools). Pure Go (no CGO), so it builds and tests anywhere -
# locally and in CI.
#
# The headline invariant, and the reason for the structural check below: the READ
# client is GET-only, and every mutation goes through the single audited
# chokepoint in lagowriter.go, which enforces an explicit allow-list of
# (method, path) pairs before a request leaves the process. A non-GET request
# constructed anywhere else would be a mutation nobody enumerated, reviewed, or
# marked destructive - so the gate fails if one appears.
#
# (This gate previously claimed the whole surface was READ-ONLY. That was true
# when the module could only report on billing and could not drive it, which left
# no autonomy for the metered-billing lifecycle. The invariant is now
# "writes exist only behind the allow-list", which is what the tests enforce.)
#
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
source "${HERE}/lib.sh"

ROOT="$(repo_root)"
PKG="${ROOT}/integrations/mcp"

if [[ ! -d "${PKG}" ]]; then
  skip "integrations/mcp/ not present"
  finish "MCP server gate"; exit $?
fi
if ! have go; then
  skip "go toolchain not found"
  finish "MCP server gate"; exit $?
fi

cd "${PKG}"

unformatted="$(gofmt -l . 2>/dev/null || true)"
if [[ -n "${unformatted}" ]]; then
  fail "gofmt: files not formatted"
  note "run: gofmt -w ${unformatted//$'\n'/ }"
else
  pass "gofmt: formatted"
fi

if go vet ./... >.vet.log 2>&1; then
  pass "go vet"
else
  fail "go vet"
  note "$(tail -n 12 .vet.log)"
fi
rm -f .vet.log

if go build ./... >.build.log 2>&1; then
  pass "go build ./... (incl. cmd/lago-mcp)"
else
  fail "go build ./..."
  note "$(tail -n 12 .build.log)"
fi
rm -f .build.log

# Prefer -race (read-only invariant is checked under concurrency); fall back if
# the race detector can't build here (needs CGO + a C compiler).
# Structural check: no non-GET Lago request may be constructed outside the single
# audited chokepoint. Greps the *outbound* client files only - http.go inspects
# r.Method on INBOUND requests, which is unrelated.
stray=""
for f in *.go; do
  [[ "${f}" == *_test.go || "${f}" == lagowriter.go || "${f}" == http.go ]] && continue
  if grep -qE 'http\.Method(Post|Put|Patch|Delete)' "${f}"; then
    stray="${stray} ${f}"
  fi
done
if [[ -n "${stray}" ]]; then
  fail "a mutation is constructed outside lagowriter.go:${stray}"
  note "every write must go through LagoWriter.do(), which enforces allowedWrites"
else
  pass "mutations confined to the lagowriter.go allow-list"
fi

# The allow-list must stay small and enumerated; an empty or wildcarded list would
# silently reopen the whole Lago API.
if grep -q 'var allowedWrites' lagowriter.go && [[ "$(grep -cE '^\s*\{http\.Method' lagowriter.go)" -gt 0 ]]; then
  pass "allowedWrites is an explicit (method, path) enumeration"
else
  fail "allowedWrites is missing or no longer an explicit enumeration"
fi

if go test -race ./... >.test.log 2>&1; then
  pass "go test -race ./... (write allow-list + fail-closed auth verified)"
elif grep -qiE 'race|cgo|gcc|cc1|C compiler' .test.log && go test ./... >.test2.log 2>&1; then
  pass "go test ./... (race detector unavailable here; ran without -race)"
  rm -f .test2.log
else
  fail "go test ./... (MCP server tests failing)"
  note "$(tail -n 20 .test.log)"
fi
rm -f .test.log .test2.log

finish "MCP server gate"
exit $?
