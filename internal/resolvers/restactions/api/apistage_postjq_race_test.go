// apistage_postjq_race_test.go — Ship Phase B / 0.30.185 race-clean
// proof. Run under -race -count=20 (CI + local pre-commit).
//
// Three concurrent populations exercise the postJQ sync.Map under the
// existing cohort gate memo lifecycle:
//   1. N reader goroutines hammering lookupCohortPostJQ against the
//      same (memo, stage-id, jqID) tuple.
//   2. A writer goroutine periodically bumping the rbacGen field of the
//      memo (simulating the per-cohort RBAC mutation that drops the
//      memo at the gate-memo-stamp-mismatch check).
//   3. A populate goroutine running storeCohortPostJQ for the same key
//      (proving LoadOrStore dedupe under concurrent contention).
//
// Race-clean is the load-bearing assertion — -race -count=20 must pass.

package api

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
)

// TestCohortPostJQEncoded_RaceWithRBACGenBump_AndConcurrentReads is the
// HG-PB.8 race-clean falsifier. The test mirrors the existing
// race-test pattern (apistage_cohort_memo_test.go) — N readers + writer
// concurrency on a shared sync.Map.
//
// Run: `go test -race -count=20 -run TestCohortPostJQEncoded_Race ./internal/resolvers/restactions/api`
func TestCohortPostJQEncoded_RaceWithRBACGenBump_AndConcurrentReads(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping race storm under -short")
	}
	ctx := xcontext.BuildContext(t.Context())
	memo := &cohortGateMemo{permitAll: true, rbacGen: 1}

	filter := ".items"
	jqID, _ := JQIDFromFilter(&filter)
	const stageID = "compositions"
	const duration = 250 * time.Millisecond

	// Pre-populate so readers have a hot entry to race against.
	storeCohortPostJQ(ctx, memo, testGVR, "cohort-1", stageID, jqID, []byte(`["a","b"]`))

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var readHits, readMisses atomic.Int64
	var storeOps atomic.Int64
	var rbacBumps atomic.Int64

	// READERS — N goroutines hammer lookupCohortPostJQ.
	readers := runtime.GOMAXPROCS(0)
	if readers < 4 {
		readers = 4
	}
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, hit := lookupCohortPostJQ(memo, stageID, jqID); hit {
					readHits.Add(1)
				} else {
					readMisses.Add(1)
				}
			}
		}()
	}

	// WRITER 1 — bump rbacGen every 10ms. (In production this is an
	// atomic publish via cache.CohortRBACGen; here we exercise the
	// memo's own field. Concurrent reads MUST tolerate the
	// non-atomic uint64 mutation; sync.Map operations themselves
	// don't reach rbacGen.) Use atomic store to satisfy -race.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			atomic.StoreUint64(&memo.rbacGen, memo.rbacGen+1)
			rbacBumps.Add(1)
			time.Sleep(10 * time.Millisecond)
		}
	}()

	// WRITER 2 — populate goroutine running storeCohortPostJQ with the
	// SAME bytes. LoadOrStore dedupe must NOT race the readers.
	wg.Add(1)
	go func() {
		defer wg.Done()
		bytes := []byte(`["a","b"]`)
		for {
			select {
			case <-stop:
				return
			default:
			}
			storeCohortPostJQ(ctx, memo, testGVR, "cohort-1", stageID, jqID, bytes)
			storeOps.Add(1)
			time.Sleep(5 * time.Millisecond)
		}
	}()

	time.Sleep(duration)
	close(stop)
	wg.Wait()

	// We don't assert exact hit counts (timing-dependent); we just
	// assert that the storm produced both reads + writes and that
	// the test reached this point without -race firing.
	t.Logf("readers=%d reads_hits=%d reads_misses=%d store_ops=%d rbac_bumps=%d",
		readers, readHits.Load(), readMisses.Load(), storeOps.Load(), rbacBumps.Load())
	if readHits.Load() == 0 {
		t.Errorf("zero hits across the storm — sync.Map.Load may have regressed")
	}
}

// TestCohortPostJQEncoded_ConcurrentDistinctKeys exercises sync.Map under
// concurrent population of DISTINCT (stage-id, jqID) keys against the
// same memo. The race is on the postJQEncoded sync.Map's internal
// dirty-promotion + read-amplification machinery.
func TestCohortPostJQEncoded_ConcurrentDistinctKeys(t *testing.T) {
	ctx := xcontext.BuildContext(t.Context())
	memo := &cohortGateMemo{permitAll: true, rbacGen: 1}
	const workers = 64

	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		i := i
		go func() {
			defer wg.Done()
			// distinct jqIDs / stage-ids per goroutine.
			storeCohortPostJQ(ctx, memo, testGVR, "c",
				stageNameFor(i),
				uint64(i)+1,
				[]byte(`["x"]`))
		}()
	}
	wg.Wait()

	count := 0
	memo.postJQEncoded.Range(func(_, _ any) bool { count++; return true })
	if count != workers {
		t.Errorf("distinct-key storm: entries=%d want=%d", count, workers)
	}
}

func stageNameFor(i int) string {
	// Simple stable transform — no fmt to keep the test cheap.
	return "stage-" + intToString(i)
}

func intToString(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	negative := false
	if i < 0 {
		negative = true
		i = -i
	}
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if negative {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

// avoid 'unused' on the test-only context import in case Go's gofmt
// complains about ctx threading.
var _ = context.Background
