# Path 3.2 — cluster_list collapse + PIP pre-warm + refresher discipline

**Author**: cache-architect
**Date**: 2026-05-31
**Status**: DESIGN — awaiting PM ratification + dev dispatch
**Scope**: Activate cluster_list collapse for ALL iterator-fan-out RAs (Diego's mandate 2026-05-31) WITHOUT regressing the cyberjoker narrow-RBAC fast path. Customer-priority invariant intact.
**Predecessor**: Path 3.1 (Bug 2 / Bug 3 fixes shipped on `ship-0.30.217-path3-1-bug-fixes`; tag pushed, image NOT in production)
**Inputs**: `feedback_cluster_list_decode_irreducibility` (memory, 2026-05-31), `docs/path3-1-precommit-diff-2026-05-31.md`, `docs/path3-1-bug-trace-2026-05-31.md`

---

## §1 Problem statement (TRACED)

Path 3.1's helm rev 372 deploy (snowplow 0.30.217) emitted **51 `cluster_list.dispatch` log lines in 10 minutes** with the following empirical decode profile (verbatim from `docs/path3-1-precommit-diff-2026-05-31.md` lines 156-168):

```
allCompositions / compositions GVR — envelope 174 MB:
  envelope_ok_elapsed = 2,024 ms
  materialise_elapsed = 2,026 ms

compositionspanels / panels GVR — envelope 99 MB:
  envelope_ok_elapsed = 912 ms
  materialise_elapsed = 1,144 ms

allCompositionResources / configmaps — envelope 30 MB:
  envelope_ok_elapsed = 274 ms
  materialise_elapsed = 312 ms
```

The pattern is **linear in envelope bytes** (~10-12 ms/MB) for the `json.Unmarshal` step inside `validateClusterListShape` and ~10-12 ms/MB for the `decodeClusterListItems` materialisation pass. **TRACED root cause**: `json.Unmarshal` requires a full byte-by-byte parse before any field-level filter can run. JSON is not a streaming format from the standard library's perspective at this call shape. **The cost is irreducible at the protocol level** (memory `feedback_cluster_list_decode_irreducibility`).

**Per-NS iteration (today's default, `clusterListCollapseEnabled=false`) does NOT pay this cost** because narrow-RBAC users (cyberjoker, ~95% of customer mix) hit ~1-2 namespaces per /call — each per-NS envelope is ~5-10 MB → ~50-120ms decode each. The 2,024ms compositions floor is specific to **cluster-scope LIST**.

**Diego's mandate (2026-05-31)**: "All RAs use cluster-wide collapse." TRACED via `feedback_cluster_list_decode_irreducibility` + verbal at architect handoff.

**Customer-priority invariant (`feedback_customer_priority_over_refresher`)**: Customer /call MUST have absolute priority + meet north-star regardless of refresher work. Refresher pollution is acceptable input; customer pays NEVER.

**Design contract — satisfy BOTH**: cluster_list collapse fires for every customer /call hitting an eligible RA, but the **2-second envelope-decode cost is paid by the REFRESHER (background), never by the customer**. Customer hits a pre-populated cell → cheap per-request UAF prune (~ms). If cell is unpopulated (cold miss): customer falls back to per-NS iterator path for THAT request only; refresher kicks off async populate.

---

## §2 Refresher SLA design

### §2.1 Target freshness — 30 seconds (TRACED justification)

**Target**: cluster_list cells refresh within **30 seconds (median)** of any informer event on the underlying GVR.

**Justification**:
- Existing AC-98.12 (Ship #98 / 0.30.215, refresher.go:432-437): "CRUD-to-completed Δt ≤ 10s under quiescent load". The 10s budget composes `refresherYieldMaxParked` (5s) + actual re-resolve work (≤3s) + queue latency.
- For cluster_list cells the re-resolve work is the 2,024ms decode + Put. So per-cell processing time ~2-3s.
- Under burst (e.g. 100 simultaneous CRUD events across all GVRs hitting refresher), serialised through `RESOLVED_CACHE_REFRESHER_PARALLELISM` workers (default 4): 100 events / 4 workers × 3s = **75s worst-case tail**.
- Picking 30s as the SLA gives **3× safety vs the 10s AC-98.12 floor** while admitting that the worst-case 75s tail under sustained CRUD storm is acceptable (the cluster reaches that state only during a controller storm, where stale-while-revalidate already serves customers from the prior cell content).

**Why not tighter**: a 10s SLA for cluster_list cells would require ≥10 parallel decoders to handle burst, multiplying decode-allocation pressure (174 MB × 10 = 1.7 GiB peak RSS). Empirical 0.30.151 cap-overshoot lesson (`feedback_capacity_caps_empirical_per_entry_cost`) blocks this.

**Why not looser**: at 60s a CRUD-driven UI refresh would feel "broken" to admin users watching their own changes; 30s is at the perceptual freshness floor.

### §2.2 Refresher loop modification — priority queue for cluster_list cells

The existing `workqueue.NewTypedRateLimitingQueue[string]` (refresher.go:160-205) is single-tier FIFO with exponential-backoff rate limiting. Path 3.2 introduces a **two-tier queue** discipline:

- **High-priority tier**: keys carrying `CacheEntryClassApistage` with the `IsClusterListCell` predicate (TRACED via inspecting the cached entry's `Inputs.Namespace == "" && Inputs.Name == ""` after lookup).
- **Low-priority tier**: all other refresh keys (per-user restactions, widgets, per-NS apistage cells).

**Implementation** (TRACED to existing patterns):
- Add a second `workqueue.TypedRateLimitingInterface[string]` field `clusterListQueue` to the `refresher` struct (refresher.go:155-205).
- Add a corresponding `enqueueClusterList(key)` method that the `Deps().SetRefreshHook` callback at refresher.go:280-283 invokes when the dirty-marked key corresponds to a cluster_list cell.
- The worker loop (`processNext` at refresher.go:439) drains `clusterListQueue` first when it is non-empty; falls through to the normal queue otherwise. Mirrors the **prewarm engine's priority-scope discipline** (prewarm_engine.go:280-303) which already has a similar drain pattern.

**Customer-priority yield preserved**: the worker still calls `yieldToCustomer` BEFORE invoking the handler (refresher.go:446-451). Cluster_list cells refresh under the same cooperative yield as per-user cells. **No code in the customer hot path is modified.**

### §2.3 Worst-case refresh latency under burst (TRACED projection)

**Scenario**: customer triggers 100 simultaneous CRUD events on compositions GVR (composition-dynamic-controller install storm, memory `project_composition_install_rbac_scale`).

- Existing dep tracker fires `Deps().refreshHook` for each LIST-scope dependent of the GVR (composition cell + every per-NS apistage cell currently in L1).
- High-priority queue gets the 1 cluster_list cell (compositions, ns="").
- Low-priority queue gets the per-NS apistage cells + every dependent per-user restaction L1 entry.
- High-priority drain: 1 cell × 3s = **3s wall-clock**. Cluster_list cell is fresh again in 3s.
- Low-priority drain: continues in background; per-user cells refresh under their own SLA.

**Worst case is therefore 3s + queue latency ≤ 5s per cluster_list cell**, well inside the 30s SLA target.

**Counter-scenario — admin's first /call AFTER a controller storm but BEFORE the high-priority drain finishes**: customer hits a STALE cluster_list cell. **This is acceptable**: stale-while-revalidate (`feedback_l1_invalidation_delete_only`) says the prior cell content is served; the cell is then refreshed for the next customer. Customer pays cheap UAF prune on stale content. Worst-case **customer-visible** staleness is bounded by the SLA itself (30s).

---

## §3 PIP boot pre-warm

### §3.1 Enumeration — which cells to populate?

PIP already enumerates RBAC cohorts via `cache.EnumerateBindingSetClasses()` (phase1_pip_seed.go:349) and walks the RA + widget sets (`contentPrewarmHarvester` + `navWidgetHarvester`). Path 3.2 adds **one additional pass** that enumerates the cluster_list-eligible cells **independent of cohort**:

- For every RA in `restactionRefs` (the harvested set), enumerate its api[].stage list.
- For every stage with a non-empty `DependsOn.Iterator`: derive the target GVR via `deriveTargetGVRForClusterList` (cluster_list.go:443-500) under the SA context (the SA carries cluster-list permission by construction).
- Skip stages where GVR derivation yields ns=="" already (the iterator is already cluster-scope by construction — nothing to collapse).
- The resulting set is the **cluster_list cell roster**: one cell per (GVR) tuple. Identity-free, cohort-free, per `feedback_l1_per_user_keyed_never_cohort` — these are apistage L1 entries which are explicitly identity-free at the storage layer (apistage.go:14-25).

**Cell count estimate at 50K production scale**: ~30-50 cluster_list-eligible iterator stages across all RAs. After GVR dedup, ~10-15 distinct GVRs. **~10-15 cells**.

### §3.2 Boot cost estimate (TRACED)

Per §1's empirical decode profile, populating one cell costs roughly `envelope_bytes × 11ms/MB × 2` (shape-check + materialise). At 50K scale:

| GVR | Envelope | Cost |
|---|---|---|
| compositions | 174 MB | ~4,050 ms |
| panels | 99 MB | ~2,050 ms |
| configmaps | 30 MB | ~590 ms |
| widgets | ~50 MB | ~1,100 ms |
| apirefs | ~50 MB | ~1,100 ms |
| ~10 smaller | ≤20 MB each | ≤450 ms each = ~4,500 ms |
| **TOTAL (parallel = GOMAXPROCS)** | | **~5s wall-clock with 8 cores** |

(Parallel population is bounded by `runtime.GOMAXPROCS(0)` matching the existing PIP errgroup limit at phase1_pip_seed.go:382-387.)

### §3.3 Boot pre-warm sequencing

Ship 2 / 0.30.196 already made PIP cohort seed **BACKGROUND best-effort AFTER MarkPhase1Done** (phase1_pip_seed.go:33-39). Path 3.2 follows the same pattern but **OPPOSITE ordering** for the cluster_list cell roster:

- The cluster_list cell roster pre-warm runs **BEFORE MarkPhase1Done** as a NEW Phase-1 step (call it Step 7.5). Goal: phase1Done flips ONLY when cluster_list cells are populated.
- **Why before, not after**: cluster_list cells are identity-free and SHARED across all cohorts (10-15 cells total). Populating them once at boot is cheap (~5s) and **eliminates the cold-miss customer-fallback** for the first /call by ANY user.
- **Per-cohort PIP seed** (the existing background pass) STILL runs AFTER MarkPhase1Done — that pass populates the **per-user** L1 entries, of which there are O(cohorts × restactions + cohorts × widgets) = potentially thousands, so it must stay background.

**Boot cost net**: existing pre-Phase-1 ~25-30s baseline + Path 3.2's 5s pre-warm = **~30-35s pod-ready**. Within Ship A.3 / 0.30.179's 8-minute global PIP budget envelope.

**Fallback if Step 7.5 overruns**: a hard `clusterListPrewarmTimeout = 60 * time.Second` cap. If pre-warm doesn't finish in 60s, MarkPhase1Done fires anyway (readiness is not blocked) and per-cell fallback policy (§4) covers the gap.

---

## §4 Customer-priority cache-miss policy

**The core mechanism**: customer /call that hits a cluster_list-eligible RA but finds the apistage cell unpopulated MUST NOT block on the 2-second envelope decode.

### §4.1 Modify `attemptClusterListCollapse` semantics

Current behaviour (cluster_list.go:152-428): `attemptClusterListCollapse` performs the dispatch + decode + Put **synchronously on the customer goroutine** (lines 265-426). The customer pays the 2,024ms decode cost on the cold-miss path.

**Path 3.2 modification** (TRACED to call sites):
1. Before the synchronous defensive prefetch at line 265, **check whether the apistage cell already exists**:
   ```
   contentKey := cache.ComputeKey(contentKeyInputs(gvr, "", ""))
   if _, hit := apistageStore.Get(contentKey); hit {
       // CELL WARM — return the cluster-scope call slice; apistageContentServe
       // will Get-hit at line 479 and the customer pays the cheap UAF prune.
       return []httpcall.RequestOptions{buildClusterListCall(...)}, true, 0
   }
   ```
   This is the **cheap warm path**: pure existence check (sync.Map Load), no decode, no Put. Customer keeps the cluster-scope call.
2. **Cell unpopulated** → DO NOT decode synchronously. Instead:
   - Trigger an **async populate** via the refresher's `clusterListQueue` (enqueueClusterList on the synthesised cell key).
   - Return `(_, false, 8)` — the new deny-gate value 8 = "cell-cold-async-populate-scheduled".
   - The caller (resolve.go:411-418) keeps `useClusterList=false`, falls through to the **per-NS iterator path** (the existing tmp slice), and the customer pays the small per-NS decode cost (~5-10ms × narrow user's 1-2 NS).
   - The refresher picks up the queued key on its next tick (≤25ms yield-poll); by the time the SECOND customer of that cell arrives, the cell is warm.

**Critical property**: the per-NS iterator path is **never deleted** — it remains the fallback for cold misses. `feedback_no_park_broken_behind_flag` applies in spirit: the iterator path is the load-bearing fallback, not a parked-broken alternative.

### §4.2 Cell warmth observability

Per `feedback_validate_content_not_just_status` we need OBSERVABILITY on whether the customer hit a warm cell vs cold-fallback:

- New log marker `cluster_list.cell.warm` on the warm-cell-return path (cluster_list.go new line ~167).
- New log marker `cluster_list.cell.cold_fallback` on the cold-miss-async-populate path (new deny-gate 8).
- New atomic counter `clusterListCellWarmTotal` / `clusterListCellColdFallbackTotal` exposed via the refresher metrics seam.
- AC-P3.2.10 in §6 anchors on the **absence** of `cluster_list.dispatch.cold` (the proposed marker name renamed to `cluster_list.cell.cold_fallback`) once the system is in steady state.

### §4.3 Boot window — first 30s post-pod-ready

In the window between `MarkPhase1Done` and the cluster_list cell roster fully populated (Step 7.5 finished or skipped), **all customer /calls hit cold-fallback**. This is **acceptable** because:
- §3.3 makes Step 7.5 a foreground Phase-1 step in the happy path: the window is empty when boot is clean.
- The 60s pre-warm timeout is the worst case where the window is non-zero. Customer pays the per-NS iterator cost during that window — exactly today's behaviour (`clusterListCollapseEnabled=false`).
- Pre-warm continues to populate cells in background; customer experience improves monotonically as cells warm up.

---

## §5 Pre-flight falsifier (per `feedback_empirical_baseline_gate`)

Three measurements, captured on the candidate image BEFORE production deploy:

### §5.1 Cold-start customer fallback + decode-attribution measurement [C1 SHARPEN 2026-05-31]

**Test**: kill snowplow pod, send the FIRST customer /call before Step 7.5 completes (force pre-warm timeout by setting clusterListPrewarmTimeout to ~0 in a probe build). Repeat for a warm /call AFTER Step 7.5 completes.
**Instrumentation**: emit a per-/call timing tuple `(shape_check_ms, materialise_ms, put_ms, decode_total_ms = sum)` from the resolve path on every /call (existing markers in cluster_list.go for envelope_ok_elapsed / materialise_elapsed already provide the first two; add put_ms + roll-up).
**Expect (cold-miss)**: customer takes the per-NS iterator path; `decode_total_ms` is the SUM across the small per-NS slices the user actually visits (~50-200ms narrow user, well under the 500ms HARD ceiling).
**Expect (warm)**: customer hits the cell-warm fast-path; `decode_total_ms` ≈ 0 (no decode on the customer goroutine).
**HARD HALT (AC-P3.2.1 gate)**: if ANY /call across the probe exhibits `decode_total_ms > 500ms` → HALT (§4 mechanism broken: customer is paying refresher's decode budget).
**Two-sided latency band (secondary)**: warm-baseline ± 200ms total wall-clock. If outside → investigate but decode-attribution is the PRIMARY gate.

### §5.2 Warm-state customer latency

**Test**: send 100 customer /calls AFTER Step 7.5 finished and cluster_list cells are populated.
**Expect**: median customer /call latency MUCH less than per-NS iterator (cluster-wide LIST + per-user UAF prune is cheap; ~5-10ms vs per-NS's 50-200ms × N).
**Two-sided HALT band**: cyberjoker /compositions warm `lastCallEnd_ms` between **800ms** (the north-star floor) and **1,800ms** (the 0.30.215 anchor). If outside → HALT.

### §5.3 Refresher SLA — cluster_list cell refresh latency

**Test**: trigger a CRUD event on compositions (delete one composition CR), measure time-to-`refresher.refresh_completed` for the compositions cluster_list cell.
**Expect**: ≤ 5s median, ≤ 30s p99.
**Two-sided HALT band**: median 0-10s, p99 0-60s. If p99 > 60s → HALT (refresher starvation).

### §5.3 (extended) — Sustained-burst per-user cell refresh tail [C2 BURST FALSIFIER 2026-05-31]

**Why this exists**: §5.3 above measures the cluster_list-cell tier in isolation. The biggest standing risk (R-refresher-bandwidth-saturation, §7) is that the high-priority cluster_list tier starves the low-priority **per-user** cell tier under a sustained CRUD storm. This probe empirically falsifies the mitigation rather than asserting it works.

**Test**: 5-minute sustained burst of 50-100 simulated CRUD events on bench-namespaces (kubectl create/delete configmaps OR equivalent composition churn — choose whichever is feasible against the GKE cluster without disturbing real customer workloads; bench-namespaces are exempt from `feedback_never_kubectl_apply` per its bench-internal carve-out). Event cadence ≈ 1 every 3-6s, spread across both cluster_list-eligible GVRs AND per-user-dependent GVRs.
**Concurrently**: measure per-user cell refresh latency across the burst — sample `refresher.refresh_completed` markers for cells of `CacheEntryClassRestaction` + `CacheEntryClassWidget` (NOT just `CacheEntryClassApistage`). Compute p50 / p95 / p99 of the wall-clock from dirty-mark emission to refresh_completed for the per-user tier.
**HALT band (HARD pass)**: p99 per-user cell refresh ≤ **60s during the burst**. If p99 > 60s → R-refresher-bandwidth-saturation has materialised: stale-while-revalidate is NOT keeping up and per-user cells are starving behind the high-priority cluster_list tier → HALT (the two-tier discipline §2.2 needs widening, e.g. dedicated per-tier worker pools, before Path 3.2 can ship).
**Additional pass criterion**: cluster_list cell refresh p99 during the burst still ≤ 60s (carryover from §5.3 base). Both tiers must hold simultaneously — if cluster_list passes but per-user fails (or vice versa), it confirms tier-skew and HALT applies.
**Methodology constraint**: burst injection via kubectl is acceptable (bench-internal); latency MEASUREMENT for per-user cells reads `refresher.refresh_completed` log markers + atomic counters — these are mechanism evidence, NOT latency-scoring (`feedback_no_kubectl_in_measurement` exempts mechanism observability). Customer-visible scoring (if any during burst) STILL goes via Chrome MCP.

**New acceptance criterion** (added to §6 below as AC-P3.2.13).

**Methodology constraint** (`feedback_no_kubectl_in_measurement`): all three falsifiers' **customer-visible latency** measurements via **Chrome MCP** against portal + ingress. Pod logs / counters read separately for mechanism evidence (NOT for latency scoring).

---

## §6 Acceptance criteria (12 anchored to north-star)

| # | AC | Confidence |
|---|----|------------|
| AC-P3.2.1 | **[C1 SHARPEN 2026-05-31]** Per-/call decode wall-clock attribution ≤ **500ms** (HARD pass). Falsifier §5.1 explicitly measures the decode-time fraction of every customer /call under cold-start AND warm conditions. If any /call shows >500ms attributed to decode work (across shape-check + materialise + put), AC FAILS regardless of total wall-clock. Cold-miss → per-NS fallback for that request keeps per-NS decode attribution well under the 500ms ceiling (narrow-RBAC: ~50-200ms aggregate). | HIGH (mechanism §4) |
| AC-P3.2.2 | Mix-weighted piechart_correct_warm ≤ **1,500ms** (relaxed target — Path 3.2 alone, NOT 800ms). | MEDIUM |
| AC-P3.2.3 | Admin /compositions warm `lastCallEnd_ms` ≤ **4,000ms**. | HIGH (cluster-wide cache hit collapses N×per-NS) |
| AC-P3.2.4 | Cyberjoker /compositions warm `lastCallEnd_ms` ≤ **1,800ms** (PRESERVED — cache hit is cheap UAF prune). | HIGH |
| AC-P3.2.5 | Refresher SLA: cluster_list cells refresh within **30s of informer event (median)**. Falsifier §5.3. | HIGH (mechanism §2) |
| AC-P3.2.6 | When cell warm: `cluster_list.dispatch` log emits with `cell_warm=true`, no per-NS fallback in resolve trace. | HIGH |
| AC-P3.2.7 | PIP Step 7.5 cluster_list pre-warm completes within **60s** (timeout cap). | HIGH (§3.2 estimate 5s) |
| AC-P3.2.8 | Pod restartCount=0 across 30-min burst with 100 simultaneous CRUD events. | MEDIUM (refresher amplification could OOM) |
| AC-P3.2.9 | RBAC 3-cohort sweep PASS (admin, cyberjoker, joker). UAF prune correct per cohort. | HIGH (apistage L1 identity-free contract) |
| AC-P3.2.10 | After 60s post-boot, customer never sees `cluster_list.cell.cold_fallback` log marker (all cells warm). | HIGH |
| AC-P3.2.11 | NO regression on cyberjoker cells from cluster_list activation (key invariant). cyberjoker /compositions cold `firstCallStart_ms` ≤ 1,800ms (matches today). | HIGH |
| AC-P3.2.12 | LCP within **100ms** of piechart_visible_correct_ms (no wave-chain regression). | MEDIUM (Phase B.1 still needed for full close) |
| AC-P3.2.13 | **[C2 BURST FALSIFIER 2026-05-31]** Under 5-min sustained CRUD storm (50-100 events) per §5.3-extended: per-user cell refresh p99 ≤ **60s** AND cluster_list cell refresh p99 ≤ **60s** simultaneously. If either tier's p99 > 60s → HALT (R-refresher-bandwidth-saturation materialised). | MEDIUM (mechanism §2.2 but tier-skew is the dominant residual risk) |

---

## §7 Risk register (6 risks)

| Risk | Severity | Mitigation |
|------|----------|------------|
| **R-pip-boot-too-long**: Step 7.5 cluster_list pre-warm overruns 60s → MarkPhase1Done fires anyway → customers see cold-fallback until cells warm. | MEDIUM | Cells populate via background refresher within ≤30s of pod-ready. Per-NS iterator fallback covers the gap. |
| **R-refresher-bandwidth-saturation**: 10-15 cluster_list cells × 2-4s refresh per cell × frequent CRUD events could saturate the 4-worker refresher pool, starving per-user cell refreshes. | HIGH | Two-tier priority queue (§2.2) ensures cluster_list cells get drained first. Per-user cells refresh under stale-while-revalidate. AC-98.12 budget already validated. |
| **R-cohort-skew**: per-cohort cells (admin vs cyberjoker per-user restactions L1) compete with cluster_list cells for refresher capacity. | MEDIUM | The two-tier queue prevents starvation in either direction. The cluster_list tier has bounded cardinality (10-15 cells); the per-user tier is high cardinality but each cell is cheap (~50-100ms refresh). |
| **R-per-NS-fallback-confusion**: developer might delete the per-NS iterator path believing cluster_list collapse is universal. | LOW | §4.1 explicitly states the iterator path is the load-bearing fallback. Add a code-comment ANCHOR at resolve.go:411 referencing this design doc. |
| **R-cell-eviction-thrash**: apistage L1 TTL-based eviction could evict warm cluster_list cells, causing customer cold-misses. | MEDIUM | Apistage L1 uses dirty-mark invalidation, NOT TTL-based eviction (`feedback_l1_invalidation_delete_only`). Cells stay warm until an informer event marks them dirty; the refresher then refreshes in place (no eviction). **Path 3.2 verifies this contract holds for cluster_list cells.** |
| **R-async-populate-leak**: if customer hits cold-miss and the async populate enqueue fails (queue shutdown, OOM), the cell never warms. | LOW | The refresher workqueue is unbounded by design (refresher.go:23-24). Shutdown path is bounded by ctx cancellation. Worst case: subsequent customers fall back to per-NS path (correct degraded posture). |

---

## §8 LOC envelope (HONEST estimate)

| Area | Production LOC | Test LOC |
|------|---------------|----------|
| `internal/cache/refresher.go` — two-tier queue (clusterListQueue field + enqueueClusterList + processNext drain logic) | ~60 | ~80 |
| `internal/handlers/dispatchers/phase1_clusterlist_prewarm.go` (new file) — Step 7.5 enumerator + roster populator | ~120 | ~100 |
| `internal/handlers/dispatchers/phase1_walk.go` — wire Step 7.5 invocation before MarkPhase1Done | ~10 | (covered by existing) |
| `internal/resolvers/restactions/api/cluster_list.go` — `attemptClusterListCollapse` cell-warm fast-path + cold-miss async-enqueue | ~40 | ~60 |
| Metrics: `clusterListCellWarmTotal` / `clusterListCellColdFallbackTotal` + `/debug/vars` wiring | ~20 | ~20 |
| Total | **~250 production** | **~260 test** |

**Within target envelope of "~200-300 Go LOC + ~150 tests"**; test estimate higher because the two-tier queue invariant + the cell-warm fast-path each need their own falsifier suite.

---

## §9 Rollout

**Single binary tag**: `0.30.218` (Path 3.1's `0.30.217` tag exists but image is NOT in production; preserve tag-to-meaningful-commit per `feedback_tag_commits`).
**Chart**: lockstep `0.30.218` (`feedback_chart_release_lockstep`).
**Push**: braghettos EXPLICIT push for chart (`feedback_chart_repo_origin_is_upstream`).
**No env vars** (`project_single_cache_flag_direction`): all behaviour gated on existing `CACHE_ENABLED` only. The `clusterListCollapseEnabled` package var stays as a build-time test hook; production path is always true once 0.30.218 deploys.
**Helm**: full `--set` override (`feedback_helm_no_reuse_values_on_chart_default_change`).
**Verification context**: kubectl context = GKE (`feedback_kubectl_verify_gke_context`).

---

## §10 Revert plan

**Clean tag rollback** (`feedback_no_park_broken_behind_flag`):
- `helm upgrade snowplow ... --version 0.30.215` (the last known-good — Path 3.1 0.30.217 was rolled back at 17:13Z per `docs/path3-1-precommit-diff-2026-05-31.md`).
- The package-level `clusterListCollapseEnabled = true` source value reverts via the snowplow tag rollback; no env-flag toggle needed.
- **NOT a flag toggle**: per the project memory, env-toggling correctness defects is forbidden. A revert = a tag downgrade.
- Detection criterion: ANY of the §6 ACs FAIL on canonical Chrome MCP → trigger revert immediately, do not chase root-cause in production.

---

## §11 Honest delta projection (TRACED vs INFERRED labels)

| Metric | Baseline | Path 3.2 projection | Confidence |
|--------|----------|---------------------|------------|
| Mix-weighted piechart_correct (warm) | 1,798ms | **~1,400ms** | INFERRED — depends on cell-warm rate. If 100% warm at customer arrival, drop is large (admin compositions stage collapses N×per-NS into 1×cache-hit). If cells are stale-while-revalidate, modest drop. |
| Cyberjoker /compositions warm | 1,731ms | **~1,700ms (flat)** | TRACED — cache-hit case for cluster_list cell is a cheap UAF prune, comparable to per-NS path (cyberjoker has narrow RBAC → 1-2 NS, today is already fast). No regression expected. |
| Admin /compositions warm | 9,408ms | **~3,500ms** | INFERRED — admin's iterator fans across ~50 namespaces; cluster_list collapse reduces this to 1 LIST + per-user UAF prune. Decode cost (~2s) is REFRESHER-paid, customer pays only UAF. **HIGH confidence** vs the 4s AC ceiling. |
| Cold-start (Step 7.5 timed out) cyberjoker /compositions | n/a | **~1,800ms** | TRACED — falls back to per-NS iterator, byte-identical to today's path. |

**Does Path 3.2 close the 800ms north-star alone? HONEST ANSWER: NO.**

Path 3.2 collapses the iterator fan-out cost (multiple per-NS LISTs → 1 cache hit) and eliminates the customer-decode cost. But the **piechart_correct** wave-chain (compositions /call → blueprint /call → panels /call → render) is still **3-4 sequential customer /calls × ~400ms each = ~1,500ms wall-clock**. To reach 800ms, **Phase B.1 (#103 — widget-tree pre-fetch)** is needed to collapse the wave-chain into one /call-tree response.

**Path 3.2 + Phase B.1 TOGETHER project to hit 800ms**. Path 3.2 alone is a STEP toward, not a CLOSE OF, the north-star.

---

## §12 Hard rules attestation

- TRACED vs INFERRED labels: present throughout §1, §2.3, §3.2, §6, §11.
- Empirical evidence: §1 quotes verbatim from 0.30.217 prod-marker probe.
- No new env vars / flags: §9 confirms `CACHE_ENABLED` only.
- No special-cases in resolver: §4.1 modifies `attemptClusterListCollapse` uniformly across all RAs; no per-RA / per-GVR / per-cohort branching.
- Customer-priority invariant: §2.2 preserves yield, §4.1 mechanism ensures customer NEVER blocks on decode.
- No code in this dispatch: design only.
- Read-only on code: confirmed.

— cache-architect
