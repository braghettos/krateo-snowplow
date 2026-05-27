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

	recordL1Lookup(handler, gvr, true)
	recordL1Lookup(handler, gvr, true)
	recordL1Lookup(handler, gvr, false)

	afterHit, afterMiss := readCell(t, v, handler+"|"+gvr)
	if got, want := afterHit-beforeHit, uint64(2); got != want {
		t.Fatalf("hit_total delta = %d, want %d", got, want)
	}
	if got, want := afterMiss-beforeMiss, uint64(1); got != want {
		t.Fatalf("miss_total delta = %d, want %d", got, want)
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
			recordL1Lookup(handler, gvr, true)
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
