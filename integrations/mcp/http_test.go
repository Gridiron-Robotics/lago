package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newHTTPStack builds the Contract A handler backed by a stub Lago backend so
// tool dispatch can be exercised without a real Lago instance.
func newHTTPStack(t *testing.T) (http.Handler, *httptest.Server) {
	t.Helper()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("stub Lago saw %s, want GET (read-only invariant)", r.Method)
		}
		_, _ = w.Write([]byte(`{"customer":{"external_id":"cust_1"}}`))
	}))
	srv := NewServer("lago", "0.1.0", ReadOnlyTools(NewLagoClient(backend.URL, "k", backend.Client())))
	return srv.HTTPHandler(), backend
}

func bearer(r *http.Request) *http.Request {
	r.Header.Set("Authorization", "Bearer test-jwt")
	return r
}

func TestHTTP_ListTools(t *testing.T) {
	h, backend := newHTTPStack(t)
	defer backend.Close()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, bearer(httptest.NewRequest(http.MethodGet, "/tools?server=lago", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /tools status = %d, want 200", rec.Code)
	}
	var body struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"input_schema"`
			Annotations struct {
				DestructiveHint bool `json:"destructiveHint"`
			} `json:"annotations"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Tools) != 6 {
		t.Fatalf("got %d tools, want 6", len(body.Tools))
	}
	for _, tool := range body.Tools {
		if tool.Name == "" {
			t.Fatalf("tool with empty name: %+v", tool)
		}
		if tool.InputSchema == nil || tool.InputSchema["type"] != "object" {
			t.Fatalf("tool %q missing object input_schema: %v", tool.Name, tool.InputSchema)
		}
		if tool.Annotations.DestructiveHint {
			t.Fatalf("tool %q must be non-destructive (read-only invariant)", tool.Name)
		}
	}
}

func TestHTTP_ListTools_UnknownServerEmpty(t *testing.T) {
	h, backend := newHTTPStack(t)
	defer backend.Close()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, bearer(httptest.NewRequest(http.MethodGet, "/tools?server=nope", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		Tools []any `json:"tools"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Tools) != 0 {
		t.Fatalf("unknown server should yield empty tools, got %d", len(body.Tools))
	}
}

func TestHTTP_ListTools_RequiresBearer(t *testing.T) {
	h, backend := newHTTPStack(t)
	defer backend.Close()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/tools?server=lago", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing bearer status = %d, want 401", rec.Code)
	}
}

func TestHTTP_Invoke_Dispatches(t *testing.T) {
	h, backend := newHTTPStack(t)
	defer backend.Close()

	rec := httptest.NewRecorder()
	req := bearer(httptest.NewRequest(http.MethodPost, "/invoke",
		strings.NewReader(`{"server":"lago","tool":"lago_get_customer","arguments":{"external_id":"cust_1"}}`)))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /invoke status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Tool   string          `json:"tool"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Tool != "lago_get_customer" {
		t.Fatalf("tool = %q, want lago_get_customer", body.Tool)
	}
	if !strings.Contains(string(body.Result), "cust_1") {
		t.Fatalf("result missing upstream payload: %s", body.Result)
	}
}

func TestHTTP_Invoke_UnknownTool404(t *testing.T) {
	h, backend := newHTTPStack(t)
	defer backend.Close()

	rec := httptest.NewRecorder()
	req := bearer(httptest.NewRequest(http.MethodPost, "/invoke",
		strings.NewReader(`{"server":"lago","tool":"lago_delete_everything","arguments":{}}`)))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown tool status = %d, want 404", rec.Code)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error == "" {
		t.Fatalf("expected JSON error body, got %s", rec.Body.String())
	}
}

func TestHTTP_Invoke_UnknownServer404(t *testing.T) {
	h, backend := newHTTPStack(t)
	defer backend.Close()

	rec := httptest.NewRecorder()
	req := bearer(httptest.NewRequest(http.MethodPost, "/invoke",
		strings.NewReader(`{"server":"billing","tool":"lago_get_customer","arguments":{"external_id":"x"}}`)))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown server status = %d, want 404", rec.Code)
	}
}

func TestHTTP_Invoke_BadBody400(t *testing.T) {
	h, backend := newHTTPStack(t)
	defer backend.Close()

	rec := httptest.NewRecorder()
	req := bearer(httptest.NewRequest(http.MethodPost, "/invoke", strings.NewReader(`{not json`)))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body status = %d, want 400", rec.Code)
	}
}

func TestHTTP_Invoke_MissingRequiredArg(t *testing.T) {
	h, backend := newHTTPStack(t)
	defer backend.Close()

	rec := httptest.NewRecorder()
	req := bearer(httptest.NewRequest(http.MethodPost, "/invoke",
		strings.NewReader(`{"tool":"lago_get_customer","arguments":{}}`)))
	h.ServeHTTP(rec, req)
	// Handler-level validation error surfaces as a structured non-2xx error.
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing required arg status = %d, want 400", rec.Code)
	}
}

func TestHTTP_Invoke_IdempotentReplay(t *testing.T) {
	h, backend := newHTTPStack(t)
	defer backend.Close()

	do := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := bearer(httptest.NewRequest(http.MethodPost, "/invoke",
			strings.NewReader(`{"server":"lago","tool":"lago_get_customer","arguments":{"external_id":"cust_1"}}`)))
		req.Header.Set("Idempotency-Key", "abc-123")
		req.Header.Set("X-Tenant-Id", "acme")
		h.ServeHTTP(rec, req)
		return rec
	}
	first := do()
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200", first.Code)
	}
	second := do()
	var body map[string]any
	if err := json.Unmarshal(second.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["replayed"] != true {
		t.Fatalf("second call should be flagged replayed; got %v", body)
	}
}

func TestHTTP_HealthHead(t *testing.T) {
	h, backend := newHTTPStack(t)
	defer backend.Close()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD / status = %d, want 200", rec.Code)
	}
}
