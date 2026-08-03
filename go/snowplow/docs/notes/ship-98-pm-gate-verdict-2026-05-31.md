# Ship #98 — PM Gate Verdict

Signed: cache-pm. Date: 2026-05-31. Status: gating, not building. Read-only audit.

## Decision: **CONDITIONAL ACCEPT**

The architect's design is structurally sound, the brief reframe (rate-limiter → CPU-contention) is independently verified against fresh 0.30.214 pprof + per-goroutine evidence, the prior-art pattern at `prewarm_engine.go:295-322` exists and implements what the design claims, and the LOC envelope is honest (+73 prod / +340 tests). The falsifier in §4 codifies a two-sided HALT band exactly as the new `feedback_empirical_baseline_gate` rule requires.

The biggest reason this is CONDITIONAL rather than full ACCEPT is **AC-98.2** — the mix-weighted piechart north-star target (≤ 800ms). The design honestly flags it as INFERRED-may-plateau in §3 and §6 R-second-order, but the AC table itself reads as a hard pass-threshold. Without an explicit "north-star aspirational, may need #99" tag on the AC line itself, dev could be set up to "fail" the ship on a structural wire long-pole that #98 is not designed to attack. Conditions C1–C8 below close this and a handful of other gaps.

---

## Brief reframe verification (independent walk)

The dispatch brief asserted: "20 internal-dispatch goroutines all wait on shared `tokenBucketRateLimiter` (0xc000487450); 99% of mutex delay; fix = separate limiter OR yield at limiter."

The architect re-framed this in §1 after a fresh 0.30.214 baseline. I independently re-traced each pillar of the reframe.

### 1. No shared client-go rate limiter — VERIFIED

`internal/dynamic/sa_client.go:170-171` (read directly):
```
rc.QPS = -1 // disable client-side rate limiter (server-side P&F is authoritative); see client-go rest/config.go:117-122
rc.Burst = 0
```

This is the singleton SA `*rest.Config` used by the internal-dispatch path (architect §3 cites `internal_dispatch.go:115-127` memoised client). With `QPS=-1`, client-go installs **no rate limiter**. The brief's "shared tokenBucketRateLimiter" claim is FALSIFIED at code level.

### 2. dictMu is per-Resolve-call local — VERIFIED

`internal/resolvers/restactions/api/resolve.go:467` (read directly):
```
var dictMu sync.Mutex
g, gctx := errgroup.WithContext(ctx)
g.SetLimit(iterParallelism(ctx))
```

`dictMu` is declared **inside** the `Resolve` function body, immediately before the errgroup that runs the stage workers. It is captured by closure into the `feedBytes`/`feedValue`/`call.ResponseHandler` closures at lines 516-533. It serialises writes to the call-local `dict` map among the SAME /call's stage workers. There is **no cross-/call sharing**, no cross-goroutine sharing across separate Resolve invocations, and no refresher-vs-request sharing. The 99% mutex cum the brief read on yesterday's pprof is intra-/call contention. The architect's framing is correct.

### 3. Fresh pprof — refresher cum CPU 50.28%, NO rate-limiter symbols — VERIFIED

`go tool pprof -top -cum -unit=ms /tmp/ship98-fresh-baseline/cpu.prof` (run directly):
```
50.32% k8s.io/apimachinery/pkg/util/wait.BackoffUntilWithContext
50.29% snowplow/internal/handlers/dispatchers.resolveAndPopulateL1
50.28% snowplow/internal/cache.(*refresher).processOne
50.28% snowplow/internal/handlers/dispatchers.RegisterRefreshHandlers.func1
24.43% runtime.gcBgMarkWorker
22.05% snowplow/internal/resolvers/restactions/api.Resolve.func5
20.93% snowplow/internal/handlers/dispatchers.resolveRestActionForRefresh
```

Grep on `tokenBucket|rate\.Limiter|WaitN` against top -cum: **0 hits**. The refresher attribution chain (RegisterRefreshHandlers.func1 → processOne → resolveAndPopulateL1) is exactly the chain the architect cited at §2. GC at 24.43% is consistent with refresher alloc thrash and the design's R-second-order risk.

### 4. Per-goroutine evidence at `/tmp/ship98-fresh-baseline/goroutine_mid_debug2.txt` — VERIFIED

`grep -c "restactions.go:139" goroutine_mid_debug2.txt` = **20 customer request goroutines** at the design's claimed frame.
Filter to IO wait state + restactions.go:139 = **20 in IO wait** (matches design exactly).
Refresher goroutines (frames at `refresher.go:307`) all in `sync.Cond.Wait` state at snapshot: **4 workers** (matches design's claim of 4 refresher workers in workqueue.Get cond-wait at idle).
Search for any `rate.Limiter`/`tokenBucket`/`WaitN`/`workqueue.*RateLimiter` stack: **0 hits**.

The architect's per-goroutine evidence reading is faithful to the artifact.

### 5. Burst result observation (added by PM)

`/tmp/ship98-fresh-baseline/burst_results.json` shows **20/20 admin RA burst requests FAILED with IncompleteRead** at 60-67s elapsed. This is the wire-write back-pressure long-pole #2 manifesting as a port-forward TCP buffer choke. It strongly corroborates R-second-order risk and is consistent with §2's "structural to the 35 MB payload × gzip-decompressing client" framing. Worth flagging to architect: today's 0.30.214 admin /compositions warm is in fact MORE degraded than #97's 11,005ms when 20 are concurrent — pure single-curl /compositions warm vs 20-concurrent burst are different shapes. Not a blocker for #98 (the scoring metric is Chrome MCP not 20-burst), but a useful pre-fix data point.

**Reframe verdict: VERIFIABLE. Brief was misread; architect re-read is correct. Approach (c) is the right pick because there is no rate limiter for (a) or (b) to target.**

---

## Approach (c) verification — prior art at `prewarm_engine.go:295-322`

The design claims this range implements a cooperative customer-priority yield pattern that the refresher should mirror. I read the range directly.

`prewarm_engine.go:88-105` (the signal):
```
var customerInFlightCount atomic.Int64

func markCustomerInFlight() func() {
    customerInFlightCount.Add(1)
    return func() { customerInFlightCount.Add(-1) }
}

func customerInFlight() bool {
    return customerInFlightCount.Load() > 0
}
```

`prewarm_engine.go:295-322` (the yield):
```
func (e *prewarmEngine) yieldToCustomer(ctx context.Context) {
    if !customerInFlight() { return }
    t := time.NewTicker(e.yieldPoll)
    defer t.Stop()
    for customerInFlight() {
        e.yieldTotal.Add(1)
        select {
        case <-ctx.Done(): return
        case <-t.C:
        }
    }
}
```

Call sites already in production: `prewarm_engine.go:275` yields before each scope; the increment/decrement pair is wired at `restactions.go:77` and `widgets.go:62` (`defer markCustomerInFlight()()`, both verified via grep). The pattern is exactly what the design proposes to mirror in `refresher.processNext` before `processOne`.

**Verdict: prior-art verified. Pattern is production-validated by Ship 0.30.214's prewarm engine. The "single ticker poll on atomic-int64 read" mechanism is correct, minimal, and idiomatic for the codebase.**

---

## Acceptance criteria (scored)

All thresholds anchored to Chrome MCP via portal LB except diagnostic items.

| # | Criterion | Type | Tag | PM verdict |
|---|---|---|---|---|
| **AC-98.1** | admin /compositions warm lastCallEnd ≤ 6,000ms (pre-fix 11,005ms) | north-star (regression close) | TRACED pre-fix | **Falsifiable, anchored, baseline cited. ACCEPT.** Threshold gives ~5,000ms margin above the 1-2s structural floor explicitly to absorb R-second-order long-pole #2. |
| **AC-98.2** | mix-weighted piechart_correct warm ≤ 800ms (pre-fix 1,989ms) | north-star | **INFERRED (may plateau)** | **Falsifiable, anchored, baseline cited — BUT see Condition C1.** Design §3 and §6 honestly disclose this MAY plateau due to wire long-pole unmask. The AC line itself does not carry that flag. Could set dev up to ship-fail on a structural floor. |
| **AC-98.3** | Refresher cum CPU% under 60s admin burst ≤ 15% (pre-fix 50.28%) | diagnostic mechanism gate | TRACED pre-fix | **Falsifiable, anchored, baseline cited. ACCEPT.** Direct mechanism falsifier — if yield engages, this MUST drop. |
| **AC-98.4** | cj /compositions warm lastCallEnd ≤ 1,800ms (pre-fix 1,304ms) | regression guard | TRACED pre-fix | **Falsifiable, anchored, baseline cited. ACCEPT.** Preserves #97's −14.9% improvement; correct guard direction. |
| **AC-98.5** | Pod restartCount = 0 across 30-min sustained burst | resilience | n/a (no baseline) | **Falsifiable. ACCEPT.** Mirrors #97 AC-97.11. |
| **AC-98.6** | RBAC symmetry — Group-only RoleBinding cohort served items match | correctness | per `feedback_predicate_subject_kind_symmetry` | **Falsifiable. ACCEPT.** Orthogonal to fix but cheap to verify; ζ HARD REVERT lesson. |
| **AC-98.7** | `-race` 4 customer + 4 refresher concurrent | concurrency | per `feedback_shared_vs_copy_is_a_concurrency_change` | **Falsifiable. ACCEPT.** The customer-inflight counter goes from 2-writer/1-reader (today's prewarm engine) to 2-writer/2-reader (add refresher workers). Race test mandatory. |
| **AC-98.8** | Per-goroutine: refresher NOT in `Resolve.func5` during burst | mechanism falsifier | per `feedback_per_goroutine_evidence_beats_cpu_pprof` | **Falsifiable. ACCEPT.** Ground-truth check that yield engaged. |
| **AC-98.9** | Post-burst refresher recovery: first `completedTotal++` ≤ 500ms after burst end | inverse-defect guard | architect-added | **Falsifiable. ACCEPT.** Catches "yield never releases" deadlock — good defensive AC. |
| **AC-98.10** | content-equivalence (`dispatch_delta` cj+admin) zero non-noise diff | correctness | mirrors #97 AC-97.6 | **Falsifiable. ACCEPT.** No JQ-shape drift across yield rollout. |
| **AC-98.11** | LOC envelope ≤ +180 prod + ≤ +350 tests | discipline | mirrors #97 | **Falsifiable. ACCEPT.** Honest estimate +73 prod / +340 tests; +530 grand pause threshold is reasonable. |

### PM-added gap

**AC-98.12 (PM-added): Refresher cache settle-time after CRUD event ≤ 10s under no customer load.** Rationale: the yield COULD in pathological cases (sustained customer pressure for the entire 10s `refresherYieldMaxParked` ceiling) delay a dirty-mark refresh by up to 10s. The "Correct data ... convergence ≤ 10s" success metric is P0. The max-parked cap is one safeguard, but we need a direct AC. Falsifier: under quiescent customer load, time-from-informer-event-to-`completedTotal++` ≤ 10s on a worst-case TTL outer-net path. Diagnostic, but binds to the convergence success metric directly.

---

## PM gate question discharge

### Q1. Would the fix make the symptom disappear?

**Partially. Mechanism path is sound; symptom-disappear is bounded by R-second-order long-pole.**

Pathway TRACED: refresher CPU at 50.28% during burst → 4 refresher workers compete for 8 cores → customer goroutines get ~4 cores → resolver work that should be ~150ms/call extends due to scheduler queueing and GC pressure → /compositions admin warm lastCallEnd = 11,005ms.

Post-yield (INFERRED): refresher yields during burst → 4 workers idle in `t.C` ticker park → customer gets ~8 cores → resolver work drops back to baseline scheduler queueing. BUT — the 20 customer goroutines in §2 are observed in IO wait at `restactions.go:139` (wire-write back-pressure), NOT in scheduler queue. The chain "refresher CPU drops → request goroutines get more scheduler time → lastCallEnd drops" CAN break at the wire-back-pressure step if the dominant per-/call wait is wire-write rather than CPU. The architect's 6,000ms AC-98.1 threshold (vs structural floor 1-2s) gives ~5,000ms of margin specifically to absorb this. AC-98.1 at 6,000ms IS reachable even if AC-98.2 plateaus. **Condition C1 below closes the AC-98.2 framing gap.**

### Q2. Is AC-98.2 properly flagged INFERRED-not-guaranteed?

**Partially. §3 and §6 R-second-order disclose honestly. The AC table row itself does not carry the flag.** See Condition C1.

### Q3. Is the prior-art reference verified?

**YES.** `prewarm_engine.go:88-105` (signal) + `prewarm_engine.go:295-322` (yield helper) + line 275 (call site) implement the exact pattern the design proposes to mirror. Production-validated by Ship 0.30.214. `feedback_check_k8s_clientgo_prior_art` is honoured: the architect re-used existing snowplow prior art rather than reinventing — and that prior art itself was a deliberate mirror of `client-go/util/workqueue` cooperative-poll patterns.

### Q4. Is the #99 prediction reasoned?

**INFERRED, honestly labelled.** §3 ("Will #98 unmask another bottleneck?") explicitly says "YES — INFERRED" and points at wire-write back-pressure as the most-likely next ranked attack. Today's burst_results.json (20/20 IncompleteRead) IS supporting evidence but on a port-forward shape, not Chrome MCP. The §6 R-second-order risk row goes further: "post-fix `lastCallEnd` may plateau between 5-8s instead of dropping to the 1-2s structural floor." This is hypothesis-grade, not commitment-grade — the post-#98 Chrome MCP re-measurement is the rank-orderer. **Acceptable as INFERRED; not asking for a guarantee of #99 here.**

### Q5. Does the design honour `feedback_empirical_baseline_gate`?

**YES, explicitly.** §4 codifies a two-sided HALT band: lower bound 22.6% refresher cum CPU (half of 50.28%), upper bound 75%; symmetric request-path band [10%, 30%]. Outside either band → dev HALTs and re-engages architect. This is exactly the rule the new memory item requires. The "Ship S.2 lesson" (8× discrepancy not caught, hard revert) is structurally prevented for #98.

---

## Risk register validation

| Risk | Mitigation testable? | Falsifier defined? | PM verdict |
|---|---|---|---|
| **R-empirical-baseline-gate** | YES — §4 falsifier + AC-98.3 | YES — two-sided HALT band | ACCEPT |
| **R-second-order long-pole** | YES — AC-98.1 threshold absorbs it (6,000ms vs 1-2s floor) | YES — Chrome MCP re-measure on /compositions admin warm | ACCEPT — but see C1 |
| **R-customer-tax-leak** | YES — AC-98.3 + AC-98.8 (refresher CPU drop + per-goroutine evidence) | YES — atomic.Load read budget (4 × 40Hz = 160 reads/s, negligible) | ACCEPT |
| **R-shared-vs-copy** | YES — AC-98.7 `-race` over concurrent 4 readers + 4 writers | YES — race detector zero-hits | ACCEPT |
| **R-yield-stall-deadlock** | YES — `refresherYieldMaxParked` cap (10s) + AC-98.9 (≤ 500ms post-burst recovery) | YES — synthetic stall test in falsifier suite | ACCEPT |
| **R-special-cases** | YES — code review, AC-98.10 | YES — `dispatch_delta` content equivalence | ACCEPT |
| **R-no-park-broken-behind-flag** | YES — no new flag at all | n/a (preventive) | ACCEPT |
| **R-cache-off-fallthrough** | YES — refresher gated at `refresher.go:181 if !ResolvedCacheEnabled()` verified | n/a (already off when cache off) | ACCEPT |

**Missing risk (PM-added):**

**R-convergence-window** — yield can delay informer-triggered refresh by up to `refresherYieldMaxParked` = 10s under sustained customer pressure. P0 success-metric "convergence ≤ 10s after CRUD" is on the boundary. Falsifier: AC-98.12 (PM-added) — measure CRUD-to-completedTotal latency under sustained customer load. Mitigation: max-parked cap is already 10s which is exactly the convergence budget. If C2 lowers this, R-convergence-window relaxes.

---

## Conditions for acceptance (must discharge before dev's first commit)

**C1 (HIGH — framing).** AC-98.2 (mix-weighted piechart_correct ≤ 800ms) MUST carry an explicit `INFERRED — may plateau due to wire long-pole; #99 candidate if missed` tag inline in the AC table row. Dev should NOT be set up to hard-fail #98 on a structural floor. Architect updates §5 table; PM signs off on the line.

**C2 (HIGH — convergence guard).** Add **AC-98.12** to §5 as PM proposed: refresher cache settle-time after a CRUD informer event ≤ 10s under quiescent customer load. Falsifier: under quiescent customer load, time-from-informer-event-to-`completedTotal++` ≤ 10s. Architect confirms or proposes a tighter cap on `refresherYieldMaxParked` (currently 10s).

**C3 (HIGH — three-way pre-commit ACK).** Per `feedback_dev_review_with_architect_pm_before_commit`, dev MUST share the diff with **architect (this design file) + PM (this verdict file)** before tag/push. Design §10 line 515-517 already commits to this; reaffirm explicitly in dev's first message after the falsifier capture.

**C4 (HIGH — falsifier first; two-sided HALT explicit).** Per `feedback_falsifier_first_before_ship` and `feedback_empirical_baseline_gate`, dev's FIRST step is the §4 pre-fix capture against a fresh 0.30.214 pod. HALT band is [22.6%, 75%] refresher cum CPU AND [10%, 30%] request-path cum CPU. Outside EITHER band → HALT, escalate to architect, do not write code. Mirrors Ship #97 PM lesson #1+#2.

**C5 (HIGH — per-goroutine post-fix evidence).** AC-98.8 alone is sufficient at the AC level, but PM adds: dev's post-fix submission MUST include a goroutine?debug=2 capture mid-burst on 0.30.215 showing the 4 refresher workers in `Cond.Wait` or `t.C` yield-park, NOT in `Resolve.func5`. The captured file path must be cited in the ledger row. Mirrors Ship #97 PM #10.

**C6 (HIGH — rollout discipline).** Single binary 0.30.215; lockstep chart tag on `braghettos/snowplow-chart` with **explicit push to braghettos** (chart `origin`=upstream per `feedback_chart_repo_origin_is_upstream`); NO values.yaml changes; NO new env vars; deploy via `helm upgrade --version 0.30.215` (NOT `kubectl set image` / NOT `kubectl apply`). Revert plan = tag rollback to **0.30.214** (NOT 0.30.212 — keep #97's mechanism win).

**C7 (MEDIUM — LOC pause).** Architect's §7 ceiling at +530 grand total (+180 prod / +350 tests) is acceptable. Dev paused for review at threshold. Honest +73 prod / +340 tests is the working estimate; +180 prod cap absorbs reasonable wiring overhead.

**C8 (MEDIUM — content-equivalence sweep).** AC-98.10 covers cj + admin compositions-panels + dashboard piechart pre vs post. Dev confirms `dispatch_delta` zero non-noise diff before tag/push.

---

## Rollout sign-off (10-condition mirror of Ship #97)

| # | Condition | Status |
|---|---|---|
| 1 | Single binary 0.30.215; single CACHE_ENABLED flag end-state | ACK in design §8 |
| 2 | Lockstep chart tag on braghettos/snowplow-chart, explicit braghettos push | ACK in design §8 |
| 3 | NO values.yaml changes, NO new env vars | ACK in design §3, §6 R-no-flag |
| 4 | Revert plan = tag rollback to 0.30.214 (NOT 0.30.212) | ACK in design §9 |
| 5 | NOT a flag toggle (per `feedback_no_park_broken_behind_flag`) | ACK in design §6 R-no-flag, §9 |
| 6 | Three-way pre-commit ACK (architect + PM + dev) | C3 above |
| 7 | LOC pause at +530 per architect §7 | ACK in design §7 + C7 |
| 8 | Per-goroutine post-fix evidence required | C5 + AC-98.8 |
| 9 | Pre-flight falsifier FIRST | C4 + design §4 |
| 10 | Two-sided HALT band on the falsifier | C4 + design §4 (band [22.6%, 75%]) |

All 10 conditions covered between design and PM conditions C1-C8.

---

## Summary

**Decision: CONDITIONAL ACCEPT.** Discharge C1–C6 before dev's first commit; C7–C8 before tag/push. The architect's reframe is independently verified at every pillar (sa_client QPS=-1, dictMu local, fresh pprof has no rate-limiter symbol, per-goroutine evidence confirms the IO wait long-pole). Prior-art at `prewarm_engine.go:295-322` exists and is faithfully cited. AC-98.2 framing is the one substantive gap; C1 closes it. #99 prediction is honest INFERRED.

— cache-pm, 2026-05-31
