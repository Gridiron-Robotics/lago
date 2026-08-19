package mcp

// The OTLP/HTTP half of the OpenObserve self-heal rail.
//
// selfheal.go writes every record to stderr as a JSON line (the write-ahead
// copy). This file is what actually SHIPS the level=error records to OpenObserve
// over OTLP/HTTP, so an alert can fire and drive the langgraph diagnose → fix →
// PR loop. Without a shipper the stderr line lands nowhere the estate watches.
//
// Ported from ach-payments' internal/observability/otel.go (the estate's
// established Go self-heal shipper), which bigcapital-enterprise also uses.
// Deliberately kept behaviour-identical so the whole estate ships error logs the
// same way.
//
// Two properties this file preserves on purpose:
//
//   - service.name is "lago" (selfHealService). That string IS the incident
//     module the langgraph loop keys on. A record tagged with anything else
//     lands somewhere nothing watches.
//   - Graceful by construction: with the env unset every send is a no-op, and an
//     exporter that fails to build degrades to stderr-only. Telemetry must never
//     be a boot dependency for a billing tool surface — an OpenObserve outage
//     must not take the surface down.
//
// Note: adding this makes the package no longer stdlib-only. That property was a
// nice-to-have, not a gate invariant (mcp-gate.sh does not assert zero deps); the
// estate direction is OTLP self-heal everywhere, so shipping errors wins.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Emitter ships ERROR-severity records to the self-heal rail. It never blocks the
// caller's work and never panics; a telemetry failure must not become a billing
// failure.
type Emitter interface {
	Error(ctx context.Context, msg string, fields map[string]any)
}

// nopEmitter is what callers get when the rail is disabled.
type nopEmitter struct{}

func (nopEmitter) Error(context.Context, string, map[string]any) {}

// NopEmitter returns an Emitter that discards everything. Used when OTLP is not
// configured and in tests.
func NopEmitter() Emitter { return nopEmitter{} }

// otlpEmitter is the initialized OTLP pipeline plus its shutdown hook.
type otlpEmitter struct {
	provider *sdklog.LoggerProvider
	logger   otellog.Logger
}

// enabledFromEnv reports whether the rail should initialize: the master switch,
// or an endpoint simply being configured.
func enabledFromEnv(getenv func(string) string) bool {
	if truthy(getenv("OTEL_ENABLED")) {
		return true
	}
	return strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_ENDPOINT")) != ""
}

// parseHeaders parses the comma-separated `key=value` list from the OTLP env
// contract.
//
// Each pair splits on the FIRST "=" only. Splitting on every "=" is the classic
// bug here: a base64 Authorization value ends in "=" padding, and naive splitting
// truncates the credential into an invalid one, producing a 401 that reads like a
// permissions problem rather than a parsing mistake.
func parseHeaders(raw string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, v, found := strings.Cut(pair, "=") // Cut splits on the first "=" only
		if !found {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if k == "" {
			continue
		}
		out[k] = v
	}
	return out
}

// emitterOptions configures newEmitter. The zero value reads everything from the
// environment.
type emitterOptions struct {
	// forceEnable builds the pipeline even with the env unset — the test hook.
	forceEnable bool
	// exporter injects an in-memory exporter so a test can assert what would ship.
	exporter sdklog.Exporter
	// synchronous uses a simple processor for a deterministic flush in tests.
	synchronous bool
	// getenv is injectable so tests need not mutate the process environment.
	getenv func(string) string
}

// newEmitter wires the OTLP log pipeline. Safe to call unconditionally; returns a
// NopEmitter (and a nil shutdown) when the env is unset or the exporter cannot be
// built. The returned shutdown flushes and stops the pipeline; it is nil for the
// disabled path.
func newEmitter(ctx context.Context, opts emitterOptions) (Emitter, func(context.Context) error) {
	getenv := opts.getenv
	if getenv == nil {
		getenv = os.Getenv
	}

	if !opts.forceEnable && opts.exporter == nil && !enabledFromEnv(getenv) {
		return NopEmitter(), nil
	}

	exporter := opts.exporter
	if exporter == nil {
		exporterOpts := []otlploghttp.Option{}
		if hdrs := parseHeaders(getenv("OTEL_EXPORTER_OTLP_HEADERS")); len(hdrs) > 0 {
			exporterOpts = append(exporterOpts, otlploghttp.WithHeaders(hdrs))
		}
		if host, path, insecure := splitEndpoint(strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))); host != "" {
			exporterOpts = append(exporterOpts, otlploghttp.WithEndpoint(host))
			if path != "" {
				exporterOpts = append(exporterOpts, otlploghttp.WithURLPath(path+"/logs"))
			}
			if insecure {
				exporterOpts = append(exporterOpts, otlploghttp.WithInsecure())
			}
		}
		var err error
		exporter, err = otlploghttp.New(ctx, exporterOpts...)
		if err != nil {
			// Degrade rather than fail: a telemetry outage must not stop the
			// billing tool surface from serving.
			return NopEmitter(), nil
		}
	}

	var processor sdklog.Processor
	if opts.synchronous {
		processor = sdklog.NewSimpleProcessor(exporter)
	} else {
		processor = sdklog.NewBatchProcessor(exporter)
	}

	serviceName := strings.TrimSpace(getenv("OTEL_SERVICE_NAME"))
	if serviceName == "" {
		serviceName = selfHealService
	}
	res, err := resource.Merge(resource.Default(),
		resource.NewSchemaless(semconv.ServiceName(serviceName)))
	if err != nil {
		return NopEmitter(), nil
	}

	provider := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(processor),
	)
	global.SetLoggerProvider(provider)

	e := &otlpEmitter{provider: provider, logger: provider.Logger(serviceName)}
	return e, provider.Shutdown
}

// splitEndpoint turns "http://host:5080/api/org/v1" into
// ("host:5080", "/api/org/v1", true).
func splitEndpoint(endpoint string) (host, path string, insecure bool) {
	if endpoint == "" {
		return "", "", false
	}
	insecure = strings.HasPrefix(endpoint, "http://")
	trimmed := strings.TrimPrefix(strings.TrimPrefix(endpoint, "http://"), "https://")
	trimmed = strings.TrimSuffix(trimmed, "/")
	host, rest, found := strings.Cut(trimmed, "/")
	if !found || rest == "" {
		return host, "", insecure
	}
	return host, "/" + rest, insecure
}

// Error emits one ERROR-severity record to the module's OpenObserve stream.
//
// SeverityText is set explicitly and is NOT optional: OpenObserve derives the
// `level` field the alert rule matches on from the text, so a record carrying
// only a numeric severity satisfies every OTel assertion and is invisible to the
// alert.
func (e *otlpEmitter) Error(ctx context.Context, msg string, fields map[string]any) {
	if e == nil || e.logger == nil {
		return
	}
	var rec otellog.Record
	now := time.Now()
	rec.SetTimestamp(now)
	rec.SetObservedTimestamp(now)
	rec.SetSeverity(otellog.SeverityError)
	rec.SetSeverityText("ERROR")
	rec.SetBody(otellog.StringValue(msg))
	for k, v := range fields {
		rec.AddAttributes(otellog.String(k, emitString(v)))
	}
	e.logger.Emit(ctx, rec)
}

// emitString renders an arbitrary self-heal field value as a string attribute.
// The fields are small scalars (ids, tool names, durations, booleans); a flat
// string rendering keeps the shipped record aligned with the stderr JSON copy.
func emitString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", t)
	}
}
