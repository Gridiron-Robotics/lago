package mcp

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// recordExporter is an in-memory sdklog.Exporter that captures what WOULD ship to
// OpenObserve, so a test can assert the shipped record without a live collector.
type recordExporter struct {
	mu      sync.Mutex
	records []sdklog.Record
}

func (e *recordExporter) Export(_ context.Context, records []sdklog.Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range records {
		e.records = append(e.records, records[i].Clone())
	}
	return nil
}

func (e *recordExporter) Shutdown(context.Context) error   { return nil }
func (e *recordExporter) ForceFlush(context.Context) error { return nil }

func (e *recordExporter) all() []sdklog.Record {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]sdklog.Record(nil), e.records...)
}

// resourceServiceName reads service.name off a shipped record's resource — the
// value the langgraph self-heal loop keys the incident module on.
func resourceServiceName(rec sdklog.Record) string {
	res := rec.Resource()
	if res == nil {
		return ""
	}
	for _, kv := range res.Attributes() {
		if string(kv.Key) == "service.name" {
			return kv.Value.AsString()
		}
	}
	return ""
}

// An error record must ship over OTLP carrying service.name="lago". That string
// IS the incident module; a record tagged with anything else lands in a stream
// nothing watches, so the surface looks instrumented while paging nobody.
func TestErrorShipsWithServiceNameLago(t *testing.T) {
	rec := &recordExporter{}
	e, shutdown := newEmitter(context.Background(), emitterOptions{
		forceEnable: true,
		exporter:    rec,
		synchronous: true, // SimpleProcessor: deterministic, no flush race
	})
	if shutdown != nil {
		defer func() { _ = shutdown(context.Background()) }()
	}

	var buf bytes.Buffer
	r := &Reporter{out: &buf, service: selfHealService, now: time.Now, emitter: e}
	r.Error("lago unreachable", map[string]any{"tool": "lago_get_customer"})

	// The stderr JSON line is the write-ahead copy and must always be there.
	if got := decodeRecords(t, &buf); len(got) != 1 || got[0]["level"] != "error" {
		t.Fatalf("stderr write-ahead line missing or wrong: %+v", got)
	}

	shipped := rec.all()
	if len(shipped) != 1 {
		t.Fatalf("expected exactly 1 shipped record, got %d", len(shipped))
	}
	if got := resourceServiceName(shipped[0]); got != "lago" {
		t.Errorf("service.name = %q, want lago", got)
	}
	if got := shipped[0].SeverityText(); got != "ERROR" {
		t.Errorf("severity text = %q, want ERROR (OpenObserve derives level from it)", got)
	}
	if got := shipped[0].Body().AsString(); got != "lago unreachable" {
		t.Errorf("body = %q, want the error message", got)
	}
}

// OTEL_SERVICE_NAME may override the stream name, but it defaults to "lago" so a
// deployment that forgets to set it still reports into the right module.
func TestServiceNameDefaultsToLago(t *testing.T) {
	rec := &recordExporter{}
	e, shutdown := newEmitter(context.Background(), emitterOptions{
		forceEnable: true,
		exporter:    rec,
		synchronous: true,
		getenv:      func(string) string { return "" }, // nothing set, incl. OTEL_SERVICE_NAME
	})
	if shutdown != nil {
		defer func() { _ = shutdown(context.Background()) }()
	}
	r := &Reporter{out: &bytes.Buffer{}, service: selfHealService, now: time.Now, emitter: e}
	r.Error("boom", nil)

	shipped := rec.all()
	if len(shipped) == 0 {
		t.Fatal("expected a shipped record")
	}
	if got := resourceServiceName(shipped[len(shipped)-1]); got != "lago" {
		t.Errorf("default service.name = %q, want lago", got)
	}
}

// With the OTEL env unset the rail is a no-op that still writes stderr: telemetry
// must never be a boot dependency for the billing tool surface.
func TestDisabledWhenEnvUnset(t *testing.T) {
	e, shutdown := newEmitter(context.Background(), emitterOptions{
		getenv: func(string) string { return "" },
	})
	if _, isNop := e.(nopEmitter); !isNop {
		t.Fatalf("emitter = %T, want NopEmitter when env unset", e)
	}
	if shutdown != nil {
		t.Error("shutdown should be nil for the disabled rail")
	}

	var buf bytes.Buffer
	r := &Reporter{out: &buf, service: selfHealService, now: time.Now, emitter: e}
	r.Error("still logged locally", nil)
	if got := decodeRecords(t, &buf); len(got) != 1 || got[0]["level"] != "error" {
		t.Fatalf("stderr line must still be written when shipping is off: %+v", got)
	}
}

// Only level=error ships. Shipping warn/info would drown the rail the alert reads.
func TestOnlyErrorShips(t *testing.T) {
	rec := &recordExporter{}
	e, shutdown := newEmitter(context.Background(), emitterOptions{
		forceEnable: true, exporter: rec, synchronous: true,
	})
	if shutdown != nil {
		defer func() { _ = shutdown(context.Background()) }()
	}
	r := &Reporter{out: &bytes.Buffer{}, service: selfHealService, now: time.Now, emitter: e}

	r.Warn("caller sent a bad decimal", nil)
	r.Info("started", nil)
	if n := len(rec.all()); n != 0 {
		t.Fatalf("warn/info must not ship, got %d shipped", n)
	}
	r.Error("upstream 500", nil)
	if n := len(rec.all()); n != 1 {
		t.Fatalf("error must ship exactly once, got %d", n)
	}
}

// parseHeaders must split on the FIRST '=' so a base64 Authorization value's '='
// padding survives — the exact bug that otherwise reads as a 401 permissions
// problem.
func TestParseHeadersKeepsBase64Padding(t *testing.T) {
	got := parseHeaders("Authorization=Basic YWxpY2U6cGFzcw==,stream-name=default")
	if got["Authorization"] != "Basic YWxpY2U6cGFzcw==" {
		t.Errorf("Authorization = %q, padding truncated", got["Authorization"])
	}
	if got["stream-name"] != "default" {
		t.Errorf("stream-name = %q", got["stream-name"])
	}
}
