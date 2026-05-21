# Production Safety Net Analysis — Upstream-Controller-Churn Failure Mode

**Date**: 2026-05-21
**Author**: Architect
**Status**: ANALYSIS (not a ship design). Diego decides which Layer(s) to ship.
**Related**: D.5 cluster-list-when-allowed, Ship G identity-free content layer (both in flight).

---

## Executive Summary

Today snowplow is at risk of **CPU saturation + apiserver-pressure feedback** when one or more upstream controllers crash-loop at high object cardinality (490K-4.9M compositions × 5+ controllers). The empirically-observed signal today (37 widget L1 re-stores in 90s, ~25/min, 1× core-provider at 1,215 restarts on 49K objects) is *bounded* by stale-while-revalidate (`internal/cache/refresher.go:7-8`, Ship C / 0.30.112) and by client-go's `workqueue.NewTypedRateLimitingQueue` dedup (`internal/cache/refresher.go:150-156`). **Layer 0 is structurally sound** — informer-event flood, dirty-mark dedup, retry backoff, poison-pill bound (Part A, `internal/cache/refresher.go:74`, `internal/cache/refresher.go:275`), and the LRU+TTL+byte cap on L1 (`internal/cache/resolved.go:636-647`) are already in place.

The **gap** is two-fold:
1. **No overall throughput cap on the refresher.** Client-go's `DefaultControllerRateLimiter` (prior art at `/Users/diegobraga/go/pkg/mod/k8s.io/client-go@v0.29.0/util/workqueue/default_rate_limiters.go:38-46`) layers a 10qps / 100-burst token bucket **on top of** the per-item exponential limiter via `MaxOfRateLimiter`. Snowplow uses **only** the per-item exponential limiter (`internal/cache/refresher.go:150-153`). A storm of distinct dirty-marks (e.g., 4.9M compositions × N users) can push the worker pool to 100% CPU because there is no overall ceiling — only per-key dedup.
2. **No queue-depth gauge exposed.** Workqueue `Len()` is available (`/Users/diegobraga/go/pkg/mod/k8s.io/client-go@v0.29.0/util/workqueue/queue.go:187`) but never published. We can only INFER saturation from the existing `refresh_enqueued - refresh_completed` delta in the 5-min summary line (`internal/cache/resolved.go:755-756`), which is too coarse for an alert.

**Recommended next-ship priority**: **Layer 1 (observability)** first, because every threshold below requires a calibration point and we have no live gauge today; then **Layer 2 (overall token-bucket rate-limit on the refresher queue)** because it is a 5-line wiring change using prior-art primitives. Layers 3-4 are deferred until Layer 1 surfaces evidence we need them.

**Ship G is itself a structural mitigation** — collapses N per-user widget L1 entries → 1 identity-free entry → dirty-mark fanout drops by N (the user-count factor). At customer scale (1000 users) this is a 1000× reduction in refresher work per controller event. Layer 2 should ship even with Ship G in flight because Ship G is identity-free at the **content** layer only; the **widget** layer remains per-user-keyed per `feedback_l1_per_user_keyed_never_cohort.md`, so widget-L1 churn at user scale persists.

---

## 1. Layer 0 — What Snowplow Already Has

These are TRACED, not aspirational.

### 1.1 Refresher queue: client-go workqueue with bounded exponential backoff
- **File**: `internal/cache/refresher.go:13-32`, `150-156`
- **Properties (TRACED)**:
  - Idempotent dedup: `Add(key)` of an already-queued key is a no-op (`internal/cache/refresher.go:18-19`). Today's 4 widget keys × 25 events/min coalesce to 4 keys queued, not 100/min.
  - Never drops: queue is unbounded (`internal/cache/refresher.go:20-21`). A burst past any buffer is queued, not dropped.
  - Bounded exponential-backoff retry: `AddRateLimited` requeues a failed key after a delay capped at `defaultRefresherMaxDelayMS=60_000` (`internal/cache/refresher.go:61`, `290`).
- **Properties NOT provided (INFERRED from client-go source)**:
  - **No overall qps cap.** The rate limiter is `NewTypedItemExponentialFailureRateLimiter` only — per-key delay, not per-pool. Compare client-go's `DefaultControllerRateLimiter` which wraps the same per-item limiter in a `MaxOfRateLimiter` with a `BucketRateLimiter{rate.NewLimiter(10, 100)}`.

### 1.2 Refresher poison-pill bound (Ship 0.30.113 Part A)
- **File**: `internal/cache/refresher.go:64-74`, `275-285`
- A key that fails `maxRefreshRequeues=5` times is Forgotten and dropped; the L1 entry stays under TTL. Prevents worker-pool spin on deterministic failures (deleted CR, malformed spec).

### 1.3 Refresher parallelism: bounded worker pool
- **File**: `internal/cache/refresher.go:57`, `193-207`
- `defaultRefresherParallelism=4`. Workers exit cleanly on ctx-cancel (`internal/cache/refresher.go:202-213`). No goroutine leak.

### 1.4 Stale-while-revalidate
- **File**: `internal/cache/refresher.go:5-8`, `feedback_l1_invalidation_delete_only.md`
- UPDATE/PATCH never evicts; serves existing entry instantly while refresher re-resolves in background. DELETE evicts only the deleted object's own self-representation; LIST-deps and dependent-GET-deps are dirty-marked, not evicted (`internal/cache/deps.go:561-646`).

### 1.5 L1 store: LRU + TTL + byte-budget cap
- **File**: `internal/cache/resolved.go:172-205`, `460-540`, `636-647`
- `defaultResolvedCacheMaxEntries=100_000` (`internal/cache/resolved.go:61`), `defaultResolvedCacheMaxBytes=2 GiB` (`internal/cache/resolved.go:62`), `defaultResolvedCacheTTLSeconds=3600` (`internal/cache/resolved.go:63`). LRU evicts when either bound is exceeded; TTL evicts lazily on read.

### 1.6 Dep tracker: bounded record count
- **File**: `internal/cache/deps.go:27-29`, `275`, `466-480`
- `defaultDepsMaxRecords=1_000_000`. New `Record` calls are silently dropped past the cap; counter `dep_record_dropped_cap` increments (`internal/cache/deps.go:919`). One-shot WARN. TTL outer net keeps cache correct.

### 1.7 Informer-event ADD-replay gate
- **File**: `internal/cache/deps_watch.go:9-14`
- During initial LIST replay every object arrives as an ADD; the gate drops these pre-sync so the refresher does NOT see a `refresh-all` storm on pod boot. Counter `dep_add_dropped_pre_sync` (`internal/cache/deps_watch.go:45`).

### 1.8 DELETE-event off-processor handoff
- **File**: `internal/cache/deps_watch.go:16-22`, `35-39`
- DELETE events are queued (depth 1024) to a single bounded worker; informer processor goroutine never blocks on the eviction burst. Falls back to inline OnDelete on a full queue (counter `delete_queue_full`).

### 1.9 503 readiness gate + CACHE_ENABLED toggle
- **File**: `internal/handlers/readyz.go`, `main.go` Phase 1 wiring
- Pod returns 503 until Phase 1 informer warmup completes. `CACHE_ENABLED=false` runtime toggle reverts to pure 0.25.x apiserver-routed mode (`internal/cache/resolved.go:228-238`).

### 1.10 expvar surface (process self-introspection)
- **File**: `internal/cache/fallthrough_meter_expvar.go:19-43`, `main.go:611-631`
- Already mounted at `GET /debug/vars`. Three counters live today: `snowplow_apiserver_fallthrough_total`, `snowplow_apiserver_fallthrough_cells`, `snowplow_assertion_violations_total`. **The refresher counters and L1 stats are NOT yet on expvar** — they only emit in the 5-min summary log line (`internal/cache/resolved.go:747-778`).

### 1.11 Prior-art primitives NOT used (gap analysis)
- `workqueue.BucketRateLimiter` (`/Users/diegobraga/go/pkg/mod/k8s.io/client-go@v0.29.0/util/workqueue/default_rate_limiters.go:47-52`) — overall qps cap. Not in snowplow.
- `workqueue.MaxOfRateLimiter` (`/Users/diegobraga/go/pkg/mod/k8s.io/client-go@v0.29.0/util/workqueue/default_rate_limiters.go:172-211`) — combine per-item + overall. Not in snowplow.
- `workqueue.MetricsProvider` (`/Users/diegobraga/go/pkg/mod/k8s.io/client-go@v0.29.0/util/workqueue/metrics.go:172-209`) — depth, adds, latency, retries, longest-running-processor. Not wired.
- `Len()` on the typed interface — available but not exposed.

---

## 2. Failure-Mode Scenario Analysis

For each scenario: detection signal (file:line where measurable), threshold (or explicit calibration gap), and impact bucket.

### Scenario 1 — Refresher queue overflow (UNREALISTIC for current shape)

**Hypothesis**: dirty-marks arrive faster than 4 workers drain. Queue grows unbounded → memory pressure → OOM.

**Detection**:
- `queue.Len()` at `internal/cache/refresher.go:260` — workqueue Type.Len() at `/Users/diegobraga/go/pkg/mod/k8s.io/client-go@v0.29.0/util/workqueue/queue.go:187` (TRACED, available, NOT exposed today).
- Today: 37 events / 90s, 4 distinct keys after dedup, queue.Len ≈ 0-4. Stable.

**Per-entry memory cost (TRACED, INFERRED conservatively)**:
- Workqueue stores `string` keys. L1 key is `sha256/base64` (~44 chars). Per queued key ~64 bytes including overhead.
- At 10M queued keys: 640 MB. **The unbounded queue is not a near-term OOM risk in the current key shape** because the key set IS bounded by `defaultResolvedCacheMaxEntries=100_000` (`internal/cache/resolved.go:61`) — only L1 keys with an existing entry can be dirty-marked (a refresh `processOne` on a missing entry is a no-op skip, `internal/cache/refresher.go:310-318`). Worst-case queue size = 100K keys ≈ 6.4 MB. Bounded by construction.

**Calibration gap**: NONE — this is structurally bounded.

**Severity**: Low. Not a P0 mitigation target.

---

### Scenario 2 — Refresher CPU saturation (PRIMARY production risk)

**Hypothesis**: dirty-mark rate × per-resolve cost (jq + RBAC + marshal + apiserver fan-out) saturates the 4-worker pool. /call latency tail blows up because workqueue contention starves Phase 1 / live request resolves.

**Today's empirical signal**:
- 37 widget refreshes / 90s = 0.41 refreshes/sec across 4 workers = ~0.1 refreshes/sec/worker.
- Estimated per-widget resolve cost: 50-500 ms (highly INFERRED — calibration gap; needs per-handler resolve-latency histogram). At 200 ms typical, 4 workers can drain ~20 refreshes/sec sustained.
- Today: 0.41 / 20 = ~2% CPU on refresher. No saturation.

**Worst-case scaling math** (INFERRED, calibration gap on per-resolve cost):

| Factor | Today | 10× | 100× | 100× + 5 controllers |
|--------|-------|-----|------|----------------------|
| Compositions | 49K | 490K | 4.9M | 4.9M |
| Crash-loop refreshes/min | 25 | 250 | 2,500 | 12,500 |
| Per-key dedup | 4 keys | unknown | unknown | unknown |
| **With Ship G (identity-free)** | 4 keys × N users | 4 × N | 4 × N | 4 × N × 5 |
| **Without Ship G (per-user)** | 4 keys × N users × 25/min | ... | ... | catastrophic |

**Critical observation**: per-user widget L1 keying (`feedback_l1_per_user_keyed_never_cohort.md`) means a single composition event dirty-marks N entries where N = users-who-saw-the-widget. At 1000 users × 25 events/min × 4 widgets = 100K dirty-marks/min, dedup-coalesced to 4×N = 4000 distinct keys. **4000 distinct keys to refresh per minute → 67/sec → ~3.3× the 20/sec drain capacity → backlog grows + workers saturate.**

**Detection (file:line, available today, NOT exposed)**:
- `r.queue.Len()` at `internal/cache/refresher.go:260`. Read via `r.completedTotal.Load()` and `r.enqueueTotal.Load()` rate diff (`internal/cache/refresher.go:296`, `242`).
- `refresh_enqueued - refresh_completed` rate over 60s — proxy for backlog growth.
- /call latency p99 — already in HTTP middleware (not enumerated here; INFERRED).

**Threshold candidates** (CALIBRATION GAP — need load test):
- Healthy: enqueue-rate ≤ completion-rate (steady-state).
- Yellow: enqueue-rate > 1.5× completion-rate sustained for >60s.
- Red: queue.Len() > 1000 sustained for >30s OR refresh-rate p95 > 5s.

**Severity**: **HIGH** at customer scale. This is the primary production risk.

**Mitigation match**: Layer 2 (overall token bucket on refresher queue) — cap refresh rate to e.g. 50/sec, smooths bursts, slower convergence to fresh data is acceptable per stale-while-revalidate contract.

---

### Scenario 3 — L1 entry capacity explosion (LOW risk by construction)

**Hypothesis**: dirty mark + miss → fresh resolve → NEW entry. If invalidation pattern produces NEW keys (not just refreshes existing), entry count grows unbounded.

**Why this CANNOT happen** (TRACED):
- The refresher only re-resolves keys that ALREADY have an L1 entry. `processOne` checks `entry, ok := c.Get(key)` at `internal/cache/refresher.go:310` and returns `skippedNoEntryTotal` if missing (`internal/cache/refresher.go:313-317`). It does NOT create new entries from dirty-marks alone — the entry was created by a prior /call resolve.
- L1 LRU cap `defaultResolvedCacheMaxEntries=100_000` (`internal/cache/resolved.go:61`) + byte cap (`internal/cache/resolved.go:62`) bounds total entries. Eviction is unconditional once over cap (`internal/cache/resolved.go:636-647`).

**Detection**: `s.Entries` in summary log (`internal/cache/resolved.go:749`). `s.EvictLRUTotal` (`internal/cache/resolved.go:752`).

**Threshold**: entries → max_entries × 0.95 sustained = budget under-sized (calibration gap; today's customer cardinality unknown).

**Severity**: **MEDIUM-LOW**. Bounded by construction; risk is *eviction pressure* (entries churning rather than reused) → cache hit-rate degradation, not memory explosion. Existing signal: `apistage_evict_pressure` (`internal/cache/resolved.go:776`) is exactly this metric for the api-stage content layer; an equivalent for restactions/widgets entries is NOT exposed but `EvictLRUTotal / StoreTotal` from existing counters can be computed.

---

### Scenario 4 — Informer-event flood (BOUNDED by Ship A 0.30.110)

**Hypothesis**: upstream controller crash-loop produces N resyncs/min × 49K objects each → snowplow informer is overwhelmed before refresher even fires.

**Bounds in place (TRACED)**:
- ADD-replay gate (`internal/cache/deps_watch.go:9-14`): initial-LIST ADDs drop before reaching dep tracker. A controller-driven resync IS an initial replay from the informer's perspective only on the FIRST sync per process; subsequent OnUpdate events on the same objects still flow.
- DELETE off-processor handoff (`internal/cache/deps_watch.go:16-22`).
- Per `internal/cache/deps_watch.go:36-39`: a full delete-evict queue falls back to inline OnDelete (correctness over R3 guarantee).

**Gap**: An upstream controller in crash-loop produces **OnUpdate** events (not ADD/DELETE) for objects whose `resourceVersion` advances on each restart-induced reconcile pass. These OnUpdate events flow synchronously through `DepTracker.OnUpdate` → `enqueueFn` → refresher queue. This is what we measure as 37/90s today. There is no rate-limit between the informer event delivery and the queue Add.

**Detection**:
- `dep_dirty_mark_total` rate from summary log (`internal/cache/resolved.go:765`, `internal/cache/deps.go:334`).
- `cache_event.consumed` INFO line at `internal/cache/deps.go:549-557` — per-event log; cardinality already throttled by informer event-rate, not synthetic.

**Severity**: **MEDIUM** — bounded by stale-while-revalidate (no user impact today) but a high-rate informer event stream IS the *input* to Scenario 2. Address by capping the refresher OUTPUT rate (Layer 2), not the informer event rate.

---

### Scenario 5 — Downstream apiserver pressure (HIGH risk at scale)

**Hypothesis**: refresher re-resolve fans out to apiserver for per-NS iterations; 37/90s × 80 NS = ~33/s apiserver GETs sustained. At 100× scale → 3,300/s → cluster-wide ratelimit.

**TRACED today**:
- Refresher invokes the registered `RefreshFunc` per kind at `internal/cache/refresher.go:332`. For widgets/restactions the func re-runs the resolver which dispatches via the informer (`internal/cache/dispatch_via_informer.go` — not enumerated here, INFERRED present from refs in `internal/cache/deps.go:155-170`).
- **The informer IS the cache** — most resolver fan-out hits the informer indexer, not the apiserver. Apiserver fallthrough is metered (`internal/cache/fallthrough_meter.go:191-195`, `snowplow_apiserver_fallthrough_total`).
- However, the dep tracker dirty-marks ONLY entries with recorded dep edges. A refresh re-resolve walks the resolver again, which may hit informer-not-synced / RBAC-deny / write-verb / external-URL fallthrough cells.

**Detection (TRACED)**:
- `snowplow_apiserver_fallthrough_total` at expvar (`internal/cache/fallthrough_meter_expvar.go:24`). Per-cell breakdown at `snowplow_apiserver_fallthrough_cells` (`internal/cache/fallthrough_meter_expvar.go:35`). **Already exposed.**
- 1-WARN-per-100-fallthroughs sampling (`internal/cache/fallthrough_meter.go:217-228`).

**Threshold**:
- Today: fallthrough rate is the existing tracked-and-asserted invariant (Ship D). Expected near-zero in steady state. A sustained non-zero rate during a refresh storm IS the smoking gun.

**Severity**: **HIGH if refresh storm coincides with fallthrough.** Already detectable; **the gap is alerting, not measurement**. Layer 5 — wire `snowplow_apiserver_fallthrough_total` rate-of-change to an external alert.

---

### Scenario 6 — User-facing symptom

**TRACED today**:
- **503**: only Phase 1 readiness (`internal/handlers/readyz.go:52`). Not emitted under refresh load.
- **Stale data without indication**: this is the *expected* behaviour during a refresh storm — stale-while-revalidate serves the existing entry instantly. User cannot tell the data is being re-resolved in the background. Per `feedback_validate_content_not_just_status.md`, status-only checks would mask this.
- **Stuck loading spinner**: would manifest if `/call` p99 latency blew up (Scenario 2 saturated). No /call timeout-with-stale-fallback path today (INFERRED — `internal/handlers/dispatchers/dispatchers.go` would need to be re-traced for the exact request lifecycle, deferred).

**Diego's flagged customer scenario**: "snowplow is not working". This maps most directly to:
- High /call latency (Scenario 2 → workers saturated → request resolves wait their turn).
- Cluster-wide apiserver ratelimit (Scenario 5 → fallthroughs fail → user sees errors).

**Severity**: **HIGH** for diagnosis — without Layer 1 observability there is NO way to distinguish "snowplow misconfigured" from "upstream controller crash-looping at scale". Operators will blame snowplow because that is what their dashboard says is slow.

---

## 3. Layered Mitigations

Ordered cheapest → most expensive. Each layer is additive; later layers depend on earlier-layer signals to make decisions.

### Layer 1 — Observability (CHEAPEST, HIGHEST VALUE)

**What**: expose existing counters via `expvar` so operators can observe in real time without parsing 5-min summary log.

**Concrete proposal** (TRACED — leverages prior-art primitives):

1. Publish refresher gauges via `expvar.Publish` at process init (mirrors `internal/cache/fallthrough_meter_expvar.go:23-43`):
   - `snowplow_refresh_queue_depth` — `r.queue.Len()` via `expvar.Func` (TRACED available at `/Users/diegobraga/go/pkg/mod/k8s.io/client-go@v0.29.0/util/workqueue/queue.go:187`).
   - `snowplow_refresh_enqueued_total`, `snowplow_refresh_completed_total`, `snowplow_refresh_failed_total`, `snowplow_refresh_retried_total`, `snowplow_refresh_dropped_total` — `r.enqueueTotal.Load()` et al. (TRACED at `internal/cache/refresher.go:362-371`).
2. Publish L1 store gauges:
   - `snowplow_l1_entries`, `snowplow_l1_bytes`, `snowplow_l1_hit_rate`, `snowplow_l1_evict_lru_total`, `snowplow_l1_evict_ttl_total`, `snowplow_l1_evict_delete_total` — TRACED at `internal/cache/resolved.go:584-602`.
3. Publish dep tracker gauges:
   - `snowplow_deps_total_records`, `snowplow_deps_dirty_mark_total`, `snowplow_deps_record_dropped_cap` — TRACED at `internal/cache/deps.go:911-926`.
4. Optionally: wire client-go's `workqueue.SetProvider` (`/Users/diegobraga/go/pkg/mod/k8s.io/client-go@v0.29.0/util/workqueue/metrics.go`) to get per-key latency histograms for free.

**Cost**: ~50 LOC, single file (`internal/cache/refresher_expvar.go` mirroring `fallthrough_meter_expvar.go`). Zero runtime cost (atomic loads).

**Risk**: Negligible. Pure read-side surface; no behavioural change.

**Empirical threshold proposal (post-Layer-1 calibration test)**:
- Run 1: today's prod-shape (49K compositions, 1 crash-loop, 4 users) → capture baseline `queue_depth`, `enqueue_rate - completion_rate` deltas.
- Run 2: synth-load harness (Section 5) drives 10× / 100× / 1000× rates; record at what numbers /call p99 starts climbing.

**Calibration gap acknowledgment**: WITHOUT this layer we cannot rationally set Layer 2 thresholds. This is the strict prerequisite for any backpressure tuning.

---

### Layer 2 — Backpressure (5-LINE CHANGE; CHEAP; PRIOR-ART-BACKED)

**What**: cap the refresher's overall qps using client-go's `MaxOfRateLimiter + BucketRateLimiter` — exactly the `DefaultControllerRateLimiter` recipe.

**File**: `internal/cache/refresher.go:150-153`. **Today**:
```go
rl := workqueue.NewTypedItemExponentialFailureRateLimiter[string](
    time.Duration(baseMS)*time.Millisecond,
    time.Duration(maxMS)*time.Millisecond,
)
```

**Proposed** (TRACED prior-art from `/Users/diegobraga/go/pkg/mod/k8s.io/client-go@v0.29.0/util/workqueue/default_rate_limiters.go:38-46`):
```go
perItem := workqueue.NewTypedItemExponentialFailureRateLimiter[string](baseDelay, maxDelay)
overall := &workqueue.TypedBucketRateLimiter[string]{
    Limiter: rate.NewLimiter(rate.Limit(qpsFromEnv), burstFromEnv),
}
rl := workqueue.NewTypedMaxOfRateLimiter[string](perItem, overall)
```

(Cross-check: the typed variants of `MaxOfRateLimiter` / `BucketRateLimiter` exist in client-go v0.29+; if not present as generic wrappers, fall back to the non-typed `MaxOfRateLimiter` since the queue API still accepts the untyped `RateLimiter` interface for typed queues with a small adapter. INFERRED — verify against the actual client-go version pinned in `go.mod`.)

**Env knobs** (additive):
- `RESOLVED_CACHE_REFRESHER_QPS` (default 50) — overall qps cap across all keys.
- `RESOLVED_CACHE_REFRESHER_BURST` (default 200) — token-bucket burst size.

**Why these defaults** (CALIBRATION GAP, but reasoned):
- 50 qps × 4 workers = 12.5 refreshes/sec/worker → ~80 ms budget per refresh. Matches the INFERRED per-resolve cost ceiling above.
- Today's 0.41/sec is 1.2% of cap → no behaviour change at today's load.
- Customer 10× scale (~250 events/min coalesced) = ~4 distinct keys/sec → well under cap.
- Customer 100× scale + 5 controllers (~12,500 events/min) → coalesced to whatever the actual distinct-key set is. If distinct-keys > 50/sec the cap activates → slower convergence, but bounded refresher CPU.

**Trade-off**: slower convergence to fresh data. Per `feedback_l1_invalidation_delete_only.md` and stale-while-revalidate contract, slower-but-eventual-convergence is acceptable. User sees stale data briefly; the TTL outer net is 1 hour.

**Risk**: LOW. The token bucket only DELAYS dirty-marks; never drops them (the queue is still unbounded). Same correctness profile as today.

**Falsifier**: synth-load test (Section 5) — verify that at 1000 events/sec input, queue.Len stabilises around `burst` and never grows linearly.

---

### Layer 3 — Circuit Breaker (DEFER until Layer 1 data justifies)

**What**: at higher severity, stop accepting new dirty-marks; let entries stale per TTL.

**Trigger**:
- `queue.Len() > X` AND `enqueue_rate / completion_rate > Y` sustained for >Z seconds.
- All thresholds CALIBRATION-GAP; Layer 1 data required.

**Mechanism**:
- `Deps().SetRefreshHook(nil)` temporarily disables enqueues — refresh hook no-ops at `internal/cache/deps.go:537-543`. (Verify the nil-safe path at `internal/cache/deps.go:533` — yes, `enqueue` is nil-checked.)
- Re-enable hook once queue drains below threshold.

**User impact**: data goes stale-per-TTL during the breaker-open window. NO 503 on /call (serving stays correct from L1). User sees the cached snapshot from up to 1 hour ago until the storm subsides.

**Why DEFER**: this is a fail-safe for Scenario 2's worst case. We do not have evidence that Layer 2 fails to absorb the storm. Layer 1 data will tell us.

---

### Layer 4 — Graceful Degradation (DEFER unless Layer 3 insufficient)

**What**: progressive subsystem disabling at extreme load.

**Steps (severity-ordered)**:
1. Disable refresher entirely — entries stale per TTL. `StopRefresher()` at `internal/cache/refresher.go:226-231`. Existing API.
2. Disable per-user L1 — fall back to identity-free Ship G content + per-request RBAC narrowing. Requires Ship G complete.
3. Disable cache entirely — `CACHE_ENABLED=false` at `internal/cache/resolved.go:228-238`. Requires SIGHUP-style reload (today the env var is read at process boot; an in-process reload path does NOT exist — INFERRED, calibration gap).

**Per `feedback_no_park_broken_behind_flag.md`**: this is NOT parking a defect behind a flag — it is a runtime fallback when the system exceeds its design envelope. Different category.

**User impact**: clear ERROR indicator preferred over silent staleness. Today no UI banner exists (INFERRED — frontend scope).

**Why DEFER**: requires Ship G to land cleanly first.

---

### Layer 5 — Cross-team Escalation

**What**: external alerting on the metrics from Layer 1.

**Proposed**: chart consumes `expvar` (sidecar scraper → Prometheus → AlertManager → PagerDuty). Snowplow stays passive — emits metrics, ops integrates.

**Snowplow-side hooks (cheap)**:
- `slog.Error` at threshold crossings — operators with log-based alerting already see these (e.g., `internal/cache/refresher.go:278-283` already does this for dropped keys).
- `cache_event.consumed` rate already available (`internal/cache/deps.go:549-557`).

**No new code in snowplow required** if Layer 1 is shipped. Layer 5 is an integration-side task.

---

## 4. Interaction with Ship G

**Ship G context** (INFERRED from briefing — Ship G design docs not in this branch).

Ship G collapses widget L1 entries from per-user → identity-free at the content layer. Implication for this analysis:

| Dimension | Pre-G | Post-G |
|-----------|-------|--------|
| L1 entries per widget | N users | 1 |
| Dirty-marks per controller event | 4 keys × N users | 4 keys |
| Refresh work per crash-loop minute | 25 × N | 25 |
| Customer 1000-user case (today) | 25,000/min | 25/min |

**Ship G is itself a 1000× structural mitigation** for the per-user dirty-mark fanout. At customer scale, Ship G removes the dominant term in Scenario 2.

**However** — per `feedback_l1_per_user_keyed_never_cohort.md`, the L1 **must stay per-user-keyed at the widget output layer**. Ship G is identity-free only at the **content** layer (the apistage content entry). The widget L1 entry still keys on user; the dependency fan-out from a content-entry refresh to N widget L1 entries persists.

**Net (TRACED contract + INFERRED dep-graph behaviour)**:
- Layer 2 still needed: Ship G eliminates content-level user fanout, but widget-L1 refresh fanout to N users remains.
- Layer 1 still needed: regardless of Ship G, we need the gauges to set Layer 2 thresholds.
- Ship G + Layer 2 are complementary, not overlapping.

---

## 5. Empirical Falsifier for the Safety Net Itself

**Question**: how do we load-test the saturation point WITHOUT real production traffic?

**Proposal** — synthetic-churn harness:

1. **Set up**: kind cluster with `CACHE_ENABLED=true`, snowplow pod, N synth-users, K compositions seeded via direct apiserver POSTs.
2. **Workload**: a side process that PATCHes M compositions/sec, varying M = {1, 10, 100, 1000, 10000}. Each PATCH bumps `resourceVersion` → informer OnUpdate → dirty-mark fanout.
3. **Measurement (per M)**:
   - `snowplow_refresh_queue_depth` from Layer 1 expvar at 1Hz.
   - `snowplow_refresh_enqueued_total` rate at 1Hz.
   - `snowplow_refresh_completed_total` rate at 1Hz.
   - `/call` latency histogram from a parallel test driver hitting 1 widget every 100ms during the test.
   - Pod CPU + RSS from `/proc/self/status` over time.
4. **Pass criteria** (CALIBRATION GAP — to be filled by the run):
   - At M ≤ X: `enqueue_rate == completion_rate`, queue stays near 0, `/call` p99 ≤ baseline + 50ms.
   - At M > X (saturation): with Layer 2 active, queue stabilises near `burst`, `/call` p99 ≤ 2× baseline, pod CPU ≤ 80% of limit.
   - Without Layer 2: queue grows linearly, `/call` p99 climbs, pod CPU saturates.

**Why this is the right test**:
- Per `feedback_empirical_root_cause_trace_before_fix.md`: empirically verify the symptom-to-fix link BEFORE shipping.
- Per `feedback_falsifier_first_before_ship.md`: this is the pre-flight falsifier.
- Per `feedback_data_driven_workflow.md`: every threshold above becomes empirically grounded after this run.

**Effort**: ~1 day to write the harness, 1 day to run + analyse. Can run in parallel with Layer 1 implementation.

---

## 6. Ship-Candidate Sequence (recommended priority)

Order: ENABLE-OBSERVABILITY → CAP → CALIBRATE → CIRCUIT-BREAK (only if needed) → DEGRADE (only if needed).

| Order | Ship | Cost | Risk | Value | Blocker | Prereq |
|-------|------|------|------|-------|---------|--------|
| 1 | **Layer 1: expvar gauges** | ~50 LOC, 1 file | Negligible (read-only) | Unblocks every threshold below | None | None |
| 2 | **Synth-load harness** | ~1d eng | Low (test-only) | Calibrates thresholds + falsifies Layer 2 | None | Layer 1 |
| 3 | **Layer 2: token-bucket cap on refresher** | ~5 LOC + 2 env knobs | Low (slower convergence, same correctness) | Bounds Scenario 2 CPU | None | Layers 1+2 data |
| 4 | **Ship G (in-flight)** | (separate stream) | (own assessment) | 1000× structural reduction in fanout | None | (own gates) |
| 5 | Layer 3: circuit breaker | ~30 LOC | Medium (entries go stale-per-TTL on activation) | Fail-safe for storm beyond Layer 2 cap | Layer 1 evidence shows Layer 2 insufficient | Layer 1 |
| 6 | Layer 4: graceful degradation | TBD | High (runtime env-reload mechanism) | Only at extreme load | Layer 3 evidence | Ship G |
| 7 | Layer 5: external alerting | (chart-side) | Low | Operator visibility | None | Layer 1 |

**Top-of-list pair**: Layer 1 + Synth-load harness. **They are the gate for everything else.** Layer 2 ships immediately after they yield calibrated thresholds.

---

## 7. Open Questions for Diego

1. **Layer 2 default qps** — do we want a conservative 50/sec or higher? Calibration test will narrow it but the env-knob defaults are a deploy-time decision (chart values). Suggest: ship with 200/sec default + chart override, validate prod-mix at 50/200/500.
2. **Layer 1 metric prefix** — `snowplow_*` matches existing `snowplow_apiserver_fallthrough_total` convention. Confirm no Prometheus-side rename needed.
3. **Should Layer 1 ALSO publish per-handler-kind breakdown** (restactions / widgets / apistage) the way `snowplow_apiserver_fallthrough_cells` does? Useful for diagnosis but adds map cardinality. Suggest: NO at first; revisit after one synth-load run.
4. **Ship sequencing vs Ship G** — Layer 1 can ship in parallel with Ship G. Layer 2 should ship before or alongside Ship G so we can attribute the fanout reduction. Confirm priority.

---

## Appendix A — TRACED vs INFERRED Inventory

**TRACED** (file:line verified):
- All Layer 0 mechanisms (sections 1.1-1.11)
- Workqueue `Len()` availability (`/Users/diegobraga/go/pkg/mod/k8s.io/client-go@v0.29.0/util/workqueue/queue.go:187`)
- `BucketRateLimiter` + `MaxOfRateLimiter` + `DefaultControllerRateLimiter` prior art (`/Users/diegobraga/go/pkg/mod/k8s.io/client-go@v0.29.0/util/workqueue/default_rate_limiters.go:38-211`)
- expvar mount point + pattern (`internal/cache/fallthrough_meter_expvar.go:19-43`, `main.go:611-631`)
- Refresher counter exposure shape (`internal/cache/refresher.go:357-372`)
- L1 store stats shape (`internal/cache/resolved.go:584-602`)
- Dep tracker stats shape (`internal/cache/deps.go:899-926`)
- Skipped-no-entry semantics ruling out L1 capacity explosion (`internal/cache/refresher.go:310-318`)

**INFERRED** (reasoned, calibration-gap acknowledged):
- Per-resolve cost (50-500 ms) — needs per-handler resolve-latency histogram run
- Customer-scale dirty-mark fanout numbers (Scenario 2 table) — depends on per-customer user-count × widget-set, not measured
- Layer 2 qps default values — needs synth-load calibration
- Layer 3 thresholds — entirely calibration-gap until Layer 1 ships
- Ship G dep-graph behaviour — Ship G design docs not in this branch; behaviour reasoned from `feedback_l1_per_user_keyed_never_cohort.md` constraint
- /call request lifecycle behaviour under saturation (Scenario 6 stuck-spinner case) — needs end-to-end trace under load
- `workqueue.TypedBucketRateLimiter` / `TypedMaxOfRateLimiter` generic-wrapper availability — confirm against pinned client-go version in `go.mod`

**HARDCODED SPECIAL CASES**: none introduced. All proposals use additive CRD-or-env knobs per `feedback_no_special_cases.md`.

---

## Appendix B — Constraint Conformance Check

- `feedback_check_k8s_clientgo_prior_art.md` — Section 1.11 + Layer 2 explicitly cite client-go prior art for `BucketRateLimiter`, `MaxOfRateLimiter`, `DefaultControllerRateLimiter`, `workqueue.MetricsProvider`, `workqueue.Type.Len()`. No reinvented primitives.
- `feedback_data_driven_workflow.md` — every threshold marked CALIBRATION GAP or TRACED.
- `feedback_architect_design_rigor.md` — file:line citations on every code claim; TRACED vs INFERRED inventory in Appendix A.
- `feedback_no_special_cases.md` — no hardcoded user/path/controller; env-knob defaults only.
- `feedback_no_park_broken_behind_flag.md` — Layer 4 disabling subsystems is a fallback at saturation, not a defect-park; Layer 2 is unconditionally on at low load (cap above today's rate).
- `feedback_falsifier_first_before_ship.md` — Section 5 is the explicit pre-flight falsifier; load harness BEFORE shipping Layer 2.
- `feedback_empirical_root_cause_trace_before_fix.md` — Scenario 2 root-cause traced to refresher absent overall qps cap (`internal/cache/refresher.go:150-153`); Layer 2 fix directly addresses that line.

---

**END OF ANALYSIS** — Diego's gate.
