package mcp

import (
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
// Deliberately stdlib-only, matching the rest of this package: no OTel SDK, no
// CGO, so `go test ./...` and the MCP gate still run anywhere. Records go to
// stderr as one JSON line each, which is what the estate log shipper reads.

// selfHealService is the OpenObserve stream / incident module for this surface.
const selfHealService = "lago"

// Reporter emits structured records on the self-heal rail.
type Reporter struct {
	mu      sync.Mutex
	out     io.Writer
	service string
	now     func() time.Time
}

// NewReporter builds a reporter writing to w (nil = os.Stderr).
func NewReporter(w io.Writer) *Reporter {
	if w == nil {
		w = os.Stderr
	}
	return &Reporter{out: w, service: selfHealService, now: time.Now}
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
	defer r.mu.Unlock()
	_, _ = r.out.Write(append(line, '\n'))
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
