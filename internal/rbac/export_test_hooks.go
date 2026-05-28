package rbac

// export_for_test.go — Ship 1.1 S1 drift-guard hooks.
//
// Exposes the SA-username parser + the synthetic-SA-group expansion to the
// cross-package parity test in internal/rbac/evaltest, which asserts the
// cache/ package's byte-faithful COPIES (cache.ParseCohortSAUsernameForTest /
// cache.CohortEffectiveGroupsForTest) stay equal to these ORIGINALS. Cache/
// cannot import rbac/ (cycle), so it carries copies; this guard fails the
// moment a copy silently re-diverges from the original — the exact bug class
// Ship 1.1 fixed (CohortNSACL had drifted from EvaluateRBAC on the SA path).
//
// Test-only: these are thin pass-throughs to the unexported originals. They
// add zero production surface (the funcs are never called by non-test code).

// ParseServiceAccountUsernameForTest exposes parseServiceAccountUsername.
func ParseServiceAccountUsernameForTest(u string) (string, string, bool) {
	return parseServiceAccountUsername(u)
}

// EffectiveGroupsForTest exposes effectiveGroups, adapted to a (groups,
// isSA, saNS) signature so the parity test can compare it directly against
// cache.CohortEffectiveGroupsForTest. It constructs the EvaluateOptions the
// original reads (only Groups is consulted by effectiveGroups).
func EffectiveGroupsForTest(groups []string, isSA bool, saNS string) []string {
	return effectiveGroups(EvaluateOptions{Groups: groups}, isSA, saNS)
}
