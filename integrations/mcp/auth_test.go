package mcp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Boundary authentication.
//
// What was here before this file: a check that the Authorization header started
// with "Bearer " and had something after it. Any string passed. The surface was
// effectively unauthenticated to anything that could reach the port — while
// fronting LAGO_API_KEY, which reads every customer's billing data and (now)
// drives the write path. "A bearer is required" was true and meaningless.

func stack(t *testing.T, cfg authConfig) http.Handler {
	t.Helper()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"customer":{}}`))
	}))
	t.Cleanup(backend.Close)
	srv := NewServer("lago", "0.1.0", ReadOnlyTools(NewLagoClient(backend.URL, "k", backend.Client())))
	return srv.httpHandlerWithAuth(cfg)
}

func get(h http.Handler, path, authHeader string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func post(h http.Handler, body, authHeader string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/invoke", strings.NewReader(body))
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// --------------------------------------------------------------------------- //
// Fail-closed
// --------------------------------------------------------------------------- //
func TestUnconfiguredSurfaceRefusesToServe(t *testing.T) {
	// The default posture. An operator who forgets LAGO_MCP_TOKEN gets an outage,
	// which is loud, instead of an open door onto billing, which is not.
	h := stack(t, authConfig{})

	if rec := get(h, "/tools", "Bearer anything"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("/tools status = %d, want 503", rec.Code)
	}
	rec := post(h, `{"tool":"lago_get_customer","arguments":{"external_id":"a"}}`, "Bearer anything")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("/invoke status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "LAGO_MCP_TOKEN") {
		t.Errorf("the 503 must name the variable to set: %s", rec.Body.String())
	}
}

func TestFailClosedIsTheDefaultMode(t *testing.T) {
	if got := (authConfig{}).mode(); got != authClosed {
		t.Fatalf("empty config mode = %q, want %q", got, authClosed)
	}
}

func TestHealthProbeStaysOpenWhenFailClosed(t *testing.T) {
	// A gateway must be able to see the process is alive without a credential,
	// and HEAD / leaks nothing.
	h := stack(t, authConfig{})
	req := httptest.NewRequest(http.MethodHead, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD / status = %d, want 200", rec.Code)
	}
}

// --------------------------------------------------------------------------- //
// Configured token
// --------------------------------------------------------------------------- //
func TestAnArbitraryBearerIsNoLongerAccepted(t *testing.T) {
	// The regression this file exists for: before, "Bearer whatever" was a valid
	// credential because only the header's SHAPE was checked.
	h := stack(t, authConfig{token: "the-real-token"})

	if rec := get(h, "/tools", "Bearer whatever"); rec.Code != http.StatusForbidden {
		t.Errorf("a wrong token got %d, want 403", rec.Code)
	}
	if rec := get(h, "/tools", "Bearer the-real-token"); rec.Code != http.StatusOK {
		t.Errorf("the correct token got %d, want 200", rec.Code)
	}
}

func TestMissingAndWrongCredentialsAreDistinguished(t *testing.T) {
	// 401 vs 403 is the difference between "you did not authenticate" and "you
	// did, and you are not allowed" — an operator debugging a gateway needs it.
	h := stack(t, authConfig{token: "tok"})
	if rec := get(h, "/tools", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no header got %d, want 401", rec.Code)
	}
	if rec := get(h, "/tools", "Bearer "); rec.Code != http.StatusUnauthorized {
		t.Errorf("empty token got %d, want 401", rec.Code)
	}
	if rec := get(h, "/tools", "Bearer nope"); rec.Code != http.StatusForbidden {
		t.Errorf("wrong token got %d, want 403", rec.Code)
	}
}

func TestANonBearerSchemeIsNotACredential(t *testing.T) {
	h := stack(t, authConfig{token: "tok"})
	for _, header := range []string{"Basic dG9rOg==", "tok", "Token tok", "bearer"} {
		if rec := get(h, "/tools", header); rec.Code == http.StatusOK {
			t.Errorf("header %q was accepted", header)
		}
	}
}

func TestBearerSchemeIsCaseInsensitive(t *testing.T) {
	// RFC 7235 makes the scheme case-insensitive; rejecting "bearer x" would break
	// a conforming client for no security benefit.
	h := stack(t, authConfig{token: "tok"})
	if rec := get(h, "/tools", "bearer tok"); rec.Code != http.StatusOK {
		t.Errorf("lowercase scheme got %d, want 200", rec.Code)
	}
}

func TestTokenComparisonIgnoresSurroundingWhitespace(t *testing.T) {
	h := stack(t, authConfig{token: "tok"})
	if rec := get(h, "/tools", "Bearer   tok  "); rec.Code != http.StatusOK {
		t.Errorf("padded token got %d, want 200", rec.Code)
	}
}

// --------------------------------------------------------------------------- //
// The explicit escape hatch
// --------------------------------------------------------------------------- //
func TestInsecureModeMustBeTypedNotInherited(t *testing.T) {
	// Insecure has to be asked for. Nobody should arrive at an open billing
	// surface by omitting a variable.
	if got := (authConfig{allowInsecure: true}).mode(); got != authInsecure {
		t.Fatalf("mode = %q, want %q", got, authInsecure)
	}
	h := stack(t, authConfig{allowInsecure: true})
	if rec := get(h, "/tools", ""); rec.Code != http.StatusOK {
		t.Errorf("insecure mode with no header got %d, want 200", rec.Code)
	}
}

func TestAConfiguredTokenBeatsTheInsecureFlag(t *testing.T) {
	// If both are set, the token wins: a leftover LAGO_MCP_ALLOW_INSECURE in a
	// production environment must not silently disable a configured credential.
	cfg := authConfig{token: "tok", allowInsecure: true}
	if got := cfg.mode(); got != authToken {
		t.Fatalf("mode = %q, want %q", got, authToken)
	}
	h := stack(t, cfg)
	if rec := get(h, "/tools", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("a configured token must still be required, got %d", rec.Code)
	}
}

func TestTruthyAcceptsTheUsualSpellingsAndNothingElse(t *testing.T) {
	for _, yes := range []string{"1", "true", "TRUE", "yes", "on", " true "} {
		if !truthy(yes) {
			t.Errorf("truthy(%q) should be true", yes)
		}
	}
	for _, no := range []string{"", "0", "false", "no", "off", "maybe", "2"} {
		if truthy(no) {
			t.Errorf("truthy(%q) should be false", no)
		}
	}
}
