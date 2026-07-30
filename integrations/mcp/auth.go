package mcp

import (
	"crypto/hmac"
	"crypto/sha256"
	"net/http"
	"os"
	"strings"
)

// Boundary authentication for the Contract A surface.
//
// What was here before: `hasBearer` checked that an Authorization header STARTED
// with "Bearer " and had something after it. Any string passed. So the surface was
// effectively unauthenticated to anything that could reach the port — and it
// fronts LAGO_API_KEY, a credential that can read every customer's billing data
// and (now that write tools exist) move money. "A bearer is required" was true
// and meaningless.
//
// Now: the presented token is compared against LAGO_MCP_TOKEN in constant time,
// and with no token configured the surface REFUSES TO SERVE (503) rather than
// accepting everything. Fail-closed is the only safe default for a surface whose
// downstream credential is a billing API key; an operator who forgets to set the
// variable gets an outage, which is loud, instead of an open door, which is not.
//
// LAGO_MCP_ALLOW_INSECURE=true is the explicit local-development escape hatch. It
// has to be typed, so nobody arrives at it by omission.

// authMode describes the boundary posture. Exported for the health/diagnostic
// surface and asserted by tests.
type authMode string

const (
	authToken    authMode = "token"
	authInsecure authMode = "insecure-explicitly-allowed"
	authClosed   authMode = "fail-closed"
)

// authOutcome is the result of checking one request.
type authOutcome struct {
	ok      bool
	status  int
	message string
}

// authConfig is the boundary credential configuration, read from the environment
// once at handler-construction time (injected in tests).
type authConfig struct {
	token         string
	allowInsecure bool
}

func authConfigFromEnv() authConfig {
	return authConfig{
		token:         strings.TrimSpace(os.Getenv("LAGO_MCP_TOKEN")),
		allowInsecure: truthy(os.Getenv("LAGO_MCP_ALLOW_INSECURE")),
	}
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func (c authConfig) mode() authMode {
	if c.token != "" {
		return authToken
	}
	if c.allowInsecure {
		return authInsecure
	}
	return authClosed
}

// bearerToken extracts the token from an Authorization header, or "".
func bearerToken(h string) string {
	if len(h) < 7 || !strings.EqualFold(h[:7], "Bearer ") {
		return ""
	}
	return strings.TrimSpace(h[7:])
}

// authorize decides whether a request may reach the tool surface.
func (c authConfig) authorize(r *http.Request) authOutcome {
	presented := bearerToken(r.Header.Get("Authorization"))

	switch c.mode() {
	case authClosed:
		return authOutcome{
			status: http.StatusServiceUnavailable,
			message: "lago MCP is not configured for authenticated access: LAGO_MCP_TOKEN is " +
				"unset. Refusing to serve billing tools, which would expose every customer's " +
				"billing data and the write path that moves money. Set LAGO_MCP_TOKEN, or set " +
				"LAGO_MCP_ALLOW_INSECURE=true for local development only.",
		}
	case authInsecure:
		return authOutcome{ok: true}
	}

	if presented == "" {
		return authOutcome{status: http.StatusUnauthorized, message: "missing bearer token"}
	}
	// Constant-time, over digests so the comparison does not leak the token's
	// length either.
	want := sha256.Sum256([]byte(c.token))
	got := sha256.Sum256([]byte(presented))
	if hmac.Equal(want[:], got[:]) {
		return authOutcome{ok: true}
	}
	return authOutcome{status: http.StatusForbidden, message: "bearer token not accepted"}
}
