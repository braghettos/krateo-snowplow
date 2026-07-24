// prewarm_boot_convergence.go — #105: the per-boot-scope-lifetime re-walk
// re-enqueue bound (Option (ii), docs/105-marketplace-rewalk-boot-budget-
// trace-2026-07-24.md §5).
//
// THE DEFECT (§1). A DETERMINISTIC seed failure — a jq-eval error on the
// marketplace-detail portal RA, which is non-apierror and non-ctx — falls
// through classifySeedErr's fail-loud default to seedFailOperational and
// (pre-#105) did an immediate enqueueScope of the WHOLE boot re-walk. That
// re-enqueue is bounded PER PASS by the reEnqueued latch, but NOT across
// passes: each re-walk is a fresh seedScopeYielding pass, the deterministic
// item fails again identically, and the boot scope is enqueued again —
// unbounded across passes. Every re-walk resets the seed budget, so the
// admin-cohort seed the loop was supposed to make never lands (§8).
//
// THE FIX. Bound the re-walk at the mechanism that owns it (the boot scope):
// stop re-enqueueing after bootMaxNoProgressPasses consecutive re-walks that
// made NO FORWARD PROGRESS, then emit prewarm.engine.boot.converged_with_skips
// (boot is "done, these items given up") and let the terminal pass run to
// boot.complete via the EXISTING nil→boot.complete→Forget path (C-105-5: NEVER
// an early-error-return, which would divert to the scope_incomplete/
// AddRateLimited door and re-open the loop).
//
// THE PROGRESS SIGNAL IS A SET-DELTA, NOT A COUNT (design §5.0 — load-bearing).
// A stable boot re-walk fresh-skips already-warm cells (they did no Put), and a
// TTL-driven re-Put re-adds an already-member target without growing the SET.
// So "seededThisPass > 0" false-negatives the real shape (healthy cohorts
// fresh-skip on stable re-walks; TTL re-Puts oscillate a count). The sound
// signal is:
//
//	grew   := len(seededSet) > priorSeededCount
//	shrank := len(failedSet) < len(priorFailedSet) && failedSet ⊆ priorFailedSet
//	madeProgress := grew || shrank
//
// seededSet is the cross-pass BootSeededSet (Put ∪ fresh-skip, ctx-carried,
// recorded at the two success sites in phase1_pip_seed.go). failedSet is the
// per-pass set of deterministically-failing targets (recorded by
// classifyEngineSeedErr's operational branch). On !madeProgress increment
// consecutiveNoProgressPasses, else reset to 0; snapshot prior* for the next
// pass. Re-enqueue ONLY while consecutiveNoProgressPasses < bootMaxNoProgressPasses.
//
// ENGINE-LIVED PER BOOT-SCOPE-KEY (mirrors declinedExtSets, prewarm_engine.go).
// The state is created on the boot scope's first processScope, REUSED across
// its AddRateLimited requeues (so priorSeededCount/priorFailedSet accumulate
// across resume passes — the whole point of the cross-pass bound), and TORN
// DOWN on genuine boot completion / config-vars redrive (clearBootConvergence
// State) so a new nav topology re-arms with fresh empty sets. It carries the
// engine handle so the tail re-enqueue targets the SAME engine that owns the
// scope (the process singleton in production; a local test engine under a
// falsifier's real worker) rather than a hardcoded singleton.

package dispatchers

import (
	"context"
	"sort"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// bootMaxNoProgressPasses is the #105 re-enqueue bound: after this many
// CONSECUTIVE boot re-walks that made no forward set-progress, the boot scope
// stops re-enqueueing itself and converges-with-skips. A const (F4-C6 style, no
// new env knob). 2 means: the pass that FIRST detects no-progress and the next
// one both still re-enqueue (a slow-converging 50K boot that reaches a new
// cohort only every other pass is NOT prematurely given up); the third
// consecutive no-progress pass is the one that stops (C-105-1 slow-convergence
// safety + Arm A bound ≤ bootMaxNoProgressPasses+1).
const bootMaxNoProgressPasses = 2

// ctxKeyBootConvergenceType is the typed context key for the engine-lived
// boot-convergence state. Distinct unexported type — no cross-package
// raw-string-key collision (mirrors ctxKeySeedResolveMemoType /
// ctxKeyBootSeededSetType).
type ctxKeyBootConvergenceType struct{}

var ctxKeyBootConvergence = ctxKeyBootConvergenceType{}

// bootConvergenceState is the engine-lived per-boot-scope-key convergence
// bookkeeping. seeded is the cross-pass BootSeededSet (also installed on the
// resolve ctx so the primitives Mark it); priorSeededCount / priorFailedSet are
// the prior-pass snapshots the set-delta compares against; consecutiveNoProgressPasses
// is the run-length of no-progress passes that the bound trips on. engine is the
// engine that owns this scope (so the tail re-enqueue targets it, not a
// hardcoded singleton).
//
// CONCURRENCY: seeded (a *cache.BootSeededSet, sync.Map) is written concurrently
// by the fan-out cohort goroutines at the two success sites. Every OTHER field
// (priorSeededCount/priorFailedSet/consecutiveNoProgressPasses) is touched ONLY
// by seedScopeYielding at a pass boundary — the seed loop is serial
// (WithPrewarmIterSerial; engineYieldCheckpoint only DEFERS) and one boot scope
// runs on one worker at a time (the queue dedups on key()=="boot"), so there is
// no concurrent writer to the bookkeeping. It is NOT shared across scope keys.
type bootConvergenceState struct {
	seeded *cache.BootSeededSet

	priorSeededCount            int
	priorFailedSet              map[string]struct{}
	consecutiveNoProgressPasses int

	// reEnqueuedLastPass records whether the most recent pass's tail re-enqueued
	// the boot scope. processScope reads it to decide teardown: the #105
	// re-enqueue is a NIL-return + plain Add (not an err!=nil AddRateLimited), so
	// a bare "err==nil → clear" (the declined-external teardown signal) would wipe
	// the cross-pass state on EVERY re-walk pass and the set-delta could never
	// accumulate. The convergence state is torn down only when the boot genuinely
	// completes — i.e. err==nil AND the tail did NOT re-enqueue (a clean pass with
	// no operational failures, or the bound tripped/converged). Written by
	// finalizeBootReEnqueue, read by processScope; both run on the single boot
	// worker (no concurrent access).
	reEnqueuedLastPass bool

	engine *prewarmEngine
}

// newBootConvergenceState builds a fresh state with an empty seeded-set + empty
// prior snapshots for engine e. priorFailedSet starts nil (an empty set); the
// first pass's failedSet ⊆ nil-set holds only when the first pass ALSO fails
// nothing, which is correct (a first pass that fails a target has NOT shrunk vs
// "no prior failures" — grew carries progress on the first pass instead).
func newBootConvergenceState(e *prewarmEngine) *bootConvergenceState {
	return &bootConvergenceState{
		seeded:         cache.NewBootSeededSet(),
		priorFailedSet: map[string]struct{}{},
		engine:         e,
	}
}

// withBootConvergenceState returns a child context carrying st. Installed ONLY
// by processScope for the boot scope; nil off the boot path so
// bootConvergenceStateFromContext returns nil and the tail re-enqueue logic
// falls back to the legacy per-pass behavior (seedScopeYielding guards on nil).
func withBootConvergenceState(ctx context.Context, st *bootConvergenceState) context.Context {
	if ctx == nil || st == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyBootConvergence, st)
}

// bootConvergenceStateFromContext returns the *bootConvergenceState on ctx, or
// nil when none is present (every non-boot path, and cache-off). nil is handled
// at the read site.
func bootConvergenceStateFromContext(ctx context.Context) *bootConvergenceState {
	if ctx == nil {
		return nil
	}
	st, _ := ctx.Value(ctxKeyBootConvergence).(*bootConvergenceState)
	return st
}

// evaluateBootProgress applies the #105 set-delta after a boot/gvr-discovered
// seedScopeYielding pass. failedSet is THIS pass's deterministically-failing
// target set (kind/label-keyed, from classifyEngineSeedErr). It returns
// (reEnqueue, givenUp, passes):
//
//   - reEnqueue: whether the caller should re-enqueue the boot scope. True while
//     consecutiveNoProgressPasses < bootMaxNoProgressPasses; false once the
//     bound trips (converged-with-skips) OR when there were zero operational
//     failures this pass (nothing to retry — a clean pass just completes).
//   - givenUp: the sorted target labels being given up (this pass's failedSet),
//     for the converged_with_skips log; empty unless the bound just tripped.
//   - passes: consecutiveNoProgressPasses at the moment the bound tripped.
//
// It MUTATES st (consecutiveNoProgressPasses + the prior* snapshots) so the next
// pass compares against this pass. Pure set logic otherwise; the caller owns the
// re-enqueue side effect + the log so the terminal path stays the nil→
// boot.complete path (C-105-5).
func (st *bootConvergenceState) evaluateBootProgress(failedSet map[string]struct{}) (reEnqueue bool, givenUp []string, passes int) {
	// No operational failures this pass → nothing drove a re-enqueue; the pass is
	// a clean completion. Reset the no-progress run (a pass with no failures is
	// unambiguous progress toward "done") and snapshot. The legacy behavior for a
	// clean pass was also "no re-enqueue" (reEnqueued stayed false), so this is
	// byte-identical to today for the common clean boot.
	if len(failedSet) == 0 {
		st.consecutiveNoProgressPasses = 0
		st.priorSeededCount = st.seeded.Len()
		st.priorFailedSet = failedSet
		return false, nil, 0
	}

	seededNow := st.seeded.Len()
	grew := seededNow > st.priorSeededCount
	shrank := len(failedSet) < len(st.priorFailedSet) && subsetOf(failedSet, st.priorFailedSet)
	madeProgress := grew || shrank

	if madeProgress {
		st.consecutiveNoProgressPasses = 0
	} else {
		st.consecutiveNoProgressPasses++
	}
	// Snapshot for the next pass BEFORE deciding, so the decision reads the
	// updated run-length but the next pass compares against THIS pass's sets.
	st.priorSeededCount = seededNow
	st.priorFailedSet = failedSet

	if st.consecutiveNoProgressPasses < bootMaxNoProgressPasses {
		// Still making (or recently made) progress, or within the grace window —
		// re-enqueue and keep converging. Legacy behavior: an operational failure
		// always re-enqueued; #105 keeps that UNTIL the bound.
		return true, nil, st.consecutiveNoProgressPasses
	}
	// Bound tripped: give up the currently-failing set, do NOT re-enqueue. The
	// terminal pass already ran to completion (the caller returns nil → the
	// existing boot.complete→Forget path fires, C-105-5).
	return false, sortedKeys(failedSet), st.consecutiveNoProgressPasses
}

// subsetOf reports whether every key of a is in b (a ⊆ b).
func subsetOf(a, b map[string]struct{}) bool {
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

// sortedKeys returns m's keys in ascending order (deterministic
// given_up_targets for the converged_with_skips log).
func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
