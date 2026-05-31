# Ship S.2 — PM Gate Verdict

Author: cache-pm. Date: 2026-05-31. Subject: `docs/ship-s2-cluster-list-cohort-memo-design-2026-05-31.md`.

## Decision: CONDITIONAL ACCEPT

S.2 is a sound enablement+safety-wiring ship of an already-shipped mechanism. The architect's counter-result (the 11s lastCallEnd is portal SPA fan-out, NOT snowplow-tractable) is supported by the 0.30.212 JSON ground-truth and ratified team policy. S.2's value proposition must be reframed and three pre-flight conditions must be discharged before dev breaks ground.

## Counter-result reconciliation

**Counter-result HOLDS.** Verified independently against `e2e/bench/0.30.212_phase6_results.json`:

- `chrome_mcp_f5_admin_compositions_warm.lastCallEnd_verdict` (line 85): `"AMBER (within 50K + 241-call SPA-fanout noise envelope; frontend architecture, not snowplow-tractable per feedback_compositions_north_star_views)"`.
- `ledger_row_diff_vs_0_30_211.lastCallEnd_p50_ms_admin.verdict` (line 119): `"regression within noise envelope (frontend SPA fanout, not snowplow-tractable)"`.
- `chrome_mcp_f5_admin_compositions_warm.total_calls = 241` (line 88): the SPA fans out into 241 child `/call`s; lastCallEnd is the SLOWEST of those 241 per-widget calls, not a single snowplow call.
- `canonical_timings_warm_curl_lb` (lines 32-66): every direct snowplow `/call` already meets the north-star target. `compositions_list` mix-weighted p50 = 749ms < 1000ms. There is no 11s single-call to attack on snowplow's side.

**Implication for ship value prop**: S.2 does NOT close the 11s lastCallEnd. It closes the per-call cost on `compositions-panels` (and `blueprints-panels`) stage-2, one of the 241 children, by collapsing ~49 per-NS LISTs to 1 cluster-LIST. Best-case mix-weighted impact: a few hundred ms shaved off the slowest child(ren) of the 241-call fanout. The 11s tail is the architecture of the portal SPA (parallel-then-serial widget loaders) and remains owned by chart/portal, not snowplow.

**Diego decision point** (raised, not blocked): S.2 still merits a single ~180 LOC ship because (a) the mechanism is already 95% shipped behind an inert gate; (b) per-call latency reduction is real and falsifiable; (c) refresher-bound is a defensive improvement orthogonal to per-call latency. But the success criteria below MUST be reframed away from "close 11s tail" and toward per-call dispatch-delta + per-call p50 on `compositions-panels` specifically.

## Acceptance criteria (≥5, falsifiable)

All baselines below are versus 0.30.212 captured 2026-05-31 (`0.30.212_phase6_results.json`).

1. **AC-S2.1 — dispatch_delta collapse on warm `compositions-panels`**: After 3 warm-up calls + 20 sample calls of `/call?…&name=compositions-panels&namespace=krateo-system` as admin on production scale (50K compositions, 50 bench namespaces), the delta in `snowplow_apiserver_fallthrough_cells[*panels*]` is **≤ 5 total** across the 20 sample calls (1 cluster-LIST dispatch + at most 4 cohort-memo misses). 0.30.212 baseline = expected delta ≈ 1000 (50 per-NS LIST × 20 calls; pre-fix, INFERRED from RA fan-out trace at resolve.go:484, design §2.1). Decisive falsifier per design §6.3.
2. **AC-S2.2 — per-call p50 on `compositions-panels` warm path**: p50 of 20 sample calls ≤ 250 ms at 50K production scale (vs INFERRED ~0.5–2s pre-fix, design §6.2). If the snowplow side cannot achieve a p50 step, the ship has no per-call benefit and must be revert-candidate.
3. **AC-S2.3 — predicate symmetry test passes for User+Group+SA subjects**: New `cluster_list_uaf_symmetry_test.go` (design §5) — three sub-tests with identical RBAC mass on distinct subject kinds (User, Group, ServiceAccount) MUST produce identical kept-set sizes AND assert exactly 1 cluster-LIST dispatch via `snowplow_apiserver_fallthrough_cells` delta. Falsifier for ζ-class HARD REVERT regression (memory: feedback_predicate_subject_kind_symmetry).
4. **AC-S2.4 — refresher amplification under sustained burst**: At sustained 1 panel ADD/sec × 5 minutes against the production-scale cluster, `refresherSkippedClusterListBudget` climbs by ≥ 250 (more than 80% of burst events skipped by the byte-budget+min-interval gate); the cluster-LIST cell's `lastUpdatedAtUnixSeconds` updates at most every 30s. `snowplow_dispatcher_inflight` p99 over the 5-minute window ≤ 2× 0.30.212 steady-state baseline (steady-state baseline captured during this same probe pre-flip). 0 pod restarts, 0 OOM, 0 panic markers in pod log during the window. Falsifier per design §7.2.
5. **AC-S2.5 — CACHE_OFF transparency**: With `CACHE_ENABLED=false`, the same admin `/call?…&name=compositions-panels` returns content byte-identical (modulo timestamps) to CACHE_ENABLED served content. The cluster-LIST collapse short-circuits at `cache.Disabled()` (cluster_list.go:156-158); fan-out reverts to per-NS dispatch via per-user kubeconfig. Falsifier: byte-diff of the response body between the two modes is empty under `jq -S 'del(.. | .resourceVersion?)'` normalisation.
6. **AC-S2.6 — F-4 freshness on cluster-LIST cell**: After CREATE of one new panel in a new namespace under load, the next `/call?…&name=compositions-panels` reflects the new item in ≤ 5s (target) / ≤ 10s (ceiling). 0.30.212 baseline `f4_freshness_validation` = 0.34s for a peer cluster-LIST cell. Verifies design §4.3 + §10 R6 (cluster_list.go:338 RecordList) holds under the flipped gate.
7. **AC-S2.7 — concurrent isolation `-race` test passes**: New `cluster_list_uaf_symmetry_test.go` MUST run under `go test -race` and pass. Per `feedback_shared_vs_copy_is_a_concurrency_change`: enabling the cluster-list collapse converts the per-NS-LIST private-copy iterator path to a shared `entry.Items` aliased path served via `cohortGateMemoServe` (apistage_cohort_memo.go:376-417); this is a concurrency change, NOT a content-equivalence change, and requires a concurrent-race test against parallel cohorts on the same cell.

## PM gate question discharge

1. **Would the fix make the symptom disappear?** — PARTIAL.
   - For the ship-stated symptom of "close 11s lastCallEnd": NO. The 11s is portal SPA architecture (241 child calls). Counter-result holds.
   - For the snowplow-tractable symptom of "stage-2 per-NS fan-out on compositions-panels": YES. Mechanism trace: gate fires at `resolve.go:411` (BEFORE the bounded errgroup at resolve.go:467-469), `attemptClusterListCollapse` (cluster_list.go:138-367) returns a one-element `tmp` slice, the existing iterator loop runs ONE iteration against the cluster-scope path, the un-gated envelope is Put under the identity-free apistage key (cluster_list.go:329), and downstream `apistageContentServe` runs `gateListItemsWithMemo` (apistage_cohort_memo.go:141-206) for the per-cohort prune. Verified on the live cluster: the inert gate today fires at cluster_list.go:151-153 returning `denyGate=1`; flipping `clusterListCollapseEnabled=true` (cluster_list.go:85) makes ALL FIVE gates evaluate. The new UAF-derivation helper (~80 LOC, design §4.1) is the only piece that has NOT been live-exercised — Gate 4 (`deriveTargetGVRForClusterList`) today fails on `compositions-panels` because the iterator element is a bare namespace string (cluster_list.go:382 expects path-resolving to succeed from the iterator element, which is a `.metadata.name` string, not a path). Without the UAF-derivation second path, ONLY RAs whose iterator elements include the full path qualify — `compositions-panels` does NOT qualify today even with the gate flipped. **The UAF-derivation helper is LOAD-BEARING; the gate flip alone is insufficient.** This is the heart of S.2.
2. **Is the symptom even what the brief claimed?** — NO. See "Counter-result reconciliation" above. Design §0 line 10 surfaces this honestly. Counter-result is preserved as part of the ship.
3. **Are the empirical traces TRACED, not INFERRED?** — Audit verdict:
   - `clusterListCollapseEnabled=false` at cluster_list.go:85 — TRACED + verified by Read.
   - Inert gate at cluster_list.go:151-153 — TRACED + verified.
   - `attemptClusterListCollapse` call site at resolve.go:411-419 — TRACED + verified.
   - `errgroup` parallel fan-out at resolve.go:467-469 — TRACED + verified.
   - `cache.Deps().RecordList(contentKey, gvr, "")` at cluster_list.go:338 — TRACED + verified.
   - `cohortGateMemo` shape at apistage_cohort_memo.go:106-118 — TRACED + verified.
   - RA spec at §2.1 — TRACED + verified live via `kubectl get restaction compositions-panels -n krateo-system -o yaml`.
   - Wire-shape probes at §2.4 — TRACED, captured 2026-05-31 against gke_neon-481711_us-central1-a_cluster-1.
   - INFERRED items are clearly labeled (design §6.2 timings, §2.5 widget-content inference). Acceptable per the design's own falsifier-before-code stance.
   - **One TRACED gap**: design claims `LastRefreshedAt()` as a new `atomic.Int64` field on `ResolvedEntry` (§7.2), but `processOne` (refresher.go:351-389, verified) currently has no such accessor. This is design intent, NOT a code claim — but the design should explicitly label it as NEW CODE rather than TRACED.
4. **Does the falsifier signal capture the production-customer path?** —
   - (a) Portal dispatch match: the portal `/compositions` page does fan out 241 child `/call`s (TRACED in 0.30.212 JSON line 88), of which `compositions-panels` is one. The architect's `curl /call?…compositions-panels` probe DOES match the customer dispatch for that one child. It does NOT measure portal-side timing for the other 240 children; that's appropriate (S.2 doesn't claim to fix them) but the success criteria must NOT regress them either — see AC-S2.4 dispatcher_inflight constraint.
   - (b) Signal-to-noise: HIGH. Single-curl probe + expvar delta + p50 step is decisive. 0.30.212 cluster has been 5.4h stable with 0 restarts (line 11); noise floor is low. Design §6.3 acceptance bounds (dispatch_delta>5 OR p50_s>0.25s = REGRESSION) are correctly tight.
   - (c) Post-fix expectation testable today: YES. `snowplow_apiserver_fallthrough_cells` expvar exists today (TRACED in §2.5); `snowplow_cluster_list_collapse_used_total` is a new sibling (design §6.2) that must be added as part of the ship — flag as a small-but-real code line item.

## Risk register validation

| # | Validation |
|---|------------|
| **R1 — Refresher amplification** | Mitigation: byte-budget + min-refresh-interval gate. **FALSIFIABILITY: ACCEPTABLE WITH CONDITION**. The 50 MiB default byte-budget is a design-time estimate. Per memory: feedback_capacity_caps_empirical_per_entry_cost (0.30.151 was 180× off), the value MUST be re-derived from empirical per-entry cost measurement on the 50K cluster BEFORE the dev writes it as the default. The cluster-LIST envelope at 50K admin-cohort scale is documented as 363 MB (cluster_list.go:248 comment / design §3) — so any entry > 50 MiB is gated, which is correct in principle. But the architect MUST capture an empirical envelope size for the current 50K + bench_namespaces=50 production state and confirm 50 MiB is the right knee. **CONDITION 1 (see below).** Mechanism falsifier (refresherSkippedClusterListBudget climbs ≥250 over 5min) is testable. |
| **R2 — Cohort memo miss-storm at cold start** | Mitigation: CohortNSACL fast-path + snapshot-publish-before-serve. Falsifiability: ACCEPTABLE. Snapshot-publish is verifiable at boot via existing prewarm phase1 ordering (TRACED via existing seed engine). Per-item EvaluateRBAC only fires on the `populateMemoFromCanonicalFilter` fallback (snapshot nil pre-readiness). Mitigation is structural, not flag-gated. |
| **R3 — GET-by-name partial-shape false-positive** | Mitigation: existing `validateClusterListShape` (cluster_list.go:542-600). Falsifiability: ACCEPTABLE. Existing test in `cluster_list_test.go`; design proposes one new sub-test for empty-cluster case. Low risk. |
| **R4 — Predicate-symmetry regression (ζ-class)** | Mitigation: AC-S2.3 above. New test is decisive on ANY refactor. Falsifiability: STRONG. |
| **R5 — L1 cross-user leak via shared parsed.items** | Mitigation: Ship 2a (0.30.209) shallow-alias contract + `-race` test in AC-S2.7. Falsifiability: STRONG with the -race condition. |
| **R6 — F-4 freshness gap on cluster-LIST cell** | Mitigation: `cache.Deps().RecordList(contentKey, gvr, "")` at cluster_list.go:338 (verified TRACED). 0.30.212 falsifier (f4_freshness_validation = 0.34s) already validated this for a peer cluster-list cell. Falsifiability: STRONG, AC-S2.6 makes it concrete for compositions-panels specifically. |
| **(implicit) R7 — UAF-derivation helper introduces a bug not present in `deriveTargetGVRForClusterList`** | NOT in design's register. The new ~80 LOC `deriveTargetGVRForClusterListFromUAFStage` is the load-bearing NEW CODE in S.2. It reads from sibling-stage `userAccessFilter` to derive (group, resource); a wrong-sibling match would silently mis-target the collapse to the wrong GVR. **CONDITION 2 (see below)**: design must add a unit test that exercises (a) the correct sibling match for compositions-panels, (b) refusal-to-derive when no sibling has matching verb=list, (c) refusal-to-derive when the iterator path template references a GVR DIFFERENT from the sibling's userAccessFilter (e.g. a malicious or buggy RA). |

## Conditions for acceptance

1. **CONDITION 1 (R1 byte budget empirical)**: Before dev writes the `CLUSTER_LIST_REFRESH_BYTE_BUDGET` default in code, capture on the 50K production cluster the empirical envelope size of the `compositions-panels` cluster-LIST cell post-flip in a candidate-binary probe. Set the default to `min(empirical_envelope × 0.75, 100 MiB)` so the gate fires reliably on the real cluster-LIST cells but does NOT mis-classify normal-size apistage entries. Attach the probe artifact to the ledger row. Per memory: feedback_capacity_caps_empirical_per_entry_cost (0.30.151 was 180× off).
2. **CONDITION 2 (R7 UAF-derivation helper test)**: Add a unit test for the new `deriveTargetGVRForClusterListFromUAFStage` helper with three sub-tests: (a) correct sibling match for compositions-panels shape, (b) no-match-no-derive when sibling userAccessFilter has `verb!=list`, (c) no-match-no-derive when the iterator path template's GVR ≠ sibling's userAccessFilter resource. This is REQUIRED before commit, NOT post-ship validation.
3. **CONDITION 3 (success criteria reframed)**: Update design doc §0 line 3 + Executive summary to explicitly state: "S.2 does NOT close the 11s lastCallEnd (architectural portal SPA fan-out). S.2 reduces per-call cost on the compositions-panels and blueprints-panels children of that fan-out." This must be in the ship's commit message and ledger row so the team and Diego do not retroactively grade S.2 against a target it never claimed.
4. **CONDITION 4 (concurrent -race test)**: AC-S2.7 must be a hard pre-merge gate, not an after-thought. Ship 0.30.209's shallow-alias contract is load-bearing here. CI must run `go test -race ./internal/resolvers/restactions/api/...` on the symmetry test file specifically.
5. **CONDITION 5 (rollback discipline)**: Per memory: feedback_no_park_broken_behind_flag — `clusterListCollapseEnabled` is being flipped to `true`, not introduced as a new flag. The revert plan (design §9.4: flip to `false` in 0.30.214) is acceptable BECAUSE the var was already there for test-injection. **DO NOT** add new fall-back flags or env knobs in S.2 to "soften" the flip. If the falsifiers fail, revert by flip-and-tag, not by adding deprecation surface. End-state remains one CACHE_ENABLED per memory: project_single_cache_flag_direction.

## Rollout sign-off

- **Single binary**: confirmed. ~180 LOC, single ship.
- **Single flag**: confirmed. `CACHE_ENABLED` is the only customer-visible flag. `clusterListCollapseEnabled` is a package-level var NOT exposed to env (cluster_list.go:74-77, design §8). No new env vars exposed to chart.
- **Chart values changes**: NONE per design §9.3. Confirmed by audit of design — no `helm` values touched.
- **Lockstep chart tag**: REQUIRED per memory: feedback_chart_release_lockstep + feedback_chart_repo_origin_is_upstream. Even though chart values are unchanged, the chart's `appVersion` must move to `0.30.213` and be pushed to braghettos explicitly. No new env knobs to wire into the chart.
- **Two new tunables (CLUSTER_LIST_REFRESH_BYTE_BUDGET, CLUSTER_LIST_MIN_REFRESH_INTERVAL_S)**: design §7.2 declares them as env knobs read inside `refresher.go`. ESCALATION CHECK: if these are read via `os.Getenv` inside snowplow with code-side defaults (50 MiB / 30s), they do NOT require chart wiring. If they are intended to be operator-tunable, they DO. **Resolution**: PM requires the defaults to be CODE-SIDE constants with NO chart surface; the env-read fallback is acceptable for SRE bench/diagnostics only, not for customer-visible operator tuning. Design must clarify this in §7.2 and §9.3 before commit.
- **Ledger row**: must be appended on ship per memory: feedback_maintain_feature_journal + project_north_star_ledger. Expected delta: per-call cost on compositions-panels child, NOT lastCallEnd. Mix-weighted impact: marginal (still GREEN).

## Falsifier load-bearing assessment

The architect's pre-flight falsifier (design §6) IS load-bearing for the ship: dispatch_delta>5 or p50_s>0.25s on the post-fix probe = REGRESSION = revert. This is the right falsifier for AC-S2.1 and AC-S2.2. It does NOT cover refresher amplification (AC-S2.4) which requires a separate sustained-burst probe — design §7.2 specifies it, but the dev must capture BOTH probes pre-merge, not just §6.

---

## Summary

CONDITIONAL ACCEPT. S.2 is structurally sound, mechanism is 95% already shipped, falsifier is HIGH signal-to-noise. The architect honestly surfaced the counter-result; the brief premise must be updated, not the ship discarded. Five conditions (empirical byte-budget, UAF-derivation unit test, success-criteria reframe, mandatory -race CI gate, no-new-flags rollback) must be discharged before dev breaks ground. R1 (refresher amplification) is the dominant risk and the byte-budget MUST be empirically grounded — Phase B 0.30.185 + 0.30.151 lessons are explicit memory.

— cache-pm
