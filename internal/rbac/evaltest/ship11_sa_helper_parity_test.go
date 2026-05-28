package evaltest

// ship11_sa_helper_parity_test.go — Ship 1.1 S1 drift-guard.
//
// cache/ carries byte-faithful COPIES of two rbac/ helpers
// (parseServiceAccountUsername, effectiveGroups) because cache/ cannot
// import rbac/ (rbac/ imports cache/ → cycle). The original divergence this
// ship fixed was exactly a cache layer (CohortNSACL) drifting from the rbac
// authoritative evaluator. This test PINS the two copies to their originals
// over a fixed corpus — incl the 8 cases the architect ran — so a future
// edit that touches one side without the other fails CI here.
//
// evaltest can import BOTH packages, so it compares the implementations
// directly via their ForTest hooks.

import (
	"testing"

	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/rbac"
)

func TestShip11Parity_ParseSAUsername(t *testing.T) {
	corpus := []string{
		"system:serviceaccount:krateo-system:snowplow", // canonical
		"system:serviceaccount:ns:name:extra",          // colon in name (name = "name:extra")
		"system:serviceaccount:ns:",                     // empty name → reject
		"system:serviceaccount::name",                   // empty ns → reject
		"system:serviceaccount:onlyone",                 // no second colon → reject
		"alice",                                         // plain user → reject
		"system:serviceaccount",                         // prefix without trailing colon → reject
		"system:serviceaccount:",                        // bare prefix → reject
		"",                                              // empty → reject
	}
	for _, u := range corpus {
		rNS, rName, rOK := rbac.ParseServiceAccountUsernameForTest(u)
		cNS, cName, cOK := cache.ParseCohortSAUsernameForTest(u)
		if rNS != cNS || rName != cName || rOK != cOK {
			t.Errorf("parseSAUsername DRIFT for %q:\n  rbac  => (%q,%q,%v)\n  cache => (%q,%q,%v)",
				u, rNS, rName, rOK, cNS, cName, cOK)
		}
	}
}

func TestShip11Parity_EffectiveGroups(t *testing.T) {
	cases := []struct {
		groups []string
		isSA   bool
		saNS   string
	}{
		{nil, false, ""},                                   // non-SA, no groups
		{[]string{"devs"}, false, ""},                      // non-SA, groups unchanged
		{nil, true, "krateo-system"},                       // SA, no base groups
		{[]string{"system:authenticated"}, true, "ns-a"},   // SA, one base group
		{[]string{"a", "b"}, true, "ns-b"},                 // SA, multiple base groups
		{[]string{}, true, ""},                             // SA, empty-string ns
	}
	for _, c := range cases {
		r := rbac.EffectiveGroupsForTest(c.groups, c.isSA, c.saNS)
		g := cache.CohortEffectiveGroupsForTest(c.groups, c.isSA, c.saNS)
		if !equalStrs(r, g) {
			t.Errorf("effectiveGroups DRIFT for groups=%v isSA=%v saNS=%q:\n  rbac  => %v\n  cache => %v",
				c.groups, c.isSA, c.saNS, r, g)
		}
	}
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
