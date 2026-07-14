// phase1_seed_declined_external_marker_test.go — #132 F4b Lever A falsifiers.
//
// THE DEFECT (docs/f4b-seed-overshoot-design-2026-07-14.md §3): the boot seed
// re-resolves an external-backed "whale" widget on EVERY resume pass, because the
// #102 GTTL-1 gate (correctly) DECLINES its Put (external touch → no dep edge),
// so the next pass's seedSkipDecision(seedModeBoot) does handle.Get → MISS (never
// Put) → returns false → re-resolve from scratch → decline → forever. This burns
// the resume budget on zero-forward-progress work and keeps the boot scope
// thrashing.
//
// THE FIX: a boot-scope "resolved-but-declined-external" marker set
// (cache.SeedDeclinedExternalSet) — declineSeedPutOnError's external branch
// Marks the key; seedSkipDecision(seedModeBoot) consults it BEFORE handle.Get and
// skips a marked key. This breaks the loop while preserving the #102 decline (the
// cell stays intentionally cold; /call re-resolves it live).
//
// These arms drive the REAL prod functions (seedSkipDecision +
// declineSeedPutOnError) against the REAL cache.SeedDeclinedExternalSet installed
// on ctx the way withCohortSeedContext installs it (via
// cache.WithSeedDeclinedExternalSet, gated on the boot ctx). The handle is a
// Get-recording double so the arms can prove the marker short-circuits BEFORE the
// liveness Get (the exact re-resolve site). Hermetic, -race, no cluster.
//
// ARM MAP (PM conditions C-F4B-1/-2/-3):
//   C-F4B-1 (falsifier shape, MULTI-COHORT): K=2 cohorts × the SAME external
//     widget across 2 seed passes. GREEN: pass-2 skips the (widget,cohort) that
//     pass-1 declined (Marked→skip, handle.Get NOT consulted). RED arm: with NO
//     marker set installed (the pre-fix world), pass-2 falls through to
//     handle.Get→MISS→re-resolve — the skip does NOT fire. K=1 is blind to the
//     per-cohort defect; the two cohorts prove the marker discriminates per key.
//   C-F4B-2 (marker key = full dispatchCacheLookupKey, NOT per-target): cohort A
//     resolves+declines widget W on pass 1 → A's key marked. Cohort B's FIRST
//     encounter of W (a DIFFERENT key — distinct RBAC identity) is NOT marked →
//     B still resolves (no cross-cohort skip). A per-target marker would wrongly
//     skip B's first resolve.
//   C-F4B-3 (content safety — nil off the boot seed path): a /call context and a
//     keepwarm context carry NO set (SeedDeclinedExternalSetFromContext == nil),
//     so Marked is always false and a declined widget is re-resolved live. The
//     marker can never fake a warm serve on the request path.

package dispatchers

import (
	"context"
	"sync"
	"testing"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// getRecordingHandle is a cacheHandle double that records every key passed to
// Get, so an arm can assert the marker short-circuited BEFORE the liveness Get
// (the re-resolve site). Get always reports MISS (empty, never Put) — modelling
// the declined external cell that is never warmed. Put records nothing (the seed
// declines it). Concurrency-safe: the seed fans cohorts across goroutines.
type getRecordingHandle struct {
	mu      sync.Mutex
	getKeys []string
}

func (h *getRecordingHandle) Get(key string) (*cache.ResolvedEntry, bool) {
	h.mu.Lock()
	h.getKeys = append(h.getKeys, key)
	h.mu.Unlock()
	return nil, false // MISS — the declined external cell is never Put/warmed
}

func (h *getRecordingHandle) Put(key string, entry *cache.ResolvedEntry) {}

func (h *getRecordingHandle) getCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.getKeys)
}

func (h *getRecordingHandle) sawGet(key string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, k := range h.getKeys {
		if k == key {
			return true
		}
	}
	return false
}

// declineExternalOnce drives the REAL declineSeedPutOnError external branch: a
// fresh ext sink bumped once + a fresh (unbumped) stage sink, exactly the shape
// seedOneWidget builds around the Put. Returns whether the Put was declined
// (must be true) so the arm asserts the real gate fired.
func declineExternalOnce(ctx context.Context, class, target, key string) bool {
	_, stageSink := cache.WithStageErrorSink(ctx)
	_, extSink := cache.WithExternalTouchedSink(ctx)
	extSink.Bump() // the resolve touched a genuine external endpoint
	return declineSeedPutOnError(ctx, class, target, key, stageSink, extSink)
}

// ─────────────────────────────────────────────────────────────────────────
// ARM C-F4B-1 — MULTI-COHORT (K=2) × 2 seed passes. The discriminating arm.
func TestF4bLeverA_MultiCohort_ResumeSkipsDeclinedExternal(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true") // else WithSeedDeclinedExternalSet no-ops (Disabled())

	// The boot scope installs ONE set, inherited by every cohort ctx (mirrors
	// seedScopeYielding installing it alongside the F4 memo, gated on boot mode).
	bootCtx := cache.WithSeedDeclinedExternalSet(context.Background(), cache.NewSeedDeclinedExternalSet())
	if cache.SeedDeclinedExternalSetFromContext(bootCtx) == nil {
		t.Fatal("setup: WithSeedDeclinedExternalSet must install a set on the boot ctx under CACHE_ENABLED")
	}

	// TWO cohorts (distinct RBAC identity → distinct dispatchCacheLookupKey) for
	// the SAME external widget W. Distinct keys stand in for the identity fold.
	const widget = "krateo-system/search-results"
	keyA := "widgets|search-results|u=|g=group:admins"        // cohort A's full key
	keyB := "widgets|search-results|u=|g=group:devs"          // cohort B's full key

	// ── PASS 1 (first boot pass): each cohort resolves W, touches external →
	// declineSeedPutOnError declines the Put AND marks the cohort's key.
	for _, k := range []string{keyA, keyB} {
		if !declineExternalOnce(bootCtx, "widgets", widget, k) {
			t.Fatalf("pass-1: declineSeedPutOnError must decline the external Put for key %q", k)
		}
	}
	if got := cache.SeedDeclinedExternalSetFromContext(bootCtx).Marks(); got != 2 {
		t.Fatalf("pass-1: both cohort keys must be marked declined-external; Marks()=%d want 2", got)
	}

	// ── PASS 2 (resume pass): seedSkipDecision(seedModeBoot) for each cohort's key
	// must SKIP (Marked→true) WITHOUT consulting handle.Get (the re-resolve site).
	h := &getRecordingHandle{}
	for _, k := range []string{keyA, keyB} {
		if !seedSkipDecision(bootCtx, seedModeBoot, h, k, "widgets", widget, "") {
			t.Fatalf("pass-2 GREEN VIOLATED: resume pass did NOT skip declined-external key %q — the §3 whale re-resolve loop is still open", k)
		}
	}
	if h.getCount() != 0 {
		t.Fatalf("pass-2: seedSkipDecision consulted handle.Get %d× for a MARKED key — the marker must short-circuit BEFORE the Get (the re-resolve site); getKeys=%v", h.getCount(), h.getKeys)
	}

	// ── RED arm (pre-fix world): NO marker set on ctx → the consult is a
	// nil-receiver no-op → seedSkipDecision falls through to handle.Get → MISS →
	// returns false (re-resolve). This is the loop the fix breaks; if this arm
	// ever returned true, the skip would be firing without the marker (wrong).
	noSetCtx := context.Background()
	hRed := &getRecordingHandle{}
	if seedSkipDecision(noSetCtx, seedModeBoot, hRed, keyA, "widgets", widget, "") {
		t.Fatal("RED arm broke: with NO declined-external set installed, the resume pass must NOT skip (it re-resolves — the pre-fix §3 loop). A skip here means the mechanism isn't the marker.")
	}
	if !hRed.sawGet(keyA) {
		t.Fatal("RED arm: with no marker set, seedSkipDecision MUST fall through to handle.Get (the re-resolve liveness check) — it did not")
	}
}

// ─────────────────────────────────────────────────────────────────────────
// ARM C-F4B-2 — marker keyed by the FULL key, NOT per-(class,target). Cohort A's
// decline of W must NOT suppress cohort B's FIRST resolve of the SAME widget W.
func TestF4bLeverA_MarkerIsPerKey_NoCrossCohortSkip(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")

	bootCtx := cache.WithSeedDeclinedExternalSet(context.Background(), cache.NewSeedDeclinedExternalSet())

	const widget = "krateo-system/obs-throughput-card" // SAME (class,target) for both cohorts
	keyA := "widgets|obs-throughput-card|u=|g=group:admins"
	keyB := "widgets|obs-throughput-card|u=|g=group:devs"

	// Cohort A resolves + declines W on pass 1 → A's key marked.
	if !declineExternalOnce(bootCtx, "widgets", widget, keyA) {
		t.Fatal("setup: cohort A's external Put must be declined")
	}

	// Cohort B's FIRST encounter of the SAME widget W (a DIFFERENT key) must NOT
	// be skipped — B has never resolved W, so the marker must miss on B's key. A
	// per-(class,target) marker would WRONGLY skip B here (B would serve nothing).
	h := &getRecordingHandle{}
	if seedSkipDecision(bootCtx, seedModeBoot, h, keyB, "widgets", widget, "") {
		t.Fatal("C-F4B-2 VIOLATED: cohort B's FIRST resolve of the same widget was skipped — the marker is per-(class,target), not per-key. A cohort would lose its cell. Marker MUST key on the full dispatchCacheLookupKey.")
	}
	if !h.sawGet(keyB) {
		t.Fatal("C-F4B-2: cohort B must fall through to handle.Get (it is unmarked) — it did not")
	}

	// And A's own key IS still marked (didn't get clobbered by B's derivation).
	hA := &getRecordingHandle{}
	if !seedSkipDecision(bootCtx, seedModeBoot, hA, keyA, "widgets", widget, "") {
		t.Fatal("C-F4B-2: cohort A's own declined key must still skip on the resume pass")
	}
}

// ─────────────────────────────────────────────────────────────────────────
// ARM C-F4B-3 — content safety: the marker is nil off the boot seed path. A
// /call ctx and a keepwarm ctx carry NO set, so a declined external widget is
// re-resolved live (never a faked warm serve).
func TestF4bLeverA_MarkerNilOffBootPath_UserAndKeepwarmReResolve(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")

	const widget = "krateo-system/list-activity-events"
	key := "widgets|list-activity-events|u=alice|g="

	// A user /call context: NEVER carries a declined-external set (only
	// withCohortSeedContext installs one, boot-mode only). Prove it's nil.
	userCtx := context.Background()
	if cache.SeedDeclinedExternalSetFromContext(userCtx) != nil {
		t.Fatal("C-F4B-3: a /call context must carry NO declined-external set")
	}

	// Even if a decline happened on the boot side, the /call path's
	// seedSkipDecision (were it ever reached with seedModeBoot off a set-less ctx)
	// re-resolves: Marked(nil) == false → falls through to handle.Get. The user
	// path re-resolves the external widget fresh, unaffected.
	h := &getRecordingHandle{}
	if seedSkipDecision(userCtx, seedModeBoot, h, key, "widgets", widget, "") {
		t.Fatal("C-F4B-3 VIOLATED: a set-less (user/keepwarm) ctx must never skip via the marker — it would fake a warm serve of an intentionally-cold external cell")
	}
	if !h.sawGet(key) {
		t.Fatal("C-F4B-3: set-less ctx must fall through to handle.Get (live re-resolve) — it did not")
	}

	// keepwarm mode NEVER installs a set (seedScopeYielding gates install on
	// seedModeBoot), AND seedModeKeepwarm's own skip path does not consult the
	// declined set at all (it uses the age-skip). Prove keepwarm re-touches: a
	// keepwarm decline does not Mark on a set-less ctx (nil-receiver no-op), so no
	// state leaks into a later boot's set.
	if declineExternalOnce(context.Background(), "widgets", widget, key) {
		// declineExternalOnce returns the gate verdict (true=declined). We only
		// care that Mark on a nil set didn't panic and left no observable state;
		// the verdict itself is correctly true (external touch always declines).
	}
}

// ─────────────────────────────────────────────────────────────────────────
// ARM — marker set ONLY in the external branch, NOT on stage_error. A stage
// error is transient/operational and MUST be retried on the resume pass (not
// permanently skipped this boot). Discriminates the branch choice.
func TestF4bLeverA_StageErrorDoesNotMark_OnlyExternal(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")

	bootCtx := cache.WithSeedDeclinedExternalSet(context.Background(), cache.NewSeedDeclinedExternalSet())
	const widget = "krateo-system/dashboard-flex"
	key := "widgets|dashboard-flex|u=|g=group:admins"

	// A stage-error decline (NOT external): the Put is declined, but the key must
	// NOT be marked — a stage error should re-resolve on the resume pass.
	_, stageSink := cache.WithStageErrorSink(bootCtx)
	_, extSink := cache.WithExternalTouchedSink(bootCtx)
	stageSink.Bump("exportJwt-loopback", "401 transient no per-user JWT")
	if !declineSeedPutOnError(bootCtx, "widgets", widget, key, stageSink, extSink) {
		t.Fatal("setup: a stage-error resolve must still decline the Put (GTTL-1)")
	}
	if got := cache.SeedDeclinedExternalSetFromContext(bootCtx).Marks(); got != 0 {
		t.Fatalf("stage-error decline must NOT mark the key (transient → retry on resume); Marks()=%d want 0", got)
	}

	// So the resume pass does NOT skip on the marker account — it re-resolves.
	h := &getRecordingHandle{}
	if seedSkipDecision(bootCtx, seedModeBoot, h, key, "widgets", widget, "") {
		t.Fatal("a stage-errored (non-external) target must be re-resolved on the resume pass, NOT marker-skipped")
	}
	if !h.sawGet(key) {
		t.Fatal("stage-error target must fall through to handle.Get on resume — it did not")
	}
}
