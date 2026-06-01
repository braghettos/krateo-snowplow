// crdwatch_walker_spawn_test.go — Ship 0 / 0.30.222 falsifier corpus for
// the walker-driven CRD-informer spawn invariant.
//
// Diego's invariant (2026-06-01): "no CRD informer if the CRD object
// itself is not walked in frontend navigation." Before Ship 0 the CRD
// informer spawned at boot via MetaQuerySeeds() regardless of whether
// frontend navigation reached a CRD object. Ship 0 moves the spawn into
// AddAutoDiscoverGroup's sync.Once side-effect — fire IFF the walker
// touches a templated apiserver path.
//
// This file is the SOLE test that asserts the invariant directly. The
// existing TestMetaQuerySeeds_ExactBudget catches a count-mismatch but
// not the semantic "the seed set must not contain a walker-undiscoverable
// boot primordial." If a future regression re-adds customResourceDefin-
// itionGVR to MetaQuerySeeds(), that older test trips on the COUNT but
// this corpus's NoTemplatedPaths_NoCRDInformer subtest is the one that
// names the invariant.
//
// Package internal so the tests can reach ResetCRDInformerSpawnedForTest
// + customResourceDefinitionGVR + the handler-extension counter.

package cache

import (
	"context"
	"sync"
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// walkerSpawnTestScheme returns a scheme with RBAC types registered so
// the dynamic fake client can decode informer LISTs. Internal-package
// equivalent of cache_test's newTestScheme.
func walkerSpawnTestScheme() *k8sruntime.Scheme {
	sch := k8sruntime.NewScheme()
	_ = rbacv1.AddToScheme(sch)
	return sch
}

// crdWalkerSpawnSetup spins up a CACHE_ENABLED=true watcher backed by a
// fake dynamic client + the RBAC List-kind set. Returns the watcher and a
// cleanup. Every subtest needs this fixture; pulling it out keeps the
// test bodies focused on the invariant assertions.
func crdWalkerSpawnSetup(t *testing.T) *ResourceWatcher {
	t.Helper()
	t.Setenv("CACHE_ENABLED", "true")
	ResetAutoDiscoverGroupsForTest()
	ResetCRDInformerSpawnedForTest()
	t.Cleanup(ResetAutoDiscoverGroupsForTest)
	t.Cleanup(ResetCRDInformerSpawnedForTest)

	listKinds := map[schema.GroupVersionResource]string{
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}:                "RoleList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}:         "RoleBindingList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}:         "ClusterRoleList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}: "ClusterRoleBindingList",
		customResourceDefinitionGVR:                                                            "CustomResourceDefinitionList",
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(walkerSpawnTestScheme(), listKinds)
	rw, err := NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	if rw == nil {
		t.Fatalf("expected non-nil watcher under CACHE_ENABLED=true")
	}
	t.Cleanup(func() {
		rw.Stop()
		time.Sleep(50 * time.Millisecond)
	})
	SetGlobal(rw)
	t.Cleanup(func() { SetGlobal(nil) })
	return rw
}

// extensionCountSnapshot captures the cache.handler_extensions_attached
// counter for the CRD-watch extension at a single instant. The Ship 0
// falsifier asserts:
//
//	NoTemplatedPaths_NoCRDInformer:     counter == 0
//	SingleTemplatedPath_OneSpawn:       counter == 1
//	MultipleTemplatedPaths_StillOneSpawn: counter == 1
//	RaceSafe_ConcurrentAddAutoDiscoverGroup: counter == 1
//	ReconcileNoOp_OnEmptyAutoDiscover:   counter == 0
//
// The extension's name is the same string crdwatch.init() declares.
const crdwatchExtensionName = "crdwatch.composition_auto_discovery"

func extensionCountSnapshot() uint64 {
	return HandlerExtensionsAttachedCount(crdwatchExtensionName)
}

// TestCRDInformer_NotSpawnedWithoutTemplatedRA is the v5 §6.5 falsifier
// corpus. Each subtest exercises one row of the §6.5 table.
func TestCRDInformer_NotSpawnedWithoutTemplatedRA(t *testing.T) {

	// SUBTEST 1 — NoTemplatedPaths_NoCRDInformer.
	//
	// Walker corpus has only static apiserver paths (no `${...}`) — none
	// of which extract a templated group via
	// ExtractAPIServerGroupFromTemplatedPath. AddAutoDiscoverGroup is
	// therefore never called → sync.Once stays untouched → CRD informer
	// must NOT be in rw.informers and the handler-extension counter must
	// be 0.
	t.Run("NoTemplatedPaths_NoCRDInformer", func(t *testing.T) {
		rw := crdWalkerSpawnSetup(t)
		before := extensionCountSnapshot()

		// Static paths only — no walker code path reaches AddAutoDiscoverGroup.
		staticPaths := []string{
			"/api/v1/namespaces/default/configmaps/foo",
			"/api/v1/namespaces/default/secrets/bar",
			"/apis/apps/v1/namespaces/default/deployments/baz",
		}
		for _, p := range staticPaths {
			// We don't actually call the walker — we just demonstrate the
			// extraction would return ok=false (no templated group) for
			// each path. The walker source only invokes
			// AddAutoDiscoverGroup when extraction succeeds.
			if grp, ok := ExtractAPIServerGroupFromTemplatedPath(p); ok && grp != "" {
				// Static-path extraction may succeed (it pulls "apps" out
				// of /apis/apps/...); the assertion is that the WALKER
				// gates AddAutoDiscoverGroup behind a templated form. For
				// this subtest we DELIBERATELY never call
				// AddAutoDiscoverGroup so the invariant under test —
				// "without an AddAutoDiscoverGroup call the CRD informer
				// does not spawn" — is exercised cleanly.
				_ = grp
			}
		}

		if got := AutoDiscoverGroupsSnapshot(); len(got) != 0 {
			t.Fatalf("auto-discover set must be empty when no walker calls AddAutoDiscoverGroup; got %v", got)
		}
		if rw.IsRegistered(customResourceDefinitionGVR) {
			t.Fatalf("Ship 0 invariant violated: CRD informer registered without any AddAutoDiscoverGroup call")
		}
		if got := extensionCountSnapshot() - before; got != 0 {
			t.Fatalf("handler-extension counter delta = %d, want 0 (no spawn)", got)
		}
	})

	// SUBTEST 2 — SingleTemplatedPath_OneSpawn.
	//
	// One AddAutoDiscoverGroup call (modelling the walker reaching one
	// templated RESTAction path) must:
	//   - populate the auto-discover set with the named group,
	//   - spawn the CRD informer (sync.Once fires),
	//   - attach the crdwatch.composition_auto_discovery handler set
	//     (handler-extension counter ticks to 1).
	t.Run("SingleTemplatedPath_OneSpawn", func(t *testing.T) {
		rw := crdWalkerSpawnSetup(t)
		before := extensionCountSnapshot()

		// Model the walker call site: resolver parsed a templated path
		// and pulled out the static group.
		grp, ok := ExtractAPIServerGroupFromTemplatedPath(
			"/apis/composition.krateo.io/${.spec.version}/namespaces/${.ns}/${.kind}",
		)
		if !ok {
			t.Fatalf("ExtractAPIServerGroupFromTemplatedPath must succeed on a templated composition path")
		}
		AddAutoDiscoverGroup(grp)

		if got := AutoDiscoverGroupsSnapshot(); len(got) != 1 || got[0] != "composition.krateo.io" {
			t.Fatalf("auto-discover set = %v, want [composition.krateo.io]", got)
		}
		if !rw.IsRegistered(customResourceDefinitionGVR) {
			t.Fatalf("Ship 0 invariant violated: AddAutoDiscoverGroup did not spawn the CRD informer")
		}
		if got := extensionCountSnapshot() - before; got != 1 {
			t.Fatalf("handler-extension counter delta = %d, want 1 (single spawn)", got)
		}
	})

	// SUBTEST 3 — MultipleTemplatedPaths_StillOneSpawn.
	//
	// N=10 AddAutoDiscoverGroup calls across 3 groups must spawn the CRD
	// informer EXACTLY ONCE (sync.Once invariant). The auto-discover set
	// grows to the unique-group count; the counter stays at 1.
	t.Run("MultipleTemplatedPaths_StillOneSpawn", func(t *testing.T) {
		rw := crdWalkerSpawnSetup(t)
		before := extensionCountSnapshot()

		groups := []string{
			"composition.krateo.io",
			"composition.krateo.io", // duplicate — auto-discover dedups
			"widgets.templates.krateo.io",
			"widgets.templates.krateo.io",
			"templates.krateo.io",
			"composition.krateo.io",
			"templates.krateo.io",
			"widgets.templates.krateo.io",
			"composition.krateo.io",
			"templates.krateo.io",
		}
		for _, g := range groups {
			AddAutoDiscoverGroup(g)
		}

		uniques := AutoDiscoverGroupsSnapshot()
		if len(uniques) != 3 {
			t.Fatalf("auto-discover set len = %d, want 3 unique groups; got %v", len(uniques), uniques)
		}
		if !rw.IsRegistered(customResourceDefinitionGVR) {
			t.Fatalf("Ship 0 invariant violated: CRD informer must be registered after the first AddAutoDiscoverGroup")
		}
		if got := extensionCountSnapshot() - before; got != 1 {
			t.Fatalf("handler-extension counter delta = %d, want 1 (sync.Once invariant — exactly one spawn across N calls)", got)
		}
	})

	// SUBTEST 4 — RaceSafe_ConcurrentAddAutoDiscoverGroup.
	//
	// 100 goroutines call AddAutoDiscoverGroup simultaneously. Under
	// `-race`:
	//   - No data race on autoDiscoverGroups (autoDiscoverMu serializes).
	//   - CRD informer spawns EXACTLY ONCE (crdInformerSpawned sync.Once).
	//   - Handler-extension counter ticks exactly ONCE.
	t.Run("RaceSafe_ConcurrentAddAutoDiscoverGroup", func(t *testing.T) {
		rw := crdWalkerSpawnSetup(t)
		before := extensionCountSnapshot()

		const goroutines = 100
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < goroutines; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				<-start
				// Mix of duplicate and distinct groups exercises the
				// dedup mutex AND the spawn sync.Once.
				if idx%3 == 0 {
					AddAutoDiscoverGroup("composition.krateo.io")
				} else if idx%3 == 1 {
					AddAutoDiscoverGroup("widgets.templates.krateo.io")
				} else {
					AddAutoDiscoverGroup("templates.krateo.io")
				}
			}(i)
		}
		close(start)
		wg.Wait()

		if !rw.IsRegistered(customResourceDefinitionGVR) {
			t.Fatalf("Ship 0 invariant violated: concurrent AddAutoDiscoverGroup did not spawn the CRD informer")
		}
		if got := extensionCountSnapshot() - before; got != 1 {
			t.Fatalf("handler-extension counter delta = %d, want 1 (sync.Once under concurrent goroutines)", got)
		}
		if got := AutoDiscoverGroupsSnapshot(); len(got) != 3 {
			t.Fatalf("auto-discover set len = %d, want 3 unique groups after concurrent calls", len(got))
		}
	})

	// SUBTEST 5 — ReconcileNoOp_OnEmptyAutoDiscover.
	//
	// Calling ReconcileAutoDiscoverCRDs with NO AddAutoDiscoverGroup
	// preceding it must:
	//   - return 0 (no CRD informer exists → graceful no-op via
	//     crdwatch.go's nil-informer guard);
	//   - NOT spawn the CRD informer (the reconcile is a passive
	//     re-scan, not a spawn trigger);
	//   - NOT panic;
	//   - leave the handler-extension counter at 0.
	t.Run("ReconcileNoOp_OnEmptyAutoDiscover", func(t *testing.T) {
		rw := crdWalkerSpawnSetup(t)
		before := extensionCountSnapshot()

		registered := rw.ReconcileAutoDiscoverCRDs()
		if registered != 0 {
			t.Fatalf("ReconcileAutoDiscoverCRDs on empty auto-discover set = %d, want 0", registered)
		}
		if rw.IsRegistered(customResourceDefinitionGVR) {
			t.Fatalf("Ship 0 invariant violated: ReconcileAutoDiscoverCRDs spawned the CRD informer; " +
				"reconcile must be a passive re-scan, not a spawn trigger")
		}
		if got := AutoDiscoverGroupsSnapshot(); len(got) != 0 {
			t.Fatalf("auto-discover set must remain empty post-reconcile; got %v", got)
		}
		if got := extensionCountSnapshot() - before; got != 0 {
			t.Fatalf("handler-extension counter delta = %d, want 0", got)
		}
	})
}
