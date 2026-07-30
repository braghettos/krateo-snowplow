package dispatchers

// dispatch_seams.go — H1 (coverage-audit) dispatcher ServeHTTP orchestration
// seams. These are package-level indirection vars for the FOUR collaborators
// the RESTAction / Widget ServeHTTP handlers depend on, defaulting to the real
// production functions:
//
//   - fetchObjectFn        → fetchObject         (helpers.go)
//   - checkDispatchRBACFn  → checkDispatchRBAC   (helpers.go)
//   - restactionsResolveFn → restactions.Resolve (restactions.go)
//   - widgetsResolveFn     → widgets.Resolve     (widgets.go)
//
// PROD-TESTABILITY, BEHAVIOR-PRESERVING: production NEVER reassigns these — the
// handlers call the var, which points at the real function, so the compiled
// prod path is byte-identical to a direct call. Only the H1 _test.go shim
// (dispatch_seams_testshim_test.go) swaps them for the duration of a test to
// drive ServeHTTP hermetically (fake fetchObject / RBAC verdict / Resolve) so
// the SERVE ORCHESTRATION itself is exercised — the ordering (RBAC-before-Get
// vs Get-before-serve), the exactly-one-Put-under-derived-key, the
// cache≡no-cache byte-parity, and the RBAC-sensitive-widget content-lookup skip
// — none of which are observable at sub-block granularity.
//
// This mirrors the repo's established var-seam idiom: resolveOnceFn
// (resolve_populate.go:70, swapped by resolve_populate_testshim_test.go) and
// the api.nestedCallResolver seam (nested_call_testshim_test.go). A package var
// rather than a handler struct field so the RESTAction()/Widgets() constructor
// signatures and the http.Handler contract are untouched.

import (
	"context"

	"github.com/krateoplatformops/snowplow/internal/resolvers/restactions"
	"github.com/krateoplatformops/snowplow/internal/resolvers/widgets"
)

// fetchObjectFn is the fetch-the-dispatched-CR seam (fetchObject → objects.Get).
var fetchObjectFn = fetchObject

// checkDispatchRBACFn is the cache=on EvaluateRBAC dispatch gate seam.
var checkDispatchRBACFn = checkDispatchRBAC

// restactionsResolveFn is the top-level RESTAction resolve seam.
var restactionsResolveFn = restactions.Resolve

// widgetsResolveFn is the top-level Widget resolve seam.
var widgetsResolveFn = func(ctx context.Context, opts widgets.ResolveOptions) (*widgets.Widget, error) {
	return widgets.Resolve(ctx, opts)
}
