// phase1_seed_classify_test.go — #158 (P9-B) falsifiers (design §4.1).
//
// Two layers:
//   1. classifySeedErr + statusErrFromResponse table tests — pin the
//      taxonomy (403/401→deny; ctx.DeadlineExceeded/5xx/unknown→operational).
//   2. The DISCRIMINATION test — drive the real runPIPSeed call site with a
//      fake seedCohort (injected via the seedCohortFn seam) returning (a) a
//      403 StatusError and (b) context.DeadlineExceeded, and assert the
//      call site BRANCHES: deny → rbac_deny counter++ + Info log + NO
//      retry; operational → operational counter++ + Warn log + exactly one
//      bounded retry. Modelled on TestSeedCohort_CtxCancelEmitsAbortLog
//      (pure, no apiserver, no informer, <1ms) — AVOIDS the destructive
//      go-test-./internal/rbac/... TestMain entirely (lives in dispatchers).
//
// RED-ON-REVERT (design §4.1): on HEAD (before this ship) both error
// shapes land in the single pipBindingSetSeedFailuresTotal counter and
// neither rbac_deny nor operational exists, and there is no retry drain —
// so TestRunPIPSeed_DiscriminatesDenyVsOperational FAILS when the
// production hunks are reverted (the counters don't move / the symbols
// don't exist). See the report's RED-on-revert proof.

package dispatchers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/krateoplatformops/plumbing/http/response"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestClassifySeedErr is the taxonomy table test (design §4.1 #1-4).
func TestClassifySeedErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want seedFailClass
	}{
		{
			name: "nil -> none",
			err:  nil,
			want: seedFailNone,
		},
		{
			name: "403 StatusError -> rbac_deny",
			err:  &apierrors.StatusError{ErrStatus: metav1.Status{Code: 403, Reason: metav1.StatusReasonForbidden}},
			want: seedFailRBACDeny,
		},
		{
			name: "401 StatusError -> rbac_deny",
			err:  &apierrors.StatusError{ErrStatus: metav1.Status{Code: 401, Reason: metav1.StatusReasonUnauthorized}},
			want: seedFailRBACDeny,
		},
		{
			name: "403 wrapped with %w (restaction path) -> rbac_deny",
			err:  fmt.Errorf("fetch RESTAction ns/name: %w", &apierrors.StatusError{ErrStatus: metav1.Status{Code: 403, Reason: metav1.StatusReasonForbidden}}),
			want: seedFailRBACDeny,
		},
		{
			name: "statusErrFromResponse(403) -> rbac_deny",
			err:  statusErrFromResponse(&response.Status{Code: 403, Reason: response.StatusReasonForbidden, Message: "forbidden"}),
			want: seedFailRBACDeny,
		},
		{
			name: "statusErrFromResponse(403) code-only (empty reason) -> rbac_deny",
			err:  statusErrFromResponse(&response.Status{Code: 403, Message: "forbidden"}),
			want: seedFailRBACDeny,
		},
		{
			name: "ctx.DeadlineExceeded wrapped (cohort timeout) -> operational",
			err:  fmt.Errorf("cohort %q restactions seed: %w", "x", context.DeadlineExceeded),
			want: seedFailOperational,
		},
		{
			name: "ctx.Canceled wrapped (parent cancel) -> operational",
			err:  fmt.Errorf("cohort %q widgets seed: %w", "x", context.Canceled),
			want: seedFailOperational,
		},
		{
			name: "503 StatusError -> operational",
			err:  &apierrors.StatusError{ErrStatus: metav1.Status{Code: 503, Reason: metav1.StatusReasonServiceUnavailable}},
			want: seedFailOperational,
		},
		{
			name: "statusErrFromResponse(500) -> operational",
			err:  statusErrFromResponse(&response.Status{Code: 500, Reason: response.StatusReasonInternalError, Message: "boom"}),
			want: seedFailOperational,
		},
		{
			name: "500 code-only (empty reason) -> operational",
			err:  statusErrFromResponse(&response.Status{Code: 500, Message: "boom"}),
			want: seedFailOperational,
		},
		{
			name: "opaque transport error -> operational (fail-loud default)",
			err:  errors.New("transport: connection refused"),
			want: seedFailOperational,
		},
		{
			name: "widget 400 validation deny -> operational (fail-loud, not a deny)",
			// seedOneWidget %w-wraps widgets.Resolve's *StatusError{Code:400,
			// Reason:BadRequest}; a 400 is NOT IsForbidden/IsUnauthorized, so
			// it falls through to the fail-loud default. (Widget RBAC denies
			// are contained per-target upstream and never reach the rollup —
			// design §1.1 — so this is the correct, safe classification.)
			err:  fmt.Errorf("resolve widget ns/name: %w", &apierrors.StatusError{ErrStatus: metav1.Status{Code: 400, Reason: metav1.StatusReasonBadRequest}}),
			want: seedFailOperational,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifySeedErr(tc.err); got != tc.want {
				t.Fatalf("classifySeedErr(%v) = %v; want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestStatusErrFromResponse_NilSafe pins the nil-safe contract.
func TestStatusErrFromResponse_NilSafe(t *testing.T) {
	if err := statusErrFromResponse(nil); err != nil {
		t.Fatalf("statusErrFromResponse(nil) = %v; want nil", err)
	}
}

// TestRunPIPSeed_DiscriminatesDenyVsOperational — DELETED 2026-07-03
// (docs/prewarm-engine-implicit-on-cache-2026-07-03.md §4.3a).
//
// COVERAGE MIGRATION (NOT silent-drop): this test drove the LEGACY runPIPSeed
// orchestration's #158 deny-vs-operational + bounded-retry drain via the
// seedCohortFn + enumerateAggregatePrewarmTargetsFn seams. Those symbols are
// DELETED with the runPIPSeed errgroup path (engine implicit-on-cache;
// runPIPSeed unreachable). The #158 discrimination is now carried EQUIVALENTLY
// on the ENGINE path by prewarm_engine_seed_latch_test.go, which drives the REAL
// classifyEngineSeedErr + reEnqueued latch (prewarm_engine_boot.go) end-to-end:
//   - TestSeedScopeYielding_RBACDeny_NoEnqueue_BumpsDenyCounter — 403 deny arm:
//     rbac_deny counter++, Info prewarm.engine.seed.expected_deny, ZERO re-enqueue.
//   - TestSeedScopeYielding_OneOperationalFailure_EnqueuesExactlyOnce +
//     _NOperationalFailures_LatchEnqueuesExactlyOnce — operational arm:
//     operational counter++, Warn prewarm.engine.seed.operational_failure, a
//     coalesced (dedup on key()=="boot") boot re-enqueue = the bounded-retry lane.
//   - TestSeedScopeYielding_CounterSumInvariant — grand-total = rbac_deny + operational.
// The taxonomy (classifySeedErr / statusErrFromResponse) is SHARED (engine calls
// it) and stays guarded by TestClassifySeedErr + TestStatusErrFromResponse_NilSafe
// above. #158 coverage is preserved on the surviving path, not dropped.

// findLogRecord returns the first decoded JSON log record whose "msg"
// equals want, or nil.
func findLogRecord(t *testing.T, logText, want string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(logText), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if msg, _ := rec["msg"].(string); msg == want {
			return rec
		}
	}
	return nil
}
