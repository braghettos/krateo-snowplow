# Live-refresh coherence — Phase-0 falsifier artifact + floor-ON amplification baseline

**Date:** 2026-06-18
**Plan:** `~/.claude/plans/snowplow-live-refresh-coherence-plan.md`
**Proposal:** `~/.claude/plans/snowplow-live-refresh-coherence-proposal.md`
**Design (ratified option B — floor-bypass bundled):** `docs/live-refresh-coherence-design-2026-06-18.md`
**Permanent test (the falsifier, lands before code per `feedback_falsifier_first_before_ship`):**
`internal/cache/refresher_live_refresh_coherence_test.go`

This file persists the Phase-0 proof that was previously narrated-only (the PM gate flagged it missing on disk). It attaches to the ledger row for the live-refresh-coherence ship. Test-only; no product `.go` was changed. All numbers are in-process (real dispatcher→L1→deps→refresher path, no apiserver), reproduced under `-race` across iterations.

---

## A. Stale-window falsifier — the premise REPRODUCES, ~3 orders larger than the proposal assumed

**Premise (proposal §2):** after a backing resource changes, `/call` serves a STALE widget result for a window — between the informer dirty-marking the dependent L1 key (`deps.go` `OnUpdate`→enqueue) and the refresher re-resolving + committing it (stale-while-revalidate). The proposal hedged "sub-ms–ms… usually fresh."

**Finding:** the window is **not** ms-scale. It is dominated by two on-by-default delay gates the proposal/plan never mention:
1. **#318-R1a per-key re-resolve rate-floor — default 2s** (`refresher.go:105 defaultRefresherRateFloorSeconds=2`; gate at `refresher.go:729-752`: a dirty-marked entry younger than the floor is `AddAfter(key, floor-elapsed)`-deferred, not re-resolved now). A live-refresh refetch hits a *young* entry (the first `/call` just populated it), so this fires on exactly the proposal's scenario.
2. **Ship #98 customer-priority yield — cap 5s** (`refresher.go:580 yieldToCustomer`, `refresherYieldMaxParked=5s` at `:124`). Under sustained `/call` traffic the refresher parks before the handler.

The code's own SLA docstring (`refresher.go:97-99`): **"yield 5s + resolve ≤3s + floor 2s = 10s."**

| Scenario | Stale window (reconcile → first fresh served) | Commit latency | Notes |
|---|---|---|---|
| **Production default** (floor=2s, no `/call` load) | **~1.96s** (1.955–1.963s across runs) | ~1.955s | Pure rate-floor. ~160× the proposal's "ms". |
| **Floor + sustained customer `/call`** (yield active) | **~5.01s** | ~5.006s | Hits the 5s yield cap; stacks on the floor. |
| floor=0 baseline (proposal's *assumed* world) | **~11ms** | ~6ms | Isolates the floor's contribution. |
| Pure refresher mechanism overhead (floor=0, 0 resolve cost) | **~11ms** | **40–400µs** | Dequeue→Put is genuinely sub-ms. |
| **Refetch-landing** (floor=2s, 26 refetches spread 0–2500ms post-reconcile) | — | — | **20/26 = 77% landed stale.** |

**Interpretation:**
- The raw refresher *mechanism* is sub-ms (40–400µs dequeue→commit), so the proposal's "sub-ms" intuition is right *about the mechanism* — but the **production window is owned by the floor/yield gates**, not the resolve.
- 77% of refetches in the first 2.5s read stale. The frontend fires its refetch *on the SSE event* (arriving ~at the front of the floor window), so real refetches are biased toward the worst part of the window — 77% is a lower bound. The frontend's ≤1-refetch/5s throttle means a stale-landing refetch isn't retried for up to 5s.
- This **strengthens** the feature's justification (the gap is deterministic seconds, not a marginal lost-race tail) and confirms the emit point: a coherent `/refreshes` signal must fire at the **post-commit** point, never the pre-resolve `SetRefreshHook` (`deps.go:407`, wired `refresher.go:400`) — emitting at dirty-mark would announce ~2s *before* L1 is fresh.

---

## B. Floor-ON amplification baseline — the denominator option B's floor-bypass must NOT regress

**Why this matters:** option B bundles a floor-bypass to close the stale window for *subscribed* keys. The floor exists (#318-R1a) to **collapse a CRUD-install churn wave into few re-resolves**. The bypass must shrink the stale-window WITHOUT turning the churn wave back into N re-resolves (the pre-#318 amplification the floor prevents). This baseline is what the architect's 9.7 comparison plugs into.

**Method:** fire `marks=200` rapid `Deps().OnUpdate` dirty-marks on ONE subscribed key over a 400ms spread (whole wave inside the 2s floor), 4 workers (production default parallelism), aged seed entry so the first mark re-resolves immediately and the rest land young→floored. Re-resolves-run is sourced from the **real `completedTotal`** counter; floored cycles from **real `flooredTotal`** (`refresher.go:260` — under the install storm "completed_total collapses while flooredTotal rises"); alloc from `runtime.MemStats.TotalAlloc` delta over the wave. Handler models a compacted ~2ms per-user widget re-resolve and writes a monotonic version (last-write-wins convergence confirmed).

| Config | marks | **re-resolves run** | collapse | floored cycles | enqueued | alloc (Δ TotalAlloc) | converge → v2 |
|---|---|---|---|---|---|---|---|
| **Floor ON (prod default 2s)** | 200 | **2** | **100:1** | 199 | 200 | ~350–490 KiB | **2.005s** |
| Floor OFF contrast (floor=0) | 200 | **200** | ~1:1 | 0 | 200 | ~350–490 KiB | ~0.45s |

**Interpretation:**
- **The floor turns 200 re-resolves into 2** — a **~100× reduction in refresher re-resolve work** for a single-key churn wave, at the cost of a ~2s convergence window. That is the exact CPU/freshness tension option B navigates.
- Re-resolves-run (**2**) is the load-bearing baseline figure — it is sourced from `completedTotal`, deterministic across `-race` iterations. `flooredCycles` (199) is ≥ the collapse delta (per the C1 accounting at `refresher.go:262-269`); do NOT equate it to the collapse delta.
- Alloc Δ is logged, not asserted: `TotalAlloc` deltas vary run-to-run (346–486 KiB) under concurrent GC. Directionally the floor-ON and floor-OFF allocs are comparable per-wave because both run the same enqueue path; the floor's win is in **re-resolve count** (CPU + downstream resolve alloc avoided), captured here by `resolves_run`. The production-scale alloc/CPU magnitude is a **50K-bench deploy-gate concern** (see below), not measurable at unit scale.

**The regression contract for option B:** for a *churn* (non-subscribed, or many-keys-changing) wave the floor-bypass must keep `resolves_run` at the collapsed level (≈ the floor-ON baseline), only shrinking the stale-window for a *subscribed* key whose freshness a client is actively waiting on. A naive always-bypass would land at the floor-OFF contrast (200 re-resolves) — the explicit anti-goal.

### 50K-bench production-scale confirmation (separate deploy gate)

This unit baseline proves the **collapse mechanism and ratio** in-process. It does **not** measure production-scale refresher CPU/alloc/cum-delay under a real composition-install storm (50K compositions × RBAC fan × per-user cells). That requires the Phase-6 50K bench under a CRUD-install churn wave with the candidate floor-bypass deployed, measuring refresher CPU/alloc and customer `/call` p50/p99 during the wave — the **deploy gate**, distinct from this pre-code falsifier. Flagged explicitly so the 9.7 comparison's unit-level numbers are not mistaken for the production sign-off.

---

## Reproduce

```
cd ~/krateo/snowplow-cache/snowplow
unset KUBECONFIG   # never ./internal/rbac/... (destructive TestMain)
go test ./internal/cache/ -run 'TestLiveRefreshCoherence' -count=1 -v
# under race (concurrent refetch readers vs the refresher):
go test ./internal/cache/ -run 'TestLiveRefreshCoherence' -count=2 -race -v
```

**Tests (all in `internal/cache/refresher_live_refresh_coherence_test.go`):**

- `TestLiveRefreshCoherence_StaleWindow_ProductionDefault` — headline: ~2s floored window; RED if floor stops applying to a young dirty-marked entry.
- `TestLiveRefreshCoherence_StaleWindow_FloorZeroBaseline` — isolates the floor (window collapses to ~ms).
- `TestLiveRefreshCoherence_StaleWindow_FloorPlusCustomerYield` — documented worst case (~5s, yield cap stacks on floor).
- `TestLiveRefreshCoherence_RefetchLandsInsideWindow` — 77% stale-landing fraction; concurrent readers, `-race`.
- `TestLiveRefreshCoherence_RefresherMechanismOverhead` — mechanism is sub-ms (commit ≤50ms guard).
- `TestLiveRefreshCoherence_FloorOnAmplificationBaseline` — **the 9.7 denominator**: 200 marks → 2 re-resolves (100:1).
- `TestLiveRefreshCoherence_FloorOffAmplificationContrast` — RED contrast: floor=0 → 200 re-resolves, what the bypass must avoid for churn.

**Real path exercised (no apiserver, no mechanism faked):**
`ResolvedCache().Put` (first `/call` commit) → `Deps().Record` (real dep edge) → `Deps().OnUpdate` (the real informer-event entry point `watcher.go`'s `AddEventHandler` calls) → real refresh hook (`refresher.go:400`) → real refresher queue + `processNext` (rate-floor + yield gates active) → registered `RefreshFunc` re-resolve → `ResolvedCache().Get` (the same read `/call` serves from). Reuses the shipped harness helpers `withCleanRefresher` / `resetRefresherForTest` / `RegisterRefreshFunc` / `StartRefresher` / `putAged`-pattern (`agedBackoff`) from `refresher_rate_floor_test.go`.

**Constraints honored:** test-only (no product `.go` change); `internal/cache/` only, KUBECONFIG unset (`feedback_no_go_test_against_remote_kubeconfig`); pure in-memory (`feedback_no_fake_production_scenarios` — the mechanism under test is real, only the resolve cost + backing-object content are modeled).
