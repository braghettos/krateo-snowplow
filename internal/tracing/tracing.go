// Package tracing wires snowplow's OpenTelemetry trace pipeline.
//
// COEXISTENCE CONTRACT (load-bearing): this package is ADDITIVE and must
// not interfere with the existing observability surfaces. Specifically:
//
//   - The shortid `X-Krateo-TraceId` correlation id (plumbing
//     xcontext.TraceId, surfaced as status.traceId, set as an outbound
//     header, and consumed by /refreshes) is UNTOUCHED. OTel spans carry a
//     separate W3C `traceparent`; the two coexist on the same request.
//
//   - The stdout -> otel-daemonset filelog -> ClickHouse `otel_logs` log
//     pipeline (slog JSON to os.Stdout) is UNTOUCHED. OTel adds
//     trace_id/span_id ATTRIBUTES to those log records for correlation but
//     does not reroute or replace the pipeline.
//
// GATING: Setup is a no-op unless OTEL_TRACES_ENABLED is true. When the
// flag is unset/false, Setup registers NOTHING — no TracerProvider, no
// propagator, no exporter — so the global otel.GetTracerProvider() stays
// the no-op default and every instrumentation site (otelhttp handler,
// otelhttp transport, span lookups) degrades to a zero-cost no-op. The
// off-path is therefore byte-identical to the pre-OTel binary.
package tracing

import (
	"context"
	"net/http"
	"time"

	"github.com/krateoplatformops/plumbing/env"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

const (
	// EnvTracesEnabled gates the entire trace pipeline. Default false.
	EnvTracesEnabled = "OTEL_TRACES_ENABLED"

	// EnvOTLPEndpoint is the OTLP/HTTP collector endpoint
	// (e.g. "otel-collector.krateo-system.svc:4318"). Consumed by the
	// standard OTEL_EXPORTER_OTLP_ENDPOINT env contract via
	// otlptracehttp's WithEndpointURL/env auto-config.
	EnvOTLPEndpoint = "OTEL_EXPORTER_OTLP_ENDPOINT"

	// serviceName is the resource service.name reported for every span.
	serviceName = "snowplow"
)

// ShutdownFunc flushes and stops the trace pipeline. Always non-nil so the
// caller can `defer shutdown(ctx)` unconditionally; it is a no-op when
// tracing was not enabled.
type ShutdownFunc func(context.Context) error

// Setup wires the OTLP/HTTP trace exporter and a batch TracerProvider when
// OTEL_TRACES_ENABLED is true, registers it as the global TracerProvider,
// and installs the composite W3C TraceContext + Baggage propagator.
//
// When the gate is off it returns a no-op ShutdownFunc and registers
// nothing (off-path byte-identical guarantee).
//
// build is the snowplow build string (main.build), recorded as the
// service.version resource attribute.
func Setup(ctx context.Context, build string) (ShutdownFunc, error) {
	noop := func(context.Context) error { return nil }

	if !env.Bool(EnvTracesEnabled, false) {
		return noop, nil
	}

	// otlptracehttp reads OTEL_EXPORTER_OTLP_ENDPOINT (and the standard
	// OTLP env vars: headers, protocol, insecure) from the environment by
	// default, so the endpoint is configured via the env contract without
	// a hard-coded address. WithEndpointURL is applied explicitly when the
	// var is set so a bare host:port or a full URL both resolve.
	opts := []otlptracehttp.Option{}
	if ep := env.String(EnvOTLPEndpoint, ""); ep != "" {
		opts = append(opts, otlptracehttp.WithEndpointURL(ep))
	}

	exp, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return noop, err
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
			semconv.ServiceVersion(build),
		),
	)
	if err != nil {
		// resource.New can return a partial resource + a non-fatal merge
		// error (e.g. schema-url skew); use whatever resource we got.
		if res == nil {
			res = resource.Default()
		}
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp,
			sdktrace.WithBatchTimeout(5*time.Second),
		),
		sdktrace.WithResource(res),
	)

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return tp.Shutdown, nil
}

// Enabled reports whether OTEL_TRACES_ENABLED is true. Instrumentation
// sites outside main (e.g. the outbound external-fetch transport) consult
// this so they only wrap when the trace pipeline is actually wired —
// keeping the off-path byte-identical.
func Enabled() bool {
	return env.Bool(EnvTracesEnabled, false)
}

// WrapTransport wraps rt with otelhttp.NewTransport when tracing is
// enabled, so an outbound request created with a span-carrying context
// gets a client span and an injected W3C traceparent (via the global
// propagator). When tracing is disabled it returns rt unchanged — the
// outbound path is then byte-identical to the pre-OTel client. A nil rt is
// returned as-is (callers default to http.DefaultTransport downstream).
func WrapTransport(rt http.RoundTripper) http.RoundTripper {
	if !Enabled() || rt == nil {
		return rt
	}
	return otelhttp.NewTransport(rt)
}
