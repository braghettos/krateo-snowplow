# Path 3.2 — pre-commit diff summary (0.30.218 candidate)

**Author**: cache-developer
**Branch**: `ship-0.30.218-path3-2-prewarm` (off `ship-0.30.217-path3-1-bug-fixes`)
**Status**: AWAITING three-way ACK (architect + PM) per
`feedback_dev_review_with_architect_pm_before_commit`.

## Scope

Path 3.2 activates cluster_list collapse for ALL iterator-fan-out RAs
(Diego's mandate 2026-05-31) WITHOUT regressing the cyberjoker
narrow-RBAC fast path. Customer-priority invariant intact via
cell-warm fast-path + cold-miss per-NS fallback + PIP boot pre-warm.

**Absorbs** Bug 2 (RLock fast-path in `EnsureResourceType`) + Bug 3
(drop per-item TypeMeta assertion) from Path 3.1 — these are
unchanged on the branch (the changes were made by Path 3.1's
`f05df53` commit and we are layered on top of it).

## Files changed

| File | Production LOC | Test LOC |
|------|----------------|----------|
| `internal/cache/refresher.go` (modified) | ~80 | 0 |
| `internal/cache/cluster_list_metrics.go` (NEW) | 18 | 0 |
| `internal/cache/refresher_two_tier_test.go` (NEW) | 0 | 268 |
| `internal/resolvers/restactions/api/cluster_list.go` (modified) | ~150 | 0 |
| `internal/resolvers/restactions/api/cluster_list_prewarm.go` (NEW) | 122 | 0 |
| `internal/resolvers/restactions/api/cluster_list_warm_fallback_test.go` (NEW) | 0 | 119 |
| `internal/resolvers/restactions/api/cluster_list_dep_record_test.go` (updated) | 0 | ~30 |
| `internal/handlers/dispatchers/phase1_walk.go` (modified) | ~15 | 0 |
| `internal/handlers/dispatchers/phase1_clusterlist_prewarm.go` (NEW) | 98 | 0 |
| Test signature updates (3 dispatcher tests) | 0 | ~5 |
| **Totals (non-comment LOC)** | **~472** | **~387** |

Under the PM hard ceiling of 1,020 (= 2 × 510 honest estimate).

## Mechanism summary

### 1. Two-tier RateLimiting workqueue (refresher.go)

Refresher gets a second `workqueue.TypedRateLimitingInterface[string]`
field — `clusterListQueue` — with its own exponential-failure rate
limiter so per-key NumRequeues counts do not leak between tiers.

Worker `processNext` PROBES `clusterListQueue.Len() > 0` first; on
non-empty draws from the high-priority queue, else blocks on the
normal queue's `Get`. Customer-priority `yieldToCustomer` from Ship #98
preserved INTACT — called BEFORE handler invocation regardless of
which queue the key was drawn from (the two-tier discipline is INSIDE
`processNext`'s source-selection branch, ORTHOGONAL to the
customer-yield gate).

Per-key tier membership lives in `clusterListKeys sync.Map`. The
`SetRefreshHook` closure consults it when routing dirty-marks:
registered → cluster_list tier; unregistered → normal tier.

New public surfaces (cache package):
- `RegisterClusterListKey(l1Key)` — marks a key as cluster_list tier
- `IsClusterListKey(l1Key) bool` — predicate (used by tests + tier hook)
- `EnqueueClusterListRefresh(l1Key)` — schedule + register
- `ClusterListRefresherStats() (enqueued, completed uint64)` — observability
- `ClusterListCellCounters() (warm, coldFallback uint64)` — observability
- `RecordClusterListCellWarm()` / `RecordClusterListCellColdFallback()` — bumpers

### 2. Cluster_list cell-warm fast-path + cold-miss async populate (cluster_list.go)

After gates 1-5 pass and the cluster-scope call is built,
`attemptClusterListCollapse` now performs a `sync.Map.Load` on the
apistage cell's contentKey:

- **WARM HIT**: return `(call, true, 0)` immediately — no decode on
  the customer goroutine. Bump `clusterListCellWarmTotal`. Emit
  `cluster_list.cell.warm` log marker.

- **COLD MISS**: schedule async populate via
  `populateClusterListCellAsync` (bounded GOMAXPROCS-wide semaphore
  + per-cell inflight dedup) and return `(_, false, 8)`. Bump
  `clusterListCellColdFallbackTotal`. Emit
  `cluster_list.cell.cold_fallback`. Caller (resolve.go) keeps
  `useClusterList=false` → per-NS iterator path executes for THIS
  request. NO decode on customer goroutine.

The synchronous populate body (dispatch → shape-check →
materialise → Put → RecordList) is EXTRACTED into a public
helper `populateClusterListCellSync` called by THREE call sites:
PIP boot pre-warm, the async populate goroutine, and the
existing refresher handler path (via dep-tracker dirty-mark
plumbing for already-populated cells).

`populateClusterListCellSync` also calls
`cache.RegisterClusterListKey(contentKey)` so future dep-tracker
dirty-marks of the same key route to the high-priority tier.

The async populate uses a detached `context.Background()`-anchored
context with the SA endpoint + REST config + apistage-content-resolve
markers carried over from the customer's `customerCtx`. A 30s
populate timeout caps the goroutine; on timeout the cell stays cold
and the next customer retries the same cold-miss path.

### 3. PIP boot pre-warm (phase1_clusterlist_prewarm.go + cluster_list_prewarm.go)

New Step 7.5 invoked from `phase1WarmupWith` AFTER the existing F2
content prewarm and BEFORE `MarkPhase1Done`:

1. Drain the harvested `contentPrewarmHarvester.refs` set
2. Fetch each RA's CR under SA identity (objects.Get)
3. `api.EnumerateClusterListCells(saCtx, log, restActions)` walks
   every stage with non-empty `DependsOn.Iterator` + ns-scope
   target GVR; dedupes by GVR.
4. `api.PrewarmClusterListCells(...)` populates the roster via
   `populateClusterListCellSync` in a `runtime.GOMAXPROCS(0)`-bounded
   errgroup with a 60s deadline.

On deadline exceeded, MarkPhase1Done fires regardless — the
cold-fallback async populate path covers any unwarmed cell at the
first customer touch.

`api.ClusterListPrewarmTimeout = 60 * time.Second` (the hard cap).

### 4. Bug 2 + Bug 3 absorbed (unchanged)

Verified by running the Path 3.1 falsifiers:
- `TestEnsureResourceType_ConcurrentHitPathScales` (Bug 2 RLock fast-path) PASS
- `TestValidateClusterListShape_AcceptsInformerWireShape_NoPerItemTypeMeta` (Bug 3) PASS
- `TestValidateClusterListShape_HappyPath` + `_ParseListEnvelopeEquivalence` (Bug 1) PASS

## 13-AC self-verification grid

| # | AC | Mechanism / falsifier | Status |
|---|----|----------------------|--------|
| AC-P3.2.1 | Per-/call decode wall-clock attribution ≤ 500ms HARD pass | `TestClusterListCollapse_CustomerPathNonBlocking` asserts customer cold-miss returns in <100ms; warm hit is sync.Map.Load (ns-scale). **Empirical Phase 5 §5.1 measurement required for FINAL AC.** | MECHANISM-VERIFIED (PASS) |
| AC-P3.2.2 | Mix-weighted piechart warm ≤ 1,500 ms (INFERRED-may-plateau) | Customer-visible only — measured at Phase 6 canonical Chrome MCP. | PHASE 6 DEFERRED |
| AC-P3.2.3 | Admin /compositions warm ≤ 4,000 ms | Customer-visible — Phase 6. Design §11 projects ~3,500 ms. | PHASE 6 DEFERRED |
| AC-P3.2.4 | Cj /compositions warm ≤ 1,800 ms (preserve baseline 1,610 ms) | Customer-visible — Phase 6. | PHASE 6 DEFERRED |
| AC-P3.2.5 | Cluster_list cell refresh median ≤ 30s | Mechanism: two-tier queue drains high-priority first. Falsifier §5.3 deferred to Phase 5; unit-form `TestRefresher_ClusterListPriorityDrain` PASS. | MECHANISM-VERIFIED (PASS) |
| AC-P3.2.6 | Cell-warm path: `useClusterList=true` + `gate=0`, no per-NS fallback | `TestClusterListCollapsePut_RecordsDepEdges` Phase 2 + `TestClusterListCollapse_ColdMissReturnsGate8` Phase 3 PASS. | UNIT-VERIFIED (PASS) |
| AC-P3.2.7 | PIP Step 7.5 completes within 60 s | `ClusterListPrewarmTimeout = 60 * time.Second` hard-coded; bounded errgroup with parallelism = `runtime.GOMAXPROCS(0)`. Phase 4 deploy will verify wall-clock ≤ 60 s in pod log markers. | MECHANISM (TIMEOUT HARD-CAPPED) |
| AC-P3.2.8 | Pod restartCount = 0 across 30-min burst | Phase 4 / Phase 5 burst probe — deferred. | PHASE 5 DEFERRED |
| AC-P3.2.9 | RBAC 3-cohort sweep PASS | Apistage L1 identity-free contract unchanged. Existing RBAC tests pass. Phase 5 cohort sweep runs admin + cj + Group-only. | PHASE 5 DEFERRED |
| AC-P3.2.10 | No `cluster_list.cell.cold_fallback` after 60 s post-boot | Counter observability via `ClusterListCellCounters()` — Phase 5 grep on pod logs. | PHASE 5 DEFERRED |
| AC-P3.2.11 | NO cj regression (cj /compositions warm baseline 1,610 ms preserved) | Cell-warm fast-path is a sync.Map.Load (zero new cost). Cold-fallback path returns gate 8 — customer takes the SAME per-NS iterator path as pre-Path-3.2. Phase 6 measurement confirms. | MECHANISM-VERIFIED |
| AC-P3.2.12 | LCP within 100 ms of piechart_visible_correct_ms | Customer-visible — Phase 6. | PHASE 6 DEFERRED |
| AC-P3.2.13 | 5-min sustained burst — per-user p99 ≤ 60 s + cluster_list p99 ≤ 60 s | Unit-form falsifier `TestRefresher_SustainedBurstNoStarvation` PASS (normal-tier p99 ~37 ms under unit-scale burst; production AC at 60s for both tiers). Phase 5 production burst probe required for FINAL AC. | UNIT-VERIFIED (PASS) |
| BUG-2 | `EnsureResourceType` RLock fast-path | `TestEnsureResourceType_ConcurrentHitPathScales` PASS under -race -count=1 | PRESERVED (PASS) |
| BUG-3 | Drop per-item TypeMeta assertion | `TestValidateClusterListShape_AcceptsInformerWireShape_NoPerItemTypeMeta` PASS | PRESERVED (PASS) |

## Test results

```
go test -race -count=1 -timeout 300s ./internal/cache/... ./internal/resolvers/restactions/api/... ./internal/handlers/dispatchers/...
ok   github.com/krateoplatformops/snowplow/internal/cache                    24.545s
ok   github.com/krateoplatformops/snowplow/internal/resolvers/restactions/api 23.259s
ok   github.com/krateoplatformops/snowplow/internal/handlers/dispatchers      8.475s

go build ./...   →  PASS (full module compiles)
```

Pre-existing unrelated failures (per Path 3.1 docs):
`TestExtractOpenAPISchemaFromCRD`, `TestMapVerbs` — pre-existing,
not Path 3.2.

## Phase 0 baselines (snowplow 0.30.215 reference state)

| Metric | Baseline | Target |
|--------|---------|--------|
| Cj /compositions warm `lastCallEnd_ms` | 1,610 ms | ≤ 1,800 ms (preserve) |
| Cj /compositions cold | 2,252 ms | ≤ 2,300 ms (preserve) |
| Admin /compositions warm | 13,055 ms | ≤ 4,000 ms |
| Mix-weighted piechart warm | 1,723 ms | ≤ 1,500 ms |

See `docs/path3-2-phase0-baselines-2026-05-31.md` for full reference.

## Awaiting

- ACK from cache-architect (design soundness — two-tier queue
  implementation, async populate dedup correctness, ctx detach
  pattern).
- ACK from PM (acceptance + falsifier — empirical decode-attribution
  ≤ 500 ms gate at Phase 5).

On both ACKs: proceed to Phase 4 (commit + tag `0.30.218`, lockstep
chart push to braghettos EXPLICITLY, helm upgrade no-reuse-values).
