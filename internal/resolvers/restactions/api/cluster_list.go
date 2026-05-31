// cluster_list.go — Ship D.5 / 0.30.152: cluster-list-when-allowed
// iterator collapse.
//
// When an RA opts in (spec.api[].clusterListWhenAllowed=true) AND the
// requester holds cluster-scope `list` on the target GVR AND the cache
// is enabled AND the Ship B typed-RBAC snapshot is published, the
// per-namespace iterator fan-out is collapsed to a SINGLE cluster-scope
// LIST against /apis/<g>/<v>/<resource>. Everything else (cache key
// derivation, identity-free apistage L1 entry, gateContentEnvelope
// per-user narrowing) flows through the existing F1 content layer.
//
// The decision is taken in `attemptClusterListCollapse` BEFORE the
// resolver's bounded errgroup. On success the helper returns ONE
// httpcall.RequestOptions (path = cluster-scope form) that the existing
// worker loop runs through apistageContentServe — the first dispatch
// populates the cache, every subsequent cluster-list-permitted user
// gets a content hit.
//
// AC-D5.13 — cache-off + Servable gate. The helper short-circuits when
//   cache.Disabled() == true (project_caching_is_provisional invariant)
//   OR when cache.Global().Snapshot() == nil (pre-readiness window: the
//   RBAC snapshot has not been published yet, so EvaluateRBAC degrades
//   to deny — we must NOT execute against a nil snapshot).
//
// AC-D5.14 — defensive multi-element shape check. After the un-gated
//   cluster-scope dispatch returns, the raw envelope is verified to be
//   a list envelope (kind ends in "List"), with a non-empty .items
//   array, and each item carries non-nil apiVersion + kind strings. On
//   any failure: the helper records cache.ReasonClusterListShapeFallback
//   and returns useClusterList=false so the resolver falls through to
//   the per-NS iterator path.
//
// All cache changes preserve the layering contract per
// feedback_restaction_no_widget_logic — the per-user RBAC narrowing
// stays at the existing gateContentEnvelope site; the cluster-list
// entry stores only the un-gated content envelope (identity-free).

package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/endpoints"
	httpcall "github.com/krateoplatformops/plumbing/http/request"
	"github.com/krateoplatformops/plumbing/jqutil"
	"github.com/krateoplatformops/plumbing/ptr"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/rbac"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// shapeCheckSlowThreshold is the AC-D5.14 conditional ratification
// budget: the defensive shape check must complete within 10ms. A slow
// fire emits a WARN so PM/architect see budget-busting evidence and
// can re-ratify. The cluster-scope envelope is parsed once at the Put
// site (apistageContentServe does the same parse on the miss path) so
// the shape check is essentially scanning a slice of decoded
// map[string]any — well inside the 10ms budget.
const shapeCheckSlowThreshold = 10 * time.Millisecond

// clusterListCollapseEnabled is the Ship S.1-re sequencing gate. The cluster-list
// iterator collapse is CORRECT but NOT yet refresher-safe: activated unconditionally
// (0.30.205) it ran the full-payload dispatch+unmarshal under the L1 refresher every
// cycle × entry-count → admin warm /call 11-22s, CPU 7.4/8. Held INERT here,
// behaviour-identical to 0.30.204 (the removed per-RA opt-in denied for all 167,111
// prod stages). S.2 flips this true AFTER landing the refresher-decoupling
// (cohort-memo reuse + skip-on-unchanged populate + customer-priority yield).
// Not an env flag (single-flag direction: end state is one CACHE_ENABLED).
//
// Ship 0.30.216 — Path 3 Phase 2'. The cluster-list collapse mechanism
// (held INERT since 0.30.212 and reverted in S.2 / 0.30.213 for wall-clock
// regression) is activated in lockstep with portal-chart 0.30.176, which
// tightens the per-stage filter on compositions-panels + blueprints-panels
// to an SPA-minimal projection (no `.spec.*` fields). Empirical post-jq
// envelope is 10.9 MiB (2.3× under 25 MB cap) per dev Phase 0 probe
// 2026-05-31. This avoids the S.2 failure mode: S.2 flipped this flag
// against the loose `del(managedFields)` per-stage filter → 56.9 MiB
// post-trim envelope → in-process scan cost dominated per-call latency.
// Phase 1' deployed portal-chart 0.30.176 FIRST against this flag still
// OFF (byte-equivalent on SPA-rendered fields), so the YAML-tight state
// is already live; this flag flip activates cluster-LIST collapse on the
// already-tight per-stage filter.
//
// Test-only override stays available via the
// withClusterListCollapseEnabledForTest helper in
// cluster_list_dep_record_test.go (the helper preserves the prior var
// value across nested invocations).
//
// Per project_single_cache_flag_direction: NOT an env flag (end state is
// one CACHE_ENABLED). The package-level var stays.
var clusterListCollapseEnabled = true

// attemptClusterListCollapse decides whether the per-stage iterator
// fan-out can be collapsed to a single cluster-scope LIST and, when so,
// returns the replacement []httpcall.RequestOptions slice — a single
// element whose Path is the cluster-scope form (no /namespaces/<x>/
// segment). The caller (resolve.go) substitutes this for the iterator's
// fan-out tmp slice; the existing bounded errgroup + apistageContentServe
// pipeline then runs over the single element and populates the
// identity-free apistage L1 entry on the miss path. Subsequent
// cluster-list-permitted users hit the cache.
//
// Returns (newTmp, useClusterList, denyGate):
//
//   - useClusterList==false — gate denied (no opt-in, cache disabled,
//     snapshot pre-readiness, RBAC deny, GVR derivation failed, or shape
//     check failed). The caller keeps its existing iterator tmp slice.
//   - useClusterList==true  — newTmp is a one-element slice; the caller
//     replaces tmp.
//
// denyGate is the 0.30.192 instrumentation seam (purely additive, no
// behaviour change): 0 means the gate passed (useClusterList==true);
// 1-7 means the corresponding gate triggered the false return — see the
// PIPStageTiming.ClusterListDenyGate doc on cache/pip_stage_timing.go for
// the value table.
//
// SEQUENCING (Ship S.1-re): the collapse is held INERT behind the
// package-level clusterListCollapseEnabled var (NEVER assigned by
// production). While inert, the FIRST statement returns deny-gate 1
// and NO later gate runs — the helper is byte-identical-behaviour to
// healthy 0.30.204 (where the removed per-RA opt-in denied for every
// prod stage). The gates 2-5 below + all helper machinery stay
// VERBATIM for S.2, just unreachable until S.2 flips the var true
// after landing refresher-decoupling.
//
// When enabled, the helper performs FOUR structural gates in order,
// short-circuiting on the first failure (no wasted work):
//
//  2. Cache-off + snapshot gate (AC-D5.13): !cache.Disabled() AND the
//     Ship B typed-RBAC snapshot is published.
//  3. Iterator present: apiCall.DependsOn.Iterator is non-empty (a
//     no-iterator stage has nothing to collapse).
//  4. GVR derivation: deriveTargetGVRForClusterList parses the first
//     iterator element's resolved Path → (GVR, ns, name); succeeds only
//     when the original path was namespace-scoped.
//  5. RBAC permission: rbac.EvaluateRBAC with Verb="list", Namespace=""
//     against the derived (group, resource) tuple returns permit==true.
//
// On ALL gates passing the helper runs the AC-D5.14 defensive
// dispatch: dispatchViaInformer un-gated → shape check → on success,
// Put under the identity-free apistage key → return the cluster-scope
// call slice. On any defensive failure the helper records the matching
// fall-through counter and returns useClusterList=false.
func attemptClusterListCollapse(
	ctx context.Context,
	log *slog.Logger,
	apiCall *templates.API,
	dict map[string]any,
	ep endpoints.Endpoint,
	apistageStore *cache.ResolvedCacheStore,
	apistageEnabled bool,
) ([]httpcall.RequestOptions, bool, int) {
	// Ship S.1-re INERT gate (was the per-RA opt-in, removed with the field).
	// Held off until S.2 lands refresher-decoupling. Returns deny-gate 1
	// (freed by the opt-in removal) so PIP timing self-documents "collapse
	// disabled, S.2-pending".
	if !clusterListCollapseEnabled {
		return nil, false, 1
	}

	// Gate 2: cache-off + Servable. AC-D5.13.
	if cache.Disabled() {
		return nil, false, 2
	}
	rw := cache.Global()
	if rw == nil {
		return nil, false, 2
	}
	if rw.Snapshot() == nil {
		// Pre-readiness window — Ship B's atomic.Pointer[RBACSnapshot]
		// has not been published yet. EvaluateRBAC degrades to deny in
		// this window; the cluster-list collapse must NOT execute
		// against a nil snapshot.
		log.Debug("cluster_list.gate_deny.snapshot_nil",
			slog.String("subsystem", "cache"),
			slog.String("ra_stage", apiCall.Name),
			slog.String("reason", "ship-b-snapshot-not-published"),
		)
		return nil, false, 2
	}
	// Gate 3: iterator present. A no-iterator stage has nothing to
	// collapse — the field is a no-op.
	if apiCall.DependsOn == nil || apiCall.DependsOn.Iterator == nil ||
		*apiCall.DependsOn.Iterator == "" {
		return nil, false, 3
	}
	// Apistage L1 is the storage substrate for the identity-free
	// cluster-list entry. When apistageEnabled is false, the storage
	// substrate is missing; we can still cluster-list-dispatch, but
	// the entry would not survive across requests, so the gate denies
	// to keep flag-off behaviour byte-identical to pre-D.5.
	if !apistageEnabled || apistageStore == nil {
		return nil, false, 2
	}

	// Gate 4: derive the target GVR from the first iterator element.
	gvr, derivedOK := deriveTargetGVRForClusterList(ctx, log, apiCall, dict)
	if !derivedOK {
		log.Debug("cluster_list.gate_deny.gvr_derivation_failed",
			slog.String("subsystem", "cache"),
			slog.String("ra_stage", apiCall.Name),
		)
		return nil, false, 4
	}

	// Gate 5: RBAC permission. cluster-scope list on the target GVR.
	user, userErr := xcontext.UserInfo(ctx)
	if userErr != nil {
		// No identity on context — degrade to iterator path; the
		// per-user token / SA-dispatch downstream narrows correctly.
		return nil, false, 5
	}
	permitOpts := rbac.EvaluateOptions{
		Username:  user.Username,
		Groups:    user.Groups,
		Verb:      "list",
		Group:     gvr.Group,
		Resource:  gvr.Resource,
		Namespace: "", // cluster-scope check — evaluate.go:213-235
	}
	permit, evalErr := rbac.EvaluateRBAC(ctx, permitOpts)
	if evalErr != nil || !permit {
		log.Debug("cluster_list.gate_deny.rbac_deny",
			slog.String("subsystem", "cache"),
			slog.String("ra_stage", apiCall.Name),
			slog.String("user", user.Username),
			slog.String("gvr", gvr.String()),
			slog.Bool("permit", permit),
			slog.Any("eval_err", evalErr),
		)
		return nil, false, 5
	}

	// All five gates passed. Build the cluster-scope call.
	clusterCall := buildClusterListCall(apiCall, ep, gvr)

	// AC-D5.14 defensive shape check — dispatch un-gated through the
	// informer pivot to obtain the raw cluster-scope envelope, then
	// verify its multi-element shape BEFORE storing in apistage L1.
	//
	// Why this prefetch + shape-check happens here (and not inside the
	// worker loop): a malformed cluster-scope envelope must NOT be
	// cached. If the shape check fired AFTER apistageContentServe's
	// Put, the bad envelope would be in L1 already and subsequent
	// requests would re-read it. Prefetching lets us reject before
	// caching, and on the success path we Put the validated envelope
	// directly so the worker loop hits the cache (no double-dispatch).
	//
	// Ship 0.30.193 Defensive prefetch breakdown — capture the
	// dispatch / parse / put sub-stage timings + envelope bytes on the
	// success path (the only path that does work whose cost matters for
	// the cohort allCompositions 91s gap). Fail-paths (unservable
	// dispatch, shape fallback) leave the accumulator at zero — the
	// sink-side AccumulateDefensive is gated on the success return
	// below at line 286.
	pipSink := cache.PIPStageTimingSinkFrom(ctx)
	dispatchStart := time.Now()
	rawEnvelope, dispatchedOK := dispatchViaInformer(
		cache.WithApistageContentResolve(ctx), clusterCall)
	defensiveDispatchMs := time.Since(dispatchStart).Milliseconds()
	if !dispatchedOK {
		// Pre-sync informer / metadata-only GVR / passthrough mode —
		// the pivot cannot serve this call. The iterator path can
		// still proceed (it dispatches per-NS through the apiserver
		// branch); fall back without recording a shape fallback (this
		// is not a malformed envelope, just a "not pivot-servable"
		// signal).
		log.Debug("cluster_list.dispatch_unservable",
			slog.String("subsystem", "cache"),
			slog.String("ra_stage", apiCall.Name),
			slog.String("gvr", gvr.String()),
		)
		return nil, false, 6
	}

	// AC-D5.14 — multi-element shape check. ≤10ms budget.
	//
	// Ship 0.30.217 Path 3.1 Bug 1 (architect-mandated correction):
	// validateClusterListShape is now O(envelope-fields + first-K-items
	// sample). The full per-item materialisation
	// (decodeClusterListItems) has been HOISTED OUT to its own
	// separately-timed step below — it is fresh-populate cost paid
	// once per cell, NOT a recurring per-call cost. The shape check's
	// `envelope_ok_elapsed` log field reflects honest, cheap shape
	// verification; the materialisation's cost surfaces under its own
	// `materialise_elapsed` field. Restores honest telemetry split per
	// architect ack of 2026-05-31.
	shapeStart := time.Now()
	shape, shapeOK, shapeReason := validateClusterListShape(gvr, rawEnvelope)
	envelopeOKElapsed := time.Since(shapeStart)
	if envelopeOKElapsed > shapeCheckSlowThreshold {
		log.Warn("cluster_list.shape_check.slow",
			slog.String("subsystem", "cache"),
			slog.String("gvr", gvr.String()),
			slog.Duration("elapsed", envelopeOKElapsed),
			slog.Duration("threshold", shapeCheckSlowThreshold),
			slog.String("hint", "AC-D5.14 budget exceeded — surface to PM"),
		)
	}
	if !shapeOK {
		cache.RecordApiserverFallthrough(ctx,
			cache.ReasonClusterListShapeFallback, gvr.String())
		log.Warn("cluster_list.shape_fallback",
			slog.String("subsystem", "cache"),
			slog.String("ra_stage", apiCall.Name),
			slog.String("gvr", gvr.String()),
			slog.String("shape_reason", shapeReason),
			slog.Int("envelope_bytes", len(rawEnvelope)),
		)
		return nil, false, 7
	}

	// Path 3.1 Bug 1 (architect-mandated correction) — materialise items
	// OUTSIDE the shape-check budget so its cost is attributed honestly
	// to its own `materialise_elapsed` log field. This is the
	// fresh-populate path; subsequent reads of the cell skip this work
	// via the stored ResolvedEntry.Items slice (the
	// apistageContentServe Get-hit path consumes parsedListEnvelope
	// without re-materialisation).
	//
	// Decode failure here is treated as a shape fallback (same
	// counter+log marker pre-correction): the envelope passed the
	// header check but a per-item byte was malformed — the iterator
	// path remains the safe fallback.
	materialiseStart := time.Now()
	parsed, decodeErr := decodeClusterListItems(shape)
	materialiseElapsed := time.Since(materialiseStart)
	if decodeErr != "" {
		cache.RecordApiserverFallthrough(ctx,
			cache.ReasonClusterListShapeFallback, gvr.String())
		log.Warn("cluster_list.shape_fallback",
			slog.String("subsystem", "cache"),
			slog.String("ra_stage", apiCall.Name),
			slog.String("gvr", gvr.String()),
			slog.String("shape_reason", decodeErr),
			slog.Int("envelope_bytes", len(rawEnvelope)),
			slog.Duration("materialise_elapsed", materialiseElapsed),
		)
		return nil, false, 7
	}

	// Store the validated envelope under the identity-free apistage key
	// BEFORE returning the cluster-scope call. The worker loop's
	// apistageContentServe will then Get-hit on this entry and skip the
	// redundant dispatchViaInformer call.
	//
	// Ship 0.30.217 Path 3.1 Bug 1 (architect-mandated correction) —
	// Items / ItemsAPIVersion / ItemsKind come from decodeClusterListItems
	// above; populating them at the Put site keeps Ship 0.30.194 Fix B's
	// no-double-decode dedup property (apistageContentServe does NOT
	// re-run parseListEnvelope on the Get-hit). Output is byte-identical
	// to pre-correction Ship 0.30.216 because decodeClusterListItems and
	// the prior in-validate per-item loop run the SAME json.Unmarshal +
	// stripManagedFields + unstructured.Unstructured wrap.
	contentKey := cache.ComputeKey(contentKeyInputs(gvr, "", ""))
	newEntry := &cache.ResolvedEntry{
		RawJSON:         rawEnvelope,
		Inputs:          ptrTo(contentKeyInputs(gvr, "", "")),
		Items:           parsed.items,
		ItemsAPIVersion: parsed.apiVersion,
		ItemsKind:       parsed.kind,
	}
	// defensiveParseMs now reflects the materialisation step (the
	// per-item decode + stripManagedFields + Unstructured wrap that the
	// architect correction moved out of validateClusterListShape). The
	// PIPStageTiming.AccumulateDefensive signature stays unchanged
	// (additive instrumentation) so seed/diff dashboards keep their
	// existing columns; the column now correctly attributes the
	// materialisation cost to the parse stage instead of leaving it
	// folded into shape_check_elapsed.
	defensiveParseMs := materialiseElapsed.Milliseconds()

	putStart := time.Now()
	apistageStore.Put(contentKey, newEntry)
	// Ship 0.30.212 — wire informer-event invalidation for the collapsed
	// cluster-scope LIST cell. Without a dep edge an informer ADD/UPDATE/
	// DELETE on any object of (gvr, *) can never dirty-mark this cell,
	// leaving it TTL-stale-forever (F-4 defect). Always LIST with name=""
	// by construction (contentKey is built with empty ns + empty name on
	// the line above), so no isList branch needed; cluster-scope → empty
	// namespace argument matches dispatcher resolve.go:550 RecordList.
	// Idempotent + sub-µs.
	cache.Deps().RecordList(contentKey, gvr, "")
	defensivePutMs := time.Since(putStart).Milliseconds()

	// Ship 0.30.193 — accumulate the defensive prefetch breakdown into
	// the in-flight PIP stage. Nil-safe sink: production /call has no
	// sink → no-op. The parent goroutine of the resolver stage loop
	// owns sink.current at this point (BeginStage has fired, no g.Go
	// has been launched yet) — no mu contention from worker writes.
	pipSink.AccumulateDefensive(
		int64(len(rawEnvelope)),
		defensiveDispatchMs,
		defensiveParseMs,
		defensivePutMs,
	)

	cache.RecordApiserverFallthrough(ctx,
		cache.ReasonClusterListDispatch, gvr.String())
	log.Info("cluster_list.dispatch",
		slog.String("subsystem", "cache"),
		slog.String("ra_stage", apiCall.Name),
		slog.String("gvr", gvr.String()),
		slog.String("user", user.Username),
		slog.Int("envelope_bytes", len(rawEnvelope)),
		slog.Duration("dispatch_elapsed", time.Since(dispatchStart)),
		// Ship 0.30.217 Path 3.1 Bug 1 (architect-mandated correction) —
		// `envelope_ok_elapsed` replaces `shape_check_elapsed`: the
		// envelope-only shape check is the actual cheap path now (no
		// per-item materialisation folded in). Field rename matches the
		// architect's "restore honest telemetry split" instruction.
		// `materialise_elapsed` is the new field for the per-item decode
		// cost — paid here on fresh-populate, NOT on cache hits.
		slog.Duration("envelope_ok_elapsed", envelopeOKElapsed),
		slog.Duration("materialise_elapsed", materialiseElapsed),
		slog.Int64("defensive_parse_ms", defensiveParseMs),
		slog.Int64("defensive_put_ms", defensivePutMs),
	)
	return []httpcall.RequestOptions{clusterCall}, true, 0
}

// deriveTargetGVRForClusterList runs ONE jq evaluation of the
// iterator's path template against the FIRST iterator element and
// parses the result via cache.ParseAPIServerPathToDep. Returns
// (gvr, true) ONLY when the parsed path was namespace-scoped — i.e.
// the iterator IS a per-namespace fan-out. A cluster-scope path
// (parsed ns=="") means the RA already operates cluster-wide — no
// collapse is needed; the caller should keep the iterator path
// verbatim. A non-apiserver / malformed path also returns false.
//
// Per design §2.3 Approach (i): the GVR is identical across all
// iteration elements by construction (the iterator fans out over
// (crd × namespace) pairs; same CRD across all of them). One
// evaluation suffices.
func deriveTargetGVRForClusterList(
	ctx context.Context,
	log *slog.Logger,
	apiCall *templates.API,
	dict map[string]any,
) (schema.GroupVersionResource, bool) {
	if apiCall.DependsOn == nil || apiCall.DependsOn.Iterator == nil {
		return schema.GroupVersionResource{}, false
	}
	iter := *apiCall.DependsOn.Iterator
	if iter == "" {
		return schema.GroupVersionResource{}, false
	}

	// Pull the FIRST iterator element via jqutil.ForEach with an
	// early-exit sentinel. Matches the per-element materialisation
	// createRequestOptions performs but stops at element 0.
	var firstElement any
	have := false
	probeErr := jqutil.ForEach(ctx, jqutil.EvalOptions{
		Query:   iter,
		Unquote: true,
		Data:    dict,
	}, func(sa any) error {
		if !have {
			firstElement = sa
			have = true
		}
		return nil
	})
	if probeErr != nil || !have {
		log.Debug("cluster_list.gvr_probe.iterator_empty_or_error",
			slog.String("subsystem", "cache"),
			slog.String("ra_stage", apiCall.Name),
			slog.Any("err", probeErr),
		)
		return schema.GroupVersionResource{}, false
	}

	// Resolve the path template against the first element. Re-use
	// evalJQ — same jq engine, same module loader as
	// createRequestOption.
	resolvedPath := evalJQ(apiCall.Path, firstElement)
	if resolvedPath == "" || strings.Contains(resolvedPath, "${") {
		return schema.GroupVersionResource{}, false
	}

	gvr, ns, _, parseOK := cache.ParseAPIServerPathToDep(resolvedPath)
	if !parseOK {
		return schema.GroupVersionResource{}, false
	}
	if ns == "" {
		// Cluster-scope path already — no collapse needed. The RA's
		// iterator does not fan out over namespaces; keep verbatim.
		return schema.GroupVersionResource{}, false
	}
	return gvr, true
}

// buildClusterListCall constructs the single cluster-scoped LIST
// httpcall.RequestOptions for the collapsed dispatch. Headers, payload,
// continue-on-error, error-key are inherited from the per-iteration
// build path (createRequestOption) so the cluster-list call sits in
// the same shape the worker loop expects — no special-case branches in
// the worker.
//
// The Path is /apis/<group>/<version>/<resource> for a named group OR
// /api/<version>/<resource> for the core group (group==""). Verb is
// forced GET (the cluster-list collapse is a read-only mechanism;
// non-GET iterator stages never reach this site — the worker would
// reject them anyway).
func buildClusterListCall(
	apiCall *templates.API,
	ep endpoints.Endpoint,
	gvr schema.GroupVersionResource,
) httpcall.RequestOptions {
	_ = ep // endpoint is wired by the worker loop's per-call assignment;
	// the cluster-list helper just constructs the call shape.

	path := clusterScopePathFor(gvr)
	out := httpcall.RequestOptions{}
	out.Path = path
	out.Verb = ptr.To(http.MethodGet)
	out.ContinueOnError = ptr.Deref(apiCall.ContinueOnError, false)
	out.ErrorKey = ptr.Deref(apiCall.ErrorKey, "error")

	// Headers — inherit the stage's declared headers VERBATIM (the
	// per-iteration evalJQ pass has nothing to substitute here: no
	// .namespace / .plural template references are present in a
	// cluster-scope call). A copy is made so a downstream mutation in
	// the worker loop never aliases back into apiCall.Headers.
	if len(apiCall.Headers) > 0 {
		out.Headers = make([]string, len(apiCall.Headers))
		copy(out.Headers, apiCall.Headers)
	}

	// Payload — clear for a LIST verb. createRequestOption evals it
	// against the iteration element; a cluster-scope LIST never carries
	// a body.

	return out
}

// clusterScopePathFor returns the apiserver REST path for the
// cluster-scoped LIST endpoint of gvr. Core group ("") uses the
// /api/<version> prefix; a named group uses /apis/<group>/<version>.
// No trailing slash. Mirrors apiserverPathFor (apistage.go) for the
// name=="" + namespace=="" form.
func clusterScopePathFor(gvr schema.GroupVersionResource) string {
	var b []byte
	if gvr.Group == "" {
		b = append(b, "/api/"...)
		b = append(b, gvr.Version...)
	} else {
		b = append(b, "/apis/"...)
		b = append(b, gvr.Group...)
		b = append(b, '/')
		b = append(b, gvr.Version...)
	}
	b = append(b, '/')
	b = append(b, gvr.Resource...)
	return string(b)
}

// shapeCheckItemSampleSize bounds the number of items the defensive
// shape check materially walks. Path 3.1 Bug 1 — the previous
// implementation iterated EVERY item in the cluster-LIST envelope (~44K
// at cyberjoker scale) which dominated per-/call latency (1.3-1.5s
// observed in 0.30.216 canonical Chrome MCP). At apiserver wire shape
// uniformity (the apiserver does not emit per-item shape drift inside
// a single LIST response — items decode through one schema), inspecting
// the first k items is sufficient to detect a malformed envelope while
// keeping the check O(1) in items.
const shapeCheckItemSampleSize = 8

// isJSONNull reports whether a json.RawMessage is the literal JSON
// `null` value, ignoring surrounding whitespace. Used by the
// sample-bounded item check: a null item slot must NOT be cached, so
// we reject at shape-check time without paying the per-item decode.
func isJSONNull(raw []byte) bool {
	// Strip ASCII whitespace (matches encoding/json's space-stripping).
	i, j := 0, len(raw)
	for i < j {
		switch raw[i] {
		case ' ', '\t', '\n', '\r':
			i++
			continue
		}
		break
	}
	for j > i {
		switch raw[j-1] {
		case ' ', '\t', '\n', '\r':
			j--
			continue
		}
		break
	}
	return j-i == 4 &&
		raw[i] == 'n' && raw[i+1] == 'u' && raw[i+2] == 'l' && raw[i+3] == 'l'
}

// envelopeShape is the Path 3.1 Bug 1 (architect-mandated correction)
// intermediate form produced by validateClusterListShape: just enough
// shape evidence to (a) confirm the envelope is a well-formed LIST and
// (b) hand off to materialisation WITHOUT having done the per-item
// decode. The rawItems slice carries the deferred per-item bytes so
// decodeClusterListItems can complete materialisation OUT of the
// latency-critical shape-check budget — the architect's exact fix
// instruction: "move decodeClusterListItems OUT of
// validateClusterListShape to the call site".
type envelopeShape struct {
	apiVersion string
	kind       string
	rawItems   []json.RawMessage
}

// validateClusterListShape enforces AC-D5.14's defensive shape check
// on the raw cluster-scope LIST envelope. Returns (shape, true, "")
// on a well-formed envelope; (zero, false, reason) otherwise.
//
// Ship 0.30.217 Path 3.1 Bug 1 + Bug 3 — surgical fixes
// (Bug 1 reflects the architect-mandated correction: materialisation
// is HOISTED out; this function is now O(envelope-fields + first-K-items
// nil-check) and never touches per-item field maps):
//
//   Bug 1 (slow shape check, 1.3-1.5s observed): the function previously
//   iterated EVERY item in the envelope with 4 map ops + per-item
//   stripManagedFields + unstructured.Unstructured allocation. At
//   cyberjoker scale (~44K items, ~10.9 MiB envelope) this dominated
//   per-/call latency. Item materialisation is redundant work in the
//   shape budget — `parseListEnvelope` runs the SAME decode at the Put
//   site (apistage.go:140-170). Fix: the shape check itself is O(1) at
//   the envelope level + sample-bounded at the item level (first k=8
//   items for nil-check only). It now returns the deferred
//   []json.RawMessage so the caller can pay the per-item decode under
//   its OWN separately-named/separately-timed step
//   (`materialise_elapsed`) — see the call site near cluster_list.go:312.
//
//   Bug 3 (per-item TypeMeta false-negative): the previous check
//   asserted `it["apiVersion"]` and `it["kind"]` are non-empty strings.
//   The apiserver does NOT emit per-item apiVersion/kind on a typed
//   LIST endpoint (those live only on the envelope; k8s API convention)
//   — and the dynamic-informer-served path (`marshalAsList` at
//   informer_dispatch.go:209-222) stores items AS DECODED with no
//   per-item TypeMeta injection. Result: EVERY informer-served
//   cluster-LIST tripped this assertion, paying both the dispatch cost
//   AND the shape-check cost for ZERO collapse benefit. Fix: drop the
//   per-item TypeMeta assertion. `parseListEnvelope` already tolerates
//   this — envelope-level TypeMeta (apiVersion/kind) is the source of
//   truth and is synthesized from the GVR if missing.
//
// Definition (post-Path-3.1):
//
//   - kind ends with "List" (envelope-level only)
//   - .items is a non-empty array of objects
//   - sampled items (first k) have non-empty raw bytes
//
// gvr is consulted ONLY when envelope.APIVersion / envelope.Kind are
// empty — synthesized from GVR per parseListEnvelope's contract.
func validateClusterListShape(gvr schema.GroupVersionResource, raw []byte) (envelopeShape, bool, string) {
	// Path 3.1 Bug 1 — envelope-only decode. We need just enough shape
	// to (a) verify the kind ends in "List", and (b) verify .items is a
	// non-empty array. json.RawMessage on Items defers the per-item
	// decode out of the latency-critical shape check; the caller picks
	// up the deferred slice via the returned envelopeShape.rawItems.
	var envelope struct {
		APIVersion string            `json:"apiVersion"`
		Kind       string            `json:"kind"`
		Items      []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return envelopeShape{}, false, "envelope-unmarshal-failed"
	}
	if !strings.HasSuffix(envelope.Kind, "List") {
		return envelopeShape{}, false, "envelope-kind-not-list"
	}
	if len(envelope.Items) == 0 {
		// AC-D5.14 says non-empty items. A genuinely-empty cluster also
		// hits this — but the iterator path's empty-result handling is
		// safer than a cached zero-item entry that could mask later
		// populations until TTL eviction. Fall back to the iterator.
		return envelopeShape{}, false, "envelope-items-empty"
	}

	// Path 3.1 Bug 1 — sample-bounded per-item nil check. The apiserver
	// emits structurally-uniform items inside a single LIST response;
	// a sample of the first k items detects malformed-envelope cases
	// (genuine nil/empty items at the byte level) without the O(N) walk.
	// "null" raw bytes are also rejected — that is the wire form of a
	// json-null item slot, which decodes to a nil map and would fail at
	// materialisation; rejecting at the sample stage keeps the budget
	// honest without inviting a cached zero-map item.
	sampleN := shapeCheckItemSampleSize
	if sampleN > len(envelope.Items) {
		sampleN = len(envelope.Items)
	}
	for i := 0; i < sampleN; i++ {
		raw := envelope.Items[i]
		if len(raw) == 0 || isJSONNull(raw) {
			return envelopeShape{}, false, "envelope-item-nil"
		}
	}

	apiVersion := envelope.APIVersion
	if apiVersion == "" {
		apiVersion = apiVersionForGVR(gvr)
	}
	kind := envelope.Kind
	if kind == "" {
		kind = listKindForResource(gvr.Resource)
	}
	return envelopeShape{
		apiVersion: apiVersion,
		kind:       kind,
		rawItems:   envelope.Items,
	}, true, ""
}

// decodeClusterListItems materialises the deferred per-item bytes from a
// validated envelopeShape into the parsedListEnvelope form that
// apistage's content-gate / ResolvedEntry consumes. Per-item decode +
// stripManagedFields run here so the latency-critical
// `validateClusterListShape` envelope check stays O(1) in items —
// Path 3.1 Bug 1 architect-mandated correction.
//
// Output is byte-identical to parseListEnvelope's output for the same
// raw input bytes (same struct tags, same stripManagedFields call, same
// []*unstructured.Unstructured{Object: it} wrap).
//
// Returns (parsedListEnvelope{}, "envelope-item-decode-failed") on any
// item-level decode failure — the caller falls back to the iterator
// path AND records cache.ReasonClusterListShapeFallback exactly as for
// a shape-check failure. Item materialisation is the cell-fresh-populate
// path; subsequent reads of the cell skip this work via the stored
// ResolvedEntry.Items slice.
func decodeClusterListItems(shape envelopeShape) (parsedListEnvelope, string) {
	items := make([]*unstructured.Unstructured, 0, len(shape.rawItems))
	for _, rawIt := range shape.rawItems {
		var it map[string]any
		if err := json.Unmarshal(rawIt, &it); err != nil {
			return parsedListEnvelope{}, "envelope-item-decode-failed"
		}
		if it == nil {
			return parsedListEnvelope{}, "envelope-item-nil"
		}
		// Ship 2a (0.30.209) — strip managedFields at the item-
		// materialisation site (mirrors parseListEnvelope), so every
		// shared entry.Items map is stripped once at load and the serve
		// path needs no per-serve removeManagedFields walk.
		stripManagedFields(it)
		items = append(items, &unstructured.Unstructured{Object: it})
	}
	return parsedListEnvelope{
		items:      items,
		apiVersion: shape.apiVersion,
		kind:       shape.kind,
	}, ""
}
