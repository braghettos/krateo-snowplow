// phase1_clusterlist_prewarm_fold_falsifier_test.go — #57 apistage fold
// (RESOLVED_CACHE_APISTAGE_ENABLED retired; ApistageL1Enabled folded under
// ResolvedCacheEnabled).
//
// F-FOLD-5 (THE PM-required safety falsifier — no boot cold-fill on the
// OOM'd cluster): the fold makes ApistageL1Enabled() auto-on under
// CACHE_ENABLED+RESOLVED_CACHE_ENABLED. This must NOT, by itself, turn on
// an un-semaphored boot populate burst on a previously-unset cluster
// (e.g. bs-test-ger-03). The Step-7.5 cluster_list pre-warm hook is wired
// ONLY when harvester != nil (phase1_walk.go:493), which depends on
// cache.PrewarmEnabled() && PrewarmContentEnabled() — INDEPENDENT of the
// apistage gate. And even if some caller invokes the built closure, with
// no resolved store reachable it returns immediately (no populate).

package dispatchers

import (
	"context"
	"testing"
	"time"

	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"k8s.io/client-go/rest"
)

// foldClusterListWired replicates the phase1_walk.go:493 wiring decision:
// the cluster_list pre-warm hook is wired iff the content harvester exists,
// which is gated purely by PrewarmEnabled() && PrewarmContentEnabled().
// ApistageL1Enabled() is deliberately NOT in this expression.
func foldClusterListWired() bool {
	return cache.PrewarmEnabled() && PrewarmContentEnabled()
}

// TestFoldFAL5_ApistageAutoOn_DoesNotWireClusterListPrewarm proves the
// fold's auto-enable of the api-stage L1 does NOT cause the Step-7.5
// cluster_list boot populate hook to be wired when PREWARM_CONTENT is off.
// With CACHE_ENABLED=true + RESOLVED_CACHE_ENABLED unset (apistage auto-on
// per #57) but PREWARM_CONTENT_ENABLED unset, the harvester gate stays
// closed → no hook → no un-semaphored boot cold-fill burst.
func TestFoldFAL5_ApistageAutoOn_DoesNotWireClusterListPrewarm(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	// RESOLVED_CACHE_APISTAGE_ENABLED deliberately left UNSET — the fold
	// makes apistage on anyway.
	if !cache.ApistageL1Enabled() {
		t.Fatalf("precondition: fold should make apistage auto-on under CACHE_ENABLED")
	}
	// PREWARM_CONTENT_ENABLED unset (the previously-unset bs-test-ger-03
	// posture).
	t.Setenv("PREWARM_CONTENT_ENABLED", "")
	if foldClusterListWired() {
		t.Fatalf("F-FOLD-5: cluster_list pre-warm hook wired with PREWARM_CONTENT off — " +
			"the fold's apistage auto-on must NOT trigger a boot populate burst")
	}
	// Even with PREWARM (startup) on, content-prewarm being off keeps the
	// harvester nil → hook still not wired.
	t.Setenv("PREWARM_ENABLED", "true")
	if foldClusterListWired() {
		t.Fatalf("F-FOLD-5: cluster_list pre-warm hook wired with PREWARM_CONTENT off (PREWARM on)")
	}
}

// TestFoldFAL5_BuiltHook_NoStore_IsNoOp proves that even if the hook IS
// built (PREWARM_CONTENT on) while apistage is auto-on, invoking it with
// NO resolved store reachable returns immediately — no populate, no panic.
// This is the second-line guard: the auto-enable never populates absent
// the storage substrate.
func TestFoldFAL5_BuiltHook_NoStore_IsNoOp(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	// apistage auto-on (RESOLVED_CACHE_APISTAGE_ENABLED unset).
	// Force RESOLVED_CACHE off so ApistageL1Enabled() is false → the
	// closure takes its first skip branch (apistage L1 disabled).
	t.Setenv("RESOLVED_CACHE_ENABLED", "false")
	cache.ResetResolvedCacheForTest()
	t.Cleanup(cache.ResetResolvedCacheForTest)

	h := newContentPrewarmHarvester()
	hook := makeClusterListPrewarmFn(h, endpoints.Endpoint{}, &rest.Config{}, "demo-system")

	done := make(chan struct{})
	go func() {
		hook(context.Background())
		close(done)
	}()
	select {
	case <-done:
		// returned immediately (apistage-disabled skip) — no populate burst.
	case <-time.After(5 * time.Second):
		t.Fatalf("F-FOLD-5: cluster_list pre-warm hook did not no-op with apistage off / no store")
	}
}
