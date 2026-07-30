// boot_resume_attempt.go — #135 F4b Lever B boot-scope resume-attempt marker.
//
// Lever B stops re-driving the discovery walk() on a boot RESUME pass. The
// navWidgetHarvester + contentPrewarmHarvester are already process-lived,
// monotonic, first-write-wins accumulators (never cleared in production), so on
// a resume pass their snapshot() already returns the union of every prior pass —
// re-walking rediscovers a set the shared harvester already holds (~255s of pure
// waste per resume, docs/f4b-leverb-discovery-snapshot-reuse-design-2026-07-22.md
// §1). This marker carries the workqueue's per-item requeue count
// (NumRequeues(s)) from processScope onto the boot scope ctx so rePrewarmBootScoped
// can distinguish pass 0 (attempt==0 → WALK) from an F.4 deadline-cut resume
// (attempt>0 → REUSE the snapshot, skip the walk).
//
// WHY THE ATTEMPT COUNT IS THE RIGHT SIGNAL (design §3): the queue's
// NumRequeues/Forget semantics already materialize the distinction Lever B needs.
// An F.4 deadline-cut resume goes through AddRateLimited → NumRequeues>0. A
// genuine config-vars redrive (topology change) goes through Forget (resets
// NumRequeues to 0) + immediate Add + a sibling Forget in enqueueBootReDrive
// (§3.2), so it re-dequeues at attempt==0 → RE-WALKS the new config. The reuse is
// additionally guarded by a non-empty-snapshot check in rePrewarmBootScoped so
// the boot-race give-up→appear→self-heal path (empty harvester) always re-walks.
//
// This is NOT a new invalidation subscription — it is a read of state client-go
// already tracks (feedback_no_special_cases: attempt is the requeue count, not a
// magic number). Installed ONLY on the boot scope ctx in processScope, mirroring
// the Lever A WithSeedDeclinedExternalSet install; absent → 0 → WALK (the safe
// default, so any non-boot / uninstrumented path always walks).

package cache

import "context"

// ctxKeyBootResumeAttemptType is the typed empty-struct context key for the
// boot-scope resume-attempt count. Distinct unexported type — no cross-package
// raw-string-key collision (mirrors ctxKeySeedDeclinedExternalSetType).
type ctxKeyBootResumeAttemptType struct{}

var ctxKeyBootResumeAttempt = ctxKeyBootResumeAttemptType{}

// WithBootResumeAttempt returns a child context carrying the boot scope's
// workqueue requeue count (attempt). Installed ONLY by processScope on the boot
// scope ctx (boot-scope-only, mirroring WithSeedDeclinedExternalSet). attempt==0
// is pass 0 (fresh enqueue or Forget-reset genuine redrive); attempt>0 is an F.4
// deadline-cut resume of the same topology. Inert under Disabled() (a cache-off
// process never runs the prewarm engine, so this is never consulted there).
func WithBootResumeAttempt(ctx context.Context, attempt int) context.Context {
	if ctx == nil || Disabled() {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyBootResumeAttempt, attempt)
}

// BootResumeAttemptFromContext returns the boot resume-attempt count installed on
// ctx, or 0 when none is present. A 0 return MUST be treated as "pass 0 — WALK"
// (the safe default): any path that did not install the marker re-walks, so Lever
// B can never skip a walk it wasn't explicitly told is a resume of a
// still-populated boot scope.
func BootResumeAttemptFromContext(ctx context.Context) int {
	if ctx == nil {
		return 0
	}
	v, _ := ctx.Value(ctxKeyBootResumeAttempt).(int)
	return v
}
