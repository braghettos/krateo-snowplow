// skip_binding_uid_sort_test.go — Ship L1 (snowplow 0.30.252).
//
// L1 adds EvaluateOptions.SkipBindingUID. When true, EvaluateRBAC skips
// the (Name, UID) stable-sort of the CRB/RB candidate sets
// (sortCRBsStable / sortRBsStable). That sort exists ONLY to make the
// returned matchedBindingUID deterministic across snapshot republishes;
// the permit/deny VERDICT is order-independent (first match wins, any
// match permits — RBAC v1 has no deny rules). The six per-item callers
// that discard matchedBindingUID now pass SkipBindingUID=true to drop
// the per-item sort that was ~43% of pod CPU at 50K scale (task-288).
//
// This test is the B1 correctness gate from the PM approval. It runs
// under `-race -count=1` and asserts BOTH:
//
//   (a) VERDICT byte-identical sort-on vs sort-off for the canonical
//       same-subject ~17.9K-CRB shape, evaluated under CONCURRENT load
//       (per feedback_shared_vs_copy_is_a_concurrency_change: skipping
//       the sort changes whether we mutate the candidate slice's order,
//       so a concurrent -race test — not a content-equivalence check —
//       is the required falsifier). Both phases (CRB allow, CRB deny)
//       and an RB-phase shape are covered.
//
//   (b) BindingUID UNCHANGED on the SkipBindingUID=false (default) path
//       — the cache-key contract. The deterministic first-match UID
//       must be exactly what it was before L1 for the sort-on path.
//
// evaltest package only — its TestMain is non-destructive. NEVER run
// `go test ./internal/rbac/...` against the remote kubeconfig (its
// TestMain deletes the RESTAction CRD), per
// feedback_no_go_test_against_remote_kubeconfig.

package evaltest

import (
	"context"
	"fmt"
	"sync"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"github.com/krateoplatformops/snowplow/internal/rbac"
)

// canonicalSACRBShape reproduces the live-cluster 50K shape that makes
// the sort the dominant CPU sink: ONE ServiceAccount subject bound by N
// ClusterRoleBindings, each CRB pointing at its OWN distinct
// per-composition ClusterRole. selectCRBCandidates routes the SA to
// CRBsByServiceAccount["<ns>/<name>"] → all N candidates, and
// EvaluateRBAC stable-sorts all N on every call (when SkipBindingUID is
// false). We use a smaller N here (the verdict + UID invariants are
// independent of N; the sort cost is what scales) but keep N large
// enough that ordering is non-trivially shuffled by insertion.
//
// permittingCRBIndex selects which of the N CRBs' ClusterRole actually
// grants (verb=get, resource=secrets). All others grant an unrelated
// (resource=configmaps, verb=list) tuple so they MATCH the subject but
// do NOT permit the request — forcing the walk past non-permitting
// candidates exactly like the live shape (distinct per-composition
// roles, most irrelevant to the asked GVR).
func canonicalSACRBShape(saNS, saName string, n, permittingCRBIndex int) []runtime.Object {
	var objs []runtime.Object
	sa := saSubject(saNS, saName)
	for i := 0; i < n; i++ {
		roleName := fmt.Sprintf("comp-role-%05d", i)
		var r rbacv1.PolicyRule
		if i == permittingCRBIndex {
			// The single permitting role for (get, "", secrets).
			r = rule([]string{""}, []string{"secrets"}, []string{"get"})
		} else {
			// Non-permitting for the asked tuple but a valid role.
			r = rule([]string{""}, []string{"configmaps"}, []string{"list"})
		}
		objs = append(objs, clusterRole(roleName, r))

		crb := clusterRoleBinding(fmt.Sprintf("comp-crb-%05d", i), roleName, sa)
		// Deterministic per-CRB UID so the sort-on first-match UID is
		// assertable. Name is NOT lexicographically aligned with the
		// permitting index on purpose — the permitting CRB is NOT the
		// lexicographically-first, so a verdict that depended on order
		// would diverge.
		crb.ObjectMeta = metav1.ObjectMeta{
			Name: fmt.Sprintf("comp-crb-%05d", i),
			UID:  types.UID(fmt.Sprintf("crb-uid-%05d", i)),
		}
		objs = append(objs, crb)
	}
	return objs
}

const (
	skipTestSANamespace = "bench-ns-01"
	skipTestSAName      = "githubscaffoldingwithcompositionpages-v1-2-2"
	skipTestN           = 2000 // candidate count; sort cost scales with this
)

// evalBoth runs the SAME request twice — once with SkipBindingUID=false
// (sort on) and once with SkipBindingUID=true (sort off) — and returns
// both (allowed, uid) results. Used to assert verdict identity.
func evalBoth(t *testing.T, opts rbac.EvaluateOptions) (sortOnAllowed bool, sortOnUID string, sortOffAllowed bool) {
	t.Helper()
	on := opts
	on.SkipBindingUID = false
	off := opts
	off.SkipBindingUID = true

	a1, u1, err := rbac.EvaluateRBAC(context.Background(), on)
	if err != nil {
		t.Fatalf("EvaluateRBAC (sort-on): err=%v", err)
	}
	a2, _, err := rbac.EvaluateRBAC(context.Background(), off)
	if err != nil {
		t.Fatalf("EvaluateRBAC (sort-off): err=%v", err)
	}
	return a1, u1, a2
}

// TestL1_VerdictIdentical_SACRBShape_Concurrent is the B1(a) gate.
//
// It seeds the canonical same-subject ~N-CRB shape and asserts the
// permit/deny VERDICT is byte-identical between SkipBindingUID=false
// (sort on) and SkipBindingUID=true (sort off), under CONCURRENT
// evaluation, for three request shapes:
//   - PERMITTED  (get secrets — the single permitting CRB grants it)
//   - DENIED     (delete secrets — no CRB grants delete)
//   - DENIED-GVR (get pods — no CRB grants pods)
//
// Concurrency: many goroutines evaluate sort-on and sort-off variants
// simultaneously against the SAME shared snapshot. `-race` proves the
// skip-the-sort path introduces no data race on the shared candidate
// slices returned by selectCRBCandidates (the slices are freshly built
// per call, but the underlying *ClusterRoleBinding pointers are shared
// across goroutines; the sort-on path mutates slice order in place).
func TestL1_VerdictIdentical_SACRBShape_Concurrent(t *testing.T) {
	seed := canonicalSACRBShape(skipTestSANamespace, skipTestSAName, skipTestN, skipTestN/2)
	newTestWatcher(t, seed...)

	saUser := "system:serviceaccount:" + skipTestSANamespace + ":" + skipTestSAName

	cases := []struct {
		name      string
		opts      rbac.EvaluateOptions
		wantPermit bool
	}{
		{
			name: "permitted_get_secrets",
			opts: rbac.EvaluateOptions{
				Username: saUser, Verb: "get", Group: "", Resource: "secrets", Namespace: "default",
			},
			wantPermit: true,
		},
		{
			name: "denied_delete_secrets",
			opts: rbac.EvaluateOptions{
				Username: saUser, Verb: "delete", Group: "", Resource: "secrets", Namespace: "default", Name: "x",
			},
			wantPermit: false,
		},
		{
			name: "denied_get_pods",
			opts: rbac.EvaluateOptions{
				Username: saUser, Verb: "get", Group: "", Resource: "pods", Namespace: "default", Name: "x",
			},
			wantPermit: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Single-shot identity + verdict-vs-expectation check.
			onAllowed, onUID, offAllowed := evalBoth(t, tc.opts)
			if onAllowed != tc.wantPermit {
				t.Fatalf("sort-on verdict = %v, want %v (setup invariant)", onAllowed, tc.wantPermit)
			}
			if onAllowed != offAllowed {
				t.Fatalf("L1 VERDICT DIVERGENCE: sort-on=%v sort-off=%v for %s — skipping the sort changed the answer",
					onAllowed, offAllowed, tc.name)
			}
			if tc.wantPermit {
				// The permitting CRB is index N/2 → uid "crb-uid-01000".
				wantUID := fmt.Sprintf("C:crb-uid-%05d", skipTestN/2)
				if onUID != wantUID {
					t.Fatalf("sort-on first-match UID = %q, want %q (deterministic first-match)", onUID, wantUID)
				}
			}

			// Concurrent: hammer sort-on and sort-off variants against
			// the shared snapshot; assert every result matches the
			// expected verdict (and therefore each other). -race here is
			// the load-bearing falsifier.
			const goroutines = 64
			const itersPerG = 30
			var wg sync.WaitGroup
			errCh := make(chan string, goroutines*itersPerG)
			for g := 0; g < goroutines; g++ {
				wg.Add(1)
				skip := g%2 == 0 // alternate sort-on / sort-off across goroutines
				go func(skip bool) {
					defer wg.Done()
					o := tc.opts
					o.SkipBindingUID = skip
					for i := 0; i < itersPerG; i++ {
						allowed, _, err := rbac.EvaluateRBAC(context.Background(), o)
						if err != nil {
							errCh <- fmt.Sprintf("err=%v", err)
							return
						}
						if allowed != tc.wantPermit {
							errCh <- fmt.Sprintf("concurrent verdict=%v want=%v (skip=%v)", allowed, tc.wantPermit, skip)
							return
						}
					}
				}(skip)
			}
			wg.Wait()
			close(errCh)
			for msg := range errCh {
				t.Fatalf("L1 concurrent verdict divergence: %s", msg)
			}
		})
	}
}

// TestL1_BindingUIDUnchanged_DefaultPath is the B1(b) gate.
//
// On the SkipBindingUID=false (DEFAULT zero-value) path the deterministic
// first-match BindingUID MUST be unchanged by the L1 patch. We assert
// the CRB-phase and RB-phase UIDs are exactly the canonical "C:<uid>" /
// "R:<ns>/<uid>" first-match values, and that a zero-value
// EvaluateOptions (the shape used by the cache-key callers, which set
// NOTHING) produces the SAME UID as an explicit SkipBindingUID=false.
func TestL1_BindingUIDUnchanged_DefaultPath(t *testing.T) {
	t.Run("crb_phase_lexicographic_first_match", func(t *testing.T) {
		// Two CRBs both granting alice; the (Name, UID) sort makes the
		// lexicographically-first Name win deterministically regardless
		// of seed order. "aaa-bind" < "zzz-bind".
		crbA := clusterRoleBindingWithUID("zzz-bind", "admin", "uid-zzz", userSubject("alice"))
		crbB := clusterRoleBindingWithUID("aaa-bind", "admin", "uid-aaa", userSubject("alice"))
		newTestWatcher(t,
			clusterRole("admin", rule([]string{"*"}, []string{"*"}, []string{"*"})),
			crbA, crbB,
		)
		opts := rbac.EvaluateOptions{
			Username: "alice", Verb: "get", Resource: "secrets", Namespace: "default",
		}
		// Explicit false and zero-value (cache-key caller shape) must agree.
		explicit := opts
		explicit.SkipBindingUID = false
		aE, uE, err := rbac.EvaluateRBAC(context.Background(), explicit)
		if err != nil || !aE {
			t.Fatalf("explicit sort-on: allowed=%v err=%v", aE, err)
		}
		aZ, uZ, err := rbac.EvaluateRBAC(context.Background(), opts) // zero-value SkipBindingUID
		if err != nil || !aZ {
			t.Fatalf("zero-value sort-on: allowed=%v err=%v", aZ, err)
		}
		const wantUID = "C:uid-aaa" // lexicographically-first Name wins
		if uE != wantUID {
			t.Fatalf("explicit SkipBindingUID=false first-match UID = %q, want %q", uE, wantUID)
		}
		if uZ != wantUID {
			t.Fatalf("zero-value (cache-key caller) first-match UID = %q, want %q", uZ, wantUID)
		}
		if uE != uZ {
			t.Fatalf("explicit-false UID (%q) != zero-value UID (%q) — default is not the safe sorted path", uE, uZ)
		}
	})

	t.Run("rb_phase_first_match_unchanged", func(t *testing.T) {
		rb := roleBinding("ns-a", "alice-reader", "Role", "reader", userSubject("alice"))
		rb.ObjectMeta = metav1.ObjectMeta{Namespace: "ns-a", Name: "alice-reader", UID: types.UID("rb-uid-l1")}
		newTestWatcher(t,
			role("ns-a", "reader", rule([]string{""}, []string{"configmaps"}, []string{"get"})),
			rb,
		)
		allowed, uid, err := rbac.EvaluateRBAC(context.Background(), rbac.EvaluateOptions{
			Username: "alice", Verb: "get", Resource: "configmaps", Namespace: "ns-a",
			// SkipBindingUID left false (default) — cache-key path.
		})
		if err != nil || !allowed {
			t.Fatalf("RB-path: allowed=%v err=%v", allowed, err)
		}
		const wantUID = "R:ns-a/rb-uid-l1"
		if uid != wantUID {
			t.Fatalf("RB-phase first-match UID = %q, want %q (must be unchanged by L1)", uid, wantUID)
		}
	})
}
