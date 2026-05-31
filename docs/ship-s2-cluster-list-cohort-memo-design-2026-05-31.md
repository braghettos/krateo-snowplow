# Ship S.2 — Cluster-LIST + Cohort Gate Memo (warmth-safe prune)

Architect deliverable, 2026-05-31.
Branch head when written: `b9ed4ce  feat(cache): Ship Phase B / 0.30.185 …` (current `ship-0.30.185-phase-b-postjq-encoded`); production stable = `0.30.212`.

## Section 0 — Ship value-prop reframe [CONDITION 3]

**Per PM CONDITION 3 (success criteria reframe), the ship's stated target is:**

> S.2 does NOT close the 11s lastCallEnd (architectural portal SPA fan-out, see 0.30.212_phase6_results.json `lastCallEnd_verdict`). S.2 reduces per-call cost on the compositions-panels and blueprints-panels children of that SPA fan-out.

[SCOPE NARROWING 2026-05-31] **Empirical live enumeration** (`docs/ship-s2-byte-budget-probe-2026-05-31.md` §"RA → GVR mapping") confirms today's qualifying set is **exactly 2 RAs** — `compositions-panels` and `blueprints-panels` — **both targeting the SAME GVR `widgets.templates.krateo.io/v1beta1/panels`**. S.2 therefore produces **exactly ONE** cluster-LIST apistage cell, shared across both RAs (same `contentKey`). TRACED to probe artifact. This is a scope NARROWING (smaller surface, lower risk) relative to the earlier note that listed buttons/markdowns/forms/flowcharts/eventlists — those are per-widget Kind cells, not cluster-LIST cells and do NOT qualify for S.2's collapse pattern.

The customer-facing impact is per-call cost reduction on the `compositions-panels` and `blueprints-panels` children of the 241-call SPA fan-out. Mix-weighted north-star impact is marginal but still GREEN (no canonical /call currently breaches the target — 0.30.212_phase6_results.json `canonical_timings_warm_curl_lb` lines 32-66). The 11s `lastCallEnd_ms` (0.30.212 line 82) belongs to the portal SPA's parallel-then-serial widget loader graph and remains chart/portal-owned (per memory: feedback_compositions_north_star_views, ratified team policy). Any post-ship grading against the 11s tail is OUT-OF-CONTRACT.

Success criteria (re-stated, dropping any 11s-tail claim):

| ID | Criterion | Falsifier |
|----|-----------|-----------|
| S.2-success.1 | Per-call cost reduction on `compositions-panels` warm path. | `dispatch_delta_post_fix` ≤ 5 across 20 sample calls (vs INFERRED ≈ 1000 pre-fix). Decisive at AC-S2.1. |
| S.2-success.2 | Per-call p50 step on `compositions-panels`. | p50 ≤ 250ms across 20 sample calls (vs INFERRED ~0.5–2s pre-fix). Decisive at AC-S2.2. |
| S.2-success.3 | No regression on the OTHER 240 children of the SPA fan-out (no inflight-saturation). | `snowplow_dispatcher_inflight` p99 over the probe window ≤ 2× steady-state baseline. AC-S2.4. |
| S.2-success.4 | Predicate symmetry preserved (User+Group+SA). | `cluster_list_uaf_symmetry_test.go` three sub-tests green under `-race`. AC-S2.3 + AC-S2.7. |
| S.2-success.5 | Refresher amplification bounded. | `refresherSkippedClusterListBudget` ≥ 250 under sustained 1 panel ADD/s × 5min burst. AC-S2.4. |

**Explicitly NOT a success criterion:** any reduction in `lastCallEnd_ms` or `lastCallEnd_p50_ms_admin`. Those are SPA-architecture metrics, not snowplow-tractable. Per memory: feedback_data_driven_workflow, feedback_anchor_is_cluster_state_dependent — we grade against the actual snowplow-tractable target on today's cluster, not against an imagined-future-state customer tail.

---

## Executive summary (5 lines)

1. **Ship reframe (CONDITION 3)**: S.2 does NOT close the 11s lastCallEnd (architectural portal SPA fan-out, see 0.30.212_phase6_results.json `lastCallEnd_verdict`). S.2 reduces per-call cost on the compositions-panels and blueprints-panels children of that SPA fan-out. Mix-weighted impact is marginal but still GREEN. Full success criteria are listed in §0 above and re-cited in §6.3.
2. **Prior-art match**: a snowplow-internal version of the design already exists and is partially shipped. `attemptClusterListCollapse` (`internal/resolvers/restactions/api/cluster_list.go`) + the apistage-content L1 with cohort gate memo (`apistage.go`, `apistage_cohort_memo.go`) + `cohort_ns_acl.go` form a complete cluster-LIST → identity-free Put → per-cohort prune → per-user fail-closed gate. The collapse is **held INERT** behind `clusterListCollapseEnabled=false` (cluster_list.go:85). S.2 is the **enablement + safety wiring** ship, not a new mechanism ship.
3. **Biggest design risk**: refresher amplification. A single cluster-LIST cell at 50K scale = ~363 MB envelope per `RefreshContentEntry` cycle, and the refresher is wired (dispatchers.go:92). One worker × per-dirty-mark × per-cohort = unbounded work even with WQ dedup. Phase B 0.30.185 was HARD REVERT'd for exactly this class (5.9× regression). S.2 design **excludes compositions-list from collapse** and bounds cohort memo work explicitly via the §7.2 AC-S2.refresh-bound. Byte-budget default is empirical-derived per CONDITION 1 (parallel dev probe).
4. **Falsifier signal-to-noise**: HIGH. The pre-fix probe is a single `curl /call?…compositions-panels` with `time` + `dispatch_l1_lookups` widgets|...panels expvar delta read; the falsifier is decisive at p50 step >300ms. Post-fix the same probe shows ~1 cluster-LIST dispatch, ~49 cohort-memo serves, no per-NS apistage entries written. Probe is capture-able TODAY on 50K production cluster.
5. **What this design ships**: ~180 LOC. Enable `clusterListCollapseEnabled=true` (1 line); plug a `derivedFromUAF` second derivation path into `deriveTargetGVRForClusterList` so the gate fires on `compositions-panels` shape (~80 LOC, cluster_list.go — load-bearing, see R-helper); add **AC-S2.refresh-bound**: cap refresher concurrency for `CacheEntryClassApistage` LIST entries whose envelope > `defaultClusterListRefreshByteBudget` constant (~50 LOC, refresher.go); add a **symmetric SA+User+Group binding-set falsifier test** (~50 LOC) covering predicate-symmetry per ζ-revert.

[SCOPE NARROWING 2026-05-31] On today's 50K production cluster, S.2 collapses **exactly 2 RAs** (`compositions-panels`, `blueprints-panels`) into **exactly 1 cluster-LIST apistage cell** keyed by `widgets.templates.krateo.io/v1beta1/panels` — TRACED to empirical probe (`docs/ship-s2-byte-budget-probe-2026-05-31.md`). Smaller surface than the earlier multi-GVR risk model assumed; refresher-amplification envelope is now a single 92 MiB cell, not N panel-widget cells.

---

## 1. Prior-art check (client-go)

**Question:** how does client-go / kubectl handle all-namespaces LIST with per-user RBAC filtering?

**Answer:** client-go does NOT solve "all-namespaces LIST + per-user filtering" the way S.2 needs. kubectl's `--all-namespaces` issues a *cluster-scope* LIST via the apiserver, and the apiserver itself does the RBAC gate (deny → 403 → kubectl prints nothing for that user). The apiserver does NOT do "filter items by per-user RBAC" — it's a binary allow/deny on the whole call. The closest client-go primitive is `informer.Lister().List(labels.Everything())` on a `SharedInformerFactory` set up with `WithNamespace(metav1.NamespaceAll)`; that returns the cluster-wide informer cache snapshot, identity-free.

**The snowplow problem and its solution prior-art** (TRACED, file:line):

- The per-user, per-item RBAC filter for a cluster-wide LIST is implemented in `internal/rbac/evaluate.go` (`EvaluateRBAC`). The cohort-level pre-index over it is `CohortNSACL` (`internal/cache/cohort_ns_acl.go:106-159`). The cohort-level cache is `CohortGateMemoStore` (`internal/cache/cohort_gate_memo_store.go:71-90`). The single-gate-site contract is `gateContentEnvelope` (`internal/resolvers/restactions/api/apistage.go:94-121`). All of these were shipped in 0.30.174 / 0.30.178 / 0.30.194 (Ships GMC, A.2, Fix A).
- The cluster-LIST collapse that consumes those primitives is `attemptClusterListCollapse` (`cluster_list.go:138-367`). It is **byte-identical-behaviour to healthy 0.30.204 today** (cluster_list.go:85: `var clusterListCollapseEnabled = false` — pre-S.2 inert gate).

**Conclusion:** S.2 is NOT a new mechanism design. It is **the enablement + safety-wiring of an already-shipped, already-tested mechanism**, plus a second derivation path for the compositions-panels shape that the 0.30.152 design did not anticipate.

---

## 2. Empirical trace — current paths

### 2.1 The compositions-panels RA spec (TRACED)

Live from cluster (`kubectl get restaction compositions-panels -n krateo-system -o yaml`):

```yaml
spec:
  api:
  - name: namespaces
    path: /api/v1/namespaces
    filter: '[.namespaces.items[] | .metadata.name]'
    userAccessFilter:
      group: widgets.templates.krateo.io
      namespaceFrom: .
      resource: panels
      verb: list
  - name: compositionspanels
    dependsOn: { iterator: .namespaces, name: namespaces }
    path: ${ "/apis/widgets.templates.krateo.io/v1beta1/namespaces/" + (.) + "/panels" }
    continueOnError: true
    filter: |
      [.compositionspanels.items[]?
       | select((.metadata.labels // {})["krateo.io/portal-page"] == "compositions")
       | …]
```

**Key shape facts**:

- Stage 1 (`namespaces`) lists ALL `/api/v1/namespaces` via apiserver, then UAF (`refilter.go:applyUserAccessFilter`, refilter.go:71) filters per-namespace by `list panels`. **This stage already does per-user prune; this is the UAF mechanism that Ship A.0 (#75) ships.**
- Stage 2 (`compositionspanels`) iterates `.namespaces` → N apiserver LIST calls of `/apis/widgets.templates.krateo.io/v1beta1/namespaces/<ns>/panels`. **THIS is where S.2 collapses.** Each item carries `metadata.namespace=<ns>` (verified: `kubectl get --raw '/apis/widgets.templates.krateo.io/v1beta1/panels?limit=2'`).

**TRACED — namespace iteration is the source of N×namespace requests:** `resolve.go:376` calls `createRequestOptions` which fans `tmp` to `N=len(iterator)` calls. `resolve.go:467-469`: `g := errgroup.WithContext(ctx); g.SetLimit(iterParallelism(ctx))` — bounded parallel. With `len(.namespaces) = 50` post-UAF on admin (TRACED: bench_namespaces=50 in `0.30.212_phase6_results.json`), this is 50 concurrent apiserver LISTs against `panels`.

### 2.2 Cohort Gate Memo definition + dispatch site (TRACED)

| Component | File | Line |
|-----------|------|------|
| `CohortGateMemoStore` struct | `internal/cache/cohort_gate_memo_store.go` | 71-90 |
| `cohortGateMemo` (kept-names + permitAll + rbacGen) | `internal/resolvers/restactions/api/apistage_cohort_memo.go` | 106-118 |
| `CohortNSACL` (per-cohort verdict generator) | `internal/cache/cohort_ns_acl.go` | 106-159 |
| `populateCohortGateMemo` (the populate site) | `apistage_cohort_memo.go` | 234-323 |
| `cohortGateMemoServe` (the serve site) | `apistage_cohort_memo.go` | 376-417 |
| `gateListItemsWithMemo` (the integration site) | `apistage_cohort_memo.go` | 141-206 |
| `apistageContentServe` (the dispatcher in resolver) | `apistage.go` | 423-596 |
| Call from resolve.go worker | `resolve.go` | 688 |
| `attemptClusterListCollapse` | `cluster_list.go` | 138-367 |
| Call from resolve.go (BEFORE g.Go) | `resolve.go` | 411-419 |

**TRACED — what the GMC currently caches:** per-cohort `keptNames map[string]struct{}` (kept-set in `cohortGateMemo.keptNames`, apistage_cohort_memo.go:117) OR `permitAll=true` flag (apistage_cohort_memo.go:112). On hit (apistage_cohort_memo.go:177): `cohortGateMemoServe` walks `parsed.items` ALIASING the shared `entry.Items` and assembles a fresh-outer-map envelope of kept items (no per-item RBAC fire — single map lookup per item, apistage_cohort_memo.go:399-406).

**TRACED — the L1 RESOLVED cache (per-user) stays untouched** (`resolved.go:611-612`: `CacheEntryClassApistage` and `CacheEntryClassWidgetContent` drop identity fields; all OTHER classes including `restactions`/`widgets` keep identity). The cohort memo lives on `ResolvedEntry.CohortGates` (atomic.Pointer, `cohort_gate_memo_store.go:CohortGateMemoStoreLoadOrInit`), NEVER as a separate per-cohort L1 entry.

### 2.3 Per-user prune predicate today (TRACED)

There are TWO independent prune points in the resolver:

1. **UAF refilter** (`refilter.go:applyUserAccessFilter`, refilter.go:71). Runs per-stage AFTER the stage's API call completes (resolve.go:886: `rf := applyUserAccessFilter(ctx, dict, apiCall)`). Operates on `dict[apiCall.Name]` items. Per-item `EvaluateRBAC` fail-closed.
2. **apistage content gate** (`gateContentEnvelope`, apistage.go:94-121, single-gate-site). Runs on a content-cache hit OR after the un-gated dispatch on a miss. Two sub-paths:
   - `gateListItems` / `gateListItemsWithMemo` for LISTs (apistage.go:209, apistage_cohort_memo.go:141)
   - `gateGetEnvelope` for GET-by-name (apistage.go:288)

**TRACED — exact insertion point for a new per-user prune predicate**: there are TWO existing seams.

- Cluster-LIST → apistage Put site is `cluster_list.go:329` (the un-gated cluster envelope Put). The per-user gate is enforced downstream when `apistageContentServe` runs `gateListItemsWithMemo` (apistage.go:587). No new seam needed.
- UAF refilter on stage 1 (`namespaces`) already prunes per-user (refilter.go:71). When Ship A.1 portal flip lands, stage 1 will still UAF-filter (no change).

### 2.4 Wire-shape probe (TRACED, captured 2026-05-31)

GKE context verified `gke_neon-481711_us-central1-a_cluster-1`. Probes run live:

```
kubectl get --raw "/apis/widgets.templates.krateo.io/v1beta1/panels?limit=2"
```

Returns `{"apiVersion":"widgets.templates.krateo.io/v1beta1","items":[{"apiVersion":"widgets.templates.krateo.io/v1beta1","kind":"Panel","metadata":{"name":"…","namespace":"bench-ns-01",…}…},…],"kind":"PanelList","metadata":{…,"remainingItemCount":31904,"resourceVersion":"…"}}`

**KEY: every item carries `metadata.namespace` populated.** This is the load-bearing wire-shape claim for the prune predicate to know which namespace each item belongs to. CohortNSACL builds `permittedNS map[string]struct{}` (cohort_ns_acl.go:139-153); `populateCohortGateMemo` filters by `it.GetNamespace()` membership (apistage_cohort_memo.go:298-309).

```
kubectl get --raw "/apis/composition.krateo.io/v1-2-2/githubscaffoldingwithcompositionpages?limit=2"
```

Returns identical shape (apiVersion + kind + items[*].metadata.namespace populated). Cluster-LIST works for compositions GVR as well (relevant for a future S.3 of compositions-list — out of scope for this ship).

### 2.5 Today's actual hot paths (TRACED, expvars sampled 2026-05-31)

From `kubectl -n krateo-system port-forward deploy/snowplow 9999:8081 ; curl /debug/vars`:

```
snowplow_cohort_memo_entries_total      = 155      (memo populated)
snowplow_cohort_memo_total_bytes        = 277      (essentially zero — all permitAll)
snowplow_cohort_memo_encoded_bytes_…    = 0        (Fix A removed encoded path)
snowplow_cohort_memo_overflow_total     = 0        (cap fine)
snowplow_dispatch_l1_lookups[…panels]   = {hit:110, miss:6068}   ← !!
snowplow_dispatch_l1_lookups[…buttons]  = {hit:195, miss:12071}  ← !!
snowplow_dispatch_l1_lookups[…markdowns]= {hit:97,  miss:6035}   ← !!
snowplow_ra_full_list_memo (panels)     = NOT PRESENT  ← compositions-panels NOT using RAFullList today
snowplow_ra_full_list_memo (compositions-list) = verdict=False  ← compositions-list NOT cluster-collapsed today
```

**Inference labeled as INFERRED:** the giant miss totals on widgets are post-fan-out per-user content lookups, not cluster-LIST candidates. They are not what S.2 fixes. S.2 fixes the per-NS LIST fan-out on stage 2 of compositions-panels / blueprints-panels: replaces ~50 informer dispatches with 1 cluster-LIST dispatch shared across cohorts.

---

## 3. Wire-shape probe results

Captured 2026-05-31 against gke_neon-481711_us-central1-a_cluster-1.

| GVR | wire-shape | `metadata.namespace` on items | cluster-scope LIST OK |
|-----|-----------|------------------------------|-----------------------|
| `widgets.templates.krateo.io/v1beta1/panels` | `PanelList` + items + `remainingItemCount:31904` | YES (`bench-ns-01` observed) | YES |
| `composition.krateo.io/v1-2-2/githubscaffoldingwithcompositionpages` | `GithubScaffoldingWithCompositionPageList` + items | YES | YES |

**No counter-result** — wire shape supports the design. Note `remainingItemCount: 31904` on the panels probe (preliminary single-page sample). [R1 EMPIRICAL 2026-05-31] **Full-LIST probe captured live on 2026-05-31** (`docs/ship-s2-byte-budget-probe-2026-05-31.md`): 33,758 items, **96,636,785 bytes ≈ 92.2 MiB**, `remainingItemCount=0` (apiserver returns the entire list in one HTTP response; no `?continue=` pagination). This is the empirical envelope budget; S.2 design respects it via refresher-bound (see §7.2, default `72 MiB`).

---

## 4. Proposed design (~180 LOC)

### 4.1 What S.2 ships

| LOC | File | Function | Change |
|-----|------|----------|--------|
| 1   | `cluster_list.go:85` | `clusterListCollapseEnabled` | `false` → `true` |
| ~80 | `cluster_list.go` (new helper) | `deriveTargetGVRForClusterListFromUAFStage` | Second derivation path: when the stage's `dependsOn.iterator` is `.namespaces` AND a SIBLING stage in the same RA carries a `userAccessFilter` whose `verb=list,resource=<R>` matches the GVR in the iterator's `path` template, we derive `(group, resource)` from the path template instead of from the first iterator element. (Today's `deriveTargetGVRForClusterList`, cluster_list.go:382, returns false when the iterator element is a bare namespace string because path-resolving fails.) |
| ~50 | `refresher.go` (new gate)  | `processOne` envelope-budget guard | Skip-to-TTL when entry's `Inputs.CacheEntryClass==apistage` AND `len(RawJSON) > CLUSTER_LIST_REFRESH_BYTE_BUDGET` AND no informer dep-record event has fired in the last `CLUSTER_LIST_MIN_REFRESH_INTERVAL_S`. New skip-counter `refresherSkippedClusterListBudget`. |
| ~50 | new test  | `cluster_list_uaf_symmetry_test.go` | Three sub-tests, ALL THREE MUST PASS: (a) User-only RoleBinding, (b) Group-only RoleBinding, (c) ServiceAccount-only RoleBinding. All three pass cluster-LIST collapse, all three get correct per-cohort prune via CohortNSACL.SA-landings (cohort_ns_acl.go:233-275, Ship 1.1 / 0.30.196). |

**Total ~180 LOC, well within the 150-250 LOC envelope.** No new types, no schema change, no chart change. Single binary ship.

### 4.2 Warmth-safe prune semantics

The cluster-LIST envelope is shared across cohorts in the apistage L1 cell (identity-free per `apistage.go:16-26`, `resolved.go:611`). The per-cohort prune is computed via `CohortNSACL` (cohort_ns_acl.go:106-159) which classifies the cohort's verdict:

| Cohort verdict | Outcome |
|---|---|
| `permitAll=true`  | Cluster-wide grant; every item kept; `cohortGateMemoServe` returns `listEnvelopeValue(apiVersion, kind, parsed.items)` shallow envelope (apistage_cohort_memo.go:385-394). |
| `permitAll=false, permittedNS={ns₁,…}` | Per-namespace grant; `keptNames` filtered by `item.GetNamespace() ∈ permittedNS` (apistage_cohort_memo.go:298-309). Serve walks parsed.items and emits the subset (apistage_cohort_memo.go:398-406). |
| `permitAll=false, permittedNS=∅` | Cohort cannot list; `keptNames` empty; `cohortGateMemoServe` emits empty `items[]`. |
| Snapshot nil (pre-readiness) | Falls back to `populateMemoFromCanonicalFilter` → per-item `filterListByRBAC` (apistage_cohort_memo.go:330-356); identical to pre-GMC behaviour. |

**Warmth-safety**: the rbacGen stamp (apistage_cohort_memo.go:108, 170) invalidates a stale memo on any RBAC mutation that touches this cohort's matched binding-set. Cohort memo storage is the per-ResolvedEntry `CohortGateMemoStore` (cohort_gate_memo_store.go), insertion-order capped at `CACHE_COHORT_MEMO_CAP` (default 128, cohort_gate_memo_store.go:64); benign on overflow (next miss re-derives, cohort_gate_memo_store.go:30-36).

### 4.3 L1 contract intact (TRACED)

- **L1 RESOLVED cache (per-user)**: unchanged. `restactions` and `widgets` entry classes still fold identity into the key (`resolved.go:611-612`). The cluster-list collapse stores the un-gated envelope under `CacheEntryClassApistage` (identity-free per design) — NOT under a restactions/widgets key. The serve path's gate runs in `apistageContentServe` (apistage.go:583-589) before the value reaches the per-user dispatcher path. No cross-user leak.
- **DELETE evicts only the deleted object's own entry; ADD/UPDATE dirty-marks**: unchanged. The dep edge for the cluster-list cell is `cache.Deps().RecordList(contentKey, gvr, "")` (cluster_list.go:338) — empty namespace, so an informer ADD/UPDATE/DELETE on any object of `(gvr, *)` dirty-marks the cell. The 0.30.212 fix made this idempotent (cluster_list.go doc:337-338).

### 4.4 No hardcoded GVRs / paths / users — qualifying shapes [SCOPE NARROWING 2026-05-31]

The new `deriveTargetGVRForClusterListFromUAFStage` reads from the RA spec only (the iterator path template + the sibling stage's `userAccessFilter.resource`). It does NOT special-case "panels" or "compositions". Any RA whose shape is "iterator over namespaces + per-iter LIST against a UAF-resolved GVR" qualifies.

**TRACED — qualifying set on the 50K production cluster** (empirical live enumeration via `kubectl get restaction -n krateo-system`, captured 2026-05-31 in `docs/ship-s2-byte-budget-probe-2026-05-31.md` §"RA → GVR mapping"):

| RA | Iterator path template | Derived GVR |
|----|------------------------|-------------|
| `compositions-panels` | `/apis/widgets.templates.krateo.io/v1beta1/namespaces/<ns>/panels` | `widgets.templates.krateo.io/v1beta1/panels` |
| `blueprints-panels`   | `/apis/widgets.templates.krateo.io/v1beta1/namespaces/<ns>/panels` | `widgets.templates.krateo.io/v1beta1/panels` |

**Qualifying RA count: 2. Qualifying GVR count: 1.** Both RAs collapse to the same `(panels GVR, empty ns, empty name)` content key (`cluster_list.go:312`), so S.2 produces **exactly ONE** cluster-LIST apistage cell on this cluster, shared across both RAs. Future RAs matching the qualifying shape gain the benefit automatically — the helper is shape-driven, not name-driven (per `feedback_no_special_cases`).

The earlier risk model noted "buttons / markdowns / forms / flowcharts / eventlists" as separate cluster-LIST cells; those are per-widget Kind cells (per-user content lookups, §2.5 expvars), NOT cluster-LIST cells. They do not match the qualifying shape and S.2 does not touch them.

---

## 5. Predicate symmetry test plan (User + Group + ServiceAccount)

Per `feedback_predicate_subject_kind_symmetry` (ζ HARD REVERT lesson, 0.30.183): predicate MUST apply symmetrically across User, Group, AND SA subject kinds. `CohortNSACL` already does (Ship 1.1 / 0.30.196 added SA landings: cohort_ns_acl.go:233-275 + 297-370).

**New test file `internal/resolvers/restactions/api/cluster_list_uaf_symmetry_test.go` (~50 LOC):**

```go
// Three sub-tests, each builds a watcher with cluster-LIST grant on the
// same GVR but a DIFFERENT subject kind binding it to the test cohort:
//   Sub-test A: User-kind RoleBinding (namespace-scoped grant)
//   Sub-test B: Group-kind RoleBinding (same)
//   Sub-test C: ServiceAccount-kind RoleBinding (same; cohort identity
//               is system:serviceaccount:<ns>:<name>)
//
// All three MUST observe identical kept-set sizes after cohort prune.
// MUST ALSO assert: only ONE cluster-LIST dispatch fired (via expvar
// snowplow_apiserver_fallthrough_cells['…|cluster-list-dispatch']
// delta == 1).
```

**Falsifier for ζ-class regression**: if ANY of the three sub-tests passes while another fails on identical RBAC mass, the predicate is asymmetric → HARD REVERT class. Three sub-tests in one file = single `go test` invocation, decisive.

---

## 6. Pre-flight falsifier (capture BEFORE writing code)

Per `feedback_falsifier_first_before_ship`. Capture against PRODUCTION SCALE (50K) on `gke_neon-481711_us-central1-a_cluster-1`.

### 6.1 Pre-fix probe (today, 0.30.212)

```bash
# Verify cluster
[[ "$(kubectl config current-context)" == "gke_neon-481711_us-central1-a_cluster-1" ]] || exit 1

# Mint admin JWT
ADMIN_JWT="$(cat /tmp/admin-jwt-0.30.212.txt 2>/dev/null || \
  curl -s -X POST http://34.136.84.51:8082/auth/login \
    -d '{"username":"admin","password":"…"}' -H 'Content-Type: application/json' | jq -r .token)"

# Sample compositions-panels TWENTY times warm; record per-call wall-clock + per-stage timing
SNOWPLOW="http://34.135.50.203:8081"
URL="$SNOWPLOW/call?apiVersion=templates.krateo.io%2Fv1&resource=restactions&name=compositions-panels&namespace=krateo-system"

# Warm-up (discard 3)
for i in 1 2 3; do curl -s -o /dev/null -H "Authorization: Bearer $ADMIN_JWT" "$URL"; done

# Pull expvar baseline
kubectl -n krateo-system port-forward deploy/snowplow 9999:8081 &>/dev/null & PF=$!; sleep 2
BASELINE="$(curl -s http://localhost:9999/debug/vars)"
PRE_DISPATCH_COUNT="$(echo "$BASELINE" | jq -r '.snowplow_apiserver_fallthrough_cells | with_entries(select(.key | contains("panels"))) | values | add // 0')"

# Probe 20 samples
for i in $(seq 1 20); do
  /usr/bin/time -f '%e' curl -s -o /dev/null -H "Authorization: Bearer $ADMIN_JWT" "$URL" 2>&1
done | sort -n | awk 'NR==10 || NR==11 {sum+=$1; n++} END {print "p50_s="sum/n}'

# Post probe
POST="$(curl -s http://localhost:9999/debug/vars)"
POST_DISPATCH_COUNT="$(echo "$POST" | jq -r '.snowplow_apiserver_fallthrough_cells | with_entries(select(.key | contains("panels"))) | values | add // 0')"
echo "dispatch_delta_pre_fix=$((POST_DISPATCH_COUNT - PRE_DISPATCH_COUNT))"
kill $PF
```

**Expected pre-fix signals:**

- `p50_s` ≥ 0.30s (TRACED in `0.30.212_phase6_results.json`: bench panels not measured directly, but `cyberjoker_p50_ms=327` for `sidebar_nav_menu` is the closest peer; admin compositions-panels is INFERRED ~0.5–2s warm at 50K).
- `dispatch_delta_pre_fix` ≈ `50 × 20 = 1000` (50 per-NS LIST × 20 samples, INFERRED from bench_namespaces=50). Even with apistage-content cache warming the L1 per-(gvr,ns) cells, FIRST cold sample = 50; subsequent warm samples = small (cache hits).
- `snowplow_cohort_memo_entries_total` delta = small (cohort already populated).

### 6.2 Post-fix expected signals

After S.2 ships (clusterListCollapseEnabled=true; UAF-derivation path lands):

- `p50_s` ≤ 0.15s (one cluster-LIST + 49 cohort-memo serves vs 49 per-NS LISTs).
- `dispatch_delta_post_fix` = 1 (one cluster-LIST cell warmed once, hit 20× thereafter).
- New expvar `snowplow_cluster_list_collapse_used_total` (publish via existing `PIPStageTiming.ClusterListUsed`) climbs by 20 (every sample hits the collapse).
- Cohort memo entries grow by exactly 1 per cohort (per-entry cohort memo store, cohort_gate_memo_store.go:107). At 50 cohorts × 1 entry/cohort/cell = 50 new entries. Bounded by cap=128.

### 6.3 Signal-to-noise estimate

**HIGH.** The expvar delta and wall-clock p50 step are decisive. The cluster has been stable for 5h+ (`0.30.212_phase6_results.json`: snowplow_uptime_seconds=19400, 0 restarts) — noise floor is low. Acceptance: any post-fix probe where dispatch_delta > 5 OR p50_s > 0.25s is a REGRESSION → revert.

[SCOPE NARROWING 2026-05-31] **Single-GVR scope STRENGTHENS the falsifier**: with exactly one cluster-LIST cell on the cluster (TRACED to probe), the expected post-fix `dispatch_delta` is a clean **1** (one cluster-LIST dispatch warmed once, hit 20× thereafter). Any non-1 reading is unambiguous — `dispatch_delta=0` ⇒ the helper isn't firing (R-helper failure mode (a)); `dispatch_delta>5` ⇒ the cohort memo isn't hitting. No multi-GVR superposition to disentangle.

---

## 7. Refresher-amplification trace + bound

This is the biggest risk per `feedback_refresher_populate_amplification` (Phase B 0.30.185 HARD REVERT lesson: 5.9× wall-clock regression from a new populate layer × refresher cycles × entry-count).

### 7.1 Refresher path for an apistage cluster-LIST cell (TRACED)

| Step | File:Line | What happens |
|------|-----------|--------------|
| Informer ADD on `panels/<ns>/<name>` fires | `cache/dep_tracker.go` | OnAdd → dirty-mark every L1 key with `RecordList(*, panels-GVR, *)` dep edge |
| Cluster-LIST cell has `Deps().RecordList(contentKey, gvr, "")` | `cluster_list.go:338` | empty-ns dep → ADD on ANY panels object dirty-marks the cluster-LIST cell |
| Refresher worker dequeues contentKey | `refresher.go:processOne:351-389` | Loads entry, dispatches RefreshFunc for `CacheEntryClassApistage` |
| RefreshFunc → `resolveAndPopulateL1` → `resolveOnceProd` | `resolve_populate.go:302-304` | Routes to `restactionsapi.RefreshContentEntry` |
| `RefreshContentEntry` re-dispatches via informer | `apistage.go:620-640` | One un-gated `dispatchViaInformer(WithApistageContentResolve(ctx), call)` against the cluster-scope path |
| Fresh envelope Put under same content key | `resolve_populate.go:255-263` | Overwrites RawJSON + Items |

**Amplification factor at 50K [R1 EMPIRICAL 2026-05-31]**: each panels CREATE/UPDATE/DELETE event fires ONE refresh of the cluster-LIST cell (workqueue dedup is the safety net — `refresher.go:18`: `Add(key) of an already-queued key is a no-op`). The cluster-LIST envelope is **~92 MiB** (TRACED via empirical probe `docs/ship-s2-byte-budget-probe-2026-05-31.md`: 96,636,785 bytes for the `panels` cluster-LIST, 33,758 items at 50K composition scale). The prior "363 MB at admin-cohort scale" comment in cluster_list.go:248 reflects a different scale point and is superseded by today's empirical 92 MiB for the single qualifying cell. At Phase B 0.30.185 storm rates (composition-dynamic-controller installs N compositions, each creating panels), the refresher single-key-dedup is the only thing keeping this bounded.

**Bound by workqueue dedup**: a burst of 1000 panels ADDs in 100ms → ONE refresh dispatch (Add coalesces). With `RESOLVED_CACHE_REFRESHER_PARALLELISM=4` (default) and **exactly ONE cluster-LIST cell on this cluster** [SCOPE NARROWING 2026-05-31] (both qualifying RAs share the same content key — TRACED), worst case = **1 concurrent ~92 MiB dispatch**, not N.

### 7.2 The risk that needs a new bound

Cluster cold-start prewarm + sustained-burst pattern: even with WQ dedup, the cell is re-dispatched on EVERY informer event burst. At sustained 1 event/s, that's 1 × ~92 MiB / s of allocator churn — the exact pattern Phase B HARD-REVERT'd, scoped to a single cell on today's cluster.

**Proposed bound — AC-S2.refresh-bound** (new code, refresher.go) [§7.2 ESCALATION]:

```go
// Code-side defaults — NOT operator-tunable, NOT chart-wired.
// These are SRE-bench/diagnostics-only override surfaces; the
// customer-visible operator surface is exactly one flag: CACHE_ENABLED.
// Per project_single_cache_flag_direction + PM §7.2 escalation.
// [R1 EMPIRICAL 2026-05-31] Default 72 MiB derived from empirical
// envelope probe (92.2 MiB worst-case × 0.75, rounded). Per-NS apistage
// entries cluster ~2 MB → 35× separation = zero false-positive risk.
// Artifact: docs/ship-s2-byte-budget-probe-2026-05-31.md.
const (
    defaultClusterListRefreshByteBudget  = 72 * 1024 * 1024 // 72 MiB (TRACED, empirical 0.75× rule)
    defaultClusterListMinRefreshInterval = 30 * time.Second
)

// processOne — new pre-dispatch gate
if entry.Inputs != nil && entry.Inputs.CacheEntryClass == CacheEntryClassApistage {
    rawBytes := int64(len(entry.RawJSON))
    if rawBytes > clusterListRefreshByteBudget() {
        // Big cluster-LIST cells are refreshed at most once per
        // defaultClusterListMinRefreshInterval (30s) regardless of
        // informer storm. Workqueue dedup is the in-burst layer;
        // this is the steady-state amplification gate.
        sinceLastRefresh := time.Since(entry.LastRefreshedAt())
        if sinceLastRefresh < clusterListMinRefreshInterval() {
            r.refresherSkippedClusterListBudget.Add(1)
            // Re-enqueue with FIXED 30s delay (not exponential — this
            // is rate-limiting, not failure backoff).
            r.queue.AddAfter(key, clusterListMinRefreshInterval() - sinceLastRefresh)
            return nil
        }
    }
}
```

**Default discipline [§7.2 ESCALATION — PM mandated]**:

- The defaults `defaultClusterListRefreshByteBudget` and `defaultClusterListMinRefreshInterval` are **CODE-SIDE CONSTANTS**, not env-var lookups at definition time. The two `*()` accessor functions read the env-var IF AND ONLY IF set, and fall back to the constants when unset. The env-var read path exists exclusively for **SRE bench/diagnostics override** (e.g. capturing falsifier data with a deliberately tight budget) and is NOT a customer-tunable operator surface.
- **NO chart wiring** of `CLUSTER_LIST_REFRESH_BYTE_BUDGET` or `CLUSTER_LIST_MIN_REFRESH_INTERVAL_S`. `chart/snowplow/values.yaml` does NOT get a new key for either. The two env-var names are documented in this design and in code comments at their accessor functions; they MUST NOT appear in any chart template, values file, or operator-facing documentation.
- End-state remains **one customer-visible flag: CACHE_ENABLED** (memory: project_single_cache_flag_direction).
- Env-var names retained from the original draft: `CLUSTER_LIST_REFRESH_BYTE_BUDGET`, `CLUSTER_LIST_MIN_REFRESH_INTERVAL_S` (parse: int bytes / int seconds; on parse-error, fall back to constant; on absence, fall back to constant). Reflected in NEW CODE labeling — they are not TRACED.

**CONDITION 1 — empirical byte-budget [R1 EMPIRICAL 2026-05-31, DISCHARGED]**: Per PM CONDITION 1 (and memory: feedback_capacity_caps_empirical_per_entry_cost — 0.30.151 was 180× off), the `defaultClusterListRefreshByteBudget` constant MUST be set from the empirical envelope size captured on the 50K production cluster. Path A (direct cluster-LIST probe via `kubectl get --raw`) was sufficient because `cluster_list.go:312-319` stores `RawJSON: rawEnvelope` verbatim — byte-identical to what S.2 would Put. **Probe captured 2026-05-31** (`docs/ship-s2-byte-budget-probe-2026-05-31.md`):

| Metric | Value |
|--------|-------|
| Qualifying GVR (single cell) | `widgets.templates.krateo.io/v1beta1/panels` |
| Items in cluster-LIST | 33,758 (first page, `remainingItemCount=0`) |
| Worst-case envelope bytes | **96,636,785 ≈ 92.2 MiB** (3 back-to-back probes, ≤0.1% variance) |
| PM formula | `min(empirical × 0.75, 100 MiB) = min(72.5 MiB, 100 MiB) = 72.5 MiB` |
| Rounded constant | **72 MiB = 75,497,472** (cleaner Go literal `72 * 1024 * 1024`) |
| Per-NS apistage entry typical size | ~2 MB (probed `bench-ns-01` = 2,173,124 B) |
| Mis-classification margin | **~35× separation** between normal per-NS entry (~2 MB) and the cluster cell threshold (72 MiB) |

The 0.75× rule is the binding constraint (below the 100 MiB ceiling). Final default `72 MiB` is set in code, attached to ledger row, and labeled TRACED.

`LastRefreshedAt()` is a **NEW CODE** (not TRACED) `atomic.Int64` field on `ResolvedEntry`. Set on Put. Per PM gate question #3 audit gap, this is labeled NEW CODE rather than TRACED.

**Falsifier for this bound**: under sustained-burst (1 panel ADD/s × 5 min), `refresherSkippedClusterListBudget` should climb to ≥298 (1/s minus the bursts that legitimately fire ≤ 10 refreshes total). The cell's `lastUpdatedAtUnixSeconds` (via `ra_full_list_memo`-style expvar — sibling addition) shows the cell is updated approximately every 30s, not every burst.

---

## 8. CACHE_OFF transparent fallback

Per `project_cache_off_is_transparent_fallback` + `project_single_cache_flag_direction`: with `CACHE_ENABLED=false`, S.2 must be transparent.

**TRACED**: `attemptClusterListCollapse` already short-circuits on `cache.Disabled()` (cluster_list.go:156-158: `if cache.Disabled() { return nil, false, 2 }`). The iterator path keeps its per-NS fan-out. Per-NS calls fall through `dispatchViaInformer` → on `cache.Disabled()` → `httpcall.Do` against per-user kubeconfig. Same data, same UI, slower (no per-NS caching). Identical to today's CACHE_OFF behaviour because `clusterListCollapseEnabled=false` makes today's behaviour ALREADY equivalent to the CACHE_OFF post-fix behaviour.

**No new operator-visible toggle [CONDITION 5].** S.2 introduces NO new customer-visible env flag. `clusterListCollapseEnabled` is hard-flipped to `true` (package-level `var`, NOT chart-exposed); the two SRE-only env-var override surfaces (`CLUSTER_LIST_REFRESH_BYTE_BUDGET`, `CLUSTER_LIST_MIN_REFRESH_INTERVAL_S`) are bounds with CODE-SIDE constant defaults and NO chart wiring (§7.2 + §9.3 ESCALATION). Consistent with `project_single_cache_flag_direction`: end-state is ONE customer-visible flag, `CACHE_ENABLED`.

---

## 9. Rollout plan

### 9.1 Single ship (recommended)

| Tag | Change |
|-----|--------|
| `0.30.213` | Ship S.2 single commit: enable + UAF-derivation + refresh-bound + symmetry test |

### 9.2 Why not phased

- **Observability-only intermediate ship has no value** because the mechanism is already shipped behind the inert gate. Production already runs the gate-deny path 100% of the time (cluster_list.go:151-153 returns `denyGate=1` immediately). An expvar that says "we'd have collapsed N times" is INFERRED — the only honest measurement is the live falsifier (§6) on the flipped binary.
- **Two-flag ship is rejected** because `clusterListCollapseEnabled` is NOT an env flag (cluster_list.go:74-77: explicitly NOT an env knob, single-flag-direction). Flipping it = code change = new tag.

### 9.3 Lockstep chart [§7.2 ESCALATION + CONDITION 5]

Per `feedback_chart_release_lockstep`: any snowplow tag MUST be lockstep-tagged on `braghettos/snowplow-chart`. **S.2 introduces ZERO chart values changes**:

- NO new `values.yaml` keys.
- NO chart wiring of `CLUSTER_LIST_REFRESH_BYTE_BUDGET` or `CLUSTER_LIST_MIN_REFRESH_INTERVAL_S` (these are SRE-only env-var override surfaces with code-side constant defaults — see §7.2; per PM §7.2 escalation).
- NO new env-var rows in `templates/configmap.yaml` or `templates/deployment.yaml`.
- NO change to `CACHE_ENABLED` semantics (project_single_cache_flag_direction; one customer-visible flag end-state preserved).
- Chart `appVersion` moves to `0.30.213` (image bump) and the tag is pushed to braghettos explicitly per `feedback_chart_repo_origin_is_upstream`.

Per `feedback_chart_repo_origin_is_upstream`: chart-side `git push braghettos <tag>` explicitly. Per `feedback_braghettos_ownership`: braghettos is Diego's, not malware-class.

**CONDITION 5 audit (no new fall-back flags)**: end-to-end audit of this design discharged below in §9.5.

### 9.4 Revert plan [CONDITION 5]

The first failed falsifier (§6.3 acceptance bounds) → revert by flipping `clusterListCollapseEnabled = false` in a `0.30.214` revert commit. Per-NS iterator path resumes byte-identical to 0.30.212. Refresh-bound code stays dormant (no apistage cells > defaultClusterListRefreshByteBudget once the cluster-LIST cell stops populating). The two SRE env-var knobs default-bounded; no operator action needed.

**No deprecation surface, no rollout-grace flag, no env-override of the flip itself.** The only new mechanism enablement is flipping `clusterListCollapseEnabled` from `false` to `true` at `cluster_list.go:85` (a `var`, NOT a `const` — already test-overridable since 0.30.212, TRACED at cluster_list.go:74-85). Revert = flip back to `false` in a 0.30.214 commit. Per memory: feedback_no_park_broken_behind_flag — if falsifiers fail, the defect is reverted out, NOT parked behind a default-off flag.

### 9.5 CONDITION 5 audit — no new fall-back flags

End-to-end audit of this design for any new flag/env-var that "softens" the flip:

| Item | Disposition | Notes |
|------|-------------|-------|
| `clusterListCollapseEnabled` (cluster_list.go:85) | Pre-existing `var`, flipped from `false` to `true`. | The ONLY new mechanism enablement. Not an env knob (cluster_list.go:74-77 explicitly so). TRACED. |
| `CACHE_ENABLED` | UNCHANGED. Still the single customer-visible toggle. | project_single_cache_flag_direction preserved. |
| `CLUSTER_LIST_REFRESH_BYTE_BUDGET` | SRE-only env-var with CODE-SIDE constant default; NO chart wiring. | Not customer-tunable. §7.2 escalation discharged. |
| `CLUSTER_LIST_MIN_REFRESH_INTERVAL_S` | SRE-only env-var with CODE-SIDE constant default; NO chart wiring. | Not customer-tunable. §7.2 escalation discharged. |
| Any soft-revert / rollout-grace flag | NONE. | Revert path is flip-and-tag (§9.4), not flag-driven. |
| Any deprecation surface | NONE. | Single-commit ship; revert is single-commit. |

**Audit result**: design introduces ZERO new operator-visible flags. The two new env vars are SRE-only overrides on top of code-side constants and have no chart surface. Discharges CONDITION 5.

---

## 10. Risk register

| # | Risk | Likelihood | Mitigation |
|---|------|-----------|------------|
| R1 [R1 EMPIRICAL 2026-05-31] | **Refresher amplification at sustained burst** (Phase B-class regression). [SCOPE NARROWING 2026-05-31] Single cluster-LIST cell × refresher cycles × controller storm = allocator pressure. TRACED envelope: **92.2 MiB worst-case** (single cell on today's cluster; both qualifying RAs share the same content key — NOT × N panel-widget cells). Refresher gate at **72 MiB threshold** = **35× separation** from typical per-NS apistage entry (~2 MB) → zero false-positive mis-classification risk. Artifact: `docs/ship-s2-byte-budget-probe-2026-05-31.md`. | MED (downgraded from HIGH: surface is one cell, not five+; empirical envelope is 4× smaller than prior 363 MB estimate; mis-classification margin is 35×) | AC-S2.refresh-bound (§7.2). New expvar tracks `refresherSkippedClusterListBudget`. Falsifier: under storm, this counter climbs ≥1/s. |
| R2 | **Cohort memo miss-storm at cold start.** A pod restart + 50 cohort users all hitting compositions-panels simultaneously = 50 × (per-item RBAC fan-out for memo populate) on the SAME cell. At 31904 items × 50 cohorts × 100ns/item EvaluateRBAC = ~160s combined CPU. | MED | CohortNSACL fast-path avoids per-item EvaluateRBAC for the `permitAll` AND `permittedNS` branches (cohort_ns_acl.go:106-159) — only the `populateMemoFromCanonicalFilter` fallback (snapshot nil pre-readiness) does per-item. Snapshot-publish-before-serve is the gate; verified at boot. Falsifier: prewarm engine boot stamps the snapshot before HTTP serving begins (TRACED: phase1 walks run before server.Serve). |
| R3 | **GET-by-name partial-shape false-positive on the cluster-LIST collapse path.** D.4.2 (apistage.go:300-363) handles missing apiVersion/kind on GET-by-name cluster-list returns LIST envelopes (kind ends with "List") — `validateClusterListShape` (cluster_list.go:542-600) already checks for `Kind="…List"` AND non-empty items AND per-item apiVersion+kind. | LOW | Test already in `cluster_list_test.go`. Add a sub-test for the empty-cluster case (validateClusterListShape returns `envelope-items-empty` and falls through to iterator). |
| R4 | **Predicate-symmetry regression (ζ-class)** if a future refactor of CohortNSACL touches User but not Group/SA landings. | MED | New `cluster_list_uaf_symmetry_test.go` (§5) — three sub-tests, single `go test` run, decisive on ANY refactor. |
| R5 | **L1 cross-user leak via shared parsed.items** — Ship 2a (0.30.209) made `listEnvelopeValue` shallow-alias (apistage.go:249-264). A gojq write into a shared item would leak to the next request's cohort. | LOW | Ship 2a falsifier (`apistage_concurrent_isolation_test`) + gojq fork's allocator-aware deleteEmpty (apistage.go:236-248) — the only gojq input-writer is now CoW. **Test contract: -race run on the new symmetry test in CI.** |
| R6 | **F-4 freshness gap on cluster-LIST cell**: an informer ADD on a panel object in a new namespace fires after the cluster-LIST has been Put. The 0.30.212 fix wired `cache.Deps().RecordList(contentKey, gvr, "")` (cluster_list.go:338) with empty-namespace, so ANY panels object event dirty-marks the cell. | LOW | The 0.30.212 falsifier (`f4_freshness_validation`, 0.34s vs ≤5s target) confirms F-4 wiring works for the cluster-list cell. New test in `cluster_list_dep_record_test.go` already covers. |
| R-helper [R-helper UPDATE 2026-05-31] | **`deriveTargetGVRForClusterListFromUAFStage` helper (~80 LOC) mis-derives target GVR → gate doesn't fire on `compositions-panels` → ship has no per-call effect.** This is the **load-bearing** NEW CODE of S.2 (PM gate question #1 audit: today's `deriveTargetGVRForClusterList` at cluster_list.go:382 fails on compositions-panels because the iterator element is a bare namespace string, so without this helper Gate 4 never passes even with `clusterListCollapseEnabled=true`). Failure modes: (a) helper silently returns false for the compositions-panels shape → gate denies → zero per-call benefit, ship reverts on AC-S2.1; (b) **["wrong sibling wins" — REDUCED likelihood under SCOPE NARROWING 2026-05-31]** helper mis-targets to a sibling GVR (derives GVR from the WRONG sibling's userAccessFilter) → wrong cluster-LIST cell warmed → per-cohort prune still per-user-safe (gateListItemsWithMemo fail-closed) but content empty or wrong → AC-S2.2 fails, ship reverts. **Single-GVR scope makes (b) materially less likely**: both qualifying RAs (`compositions-panels`, `blueprints-panels`) target the SAME GVR (`panels`), so there is no "competing sibling cell" of a different GVR on the cluster today. (b) remains *theoretically* reachable for a malformed RA spec where the helper picks up a non-matching sibling, but the failure surface narrows to spec-level shape rather than cross-GVR cell collision. | MED-LOW (downgraded from MED: failure mode (b) reduced to spec-shape rather than cross-cell collision) | (1) **Unit test with 4 sub-tests** per PM CONDITION 2 (dev will implement; sub-test count raised from 3 → 4 per architect-dev tension flag): (a) correct sibling match for compositions-panels shape; (b) refusal-to-derive when sibling userAccessFilter has `verb!=list`; (c) refusal-to-derive when iterator path template's GVR ≠ sibling's userAccessFilter resource; (d) [NEW per SCOPE NARROWING discussion] refusal-to-derive when MULTIPLE sibling stages carry a `userAccessFilter.verb=list` (ambiguous — explicit failure rather than first-wins). (2) **Falsifier (same as §6)**: post-fix `dispatch_delta` ≈ **1** across the 20-sample probe (single-GVR scope makes the expected value a clean integer — see §6.3) — if zero, the helper isn't firing on compositions-panels (gate denies); if > 5, the cluster-LIST path isn't shared across cohorts (memo not hitting). (3) **No hardcoded GVRs/paths** per `feedback_no_special_cases`: helper reads only from RA spec (iterator path template + sibling stage's `userAccessFilter`), never from a name allowlist. |

---

## Appendix A: Surprises / future optimization candidates [SCOPE NARROWING 2026-05-31]

Captured during the empirical byte-budget probe (`docs/ship-s2-byte-budget-probe-2026-05-31.md`), out-of-scope for S.2 but worth journaling:

| Finding | Number | Future ship implication |
|---------|--------|-------------------------|
| `managedFields` is ~28% of the 92 MiB envelope | 27.5 MB / 96.6 MB (TRACED: stripped-managedFields envelope = 69,142,524 bytes) | A future **strip-on-Put** refactor (cluster_list.go:312-319 currently stores `RawJSON: rawEnvelope` verbatim) could drop worst-case from 92 MiB → 69 MiB (-28%). Per-cohort dedup memory falls correspondingly. Not in scope for S.2 — `Items` already has managedFields stripped (apistage.go:158, cluster_list.go:588); only `RawJSON` is verbatim. **Candidate for post-S.2 cleanup work (#57 territory).** No correctness change — managedFields is server-internal accounting and is not part of any widget's exposed shape. |
| `kubectl.kubernetes.io/last-applied-configuration` annotation | ~3 bytes per item | Negligible; no action. |
| Per-NS apistage entry typical size | ~2 MB (probed: `bench-ns-01` panels = 2,173,124 B; `bench-ns-50` ≈ 150 B for empty NS) | Confirms 35× separation from 72 MiB cluster-cell gate — zero false-positive risk. |
| No gzip applied at apistage layer | (Fix A 0.30.194 removed Phase B's per-cohort gzip; apistage_cohort_memo.go:45,220) | A future value-layer compressed-bytes cache could revisit; out-of-scope for S.2. See memory: feedback_no_naive_compression_middleware (don't wrap large-payload HTTP handlers with naïve middleware; cache pre-compressed bytes at value layer). |

These are deferred follow-ups, NOT blockers for S.2. Log as `#57 territory` post-S.2 cleanup candidates.

---

## Appendix: TRACED vs INFERRED summary

Every code claim above is labeled. Quick index:

- **TRACED** (file:line excerpt or live expvar): all of §1, §2.1, §2.2, §2.3, §2.4, §2.5 (sampled live 2026-05-31), §4.3, §7.1, §8 (the cache.Disabled gate).
- **INFERRED** (clearly labeled): the predicted per-call timings in §6.2 (these become TRACED once the dev captures the live falsifier against a candidate binary).

If empirical data during pre-flight probe contradicts the design's assumed pre-fix p50, surface immediately — do NOT proceed to code. The brief explicitly endorses this (`feedback_baseline_before_speculation`, `feedback_data_driven_workflow`).
