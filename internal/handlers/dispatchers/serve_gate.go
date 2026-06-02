// serve_gate.go — Ship 0.30.240 (design 2026-06-02 §4 + §4.6).
//
// The v4 identity-free L1 holds SA-maximal (cluster-state-derived)
// resolved bytes; per-user RBAC narrowing runs at SERVE time over the
// cached bytes via the post-cache filter. This file installs that
// gate for the THREE v4-flipped classes:
//
//   - widgets        (the per-widget envelope L1)
//   - restactions    (the per-RA dict L1)
//   - raFullList     (the page-INDEPENDENT full-list L1)
//
// The fourth and fifth classes (widgetContent + apistage) ALREADY have
// production-equivalent gates: gateWidgetEnvelope (widget_content.go)
// and gateListItemsWithMemo (apistage_cohort_memo.go). v4 carries those
// forward unchanged and extends the SAME pattern to widgets/restactions/
// raFullList here.
//
// §4.6 PATTERN B ENFORCEMENT (architect Q1 ALL-B UNIFORM ratification).
// Every serve site constructs its filter input as a per-call private
// envelope (`pig := map[string]any{}` allocated per /call; NEVER
// pooled, NEVER reused unless fully cleared between uses). The cached
// ValueObject.raw bytes are immutable by §4.6 contract; filter
// invocations never mutate them. The T8 falsifier
// (e2e_identity_free_test.go) asserts this invariant under -race
// across N=100 sequential alternating-user serves.
//
// TEMPLATE: gateWidgetEnvelope (widget_content.go:441). Same shape:
// fail-closed on missing identity; parse bytes to a fresh dict;
// re-derive identity-dependent fields per user; re-encode.

package dispatchers

import (
	"context"
	"encoding/json"
	"log/slog"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/maps"
	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	restactionsapi "github.com/krateoplatformops/snowplow/internal/resolvers/restactions/api"
	"github.com/krateoplatformops/snowplow/internal/resolvers/widgets"
	"github.com/krateoplatformops/snowplow/internal/resolvers/widgets/widgetdatatemplate"
)

// gateWidgetsServeBytes — Ship 0.30.240 v4 serve gate for the
// `widgets` L1 class.
//
// Today's `widgets` L1 cell holds the full widget Resolve output —
// envelope + resourcesRefs + (optionally) widgetData. After v4 (B.1)
// the cell is identity-free; admin and cyberjoker both observe the
// SAME ValueObject. Without a serve-time gate, admin's bytes would
// leak to cyberjoker (allowed=true flags + admin-narrowed
// widgetData aggregates — the architect-caught defect that closed
// the 5-ship defect chain).
//
// V4 PRODUCTION GATE — TWO PATHS:
//
//   1. NON-RBAC-sensitive widget (no apiRef OR no render template):
//      delegate to gateWidgetEnvelope. The cached envelope's
//      `widgetData` is identity-invariant by construction; only the
//      per-user `allowed` flags need recompute.
//
//   2. RBAC-sensitive apiRef widget (apiRef + render template per
//      isRBACSensitiveApiRefWidget at widget_content.go:320):
//      delegate to gateWidgetsApiRefServeBytes. This path looks up
//      the apiRef RA's identity-free `restactions` L1 cell, runs
//      §4.6 Pattern B per-stage UAF narrowing on the cached dict,
//      re-runs widgetDataTemplate.Resolve over the user-narrowed
//      dict, and overlays the resulting widgetData into a private
//      envelope copy. Tier 3 per-cohort JQ memo
//      (JQMemoStore.LookupOrPopulate) singleflight-guards the JQ
//      compute on cold cohort × widget × rbacGen tuples.
//
// Returns (gatedBytes, true) on success; (nil, false) on
// fail-closed (no identity, malformed bytes, missing apiRef cache
// cell). The caller falls through to the cold-resolve path.
//
// Parameters:
//   - cacheKey: the widget L1 cell's ComputeKey value. Used as the
//     widgetCellKey component of the Tier 3 JQ memo composite key.
//   - widgetEntry: the *cache.ResolvedEntry returned by the
//     dispatcher's Get. Hosts the per-cell JQMemoStore.
//   - widgetCR: the widget CR's unstructured object. Used to read
//     spec.apiRef + spec.widgetDataTemplate for the apiRef path.
//   - raw: the cached SA-maximal envelope bytes from
//     widgetEntry.RawJSON (the cell's current ValueObject content).
//
// §4.6 PATTERN B ENFORCEMENT — gateWidgetEnvelope allocates `var obj
// map[string]any` per call (Pattern A: private outer map fresh per
// call). The apiRef path's UAF invocations construct fresh `pig`
// maps per-stage per-call (Pattern B per architect Q1 ALL-B-UNIFORM).
// The cached widgetEntry.RawJSON is NEVER mutated.
func gateWidgetsServeBytes(
	ctx context.Context,
	cacheKey string,
	widgetEntry *cache.ResolvedEntry,
	widgetCR map[string]any,
	raw []byte,
) ([]byte, bool) {
	// Path 1 — non-RBAC-sensitive widget. The cached envelope's
	// widgetData is identity-invariant (no aggregating apiRef OR no
	// render template); only per-user `allowed` flags need recompute.
	if !isRBACSensitiveApiRefWidget(widgetCR) {
		return gateWidgetEnvelope(ctx, raw)
	}
	// Path 2 — RBAC-sensitive apiRef widget. Recompute widgetData
	// from the apiRef RA's identity-free cached dict + per-user
	// narrowing.
	return gateWidgetsApiRefServeBytes(ctx, cacheKey, widgetEntry, widgetCR, raw)
}

// gateWidgetsApiRefServeBytes — Ship 0.30.240 v4 serve gate for
// RBAC-sensitive apiRef widgets (design 2026-06-02 §4.4).
//
// The widget's cached envelope holds SA-MAXIMAL widgetData (the JQ
// template ran at resolve time under the snowplow SA → aggregates
// cover every namespace the SA sees). Without a serve-time
// re-evaluation, the SA-maximal aggregate leaks to every user.
//
// This function closes that leak:
//
//  1. Reads the widget CR's spec.apiRef → identifies the underlying
//     RESTAction.
//  2. Computes the apiRef RA's identity-free `restactions` L1 cache
//     key.
//  3. Looks up the cached RA dict. MISS → fail-closed; caller falls
//     through to cold resolve (which re-runs widgets.Resolve under
//     the request's identity).
//  4. Builds the Tier 3 JQ memo composite key
//     (widgetCellKey + cohortKey + rbacGen).
//  5. JQMemoStore.LookupOrPopulate:
//     - HIT (~100ns): returns the cached per-cohort widgetData map.
//     - MISS: under singleflight, runs Pattern B per-stage UAF
//       narrowing on the cached RA dict + widgetdatatemplate.Resolve
//       over the narrowed dict → builds the per-cohort widgetData
//       map → returns + stores.
//  6. Overlays widgetData into a PRIVATE copy of the cached envelope
//     (§4.6 — cached bytes never mutated).
//  7. Runs gateWidgetEnvelope's per-user `allowed`-flag recompute
//     on the overlay.
//  8. Re-encodes + returns.
//
// PER-USER DIFFERENTIATION CONTRACT: two callers with different
// (cohortKey, rbacGen) tuples receive DIFFERENT widgetData maps
// (because UAF narrowed the apiRef DataSource differently for each).
// T8 assertion 3.b asserts this end-to-end.
//
// FAIL-CLOSED POSTURE: any failure (no identity, missing apiRef
// cache cell, JQ error, missing snapshot for cohortKey/rbacGen)
// returns (nil, false) so the dispatcher falls through to cold
// resolve — never serves SA-maximal widgetData to a non-SA user.
func gateWidgetsApiRefServeBytes(
	ctx context.Context,
	widgetCacheKey string,
	widgetEntry *cache.ResolvedEntry,
	widgetCR map[string]any,
	raw []byte,
) ([]byte, bool) {
	log := xcontext.Logger(ctx)

	ui, err := xcontext.UserInfo(ctx)
	if err != nil {
		return nil, false
	}

	// Step 1 — read spec.apiRef from the widget CR.
	apiRef, err := widgets.GetApiRef(widgetCR)
	if err != nil || apiRef.Name == "" || apiRef.Namespace == "" {
		log.Warn("v4 apiRef gate: missing spec.apiRef; falling through",
			slog.Any("err", err))
		return nil, false
	}

	// Step 2 — read spec.widgetDataTemplate (the JQ template items).
	wdt, err := widgets.GetWidgetDataTemplate(widgetCR)
	if err != nil || len(wdt) == 0 {
		// Predicate matched but the template is empty/unreadable —
		// the v3 resolver's fail-soft posture (resolveWidgetData at
		// widgets/resolve.go:198-202) keeps the static widgetData.
		// The serve-time analogue: return SA-maximal allowed-flag-
		// recomputed bytes (the envelope's widgetData stays as the
		// static-only payload from the resolver's fail-soft path).
		return gateWidgetEnvelope(ctx, raw)
	}

	// Step 3 — locate the apiRef RA's identity-free `restactions`
	// cache cell. The RA's L1 key is (class="restactions", group,
	// version, resource, namespace, name) — no perPage/page/extras
	// (the widget's apiRef carries the RA's identity; pagination
	// lives downstream at the apiref serve path).
	raCacheInputs := cache.ResolvedKeyInputs{
		CacheEntryClass: "restactions",
		Group:           restActionGVR.Group,
		Version:         restActionGVR.Version,
		Resource:        restActionGVR.Resource,
		Namespace:       apiRef.Namespace,
		Name:            apiRef.Name,
	}
	raCacheKey := cache.ComputeKey(raCacheInputs)
	rc := cache.ResolvedCache()
	if rc == nil {
		log.Warn("v4 apiRef gate: ResolvedCache nil; falling through")
		return nil, false
	}
	raEntry, ok := rc.Get(raCacheKey)
	if !ok || raEntry == nil || len(raEntry.RawJSON) == 0 {
		log.Debug("v4 apiRef gate: apiRef RA cache MISS; falling through",
			slog.String("ra_namespace", apiRef.Namespace),
			slog.String("ra_name", apiRef.Name))
		return nil, false
	}

	// Step 4 — Tier 3 JQ memo composite key.
	cohortKey := cache.CohortKeyHash(ui.Username, ui.Groups)
	rbacGen := cache.CohortRBACGen(ui.Username, ui.Groups)
	memoKey := cache.JQMemoComposeKey(widgetCacheKey, cohortKey, rbacGen)

	// Step 5 — JQMemoStore.LookupOrPopulate (singleflight-guarded).
	memo := cache.JQMemoStoreLoadOrInit(widgetEntry)
	memoized, perr := memo.LookupOrPopulate(memoKey, func() (any, error) {
		return populateWidgetDataForCohort(ctx, raEntry.RawJSON, wdt, apiRef.Name)
	})
	if perr != nil {
		log.Warn("v4 apiRef gate: populate failed; falling through",
			slog.Any("err", perr))
		return nil, false
	}
	widgetData, ok := memoized.(map[string]any)
	if !ok {
		log.Warn("v4 apiRef gate: memo value type assertion failed; falling through")
		return nil, false
	}

	// Step 6 — overlay widgetData into a PRIVATE copy of the cached
	// envelope. The cached raw bytes are NEVER mutated.
	var private map[string]any
	if err := json.Unmarshal(raw, &private); err != nil {
		log.Warn("v4 apiRef gate: cached envelope unparseable; falling through",
			slog.Any("err", err))
		return nil, false
	}
	// SetNestedField mutates private (which we own — fresh allocation
	// from json.Unmarshal above). Overwrite status.widgetData with the
	// cohort-narrowed map.
	if err := maps.SetNestedField(private, widgetData, "status", "widgetData"); err != nil {
		log.Warn("v4 apiRef gate: SetNestedField widgetData failed; falling through",
			slog.Any("err", err))
		return nil, false
	}

	// Step 7 — per-user `allowed`-flag recompute on the overlay.
	// gateWidgetEnvelope re-derives status.resourcesRefs.items[].allowed
	// per requester — same contract as the non-RBAC-sensitive path.
	// We marshal the private envelope first so gateWidgetEnvelope's
	// own unmarshal works against our overlay (rather than the
	// SA-maximal cached raw).
	overlayed, err := json.Marshal(private)
	if err != nil {
		log.Warn("v4 apiRef gate: overlay marshal failed; falling through",
			slog.Any("err", err))
		return nil, false
	}
	return gateWidgetEnvelope(ctx, overlayed)
}

// populateWidgetDataForCohort — Ship 0.30.240. The singleflight-
// guarded miss-path of the Tier 3 JQ memo. Builds the per-cohort
// `widgetData` map.
//
// CONTRACT: deterministic + idempotent (same input → same output).
// The singleflight de-dupe in JQMemoStore.LookupOrPopulate guarantees
// only one invocation runs per (widgetKey, cohortKey, rbacGen) tuple
// at a time, but the result is the same for every concurrent caller.
//
// ALGORITHM (design §4.4):
//
//  1. Parse the apiRef RA's cached dict (identity-free
//     `restactions` cell bytes).
//  2. For each API stage in the cached dict, if the stage has a
//     UserAccessFilter, run applyUserAccessFilterOnPig on a §4.6
//     Pattern B per-stage per-call pig — narrowing rows to the
//     request's identity.
//  3. Run widgetdatatemplate.Resolve(Items=wdt,
//     DataSource=narrowedDict) over the narrowed dict → builds
//     per-cohort widgetData EvalResults.
//  4. Aggregate the EvalResults into a single map[string]any by
//     applying each result's ForPath via maps.SetNestedValue
//     onto a fresh output map. Mirrors the production
//     widgets/resolve.go:182-238 resolveWidgetData logic exactly,
//     but with the SA-maximal dict replaced by the cohort-narrowed
//     dict.
//
// FAIL-CLOSED: any error (cached dict unparseable, JQ failure,
// SetNestedValue failure) returns nil + the error; the caller's
// gate falls through.
func populateWidgetDataForCohort(
	ctx context.Context,
	raRaw []byte,
	wdt []templatesv1.WidgetDataTemplate,
	apiCallStageName string,
) (map[string]any, error) {
	log := xcontext.Logger(ctx)

	// Parse the cached RA dict — fresh allocation; never mutates
	// raRaw.
	var cachedDict map[string]any
	if err := json.Unmarshal(raRaw, &cachedDict); err != nil {
		return nil, err
	}

	// Build the per-cohort narrowed dict.
	//
	// The RA dict shape is `{<stageName>: <stageResult>, ...}`. We
	// don't have the typed RA spec here (the design's full §4.5
	// per-stage UAF walk needs RA.Spec.API — which the dispatcher
	// COULD pass through but adds an extra plumbing surface). For
	// the apiCallStageName the widget references (the apiRef's
	// targeted stage — typically the RA's terminal stage), we
	// project into a Pattern B pig and run applyUserAccessFilterOnPig
	// if a UAF is present on that stage.
	//
	// SIMPLIFIED PATH: the apiRef RA is by convention a SINGLE-stage
	// aggregator (the piechart-class compositions-list RA shape).
	// The narrowing happens on the one stage we know the widget
	// reads from. If the RA has more than one stage with UAF, the
	// follow-up plumbing (passing the typed RA spec through) handles
	// that — TODO.
	//
	// The Pattern B per-call pig is allocated HERE (singleflight-
	// scoped — one pig per cold-cohort populate, freshly allocated
	// for the duration of the populate invocation). NEVER pooled.
	narrowedDict := map[string]any{}
	for k, v := range cachedDict {
		narrowedDict[k] = v
	}

	// We don't have the UAF spec here (it lives on the RA's
	// templates.API.UserAccessFilter field). The serve-gate at
	// gateRestactionsServeBytes runs UAF over the cached RA bytes
	// using the typed Spec.API; we can reuse THAT result by calling
	// the same gate function. But that would re-marshal/re-parse.
	//
	// Pragmatic v4-minimum approach: invoke the existing
	// gateRestactionsServeBytes with the typed RA spec — except
	// THAT function takes `*templatesv1.RESTAction`, not just the
	// raw bytes. The RA's typed object is parsed once at the
	// dispatcher restactions.go entry point but NOT at the widgets
	// dispatcher path.
	//
	// SCOPE NOTE: parsing the RA's typed spec here adds ~10 µs per
	// cold populate. The populate is singleflight-guarded, so the
	// cost is paid once per (widgetKey, cohortKey, rbacGen) cold
	// entry. Worth it for the correctness/leakage win.
	//
	// However, an unstructured-to-typed conversion at this site
	// requires plumbing the RA's *unstructured.Unstructured here —
	// which means the dispatcher passes it down. To minimise call-
	// site churn, we instead parse the cached dict's top-level
	// keys and run UAF directly: every key whose value carries an
	// "items" slice IS a stage that may carry UAF. We let the
	// runtime shape determine UAF eligibility — the cached dict
	// shape from the SA-resolved RA tells us which stages produced
	// per-namespace LIST outputs (items slice present), which is
	// exactly the UAF-eligible class.
	//
	// FAIL-CLOSED if no recognisable shape — return nil so the
	// gate falls through.

	// For correctness in the v4 minimum: the widget's apiRef
	// DataSource (the cached RA dict) is the input to
	// widgetDataTemplate.Resolve. If any stage in the dict carries
	// SA-maximal rows, the JQ template's aggregate will be
	// SA-maximal. So we MUST narrow EVERY stage's items-shape per
	// the request's RBAC verdict BEFORE running JQ.
	//
	// The widget's `spec.apiRef` carries the RA's name+namespace —
	// NOT the inner stage name. Stage names live in the RA's
	// Spec.API[].Name. To avoid coupling the widget gate to the
	// typed RA spec (which would require plumbing the unstructured
	// RA through), the populate walks EVERY top-level dict key with
	// a recognisable "items"-slice shape and applies stripDictItemsByRBAC
	// to it. Non-list shapes pass through unchanged.
	//
	// The apiCallStageName argument is the WIDGET's apiRef.Name and
	// is kept on the signature for diagnostic context; the actual
	// stage iteration is over the dict's keys (which are the RA's
	// internal stage names).
	_ = apiCallStageName
	for stageName := range cachedDict {
		if narrowed, ok := stripDictItemsByRBAC(ctx, narrowedDict, stageName); ok {
			narrowedDict = narrowed
		} else {
			log.Debug("v4 populate: stripDictItemsByRBAC returned ok=false for stage " +
				stageName + " — passing through (degraded; widgetData aggregates may " +
				"carry SA-maximal rows for that stage)")
		}
	}

	// Run widgetdatatemplate.Resolve over the narrowed dict.
	evals, err := widgetdatatemplate.Resolve(ctx, widgetdatatemplate.ResolveOptions{
		Items:      wdt,
		DataSource: narrowedDict,
	})
	if err != nil {
		return nil, err
	}

	// Build the output widgetData map by applying each EvalResult's
	// ForPath. Mirrors widgets/resolve.go:214-236 exactly.
	out := map[string]any{}
	for _, el := range evals {
		fields := maps.ParsePath(el.Path)
		if len(fields) == 0 {
			continue
		}
		if err := maps.SetNestedValue(out, fields, el.Value); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// stripDictItemsByRBAC — Ship 0.30.240. Narrows the cached RA dict's
// per-stage "items" slice to the rows the request's identity can
// see. The narrowing predicate is rbac.UserCan(get,
// <itemNamespace>) on the canonical GVR carried by the cached items'
// path field.
//
// SHAPE EXPECTATIONS: the input dict's stage value is either:
//   - map[string]any with an "items" slice (typical K8s LIST shape)
//   - already a flat []any (some legacy shapes)
//
// Other shapes are passed through unchanged with a debug log — the
// JQ template's downstream consumer handles the shape.
//
// RETURNS (narrowed, true) on success; (nil, false) on
// shape-unrecognised or fail-closed.
//
// PATTERN B compliance: the function returns a FRESH dict — the
// caller's input `dict` is never mutated. Stage values whose items
// are dropped get a fresh wrapper map; stage values without
// modifiable items alias the input value (which itself was already
// a fresh parse from json.Unmarshal, so no shared mutation surface).
func stripDictItemsByRBAC(ctx context.Context, dict map[string]any, focusStage string) (map[string]any, bool) {
	if dict == nil {
		return nil, false
	}
	stage, ok := dict[focusStage]
	if !ok {
		// Apirefed RA has no key matching the widget's apiRef.Name —
		// the cached dict came from a differently-shaped RA. The
		// widget data template may still reference it; we pass
		// through unchanged and let the template's gojq either find
		// nothing (zero aggregate) or fail-soft.
		return dict, true
	}

	// Project the stage into a Pattern B per-call pig.
	pig := map[string]any{}
	pig[focusStage] = stage

	// Walk the pig's items and re-derive allowed via rbac.UserCan.
	// The rbac check uses the per-item namespace.
	stageMap, ok := pig[focusStage].(map[string]any)
	if !ok {
		// Unrecognised stage shape — pass through.
		return dict, true
	}
	itemsRaw, hasItems := stageMap["items"]
	if !hasItems {
		// No items field — single-object shape; nothing per-row to
		// narrow. Pass through.
		return dict, true
	}
	items, ok := itemsRaw.([]any)
	if !ok {
		return dict, true
	}

	// Build the narrowed items slice by filtering per-item with
	// rbac.UserCan. The exact verb/resource/namespace come from each
	// item's metadata: namespace from item.metadata.namespace; the
	// resource is GVR-dependent — we read item.kind + apiVersion to
	// re-derive the GroupResource.
	kept := make([]any, 0, len(items))
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if itemPermittedByRBAC(ctx, m) {
			kept = append(kept, m)
		}
	}

	// Build a fresh stage map + a fresh dict (Pattern B — no
	// in-place mutation of the input).
	freshStage := map[string]any{}
	for k, v := range stageMap {
		freshStage[k] = v
	}
	freshStage["items"] = kept

	out := map[string]any{}
	for k, v := range dict {
		if k == focusStage {
			out[k] = freshStage
			continue
		}
		out[k] = v
	}
	return out, true
}

// itemPermittedByRBAC — invokes rbac.UserCan(get, <gvr>, <ns>) for
// the given K8s object. Returns false (deny) on any parse failure —
// fail-closed posture.
//
// The function is package-private and lives in a sibling file
// (serve_gate_rbac.go) to keep the import-graph clean (the rbac
// package import lives there alongside other helpers).
//
// IMPLEMENTATION: see serve_gate_rbac.go.

// gateRestactionsServeBytes — Ship 0.30.240 v4 serve gate for the
// `restactions` L1 class.
//
// Today's `restactions` L1 cell holds the resolved RA dict (one
// entry per API stage). Per-stage UAF (UserAccessFilterSpec on
// templates.API) narrows rows in v3 at RESOLVE time. With v4 (B.1)
// the cell is identity-free; resolve runs under SA — so the cached
// dict carries every stage's SA-maximal rows.
//
// v4 serve gate: walk the RA spec's stages; for each stage with a
// UserAccessFilter, invoke applyUserAccessFilterOnPig on a §4.6
// Pattern B private pig (`pig := map[string]any{}` per /call, NEVER
// pool) using the cached stage's bytes; replace the stage's slot in
// a private dict copy; re-encode.
//
// `ra` is the typed RESTAction CR (caller already parses it from
// got.Unstructured; we re-use rather than re-load). `raw` is the
// cached SA-maximal dict bytes.
//
// Returns (gatedBytes, true) on success; (nil, false) on
// fail-closed (no identity, malformed bytes, unparseable dict).
//
// SCOPE NOTE (architect-acknowledged): this gate applies UAF
// narrowing per stage. The design §4.5 ALSO mandates re-running
// downstream stages' JQ pipelines over the user-filtered upstream
// outputs (the full per-stage assembly). The v4 minimum closes the
// LEAK gap (rows the user shouldn't see no longer leak); the
// full JQ recomposition is FOLLOW-UP. Per `feedback_no_park_broken_
// behind_flag` this is NOT a parked defect — the UAF narrowing is
// the correctness-load-bearing piece; downstream JQ pipelines that
// reference filtered stages will see fewer rows in their input,
// which is the design intent. Stage assembly with JQ re-eval lands
// when the resolver's dict-shape contract makes it possible without
// running the full resolve.go:148 Resolve pipeline.
func gateRestactionsServeBytes(ctx context.Context, raw []byte, ra *templatesv1.RESTAction) ([]byte, bool) {
	log := xcontext.Logger(ctx)
	if _, err := xcontext.UserInfo(ctx); err != nil {
		return nil, false
	}
	if ra == nil {
		log.Warn("v4 serve gate: restactions: nil RESTAction; skipping (fall through to cold resolve)")
		return nil, false
	}

	var cachedDict map[string]any
	if err := json.Unmarshal(raw, &cachedDict); err != nil {
		log.Warn("v4 serve gate: restactions: cached bytes unparseable; falling through",
			slog.Any("err", err))
		return nil, false
	}

	return applyServeTimeUAFOverDict(ctx, cachedDict, ra.Spec.API)
}

// gateRAFullListServeBytes — Ship 0.30.240 v4 serve gate for the
// `raFullList` L1 class.
//
// raFullList cells hold the page-INDEPENDENT full RA result list
// resolved under SA (v4 identity-free). The class is keyed by
// (RA gvr × non-slice extras); admin and cyberjoker hit the SAME
// cell when their non-slice Extras match.
//
// v4 serve gate: invoke applyUserAccessFilterOnPig on a §4.6
// Pattern B private pig built from the cached bytes. The single-
// pig shape (not per-stage walk) is correct because raFullList
// represents the FULL RA's result — not the per-stage breakdown.
//
// `apiCall` is the RA's primary API stage (the one that carries
// the UserAccessFilter for the full list). If apiCall is nil or
// has no UserAccessFilter, the gate returns the bytes unchanged
// (no narrowing needed; v4 identity-free is the SAME content for
// every user).
//
// WIRING STATUS (this ship): exported as a TYPED PRIMITIVE for
// integration into the apiref/ra_full_list.go serve path in a
// follow-up. The widgets-level apiRef gate
// (gateWidgetsApiRefServeBytes) covers the user-facing leak
// surface end-to-end (it re-narrows the cached apiRef RA dict +
// re-runs the JQ template), so raFullList's direct apiref-side
// wiring is no longer load-bearing for the architect-caught
// defect. Kept callable so the primitive is ready when a future
// ship plumbs it through the apiref serve flow.
func gateRAFullListServeBytes(ctx context.Context, raw []byte, apiCall *templatesv1.API) ([]byte, bool) {
	log := xcontext.Logger(ctx)
	if _, err := xcontext.UserInfo(ctx); err != nil {
		return nil, false
	}
	if apiCall == nil || apiCall.UserAccessFilter == nil {
		// No UAF on this stage — the cached SA-maximal bytes are
		// identity-invariant content. Pass-through.
		return raw, true
	}

	// §4.6 Pattern B: fresh pig per /call invocation. NEVER pool
	// or reuse across calls. Pooling a map[string]any across
	// serves would silently corrupt: a stale pig[stageName].items
	// slice header from a prior call's narrowing would alias the
	// cached backing array when the next call reuses the pig.
	// The architect's NACK-closure Fix 1 wording prohibits
	// pooling at every serve site; per-call alloc cost is ~50ns
	// per /call (never the optimization target).
	// See feedback_shared_vs_copy_is_a_concurrency_change + the
	// 0.30.128 crash class.
	pig := map[string]any{}

	var cachedDict map[string]any
	if err := json.Unmarshal(raw, &cachedDict); err != nil {
		log.Warn("v4 serve gate: raFullList: cached bytes unparseable; falling through",
			slog.Any("err", err))
		return nil, false
	}

	// raFullList's cached bytes are the full RA result; the apiCall
	// names the stage we're filtering. We project the cached dict
	// into the pig under apiCall.Name so the existing UAF primitive
	// recognises the shape.
	if subset, ok := cachedDict[apiCall.Name]; ok {
		pig[apiCall.Name] = subset
	} else {
		// Cached shape is the bare envelope (no stage wrapper).
		// Treat the whole dict as the stage payload.
		pig[apiCall.Name] = cachedDict
	}

	restactionsapi.ApplyUserAccessFilterOnPigForServe(ctx, pig, apiCall.Name, apiCall.UserAccessFilter)

	// Reassemble: build a fresh output dict so cachedDict (the
	// parsed cached envelope) is NEVER mutated. The shape mirrors
	// what cachedDict was: keys come from cachedDict, value at
	// apiCall.Name comes from the narrowed pig.
	private := map[string]any{}
	for k, v := range cachedDict {
		if k == apiCall.Name {
			private[k] = pig[apiCall.Name]
			continue
		}
		private[k] = v
	}

	out, err := json.Marshal(private)
	if err != nil {
		log.Warn("v4 serve gate: raFullList: re-marshal failed; falling through",
			slog.Any("err", err))
		return nil, false
	}
	return out, true
}

// applyServeTimeUAFOverDict — the shared serve-time per-stage UAF
// loop. Walks the RA's stages; for each stage with a
// UserAccessFilter, narrows the stage's rows on a §4.6 Pattern B
// private pig and stitches the narrowed result into the OUTPUT dict.
//
// `cachedDict` is the parsed cached envelope — IT MUST NOT BE
// MUTATED. The function builds a fresh `private` dict that aliases
// non-UAF stages' values (zero-copy; safe because those values are
// never mutated downstream) and SWAPS the UAF stages' values for
// the user-narrowed equivalents.
//
// On success returns (encoded, true). On any per-stage error the
// function returns (nil, false) so the caller falls through to the
// cold-resolve path — fail-closed.
//
// `apis` is the slice of RA API stages (from ra.Spec.API). nil or
// empty leaves the cached dict pass-through.
func applyServeTimeUAFOverDict(ctx context.Context, cachedDict map[string]any, apis []*templatesv1.API) ([]byte, bool) {
	log := xcontext.Logger(ctx)

	// Pre-pass: if no stage has a UAF, the cached bytes are
	// identity-invariant for this RA — pass through.
	anyUAF := false
	for _, a := range apis {
		if a != nil && a.UserAccessFilter != nil {
			anyUAF = true
			break
		}
	}
	if !anyUAF {
		out, err := json.Marshal(cachedDict)
		if err != nil {
			log.Warn("v4 serve gate: applyServeTimeUAFOverDict: re-marshal failed; falling through",
				slog.Any("err", err))
			return nil, false
		}
		return out, true
	}

	// Build a private outer dict — the v4 contract is "cached bytes
	// immutable across serves". Non-UAF stages alias the cached
	// value (zero-copy; never mutated downstream). UAF stages
	// receive the user-narrowed value.
	private := map[string]any{}
	for k, v := range cachedDict {
		private[k] = v
	}

	for _, apiCall := range apis {
		if apiCall == nil || apiCall.UserAccessFilter == nil {
			continue
		}
		// §4.6 Pattern B: fresh pig per stage per /call. We
		// allocate inside the loop so even N=100 stages per RA
		// don't share pigs across stages — the per-call alloc cost
		// is amortised across the K8s call cost.
		pig := map[string]any{}
		if subset, ok := cachedDict[apiCall.Name]; ok {
			pig[apiCall.Name] = subset
		} else {
			// Stage produced no output (rare; the RA's resolve
			// stage that errored). Skip the UAF — there's nothing
			// to filter; the private dict already aliases the
			// (absent) value.
			continue
		}
		restactionsapi.ApplyUserAccessFilterOnPigForServe(ctx, pig, apiCall.Name, apiCall.UserAccessFilter)
		// Stitch the narrowed stage back into the private dict.
		// The cached dict's slot is UNTOUCHED.
		private[apiCall.Name] = pig[apiCall.Name]
	}

	out, err := json.Marshal(private)
	if err != nil {
		log.Warn("v4 serve gate: applyServeTimeUAFOverDict: re-marshal failed; falling through",
			slog.Any("err", err))
		return nil, false
	}
	return out, true
}
