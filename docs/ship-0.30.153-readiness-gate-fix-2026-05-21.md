# Ship 0.30.153 — Readiness-Gate Fix for `CACHE_ENABLED=false` + D.5 CRD Regen

**Author**: cache-architect (sub-agent)
**Date**: 2026-05-21
**Branch**: `ship-0.30.153-readiness-gate-fix`
**Bundle**: two small mechanical changes in one ship
**Priority**: P0 (incident fix — emergency `CACHE_ENABLED=false` fallback is BROKEN in production builds)

---

## §1 — Header

Ship 0.30.153 bundles two narrowly-scoped fixes:

1. **Readiness-gate fix** — `main.go:541` safety-net was missed when `CACHE_ENABLED=false` AND `PREWARM_ENABLED=true` AND the passthrough watcher was successfully constructed. The pod is stuck `{"status":"warming","phase1Done":false}` forever; the Service drops the pod from endpoints; the snowplow LB has 0 reachable backends. Tester surfaced this running D.5 HG-5 on 0.30.152 — pre-existing latent bug since 0.30.102 (Tag B introduced the readiness gate), exposed by HG-5, NOT a D.5 regression. Per `feedback_no_park_broken_behind_flag` — the cache=off path is a correctness defect and must be fixed.

2. **CRD regen for D.5's `clusterListWhenAllowed` field** — the Go type at `apis/templates/v1/core.go:106` was added in D.5 but `bash scripts/gen.sh` was not run. The committed `crds/templates.krateo.io_restactions.yaml` lacks the schema for `clusterListWhenAllowed`, so production apiserver rejects any RA that opts in (`unknown field`). D.5's cluster-list dispatch mechanism cannot fire in production until the CRD ships.

Both changes are mechanical and low-risk. Ship together to minimize tag/rollout overhead. Per `feedback_no_shortcuts_or_workarounds`: NO env-flag bypass, NO quick-patch — the fix lands in the actual readiness-gate code and the actual CRD pipeline.

---

## §2 — Readiness-Gate Root Cause + Selected Fix Shape

### §2.1 TRACED root cause (file:line, against committed `main.go`)

The startup sequence at `main.go:139` opens an if/else on `cache.Disabled()`:

- `main.go:139` — `if !cache.Disabled() { ... } else { ... }`
- `main.go:484-522` — `else` branch (the "true cache-off" diagnostic-passthrough mode).
- The else branch may construct a passthrough `ResourceWatcher` (lines 504, 510, 517-518 `cacheWatcher = w`) — `cache.NewResourceWatcher(cacheCtx, dynCli)` returns a `modePassthrough` watcher when called in cache-off, but the LOCAL variable `cacheWatcher` IS set to non-nil at line 517.
- **The else branch DOES NOT CALL `cache.MarkPhase1Done()`.** [TRACED: re-read of `main.go:484-522` shows zero `MarkPhase1Done` invocations in this branch.]

After the if/else closes (`main.go:523-528` is the watcher-stop defer), the safety-net runs:

- `main.go:541` — `if !cache.PrewarmEnabled() || cacheWatcher == nil { cache.MarkPhase1Done() }`

With the production helm values that triggered the incident:

- `CACHE_ENABLED=false` → `cache.Disabled() == true` → enter `main.go:484` else-branch
- `PREWARM_ENABLED=true` → `cache.PrewarmEnabled() == true` → `!cache.PrewarmEnabled() == false`
- `rest.InClusterConfig()` succeeded + `dynamic.NewForConfig` succeeded + `cache.NewResourceWatcher` succeeded → `cacheWatcher != nil`
- Result: BOTH disjuncts of `main.go:541`'s condition are false → `cache.MarkPhase1Done()` is NOT called.

Consequence chain (TRACED):

- `internal/cache/phase1.go:72` — `phase1Done atomic.Bool` defaults to `false`.
- `internal/handlers/readyz.go:49` — `done := cache.IsPhase1Done()` returns `false` → 503.
- The startupProbe / readinessProbe in the chart 503s indefinitely.
- Kubernetes Service controller removes the pod from `Endpoints`/`EndpointSlices`.
- The cluster-internal LB to `snowplow:8081` has 0 endpoints — every `/call` from the portal frontend dies at the LB layer.

Symptom captured: `{"status":"warming","phase1Done":false}` in `/health` is the OBSERVED output of `internal/handlers/healthz.go` (combined health+phase1 status) — confirming `phase1Done == false` at runtime, matching the trace above. [TRACED via tester's incident log.]

Tester's diagnosis is correct verbatim. Root cause is at the `main.go:541` safety-net condition; the diagnostic-passthrough branch should have been covered there from the start.

### §2.2 client-go prior art (per `feedback_check_k8s_clientgo_prior_art`)

There is no client-go primitive for this state — `IsPhase1Done` is a snowplow-internal startup signal (informer-set sync barrier + readiness flip). client-go's `WaitForCacheSync` is one of the building blocks Phase 1 already composes, but the higher-level "are we serving traffic yet" flag is intrinsically application-level. No prior art to reuse. [INFERRED.]

### §2.3 Selected fix shape

**Option (i) — extend the safety-net at `main.go:541`.** Chosen.

```go
// 0.30.153 — readiness-gate fix: also flip when the cache is disabled.
// The diagnostic-passthrough branch (main.go:484) constructs a
// modePassthrough watcher but never calls MarkPhase1Done — there are no
// informers to wait for, so /readyz must return 200 immediately.
if cache.Disabled() || !cache.PrewarmEnabled() || cacheWatcher == nil {
    cache.MarkPhase1Done()
}
```

Why option (i) over option (ii) (calling `MarkPhase1Done()` inside the diagnostic-passthrough branch):

- **Single source of truth.** The "nothing-to-warm → readiness must fire" invariant lives in ONE place at `main.go:541`, not spread across both branches. The comment block at `main.go:530-540` already enumerates the three "bypass" startup paths it covers — adding `CACHE_ENABLED=false` as a fourth disjunct extends the existing safety-net contract.
- **Doc-aligned.** The existing comment at `main.go:533` LITERALLY says "several startup paths bypass that block entirely: CACHE_ENABLED=false (diagnostic passthrough), or a cache-setup failure (nil watcher)" — i.e. the author KNEW the cache=off path was supposed to be covered but the condition check missed it. The fix realizes the documented intent.
- **No new failure mode.** Adding a disjunct to an OR makes the gate fire MORE often, never less. The four-disjunct condition cannot regress the existing three cases. `MarkPhase1Done()` is idempotent (`phase1Done.Store(true)`, `internal/cache/phase1.go:78-80`) so any double-call is safe.

Option (iii) considered + rejected: a 3-state startup-state machine (warming/ready/cache-off) would be cleaner long-term but is over-engineering for a one-line fix. Reserve for a future refactor.

### §2.4 Why this is a correctness fix, not feature work (per `feedback_no_park_broken_behind_flag`)

The emergency-fallback path `CACHE_ENABLED=false` is the documented recovery mechanism per `project_redis_removal.md` + `project_caching_is_provisional.md`. If snowplow has any cache subsystem regression in production, the operator's first-resort is `helm upgrade --set env.CACHE_ENABLED=false` to bypass the cache and route to apiserver. That recovery mechanism is BROKEN today — flipping the toggle makes the pod permanently unhealthy. The fix is not optional and is not parked.

---

## §3 — CRD Regen Mechanics

### §3.1 Current state (TRACED)

- `apis/templates/v1/core.go:106` — `ClusterListWhenAllowed *bool` field exists (D.5).
- `crds/templates.krateo.io_restactions.yaml` — committed YAML is 188 lines, contains `userAccessFilter` + `exportJwt` (prior shipped fields) but NOT `clusterListWhenAllowed`. [TRACED via grep.]
- `scripts/gen.sh` — existing pipeline. Runs `go mod tidy` + `go generate ./...`, where `apis/templates/v1/zz_generated.deepcopy.go:19` confirms controller-gen is wired via a `go:generate` directive resolved against `sigs.k8s.io/controller-tools` pinned in `go.mod`.
- `scripts/gen.sh --check` — drift-detection mode used by CI. Compares committed `crds/` against a freshly-generated tree; fails if non-equal.

### §3.2 Regen commands (3 lines, per the existing pipeline)

```bash
cd /Users/diegobraga/krateo/snowplow-cache/snowplow
bash scripts/gen.sh
bash scripts/gen.sh --check   # verify drift-clean
```

Expected diff: ONE new property block under `restActions[].api[].properties.clusterListWhenAllowed` mirroring the Go-tag JSON description block at `apis/templates/v1/core.go:61-105`. No other fields should change — `userAccessFilter`, `exportJwt`, and all pre-existing fields stay byte-identical.

### §3.3 Chart sync (per `feedback_crd_pipeline_hygiene`, task #119)

The chart repo `/Users/diegobraga/krateo/snowplow-chart-braghettos/` does NOT carry a CRD copy — the snowplow chart applies CRDs from `crds/` upstream, not from a chart-resident `crd-chart/` directory. [TRACED: `grep -rn clusterListWhenAllowed /Users/diegobraga/krateo/snowplow-chart-braghettos/` returns zero hits, AND `ls .../chart/crds/` returns nothing.] So the regen is one-step: commit the regenerated `crds/templates.krateo.io_restactions.yaml` in the snowplow repo. The chart's CRD bootstrap is out-of-band.

If a future repo-layout change adds a `crd-chart/` directory in `snowplow-chart-braghettos`, this step would extend to `cp crds/*.yaml /path/to/chart/crds/`. Today it does not exist.

### §3.4 Auditable invariant

After regen, the post-condition is: `scripts/gen.sh --check` exits 0 AND `grep -c clusterListWhenAllowed crds/templates.krateo.io_restactions.yaml >= 2` (the property name + the description block).

---

## §4 — Acceptance Criteria (7 ACs — small ship)

- **AC-1**: `main.go:541` safety-net is `if cache.Disabled() || !cache.PrewarmEnabled() || cacheWatcher == nil { cache.MarkPhase1Done() }`. The comment block above (lines 530-540) is updated to enumerate the four bypass paths (was three).
- **AC-2**: `crds/templates.krateo.io_restactions.yaml` contains the `clusterListWhenAllowed` JSONSchema property block. `bash scripts/gen.sh --check` exits 0.
- **AC-3**: A new unit test (`main_test.go` or `readyz_test.go` extension) exercises the cache=off + prewarm=on + non-nil-watcher state and asserts `IsPhase1Done() == true` after the readiness-gate block runs. [The existing readyz tests at `internal/handlers/readyz_test.go` are handler-level; AC-3 is the startup-glue-level coverage gap that let this bug land.]
- **AC-4**: No new env flag. No new code path. No new exported symbol. The fix is a one-disjunct extension; nothing else changes.
- **AC-5**: An RA YAML carrying `spec.api[].clusterListWhenAllowed: true` `kubectl apply`s successfully against the upgraded snowplow chart. (HG-3 validator.)
- **AC-6**: Pod with `CACHE_ENABLED=false` AND `PREWARM_ENABLED=true` reaches `/health` "ready" (HTTP 200) within 60 s of start AND the cluster-internal LB to `snowplow:8081` resolves to a non-empty Endpoints list. (HG-1 validator.)
- **AC-7**: Pod with `CACHE_ENABLED=true` AND `PREWARM_ENABLED=true` is unchanged from 0.30.152 — admin-cold first-paint stays within ±5% of the 0.30.152 baseline. (HG-2 regression validator.)

Per `project_session_checkpoint_2026_05_18`-style ship discipline, 7 ACs is below the 14-AC over-engineering threshold the brief flagged.

---

## §5 — Hard Gates

### HG-1 — `CACHE_ENABLED=false` pod becomes healthy + LB reachable within 60 s (the INCIDENT VALIDATOR)

**Setup**: chart `helm upgrade --set env.CACHE_ENABLED=false --set env.PREWARM_ENABLED=true` against 0.30.153.

**Probe**: after `kubectl rollout status deploy/snowplow` returns, within 60 s of pod-Ready (or 120 s wall-clock from upgrade start):
1. `curl -fsS http://<pod-ip>:8081/health` returns `{"status":"ready",...}` (NOT `"warming"`).
2. `curl -fsS http://<pod-ip>:8081/readyz` returns 200.
3. `kubectl get endpoints snowplow -o json | jq '.subsets[0].addresses | length'` returns `>= 1`.
4. From a co-located test pod, `curl -fsS http://snowplow.krateo-system:8081/call?... ` returns 200 (passthrough to apiserver works end-to-end).

**Pass**: all 4 conditions hold.
**Fail**: ANY condition fails → revert. This is the incident-recurrence gate.

### HG-2 — `CACHE_ENABLED=true` no regression (the SCORING-METRIC gate)

**Setup**: chart `helm upgrade --set env.CACHE_ENABLED=true --set env.PREWARM_ENABLED=true` against 0.30.153.

**Probe**: per the north-star ledger pattern (`project_north_star_ledger.md`) — Chrome MCP page-load for admin user, cold load (no service-worker cache, no L1).

**Pass**: admin first-paint ≤ 5% over the 0.30.152 baseline (~9.94 s session-cold per `project_session_checkpoint_2026_05_18`). All `/call` and `/readyz` continue to fire as before.
**Fail**: regression ≥ 5% → root-cause before promotion.

Per `feedback_byte_identical_baselines_clean_wire_shape`: HG-2 is wall-clock-perceptive, not corpus-byte-identical — the readiness-gate fix should be wire-shape-invisible (it only flips a startup flag earlier in cache-off mode).

### HG-3 — RA with `clusterListWhenAllowed: true` ACCEPTED by apiserver

**Setup**: against 0.30.153 (or any cluster with the regenerated CRD applied), `kubectl apply -f` a minimal RA stanza:

```yaml
apiVersion: templates.krateo.io/v1
kind: RestAction
metadata: { name: hg3-cluster-list-probe, namespace: krateo-system }
spec:
  api:
  - name: probe
    path: /apis/composition.krateo.io/v1/dummies
    endpointRef: { name: krateo-kube, namespace: krateo-system }
    clusterListWhenAllowed: true
```

**Pass**: `kubectl apply` exits 0; `kubectl get restaction hg3-cluster-list-probe -o yaml | grep clusterListWhenAllowed` returns the field.
**Fail**: apiserver rejects with `unknown field "clusterListWhenAllowed"` → the CRD regen didn't land or didn't propagate to the live cluster.

### HG-4 — D.5 mechanism FIRES when opted-in

**Setup**: against 0.30.153 cache=on, with a real RA that has BOTH `clusterListWhenAllowed: true` AND a non-empty `dependsOn.iterator` (without the iterator, the field is a no-op per `apis/templates/v1/core.go:102-104`).

**Probe**: dispatch `/call?resource=<that-RA>` against the cluster's admin identity (cluster-list permission granted).

**Pass**: `curl -fsS http://<pod-ip>:8081/debug/vars | jq '.snowplow_apiserver_fallthrough_total[] | select(.reason == "cluster-list-dispatch")'` returns a counter cell with `value >= 1`. AND the response payload is non-empty + RBAC-consistent with the iterator-fan-out baseline.
**Fail**: counter stays at 0 → mechanism isn't firing → the CRD shipped but the resolver wiring is wrong OR the apply was rejected silently.

[TRACED: `internal/cache/fallthrough_meter.go:169` defines `ReasonClusterListDispatch FallthroughReason = "cluster-list-dispatch"`; `internal/resolvers/restactions/api/resolve.go:344` is the dispatcher call site `attemptClusterListCollapse`.]

---

## §6 — Falsifier-First Pre-Deploy Reproduction (per `feedback_falsifier_first_before_ship`)

The incident IS the falsifier. Reproduce on a sandbox or local kind cluster BEFORE building 0.30.153:

### §6.1 Reproduce stuck-warming on the BROKEN build (0.30.152)

1. `kind create cluster --name snowplow-bug-repro` (or use any cluster).
2. `helm install snowplow snowplow/snowplow --set image.tag=0.30.152 --set env.CACHE_ENABLED=false --set env.PREWARM_ENABLED=true`
3. Wait 120 s.
4. `curl -fsS http://<pod-ip>:8081/health` → expect `{"status":"warming","phase1Done":false}`.
5. `kubectl get endpoints snowplow -o json | jq '.subsets[0].addresses // []'` → expect `[]`.
6. Stuck indefinitely (verify by polling at 5-min intervals — `phase1Done` will never flip).

### §6.2 Verify the fix on a 0.30.153 dev image

7. `helm upgrade snowplow snowplow/snowplow --set image.tag=0.30.153-dev --set env.CACHE_ENABLED=false --set env.PREWARM_ENABLED=true`
8. Within 60 s post-rollout: `/health` → `{"status":"ready",...}`, Endpoints non-empty (HG-1 mechanical equivalent).

### §6.3 Auditable artifact

Capture the `/health` and `Endpoints` output from BOTH step 4-5 (broken-0.30.152 baseline) AND step 8 (fixed-0.30.153) and attach to the ship ledger row, per `feedback_falsifier_first_before_ship`. Without the broken-build artifact, this ship is shipped against imagined state.

---

## §7 — Test Plan

### §7.1 New unit test (AC-3)

`internal/handlers/readyz_test.go` or `main_test.go` (TBD by dev based on test-package layering):

- Test name: `TestReadinessGateFires_WhenCacheDisabledWithPrewarmEnabled`.
- Setup: `t.Setenv("CACHE_ENABLED", "false")`, `t.Setenv("PREWARM_ENABLED", "true")`, `cache.ResetPhase1DoneForTest()`.
- Drive the same logic the `main.go:541` block executes (or refactor the condition into a small helper `func shouldFlipPhase1Done() bool` for testability).
- Assert `cache.IsPhase1Done() == true` after the gate block.

Companion test: `TestReadinessGateFires_WhenCacheEnabledWithPrewarmDisabled` (existing behavior — regression-guards the original disjunct).

### §7.2 Existing regression coverage

- `internal/handlers/readyz_test.go:25, 53, 80, 89` — existing handler tests for the `/readyz` response shape.
- `internal/handlers/dispatchers/phase1_walk_test.go:206-245` — existing premature-Ready falsifier (asserts `IsPhase1Done()` stays false until Phase 1 actually completes). Per `feedback_no_park_broken_behind_flag` ethos: the new disjunct must NOT make this falsifier flake — only the cache-off path early-flips, and the falsifier exercises cache-on.

### §7.3 Pre-build CRD-drift CI gate

After `bash scripts/gen.sh`, run `bash scripts/gen.sh --check` locally. The check MUST exit 0 BEFORE the dev commits. CI will re-run it; a drift failure there means the regen wasn't committed.

### §7.4 In-cluster integration tests

- HG-1 (cache=off): manual chart upgrade per §5.HG-1.
- HG-2 (cache=on regression): the tester's existing 0.30.152 baseline workflow.
- HG-3 (CRD applies): a kubectl apply against the upgraded chart.
- HG-4 (mechanism fires): a /debug/vars probe against the same upgraded chart, with an opted-in RA.

---

## §8 — Open Questions

**OQ-1** — Should `main.go:541` be refactored into a named helper for testability (`shouldFlipPhase1DoneOnStartup(cacheEnabled, prewarmEnabled, watcherIsNil bool) bool`)? Pro: AC-3's unit test no longer needs a setenv dance. Con: adds 10 lines for a 1-line policy. Architect recommends YES — small refactor, large readability win, makes the four-disjunct invariant explicit. Dev decision; either resolution is acceptable.

**OQ-2** — In §3.3, the chart-side CRD copy is asserted ABSENT today; confirm with the chart maintainer (Diego, or check the chart repo's deploy-time CRD source) before shipping. If a `crd-chart/` is created during 0.30.153 lead-time, the dev MUST also sync it. [INFERRED today; verify before ship.]

---

## §9 — Tag / Commit / Ledger Template

### §9.1 Commit message

```
fix(cache): readiness-gate fires when CACHE_ENABLED=false (Ship 0.30.153)

The diagnostic-passthrough branch at main.go:484 never calls
cache.MarkPhase1Done(); the safety-net at main.go:541 only fired when
PREWARM_ENABLED is OFF or the watcher is nil. With CACHE_ENABLED=false
+ PREWARM_ENABLED=true + a successfully-constructed passthrough
watcher, neither disjunct fired — pod stuck warming forever, Service
endpoints empty, snowplow LB unroutable.

Fix: add cache.Disabled() as a fourth disjunct to the main.go:541
safety-net. MarkPhase1Done is idempotent; the existing three startup
paths are unchanged.

Bundled CRD regen: scripts/gen.sh refreshes crds/ for D.5's
clusterListWhenAllowed field that D.5 (0.30.152) added to the Go type
but did not run controller-gen against. Apiserver rejects any RA
opting in without the regenerated CRD; D.5's mechanism cannot fire
until this ships.

Closes: incident-2026-05-21-hg5-stuck-warming
Refs: D.5 (0.30.152), Tag B (0.30.102)
```

### §9.2 Tag

`v0.30.153`

### §9.3 Ledger row

| Ship | Tag | Date | Cache-off cold | Cache-on cold | First-paint | Counter | Verdict |
|---|---|---|---|---|---|---|---|
| 0.30.153 | v0.30.153 | 2026-05-21 | ✅ <60s healthy (HG-1) | ~9.94s session-cold (HG-2) | unchanged | `cluster-list-dispatch ≥ 1` (HG-4) | PASS / FAIL |

Per `feedback_maintain_feature_journal` AND `feedback_maintain_regression_journal`: this ship is BOTH a feature (D.5 CRD activation) AND a regression-fix (cache-off readiness). Append to BOTH journals with the broken-build falsifier (§6.1) cited as the original-defect artifact.

---

## §10 — One-Paragraph Summary

Ship 0.30.153 fixes the latent `CACHE_ENABLED=false` readiness-gate bug exposed by D.5's HG-5 (pod stuck `{"status":"warming"}`, Service has 0 endpoints, LB unroutable — the snowplow emergency-recovery toggle is unusable today) AND ships the missing CRD regeneration for D.5's `clusterListWhenAllowed` field (without which production apiservers reject any RA opting in, so D.5's cluster-list mechanism can never fire). Both changes are mechanical: a one-disjunct extension at `main.go:541` (`cache.Disabled() || !cache.PrewarmEnabled() || cacheWatcher == nil`) plus `bash scripts/gen.sh` + commit. 7 ACs, 4 hard gates (HG-1 incident validator, HG-2 cache-on regression guard, HG-3 CRD schema validation, HG-4 D.5 mechanism activation), falsifier is the incident itself (reproduce stuck-warming on 0.30.152, verify clean on 0.30.153). Risk: near-zero — the readiness fix only makes an idempotent `MarkPhase1Done` fire in one additional case; the CRD regen is pure JSONSchema addition (no existing field changes). Promotion gate: HG-1 PASS is mandatory (the recurrence guard); HG-2 PASS is the no-regression sign-off; HG-3 + HG-4 unblock D.5 in production.
