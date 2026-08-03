# Path 3.2 — Phase 0 baselines (snowplow 0.30.215)

**Author**: cache-developer
**Date**: 2026-05-31
**Context**: GKE `gke_neon-481711_us-central1-a_cluster-1` — VERIFIED kubectl context.
**Snowplow**: helm rev 373 (chart 0.30.215, image 0.30.215). Pod 51m uptime, RESTARTS=0.
**Flag state**: `clusterListCollapseEnabled = true` in source (set at 0.30.216) — but image 0.30.215 was BEFORE the flag flip, so cluster_list mechanism is OFF in production (per design §1; verified by zero `cluster_list.dispatch` markers in current pod logs).

## Baselines (from `docs/chrome-mcp-north-star-reanchor-2026-05-31.md`)

These are the canonical Chrome MCP measurements taken on the SAME 0.30.215 image now running in production. Used as Path 3.2 reference values.

### AC-P3.2.1 reference — decode-attribution per /call

Today on 0.30.215 (cluster_list OFF): customer /call decode-attribution is the per-NS iterator path's small per-NS Unmarshal cost.

- cyberjoker /compositions warm `lastCallEnd_ms` = **1,610 ms** (total wall-clock across 10 /calls; per-/call median = 247 ms; max = 453 ms)
- decode-attribution per /call (TRACED from §1 of design doc): ~50-200 ms aggregate per /call for narrow-RBAC users (1-2 NS × 5-10 ms decode each)

**Path 3.2 AC-P3.2.1 gate**: ≤ 500 ms decode-attribution per /call on EVERY /call (cold AND warm). Baseline today is well under (≤200 ms).

### AC-P3.2.4 cj /compositions warm reference (PRESERVE — no regression)

- cyberjoker /compositions warm `lastCallEnd_ms` = **1,610 ms** (n=1 session; median across 10 /calls = 247 ms)
- cyberjoker /compositions cold `lastCallEnd_ms` = **2,252 ms** (n=1)
- AC-P3.2.4 ceiling = **1,800 ms warm** (design §6 — slight headroom over 1,610 baseline)
- AC-P3.2.11 (no-regression invariant) = match today's cold floor ≤ 1,800 ms warm / ≤ 2,300 ms cold

### AC-P3.2.3 admin /compositions warm reference

- admin /compositions warm `lastCallEnd_ms` = **13,055 ms** (n=1, 241 /calls fan-out)
- AC-P3.2.3 ceiling = **4,000 ms warm** (design §6 — projection §11 = 3,500 ms)
- This is the BIG win lever: cluster_list collapse drops the 241 per-NS LISTs to ~10-15 cell hits.

### AC-P3.2.2 mix-weighted piechart_correct warm reference

- cj warm `piechart_visible_correct_ms` median (n=2) = **1,734 ms**
- admin warm `piechart_visible_correct_ms` ≈ **1,512 ms** (n=1, compositions-list /call ≈ 1,512 ms inside the dashboard wave)
- Mix-weighted (0.95 cj + 0.05 admin) = 0.95 × 1734 + 0.05 × 1512 = **1,723 ms** (canonical reanchor)
- AC-P3.2.2 ceiling = **1,500 ms warm** (relaxed per design §11 — Path 3.2 alone INFERRED-may-plateau, full close needs Phase B.1)

### Mix-weighted lastCallEnd (warm dashboard)

- cj warm dashboard `lastCallEnd_ms` median (n=2) = **1,740 ms** (1589 + 1892 / 2)
- admin warm dashboard `lastCallEnd_ms` ≈ **1,800 ms** (no formal number — derived from cj path)
- Mix-weighted ≈ **1,743 ms**

## Verification

GKE context = `gke_neon-481711_us-central1-a_cluster-1` (kubectl config current-context output verified at Phase 0 start).
Snowplow image = `ghcr.io/braghettos/snowplow:0.30.215` (`kubectl get pod` confirmed).
Helm chart = `snowplow-0.30.215` (helm list confirmed).
Pod restartCount = 0 across 51m uptime (`kubectl get pods -o wide` confirmed).
No `cluster_list.dispatch` markers in current logs (mechanism is OFF in production — flag flip is in source but only 0.30.216+ image ever activated it).

## Path 3.2 ACs anchored to these baselines

| AC | Baseline (0.30.215) | Target (Path 3.2) | Direction |
|----|---------------------|-------------------|-----------|
| AC-P3.2.1 decode-attribution | ~50-200 ms (per-NS iterator) | ≤ 500 ms HARD | preserve |
| AC-P3.2.2 piechart warm mix | 1,723 ms | ≤ 1,500 ms (INFERRED-may-plateau) | reduce |
| AC-P3.2.3 admin /compositions warm | 13,055 ms | ≤ 4,000 ms | reduce 70% |
| AC-P3.2.4 cj /compositions warm | 1,610 ms | ≤ 1,800 ms | preserve |
| AC-P3.2.11 cj no-regression | 1,610 ms warm / 2,252 ms cold | preserve (no >5% regression) | preserve |
| AC-P3.2.12 LCP vs piechart-visible | TBD | within 100 ms | preserve |

— cache-developer (Phase 0 complete)
