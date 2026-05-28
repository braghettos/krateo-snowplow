package cache

// export_for_test.go — Ship 1.1 S1 drift-guard hooks.
//
// Exposes the cache/ COPIES of the SA-username parser + the synthetic-SA-
// group expansion (parseCohortSAUsername / cohortEffectiveGroups) to the
// cross-package parity test in internal/rbac/evaltest, which asserts they
// stay byte-equal to the rbac/ ORIGINALS (rbac.ParseServiceAccountUsernameForTest /
// rbac.EffectiveGroupsForTest). cache/ cannot import rbac/ (cycle), so it
// carries copies; this guard catches a future silent re-divergence — the
// exact bug class Ship 1.1 fixed.
//
// Test-only: thin pass-throughs to the unexported copies. Zero production
// surface.

// ParseCohortSAUsernameForTest exposes parseCohortSAUsername.
func ParseCohortSAUsernameForTest(u string) (string, string, bool) {
	return parseCohortSAUsername(u)
}

// CohortEffectiveGroupsForTest exposes cohortEffectiveGroups.
func CohortEffectiveGroupsForTest(groups []string, isSA bool, saNS string) []string {
	return cohortEffectiveGroups(groups, isSA, saNS)
}
