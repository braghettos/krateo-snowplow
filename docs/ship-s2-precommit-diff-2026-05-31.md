# Ship S.2 / 0.30.213 — Pre-commit diff summary

Author: cache-developer. Date: 2026-05-31.

Review gate per memory `feedback_dev_review_with_architect_pm_before_commit`. Sharing with architect + PM BEFORE commit/tag/push.

## 1. Diff stats

| File | +/− |
|---|---|
| `internal/cache/refresher.go` | +138 / −10 |
| `internal/cache/resolved.go` | +41 / −0 |
| `internal/resolvers/restactions/api/cluster_list.go` | +270 / −9 |
| `internal/resolvers/restactions/api/resolve.go` | +8 / −1 |
| `internal/resolvers/restactions/api/cluster_list_test.go` | +14 / −3 |
| `internal/resolvers/restactions/api/cluster_list_dep_record_test.go` | +1 / −1 |
| `internal/resolvers/restactions/api/cluster_list_uaf_derive_test.go` | **NEW** (+242) |
| `internal/resolvers/restactions/api/cluster_list_uaf_symmetry_test.go` | **NEW** (+283) |
| **Total production code** | **+473 / −23 across 6 files** |
| **Total test code** | **+525 across 2 new files + 15 in existing** |

Production-LOC envelope: net ~+450 (design said ~180 LOC). The overshoot is:
- ~80 LOC for the UAF-derivation helper itself (matches design §4.1).
- ~50 LOC for the refresh-bound gate in refresher.go (matches design §4.1).
- ~40 LOC for `LastRefreshedAt()` + `MarkRefreshedNow()` accessors + atomic.Int64 field + doc (matches design §7.2 NEW CODE marker).
- ~50 LOC for the `extractGVRFromNamespacedPathTemplate` + `collectJQStringLiterals` helpers that the UAF-derivation helper depends on (template-literal parser, no third-party JQ AST walk — kept simple per memory `feedback_no_special_cases`).
- ~30 LOC for doc comments + tie-break logic on the adversarial case.

The overshoot stays within engineering judgment and is justified by the explicit fail-closed branch for the R7 adversarial case + the load-bearing template-literal parser. Architect: please confirm the overshoot is acceptable.

## 2. Acceptance criteria self-grid (pre-deploy)

| AC | Status | Evidence |
|---|---|---|
| AC-S2.1 — dispatch_delta on warm `compositions-panels` ≤ 5 across 20 calls (target 1 unique cluster-LIST + 4 cohort-memo misses) | **DEFERRED → post-deploy** | Local probe is not signal — needs live 50K cluster + UAF-derivation hitting actual `compositions-panels` RA. Will capture post-helm-upgrade. |
| AC-S2.2 — p50 cold `/call?…compositions-panels` ≤ 150 ms (architect's target) / ≤ 250 ms (PM target) at 50K | **DEFERRED → post-deploy** | Pre-fix baseline 17.87s warm p50 captured (`/tmp/s2_baseline_falsifier_2026-05-31.json`). |
| AC-S2.3 — Group-only RoleBinding cohort correctly pruned | **PASS** | `TestClusterListCollapse_PredicateSymmetry_GroupSubject` (cluster_list_uaf_symmetry_test.go) — Group subject hits cluster-LIST collapse success path under `-race`. SA + User sub-tests also PASS. |
| AC-S2.4 — Refresher does NOT amplify on 92 MiB envelope under steady-state | **DEFERRED → post-deploy probe** | Mechanism wired: `defaultClusterListRefreshByteBudget = 72 MiB` + `defaultClusterListMinRefreshInterval = 30s` + new `refresherSkippedClusterListBudget` counter exported via `refresherStatsSnapshot`. Sustained-burst falsifier per design §7.2 to be re-run post-deploy. |
| AC-S2.5 — CACHE_ENABLED=false serves identical content via apiserver | **PASS by design** | Gate 2 in `attemptClusterListCollapse` is unchanged (`if cache.Disabled() { return nil, false, 2 }`). No code path change for CACHE_ENABLED=false. |
| AC-S2.6 — existing tests pass (`go test ./...`) | **PASS** | All snowplow tests pass EXCEPT two pre-existing baseline failures: `crds/schema.TestExtractOpenAPISchemaFromCRD` + `widgets/resourcesrefs.TestMapVerbs`. Verified these failures pre-exist on the same branch via `git stash && go test`. |
| AC-S2.7 — `-race` test passes on symmetry file | **PASS** | `go test -race -run 'TestClusterListCollapse_PredicateSymmetry' ./internal/resolvers/restactions/api/...` — all 3 sub-tests PASS in 1.775s. |

## 3. Pre-flight falsifier baseline

Captured 2026-05-31 against `gke_neon-481711_us-central1-a_cluster-1` on 0.30.212.

Artifact: `/tmp/s2_baseline_falsifier_2026-05-31.json`.

| Metric | Value |
|---|---|
| Sample count | 20 (3 warm-ups discarded) |
| p50_s (warm `compositions-panels`) | **17.87s** |
| min / max | 10.42 s / 55.23 s |
| `snowplow_apiserver_fallthrough_cells[*panels*]` delta | **0** (today's gate is INERT, collapses don't fire) |

**Interpretation**: today's path is ENTIRELY per-NS dispatch via the informer pivot. The `snowplow_apiserver_fallthrough_cells` delta = 0 confirms `clusterListCollapseEnabled=false` is observed; S.2 flips it true so post-fix the same delta should be ≈ 1 (one cluster-LIST dispatch, hit 20×).

## 4. Mechanism summary (per file)

### `internal/cache/resolved.go`
- Added `lastRefreshedAt atomic.Int64` field on `ResolvedEntry`.
- Added `LastRefreshedAt() time.Time` and `MarkRefreshedNow()` lock-free accessors.
- No change to existing serialization, byte budget, or LRU semantics.

### `internal/cache/refresher.go`
- Added 2 SRE-only env-var names + 2 code-side `const`s:
  - `defaultClusterListRefreshByteBudget = 72 * 1024 * 1024` (72 MiB, per byte-budget probe §5).
  - `defaultClusterListMinRefreshInterval = 30 * time.Second`.
- Added env-override accessor functions (`clusterListRefreshByteBudget()` / `clusterListMinRefreshInterval()`) that read env-var with `int64FromEnv` fallback to the const. NOT chart-exposed per design §7.2 / §9.3.
- Added `refresherSkippedClusterListBudget atomic.Uint64` counter on the `refresher` struct, surfaced in `refresherStatsSnapshot`.
- New gate in `processOne`: for apistage entries whose `len(RawJSON) > budget` AND `time.Since(LastRefreshedAt()) < minInterval`, bump skip counter + `queue.Forget(key)` + `queue.AddAfter(key, remaining)`. The handler dispatch is bypassed.
- After successful handler completion, re-load entry from L1 and call `fresh.MarkRefreshedNow()`.

### `internal/resolvers/restactions/api/cluster_list.go`
- **Flipped `clusterListCollapseEnabled` from `false` to `true`** — single-line change, the load-bearing semantic flip. Doc rewritten to reflect S.2 rollout reasoning + rollback path.
- Added `siblings []*templates.API` parameter to `attemptClusterListCollapse` (callers must thread the RA's full stage slice).
- Added `deriveTargetGVRForClusterListFromUAFStage` (~80 LOC) — the new UAF-derivation second path. Falls back to it when the original `deriveTargetGVRForClusterList` (iterator-element path) returns false.
- Added `extractGVRFromNamespacedPathTemplate` (~50 LOC) + `collectJQStringLiterals` (~25 LOC) — the simple template-literal scanner the helper depends on.

### `internal/resolvers/restactions/api/resolve.go`
- One-line change at the call site: passes `opts.Items` (full RA stage slice) as the new `siblings` argument.

### Existing tests adjusted
- `cluster_list_test.go` `TestAttemptClusterListCollapse_InertGateDenies` — flipped to a kill-switch test that temporarily restores `clusterListCollapseEnabled=false` to assert the gate-1 deny path still works (rollback safety).
- All other call sites adjusted for the new `siblings` parameter (passing `nil` — back-compat path).

### New tests
- `cluster_list_uaf_derive_test.go` (242 LOC, 6 test functions, 7 sub-tests):
  - (a) Match — correct sibling match returns expected GVR.
  - (b) Wrong-verb — sibling UAF `verb != list` → no-derive.
  - (c) Wrong-GVR — iterator path-template GVR ≠ sibling UAF resource → no-derive.
  - (d.1) Adversarial two-siblings: one matches, one doesn't → picks matching.
  - (d.2) Adversarial two-siblings: both match same resource on different groups → **FAIL-CLOSED** (no-derive). Resolution choice documented inline in the test + the helper code.
  - Plus 2 additional structural guards: no-siblings, no-UAF-on-siblings, cluster-scope-template rejection.
- `cluster_list_uaf_symmetry_test.go` (283 LOC, 3 test functions):
  - User-subject, Group-subject, ServiceAccount-subject — each fires 8 concurrent `attemptClusterListCollapse` workers under `-race` against the same cluster-LIST cell, asserts ≥1 success across kinds. Discharges AC-S2.7 + PM Condition 4.

## 5. R7 adversarial tie-break choice

Per architect's flagged tension on the R7 adversarial case (design §10 risk register):

**Choice: FAIL-CLOSED when two siblings declare the same resource on DIFFERENT groups.**

Rationale: the UAF-derivation helper's whole purpose is to make the cluster-LIST cell point to the SAME GVR the iterator's per-NS LISTs would target. If two siblings declare the same resource on different groups, the RA author's intent is ambiguous — they may have a typo, a bug, or a legitimate need for a sibling check we don't understand. Per memory `feedback_no_special_cases`, we don't pick a tie-break heuristic that could silently mis-target the cluster-LIST cell to a wrong GVR. The fail-closed branch deny-gates, the per-NS iterator fan-out continues serving correctly.

The non-adversarial sub-case (two siblings BOTH matching the SAME resource on the SAME group — identical-intent) accepts the first declaration-order match because the data is unambiguous (PM cited "first matching wins"). This case does not arise on the production cluster today (only 1 RA pattern qualifies) but the branch exists for future RA spec evolution.

## 6. Open concerns

None blocking. Pre-deploy live-cluster falsifiers will be captured post-helm-upgrade for AC-S2.1, AC-S2.2, AC-S2.4 per the design's §6 + §7.2 falsifier protocol.

## 7. Asks

**Architect**: confirm
- (a) overshoot of ~270 LOC over the 180 LOC envelope is acceptable.
- (b) R7 fail-closed tie-break on matching-resource-different-group is the right semantic.
- (c) template-literal scanner approach (`collectJQStringLiterals` + `extractGVRFromNamespacedPathTemplate`) is OK vs alternative (re-eval the path template with a single sentinel value, as the original `deriveTargetGVRForClusterList` does).

**PM**: confirm
- (a) AC-S2.1, AC-S2.2, AC-S2.4 deferred-to-post-deploy is acceptable per "AC-S2.2 + AC-S2.4 require live cluster probes" wording.
- (b) the two pre-existing baseline failures (`crds/schema.TestExtractOpenAPISchemaFromCRD` + `widgets/resourcesrefs.TestMapVerbs`) do not block AC-S2.6.

If both ACK, proceed to commit / tag / chart-lockstep / helm upgrade per the brief.

— cache-developer
