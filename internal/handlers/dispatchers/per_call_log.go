package dispatchers

import (
	"log/slog"
	"net/http"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"go.opentelemetry.io/otel/trace"
)

// perCallState carries the per-/call timing + observability state that
// gets emitted into the snowplow stdout -> otel-daemonset filelog ->
// ClickHouse otel_logs pipeline at dispatcher exit. Created once per
// ServeHTTP and mutated as the call progresses (l1Hit, gvr) before the
// deferred emit() runs.
//
// Ship 0.30.171-debug only. The OTel SDK isn't wired on the shipping
// branch; the existing stdout->otel-daemonset pipeline IS (~28k rows/hr
// empirically). This emission is a single slog.InfoContext per /call
// (overhead well below 1ms) — used to identify the slow /call class in
// the 8-cycle parallelism diagnostic (slowest_call_ms ~470ms, chain
// ~3.65 => par=2.0 vs anchor par=4.3).
type perCallState struct {
	start    time.Time
	path     string
	method   string
	handler  string // "restactions" | "widgets"
	l1Hit    string // "hit" | "miss" | "content-hit" | "n/a"
	gvr      string // group/version/resource — set once fetchObject succeeds
	user     string // captured at emit() from xcontext.UserInfo(ctx)
}

// beginPerCall is called as the FIRST line of each ServeHTTP body. It
// returns the live state pointer so the dispatcher can update l1Hit /
// gvr as the call progresses, and a deferred emit() closure that should
// be invoked at function exit via `defer beginPerCall(...)()`-style
// usage. Default l1Hit is "n/a" so an error before lookup still emits a
// well-formed row.
func beginPerCall(r *http.Request, handler string) (*perCallState, func()) {
	st := &perCallState{
		start:   time.Now(),
		path:    r.URL.Path,
		method:  r.Method,
		handler: handler,
		l1Hit:   "n/a",
	}
	ctx := r.Context()
	return st, func() {
		user := ""
		if ui, err := xcontext.UserInfo(ctx); err == nil {
			user = ui.Username
		}
		attrs := []any{
			slog.String("handler", handler),
			slog.String("path", st.path),
			slog.String("method", st.method),
			slog.String("user", user),
			slog.String("l1_hit", st.l1Hit),
			slog.String("gvr", st.gvr),
			slog.Int64("total_ms", time.Since(st.start).Milliseconds()),
		}
		// OTel log-correlation (ADDITIVE + default-OFF). When tracing is
		// enabled, an OTel server span is active on ctx (otelhttp wrap in
		// main.go), so attach its W3C trace_id/span_id to this
		// otel_logs-bound record for trace<->log correlation in ClickHouse.
		// When tracing is disabled SpanContextFromContext returns an invalid
		// span context, so these fields are simply omitted and the record is
		// byte-identical to the pre-OTel emission. This does NOT touch the
		// shortid X-Krateo-TraceId correlation id, which lives on a separate
		// header/status field and is unaffected here.
		if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
			attrs = append(attrs,
				slog.String("trace_id", sc.TraceID().String()),
				slog.String("span_id", sc.SpanID().String()),
			)
		}
		slog.InfoContext(ctx, "dispatcher.call.complete", attrs...)
	}
}
