// Q-COHORT-PREWARM (v0.25.312) — unit tests for RBACWatcher's binding-
// ADD cohort fan-out.
//
// Test strategy: drive scheduleCohortPrewarmFromBinding directly with
// fabricated *rbacv1.RoleBinding objects and a mock RBACPrewarmer.
// This bypasses the K8s informer wiring while exercising:
//   - Subject extraction (User vs Group).
//   - Group expansion against the active-users set in the cache.
//   - Debounce coalescing (multiple ADDs collapse into one fan-out).
//   - Storm safety (1004 ADDs in a tight burst → one fan-out).
//   - Race cleanliness (concurrent calls to the scheduler).
package cache

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// mockPrewarmer captures EnqueueForCohort calls. Thread-safe.
type mockPrewarmer struct {
	mu       sync.Mutex
	calls    int
	users    [][]string // recorded usernames per call
	delay    time.Duration
	onCall   func()
	accepted int
	dropped  int
}

func (m *mockPrewarmer) EnqueueForCohort(ctx context.Context, usernames []string) (accepted, dropped int) {
	m.mu.Lock()
	m.calls++
	cp := append([]string(nil), usernames...)
	m.users = append(m.users, cp)
	d := m.delay
	cb := m.onCall
	a := m.accepted
	dr := m.dropped
	m.mu.Unlock()
	if d > 0 {
		time.Sleep(d)
	}
	if cb != nil {
		cb()
	}
	if a == 0 && dr == 0 {
		// default: accept all
		return len(usernames), 0
	}
	return a, dr
}

func (m *mockPrewarmer) snapshot() (calls int, lastUsers []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	calls = m.calls
	if len(m.users) > 0 {
		last := m.users[len(m.users)-1]
		lastUsers = append([]string(nil), last...)
	}
	return
}

// newTestRBACWatcher constructs a minimally-initialised RBACWatcher
// suitable for direct scheduler tests. The informer wiring is skipped;
// we drive scheduleCohortPrewarmFromBinding directly.
func newTestRBACWatcher(c Cache) *RBACWatcher {
	return &RBACWatcher{cache: c}
}

// fixtureRoleBinding builds a *rbacv1.RoleBinding with the given subjects.
func fixtureRoleBinding(name string, subjects ...rbacv1.Subject) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Subjects:   subjects,
	}
}

func userSubject(name string) rbacv1.Subject {
	return rbacv1.Subject{Kind: rbacv1.UserKind, Name: name}
}

func groupSubject(name string) rbacv1.Subject {
	return rbacv1.Subject{Kind: rbacv1.GroupKind, Name: name}
}

// waitForCalls polls the mock for `want` calls or fails after timeout.
func waitForCalls(t *testing.T, m *mockPrewarmer, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c, _ := m.snapshot()
		if c >= want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	c, _ := m.snapshot()
	t.Fatalf("waitForCalls: got %d, want %d after %v", c, want, timeout)
}

// TestRBACAdd_FiresPrewarmer_UserSubject — fundamental contract: a
// binding with one User subject fires the prewarmer once for that user
// after the debounce window elapses.
func TestRBACAdd_FiresPrewarmer_UserSubject(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rw := newTestRBACWatcher(NewMem(time.Hour))
	mp := &mockPrewarmer{}
	rw.SetPrewarmer(mp)

	rb := fixtureRoleBinding("rb-1", userSubject("alice"))
	rw.scheduleCohortPrewarmFromBinding(ctx, rb)

	waitForCalls(t, mp, 1, 3*rbacDebounceWindow)
	calls, users := mp.snapshot()
	if calls != 1 {
		t.Fatalf("calls: got %d, want 1", calls)
	}
	if len(users) != 1 || users[0] != "alice" {
		t.Errorf("users: got %v, want [alice]", users)
	}
}

// TestRBACAdd_DebounceCoalesces — many ADDs within rbacDebounceWindow
// produce ONE fan-out. This is the load-bearing storm-coalescing
// invariant: 50 RBs in <1s → 1 prewarmer call.
func TestRBACAdd_DebounceCoalesces(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rw := newTestRBACWatcher(NewMem(time.Hour))
	mp := &mockPrewarmer{}
	rw.SetPrewarmer(mp)

	const N = 50
	for i := 0; i < N; i++ {
		rb := fixtureRoleBinding("rb-burst", userSubject("alice"))
		rw.scheduleCohortPrewarmFromBinding(ctx, rb)
	}

	// All ADDs should coalesce; wait for ONE call.
	waitForCalls(t, mp, 1, 3*rbacDebounceWindow)

	// Sleep past the debounce window to ensure no second fire happens.
	time.Sleep(rbacDebounceWindow + 500*time.Millisecond)
	calls, users := mp.snapshot()
	if calls != 1 {
		t.Errorf("calls: got %d, want 1 (debounce should coalesce)", calls)
	}
	if len(users) != 1 || users[0] != "alice" {
		t.Errorf("users: got %v, want [alice]", users)
	}
}

// TestRBACAdd_DebounceCoalesces_Storm1004 is the PM-mandated G-QUEUE-
// DRAIN gate at the unit level: 1004 binding ADDs in a tight burst
// (matching the bench cluster's active-user count, simulating the
// initial-LIST storm + helm rollout) must coalesce into ONE fan-out
// covering the union of subjects. No saturation, no panic.
//
// Different subjects ensure the union path is exercised — if the
// scheduler dropped subjects on burst, we'd see a smaller union.
func TestRBACAdd_DebounceCoalesces_Storm1004(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rw := newTestRBACWatcher(NewMem(time.Hour))
	mp := &mockPrewarmer{}
	rw.SetPrewarmer(mp)

	const N = 1004
	expected := make(map[string]struct{}, N)
	start := time.Now()
	for i := 0; i < N; i++ {
		u := usernameForBench(i)
		expected[u] = struct{}{}
		rb := fixtureRoleBinding("rb-storm", userSubject(u))
		rw.scheduleCohortPrewarmFromBinding(ctx, rb)
	}
	scheduleElapsed := time.Since(start)
	if scheduleElapsed > 5*time.Second {
		t.Errorf("schedule loop took too long: %v (must be non-blocking)", scheduleElapsed)
	}

	// Exactly one fan-out must fire after the debounce window.
	waitForCalls(t, mp, 1, 3*rbacDebounceWindow)

	// Verify no spurious second fan-out within another window.
	time.Sleep(rbacDebounceWindow + 500*time.Millisecond)
	calls, users := mp.snapshot()
	if calls != 1 {
		t.Errorf("calls: got %d, want 1 (storm must coalesce)", calls)
	}
	if len(users) != N {
		t.Errorf("users: got %d unique, want %d", len(users), N)
	}
	for _, u := range users {
		if _, ok := expected[u]; !ok {
			t.Errorf("unexpected user in fan-out: %q", u)
			break
		}
	}
}

// TestRBACAdd_GroupSubjectExpansion — a binding with a Group subject
// fans out to ALL active users in the cache (Option A). Direct User
// subjects in the same burst are deduped against the active-users set.
func TestRBACAdd_GroupSubjectExpansion(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := NewMem(time.Hour)
	// Seed 3 active users.
	for _, u := range []string{"alice", "bob", "carol"} {
		if err := c.SAddUser(ctx, u); err != nil {
			t.Fatalf("SAddUser(%q): %v", u, err)
		}
	}

	rw := newTestRBACWatcher(c)
	mp := &mockPrewarmer{}
	rw.SetPrewarmer(mp)

	rb := fixtureRoleBinding("rb-devs", groupSubject("devs"))
	rw.scheduleCohortPrewarmFromBinding(ctx, rb)

	waitForCalls(t, mp, 1, 3*rbacDebounceWindow)
	calls, users := mp.snapshot()
	if calls != 1 {
		t.Fatalf("calls: got %d, want 1", calls)
	}

	// Expansion must produce all 3 active users (Option A).
	got := make(map[string]bool, len(users))
	for _, u := range users {
		got[u] = true
	}
	for _, want := range []string{"alice", "bob", "carol"} {
		if !got[want] {
			t.Errorf("expected %q in expanded fan-out, got %v", want, users)
		}
	}
}

// TestRBACAdd_NoPrewarmerIsSafe — when SetPrewarmer was never called,
// the scheduler must not panic and must not schedule a no-op timer
// indefinitely.
func TestRBACAdd_NoPrewarmerIsSafe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rw := newTestRBACWatcher(NewMem(time.Hour))
	// No SetPrewarmer call.
	rb := fixtureRoleBinding("rb-1", userSubject("alice"))
	rw.scheduleCohortPrewarmFromBinding(ctx, rb)

	// Wait for the debounce timer to fire and discover the nil prewarmer.
	time.Sleep(rbacDebounceWindow + 500*time.Millisecond)
	// Subsequent ADDs should still be safe.
	rw.scheduleCohortPrewarmFromBinding(ctx, rb)
	time.Sleep(rbacDebounceWindow + 500*time.Millisecond)
	// If we got here without panic, the contract is satisfied.
}

// TestRBACAdd_RaceClean — concurrent ADDs from many goroutines must not
// corrupt the accumulator. Run under -race to flag map mutation
// races.
func TestRBACAdd_RaceClean(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rw := newTestRBACWatcher(NewMem(time.Hour))
	mp := &mockPrewarmer{}
	rw.SetPrewarmer(mp)

	const G = 32
	const N = 100
	var wg sync.WaitGroup
	for g := 0; g < G; g++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for i := 0; i < N; i++ {
				u := usernameForBench(seed*N + i)
				rb := fixtureRoleBinding("rb-race", userSubject(u))
				rw.scheduleCohortPrewarmFromBinding(ctx, rb)
			}
		}(g)
	}
	wg.Wait()

	// Wait for the coalesced fan-out.
	waitForCalls(t, mp, 1, 3*rbacDebounceWindow)
	calls, users := mp.snapshot()
	if calls != 1 {
		t.Errorf("calls: got %d, want 1 (storm + race)", calls)
	}
	// Sanity: union should cover the unique usernames generated above.
	want := G * N
	if len(users) != want {
		t.Errorf("users: got %d unique, want %d", len(users), want)
	}
}

// TestRBACAdd_NonBindingObjectIgnored — extractSubjects refuses non-
// binding types; scheduler must skip without panic and without
// scheduling a fan-out.
func TestRBACAdd_NonBindingObjectIgnored(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rw := newTestRBACWatcher(NewMem(time.Hour))
	mp := &mockPrewarmer{}
	rw.SetPrewarmer(mp)

	rw.scheduleCohortPrewarmFromBinding(ctx, "not a binding")
	rw.scheduleCohortPrewarmFromBinding(ctx, struct{}{})

	time.Sleep(rbacDebounceWindow + 500*time.Millisecond)
	calls, _ := mp.snapshot()
	if calls != 0 {
		t.Errorf("calls: got %d, want 0 (non-binding objects must be ignored)", calls)
	}
}

// TestRBACAdd_DropsArePropagated — when the prewarmer reports drops,
// the scheduler logs them but does not retry within the same window.
// This documents the contract: drops are absorbed by the next ADD or
// by the request-driven backstop (deferred per PM scope).
func TestRBACAdd_DropsArePropagated(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rw := newTestRBACWatcher(NewMem(time.Hour))
	var dropCalls atomic.Int64
	mp := &mockPrewarmer{accepted: 0, dropped: 1, onCall: func() { dropCalls.Add(1) }}
	rw.SetPrewarmer(mp)

	rb := fixtureRoleBinding("rb-drop", userSubject("alice"))
	rw.scheduleCohortPrewarmFromBinding(ctx, rb)

	waitForCalls(t, mp, 1, 3*rbacDebounceWindow)
	if dropCalls.Load() != 1 {
		t.Errorf("dropCalls: got %d, want 1", dropCalls.Load())
	}
}

// usernameForBench yields a deterministic stable username for a given
// index. Avoids fmt.Sprintf in hot loops.
func usernameForBench(i int) string {
	const alpha = "abcdefghijklmnopqrstuvwxyz"
	return "u-" +
		string(alpha[i%len(alpha)]) +
		string(alpha[(i/len(alpha))%len(alpha)]) +
		string(alpha[(i/(len(alpha)*len(alpha)))%len(alpha)]) +
		string(alpha[(i/(len(alpha)*len(alpha)*len(alpha)))%len(alpha)])
}
