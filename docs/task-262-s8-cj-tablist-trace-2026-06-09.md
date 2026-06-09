# Task #262 — S8 cyberjoker widget-error TRACE (re-run on aligned-window data)

**Date:** 2026-06-09
**Author:** Architect
**Status:** TRACED; root cause + fix identified; safe bench-side fix
**Inputs:** sealed run `/tmp/snowplow-runs/0.30.250/run-20260609-150039/`, tester log `/tmp/phase6-0.30.250-cache-on-r2-150039.log`, live GKE cluster post-run (snowplow `0.30.250`, pod uptime 78m at trace time, 0 restarts)
**Budget:** 60–90 min — within budget

---

## 0. Executive summary

- The defect signature `cj_widget_error_count = 15` at S8 is **5 widget errors × 3 navs**, all of them HTTP errors on the per-card **Panel** widget `/call` (NOT on tablist `/call`).
- The Panel `/call` dispatcher-RBAC gate at `internal/handlers/dispatchers/widgets.go:91` **passes** (cj has `panels:get` from the S8 RB).
- The Panel falls through to a per-user (BindingUID-keyed) L1 miss and then to `widgets.Resolve`, which calls `resolveApiRef` → `objects.Get(panel.spec.apiRef)`. The apiRef target is a **RESTAction CR** (`templates.krateo.io: restactions`), and **cj is not granted `restactions:get`** by the S8 RoleBinding. The apiserver returns 403; `objects.Get` faithfully wraps it as `response.New(http.StatusForbidden, err)`; but `apiref.Resolve` then **strips the status code** with `fmt.Errorf("%s", res.Err.Message)` (`internal/resolvers/widgets/apiref/resolve.go:30`), and the dispatcher's `errors.As(err, *apierrors.StatusError)` check at `internal/handlers/dispatchers/widgets.go:228-234` **fails** (the wrapper is no longer a `StatusError`), so it lands in `response.InternalError` → **HTTP 500**, not 403.
- Probe A / Probe B PASS because they hit a different snowplow endpoint (the L1-snapshot view of cj's visible compositions), which only requires `composition.krateo.io: get,list` — already granted by the S8 RB.
- v4 of the S8 Role (Diego 2026-06-08, commit bc23163) added the `tablists` grant on the hypothesis that tablists were the cause. The grant was correct (cj needed it for click-nav anyway) but did NOT cause the 15 errors. **Tablists is fetched on click, not on render** — verified by reading `frontend/src/widgets/Panel/Panel.tsx`. The 15 errors at render time are entirely panel-apiRef-resolution failures.

**Fix:** add `("templates.krateo.io", ["restactions"], ["get", "list"])` to the S8 Role rules at `e2e/bench/bench/phases.py:1329-1339`. ~3 LOC. Falsifier: re-run Phase 6 cache-ON 50K → `proofs/S8.json` shows `content_card_present=True` AND `cj_widget_error_count=0`; tester reports `calls=30 http=30ok` for all three S8 navs.

**Hard procedural blocker hit during trace:** `pod_logs/S8.txt` (and S5..S10) contain zero snowplow log lines — they are 100% `--- STREAM RECONNECT ---` markers. The pod did NOT restart (78m uptime, 0 restarts confirmed via `kubectl describe`). The bench's per-stage log-tailing infrastructure broke after S2. The S8-window pod logs (13:44–13:50 UTC) are now lost to apiserver log rotation (earliest live log `14:10:14 UTC`). I worked the trace through tester-log + code + live cluster RBAC probes instead. **Surfaced separately** — bench log-capture fix is its own follow-up.

---

## 1. Evidence layer — what the sealed artifacts actually say

### 1.1 S8 proof (`proofs/S8.json`)

```
subject_user        = cyberjoker  (group "devs")
target_ns           = bench-ns-01
role_name           = bench-cyberjoker-bench-ns-01-comp-reader
rb_name             = bench-cyberjoker-bench-ns-01-comp-reader-binding
comps_in_ns         = 999
propagation_ok      = true   (81.498 s, rbac_publish_seq 6201 → 6393)
probe_a_pass        = true
probe_b_pass        = true
user_visible_count  = 999    (== expected_visible)
expected_card_name  = bench-app-01-02
content_card_present = FALSE          ← defect
cj_widget_error_count = 15            ← defect
__passed__           = false
```

### 1.2 Tester log — the 15-error breakdown

`/tmp/phase6-0.30.250-cache-on-r2-150039.log:324-332` records three S8 navs back-to-back. EVERY nav shows the SAME shape (TRACED):

```
[15:50:21]  [WARN] errored_widgets=5 at S8 ON nav#1 Compositions
[15:50:21]  [FAIL] call_count_mismatch[cyberjoker]:
              expected=30±0 actual=15 (n_visible=999) at S8 ON nav#1 Compositions
[15:50:21]    COLD Compositions  waterfall=0ms  load=162ms  calls=15  http=10ok/5err
[15:50:25]  [WARN] errored_widgets=5 at S8 ON nav#2 Compositions
[15:50:25]  [FAIL] call_count_mismatch[cyberjoker]: ... calls=15 http=10ok/5err
[15:50:30]  [WARN] errored_widgets=5 at S8 ON nav#3 Compositions
[15:50:30]  [FAIL] call_count_mismatch[cyberjoker]: ... calls=15 http=10ok/5err
```

Math (TRACED):
- expected = `BASE_STRUCTURAL(10) + COMP_PER_CARD_WIDGETS(4) × min(N_visible=999, COMP_DATAGRID_PER_PAGE=5)` = **30** (per `e2e/bench/bench/expected.py:44-86`).
- observed = 15 = **10 structural OK + 5 per-card ERR**.
- `.ant-result-error` DOM count = 5 per nav (`browser.py:1034` — TRACED). Three navs → 15 total → exactly matches `cj_widget_error_count = 15`.
- Critical inference (TRACED via `frontend/src/widgets/DataGrid/DataGrid.tsx:14-26`): the datagrid renders ONE `<WidgetRenderer>` per visible item; each `<WidgetRenderer>` issues ONE `/call?widget=panel-…-bench-app-01-NN`. **Each of the 5 visible cards fires its panel `/call` exactly once; all 5 fail.** The 3 inner per-panel widget calls (markdown + 2 buttons — see `frontend/src/widgets/Panel/Panel.tsx:131-141, 80-90`) **never fire**, because the SPA short-circuits on the panel's `!res.ok` (`frontend/src/hooks/useWidgetQuery.ts:53`) and renders `.ant-result-error` for the card.

So:
- "Which 15 widgets?" — **5 distinct panel widgets, each requested 3× across navs**:
  `githubscaffoldingwithcompositionpage-bench-app-01-{01,02,03,04,05}-composition-panel`
  (datagrid default `per_page=5`; alphabetical order). NOT 15 distinct widgets, not tablists.
- "Is it really 403?" — the apiserver-side error against the apiRef RESTAction IS a 403, but by the time it lands on the SPA wire it is **HTTP 500** with a generic message (the StatusError is stripped at the `apiref.Resolve` boundary; details in §3.3).

### 1.3 Probe A / Probe B pass — why this is not contradictory

`propagation_diag.probe_a_pass = true` and `propagation_diag.probe_b_pass = true` (with `user_visible_count = 999`) measure cj's **snowplow-side L1 listing of compositions**, NOT the per-card panel widget render. Those probes hit the snowplow `/v1/list` snapshot view of the dispatcher's per-cohort `widgets` L1 cell for the `compositions-list` Datagrid (cohort-keyed). The cohort cell is correctly RBAC-narrowed and shows the 999 compositions cj is now permitted to see. **The cohort cell does not depend on cj's `restactions:get` permission** — it is populated under SA by PIP and re-gated per requester via `gateWidgetEnvelope` (TRACED `widgets.go:134-164` + `widget_content.go:441-480`).

**The defect lives one level deeper, in the per-card widget render**, which is the path Probe A/B do not exercise.

### 1.4 Pod log capture was broken — PROCEDURAL BLOCKER #1

`/tmp/snowplow-runs/0.30.250/run-20260609-150039/pod_logs/S8.txt` is 19.8 KiB, 228 lines, **all of them `STREAM RECONNECT @ <ts> (pod restart / stream EOF)` markers** — zero actual snowplow log lines. Same for S5, S6, S7, S9, S10. S1+S2 captured 815 + 9279 genuine lines each, so the capture **WORKED early then broke around S5**.

The "pod restart" annotation is misleading: `kubectl --context=… -n krateo-system describe pod snowplow-75784b5fd8-g2vgn` reports `Restart Count: 0`, `Started: Tue, 09 Jun 2026 14:59:32 +0200` — the pod has been alive 78 minutes, all the way through S0–S10. So the EOFs are a `kubectl logs -f` / capture-side defect, not a pod crash. The bench's log-tailing harness needs investigation (out of scope of #262 — surfacing as a follow-up).

### 1.5 Live snowplow logs from S8 window — PROCEDURAL BLOCKER #2

K8s apiserver log retention rotated past the S8 window. Earliest available pod log is `14:10:14 UTC` (S8 ran `13:44–13:50 UTC`). I could not retrieve the actual `widgets.go` error lines for cj's panel `/call`. The trace relies on **code path identification** (§§2–3) + **live RBAC probes against the cluster** (§4); the conclusion is reproducible from those without the lost pod logs.

---

## 2. Datagrid → Panel → apiRef RESTAction render chain (TRACED)

### 2.1 What runs at S8 nav#1, in order

1. SPA navigates to `/compositions`. Browser issues 10 base structural `/call`s (page widget, navmenu items, the `compositions-list` Datagrid, etc.) — `expected.py:57 COMP_BASE_CALLS_STRUCTURAL = 10`. All 10 pass for cj because every base call is cohort-keyed and pre-warmed.

2. The `compositions-list` Datagrid resolves; its `status.resourcesRefs.items[]` contains one `endpoint` per visible composition, each pointing to a per-card Panel widget. cj's narrowed cohort cell shows the FIRST 5 cards (datagrid `per_page=5`): `bench-app-01-{01,02,03,04,05}`.

3. The SPA's `<DataGrid>` (`frontend/src/widgets/DataGrid/DataGrid.tsx:14-26`) renders 5 `<WidgetRenderer>` mounts, each firing `GET /call?…&resource=panels&namespace=bench-ns-01&name=…-composition-panel`.

4. Per panel `/call`, snowplow's `widgetsHandler.ServeHTTP` (`internal/handlers/dispatchers/widgets.go:50`) executes:

   - line 91 — `checkDispatchRBAC(ctx, gvr=panels, ns=bench-ns-01)`: cj has `panels:get` from the S8 RB (`phases.py:1336-1338`). **PASSES.**
   - line 134 — `isRBACSensitiveApiRefWidget(panel)`: panel has `spec.apiRef` AND `spec.widgetDataTemplate` of length 3 (verified live: `kubectl get panel … -o json`). Predicate (`widget_content.go:320-331`) returns **TRUE** → the identity-free `widgetContent` content L1 layer is **SKIPPED** (line 134 negated branch).
   - line 170 — `dispatchCacheLookupKey`: builds a per-user (BindingUID-keyed) cache key. BindingUID is derived per-request via `rbac.EvaluateRBAC(get, panels, bench-ns-01)` under cj's identity → returns the S8 RoleBinding's UID. PIP seeded the panel under the **SA's** BindingUID (different value) → **MISS** at line 184.
   - line 218 — falls through to `widgets.Resolve(ctx, …)`.

5. `widgets.Resolve` (`internal/resolvers/widgets/resolve.go:38`):
   - line 70 — `resolveApiRef(ctx, opts)` runs FIRST. The apiRef on this panel is `{name: …-composition-values, namespace: bench-ns-01, apiVersion: templates.krateo.io/v1, resource: restactions}` (verified live).
   - `apiref.Resolve` (`internal/resolvers/widgets/apiref/resolve.go:23-89`) calls `objects.Get(opts.ApiRef)` at line 28.

6. `objects.Get` (`internal/objects/get.go:28-154`) with cache-on + useInformer=true:
   - line 87 — `IsServable(gvr=restactions)` — pivot is registered. OK.
   - line 99 — `rw.GetObject(gvr, "bench-ns-01", "…-composition-values")` — informer holds the RA CR.
   - line 99 (same line) — `filterGetByRBAC(ctx, gvr=restactions, obj)` evaluates RBAC for cj's `get restactions` in `bench-ns-01`. **cj is NOT granted this** (see §4.1) → returns FALSE → the served-from-informer branch is **skipped**.
   - line 148 — falls through to `getFromAPIServer(ctx, ref)`.
   - `getFromAPIServer` (line 156) issues the GET under cj's own kubeconfig (per-user bearer token, line 167). The apiserver returns **403 Forbidden** for `get restactions/…-composition-values in bench-ns-01`.
   - line 210 — `apierrors.IsForbidden(err)` is true → `res.Err = response.New(http.StatusForbidden, err)`. **status code 403 is preserved here.**

7. **Status code stripping (the bug):** `apiref.Resolve` at `internal/resolvers/widgets/apiref/resolve.go:29-31`:

   ```go
   res := objects.Get(ctx, opts.ApiRef)
   if res.Err != nil {
       return map[string]any{}, fmt.Errorf("%s", res.Err.Message)
   }
   ```

   The 403 (carried inside `res.Err`, a `*response.Status` with `Code=403`) is **discarded**. Only the message string survives, wrapped in a plain `fmt.Errorf`. **No `*apierrors.StatusError` is constructed, no 403 propagates upward.**

8. `widgets.Resolve` (`resolve.go:72-76`) sees the plain error and returns it.

9. Back in the dispatcher, `internal/handlers/dispatchers/widgets.go:226-237`:

   ```go
   res, err := widgets.Resolve(ctx, …)
   if err != nil {
       log.Error("unable to resolve widget", slog.Any("err", err))
       var statusErr *apierrors.StatusError
       if errors.As(err, &statusErr) {
           code := int(statusErr.Status().Code)
           …
           response.Encode(wri, response.New(code, msg))
           return
       }
       response.InternalError(wri, err)   // ← HTTP 500
       return
   }
   ```

   `errors.As(err, &statusErr)` returns FALSE — the wrapper from step 7 is not a `*apierrors.StatusError`. Falls through to `response.InternalError(wri, err)` (`plumbing/http/response/encode.go:12-14`): **HTTP 500**, content-type `application/json`, body `{"code":500, "message":"...forbidden: restactions.templates.krateo.io …"}`.

10. Frontend `useWidgetQuery.ts:53` throws on `!res.ok`. React Query surfaces the failure to `<WidgetRenderer>` which renders `.ant-result-error` for that card. **bench's `errored_count = page.locator('.ant-result-error').count()` increments by 1.**

Repeat 5× per nav (5 visible cards) × 3 navs = **15 errors**. ✓ Matches `cj_widget_error_count = 15`.

### 2.2 Why tablists is a red herring (re-examined)

The S8 Role v4 grants `tablists` (`phases.py:1336-1338`). Tablists IS the click-nav target per `Panel.tsx:38-56` (`clickActionId: composition-click-action` → `composition-tablist`). It is **never requested at render time**. The 15 errors are emitted before any user click. The v4 grant was correct (cj needs it for click-through) but is unrelated to render-time errors.

The `phases.py:1316-1318` comment explicitly states the prior task-215 attribution: *"5 cards × 1 tablist 403 × 3 navs"*. The COUNT factorization is correct (5 × 1 × 3); only the WIDGET class was wrong (panel, not tablist). The shape `5 × 1 × 3` is structural — 5 cards on page 1, ONE failed widget per card (whichever is fetched first and short-circuits the rest), 3 navs in the S8 measurement.

---

## 3. Why probes pass but content_card_present fails (architectural gap)

### 3.1 What probe A / B actually hit

`_wait_rbac_propagation_to_snowplow` (`phases.py`, called at line 1374) runs two probes. Both target snowplow's **L1 listing** of compositions visible to cj — the cohort cell for the Compositions Datagrid + the per-user view of `compositions.composition.krateo.io`. These both:

- Pass through the dispatcher's `checkDispatchRBAC(get, compositions, *)` gate (granted by `phases.py:1330-1331`, `composition.krateo.io: *`).
- Hit the cohort-shared `widgets` L1 cell that PIP populated under SA, then `gateWidgetEnvelope` re-narrows the embedded `resourcesRefs.items[].allowed` flags per cj.
- **Do not require cj to resolve `restactions:get`**, because the cohort cell already contains the SA-resolved apiRef expansion baked in. The probe only reads `user_visible_count` from that cell.

So `user_visible_count = 999 = expected_visible` is correct: cj's snowplow-side L1 view of the 999 compositions in bench-ns-01 IS populated and IS RBAC-correct.

### 3.2 What `content_card_present` checks

`_user_card_text_present` (`phases.py:1102-1135`) drives the live Playwright page and does `page.locator(f"text={card_name}").count() >= 1`. The card name (`bench-app-01-02`) is only present in the DOM if **the per-card Panel widget rendered**. The Panel renders the title from `widgetData.title` (`Panel.tsx:108-125`), and `widgetData.title` only exists in the response body when `widgets.Resolve` succeeds. With `widgets.Resolve` returning an error (HTTP 500), the SPA renders `.ant-result-error` instead — the card title `bench-app-01-02` never lands in the DOM → `content_card_present = False`.

### 3.3 The architectural gap

The dispatcher and `apiref.Resolve` boundaries are inconsistent on **error type discipline**:

- `objects.Get` (`get.go:209-214`) faithfully preserves the apiserver's 403 by wrapping it in `response.New(http.StatusForbidden, …)`.
- `apiref.Resolve` (`apiref/resolve.go:29-31`) **discards** that status code by re-wrapping in a plain `fmt.Errorf("%s", res.Err.Message)`.
- The dispatcher's downstream `errors.As(err, *apierrors.StatusError)` (`widgets.go:228`) can only recover a status code if the underlying error is an `*apierrors.StatusError`. The `response.Status` wrapper from step 7 is neither.

Result: any apiRef-resolution error becomes HTTP 500, regardless of the apiserver's actual response code. The frontend cannot distinguish "you lack permission" from "snowplow exploded". The user sees a generic `.ant-result-error`.

This is **also** the same pattern documented in `docs/task-267-s6-admin-2-widget-silent-skip-trace-2026-06-09.md:169-177` (admin Dashboard widget silent skip): apiserver error inside widgets.Resolve → wrapped plainly → 500. That task closed the symptom by removing the apiserver call entirely (paged LIST, Ship A.1 / `0.30.250`); the status-code-stripping defect was never fixed in-place.

---

## 4. Live RBAC verification on the cluster (TRACED)

### 4.1 Current cj/devs cluster permissions on bench-ns-01

(Post-S9: the S8 RoleBinding was removed; cj is back to baseline.)

```
kubectl --context=gke_neon-481711_us-central1-a_cluster-1 \
  auth can-i get restactions -n bench-ns-01 --as=cyberjoker --as-group=devs
→ no   (exit 1)
```

The TWO ClusterRoleBindings touching `Group devs` (TRACED via `kubectl get clusterrolebinding`):

| CRB | ClusterRole | Grant |
|---|---|---|
| `devs-get-list-any-customresourcedefinition-in-cluster` | same | `get,list customresourcedefinitions` cluster-wide |
| `devs-list-any-namespace-in-cluster` | same | `list namespaces` cluster-wide |

Neither grants any `templates.krateo.io: restactions`. The ONLY ClusterRole on the cluster that grants `restactions` to a non-SA subject is `authn-group-krateo-system` (binds `Group authn`, not `Group devs`).

### 4.2 What the bench S8 RB grants (vs. what's needed)

`e2e/bench/bench/phases.py:1325-1340` — current v4 rules:

```python
rules=[
    (["composition.krateo.io"], ["*"], ["get", "list"]),
    (["widgets.templates.krateo.io"],
     ["panels", "markdowns", "buttons", "tablists"],
     ["get", "list"]),
]
```

**Missing rule:** `(["templates.krateo.io"], ["restactions"], ["get", "list"])`.

This is exactly the GVR group + resource the per-card Panel widget's `spec.apiRef` resolves under (verified live: `apiRef.namespace = bench-ns-01`, `apiRef.name = …-composition-values`; the CR is a `restactions.templates.krateo.io/v1`).

### 4.3 Why admin succeeds at this same chain (sanity)

Admin holds cluster-wide `*/*` access (the cluster's `cluster-admin` CRB, standard portal practice). Admin's `objects.Get(panel.spec.apiRef)` returns the RA, the apiRef resolves, the panel renders, no error. The defect is **cj-narrow-RBAC specific**.

---

## 5. Why this only fails at S8 (not S0..S7, not S9)

| Stage | Subject | Bench Role granted | Visible compositions for cj | Panels rendered for cj | apiRef chain exercised | Result |
|---|---|---|---|---|---|---|
| S0..S5 | admin only | n/a | n/a (admin cluster-wide) | n/a | admin's `*/*` covers it | PASS |
| S4–S6 | admin only | n/a | admin's cluster-wide view | admin renders | admin's `*/*` covers it | PASS |
| S7 | admin deletes 1 comp | n/a | admin's cluster-wide view | admin renders | admin's `*/*` covers it | PASS |
| **S8** | **cj + new RB granting comp+panel+md+btn+tablist (NO restactions)** | **5 cards × 1 panel render each** | **render fails (5×3=15 errors)** | **apiRef RA fetch DENIED** | **FAIL** |
| S9 | cj loses RB | RB removed → 0 visible | datagrid empty → 0 cards rendered | no fan-out → no apiRef chain | PASS |

S9 passes because `n_visible = 0` → `expected_calls = 10 + 4×0 = 10` → no per-card render → no apiRef chain to fail on. The chain is **broken only when cj has cards to render**.

---

## 6. Hypotheses tested and falsified

These I considered before landing on the apiRef-RBAC root cause, to be transparent on which paths were ruled out:

| Hypothesis | Test | Result |
|---|---|---|
| H-A: tablists 403 (per phases.py:1316 comment) | `kubectl auth can-i list tablists --as=cj …` post-fix; trace Panel.tsx render order | FALSIFIED — tablists is fetched on click, not render; v4 grant covers it anyway. |
| H-B: cohort-cell BindingUID divergence between PIP seed and cj's serve | Read `ComputeKey` (resolved.go:600-647); confirm `BindingUID` is folded for `widgets` class | CONFIRMED as part of the chain (PIP cell never serves cj), but ALONE does not cause an error — cj would fall through to `widgets.Resolve` which would succeed if the apiRef were resolvable. So this is a contributory factor (forces a cold resolve) but not the defect root. |
| H-C: snowplow returns a 403 directly | Read `widgets.go:226-237` + `apiref.Resolve` + `apierrors.IsForbidden` + `response.InternalError` | FALSIFIED for the wire — the apiserver returns 403, but `apiref.Resolve` strips it to a plain error; dispatcher emits HTTP 500. The "403" in task title is the underlying cause, not the wire status. |
| H-D: gateWidgetEnvelope drops items rather than flags them | Read `widget_content.go:441-480` + design header at line 420-440 | FALSIFIED — flag-not-drop by design; explains why probes pass while render fails. |
| H-E: cohort-cell is mis-populated for cj | Probe A/B PASS shows `user_visible_count = 999` | FALSIFIED — cohort cell is correctly RBAC-narrowed for cj. |

---

## 7. Fix

### 7.1 Recommended fix — bench-side, minimal (3 LOC)

Add the missing `restactions` rule to the S8 Role in `e2e/bench/bench/phases.py:1329-1339`:

```python
rules=[
    (["composition.krateo.io"], ["*"],
     ["get", "list"]),
    (["widgets.templates.krateo.io"],
     ["panels", "markdowns", "buttons", "tablists"],
     ["get", "list"]),
    # NEW: panel.spec.apiRef → templates.krateo.io/restactions/…-composition-values.
    # Without this, widgets.Resolve fails on objects.Get(apiRef) → apiserver 403,
    # silently downgraded to HTTP 500 at widgets.go:236 → 5 .ant-result-error
    # cards per nav → 15 widget errors total.
    (["templates.krateo.io"], ["restactions"], ["get", "list"]),
]
```

Update the comment block above (`phases.py:1300-1324`) to reflect the v5 re-gate ("v5 / 2026-06-09 adds restactions per architect trace task #262") and remove the prior tablist-as-cause attribution.

Same companion update in `e2e/bench/bench/tests/test_cluster.py` if the test fixture mirrors the production rule shape.

### 7.2 Falsifier (acceptance criterion)

Re-run Phase 6 cache-ON at SCALE=50000 on tag carrying the fix:
- `proofs/S8.json`: `content_card_present = True`, `cj_widget_error_count = 0`, `__passed__ = true`.
- Tester log for S8: three navs each show `calls=30  http=30ok` (10 base + 4×5 fan-out, no errors).
- Tester log for S9: unchanged from today (`calls=10 http=10ok`).
- Stages_completed includes "S8".

### 7.3 NOT recommended: in-place fix of the status-code-stripping defect

Fixing `apiref.Resolve` to preserve the 403 (e.g. constructing an `apierrors.StatusError` with the preserved code) is correct hygiene and would let snowplow emit a real 403 instead of a 500 on this chain. **But** doing it in scope of #262 would:

1. Change wire shape for an in-flight ship loop (a class of widget errors that currently report 500 would start reporting 403). Existing tests, browser code, observability dashboards may assume the current shape.
2. Mask, not fix, the real defect — which is that cj's customer-side RBAC blueprint forgot a GVR. In production the customer would also see 500s (today's shape) until they grant `restactions`.

Recommend separating the status-code-discipline fix into its own follow-up ship (would also benefit the admin S6 chain in `task-267`).

### 7.4 Follow-up — bench log-capture (separate, blocks future traces)

The S5+ pod-log capture is broken (§1.4). Filing as a separate follow-up rather than mixing into the #262 fix.

---

## 8. Citations index

| Claim | File:line |
|---|---|
| widgetsHandler.ServeHTTP entrypoint | `internal/handlers/dispatchers/widgets.go:50` |
| checkDispatchRBAC gate | `internal/handlers/dispatchers/widgets.go:91`; `helpers.go:93-126` |
| isRBACSensitiveApiRefWidget predicate | `internal/handlers/dispatchers/widget_content.go:320-331` |
| Per-user (BindingUID) L1 lookup | `internal/handlers/dispatchers/widgets.go:170`; `helpers.go:197-240` |
| widgets.Resolve entrypoint | `internal/resolvers/widgets/resolve.go:38` |
| resolveApiRef → apiref.Resolve | `internal/resolvers/widgets/resolve.go:70`; `internal/resolvers/widgets/apiref/resolve.go:23-89` |
| **Status code stripping defect** | `internal/resolvers/widgets/apiref/resolve.go:29-31` |
| objects.Get + filterGetByRBAC | `internal/objects/get.go:28-154` (esp. lines 87, 99, 148) |
| objects.Get apiserver 403 wrap | `internal/objects/get.go:200-217` |
| **Dispatcher error → HTTP 500** | `internal/handlers/dispatchers/widgets.go:226-237`; `plumbing/http/response/encode.go:12-14` |
| ComputeKey folds BindingUID for widgets class | `internal/cache/resolved.go:600-647` |
| gateWidgetEnvelope (flag-not-drop) | `internal/handlers/dispatchers/widget_content.go:441-480` |
| Panel React renders items+footer only | `frontend/src/widgets/Panel/Panel.tsx:131-141, 80-90` |
| Datagrid React fans 1 WidgetRenderer/item | `frontend/src/widgets/DataGrid/DataGrid.tsx:14-26` |
| Frontend throws on !res.ok | `frontend/src/hooks/useWidgetQuery.ts:53` |
| .ant-result-error count is bench errored_count | `e2e/bench/bench/browser.py:1034` |
| Expected_calls formula | `e2e/bench/bench/expected.py:44-86` |
| S8 Role rules (current v4) | `e2e/bench/bench/phases.py:1325-1340` |
| Panel.spec.apiRef + widgetDataTemplate (live verification) | `kubectl get panel … -o json` against `bench-ns-01` |
| cj/devs cluster RBAC (live verification) | `kubectl auth can-i …` against `gke_neon-481711_us-central1-a_cluster-1` |
