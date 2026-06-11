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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/http/response"
	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
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

// TestRunPIPSeed_DiscriminatesDenyVsOperational is the design §4.1 #5 core
// discrimination falsifier. It drives the REAL runPIPSeed call site (via
// the seedCohortFn + enumerateAggregatePrewarmTargetsFn seams) with two
// cohorts — one whose fake seed returns a 403 StatusError, one whose fake
// seed returns context.DeadlineExceeded — and asserts that the call site
// discriminates:
//
//   - the 403 cohort   → pipSeedRBACDenyTotal += 1, NO operational bump,
//     log event phase1.seed.cohort.expected_deny at Info, NO retry (the
//     fake is called exactly ONCE for it).
//   - the timeout cohort → pipSeedOperationalFailTotal += 1, log event
//     phase1.seed.cohort.operational_failure at Warn, exactly ONE bounded
//     retry (the fake is called exactly TWICE for it — once in the
//     errgroup, once in the retry drain — and phase1.seed.retry.* fires).
//
// On HEAD this FAILS: both shapes land in pipBindingSetSeedFailuresTotal,
// the rbac_deny / operational counters do not exist, and there is no retry
// drain — so the per-class delta assertions and the call-count assertion
// cannot hold.
func TestRunPIPSeed_DiscriminatesDenyVsOperational(t *testing.T) {
	// Capture logs for event-name assertions.
	var buf bytes.Buffer
	h := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	// Cohort labels chosen so cohortLogLabel renders them verbatim
	// (Username non-empty path).
	const denyLabel = "test-deny-cohort"
	const opLabel = "test-operational-cohort"

	// Inject a fixed two-cohort target list (bypasses the live RBAC
	// snapshot enumerator).
	prevEnum := enumerateAggregatePrewarmTargetsFn
	enumerateAggregatePrewarmTargetsFn = func() []seedTarget {
		return []seedTarget{
			{Username: denyLabel},
			{Username: opLabel},
		}
	}
	t.Cleanup(func() { enumerateAggregatePrewarmTargetsFn = prevEnum })

	// Inject a fake seedCohort that returns a controlled error class per
	// cohort and records its per-cohort call count (to prove the retry
	// fired exactly once for the operational cohort and zero times for the
	// deny cohort).
	var denyCalls, opCalls atomic.Int64
	deny403 := fmt.Errorf("fetch RESTAction ns/name: %w",
		&apierrors.StatusError{ErrStatus: metav1.Status{Code: 403, Reason: metav1.StatusReasonForbidden}})
	opTimeout := fmt.Errorf("cohort %q restactions seed: %w", opLabel, context.DeadlineExceeded)

	prevSeed := seedCohortFn
	seedCohortFn = func(_ context.Context, cohort seedTarget,
		_ []templatesv1.ObjectReference, _ []navWidgetEntry,
		_ endpoints.Endpoint, _ *rest.Config, _ string) error {
		switch cohort.Username {
		case denyLabel:
			denyCalls.Add(1)
			return deny403
		case opLabel:
			opCalls.Add(1)
			return opTimeout
		default:
			return nil
		}
	}
	t.Cleanup(func() { seedCohortFn = prevSeed })

	// Snapshot the global counters BEFORE (they are package-level atomics
	// shared across tests — assert DELTAS, not absolutes).
	denyBefore := pipSeedRBACDenyTotal.Load()
	opBefore := pipSeedOperationalFailTotal.Load()
	grandBefore := pipBindingSetSeedFailuresTotal.Load()

	// Empty harvesters — the fake seedCohort ignores them. Non-nil so
	// snapshot() is safe.
	hHarv := newContentPrewarmHarvester()
	nh := newNavWidgetHarvester()

	if err := runPIPSeed(context.Background(), hHarv, nh,
		endpoints.Endpoint{}, nil /* rc */, "test-authn-ns"); err != nil {
		t.Fatalf("runPIPSeed returned %v; want nil (per-cohort errors are non-fatal)", err)
	}

	// ── Counter discrimination ──────────────────────────────────────────
	if got := pipSeedRBACDenyTotal.Load() - denyBefore; got != 1 {
		t.Errorf("rbac_deny counter delta = %d; want 1 (the 403 cohort must bump rbac_deny exactly once)", got)
	}
	if got := pipSeedOperationalFailTotal.Load() - opBefore; got != 1 {
		t.Errorf("operational counter delta = %d; want 1 (the timeout cohort must bump operational exactly once)", got)
	}
	// Back-compat grand total = sum of the two new ones (both cohorts
	// failed once in the errgroup → +2; the retry's second op failure does
	// NOT bump the grand total again — only the in-loop classification
	// does).
	if got := pipBindingSetSeedFailuresTotal.Load() - grandBefore; got != 2 {
		t.Errorf("back-compat grand-total delta = %d; want 2 (= rbac_deny + operational from the in-loop classification)", got)
	}

	// ── Retry discrimination (re-enqueue fired exactly once) ────────────
	if got := denyCalls.Load(); got != 1 {
		t.Errorf("deny cohort seedCohort call count = %d; want 1 (an RBAC deny must NOT be retried)", got)
	}
	if got := opCalls.Load(); got != 2 {
		t.Errorf("operational cohort seedCohort call count = %d; want 2 "+
			"(once in the errgroup + exactly one bounded retry drain)", got)
	}

	// ── Log-event discrimination ────────────────────────────────────────
	logText := buf.String()
	denyRec := findLogRecord(t, logText, "phase1.seed.cohort.expected_deny")
	if denyRec == nil {
		t.Fatalf("missing Info event phase1.seed.cohort.expected_deny for the 403 cohort; logs:\n%s", logText)
	}
	if lvl, _ := denyRec["level"].(string); lvl != "INFO" {
		t.Errorf("expected_deny level = %q; want INFO (a genuine deny is expected, not a Warn)", lvl)
	}
	if c, _ := denyRec["cohort"].(string); c != denyLabel {
		t.Errorf("expected_deny cohort = %q; want %q", c, denyLabel)
	}

	opRec := findLogRecord(t, logText, "phase1.seed.cohort.operational_failure")
	if opRec == nil {
		t.Fatalf("missing Warn event phase1.seed.cohort.operational_failure for the timeout cohort; logs:\n%s", logText)
	}
	if lvl, _ := opRec["level"].(string); lvl != "WARN" {
		t.Errorf("operational_failure level = %q; want WARN", lvl)
	}
	if c, _ := opRec["cohort"].(string); c != opLabel {
		t.Errorf("operational_failure cohort = %q; want %q", c, opLabel)
	}

	// The retry drain must have run (proves the legacy bounded re-enqueue
	// lane fired for the operational cohort).
	if findLogRecord(t, logText, "phase1.seed.retry.started") == nil {
		t.Errorf("missing phase1.seed.retry.started — the operational cohort was not queued for re-attempt; logs:\n%s", logText)
	}
	// The retry re-failed (the fake always returns the timeout) so the
	// bound holds: dropped after one re-attempt.
	if findLogRecord(t, logText, "phase1.seed.cohort.retry_exhausted") == nil {
		t.Errorf("missing phase1.seed.cohort.retry_exhausted — the ≤1-retry bound was not exercised; logs:\n%s", logText)
	}
}

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
