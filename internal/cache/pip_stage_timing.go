// pip_stage_timing.go — Ship 0.30.192 PURE-ADDITIVE INSTRUMENTATION.
//
// Per-stage timing sink for PIP seed cohort restactions. The 0.30.179
// cluster-list-deny / per-NS iterator fallback at iter_serial=1 is the
// architect's TRACED hypothesis for the 46s/restaction wall-clock on
// the four 0.30.189-sentinel cohorts (system:cohort:group-only:v1
// etc.), but the cost-attribution is unverified: their "~5K namespaces
// × ~10ms" projection was invalidated by the cluster reality
// (62 namespaces actually present). This file lets the resolver record
// per-stage timing + cluster-list outcome + iterator-call counts during
// a seed pass so we can attribute the 46s to a real code path before
// designing the 0.30.193 fix.
//
// CONCURRENCY: one PIPStageTimingSink is created per seedOneRestaction
// invocation and attached to that goroutine's context — there is NO
// cross-goroutine sharing within a sink. The inner-call iterator loop
// in resolve.go DOES run goroutines that share the parent ctx (and
// therefore the sink), but only the parent goroutine reads/writes
// stage-level fields between/around the errgroup.Wait — never inside
// the worker. The sync.Mutex is defensive (so a future code path that
// records from a worker is race-safe), not a correctness requirement
// for today's call sites.
//
// PURE-ADDITIVE: nil-safe everywhere — a ctx without WithPIPStageTimingSink
// returns a nil *PIPStageTimingSink from PIPStageTimingSinkFrom, and
// every sink-method short-circuits on a nil receiver. Production paths
// (the live /call request, the SA content-warm) carry no sink and pay
// zero overhead beyond the nil-receiver branch.

package cache

import (
	"context"
	"sync"
)

// PIPStageTiming captures per-stage cost-attribution data for ONE
// spec.api[] stage inside a seedOneRestaction resolve. The architect's
// 0.30.192 instrumentation spec (4 checkpoints); the minimum-acceptable
// scope shipped is fields 1-5 (StageID, elapsed_ms_total/_inner,
// ClusterListUsed, ClusterListDenyGate, IteratorCalls). Per-call
// content-hit and apistage-memo fields are RESERVED — populated by
// follow-up instrumentation when those checkpoints are wired.
type PIPStageTiming struct {
	// StageID is the spec.api[].name of the stage (e.g.
	// "compositions-by-ns"). Used in log diff between cohorts.
	StageID string `json:"stage_id"`

	// ElapsedMs is the wall-clock from stage-start to stage-end (the
	// outer for-loop iteration in resolve.go's stage loop). Includes
	// endpoint resolution + cluster-list gate check + iterator dispatch
	// + apistage-content serve.
	ElapsedMs int64 `json:"elapsed_ms"`

	// ClusterListAttempted is true when attemptClusterListCollapse
	// was reached (the stage had ClusterListWhenAllowed=true and the
	// resolver decided to evaluate the gate). When false, the stage
	// did NOT opt in (apiCall.ClusterListWhenAllowed nil/false) and
	// went straight to the iterator path.
	ClusterListAttempted bool `json:"cl_attempt"`

	// ClusterListUsed reports the outcome of the gate: true means the
	// helper returned useClusterList=true and tmp was collapsed to a
	// single cluster-scope call. False means a deny — see
	// ClusterListDenyGate.
	ClusterListUsed bool `json:"cl_used"`

	// ClusterListDenyGate is the gate number (1-5) that triggered the
	// false return from attemptClusterListCollapse — see cluster_list.go
	// :86-104. 0 means no deny (gate passed, ClusterListUsed=true) OR
	// the helper was not attempted. Values:
	//   1 — opt-in deny (ClusterListWhenAllowed false/nil)
	//   2 — cache-off / snapshot pre-readiness
	//   3 — no iterator on the stage
	//   4 — GVR derivation failed (iterator path not namespace-scoped)
	//   5 — RBAC permission deny
	//   6 — dispatch returned non-servable (rare — pre-sync informer)
	//   7 — shape check failed (cluster-list multi-element check)
	ClusterListDenyGate int `json:"cl_deny_gate"`

	// IteratorCalls is the number of inner-call elements the bounded
	// errgroup dispatched for this stage. For a cluster-list-collapsed
	// stage this is 1. For a per-NS iterator path with no collapse, this
	// is the number of namespaces matched by the iterator query.
	IteratorCalls int `json:"iter_calls"`

	// IteratorElapsedMs is the wall-clock from g.SetLimit() to
	// g.Wait() for this stage's inner-call errgroup. The dominant cost
	// of a high-fan-out iterator stage; isolates iteration cost from
	// endpoint-resolution / gate-check cost.
	IteratorElapsedMs int64 `json:"iter_elapsed_ms"`

	// ContentHits / ContentMisses — reserved for the Checkpoint 3
	// apistageContentServe instrumentation; ship-0.30.192 does NOT
	// populate these (deferred to keep the diff small). Left in the
	// struct so the slog JSON envelope shape is the same once they
	// land.
	ContentHits   int `json:"content_hits"`
	ContentMisses int `json:"content_misses"`

	// ApistageMemoPermitAll / ApistageMemoKeptCount — reserved for
	// Checkpoint 4 (populateCohortGateMemo). Same forward-compat
	// rationale as Content{Hits,Misses}.
	ApistageMemoPermitAll bool `json:"memo_permit_all"`
	ApistageMemoKeptCount int  `json:"memo_kept_count"`
}

// PIPStageTimingSink collects per-stage timings for a single
// seedOneRestaction call. Constructed by the caller (phase1_pip_seed.go)
// and attached to ctx via WithPIPStageTimingSink. The resolver reads
// the sink from ctx via PIPStageTimingSinkFrom and appends one entry
// per spec.api[] stage.
type PIPStageTimingSink struct {
	mu     sync.Mutex
	stages []PIPStageTiming
}

// NewPIPStageTimingSink constructs an empty sink. Returns a non-nil
// pointer; callers attach it to ctx and pass that ctx to Resolve.
func NewPIPStageTimingSink() *PIPStageTimingSink {
	return &PIPStageTimingSink{stages: make([]PIPStageTiming, 0, 8)}
}

// Append adds a finalised PIPStageTiming entry to the sink. Nil-safe
// receiver: a nil sink (no ctx wiring) is a no-op so the resolver's
// stage loop can always call sink.Append() unguarded after the
// PIPStageTimingSinkFrom lookup.
func (s *PIPStageTimingSink) Append(t PIPStageTiming) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stages = append(s.stages, t)
}

// Snapshot returns a defensive copy of the sink's stages slice. The
// caller (phase1_pip_seed.go's deferred log line) reads it ONCE at the
// end of seedOneRestaction. A nil receiver returns nil.
func (s *PIPStageTimingSink) Snapshot() []PIPStageTiming {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]PIPStageTiming, len(s.stages))
	copy(out, s.stages)
	return out
}

// ctxKeyPIPStageTimingSinkType is the typed context key for the
// PIPStageTimingSink seam. Follows the same anonymous-struct-typed-key
// pattern as ctxKeyApistagePrewarmType and friends.
type ctxKeyPIPStageTimingSinkType struct{}

var ctxKeyPIPStageTimingSink = ctxKeyPIPStageTimingSinkType{}

// WithPIPStageTimingSink returns a child context carrying the given
// sink. A nil ctx is returned unchanged (matches the WithApistagePrewarm
// nil-safety contract).
func WithPIPStageTimingSink(ctx context.Context, sink *PIPStageTimingSink) context.Context {
	if ctx == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyPIPStageTimingSink, sink)
}

// PIPStageTimingSinkFrom returns the sink attached to ctx, or nil when
// no sink is wired. Nil-safe on a nil ctx. The resolver's stage loop
// uses this to optionally record timing — the production /call path
// never installs a sink, so the resolver pays one nil-check per stage
// and nothing else.
func PIPStageTimingSinkFrom(ctx context.Context) *PIPStageTimingSink {
	if ctx == nil {
		return nil
	}
	v, _ := ctx.Value(ctxKeyPIPStageTimingSink).(*PIPStageTimingSink)
	return v
}
