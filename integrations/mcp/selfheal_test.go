package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The OpenObserve self-heal rail.
//
// The trap this file exists for: Contract A converts every tool failure into a
// structured non-2xx JSON response. There is no unhandled panic and no 5xx for an
// alert to key off — so a rail hung off "the process crashed" reports NOTHING
// while the entire billing tool surface fails every call. The rail has to fire
// from the handler path, and these tests are what keep it there.

func decodeRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("record is not JSON: %q (%v)", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// --------------------------------------------------------------------------- //
// Record shape
// --------------------------------------------------------------------------- //
func TestErrorRecordCarriesLevelError(t *testing.T) {
	// `level` is the field the estate alert rule matches. Anything else and the
	// module is instrumented but unalertable.
	var buf bytes.Buffer
	NewReporter(&buf).Error("lago unreachable", map[string]any{"tool": "lago_get_customer"})

	recs := decodeRecords(t, &buf)
	if len(recs) != 1 {
		t.Fatalf("expected 1 record, got %d", len(recs))
	}
	if recs[0]["level"] != "error" {
		t.Errorf("level = %v, want error", recs[0]["level"])
	}
	if recs[0]["message"] != "lago unreachable" {
		t.Errorf("message = %v", recs[0]["message"])
	}
	if recs[0]["tool"] != "lago_get_customer" {
		t.Errorf("field not carried through: %+v", recs[0])
	}
}

func TestServiceIsTheStreamName(t *testing.T) {
	// service == the OpenObserve stream == the incident `module`. A wrong value
	// routes the record to a stream nothing watches.
	var buf bytes.Buffer
	NewReporter(&buf).Error("x", nil)
	if got := decodeRecords(t, &buf)[0]["service"]; got != "lago" {
		t.Fatalf("service = %v, want lago", got)
	}
}

func TestRecordCarriesAnRFC3339Timestamp(t *testing.T) {
	var buf bytes.Buffer
	NewReporter(&buf).Error("x", nil)
	ts, _ := decodeRecords(t, &buf)[0]["ts"].(string)
	if _, err := time.Parse(time.RFC3339Nano, ts); err != nil {
		t.Fatalf("ts %q is not RFC3339: %v", ts, err)
	}
}

func TestReservedFieldsCannotBeOverwrittenByCallerFields(t *testing.T) {
	// A field named "level" in the caller's map must not be able to downgrade an
	// error record to info — that would be a silent way to disable the alert.
	var buf bytes.Buffer
	NewReporter(&buf).Error("boom", map[string]any{
		"level": "info", "service": "somewhere-else", "message": "nothing to see",
	})
	rec := decodeRecords(t, &buf)[0]
	if rec["level"] != "error" {
		t.Errorf("level was overwritten to %v", rec["level"])
	}
	if rec["service"] != "lago" {
		t.Errorf("service was overwritten to %v", rec["service"])
	}
	if rec["message"] != "boom" {
		t.Errorf("message was overwritten to %v", rec["message"])
	}
}

func TestAnUnencodableFieldStillProducesAVisibleRecord(t *testing.T) {
	// A record that cannot be marshalled must not vanish — that would swallow the
	// very incident it was reporting.
	var buf bytes.Buffer
	NewReporter(&buf).Error("boom", map[string]any{"bad": func() {}})
	out := buf.String()
	if out == "" {
		t.Fatal("nothing was written")
	}
	if !strings.Contains(out, `"level":"error"`) {
		t.Errorf("the fallback record lost its level: %s", out)
	}
}

func TestWarnAndInfoAreDistinctLevels(t *testing.T) {
	var buf bytes.Buffer
	r := NewReporter(&buf)
	r.Warn("caller mistake", nil)
	r.Info("started", nil)
	recs := decodeRecords(t, &buf)
	if recs[0]["level"] != "warn" || recs[1]["level"] != "info" {
		t.Fatalf("levels wrong: %v, %v", recs[0]["level"], recs[1]["level"])
	}
}

// --------------------------------------------------------------------------- //
// Caller error vs operational fault
// --------------------------------------------------------------------------- //
func TestCallerMistakesAreClassifiedAsSuch(t *testing.T) {
	// These are the agent's to fix. Logging them at error level would raise a
	// self-heal incident for every malformed tool call and bury the real faults.
	callerErrors := []error{
		errors.New(`missing required argument "transaction_id"`),
		errors.New(`argument "code" must be a non-empty string`),
		errors.New(`argument "properties" must be an object`),
		errors.New(`paid_credits must be a plain non-negative decimal string like "100.0", got "-5"`),
		errors.New(`billing_time must be 'calendar' or 'anniversary', got "monthly"`),
		errors.New("external_subscription_id is required"),
		errors.New("refused: DELETE /api/v1/customers/acme is not an allowed Lago mutation"),
	}
	for _, err := range callerErrors {
		if !isCallerError(err) {
			t.Errorf("should be a caller error: %v", err)
		}
	}
}

func TestOperationalFaultsAreNotClassifiedAsCallerErrors(t *testing.T) {
	// These must reach the rail: they are ours, and nobody is watching a console.
	operational := []error{
		errors.New("lago POST /api/v1/events -> HTTP 500: internal server error"),
		errors.New("lago request: dial tcp: connection refused"),
		errors.New("lago writer not configured (set LAGO_API_URL and LAGO_API_KEY)"),
		errors.New("context deadline exceeded"),
	}
	for _, err := range operational {
		if isCallerError(err) {
			t.Errorf("should NOT be a caller error: %v", err)
		}
	}
	if isCallerError(nil) {
		t.Error("nil is not a caller error")
	}
}

// --------------------------------------------------------------------------- //
// The rail fires from the handler path
// --------------------------------------------------------------------------- //
func railStack(t *testing.T, lagoStatus int, lagoBody string) (http.Handler, *bytes.Buffer) {
	t.Helper()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(lagoStatus)
		_, _ = w.Write([]byte(lagoBody))
	}))
	t.Cleanup(backend.Close)

	srv := NewServer("lago", "0.1.0", ReadOnlyTools(NewLagoClient(backend.URL, "k", backend.Client())))

	// Same adapter production builds, with the rail's writer redirected into a
	// buffer — so these tests cover the real wiring rather than a parallel
	// reporter that only exists in the test.
	var buf bytes.Buffer
	return withReporter(srv, authConfig{token: "tok"}, &buf), &buf
}

// withReporter builds the Contract A handler with a reporter writing to w.
func withReporter(s *Server, cfg authConfig, w *bytes.Buffer) http.Handler {
	a := &httpAdapter{
		srv:      s,
		auth:     cfg,
		reporter: NewReporter(w),
		replay:   make(map[[2]string]map[string]any),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/tools", a.handleTools)
	mux.HandleFunc("/invoke", a.handleInvoke)
	mux.HandleFunc("/", a.handleRoot)
	return mux
}

func invoke(h http.Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/invoke", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAToolFailureAgainstLagoReachesTheRail(t *testing.T) {
	h, buf := railStack(t, http.StatusInternalServerError, `{"error":"boom"}`)
	res := invoke(h, `{"tool":"lago_get_customer","arguments":{"external_id":"acme"}}`)

	// 502, not 400: the caller's request was fine, the upstream failed.
	if res.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", res.Code)
	}
	recs := decodeRecords(t, buf)
	if len(recs) == 0 {
		t.Fatal("a tool failure emitted no record — the rail is not wired")
	}
	last := recs[len(recs)-1]
	if last["level"] != "error" {
		t.Errorf("level = %v, want error", last["level"])
	}
	if last["tool"] != "lago_get_customer" {
		t.Errorf("tool not recorded: %+v", last)
	}
}

func TestACallerMistakeStaysAtWarn(t *testing.T) {
	h, buf := railStack(t, http.StatusOK, `{}`)
	res := invoke(h, `{"tool":"lago_get_customer","arguments":{}}`)

	if res.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", res.Code)
	}
	for _, rec := range decodeRecords(t, buf) {
		if rec["level"] == "error" {
			t.Errorf("a missing-argument mistake raised an ERROR record: %+v", rec)
		}
	}
}

func TestTheRecordSaysWhetherTheFailedToolWasDestructive(t *testing.T) {
	// An operator triaging an incident needs to know immediately whether a failed
	// call could have moved money.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer backend.Close()

	writer := NewLagoWriter(backend.URL, "k", backend.Client())
	srv := NewServer("lago", "0.1.0", WriteTools(writer))
	var buf bytes.Buffer
	h := withReporter(srv, authConfig{token: "tok"}, &buf)

	invoke(h, `{"tool":"lago_top_up_wallet","arguments":{"wallet_id":"w","paid_credits":"10"}}`)

	recs := decodeRecords(t, &buf)
	if len(recs) == 0 {
		t.Fatal("no record emitted")
	}
	last := recs[len(recs)-1]
	if last["destructive"] != true {
		t.Errorf("a failed write must be recorded as destructive: %+v", last)
	}
}

func TestAnUnknownToolIsReportedDestructiveByDefault(t *testing.T) {
	// Fail-safe: a tool missing from the registry must not be treated as harmless
	// because a lookup missed.
	srv := NewServer("lago", "0.1.0", nil)
	if !srv.isDestructive("something_that_does_not_exist") {
		t.Fatal("an unknown tool must default to destructive")
	}
}

func TestASuccessfulInvokeReportsWhetherItMutated(t *testing.T) {
	h, _ := railStack(t, http.StatusOK, `{"customer":{}}`)
	res := invoke(h, `{"tool":"lago_get_customer","arguments":{"external_id":"acme"}}`)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", res.Code, res.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["destructive"] != false {
		t.Errorf("a read must report destructive=false, got %v", body["destructive"])
	}
}
