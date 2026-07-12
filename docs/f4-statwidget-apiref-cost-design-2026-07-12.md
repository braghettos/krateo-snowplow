# F4 — statistics/tag-widget apiRef seed cost (the #130 binding constraint)

Date: 2026-07-12
Author: cache-architect
Artifacts: `/tmp/f3br2-deploy/boot-1.7.8-t0.log` (12578 lines), `/tmp/f3br2-deploy/acceptance-summary-1.7.8.txt`
Cluster (read-only diagnostic): `gke_operations-dev-krateo-io_europe-west3_krateo-installer-test`, pod `snowplow-85766bd5cd-vzqnn` (image 1.7.8, krateo-system)
Repo: main = ship line, 1.7.8 = merge b365223

## Headline

The ~71 slow nav widgets are **NOT** class (a) live-apiserver, **NOT** class (b) external HTTP, **NOT** class (c) nested-composition fan-out, **NOT** (d)-as-guessed. The mechanism is a single class:

**Per-widget re-execution of an expensive multi-pass `gojq` filter over the whole 60,000-object `benchapps.composition.krateo.io` GVR, run TWICE per widget seed (Ship-4a unpaginated + paged double-resolve), across ~71 widgets that all share one of two composition-listing RESTActions.** The apiserver/informer bytes are already cheap (informer-served, all content HITs); the cost is CPU spent in jq over a 60K-item array. This is a **jq-over-large-list cost class**, closest to (d), and it is fully informer-servable-substrate — the informer is not the problem, the **repeated jq materialization** is.

Split: **all ~71 are the same class** (jq over the shared 60K composition list). **0 are external/(b)** — #129's TTL-external-cache lever does NOT apply here.

## Root cause (TRACED)

### 1. What the slow widgets are and where their cost lands

Slow-widget inventory (`apiref_ms > 3s`, from `phase1.seed.widget.phase.timing`, boot log):

| widget | kind | apiRef → RA | count (incl re-walks) |
|---|---|---|---|
| stat-compositions / stat-healthy / stat-reconciles / stat-failed | Statistic | `compositions-list` | 43 |
| delta-tag-reconciles / -healthy / -failed | Tag | `compositions-list` | 30 |
| delta-tag-compositions | Tag | `dashboard-data` | 3 |
| greeting-subtitle | Paragraph | (composition list) | 2 |

All apiRefs resolve to `compositions-list` or `dashboard-data`, both in krateo-system (`kubectl get statistics/tags -o json`, apiRef field). Both RAs have the identical hot step:

```
compositions-list.spec.api[1]: name=allCompositions verb=GET
  path=${ "/apis/composition.krateo.io/" + (.version) + "/" + (.plural) }   endpointRef=None
dashboard-data.spec.api[1]:    name=allCompositions  (identical shape)
```

`allCompositions` fans over the 29 `composition.krateo.io` CRDs (`kubectl get crds`, group filter). Of those GVRs, **`benchapps` holds 60,000 objects** (`kubectl get benchapps -A | wc -l` = 60000). The other 28 hold 0–1 each.

### 2. The cost is jq, not I/O — proven by the per-stage timing breakdown

`phase1.seed.widget.timing` (emitted at `internal/handlers/dispatchers/phase1_pip_seed.go:910`, wrapping the full `widgets.Resolve` at :928; per-api-stage entries from the timing sink installed at :906/:921) for `stat-reconciles`:

```
elapsed_ms_total: 23550   stages_total: 4     (crds, allCompositions, crds, allCompositions — the stage list appears TWICE)
  stage=allCompositions el_ms=2807 iter_calls=29 iter_ms=2806 content_hits=29 content_misses=0  disp_ms=0 parse_ms=0 put_ms=0
  stage=allCompositions el_ms=2372 iter_calls=29 iter_ms=2371 content_hits=29 content_misses=0  disp_ms=0 parse_ms=0 put_ms=0
```

Key reads:
- **`content_hits=29, content_misses=0`** — every per-GVR LIST (including the 60K benchapps LIST) is served from the synced informer, NOT the live apiserver. Corroborated by `internal_dispatch.list.informer_served` (208 over the boot; `cache.streaming_list.completed items=60000 pages=1` at 07:53:36) and `internal_dispatch.list.serve_miss` = only 2 total. So the **apiserver-read hypothesis (a) is FALSIFIED**: reads are already informer-served.
- **`endpointRef=None`** on both hot steps — no external HTTP backend. **Class (b) is FALSIFIED** for these widgets: `compositions-list`/`dashboard-data` never touch ClickHouse/observability. #129's external-TTL-cache lever is IRRELEVANT here.
- **Stage-sum ≈ 5.2s but `elapsed_ms_total` = 23.5s.** The missing ~18s is spent OUTSIDE the per-stage instrumentation — i.e. in the **RESTAction-level `spec.filter` jq** (`internal/resolvers/restactions/restactions.go:63-70`, `jqutil.Eval` over the accumulated dict) and the widget's `widgetDataTemplate` jq (`widget_data_ms`=1800 captures only part; the RA-filter is un-instrumented). Neither is an I/O phase; both are pure gojq CPU over the 60K-item array.

### 3. Why the RA filter is ~18s of CPU

`compositions-list.spec.filter` (read live) makes ~8 full passes over `.allCompositions` (the 60K array):
- `map(select(project match))` — 60K
- `map(. + {state, railState, healthPercent})` — 60K, evaluates `any(conditions[]?...)` per item
- `map(select(date-window))` — 60K, `fromdateiso8601` per item
- `map(select(status/q match))` for `filtered` — 60K
- 4× `map(select(...)) | length` for `counts` — 4×60K
- emits `list: $all` = the FULL 60K enriched array back to the widget

The widget then runs `widgetDataTemplate` jq (`stat-reconciles`: `[.list[] | {...}] | unique` and `[.list[]|select(...)]|length`) over that same 60K array. So the pipeline is roughly **~10 full 60K-array gojq passes per resolve** — measured ~18-21s of CPU.

### 4. Why it runs TWICE per widget (the 2× in `stages_total:4`)

The widget seed path resolves the apiRef twice:
1. `widgets.Resolve` (paged, per-user cell) — `phase1_pip_seed.go:928`.
2. `seedRAFullListForWidget` (unpaginated full-list pin, Ship-4a) — `phase1_pip_seed.go:983` → `apiref.Resolve` → `raFullListServe` → `resolveRA(ctx, 0, 0)` unpaginated (`apiref/resolve.go:167-177`).

Both re-run the whole `allCompositions` + filter pipeline. Hence two `allCompositions` stages and ~2× the jq cost. The unpaginated full-list resolve materializes and filters all 60K rows specifically to establish the sliceability verdict — for a Statistic/Tag widget that only needs a scalar count, this is pure waste.

### 5. Why the milestone fails and the post-Ready rewalk churn (secondary item)

`prewarm.engine.boot.rewalk_complete ... widgets:186 elapsed_ms=317963`. Summing `apiref_ms` for slow widgets ≈ **647s of jq (unique) — 1295s counting re-walk dupes** — against a 480s (`pipGlobalTimeout`) budget (`phase1_pip_seed.go:141`). No seed ORDER fits ~647s into 480s → `prewarm.engine.scope_incomplete err="context deadline exceeded"` (08:04:00) → F.4 `scope_requeued attempt:1` → 317s rewalk → cut again (08:12) → attempt:2. The rewalk churn is **NOT a separate defect** — it is this same per-target-cost overrun re-entering via the F.4 boot-resume, `operational_failure=0` confirming no error trigger. It disappears when the per-widget jq cost is cut below budget.

**Falsifier basis:** the smoking gun is `content_hits=29/content_misses=0` (I/O cheap) with `elapsed_ms_total`≫stage-sum (jq expensive) — reproducible on any boot log. A hand-curl probe would MASK this (it wouldn't exercise the seed's double-resolve nor the 71-widget fan-out) — mechanism traced against the runtime log + live CR shapes, not constructed.

## Prior art check

- client-go / informer: already used (reads are informer-served). No further client-go lever — the bottleneck is downstream of the informer, in jq.
- Ship-4a RAFullList (`apiref/resolve.go`) already exists to share a page-independent full list ACROSS pages+widgets — but its *serve* is a Go-slice, while its *first-sight* still runs the full RA filter per (widget×cohort), and Statistic/Tag widgets are unpaginated so 4a's slice-serve never engages for them.
- `#121/#122` compile-jq-once covers the refilter/step path (`refilter_compile_once_falsifier_test.go`), NOT the RA-level `spec.filter` at `restactions.go:65`. Compilation is not the cost anyway — execution over 60K is.

## Options (ranked, PM-gateable)

### Option 1 — Shared RA-resolve memo across widgets (RECOMMENDED)

The 71 widgets re-resolve the SAME two RAs (`compositions-list`, `dashboard-data`) at the SAME identity (boot SA / cohort) with the SAME empty/near-empty extras. The RA `allCompositions`+filter output is **identical** across all Statistic/Tag widgets sharing an apiRef — they differ only in the cheap `widgetDataTemplate` jq applied afterward. Today each widget re-runs the whole 18s filter from scratch.

**Design:** within a single seed pass, memoize the resolved RA `Status` (post-filter dict) keyed by (RA namespace/name, identity/cohort key, effective-extras hash, page/perPage). First widget pays the ~18s; the other ~70 sharing that apiRef hit the memo (~0). Place the memo at the `apiref.Resolve` seam (`internal/resolvers/widgets/apiref/resolve.go:78`) or one level down at `restactions.Resolve` entry, scoped to the seed context (a `context`-carried `sync.Map`, torn down at pass end — never a process-global that could leak identity across users).

- Effect on symptom: 71×18s → ~2×18s (one per shared RA) + 71× cheap widgetDataTemplate. ~636s → ~40s. Fits 480s with wide margin. Rewalk churn disappears (same lever).
- Cost bound: one memo entry per (RA,identity,extras,page) per pass; bounded by RA-count not object-count. Torn down per pass.
- RBAC safety: the memo key MUST include the full identity/cohort key (per `feedback_l1_per_user_keyed_never_cohort`) — the seed already resolves per-cohort, so the memo is per-cohort-identity, never cross-user. This is the load-bearing safety condition.
- LOC: ~40-70 (memo struct + context accessor + key derivation reusing existing coordinate derivation + wrap at one seam).
- Also collapses the Ship-4a double-resolve within a pass (the unpaginated + paged resolves of the same RA/identity share the memo IF page is part of the key and the unpaginated result can serve the paged slice — needs care; simplest correct version keys page in, giving 2× not 1×, still ~40s).

### Option 2 — Skip the unpaginated Ship-4a full-list pin for count-only (unpaginated-consuming) widgets

The `seedRAFullListForWidget` unpaginated resolve (`phase1_pip_seed.go:983`) exists to pin a sliceable full-list for PAGINATED /call serves. Statistic/Tag widgets are unpaginated (they consume `list:` wholesale for a count) — they gain nothing from the 4a slice cache but pay the second full 18s materialization.

**Design:** skip `seedRAFullListForWidget` when the widget resolve is unpaginated (page/perPage == 0), reusing the EXISTING `shouldServeRAFullList` predicate shape (`apiref/resolve.go:68`, already gates on `perPage>0 && page>0`). Data-derived (reads pagination, not widget-kind) → satisfies `feedback_no_special_cases`.

- Effect: halves the slow-widget cost (~647s → ~324s). Alone, still over 480s — insufficient, but a clean ~30% cut and a natural companion to Option 1.
- LOC: ~10.

### Option 3 — Parallelism within the seed budget

The seed is effectively serialized: `enterSeedUnit` (`phase1_pip_seed.go:881`, `seed_bound.go`) serializes against GOMEMLIMIT headroom. On this pod NO SEED/FOOTPRINT/GOMEMLIMIT env is set (verified: pod env has none) so the adaptive gate is transparent — but the cohort fan-out width still bounds concurrency, and 647s of CPU on a 4-CPU pod (limits.cpu=4) cannot parallelize below ~160s regardless.

**Design:** widen seed cohort concurrency. **Rejected as primary** — 647s/4cpu ≈ 162s floor is under 480s but leaves no margin, wastes the whole pod's CPU on redundant work, and does nothing for the real waste (the same list filtered 71×). Parallelism should come AFTER Option 1 removes the redundancy, not instead of it.

### Option 4 — (b)-class external TTL cache (#129)

**Not applicable.** Traced: these widgets have `endpointRef=None`, are informer-served, touch no external backend. Keep #129 parked for the genuinely-external observability widgets; it will not move this needle.

## Recommendation

**Option 1 (shared RA-resolve memo), with Option 2 folded in** as the companion cut. Option 1 attacks the actual waste (71 identical 18s filters → ~1-2), Option 2 removes the redundant unpaginated pin for count-only widgets. Together: ~647s → ~20-40s, comfortably inside 480s, and the post-Ready rewalk churn resolves as a consequence (same root cause). Do NOT reach for Option 3/4.

### PROJECTION CORRECTION (PM gate C-F4-1, 2026-07-12 — supersedes the "~20-40s" / "~40s" figures above)

The "~40s" figures in §95, §99, §122 are a **single-cohort** projection (§91 states the premise: "the SAME identity (boot SA / cohort)"). The memo key correctly folds the full RBAC identity (username + sorted groups — C-F4-4), so it **cannot** collapse resolves across cohorts. The honest **aggregate** residual is therefore per-cohort:

    residual ≈ #cohorts × #distinct-heavy-RAs × ~18s  ≈  5 × 2 × 18  ≈  ~180s

- `#distinct-heavy-RAs` ≈ 2 (`compositions-list`, `dashboard-data`).
- `#cohorts` is **data-derived** (the count of distinct dedup-collapsed identity ranks the seed enumerates at 60K) — a **tester on-cluster confirm**, not a code constant. ~5 is the 1.7.8-observed order of magnitude; the projection scales linearly with it. If the live cohort count is materially >5, re-check the 480s fit.
- ~180s still fits the 480s budget with margin, so the milestone (latch fires via segment-complete, `scope_incomplete=0`, seed hits >0) holds. Option 2 additionally removes the second (4a-pin) 18s resolve per unpaginated widget, so the per-cohort cost is ~(2 RAs × 18s) not ~(2 RAs × 2 passes × 18s).

Acceptance is measured against **~180s aggregate residual**, NOT the single-cohort ~40s. The tester confirms the actual cohort count on-cluster and re-derives the fit.

Strategic choice to surface to Diego / TL: **memo scope**. Recommended = per-seed-pass context-carried memo (torn down each pass, per-cohort-identity keyed). A longer-lived memo (survive across passes) would help the rewalk even more but raises the cross-user-leak and staleness surface — recommend NOT going there until per-pass is proven.

## Falsifier (proves the fix works — or didn't)

1. **Primary (symptom):** re-boot 1.7.x+fix on installer-test, grep `phase1.seed.widget.phase.timing`: the 2nd..71st slow widget's `apiref_ms` drops from 6-24s to <500ms (memo hit); first-per-RA stays ~18s. Sum of slow `apiref_ms` < 480s.
2. **Milestone:** `prewarm.first_nav.latch` line FIRES (>0), `prewarm.engine.scope_incomplete` = 0, Ready via segment-complete not backstop, `hit_source` seed-attributable > 0.
3. **Rewalk:** zero `scope_requeued` for the boot scope over 20min.
4. **RBAC safety (hermetic, gate-blocking):** two cohorts with divergent RBAC resolving the same shared-apiRef widget get DIFFERENT memo entries (key includes identity); a RED arm that drops identity from the memo key must leak cohort-A's filtered list into cohort-B and FAIL the test. This is the load-bearing arm per `feedback_l1_per_user_keyed_never_cohort`.
5. **Correctness:** memo-hit widget output == cold-resolve widget output (byte-identical widgetData) for the same (RA,identity,page).
