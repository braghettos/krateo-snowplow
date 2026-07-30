package rbac

import (
	"context"

	"github.com/krateoplatformops/plumbing/endpoints"
	"k8s.io/client-go/kubernetes"
)

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

// CanonicalGroupsHashForTest exposes canonicalGroupsHash (groups_hash.go)
// to the cross-package M9 collision/order/purity falsifier in evaltest.
// The hasher is the single source of truth for the memo key's GroupsHash
// dimension; the falsifier asserts its contract directly (length-prefix
// framing defeats the ["a","bc"]/["ab","c"] alias; order independence;
// nil==empty!=[""]; caller slice unmutated) WITHOUT routing through
// EvaluateRBAC's observable behaviour, so a regression in the hasher is
// caught at the unit boundary. Thin pass-through; zero production surface.
func CanonicalGroupsHashForTest(groups []string) uint64 {
	return canonicalGroupsHash(groups)
}

// SetSARClientsetForTest overrides the SAR-baseline clientset factory
// (rbac.go's sarClientsetForEndpoint) so the hermetic cache=off L5 test
// can inject a fake authorization clientset instead of building a real
// one from the user endpoint. Returns a restore func; production code
// MUST NOT call it. The factory receives the endpoint the SAR path
// resolved from ctx and returns the kubernetes.Interface whose
// AuthorizationV1().SelfSubjectAccessReviews() the SAR path calls.
func SetSARClientsetForTest(f func(ctx context.Context, ep endpoints.Endpoint) (kubernetes.Interface, error)) (restore func()) {
	prev := sarClientsetForEndpoint
	sarClientsetForEndpoint = f
	return func() { sarClientsetForEndpoint = prev }
}
