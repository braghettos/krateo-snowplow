// resolve_populate_testshim_test.go — test-only shim for swapping the
// resolveAndPopulateL1 resolve seam (resolveOnceFn).
//
// The shim lives in a _test.go file so the production resolve_populate.go
// never exposes a setter — the seam is reassignable ONLY from tests.

package dispatchers

import (
	"context"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// setResolveOnceForTest swaps the package-level resolveOnceFn for the
// duration of a test and returns a restore function the caller MUST
// defer/Cleanup. Production code never calls this.
func setResolveOnceForTest(fn func(ctx context.Context, inputs cache.ResolvedKeyInputs) ([]byte, error)) func() {
	prev := resolveOnceFn
	resolveOnceFn = fn
	return func() { resolveOnceFn = prev }
}
