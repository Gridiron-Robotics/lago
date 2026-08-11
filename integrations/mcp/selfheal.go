package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// The OpenObserve self-heal rail for the Lago MCP surface.
//
// The estate alert fires on any record with `level=error` in the module's stream,
// which drives the langgraph diagnose → fix → PR loop. Two things make that
// non-trivial here, and both are why this file exists rather than a bare
// log.Printf:
//
//  1. `service` must be the stream name (`lago`), because the stream IS the
//     incident `module`. A record tagged with anything else lands somewhere
//     nothing watches, and the module looks instrumented while firing nothing.
//
//  2. Contract A turns tool failures into structured non-2xx JSON responses. There
//     is no unhandled panic or 5xx for a rail to hang off, so the rail has to be
//     fired from the handler path explicitly — otherwise the entire billing tool
//     surface can be failing every call and the alert sees an empty result set.
//
// Records go to stderr as one JSON line each (the write-ahead copy), AND, when
// OTLP is configured, ship to OpenObserve over OTLP/HTTP via observability.go so
// an alert can actually fire. The stderr line is written first and the OTLP send
// is best-effort, so a collector outage can never lose or block a record.

// selfHealService is the OpenObserve stream / incident module for this surface.
const selfHealService = "lago"

// emitter is the process-wide OTLP shipper attached to reporters built with
// NewReporter. It defaults to a no-op and is replaced by InitSelfHeal(...) once
// the OTLP env is read at startup. A NewReporter constructed before InitSelfHeal
// (e.g. in tests) simply gets the no-op and stays stderr-only.
var emitter Emitter = NopEmitter()

// InitSelfHeal builds the OTLP shipper from the OTEL_* environment and installs
// it as the process-wide emitter. Call it once at startup, before HTTPHandler
// builds its reporter. Returns a shutdown hook (nil when the rail is disabled)
// that flushes pending records; defer it so a clean exit does not drop the last
// batch.
func InitSelfHeal(ctx context.Context) func(context.Context) error {
	e, shutdown := newEmitter(ctx, emitterOptions{})
	emitter = e
	return shutdown
}

// Reporter emits structured records on the self-heal rail.
type Reporter struct {
	mu      sync.Mutex
	out     io.Writer
	service string
	now     func() time.Time
	emitter Emitter
}

// NewReporter builds a reporter writing to w (nil = os.Stderr), shipping level=
// error records through the process-wide OTLP emitter installed by InitSelfHeal.
func NewReporter(w io.Writer) *Reporter {
	if w == nil {
		w = os.Stderr
	}
	return &Reporter{out: w, service: selfHealService, now: time.Now, emitter: emitter}
}

func (r *Reporter) emit(level, message string, fields map[string]any) map[string]any {
	rec := map[string]any{
		"level":   level,
		"service": r.service,
		"ts":      r.now().UTC().Format(time.RFC3339Nano),
		"message": message,
	}
	for k, v := range fields {
		if _, taken := rec[k]; !taken {
			rec[k] = v
		}
	}
	line, err := json.Marshal(rec)
	if err != nil {
		// A record that cannot be encoded must still be visible, or a bad field
		// value silently swallows the incident it was reporting.
		line = []byte(fmt.Sprintf(
			`{"level":%q,"service":%q,"message":"unencodable record: %s"}`,
			level, r.service, strings.ReplaceAll(err.Error(), `"`, "'")))
	}
	r.mu.Lock()
	_, _ = r.out.Write(append(line, '\n'))
	r.mu.Unlock()

	// Ship only level=error to OpenObserve: that is the field the estate alert
	// matches on, and shipping warn/info would drown the rail. Best-effort and
	// after the stderr write, so the JSON line stays the write-ahead copy and a
	// collector outage never loses or blocks a record.
	if level == "error" && r.emitter != nil {
		r.emitter.Error(context.Background(), message, fields)
	}
	return rec
}

// Error emits a level=error record — the estate self-heal trigger.
func (r *Reporter) Error(message string, fields map[string]any) map[string]any {
	return r.emit("error", message, fields)
}

// Warn emits a level=warn record. Caller mistakes go here, NOT to Error: a bad
// argument from an agent is the agent's to fix, and raising a self-heal incident
// for every malformed tool call buries the real faults.
func (r *Reporter) Warn(message string, fields map[string]any) map[string]any {
	return r.emit("warn", message, fields)
}

// Info emits a level=info record.
func (r *Reporter) Info(message string, fields map[string]any) map[string]any {
	return r.emit("info", message, fields)
}

// isCallerError reports whether an error is the caller's fault (bad or missing
// arguments, a refused mutation) rather than an operational fault worth waking
// the self-heal loop for.
//
// The distinction is drawn on messages this package produces itself — a Lago HTTP
// failure, a timeout, or an unconfigured client is operational and must reach the
// rail.
func isCallerError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, marker := range []string{
		"missing required argument",
		"must be a non-empty string",
		"must be an object",
		"must be a plain non-negative decimal",
		"must be 'calendar' or 'anniversary'",
		"is required",
		"refused:",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}
