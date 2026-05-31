# Path 3.2 — PM Gate Verdict

**Author**: cache-pm
**Date**: 2026-05-31
**Design audited**: `docs/path3-2-cluster-list-prewarm-refresher-discipline-2026-05-31.md`

## Decision: CONDITIONAL ACCEPT

Mechanism is sound; AC table needs sharpening and two falsifier conditions strengthening before dev dispatch.

## Refresher SLA verification (30s median achievable?)

YES — but the 75s p99 math (100 events / 4 workers × 3s) covers ONLY the cluster_list tier in isolation. Under the composition-install storm (`project_composition_install_rbac_scale`), 100 compositions CRUD events fire dependent dirty-marks across cluster_list + per-NS + per-user cells. Two-tier queue (§2.2) makes cluster_list cells preempt per-user work, so the **cluster_list-cell** SLA holds. The 30s median is defensible against AC-98.12's 10s quiescent floor because cluster_list per-cell cost (2-3s) is 30× per-NS cell cost.

## PIP pre-warm boot cost verification (~5s realistic?)

CONDITIONALLY YES. Per §3.2: 8 cores × ~11ms/MB sequential cost = ~5s wall-clock assumes GOMAXPROCS=8 AND no contention with PIP cohort seed (which §3.3 puts AFTER MarkPhase1Done — good ordering). Memory pressure 5×174 MB transient = ~870 MiB; pod headroom adequate. **Caveat**: Phase 1 baseline is already 25-30s; +5s to ~30-35s pod-ready is acceptable but the 60s timeout cap (§3.3) is essential safety.

## Cache-miss fallback policy verification (customer-priority preserved?)

YES. §4.1 mechanism is correct: `sync.Map.Load` existence check is ns-scale (~ns), zero decode. Cold-miss returns `(_, false, 8)` → resolve.go:411 keeps `useClusterList=false` → per-NS iterator path executes for THAT request. Per-NS path is preserved (§4.1, R-per-NS-fallback-confusion mitigation). **Verified**: no thundering herd risk because per-NS cost on cyberjoker is ~50-200ms (already today's fast path).

## Two-tier queue vs customer-priority yield order

PRESERVED. §2.2 explicitly states `yieldToCustomer` is called BEFORE handler invocation at refresher.go:446-451. The two-tier drain happens INSIDE `processNext` priority selection, which runs AFTER park-on-customer-load yield. Ship #98 invariant intact.

## R-refresher-bandwidth-saturation mitigation testable?

PARTIALLY. §5.3 falsifier covers single-cell refresh latency, NOT sustained-burst starvation of per-user tier. **CONDITION 3** below adds this.

## Acceptance criteria (12 scored)

12 ACs present in §6. AC-P3.2.2 honestly tagged "Path 3.2 alone NOT 800ms" (matches §11 disclosure). AC-P3.2.5 anchored on §5.3 falsifier. AC-P3.2.11 explicit cyberjoker no-regression invariant.

**Gaps**:
- AC-P3.2.1 needs an empirical decode-attribution threshold: "if any /call shows >500ms decode wall-clock attribution to envelope-unmarshal, fix violated".
- AC-P3.2.2 needs "INFERRED-may-plateau" tag mirroring Path 3 C1 honesty.

## PM gate questions (5 discharges)

1. **Symptom disappears?** YES — Path 3 +50.2% regression caused by customer paying 2,024ms decode; Path 3.2 §4 redirects cold-miss to per-NS path (~50-200ms). Trace verified.
2. **AC-P3.2.1 empirically testable?** YES with §5.1 falsifier; CONDITION 1 adds explicit decode-attribution threshold.
3. **800ms honestly disclosed?** YES — §11 explicitly states Path 3.2 + Phase B.1 together hit 800ms; AC-P3.2.2 ceiling raised to 1,500ms.
4. **Risk register complete?** 6 risks listed; R-refresher-bandwidth-saturation correctly flagged HIGH.
5. **LOC envelope?** 250 prod + 260 test = 510. Pause at 1,020 (2×). Reasonable.

## Risk register validation

6 risks (one above 5 minimum); HIGH severity correctly assigned to R-refresher-bandwidth-saturation. R-cell-eviction-thrash references the dirty-mark contract (`feedback_l1_invalidation_delete_only`) — verified consistent with project memory.

## Conditions for acceptance (CONDITIONAL — numbered)

1. **AC-P3.2.1 sharpened**: add empirical threshold "decode-attribution per /call ≤ 500ms wall-clock" measured via per-call timing log. PASS criterion is HARD (not advisory).
2. **AC-P3.2.2 tagged INFERRED-may-plateau**: mirror Path 3 C1 discipline. The 1,400ms projection in §11 is INFERRED; if measured value plateaus at ~1,500ms acceptance still passes.
3. **§5.3 falsifier expanded to sustained-burst**: add a 5-minute storm probe (50-100 CRUD events spread over 5min) measuring per-user cell refresh tail. Two-sided HALT band: per-user-cell p99 ≤ 60s; if > 60s → HALT (per-user tier starvation by cluster_list tier).
4. **Pre-flight falsifier FIRST**: per `feedback_falsifier_first_before_ship`, capture §5.1 + §5.2 + expanded §5.3 BEFORE commit. Attach artifacts to ledger row.
5. **Three-way pre-commit ACK** (`feedback_dev_review_with_architect_pm_before_commit`): dev shares diff with architect + PM before tag push.
6. **Per-goroutine post-fix evidence** (`feedback_per_goroutine_evidence_beats_cpu_pprof`): under sustained burst, dump `go tool trace -d=parsed` and verify customer goroutines are on network-wait, NOT scheduler-delay, during cluster_list refresher activity.
7. **Empirical boot pre-warm measurement**: §3.2's ~5s parallel claim must be verified on actual GOMAXPROCS pod with real envelopes. If wall-clock > 15s, raise `clusterListPrewarmTimeout` to 90s OR accept the gap with §4.3 fallback active.

## Rollout sign-off

- Tag: 0.30.218 (single binary, clean — preserves `feedback_tag_commits`). Path 3.1's 0.30.217 superseded.
- Chart: lockstep 0.30.218 (`feedback_chart_release_lockstep`); explicit braghettos push (`feedback_chart_repo_origin_is_upstream`).
- NO new env vars (`project_single_cache_flag_direction`): `CACHE_ENABLED` only. Two-tier queue internal. `clusterListCollapseEnabled` stays as build-time test hook; production = always true.
- Revert: tag rollback to 0.30.215 (NOT flag toggle); per-NS iterator path always available as fallback (`feedback_no_park_broken_behind_flag` honored — fallback is load-bearing, not parked-broken).
- LOC pause: 2× envelope = 1,020 total.
- RBAC 3-cohort sweep (admin / cyberjoker / joker) mandatory at AC-P3.2.9.
- Chrome MCP only for latency scoring (`feedback_no_kubectl_in_measurement`).
- GKE context verification on EVERY kubectl probe (`feedback_kubectl_verify_gke_context`).
- Two-sided HALT bands on §5.1, §5.2, §5.3 falsifiers.

— cache-pm
