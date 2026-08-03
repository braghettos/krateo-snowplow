// seed_declined_external_set.go — the #132 F4b Lever A boot-scope
// "resolved-but-declined-external" marker set.
//
// THE DEFECT (docs/f4b-seed-overshoot-design-2026-07-14.md §3): the boot seed
// resolves ~9 external-backed "whale" widgets (observability/ClickHouse +
// marketplace, endpointRef≠None). Each resolve touches a genuine external
// endpoint, so the #102 GTTL-1 declineSeedPutOnError gate (correctly) DECLINES
// the handle.Put — an external-touch cell has no dep edge to invalidate it, so a
// warm entry would go stale silently. The cell is never warmed. On the NEXT
// resume pass, seedSkipDecision(seedModeBoot, ...) does handle.Get → MISS (never
// Put) → returns false → the whale is re-resolved from scratch, paying its full
// external round-trip again. GOTO decline, forever, every resume pass
// (search-results alone = ~42.5s/pass × 5 resume passes = ~210s of pure repeated
// external work the seed can never make progress on). This is the self-inflicted
// resume loop that keeps the boot scope requeuing.
//
// THE FIX (Lever A): give the seed a boot-scoped MEMORY that "this (widget,
// cohort) key was resolved and INTENTIONALLY declined (external) this boot." A
// resume pass consults the marker BEFORE handle.Get; a marked key is skipped
// (the cell is intentionally cold — re-resolving it every resume pass makes zero
// forward progress and only burns budget). The FIRST boot pass still resolves
// each external whale once (records nothing warm — the Put was going to be
// declined anyway; the cell is cold-on-first-/call regardless). Lever A changes
// ONLY whether the seed WASTES budget re-resolving a cell it will decline again.
// It never warms an un-invalidatable cell — the #102 decline is fully preserved.
//
// WHY A CONTEXT-CARRIED SET (not a process-global) — mirrors SeedResolveMemo /
// StageErrorSink / ExternalTouchedSink (the established per-seed-pass /
// per-resolve sink idiom):
//
//   - CORRECTNESS / TEARDOWN: the set is installed by the boot scope
//     (withCohortSeedContext, boot-scope only) and lives ONLY for that boot
//     scope's context. It is torn down when the boot scope's context goes away,
//     so it can never make a later boot, a keepwarm sweep, or a user /call skip a
//     resolve. A user /call context never carries a set, so
//     SeedDeclinedExternalSetFromContext returns nil there and Marked() is always
//     false on the request path — the /call path re-resolves the external widget
//     fresh, unaffected (C-F4B-3).
//
//   - KEY = THE FULL dispatchCacheLookupKey (C-F4B-2): the marker is keyed by the
//     EXACT `key` the Put/Get pair uses — the cohort-identity-folding
//     dispatchCacheLookupKey, NEVER per-(class,target). Cohort A resolving+
//     declining widget W marks A's key; cohort B's FIRST encounter of W derives a
//     DIFFERENT key (distinct RBAC identity) → NOT marked → B still resolves. A
//     per-target marker would be a cross-cohort correctness bug (B would skip a
//     widget it never resolved). Single-derivation: the caller passes the same
//     `key` seedSkipDecision consults and declineSeedPutOnError marks, so
//     marker-key correctness cannot drift from Put-key correctness
//     (feedback_consultation_mutation_is_not_key_correctness).
//
// TOGGLE (C-F4B / C-F4-8 pattern): the set is only ever installed on the boot
// seed context, which only exists when the boot seed runs, which only runs under
// ResolvedCacheEnabled() / the prewarm gate. A cache-off process never installs a
// set → SeedDeclinedExternalSetFromContext is nil everywhere → zero behavior
// change. WithSeedDeclinedExternalSet additionally no-ops the install under
// Disabled().

package cache

import (
	"context"
	"sync"
)

// ctxKeySeedDeclinedExternalSetType is the typed empty-struct context key for
// the boot-scope declined-external marker set. Distinct unexported type — no
// cross-package raw-string-key collision (mirrors ctxKeySeedResolveMemoType).
type ctxKeySeedDeclinedExternalSetType struct{}

var ctxKeySeedDeclinedExternalSet = ctxKeySeedDeclinedExternalSetType{}

// SeedDeclinedExternalSet records the FULL cache keys the boot seed
// resolved-and-declined for an EXTERNAL touch WITHIN A SINGLE BOOT SCOPE. A
// resume pass consults it (Marked) before its handle.Get so a whale the same
// boot already resolved-and-declined is not re-resolved on the resume pass.
// Concurrency-safe (sync.Map): the seed fans widgets across cohort goroutines.
//
// Membership is keyed by the exact dispatchCacheLookupKey the Put/Get pair uses,
// so cohort identity is encoded in the key — a marker set by cohort A can never
// suppress cohort B's first resolve of the same widget (C-F4B-2).
type SeedDeclinedExternalSet struct {
	m sync.Map // key string -> struct{} (membership only; value is irrelevant)

	marks   uint64
	mu      sync.Mutex // guards marks (diagnostic counter only)
}

// Mark records key as resolved-and-declined-external for this boot scope.
// nil-receiver-safe (no-op) so an uninstalled call site never marks. Idempotent:
// a second Mark of the same key is a harmless no-op (LoadOrStore).
func (s *SeedDeclinedExternalSet) Mark(key string) {
	if s == nil || key == "" {
		return
	}
	if _, loaded := s.m.LoadOrStore(key, struct{}{}); !loaded {
		s.mu.Lock()
		s.marks++
		s.mu.Unlock()
	}
}

// Marked reports whether key was already resolved-and-declined-external this
// boot scope. nil-receiver-safe: (nil).Marked is always false so a call site
// with no set installed behaves as "never skip on this account" (the resume pass
// falls through to the normal handle.Get liveness check).
func (s *SeedDeclinedExternalSet) Marked(key string) bool {
	if s == nil || key == "" {
		return false
	}
	_, ok := s.m.Load(key)
	return ok
}

// Marks returns the count of distinct keys marked (diagnostic — feeds the
// phase1.seed.skip.declined_external falsifier/summary line). nil-receiver-safe.
func (s *SeedDeclinedExternalSet) Marks() uint64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.marks
}

// NewSeedDeclinedExternalSet builds an empty declined-external marker set.
func NewSeedDeclinedExternalSet() *SeedDeclinedExternalSet {
	return &SeedDeclinedExternalSet{}
}

// WithSeedDeclinedExternalSet returns a child context carrying set. Installed
// ONLY by the boot seed pass (withCohortSeedContext, boot-scope only) — a user
// /call, the refresher, the keepwarm sweep, and the discovery walk never install
// one, so the set is a strict no-op off the boot seed path (C-F4B-3). Inert
// under Disabled(): returns ctx unchanged so a cache-off process carries no set.
func WithSeedDeclinedExternalSet(ctx context.Context, set *SeedDeclinedExternalSet) context.Context {
	if ctx == nil || set == nil || Disabled() {
		return ctx
	}
	return context.WithValue(ctx, ctxKeySeedDeclinedExternalSet, set)
}

// SeedDeclinedExternalSetFromContext returns the *SeedDeclinedExternalSet
// installed on ctx, or nil when none is present. A nil return MUST be treated as
// "no set — never skip on this account" (the methods are nil-receiver-safe). Off
// the boot seed path this is always nil, which is what makes the marker provably
// untouched on the /call path and the keepwarm path.
func SeedDeclinedExternalSetFromContext(ctx context.Context) *SeedDeclinedExternalSet {
	if ctx == nil {
		return nil
	}
	v, _ := ctx.Value(ctxKeySeedDeclinedExternalSet).(*SeedDeclinedExternalSet)
	return v
}
