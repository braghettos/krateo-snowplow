// prewarm_engine_boot_test.go — Ship 0.30.238 Component A unit tests.
//
// MANDATORY per design §4.4.2 + feedback_no_shortcuts_or_workarounds:
// these tests ship in the SAME commit as unionCohorts, no defer. The
// sentinel-double-count case (group_only_sentinel_no_double_count) is
// load-bearing — it verifies cohortKey discriminates between the synthetic
// group-only sentinel identity and a real user-cohort that happens to
// carry the same Groups slice. If the two were ever collapsed by the
// union, Falsifier A's cyberjoker-in-seed_widgets_by_cohort assertion
// could only catch the User-cohort and would miss a Group-only-cohort
// regression.
//
// Pure-Go — no kubeconfig, no fake clients, runnable via:
//   go test ./internal/handlers/dispatchers/ -run TestUnionCohorts

package dispatchers

import (
	"testing"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// groupOnlyCohortSentinelLocal mirrors the unexported constant in
// internal/cache/binding_set_enumeration.go:146. It is the synthetic
// Username assigned to a Cohort that represents a pure group-only RBAC
// projection (no user subject). The test references the string literal
// directly because the constant is package-private to internal/cache; if
// the upstream constant ever changes, this literal MUST be updated
// lockstep (and the test should fail loudly when it isn't).
const groupOnlyCohortSentinelLocal = "system:cohort:group-only:v1"

func TestUnionCohorts_NilLeft(t *testing.T) {
	x := []cache.Cohort{
		{Username: "alice", Groups: []string{"devs"}},
		{Username: "bob", Groups: []string{"ops"}},
	}
	got := unionCohorts(nil, x)
	if len(got) != len(x) {
		t.Fatalf("nil_left: len(unionCohorts(nil, x))=%d want %d", len(got), len(x))
	}
	for i := range x {
		if got[i].Username != x[i].Username {
			t.Fatalf("nil_left: got[%d].Username=%q want %q", i, got[i].Username, x[i].Username)
		}
	}
}

func TestUnionCohorts_NilRight(t *testing.T) {
	x := []cache.Cohort{
		{Username: "alice", Groups: []string{"devs"}},
	}
	got := unionCohorts(x, nil)
	if len(got) != len(x) {
		t.Fatalf("nil_right: len(unionCohorts(x, nil))=%d want %d", len(got), len(x))
	}
	if got[0].Username != "alice" {
		t.Fatalf("nil_right: got[0].Username=%q want %q", got[0].Username, "alice")
	}
}

func TestUnionCohorts_Idempotent(t *testing.T) {
	x := []cache.Cohort{
		{Username: "alice", Groups: []string{"devs"}},
		{Username: "bob", Groups: []string{"ops", "admins"}},
	}
	got := unionCohorts(x, x)
	if len(got) != len(x) {
		t.Fatalf("idempotent: len(unionCohorts(x, x))=%d want %d (dedup must collapse identical cohorts)", len(got), len(x))
	}
}

func TestUnionCohorts_DedupOnCohortKey(t *testing.T) {
	// B appears in both a and b with the SAME Username + Groups (with a
	// deliberately-shuffled Groups slice in b to verify cohortKey sorts
	// Groups before hashing).
	a := []cache.Cohort{
		{Username: "alice", Groups: []string{"devs"}},
		{Username: "bob", Groups: []string{"admins", "ops"}},
	}
	b := []cache.Cohort{
		{Username: "bob", Groups: []string{"ops", "admins"}}, // shuffled
		{Username: "carol", Groups: []string{"viewers"}},
	}
	got := unionCohorts(a, b)
	if len(got) != 3 {
		t.Fatalf("dedup: len(unionCohorts(a, b))=%d want 3 (got cohorts=%+v)", len(got), got)
	}
	// Order: a first (in input order), then b in input order minus dedup'd.
	want := []string{"alice", "bob", "carol"}
	for i, w := range want {
		if got[i].Username != w {
			t.Fatalf("dedup: got[%d].Username=%q want %q (full result=%+v)", i, got[i].Username, w, got)
		}
	}
}

func TestUnionCohorts_GroupOnlySentinelNoDoubleCount(t *testing.T) {
	// LOAD-BEARING peer-review case (design §4.4.2 row 5):
	//
	// (Username=cyberjoker, Groups=["g"]) and (Username=sentinel, Groups=["g"])
	// MUST remain DISTINCT identities — they are two different cohorts in
	// the seed (one carries a user identity, the other is the synthetic
	// group-only projection). cohortKey discriminates them via Username so
	// the union returns 2 entries, NOT 1.
	a := []cache.Cohort{
		{Username: "cyberjoker", Groups: []string{"g"}},
	}
	b := []cache.Cohort{
		{Username: groupOnlyCohortSentinelLocal, Groups: []string{"g"}},
	}
	got := unionCohorts(a, b)
	if len(got) != 2 {
		t.Fatalf("group_only_sentinel_no_double_count: len=%d want 2 (sentinel-prefixed group-only MUST NOT collapse with real user carrying the same group; got=%+v)", len(got), got)
	}
	// Verify both identities survive — order is a-first.
	if got[0].Username != "cyberjoker" {
		t.Fatalf("group_only_sentinel: got[0].Username=%q want %q", got[0].Username, "cyberjoker")
	}
	if got[1].Username != groupOnlyCohortSentinelLocal {
		t.Fatalf("group_only_sentinel: got[1].Username=%q want %q", got[1].Username, groupOnlyCohortSentinelLocal)
	}
}
