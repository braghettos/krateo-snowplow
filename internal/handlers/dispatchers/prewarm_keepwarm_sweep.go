// prewarm_keepwarm_sweep.go — #102 c1: the TTL-cadenced quiet-page keep-warm
// sweep ticker (docs/g-ttl-quiet-page-keepwarm-design-2026-07-04.md §3 option c1).
//
// THE GAP IT CLOSES (§1). A resolved-cache cell whose underlying DATA goes quiet
// for > RESOLVED_CACHE_TTL_SECONDS dies at TTL by design — CreatedAt slides ONLY
// on Put (a read never refreshes it), the refresher is event-driven and skips
// absent/quiet cells, and there is no store janitor (lazy-on-Get expiry only).
// So a >1h-returning user on a quiet page pays a full cold resolve (seconds at
// 50K), with nothing to re-create the entry (the dep index does not survive
// eviction, §1.4). This is broader than "unread pages": EVERY quiet-data cell
// dies hourly, read-hot or not.
//
// THE MECHANISM (§3 c1). A ticker anchored to the process ctx enqueues a
// scopeKindKeepwarm on the prewarm engine at TTL×3/4 — a design ratio derived
// from the SAME RESOLVED_CACHE_TTL_SECONDS that governs expiry (cache.ResolvedCacheTTL),
// NOT a new env knob. The engine handler (rePrewarmKeepwarm) runs the boot
// re-walk + seed core with the seed bounded to rank-1 (the 95%-mix cohort, ALL
// pages) — each sweep Put RE-RESOLVES fresh bytes and resets CreatedAt, so
// rank-1's covered cells never lazy-expire. Re-resolving (not TTL-extending)
// preserves the §1.6 staleness backstop by construction; the GTTL-1 error-aware
// Put-gate (declineSeedPutOnError) declines a degraded re-resolve rather than
// overwrite a good warm entry.
//
// WHY TTL×3/4. The cell must be re-Put BEFORE it expires. Firing at 3/4 of the
// TTL leaves a 1/4-TTL margin for the sweep itself to run (the rank-1 pass ≈
// 190s with FIX-G, well inside the ~15min margin at the 3600s default) plus
// engine-yield deferrals under customer load — so a covered cell is always
// re-Put within its lifetime even if one sweep is delayed by a customer burst.
//
// SCAN-FREE (§6 GTTL-6). The store keeps NO janitor and is NOT scanned — the
// sweep is SEED-SIDE re-enqueue (the audit's no-store-sweep guidance holds). The
// engine queue coalesces on key()=="keepwarm": a tick arriving while a sweep
// still runs dedups to at most one pending sweep (no pile-up).
//
// NO NEW ENV: cadence = TTL×3/4 (derived ratio); scope = rank-1 (a structural
// boundary, not a count); enablement rides the existing prewarm/engine gate
// (main.go). Back-out = CACHE_ENABLED=false (kills prewarm, the engine, and this
// ticker together).

package dispatchers

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// keepwarmSweepStarted guards StartKeepwarmSweep against a double start
// (idempotent per process — the first successful start wins). Package-level to
// mirror the other Phase-1 singletons (configVarsWatchStarted, engineProcessCtx).
var keepwarmSweepStarted atomic.Bool

// keepwarmCadenceNumerator / keepwarmCadenceDenominator express the TTL×3/4
// design ratio as a fraction (NOT a magic duration): the ticker fires every
// TTL × 3/4. Stated as named constants so the ratio is self-documenting and the
// falsifier can reference the same fraction rather than a hardcoded interval.
const (
	keepwarmCadenceNumerator   = 3
	keepwarmCadenceDenominator = 4
)

// keepwarmSweepInterval returns the TTL×3/4 cadence derived from the effective
// resolved-cache TTL (cache.ResolvedCacheTTL — the SAME source that governs
// expiry). Always positive (ResolvedCacheTTL never returns <=0), so the ticker
// interval is always valid.
func keepwarmSweepInterval() time.Duration {
	return cache.ResolvedCacheTTL() * keepwarmCadenceNumerator / keepwarmCadenceDenominator
}

// StartKeepwarmSweep starts the #102 c1 keep-warm sweep ticker on the given
// process-lifetime context (main.go passes cacheCtx). Idempotent: the first
// call wins; later calls are no-ops. The ticker goroutine exits when ctx is
// cancelled (process shutdown) — not a leak.
//
// The tick handler is intentionally TINY (the phase1_configvars_watch.go
// contract): it only calls the O(1) non-blocking enqueueScope on the engine
// singleton. The actual sweep work (rePrewarmKeepwarm) runs on the engine
// worker goroutine under its customer-priority yield + bounded dedup queue +
// adaptive memory gate — the sweep contends with customers exactly as the boot
// seed does (§4, proven by the FIX-F non-contention arm which covers this path
// identically).
func StartKeepwarmSweep(ctx context.Context) {
	if !keepwarmSweepStarted.CompareAndSwap(false, true) {
		return
	}
	interval := keepwarmSweepInterval()
	slog.Default().Info("prewarm.keepwarm.sweep.started",
		slog.String("subsystem", "cache"),
		slog.Int64("interval_ms", interval.Milliseconds()),
		slog.Int64("ttl_ms", cache.ResolvedCacheTTL().Milliseconds()),
		slog.String("cadence", "TTL×3/4"),
		slog.String("effect", "rank-1 first-nav cell set is re-seeded on this cadence so it never "+
			"lazy-expires (each sweep Put resets CreatedAt); re-resolves fresh bytes (not TTL-extend)"),
	)
	go runKeepwarmSweepLoop(ctx, interval)
}

// runKeepwarmSweepLoop is the ticker body — extracted so a test can drive it
// with a short interval + a cancellable ctx and observe the enqueue cadence
// without the process-lifetime StartKeepwarmSweep guard.
func runKeepwarmSweepLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			slog.Default().Info("prewarm.keepwarm.sweep.stopped",
				slog.String("subsystem", "cache"),
				slog.Any("cause", ctx.Err()),
			)
			return
		case <-ticker.C:
			enqueueKeepwarmSweep()
		}
	}
}

// enqueueKeepwarmSweep enqueues one scopeKindKeepwarm on the process engine
// singleton (coalesced on key()=="keepwarm"). Extracted so the falsifier can
// drive one sweep enqueue deterministically without waiting a real tick.
func enqueueKeepwarmSweep() {
	slog.Default().Info("prewarm.keepwarm.sweep.tick",
		slog.String("subsystem", "cache"),
		slog.String("effect", "enqueuing rank-1 keep-warm sweep (coalesced on \"keepwarm\"); "+
			"engine worker runs it customer-yielded"),
	)
	prewarmEngineSingleton().enqueueScope(prewarmScope{kind: scopeKindKeepwarm})
}

// resetKeepwarmSweepStartedForTest clears the once-guard so a test can drive
// StartKeepwarmSweep/runKeepwarmSweepLoop fresh. TEST-ONLY — production lifecycle
// is set-once at boot.
func resetKeepwarmSweepStartedForTest() {
	keepwarmSweepStarted.Store(false)
}
