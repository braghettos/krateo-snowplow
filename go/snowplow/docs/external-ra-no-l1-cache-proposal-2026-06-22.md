# Unified proposal — internal references resolve in-process (cacheable); external endpoints take HTTP + skip L1 (2026-06-22)

**Status:** Internal-half mechanism **RATIFIED by Diego 2026-06-22** (direct-apiserver-path + `spec.api[].resolve`, default true). External half: PROPOSAL → PM gate → dev. Plan only.
**Unified verdict:** **FEASIBLE-WITH-CAVEATS.** Both halves share one rule and compose cleanly by construction. The external half is sound (exact #313 prior art). The internal half resolves a referenced RESTAction/Widget IN-PROCESS off a **direct apiserver path** (not a `/call` loopback), gated by the new `resolve` property — which records the referenced-CR dep edge **for free** (the apiserver path is `parseOK=true`), so the loopback form's dep-staleness gap is auto-closed. Two caveats survive: the default-true backward-compat surface (a direct GET of a raw RA/widget CR now resolves) and the dep-propagation constraint (the in-process resolve must run under the outer L1-key ctx).

## The unifying rule
**Internal references (RA→RA, RA→widget) resolve IN-PROCESS and stay cacheable; only genuine external endpoints take the HTTP path and skip L1.** A reference is "internal" if its api-step `path` is an apiserver path (`/api…`/`/apis…`, parseOK at resolve.go:1256) — including a direct path to a RESTAction/Widget CR with `resolve: true` (the new recommended form) — or the legacy `/call?resource=…` loopback (resolve.go:625/627); it is "external" only if control falls through to the live HTTP fetch (`httpFetchAllowingNonJSON`, resolve.go:927).

---

# EXTERNAL HALF — external-API RESTActions skip the L1 resolve cache

## Goal
A RESTAction whose resolve **touches an external endpoint** (an `endpointRef` to a non-apiserver URL — the live `httpFetchAllowingNonJSON` path, ADR 0006) must **not** have its resolved result Put into the L1 resolve cache. External data has no informer/dep edge to invalidate it (`RecordList`/`Record` fire only for apiserver GVRs), so today an external-RA L1 entry serves up-to-TTL-stale data and is never invalidated when the source changes. Skipping the cache makes every `/call` re-resolve and re-fetch the external API **live (fresh)**.

## Retained advantage (CONFIRMED, TRACED)
Skipping the OUTER resolve cache loses ONLY whole-result memoization. Apiserver stages are served per-stage **during** resolve, independent of the outer cache: `apistageContentServe` (`resolve.go:738`) / `dispatchViaInformer` (`:784`), both inside `if resolverUseInformer()` (`:736`) — a `served=true` returns the stage from the **L3 informer**, no apiserver round-trip. `resolveOne` branch order: (1) loopback `:625`, (2) informer/pivot `:736`, (3) internal-rest-config SA dispatch `:852`, (4) **external fetch `:927`** (fall-through). So a mixed RA's apiserver stages still L3-serve; only the resolve jq + the external fetch re-run each call.

## Detection (TRACED — structural, no URL/host literals)
"A true external fetch" ≡ control reached `httpFetchAllowingNonJSON` at **`resolve.go:927`**. By construction this is reachable only when not-loopback (branch 1 skipped) and `parseOK=false`/pivot-declined (branches 2–3 declined) — the loopback / SA-transport / informer-pivot / internal-rest-config paths all `return nil` *before* `:927` and **cannot** reach it. So mis-classifying an internal branch as external is **structurally impossible** if the signal is bumped **at the dispatch site (`:927`)**, not inferred from the Endpoint shape.

**Mechanism:** add `WithExternalTouchedSink` — a byte-for-byte clone of `StageErrorSink` (`internal/cache/stage_error_sink.go`, an `*atomic.Int64` on `ctx`). Bump it on one line at `resolve.go:927`. No special-cases (`feedback_no_special_cases`). Each Put site reads `extSink.Count() > 0` and **declines the Put** (body still served — identical to the #313 partial-result posture).

## The caveat — FIVE Put surfaces must gate (TRACED)
| # | Put site | file:line | sink reaches it? | action |
|---|---|---|---|---|
| 1 | restactions per-cohort | `dispatchers/restactions.go:265` | yes (`ctx` sink `:212`) | gate |
| 2 | widgets per-cohort | `dispatchers/widgets.go:307` | yes (`ctx` sink `:248`; sink visible at `:927` via widget→apiref→RA ctx inheritance) | gate |
| 3 | widgetContent shell | `dispatchers/widget_content.go:159` (`populateWidgetContentL1`) | **NO** — F2 walker ctx installs no sink | install sink on walker `resolveCtx` + gate (3a) |
| 4 | RAFullList apiref cell | `widgets/apiref/ra_full_list.go:159` & `:262` (`PutRAFullList`) | sink present on `fullCtx`, but Put is inside the apiref resolver | gate at both lines (+ skip the `Deps().Record`) |
| 5 | refresher re-Put | `dispatchers/resolve_populate.go:291` | only re-Puts already-cached keys (never external) | defense-in-depth gate (recommended) |

**#4 is load-bearing:** if only the dispatcher Put (#2) is gated and the RAFullList cell (#4) is forgotten, the external aggregate is cached and served stale across pages/widgets — the exact bug this proposal kills.

## Refresher / dep / key (TRACED)
Self-dep `Deps().Record` calls sit *inside* the gated Put branches → declining the Put declines the dep edge → external RAs never enter the DepTracker → no dirty-marks, no refresher entries, no churn. Cache key unchanged (`ComputeKey` untouched).

## Cost / risk
- Today: 1 cold + N warm hits serving up-to-TTL-stale external data (the defect).
- Proposed: N live external fetches; **internal stages still L3-serve**. Delta = +1 external round-trip + jq CPU per external `/call`. Correct trade — external must be fresh (`feedback_no_park_broken_behind_flag`: this is a correctness fix, not a perf regression).
- RBAC: none — declining a Put only forces a re-resolve (full per-user RBAC re-runs); no leak surface.

## Falsifiers (kind/in-process, NO remote kubeconfig)
(a) external-only RA → 2 identical `/calls` → httptest hit-counter == 2, Get always-misses. (b) internal-only RA → byte-identical + 2nd call is an L1 hit (sink never bumped). (c) mixed RA → outer NOT Put, apiserver stage served from informer with ZERO live apiserver calls (retained advantage). (d) widget with external apiRef → cells #2/#3/#4 all NOT Put. (e) loopback/SA-dispatch/pivot RA → sink Count()==0, still cached (no mis-classification). (f) `-race` on the sink under concurrent multi-stage resolve (shared across the errgroup, like the stage-error sink — `feedback_shared_vs_copy_is_a_concurrency_change`).

---

# INTERNAL HALF — direct-apiserver-path reference + `resolve: true` resolves in-process and stays cacheable

**Mechanism (Diego refinement 2026-06-22):** the internal reference is NOT a `/call?resource=…` loopback. The api-step `path` points **directly at the Kubernetes API path of the RA/widget CR** (e.g. `/apis/templates.krateo.io/v1/namespaces/{ns}/restactions/{name}`, or the `widgets` resource path). A NEW step property `resolve` (default true) controls post-fetch behaviour:
- **`resolve: true`** → snowplow fetches the CR from the **informer** (internal, cacheable, dep-tracked by the normal apiserver-path mechanism) AND runs the fetched object through the snowplow resolver IN-PROCESS (`restactions.Resolve` if it's a RESTAction, `widgets.Resolve` if a Widget), substituting the resolved Status.Raw for the stage output — "as if /call'd", with NO /call HTTP round-trip.
- **`resolve: false`** → return the RAW stored CR (today's plain `objects.Get`/informer-serve behaviour), unresolved.

This SUPERSEDES the `/call`-loopback-formalization framing. The `/call`-loopback (resolve.go:625) stays for back-compat (see #3) but the direct-path+resolve form is the new recommended pattern.

## Current state of in-process resolution (TRACED — build-on)
- RA→RA via `/call?resource=…` loopback already works in-process: `dispatchers.ResolveNestedCall` (nested_call.go:60), RBAC-gated (`checkDispatchRBAC`, nested_call.go:100) + depth-capped (`NestedCallMaxDepth()==8`, nested_call_depth.go:30). It is RESTAction-only (decodes to `v1.RESTAction`, dispatches `restactions.Resolve`, nested_call.go:116/134 — no widget arm).
- A **direct apiserver path** to an RA/widget CR is `parseOK=true` (`ParseAPIServerPathToDep`, inventory.go:251) → it takes the **informer/apistage pivot** (resolve.go:736/784) or the internal-rest-config dispatch (`:852`), returning the **raw CR bytes**. Today that raw CR is fed downstream via `feedBytes(raw)` (resolve.go:797/886) — the stage output is the unresolved CR. So `resolve:false` ≡ today's behaviour exactly.

## #1 — The mechanism: resolve-and-substitute after the cacheable fetch (TRACED insertion point + design)
**Cleanest insertion point: right after the informer/internal-dispatch branch yields `raw`, before `feedBytes(raw)`** (resolve.go:797 for the pivot/informer arm, resolve.go:886 for the internal-rest-config arm). At that point: the CR has been fetched from the cacheable internal path, the dep edge on the RA/widget GVR is already recorded (resolve.go:1254, parseOK=true), and `raw` is the CR envelope bytes.

Insert a `maybeResolveInProcess(gctx, raw, sc.resolve)` step:
1. **Gate on `resolve` (the stage property) being true** AND the cache being on. `resolve:false` → skip, `feedBytes(raw)` unchanged (byte-identical to today).
2. **Sniff the fetched object's kind/apiVersion** from `raw` (a cheap top-level `{apiVersion,kind}` decode — no full unmarshal). If the GVR is RESTAction → run `restactions.Resolve`; if Widget → `widgets.Resolve`; **else (any other kind) → no-op, `feedBytes(raw)` the raw object** (resolve is meaningful only for RA/widget; a `resolve:true` on a configmap path is a harmless raw fetch).
3. **RBAC + depth:** the resolve runs under `gctx`, which already carries the request identity. Run the **same `checkDispatchRBAC` gate** the dispatcher/ResolveNestedCall uses on the fetched object's GVR+namespace before the in-process resolve (the in-process resolve bypasses the per-user apiserver edge — the explicit gate is load-bearing, nested_call.go:22-26). Increment `WithNestedCallDepth` so an RA-that-resolves-an-RA-that-resolves-… chain is bounded at 8.
4. **Substitute:** `feedBytes(encodeResolvedJSON(res))` — the resolved envelope, byte-identical to the HTTP `/call` body (same encoder), instead of the raw CR.

**The IMPORT-CYCLE constraint applies again (TRACED):** the `api` package cannot import `restactions`/`widgets`/`dispatchers` (cycle — nested_call_seam.go:4-12). So `maybeResolveInProcess` must go through a **seam** exactly like the existing `nestedCallResolver` (nested_call_seam.go:67). RECOMMEND: **reuse the existing `ResolveNestedCall` seam** — it already does objects.Get→checkDispatchRBAC→resolve and (with the #2 widget arm below) handles both kinds. The direct-path branch decodes the GVR+name+ns from the already-fetched `raw` (or re-derives the `ObjectReference` from `call.Path` via `ParseAPIServerPathToDep`) and calls `nestedCallResolver(gctx, ref, perPage, page, extras)`. This avoids a second seam and a second RBAC/depth implementation. (Minor: it does a second objects.Get of the same CR — informer-served, cheap; or we add a `ResolveObjectInProcess(ctx, rawBytes)` seam variant that skips the re-fetch, ~10 extra LOC. Recommend the re-fetch form first for minimal surface.)

## #2 — RA→widget arm in the resolve seam (serves the direct-path form)
`ResolveNestedCall` is RESTAction-only today (nested_call.go:116/134). The direct-path `resolve:true` form needs the shared resolve seam to branch on the fetched GVR. **Design:** after `objects.Get` + `checkDispatchRBAC` (both already kind-agnostic), branch on `got.GVR`:
- widget GVR → `widgets.Resolve(innerCtx, widgets.ResolveOptions{In: got.Unstructured, RC: rcFromCtx(innerCtx), AuthnNS, PerPage, Page, Extras})` then `encodeResolvedJSON` — the EXACT shape `widgetsHandler.ServeHTTP` + `resolveWidgetForRefresh` use (resolve_populate.go:529).
- restactions GVR → today's path unchanged.
- else → return the raw object (no-op resolve), consistent with #1 step 2.

Same `innerCtx` depth increment, same RBAC gate (GVR-parameterised, already correct). ~20 LOC. **Loopback disposition (see #4):** the legacy `/call` loopback does NOT route widgets through this arm — it rejects a non-RESTAction GVR with a clear error, since direct-path is the single supported widget path.

## #3 — The `resolve` property: CRD-additive (`spec.api[].resolve *bool`, default true)
- **Attach point:** new field on the `API` struct, `apis/templates/v1/core.go:33` (the step struct; siblings are `Path`, `Verb`, `EndpointRef`, `ContinueOnError`, `ErrorKey`, `ExportJWT`). `Resolve *bool json:"resolve,omitempty"`. CRD-additive (kubebuilder regen).
- **Carry to the resolver (TRACED constraint):** `createRequestOption` (setup.go:57) maps the `API` step into a **plumbing `httpcall.RequestOptions`**, which has NO `resolve` field and is upstream-owned (`feedback_no_special_cases` / cannot patch plumbing). So `resolve` is a **stage-level** snowplow signal carried PARALLEL to `tmp []httpcall.RequestOptions` — as a `resolve bool` field on the snowplow-side `stageCtx` (resolve.go:1225), set once per stage from `ptr.Deref(in.Resolve, true)`. `resolve` is uniform across all iterator items of a stage (it is a property of the step, not the item), so a single bool on `stageCtx` is correct; no per-`tmp[i]` carry needed. ~6 LOC (struct field + the createRequestOptions caller threads `in.Resolve` to the stage build).
- **(b) Default-true backward-compat assessment (TRACED behavioural question):** today a direct GET of an RA/widget CR via an api-step returns the RAW CR (informer-serve → `feedBytes(raw)`, resolve.go:797). With `resolve` default-true, that SAME step would now **resolve** the RA/widget instead of returning it raw — a **behaviour change for any existing RESTAction that fetches an RA/widget CR by its direct apiserver path and consumes the RAW spec.** Whether this breaks anyone is an empirical question about customer RA corpora I cannot answer from source (INFERRED: rare — a RESTAction that GETs another RESTAction's raw CR spec, rather than its resolved output, is an unusual pattern; the common reason to reference another RA is to get its resolved data, which is exactly what `resolve:true` gives). **RECOMMENDATION: default-true is the right ergonomic default, BUT flag the back-compat risk to PM/Diego** — if any shipped RA fetches a raw RA/widget CR by direct path, default-true silently changes its output. Mitigation options: (i) default-true + a release note + an audit of the customer RA corpus (OQ I-1); (ii) default-FALSE for one release (opt-in), flip to true once the corpus is audited clean. I lean (i) if the corpus audit is cheap, else (ii).

## #4 — Relationship to the legacy `/call`-loopback + `RESOLVER_INPROCESS_NESTED_CALL` (recommendation)
**Coexist, do not replace.** The direct-path+`resolve` form becomes the **recommended** pattern (cleaner deps — see #5). The legacy `/call?resource=…` loopback (resolve.go:625) and its `RESOLVER_INPROCESS_NESTED_CALL` kill-switch STAY for **RA→RA back-compat** — existing RAs using the `/call` form keep working unchanged. **The RA→widget arm (#2) serves the DIRECT-PATH form only;** the loopback path is the supported pattern for RA→RA, and direct-path is the single supported pattern for RA→widget — so the loopback path should **reject a non-RESTAction (e.g. widget) GVR with a clear error** (~4 LOC) rather than mis-resolve it as today (it decodes a widget CR as a RESTAction and silently produces garbage). No deprecation in this ship; a future doc note steers authors to the direct-path form. The two forms are non-conflicting: a step's `path` is either a `/call?…` shape (loopback branch) or a direct apiserver path (informer pivot + resolve); never both.

## #5 — Cacheability + dep propagation (TRACED — this is the WIN over the loopback)
- **Referenced RA/widget CR → outer entry: WORKS BY CONSTRUCTION (cleaner than loopback).** The fetch is a **direct apiserver path**, so it is `parseOK=true` at the dep-recording site (resolve.go:1256) and records `Deps().Record(outerL1Key, refGVR, ns, name)` (resolve.go:1260) on the RA/widget GVR. **Editing the referenced RA/widget CR dirty-marks the outer entry** → refresher re-resolves. This is the gap the loopback form had (the loopback `/call?…` path was NOT parseOK, so it recorded no CR edge — old-doc #4 gap); the direct-path form **closes that gap for free** because the path IS an apiserver path. No extra dep-recording code needed for the direct-path form.
- **Nested RA/widget's OWN underlying apiserver data → outer entry (dep propagation, TRACED):** the in-process resolve runs under `gctx`/`innerCtx`, which inherits the outer `WithL1KeyContext` (context.WithValue preserves parent values — verified for `WithNestedCallDepth`, nested_call_depth.go:44). So when the nested RA/widget resolves, ITS inner apiserver calls hit the SAME dep-recording site (resolve.go:1254) reading the **outer** L1 key, and record the nested data deps against the outer entry. **Transitive data invalidation works.** (CAVEAT to verify in dev: the in-process resolve must run under a ctx that still carries `L1KeyFromContext` = the outer key. If the seam re-derives a fresh ctx that drops it, the nested data deps attach to nothing — a falsifier I-4 must assert the nested data dep lands on the outer key. The existing loopback path preserves it via `WithNestedCallDepth(ctx,…)`; the direct-path seam MUST do the same.)

## #6 — Composition with the external half (load-bearing, TRACED)
**Still holds.** A direct apiserver path is `parseOK=true` → it takes branch 2/3 (informer/internal-dispatch, resolve.go:736/852) and `return nil`s the stage **before** the external-fetch bump at resolve.go:927 — it NEVER reaches `:927`, never trips the external-touched sink → the stage's RA **stays cacheable**. `resolve:true` is purely a "also run the resolver on the fetched RA/widget object" step DOWNSTREAM of the cacheable internal fetch; it does not change the branch taken, so it cannot trip the external sink. The external sink's `Count()>0` remains the exact complement of "all stages internal." (A `resolve:true` step whose path is GENUINELY external — non-apiserver URL — reaches `:927`, bumps the sink, AND would have nothing RA/widget-shaped to resolve; that step is external → not cached, consistent with the external half.)

## #7 — Falsifiers, internal half (kind/in-process, NO remote kubeconfig)
- **I-1 resolve:true on a direct restactions path** → stage output == the referenced RA's resolved Status.Raw (byte-identical to an HTTP `/call?resource=restactions` of that RA), resolved IN-PROCESS (assert NO outbound HTTP — httptest hit-count 0 for the inner resolve).
- **I-2 resolve:true on a direct widgets path** → stage output == the referenced widget's resolved output (widget envelope shape).
- **I-3 resolve:false** → stage output == the RAW CR (today's behaviour, byte-identical).
- **I-4 resolve:true on a non-RA/widget GVR** (e.g. a configmap path) → no-op, stage output == raw fetched object (no resolve attempted, no error).
- **I-5 RBAC-gated:** a denied identity on the referenced RA/widget → 403-class error, NOT empty content.
- **I-6 depth-capped:** a cyclic resolve:true chain (RA-A path→RA-B path→RA-A) terminates at depth 8 with the bounded error, no panic.
- **I-7 dep-recorded (the WIN):** resolve an outer RA with a `resolve:true` direct-path ref → assert a dep edge from the outer L1 key to the referenced RA/widget GVR exists (`Deps()` introspection); UPDATE the referenced CR → outer entry dirty-marks (refresher re-resolves). AND assert the nested RA's OWN data deps land on the outer key (dep propagation, #5 caveat).
- **I-8 composition (mutual exclusivity):** (i) RA with a `resolve:true` direct-path internal ref → external-touched sink `Count()==0` → outer result IS cached (L1 hit on 2nd call). (ii) RA with a genuine external fetch → sink `Count()>0` → NOT cached. Proves disjoint composition.
- **I-9 backward-compat:** an existing RA that GETs a raw RA/widget CR by direct path WITHOUT a `resolve` field → under default-true, its output CHANGES from raw-CR to resolved (this is the documented back-compat risk, #3b — the falsifier MAKES the change visible so PM can assess corpus impact).

---

# Combined open questions (Diego / PM)

**External half:**
1. Refresher gate (#5) defense-in-depth even though no path reaches it today? (rec YES, ~3 LOC)
2. widgetContent prewalk (#3): gate the F2 walker so external widgets are never prewarmed (3a, recommended) vs rely on serve-time gate (3b, rejected — still serves TTL-stale).
3. Observability: add `externalSkippedPut` expvar (production falsifier for "did the gate fire?") — rec YES.
4. **SCOPE (Diego):** a `${…}`-templated path that resolves at runtime to an apiserver path is treated as **internal/cacheable** (parseOK after substitution → branch 2/3, never reaches `:927`); only genuinely non-apiserver URLs are external. Confirm this matches intent.

**Internal half (direct-path + `resolve` mechanism):**
> The `resolve`-property-or-not question is **RESOLVED — Diego ratified the property (default true) on 2026-06-22.** It is the essential trigger of the mechanism, not redundant. Remaining OQs are the back-compat surface, the seam form, and the loopback disposition.

5. **I-1 (`resolve` default-true back-compat) — the load-bearing surface to verify, NOT to re-decide.** Default-true is ratified. The residual question is purely empirical: does any existing RA fetch a raw RA/widget CR by its direct apiserver path (whose output would silently flip from raw-CR to RESOLVED)? I cannot answer from source (INFERRED rare — referencing another RA almost always wants its resolved data, which is what `resolve:true` gives). ACTION: PM/tester audit the customer RA corpus + release note; falsifier I-9 makes the behaviour change visible. NOT a blocker to building the mechanism — a corpus audit + note.
6. **I-2 (seam form) — open implementation choice.** Reuse the existing `ResolveNestedCall` seam for the direct-path resolve (does a second informer-served objects.Get of the same CR — cheap) vs add a `ResolveObjectInProcess(ctx, rawBytes)` seam variant that skips the re-fetch (+~10 LOC, avoids the double-Get). Recommend the re-fetch form first (minimal surface); optimise only if the double-Get shows in a profile.
7. **I-3 (dep-propagation constraint — NOT an open choice, a dev instruction).** The direct-path seam MUST run the in-process resolve under a ctx that still carries the outer `L1KeyFromContext`, else the nested RA/widget's OWN data deps attach to nothing. The loopback path preserves it via `WithNestedCallDepth(ctx,…)`; dev must replicate + falsifier I-7 must assert the nested data dep lands on the outer key. Flagged so dev does not drop it.
8. **I-4 (loopback disposition) — recommendation.** Keep the legacy `/call?resource=…` loopback (resolve.go:625) + `RESOLVER_INPROCESS_NESTED_CALL` for **RA→RA back-compat** (existing RAs using the `/call` form keep working). Direct-path+`resolve` is the new recommended form; no deprecation this ship. For the **RA→widget-via-loopback bug** (the loopback path decodes a widget CR as a RESTAction → mis-resolves): since direct-path+`resolve` is now the SUPPORTED widget path, RECOMMEND making the loopback path **reject a non-RESTAction GVR with a clear error** (~4 LOC) rather than build a widget arm into the loopback too — one supported widget path (direct-path), one clear error on the unsupported one. (The RA→widget arm in `ResolveNestedCall`/the resolve seam is still built — it serves the direct-path mechanism; the loopback simply won't route widgets to it.) Confirm.

# Combined LOC bound
- External half: ~40 (clone sink ~15 + bump ~2 + 5 Put-gate sites × ~4).
- Internal half: `resolve` CRD field + stageCtx carry ~6 + resolve-and-substitute seam call at resolve.go:797/886 ~12 + RA→widget arm in nested_call.go ~20 + loopback non-RESTAction reject ~4 = **~42**. (No separate dep-edge fix needed — the direct apiserver path records the CR dep for free, unlike the loopback form.)
- **Unified total: ~82 LOC**, one CRD-additive field (`spec.api[].resolve`), one cloned sink (external half), no cache-key change. (+~10 if the no-re-fetch seam variant is chosen.)
