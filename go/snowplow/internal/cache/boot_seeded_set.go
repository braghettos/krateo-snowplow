// boot_seeded_set.go — the #105 boot-convergence "seeded-this-boot" marker set.
//
// THE DEFECT (docs/105-marketplace-rewalk-boot-budget-trace-2026-07-24.md §1):
// a DETERMINISTIC seed failure (a jq-eval error on the marketplace-detail
// portal RA — non-apierror, non-ctx) is classified seedFailOperational by the
// fail-loud default and does an immediate enqueueScope of the WHOLE boot
// re-walk. That re-enqueue is bounded per-pass (the reEnqueued latch) but NOT
// across passes: each re-walk is a fresh seedScopeYielding pass, the item fails
// again identically, and re-enqueues the boot scope again — unbounded across
// passes, starving the admin-cohort seed budget forever.
//
// THE FIX (Option (ii), per-boot-scope-lifetime re-enqueue bound): stop
// re-enqueueing after N consecutive re-walks that made NO forward progress.
// The SOUND progress signal is a SET-DELTA, not a count (design §5.0): "did the
// set of successfully-seeded targets GROW, or did the set of failing targets
// SHRINK, versus the prior pass?" A count / "seeded>0" flag false-negatives the
// real shape — on a stable re-walk every healthy cohort cell is already warm and
// FRESH-SKIPS (no Put), and a TTL-driven re-Put re-adds an already-member target
// without growing the SET. Only a set-delta correctly reads "no net progress"
// when the only remaining activity is re-attempting-and-re-failing one
// deterministic item.
//
// THIS TYPE is the "seeded SET" half of that signal: the set of successfully-
// warmed target CELL KEYS reached THIS boot scope. It is recorded at BOTH
// success sites the design names (§As-built):
//
//   - each genuine Put (phase1_pip_seed.go seedOneRestaction / seedOneWidget —
//     the same site that bumps pipBindingSetSeedResolvesTotal), AND
//   - the fresh-SKIP site (seedSkipDecision→true — a fresh-skip means "this
//     target IS warm this boot," so it belongs in the seeded SET even though it
//     did no Put this pass).
//
// The union "Put this pass ∪ fresh-skipped this pass" = "warm targets reached
// this boot," which GROWS only when a genuinely-new cohort/target is reached and
// does NOT grow on a TTL re-Put of an already-member target — exactly the
// progress signal §5.0 prescribes. The failing-SET half (which targets failed
// this pass) is a pass-local map in seedScopeYielding populated by
// classifyEngineSeedErr; only the seededSet needs ctx-carrying so the two
// success sites in the primitives can reach it.
//
// WHY A CONTEXT-CARRIED SET (not process-global) — mirrors SeedDeclinedExternalSet
// / SeedResolveMemo (the established per-seed-pass sink idiom): the set is
// installed by the boot scope onto its scope context, ENGINE-LIVED per
// boot-scope-key (REUSED across the scope's AddRateLimited requeues so a resume
// pass accumulates into the SAME set the prior pass populated), and TORN DOWN on
// genuine boot completion / config-vars redrive. A user /call, the refresher,
// the keepwarm sweep, and the discovery walk never install one, so
// BootSeededSetFromContext returns nil there and Mark() is a strict no-op off the
// boot seed path.
//
// KEY = THE FULL dispatchCacheLookupKey (single-derivation, mirrors the declined
// set §C-F4B-2): the marker is keyed by the EXACT `key` the Put/Get/skip triple
// uses — the cohort-identity-folding dispatchCacheLookupKey. Cohort A warming
// widget W marks A's key; cohort B's first warm of W derives a DIFFERENT key
// (distinct RBAC identity) → a NEW member → the seeded SET GROWS. That growth IS
// the "a genuinely-new cohort was reached" progress signal; keying on the full
// per-cohort key is what makes the set-delta detect newly-reached cohorts.

package cache

import (
	"context"
	"sync"
)

// ctxKeyBootSeededSetType is the typed empty-struct context key for the
// boot-convergence seeded-set. Distinct unexported type — no cross-package
// raw-string-key collision (mirrors ctxKeySeedDeclinedExternalSetType).
type ctxKeyBootSeededSetType struct{}

var ctxKeyBootSeededSet = ctxKeyBootSeededSetType{}

// BootSeededSet records the FULL cache keys the boot seed successfully WARMED
// (Put or fresh-skipped) WITHIN A SINGLE BOOT SCOPE (across all its resume
// passes). seedScopeYielding reads len(set) at each pass tail to compute the
// seeded-set-grew half of the #105 progress signal. Concurrency-safe (sync.Map):
// the seed fans widgets across cohort goroutines and both success sites Mark
// concurrently.
//
// Membership is keyed by the exact dispatchCacheLookupKey the Put/Get/skip triple
// uses, so cohort identity is encoded in the key — a genuinely-new cohort warming
// the same widget adds a NEW member (the set GROWS), which is precisely the
// forward-progress the loop was resetting (§8).
type BootSeededSet struct {
	m sync.Map // key string -> struct{} (membership only; value irrelevant)

	mu    sync.Mutex // guards count (the cheap len() snapshot the set-delta reads)
	count int
}

// Mark records key as successfully-warmed (Put or fresh-skip) this boot scope.
// nil-receiver-safe (no-op) so an uninstalled call site never marks. Idempotent:
// a second Mark of the same key (e.g. a TTL re-Put on a resume pass) is a
// harmless no-op via LoadOrStore and does NOT grow the count — the crux that
// makes a TTL re-Put read as "no net progress."
func (s *BootSeededSet) Mark(key string) {
	if s == nil || key == "" {
		return
	}
	if _, loaded := s.m.LoadOrStore(key, struct{}{}); !loaded {
		s.mu.Lock()
		s.count++
		s.mu.Unlock()
	}
}

// Len returns the count of distinct keys warmed this boot scope — the cumulative
// cross-pass seeded-set size the set-delta compares against priorSeededCount.
// nil-receiver-safe (returns 0). O(1) — reads the maintained counter, not a
// sync.Map range.
func (s *BootSeededSet) Len() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count
}

// NewBootSeededSet builds an empty seeded-set.
func NewBootSeededSet() *BootSeededSet {
	return &BootSeededSet{}
}

// WithBootSeededSet returns a child context carrying set. Installed ONLY by the
// boot scope pass (processScope, scopeKindBoot only) — a user /call, the
// refresher, the keepwarm sweep, and the discovery walk never install one, so the
// set is a strict no-op off the boot seed path. Inert under Disabled(): returns
// ctx unchanged so a cache-off process carries no set.
func WithBootSeededSet(ctx context.Context, set *BootSeededSet) context.Context {
	if ctx == nil || set == nil || Disabled() {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyBootSeededSet, set)
}

// BootSeededSetFromContext returns the *BootSeededSet installed on ctx, or nil
// when none is present. A nil return MUST be treated as "no set — record
// nothing" (Mark is nil-receiver-safe). Off the boot seed path this is always
// nil, which is what makes the marker provably untouched on the /call path.
func BootSeededSetFromContext(ctx context.Context) *BootSeededSet {
	if ctx == nil {
		return nil
	}
	v, _ := ctx.Value(ctxKeyBootSeededSet).(*BootSeededSet)
	return v
}
