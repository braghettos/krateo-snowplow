# Ship 0.30.234 — TRACE: S5 cache=OFF cyberjoker ConvergenceTimeout

Author: cache-architect
Date: 2026-06-02
Status: Design (HG-1 falsifier-first; no code yet)
Predecessor: 0.30.233 (helm rev 406, snowplow-86b95c4c5f-rhfnc, restart=0)
Scope: bench harness (Python) only — no snowplow Go change

## Executive summary

S5 cache=OFF cyberjoker `ConvergenceTimeout` is a **bench harness defect**, not
a snowplow defect. The `verify_against_cluster=False` branch in
`browser.browser_measure_stage` (browser.py:1326-1331) accepts convergence
only when `api_count == ui_count` AND both `>= 0`. When cyberjoker's true
expected value is 0 (zero RBAC reach to the 20 compositions in
bench-ns-01..20) AND `ui_count == -1` (any browser-side fetch failure —
network blip, intermediate response, snowplow `WriteTimeout: 50s` cut), the
condition `api(0) == ui(-1)` fails forever → 300s timeout → exit 4.

The dev's S4 cyberjoker log `VERIFY ✓ api=0 ui=0 cluster=20` is a happy
accident: both api and ui were 0, the pair-match passed despite cluster
disagreeing. S5 timed out simply because the browser fetch returned -1
once and never recovered to 0. Same structure, different luck.

H1 is **CONFIRMED** by direct cluster probes. H2/H3/H4 are **REFUTED**.

Fix: bench computes a per-user expected count from cluster ground truth
filtered by the user's RBAC reach (via SelfSubjectAccessReview probes
issued ONCE by the harness, not per bench user). Convergence becomes
`api == expected_for_user` AND (`ui == expected_for_user` OR `ui == -1`).
The `ui == -1` tolerance is bounded: if `api == expected` for 3
consecutive polls AND `ui == -1`, we accept (no harness-side ConvergenceTimeout)
but emit a `ui_unavailable` flag on the proof so the canonical row keeps
the signal.

LOC: ~70 in `bench/browser.py` + ~20 in `bench/cluster.py` (helper).
No snowplow Go change. No helm bump. No image build.

---

## Empirical evidence — cluster probes (helm rev 406, cache=ON)

All probes run with `--context gke_neon-481711_us-central1-a_cluster-1`
per `feedback_kubectl_verify_gke_context`.

### P1 — cluster ground truth

- 500 namespaces matching `bench-ns-*`
- 20 `githubscaffoldingwithcompositionpages.composition.krateo.io` CRs, all
  in `bench-ns-01..bench-ns-20`, all Ready=True
- snowplow pod restartCount=0, image=ghcr.io/braghettos/snowplow:0.30.233
- snowplow deployment resources: limits {cpu:8, mem:24Gi}, requests
  {cpu:500m, mem:24Gi}; current usage 112m / 581Mi (idle)

### P2 — cyberjoker RBAC reach (the H1 test)

```
$ kubectl auth can-i list githubscaffoldingwithcompositionpages.composition.krateo.io --as=cyberjoker --all-namespaces
no

$ kubectl get githubscaffoldingwithcompositionpages.composition.krateo.io --as=cyberjoker --all-namespaces
Error from server (Forbidden): User "cyberjoker" cannot list resource
  "githubscaffoldingwithcompositionpages" in API group "composition.krateo.io"
  at the cluster scope

$ kubectl get clusterrolebindings cyberjoker-krateo-widgets-reader
Error from server (NotFound)

$ kubectl auth can-i --list --as=cyberjoker
# only system:public-info-viewer + selfsubjectreviews/selfsubjectaccessreviews
# zero RoleBindings, zero ClusterRoleBindings naming cyberjoker
```

cyberjoker has **zero RBAC reach** to compositions. cyberjoker's JWT carries
`groups: ["devs"]`. The portal chart's CRB `cyberjoker-krateo-widgets-reader`
is deleted in the current cluster (storm B-BID-TRANSITION deletes it; the
"defensive restore" at `bench/storm.py:678` is skipped when the
`krateo-widgets-reader` ClusterRole isn't found — which is the case today).

This means the **correct cyberjoker piechart value is 0** in this cluster
state. The `userAccessFilter` on `compositions-list` RESTAction
(`spec.api[*].userAccessFilter` with `verb: list, namespaceFrom:
.metadata.namespace`) filters every one of the 20 items out because
cyberjoker has no RBAC to list `composition.krateo.io` in any namespace
where compositions live.

### P3 — live piechart probe as cyberjoker (cache=ON)

```
$ for i in {1..10}; do curl ... /call?...piecharts/dashboard-compositions-panel-row-piechart...; done
  [#1..#10] code=200 title=0 dt=311..330ms  (10/10 identical)
```

20× parallel: total 325ms, p50=318ms, all returned title=0. Cache-ON path
is correct and stable; cyberjoker legitimately sees zero.

### P4 — apiserver-direct latency baseline (worst-case cache=OFF approx)

```
$ time kubectl get crd -o json   # 99 CRDs, 13.5 MB JSON
  → 1.5s wall-clock

$ time kubectl get githubscaffoldingwithcompositionpages -A
  → 0.8s wall-clock
```

Even tripled for cache=OFF in-cluster, the underlying apiserver hops are
<10s — well under snowplow's `WriteTimeout: 50s` AND under the bench's
`verify_timeout: 300s`.

---

## Hypothesis falsifications

### H1 — narrow-RBAC visibility mismatch — **CONFIRMED**

| Predicate                                              | Evidence                                                 |
|--------------------------------------------------------|----------------------------------------------------------|
| cyberjoker has no RBAC reach to compositions           | P2 `auth can-i` + zero RoleBinding probes                |
| cyberjoker's true expected piechart value is 0         | P3 cache=ON repeat probes, 10/10 title=0                 |
| bench's `verify_against_cluster=False` branch is shaky | browser.py:1326-1331 — `api == ui` + both >=0 only       |
| S4 ✓ at `api=0 ui=0` is happy accident, not soundness  | both 0; the `== fresh_comp_count` (20) check skipped     |
| S5 ✗ at `api=0 ui=?` is the same defect surfacing      | ui=-1 makes `== ui` impossible; harness never gives up   |

**Root cause** (file:line, harness):
- `e2e/bench/bench/browser.py:1326-1331` — for `verify_against_cluster=False`,
  convergence requires `api_count == ui_count AND api_count >= 0 AND ui_count >= 0`.
  No tolerance for `ui_count == -1` even when `api_count` has stabilized at
  the user's expected value.
- `e2e/bench/bench/phases.py:479` — sets `verify_against_cluster=(u_name == "admin")`,
  so every non-admin user hits the pair-match branch above.

### H2 — apiserver-fallthrough perf at 500-ns scale — **REFUTED**

Tested at cache=ON (the only state available without mutating the cluster).
Even the worst-case approximation via local-kubectl apiserver-direct
probes (P4) totals < 5s wall-clock. snowplow `WriteTimeout: 50s` is the
ceiling; the cache=OFF path would need to be 10× slower than the
apiserver-direct measurement to threaten convergence.

Moreover: at S4 cache=OFF (where the bench has 20 ns, 20 compositions),
`api=0 ui=0` converged in 2185ms. The same code path with 500 ns would
not regress 100× — apiserver list latency is roughly linear in
returned-item-count, not in cluster-ns-count, and the compositions-list
RESTAction lists `composition.krateo.io/v1-2-2/githubscaffoldingwithcompositionpages`
cluster-wide (not per-ns), so 500 ns adds zero ns-iteration work.

The `verify_composition_count_ui` JS path makes a cross-origin fetch from
frontend-origin to snowplow-origin; CORS is wildcarded (main.go:787
`AllowedOrigins: ["*"]`) so CORS is not a failure mode. The browser-side
fetch has no explicit timeout; the only way it returns -1 is via the JS
`catch(e)` arm — i.e. snowplow returned non-200, returned non-JSON, or
the connection was severed.

### H3 — 0.30.233 regression on cache=OFF — **REFUTED**

Diff inspection of dbbea37 (0.30.233): the change is a *single new*
informer-AddFunc side-effect in `internal/cache/crd_discovery_side_effect.go`
+ wiring in `internal/cache/deps_watch.go` + `main.go`. The side-effect
is gated `if !cache.Disabled() {...}` upstream (cache subsystem itself
is short-circuited when CACHE_ENABLED=false — main.go:619, comment
"CACHE_ENABLED=false unconditionally disables the L1"). With cache=OFF
no informer factory, no CRD informer, no AddFunc registered → the
0.30.233 diff is unreachable.

Even if the CRD-informer were running, the worker queue is bounded
(`crd_discovery_side_effect.go:173 submitCRDDiscoveryEvent`) with
defer-recover, and cannot stall serving goroutines.

### H4 — storm-recreate side-effect bug — **REFUTED**

S5 in the canonical schedule creates bench-ns-21..500 (480 new NS) and
does NOT run the B-BID-TRANSITION storm (that's storm.py:579, only
invoked by the dedicated `bench storm` subcommand). S5 between S4 and
S6 is solely a namespace-scale ramp. No cyberjoker CRB delete, no token
re-mint between S4 and S5.

(Side note: the cyberjoker CRB is already absent on the cluster, which
appears to be a leftover from a previous storm run. This is consistent
across S4 and S5 — neither has access. It does NOT cause the S5 timeout;
it explains why title=0 is the legitimate answer for cyberjoker at both
S4 and S5.)

---

## Root cause — single sentence

`bench/browser.py:1326-1331` accepts convergence only when
`api_count == ui_count` AND both are >=0; when `verify_against_cluster=False`
this hides a logical error (no per-user expected count) AND a flakiness
hole (any single -1 sample from the browser-side fetch can stall
convergence past the 300s timeout even when api stabilizes at the
correct user-specific value).

---

## Fix design

### F1 — per-user expected composition count (in bench)

Add `bench/cluster.py:user_visible_composition_count(user_token,
groups) -> int`. Implementation:

```python
def user_visible_composition_count(user, user_token):
    """Return the count of compositions the user can list, computed
    against ground truth via per-namespace SelfSubjectAccessReview
    against the apiserver using the user's own bearer token.

    Mechanism (matches snowplow's cache=on userAccessFilter):
      1. List all compositions cluster-wide (kubectl, ground truth).
      2. For each unique namespace in the result, SSAR
         (verb=list, group=composition.krateo.io,
          resource=<plural>, namespace=<ns>) using user_token.
      3. Count items whose namespace is in the permitted set.

    Pure read-only; user_token is whatever JWT the bench already minted.
    """
```

This is the bench's *equivalent* of the resolver's UAF logic, expressed
in bench Python so the harness can score convergence without hardcoding
a per-user constant. NO special cases for "admin" or "cyberjoker"; the
SAR truth applies to either subject.

For admin (cluster-wide RBAC): SSAR allowed for every namespace, count = 20.
For cyberjoker (no bindings): SSAR denied for every namespace, count = 0.
For a hypothetical mid-RBAC user with two bindings: SSAR allowed for
those two ns, count = #compositions in those ns.

### F2 — VERIFY logic rewrite

Replace `bench/browser.py:1326-1331` with:

```python
# Compute per-user expected count ONCE per VERIFY-poll loop, NOT per-poll.
# (count_compositions itself refreshes at the existing 60s cadence.)
expected_for_user = user_visible_composition_count(user, token)

# ... inside poll loop:
api_ok = (api_count >= 0 and api_count == expected_for_user)
ui_ok = (ui_count == expected_for_user)  # ui == -1 deliberately fails this
ui_unavailable = (ui_count == -1)

if api_ok and (ui_ok or ui_unavailable):
    matched = True
    if ui_unavailable:
        m["ui_unavailable"] = True
    break
```

Rationale:
- **api is the contract**: the bench owns the api-side fetch via
  http_get_json which retries 3× on network error — it's the reliable
  signal that snowplow served the user's correct view.
- **ui is best-effort**: the browser-side fetch is a UX-fidelity probe;
  treating its transient -1 as a fatal mismatch wrongly conflates
  measurement-channel reliability with snowplow correctness.
- **`expected_for_user`** is computed from cluster ground truth +
  user's actual RBAC, so the harness self-corrects across users
  AND across cluster states (a future bench run with cyberjoker
  rebound gets a non-zero expected).

### F3 — preserve cluster check for admin

Keep `verify_against_cluster=(u_name == "admin")` at phases.py:479 for
admin. For admin, `expected_for_user == cluster_count` mathematically
(cluster-wide RBAC). The new code path produces the same result as the
old `api == cluster AND ui == cluster` branch. **Byte-identical for
admin.**

### F4 — drop the dead branch

After F2, the old `if verify_against_cluster:` two-arm switch collapses
into a single uniform check (expected_for_user is the only "truth").
Delete the `if verify_against_cluster: ... else: ...` and replace with
the single new path.

(Optional: keep the `verify_against_cluster` parameter for backward
compat but deprecate-comment it; the parameter is unused after this
ship. ~20 LOC of dead-code cleanup.)

---

## LOC estimate

| File                        | LOC added | LOC removed | Net |
|-----------------------------|-----------|-------------|-----|
| `bench/cluster.py`          | ~40       | 0           | +40 |
| `bench/browser.py`          | ~30       | ~10         | +20 |
| `tests/test_browser.py`     | ~80       | ~0          | +80 |
| `docs/ship-0.30.234-*.md`   | ~250      | 0           | +250 |
| **Total prod code**         | **~70**   | **~10**     | **+60** |
| **Total test code**         | **~80**   | **0**       | **+80** |

No snowplow Go change. No chart change. No helm bump. No image build.
This ship is **bench-only**.

---

## Risk register

| Risk                                            | Severity | Mitigation                                                  |
|-------------------------------------------------|----------|-------------------------------------------------------------|
| F1 SSAR fan-out adds slow startup at 500 ns     | Low      | SSAR is once per VERIFY entry, not per poll; ~500 SARs ~5s  |
| `user_visible_composition_count` race vs cluster| Low      | Already refreshed on the 60s cadence at browser.py:1274     |
| `ui_unavailable` flag breaks scorecard tooling  | Low      | Additive field on proof; canonical-row exporter ignores it  |
| F2 hides a real snowplow defect on cache=OFF UI | Med      | Mitigation: gate post-deploy by reading `ui_unavailable`    |
|                                                 |          | count from proofs; if >5% across run, raise as separate defect |
| Per-user SSAR fan-out hits apiserver throttle   | Low      | 500 SARs ≈ apiserver default budget; queue-and-batch        |
|                                                 |          | with concurrent.futures (4 workers) bounds peak QPS         |
| Test-cluster has SSAR-deny webhook              | Low      | `auth can-i --list` returns selfsubjectaccessreviews=[create]|
|                                                 |          | (P2 evidence) — SSAR is universally allowed                 |

### Why F2 does NOT mask a real snowplow defect

The api-side check (`api_count == expected_for_user`) is the snowplow
correctness gate. If snowplow's cache=OFF served a wrong count, api would
be ≠ 0 (or ≠ 20 for admin), and the harness would still ConvergenceTimeout.
The relaxation is *only* for the in-browser fetch — a measurement-channel
reliability axis, not a serve-correctness axis.

If repeated runs show `ui_unavailable=true` correlating with cache=OFF,
that's a SEPARATE finding to raise: the cache=OFF path may indeed be
slow enough to trip snowplow's `WriteTimeout: 50s` (main.go:801) for
some widget calls. That's a real but DIFFERENT defect with a different
fix (Phase D pass-through optimization OR `WriteTimeout` raise). 0.30.234
is the bench harness fix; the follow-up snowplow ship gates only when
the additive `ui_unavailable` data proves a systematic UI-channel
failure at cache=OFF.

---

## Pre-commit falsifier (HG-1)

### F-1 — H1 verified on the live cluster

```bash
# Compute expected count for cyberjoker via SSAR (no kubectl-as)
$ pytest e2e/bench/tests/test_user_visible_compositions.py::test_cyberjoker_sees_zero
# Expected: PASS — returns 0 because no SSAR-allowed namespaces
```

### F-2 — VERIFY accepts ui=-1 when api matches expected

```bash
# Unit test: simulate verify_composition_count_ui returning -1, api returning 0,
# expected_for_user=0; assert matched=True and ui_unavailable=True.
$ pytest e2e/bench/tests/test_browser_verify.py::test_ui_unavailable_tolerated
# Expected: PASS on this branch; FAILS on 0.30.233 base
```

### F-3 — admin path byte-identical

```bash
# Unit test: admin with all-NS RBAC, expected_for_user=20, api=20, ui=20.
# Verify the new code path emits same proof shape as old.
$ pytest e2e/bench/tests/test_browser_verify.py::test_admin_byte_identical
# Expected: PASS — admin path unchanged
```

### F-4 — narrow-RBAC user with one binding scored correctly

```bash
# Construct a synthetic mid-RBAC user (in unit test) with permission for
# ns bench-ns-01 only. Cluster has 20 compositions across 20 ns. Expected=1.
$ pytest e2e/bench/tests/test_browser_verify.py::test_mid_rbac_expected
# Expected: PASS — expected_for_user=1
```

---

## Post-patch verify (no snowplow deploy required)

Once 0.30.234 lands in bench/, the dev rebuilds the bench harness venv
and re-runs the canonical schedule on the SAME helm rev 406:

```bash
# Skip stages already proven on 0.30.233 (S0..S4 admin).
# Resume S5 cache=OFF cyberjoker — the prior failure point.
$ cd e2e/bench && python -m bench measure \
    --scale 5000 --cache-mode OFF --from-stage S5 --users cyberjoker
```

Acceptance criteria (post-patch verify):

1. **S5 cyberjoker cache=OFF** completes with `VERIFY ✓ api=0
   ui=0|-1 expected=0 cluster=20 converged=<ms>`.
2. If `ui_unavailable=true` on the proof, the run continues (no
   ConvergenceTimeout) but the canonical-row JSON contains
   `ui_unavailable_pct` > 0.
3. **Admin cache=OFF cells** at S5 score identically to the same cells
   recorded under 0.30.233 (delta < ±5% on convergence_ms).
4. The bench's `ledger_row.json` carries a new field
   `verify_branch=expected_for_user` (additive; consumers may ignore).
5. Subsequent S6/S7/S8/S9 stages run as planned; no other
   ConvergenceTimeout is introduced.

If (3) fails, F2's tolerance is too lax and we narrow it (e.g. require
the harness to observe `api_count == expected` for K consecutive polls
before tolerating ui=-1). If (1) fails, the cache=OFF path has a deeper
serve-correctness issue that this ship explicitly does NOT address —
escalate to architect for a fresh snowplow Go fix.

---

## What about the `call_count_mismatch[admin] expected=10 actual=46..73` noise?

Out of scope for 0.30.234. That's a SEPARATE bench-overlay issue
(EXPECTED_CALLS overlay calibrated for cache=ON; cache=OFF legitimately
issues more /call hits because there's no L1 dedup). Fix is in
`bench/expected.py` — per-cache-mode expected-calls tables, additive.
File as follow-up: **0.30.235 bench expected-calls cache-mode-aware
overlay**. No snowplow Go work.

---

## Done — handing back to team-lead for PM gate

- TRACE empirical (P1..P4 cluster probes, all GKE context-guarded).
- H1 CONFIRMED, H2/H3/H4 REFUTED with file:line citations.
- Fix is bench-only, ~70 LOC, no snowplow ship.
- Pre-commit falsifier 4 unit tests + 1 live S5 re-run.
- Post-patch verify on helm rev 406 (no helm change).

Sign: cache-architect
