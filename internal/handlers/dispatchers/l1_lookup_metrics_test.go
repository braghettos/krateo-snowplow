// l1_lookup_metrics_test.go — Ship OBS-1 (0.30.186) unit tests.
//
// Asserts:
//   - registerL1LookupMetrics is idempotent (second call no-ops).
//   - recordL1Lookup bumps the right (handlerKind, gvrString) cell.
//   - The expvar.Func returns a map shape with hit_total / miss_total
//     keys per cell.

package dispatchers

import (
	"encoding/json"
	"expvar"
	"sync"
	"testing"
)

// TestRegisterL1LookupMetrics_Idempotent — calling the helper twice
// must NOT panic on duplicate expvar.Publish.
func TestRegisterL1LookupMetrics_Idempotent(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("registerL1LookupMetrics panicked on second call: %v", r)
		}
	}()
	registerL1LookupMetrics()
	registerL1LookupMetrics()
}

// TestRecordL1Lookup_BumpsHitMissPerCell — drives the recordL1Lookup
// helper and confirms the published expvar.Func reflects the cell.
func TestRecordL1Lookup_BumpsHitMissPerCell(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	registerL1LookupMetrics()

	// Use an OBS-1-prefixed GVR to avoid collisions with any other
	// in-process test that might be sharing the same sync.Map.
	const (
		handler = "restactions"
		gvr     = "obs1.example.io/v1, Resource=test"
	)

	// Snapshot baseline (other tests may have shared the package state).
	v := expvar.Get("snowplow_dispatch_l1_lookups")
	if v == nil {
		t.Fatalf("snowplow_dispatch_l1_lookups not published")
	}
	beforeHit, beforeMiss := readCell(t, v, handler+"|"+gvr)

	// #130 F3: two hits (one seed-attributable, one traffic) + one miss.
	seedAttrBefore := hitsSeedAttributable.Load()
	recordL1Lookup(handler, gvr, true, true)  // seed hit
	recordL1Lookup(handler, gvr, true, false) // traffic hit
	recordL1Lookup(handler, gvr, false, false)

	afterHit, afterMiss := readCell(t, v, handler+"|"+gvr)
	if got, want := afterHit-beforeHit, uint64(2); got != want {
		t.Fatalf("hit_total delta = %d, want %d", got, want)
	}
	if got, want := afterMiss-beforeMiss, uint64(1); got != want {
		t.Fatalf("miss_total delta = %d, want %d", got, want)
	}
	// #130 F3: exactly ONE of the two hits was seed-attributable — both the
	// per-cell seed_hit_total and the process-wide aggregate bump by 1.
	if got, want := hitsSeedAttributable.Load()-seedAttrBefore, uint64(1); got != want {
		t.Fatalf("hits_seed_attributable delta = %d, want %d (only the SeededAtBoot hit counts)", got, want)
	}
	seedHit := readSeedHitCell(t, v, handler+"|"+gvr)
	if seedHit < 1 {
		t.Fatalf("seed_hit_total = %d, want >=1 (the seed-attributable hit)", seedHit)
	}
}

// TestRecordL1Lookup_ConcurrentFirstObservation — N goroutines hit
// the same fresh (handler, gvr) cell. Run under -race; total bumps
// must equal N (no lost updates from the LoadOrStore race).
func TestRecordL1Lookup_ConcurrentFirstObservation(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	registerL1LookupMetrics()

	const (
		handler = "widgets"
		// Distinct GVR so the cell is fresh.
		gvr = "obs1-concurrent.example.io/v1, Resource=race"
		N   = 200
	)

	var wg sync.WaitGroup
	wg.Add(N)
	start := make(chan struct{})
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			<-start
			recordL1Lookup(handler, gvr, true, false)
		}()
	}
	close(start)
	wg.Wait()

	v := expvar.Get("snowplow_dispatch_l1_lookups")
	if v == nil {
		t.Fatalf("snowplow_dispatch_l1_lookups not published")
	}
	hit, _ := readCell(t, v, handler+"|"+gvr)
	if hit != N {
		t.Fatalf("concurrent hit_total = %d, want %d (lost-update race?)", hit, N)
	}
}

// readCell unmarshals the expvar.Func JSON and returns the hit/miss
// values for the given cell key. Returns 0,0 when the cell is not
// present (the cell is lazily created on first record).
func readCell(t *testing.T, v expvar.Var, cellKey string) (uint64, uint64) {
	t.Helper()
	raw := v.String()
	var parsed map[string]map[string]uint64
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("decode expvar JSON %q: %v", raw, err)
	}
	cell, ok := parsed[cellKey]
	if !ok {
		return 0, 0
	}
	return cell["hit_total"], cell["miss_total"]
}

// readSeedHitCell reads the #130 F3 seed_hit_total for a cell (0 if absent).
func readSeedHitCell(t *testing.T, v expvar.Var, cellKey string) uint64 {
	t.Helper()
	var parsed map[string]map[string]uint64
	if err := json.Unmarshal([]byte(v.String()), &parsed); err != nil {
		t.Fatalf("decode expvar JSON: %v", err)
	}
	if cell, ok := parsed[cellKey]; ok {
		return cell["seed_hit_total"]
	}
	return 0
}
