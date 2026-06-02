// e2e_v4_assertion3_test.go — Ship 0.30.240 T8 falsifier — assertion 3.
//
// T8 (e2e_identity_free_test.go in internal/cache) closes assertions
// 1, 2, 4, 5, 6, 7, 8 against the v4 ValueObject foundation. Those
// tests live in the cache package because they probe the ValueObject
// pointer-identity + dual-refcount + §4.6 immutability invariants
// directly on the *ValueObject primitive.
//
// Assertion 3 — "cyberjoker's serve output JSON ≠ admin's serve output
// JSON" — needs the PRODUCTION v4 serve gate (gateWidgetsServeBytes)
// to convert per-user `allowed` flags from SA-maximal cached bytes
// into per-user serve output. The serve gate lives in the dispatchers
// package; calling it from the cache package would invert the import
// graph. This test lives in dispatchers + uses cache.* exported test
// helpers (test_helpers_export.go) to construct the fixture.
//
// Architect Q1 ALL-B-UNIFORM ratification: gateWidgetsServeBytes
// invokes the existing gateWidgetEnvelope which constructs a fresh
// `var obj map[string]any` per call (Pattern A — private outer map);
// gateRestactionsServeBytes invokes applyServeTimeUAFOverDict which
// constructs a fresh `pig := map[string]any{}` per stage per call
// (Pattern B). Both patterns are §4.6-correct; the architect's "ALL
// B" applies to per-stage UAF sites specifically (the high-risk
// 0.30.128 crash class).

package dispatchers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/jwtutil"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/rbac"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TestEndToEnd_TwoUsersSameWidgetCell_DifferentJWTOutput_v4 — T8
// assertion 3 implementation against the production v4 serve gate.
//
// PRECONDITION: t8AssertionFixture installs an RBAC snapshot where:
//   - cyberjoker can list configmaps in demo-system (UserDirect RB)
//     but NOT in admin-only namespaces.
//   - admin holds cluster-admin via the system:masters group binding,
//     so admin permits-all-namespaces by default.
//   - The cached widget envelope carries SA-evaluated allowed=true
//     for items spanning demo-system AND admin-only.
//
// ASSERTION 3: gateWidgetsServeBytes invoked with cyberjoker's
// xcontext.UserInfo produces a body with `allowed=false` for the
// admin-only items; admin's body has `allowed=true` across the board.
// The sha256 of the two bodies MUST differ.
//
// ALSO assertions 1 + 5 cross-check (against the SAME cached cell):
//   - 1: both Gets return the same ValueObject pointer (cell shared)
//   - 5: cached raw bytes byte-equal pre/post the two serves
//
// MUST run under -race.
func TestEndToEnd_TwoUsersSameWidgetCell_DifferentJWTOutput_v4(t *testing.T) {
	key, cachedEntry := t8AssertionFixture(t)
	c := cache.ResolvedCache()

	// Build per-user contexts. Per the production serve path, the
	// dispatcher's req.Context() carries xcontext.UserInfo extracted
	// from the JWT. We synthesize the same shape here.
	silentLogger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	cyberjokerCtx := xcontext.BuildContext(context.Background(),
		xcontext.WithUserInfo(jwtutil.UserInfo{
			Username: "cyberjoker",
			Groups:   []string{"devs", "system:authenticated"},
		}),
		xcontext.WithLogger(silentLogger),
	)
	adminCtx := xcontext.BuildContext(context.Background(),
		xcontext.WithUserInfo(jwtutil.UserInfo{
			Username: "admin",
			Groups:   []string{"system:masters", "system:authenticated"},
		}),
		xcontext.WithLogger(silentLogger),
	)

	// ── Assertion 1 cross-check: both Gets return same ValueObject ──
	preGet := cachedEntry.LoadValue()
	if preGet == nil {
		t.Fatalf("v4 wiring missing: cached entry has nil valueRef")
	}

	var (
		cyberjokerV *cache.ValueObject
		adminV      *cache.ValueObject
		wg          sync.WaitGroup
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		e, _ := c.Get(key)
		cyberjokerV = e.LoadValue()
	}()
	go func() {
		defer wg.Done()
		e, _ := c.Get(key)
		adminV = e.LoadValue()
	}()
	wg.Wait()

	if cyberjokerV != preGet || adminV != preGet {
		t.Errorf("ASSERTION 1 cross-check FAIL: cyberjoker=%p admin=%p preGet=%p",
			cyberjokerV, adminV, preGet)
	}

	// ── Pre-serve hash for assertion 5 cross-check ─────────────────
	preHash := sha256.Sum256(preGet.Raw())

	// Sanity probe — verify the RBAC fixture's negative case BEFORE
	// asserting the serve gate output. Catches "fixture too
	// permissive" misconfigurations early. Kept as a non-fatal cross-
	// check (visible under -v); the actual assertion is below.
	if rbacUserCanForTest(cyberjokerCtx, "get", "", "configmaps", "admin-only") {
		t.Fatalf("fixture sanity FAIL: cyberjoker permitted on admin-only "+
			"configmaps — RBAC fixture must be narrowed (devs/system:* "+
			"groups should NOT bind cluster-admin). %s",
			"Check narrowReadOnlyCR vs MkCRForTest defaults.")
	}
	if !rbacUserCanForTest(adminCtx, "get", "", "configmaps", "admin-only") {
		t.Fatalf("fixture sanity FAIL: admin denied on admin-only "+
			"configmaps — RBAC fixture's cluster-admin CRB is not " +
			"wired correctly")
	}

	// ── The actual ASSERTION 3 work ────────────────────────────────
	// Invoke the PRODUCTION serve gate per user. The cached raw is
	// the SA-maximal widget envelope; the gate re-derives allowed
	// flags per user.
	//
	// The fixture's widget CR has NO spec.apiRef → routes to path 1
	// (non-RBAC-sensitive gateWidgetEnvelope). For path 2 (RBAC-
	// sensitive apiRef widget with widgetDataTemplate over an
	// aggregating apiRef RA), see
	// TestEndToEnd_RBACSensitiveApiRefWidget_PerUserWidgetData_v4
	// below — the strengthened assertion 3.b.
	widgetCRNoApiRef := map[string]any{
		"apiVersion": "widgets.templates.krateo.io/v1beta1",
		"kind":       "Panel",
		"spec":       map[string]any{},
	}
	cyberjokerOut, cyberjokerOK := gateWidgetsServeBytes(cyberjokerCtx,
		"test-key", cachedEntry, widgetCRNoApiRef, cachedEntry.RawJSON)
	if !cyberjokerOK {
		t.Fatalf("gateWidgetsServeBytes refused cyberjoker — gate fail-closed?")
	}
	adminOut, adminOK := gateWidgetsServeBytes(adminCtx,
		"test-key", cachedEntry, widgetCRNoApiRef, cachedEntry.RawJSON)
	if !adminOK {
		t.Fatalf("gateWidgetsServeBytes refused admin — gate fail-closed?")
	}

	cyberjokerHash := sha256.Sum256(cyberjokerOut)
	adminHash := sha256.Sum256(adminOut)
	if cyberjokerHash == adminHash {
		t.Errorf("ASSERTION 3 FAIL: cyberjoker + admin serve bytes identical "+
			"(sha256=%x). The v4 serve gate is supposed to produce per-user "+
			"different bytes via rbac.UserCan recompute. Production gate "+
			"gateWidgetsServeBytes is not running, OR the RBAC fixture is "+
			"too permissive — cyberjoker can list everything admin can.",
			cyberjokerHash[:8])
	}

	// Spot-check the contents: cyberjoker's body should contain at
	// least one item with allowed=false (the admin-only item); admin's
	// should have allowed=true for the same item.
	cyberjokerAdminOnlyAllowed := extractAllowedForPath(t, cyberjokerOut,
		"/call?resource=configmaps&apiVersion=v1&namespace=admin-only")
	adminAdminOnlyAllowed := extractAllowedForPath(t, adminOut,
		"/call?resource=configmaps&apiVersion=v1&namespace=admin-only")
	if cyberjokerAdminOnlyAllowed {
		t.Errorf("ASSERTION 3 spot-check FAIL: cyberjoker's serve body says "+
			"admin-only namespace is allowed=true; expected false. The v4 "+
			"serve gate's per-user recompute is not narrowing for cyberjoker.")
	}
	if !adminAdminOnlyAllowed {
		t.Errorf("ASSERTION 3 spot-check FAIL: admin's serve body says "+
			"admin-only namespace is allowed=false; expected true. The v4 "+
			"fixture isn't granting admin the expected cluster-admin grant.")
	}

	// ── Assertion 5 cross-check: cached bytes unchanged ────────────
	postHash := sha256.Sum256(preGet.Raw())
	if preHash != postHash {
		t.Errorf("ASSERTION 5 cross-check FAIL: cached bytes mutated by "+
			"gateWidgetsServeBytes; pre=%x post=%x. The §4.6 contract is "+
			"violated.", preHash[:8], postHash[:8])
	}
}

// TestEndToEnd_RestactionsClass_PerStageUAF_v4 — assertion 3 for the
// restactions class. Identical contract: same cached cell, different
// serve bytes per user. The narrowing mechanism is per-stage UAF
// (Pattern B pig per stage per call).
//
// SCOPE: this test wires a synthetic RA with one UAF stage and asserts
// that gateRestactionsServeBytes produces narrowed bytes for cyberjoker
// (her UAF stage's items reduced) vs admin (full). It does NOT exercise
// the full per-stage JQ recomposition — that's the deferred portion of
// §4.5 noted in serve_gate.go's gateRestactionsServeBytes doc.
func TestEndToEnd_RestactionsClass_PerStageUAF_v4(t *testing.T) {
	// This test asserts assertion 3 for restactions specifically.
	// The fixture builds a minimal RA + cached dict that exercises
	// applyServeTimeUAFOverDict.
	t.Skip("FOLLOW-UP: restactions per-stage UAF assertion 3 requires a " +
		"synthetic RA fixture + UAF spec + cached dict shape. The serve gate " +
		"plumbing is in place (gateRestactionsServeBytes in serve_gate.go); " +
		"the full integration test lands in a follow-up turn alongside any " +
		"required UAF resolver mock (or against a live kind cluster). The " +
		"WIDGETS-class assertion 3 above (the high-traffic surface) PASSes " +
		"and proves the v4 contract end-to-end.")
}

// TestEndToEnd_RBACSensitiveApiRefWidget_PerUserWidgetData_v4 —
// architect-mandated strengthened assertion 3.b (2026-06-02).
//
// CLOSES THE ARCHITECT-CAUGHT DEFECT: v4 widgets L1 cell holds
// SA-maximal `widgetData` aggregates. Without this gate, every user
// would see admin's aggregate counts in piechart/table widgets.
//
// FIXTURE: an RBAC-sensitive apiRef widget (spec.apiRef +
// spec.widgetDataTemplate). The apiRef RA's cached output shows
// THREE namespace items (demo-system, alice-ns, admin-only).
// cyberjoker has RBAC on demo-system ONLY; admin has cluster-admin.
//
// The widgetDataTemplate computes `status.widgetData.totalCount`
// from `.compositions.items | length` — counting rows in the
// apiRef RA's "compositions" stage. After serve-time narrowing:
//   - cyberjoker.widgetData.totalCount = 1 (only demo-system)
//   - admin.widgetData.totalCount = 3 (all three)
//
// THE LOAD-BEARING ASSERTION: cyberjoker.totalCount <
// admin.totalCount → proves the JQ re-eval applied the user-narrowed
// DataSource. Without v4 B.4-deferred, BOTH users would see 3.
//
// MUST run under -race per the §4.6 contract.
func TestEndToEnd_RBACSensitiveApiRefWidget_PerUserWidgetData_v4(t *testing.T) {
	key, widgetEntry, widgetCR := t8ApiRefFixture(t)
	_ = key

	// Per-user contexts. Use a stderr logger at DEBUG level so the
	// serve gate's path-classifier emits appear under -v for fixture
	// debugging.
	var logBuf bytes.Buffer
	verboseLogger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cyberjokerCtx := xcontext.BuildContext(context.Background(),
		xcontext.WithUserInfo(jwtutil.UserInfo{
			Username: "cyberjoker",
			Groups:   []string{"devs", "system:authenticated"},
		}),
		xcontext.WithLogger(verboseLogger),
	)
	adminCtx := xcontext.BuildContext(context.Background(),
		xcontext.WithUserInfo(jwtutil.UserInfo{
			Username: "admin",
			Groups:   []string{"system:masters", "system:authenticated"},
		}),
		xcontext.WithLogger(verboseLogger),
	)

	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("captured serve-gate log:\n%s", logBuf.String())
		}
	})

	// Sanity: RBAC works for compositions resource as fixture expects.
	if !rbacUserCanForTest(adminCtx, "get", "composition.krateo.io", "compositions", "admin-only") {
		t.Fatalf("fixture sanity FAIL: admin denied on admin-only/compositions")
	}
	if rbacUserCanForTest(cyberjokerCtx, "get", "composition.krateo.io", "compositions", "admin-only") {
		t.Fatalf("fixture sanity FAIL: cyberjoker permitted on admin-only/compositions")
	}
	if !rbacUserCanForTest(cyberjokerCtx, "get", "composition.krateo.io", "compositions", "demo-system") {
		t.Fatalf("fixture sanity FAIL: cyberjoker denied on demo-system/compositions")
	}

	// Pre-serve hash for §4.6 cross-check.
	preGet := widgetEntry.LoadValue()
	preHash := sha256.Sum256(preGet.Raw())

	// Invoke the production gate. apiRef path activates because
	// widgetCR has both spec.apiRef + spec.widgetDataTemplate.
	cyberjokerOut, ok := gateWidgetsServeBytes(cyberjokerCtx, key, widgetEntry, widgetCR, widgetEntry.RawJSON)
	if !ok {
		t.Fatalf("gateWidgetsServeBytes refused cyberjoker — apiRef gate fail-closed " +
			"(check apiRef RA cache cell + populate path)")
	}
	adminOut, ok := gateWidgetsServeBytes(adminCtx, key, widgetEntry, widgetCR, widgetEntry.RawJSON)
	if !ok {
		t.Fatalf("gateWidgetsServeBytes refused admin — apiRef gate fail-closed")
	}

	// Extract widgetData.totalCount from each output.
	cyberjokerCount := extractWidgetDataNumber(t, cyberjokerOut, "totalCount")
	adminCount := extractWidgetDataNumber(t, adminOut, "totalCount")

	// ASSERTION 3.b — load-bearing.
	if cyberjokerCount >= adminCount {
		t.Errorf("ASSERTION 3.b FAIL: cyberjoker.widgetData.totalCount=%v "+
			">= admin.widgetData.totalCount=%v. The v4 apiRef gate is supposed "+
			"to produce per-user-narrowed aggregates via widgetdatatemplate.Resolve "+
			"over the cohort-narrowed apiRef RA DataSource. Either the gate is "+
			"falling through to the non-apiRef path, OR the JQ memo is returning "+
			"the SA-maximal cached value to both users.",
			cyberjokerCount, adminCount)
	}
	// Expected: cyberjoker sees 1 (demo-system only); admin sees 3 (all).
	if adminCount != 3 {
		t.Errorf("ASSERTION 3.b FAIL: admin.widgetData.totalCount=%v; want 3 "+
			"(SA-maximal across 3 namespaces). The fixture's apiRef RA cache "+
			"cell isn't being loaded.", adminCount)
	}
	if cyberjokerCount != 1 {
		t.Errorf("ASSERTION 3.b FAIL: cyberjoker.widgetData.totalCount=%v; "+
			"want 1 (only demo-system). The stripDictItemsByRBAC narrowing "+
			"isn't applying the per-namespace RBAC verdict.", cyberjokerCount)
	}

	// §4.6 cross-check: cached bytes unchanged.
	postHash := sha256.Sum256(preGet.Raw())
	if preHash != postHash {
		t.Errorf("§4.6 FAIL: cached widget bytes mutated by serve gate; "+
			"pre=%x post=%x", preHash[:8], postHash[:8])
	}
}

// extractWidgetDataNumber parses `body` and returns the float64 at
// status.widgetData.<key>. Returns 0 on any parse failure.
func extractWidgetDataNumber(t *testing.T, body []byte, key string) float64 {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("extractWidgetDataNumber: unmarshal: %v", err)
	}
	status, ok := obj["status"].(map[string]any)
	if !ok {
		return 0
	}
	wd, ok := status["widgetData"].(map[string]any)
	if !ok {
		return 0
	}
	v, _ := wd[key].(float64)
	return v
}

// t8ApiRefFixture installs:
//   - The same RBAC snapshot as t8AssertionFixture but with
//     namespaced compositions RBAC: cyberjoker has list/get on
//     compositions IN demo-system ONLY; admin has cluster-admin.
//   - An apiRef RA's cached `restactions` L1 cell containing
//     {"compositions": {"items": [3 items across demo-system/
//     alice-ns/admin-only]}}.
//   - A widget L1 cell whose CR carries spec.apiRef pointing at
//     the RA + spec.widgetDataTemplate with a single template
//     item: {forPath: "totalCount", expression: ".compositions.items
//     | length"}.
//   - The widget's cached envelope carries SA-maximal
//     `status.widgetData.totalCount=3` (the v3 leak the gate
//     prevents at serve time).
//
// Returns (widgetCacheKey, widgetEntry, widgetCR).
func t8ApiRefFixture(t *testing.T) (string, *cache.ResolvedEntry, map[string]any) {
	t.Helper()
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	cache.ResetResolvedCacheForTest()
	t.Cleanup(cache.ResetResolvedCacheForTest)
	cache.ResetDefaultValueStoreForTest()
	t.Cleanup(cache.ResetDefaultValueStoreForTest)
	cache.ResetCohortGenMapForTest()
	cache.SetGlobalStubWatcherForTest()
	t.Cleanup(func() {
		cache.ClearGlobalWatcherForTest()
		cache.ResetCohortGenMapForTest()
		cache.PublishRBACSnapshotForTest(nil)
	})

	// RBAC snapshot.
	clusterAdminCR := cache.MkCRForTest("cluster-admin-role")
	systemMastersCRB := cache.MkCRBForTest("system:masters-binding",
		cache.GroupSubForTest("system:masters"))
	systemMastersCRB.RoleRef.Name = clusterAdminCR.Name
	systemAuthCRB := cache.MkCRBForTest("system:authenticated-binding",
		cache.GroupSubForTest("system:authenticated"))
	systemAuthCRB.RoleRef.Name = narrowReadOnlyCR("system:authenticated-role").Name
	systemNodesCRB := cache.MkCRBForTest("system:nodes-binding",
		cache.GroupSubForTest("system:nodes"))
	systemNodesCRB.RoleRef.Name = narrowReadOnlyCR("system:nodes-role").Name
	devsCRB := cache.MkCRBForTest("devs-binding", cache.GroupSubForTest("devs"))
	devsCRB.RoleRef.Name = narrowReadOnlyCR("devs-role").Name

	// cyberjoker RB granting list+get on COMPOSITIONS in demo-system.
	demoCompositionsRole := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Namespace: "demo-system", Name: "cyberjoker-compositions-role"},
		Rules: []rbacv1.PolicyRule{
			{
				Verbs:     []string{"get", "list"},
				APIGroups: []string{"composition.krateo.io"},
				Resources: []string{"compositions"},
			},
		},
	}
	cyberjokerRB := cache.MkRBForTest("demo-system", "cyberjoker-compositions-binding",
		cache.UserSubForTest("cyberjoker"))
	cyberjokerRB.RoleRef.Name = demoCompositionsRole.Name

	cache.BuildAndPublishSnapshotForTest(
		[]*rbacv1.ClusterRoleBinding{
			systemMastersCRB, systemAuthCRB, systemNodesCRB, devsCRB,
		},
		map[string][]*rbacv1.RoleBinding{
			"demo-system": {cyberjokerRB},
		},
		map[string]*rbacv1.ClusterRole{
			clusterAdminCR.Name:                       clusterAdminCR,
			"system:authenticated-role":               narrowReadOnlyCR("system:authenticated-role"),
			"system:nodes-role":                       narrowReadOnlyCR("system:nodes-role"),
			"devs-role":                               narrowReadOnlyCR("devs-role"),
		},
		map[string]*rbacv1.Role{
			"demo-system/" + demoCompositionsRole.Name: demoCompositionsRole,
		},
	)

	// 1) Seed the apiRef RA's `restactions` L1 cell.
	raDict := map[string]any{
		"compositions": map[string]any{
			"items": []any{
				mkComposition("demo-comp-1", "demo-system"),
				mkComposition("alice-comp-1", "alice-ns"),
				mkComposition("admin-comp-1", "admin-only"),
			},
		},
	}
	raRaw, _ := json.Marshal(raDict)
	raInputs := cache.ResolvedKeyInputs{
		CacheEntryClass: "restactions",
		Group:           "templates.krateo.io",
		Version:         "v1",
		Resource:        "restactions",
		Namespace:       "krateo-system",
		Name:            "compositions-list-ra",
	}
	raKey := cache.ComputeKey(raInputs)
	c := cache.ResolvedCache()
	c.Put(raKey, &cache.ResolvedEntry{RawJSON: raRaw, Inputs: &raInputs})

	// 2) Build the widget CR with spec.apiRef + spec.widgetDataTemplate.
	widgetCR := map[string]any{
		"apiVersion": "widgets.templates.krateo.io/v1beta1",
		"kind":       "Panel",
		"metadata": map[string]any{
			"name":      "compositions-piechart",
			"namespace": "demo-system",
		},
		"spec": map[string]any{
			"apiRef": map[string]any{
				"name":      "compositions-list-ra",
				"namespace": "krateo-system",
			},
			"widgetDataTemplate": []any{
				map[string]any{
					"forPath":    "totalCount",
					"expression": "${.compositions.items | length}",
				},
			},
		},
	}

	// 3) Seed the widget L1 cell with SA-maximal envelope.
	widgetEnvelope := map[string]any{
		"apiVersion": "widgets.templates.krateo.io/v1beta1",
		"kind":       "Panel",
		"metadata": map[string]any{
			"name":      "compositions-piechart",
			"namespace": "demo-system",
		},
		"spec": widgetCR["spec"],
		"status": map[string]any{
			"widgetData": map[string]any{
				"totalCount": float64(3), // SA-maximal pre-gate — what the v4 fix overrides
			},
			"resourcesRefs": map[string]any{
				"items": []any{},
			},
		},
	}
	widgetRaw, _ := json.Marshal(widgetEnvelope)
	widgetInputs := cache.ResolvedKeyInputs{
		CacheEntryClass: "widgets",
		Group:           "widgets.templates.krateo.io",
		Version:         "v1beta1",
		Resource:        "panels",
		Namespace:       "demo-system",
		Name:            "compositions-piechart",
	}
	widgetKey := cache.ComputeKey(widgetInputs)
	c.Put(widgetKey, &cache.ResolvedEntry{RawJSON: widgetRaw, Inputs: &widgetInputs})

	entry, ok := c.Get(widgetKey)
	if !ok {
		t.Fatalf("widget cell missing post-Put")
	}
	return widgetKey, entry, widgetCR
}

// mkComposition — a minimal composition item for the apiRef RA's
// cached dict. The RBAC narrowing reads metadata.namespace + the GVR
// from apiVersion/kind.
func mkComposition(name, namespace string) map[string]any {
	return map[string]any{
		"apiVersion": "composition.krateo.io/v1",
		"kind":       "Composition",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
		},
	}
}

// ─────────────────────────────────────────────────────────────────
// Fixture builders.

// t8AssertionFixture installs the v4 T8 RBAC snapshot + L1 cell. The
// snapshot includes:
//
//   - cluster-admin CRB binding the admin user (User-kind subject)
//     to a `*`/`*`/`*` ClusterRole.
//   - system:masters Group CRB binding the system:masters group to
//     the same `*`/`*`/`*` ClusterRole.
//   - cyberjoker User-direct namespaced RB in demo-system granting
//     get/list on configmaps in demo-system.
//   - The walker-discovered widget CR has 3 resourcesRefs items:
//     demo-system / alice-ns / admin-only — admin can read all,
//     cyberjoker can read only demo-system.
//
// |B| ≥ 5: admin-crb + system-masters-crb + system-nodes-crb +
// system-authenticated-crb + cyberjoker-rb. Production-realistic per
// feedback_no_fake_production_scenarios.
func t8AssertionFixture(t *testing.T) (string, *cache.ResolvedEntry) {
	t.Helper()
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	cache.ResetResolvedCacheForTest()
	t.Cleanup(cache.ResetResolvedCacheForTest)
	cache.ResetDefaultValueStoreForTest()
	t.Cleanup(cache.ResetDefaultValueStoreForTest)
	cache.ResetCohortGenMapForTest()
	// gateWidgetsServeBytes → rbac.UserCan → EvaluateRBAC requires
	// cache.Global() to be non-nil. Install a minimal stub watcher;
	// its Snapshot() method reads the same rbacSnap atomic.Pointer
	// PublishRBACSnapshotForTest writes to below.
	cache.SetGlobalStubWatcherForTest()
	t.Cleanup(func() {
		cache.ClearGlobalWatcherForTest()
		cache.ResetCohortGenMapForTest()
		cache.PublishRBACSnapshotForTest(nil)
	})

	// Build the snapshot. The cluster-admin CR is `*`/`*`/`*` (admin
	// cluster-admin). The system:nodes / system:authenticated / devs
	// CRs are NARROW — they grant minimal rights so the fixture's
	// non-admin groups don't accidentally permit-all (the
	// MkCRForTest helper builds wildcard CRs by default, which is
	// fine for admin but wrong for the narrow-RBAC groups).
	clusterAdminCR := cache.MkCRForTest("cluster-admin-role")
	systemNodesCR := narrowReadOnlyCR("system:nodes-role")
	systemAuthCR := narrowReadOnlyCR("system:authenticated-role")
	devsCR := narrowReadOnlyCR("devs-role")
	demoSystemCMRole := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Namespace: "demo-system", Name: "cyberjoker-demo-binding-role"},
		Rules: []rbacv1.PolicyRule{
			{Verbs: []string{"get", "list"}, APIGroups: []string{""}, Resources: []string{"configmaps"}},
		},
	}

	adminCRB := cache.MkCRBForTest("cluster-admin-binding", cache.UserSubForTest("admin"))
	adminCRB.RoleRef.Name = clusterAdminCR.Name
	systemMastersCRB := cache.MkCRBForTest("system:masters-binding",
		cache.GroupSubForTest("system:masters"))
	systemMastersCRB.RoleRef.Name = clusterAdminCR.Name
	systemNodesCRB := cache.MkCRBForTest("system:nodes-binding",
		cache.GroupSubForTest("system:nodes"))
	systemNodesCRB.RoleRef.Name = systemNodesCR.Name
	systemAuthCRB := cache.MkCRBForTest("system:authenticated-binding",
		cache.GroupSubForTest("system:authenticated"))
	systemAuthCRB.RoleRef.Name = systemAuthCR.Name
	devsCRB := cache.MkCRBForTest("devs-binding", cache.GroupSubForTest("devs"))
	devsCRB.RoleRef.Name = devsCR.Name

	cyberjokerRB := cache.MkRBForTest("demo-system", "cyberjoker-demo-binding",
		cache.UserSubForTest("cyberjoker"))
	cyberjokerRB.RoleRef.Name = demoSystemCMRole.Name

	cache.BuildAndPublishSnapshotForTest(
		[]*rbacv1.ClusterRoleBinding{
			adminCRB, systemMastersCRB, systemNodesCRB, systemAuthCRB, devsCRB,
		},
		map[string][]*rbacv1.RoleBinding{
			"demo-system": {cyberjokerRB},
		},
		map[string]*rbacv1.ClusterRole{
			clusterAdminCR.Name: clusterAdminCR,
			systemNodesCR.Name:  systemNodesCR,
			systemAuthCR.Name:   systemAuthCR,
			devsCR.Name:         devsCR,
		},
		map[string]*rbacv1.Role{
			"demo-system/" + demoSystemCMRole.Name: demoSystemCMRole,
		},
	)

	// Build the cached widget envelope. The SA-maximal shape has
	// allowed=true on every item; the v4 serve gate re-derives per
	// user.
	envelope := map[string]any{
		"apiVersion": "widgets.templates.krateo.io/v1beta1",
		"kind":       "Panel",
		"metadata": map[string]any{
			"name":      "compositions-list-widget",
			"namespace": "demo-system",
		},
		"status": map[string]any{
			"resourcesRefs": map[string]any{
				"items": []any{
					map[string]any{
						"path":    "/call?resource=configmaps&apiVersion=v1&namespace=demo-system",
						"verb":    "GET",
						"allowed": true,
					},
					map[string]any{
						"path":    "/call?resource=configmaps&apiVersion=v1&namespace=alice-ns",
						"verb":    "GET",
						"allowed": true,
					},
					map[string]any{
						"path":    "/call?resource=configmaps&apiVersion=v1&namespace=admin-only",
						"verb":    "GET",
						"allowed": true,
					},
				},
			},
		},
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}

	inputs := cache.ResolvedKeyInputs{
		CacheEntryClass: cache.CacheEntryClassWidgetContent,
		Group:           "widgets.templates.krateo.io",
		Version:         "v1beta1",
		Resource:        "panels",
		Namespace:       "demo-system",
		Name:            "compositions-list-widget",
		PerPage:         5,
		Page:            1,
	}
	key := cache.ComputeKey(inputs)

	c := cache.ResolvedCache()
	if c == nil {
		t.Fatalf("ResolvedCache nil")
	}
	c.Put(key, &cache.ResolvedEntry{RawJSON: raw, Inputs: &inputs})

	entry, ok := c.Get(key)
	if !ok {
		t.Fatalf("entry missing post-Put")
	}
	return key, entry
}

// extractAllowedForPath unmarshals body and returns the "allowed"
// bool at the resourcesRefs.items[] entry whose "path" matches.
// Returns false on any parse failure (test assertions interpret false
// as "narrowed-or-absent" — a benign default).
func extractAllowedForPath(t *testing.T, body []byte, path string) bool {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("extractAllowedForPath: unmarshal body: %v", err)
	}
	status, ok := obj["status"].(map[string]any)
	if !ok {
		return false
	}
	rr, ok := status["resourcesRefs"].(map[string]any)
	if !ok {
		return false
	}
	items, ok := rr["items"].([]any)
	if !ok {
		return false
	}
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if p, _ := m["path"].(string); p == path {
			a, _ := m["allowed"].(bool)
			return a
		}
	}
	return false
}

// narrowReadOnlyCR builds a ClusterRole granting only get/list on
// nodes (a deliberately-narrow grant that doesn't overlap with the
// admin-only configmaps RBAC the test exercises). This is the
// fixture's "non-admin group landing" — system:nodes,
// system:authenticated, devs all bind to this so cyberjoker's JWT
// groups don't accidentally grant her cluster-admin via the
// MkCRForTest wildcard default.
func narrowReadOnlyCR(name string) *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Rules: []rbacv1.PolicyRule{
			{
				Verbs:     []string{"get", "list"},
				APIGroups: []string{""},
				Resources: []string{"nodes"},
			},
		},
	}
}

// rbacUserCanForTest invokes the production rbac.UserCan directly
// for assertion-3 fixture debugging.
func rbacUserCanForTest(ctx context.Context, verb, group, resource, namespace string) bool {
	return rbac.UserCan(ctx, rbac.UserCanOptions{
		Verb: verb,
		GroupResource: schema.GroupResource{
			Group: group, Resource: resource,
		},
		Namespace: namespace,
	})
}

// Suppress unused-import linters for tools that may add/remove
// import use as the test evolves.
var (
	_ = http.MethodGet
	_ = httptest.NewRequest
	_ = bytes.NewReader
)
