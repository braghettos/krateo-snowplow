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
