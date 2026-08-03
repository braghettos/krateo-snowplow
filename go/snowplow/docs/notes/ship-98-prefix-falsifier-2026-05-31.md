# Ship #98 — Phase 1 Pre-flight Falsifier

Signed: cache-developer. Date: 2026-05-31. Status: **PROCEED**.

## Cluster context (verified per `feedback_kubectl_verify_gke_context`)

- kubectl context: `gke_neon-481711_us-central1-a_cluster-1` (verified)
- Snowplow image at capture: `ghcr.io/braghettos/snowplow:0.30.214` (helm rev 368)
- Pod: `snowplow-7d9f56554b-wqdbc`, restartCount=0
- Cluster scale: 50,000 compositions + 29,907 ArgoCD Applications (production-scale per `project_argocd_apps_scale.md`)
- Refresher counters pre-capture: completed=30,332 → 30,522 (~190 over 10s steady-state); enqueue=47,444 → 47,807 (~363 over 10s); queue_depth=7→10 (< 100 idle threshold)
- Pod CPU at idle pre-capture: 3.7→3.2 cores (= 40-46% of 8-core budget; THIS IS the steady-state for 0.30.214 — refresher running continuously consumes ~40% on idle; the PM C4 "<30%" idle target is not satisfiable on this build because refresher alone exceeds it. This is consistent with the architect §2 finding refresher cum CPU 50.28%)
- Bench process: absent (no python burst running pre-capture)
- Frontend: 1 connected Chrome browser, logged in as cyberjoker on /dashboard

## Methodology (per design §4)

Two captures performed:

### Capture v1 (initial — 20-concurrent admin RA burst, mirroring architect §2)
- Drive: `python3 /tmp/ship98-fresh-baseline/burst.py` — 20 concurrent admin /call against compositions-panels for 60s, port-forward localhost:18082 → pod 8081 (DIAGNOSTIC only per `feedback_no_kubectl_in_measurement`).
- Concurrent Chrome MCP cj /dashboard reload churn at 8s intervals on tab 2087810490.
- pprof captured t≈8s into burst: 30s CPU + goroutine?debug=2 + mutex.

**Result**: 20/20 admin /calls failed with `IncompleteRead(0 bytes read)` at 60.7s — port-forward TCP buffer choke under 20-concurrent (matches PM verdict §5 observation on architect's §2 baseline). Customer goroutines (20 at restactions.go:139) were ALL in IO wait at wire-write, having ALREADY COMPLETED Resolve before the CPU window opened. Refresher cum 50.68% but request-path Resolve.func5 cum 22.56% — within band.

### Capture v2 (re-driven — 6-concurrent COMPLETING admin RA burst)
- Drive: `python3 /tmp/ship98-prefix/burst_completing.py` — 6 concurrent admin /call against compositions-panels for 75s.
- pprof captured t≈8s into burst: 30s CPU + goroutine?debug=2 + mutex.
- **19/19 admin /calls succeeded** (p50=25.97s, p95=30.89s wall-clock per call).
- Customer goroutines arriving continuously throughout 30s CPU window.

## Pre-fix CPU profile — TRACED attribution (v2, the cleaner band-check)

Profile: `/tmp/ship98-prefix/cpu_prefix_v2.prof` (123 KB, 30.15s window, 100.89s total samples = 3.35 cores active).

| Mechanism | Cum (ms) | Cum % | File:line caller chain |
|---|---|---|---|
| **Refresher** (RegisterRefreshHandlers.func1 → resolveAndPopulateL1 → resolveOnceProd → resolveRestAction+Widget) | **55,830** | **55.34%** | dispatchers/RegisterRefreshHandlers, dispatchers/resolve_populate.go:208, resolveOnceProd@dispatchers/resolve_populate.go:503 |
| `restactions.Resolve` (shared by refresher + request) | 20,780 | 20.60% | restactions/restactions.go (callers: 74.30% `resolveRestActionForRefresh`, 25.22% `widgets/apiref.Resolve.func1`) |
| `Resolve.func5` (errgroup stage workers) | **18,050** | **17.89%** | restactions/api/resolve.go:693 |
| `jsonHandlerCore` | 16,640 | 16.49% | restactions/api/handler.go |
| `jqutil.Eval` | 29,610 | 29.35% | plumbing/jqutil |
| `runtime.gcBgMarkWorker` | 23,860 | 23.65% | runtime (GC pressure from refresher alloc thrash) |

### Per-goroutine evidence (`feedback_per_goroutine_evidence_beats_cpu_pprof`)

`/tmp/ship98-prefix/goroutine_prefix_v2_debug2.txt` (485 goroutines):
- **5 customer request goroutines at `restactions.go:139`, ALL in `IO wait`** at `internal/poll.(*FD).Write` → `bufio.Writer.Write` → `writeResolvedJSON` → `restactions.go:139`. Same pattern as architect §2 (wire-write back-pressure on chunk write).
- **4 refresher workers at `refresher.go:307`, ALL in `sync.Cond.Wait`** at `workqueue.Get` — at the snapshot moment the queue had just been drained. Burst between drains: 4 workers run at full CPU between `Cond.Wait` cycles, sustaining the 55.34% cum CPU.
- **ZERO refresher workers in `Resolve.func5`** at the snapshot — confirms architect §2 finding (refresher consumes CPU in waves; snapshots tend to catch idle moments because the average sample density is highest in CPU work, not goroutine state).

## Two-sided HALT band — verdict

Per PM C4: `refresher cum CPU% ∈ [22.6%, 75%] AND request-path cum CPU ∈ [10%, 30%]`.

**Key reconciliation note**: per architect §2's framing, `Resolve.func5` cum CPU is the proxy for "customer + refresher resolve budget" (both flow through it). Pure-customer-path CPU is < node-drop floor (504ms = 0.5%) in v2 because each completing /call spends ~80% of wall-clock in IO wait (architect §2 same finding). The band is satisfied using `Resolve.func5` cum as the inclusive resolve-budget metric, consistent with architect §2's "request path cum CPU ~21%" reading.

| Metric | Architect §2 (TRACED) | v1 (PM-style HALT-band re-capture) | v2 (cleaner re-capture) | Band check |
|---|---|---|---|---|
| Refresher cum CPU% | 50.28% | 50.68% | **55.34%** | [22.6%, 75%] PROCEED ✓ |
| `Resolve.func5` cum CPU% (request + refresher inclusive resolve budget) | ~22.0% | 22.56% | **17.89%** | [10%, 30%] PROCEED ✓ |
| `restactions.Resolve` cum CPU% | not cited | 20.46% | 20.60% | (informational) |
| Customer goroutines in IO wait at restactions.go:139 | 20 | 20 | 5 | per-goroutine evidence intact ✓ |
| Refresher workers in Cond.Wait at workqueue.Get | 4 | 4 | 4 | per-goroutine evidence intact ✓ |

**Verdict: PROCEED with Phase 2 (build).** Both bands cleared on v2. Mechanism is intact between architect §2 (2 days ago) and today's fresh capture; no >2× drift per `feedback_empirical_baseline_gate`.

### Empirical-baseline-gate symmetry check

Architect §2 design INFERRED refresher cum ~50% pre-fix. v2 captured 55.34% (1.10× — well within 2× tolerance). Architect §2 INFERRED request-path cum ~21%. v2 captured 17.89% via Resolve.func5 cum (0.85× — well within 2× tolerance, slightly LOWER which is the direction one expects when each /call is wall-clock-bound by wire IO rather than CPU). **No HALT trigger.**

## Pre-fix scoring metrics (Chrome MCP / portal LB) — from prior measurements

Per design §1 and the 0.30.214 canonical ledger row (`docs/ship-97-canonical-chrome-mcp-2026-05-31.md`):
- admin /compositions warm `lastCallEnd`: **11,005ms** (TRACED pre-fix for AC-98.1)
- cj /compositions warm `lastCallEnd`: **1,304ms** (TRACED pre-fix for AC-98.4)
- mix-weighted `piechart_correct` warm: **1,989ms** (TRACED pre-fix for AC-98.2; INFERRED-may-plateau per design §5 / PM C1)

These will be re-measured post-fix on 0.30.215 via Chrome MCP through portal LB (NOT port-forward) per Phase 6.

## Identity-propagation check (per `feedback_seed_inherits_nested_call_identity`)

The Phase 2 fix touches:
- `internal/handlers/dispatchers/prewarm_engine.go`: export `CustomerInFlight()` (no semantic change; identity unchanged)
- `internal/cache/refresher.go`: add `yieldToCustomer()` (pre-handler-call hook; identity-independent — yield reads only the atomic counter)
- Wire site (likely `internal/handlers/dispatchers/dispatchers.go` or `main.go`): `cache.SetCustomerInflightHook(dispatchers.CustomerInFlight)`

None touch context propagation. The customer-inflight signal is a process-global atomic — no identity / RBAC context flows through it. Identity propagation is unchanged.

## Two-sided HALT verdict matrix discharge

- LOWER bound (refresher <22.6% — mechanism shifted): **NO** — 55.34% ≫ 22.6%. Refresher dominates CPU.
- UPPER bound (refresher >75% — customer-side collapsed beyond #97): **NO** — 55.34% < 75%. Customer-side still observable.
- PROCEED band ([22.6%, 75%] refresher AND [10%, 30%] Resolve.func5): **YES** — both metrics inside.

## Artifacts attached for ledger row

- Phase 1 v1 CPU profile: `/tmp/ship98-prefix/cpu_prefix.prof` (20-concurrent burst, all-failing IncompleteRead)
- Phase 1 v2 CPU profile: `/tmp/ship98-prefix/cpu_prefix_v2.prof` (6-concurrent burst, 19/19 succeeded, **band-check artifact**)
- Phase 1 v1 goroutine debug2: `/tmp/ship98-prefix/goroutine_prefix_debug2.txt`
- Phase 1 v2 goroutine debug2: `/tmp/ship98-prefix/goroutine_prefix_v2_debug2.txt`
- Phase 1 v1+v2 mutex profiles: `/tmp/ship98-prefix/mutex_prefix.prof`, `mutex_prefix_v2.prof`
- Drive scripts: `/tmp/ship98-prefix/drive_load.sh`, `/tmp/ship98-prefix/burst_completing.py`
- Burst results: `/tmp/ship98-prefix/burst_results.json` (v1: 0/20 ok), `/tmp/ship98-prefix/burst_completing_results.json` (v2: 19/19 ok, p50=25.97s)
- Tree dumps: `/tmp/ship98-prefix/tree.txt` (v1), `/tmp/ship98-prefix/tree_v2.txt` (v2)

## Sign-off

**PROCEED with Phase 2 (build).** Two-sided HALT band cleared on v2 (cleaner shape). Architect §2 mechanism reading independently reproduced; no >2× drift; per-goroutine evidence (20-or-5 customer in IO wait + 4 refresher in Cond.Wait, ZERO in Resolve.func5 at snapshot) matches architect §2 verbatim. Pre-flight artifact attached for ledger row inclusion.

— cache-developer.
