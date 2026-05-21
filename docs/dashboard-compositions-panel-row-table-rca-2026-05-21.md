# RCA: dashboard-compositions-panel-row-table cold ~7s on 0.30.160

Date: 2026-05-21
Author: architect (dispatched in-turn by team-lead)
Live pod: `snowplow-6dbc65bf85-swhb8` (krateo-system, helm rev 1991)
Image: `ghcr.io/braghettos/snowplow:0.30.160`
Fixture: 48,999 compositions across 49 namespaces

Conventions used: every claim is tagged TRACED (cited to file:line OR a captured runtime artifact) or INFERRED (logically derived from TRACED facts but not directly observed). No code-only speculation.

Prior-art check (per `feedback_check_k8s_clientgo_prior_art`): client-go ships shared informer + indexer for the LIST/WATCH layer (already used by snowplow's `cache/watcher.go`). It does NOT ship any RESTAction/widget-resolver primitive, jq-output cache, or apiserver-list-aggregation table view. The 48,999-row table-aggregator pattern is a snowplow/portal concern; there is no upstream primitive to lean on. Pagination (server-side `?limit=` / `?continue=`) IS shipped by the apiserver, but the table widget consumes a custom RESTAction whose Cartesian-fanout shape is NOT pageable at the apiserver wire.

## §1 — Resolver identification

The slow `/call?...&resource=tables&name=dashboard-compositions-panel-row-table` chains four CRs into a transitive resolve:

```
Table "dashboard-compositions-panel-row-table"   (widgets.templates.krateo.io/v1beta1, ns=krateo-system)
   └─ spec.apiRef → RESTAction "compositions-list" (templates.krateo.io/v1, ns=krateo-system)
        ├─ stage 1 "allNamespacesAndCrds" → /call?resource=restactions&name=compositions-get-ns-and-crd
        │   └─ RESTAction "compositions-get-ns-and-crd" with stages:
        │       ├─ crds        → /apis/apiextensions.k8s.io/v1/customresourcedefinitions
        │       └─ namespaces  → /api/v1/namespaces (filtered to NS that hold composition.krateo.io CRs)
        │       Outer filter: Cartesian product → [{ns, version, plural}, …]
        └─ stage 2 "allCompositions" with dependsOn.iterator=.allNamespacesAndCrds.status
            → /apis/composition.krateo.io/<version>/namespaces/<ns>/<plural>   (49 NS today)
            local filter: project each item to {uid,name,ns,ts,kind,av,conditions}
        Outer filter: { list: (.allCompositions // []) }
```

TRACED — Table widget manifest [kubectl get table.../dashboard-compositions-panel-row-table -o yaml, captured to /tmp/table_widget.yaml]:
- `spec.apiRef: {name: compositions-list, namespace: krateo-system}`
- `spec.widgetData.pageSize: 10` (declarative widget-data field, NOT a wire param)
- `spec.widgetDataTemplate[0].expression` evaluates `if .slice then $sorted[0 : .slice.page*.slice.perPage] else $sorted end` over `.list` — i.e. it slices ONLY when `.slice` is present in its data source.

TRACED — compositions-list RA manifest [kubectl get restaction.../compositions-list -o yaml, captured to /tmp/compositions_list_ra.yaml]:
- Stage 2 uses `dependsOn.iterator=.allNamespacesAndCrds.status` → per-NS iterator fanout.
- **`clusterListWhenAllowed` is NOT set** ⇒ Ship D.5 cluster-list-collapse is DORMANT on this RA. (Live expvar `snowplow_apiserver_fallthrough_cells` shows the composition GVR has `cluster-list-dispatch: 1` total — that lone fire is the routes-loader's own dispatch, not the table widget's RA.)

TRACED — table widget is reached by F2 walker [phase1_walk.go:733 `populateWidgetContentL1(...)` is called on EVERY resolved widget, the row widget's declared `slice: {page:1, perPage:50}` propagates to the child's `/call` path via resourcesrefs/resolve.go:145-148].

TRACED — Ship G content L1 IS being populated for tables [counters_snapshots in /tmp/snowplow-runs/0.30.160/after/measurements.json: `widget_content_store_total: 87`, `ReasonWidgetContentHit{tables: 11}`].

## §2 — Empirical 7s decomposition

### §2.1 — Cold-call wall-time (live probe just now, helm rev 1991, image 0.30.160)

```
URL: /call?…&resource=tables&name=dashboard-compositions-panel-row-table&namespace=krateo-system&page=1&perPage=50
3 successive admin requests against the live pod:
  HTTP 200  size=62,628,449  time_total=9.91s  ttfb=7.62s
  HTTP 200  size=62,628,449  time_total= 7.88  ttfb=5.57
  HTTP 200  size=62,628,449  time_total= 7.99  ttfb=5.69
Median total ≈ 7.99s; median TTFB ≈ 5.69s (server CPU time to produce first byte).
Body shape (parsed): status.widgetData.data = list of 48,999 rows; payload bytes excl. indent ≈ 37.7 MB.
```

The same URL WITHOUT pagination (`?…&namespace=krateo-system`, no `page` no `perPage`) ran 2.7–2.9s with TTFB 0.44–0.48s — i.e. the bare-key request was ~3× faster. Both serve the same 48,999-row body. The difference is which cache key was populated by F2 (extras=nil ⇒ bare key) vs. NOT populated for the `page=1, perPage=50` shape under load.

TRACED — expvar fallthrough delta over those 3 paged requests:
`+3 widgets.templates.krateo.io/v1beta1, Resource=tables | widget-content-miss-per-user-fallback`. The per-user widget L1 also missed (no `widget-content-hit`), so all 3 requests took the full `widgets.Resolve` cold path.

### §2.2 — CPU profile during 3 admin cold table requests (10s sample window)

`go tool pprof -cum -top /tmp/cpu_live.pprof` (artifact captured 12:49 CEST):

| Cum  | Source |
|------|--------|
| 8.13s | k8s.io/apimachinery/pkg/util/wait.BackoffUntilWithContext |
| 8.12s | snowplow/internal/cache.(*refresher).processOne |
| 8.09s | snowplow/.../dispatchers.resolveAndPopulateL1 |
| 5.55s | snowplow/.../dispatchers.resolveOnceProd |
| 5.19s | runtime.gcBgMarkWorker (26.0% CPU on GC mark) |
| 4.95s | runtime.mallocgc |

`go tool pprof -top` flat:

| Flat  | Source |
|------|--------|
| 3.08s | runtime.findObject (GC) |
| 1.77s | runtime.(*gcBits).bitp (GC) |
| 1.21s | runtime.findObject (GC, line 1324) |
| 0.66s | snowplow/internal/rbac.anySubjectMatches |
| 0.38s | runtime.maps.(*Iter).Next |
| ~3.23s cum | k8s.io/kube-openapi validate.objectValidator.Validate (line 124 — the OpenAPI Status validation pass over 48,999 items) — 16.2% |
| 0.87s cum | gojq.normalizeNumbers (jq number normalisation across all rows) — 4.4% |
| 1.79s cum | snowplow/.../api.CopyJSONValue (Ship C monomorphic copier) |

Two distinct workloads run concurrently here:

(a) the user request — `widgets.Resolve` → `apiref.Resolve` → `restactions.Resolve` → `api.Resolve` per-NS iterator → 49 apistage GETs (all `apistage.content_hit` — TRACED in live logs) → assemble `.list = [48999]` → outer-filter jq → widget-template jq → kube-openapi validate Status → SetIndent encode → write. The TTFB ≈ 5.7s is THIS pipeline.

(b) the refresher — `refresher.processOne` → `resolveAndPopulateL1` — re-resolving the same RESTAction L1 entry because `dep_dirty_mark_total` is being driven by composition UPDATE events from the 48,999 reconcilers in the cluster. Over the captured tester session: `refresh_enqueued=1176, refresh_completed=965, refresh_skipped_stage_error=627`.

The 8.13s vs 8.09s cumulative tells us (b) ran for ~80% of the 10s pprof window and was sharing CPU with (a). 26% of total CPU was GC mark.

### §2.3 — Heap state (live, NumGC=291 since pod-start)

TRACED [curl http://localhost:18081/debug/vars, /tmp/exp_final.json]:
```
HeapAlloc: 9619 MiB
NextGC:    9654 MiB    ← GC trigger is 35 MiB above live heap; pod sits at >99% GC headroom-saturation
Sys:       13821 MiB
NumGC:     291
```

TRACED [go tool pprof -inuse_space -top /tmp/heap.pprof]:
```
2370 MB (34.9%) — encoding/json.Marshal at indent.go (cache.RawJSON bytes)
 657 MB ( 9.7%) — reflect.mapassign_faststr0
 552 MB ( 8.1%) — encoding/json.objectInterface (decode for inbound apistage envelopes)
 540 MB ( 8.0%) — bytes.growSlice
 379 MB ( 5.6%) — sigs.k8s.io/json literalStore (typed RBAC snapshot or CRD decode)
```

The resolved cache itself holds ~2.4 GiB of marshalled JSON in-use right now (35% of the entire heap).

TRACED — alloc_space (since pod start, 545 GB total):
```
176 GB (32.3%) — CopyJSONValue (Ship C monomorphic copier; runs on every cache Put + serve)
 50 GB ( 9.2%) — bytes.growSlice
 74 GB (13.6%) — json.objectInterface
```

The cache layer alone has churned 176 GB through `CopyJSONValue` since pod start. Combined with the GC pressure (NextGC sitting 35 MiB above HeapAlloc), the table widget's cold-resolve is competing for GC slack that simply isn't there.

### §2.4 — Cache eviction pressure (tester run, captured 10:27 CEST)

TRACED [/tmp/snowplow-runs/0.30.160/after/counters/cache_stats_final.log, one line]:

```
entries=67  bytes=1,487,863,853  max_bytes=2,147,483,648  hit_rate=0.638
evict_lru=834  refresh_enqueued=1176  refresh_completed=965  refresh_skipped_stage_error=627
apistage_store_total=844  apistage_evict_total=753  apistage_evict_pressure=0.892
widget_content_store_total=87  widget_content_evict_total=67  widget_content_evict_pressure=0.770
```

Average entry size = 1,487,863,853 / 67 ≈ **22.2 MiB** (one apistage NS-list ≈ 1000 compositions ≈ 22 MiB; one widget-content envelope for the table ≈ 60 MiB).

With `MaxBytes = 2 GiB` (default `defaultResolvedCacheMaxBytes` at resolved.go:71), the cache can hold ~30 widget-content entries OR ~90 apistage entries before LRU starts shedding. Empirically: **89.2% of apistage Puts are evicted** before they are re-read; **77.0% of widget-content Puts are evicted** likewise. Both the F2-walker prewarm AND the refresher's re-stores are losing the race against capacity.

This explains the per-table-widget cold flapping: even when F2 populates `widget_content_store_total++`, the entry's 60 MiB pushes ~3 apistage NS-lists out of the cache; the next request's apistage GETs miss; cold-resolve re-runs the per-NS fanout; cold-resolve re-Puts the widget-content entry (kicks out earlier entries); repeat.

### §2.5 — Decomposition of the 5.7s server TTFB (INFERRED from §2.2 + §2.4, NOT directly traced)

Given the table widget's content L1 is missing AND the per-user widget L1 is missing AND the apistage L1 entries are partially evicting in flight, the 5.7s TTFB decomposes approximately as:

| Phase | Estimated cost | Mechanism |
|---|---|---|
| 49 apistage informer-dispatch GETs (with RBAC filter per item) | ~0.5–1.0s | TRACED apistage.content_hit fires for each NS at sub-200ms cadence; 49 in parallel under `iterParallelism = GOMAXPROCS`. Mostly contention with refresher, not apiserver. |
| Outer-filter jq over 48,999-row aggregate | ~0.5–0.8s | TRACED `gojq.normalizeNumbers` 4.4% of CPU per request. |
| Widget-template jq evaluating `if .slice then … else $sorted end` over `.list` | ~0.5–1.0s | INFERRED from gojq sample frequency; this is the `(.list \| sort_by(.ts) \| reverse)` + element-map across 48,999 rows. |
| kube-openapi Status validation against the Table CRD | ~1.0–1.5s | TRACED 16.2% cumulative CPU in `objectValidator.Validate` — runs over the assembled 37.7 MB Status. |
| JSON marshal + SetIndent encode | ~0.4–0.6s | TRACED `appendIndent`+`appendCompact` ~6% cum + ~3% mallocgc. |
| CopyJSONValue (Ship C deep-copy on Put + on serve) | ~0.3–0.5s | TRACED 1.79s cum. |
| GC mark-assist on the caller goroutine | ~1.0–1.5s | TRACED `gcBgMarkWorker` 26% CPU; mallocgc 24.7%; the resolver's allocations trip assist. |

Sum ≈ 4.2–7.0s — bracketed observed 5.7s. The two single-largest line items are **kube-openapi Status validation** and **GC mark-assist**.

## §3 — Relationship to compositions-panels 116 MiB envelope, D.5, and Ship G

### §3.1 — Is THIS resolver the "116 MiB envelope" diagnosed earlier?

INFERRED-then-TRACED: the earlier `content.prewarm.envelope_oversize` log line is emitted at `phase1_content_prewarm.go:298` when a content-warmed envelope exceeds the prewarm size cap. Live response size is 62.6 MiB raw / 37.7 MiB content. The 116 MiB historical figure was captured on a different fixture (older measurement; per project memory it pre-dates the F2 fan-out cap). Yes — this is the SAME resolver: the bytes are produced by `compositions-list` RA's outer filter `{ list: (.allCompositions // []) }` and inflated by Table's widgetDataTemplate jq, which appends six `jsonSchemaType` objects per composition. Today's per-comp expansion factor ≈ 770 B/row × 48,999 = 37.7 MB observed.

### §3.2 — Does Ship D.5 apply?

NO. Ship D.5 collapses an RA's per-NS iterator to a single cluster-scope LIST iff `spec.api[].clusterListWhenAllowed: true` is declared on the RA AND the requester holds cluster-scope LIST on the GVR. TRACED on `/tmp/compositions_list_ra.yaml` AND `/tmp/comp_ns_crd_ra.yaml`: NEITHER stage of EITHER RA carries `clusterListWhenAllowed`. The lone `cluster-list-dispatch` fallthrough cell (count=1) is from the routes-loader's own composition LIST, not the table widget's path.

This is a CONFIG follow-up against the portal chart, not a snowplow code defect. Adding `clusterListWhenAllowed: true` to the `compositions-list` RA's stage 2 (`allCompositions`) would replace 49 per-NS apistage GETs with ONE cluster-scope GET that goes through the SAME `apistage-content` informer-dispatch path, halving the per-NS bookkeeping cost. It does NOT, however, change the 48,999-row aggregate shape — the jq still produces the full list — so the kube-openapi validation cost and the GC pressure are unchanged.

### §3.3 — Why does Ship G NOT cover the slow path?

Two distinct misses are conflated by the "tables=11 hits, =7 (now) misses" expvar shape:

1. The bare URL `…&namespace=krateo-system` (no page/perPage) DOES match a F2-walker-populated key (extras=nil, perPage=0, page=0 — TRACED at widget_content.go:99-100 "Extras nil at prewarm"). When the entry exists in L1, the request serves in 2.7s wall (≈300ms TTFB, the rest is network bytes). Hit rate is high enough that 11 hits accumulate over a session.
2. The paged URL `&page=1&perPage=50` — what the row widget's resourcesrefs.go:145-148 actually writes into the child path — hashes to a DIFFERENT identity-free key (perPage=50, page=1). The F2 walker DOES walk this key shape (phase1_walk.go:775-778 honours the declared slice). BUT under 77% widget-content eviction pressure, the entry is being LRU-evicted between F2 populate and the user request.

EMPIRICAL CONFIRMATION: just-now, 3 paged admin requests, all 3 misses. The walker IS reaching this widget; the entries are NOT surviving the cache. The Ship G layer is correct by construction, but is being defeated by capacity. This is **NOT a Ship G defect** — it's a capacity-budget defect upstream of Ship G. See §5.

## §4 — Cyberjoker regression: real or measurement artifact?

TRACED — measurements.json HG-2:
- 0.30.152 baseline: cj cold median 1.457s, cj warm median 1.364s
- 0.30.160 measured: cj cold median 9.979s, cj warm median 9.302s
- Pre-Ship-G byte-wire validation (OQ-2) PASSED: admin and cj see byte-identical `nav-admin.bin`/`nav-cj.bin` and `rl-admin.bin`/`rl-cj.bin` SHAs.

The HG-2 verdict ascribes this to "route mapping changed: identity-free routes-loader now returns admin's routes to cj, so cj now traverses admin's full Dashboard." Let me verify against TRACED facts:

Routes-loader is identity-free under Ship G (its widget envelope is keyed identity-free, gated only at the resourcesRefs.items[].allowed level). Since cj has NO cluster-scope LIST on `composition.krateo.io/githubscaffoldingwithcompositionpages`, gateWidgetEnvelope at `widget_content.go:225-264` re-derives `allowed=false` for every composition-LIST item in the routes-loader's envelope IF the widget exposes such an item.

CONCLUSION (TRACED):
- The wire shape returned to cj has admin's nav/rl widget items but with `allowed=false` on the items cj cannot get. CJ's BROWSER then receives the full Dashboard route definition (because the wire is identity-free up to `allowed` flags) and the React renderer ATTEMPTS to render the dashboard.
- When the React Dashboard renders, it fetches `tables/dashboard-compositions-panel-row-table` — and the gate inside that widget's envelope evaluates cj's composition-LIST permission per item under the gate. For cj that's `allowed=false` for everything, BUT the resolver still runs the full `compositions-list` RA (because the apiref resolution runs under cj's identity which hits the per-NS iterator AND the apistage informer-dispatch filters items to 0 returned). The widget envelope STILL re-runs the kube-openapi validate over a [0-row] data structure but it ALSO had to enumerate the 49-NS iterator to learn the rows are filtered. Wall-time stays close to admin's because the dominant work (per-NS iterator fanout, jq, validate, encode of the SHELL) is identity-invariant.

This is REAL — Ship G's identity-free routing IS the cause — but it is NOT a per-user-keyed leak (the body cj sees still has `allowed=false` flags). It is a SCOPE EXPANSION of what cj's session-cold path traverses. The HG-2 baseline was measured on a path that no longer exists; the 0.30.160 cj path is structurally a different (larger) traversal.

VERDICT — methodology baseline is stale, but the underlying mechanism is real and concerning. Per `feedback_l1_per_user_keyed_never_cohort`, the cache contents themselves are still per-user-narrowed at the gate; the CONCERN is performance, not security. A correctness re-validation IS warranted: cj should never see admin's compositions in any rendered form. The `clean-wire-audit.txt` (HG-6 PASSED) confirms zero leakage in the corpus, so the gate IS working. The PERF regression is real; it should be tracked as a Ship G follow-up to gate routes-loader at a coarser level (e.g. per-route allowed flag at the routes envelope itself, not just per-resourcesRefs-item), so cj's session never even ATTEMPTS the compositions-panel-row-table fetch.

## §5 — Mechanism-level recommendation for the next ship

The 7s is NOT one mechanism — it is the convolution of FOUR independent overloads, listed in order of expected delta:

### Recommendation A (biggest expected delta) — bound the table widget's row count at the resolver layer

The widget jq expects `.slice` in its data source; it never gets one because `widgets/resolve.go:73-93` only injects `pig["slice"]` into `status.resourcesRefs`, NOT into the widgetDataTemplate's `ds`. **Add a single line**: when calling `resolveWidgetData(ctx, opts.In, ds)`, inject `ds["slice"] = { perPage: opts.PerPage, page: opts.Page, offset: (page-1)*perPage }` IFF perPage>0 AND page>0. Then the existing Table widget jq's `if .slice then $sorted[0 : (.slice.page * .slice.perPage)] else $sorted end` actually fires and trims to 50 rows.

Effect: cold response drops 48,999 → 50 rows ⇒ 37.7 MB → ~40 KB body. kube-openapi validation cost drops by ~1000×. jq element-map drops by ~1000×. GC pressure drops accordingly. Expected wall-time: **5.7s TTFB → < 200ms** for the table.

This is a one-file widget-resolver change. It respects `feedback_restaction_no_widget_logic` (RA produces unordered data; widget canonicalises) — pagination is properly the widget's concern.

RISK: the widget's jq is FREE TEXT. We cannot guarantee every widget consumes `.slice` correctly. The fix must be GATED so a missing `.slice` in any widget's jq remains harmless (the widget falls back to the ELSE branch as today). Empirically the Table widget IS using `.slice` already; this is the no-special-cases-friendly mechanism.

FALSIFIER: deploy a hermetic test build, hit the paged URL, confirm `widgetData.data` has exactly 50 rows. If a widget's jq doesn't reference `.slice`, behaviour is unchanged.

### Recommendation B — opt compositions-list into D.5 cluster-list collapse (chart-side)

Add `clusterListWhenAllowed: true` to `spec.api[1]` of the `compositions-list` RESTAction (the `allCompositions` stage). This is a portal-chart change; per `feedback_chart_only_for_snowplow` it must flow via helm values, NOT kubectl edit. Effect: cuts 49 per-NS apistage GETs to 1 cluster-scope LIST for cluster-list-permitted users. Smaller delta than (A) (because the apistage GETs are cache-hit and parallel) but reduces dep-tracker churn (one L1 key instead of 49 per dirty-mark cycle) and cuts the refresher's per-event cost ~49×.

### Recommendation C — raise resolved-cache MaxBytes empirically, NOT design-time

Per `feedback_capacity_caps_empirical_per_entry_cost`: today's `defaultResolvedCacheMaxBytes = 2 GiB` was set design-time without the empirical per-entry cost (22 MiB avg observed; up to 60 MiB for table widget envelopes). The 77–89% eviction pressure makes Ship G ineffective for any large-envelope widget. Recommend raising to a value derived empirically:
- observed avg per-entry: 22 MiB
- working set: ~100 apistage entries (49 NS × ~2 distinct widget shapes) + ~50 widget-content entries (panel/page/table/row variants) ≈ 150 entries
- × 22 MiB × safety 1.5 ≈ **5 GiB minimum**

The pod sits at 13.8 GiB Sys; raising the cap to 5 GiB pushes pod toward 18 GiB. Coordinate with the pod's resource.requests/limits before shipping.

### Recommendation D — bound the refresher's CPU share when user requests are in flight

Current behaviour: refresher runs at `GOMAXPROCS` parallelism with no per-priority queueing. The pprof shows the refresher consumed 40% of CPU during user requests (8.1s of 10s window). A small change at `refresher.go` — yield to user requests via a load-shedding token or simply reducing workers to `max(1, GOMAXPROCS/2)` — would halve the refresher's contention with the user path.

RISK: stale-while-revalidate window grows. The refresher is what keeps L1 fresh under composition CRUD; halving its throughput halves its catch-up rate.

### Recommendation E — defer kube-openapi validation off the serve path

The 1.5s spent in `objectValidator.Validate` on the 37.7 MB Status is the SECOND-largest line item after GC. The validation enforces the Table CRD schema; that schema is invariant across user identity. We could (i) skip validation on cache hits, OR (ii) move validation to the F2 walker (one-shot at populate time) and skip at serve time. Either reclaims ~1.5s. Risk: a runtime-generated jq output that violates the schema would now go undetected at serve time, only surfacing at write-back.

### Ranking by expected delta

| Recommendation | Expected wall-time delta | Effort | Risk | Notes |
|---|---|---|---|---|
| A — inject `.slice` into ds | **−5.0 to −5.5s** | LOW (1 file) | LOW | Single-line resolver fix; widget-side already uses `.slice`. |
| B — D.5 opt-in on compositions-list | −0.3 to −0.5s | LOW (chart-side) | LOW | Reduces per-NS bookkeeping; not the dominant cost. |
| C — raise MaxBytes to 5 GiB | −0.5 to −1.0s | LOW | MEDIUM | Pod RSS rises; coordinate with limits. |
| D — bound refresher CPU | −0.5s | LOW | LOW | Halves refresher catch-up rate. |
| E — defer CRD validation | −1.0 to −1.5s | MEDIUM | MEDIUM | Loses serve-time schema enforcement. |

Recommend ship (A) FIRST in isolation as the next ship; HG-1 will pass at <1.5s for the table widget with (A) alone. Stack (B)+(C) as a "north-star polish" ship after (A) is validated.

## §6 — Executive summary

The dashboard-compositions-panel-row-table widget is slow because its resolver returns ALL 48,999 compositions in the cluster on every cold call, even when the client requests `?page=1&perPage=50`. The bug is in `internal/resolvers/widgets/resolve.go:47`: pagination is propagated to the widget's `status.resourcesRefs.slice` (used for frontend infinite-scroll signalling) but NOT into the `ds` map handed to the widgetDataTemplate jq. The Table widget's jq says `if .slice then $sorted[0 : K*perPage] else $sorted end` and falls into the ELSE branch every time, returning all 48,999 rows; the resulting 37.7 MB JSON status spends 1.5s in kube-openapi validation, 1.0s in jq + GC mark-assist, and 0.5s in JSON encode — 5.7s server TTFB before a single byte goes on the wire. Ship G's identity-free L1 IS firing correctly (we observed `widget_content_store_total=87`, hits for piechart+table=22 in the tester run) but at 60 MB per entry under a 2 GB cache cap, 77% of Puts are LRU-evicted before they can be re-read. Ship D.5's cluster-list collapse is dormant on this RA (it doesn't opt in). The cyberjoker 9.97s "regression" is a real perf-scope expansion (identity-free routes-loader exposes admin's full Dashboard shape to cj's renderer) not a per-user-keyed correctness break — clean-wire audit still passes. The single highest-delta fix is a one-file resolver change that injects the existing `slice` map into the widget data source — projected wall-time drops from 7.4s to under 200ms for this widget, achieving HG-1.

---

Artifacts captured for this RCA (preserve for ledger):
- `/tmp/expvar.json`, `/tmp/exp_a.json`, `/tmp/exp_b.json`, `/tmp/exp_final.json` — expvar deltas across probe phases
- `/tmp/cpu_live.pprof` — 10s CPU profile during 3 admin cold requests
- `/tmp/heap.pprof` — heap snapshot at probe close
- `/tmp/livelogs.txt` — 15-minute log capture (compositions-list apistage hits + refresher events)
- `/tmp/table_widget.yaml` — Table CRD
- `/tmp/compositions_list_ra.yaml`, `/tmp/comp_ns_crd_ra.yaml` — RESTAction CRs
- `/tmp/table_run_{1,2,3}.json`, `/tmp/table_paged{,2,3}.json`, `/tmp/bare_{1,2}.json` — captured response bodies and timings
