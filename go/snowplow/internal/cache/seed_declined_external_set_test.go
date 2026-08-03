// seed_declined_external_set_test.go — coverage-audit L4.
//
// SeedDeclinedExternalSet (seed_declined_external_set.go) is the #132 F4b Lever A
// boot-scope "resolved-but-declined-external" marker. L4 pins its contract:
//
//   - Mark is IDEMPOTENT: a second Mark of the same key is a no-op — Marks()
//     counts DISTINCT keys, never double-counts (LoadOrStore guard).
//   - nil-safe: (nil).Mark / (nil).Marked / (nil).Marks never panic; Marked is
//     always false and Marks is always 0 on a nil receiver (the "no set
//     installed" posture on the /call path).
//   - empty-key is ignored (Mark("") is a no-op, Marked("") is false).
//   - WithSeedDeclinedExternalSet installs the set ONLY when the cache subsystem
//     is enabled — under Disabled() it returns ctx UNCHANGED (no set carried).
//     Its inverse (SeedDeclinedExternalSetFromContext) is nil off the install
//     path — the property that makes the marker provably untouched on /call.
//
// RED PROOFS:
//   - double-count: a Mark that increments unconditionally (no LoadOrStore
//     guard) over-counts a repeated key — shadow markDoubleCounts proves the
//     idempotency arm discriminates.
//   - install-under-Disabled: a With* that installs regardless of Disabled()
//     leaks a set onto a cache-off context — shadow withIgnoringDisabled proves
//     the gate arm discriminates.
package cache

import (
	"context"
	"testing"
)

// TestSeedDeclinedExternalSet_MarkIdempotent is L4 part 1: Marks() counts
// distinct keys; a repeated Mark does not double-count.
func TestSeedDeclinedExternalSet_MarkIdempotent(t *testing.T) {
	s := NewSeedDeclinedExternalSet()

	s.Mark("k1")
	s.Mark("k1") // repeat — must NOT double-count
	s.Mark("k2")

	if !s.Marked("k1") || !s.Marked("k2") {
		t.Fatalf("Mark must make keys Marked: k1=%v k2=%v", s.Marked("k1"), s.Marked("k2"))
	}
	if s.Marked("k3") {
		t.Fatalf("an unmarked key must not be Marked")
	}
	if got := s.Marks(); got != 2 {
		t.Fatalf("Marks() must count DISTINCT keys, got %d want 2 (idempotent Mark)", got)
	}

	// Third distinct key bumps to 3; another repeat of k1 stays at 3.
	s.Mark("k3")
	s.Mark("k1")
	if got := s.Marks(); got != 3 {
		t.Fatalf("Marks() after a new distinct key + a repeat = %d, want 3", got)
	}
}

// TestSeedDeclinedExternalSet_NilAndEmptyKeySafe is L4 part 2: nil-receiver and
// empty-key are safe no-ops.
func TestSeedDeclinedExternalSet_NilAndEmptyKeySafe(t *testing.T) {
	var nilSet *SeedDeclinedExternalSet
	// nil receiver: no panic, Marked false, Marks 0.
	nilSet.Mark("anything") // must not panic
	if nilSet.Marked("anything") {
		t.Fatalf("(nil).Marked must be false")
	}
	if nilSet.Marks() != 0 {
		t.Fatalf("(nil).Marks must be 0")
	}

	// empty key on a real set: ignored.
	s := NewSeedDeclinedExternalSet()
	s.Mark("") // no-op
	if s.Marked("") {
		t.Fatalf("Marked(\"\") must be false — empty key is ignored")
	}
	if s.Marks() != 0 {
		t.Fatalf("Mark(\"\") must not count, got %d", s.Marks())
	}
}

// TestSeedDeclinedExternalSet_InstallOnlyWhenEnabled is L4 part 3: the context
// installer honours the master gate — a set is carried when the cache subsystem
// is enabled and NOT carried under Disabled().
func TestSeedDeclinedExternalSet_InstallOnlyWhenEnabled(t *testing.T) {
	set := NewSeedDeclinedExternalSet()

	// Cache subsystem ENABLED → the set is installed and retrievable.
	t.Setenv("CACHE_ENABLED", "true")
	ctxOn := WithSeedDeclinedExternalSet(context.Background(), set)
	if got := SeedDeclinedExternalSetFromContext(ctxOn); got != set {
		t.Fatalf("with CACHE_ENABLED=true the set must be installed and retrievable; got %v", got)
	}

	// Cache subsystem DISABLED → install is a no-op; ctx unchanged; no set.
	t.Setenv("CACHE_ENABLED", "false")
	base := context.Background()
	ctxOff := WithSeedDeclinedExternalSet(base, set)
	if got := SeedDeclinedExternalSetFromContext(ctxOff); got != nil {
		t.Fatalf("under Disabled() no set must be carried; got %v", got)
	}

	// A background context with nothing installed also yields nil (the /call
	// posture — the marker is provably untouched off the boot-seed path).
	if got := SeedDeclinedExternalSetFromContext(context.Background()); got != nil {
		t.Fatalf("a plain context must carry no set; got %v", got)
	}
}

// --- RED-arm proofs ---------------------------------------------------------

// markDoubleCounts is the L4 WRONG Mark: it increments unconditionally (no
// LoadOrStore idempotency guard), over-counting a repeated key.
type doubleCountingSet struct {
	seen  map[string]struct{}
	marks uint64
}

func (d *doubleCountingSet) Mark(key string) {
	if key == "" {
		return
	}
	if d.seen == nil {
		d.seen = map[string]struct{}{}
	}
	d.seen[key] = struct{}{}
	d.marks++ // defect: bumps even on a repeat
}
func (d *doubleCountingSet) Marks() uint64 { return d.marks }

// TestSeedDeclinedExternalSet_DoubleCount_RedArm proves the L4 idempotency arm
// discriminates: the double-counting wrong impl reports 3 for two distinct keys
// (one repeated), where the correct impl reports 2.
func TestSeedDeclinedExternalSet_DoubleCount_RedArm(t *testing.T) {
	d := &doubleCountingSet{}
	d.Mark("k1")
	d.Mark("k1") // repeat
	d.Mark("k2")
	if got := d.Marks(); got != 3 {
		t.Fatalf("RED-arm sanity: the double-counting wrong impl SHOULD over-count to 3, got %d", got)
	}
	// The correct impl reports 2 for the same sequence (asserted in the GREEN
	// idempotency test); 3 != 2 confirms the arm discriminates.
}

// withIgnoringDisabled is the L4 WRONG installer: it installs the set regardless
// of Disabled(), leaking a marker onto a cache-off context.
func withIgnoringDisabled(ctx context.Context, set *SeedDeclinedExternalSet) context.Context {
	if ctx == nil || set == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKeySeedDeclinedExternalSet, set) // defect: no Disabled() guard
}

// TestSeedDeclinedExternalSet_InstallUnderDisabled_RedArm proves the L4 gate arm
// discriminates: under Disabled() the wrong installer STILL carries a set, where
// the correct installer returns ctx unchanged (nil retrieval).
func TestSeedDeclinedExternalSet_InstallUnderDisabled_RedArm(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "false") // Disabled() == true
	set := NewSeedDeclinedExternalSet()

	ctxWrong := withIgnoringDisabled(context.Background(), set)
	if got := SeedDeclinedExternalSetFromContext(ctxWrong); got != set {
		t.Fatalf("RED-arm sanity: the install-ignoring-Disabled wrong impl SHOULD carry a set " +
			"even under Disabled(); the L4 gate arm is not discriminating otherwise")
	}
	// The correct installer returns ctx unchanged under Disabled() (asserted in
	// the GREEN gate test) → the two behaviours differ, so the arm discriminates.
}
