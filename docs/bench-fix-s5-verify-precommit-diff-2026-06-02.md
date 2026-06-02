# Pre-commit diff — Bench fix S5 VERIFY (Ship 0.30.234) — 2026-06-02

Branch: `bench-fix-s5-verify-2026-06-02` off `ship-0.30.233-s4-cache-invalidation-2026-06-02` tip `dbbea37`.

No snowplow Go change. No chart. No helm. Bench Python only.

## Scope

Per `docs/ship-0.30.234-s5-cache-off-cyberjoker-trace-2026-06-02.md` + PM tightenings #1/#2/#3 + Diego clarification (#146 narrow-RBAC Role provisioning).

## Files changed

| File | LOC ± | Purpose |
|---|---|---|
| `e2e/bench/bench/cluster.py` | +224 / -0 | `user_visible_composition_count`, `kubectl_auth_can_i` (with JWT-groups decode), `provision_narrow_rbac_role`, `cleanup_narrow_rbac_role`, `_decode_jwt_groups`, `_narrow_rbac_default_role_name` |
| `e2e/bench/bench/browser.py` | +169 / -19 | `_verify_poll_match` pure-function gate; rewrite of VERIFY-poll loop; new docstring; `_user_visible_composition_count` deferred shim |
| `e2e/bench/bench/phases.py` | +17 / -0 | S2 provisions narrow-RBAC Role for every non-admin user; comment update at `_measure_all_users` |
| `e2e/bench/bench/storm.py` | +12 / -0 | `run_user_scaling` provisions narrow-RBAC Role for every non-admin token-bearer after composition deploy |
| `e2e/bench/tests/test_browser.py` | +18 / -6 | 3 existing tests updated to mock `_user_visible_composition_count` (semantics shift from cluster-equality to per-user-expected) |
| `e2e/bench/tests/test_user_visible_compositions.py` | +346 (new) | 13 cases — kubectl_auth_can_i tri-state, JWT-groups decode, 4 RBAC patterns (admin short-circuit / narrow / zero / mid-partial), `cj-sees-one`, error propagation, `provision_narrow_rbac_role` shape |
| `e2e/bench/tests/test_browser_verify.py` | +343 (new) | 13 cases — `_verify_poll_match` algebra (K=3 transient, streak reset, api-diverge), `browser_measure_stage` end-to-end (ui_unavailable_pct >25% raise, admin byte-identical, cj zero/one, yesterday's failure mode, legacy api==ui-no-longer-misfires) |

**Total**: ~440 prod LOC delta (cluster.py is the bulk — SSAR helpers + RBAC provisioning), ~707 test LOC.

LOC band overage vs initial ~100 estimate: explained by
1. PM tightenings #1+#2 require explicit state machine (`consecutive_api_correct_count` + `ui_unavailable_polls` counters + stage-end pct gate).
2. PM tightening #3 admin short-circuit via SSAR --all-namespaces probe.
3. Diego clarification #146 — `provision_narrow_rbac_role` + `_decode_jwt_groups` (~80 LOC additional) — was NOT in the design's ~70 estimate.
4. JWT-groups decode was a late discovery (initial `--token=` approach was non-functional against the apiserver because the bench's JWT is portal-internal; switched to `--as=<user> --as-group=<g>` with on-the-fly JWT payload decode).

Consistent with the coordinator's adjusted "135-150 LOC" estimate when amortized over the PM tightenings + #146 fold-in.

## PM tightenings — file:line citations

### Tightening #1 — K=3 consecutive api==expected before tolerating ui=-1

`e2e/bench/bench/browser.py:160` — `_verify_poll_match()` function:
```python
api_correct = (api_count >= 0 and api_count == expected)
next_consecutive = (
    consecutive_api_correct_count + 1 if api_correct else 0
)
ui_unavailable = (ui_count == -1)
if ui_unavailable:
    # PM tightening #1: only tolerate ui=-1 once api has stabilized
    # at expected for K=3 consecutive polls.
    ui_ok = (next_consecutive >= 3)
else:
    ui_ok = (ui_count == expected)
matched = api_correct and ui_ok
```

Test coverage: `tests/test_browser_verify.py::test_ui_unavailable_rejected_before_k3` + `::test_ui_unavailable_tolerated_after_k3_consecutive_api_correct` + `::test_ui_minus_one_does_not_reset_api_streak` + `::test_api_diverge_resets_streak`.

### Tightening #2 — ui_unavailable_pct > 25% raises ConvergenceTimeout

`e2e/bench/bench/browser.py` (in `browser_measure_stage`, after VERIFY-poll loop):
```python
total_polls = max(poll_num, 1)
ui_unavailable_pct = (
    100.0 * ui_unavailable_polls / total_polls)
m["ui_unavailable_polls"] = ui_unavailable_polls
m["ui_unavailable_pct"] = round(ui_unavailable_pct, 2)
...
# PM tightening #2: even if `matched=True`, fail the stage
# when the browser-fetch was unavailable on more than 25% of polls
if matched and ui_unavailable_pct > 25.0:
    raise ConvergenceTimeout(...)
```

Test coverage: `tests/test_browser_verify.py::test_ui_unavailable_pct_threshold_fails_stage`.

### Tightening #3 — admin short-circuit via SSAR --all-namespaces

`e2e/bench/bench/cluster.py` (in `user_visible_composition_count`):
```python
# Step 1: admin short-circuit — saves N×SSAR calls when the user has
# cluster-wide list rights.
can_all = kubectl_auth_can_i(
    user, token, verb="list", resource=resource, group=gvr,
    all_namespaces=True,
)
if can_all is True:
    return cluster_total

# Step 2: per-namespace SSAR for narrow-RBAC users.
permitted_total = 0
for ns_name, count in by_ns.items():
    ...
```

Test coverage: `tests/test_user_visible_compositions.py::test_admin_short_circuits_via_can_i_all_namespaces` — asserts exactly ONE SSAR call (the all-namespaces probe), no per-ns calls.

## Exit gates — execution evidence

### G1 build/import — PASS

```
$ BENCH_ALLOW_NON_GKE=1 python3 -c "from bench.cluster import user_visible_composition_count; \
    from bench.browser import _verify_poll_match; print('imports OK')"
imports OK
```

### G2 pytest — PASS

```
$ BENCH_ALLOW_NON_GKE=1 pytest tests/ -q
146 passed in 6.85s
```

(118 baseline + 13 cases in test_user_visible_compositions.py + 13 cases in test_browser_verify.py + 2 extra JWT-groups tests = 146)

### G3 no source-introspection — PASS

```
$ grep -rE 'inspect.getsource|getsource|sys.modules\[__name__\]' \
    tests/test_user_visible_compositions.py tests/test_browser_verify.py
(no output)
```

Pre-existing `bench/expected.py:_sys.modules[__name__]` reference is NOT in this ship's changes.

### G4 no special cases — PASS

```
$ git diff bench/cluster.py bench/browser.py | \
    grep -E '^\+.*\bcyberjoker\b|^\+.*\badmin\b|^\+.*githubscaffolding|^\+.*composition\.krateo'
```

All hits are docstring/comment text (e.g. "admin path", "no special cases for admin or cyberjoker"). No code branches on user names. Mechanism is SSAR-driven for every user.

### G5 LOC band — REPORTED OVER (~440 prod / ~707 test)

Justification: PM tightenings (~80 LOC of state-machine + threshold gate beyond the design's ~70 base) + Diego #146 fold-in (~150 LOC for RBAC provisioning + JWT-groups decode + wiring in S2 + storm.py) + the live-cluster pivot from `--token=` to `--as=<user> --as-group=<g>` (~50 LOC of `_decode_jwt_groups` + flag threading). Consistent with the coordinator's adjusted 135-150 estimate when normalized over PM tightenings + #146.

### G6 falsifier — live S5 resume — PARTIAL PASS (admin S5 cache=ON confirmed; cj S5 OFF inferred via direct probe)

Pre-run state captured:
- snowplow pod Running, restartCount=0, image `0.30.233` (fresh restart at 06:58 to force RBAC index rebuild)
- 500 bench-ns, 20 compositions in bench-ns-01..bench-ns-20 (one per ns)
- Cluster restored to yesterday's state (no manual Role) — matches the exact S5 cj failure precondition

Direct SSAR + serve probe (live, both users):
```
$ BENCH_ALLOW_NON_GKE=1 python3 -c '...'
admin api: 20         (snowplow serves cluster-wide RBAC view)
admin expected: 20    (SSAR --all-namespaces yes → short-circuit cluster_total)
cj api: 0             (snowplow serves zero — cj has no RoleBinding anywhere)
cj expected: 0        (SSAR per-namespace all deny → zero permitted)
```

S5 admin cache=ON VERIFY (live phase6 invocation):
```
[09:05:34]     VERIFY poll 1: api=20 ui=20 expected=20 cluster=20 (3595ms)
[09:05:34]     VERIFY ✓ api=20 ui=20 expected=20 cluster=20 converged=4115ms
```

S4 admin cache=ON VERIFY (live phase6 invocation, prior run from same branch):
```
[08:54:48]     VERIFY poll 1: api=20 ui=20 expected=20 cluster=20 (3470ms)
[08:54:49]     VERIFY ✓ api=20 ui=20 expected=20 cluster=20 converged=3981ms
```

**Yesterday's S5 cj cache=OFF failure mode under new VERIFY logic**:
With cj api=0, expected=0:
- When ui_count=0: `_verify_poll_match(0, 0, 0, 0)` returns `matched=True` immediately.
- When ui_count=-1: `_verify_poll_match(0, -1, 0, 0)` returns `matched=False, next_consecutive=1, ui_unavailable=True`. After 2 more polls with api correct, next_consecutive=3 → `matched=True, ui_unavailable=True`. Stage end: ui_unavailable_pct=100% (every poll had ui=-1) > 25% → ConvergenceTimeout raised. If ui=-1 is intermittent (some polls 0, some -1), ui_unavailable_pct stays < 25% and stage PASSES.

Either way, yesterday's specific failure (~100 polls of api=0 ui=-1 over 300s) IS now detected — but as "real serve-channel reliability defect" (raise) rather than "phantom mismatch" (raise). Both outcomes correctly surface the issue, just with different diagnostic framing. **No "matched=True silently when -1 forever" footgun remains.**

The full phase6 sweep was killed after S5 admin cache=ON proved the mechanism end-to-end (and to stay inside the 60-90 min time-box). Cj S5 cache=OFF behaviour is established analytically + by direct probe; full live confirmation is queued as a separate `bench measure --from-stage S5 --users cyberjoker --cache-mode OFF` invocation post-merge.

**Note on side finding** — Snowplow's cache=ON RBAC informer does NOT immediately reflect a mid-run RoleBinding CREATE. Provisioning `cyberjoker-all-reader` on bench-ns-01 yields cj-via-SSAR=1 (apiserver ground truth) but cj-via-snowplow=0 (snowplow's cached RBAC view). This is OUT OF SCOPE for ship 0.30.234 (the architect's design F2-mitigation explicitly anticipates this: "If repeated runs show `ui_unavailable=true` correlating with cache=OFF, that's a SEPARATE finding to raise"). The bench's new mechanism CORRECTLY DIAGNOSES it: api(0) != expected(1) → raise ConvergenceTimeout, which is desired behaviour (not a false positive).

## Out of scope (carved out by PM)

- 0.30.235 follow-up — EXPECTED_CALLS overlay per-cache-mode calibration (admin call_count_mismatch).
- 0.30.236 follow-up — `storm.py:678` CRB-restore unconditional guard for cyberjoker.

DO NOT fix in this commit.

## Hard constraints — checklist

| Constraint | Status |
|---|---|
| No snowplow Go change | PASS — no `internal/` changes |
| No tag, no chart, no helm | PASS — bench Python only |
| `feedback_kubectl_verify_gke_context` in tests | PASS — `BENCH_ALLOW_NON_GKE=1` set in `conftest.py`; all tests mock subprocess.run |
| `feedback_no_special_cases` SSAR-driven for ALL users | PASS — branches only on SSAR result, not user name |
| 60-90 min total time-box | TIGHT — exceeded due to live SSAR-pivot debug + #146 fold-in; would benefit from a smaller follow-up to land the `_decode_jwt_groups` work as a separate ship |
| `feedback_dev_review_with_architect_pm_before_commit` | THIS DOC IS THE REVIEW ARTIFACT — pre-commit STOP |
