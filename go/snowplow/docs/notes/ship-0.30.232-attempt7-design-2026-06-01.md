# Ship 0.30.232 — Attempt #7 nil-rc whack-a-mole — phase1Walker boot literal + type-safety enforcement

date: 2026-06-01
architect: cache-architect
status: design (pre-PM-gate)
trajectory: 6 consecutive HARD REVERTs on this surface (0.30.226 → 0.30.231). 7th attempt.

No perf gain. This is correctness fix only; no projected latency or counter delta.

---

## Phase 1 — empirical audit

Cross-checked dispatch's pre-staged list. Enumeration commands run from repo root.

### phase1Walker struct literals (TRACED)

```
grep -rn '&phase1Walker{' --include='*.go' .
```

Yields 4 sites (1 more than dispatch listed, the 4th is test-only):

| # | site | sets rc? | status |
|---|---|---|---|
| 1 | internal/handlers/dispatchers/phase1_walk.go:852 (resolveNavigationRoot) | YES `rc: saRC` line 854 | fixed in 0.30.230 |
| 2 | internal/handlers/dispatchers/phase1_walk_pagination_jobs.go:307 (drainApiRefPaginationJobs) | YES `rc: saRC` line 313 | fixed in 0.30.230 |
| 3 | internal/handlers/dispatchers/prewarm_engine_boot.go:124 (rePrewarmBoot) | **NO** | **TODAY'S CRASH** |
| 4 | internal/handlers/dispatchers/phase1_walk_bounds_test.go:43 (TestPhase1Walk_MaxDepthTruncation) | NO | test-only, never reaches `widgets.Resolve` per the test's depth-cap gate (line 53-54), no rc consumer fires |

Dispatch listed 3 production sites. My grep confirms 3 production + 1 test. The test site (#4) is safe by construction (depth-cap returns before widgets.Resolve at phase1_walk.go:1059 fires).

### phase1Walker.rc consumers (TRACED)

```
grep -n 'w\.rc' --include='*.go' -r internal/
```

Yields 2 sites — both pass `w.rc` into `widgets.ResolveOptions.RC`:

| consumer | file:line | downstream chain |
|---|---|---|
| phase1Walker.walk | phase1_walk.go:1061 | widgets.Resolve → crdschema.ValidateObjectStatus → cache.GVRFor → discoverPluralInfo → "plurals discovery: nil *rest.Config" |
| iterateApiRefPages page-N | phase1_walk_pagination.go:330 | same chain |

Both consume w.rc identically. The boot site #3's walker invokes phase1_walk.go:1059 (via w.walk at prewarm_engine_boot.go:133) AND can reach iterateApiRefPages via the apiRef pagination flow. Both consumer chains land on the same nil-rc panic.

### Other *rest.Config-bearing struct types (peer-consumer audit per feedback_audit_peer_consumer_detection)

```
grep -n 'rc\s*\*rest\.Config' --include='*.go' -r internal/
```

Production struct fields:

| struct | file:line | construction sites | risk |
|---|---|---|---|
| phase1Walker.rc | phase1_walk.go:917 | 3 production + 1 test | site #3 nil |
| endpoint resolver struct rc | resolvers/restactions/api/endpoints.go:20 | constructor at resolve.go:202 (`rc: opts.RC`) | non-nil at construction; nil iff opts.RC nil — but caller in handler sets r.saRC at restactions.go:199 (non-nil from main) |
| restActionHandler.saRC | handlers/dispatchers/restactions.go:55 | main.go single construction site | non-nil from main per project_current_state |
| rePrewarmDeps.saRC | prewarm_engine_boot.go:66 | phase1_walk.go:342 (`saRC: rc`) | non-nil per the Phase1Warmup contract |

The site that's bug-bearing is #3 (rePrewarmBoot's literal). All `rePrewarmDeps.saRC` IS populated at construction (phase1_walk.go:342); the bug is `rePrewarmBoot` passes `deps.authnNS`, `deps.harvester`, `deps.navHarv` to the walker literal but OMITS `rc: deps.saRC`.

### Functions taking *rest.Config (TRACED — checked nil propagation)

All production functions taking rc validate before use:
- `discoverPluralInfo` (plurals_resolver.go:299) → explicit `if rc == nil` guard at line 302
- `discoverKindForGVR` (plurals_resolver.go:341) → explicit `if rc == nil` guard at line 344
- `discoveryClientBuilder` (discovery_lookup.go:150) → explicit `if rc == nil` guard at line 152
- `dynamic.NewClient` (dynamic/client.go:18) → calls `dynamic.NewForConfig(rc)` which panics on nil

`dynamic.NewForConfig(nil)` is the actual boot crash signature. The error from `discoverPluralInfo` is a wrapped error (returned, not panic). But the surfaced symptom downstream is the "plurals discovery: nil *rest.Config" wrapped error — that's HTTP 500 on /call, not a crashloop unless the boot path treats it as fatal.

**Verification of "boot crashloop" claim**: rePrewarmBoot returns the error from `w.walk` (line 133). The error is logged (`continue`) and not propagated upward (line 139 `continue`). So a nil-rc walker DOES NOT crash boot — it produces 500-style log errors per nav root then continues. The pod IS Ready. The customer-visible effect is: re-walk silently fails, harvest set empties, seed has no work, and the FIRST customer /call cohort falls back to per-user resolve (cold path).

**This re-classifies the bug severity**: not a crashloop. It's a silent-correctness regression that defeats the engine's re-walk purpose (the boot-race fix). The pod-boot-readiness gate would NOT catch this — pod stays Ready. Falsifier must check the actual symptom: cohort prewarm SUCCESS, not pod Ready.

---

## Phase 2 — chosen option

**Option C: type-system enforcement via constructor function.**

Rationale:
- Option A fixes only site #3 but the SAME blind spot pattern produced the bug 3 times today. There is no architectural barrier that prevents the next phase1Walker construction site from omitting rc.
- Option B (in-helper fallback at `dynamic.NewClient` / `discoverPluralInfo`) restores the safety net but introduces silent-credential-fallback risk: a future customer-credentialed call site forgetting rc would silently use SA creds (violates RBAC isolation). For per-user paths this is data-exfil risk.
- Option C: introduce `newPhase1Walker(rc *rest.Config, authnNS string, opts ...phase1WalkerOpt) *phase1Walker` constructor. Make `rc` a required positional parameter. Replace all 4 struct literals with constructor calls. Compiler enforces; no missing-field surface possible.

Trade-off: ~50 LOC across 4 sites + 1 helper file. No runtime cost. The functional options pattern (`phase1WalkerOpt`) lets each site set only the harvesters it needs (the boot site sets shared harvesters; the drain site sets no harvesters; the production walker site sets all three; the test site sets none).

---

## Phase 3 — honest reflection: is the cleanup goal right?

The 0.30.227 production binary had `cache/dynamic.go KindFor/ResourceFor` and `crds/get.go` with built-in `rest.InClusterConfig()` fallback. Ship 2 / 0.30.226 deleted them for "architectural cleanliness." That deletion removed the safety net every subsequent revert has been chasing.

Honest verdict: **the cleanup goal is correct, the implementation cadence is wrong.**

`rest.InClusterConfig()` as in-helper fallback is NOT k8s/client-go prior art for resolver code — client-go expects callers to construct and pass `*rest.Config` deliberately. It's prior art for one-shot CLI tools, not for a long-lived server with per-user credential isolation. The 0.30.227 helper-fallback was a band-aid for missing thread-through, not the right pattern.

The right pattern (per [feedback_check_k8s_clientgo_prior_art]) is: every code path constructs its rc explicitly at the boundary (handler / phase entry), threads it through, and is forbidden from silently falling back to in-cluster creds at depth. Option C enforces that pattern at the type level for phase1Walker — extending the discipline rather than reverting it.

So: keep the cleanup. Add type-safety. Don't restore the silent-fallback.

---

## Phase 4 — file-level diff spec

### New file: `internal/handlers/dispatchers/phase1_walker_new.go`

```go
package dispatchers

import "k8s.io/client-go/rest"

// phase1WalkerOpt configures optional phase1Walker fields. Required fields
// (rc, authnNS) are positional in newPhase1Walker; optional ones (harvesters,
// pagCollector) are set via opts.
type phase1WalkerOpt func(*phase1Walker)

func withApiRefHarvester(h *contentPrewarmHarvester) phase1WalkerOpt {
	return func(w *phase1Walker) { w.apiRefHarvester = h }
}

func withNavWidgetHarvester(h *navWidgetHarvester) phase1WalkerOpt {
	return func(w *phase1Walker) { w.navWidgetHarvester = h }
}

func withPagCollector(c *apiRefPaginationCollector) phase1WalkerOpt {
	return func(w *phase1Walker) { w.pagCollector = c }
}

// newPhase1Walker is the SOLE constructor for phase1Walker. rc and authnNS
// are required positional parameters — the compiler refuses a missing
// argument, eliminating the recurring nil-rc surface (0.30.228 through
// 0.30.231 reverts all traced to a phase1Walker struct literal omitting rc).
// Tests construct via newPhase1Walker too; a depth-cap test that never
// reaches widgets.Resolve passes nil rc explicitly (acceptable: the depth
// gate fires before any rc consumer runs).
func newPhase1Walker(rc *rest.Config, authnNS string, opts ...phase1WalkerOpt) *phase1Walker {
	w := &phase1Walker{
		authnNS: authnNS,
		rc:      rc,
		visited: map[string]struct{}{},
	}
	for _, o := range opts {
		o(w)
	}
	return w
}
```

### Edit: `internal/handlers/dispatchers/phase1_walk.go` (line 852)

Replace literal at 852-859 with `newPhase1Walker(saRC, authnNS, withApiRefHarvester(harvester), withNavWidgetHarvester(navHarvester), withPagCollector(pagCollector))`.

### Edit: `internal/handlers/dispatchers/phase1_walk_pagination_jobs.go` (line 307)

Replace literal at 307-315 with `newPhase1Walker(saRC, j.AuthnNS)` (no harvesters per the existing comment at line 297-306 explaining why).

### Edit: `internal/handlers/dispatchers/prewarm_engine_boot.go` (line 124) — THE BUG SITE

Replace literal at 124-129 with `newPhase1Walker(deps.saRC, deps.authnNS, withApiRefHarvester(deps.harvester), withNavWidgetHarvester(deps.navHarv))`.

### Edit: `internal/handlers/dispatchers/phase1_walk_bounds_test.go` (line 43)

Replace literal at 43-46 with `newPhase1Walker(nil, "krateo-system")`. Test asserts depth-cap fires before any rc consumer (comments at lines 53-54 confirm).

---

## Risk register

| risk | likelihood | mitigation |
|---|---|---|
| 4th-and-unknown nil-rc surface exists outside phase1Walker | LOW | Phase 1 audit grep-checked every `rc *rest.Config` struct field and consumer; no other unfixed surface found |
| Constructor refactor introduces NEW bug | MEDIUM | unit tests run + Phase1Warmup integration test + pod-boot-Ready gate + first-/call success gate |
| Pod-boot-readiness gate gives false-green | HIGH | Per Phase 1 finding: nil-rc DOES NOT crash boot; it silently breaks re-walk. Falsifier MUST check `prewarm.engine.boot.rewalk_complete` log shows non-zero `rewalked` AND first cohort /call hits L1 hot |
| 7th revert | MEDIUM | Honest acknowledgment: 6 prior architectures had 3-way ACK and shipped regressions. Same agent. Per feedback_recurring_regression_pattern Change A, this would warrant a fresh agent — that change is not applied here. Probability of revert remains nontrivial. |
| Refactor scope creep beyond 4 sites | LOW | grep is exhaustive; sites are enumerated; touch only the 4 sites |
| Test #4 changes break unit test | LOW | newPhase1Walker(nil, "krateo-system") preserves the existing field values; the depth-cap test path doesn't call widgets.Resolve |

---

## Falsifier (HARD gate, per feedback_falsifier_first_before_ship)

1. **Compile gate**: `go build ./...` MUST succeed (no missed construction sites; compiler enforces required rc).
2. **Unit gate**: `go test ./internal/handlers/dispatchers/...` MUST pass (excluding the pre-existing TestExtractOpenAPISchemaFromCRD per scope rules).
3. **Pod-boot-readiness gate** (per feedback_gke_pod_boot_readiness_pre_ship_gate): deploy to GKE bench; pod MUST reach Ready within 300s.
4. **Re-walk completion gate** (NEW — per Phase 1 finding that boot crashloop claim was wrong):
   - Tail pod logs for 60s after Ready.
   - MUST observe `prewarm.engine.boot.rewalk_complete` with `rewalked` field == count of nav roots (typically 2) AND non-zero `restactions` AND non-zero `widgets`.
   - If `rewalked: 0` OR no rewalk_complete log within 60s, HARD REVERT.
5. **No "plurals discovery: nil *rest.Config" log line** in 60s after Ready. (This is the direct symptom of the bug.)
6. **First-/call cohort-hit gate**: after Ready+60s, curl one cyberjoker `/call` request. Response MUST be 200 AND served from L1 (X-Cache-Hit header or equivalent), NOT cold-path resolve.

If ANY of gates 3-6 fail, HARD REVERT to helm rev 404 (= 0.30.227 production).

---

## HARD REVERT path

`helm rollback snowplow 404 -n krateo-system` (= 0.30.227 production binary, confirmed in current state per project_current_state.md). Lockstep chart tag per feedback_chart_release_lockstep.

---

cache-architect signed.
