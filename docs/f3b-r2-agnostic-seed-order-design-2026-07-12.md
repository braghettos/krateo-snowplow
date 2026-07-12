# F3b-r2 — Agnostic seed order (Fix #130 F3b, redesign)

Date: 2026-07-12
Author: cache architect
Repo state: main @ b40117d (F3 build, 1.7.7)
Supersedes: the RootIndex==0 Phase-A/Phase-B split in docs/f3b-dashboard-first-seed-design-2026-07-12.md (Diego rejected as hardcoded frontend logic)

## TL;DR — the chosen order

**Sort the ENTIRE (widget-target, cohort) work list by `NavOrder` ASC as the primary
key, in one pass — no phases, no RootIndex branch, no frontend concept.** `NavOrder`
is a monotonic sequence number the engine ALREADY stamps at walk-discovery time
(phase1_pip_seed.go:317-333): it is literally "the order the walk reached this widget,"
which is the order a browser requests things from the nav entry outward. The dashboard
seeds first because it IS NavOrder-lowest in the DATA — not because any code says
"dashboard." This is candidate (a) (nav-graph discovery order), realized with the
field the engine already has and zero frontend vocabulary in the branch.

Fix 2 (the serve-watcher line in `withCohortSeedContext`) is UNCHANGED — uniform,
keep exactly as designed in the F3b doc. It is what makes the whale LISTs cheap so any
order fits budget.

## 1. WHY the RootIndex order was a special-case (agreeing with Diego)

The F3 build encodes `RootIndex==0` as a BRANCH in TWO places:
- The Lever-1 rank tier (`firstNavReachable`, prewarm_engine_boot.go:729/758-766/778):
  "cohorts with a RootIndex==0 widget sort first."
- The Lever-2 latch arming (`reachableFirstNav`, prewarm_engine_boot.go:940-976 +
  fire-decrement :1056): "fire readyz when RootIndex==0 targets are done."
And my rejected F3b added a THIRD: the Phase-A/Phase-B split keyed on `RootIndex==0`.

`RootIndex==0` means "the default-route/dashboard config-root subtree." Even though the
VALUE is walk-derived, using `==0` as a partition predicate hardcodes the proposition
"the first config root is special / is the dashboard / is first-nav." That is a
frontend-page concept baked into resolver seed logic — exactly what Diego rejects
(no special cases, one dynamic engine, everything from data). A different portal with
a different config.json root order, or a multi-root nav, would silently mis-key.

`NavOrder` carries the SAME information WITHOUT the branch: it is a total order, not a
boolean partition. "Seed lowest-NavOrder first" makes no claim about which root is
special — it just follows the walk's own discovery sequence. The dashboard wins
because the walk reached it first (config.json order, depth-first — phase1_pip_seed.go
:315-317), which is a property of the DATA the operator authored, not a code literal.

## 2. Candidate evaluation (ruling)

- **(a) Nav-graph discovery order (NavOrder ASC) — CHOSEN.** One uniform total order
  over all (widget-target, cohort) pairs. "Seed in the order a browser walks the nav,
  nearest-to-entry first." Zero frontend concept in the branch (the branch is just
  `a.NavOrder < b.NavOrder`). Uses the existing `NavOrder` field — no new plumbing.
  Generalizes to any portal structure. See §3.
  - Sub-ruling — BFS-by-depth vs DFS-sequence: I evaluated plumbing a per-widget
    walk-DEPTH (BFS: "seed all depth-0 across cohorts, then depth-1"). REJECTED as
    unnecessary: (i) `NavOrder` (DFS discovery seq) already places the dashboard
    subtree first because it is config-root 0 descended depth-first, and within it the
    shallowest widgets are reached first; (ii) BFS-depth would need a new field on
    navWidgetEntry (the walk is DFS-recursive, phase1_walk.go:1326 — depth is a local
    param, only plumbed onto apiRefPaginationJob:1494, NOT the harvested entry) = more
    code for no seed-coverage difference on the observed single-root topology; (iii)
    DFS discovery order is MORE faithful to "the order a browser requests things"
    (a browser follows the render tree depth-first, not level-order). If a future
    multi-root portal ever needs cross-root level-interleave, adding a walk-depth field
    is a clean follow-up — but it is not needed to close this milestone.
- **(b) Cost-based shortest-job-first.** Post-Fix-2 all targets are cheap, so SJF
  degenerates to "roughly uniform," losing the "nearest-to-entry" property that makes
  the FIRST page warm first. It also needs a measured/predicted per-target cost input
  (new state, or a pilot pass) and can starve a slightly-more-expensive dashboard
  widget behind many trivial tail widgets. REJECTED: no coverage benefit over (a),
  adds a cost oracle, and "cheapest first" is itself an arbitrary priority not tied to
  what the user sees first.
- **(c) Pure cohort round-robin (one target per cohort per turn).** Achieves per-cohort
  fairness with zero nav concept, BUT within a cohort it needs a SECONDARY order and
  the only sensible one is again NavOrder (else it seeds a random tail widget before
  the cohort's dashboard). So (c) reduces to (a) with a cohort-interleave outer loop.
  I FOLD the useful part of (c) into (a): the sort is over (NavOrder, cohort-rank) so
  equal-NavOrder widgets across cohorts interleave fairly (see §3). Standalone (c)
  without NavOrder REJECTED (random within-cohort order re-opens the "cheap-but-late
  widget before the dashboard" defect FIX-E already fixed).

## 3. DESIGN — one-pass NavOrder total order

Replace the two-phase / rank-major widget loop with a SINGLE flat sort over all
(widget, cohort-target) pairs, then one linear seed pass.

### The order (uniform total order, one comparator)
Build a flat slice of seed units `{navOrder, cohortRankIndex, widgetEntry, target}`
for every (widgetSeed ws, target c) with c.identityKey a ranked cohort. Sort by:

    1. ws.NavOrder ASC          — walk-discovery order (the ONLY ordering signal)
    2. cohortRankIndex ASC      — tie-break: at equal NavOrder, cohorts in the
                                  existing rank order (widgetMax DESC, allMax DESC,
                                  key ASC) — devs before admins before masters
    3. ns/name ASC              — final determinism

Then seed the flat list in order (widgets), followed by the RA tail in its existing
ascending-len order (RAs carry no NavOrder — they are not nav widgets; they seed after
the widget list, exactly as the RA tail does today).

- Key point: this is ONE pass, ONE comparator, applied to the WHOLE set. There is no
  "these are special, do them first" branch — the dashboard widgets are simply the
  low-NavOrder prefix of the sorted list. The SET is unchanged (PURE ORDERING, the
  FIX-E invariant); only the sequence is now NavOrder-major instead of rank-major.
- Cohort interleave: because NavOrder is the primary key, widget W's devs-target,
  admins-target and masters-target (all sharing W's NavOrder) seed adjacently. So every
  cohort's copy of the dashboard's first widget seeds before any cohort's second
  widget. This is the "reachable cohorts' dashboards complete early" property — now
  achieved by the data order, not a RootIndex phase.

### The latch (Diego's explicit question: is the EXISTING RootIndex latch objectionable?)
**RULING: the latch must ALSO drop RootIndex — replace its fire-condition with a
NavOrder threshold, keeping the same readiness SEMANTICS.** The latch's JOB (flip
readyz once "the first-nav page is warm for every cohort") is legitimate data-derived
readiness — but keying it on `RootIndex==0` re-imports the same frontend-page branch.
Under Diego's ruling applied consistently, the latch's partition must be data-order,
not a config-root boolean.

The uniform replacement: the latch fires when the seed pass has completed all units up
to a NavOrder WATERSHED — the NavOrder at which the walk finished the first
config-root's subtree. But "first config-root subtree" is itself the RootIndex concept.
The clean, branch-free formulation: **the latch fires when every cohort has been seeded
through the same NavOrder prefix that the FIRST-DISCOVERED cohort-reachable widget-set
spans** — i.e. arm the latch on the count of (widget,cohort) units whose NavOrder ≤ the
LARGEST NavOrder reachable from nav-root discovery before the walk's first descent
completes. This is still "a data-derived prefix," but I judge it too clever and
still smuggles "first subtree = special."

Simpler and honest: **readiness = the whole widget list seeded** (the NavOrder-sorted
widget prefix, i.e. Phase-A-equivalent = ALL nav widgets across all cohorts, before the
RA tail). Post-Fix-2 the entire widget list is ~40-120s (§4), well inside budget, so
gating on "all nav widgets seeded, RA tail may still run" needs no dashboard/first-nav
concept at all: readyz means "every cohort's nav widgets are warm; the RA-backed
content tail warms in background." The watershed is the widget/RA class boundary — a
data-structural fact (a target either is or isn't a nav widget), NOT a frontend-page
concept. That IS uniform and defensible.

- So the latch arms `navWidgetRemaining` = total (widget × cohort) units, fires at 0.
  No RootIndex, no NavOrder-threshold magic. The RA tail (Phase-B-equivalent) is
  explicitly excluded from readiness because RAs are the background content layer
  (they carry no NavOrder; a widget renders its structure without its RA-backed data,
  and the RA cells warm moments later + are covered by keepwarm).
- This is a STRICT GENERALIZATION of today's latch: today it waits for RootIndex==0
  widgets; now it waits for ALL nav widgets. Since post-Fix-2 all nav widgets are cheap,
  waiting for all of them (not just root-0) costs little and removes the special-case.
- Boundary preserved: MarkPhase1Done backstop on PHASE1_TIMEOUT/pipGlobalTimeout is
  UNCHANGED (C2 liveness — a pathological widget cannot hang readyz forever).

### Legibility / seam
The seam is now MORE legible than the rejected two-phase design: one flat sorted list,
one comparator, one linear seed loop, then the RA tail. The "why NavOrder" is a
one-line comment ("walk-discovery order = nearest-to-nav-entry first; the dashboard is
the low-NavOrder prefix by DATA, not by branch"). No RootIndex anywhere in the seed
ordering or the latch. The rank order survives only as the NavOrder tie-break
(cohortRankIndex) — devs still slightly precede admins at equal NavOrder, preserving
the largest-cohort-first property within a nav position without any special-case.

## 4. Sizing / Ready projection (with Fix 2)

- 186 distinct nav widgets × 3 reachable cohorts = ~558 widget-target units (the
  readiness-gating set under the new latch).
- Post-Fix-2 the ~14 whale widgets (benchapps/compositiondefinitions LISTs) serve from
  the synced informer (~0.3s each vs ~20s live); the other ~172 are ~0.05s.
- Per cohort: 14×0.3 + 172×0.05 ≈ 4.2 + 8.6 ≈ 13s. × 3 cohorts serial ≈ **~40s**.
  (Same ~40s as the F3b doc's Phase A — the readiness-gating widget set is identical;
  only the ORDER within it changed from RootIndex-phased to NavOrder-sorted.)
- Well inside pipGlobalTimeout (480s) and PHASE1_TIMEOUT (900s). Ready via the latch
  (NavOrder-widget-list-complete), NOT the backstop.
- #128 independence UNCHANGED from the F3b doc: the marketplace-detail null-jq is an
  RA (seeds in the tail, after the latch fires) and its first op-failure fires
  ~9.5 min into boot; a ~40s widget pass fires the latch ~8-9 min before the churn.
  #128 fix NOT a prerequisite for readiness acceptance; still a fast-follow for
  RA-backed first-nav content.

## 5. WHY THIS CONTAINS NO FRONTEND SPECIAL-CASE (the paragraph for Diego)

The seed order is a single total order — `NavOrder` ASC — applied uniformly to every
(widget, cohort) unit in one pass. `NavOrder` is not a frontend concept encoded in
code; it is a monotonic counter the engine stamps as the nav WALK discovers each widget
(phase1_pip_seed.go:317), i.e. it is the portal operator's own authored nav structure
expressed as a sequence. The resolver never asks "is this the dashboard?" or "is this
first-nav?" or "is this config-root 0?" — it asks only "did the walk reach A before B?"
The dashboard ends up first purely because the walk reaches it first in the DATA; on a
differently-structured portal, whatever the walk reaches first seeds first, with no
code change. Every `RootIndex==0` branch — in the rank tier, in the latch, and in the
rejected two-phase split — is DELETED and replaced by this one data-derived order. The
readiness latch keys on the widget-vs-RA CLASS boundary (a structural fact: a target
either is or isn't a nav widget), never on a page/route/dashboard concept. There is no
static list, no lazy-fill-cold, L1 stays BindingUID-keyed, one engine, and CRUD
re-runs the identical enumeration + sort.

## 6. Falsifiers (adapted to the new order; PM gate)

- **(a) Hermetic — NavOrder total order, no RootIndex (K>1 cohorts × M>1 widgets).**
  Build widgetSeeds with NavOrder 0..M-1 spanning K≥2 cohorts, with the LOW-NavOrder
  widgets deliberately NOT in config-root 0 (set RootIndex arbitrarily / to a non-zero
  value on a low-NavOrder widget). Assert the seed order follows NavOrder ASC
  regardless of RootIndex, and every cohort's low-NavOrder widgets seed before any
  cohort's high-NavOrder widget. RED arm = the F3 RootIndex-keyed order (would seed a
  RootIndex==0 high-NavOrder widget before a RootIndex!=0 low-NavOrder one) → order
  diverges. This RED specifically proves the RootIndex branch is GONE. Degenerate
  K=1/M=1 inadmissible (feedback_falsifier_shape_must_discriminate).
- **(b) Hermetic — latch fires on all-nav-widgets, no RootIndex.** K≥2 cohorts, each
  with M≥2 nav widgets (mixed RootIndex) + M≥2 RA tail targets. Assert the latch fires
  after the LAST nav widget of ANY cohort and BEFORE any RA seeds, independent of
  RootIndex. RED = the F3 latch (fires when RootIndex==0 widgets done, leaving
  RootIndex!=0 nav widgets cold) → fires early.
- **(c) Hermetic — serve-watcher on seed ctx (Fix 2, unchanged).** Cohort-seed widget
  LIST serves from the informer, not a live paged LIST. RED = remove the
  WithServeWatcher line.
- **(d) Standing C-F3-4 acceptance (unchanged) + timing.** Repeat the exact reboot +
  counted admin first-nav (real Chrome, per-/call resolved_cache.lookup, first-touch,
  cold boot no prior admin session). REQUIRE: `prewarm.first_nav.latch` FIRES (Ready
  via latch, NOT backstop) with elapsed « budget (~40s); `hits_seed_attributable > 0`;
  admin first-nav hit-rate ≥ 85% (same #128/#129 residual justification). Capture the
  seed order from the (new) telemetry to confirm NavOrder-major.
- **(e) #128-independence arm (unchanged).** Assert the latch fire timestamp PRECEDES
  the first marketplace-detail operational_failure timestamp.

## 7. REACHABILITY (F1 lesson — deployed binary reaches the new code)

- The seed engine, rank sort, and latch all RAN on the measured 1.7.7 boot (ranks
  lines 1968+, seed.abort, backstop Ready). The redesign changes the SAME code loci:
  - The flat NavOrder sort replaces the rank-major widget loop at
    prewarm_engine_boot.go:998-1063 (the loop that processed 222 targets on 1.7.7).
  - The latch arming (:940-976) + fire (:1056) change from RootIndex to
    navWidgetRemaining — the same latch that (failed to) fire on 1.7.7.
  - Fix 2 = one line in withCohortSeedContext (phase1_pip_seed.go:439), called by
    seedOneTarget:593 for every target every boot.
  - `NavOrder` is already stamped on every harvested entry (phase1_pip_seed.go:333) —
    already emitted in the widget_targets telemetry (:814) on 1.7.7, so the data the
    sort consumes is proven present in the deployed binary.
- All on the unconditional seed path; no flag, no env gate.
- Deployed-binary reach proof: falsifier (d)'s `first_nav.latch` FIRING on a real
  reboot (vs 1.7.7's backstop-only) proves the NavOrder-ordered pass reached every
  cohort's nav widgets within budget; the seed-order telemetry confirms NavOrder-major
  (not rank-major). The hits_seed_attributable observable is the standing regression
  probe.

## 8. Options with recommendation (strategic — surface to Diego)

- **Option A (recommended): NavOrder total order + all-nav-widget latch + Fix 2.**
  Fully agnostic (no RootIndex anywhere), closes the milestone, one comparator + one
  line. Requires touching the latch (removing its RootIndex) — a slightly larger diff
  than "reorder only," but it is the consistent application of Diego's ruling.
- **Option B: NavOrder order for the SEED, keep the RootIndex latch.** Smaller diff
  (latch untouched) but INCONSISTENT — the latch still hardcodes RootIndex==0, which
  is the same objection. NOT recommended; Diego's ruling applies to the latch too
  (his explicit question). Only choose if he decides the latch's readiness semantics
  are acceptable as-is and only the SEED-ORDER branch was the objection.
- **Option C: add a walk-depth field + BFS level-order.** More faithful cross-root
  interleave on hypothetical multi-root portals, but needs new plumbing and gives no
  coverage gain on the real single-root topology. Defer to a follow-up if multi-root
  ever appears.
Recommend A. The one open question for Diego's concept nod (§5): is deleting the
RootIndex latch (→ all-nav-widget latch) the correct consistent reading, or does he
consider the latch's RootIndex an acceptable data-derived readiness signal (Option B)?
State this explicitly to him before the PM gate re-runs.
