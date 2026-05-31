// phase1_cohort_independent_readiness_test.go — Ship 2 / 0.30.196
// falsifiers for the cohort-count-INDEPENDENT readiness redesign.
//
// THE LANDMINE THIS SHIPS AGAINST. Pre-0.30.196, phase1WarmupWith called
// the per-cohort PIP seed (Step 7.6) SYNCHRONOUSLY and BEFORE
// MarkPhase1Done (Step 8), and runPIPSeed FAIL-CLOSED when the enumerated
// cohort count exceeded a hardcoded cap (pipCohortCapDefault=50): it
// returned a `cohort_cap_exceeded` error, phase1WarmupWith returned
// WITHOUT calling MarkPhase1Done, and /readyz stayed 503 FOREVER. A
// customer with per-user User-kind RBAC bindings (cohort count = O(users))
// would hit cohort #51 and the pod would never go Ready.
//
// THE FIX (Ship 2 / 0.30.196):
//   - MarkPhase1Done is called immediately after the cohort-INDEPENDENT
//     substrate is warm (sync barrier + content pass) and BEFORE the
//     per-cohort seed.
//   - the per-cohort seed runs as a bounded best-effort BACKGROUND warm
//     whose outcome is log-only — it never withholds readiness.
//   - the cohort cap + the cohort_cap_exceeded fail-closed branch are
//     DELETED.
//
// THREE FALSIFIERS, each exercising a DISTINCT real code path:
//
//  1. TestRunPIPSeed_NoCapWhenClassesExceedFifty — the CAP-DELETION
//     falsifier. Drives the ACTUAL runPIPSeed (not a stub) against a
//     published RBAC snapshot whose EnumerateBindingSetClasses yields > 50
//     binding-set classes, and asserts it returns no error and emits no
//     `cohort_cap_exceeded` line. Against OLD code this FAILS (the
//     `if len(cohorts) > cap` branch fires). PM review caught that the
//     prior >50 test stubbed pipSeed with a nil-returning closure and so
//     never reached the cap check — a tautology; this replaces it.
//
//  2. TestPhase1_ReadinessFlips_BeforeBackgroundSeedCompletes — the
//     DECOUPLING falsifier. While the seed is blocked, readiness is
//     already 200. Against OLD code phase1WarmupWith HANGS on the seed.
//
//  3. TestPhase1_ReadinessFlips_WhenSeedErrors — readiness flips even
//     when the seed errors. Against OLD code a non-nil seed return skips
//     MarkPhase1Done.

package dispatchers

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/snowplow/internal/cache"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// publishNUserCohortSnapshot publishes an RBAC snapshot with n distinct
// User-kind cohorts: n ClusterRoleBindings, each with a distinct name +
// distinct UID and a single distinct User subject. Distinct UIDs give
// each user a distinct matched-binding-id set, so BindingSetHash yields n
// distinct classes — EnumerateBindingSetClasses returns n cohorts. Uses
// ONLY exported cache symbols (cross-package: this test is in the
// dispatchers package, the cache fixture helpers are unexported). The
// snapshot is reset to nil on cleanup so the package's other tests see a
// clean slate.
func publishNUserCohortSnapshot(t *testing.T, n int) {
	t.Helper()
	crbs := make([]*rbacv1.ClusterRoleBinding, 0, n)
	byUser := map[string][]*rbacv1.ClusterRoleBinding{}
	for i := 0; i < n; i++ {
		user := "user-" + itoa(i)
		crb := &rbacv1.ClusterRoleBinding{
			TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
			ObjectMeta: metav1.ObjectMeta{
				Name: "bind-" + itoa(i),
				// Distinct UID per binding — Ship 1 (0.30.195) hashes
				// metadata.uid, so distinct UIDs guarantee distinct
				// per-cohort binding-id sets → distinct BindingSetHash →
				// distinct classes (no accidental dedupe collapse).
				UID: types.UID("uid-" + itoa(i)),
			},
			Subjects: []rbacv1.Subject{{
				Kind: "User", APIGroup: "rbac.authorization.k8s.io", Name: user,
			}},
			RoleRef: rbacv1.RoleRef{
				APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "role-" + itoa(i),
			},
		}
		crbs = append(crbs, crb)
		byUser[user] = []*rbacv1.ClusterRoleBinding{crb}
	}
	snap := &cache.RBACSnapshot{
		ClusterRoleBindings: crbs,
		CRBsByUser:          byUser,
	}
	cache.PublishRBACSnapshotForTest(snap)
	t.Cleanup(func() { cache.PublishRBACSnapshotForTest(nil) })
}

// itoa is a tiny dependency-free int->string (avoids importing strconv
// just for the fixture loop).
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}

// TestRunPIPSeed_NoCapWhenClassesExceedFifty is the Ship 2 / 0.30.196
// CAP-DELETION falsifier. It drives the REAL runPIPSeed against a snapshot
// whose EnumerateBindingSetClasses yields 60 binding-set classes (> the
// OLD cap of 50) and asserts:
//
//   - EnumerateBindingSetClasses actually returns > 50 (fixture sanity), AND
//   - runPIPSeed returns NO error, AND
//   - NO `cohort_cap_exceeded` log line is emitted.
//
// Against OLD code (cap=50 present) the `if len(cohorts) > cap` branch
// fires: runPIPSeed logs `cohort_cap_exceeded` and returns a non-nil
// error — both assertions below FAIL. This is a genuine falsifier of the
// deletion, not a tautology.
//
// HERMETIC: the harvesters are empty, so seedCohort iterates zero
// restactions + zero widgets per cohort and returns nil with NO apiserver
// / REST round-trip. The zero-value SA endpoint + nil *rest.Config are
// never dereferenced (withCohortSeedContext only installs context values).
func TestRunPIPSeed_NoCapWhenClassesExceedFifty(t *testing.T) {
	const n = 60 // > old cap 50
	publishNUserCohortSnapshot(t, n)

	// Fixture sanity: confirm the snapshot really enumerates > 50 classes —
	// otherwise the test would pass vacuously.
	if got := len(cache.EnumerateBindingSetClasses()); got <= 50 {
		t.Fatalf("fixture sanity: EnumerateBindingSetClasses returned %d classes; "+
			"need > 50 to exercise the deleted cap branch", got)
	}

	// Capture logs so we can assert the cohort_cap_exceeded line is ABSENT.
	var buf bytes.Buffer
	h := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// Empty harvesters — runPIPSeed enumerates the 60 cohorts but each
	// cohort's seed loop has zero targets (no apiserver calls).
	emptyApiRef := newContentPrewarmHarvester()
	emptyNav := newNavWidgetHarvester()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := runPIPSeed(ctx, emptyApiRef, emptyNav, endpoints.Endpoint{}, nil /* saRC */, "test-authn-ns")
	if err != nil {
		t.Fatalf("Ship 2 CAP-DELETION FAIL: runPIPSeed returned %v for %d cohorts (> old cap 50). "+
			"The cohort cap fail-closed branch must be DELETED — readiness must never "+
			"depend on cohort count", err, n)
	}

	if strings.Contains(buf.String(), "cohort_cap_exceeded") {
		t.Fatalf("Ship 2 CAP-DELETION FAIL: runPIPSeed emitted a `cohort_cap_exceeded` log line "+
			"for %d cohorts — the cap branch is still live:\n%s", n, buf.String())
	}
}

// TestPhase1_ReadinessFlips_BeforeBackgroundSeedCompletes proves the
// decoupling directly: while the per-cohort seed is still in flight
// (blocked), readiness is ALREADY 200. This is the cohort-count-
// INDEPENDENT boot-wall-clock invariant — the pod goes Ready on the
// substrate alone and the seed warms behind it.
//
// A regression that runs the seed synchronously before MarkPhase1Done
// would block phase1WarmupWith on the seed; the assertion that
// phase1WarmupWith has returned AND Phase1Done is true WHILE the seed is
// still blocked would FAIL.
func TestPhase1_ReadinessFlips_BeforeBackgroundSeedCompletes(t *testing.T) {
	rw := phase1TestWatcher(t)
	cache.ResetPhase1DoneForTest()
	cache.ResetAutoDiscoverGroupsForTest()
	t.Cleanup(cache.ResetPhase1DoneForTest)
	t.Cleanup(cache.ResetAutoDiscoverGroupsForTest)

	lister := func(ctx context.Context) ([]navigationRoot, error) {
		return []navigationRoot{{Root: routesLoaderCR("ns-a", "main"), GVR: gvrReached}}, nil
	}
	resolver := func(ctx context.Context, root navigationRoot) error {
		rw.EnsureResourceType(gvrReached)
		return nil
	}

	// The seed blocks until the test releases it. If readiness were gated
	// on the seed (the old synchronous path), phase1WarmupWith would block
	// here and the test would hit its own deadline.
	release := make(chan struct{})
	seedEntered := make(chan struct{})
	pipSeed := func(ctx context.Context) error {
		close(seedEntered)
		<-release
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// phase1WarmupWith MUST return promptly — it launches the seed in the
	// background and does not wait on it.
	if err := phase1WarmupWith(ctx, rw, lister, resolver, nil, nil, pipSeed, nil); err != nil {
		t.Fatalf("Ship 2: phase1WarmupWith returned error: %v", err)
	}

	// Readiness is already flipped while the background seed is still blocked.
	if !cache.IsPhase1Done() {
		t.Fatalf("Ship 2 FAIL: Phase1Done is false after phase1WarmupWith returned, "+
			"while the background seed is still blocked — readiness must not wait on the seed")
	}

	// Confirm the seed is genuinely in flight (entered, not yet released) —
	// proving phase1WarmupWith returned WITHOUT waiting for the seed.
	select {
	case <-seedEntered:
		// good — the background goroutine is running and blocked.
	case <-time.After(5 * time.Second):
		t.Fatalf("Ship 2: background seed goroutine never started")
	}

	// Release the seed so the background goroutine can exit cleanly before
	// test teardown.
	close(release)
}

// TestPhase1_ReadinessFlips_WhenSeedErrors proves that even a per-cohort
// seed that ERRORS (e.g. a pathological large topology that times out)
// does not withhold readiness. The seed's outcome is log-only.
//
// Pre-0.30.196 a non-nil pipSeed return caused phase1WarmupWith to return
// WITHOUT calling MarkPhase1Done — this assertion would FAIL.
func TestPhase1_ReadinessFlips_WhenSeedErrors(t *testing.T) {
	rw := phase1TestWatcher(t)
	cache.ResetPhase1DoneForTest()
	cache.ResetAutoDiscoverGroupsForTest()
	t.Cleanup(cache.ResetPhase1DoneForTest)
	t.Cleanup(cache.ResetAutoDiscoverGroupsForTest)

	lister := func(ctx context.Context) ([]navigationRoot, error) {
		return []navigationRoot{{Root: routesLoaderCR("ns-a", "main"), GVR: gvrReached}}, nil
	}
	resolver := func(ctx context.Context, root navigationRoot) error {
		rw.EnsureResourceType(gvrReached)
		return nil
	}

	seedDone := make(chan struct{})
	pipSeed := func(ctx context.Context) error {
		defer close(seedDone)
		return errors.New("simulated large-topology seed failure")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := phase1WarmupWith(ctx, rw, lister, resolver, nil, nil, pipSeed, nil); err != nil {
		t.Fatalf("Ship 2: phase1WarmupWith must NOT propagate a background-seed error as "+
			"a fatal return: %v", err)
	}
	if !cache.IsPhase1Done() {
		t.Fatalf("Ship 2 FAIL: Phase1Done is false after a seed error — the background "+
			"seed must never withhold readiness")
	}

	// Let the background goroutine observe its error + exit before teardown.
	select {
	case <-seedDone:
	case <-time.After(5 * time.Second):
		t.Fatalf("Ship 2: background seed goroutine never ran")
	}
}
