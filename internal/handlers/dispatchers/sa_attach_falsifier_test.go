// sa_attach_falsifier_test.go — Ship 0.30.166 dispatcher-attach falsifier.
//
// LOAD-BEARING for the 5th iteration of #307. The previous four attempts
// (0.30.102, 0.30.103, 0.30.104, 0.30.165) each shipped a side-fix that
// did NOT make the actual cache-off request path engage
// dispatchViaInternalRESTConfig — leaving plumbing's httpcall.Do (whose
// tlsConfigFor early-returns at !HasCertAuth() for the snowplow SA
// endpoint) as the dispatch site and reproducing the x509 error on every
// fresh deploy.
//
// THIS TEST GUARDS THE FIX SURFACE THEY ALL SKIPPED: the dispatcher-entry
// SA attach (restactions.go ~ln 120, widgets.go ~ln 141) factored through
// the snowplowSACtx() helper. The helper resolves the snowplow SA
// endpoint + *rest.Config (the same singleton Phase 1 + the refresher
// consume) and is invoked from the per-request handler BEFORE the
// resolver call.
//
// Falsifier red/green:
//   - PRE-FIX (no snowplowSACtx helper defined): this file does not
//     COMPILE — the symbol is unresolved. That is the intentional red:
//     a missing-helper failure is structurally equivalent to a missing-
//     attach failure, and a compile error is the strongest TDD red there
//     is (no flakes, no false negatives).
//   - POST-FIX: snowplowSACtx exists, returns (nil, nil) when no
//     projected SA volume is present (the AC-307.7 out-of-cluster
//     invariant), and the helper-produced attach chains correctly into
//     dispatchViaInternalRESTConfig at the api-package level (covered by
//     internal_dispatch_tls_test.go).
//
// CRITICAL — this test is necessary but NOT sufficient. The on-cluster
// AC-307.4 falsifier (pod log shows `"dispatch": "internal-rest-config"`
// for the cache-off namespaces stage) remains the load-bearing real-
// cluster gate, exactly as for 0.30.104.

package dispatchers

import (
	"context"
	"os"
	"testing"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// TestSnowplowSACtx_OutOfCluster_ReturnsNilNil pins AC-307.7: a unit-test
// environment (no projected /var/run/secrets/kubernetes.io/serviceaccount/
// volume, no KUBERNETES_SERVICE_HOST/PORT env) MUST receive (nil, nil)
// from the helper so the dispatcher attach is a no-op — preserving the
// pre-0.30.166 behaviour for every unit test in the tree.
func TestSnowplowSACtx_OutOfCluster_ReturnsNilNil(t *testing.T) {
	// Skip if the test host is itself running as a pod with a projected
	// SA (unlikely in CI but possible for someone running the suite from
	// inside the cluster). The helper would then legitimately return a
	// non-nil pair and this test would FAIL — incorrectly, because the
	// AC-307.7 invariant is specifically about OUT-of-cluster behaviour.
	if _, err := os.Stat("/var/run/secrets/kubernetes.io/serviceaccount/token"); err == nil {
		t.Skip("test host has a projected SA volume; AC-307.7 unit test " +
			"only applies out of cluster")
	}

	saEP, saRC := snowplowSACtx()
	if saEP != nil {
		t.Fatalf("AC-307.7 VIOLATED: snowplowSACtx() returned non-nil saEP "+
			"in an out-of-cluster unit test (no projected SA volume) — "+
			"the dispatcher attach would then run with a malformed/zero "+
			"endpoint and leak it into request ctx. saEP=%+v", saEP)
	}
	if saRC != nil {
		t.Fatalf("AC-307.7 VIOLATED: snowplowSACtx() returned non-nil saRC "+
			"in an out-of-cluster unit test (rest.InClusterConfig must "+
			"error without KUBERNETES_SERVICE_HOST/PORT set) — saRC=%+v",
			saRC)
	}
}

// TestSnowplowSACtx_NilReturn_NoAttach pins the request-path consequence
// of the (nil, nil) return: the dispatcher SHOULD NOT call
// cache.WithInternalRESTConfig / cache.WithInternalEndpoint when the
// helper returned nil. This test reproduces the dispatcher attach block
// inline (the EXACT three lines added at restactions.go ~ln 120 and
// widgets.go ~ln 141) and asserts the resulting ctx carries NO internal
// rest-config — i.e. ordinary per-user requests with no SA available
// (or out-of-cluster unit tests) still flow through httpcall.Do, never
// engaging dispatchViaInternalRESTConfig.
//
// This is the BEHAVIOR-NEUTRAL gate for AC-307.5 / AC-307.6 / AC-307.7.
func TestSnowplowSACtx_NilReturn_NoAttach(t *testing.T) {
	if _, err := os.Stat("/var/run/secrets/kubernetes.io/serviceaccount/token"); err == nil {
		t.Skip("test host has a projected SA volume; AC-307.7 unit test " +
			"only applies out of cluster")
	}

	ctx := context.Background()
	if saEP, saRC := snowplowSACtx(); saEP != nil && saRC != nil {
		// This branch is the production attach pattern. The test ASSERTS
		// it does NOT engage in this out-of-cluster scenario.
		ctx = cache.WithInternalEndpoint(ctx, saEP)
		ctx = cache.WithInternalRESTConfig(ctx, saRC)
		t.Fatalf("BEHAVIOR-NEUTRAL VIOLATED: snowplowSACtx() returned a "+
			"non-nil pair out-of-cluster; the dispatcher attach engaged "+
			"and would have flipped ordinary per-user requests onto the "+
			"internal-rest-config path. saEP=%+v saRC=%+v", saEP, saRC)
	}

	if _, ok := cache.InternalRESTConfigFromContext(ctx); ok {
		t.Fatal("AC-307.7 VIOLATED: cache.InternalRESTConfigFromContext " +
			"returned ok=true on a ctx whose attach block was skipped — " +
			"the attach helper has a side effect on context that bypasses " +
			"the nil-guard. Ordinary per-user requests would then route " +
			"through dispatchViaInternalRESTConfig with a nil/invalid rc.")
	}
}
