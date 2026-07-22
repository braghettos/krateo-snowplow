// Package logging wires snowplow's OpenTelemetry Logs pipeline — the sink
// for the canonical, trace-correlated AuditEvent (D19a).
//
// COEXISTENCE CONTRACT (load-bearing): this package is ADDITIVE and is a
// SEPARATE signal from the existing stdout -> otel-daemonset filelog ->
// ClickHouse `otel_logs` per-call diagnostic log. That stdout pipeline is
// UNTOUCHED. The audit event is a FIRST-CLASS OTLP LogRecord emitted
// directly by the Logs SDK on the shared OTel Collector -> ClickHouse
// `otel_logs` plane, stamped with trace_id/span_id from the active span so
// it joins the traces/logs it caused (otel_logs.idx_trace_id).
//
// GATING: Setup mirrors tracing.Setup exactly. It is a no-op unless the
// pipeline resolves to enabled. Logs are gated by OTEL_LOGS_ENABLED, which
// DEFAULTS to the value of the OTEL_ENABLED master switch when unset — so
// OTEL_ENABLED=true turns audit-log emission on, and a per-signal
// OTEL_LOGS_ENABLED still overrides. When the gate is off, Setup registers
// NOTHING (no LoggerProvider, no exporter) and returns a nil provider, so
// the emitter degrades to a zero-cost no-op and the off-path is
// byte-identical to the pre-OTel binary.
//
//	OTEL_ENABLED                master switch                (default false)
//	OTEL_LOGS_ENABLED           gate audit logs (default: value of OTEL_ENABLED)
//	OTEL_EXPORTER_OTLP_ENDPOINT collector OTLP/HTTP endpoint
package logging

import (
	"context"

	"github.com/krateoplatformops/plumbing/env"

	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

const (
	// EnvEnabled is the master OTel switch (shared with tracing/metrics).
	EnvEnabled = "OTEL_ENABLED"

	// EnvLogsEnabled gates the audit-log pipeline. It DEFAULTS to the value
	// of EnvEnabled (OTEL_ENABLED) when unset; a per-signal value overrides
	// the master. Default (with the master unset) is false.
	EnvLogsEnabled = "OTEL_LOGS_ENABLED"

	// EnvOTLPEndpoint is the OTLP/HTTP collector endpoint, consumed via the
	// standard OTEL_EXPORTER_OTLP_ENDPOINT env contract.
	EnvOTLPEndpoint = "OTEL_EXPORTER_OTLP_ENDPOINT"

	// serviceName is the resource service.name reported on every audit
	// LogRecord — the otel_logs.ServiceName primary key. Must match the
	// tracing package's service.name so audit logs and spans agree.
	serviceName = "snowplow"
)

// ShutdownFunc flushes and stops the logs pipeline. Always non-nil so the
// caller can `defer shutdown(ctx)` unconditionally; it is a no-op when the
// pipeline was not enabled.
type ShutdownFunc func(context.Context) error

// logsEnabled resolves the logs gate from the canonical contract:
// OTEL_LOGS_ENABLED, defaulting to the value of the OTEL_ENABLED master when
// the per-signal var is unset.
func logsEnabled() bool {
	return env.Bool(EnvLogsEnabled, env.Bool(EnvEnabled, false))
}

// Enabled reports whether the audit-log pipeline resolves to enabled.
func Enabled() bool { return logsEnabled() }

// Setup wires an OTLP/HTTP log exporter and a batch LoggerProvider carrying
// a Resource with service.name=snowplow / service.version=build (identical
// to the TracerProvider's resource so logs and spans agree). It returns the
// provider — from which callers obtain a log.Logger for the audit emitter —
// and a shutdown to flush on exit.
//
// When the gate is off it returns (nil, no-op, nil): the caller gets a nil
// provider and the audit emitter is a no-op (off-path byte-identical).
func Setup(ctx context.Context, build string) (*sdklog.LoggerProvider, ShutdownFunc, error) {
	noop := func(context.Context) error { return nil }

	if !logsEnabled() {
		return nil, noop, nil
	}

	// otlploghttp reads OTEL_EXPORTER_OTLP_ENDPOINT (and the standard OTLP
	// env vars) from the environment by default; WithEndpointURL is applied
	// explicitly when the var is set so a bare host:port or a full URL both
	// resolve — same contract as the trace exporter.
	opts := []otlploghttp.Option{}
	if ep := env.String(EnvOTLPEndpoint, ""); ep != "" {
		opts = append(opts, otlploghttp.WithEndpointURL(ep))
	}

	exp, err := otlploghttp.New(ctx, opts...)
	if err != nil {
		return nil, noop, err
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
			semconv.ServiceVersion(build),
		),
	)
	if err != nil {
		if res == nil {
			res = resource.Default()
		}
	}

	lp := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(exp)),
	)

	return lp, lp.Shutdown, nil
}
