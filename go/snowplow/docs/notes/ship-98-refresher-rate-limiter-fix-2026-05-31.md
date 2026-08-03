# Ship #98 — Refresher customer-priority yield (cooperative)

Signed: cache-architect. Date: 2026-05-31. Status: design + PM gate.
Single-file design; obeys `feedback_architect_design_rigor` (TRACED/INFERRED
labels mandatory; cite every code claim file:line; isolated-data root cause;
falsifier band; coverage = end-to-end on north-star path).

---

## §0 TL;DR

Fix the `feedback_customer_priority_over_refresher` contract violation by
**re-using the existing prewarm-engine customer-priority signal**
(`customerInFlight()` / `markCustomerInFlight()`, prior art at
`internal/handlers/dispatchers/prewarm_engine.go:88-105`,
`prewarm_engine.go:295-322`) inside `internal/cache/refresher.go`'s
`processOne` and between cohort/per-call work inside the resolve path. The
refresher already runs on its own client-go workqueue with its own worker
pool (`refresher.go:150-156`, `refresher.go:204-215`); the missing
mechanism is the **cooperative yield at the start of each processOne and
between major sub-resolutions**, mirroring `prewarmEngine.runWorker`
yielding at `prewarm_engine.go:275`. **NO new rate limiters**; **NO
separate transports**; **NO new flags** — Ship 0.30.214 (prewarm engine)
proved this pattern works for the boot pre-warm path; #98 brings the
same pattern to the steady-state refresher.

Pick approach **(c)** in the brief's §3 menu: reuse the L1-refresher /
prewarm-engine customer-priority-yield pattern. Approach (a) (separate
rate limiters) is structurally wrong because **there is no shared rate
limiter today** (FALSIFIED in §1 + §2 below); approach (b) (limiter-level
yield) does not apply for the same reason.

---

## §1 Problem statement (TRACED + correction to brief premise)

### What customer-priority contract violation looks like, TRACED on 0.30.214

Ship #97 (0.30.214) Chrome MCP scoring measurement
(`docs/ship-97-canonical-chrome-mcp-2026-05-31.md`): admin /compositions
warm `lastCallEnd` = **11,005ms** vs 0.30.212 baseline 9,573ms (+15.0%
regression). Mechanism win (parseListEnvelope 45.35% → 5.19% CPU) is
flawless per `feedback_per_goroutine_evidence_beats_cpu_pprof` but the
customer-facing scoring metric REGRESSED. The brief identifies this as
the newly-binding constraint.

Fresh empirical re-baseline on 0.30.214
(`/tmp/ship98-fresh-baseline/cpu.prof` captured 2026-05-31 13:38, GKE
verified, 20-concurrent admin RA compositions-panels burst, 30s CPU
window, see §2 below) shows **TRACED** at file:line:

| Path | Cumulative CPU% (of 101.10s sampled, 8 cores × 30s) | Site |
|---|---|---|
| Refresher: `RegisterRefreshHandlers.func1` → `resolveAndPopulateL1` → `resolveWidgetForRefresh` → `restactions.Resolve.func5` | **50.28%** (50.83s) | `dispatchers.go:80`, `resolve_populate.go:208`, `resolve_populate.go:503` |
| Customer request: `restactions.go:139` → resolver | **~21%** (21.16s of `Resolve.func5`, of which ~21.13s is `jsonHandlerCore`/`EvalValue`) | `restactions.go:139`, `resolve.go:693`, `handler.go:100` |
| Runtime GC | **24.43%** (`gcBgMarkWorker` 24.7s) | runtime |

Per-goroutine evidence (`/tmp/ship98-fresh-baseline/goroutine_mid_debug2.txt`,
521 goroutines):
- **20 customer request goroutines, all in `IO wait`** at
  `internal/poll.(*FD).Write` → `bufio.Writer.Write` →
  `net/http.(*response).Write` (`server.go:1694`). Top frame is
  `restactions.go:139 +0xec7` (the entry point; resolver chain has already
  completed; wire-send back-pressure).
- **4 refresher workers, all in `sync.Cond.Wait`** at `workqueue.Get`
  (queue empty at the snapshot moment; bursts when dirty-marks accumulate).
- **ZERO `Resolve.func5` worker frames** — meaning the 20 customer requests
  had ALL completed Resolve and were blocked on wire-write at snapshot time.

So the per-goroutine ground truth is:
- **Customer-tax mechanism is NOT a shared mutex; it is CPU contention.**
  The refresher's 4 workers are eating ~half of the pod's CPU (`50.28%`
  cum) during the customer burst. With 8 cores available, the customer
  burst gets ~4 cores; the resolver work that should complete in ~150ms
  per /call instead competes with refresher resolves for CPU and queue
  scheduling time, extending per-/call wall-clock.

### Contract violation (the actual one):

`feedback_customer_priority_over_refresher` says *"Customer /call MUST
have absolute priority + meet north-star regardless. Reject designs that
'calm refresher'; require designs that 'decouple customer from refresher
budget.'"* Today, the refresher and customer paths share Go runtime CPU /
scheduler / GC budget. There is no yield: `refresher.processOne`
(`refresher.go:306-345`) calls the handler `fn(ctx, key, *entry.Inputs)`
unconditionally (`refresher.go:379`). **The contract is violated by the
ABSENCE of a yield, not by a shared rate limiter.**

### Correction to the brief's premise (TRACED)

The brief asserts:
> "Refresher amplification via shared client-go rate limiter — 20 internal-
> dispatch goroutines all wait on the same `tokenBucketRateLimiter`
> (`0xc000487450`) at `internal_dispatch.go:233`; 99% of mutex delay
> (4,015s cum over 30s)."

This was the brief author's reading of yesterday's `mutex_mid.prof`. On
re-inspection of both yesterday's profile and today's fresh 0.30.214
profile:

- **The 99% mutex stack is `sync.(*Mutex).Unlock` at `mutex.go:65`** —
  NOT a token-bucket rate limiter. The cumulative attribution travels:
  `errgroup.(*Group).Go.func1` → `Resolve.func5` (`resolve.go:693`) →
  `Resolve.func4` (`resolve.go:532`) → `sync.(*Mutex).Unlock`.
- **That mutex is `dictMu`** (`resolve.go:467 var dictMu sync.Mutex`),
  the resolver's local dict-protection mutex. It's a stack-local
  `sync.Mutex` per `Resolve` invocation. Every stage worker goroutine
  takes `dictMu.Lock()` in `feedBytes`/`feedValue` (`resolve.go:525,
  530`) and `feedDict`(`resolve.go:517`). The 99% mutex delay is
  intra-/call contention among the SAME /call's stage workers — NOT
  cross-/call sharing, NOT refresher-vs-request sharing.
- **`internal_dispatch.go:233` is `nri.Get(ctx, name, ...)`** — a
  client-go dynamic Get that, on the SA `*rest.Config`, uses
  `rc.QPS = -1` / `rc.Burst = 0` per `internal/dynamic/sa_client.go:170-171`
  ("disable client-side rate limiter; server-side P&F is authoritative").
  **There is no token bucket in this path.** The pprof tree (top + tree
  + line listing in §2 below) does not contain `tokenBucketRateLimiter`
  or `rate.Limiter.WaitN` in any frame; the brief's "rate limiter
  contention" reading was a mis-read of the cumulative attribution.
- **What the brief is reading is the refresher's CPU pressure, not
  mutex contention.** The 50.28% cum CPU on the refresher path under
  burst (TRACED §2) IS the customer-tax mechanism: refresher CPU
  competes with request CPU on the same cores. The mutex-time numbers
  are intra-/call, not cross-path.

Per `feedback_empirical_baseline_gate`, this discrepancy (brief's "rate
limiter" claim vs empirical "no rate limiter, CPU contention") IS a
two-sided HALT trigger (the upper-bound symmetry condition PM added in
Ship #97 condition #2). The fresh empirical re-baseline in §2 explicitly
addresses this: the **mechanism the brief named (rate limiter) is wrong,
but the contract violation it points at (refresher consuming customer
budget) is real and ranked #1 cum CPU on the post-#97 path**.

### What the fix targets, then

The cause of the +15% admin /compositions warm regression on 0.30.214 is
that the refresher does not yield to customer /call. With Ship #97
removing parseListEnvelope's CPU bloat from the per-call hot path, the
refresher's relative CPU share grew (the per-call cost dropped from 6.6s
to ~1.2s but the refresher's per-cycle cost stayed the same → refresher
became more CPU-dominant). The fix is to make the refresher
**cooperatively yield** while customer /calls are in flight, exactly as
the prewarm engine does between scopes (`prewarm_engine.go:275`).

---

## §2 Empirical re-baseline against 0.30.214 (TRACED)

Per `feedback_empirical_baseline_gate`: a separate fresh pprof was
captured against 0.30.214 (pod `snowplow-7d9f56554b-wqdbc`, helm rev 368,
GKE context verified) BEFORE writing this design. Yesterday's pprof was
on 0.30.212; #97 changed the post-fix landscape.

### Probe

| Field | Value |
|---|---|
| Cluster | `gke_neon-481711_us-central1-a_cluster-1` (verified) |
| Pod | `snowplow-7d9f56554b-wqdbc`, image 0.30.214, restarts=0 |
| Cluster scale | 50,000 compositions, 50 bench namespaces |
| Burst | 60s sustained, 20 concurrent, urllib (gzip Accept-Encoding), 180s per-req timeout; same shape as `/tmp/admin-ra-pprof-2026-05-31/burst.py` |
| Transport | kubectl port-forward localhost:18082 → pod 8081 (DIAGNOSTIC only per `feedback_no_kubectl_in_measurement`; latency numbers below are NOT for scoring — Chrome MCP is the scoring tool — but mechanism attribution from pprof is) |
| Content validation | Token sanity check via single curl returns HTTP 200 + `kind=RESTAction` + non-empty `compositionspanels` list |
| pprof captures | mutex / block / cpu (30s) / goroutine / goroutine?debug=2 — all at t=15s into burst |

### Headline mechanism numbers (TRACED, `/tmp/ship98-fresh-baseline/`)

| Profile | Top cum site | % | Note |
|---|---|---|---|
| `mutex_mid.prof` | `sync.(*Mutex).Unlock` at `mutex.go:65` | **99.02%** (2.37hrs of 2.40hrs total delay) | All attributed to `Resolve.func5` (resolve.go:693) → `Resolve.func4` (resolve.go:532), i.e. **`dictMu`** — local to each Resolve invocation, intra-/call only |
| `block_mid.prof` | `runtime.selectgo` | 53.46% (83.75hrs) | Long-lived informer queue + workqueue waits; not in scoring path |
| `cpu.prof` | `RegisterRefreshHandlers.func1` (refresher path) | **50.28%** (50.83s of 101.10s) | `resolveAndPopulateL1` 49.87s; `resolveWidgetForRefresh` 26.72s. **THIS is the customer-tax mechanism** |
| `cpu.prof` | `runtime.gcBgMarkWorker` | 24.43% (24.7s) | GC pressure from refresher allocs (and burst response encode) |
| `cpu.prof` | `Resolve.func5` (request path, cum) | ~21% (21.16s) | Including jsonHandlerCore 21.03s, EvalValue 20.58s — the actual customer dispatch work |

### Per-goroutine evidence (`feedback_per_goroutine_evidence_beats_cpu_pprof`)

| Population | Count | State | Top snowplow frame |
|---|---|---|---|
| Customer request handlers | 20 | `IO wait` | `restactions.go:139 +0xec7` → `writeResolvedJSON` → `internal/poll.(*FD).Write` |
| Refresher workers | 4 | `sync.Cond.Wait` (workqueue.Get) | `refresher.go:307` |
| Long-lived informer workers | ~108+168 | `chan receive` / `select` | Background, not scoring path |

### Ranked attribution on 0.30.214 (the binding constraint)

| # | Mechanism | TRACED file:line | Customer impact |
|---|---|---|---|
| 1 | **Refresher CPU share during customer burst** | `dispatchers.go:80` → `resolve_populate.go:208` → `resolve_populate.go:503` → `restactions.Resolve` | 50.28% of pod CPU; halves the cores available to customer burst |
| 2 | Customer wire-write back-pressure (Chrome / kubectl-pf TCP buffer) | `restactions.go:139`/`writeResolvedJSON` (helpers.go:375) → `net/http.(*response).Write` (server.go:1694) | 20 goroutines pinned in IO wait during burst (consistent with 0.30.212 finding; structural to the 35 MB payload + slow gzip client) |
| 3 | dictMu intra-/call contention | `resolve.go:525`/`:530` (`feedBytes`/`feedValue`) | 99% of mutex delay but per-/call local; not a #98 attack target |
| 4 | GC pressure from refresher allocs | `runtime/mgc.go:1523` `gcBgMarkWorker` | 24.43% CPU; secondary cost of refresher CPU pressure (see #1) |

### Empirical-baseline-gate verdict

**Discrepancy from brief identified** (the "rate limiter" naming is not
TRACED on either pre-#97 or post-#97 pprof). The mechanism-#1 ranking
in this re-baseline (refresher CPU share at 50.28% cum) is consistent
with the brief's CONTRACT-violation framing — refresher amplifies under
customer burst — but the SHAPE of the violation (CPU competition, not
rate-limiter queueing) determines the right fix. We DID re-frame to
cooperative yield (approach c) before writing any production code, in
keeping with `feedback_empirical_baseline_gate` (re-baseline + reconcile
before code).

---

## §3 Fix mechanism — approach (c), with rationale

### The fix

Make `refresher.processOne` (`internal/cache/refresher.go:306-345`) and
the per-handler call site (`refresher.go:379` `fn(ctx, key, *entry.Inputs)`)
**cooperatively yield to in-flight customer /calls**, using the existing
`customerInFlight()` signal already wired at `restactions.go:77` and
`widgets.go:62`.

The signal lives in `internal/handlers/dispatchers/prewarm_engine.go`
(`customerInFlightCount atomic.Int64` at line 92; `customerInFlight()` at
line 103). It is currently unexported. **One small export** plus **one
small yield function in the cache package** (the cache package cannot
import dispatchers — cyclic — so the yield helper lives in cache and
takes a `func() bool` injected from dispatchers wiring at startup), and
the refresher's processOne and processNext loop respect customer
priority.

### Why approach (c) (re-use customer-inflight signal), NOT (a) or (b)

| Brief approach | Verdict | Why |
|---|---|---|
| (a) Separate rate-limiter instances for refresher vs request | **REJECTED** | There is no shared rate limiter today. `sa_client.go:170-171` sets `QPS=-1, Burst=0` on the SA `*rest.Config`; the per-request path uses a different transport (plumbing's httpcall.Do, user JWT). The dispatchViaInternalRESTConfig path is keyed on a singleton SA `*rest.Config` whose client-go limiter is **disabled** (see also `internal_dispatch.go:115-127` memoised client). Splitting non-existent rate limiters is a non-fix. |
| (b) Customer-priority yield AT the limiter level | **REJECTED** | Same reason as (a) — no limiter to gate at. |
| **(c) Reuse customer-inflight signal + cooperative yield (Ship 0.30.214 / prewarm_engine.go pattern)** | **SELECTED** | Prior-art exists and is *production-validated* by Ship 0.30.214 (the prewarm engine yields exactly this way at `prewarm_engine.go:275`). The signal counter is already incremented/decremented by every customer /call entry at `restactions.go:77` and `widgets.go:62`. The contract violation is the refresher's **lack** of yield; bringing it parity with the prewarm engine closes the contract correctly. Smallest, lowest-risk change. |

### Concrete plumbing (TRACED)

1. **Export** the prewarm engine's `customerInFlight()` as
   `dispatchers.CustomerInFlight()` (capitalised). One-line change to
   `internal/handlers/dispatchers/prewarm_engine.go:103`. No semantic
   change.

2. **Inject** the predicate into the cache package at startup. Pattern
   mirrors `cache.Deps().SetRefreshHook` at `refresher.go:206`
   (function-pointer injection — cache package owns the receiver, the
   caller injects the dependency). New API on the refresher singleton:
   `cache.SetCustomerInflightHook(func() bool)` called once from main.go
   (or from dispatchers.RegisterRefreshHandlers wiring) BEFORE
   StartRefresher returns. Default behaviour when hook is nil: skip the
   yield (so unit tests + the cache-disabled mode are unaffected).

3. **Yield in `processNext`** between Get and processOne. Mirrors
   `prewarm_engine.go:271-275` ("yield before running the scope"):

   ```
   key, shutdown := r.queue.Get()
   if shutdown { return false }
   defer r.queue.Done(key)
   r.yieldToCustomer(ctx)   // NEW — cooperative park while customerInFlight()
   if err := r.processOne(ctx, key); err != nil {
     // unchanged
   }
   ```

   `yieldToCustomer` uses the SAME `defaultEngineYieldPoll = 25ms` from
   `prewarm_engine.go:182`. Bounded by `ctx.Done()` and by a
   `refresherYieldMaxParked` ceiling (**5s** — tightened from initial-
   design 10s per PM verdict C2, to leave ~5s headroom under the
   AC-98.12 10s convergence SLA; beyond 5s the refresher proceeds
   anyway, so a buggy never-decrementing customer counter cannot stall
   refresh forever; analogous to the prewarm engine relying on ctx +
   the engine's bounded per-scope deadline). <!-- [C2 DISCHARGE] cap
   tightened 10s → 5s to leave AC-98.12 convergence headroom -->

4. **(Optional, ship-internal-followup)** Per-handler yield checkpoint
   inside `resolveAndPopulateL1` and `resolveOnceProd` between major
   sub-resolutions (between cohort N and cohort N+1, similar to
   `engineYieldCheckpoint` at `prewarm_engine.go:316-322`). Not in this
   ship's scope to keep LOC contained; #98 ships the processOne yield
   only. A burst arriving MID-cohort will still complete that cohort
   (and the refresher releases CPU as it returns from
   resolveWidgetForRefresh) — the per-cohort granularity is fine for
   the dominant case of long-running cohort scans.

### What this does NOT change

- Refresher correctness (TTL outer-net, exponential backoff, poison-
  pill bound) — unchanged. Yield is BEFORE the handler call; if the
  handler succeeds, `Forget(key) + completedTotal++` still happens.
- L1 / apistage / cohort memo semantics — unchanged.
- Customer dispatch path — unchanged (still increments
  customerInFlightCount; the increment is already in production at
  `restactions.go:77`).
- Refresher parallelism (default 4 from `defaultRefresherParallelism`,
  `refresher.go:57`) — unchanged. All 4 workers yield independently.

### Will #98 unmask another bottleneck?

YES — INFERRED. With refresher CPU yielded during customer burst, the
next-binding constraint will likely be **wire write back-pressure**
(long-pole #2 in §2 above; 20 customer goroutines in IO wait throughout
the burst). That long-pole is NOT a #98 attack target — it is structural
to the 35MB payload × gzip-decompressing client. A subsequent #99
addressing wire / payload (pre-encoded gzip, slice limits, or
client-side pagination) would be the next ranked attack — INFERRED, not
guaranteed; the post-#98 chrome-mcp re-measurement will rank what's
left. State of #98: **mechanism gate that closes the #97 regression
AND may or may not be the LAST mechanism ship — the residual will be
known only post-deploy.**

---

## §4 Pre-flight falsifier (HARD GATE)

Per `feedback_falsifier_first_before_ship` + `feedback_empirical_baseline_gate`,
dev MUST capture this before any production code:

### Capture (mirrors §2's protocol)

1. Verify GKE context (`feedback_kubectl_verify_gke_context`).
2. kubectl port-forward to current 0.30.214 pod port 8081 (diagnostic only).
3. Run 60s burst with 20 concurrent admin RA compositions-panels (the
   `/tmp/ship98-fresh-baseline/burst.py` script — copy under a new
   bench dir to keep artifacts clean).
4. At t=15s capture pprof in parallel:
   - `/debug/pprof/profile?seconds=30` → `cpu_prefix.prof`
   - `/debug/pprof/mutex` → `mutex_prefix.prof`
   - `/debug/pprof/goroutine?debug=2` → `goroutine_prefix_debug2.txt`

### Pre-fix expected (from §2)

- Refresher cum CPU% on `cpu.prof`: **45-55%** (TRACED today 50.28% on
  0.30.214 burst; ±10% empirical noise envelope).
- Refresher goroutines (4) in `Cond.Wait` AT IDLE; in `runnable` /
  `Resolve.func5` during burst peaks.

### Two-sided HALT band (`feedback_empirical_baseline_gate` symmetry)

| Bound | Trigger | Action |
|---|---|---|
| **LOWER** | Refresher cum CPU < **22.6%** (50.28% / 2.22 = half of expected) | Mechanism shifted — refresher already idled. The /compositions regression is NOT this defect; HALT, re-engage architect. Likely culprit is a transient pod state (refresher ran a cohort sweep just before burst) or a baseline drift. |
| **UPPER** | Refresher cum CPU > **75%** | Customer-side mechanism has further collapsed beyond #97. HALT, re-baseline customer-path CPU% — if request path is now <10%, the binding constraint is something other than refresher CPU pressure (possibly GC heap thrash or scheduler starvation), and approach (c) is not the right fix. |
| **PROCEED** | Refresher cum CPU ∈ [22.6%, 75%] AND request path cum CPU ∈ [10%, 30%] (TRACED today ~21%) | Mechanism intact; the contract violation is the refresher's lack of yield. Write code. |

### Post-fix expected

- Refresher cum CPU during admin burst: **< 15%** (yielded for ~95% of
  the burst window; the residual is the time between yield re-checks
  at 25ms cadence plus refresher work that started before customer
  arrived).
- Customer request cum CPU: **> 30%** (gets the cores the refresher
  released — but burst is bounded by wire back-pressure (long-pole #2),
  so this number is INFERRED — it MAY plateau lower if wire-wait
  saturates first).
- Per-goroutine evidence: 4 refresher workers in `Cond.Wait` (workqueue
  block) OR in the new `t.C` yield-park, NOT in `Resolve.func5` running
  CPU work, throughout the burst window.

### What "falsifier failed" means at ship time

If post-fix dev re-runs the burst on 0.30.215 and refresher cum CPU stays
≥30%, the yield mechanism did not engage — HALT, do not deploy.
Diagnosable cases: customerInFlightCount stayed 0 (signal broken at
ServeHTTP increment), refresher hook not wired (cache.SetCustomerInflightHook
not called from main.go), or yield loop has a logic bug. None of these
are deploy-time discoveries; the post-fix burst pprof IS the deploy
gate.

---

## §5 Acceptance criteria (12 ACs, falsifiable, anchored to north-star)

<!-- [C1 DISCHARGE] AC-98.2 row carries inline INFERRED tag per PM verdict condition C1 -->
<!-- [C2 DISCHARGE] AC-98.12 added per PM verdict condition C2; refresherYieldMaxParked tightened 10s → 5s in §3 plumbing #3 -->

All thresholds tied to Chrome MCP via portal LB (`feedback_no_kubectl_in_measurement`)
and to TRACED pre-fix numbers from §2. TRACED-pre-fix vs INFERRED-may-plateau
labels added inline per `feedback_architect_design_rigor` and PM verdict C1.

| # | Criterion | Measurement tool | Pre-fix (TRACED) | Pass threshold |
|---|---|---|---|---|
| **AC-98.1** | admin /compositions warm `lastCallEnd` — `TRACED pre-fix` | Chrome MCP, portal LB | 11,005ms (0.30.214) | **≤ 6,000ms** (closes #97's +15% regression with margin; brief target). Stretch: ≤ 5,500ms. |
| **AC-98.2** | mix-weighted `piechart_correct` warm (0.95×cj + 0.05×admin) — `INFERRED — may plateau due to wire long-pole; #99 candidate if missed` <!-- [C1 DISCHARGE] inline INFERRED tag per PM verdict C1 --> | Chrome MCP, portal LB | 1,989ms (0.30.214) | **≤ 800ms** (the north-star, still pending from #97) |
| **AC-98.3** | Refresher cum CPU% under 60s admin burst — `TRACED pre-fix` | pprof CPU on port-forward (diagnostic only) | 50.28% (TRACED) | **≤ 15%** post-fix |
| **AC-98.4** | cj /compositions warm `lastCallEnd` — `TRACED pre-fix` | Chrome MCP, portal LB | 1,304ms (0.30.214) | **≤ 1,800ms** (preserve #97's −14.9% improvement; no regression) |
| **AC-98.5** | Pod restartCount over 30-min sustained burst | `kubectl get pod` | 0 | 0 |
| **AC-98.6** | RBAC symmetry — Group-only RoleBinding cohort | curl admin + cj test RA | n/a (orthogonal to fix) | served items match per cohort (`feedback_predicate_subject_kind_symmetry`) |
| **AC-98.7** | `-race` test: 4 concurrent customer-inflight increments + 4 concurrent refresher dequeues + yield | `go test -race ./internal/cache/...` | n/a | zero race detector hits |
| **AC-98.8** | Per-goroutine evidence: refresher goroutines NOT in `Resolve.func5` during burst | `goroutine?debug=2` post-fix | 4 in `Cond.Wait` baseline; spikes to `Resolve.func5` during cycles today | refresher goroutines remain in `Cond.Wait` OR `t.C` yield-park during burst; ZERO in `Resolve.func5` |
| **AC-98.9** | Refresher post-burst recovery: time-to-first-completed after burst end | refresher `completedTotal` delta after burst-end timestamp | unmeasured today, but refresher runs nominally | first `completedTotal++` ≤ 500ms after burst-end (i.e. yield releases promptly) |
| **AC-98.10** | content-equivalence (no JQ shape drift) | `dispatch_delta` cj + admin on /dashboard piechart and /compositions panels (pre vs post-fix) | n/a | zero non-noise diff modulo per-request `status.traceId` |
| **AC-98.11** | LOC envelope | `git diff --stat` | n/a | **≤ +180 LOC prod + ≤ +350 LOC tests** (see §7); pause + review if exceeded |
| **AC-98.12** <!-- [C2 DISCHARGE] refresher convergence guard per PM verdict C2 --> | Refresher cache settle-time after a CRUD informer event — TRACED post-fix | debug log line `refresher: completed key=...` + `snowplow_refresher_completed_total` expvar delta (per-key), timestamped against the informer ADD/UPDATE/DELETE log line `cache: dirty-mark key=...` | **PRE-FIX BASELINE**: capture today on 0.30.214 (refresher fully un-yielded — represents the "best case" settle-time the yield must not regress beyond by more than the cap headroom). Method: `kubectl logs -f snowplow-<pod>` filtered to `dirty-mark` + `completed`, plus `curl :8081/debug/vars` snapshot before+after; compute Δt per key. Expected pre-fix: ≤ 3s under quiescent load. | **POST-FIX**: ≤ 10s under quiescent customer load (no admin/cj /call bursts). Falsifier: trigger a single `kubectl apply` of a Widget CR change on a known cohort, watch the debug log + expvar delta for `completedTotal++` on the affected key, assert Δt ≤ 10s. The new `refresherYieldMaxParked = 5s` cap (tightened from design's initial 10s, see §3 plumbing #3) leaves ~5s headroom for the actual resolve+populate work under quiescent load (TRACED ~1-3s/scope). Must hold across 5 successive CRUD events. |

AC-98.6 carried per `feedback_predicate_subject_kind_symmetry` (Ship 0.30.183 ζ
HARD REVERT lesson). AC-98.7 per `feedback_shared_vs_copy_is_a_concurrency_change`
(the customer-inflight signal is shared across goroutines; the refresher's
new yield reads it under race conditions). AC-98.9 added by architect to
catch the inverse defect (yield never releases, refresher backs up).
AC-98.10 added per Ship #97 condition #3 (four-cell dispatch_delta).
**AC-98.12 added per PM verdict condition C2** — guards the convergence
budget against the cooperative yield's worst-case park duration. PM left
the `refresherYieldMaxParked` cap to architect judgment; tightened from
10s to **5s** (see §3 plumbing #3 update) to leave ~5s headroom under
the 10s convergence SLA. Empirical-baseline-gate clause: dev MUST capture
the pre-fix Δt on 0.30.214 BEFORE the post-fix capture, to verify the
yield does not regress beyond the cap headroom.

---

## §6 Risk register

| Risk | Mitigation | AC mapping |
|---|---|---|
| **R-empirical-baseline-gate** (`feedback_empirical_baseline_gate`) — pre-fix baseline already drifted; mechanism may have shifted off refresher CPU pressure between today's design and dev's first commit | §4 falsifier two-sided HALT band; dev MUST re-capture mutex+CPU+goroutine on a freshly observed 0.30.214 pod before code, attach to ledger row | §4 + AC-98.3 |
| **R-second-order long-pole** (this ship's likely unmask) — with refresher CPU yielded, the next bottleneck on admin /compositions warm is **wire write back-pressure** (long-pole #2 in §2): 20 customer goroutines pinned in IO wait writing 35 MB to gzip-decompressing client. Post-fix `lastCallEnd` may plateau between 5-8s instead of dropping to the 1-2s structural floor. | The brief explicitly targets the +15% regression close (6,000ms band) NOT the deep structural floor. AC-98.1 at 6,000ms is reachable WITHOUT also fixing wire back-pressure. If post-fix Chrome MCP shows lastCallEnd ∈ [5,000, 6,500ms], #98 is GREEN on AC-98.1 and #99 is queued for wire/payload work. INFERRED: this is a "mechanism gate that closes the regression but is NOT the last ship before north-star" — wire long-pole likely needs its own ship. | AC-98.1 design margin + §3 explicit "will unmask" disclosure |
| **R-customer-tax-leak** (NEW from refresher state — `feedback_customer_priority_over_refresher`) — the yield ITSELF is on the customer-inflight signal path, not on the customer-/call path. If the yield's read of `customerInFlightCount` introduces a hot atomic that customer dispatches contend on, we have re-introduced a customer-tax pathway. | `customerInFlightCount.Load()` is a single atomic-int64 read. No cache-line bouncing on the read side. The hot path that INCREMENTS is `restactions.go:77` / `widgets.go:62` — those are unchanged. The refresher reads at 25ms cadence per yielding worker — 4 workers × 40/s = 160 reads/s = negligible. **AC-98.3 + per-goroutine evidence at AC-98.8 are the falsifier.** | AC-98.3 + AC-98.8 |
| **R-shared-vs-copy** (`feedback_shared_vs_copy_is_a_concurrency_change`) — the customerInFlight predicate is a shared atomic-int64 read from goroutines (4 refresher workers) that previously did not read it. Per the rule: "concurrency-safety change — needs `-race` test, not content-equivalence check." | AC-98.7 mandates `-race` over 4+ refresher workers + 4+ customer goroutines simultaneously incrementing/decrementing the inflight counter. Test scaffolding: lift the inflight counter into a small testable-in-cache primitive (one-time refactor) OR inject the predicate as a func, mock it under race. Dev picks. | AC-98.7 |
| **R-yield-stall-deadlock** (NEW from architect) — if a customer /call's dispatch chain spawns a refresher path that needs to complete BEFORE the customer call can release the inflight counter, the refresher waits for the customer, the customer waits for the refresher — deadlock. | The refresher is **fire-and-forget** w.r.t. /call (no synchronous /call-to-refresher dependency by design at refresher.go: the refresher is dirty-mark-driven and runs in its own worker pool). However, defense-in-depth: add `refresherYieldMaxParked` (**5s** per C2 discharge — tightened from initial 10s to leave AC-98.12 convergence headroom) cap on yield duration — beyond that, refresher proceeds regardless of `customerInFlight`. Below: no /call dispatch path in production code blocks on refresher completion (the dispatchers/restactions.go path consults L1; on miss it dispatches inline, no refresher wait). | §3 plumbing #3 (max-parked cap); AC-98.9 (post-burst refresher recovery ≤ 500ms — catches stall too); AC-98.12 (CRUD-to-completed Δt ≤ 10s — catches convergence regression from the yield itself) |
| **R-convergence-window** <!-- [C2 DISCHARGE] PM-added risk per verdict --> (NEW from PM verdict) — sustained customer pressure for the full `refresherYieldMaxParked` window can delay an informer-triggered refresh by up to the cap. P0 success-metric "convergence ≤ 10s after CRUD" is the SLA. | Cap tightened to **5s** (was 10s in initial design) — leaves ~5s headroom under the 10s SLA for the actual resolve+populate work (TRACED ~1-3s/scope quiescent). AC-98.12 directly falsifies under quiescent customer load (the worst-case-yield-time-equals-zero condition); for the under-pressure case, the cap upper-bounds it at 5s yield + 3s resolve = 8s settle. | AC-98.12 (CRUD-to-completed Δt ≤ 10s); §3 plumbing #3 (cap = 5s) |
| **R-special-cases** (`feedback_no_special_cases`) — fix introduces no GVR/user/path literals; the yield gate is uniform across all refresher handlers (`restactions`, `widgets`) regardless of class | The yield is in `processNext` before any handler dispatch — uniform across handlers. No carve-out. | code review + AC-98.10 |
| **R-no-park-broken-behind-flag** (`feedback_no_park_broken_behind_flag`) — no new flag introduced; yield is unconditional when customerInflight hook is set; off-state (hook nil at unit test time only) is byte-identical to today | `cache.SetCustomerInflightHook(nil)` is the test-only default; production wires it from `main.go` after `RegisterRefreshHandlers`. No prod env var, no opt-in/opt-out. | §3 explicit + §8 rollout |
| **R-cache-off-fallthrough** (`project_cache_off_is_transparent_fallback`) — CACHE_ENABLED=false MUST be transparent fallback; refresher is gated on cache-on already at `refresher.go:184 if !ResolvedCacheEnabled() return` so the entire yield code path is dormant when cache is off | verified at refresher.go:184; no code change needed for cache-off invariance | AC-98.5 implicit |

---

## §7 LOC envelope (honest)

Ship #97 was +25 production / +484 tests. Ship #98 is **slightly larger
production** because the customer-inflight signal lives in `dispatchers`
and the refresher is in `cache` — there's a small wiring delta. Estimate:

| File | Production LOC | Notes |
|---|---|---|
| `internal/cache/refresher.go` | +60 | `yieldToCustomer` method (mirror of `prewarm_engine.go:295-314`) + `customerInflightHook` package var + `SetCustomerInflightHook` setter + call site in `processNext` + `refresherYieldMaxParked` ceiling |
| `internal/handlers/dispatchers/prewarm_engine.go` | +5 | Export `customerInFlight()` as `CustomerInFlight()` (one-line rename, or new exported wrapper) |
| `internal/handlers/dispatchers/dispatchers.go` (or `main.go`) | +8 | Wire `cache.SetCustomerInflightHook(dispatchers.CustomerInFlight)` once, near `RegisterRefreshHandlers` |
| **Production total** | **+73** | Bounded |
| `internal/cache/refresher_customer_yield_test.go` (new) | +220 | Yield-engages test, yield-releases test, max-parked-cap test, -race with concurrent inflight ↑/↓ + refresher dequeue |
| `internal/cache/refresher_yield_falsifier_test.go` (new) | +120 | End-to-end falsifier: ramp customer inflight, observe refresher.completedTotal does NOT advance until inflight=0 (within yield-cadence + max-parked-cap envelope) |
| **Test total** | **+340** | |
| **Grand total** | **+413** | |

LOC pause threshold: if total exceeds **+180 prod + +350 tests = +530**,
dev pauses and reviews with architect+PM. Mirrors Ship #97's pause
threshold scaled for the slightly larger surface.

If test scaffolding requires a new package-internal seam (e.g. exported
`customerInFlightFor(testing.T)` helper to drive the counter without
touching the dispatcher entry points), that's expected and within
budget. The dev MUST NOT change the customer-inflight semantic
(increment at ServeHTTP entry, decrement at exit) — that is the contract.

---

## §8 Rollout

- **Single binary** `ghcr.io/braghettos/snowplow:0.30.215` (the next clean
  slot after 0.30.214).
- **Single CACHE_ENABLED flag end-state preserved** (`project_single_cache_flag_direction`).
  No new env vars. No values.yaml changes. No new chart inputs.
- **Lockstep chart tag** `snowplow-0.30.215` on `braghettos/snowplow-chart`,
  pushed **explicitly to braghettos** (chart repo `origin`=upstream per
  `feedback_chart_repo_origin_is_upstream`).
- **Deploy** via `helm upgrade snowplow braghettos/snowplow --version 0.30.215`
  (NOT `kubectl set image`, NOT `kubectl apply` —
  `feedback_chart_only_for_snowplow`, `feedback_never_kubectl_apply`).
- **Deploy gate**: pod /readyz=200, phase1Done=true,
  refresher_yielded_total metric increments under a brief admin /call
  load, all within 5 minutes of rollout.

---

## §9 Revert plan

- **Tag rollback to 0.30.214** (NOT 0.30.212 — Ship #97's mechanism win
  stays in place). Per `feedback_no_park_broken_behind_flag` — not a
  flag toggle.
- Procedure: `helm upgrade snowplow braghettos/snowplow --version 0.30.214`
  (chart 0.30.214 lockstep tag stays on `braghettos/snowplow-chart`).
- Persistent state to roll back: none. The customer-inflight signal
  is in-process atomic counter (lost on pod restart, re-warmed by
  customer traffic). The refresher's workqueue is in-process. No
  Redis state, no L1 schema change. Forward + backward compatible.
- Acceptable revert wall-clock: ≤ 3 min (one helm upgrade).

---

## §10 PM gate question discharge (architect-side preparation)

Mirroring Ship #97's PM verdict shape so PM has 10 ready conditions:

1. **Would the fix make the symptom disappear?** — INFERRED via §4
   falsifier on /compositions admin warm. The mechanism (refresher CPU
   yield) maps to the symptom (admin /compositions warm lastCallEnd
   +15%) via the chain: refresher CPU pressure → customer request CPU
   starvation → per-call wall-clock extension. AC-98.1 at 6,000ms is
   the symptom-disappear threshold. Per the unmask-risk in §6 (R-second-
   order long-pole), the fix MAY plateau at 5-8s rather than reach the
   1-2s structural floor — and that's OK; the brief targets the +15%
   regression close, not the deep floor.

2. **Is the scope correction defensible?** — N/A; no scope reduction
   in #98. The fix surface is the refresher's processNext path; no
   other Put/Get sites touched.

3. **Does the design honour `feedback_empirical_baseline_gate`?** — YES;
   §2 captured fresh pprof + §4 codifies a two-sided HALT band before
   any production code.

4. **Risk register completeness?** — 9 risks enumerated (8 original +
   R-convergence-window added per PM verdict C2); R-empirical,
   R-second-order, R-customer-tax, R-shared-vs-copy, R-yield-stall,
   R-convergence-window, R-special-cases, R-no-flag, R-cache-off all
   addressed with AC mapping.

5. **LOC envelope honesty?** — §7 lists +73 prod / +340 tests / +413
   grand total with explicit ceiling at +530 (pause threshold). Honest
   estimate; not a 2× sandbag.

6. **Per-goroutine post-fix evidence** (`feedback_per_goroutine_evidence_beats_cpu_pprof`)
   — AC-98.8 mandates refresher goroutines NOT in `Resolve.func5` during
   burst (`Cond.Wait` or yield-park state only).

7. **`-race` requirement** — AC-98.7 mandates 4-reader 4-writer concurrent
   race test on the customer-inflight signal + yield loop.

8. **No new flags / no special-cases / cache-off transparent** —
   §3, §6 all explicit.

9. **Chart lockstep + braghettos push + helm upgrade** — §8 explicit
   with all three memory rules cited.

10. **Three-way pre-commit review** (`feedback_dev_review_with_architect_pm_before_commit`)
    — design is final pre-commit; dev must share diff with architect
    (this file) + PM (post-PM-verdict file) before tag/push.

---

## Artifacts cited

- `/Users/diegobraga/krateo/snowplow-cache/snowplow/docs/admin-ra-empirical-pprof-2026-05-31.md` (prior pprof on 0.30.212)
- `/Users/diegobraga/krateo/snowplow-cache/snowplow/docs/ship-97-canonical-chrome-mcp-2026-05-31.md` (regression evidence)
- `/Users/diegobraga/krateo/snowplow-cache/snowplow/docs/ship-97-pm-gate-verdict-2026-05-31.md` (verdict template followed)
- `/tmp/ship98-fresh-baseline/cpu.prof` (fresh 0.30.214 burst CPU profile, TRACED to 50.28% refresher cum CPU)
- `/tmp/ship98-fresh-baseline/mutex_mid.prof` (corrects brief premise on "rate limiter")
- `/tmp/ship98-fresh-baseline/goroutine_mid_debug2.txt` (per-goroutine evidence on 0.30.214 burst)
- `/tmp/admin-ra-pprof-2026-05-31/mutex_mid.prof` (yesterday's mutex on 0.30.212 — same dictMu pattern, same NO-rate-limiter conclusion)

— cache-architect, 2026-05-31. Read-only on code. No commits.
