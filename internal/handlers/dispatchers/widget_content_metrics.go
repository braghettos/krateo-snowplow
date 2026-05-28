// widget_content_metrics.go — Ship 1.3 (lever 1) observability for the
// identity-free widget-content empty-shell guard.
//
// PURPOSE. populateWidgetContentL1 (widget_content.go) refuses to store a
// TRANSIENT-EMPTY POISON SHELL — an apiRef+resourcesRefsTemplate widget
// that resolved with empty status.resourcesRefs.items under the SA-maximal
// walk identity (a boot data-availability artifact, never a genuine zero).
// This counter surfaces how often the guard fired so the falsifier can
// confirm the boot walk stopped poisoning the compositions datagrid cell
// (a non-zero value during boot = the guard caught at least one poison
// shell; zero after warm-up = the cell is populated, not re-guarded).
//
// PRIOR ART — phase1_walk_metrics.go (snowplow_phase1_walk_zero_children_total):
// the SAME atomic.Uint64 + expvar.Func + sync.Once idiom, same CFG-1
// cache-off init gate (project_cache_off_is_transparent_fallback). Read-
// only, lazy at scrape time, zero per-/call cost.

package dispatchers

import (
	"expvar"
	"sync"
	"sync/atomic"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// widgetContentSkippedEmptyShellTotal is the process-wide count of
// populateWidgetContentL1 Puts the lever-1 guard declined because the
// resolved widget was a transient-empty poison shell.
var widgetContentSkippedEmptyShellTotal atomic.Uint64

// widgetContentSkippedRBACSensitiveTotal is the process-wide count of
// populateWidgetContentL1 Puts the RBAC-sensitivity guard (task #69)
// declined because the widget is an apiRef-driven render-template widget
// that renders from status.widgetData (not narrowed by the serve-gate) and
// is therefore routed to the per-cohort `widgets` L1 instead of the
// shared, identity-free content cell. A non-zero value confirms the
// classified piechart/table/datagrid widgets are NOT poisoning the shared
// cell — they self-route to the RBAC-correct per-cohort path.
var widgetContentSkippedRBACSensitiveTotal atomic.Uint64

// bumpWidgetContentSkippedEmptyShell increments the guard counter. Called
// by populateWidgetContentL1 when shouldSkipEmptyWidgetShell fires.
func bumpWidgetContentSkippedEmptyShell() {
	widgetContentSkippedEmptyShellTotal.Add(1)
}

// bumpWidgetContentSkippedRBACSensitive increments the RBAC-sensitivity
// guard counter. Called by populateWidgetContentL1 when
// isRBACSensitiveApiRefWidget fires.
func bumpWidgetContentSkippedRBACSensitive() {
	widgetContentSkippedRBACSensitiveTotal.Add(1)
}

// widgetContentMetricsOnce guards expvar.Publish against the double-
// publish panic (mirrors phase1WalkMetricsOnce).
var widgetContentMetricsOnce sync.Once

func init() {
	// CFG-1 mirror: under CACHE_ENABLED=false the content layer does not
	// run and this gauge MUST NOT be registered.
	if cache.Disabled() {
		return
	}
	registerWidgetContentMetrics()
}

// registerWidgetContentMetrics publishes the lever-1 guard counter.
// Guarded by widgetContentMetricsOnce so it is safe from init() and a
// test helper.
func registerWidgetContentMetrics() {
	widgetContentMetricsOnce.Do(func() {
		expvar.Publish("snowplow_widget_content_skipped_empty_shell_total", expvar.Func(func() any {
			return widgetContentSkippedEmptyShellTotal.Load()
		}))
		expvar.Publish("snowplow_widget_content_skipped_rbac_sensitive_total", expvar.Func(func() any {
			return widgetContentSkippedRBACSensitiveTotal.Load()
		}))
	})
}

// RegisterWidgetContentMetricsForTest forces registration under tests that
// flip CACHE_ENABLED=true via t.Setenv after init() already ran with the
// var unset. Idempotent. Production callers MUST NOT use this.
func RegisterWidgetContentMetricsForTest() {
	registerWidgetContentMetrics()
}
