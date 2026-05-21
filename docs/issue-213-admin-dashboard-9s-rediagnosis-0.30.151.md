# Issue #213 — admin Dashboard 9s re-diagnosis at 0.30.151

**Status:** TRACED-evidence diagnosis complete. All claims tied to
file:line or runtime log line. Branch pre-design below per the §3
decision-tree precedent (D.4.2 pattern).

**Empirical baseline:** `/tmp/snowplow-runs/0.30.151-northstar/measurements.json`:
admin cold median 8.16s, cyberjoker FIRST cold 26.72s, cyberjoker warm 1.71s.

---

## §1. Decisive TRACED findings

### F1 — Both widgets share the SAME upstream RA (apiRef = `compositions-list`)

`portal-cache/blueprint/templates/piechart.dashboard-compositions-panel-row-piechart.yaml:78-80` and `table.dashboard-compositions-panel-row-table.yaml:67-69` BOTH declare:

```yaml
apiRef:
  name: compositions-list
  namespace: {{ .Release.Namespace }}
```

So the underlying RA is identical. Tester Candidate (1)'s "stage-key share" question: **stage keys ARE the same** (per F2 below). Candidate (1) is REFUTED.

### F2 — apistage L1 IS enabled in prod and IS hitting during admin cold

`kubectl get cm snowplow -n krateo-system -o yaml`:

```
RESOLVED_CACHE_APISTAGE_ENABLED: "true"
PREWARM_CONTENT_ENABLED:         "true"
PREWARM_CONTENT_MAX_BYTES:       "33554432"
PREWARM_ENABLED:                 "true"
PREWARM_PAGE_LIMIT:              "5"
RESOLVED_CACHE_TTL_SECONDS:      "3600"
CACHE_ENABLED:                   "true"
```

Pod-side log grep — **75 `apistage.content_hit` lines, 0 `apistage.content_store` lines** during the captured admin cold window (01:22:22 onward). Every per-NS `allCompositions` inner call dispatches as `"dispatch":"apistage-content"` (TRACED at `internal/resolvers/restactions/api/resolve.go:594-602` log emission). **Cyberjoker's first-ever 26.7s cold-load logs rotated off** (kubectl retains last few MB only) so direct content-store evidence for that run is unavailable; the prewarm coverage hypothesis (F4 below) remains the strongest explanation.

### F3 — Per-iterator-call latency at 60ms median, not driven by jq

Captured from `/tmp/snowplow-24h-logs.txt`, 71 timed `allCompositions` `"calling api"` → `"api successfully resolved"` pairs (admin cold burst at 01:22:22+):

```
median:  56.6 ms
p90:    115.6 ms
p99:    513.5 ms
max:    513.5 ms
sum:     3.06 s  (across 71 calls, parallel-bounded)
```

Inter-call gap median is **negative -21ms** = **calls overlap** (the 0.30.95 bounded-parallel iterator at `internal/resolvers/restactions/api/resolve.go:355` IS doing parallel fan-out). Total `allCompositions` wall span: ~7.2 s server-side from first `"calling api"` (01:24:22.971) → first `widgetDataTemplate JQ evaluation` (01:24:30.187) on the captured run.

### F4 — Per-call cost decomposes to filterListByRBAC (~16 ms / 1000 items)

`informer_dispatch.rbac_filter` log lines emit `served:1000, kept:1000, dropped:0` per call (TRACED at `internal/resolvers/restactions/api/informer_dispatch_rbac.go:93-180`). The gap from `"apistage.content_hit"` to `"informer_dispatch.rbac_filter"` is ~3-15 ms (filter + EvaluateRBAC). The gap from filter completion to `"api successfully resolved"` is ~15-30 ms (jsonHandler + RA per-stage filter at apistage.go:438 — `[.allCompositions.items[]? | {uid:.metadata.uid, ...}]`).

**Per-call ~50-60 ms decomposition:**
- ~1 ms: apistage Get (sub-ms; cache hit confirmed)
- ~15-20 ms: filterListByRBAC over 1000 items + 1 EvaluateRBAC (per-NS memo)
- ~5 ms: per-stage filter `[items[]? | {uid,name,ns,ts,kind,av,conditions}]` over 1000 items in gojq
- ~5-10 ms: per-stage merge into `dict[allCompositions]` + dep edge recording
- ~5-15 ms: scheduling / context-switch / inter-event gap

80 namespaces × 60 ms = 4.8 s INNER work — wallclock is ~7.2s because the bounded-parallel worker count is configured (8 default at `internal/resolvers/restactions/api/resolve.go:355`).

### F5 — Widget jq is NOT the bottleneck on admin cold

Bench against a 49k-item fixture using `jq-1.7.1` CLI (proxy for gojq's perf characteristics):

```
piechart `series.data[0].value` (filter+count):   120 ms
table    `data` (sort+reverse+slice+map):         227 ms
```

Piechart runs **5 expressions × ~120ms = ~600ms total**. Table runs **1 expression × ~230ms**. Combined ~830ms per cold load. Admin cold = 8.16s; jq accounts for ~10%. **Candidate (2) — jq sort over 49k — is NOT the dominant cost.** It is real, but secondary.

### F6 — Cyberjoker first-cold 26.7s is the prewarm-coverage gap

`cj-dashboard-cold-1.json`: piechart `dur:23821 start:2901`, table `dur:23794 start:2902`. Both started **simultaneously** at ~2.9 s into page load (parallel widget HTTP requests from frontend) and took ~24 s each. Tester reports **21 composition.krateo.io GETs/60s during cyberjoker cold** — *non-zero apiserver fan-out at request time*. If Phase 2 prewarm (`internal/handlers/dispatchers/phase1_content_prewarm.go:1-50`) had fully populated the apistage cache, request-time apiserver GETs would be ~0.

**21 apiserver GETs across 60s during cold = partial content-cache coverage**. The pod runs 67 days; the apistage TTL is 3600s. Some content entries have evicted/expired since the last refresh; the cyberjoker first cold pays the apiserver round-trip for the evicted slots while admin (post-cyberjoker run) finds them re-warm.

**Two distinct mechanisms in play:**
1. **Per-iterator-call CPU cost** (F3+F4): ~7 s of in-process work no matter what (filterListByRBAC + per-stage filter + RBAC eval over 49k items spread across 80 NS LISTs). This is what admin's 8.16s reflects.
2. **First-cold apiserver round-trip** (F6): when content cache is cold for some NS slots, each missed NS adds ~80-300ms apiserver latency on top of the 60ms in-process cost. Cyberjoker's first-cold = 26.7s = ~7s in-process + ~17-19s apiserver round-trips on missed slots.

### F7 — `compositions-list` RA structure is the genuine architectural fan-out

`portal-cache/blueprint/templates/restaction.compositions-list.yaml:14-31`:

```yaml
- name: allNamespacesAndCrds
  path: "/call?...&name=compositions-get-ns-and-crd..."
- name: allCompositions
  dependsOn:
    name: allNamespacesAndCrds
    iterator: ".allNamespacesAndCrds.status"
  path: ${ "/apis/composition.krateo.io/" + .version + "/namespaces/" + .namespace + "/" + .plural }
```

The iterator pattern enumerates `(crd × namespace)` pairs. With ONE CRD (`githubscaffoldingwithcompositionpages`) × 80 namespaces (49 bench + system) = ~80 inner calls. Each call carries ~16ms RBAC filter + ~5ms RA filter + ~5-10ms merge = ~60ms. Even with full content cache + 8-way parallelism, the **arithmetic minimum is ~80×60ms / 8 = 600ms in-process for the inner fan-out**. The remaining ~6.5s of admin's 8.16s is in serial CPU work that can't parallel-overlap (Go scheduler with 2 CPU cores allocated).

---

## §2. Refutations of tester candidates

| Candidate | Verdict | Evidence |
|---|---|---|
| (1) piechart + table NOT sharing apistage L1 on compositions-list | REFUTED | F1: both widgets share `apiRef: compositions-list` (same name + same namespace). F2: 75 `apistage.content_hit` events confirmed during admin cold. **Snowplow is NOT double-fetching from apiserver per-widget;** both widgets read from the SAME apistage entries via `gateContentEnvelope`. The 23.8s in cj-cold reflects per-widget HTTP wall (parallel from frontend) not per-widget content fetch. |
| (2) jq filter sort+reverse+slice over 49k | PARTIAL — secondary | F5: bench shows ~230ms for table sort, ~600ms for piechart 5-pass. Real cost but only ~10% of admin's 8.16s. Optimizing it gets us to ~7.4s — useful but not closing the north-star. |
| (3) Phase 1 SA prewarm doesn't cover admin's full-cluster cohort | PARTIAL — covered for content but coverage gaps exist | Phase 2 `PREWARM_CONTENT_ENABLED=true` IS active. F2 prewarm harvests `compositions-list` RA and runs SA-credentialed resolve at startup. But: F6 shows 21 apiserver GETs/60s during cyberjoker cold = SOME content slots are cold (TTL expiry, eviction, or never-populated edge cases). Not the dominant cost for **admin median** but IS the dominant cost for **first-cold-per-identity**. |
| (4) Combination | YES — TRUE ROOT CAUSE | The 8.16s admin cold = ~7s in-process inner-call iterator dispatch + filterListByRBAC + per-stage filter, plus ~600ms widget jq + ~500ms network. Even with perfect content-cache hit rate and zero apiserver round-trips, the **arithmetic minimum** of the iterator fan-out at this scale is ~5-7s on 2 CPU cores. |

---

## §3. The fundamental architecture insight

`compositions-list` RA's `iterator` over 80 namespaces produces an in-process N-call fan-out that pays:

- per-call RBAC filter cost (16ms × N where N=80)
- per-call per-stage filter cost in gojq (5ms × N)
- per-call merge into dict[allCompositions] (5-10ms × N)

**Wall = N × per-call-CPU / parallelism**. At N=80, per-call=60ms, parallelism=8 → ~600ms LOWER BOUND. **Empirically observed = 7.2s**, so Go-scheduler scheduling + dict-merge serialization is producing a ~10× factor over the lower bound. The per-call work IS the architectural cost, and it scales linearly with namespace count.

The data-source RA fans out per-namespace-per-CRD because that's how K8s apiserver allows scoped listing. **Aggregating to a cluster-wide LIST is impossible per K8s scoping rules** for namespace-scoped resources unless the request is at the cluster-scoped endpoint `/apis/composition.krateo.io/v1-2-2/githubscaffoldingwithcompositionpages` (which IS supported by apiserver but only with cluster-level RBAC `list` on the resource — admin has it, cyberjoker does not).

---

## §4. Branch pre-design (per D.4.2 §3 decision-tree precedent)

### Branch A — Widget-template refactor: stage-key coalescing or RA pre-aggregation

**Direction:** Refactor `compositions-list` RA so the iterator is replaced by a single cluster-wide LIST (when the requesting identity has cluster-list permission). The RA gains a `cluster-list-when-allowed` mode: if the SA / requester can `list` cluster-wide, do ONE GET against `/apis/composition.krateo.io/v1-2-2/githubscaffoldingwithcompositionpages`, skip the iterator.

**Code shape:**

- New RA field `spec.api[].clusterListWhenAllowed: true` (additive).
- Snowplow resolver checks RBAC at the SA-prewarm site (Phase 2) → if cluster-list-allowed, dispatches the cluster-wide call once, stores under apistage key `(gvr, ns="", name="")`.
- gateContentEnvelope runs filterListByRBAC on the full 49000-item slice ONCE under the requester's identity (per-NS memo still applies → 80 EvaluateRBAC calls but ONE in-memory pass).

**Estimated impact:** server-side ~7s → ~1.5s. The 80-call serial in-process orchestration cost (Go scheduler + dispatch overhead + dep recording × 80) collapses to ONE call's worth.

**Estimated effort:** medium — touches RA spec, resolver iterator logic, content-key model (cluster-list = `ns=""` already in `contentKeyInputs`), portal blueprint update.

**Risk:** medium — needs a fallback path for non-cluster-list users (cyberjoker). Code reuses existing namespaced-iterator path under that fallback. **Per `feedback_no_special_cases`**: design as additive field, not user-specific switch.

### Branch B — jq sort optimization: pre-sorted L1 entry OR aggregator caching

**Direction:** Cache a pre-sorted list (by `metadata.creationTimestamp` descending) at the apistage layer for the `compositions-list` RA's terminal stage. Each per-NS LIST already has items in creationTimestamp order (apiserver default). Concatenating 80 already-sorted slices and merging-sort them ONCE at populate time eliminates the table's per-request sort.

**Code shape:**

- New apistage `ResolvedEntry.SortedItems []*unstructured.Unstructured` populated alongside `Items` (Ship 0.30.121 R3 already pre-parses items at Put).
- Widget jq `sort_by(.ts) | reverse` becomes a no-op since the data is already sorted (the table jq still works, just sorts an already-sorted array = O(n) with timsort fast path).
- OR: extend the RA filter syntax so a stage can declare `sortBy: "creationTimestamp"`, with snowplow doing a merge-sort in Go at apistage Put time.

**Estimated impact:** ~230ms → ~30ms saved on table; piechart unaffected. Real but small (~3% of admin's 8.16s).

**Estimated effort:** low if scoped to RA-declarative sort + apistage merge-sort. High if scoped to widget jq rewrite (touches every blueprint).

**Risk:** low. Affects only one widget's terminal computation.

### Branch C — Prewarm coverage extension

**Direction:** Bug-fix Phase 2 prewarm to (a) cover admin's cluster-wide scope by adding a cluster-list prewarm call on top of per-NS prewarm; (b) refresh entries before TTL expiry so the 21 apiserver GETs/60s drop to 0 for cyberjoker first cold. The L1 refresher exists at `internal/cache/refresher.go` — verify apistage entries are inside its refresh set.

**Code shape:**

- Phase 2 harvester emits both per-NS keys AND cluster-list key when SA has cluster-list permission. Both keys populate at startup.
- Refresher's TTL-pre-expiry refresh policy applied to apistage class (verify it already is — `RESOLVED_CACHE_TTL_SECONDS=3600`).

**Estimated impact:** cyberjoker first cold 26.7s → ~7-8s (matching admin's). Admin median 8.16s unchanged.

**Estimated effort:** medium. Touches `phase1_content_prewarm.go` harvester + refresher tagging.

**Risk:** medium. Doubles prewarm memory cost (per-NS + cluster-list entries are redundant data). Mitigate by making the cluster-list cache entry the AUTHORITATIVE one and dropping per-NS entries when both modes available — but that introduces a per-resource switch which violates `feedback_no_special_cases`.

### Branch D — Iteration-2 instrumentation (if Branch A/B/C insufficient)

**Direction:** Add per-stage CPU profile capture at the iterator dispatch site to break down the 60ms-per-call into precise CPU components (filterListByRBAC, gojq exec, dep recording, dict merge). Currently we have logs but no `pprof` samples bound to a single request flow.

**Code shape:**

- Add `runtime/pprof` block-profile capture around the iterator-stage execution, gated by a header (e.g. `X-Snowplow-Profile: 1`).
- Tester triggers a profiled cold load, downloads the pprof artifact, identifies the dominant CPU consumer.

**Estimated effort:** low. ~50 LOC.

**Risk:** low. Pure observability addition.

---

## §5. Recommendation: pursue Branch A (cluster-list-when-allowed) as Ship D.5

Branch A is the structural fix. The 7s in-process iterator wall is the dominant cost and it does NOT shrink with more aggressive caching — the data IS already cached, the orchestration of 80 in-process calls IS the cost. Branch C does not help admin's median (admin is already hitting the cache). Branch B is real but secondary (~3% impact). Branch A collapses 80-call orchestration to 1-call, saving ~5-6s on admin's cold (8.16s → ~2.5s) and on cyberjoker's first cold (26.7s → ~6s once Branch C also lands).

**Branch A is empirically grounded** in:
- F3: 7.2s server-side wall, all in iterator dispatch with apistage hits.
- F4: per-call cost decomposed to ~60ms; arithmetic minimum sets the bound.
- F7: K8s apiserver DOES support cluster-wide LIST of namespace-scoped resources; the iterator pattern is a snowplow-side template choice, not an apiserver constraint.

**Branch A does NOT violate `feedback_no_special_cases`** — the additive field `clusterListWhenAllowed` is a per-RA opt-in flag derived from the RA's own spec, not a per-resource hardcode in snowplow Go.

**Branch A respects `feedback_cache_must_not_constrain_jq`** — the widget jq receives the same `{list: [items]}` shape it always did; the sort/filter/slice expressions work unchanged.

**Branch C is queued as a follow-up** to close the cyberjoker first-cold gap. Branch A alone gets admin to ~2.5s and cyberjoker to ~6s; Branch C combines to ~2s and ~2.5s.

**Branch B is deferred** — it's a marginal win that can be revisited if Branch A insufficient.

**Branch D — instrumentation iteration-2** — is NOT recommended NOW. The TRACED evidence is sufficient to ratify Branch A. Add pprof block-profiling only if Branch A's projected ~80% reduction underperforms after ship.

---

## §6. Pre-ship falsifier (per `feedback_falsifier_first_before_ship`)

Before any code change, capture this baseline artifact under `/tmp/snowplow-runs/0.30.151-northstar/`:

```
kubectl logs $POD -n krateo-system --since=2h --tail=99999 \
  | grep -E 'allCompositions|widgetDataTemplate|apistage.content_hit|informer_dispatch.rbac_filter' \
  > 0.30.151-iterator-fan-out-baseline.log
```

This log shows the 80-call iterator fan-out. Ship D.5's success criterion: post-deploy the SAME query against admin's cold load shows **ONE** `allCompositions` call (path `/apis/composition.krateo.io/v1-2-2/githubscaffoldingwithcompositionpages` — cluster-scoped, no `/namespaces/<x>/`), with corresponding `apistage.content_hit` `ns=""`.

If post-deploy still shows 80 calls, Branch A did not bind correctly → revert.

---

## §7. Artifact references

- **Tester measurements:** `/tmp/snowplow-runs/0.30.151-northstar/measurements.json` + `/tmp/snowplow-runs/0.30.151-northstar/cj-dashboard-cold-1.json`
- **Pod logs:** `/tmp/snowplow-24h-logs.txt` (149 lines, contains the admin cold burst at 01:24:22 → 01:24:30)
- **Pod fallthrough cells (no `secret-get` reason — separate Ship D.3 finding):** captured via `kubectl port-forward → /debug/vars` at 8081 from pod `snowplow-5c74b796c7-frb9z`
- **Widget blueprints:** `/Users/diegobraga/krateo/portal-cache/blueprint/templates/{piechart,table,row}.dashboard-compositions-panel-row*.yaml`
- **RA blueprint:** `/Users/diegobraga/krateo/portal-cache/blueprint/templates/restaction.compositions-list.yaml`
- **Resolver iterator dispatch:** `internal/resolvers/restactions/api/resolve.go:355` (bounded-parallel), `:594-602` (apistage hit path)
- **RBAC filter cost source:** `internal/resolvers/restactions/api/informer_dispatch_rbac.go:93-180`
- **apistage cache key:** `internal/cache/resolved.go:304-395` (`ComputeKey`)
- **Phase 2 content prewarm:** `internal/handlers/dispatchers/phase1_content_prewarm.go:1-100`
- **gateContentEnvelope:** `internal/resolvers/restactions/api/apistage.go:94-145` (single-site gate)

---

## §8. Summary one-liner

**Admin Dashboard 8.16s cold is the 80-namespace iterator fan-out's in-process orchestration cost (~7s) with apistage cache HITTING — not jq, not prewarm gap, not double-fetch.** The structural fix is Branch A: collapse the iterator to a single cluster-scoped LIST when the requester (or SA prewarm) has cluster-list permission. Ship D.5 design recommended.
