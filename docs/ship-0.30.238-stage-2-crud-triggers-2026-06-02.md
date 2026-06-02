# Ship 0.30.238 — Stage 2: CRUD + RBAC delta re-seed triggers (FOLLOW-UP)

**Architect (peer reviewer)** | **Date**: 2026-06-02 | **Prereq**: 0.30.237 Stage 1 SHIPPED + Gate C PASSED

> Per PM scope-split mandate (docs/ship-0.30.237-stage-1-boot-parallel-f3-reapply-2026-06-02.md §3),
> Stage 2 is the **non-load-bearing** follow-up that adds widget-CR / RA-CR / RBAC-shift
> delta re-seed scopes. Stage 1 fixes Gate C (boot-time customer correctness) alone.
> Stage 2 is an admin-UX improvement: new widget CR / new RoleBinding → cohort cells
> stay seeded without waiting for the next pod restart.

## 1. PRECONDITION

Stage 2 does NOT ship until:
1. Stage 1 (0.30.237) deployed.
2. Stage 1 Gate C bench PASSED (`widgets|panels miss% ≤ 1%`, `markdowns ≤ 1%`, `buttons ≤ 1%`).
3. Stage 1 has been in production for ≥ 1 ship cadence cycle (≥ 24 h) with `resident_demote_total = 0`
   and `prewarm.engine.scope_incomplete` log count = 0.

If any precondition fails, Stage 2 is paused — the load-bearing fix is Stage 1; Stage 2 chases
a different defect (admin-CRUD freshness gap).

## 2. SYMPTOM Stage 2 fixes

Two related freshness gaps Stage 1 does NOT close:

**Gap A — admin creates a new widget CR**: until the next pod restart, the cohort cells
for THIS widget's GVR remain populated only with the OLD widgets present at boot. A
customer dispatch reaching the new widget pays one cold-fill (the per-user fallback Put
with `Pinned: true` covers it from then on, but the first /call is slow).

**Gap B — new RoleBinding added** (composition install): the cohort set for the bound
GVR changes; the engine's seeded cells were keyed against the OLD cohort set. The new
cohort's cell is empty until first /call. Stage 1's boot scope doesn't re-run.

Both gaps are bounded by the per-user-fallback `Pinned: true` Put (F3 from 0.30.236 retained
in HEAD); Stage 2 prevents the first cold-fill rather than tolerating it.

## 3. DESIGN

### 3.1 Scope kinds — extend `prewarm_engine.go`

```go
// internal/handlers/dispatchers/prewarm_engine.go (extend existing scopeKind enum)
const (
    scopeKindBoot      prewarmScopeKind = "boot"     // unchanged
    scopeKindWidgetCR  prewarmScopeKind = "widget-cr"  // NEW Stage 2
    scopeKindRACR      prewarmScopeKind = "ra-cr"      // NEW Stage 2
    scopeKindRBACShift prewarmScopeKind = "rbac-shift" // NEW Stage 2
)

type prewarmScope struct {
    kind prewarmScopeKind
    gvr  schema.GroupVersionResource
    ns   string
    name string
}

func (s prewarmScope) key() string {
    switch s.kind {
    case scopeKindBoot:
        return "boot"
    case scopeKindWidgetCR:
        return "widget-cr|" + s.gvr.String() + "|" + s.ns + "|" + s.name
    case scopeKindRACR:
        return "ra-cr|" + s.gvr.String() + "|" + s.ns + "|" + s.name
    case scopeKindRBACShift:
        return "rbac-shift|" + s.gvr.String()
    }
    return string(s.kind)
}
```

### 3.2 Affected-GVR aggregator — NEW helper in `bindings_by_gvr_delta.go`

The predecessor design assumed `onBindingChanged(affected []GVR)` exists. It does NOT.
Stage 2 adds a small new method on `bindingsByGVRIndex` that captures the affected GVR
set during the mutate path:

```go
// internal/cache/bindings_by_gvr_delta.go (NEW additions)

// applyBindingAddAndReturnAffected is applyBindingAdd that ALSO returns the
// set of GVRs whose cohort bucket changed. Used by the prewarm-engine hook
// to enqueue an rbac-shift scope for each affected GVR.
//
// The affected set is computed BEFORE + AFTER enrolment:
//   - Pre-set:  enumerated GVRs whose byGVR bucket already contained this id (none,
//     for a true ADD; for an UPDATE-via-onBindingUpdate this is the OLD bucket set).
//   - Post-set: enumerated GVRs whose byGVR bucket NOW contains this id.
// Affected = symmetric difference (pre XOR post).
// For a pure ADD: affected = post-set (everything is newly added).
//
// Callers: onBindingAdd, onBindingUpdate (for new), onBindingDelete (for old).
func (idx *bindingsByGVRIndex) applyBindingAddAndReturnAffected(
    namespace string, ref rbacv1.RoleRef, id bindingID, subjects []subjectKey,
) []schema.GroupVersionResource {
    snap := rbacSnap.Load()
    rules, ok := rulesForRoleRef(snap, namespace, ref)
    rk := roleRefKey(namespace, ref)

    idx.mu.Lock()
    defer idx.mu.Unlock()

    if !ok {
        // Unresolvable — only byRole touched. No GVR bucket affected.
        idx.entries[id] = bindingEntry{id: id, subjects: subjects}
        if rk != "" {
            set := idx.byRole[rk]
            if set == nil { set = map[bindingID]struct{}{}; idx.byRole[rk] = set }
            set[id] = struct{}{}
        }
        return nil
    }

    // enrolLocked iterates idx.navigated and checks rulesGrantGetList(rules, gr)
    // to decide which buckets to add to. Replicate the same iteration here to
    // build the affected list, then perform the actual enrol.
    var affected []schema.GroupVersionResource
    if rulesGrantWildcard(rules) {
        // wildcard touches all navigated GVRs
        for gr := range idx.navigated {
            affected = append(affected, schema.GroupVersionResource{
                Group: gr.group, Resource: gr.resource,
            })
        }
    } else {
        for gr := range idx.navigated {
            if rulesGrantGetList(rules, gr) {
                affected = append(affected, schema.GroupVersionResource{
                    Group: gr.group, Resource: gr.resource,
                })
            }
        }
    }
    idx.enrolLocked(bindingEntry{id: id, subjects: subjects}, rules, rk)
    return affected
}

// applyBindingDeleteAndReturnAffected is applyBindingDelete that ALSO returns
// the set of GVRs whose cohort bucket changed (i.e., GVRs that DID contain
// this binding before the delete).
//
// Implementation: pre-snapshot the byGVR membership for this id; unrol; the
// pre-snapshot IS the affected set.
func (idx *bindingsByGVRIndex) applyBindingDeleteAndReturnAffected(
    id bindingID, rk string,
) []schema.GroupVersionResource {
    idx.mu.Lock()
    defer idx.mu.Unlock()

    var affected []schema.GroupVersionResource
    for gr, bucket := range idx.byGVR {
        if _, ok := bucket[id]; ok {
            affected = append(affected, schema.GroupVersionResource{
                Group: gr.group, Resource: gr.resource,
            })
        }
    }
    // Wildcard is "all GVRs"; treat as affecting all navigated GVRs.
    if _, isWild := idx.wildcard[id]; isWild {
        for gr := range idx.navigated {
            affected = append(affected, schema.GroupVersionResource{
                Group: gr.group, Resource: gr.resource,
            })
        }
    }
    idx.unrolLocked(id, rk)
    return affected
}
```

The four existing mutator entry points (`onBindingAdd`, `onBindingUpdate`, `onBindingDelete`,
`onRoleObjectChanged`) call these new variants and then invoke the rbac-shift hook for each
affected GVR (dedup'd by the engine queue):

```go
// onBindingAdd (edit existing — replace applyBindingAdd call site)
func onBindingAdd(obj interface{}) {
    idx := bindingsByGVRSingleton()
    if !idx.deltaActive() { return }
    var affected []schema.GroupVersionResource
    if o, ok := asCRB(obj); ok {
        affected = idx.applyBindingAddAndReturnAffected("", o.RoleRef, crbBindingID(o), subjectsFromRBAC(o.Subjects))
    } else if o, ok := asRB(obj); ok {
        affected = idx.applyBindingAddAndReturnAffected(o.Namespace, o.RoleRef, rbBindingID(o), subjectsFromRBAC(o.Subjects))
    } else {
        deltaDropNonTyped("RoleBinding/ClusterRoleBinding(add)")
        return
    }
    // Stage 2 hook — bindings index has been updated; engine sees post-shift cohorts.
    fireRBACShiftHook(affected)
}
```

`onBindingUpdate` / `onBindingDelete` / `onRoleObjectChanged` are analogous (each gathers
its own affected set per the legacy mutate flow).

### 3.3 Hook indirection — `internal/handlers/dispatchers` registers, `internal/cache` calls

Mirrors `cache.SetCustomerInflightHook` pattern (registered in `main.go:318` per predecessor
trace §3.5).

```go
// NEW file: internal/cache/prewarm_hooks.go
package cache

import (
    "sync/atomic"

    "k8s.io/apimachinery/pkg/runtime/schema"
)

type widgetCRHookFn func(gvr schema.GroupVersionResource, ns, name string)
type raCRHookFn func(gvr schema.GroupVersionResource, ns, name string)
type rbacShiftHookFn func(affected []schema.GroupVersionResource)

var (
    widgetCRHook  atomic.Pointer[widgetCRHookFn]
    raCRHook      atomic.Pointer[raCRHookFn]
    rbacShiftHook atomic.Pointer[rbacShiftHookFn]
)

func SetWidgetCRHook(f widgetCRHookFn)  { widgetCRHook.Store(&f) }
func SetRACRHook(f raCRHookFn)          { raCRHook.Store(&f) }
func SetRBACShiftHook(f rbacShiftHookFn) { rbacShiftHook.Store(&f) }

func fireWidgetCRHook(gvr schema.GroupVersionResource, ns, name string) {
    if p := widgetCRHook.Load(); p != nil && *p != nil { (*p)(gvr, ns, name) }
}
func fireRACRHook(gvr schema.GroupVersionResource, ns, name string) {
    if p := raCRHook.Load(); p != nil && *p != nil { (*p)(gvr, ns, name) }
}
func fireRBACShiftHook(affected []schema.GroupVersionResource) {
    if p := rbacShiftHook.Load(); p != nil && *p != nil { (*p)(affected) }
}
```

### 3.4 Wiring — informer event hooks

```go
// internal/cache/deps_watch.go (edit AddFunc, UpdateFunc — DeleteFunc skipped since
// the existing Deps().OnDelete path already evicts the cell; no re-seed needed for delete).
AddFunc: func(obj interface{}) {
    // ...existing Deps().OnAdd + CRD side-effect...

    // Stage 2 — widget/RA CR ADD triggers a per-CR re-seed scope.
    ns, name := metaNSName(obj)
    if isHarvestedWidgetGVR(gvr) {
        fireWidgetCRHook(gvr, ns, name)
    } else if isRestActionGVR(gvr) {
        fireRACRHook(gvr, ns, name)
    }
},
UpdateFunc: func(_, newObj interface{}) {
    ns, name := metaNSName(newObj)
    Deps().OnUpdate(gvr, ns, name)
    if isHarvestedWidgetGVR(gvr) {
        fireWidgetCRHook(gvr, ns, name)
    } else if isRestActionGVR(gvr) {
        fireRACRHook(gvr, ns, name)
    }
},
```

`isHarvestedWidgetGVR(gvr)` reads the harvester's GVR set
(`navWidgetHarvester` already maintains a per-GVR index for `snapshot()`); extend with a
boolean lookup helper.

`isRestActionGVR(gvr)` compares against `restActionGVR` (already defined at
`internal/handlers/dispatchers/deps_extract.go`).

### 3.5 Scope handler — `makeScopeHandler` dispatch

```go
// internal/handlers/dispatchers/prewarm_engine_boot.go (rename makeBootScopeHandler → makeScopeHandler)
func makeScopeHandler(deps rePrewarmDeps) func(context.Context, prewarmScope) error {
    return func(ctx context.Context, s prewarmScope) error {
        switch s.kind {
        case scopeKindBoot:
            return rePrewarmBoot(ctx, deps)
        case scopeKindWidgetCR:
            return rePrewarmWidgetCR(ctx, deps, s)
        case scopeKindRACR:
            return rePrewarmRACR(ctx, deps, s)
        case scopeKindRBACShift:
            return rePrewarmRBACShift(ctx, deps, s)
        }
        return fmt.Errorf("unknown scope kind: %s", s.kind)
    }
}
```

### 3.6 Per-scope handlers

**`rePrewarmWidgetCR`** (~80 LOC, new file `prewarm_engine_widgetcr.go`):

```go
func rePrewarmWidgetCR(ctx context.Context, deps rePrewarmDeps, s prewarmScope) error {
    // 1. Fetch the widget object fresh.
    obj, err := objects.Get(ctx, templatesv1.ObjectReference{
        APIVersion: s.gvr.GroupVersion().String(),
        Kind:       kindForGVR(s.gvr),    // helper — existing kindForGVR or build from GVR
        Namespace:  s.ns,
        Name:       s.name,
    })
    if err != nil || obj.Unstructured == nil {
        // CR deleted between event + scope dequeue — Deps().OnDelete handled the eviction.
        return nil
    }

    // 2. Compute the cohort set for this widget's GVR (post-current-RBAC).
    cohorts := cache.EnumerateResourceCohorts(s.gvr)
    if len(cohorts) == 0 {
        return nil
    }

    // 3. Re-seed THIS widget across cohorts. Reuses seedScopeParallel by passing
    //    a single-element widgetEntries slice + empty restactionRefs.
    entry := navWidgetEntry{W: obj.Unstructured, GVR: s.gvr, /* tuples derived from obj */}
    return seedScopeParallel(ctx, nil, []navWidgetEntry{entry}, deps.saEP, deps.saRC, deps.authnNS)
}
```

**`rePrewarmRACR`** (~60 LOC, new file `prewarm_engine_racr.go`): symmetric. Builds a
single-element `restactionRefs` slice + empty `widgetEntries`.

**`rePrewarmRBACShift`** (~80 LOC, new file `prewarm_engine_rbacshift.go`):

```go
func rePrewarmRBACShift(ctx context.Context, deps rePrewarmDeps, s prewarmScope) error {
    cohorts := cache.EnumerateResourceCohorts(s.gvr)
    if len(cohorts) == 0 {
        return nil
    }

    // Re-seed every harvested widget + RA targeting this GVR across the new cohort set.
    // Stage 2 NEW helpers (do NOT exist today):
    widgetEntries := deps.navHarv.snapshotForGVR(s.gvr)
    restactionRefs := deps.harvester.snapshotForTargetGVR(ctx, s.gvr)
    return seedScopeParallel(ctx, restactionRefs, widgetEntries, deps.saEP, deps.saRC, deps.authnNS)
}
```

### 3.7 NEW helper code in harvesters (does not exist today)

`internal/handlers/dispatchers/phase1_content_prewarm.go` (extend `contentPrewarmHarvester`):

```go
// snapshotForTargetGVR returns the harvested RA refs whose target-GVR matches
// the argument. Used by Stage 2 rePrewarmRBACShift.
func (h *contentPrewarmHarvester) snapshotForTargetGVR(ctx context.Context, gvr schema.GroupVersionResource) []templatesv1.ObjectReference {
    if h == nil { return nil }
    h.mu.Lock()
    defer h.mu.Unlock()
    var out []templatesv1.ObjectReference
    for _, ref := range h.refs {
        // restActionTargetGVR re-derives via objects.Get; cache-hot.
        target, ok := restActionTargetGVR(ctx, ref)
        if !ok || target != gvr {
            continue
        }
        out = append(out, ref)
    }
    return out
}
```

`internal/handlers/dispatchers/phase1_pip_seed.go` (extend `navWidgetHarvester`):

```go
// snapshotForGVR returns the harvested widget entries with W.GVR matching gvr.
func (h *navWidgetHarvester) snapshotForGVR(gvr schema.GroupVersionResource) []navWidgetEntry {
    if h == nil { return nil }
    h.mu.Lock()
    defer h.mu.Unlock()
    var out []navWidgetEntry
    for _, e := range h.entries {
        if e.GVR == gvr {
            out = append(out, e)
        }
    }
    return out
}
```

### 3.8 main.go registration

```go
// main.go (near existing cache.SetCustomerInflightHook at line ~318)
cache.SetWidgetCRHook(func(gvr schema.GroupVersionResource, ns, name string) {
    prewarmEngineSingleton().enqueueScope(prewarmScope{kind: scopeKindWidgetCR, gvr: gvr, ns: ns, name: name})
})
cache.SetRACRHook(func(gvr schema.GroupVersionResource, ns, name string) {
    prewarmEngineSingleton().enqueueScope(prewarmScope{kind: scopeKindRACR, gvr: gvr, ns: ns, name: name})
})
cache.SetRBACShiftHook(func(affected []schema.GroupVersionResource) {
    for _, gvr := range affected {
        prewarmEngineSingleton().enqueueScope(prewarmScope{kind: scopeKindRBACShift, gvr: gvr})
    }
})
```

### 3.9 Files changed / created (Stage 2 only)

| File | Type | LOC | Purpose |
|---|---|---:|---|
| `internal/handlers/dispatchers/prewarm_engine.go` | Edit | ~30 | Add 3 scopeKinds + extend prewarmScope struct + key() |
| `internal/handlers/dispatchers/prewarm_engine_boot.go` | Edit | ~15 | Rename makeBootScopeHandler → makeScopeHandler + switch |
| `internal/handlers/dispatchers/prewarm_engine_widgetcr.go` | NEW | ~80 | rePrewarmWidgetCR + kindForGVR helper |
| `internal/handlers/dispatchers/prewarm_engine_racr.go` | NEW | ~60 | rePrewarmRACR |
| `internal/handlers/dispatchers/prewarm_engine_rbacshift.go` | NEW | ~80 | rePrewarmRBACShift |
| `internal/handlers/dispatchers/phase1_content_prewarm.go` | Edit | ~25 | snapshotForTargetGVR |
| `internal/handlers/dispatchers/phase1_pip_seed.go` | Edit | ~15 | snapshotForGVR on navWidgetHarvester |
| `internal/cache/prewarm_hooks.go` | NEW | ~60 | 3 hook indirections |
| `internal/cache/deps_watch.go` | Edit | ~15 | AddFunc + UpdateFunc hook calls |
| `internal/cache/bindings_by_gvr_delta.go` | Edit | ~80 | applyBindingAddAndReturnAffected + applyBindingDeleteAndReturnAffected + replace call sites in 4 onXxx funcs |
| `main.go` | Edit | ~15 | 3 hook registrations |
| **TOTAL** | | **~475** | |

(Larger than predecessor's claimed 200 LOC for Stage 2 because the affected-GVR aggregator
work was hidden in the predecessor design's imaginary `onBindingChanged`.)

## 4. FALSIFIER

**Falsifier — widget-CR scope fires within 5s**:

1. With Stage 2 deployed and Stage 1 boot complete:
   `kubectl apply -f <new-widget-cr-yaml>` (one widget under a harvested GVR).
2. Within 5 seconds, observe in pod logs:
   - `prewarm.engine.scope_processed scope=widget-cr|<gvr>|<ns>|<name>`
3. Verify `/debug/vars`:
   - `phase1_seed_widgets_total` incremented by `len(EnumerateResourceCohorts(gvr))`.

**Falsifier — RBAC-shift scope fires within 5s**:

1. `kubectl apply -f <new-rolebinding-yaml>` (one RB granting get/list on a harvested GVR).
2. Within 5 seconds:
   - `prewarm.engine.scope_processed scope=rbac-shift|<gvr>` (for each affected GVR).
   - `/debug/vars` `cohort_memo_entries_total` increments to reflect the new cohort cell.
3. Run customer /call for a user in the new binding — verify `dispatch_l1_lookups`
   shows a HIT (cohort cell already seeded by the rbac-shift scope).

**Falsifier — RBAC-shift on DELETE doesn't crash**:

1. `kubectl delete rolebinding/<name>` — the test rolebinding from previous step.
2. Within 5 seconds:
   - `prewarm.engine.scope_processed scope=rbac-shift|<gvr>` fires for the formerly-affected GVRs.
   - The re-seed pass skips cohorts that no longer enumerate (the cohort is GONE from the index).
3. Pod stays healthy; no panic, no scope-incomplete logs.

## 5. POST-DEPLOY GATE

Re-run Stage 1 Gate C bench at SCALE=5000 (regression check — Stage 2 must not break Stage 1).

Stage 2 adds:
- `phase1_seed_units_planned_total` incremented by per-scope seed counts (so the gate
  can verify "Stage 2 ran in addition to Stage 1 boot").

Specific Stage 2 assertions (`feedback_validate_content_not_just_status`):
- Single new widget CR added mid-bench → customer dispatch for that widget HITS L1 (no
  cold-fill miss-delta).
- Single new RoleBinding added mid-bench → customer dispatch for the new user HITS L1
  for the bound GVR.

## 6. RISK REGISTER

| Risk | Likelihood | Severity | Mitigation |
|---|---|---|---|
| RBAC-shift storm during composition install (`project_composition_install_rbac_scale`) | High | Med | Engine queue dedup coalesces `rbac-shift\|<gvr>` keys; a 5K-RB burst produces O(unique-affected-GVRs) scopes; per `feedback_customer_priority_over_refresher` the engine yields between scopes |
| Affected-GVR aggregator iterates idx.navigated under write lock | Med | Low | Iteration is O(|navigated|) ≈ 50 GVRs; lock held microseconds; pre-existing `enrolLocked` does the same iteration |
| New harvester helpers race with concurrent harvest (boot still running) | Low | Low | Harvester has `mu sync.Mutex`; helpers hold it for read; same shape as `snapshot()` |
| Widget-CR ADD on a GVR not in harvester (ad-hoc widget kind) | Low | Low | `isHarvestedWidgetGVR` returns false; hook fast-path skip |
| Deps().OnAdd + widget-CR-hook fire BOTH on the same event | Confirmed | Low | By design: OnAdd dirty-marks the LIST cell; widget-CR scope re-seeds the cohort-keyed widget cells. Both are correct and non-overlapping (different L1 key shapes) |
| RBAC-shift hook fires BEFORE the index update has propagated | Investigated | High | Hook is called AFTER `idx.unrolLocked`/`enrolLocked` returns under the write lock; subsequent `EnumerateResourceCohorts` reads the post-shift state. Verified by re-reading bindings_by_gvr_delta.go:131,151,176,203 |
| `prewarm_engine.go` `enqueueScope` not exported / not thread-safe | Low | High | enqueueScope is engine-internal; main.go is in the same module; the queue's lock + dedup map handle concurrency (same shape Stage 1 already uses for the boot scope) |
| The affected-GVR aggregator's wildcard expansion is O(|navigated|) per binding event | Low | Low | Wildcard bindings are rare (admin-only); the 50-GVR-wide expansion is a one-time per-event cost |
| Boot scope still running when widget-CR scope arrives | Med | Low | Engine queue serializes; widget-CR waits behind boot; boot's per-cohort 120s cap bounds the wait |

## 7. RESIDUAL REVERT PROBABILITY

Stage 2 risk model (independent peer reviewer assessment):

| Factor | Risk |
|---|---|
| Mechanism novelty | MED (new scope kinds + new aggregator + new hook indirections; no prior art for the affected-GVR derivation) |
| LOC size | MED (475 LOC across 11 files) |
| Falsifier coverage | HIGH (per-scope falsifiers are mechanical kubectl observations) |
| Recovery path if defect | MED (revert tag if scope handler crashes; engine queue dedup limits blast radius) |
| Latent defect surface | MED (RBAC-shift storm during composition install is the main risk; engine dedup is the only damper) |

**Independent residual revert probability estimate: ~30%.**

Stage 2 is genuinely riskier than Stage 1: NEW code surfaces (hook indirection, aggregator,
3 scope handlers), and the RBAC-shift path interacts with the composition-install storm
scenario. Stage 1's CPU yield contract for the engine is the safety net.

## 8. NOT IN SCOPE (Stage 3+ future ships)

- Per-user cohort first-encounter (`feedback_dynamic_cohort_prewarm_no_static_no_cold_fill`
  excludes lazy-fill-cold).
- Walker discovery of widgets via non-nav entry points.
- 1.5 GiB resident cap auto-bump.

These remain architectural follow-ups documented in the predecessor §4 G1/G2/G3.

## 9. REFERENCES

- `docs/ship-0.30.237-stage-1-boot-parallel-f3-reapply-2026-06-02.md` — load-bearing prereq
- `docs/ship-0.30.237-f4-prewarm-engine-trace-2026-06-02.md` — predecessor HYPOTHESIS
- `internal/cache/bindings_by_gvr_delta.go:131,151,176,203` — actual mutator entry points
  (REAL — not the imagined `onBindingChanged`)
- `internal/cache/bindings_by_gvr.go:301-343` — `enrolLocked` / `unrolLocked`
- `internal/cache/deps_watch.go:201,221` — informer ADD/UPDATE hook insertion sites
- `internal/handlers/dispatchers/phase1_content_prewarm.go:177` — current `snapshot()`
- `internal/handlers/dispatchers/phase1_pip_seed.go:209` — `navWidgetHarvester`
- `feedback_customer_priority_over_refresher`
- `feedback_no_park_broken_behind_flag`
- `feedback_dynamic_cohort_prewarm_no_static_no_cold_fill`
- `feedback_recurring_regression_pattern` Change A

— architect (peer reviewer), 0.30.238 Stage 2, 2026-06-02
