// refresher_metrics_test.go — Ship OBS-1 (0.30.186) unit tests.
//
// Asserts:
//   - registerRefresherMetrics is idempotent (second call is a no-op,
//     does NOT panic on duplicate expvar.Publish).
//   - The published expvar.Func values mirror the underlying atomics
//     (a synthetic Add on enqueueTotal shows up via expvar.Get).
//
// These tests run with CACHE_ENABLED=true so the production init()
// path also registers; the explicit registerRefresherMetrics() calls
// below exercise the sync.Once guard.

package cache

import (
	"expvar"
	"testing"
)

// TestRegisterRefresherMetrics_Idempotent — calling the registration
// helper twice must NOT panic. expvar.Publish panics on duplicate
// key; the sync.Once guard inside registerRefresherMetrics is what
// makes the helper safe to call repeatedly (from init() in production,
// from RegisterExpvarForTest in tests, and back-to-back here).
func TestRegisterRefresherMetrics_Idempotent(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("registerRefresherMetrics panicked on second call: %v", r)
		}
	}()
	registerRefresherMetrics()
	registerRefresherMetrics() // second call must be a no-op
}

// TestRefresherMetrics_PublishedAndReflectsAtomics — drives the
// underlying atomic, then reads expvar.Get to confirm the published
// Func observes the bump. This is the OBS-1.1 falsifier in unit-test
// form: the expvar key MUST exist and MUST report the live counter.
func TestRefresherMetrics_PublishedAndReflectsAtomics(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	registerRefresherMetrics()

	// Pull the published Func by name. nil here would mean
	// registerRefresherMetrics never published it — fail loud.
	got := expvar.Get("snowplow_refresher_enqueue_total")
	if got == nil {
		t.Fatalf("snowplow_refresher_enqueue_total not published")
	}

	// Snapshot the current value (other tests in the same binary may
	// have bumped the atomic), then drive an enqueue and check the
	// delta. We intentionally read the published Func — NOT the
	// atomic directly — so the test exercises the full expvar surface.
	before := refresherStatsSnapshot().enqueued

	// Drive the atomic via the same enqueue path the production code
	// uses. An empty key is the no-op branch — use a real key.
	r := refresherSingleton()
	r.enqueue("OBS1-test-key")
	defer r.queue.ShutDown() // tidy up the workqueue we just lit up

	after := refresherStatsSnapshot().enqueued
	if after != before+1 {
		t.Fatalf("enqueue_total after enqueue: got %d, want %d", after, before+1)
	}

	// Confirm the published Func reports the same value.
	expvarVal := got.String() // expvar.Func renders JSON; uint64 → "<n>"
	wantStr := jsonUint64(after)
	if expvarVal != wantStr {
		t.Fatalf("snowplow_refresher_enqueue_total expvar = %s, want %s", expvarVal, wantStr)
	}

	// Spot-check a second key to prove the registration covers more
	// than just one Publish call.
	if expvar.Get("snowplow_refresher_queue_depth") == nil {
		t.Fatalf("snowplow_refresher_queue_depth not published")
	}
}

// jsonUint64 renders n the way expvar.Func.String() does for a
// uint64 return: no quoting, plain decimal.
func jsonUint64(n uint64) string {
	// expvar.Func calls json.Marshal — uint64 marshals to bare decimal.
	// Replicating that here avoids importing encoding/json just to
	// build the expected string.
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}
