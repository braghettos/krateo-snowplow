# FIX-F first-nav latch zero-fire — root-cause trace (2026-07-06)

Repo: main @ 70b9fba (== tag 1.6.0, verified `git merge-base --is-ancestor` + empty
`git log 1.6.0..HEAD`; the brief's "dd6d7d4" is an ancestor, the deployed 1.6.0 image
is byte-identical to this checkout). Live evidence: fresh2 60K, pods kb8bb
(PHASE1_TIMEOUT=180, /tmp/boot160.log) and mgrt7 (PHASE1_TIMEOUT=600, /tmp/boot600.log).
boot160.log's capture ends 20:14:03 (before its latch fire); boot600.log contains the
full arming→fire sequence and is the primary evidence.

## 1. Symptom

`prewarm.first_nav.latch reason=zero-first-nav-targets first_nav_widgets=0
first_nav_targets=0 elapsed_ms=201` at 20:33:04.202 (boot600 line 5795) — on a boot
where the harvest was demonstrably present and healthy at arming time:

- `prewarm.engine.boot.rewalk_complete restactions=25 widgets=175 elapsed_ms=161349`
  20:33:03.772 (line 5386).
- 175 `prewarm.engine.seed.widget_targets` lines, `nav_order` 0..174 stamped in walk
  order (`app-shell` nav_order=0, `sider-content` nav_order=1), `targets` = 16 on 174
  of 175 (15 on one) — the uniform widget-floor identity set.
- `prewarm.enumerate.dedup` ranking substrate non-empty (16–52 identities per GVR),
  `index_built bindings_enrolled=4187` (line 5383).

So every input the latch consumes existed — yet the rank-1 × RootIndex==0 count was 0.

## 2. Adjudication of the briefed hypothesis — REFUTED (as the operative mechanism)

Hypothesis: the redrive/re-walk path never calls `BeginRoot()` → widgets harvested by a
redrive stamp `RootIndex=-1` → the arming count at `RootIndex==0` is 0.

Two code facts kill it for THESE boots:

1. **TRACED — the -1 case cannot exist.** `harvestNavWidget` clamps a negative
   `curRoot` to 0 at stamp time (phase1_pip_seed.go:301-304: `rootIdx := h.curRoot;
   if rootIdx < 0 { rootIdx = 0 }`). A harvest that precedes every `BeginRoot` stamps
   RootIndex=0, never -1.
2. **TRACED — this deployment is single-root, so curRoot is pinned at 0.** The frontend
   config.json declares only `.api.INIT`; `.api.ROUTES_LOADER` is empty —
   `phase1.roots.entry_point_empty field=ROUTES_LOADER` (phase1_roots.go:157-165 logs
   per FIELD, not per walk failure) at 20:24:32 (initial lister, boot600 line 39) and
   again at 20:30:22 (re-walk lister, line 3444); `roots_discovered roots_count=1`
   (line 44). The initial boot walk's resolver closure calls `BeginRoot()` once
   (phase1_walk.go:401) → curRoot: -1→0. The engine re-walk
   (prewarm_engine_boot.go:259-290) indeed NEVER calls `BeginRoot` — it builds
   `newPhase1Walker` and calls `w.walk()` directly (:274-281) — but with curRoot
   already 0, every widget first-harvested by ANY pass (initial walk, engine re-walk,
   config-vars redrive re-walk) stamps RootIndex=0.

So on both boots **all 175 harvested widgets carry RootIndex==0**, and the arming loop
(prewarm_engine_boot.go:755-756 `if ws.e.RootIndex != 0 { continue }`) filtered none of
them out. RootIndex stamping is NOT why the count was 0.

**The missing `BeginRoot` on the re-walk IS a real latent defect** (TRACED-latent,
prewarm_engine_boot.go:259-290): with N≥2 config roots the boot walk leaves
curRoot=N-1, so any widget first-harvested only during a redrive re-walk (the common
case at 50K+ where the effective harvest comes from the config-vars redrive) stamps
RootIndex=N-1≠0 → the same zero-fire, for a different reason. It must be fixed for
multi-root correctness, but it did not produce this boot's symptom.

## 3. TRACED root cause — rank-1 identity has zero widget targets

The latch segment is defined as **ranked[0] ∩ RootIndex==0 widgets**
(prewarm_engine_boot.go:749-770): `rank1 := ranked[0].key` (:754), and a widget target
counts only when `identityKey(c) == rank1` (:761-764).

`ranked` is built over the UNION of widget-target and restaction-target identities
(:635-667): `noteIdentity` records each identity's **first-seen** `CollapsedBindings`
(:641-647, `if _, ok := rankOf[k]; !ok` — first write wins), widgets loop first
(:648-652), then restactions (:653-657); sort DESC by collapsed (:662-667). Nothing
guarantees ranked[0] appears in ANY widget target set.

On boot600 it did not. The rank-major loop seeds ranked[0]'s widgets first, then its
restactions (:792-845). The complete pre-latch seed activity (20:33:04.001→.202, i.e.
the entire rank-1 pass) was:

- **zero widget seeds** (no `site=seed handler=widgets` diag line), and
- **8 restaction seeds, all under cohort
  `system:serviceaccount:bench-ns-01:benchapps-v0-1-0`**, every one skipped with
  `phase1.seed.skip.empty_binding` (BindingUID re-derived "" — EvaluateRBAC fail-closed
  at the RESTAction's own coordinates; the FIX-C A4 populate guard,
  phase1_pip_seed.go:487-497): settings-krateo-status, blueprint-install-formdef,
  blueprints-cards, blueprints-catalog, blueprints-list, global-search,
  marketplace-detail, settings-users.

Since the rank-major loop seeds ranked[0] first, **ranked[0] IS the bench SA**
(TRACED by loop semantics + observed order). That SA appears only in restaction
TARGET-GVR enumerations — `apps/deployments` (1344 bindings → 37 identities),
`core.krateo.io/compositiondefinitions` (48→47), `core/secrets` (62→52) — and NOT in
the widget-floor set (`widgets.templates.krateo.io/*`: 1308 bindings → 16 identities,
uniform across all 19 widget GVRs; all 175 widgets logged `targets:16`, and none of the
16 matched rank1, hence `first_nav_widgets:0`).

Chain: bench-SA identity noted from a restaction target set with a first-seen
CollapsedBindings higher than every widget-floor identity's (the deployments bucket is
the only set large enough — 1344 raw bindings; the per-identity split is INFERRED, the
ranking OUTCOME is TRACED) → ranked[0] = bench SA → the arming scan over all 175
RootIndex==0 widgets finds zero rank-1 targets → `firstNavTotal=0` → the rank-1
boundary fires `zero-first-nav-targets` (prewarm_engine_boot.go:854-856) → engineSeed's
select unblocks (phase1_walk.go:505-507) → deferred `MarkPhase1Done` flips /readyz —
**without the dominant login cohort's dashboard cells warm, on every boot with this
RBAC topology**. The gate FIX-F was built to be (#99, F-C1..C3) never actually gates.

### 3a. Secondary findings (all TRACED unless noted)

1. **Rank-1 is nondeterministic across boots.** `noteIdentity` keeps the FIRST-seen
   CollapsedBindings; the restaction precompute iterates the harvester snapshot in Go
   map order (prewarm_engine_boot.go:615-626 over `restactionRefs` from
   `contentPrewarmHarvester.snapshot()`). The same identity carries a different count
   per enumerate (per-bucket counting, prewarm_enumeration.go:139-147 bucket∪wildcard,
   :192-208), so which count wins is map-order-dependent → on some boots the bench SA
   may be noted from the secrets set (small count) and a real cohort ranks first. The
   zero-fire is therefore intermittent by construction — worse than a deterministic bug.
2. **The whole rank-1 slot is waste on this topology**: all 8 bench-SA restaction seeds
   skip on empty_binding (index says the SA can list deployments; EvaluateRBAC at the
   RA's `templates.krateo.io/restactions ns=krateo-system` coordinates denies). Cheap
   (~25ms each) but it means the FIX-D "95%-mix dominant cohort" heuristic selected a
   machine SA that can never log into the portal.
3. **Observability gap**: `widget_targets` logs `nav_order` but not `root_index`
   (prewarm_engine_boot.go:672-681), and the latch line does not name the rank-1
   identity — which is why this trace needed the seed-order inference. NavOrder and
   RootIndex have no different stamping condition (navSeq increments unconditionally at
   first harvest, phase1_pip_seed.go:296-297; RootIndex was stamped 0 everywhere here,
   :301-313) — RootIndex is simply invisible in logs.

### 3b. Answers to the brief's side questions

- **entry_point_empty on both walks**: it is per-FIELD — ROUTES_LOADER is genuinely
  empty in this deployment's config.json; INIT resolved both times. One root, both
  passes.
- **Which walk harvested**: both write into the SAME harvester (shared by reference,
  first-write-wins dedupe, phase1_pip_seed.go:279-284). The initial walk (20:24:32→
  sync barrier) harvested whatever was resolvable pre-sync; the engine re-walk
  (20:30:22→20:33:03, 161s) filled the rest. Neither path's BeginRoot behaviour matters
  on a single-root config (curRoot pinned at 0). The re-walk path does NOT call
  BeginRoot — latent multi-root defect only (§2).
- **nav_order stamped but RootIndex not**: false premise — both stamped; RootIndex==0
  for all 175; it is not logged.

## 4. Fix design (ranked)

**Fix 1 — latch segment identity = highest-ranked identity that actually has a
RootIndex==0 widget target (RECOMMENDED, ~20 LOC).**
At the arming block (prewarm_engine_boot.go:749-770): instead of hard-wiring
`rank1 := ranked[0].key`, scan `ranked` in order and pick the first identity with ≥1
RootIndex==0 widget target as `segKey`/`segRank`; count that identity's segment. The
decrement condition (:827) changes `ri == 0 && ... == rankKey` → `ri == segRank &&
identityKey(c) == segKey`; the zero-fire boundary (:854) changes `ri == 0` →
`ri == segRank` (fires only when NO ranked identity has any first-nav widget target —
preserves F-C3's provably-empty semantics; ranked-empty fire at :861-863 unchanged).
Seed ORDER is untouched (pure gate fix; the bench-SA rank-1 pass remains a cheap
skipped prefix). Walk-derived, no name literals, no caps. This makes the latch correct
REGARDLESS of rank pollution and of finding 1 (nondeterminism).

**Fix 2 — re-walk root stamping for multi-root correctness (~12 LOC, same ship).**
Add `navWidgetHarvester.BeginWalk()` (reset `curRoot = -1` under mu) and call it at the
top of each walk pass; call `BeginRoot()` per root in the engine re-walk loop
(prewarm_engine_boot.go:259, before `w.walk`). NOTE: naively adding only `BeginRoot()`
to the re-walk loop is WRONG — it would increment past the boot walk's final value and
stamp redrive-harvested widgets N..2N-1. The reset+increment pair keeps root indices
stable per pass (roots iterate in config.json order every pass). First-write-wins
preserves boot-walk stamps. Inert today (single root); required the day ROUTES_LOADER
is declared.

**Fix 3 — rank hygiene (follow-up, separate gate).** Make `noteIdentity` fold the MAX
(or sum) CollapsedBindings across enumerates instead of first-seen (kills the
map-order nondeterminism), and/or rank widget-capable identities ahead of
restaction-only identities so the prime seed slot is never spent on a machine SA.
Changes global seed order → needs the FIX-E interleave falsifier re-run; not required
for gate correctness once Fix 1 lands.

**Observability (ride-along, ~4 LOC).** Add `root_index` to the `widget_targets` line
and the segment identity (+ its rank) to the `prewarm.first_nav.latch` line.

## 5. Falsifier

Hermetic (uses the existing seams `enumeratePrewarmTargetsForGVRFn` /
`seedOneWidgetFn` / `restActionTargetGVRFn` / `seedOneRestactionFn` +
`resetFirstNavLatchForTest` + `firstNavFireObserver`, prewarm_engine_boot.go:394-409,
prewarm_first_nav_latch.go:133-146):

- Arrange: 2 widgets RootIndex==0 + 1 widget RootIndex==1; widget target sets = {U1
  (collapsed 5), U2 (collapsed 2)}; 1 restaction whose target set = {M (collapsed
  50)} — M in NO widget set (the boot600 shape: machine identity out-ranks via a
  restaction-only enumerate).
- **RED (current code)**: latch fires `reason=zero-first-nav-targets`,
  `first_nav_targets=0`, positioned (observer) BEFORE any widget seed.
- **GREEN (Fix 1)**: latch fires `reason=segment-complete`, `first_nav_widgets=2`,
  `first_nav_targets=2` (U1 × RootIndex==0 pairs), positioned after the 2nd U1
  RootIndex==0 widget seed and BEFORE the RootIndex==1 widget and the restaction tail
  (ARM-TAIL preserved).
- **Fix 2 arm**: 2 roots; boot-walk pass harvests nothing (empty subtrees); a second
  walk pass (redrive shape: fresh walkers, shared harvester) harvests both subtrees;
  assert root-0's widgets stamp RootIndex==0 (RED today: both stamp 1) and the latch
  then counts them.
- Verify the arms ACTUALLY RUN (=== RUN lines / arm count — no bare green;
  feedback_falsifier_must_actually_run_under_gate_tag_env).

On-cluster proof (next 60K boot): `grep prewarm.first_nav.latch` →
`reason=segment-complete first_nav_targets>0`, fire timestamp strictly after the
segment identity's RootIndex==0 widget seed diag lines and well before
`prewarm.engine.boot.complete`; post-Ready `/dashboard` nav#1 l1:HIT content check
(the F-C4 guard) unchanged.

## 6. Customer-impact framing

No cold-serve regression is attributable to this defect on the observed boots: the
zero-fire happens AFTER the engine re-walk (content/identity-free L1 already warm —
live probe was a 3ms content-hit at Ready), and the per-user cells the latch was meant
to gate are seeded moments later by the same boot scope. This is gate-SEMANTICS
correctness (readiness flips before the rank-1 first-nav per-user segment is warm —
exactly the degeneration FIX-F was shipped to remove, plus a nondeterministic rank-1
selection). Priority: correctness fix on the next ship train, not a hotfix.

Evidence paths: /tmp/boot600.log (lines 38-44, 3441-3444, 5383-5386, 5571-5745,
5787-5795), /tmp/boot160.log (lines 38-44, 2019-2030; capture ends 20:14:03 pre-latch).
