// seed_resolves_counter_test.go — counters-hygiene 2026-07-04: the light
// hermetic PUBLISH-WIRING falsifier for the re-wired
// snowplow_phase1_bindingset_seed_resolves_total (team-lead ruling (a)).
//
// The original red-herring had TWO halves: (1) declared-but-never-incremented
// (the dead .Add — its only feeder, runPIPSeed, was deleted in the
// prewarm-family fold), and (2) a metric that must actually SURFACE at
// /debug/vars to be read. This arm discriminates the PUBLISH-WIRING regression
// class: the counter must be expvar-PUBLISHED and its published value must
// reflect the backing atomic (a Func that reads the live counter, not a stale
// snapshot / an unpublished var). RED = the expvar.Publish removed (key absent)
// OR the Func decoupled from the atomic.
//
// INCREMENT-SITE SPLIT (documented per the FIX-C / #95-Put-gate precedent — do
// NOT "fix" this with a live-dial test): the .Add(1) itself lives in
// seedOneWidget/seedOneRestaction immediately AFTER the success handle.Put.
// Reaching that Put hermetically is NOT possible — widgets.Resolve /
// restactions.Resolve make a live apiserver GET over the SA transport (dial
// kubernetes.default.svc → connection refused in-test); three harness shapes
// (granted watcher / built-in-GVK to dodge plurals discovery / non-nil
// rest.Config) all hit that reach limit, and no existing hermetic test drives a
// seed primitive to a real Put for exactly this reason. So the increment-SITE
// is covered by the ON-CLUSTER arm: on the next seeded boot,
// snowplow_phase1_bindingset_seed_resolves_total climbs > 0 (RED = the dead-0
// this whole task exists to fix). A hermetic live-dial test would be
// non-hermetic/flaky (feedback_no_fake_production_scenarios); rejected.
//
// -race, own package, no ./internal/rbac.

package dispatchers

import (
	"expvar"
	"strconv"
	"strings"
	"testing"
)

// parseCounterString parses an expvar.Func(uint64).String() rendering (a plain
// integer, possibly quoted/whitespaced) into a uint64.
func parseCounterString(t *testing.T, s string) uint64 {
	t.Helper()
	s = strings.TrimSpace(strings.Trim(s, `"`))
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		t.Fatalf("cannot parse counter expvar string %q: %v", s, err)
	}
	return n
}

func TestSeedResolvesCounter_PublishedAndReflectsAtomic(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	registerPIPMetrics() // idempotent (pipMetricsOnce); publishes under CACHE_ENABLED

	const key = "snowplow_phase1_bindingset_seed_resolves_total"

	// (1) PUBLISHED — the declared-but-unpublished regression class. Before the
	// counters-hygiene pass this key existed as an atomic but its published Func
	// read a dead-at-0 counter; here we assert it SURFACES at all.
	v := expvar.Get(key)
	if v == nil {
		t.Fatalf("%s is NOT expvar-published — a metric that never surfaces at /debug/vars is unreadable observability", key)
	}

	// (2) The published Func reflects the LIVE atomic. Read the current value,
	// bump the backing counter directly, and assert the published string moves
	// by the same delta — proves the Func is wired to the counter the seed Put
	// path increments (not a decoupled/stale snapshot). We do NOT drive the
	// full seed here (the Put is apiserver-I/O-bound — see the SPLIT note).
	before := parseCounterString(t, v.String())
	pipBindingSetSeedResolvesTotal.Add(2)
	after := parseCounterString(t, v.String())
	if after-before != 2 {
		t.Fatalf("%s published value delta = %d after a +2 atomic bump; want 2 "+
			"(the published Func must read the LIVE counter — the one seedOneWidget/seedOneRestaction "+
			"increment on a successful seed Put)", key, after-before)
	}
}
