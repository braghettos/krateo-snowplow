// schema_cache_race_test.go — Task #323 (#318-R2 Commit 2-B) the mandatory
// concurrent -race test for the private->shared conversion (the 0.30.128
// hazard class — feedback_shared_vs_copy_is_a_concurrency_change). Commit 2-B
// converts a per-call-PRIVATE recompiled CRV (one fresh compile per
// ValidateObjectStatus) into ONE process-SHARED cached CRV read by every
// drain-walker + customer-/call goroutine and RESET by the CRD-lifecycle
// bridge worker (InvalidateCRDSchemaMemo).
//
// The invalidator MUST run on a SEPARATE goroutine from the concurrent memo
// readers — racing the full-reset Range/Delete against Load/Store, the NEW
// shared-mutation surface (not just N concurrent reads).
//
// Run with: go test -race -count=1 -run TestCRDSchemaMemo_Race ./internal/resolvers/crds/schema/
// PASS criterion: zero data races AND no panic under concurrent reset+read.
//
// (Run -run-filtered to skip the pre-existing #312 TestExtractOpenAPISchemaFromCRD
// panic in this package — out of scope; see the deliverable.)

package schema

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	runtimeschema "k8s.io/apimachinery/pkg/runtime/schema"
)

// TestCRDSchemaMemo_RaceConcurrentReadersVsInvalidator races K reader
// goroutines (each repeatedly lookup-or-store-ing the per-GVR memo and READING
// the cached CRV's OpenAPIV3Schema, exactly as validateCustomResource does)
// against a SEPARATE invalidator goroutine looping InvalidateCRDSchemaMemo()
// (the full Range/Delete reset). The reader and writer touch the SAME sync.Map
// + the SAME shared *CRV concurrently.
//
// sync.Map is race-safe by construction; the cached *CRV is immutable after
// store (readers only read crv.OpenAPIV3Schema). -race must report clean and
// no goroutine may panic.
func TestCRDSchemaMemo_RaceConcurrentReadersVsInvalidator(t *testing.T) {
	resetCRDSchemaMemoForTest()
	t.Cleanup(resetCRDSchemaMemoForTest)

	crd := tinyCRD("v1-2-2")
	// Pre-build one compiled CRV the readers store/share.
	seed, err := extractOpenAPISchemaFromCRD(crd, "v1-2-2")
	if err != nil {
		t.Fatalf("seed compile errored: %v", err)
	}

	// A small set of GVR keys so the readers contend on the same entries the
	// invalidator deletes.
	keys := []runtimeschema.GroupVersionResource{
		{Group: "composition.krateo.io", Version: "v1-2-2", Resource: "r0"},
		{Group: "composition.krateo.io", Version: "v1-2-2", Resource: "r1"},
		{Group: "composition.krateo.io", Version: "v1-2-2", Resource: "r2"},
		{Group: "composition.krateo.io", Version: "v1-2-2", Resource: "r3"},
	}

	const (
		readers       = 16
		opsPerRdr     = 400
		invalidations = 200
	)

	var (
		wg   sync.WaitGroup
		stop atomic.Bool
	)

	// READER goroutines — lookup-or-store + read the shared CRV's schema (the
	// read side of the race, mirroring validateCustomResource's
	// NewSchemaValidator(crv.OpenAPIV3Schema)).
	wg.Add(readers)
	for r := 0; r < readers; r++ {
		go func(rid int) {
			defer wg.Done()
			for i := 0; i < opsPerRdr; i++ {
				k := keys[(rid+i)%len(keys)]
				crv, hit := lookupCRDSchema(k)
				if !hit {
					// Miss (entry was just reset) — store a fresh CRV under the
					// generation fence, exactly as ValidateObjectStatus does on
					// a miss (snapshot gen, then store; a concurrent reset may
					// drop the store, which is fine — seed is identical anyway).
					gen := currentSchemaGen()
					storeCRDSchema(k, seed, gen)
					crv = seed
				}
				if crv != nil {
					// READ the shared CRV's schema — the immutable-after-store
					// read validateCustomResource performs. Must not race with
					// the concurrent reset (reset deletes the map entry; it
					// does NOT mutate the *CRV).
					_ = crv.OpenAPIV3Schema
					_ = readSchemaType(crv)
				}
			}
		}(r)
	}

	// INVALIDATOR goroutine — SEPARATE from the readers. Loops the full-reset
	// InvalidateCRDSchemaMemo() concurrently with the reads.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < invalidations && !stop.Load(); i++ {
			InvalidateCRDSchemaMemo()
			time.Sleep(time.Microsecond) // interleave, don't starve
		}
	}()

	wg.Wait()
	stop.Store(true)
	// Reaching here at all proves no panic; the -race detector is the
	// data-race gate.
}

// readSchemaType reads a field off the shared CRV — a representative
// immutable read to ensure the race detector observes concurrent reads of the
// shared value while the invalidator deletes map entries.
func readSchemaType(crv *apiextensions.CustomResourceValidation) string {
	if crv == nil || crv.OpenAPIV3Schema == nil {
		return ""
	}
	return string(crv.OpenAPIV3Schema.Type)
}

// markerCRV builds a CRV whose OpenAPIV3Schema.Type is a DISTINGUISHABLE marker
// ("OLD"/"NEW"), so a stale re-install is visible (unlike the -race test which
// stores an identical seed on every miss — staleness is invisible there). This
// is what makes the A1 fence falsifiable.
func markerCRV(mark string) *apiextensions.CustomResourceValidation {
	return &apiextensions.CustomResourceValidation{
		OpenAPIV3Schema: &apiextensions.JSONSchemaProps{Type: mark},
	}
}

// TestCRDSchemaMemo_GenerationFence_DropsStaleInflightStore is the architect-C
// deterministic ordered-interleaving regression test for the A1 generation
// fence. It reproduces the exact invalidate-vs-inflight-fill timeline WITHOUT
// goroutines (fully ordered, so deterministic):
//
//	t0: inflight ValidateObjectStatus for GVR X misses -> snapshots gen0
//	t1: it does its CRD GET (reads OLD bytes) + compiles CRV_OLD
//	t2: a CRD UPDATE lands -> the bridge fires InvalidateCRDSchemaMemo()
//	    (bumps the generation, clears the map)
//	t3: the inflight call stores CRV_OLD with the STALE gen0
//
// CORRECT (fenced): the t3 store is DROPPED (gen moved) -> X is NOT installed
// -> the next miss recompiles CRV_NEW. RED against pre-fence code (sticky
// LoadOrStore with no gen check): CRV_OLD is installed and PERSISTS (a second
// fresh store does not overwrite it) -> a hit serves the stale "OLD" schema for
// the process lifetime.
//
// RED-EVIDENCE NOTE: against the pre-fence storeCRDSchema (2-arg LoadOrStore, no
// generation parameter) this test does not compile; toggling the fence body OFF
// (making storeCRDSchema a plain LoadOrStore that ignores gen) makes it FAIL on
// the "stale OLD installed" assertion — captured in the deliverable.
func TestCRDSchemaMemo_GenerationFence_DropsStaleInflightStore(t *testing.T) {
	resetCRDSchemaMemoForTest()
	t.Cleanup(resetCRDSchemaMemoForTest)

	// Seam the compile step so successive misses yield DISTINGUISHABLE CRVs:
	// first compile => OLD, subsequent => NEW (simulating the post-UPDATE
	// recompile reading fresh bytes).
	var compiles int
	orig := compileCRDSchemaFn
	compileCRDSchemaFn = func(_ map[string]any, _ string) (*apiextensions.CustomResourceValidation, error) {
		compiles++
		if compiles == 1 {
			return markerCRV("OLD"), nil
		}
		return markerCRV("NEW"), nil
	}
	t.Cleanup(func() { compileCRDSchemaFn = orig })

	x := runtimeschema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-2-2", Resource: "fencepanels"}
	crd := tinyCRD("v1-2-2")

	// t0: inflight call misses, snapshots gen BEFORE its "CRD GET".
	if _, hit := lookupCRDSchema(x); hit {
		t.Fatalf("precondition: X must miss on a fresh memo")
	}
	gen0 := currentSchemaGen()

	// t1: it compiles CRV_OLD from (stale) bytes.
	crvOld, err := compileCRDSchemaFn(crd, "v1-2-2")
	if err != nil {
		t.Fatalf("compile OLD errored: %v", err)
	}
	if got := readSchemaType(crvOld); got != "OLD" {
		t.Fatalf("setup: first compile should be OLD, got %q", got)
	}

	// t2: the bridge reset lands DURING the inflight call's window.
	InvalidateCRDSchemaMemo()

	// t3: the inflight call stores CRV_OLD with the now-STALE gen0.
	storeCRDSchema(x, crvOld, gen0)

	// ASSERTION 1 (the A1 hole): the stale OLD store must NOT have been
	// installed. Pre-fence: it IS installed and a hit returns "OLD".
	if crv, hit := lookupCRDSchema(x); hit {
		t.Fatalf("A1 STALENESS HOLE: a hit for X returned an installed CRV (Type=%q) after a "+
			"reset landed between the inflight call's gen snapshot and its store — the stale "+
			"store was NOT fenced. The generation fence must DROP a store whose snapshot is "+
			"stale.", readSchemaType(crv))
	}
	// The drop is observable on the counter.
	if s := crdSchemaMemoStats(); s.StaleDropped < 1 {
		t.Fatalf("StaleDropped=%d want >=1 (the fenced store should have recorded a drop)", s.StaleDropped)
	}

	// ASSERTION 2 (self-healing): the NEXT miss recompiles fresh -> NEW, and a
	// store under the CURRENT generation installs it.
	gen1 := currentSchemaGen()
	crvNew, err := compileCRDSchemaFn(crd, "v1-2-2")
	if err != nil {
		t.Fatalf("compile NEW errored: %v", err)
	}
	if got := readSchemaType(crvNew); got != "NEW" {
		t.Fatalf("self-heal: second compile should be NEW, got %q", got)
	}
	storeCRDSchema(x, crvNew, gen1)
	crv, hit := lookupCRDSchema(x)
	if !hit {
		t.Fatalf("self-heal: X must be installed after a fresh store under the current generation")
	}
	if got := readSchemaType(crv); got != "NEW" {
		t.Fatalf("self-heal: X resolved to %q, want NEW — the post-reset recompile did not "+
			"win (the memo is serving stale OLD)", got)
	}
}

// TestCRDSchemaMemo_GenerationFence_StoreUnderSameGenSucceeds pins the happy
// path: with NO reset between snapshot and store, the fenced store installs
// normally (the fence must not break the common case).
func TestCRDSchemaMemo_GenerationFence_StoreUnderSameGenSucceeds(t *testing.T) {
	resetCRDSchemaMemoForTest()
	t.Cleanup(resetCRDSchemaMemoForTest)

	x := runtimeschema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-2-2", Resource: "happypanels"}
	gen := currentSchemaGen()
	storeCRDSchema(x, markerCRV("NEW"), gen)

	crv, hit := lookupCRDSchema(x)
	if !hit {
		t.Fatalf("a fenced store with NO intervening reset must install (the fence broke the " +
			"common no-reset case)")
	}
	if got := readSchemaType(crv); got != "NEW" {
		t.Fatalf("installed CRV Type=%q want NEW", got)
	}
	if s := crdSchemaMemoStats(); s.StaleDropped != 0 {
		t.Fatalf("StaleDropped=%d want 0 (no reset happened — nothing should be dropped)", s.StaleDropped)
	}
}
