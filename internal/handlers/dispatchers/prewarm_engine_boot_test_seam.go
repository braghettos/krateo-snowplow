// prewarm_engine_boot_test_seam.go — Ship 0.30.241.
//
// Test-only seams so the seed-coverage falsifier
// (seed_coverage_falsifier_test.go) can stub the production
// seedOneRestaction / seedOneWidget / withPhase1SAContext call sites
// + invoke seedScopeYielding with controllable harvester input.
//
// PRODUCTION CONTRACT: when the seams are at their default values
// (the real production functions), production seedScopeYielding
// behavior is byte-identical to a build without this file. The
// indirection is a function-pointer var per call site; calling
// through the pointer is one indirect dispatch — measured ~1ns
// overhead per seed invocation, irrelevant against the seed's
// per-RA wall-clock (~5s).
//
// TEST-ONLY: the setSeedOneRestactionForTest / setSeedOneWidgetForTest /
// setWithPhase1SAContextForTest helpers swap the seam pointers and
// return a restore closure. Tests defer the restore to keep package-
// level state clean.
//
// This file is NOT a _test.go file because the production
// seedScopeYielding calls through the seam variables — those vars
// MUST exist in the production binary. The setter helpers are guarded
// by ForTest naming so a future audit grep can find every test-only
// hook.

package dispatchers

import (
	"context"

	"github.com/krateoplatformops/plumbing/endpoints"
	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"k8s.io/client-go/rest"
)

// Type aliases so test stubs in _test.go files can match the seam's
// function-pointer signature without importing the full endpoint/rest
// surface.
type endpointStub = endpoints.Endpoint
type restConfigStub = *rest.Config

// Seam function-pointer vars. Default to the production
// implementations; tests swap via setXxxForTest helpers.
var (
	seedOneRestactionFn = seedOneRestaction
	seedOneWidgetFn     = seedOneWidget
	withPhase1SAContextFn = func(ctx context.Context, saEP endpoints.Endpoint, saRC *rest.Config) context.Context {
		return withPhase1SAContext(ctx, saEP, saRC)
	}
)

// setSeedOneRestactionForTest swaps the production seedOneRestaction
// for a test stub and returns a restore closure. Defer the restore.
func setSeedOneRestactionForTest(stub func(ctx context.Context, cohortLabel string, ref templatesv1.ObjectReference, authnNS string) error) func() {
	prev := seedOneRestactionFn
	seedOneRestactionFn = stub
	return func() { seedOneRestactionFn = prev }
}

// setSeedOneWidgetForTest swaps the production seedOneWidget for a
// test stub and returns a restore closure. Defer the restore.
func setSeedOneWidgetForTest(stub func(ctx context.Context, e navWidgetEntry, authnNS string) error) func() {
	prev := seedOneWidgetFn
	seedOneWidgetFn = stub
	return func() { seedOneWidgetFn = prev }
}

// setWithPhase1SAContextForTest swaps the production withPhase1SAContext
// for a test stub (typically an identity-passthrough that bypasses the
// real SA endpoint plumbing).
func setWithPhase1SAContextForTest(stub func(ctx context.Context, saEP endpointStub, saRC restConfigStub) context.Context) func() {
	prev := withPhase1SAContextFn
	withPhase1SAContextFn = stub
	return func() { withPhase1SAContextFn = prev }
}

// seedScopeYieldingForTest is the test entry point — calls the
// production seedScopeYielding with zero-value SA credentials. The
// production seedScopeYielding routes its SA calls through
// withPhase1SAContextFn / seedOneRestactionFn / seedOneWidgetFn, so
// the test's stubs intercept every external call surface.
//
// authnNS defaults to "" (matches the boot path's read of
// AUTHN_NAMESPACE env var, which is empty in unit tests).
func seedScopeYieldingForTest(ctx context.Context, restactionRefs []templatesv1.ObjectReference, widgetEntries []navWidgetEntry) error {
	return seedScopeYielding(ctx, restactionRefs, widgetEntries, endpoints.Endpoint{}, nil, "")
}
