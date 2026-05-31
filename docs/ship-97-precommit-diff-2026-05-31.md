# Ship #97 — Pre-commit diff + acceptance self-verification

Signed: cache-developer. Date: 2026-05-31. Status: AWAITING ARCHITECT + PM ACK.

## Phase 1 verdict — PROCEED

See `docs/ship-97-prefix-falsifier-2026-05-31.md`.

- `parseListEnvelope` cum CPU% on dashboard hot-path: **45.35%** (inside PROCEED band 12.5–50%).
- 100% of `parseListEnvelope` CPU comes from `gateListEnvelope` (the FALLBACK path). The R3 fast path consumed 2.10s of 328.78s (0.6%). The architect's diagnosis is empirically correct.
- pre-fix piechart_correct mix-weighted warm: 2,103ms (inside band 1,000–4,200ms).

## Diff stats

| File | +LOC | -LOC | Notes |
|---|---|---|---|
| `internal/handlers/dispatchers/resolve_populate.go` | +23 | -2 | Apistage-class branch around Put |
| `internal/resolvers/restactions/api/apistage.go` | +48 | 0 | New exported helper `ParseListEnvelopeForRefresh` |
| **Production total** | **+71** | **-2** | ~25 LOC actual logic; rest is doc comment |
| `internal/resolvers/restactions/api/apistage_parse_for_refresh_test.go` | +148 (new) | — | 3 unit tests (byte-equivalence, GET-by-name guard, malformed guard) |
| `internal/handlers/dispatchers/ship97_refresher_populates_items_test.go` | +336 (new) | — | 4 integration falsifiers (F1 populate-Items, F2 GET-by-name leaves nil, F3 non-apistage unaffected, F4 -race 4 readers) |
| **Test total** | **+484** | — | |
| **Grand total** | **+555** | **-2** | |

## LOC gate (PM condition #6) — SURFACED HERE

Architect design §8 budget: +125 total with self-pause at +250.
Actual: **+555**.

**Decomposition**:
- Production logic: ~25 LOC (matches design exactly).
- Production doc comments: ~46 LOC (Ship #97 mechanism description + risk-rationale at Put site + helper doc comment with confidence/identity/customer-priority labels).
- Test logic: ~378 LOC across 4 falsifier test cases (F1–F4) + 3 unit tests.
- Test doc comments: ~121 LOC (file-header risk-mapping per dispatcher test convention + per-test "what fails / what is asserted" headers).

**Why overshoot:**
1. Per-test "PRE-FIX FAILS / POST-FIX PASSES" doc-comment convention adopted by every other falsifier file in `internal/handlers/dispatchers/`. The 484-LOC test file matches the convention of `refresher_noop_falsifier_test.go` (108 LOC for 1 falsifier with similar headers). My file has 4 falsifiers ≈ 4 × 108 = 432 LOC just by precedent.
2. Discharging PM condition #5 (4+ concurrent readers under -race) requires a non-trivial reader-pool harness (~70 LOC).
3. The byte-equivalence-with-MISS-branch invariant (R-Items-shape-mismatch risk in design §7) needs a non-trivial structural compare (~30 LOC).

**Mitigation considered:**
- Tightening test doc to match minimum convention: would save ~80 LOC, still ≈ +475.
- Dropping F3 (non-apistage classes unaffected): saves ~50 LOC. But F3 directly falsifies the class-gating in the Put site change — without it, a future refactor could silently widen the branch and cause cross-class regressions. **NOT RECOMMENDED.**
- Dropping F4 (race test): saves ~80 LOC. Violates PM condition #5. **NOT ALLOWED.**

**Recommendation:** PAUSE here. Ack overshoot. The overshoot is mostly test rigor (4 falsifiers + 3 unit + 1 race) and matches the convention in the package. Awaiting architect + PM call: ship as-is or trim per priority.

## Acceptance criteria self-verification grid

| AC | Criterion | Test artifact | Self-status |
|---|---|---|---|
| AC-97.1 | piechart_visible_correct_ms mix-weighted warm ≤ 1,500ms | Post-deploy Chrome MCP | DEFERRED to Phase 6 |
| AC-97.2 | piechart_visible_correct_ms admin warm ≤ 1,500ms | Post-deploy Chrome MCP | DEFERRED to Phase 6 |
| AC-97.3 | admin /compositions warm lastCallEnd ≤ 7,000ms | Post-deploy Chrome MCP | DEFERRED to Phase 6 |
| AC-97.4 | `parseListEnvelope` cum CPU% on /dashboard burst < 10% | Post-deploy pprof | DEFERRED to Phase 6 |
| AC-97.5 | R3 fast-path fires on refresher-populated entries | TestShip97_F1 (PASS), TestShip97_F2 (PASS), apistage.go:510 `preparsed=true` slog at runtime | UNIT PASS; runtime DEFERRED to Phase 6 |
| AC-97.6 | Output content-equivalence (dispatch_delta) | TestParseListEnvelopeForRefresh_ItemsByteEquivalentToMissBranch (PASS); post-deploy dispatch_delta on 4 cells | UNIT PASS; runtime DEFERRED to Phase 6 |
| AC-97.7 | RBAC symmetry preserved (Group-kind cohort) | **Code review attestation below** (no test added — fix DOES NOT touch any RBAC predicate) | DISCHARGED BY ATTESTATION (see below) |
| AC-97.8 | Per-goroutine attribution — no customer-tax | Post-deploy `goroutine?debug=2`: parseListEnvelope stacks ONLY from refresher | DEFERRED to Phase 6 |
| AC-97.9 | Refresher cycle CPU does not regress > 10% | Post-deploy `process.cpu_seconds_total` rate vs `snowplow_refresher_completed_total` rate | DEFERRED to Phase 6 |
| AC-97.10 | `-race` test on concurrent Get over refresher-Put entry passes | TestShip97_F4 (4 readers, 200 refresher Puts, **PASS** with -race) | PASS |
| AC-97.11 | Pod restartCount = 0 over 30-min sustained burst | Post-deploy kubectl observation | DEFERRED to Phase 6 |
| AC-97.12 | LOC envelope ≤ +250 | Diff stats above | **EXCEEDED — see LOC gate section** |

## AC-97.7 — RBAC symmetry attestation

The Ship #97 production diff touches exactly two code locations:

1. `internal/resolvers/restactions/api/apistage.go` — new pure helper `ParseListEnvelopeForRefresh(inputs, raw) → (items, apiVersion, kind, ok)`. Branches on:
   - `inputs.Name != ""` → returns ok=false. **No RBAC kind branch.**
   - `parseListEnvelope` parse failure → returns ok=false. **No RBAC kind branch.**
2. `internal/handlers/dispatchers/resolve_populate.go` — apistage-class branch around existing Put. Branches on:
   - `inputs.CacheEntryClass == cache.CacheEntryClassApistage` (a cache class predicate, identical-shape to the existing branch at resolve_populate.go:302). **No RBAC kind branch.**

Neither diff site touches any RBAC subject predicate, RoleBinding/ClusterRoleBinding logic, User/Group/ServiceAccount kind switch, or `filterListByRBAC` invocation. The R3 fast-path read site at `apistage.go:587` (`gateListItemsWithMemo`) is unchanged — same RBAC gate as today.

Per `feedback_predicate_subject_kind_symmetry`, the ζ HARD REVERT lesson (0.30.183) was a predicate that explicitly enumerated `Subject.Kind == "User"` and forgot `"Group"`. The Ship #97 fix introduces no such enumeration. The RBAC symmetry invariant is upheld by code-shape; no Group-kind test is needed.

## Risk register discharge

| Risk (design §7) | Discharged by |
|---|---|
| R-decode-on-Put (refresher cycle CPU regression) | DEFERRED to Phase 6 AC-97.9 (post-deploy measurement). Phase 1 baseline shows refresher already at 30.98% cum CPU; design argues this is a strict win (parse moves from per-Get to per-Put, Puts << Gets). |
| R-Items-shape-mismatch | `TestParseListEnvelopeForRefresh_ItemsByteEquivalentToMissBranch` PASS — helper produces items with same metadata + same managedFields-stripping as MISS branch. |
| R-content-correctness | `TestParseListEnvelopeForRefresh_ItemsByteEquivalentToMissBranch` PASS (unit); post-deploy `dispatch_delta` AC-97.6 (runtime). |
| R-race | **TestShip97_F4 PASS with `go test -race`** — 4 readers concurrent with 200 refresh Puts, zero detector hit. |
| R-identity-propagation | Code review: `ParseListEnvelopeForRefresh(inputs, raw)` is a pure function with no context parameter. Refresher's `WithUserInfo` / `WithUserConfig` / `WithInternalRESTConfig` chain in `resolveAndPopulateL1` is untouched. The diff at `resolve_populate.go:255` writes the same `entry` struct fields the cache already exposes (Items + ItemsAPIVersion + ItemsKind, populated by the MISS branch at apistage.go:530-538 today). Identity propagation is unchanged. |
| R-special-cases | Code review: the branch is on `inputs.CacheEntryClass == cache.CacheEntryClassApistage` (cache-class constant, same predicate at resolve_populate.go:302) and `inputs.Name == ""` (LIST vs GET, same predicate at apistage.go:115 and :462). No GVR, path, user, or namespace literal. |
| R-empirical-baseline-gate | Discharged by Phase 1 falsifier (PROCEED band cleared). |

## Code-review attestation

- `go build ./...` — clean.
- `go test -count=1 ./internal/handlers/dispatchers/... ./internal/resolvers/restactions/api/...` — PASS.
- `go test -count=1 -race -run 'TestShip97|TestParseListEnvelopeForRefresh' ./internal/handlers/dispatchers/ ./internal/resolvers/restactions/api/` — PASS.
- Pre-existing flake: `TestRecordL1Lookup_ConcurrentFirstObservation` fails with `-count=2` because it doesn't reset its counter between runs. **Verified flaky on 0.30.212 baseline (git stash) — NOT caused by Ship #97.** Not in scope.

## Request for ACK

- **Architect**: please confirm the diff matches design §2 (preferred surface — exported helper + class-gated Put-site branch) and the LOC overshoot is acceptable given the rigour-vs-budget trade-off.
- **PM**: please confirm the 10 conditions are discharged (or excused for the post-deploy ones marked DEFERRED). Specifically: condition #1 PROCEED band (PASS), condition #5 race test (PASS), condition #7 three-way review (this doc), condition #6 LOC pause (acknowledged overshoot — awaiting waiver or trim instruction).

— cache-developer.
