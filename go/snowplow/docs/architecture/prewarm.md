# Prewarm

How snowplow makes the *first* page-load warm: it replays the frontend's own
navigation under the service-account identity at boot, populates the informer
set and the per-user L1 cache, and keeps those cells fresh as objects change —
without ever hardcoding a navigation path, a cohort list, or a GVR.

This is a maintainer deep-dive. It leads with the flow, then traces the named
files at `file:line`, then states the invariants and the known failure modes.
Every claim below is verified against the tree at commit `a42b072`.

---

## ASCII flow

```
 BOOT (Phase1Warmup → phase1WarmupWith, internal/handlers/dispatchers/phase1_walk.go)
 ┌──────────────────────────────────────────────────────────────────────────────┐
 │ Step 1  RegisterMetaQuerySeeds()           ── the ONLY hardcoded GVRs at boot  │
 │           (cache/phase1.go:277 — bare meta-query anchors, no business GVR)     │
 │ Step 3  lister(ctx)  ── READ nav roots from the frontend ConfigMap            │
 │           config.json .api.INIT + .api.ROUTES_LOADER → 2 widget CRs           │
 │           (phase1_roots.go:125 listNavigationRootsFromConfigMap)              │
 │ Step 4  for each root:  resolveNavigationRoot → phase1Walker.walk()           │
 │           recursive replay of frontend nav (phase1_walk.go:1116)              │
 │             • harvestApiRef + harvestNavWidget on every walked widget         │
 │             • widgets.Resolve → status.resourcesRefs.items[]                  │
 │             • recurse ONLY items where verb=="GET"  (walkShouldRecurse,       │
 │               phase1_walk.go:1480) — load-bearing read-only gate              │
 │             • lazyRegister + DiscoverGroupResources register informers        │
 │ Step 6  settleRegisteredSet  ── let the discovered informer set stop growing  │
 │ Step 7  WaitAllInformersSynced  ── the SYNC BARRIER (cache/phase1.go:297)     │
 │ Step 7.5 content pre-warm + cluster_list pre-warm   (behind 503 readiness)    │
 │ Step 8  MarkPhase1Done()        ── /readyz flips 200 (phase1_walk.go:720)     │
 └───────────────────────────────────────────────┬──────────────────────────────┘
                                                  │  (BACKGROUND, post-Ready)
                                                  ▼
 SEED — one of two paths (PREWARM_ENGINE_ENABLED selects):
   ┌─ engine (default-off flag ON)  ─ StartPrewarmEngine → worker → rePrewarmBoot
   │     ① RE-WALK both roots, FRESH visited map, AFTER the sync barrier
   │        (so the dynamic navmenu children — empty pre-sync — are now present)
   │     ② settleRegisteredSet  ③ BuildBindingsByGVRIndex over navigated GVRs
   │     ④ seedScopeYielding: per-binding TARGETS from the live index
   │        (cache.EnumeratePrewarmTargetsForGVR), seeds per-user L1 cells,
   │        yields to in-flight customer /call between every target
   └─ legacy runPIPSeed (flag OFF) ─ same harvested set, global cohort enumeration

 COHORT MODEL: one dynamic engine. Targets come from the LIVE BindingsByGVR
   index per (GVR, verb) — NO static cohort list, and when the index yields
   nothing for a GVR the seed SKIPS it (NO global fallback, NO lazy cold-fill).
   A skipped/runtime-discovered target is resolved cold-then-warm at first /call.

 STEADY STATE — CRUD-triggered re-prewarm:
   informer ADD/UPDATE ─ DepTracker.onChange → dirty-mark dependent L1 keys
                          → refresher RE-RESOLVES (never evicts)
   informer DELETE     ─ self entry evicted; LIST/dependent deps dirty-marked
   new GVR discovered  ─ cache GVR-discovered hook → engine scopeKindGVRDiscovered
                          → rePrewarmBoot re-walk (records the new dep edge)
```

---

## Trace

### Boot seed — the only hardcoded GVRs

`phase1WarmupWith` (`internal/handlers/dispatchers/phase1_walk.go:594`) is the
boot orchestrator. **Step 1** is the entire "boot seed":
`rw.RegisterMetaQuerySeeds()` (`phase1_walk.go:605`). That call
(`internal/cache/phase1.go:277`) registers only `MetaQuerySeeds()` —
described in its own log line as *"bare meta-query anchors only — every business
GVR is discovered by resolution"* (`cache/phase1.go:292`). Ship 0/0.5
(`phase1_walk.go:598-616`) deleted the `customresourcedefinitions` seed and the
CRD informer entirely; composition GVRs are now discovered as a synchronous
side-effect of the walk (`cache.DiscoverGroupResources`), not pre-seeded.

### Phase-1 walker roots — read from the frontend ConfigMap, not hardcoded

The roots are NOT `navmenus` / `routesloaders` literals. **Step 3**
(`phase1_walk.go:621`) calls the lister, which is
`listNavigationRootsFromConfigMap` (`phase1_roots.go:125`). It:

1. Reads the frontend ConfigMap — name from env `FRONTEND_CONFIG_CONFIGMAP`
   (default `frontend-config-vars`, `phase1_roots.go:66-71,101-106`), namespace
   = `AUTHN_NAMESPACE` (`phase1_walk.go:333`).
2. Parses its `config.json` key into `frontendConfig`, reading the two fields
   `.api.INIT` and `.api.ROUTES_LOADER` (`phase1_roots.go:90-95`,
   `readFrontendConfig` at `:235`). These are the two `/call?...` URLs the
   frontend itself dispatches on login.
3. Decodes each URL into an `ObjectReference` via
   `objects.ParseCallPathToObjectRef` (`phase1_roots.go:166`) — the same generic
   `/call` decoder the recursive walk uses; not a path special-case.
4. Fetches each named root widget CR via `objects.Get` and returns
   `navigationRoot{Root, GVR}` pairs (`phase1_roots.go:208-230`).

The strings `navmenus`/`routesloaders` appear **nowhere** as Go literals driving
root selection (`phase1_roots.go:24-27`). If the frontend changes its INIT
widget, Phase 1 follows with zero Go change. A missing/unparseable ConfigMap is
**degraded, not fatal**: it returns an error, and the warmup still runs the sync
barrier and flips readiness (`phase1_walk.go:622-633`) — lazy
register-on-navigation covers every GVR on the first real request.

### The recursive walk — replaying navigation, GET-only

**Step 4** (`phase1_walk.go:640-666`) calls `resolve(ctx, root)` per root, which
threads into `phase1Walker.walk()` (`phase1_walk.go:1116`). Per walked widget:

- `harvestApiRef(in)` + `harvestNavWidget(in, gvr, …)` collect the widget's
  apiRef RESTAction and the widget CR + its (GVR, pagination) tuple into the two
  shared harvesters the seed later drains (`phase1_walk.go:1138,1152`).
- `widgets.Resolve` runs the widget under the SA identity; its inner-call walk
  auto-registers an informer per touched GVR and invokes
  `cache.DiscoverGroupResources` for templated apiserver paths
  (`phase1_walk.go:1201-1207`, Step-4 comment `:640-644`).
- It reads `status.resourcesRefs.items[]` — the child widget endpoints —
  via `extractResourcesRefsItems` (`phase1_walk.go:1287,1489`).

**The recursion gate is `verb == "GET"` only.** Before descending into a child,
the loop calls `walkShouldRecurse(child)` (`phase1_walk.go:1331`), which is
exactly:

```go
func walkShouldRecurse(child navChildRef) bool {
    return strings.EqualFold(child.Verb, "GET") && child.Path != ""
}
```

(`phase1_walk.go:1480-1482`). This is load-bearing for both correctness and
safety (`phase1_walk.go:1426-1435`): a non-GET `resourcesRefs` item is a
mutation/action endpoint (POST/PUT/PATCH/DELETE) bound to a widget's `actions`,
and because the walk runs with the SA's *privileged* credentials, following one
would issue a destructive apiserver mutation. The `allowed` RBAC flag is
**deliberately not** a recursion gate (`phase1_walk.go:1437-1467`): Phase 1 is
identity-independent informer *discovery*; the per-user `allowed` render gate is
applied later, at real request time. Recursion bounds are only the visited-set
(`phase1_walk.go:1342-1350`) and `phase1MaxWalkDepth` (`:1124`); child fan-out is
bounded by the declared `slice` or `prewarmPageLimit()` (`:1367`).

### The sync barrier and readiness

After the walk, `settleRegisteredSet` (Step 6, `phase1_walk.go:681`) waits for
the discovered informer set to stop growing, then `WaitAllInformersSynced`
(Step 7, `phase1_walk.go:686` → `cache/phase1.go:297`) blocks until every
registered informer `HasSynced` AND no new informer appeared during the wait
(the re-snapshot loop, `cache/phase1.go:306-319`). `MarkPhase1Done()`
(Step 8, `phase1_walk.go:720`) flips `/readyz` to 200 — *before* the per-cohort
seed, so boot wall-clock is cohort-count-independent (`phase1_walk.go:710-720`).

### The seed — one dynamic engine

The seed runs **after** `MarkPhase1Done`, on a bounded background goroutine
(`phase1_walk.go:739-762`), and never withholds readiness. Two implementations,
selected by `PREWARM_ENGINE_ENABLED` (default `"false"`,
`prewarm_engine.go:76-83`); the flag is the documented one-knob back-out
(`phase1_walk.go:477-484`).

When the engine is on (and the PIP harvesters exist), `engineSeed`
(`phase1_walk.go:419-474`) calls `StartPrewarmEngine`
(`prewarm_engine.go:317`) and enqueues exactly one `scopeKindBoot`
(`phase1_walk.go:455`). The worker (`prewarm_engine.go:382`) yields to any
in-flight customer `/call` before each scope (`yieldToCustomer`,
`prewarm_engine.go:437`) and runs `rePrewarmBoot` (`prewarm_engine_boot.go:182`):

1. **RE-WALK both roots with a FRESH `phase1Walker` per root** (new visited map,
   `prewarm_engine_boot.go:222-229`). This is the boot-race fix
   (`prewarm_engine.go:24-31`): the Step-4 walk runs *before* the sync barrier
   and is single-pass, so the navmenu's *dynamic* children
   (`resourcesRefsTemplate` over the apiRef) resolve to 0 while fallthrough data
   is empty. The re-walk runs *after* the barrier, when the data is present, so
   the full nav tree is discovered and the harvesters are populated. Reusing the
   old visited map would short-circuit and descend nothing
   (`prewarm_engine_boot.go:211-213`).
2. `settleRegisteredSet` once (`prewarm_engine_boot.go:243`).
3. `cache.BuildBindingsByGVRIndex(rw.RegisteredGVRs())`
   (`prewarm_engine_boot.go:249-250`) — the cohort-scoping substrate, built over
   the *navigated* GVRs.
4. `seedScopeYielding` (`prewarm_engine_boot.go:279,333`) drains the harvested
   restactions + widgets and seeds the per-user top-level L1 cells.

### The cohort model — dynamic, no static list, no cold-fill

`seedScopeYielding` does **not** iterate a cohort list. For each harvested
widget it scopes on the widget's GVR; for each harvested RESTAction it scopes on
the RA's *target* GVR derived from its `userAccessFilter`
(`restActionTargetGVR`, `prewarm_engine_boot.go:498-530`). It then asks the
**live index** for the per-binding targets via
`enumeratePrewarmTargetsForGVRFn` → `cache.EnumeratePrewarmTargetsForGVR(gvr,
"list")` (`prewarm_engine_boot.go:393` → `cache/prewarm_enumeration.go:91`). Each
authorising RBAC binding yields exactly one `PrewarmTarget`
(`prewarm_enumeration.go:60-65,129-135`); the representative subject identity is
drawn from the binding's subjects.

Three "no static / no cold-fill" facts, verified in code:

- **No static cohort list.** Targets are read from `BindingsByGVR` per snapshot;
  there is no Go-literal cohort set (`prewarm_engine_boot.go:32-35`,
  `prewarm_enumeration.go:22-35`).
- **No global fallback.** The v3 `EnumerateBindingSetClasses()` global-cohort
  fallback was **removed** (`prewarm_engine_boot.go:303-310`). When the per-GVR
  enumerator returns empty (no authorising binding, or the target GVR is
  runtime-discovered with no static literal), the seed **skips** that GVR rather
  than widening to a universe of identities that can't authorise it
  (`targetsFor` returns `nil` at `prewarm_engine_boot.go:389-396`;
  `prewarm_enumeration.go:75-83`).
- **No lazy cold-fill.** A skipped target is not back-filled by a background
  job; runtime-discovered RA targets are explicitly *"resolved cold-then-warm
  via the on-demand dispatcher"* at first `/call`
  (`prewarm_engine_boot.go:311-314`).

Customer priority is enforced throughout the seed: `engineYieldCheckpoint(ctx)`
runs between every target (`prewarm_engine_boot.go:418,430,460`), so a customer
burst arriving mid-seed defers the remaining work.

### CRUD-triggered re-prewarm

Two distinct mechanisms keep prewarmed cells correct as the cluster changes:

1. **Existing cells stay fresh via the refresher (object CRUD).** An informer
   ADD/UPDATE event calls `DepTracker.OnAdd`/`OnUpdate` →
   `onChange` (`internal/cache/deps.go:504-558`), which dirty-marks every
   dependent L1 key (exact-object deps + LIST-scope deps) into the refresher.
   The refresher **re-resolves** the entry — never evicts on ADD/UPDATE
   (`deps.go:500-501,520-523`; `internal/cache/refresher.go:4-7`). DELETE evicts
   only the deleted object's own self entry and dirty-marks its dependents
   (`refresher.go:711-716`). This is the steady-state "re-prewarm" of cells the
   boot seed populated.
2. **New navigation structure via the engine (CRD/GVR CRUD).** When a genuinely
   new GVR is first registered post-boot, the cache fires its GVR-discovered
   hook (`cache.RegisterGVRDiscoveredHook`), which the engine wires at start
   (`registerEngineGVRDiscoveredHook`, `prewarm_engine_boot.go:112-119`) to
   enqueue a `scopeKindGVRDiscovered` scope (`prewarm_engine.go:151`). That scope
   runs the **same** `rePrewarmBoot` core (`prewarm_engine_boot.go:157-177`):
   re-walking the nav tree with a fresh visited set and re-reading the
   now-widened index records the dep edge against the new GVR, so subsequent CR
   events propagate via the normal `onChange` → dirty-mark → refresher path. The
   queue dedups distinct GVRs as distinct work and coalesces repeats
   (`prewarm_engine.go:185-190`).

The placeholder scope kinds `scopeKindWidgetCR` / `scopeKindRBACShift` are
**declared but not wired** this ship (`prewarm_engine.go:153-157`): a
widget/RESTAction *CR* add/update/delete does not yet enqueue an engine re-walk
of its subtree. Today such changes are caught by the refresher (for already-cached
dependent keys) and by lazy resolve-on-navigation at `/call`.

---

## Invariants

- **No special cases / no hardcoded navigation.** The only hardcoded GVRs at
  boot are the meta-query seeds (`cache/phase1.go:277-294`). Navigation roots,
  the cohort set, and every business GVR are read from config / the live RBAC
  index / the walk — never Go literals (`phase1_roots.go:24-27`,
  `prewarm_engine_boot.go:32-35`).
- **The walk is strictly read-only.** Recursion is gated on `verb == "GET"`
  alone (`walkShouldRecurse`, `phase1_walk.go:1480`); the SA's privileged
  credentials never reach a mutation endpoint.
- **Discovery is identity-independent; rendering is not.** `allowed` is never a
  recursion gate during prewarm (`phase1_walk.go:1437-1467`). The per-user
  `allowed` render verdict is re-derived at request time — prewarmed
  SA-evaluated `allowed=true` flags are never served verbatim
  (`phase1_walk.go:1227-1234`).
- **Readiness precedes the seed.** `MarkPhase1Done` flips `/readyz`
  *before* the per-cohort seed (`phase1_walk.go:710-720`); boot wall-clock is
  cohort-count-independent and the seed can never make a pod not-Ready.
- **The re-walk MUST use a fresh visited map and the same `walk()`.** Reusing
  the boot pass's visited set descends nothing
  (`prewarm_engine.go:18-31`, `prewarm_engine_boot.go:211-213`).
- **Customer `/call` has absolute priority.** Both the engine worker and the
  steady-state refresher yield on `customerInFlight()`
  (`prewarm_engine.go:118-120,437-451`).
- **The whole feature is toggle-off-able.** `PREWARM_ENGINE_ENABLED=false`
  reverts to the legacy `runPIPSeed` path in one helm `--set`
  (`phase1_walk.go:477-484`); `cache.PrewarmEnabled()`/`CACHE_ENABLED=false`
  makes the walk's harvesters nil and the seed a no-op (`phase1_walk.go:351-395`).

## Known failure modes

- **Pre-sync single-pass dynamic-children miss (the boot-race).** The Step-4
  walk runs before the sync barrier, so a navmenu whose children are
  `resourcesRefsTemplate`-driven sees empty fallthrough data and harvests only
  the 2 roots. *Mitigation:* the engine's post-barrier re-walk
  (`prewarm_engine.go:24-31`). With the engine OFF, the legacy seed runs on the
  single pre-barrier pass and under-warms dynamic subtrees.
- **Missing / unparseable frontend ConfigMap.** Yields no roots; warmup still
  syncs and flips readiness, and lazy register-on-navigation covers GVRs on
  first request (`phase1_roots.go:35-40`, `phase1_walk.go:622-633`). Degraded
  first-load latency, not an outage.
- **Engine worker killed at boot-seed return (the 0.30.247 regression).** If the
  worker were bound to the boot-seed orchestration ctx, it would die the instant
  the boot scope completes — dropping later `scopeKindGVRDiscovered` events
  (admin-path defect). *Fix v2:* the worker uses the process-lifetime ctx wired
  via `SetEngineProcessContext` (`prewarm_engine.go:295-316`,
  `phase1_walk.go:427-467`).
- **Six HARD REVERTs from a missing `rc` field.** A `phase1Walker` struct
  literal that omitted `rc` (the SA `*rest.Config`) fail-closed at
  `discoverPluralInfo`. *Fix:* `newPhase1Walker` makes `rc` a required positional
  parameter (`phase1_walker_new.go:78-104`); the boot re-walk site uses it
  (`prewarm_engine_boot.go:222`).
- **Empty per-GVR index → skipped seed.** If `BuildBindingsByGVRIndex` runs
  before the RBAC snapshot is ready, or a navigated GVR has no authorising
  binding, `EnumeratePrewarmTargetsForGVR` returns nil and the seed skips that
  GVR by design (`prewarm_enumeration.go:75-83`). Those targets fall back to
  cold-then-warm at first `/call` — never to a global-cohort universe (that
  fallback was an RBAC-leak risk and was removed,
  `prewarm_engine_boot.go:303-310`).
- **Transient apiserver pressure during seed.** An operational (non-RBAC-deny)
  per-target seed failure re-enqueues a single coalesced `scopeKindBoot` to
  retry after pressure clears (`classifyEngineSeedErr`,
  `prewarm_engine_boot.go:353-381`); RBAC-deny failures are expected and not
  retried.
- **Widget/RESTAction CR edits do not re-walk their subtree (gap, not bug).**
  `scopeKindWidgetCR`/`scopeKindRBACShift` are declared but unwired
  (`prewarm_engine.go:153-157`); such edits are covered only by the refresher
  (cached dependents) and lazy resolve-on-navigation until a future ship wires
  them.
