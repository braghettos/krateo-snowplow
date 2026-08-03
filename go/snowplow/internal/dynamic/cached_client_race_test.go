// cached_client_race_test.go — Task #322 (#318-R2) Commit 1 falsifier
// F-4: the mandatory concurrent -race test for the private->shared
// conversion (the 0.30.128 hazard class —
// feedback_shared_vs_copy_is_a_concurrency_change). Commit 1 converts
// per-call-PRIVATE dynamic clients (one fresh mapper per
// ValidateObjectStatus) into ONE process-SHARED mapper read by every
// drain-walker + customer-/call goroutine and mutated by the
// CRD-lifecycle bridge worker (InvalidateSADiscovery -> mapper.Reset()).
//
// Per C2 (Q2-a): the invalidator MUST run on a SEPARATE goroutine from
// the concurrent readers — racing Reset()/Invalidate() against KindFor,
// which is the NEW shared-mutation surface, not just N concurrent reads.
//
// Run with: go test -race -count=1 ./internal/dynamic/...
// PASS criterion: zero data races AND no panic under concurrent
// invalidate+read.

package dynamic

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/rest"
)

// TestSharedSADiscoveryClient_RaceConcurrentReadersVsInvalidator races K
// reader goroutines (each repeatedly resolving the cached client's
// mapper via resourceInterfaceFor -> KindFor/RESTMapping) against a
// SEPARATE invalidator goroutine looping InvalidateSADiscovery() (->
// mapper.Reset() -> memCacheClient.Invalidate() + nil delegate). The
// reader and writer touch the SAME shared mapper concurrently.
//
// client-go's Reset is initMu-guarded (discovery.go:216) and
// Invalidate/refreshLocked are lock-guarded (memcache.go:221/235); our
// wrapper adds an RWMutex only for the singleton pointer. -race must
// report clean and no goroutine may panic.
func TestSharedSADiscoveryClient_RaceConcurrentReadersVsInvalidator(t *testing.T) {
	fake := &fakeCachedDiscovery{}
	installFakeCachedDiscovery(t, fake)

	rc := &rest.Config{Host: "https://race.invalid"}

	// Build the singleton once up front so all readers share it.
	if _, err := SharedSADiscoveryClient(rc); err != nil {
		t.Fatalf("initial build errored: %v", err)
	}

	const (
		readers       = 16
		readsPerRdr   = 400
		invalidations = 200
	)

	var (
		wg       sync.WaitGroup
		stop     atomic.Bool
		readErrs atomic.Int64
	)

	// READER goroutines — concurrent KindFor/RESTMapping derefs of the
	// shared mapper.
	wg.Add(readers)
	for r := 0; r < readers; r++ {
		go func() {
			defer wg.Done()
			for i := 0; i < readsPerRdr; i++ {
				cli, err := SharedSADiscoveryClient(rc)
				if err != nil {
					readErrs.Add(1)
					return
				}
				uc, ok := cli.(*unstructuredClient)
				if !ok {
					readErrs.Add(1)
					return
				}
				// Deref the shared mapper. This is the read side of the
				// race. An error here is acceptable IF it coincides with
				// a concurrent Reset (the delegate may be momentarily
				// nil between Invalidate and the next rebuild) — KindFor
				// rebuilds on the next call. We do NOT fail on a returned
				// error; we fail on a data race (the -race detector) or a
				// panic (would crash the test binary).
				_, _ = uc.resourceInterfaceFor(Options{GVR: crdMetaGVR})
			}
		}()
	}

	// INVALIDATOR goroutine — SEPARATE from the readers (C2/Q2-a). Loops
	// InvalidateSADiscovery() concurrently with the reads.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < invalidations && !stop.Load(); i++ {
			InvalidateSADiscovery()
			// A tiny yield so invalidations interleave with reads rather
			// than starving them.
			time.Sleep(time.Microsecond)
		}
	}()

	wg.Wait()
	stop.Store(true)

	// readErrs is diagnostic only — a transient error during a Reset is
	// not a failure. The gate is: no -race report, no panic (the test
	// binary reaching here at all proves no panic). Log for visibility.
	if e := readErrs.Load(); e > 0 {
		t.Logf("note: %d reader calls returned a transient error during a "+
			"concurrent invalidate (acceptable — KindFor rebuilds next call); "+
			"the -race gate is the data-race detector + no-panic", e)
	}
}

// TestSharedSADiscoveryClient_RaceConcurrentBuild races many goroutines
// all calling SharedSADiscoveryClient(rc) for the FIRST time (cold
// singleton) — the build-once-under-write-lock path. -race must report
// clean and all returned Clients must be the SAME pointer (no torn
// double-build leaking distinct mappers).
func TestSharedSADiscoveryClient_RaceConcurrentBuild(t *testing.T) {
	fake := &fakeCachedDiscovery{}
	installFakeCachedDiscovery(t, fake) // resets the singleton

	rc := &rest.Config{Host: "https://race-build.invalid"}

	const goroutines = 32
	var wg sync.WaitGroup
	wg.Add(goroutines)

	results := make([]Client, goroutines)
	errs := make([]error, goroutines)

	start := make(chan struct{})
	for g := 0; g < goroutines; g++ {
		go func(idx int) {
			defer wg.Done()
			<-start // maximise contention on the first build
			results[idx], errs[idx] = SharedSADiscoveryClient(rc)
		}(g)
	}
	close(start)
	wg.Wait()

	var first Client
	for g := 0; g < goroutines; g++ {
		if errs[g] != nil {
			t.Fatalf("goroutine %d errored: %v", g, errs[g])
		}
		if results[g] == nil {
			t.Fatalf("goroutine %d got nil Client", g)
		}
		if first == nil {
			first = results[g]
			continue
		}
		if results[g] != first {
			t.Fatalf("concurrent first-build produced DISTINCT Client pointers "+
				"(goroutine %d=%p vs first=%p) — the build-once contract leaked a "+
				"second mapper", g, results[g], first)
		}
	}

	// Exactly one boot download survived the concurrent build (the
	// winner's mapper; the warm fast-path returns it). Because KindFor is
	// lazy, ServerGroups() has not fired yet (no resolve happened) — so
	// assert <=1 (0 if no reader, 1 if the winner already resolved). We
	// did not resolve here, so it must be 0.
	if got := fake.groupsCallCount(); got != 0 {
		t.Fatalf("ServerGroups() called %d times after concurrent build with no resolve, "+
			"want 0 (KindFor is lazy — discovery downloads on first deref, not on build)", got)
	}
}
