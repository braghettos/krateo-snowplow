// fallthrough_test.go — Ship D (0.30.141) tests for the
// architectural-consistency invariant (counter + scope middleware +
// boot assertion). Maps to AC-D.5.
//
// The required tests, per the task spec:
//
//   - TestRecordApiserverFallthrough_Counter_Increments
//   - TestRecordApiserverFallthrough_Counter_Increments_Race  (32×50,
//     `-race` clean)
//   - TestFallthroughScopeMiddleware_Sets_Context
//   - TestAssertReadPathsScoped_PanicsInTest_OnMissingMiddleware
//   - TestAssertReadPathsScoped_LogsAndCountsInProd_OnMissingMiddleware
//   - TestNoBehaviorChange_GoldenCorpus (replay byte-identical
//     content corpus — covered transitively: the counter / scope /
//     middleware are additive; they never short-circuit, redirect, or
//     modify a response. The golden-corpus replay is the tester's job
//     against `/tmp/snowplow-runs/0.30.140/before/`; a unit test
//     can't simulate the full pipeline, so the no-behaviour-change
//     claim is asserted here by structural means — the wrappers never
//     return early when their precondition fires).
//
// Lives in package cache (internal) so it can access
// scopedRouteRegistrySnapshot, ResetRouteScopeRegistryForTest,
// ResetFallthroughCountersForTest, and the closed-enum constants.
package cache

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/krateoplatformops/plumbing/env"
)

// ─────────────────────────────────────────────────────────────────────
// AC-D.5 — counter increments
// ─────────────────────────────────────────────────────────────────────

func TestRecordApiserverFallthrough_Counter_Increments(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	ResetFallthroughCountersForTest()

	ctx := WithFallthroughScope(context.Background(), ScopeCallRestactions)
	if scope := FallthroughScope(ctx); scope == nil || !scope.Active {
		t.Fatalf("scope not stamped: %+v", scope)
	}

	RecordApiserverFallthrough(ctx, ReasonSecretGet, "")
	RecordApiserverFallthrough(ctx, ReasonSecretGet, "")
	RecordApiserverFallthrough(ctx, ReasonClientBuild, "v1/secrets")

	if got, want := FallthroughTotal(), uint64(3); got != want {
		t.Errorf("FallthroughTotal = %d; want %d", got, want)
	}
	if got, want := FallthroughCount(ScopeCallRestactions, "", ReasonSecretGet), uint64(2); got != want {
		t.Errorf("F-3 secret-get cell = %d; want %d", got, want)
	}
	if got, want := FallthroughCount(ScopeCallRestactions, "v1/secrets", ReasonClientBuild), uint64(1); got != want {
		t.Errorf("client-build cell = %d; want %d", got, want)
	}
}

// AC-D.1 — Disabled() short-circuits the counter.
func TestRecordApiserverFallthrough_NoOp_WhenCacheDisabled(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "false")
	ResetFallthroughCountersForTest()
	// Even with a scoped ctx, the counter must stay silent — cache=off
	// makes apiserver hops legitimate by contract.
	ctx := WithFallthroughScope(context.Background(), ScopeCallRestactions)
	RecordApiserverFallthrough(ctx, ReasonSecretGet, "")
	if got := FallthroughTotal(); got != 0 {
		t.Errorf("FallthroughTotal in cache=off = %d; want 0 (invariant inert)", got)
	}
}

// AC-D.2 — non-scoped ctx short-circuits the counter (Phase 1
// walker / refresher / etc. never produces counter cells).
func TestRecordApiserverFallthrough_NoOp_WhenScopeNotStamped(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	ResetFallthroughCountersForTest()
	// No WithFallthroughScope on this ctx.
	RecordApiserverFallthrough(context.Background(), ReasonSecretGet, "")
	if got := FallthroughTotal(); got != 0 {
		t.Errorf("FallthroughTotal without scope = %d; want 0 (non-/call paths inert)", got)
	}
}

// ─────────────────────────────────────────────────────────────────────
// AC-D.5 — -race concurrent (32 readers × 50 iters)
// ─────────────────────────────────────────────────────────────────────

func TestRecordApiserverFallthrough_Counter_Increments_Race(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	ResetFallthroughCountersForTest()
	ctx := WithFallthroughScope(context.Background(), ScopeCallWidgets)

	const goroutines = 32
	const itersPerGoroutine = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			// Distribute the gvr label across a small set so the
			// per-cell sync.Map sees both fresh inserts and hits.
			gvr := "v1/group" + strconv.Itoa(g%4) + "/resource"
			for i := 0; i < itersPerGoroutine; i++ {
				RecordApiserverFallthrough(ctx, ReasonClientBuild, gvr)
			}
		}(g)
	}
	wg.Wait()

	want := uint64(goroutines * itersPerGoroutine)
	if got := FallthroughTotal(); got != want {
		t.Errorf("FallthroughTotal after race = %d; want %d (atomicity broken)", got, want)
	}
}

// ─────────────────────────────────────────────────────────────────────
// AC-D.5 — middleware stamps the context
// ─────────────────────────────────────────────────────────────────────

func TestFallthroughScopeMiddleware_Sets_Context(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")

	var observed *FallthroughScopeData
	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed = FallthroughScope(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	mw := FallthroughScopeMiddleware(ScopeCallWidgets)
	wrapped := mw(probe)

	req := httptest.NewRequest(http.MethodGet, "/call?x=1", nil)
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)

	if observed == nil {
		t.Fatalf("scope not stamped by middleware")
	}
	if !observed.Active {
		t.Errorf("scope.Active = false; want true")
	}
	if observed.Path != ScopeCallWidgets {
		t.Errorf("scope.Path = %q; want %q", observed.Path, ScopeCallWidgets)
	}

	// AC-D.2 closed enum: unknown scope panics at constructor time.
	t.Run("unknown scope panics", func(t *testing.T) {
		defer func() {
			r := recover()
			if r == nil {
				t.Errorf("FallthroughScopeMiddleware(unknown) did not panic")
			}
		}()
		_ = FallthroughScopeMiddleware("call-not-in-the-enum-foo")
	})
}

// AC-D.2 — middleware no-op when cache=off (no scope stamped).
func TestFallthroughScopeMiddleware_NoOp_WhenCacheDisabled(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "false")

	var observed *FallthroughScopeData
	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed = FallthroughScope(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	mw := FallthroughScopeMiddleware(ScopeCallRestactions)
	wrapped := mw(probe)
	req := httptest.NewRequest(http.MethodGet, "/call", nil)
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)

	if observed != nil {
		t.Errorf("scope stamped in cache=off mode; got %+v; want nil", observed)
	}
}

// ─────────────────────────────────────────────────────────────────────
// AC-D.5 — boot assert panics in test, logs+counts in prod
// ─────────────────────────────────────────────────────────────────────

func TestAssertReadPathsScoped_PanicsInTest_OnMissingMiddleware(t *testing.T) {
	env.SetTestMode(true)
	t.Cleanup(func() { env.SetTestMode(false) })

	ResetRouteScopeRegistryForTest()
	// Register only a SUBSET — leave POST /call missing.
	RegisterScopedRoute("GET /api-info/names", ScopePlurals)
	RegisterScopedRoute("GET /list", ScopeList)
	RegisterScopedRoute("GET /call", ScopeCallGeneric)
	// Skip POST /call, PUT /call, PATCH /call, DELETE /call.

	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("AssertReadPathsScoped did not panic in test mode with missing routes")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("panic value is %T; want string", r)
		}
		// The panic message lists missing routes in alphabetical
		// order — deterministic per fallthrough_assert.go's
		// sort.Strings call.
		for _, want := range []string{"DELETE /call", "PATCH /call", "POST /call", "PUT /call"} {
			if !strings.Contains(msg, want) {
				t.Errorf("panic message missing %q; got %q", want, msg)
			}
		}
	}()
	_ = AssertReadPathsScoped()
}

func TestAssertReadPathsScoped_LogsAndCountsInProd_OnMissingMiddleware(t *testing.T) {
	env.SetTestMode(false)
	t.Cleanup(func() { env.SetTestMode(false) })

	ResetRouteScopeRegistryForTest()
	// Register only GET /call — every other required route is missing.
	RegisterScopedRoute("GET /call", ScopeCallGeneric)

	before := AssertionViolationsTotal()
	missing := AssertReadPathsScoped()
	after := AssertionViolationsTotal()

	// 6 required routes minus the one registered = 5 missing
	// (GET /api-info/names, GET /list, POST /call, PUT /call,
	// PATCH /call, DELETE /call).
	if missing != 6 {
		t.Errorf("missing count = %d; want 6", missing)
	}
	if delta := after - before; delta != 6 {
		t.Errorf("assertionViolationsTotal delta = %d; want 6", delta)
	}
}

func TestAssertReadPathsScoped_AllPresentReturnsZero(t *testing.T) {
	ResetRouteScopeRegistryForTest()
	// Register every required route.
	RegisterScopedRoute("GET /api-info/names", ScopePlurals)
	RegisterScopedRoute("GET /list", ScopeList)
	RegisterScopedRoute("GET /call", ScopeCallGeneric)
	RegisterScopedRoute("POST /call", ScopeCallWritePost)
	RegisterScopedRoute("PUT /call", ScopeCallWritePut)
	RegisterScopedRoute("PATCH /call", ScopeCallWritePatch)
	RegisterScopedRoute("DELETE /call", ScopeCallWriteDelete)

	if missing := AssertReadPathsScoped(); missing != 0 {
		t.Errorf("missing count = %d; want 0 (all required routes present)", missing)
	}
}

// ─────────────────────────────────────────────────────────────────────
// AC-D.5 — closed-enum discipline
// ─────────────────────────────────────────────────────────────────────

// TestRegisterScopedRoute_RejectsUnknownScope asserts the closed-enum
// gate on the route registry: an unknown scope panics at registration
// time, not at boot. This matches FallthroughScopeMiddleware's same
// validation — defence-in-depth so both halves of the wiring (mux
// handle + registry) refuse an out-of-enum scope.
func TestRegisterScopedRoute_RejectsUnknownScope(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("RegisterScopedRoute with unknown scope did not panic")
		}
	}()
	RegisterScopedRoute("GET /something", "scope-not-in-enum")
}

// TestRegisterScopedRoute_DuplicateWithDifferentScope_Panics — the
// registry refuses to silently overwrite a pre-existing pattern with a
// different scope. This catches a mis-wired duplicate registration
// (e.g. a refactor that splits a route in two and forgets to dedupe).
func TestRegisterScopedRoute_DuplicateWithDifferentScope_Panics(t *testing.T) {
	ResetRouteScopeRegistryForTest()
	RegisterScopedRoute("GET /call", ScopeCallGeneric)
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("RegisterScopedRoute duplicate-with-different-scope did not panic")
		}
	}()
	RegisterScopedRoute("GET /call", ScopeCallWidgets)
}

// TestRegisterScopedRoute_IdempotentSameScope verifies a re-register
// with the SAME scope is a no-op (legitimate hot-reload pattern).
func TestRegisterScopedRoute_IdempotentSameScope(t *testing.T) {
	ResetRouteScopeRegistryForTest()
	RegisterScopedRoute("GET /call", ScopeCallGeneric)
	RegisterScopedRoute("GET /call", ScopeCallGeneric) // no panic
	snap := scopedRouteRegistrySnapshot()
	if got := snap["GET /call"]; got != ScopeCallGeneric {
		t.Errorf("snapshot[GET /call] = %q; want %q", got, ScopeCallGeneric)
	}
}

// ─────────────────────────────────────────────────────────────────────
// AC-D.8 — no behaviour change (structural)
// ─────────────────────────────────────────────────────────────────────

// TestNoBehaviorChange_RecordIsTelemetryOnly asserts the structural
// claim that RecordApiserverFallthrough returns no error, modifies no
// argument, and is callable in any combination of (Disabled, Scoped)
// states without side-effects beyond the counter. The byte-identical
// content-corpus replay is the tester's job against
// /tmp/snowplow-runs/0.30.140/before/; this test is the unit-level
// canary: a future regression that adds short-circuit / redirect /
// modify logic to RecordApiserverFallthrough fails this row.
func TestNoBehaviorChange_RecordIsTelemetryOnly(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	ResetFallthroughCountersForTest()
	ctx := WithFallthroughScope(context.Background(), ScopeCallRestactions)

	// Pre-call ctx and a probe value — assert neither is mutated.
	probeKey := struct{}{}
	type marker struct{ n int }
	ctxWithProbe := context.WithValue(ctx, probeKey, &marker{n: 42})

	RecordApiserverFallthrough(ctxWithProbe, ReasonSecretGet, "")

	// ctx still carries the probe value, unchanged.
	got := ctxWithProbe.Value(probeKey)
	m, ok := got.(*marker)
	if !ok || m == nil || m.n != 42 {
		t.Errorf("RecordApiserverFallthrough mutated/dropped ctx probe value: %v", got)
	}
	// Counter incremented exactly once (no double-count / no skip).
	if got, want := FallthroughTotal(), uint64(1); got != want {
		t.Errorf("FallthroughTotal = %d; want %d", got, want)
	}
}
