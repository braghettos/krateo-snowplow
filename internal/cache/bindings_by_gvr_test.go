// bindings_by_gvr_test.go — Ship 1 unit tests for the incremental
// BindingsByGVR reverse index. Package cache (not cache_test) so it can
// reach the unexported build/enumerate/delta helpers + PublishRBACSnapshotForTest.
//
// NON-DESTRUCTIVE — builds synthetic snapshots in-process; never touches
// the rbac package's destructive TestMain. Safe under `go test
// ./internal/cache/...`.

package cache

import (
	"expvar"
	"sort"
	"sync"
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// gr is a terse GVR (group, resource) for the tests (version irrelevant
// to the index).
func gr(group, resource string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: group, Resource: resource}
}

// crbRuleUID builds a CRB whose roleRef points at a ClusterRole carrying
// the given rules. Returns the CRB + the CR. uid makes the binding
// identity stable across the index build + delta hooks.
func crbRuleUID(name, uid string, sub rbacv1.Subject, rules []rbacv1.PolicyRule) (*rbacv1.ClusterRoleBinding, *rbacv1.ClusterRole) {
	cr := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-role"},
		Rules:      rules,
	}
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(uid)},
		Subjects:   []rbacv1.Subject{sub},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: name + "-role"},
	}
	return crb, cr
}

func wildcardRules() []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{{Verbs: []string{"*"}, APIGroups: []string{"*"}, Resources: []string{"*"}}}
}

func getListRules(group, resource string) []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{{
		Verbs:     []string{"get", "list"},
		APIGroups: []string{group},
		Resources: []string{resource},
	}}
}

// buildSnap assembles a snapshot from CRBs + CRs (+ their subject indexes
// so the cohort hash helpers stay consistent).
func buildSnap(crbs []*rbacv1.ClusterRoleBinding, crs []*rbacv1.ClusterRole) *RBACSnapshot {
	s := &RBACSnapshot{
		ClusterRoleBindings: crbs,
		RoleBindingsByNS:    map[string][]*rbacv1.RoleBinding{},
		ClusterRolesByName:  map[string]*rbacv1.ClusterRole{},
		RolesByNSName:       map[string]*rbacv1.Role{},
	}
	for _, cr := range crs {
		s.ClusterRolesByName[cr.Name] = cr
	}
	rebuildSubjectIndexes(s)
	return s
}

func TestBindingsByGVRIndex_WildcardMatchesEveryGVR(t *testing.T) {
	ResetBindingsByGVRIndexForTest()

	adminCRB, adminCR := crbRuleUID("admin-binding", "uid-admin",
		rbacv1.Subject{Kind: "User", Name: "admin"}, wildcardRules())
	snap := buildSnap([]*rbacv1.ClusterRoleBinding{adminCRB}, []*rbacv1.ClusterRole{adminCR})
	PublishRBACSnapshotForTest(snap)

	navigated := []schema.GroupVersionResource{
		gr("composition.krateo.io", "compositions"),
		gr("widgets.templates.krateo.io", "panels"),
	}
	if n := BuildBindingsByGVRIndex(navigated); n != 1 {
		t.Fatalf("expected 1 binding enrolled, got %d", n)
	}

	// The wildcard binding must match EVERY navigated GVR.
	for _, g := range navigated {
		cohorts := EnumerateResourceCohorts(g)
		if len(cohorts) != 1 {
			t.Fatalf("gvr %s: expected 1 cohort (admin via wildcard), got %d: %+v", g, len(cohorts), cohorts)
		}
		if cohorts[0].Username != "admin" {
			t.Fatalf("gvr %s: expected admin cohort, got %q", g, cohorts[0].Username)
		}
	}
}

func TestBindingsByGVRIndex_ExactMatchScopesToGVR(t *testing.T) {
	ResetBindingsByGVRIndexForTest()

	// devsCRB grants get/list ONLY on compositions.
	devsCRB, devsCR := crbRuleUID("devs-binding", "uid-devs",
		rbacv1.Subject{Kind: "Group", Name: "devs"},
		getListRules("composition.krateo.io", "compositions"))
	snap := buildSnap([]*rbacv1.ClusterRoleBinding{devsCRB}, []*rbacv1.ClusterRole{devsCR})
	PublishRBACSnapshotForTest(snap)

	compGVR := gr("composition.krateo.io", "compositions")
	panelGVR := gr("widgets.templates.krateo.io", "panels")
	BuildBindingsByGVRIndex([]schema.GroupVersionResource{compGVR, panelGVR})

	// devs matches compositions (group-only sentinel cohort).
	comp := EnumerateResourceCohorts(compGVR)
	if len(comp) != 1 {
		t.Fatalf("compositions: expected 1 cohort, got %d: %+v", len(comp), comp)
	}
	if comp[0].Username != groupOnlyCohortSentinel || len(comp[0].Groups) != 1 || comp[0].Groups[0] != "devs" {
		t.Fatalf("compositions cohort: want sentinel+[devs], got %+v", comp[0])
	}

	// devs does NOT match panels — the scoping must exclude it.
	if pan := EnumerateResourceCohorts(panelGVR); len(pan) != 0 {
		t.Fatalf("panels: expected 0 cohorts (devs doesn't grant panels), got %d: %+v", len(pan), pan)
	}
}

func TestBindingsByGVRIndex_DeltaAddRemove(t *testing.T) {
	ResetBindingsByGVRIndexForTest()

	compGVR := gr("composition.krateo.io", "compositions")

	// Start with admin (wildcard) only.
	adminCRB, adminCR := crbRuleUID("admin-binding", "uid-admin",
		rbacv1.Subject{Kind: "User", Name: "admin"}, wildcardRules())
	devsCRB, devsCR := crbRuleUID("devs-binding", "uid-devs",
		rbacv1.Subject{Kind: "Group", Name: "devs"},
		getListRules("composition.krateo.io", "compositions"))
	snap := buildSnap([]*rbacv1.ClusterRoleBinding{adminCRB, devsCRB},
		[]*rbacv1.ClusterRole{adminCR, devsCR})
	PublishRBACSnapshotForTest(snap)

	BuildBindingsByGVRIndex([]schema.GroupVersionResource{compGVR})
	if got := cohortUsernames(EnumerateResourceCohorts(compGVR)); !equalSorted(got, []string{"admin", groupOnlyCohortSentinel}) {
		t.Fatalf("after build: want [admin sentinel], got %v", got)
	}

	// DELETE devs binding → only admin (wildcard) remains.
	onBindingDelete(devsCRB)
	if got := cohortUsernames(EnumerateResourceCohorts(compGVR)); !equalSorted(got, []string{"admin"}) {
		t.Fatalf("after devs delete: want [admin], got %v", got)
	}

	// ADD a new binding granting compositions to a fresh group → it appears.
	opsCRB, opsCR := crbRuleUID("ops-binding", "uid-ops",
		rbacv1.Subject{Kind: "Group", Name: "ops"},
		getListRules("composition.krateo.io", "compositions"))
	// The new role must be in the published snapshot for the delta hook to
	// resolve its rules — republish with it.
	snap2 := buildSnap([]*rbacv1.ClusterRoleBinding{adminCRB, opsCRB},
		[]*rbacv1.ClusterRole{adminCR, opsCR})
	PublishRBACSnapshotForTest(snap2)
	onBindingAdd(opsCRB)
	if got := cohortUsernames(EnumerateResourceCohorts(compGVR)); !equalSorted(got, []string{"admin", groupOnlyCohortSentinel}) {
		t.Fatalf("after ops add: want [admin sentinel], got %v", got)
	}
}

func TestBindingsByGVRIndex_NotBuiltReturnsNil(t *testing.T) {
	ResetBindingsByGVRIndexForTest()
	if c := EnumerateResourceCohorts(gr("composition.krateo.io", "compositions")); c != nil {
		t.Fatalf("expected nil before build, got %+v", c)
	}
	if BindingsByGVRIndexBuilt() {
		t.Fatal("expected built=false before build")
	}
}

// TestBindingsByGVRIndex_DeltaNonTypedBumpsCanary (S1) asserts a delta
// event whose object is neither a typed *rbacv1.* pointer nor convertible
// bumps the drift canary instead of silently dropping.
func TestBindingsByGVRIndex_DeltaNonTypedBumpsCanary(t *testing.T) {
	ResetBindingsByGVRIndexForTest()
	// Build (empty navigated) so deltaActive() is true.
	PublishRBACSnapshotForTest(buildSnap(nil, nil))
	BuildBindingsByGVRIndex([]schema.GroupVersionResource{gr("composition.krateo.io", "compositions")})

	before := BindingsIndexDeltaSkippedNonTyped()
	// A garbage object — not typed, not a bytesObject/Unstructured.
	onBindingAdd("not-a-binding")
	if got := BindingsIndexDeltaSkippedNonTyped(); got != before+1 {
		t.Fatalf("expected drift canary +1 on non-typed delta, got before=%d after=%d", before, got)
	}
}

// TestRAFullListMemoExpvarPublished (0.30.210) asserts the
// snowplow_ra_full_list_memo expvar:
//   - is published under CACHE_ENABLED=true,
//   - returns a snapshot whose shape includes raKey + sliceShape + verdict
//     + recordedAtUnixSeconds + caller-CR labels per entry,
//   - reflects EVERY recorded (RA × sliceShape) entry (true + false verdicts)
//     with labels populated from RecordSliceabilityWithLabels,
//   - is safe under -race against concurrent record() writers (the race
//     coverage lives in TestRAFullList_ResidentAndMemoRace; this test adds
//     a concurrent walk-while-write check specifically against the snapshot
//     reader).
func TestRAFullListMemoExpvarPublished(t *testing.T) {
	resetSliceabilityMemoForTest()
	RegisterBindingsByGVRMetricsForTest()

	v := expvar.Get("snowplow_ra_full_list_memo")
	if v == nil {
		t.Fatal("snowplow_ra_full_list_memo not published")
	}

	// Seed two entries: one TRUE verdict + labels, one FALSE verdict + labels.
	raKeyA := ComputeKey(RAFullListKeyInputs("composition.krateo.io", "v1", "panels",
		"krateo-system", "compositions-panels", 0x1234, nil))
	shapeA := SliceShapeHash("apiref", "widgets.templates.krateo.io", "v1beta1",
		"tables", "krateo-system", "compositions-page-datagrid", "{}")
	labelsA := SliceabilityLabels{
		CallerClass:     "apiref",
		CallerGroup:     "widgets.templates.krateo.io",
		CallerVersion:   "v1beta1",
		CallerResource:  "tables",
		CallerNamespace: "krateo-system",
		CallerName:      "compositions-page-datagrid",
	}
	RecordSliceabilityWithLabels(raKeyA, shapeA, true, labelsA)

	raKeyB := ComputeKey(RAFullListKeyInputs("composition.krateo.io", "v1", "panels",
		"OTHER-NS", "compositions-panels", 0xC0FFEE, nil))
	shapeB := SliceShapeHash("apiref", "widgets.templates.krateo.io", "v1beta1",
		"charts", "krateo-system", "compositions-chart", "{ sum: 0 }")
	labelsB := SliceabilityLabels{
		CallerClass:     "apiref",
		CallerGroup:     "widgets.templates.krateo.io",
		CallerVersion:   "v1beta1",
		CallerResource:  "charts",
		CallerNamespace: "krateo-system",
		CallerName:      "compositions-chart",
	}
	RecordSliceabilityWithLabels(raKeyB, shapeB, false, labelsB)

	// Scrape via the expvar.Func interface — must return []SliceabilityMemoEntry.
	raw := v.(expvar.Func).Value()
	snap, ok := raw.([]SliceabilityMemoEntry)
	if !ok {
		t.Fatalf("expvar value wrong type: %T (want []SliceabilityMemoEntry)", raw)
	}
	if len(snap) == 0 {
		t.Fatal("snapshot empty after two RecordSliceabilityWithLabels calls")
	}

	findByName := func(callerName string) *SliceabilityMemoEntry {
		for i := range snap {
			if snap[i].CallerName == callerName {
				return &snap[i]
			}
		}
		return nil
	}

	gridEntry := findByName("compositions-page-datagrid")
	if gridEntry == nil {
		t.Fatalf("compositions-page-datagrid entry missing from snapshot: %+v", snap)
	}
	if !gridEntry.Verdict {
		t.Fatalf("compositions-page-datagrid verdict = %v, want true", gridEntry.Verdict)
	}
	if gridEntry.RAKey != raKeyA {
		t.Fatalf("compositions-page-datagrid raKey = %q, want %q", gridEntry.RAKey, raKeyA)
	}
	if gridEntry.SliceShape != shapeA {
		t.Fatalf("compositions-page-datagrid sliceShape = %q, want %q", gridEntry.SliceShape, shapeA)
	}
	if gridEntry.RecordedAtUnixSeconds == 0 {
		t.Fatalf("compositions-page-datagrid recordedAtUnixSeconds = 0, want non-zero")
	}
	// REQ-2 (0.30.210): on INSERT, recordedAt == lastUpdatedAt (same clock
	// read for both). The consumer cares about monotonicity, not byte-equality.
	if gridEntry.LastUpdatedAtUnixSeconds != gridEntry.RecordedAtUnixSeconds {
		t.Fatalf("on insert lastUpdatedAt (%d) should equal recordedAt (%d)",
			gridEntry.LastUpdatedAtUnixSeconds, gridEntry.RecordedAtUnixSeconds)
	}
	if gridEntry.CallerClass != labelsA.CallerClass ||
		gridEntry.CallerGroup != labelsA.CallerGroup ||
		gridEntry.CallerVersion != labelsA.CallerVersion ||
		gridEntry.CallerResource != labelsA.CallerResource ||
		gridEntry.CallerNamespace != labelsA.CallerNamespace {
		t.Fatalf("compositions-page-datagrid labels mismatch: got %+v want %+v", gridEntry, labelsA)
	}

	chartEntry := findByName("compositions-chart")
	if chartEntry == nil {
		t.Fatalf("compositions-chart entry missing from snapshot: %+v", snap)
	}
	if chartEntry.Verdict {
		t.Fatalf("compositions-chart verdict = %v, want false", chartEntry.Verdict)
	}
	if chartEntry.CallerName != "compositions-chart" || chartEntry.CallerResource != "charts" {
		t.Fatalf("compositions-chart labels mismatch: %+v", chartEntry)
	}

	// Refresh-in-place preserves recordedAt (first-freeze semantics) BUT
	// updates lastUpdatedAt to "now" (REQ-2 — Mode-3 stuck-vs-refreshing
	// discriminator).
	firstRecordedAt := gridEntry.RecordedAtUnixSeconds
	// Bump the simulated clock so a NEW lastUpdatedAt is observable.
	bumped := firstRecordedAt + 100
	f := func() int64 { return bumped }
	prev := nowUnix.Load()
	nowUnix.Store(&f)
	t.Cleanup(func() { nowUnix.Store(prev) })

	// Re-record FLIPPING the verdict for the SAME (raKey, shape) — labels arg
	// here is empty to assert that refresh-in-place preserves the ORIGINAL
	// labels (does not overwrite them with empty).
	RecordSliceabilityWithLabels(raKeyA, shapeA, false, SliceabilityLabels{})
	snap2 := v.(expvar.Func).Value().([]SliceabilityMemoEntry)
	grid2 := func() *SliceabilityMemoEntry {
		for i := range snap2 {
			if snap2[i].CallerName == "compositions-page-datagrid" {
				return &snap2[i]
			}
		}
		return nil
	}()
	if grid2 == nil {
		t.Fatal("compositions-page-datagrid entry lost after refresh")
	}
	if grid2.Verdict {
		t.Fatalf("verdict refresh did not flip: %+v", grid2)
	}
	if grid2.RecordedAtUnixSeconds != firstRecordedAt {
		t.Fatalf("recordedAt mutated on refresh: was %d now %d",
			firstRecordedAt, grid2.RecordedAtUnixSeconds)
	}
	// REQ-2: lastUpdatedAt tracks last-write, not first-freeze.
	if grid2.LastUpdatedAtUnixSeconds != bumped {
		t.Fatalf("lastUpdatedAt did not advance on refresh: got %d, want %d",
			grid2.LastUpdatedAtUnixSeconds, bumped)
	}
	if grid2.LastUpdatedAtUnixSeconds <= grid2.RecordedAtUnixSeconds {
		t.Fatalf("after refresh lastUpdatedAt (%d) must be > recordedAt (%d)",
			grid2.LastUpdatedAtUnixSeconds, grid2.RecordedAtUnixSeconds)
	}
	if grid2.CallerName != "compositions-page-datagrid" {
		t.Fatalf("refresh-in-place LOST labels: %+v", grid2)
	}
}

// TestRAFullListMemoSnapshotConcurrentWriters (0.30.210) — race coverage for
// the snapshot reader running concurrently with record() writers. The
// existing TestRAFullList_ResidentAndMemoRace exercises lookup/record but not
// the snapshot walk. Run under -race; passes when no race report fires and
// the snapshot is never observed with a nil entry.
func TestRAFullListMemoSnapshotConcurrentWriters(t *testing.T) {
	resetSliceabilityMemoForTest()

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// 4 writers churning distinct (raKey, shape) pairs.
	for w := 0; w < 4; w++ {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				rk := "rakey-" + itoa(w) + "-" + itoa(i%17)
				sh := SliceShapeHash("apiref", "g", "v", "r", "ns",
					"name-"+itoa(w)+"-"+itoa(i%13), "{}")
				RecordSliceabilityWithLabels(rk, sh, (i%2) == 0, SliceabilityLabels{
					CallerClass:    "apiref",
					CallerResource: "r",
					CallerName:     "name-" + itoa(w),
				})
			}
		}()
	}

	// 2 snapshot readers.
	for r := 0; r < 2; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				snap := SliceabilityMemoSnapshot()
				for _, e := range snap {
					// Defensive: an entry's raKey/sliceShape must not be empty
					// (we never record with empty strings, and the snapshot
					// must not invent any). Catches an interleaving where the
					// reader sees a half-initialised entry.
					if e.RAKey == "" || e.SliceShape == "" {
						panic("snapshot returned entry with empty raKey/sliceShape: " + e.RAKey + "/" + e.SliceShape)
					}
				}
			}
		}()
	}

	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestRAFullListServeExpvarPublished (clause-5) asserts the
// snowplow_ra_full_list_serve expvar is published and reflects a recorded
// Hit — so the tester can assert the cheap raFullList serve path over the
// LB.
func TestRAFullListServeExpvarPublished(t *testing.T) {
	RegisterBindingsByGVRMetricsForTest()
	v := expvar.Get("snowplow_ra_full_list_serve")
	if v == nil {
		t.Fatal("snowplow_ra_full_list_serve not published")
	}
	before := RAFullListServeSnapshot().Hit
	RecordRAFullListServe(RAFullListServeHit)
	if got := RAFullListServeSnapshot().Hit; got != before+1 {
		t.Fatalf("Hit counter did not advance: before=%d after=%d", before, got)
	}
	// The expvar Func must reflect the live counter at scrape time.
	m, ok := v.(expvar.Func).Value().(map[string]uint64)
	if !ok {
		t.Fatalf("expvar value wrong type: %T", v.(expvar.Func).Value())
	}
	if m["hit"] != RAFullListServeSnapshot().Hit {
		t.Fatalf("expvar hit=%d != snapshot hit=%d", m["hit"], RAFullListServeSnapshot().Hit)
	}
}

// ── helpers ──

func cohortUsernames(cs []Cohort) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Username)
	}
	return out
}

func equalSorted(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	ac := append([]string(nil), a...)
	bc := append([]string(nil), b...)
	sort.Strings(ac)
	sort.Strings(bc)
	for i := range ac {
		if ac[i] != bc[i] {
			return false
		}
	}
	return true
}

// TestBindingsByGVRIndex_ConcurrentRace (M1) — the concurrency falsifier.
// N goroutines mutate the index via the delta hooks (onBindingAdd /
// onBindingUpdate / onBindingDelete / onRoleObjectChanged), M goroutines
// enumerate (EnumerateResourceCohorts), and one rebuilds
// (BuildBindingsByGVRIndex) — all concurrent against the singleton. The
// real hazard is the RBAC informer processor goroutine mutating while the
// engine goroutine enumerates; the single-goroutine -race run never
// exercised it. Run with `go test -race`: asserts no data race, no panic.
func TestBindingsByGVRIndex_ConcurrentRace(t *testing.T) {
	ResetBindingsByGVRIndexForTest()

	compGVR := gr("composition.krateo.io", "compositions")
	panelGVR := gr("widgets.templates.krateo.io", "panels")
	navigated := []schema.GroupVersionResource{compGVR, panelGVR}

	// Seed an initial snapshot + build so deltaActive() is true and the
	// hooks do real work. Pre-build the churn roles into the snapshot so
	// the add hooks enrol rather than parking in byRole.
	adminCRB, adminCR := crbRuleUID("admin-binding", "uid-admin",
		rbacv1.Subject{Kind: "User", Name: "admin"}, wildcardRules())
	const poolSize = 16
	pool := make([]*rbacv1.ClusterRoleBinding, poolSize)
	crs := []*rbacv1.ClusterRole{adminCR}
	for i := 0; i < poolSize; i++ {
		crb, cr := crbRuleUID(
			"churn-"+itoa(i), "uid-churn-"+itoa(i),
			rbacv1.Subject{Kind: "Group", Name: "g" + itoa(i)},
			getListRules("composition.krateo.io", "compositions"))
		pool[i] = crb
		crs = append(crs, cr)
	}
	snap := buildSnap(append([]*rbacv1.ClusterRoleBinding{adminCRB}, pool...), crs)
	PublishRBACSnapshotForTest(snap)
	BuildBindingsByGVRIndex(navigated)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Mutators.
	for w := 0; w < 6; w++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			i := seed
			for {
				select {
				case <-stop:
					return
				default:
				}
				b := pool[i%poolSize]
				switch i % 4 {
				case 0:
					onBindingAdd(b)
				case 1:
					onBindingUpdate(b, b)
				case 2:
					onBindingDelete(b)
				case 3:
					onRoleObjectChanged(adminCR)
				}
				i++
			}
		}(w)
	}

	// Enumerators.
	for r := 0; r < 6; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = EnumerateResourceCohorts(compGVR)
				_ = EnumerateResourceCohorts(panelGVR)
			}
		}()
	}

	// One rebuilder.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			BuildBindingsByGVRIndex(navigated)
		}
	}()

	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
	// Reaching here without a -race report or panic is the pass.
}
