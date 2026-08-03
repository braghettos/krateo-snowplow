# Fix-3 rank hygiene — deterministic, widget-capable-first identity ranking (design, 2026-07-07)

Repo: main @ bbaba28 (1.6.2). Origin: docs/fixf-rootindex-redrive-walk-trace-2026-07-06.md §3a/§4
(Fix 3). Live evidence: fresh2 60K boot600 (/tmp/boot600.log) + boot krkbl (1.6.2). Design only —
no code in this change.

**Paths (as-built correction, 2026-07-07):** every `prewarm_engine_boot.go` /
`prewarm_enumeration.go` / `phase1_pip_seed.go` / `prewarm_first_nav_latch_segment_test.go`
reference below lives under **`internal/handlers/dispatchers/`** (NOT `internal/prewarm/` —
that package does not exist). The ranking site is
`internal/handlers/dispatchers/prewarm_engine_boot.go`; the CollapsedBindings enumerator is
`internal/handlers/dispatchers/../cache/prewarm_enumeration.go`
(package `cache`, imported as `EnumeratePrewarmTargetsForGVR`).

## 0. Prior-art check

- k8s.io/apimachinery `sets.Set[T].List()` / `sets.List` is the canonical "never emit Go map
  order" idiom — sorted keys, total order. We already follow it structurally
  (sort.SliceStable over a full-key comparator, prewarm_engine_boot.go:701-706); the defect is
  not the SORT, it is the sort KEY: `noteIdentity` records a map-iteration-order-dependent
  VALUE (first-seen CollapsedBindings, :683-685). No client-go/apimachinery primitive ranks
  identities; the canonical fix for order-dependent aggregation is a commutative/associative/
  idempotent fold (max), which needs no library. Nothing to reuse beyond what is already used.
- client-go workqueue priority/rate-limiting queues solve scheduling fairness, not static
  precomputed ordering — not applicable.

## 1. Root cause recap (TRACED, from the §3a trace, re-verified at 1.6.2 line numbers)

1. **Nondeterministic rank value.** `noteIdentity` keeps the FIRST-SEEN `CollapsedBindings`
   (prewarm_engine_boot.go:680-686, `if _, ok := rankOf[k]; !ok`). The same identity carries a
   DIFFERENT count per GVR bucket by construction (per-bucket counting,
   prewarm_enumeration.go:192-208: count = raw bindings collapsing into the identity within
   that bucket ∪ wildcard). Widget seeds are noted first (:687-691) in deterministic NavOrder,
   but restaction refs iterate the harvester snapshot in Go map order upstream, and any
   identity NOT present in a widget set gets whichever restaction bucket's count happens to be
   seen first → ranked order flips across boots (observed: rank-1 flipped between the bench SA
   and devs across consecutive 1.6.1 boots).
2. **Rank won by a widget-less identity.** Nothing prefers identities that can actually render
   the portal: the bench SA (present only in RA target enumerations via the 1,344-binding
   `apps/deployments` bucket, absent from the 16-identity widget floor) took ranked[0] on
   boot600 → the prime seed slot = 8 restaction seeds ALL skipped `empty_binding`, and the
   #102 keepwarm sweep (rank1Only → ranked[0], :872-874) re-seeded the same useless cohort
   every TTL×3/4 window (16 empty-binding skips observed in the first live sweep window).
   #99b fixed the LATCH (segKey scan :816-844) but deliberately left ranking + seed order
   untouched.

## 2. Chosen shape

**Rank key = (widgetMax DESC, allMax DESC, identityKey ASC)**, where both counts are
**max-folds** over every observation of the identity across all precomputed target sets:

- `widgetMax` = max CollapsedBindings over the identity's WIDGET-target observations
  (0 if it appears in no widget target set).
- `allMax` = max CollapsedBindings over ALL observations (widget + restaction).

Properties, by construction:

- **Deterministic.** max is commutative/associative/idempotent → the fold result is a pure
  function of the observation MULTISET, independent of widgetSeeds/restactionSeeds iteration
  order and of Go map order. Combined with the existing full-key comparator there is exactly
  one ranked order per (harvest, index) snapshot.
- **Widget-capable-first as a tier, without a tier flag.** CollapsedBindings ≥ 1 always
  (prewarm_enumeration.go:207), so `widgetMax ≥ 1 ⇔ widget-capable`. Sorting widgetMax DESC
  alone places every widget-capable identity strictly above every widget-less one; no boolean
  field, no second sort pass.
- **Population-proxy fidelity (FIX-D intent).** Within the widget-capable tier the primary
  metric is the identity's largest widget-bucket collapse — i.e. the per-composition
  RoleBinding population that can actually LIST widget kinds (devs ≫ singleton users). This
  deliberately does NOT let a huge NON-widget bucket (the deployments whale) outrank a real
  login cohort inside tier 1 — the exact pollution class recurring one tier down. `allMax` is
  the secondary key: it orders the widget-less tier (their only observations) and breaks
  widget-count ties inside tier 1 by breadth elsewhere.
- **Tie behavior.** The uniform widget floor makes the same identity's count IDENTICAL across
  widget buckets (same binding set per bucket) — max/sum/first coincide there — but counts
  DIFFER between identities within a bucket (devs collapse hundreds; singletons are 1), so
  tier-1 order is not degenerate. Genuine ties (e.g. many singleton users at
  widgetMax=1,allMax=1) fall to the existing `identityKey` ascending tie-break
  (prewarm_engine_boot.go:705) — total, deterministic, no starvation.

**Rejected alternatives (Q1):**

- *sum across buckets*: double-counts — a wildcard binding lands in EVERY bucket
  (prewarm_enumeration.go:145-147), so sum scales with the number of GVRs enumerated, and on
  the uniform floor sum = count × nWidgetGVRs (same order as max, more distortion elsewhere).
  Deterministic but semantically mushy.
- *max over ALL buckets as the tier-1 primary*: deterministic, but re-admits the pollution
  class within tier 1 (an identity with one widget binding + a whale RA bucket outranks devs).
- *first-seen (status quo)*: the defect.

## 3. Q2 adjudication — widget-less identities: order last, do NOT exclude

- **Frontend grounding (TRACED, SPA source /Users/diegobraga/krateo/frontend-draganddrop/
  frontend):** every RESTAction consumption path in the portal is widget-mediated —
  src/hooks/useWidgetQuery.ts, src/hooks/useHandleActions*, src/widgets/Table/Table.tsx,
  src/components/CommandPalette/CommandPalette.tsx; grep for non-widget restaction fetch paths
  is empty. A user who can list no widget kind renders no page and therefore never triggers an
  RA /call from the SPA. So a widget-less identity's RA cells have NO frontend consumer — the
  only possible consumer is a direct /call API client (machine SA), which is outside the
  north-star (feedback_north_star_is_frontend_ux, feedback_prewarm_follows_frontend).
- **Server grounding (TRACED):** the restactions /call path has no widget-capability
  precondition — any identity whose RBAC allows the RA is served; lazily populated on first
  call like any cold cell. Excluding widget-less identities from SEEDING would not break them,
  only leave them cold.
- **Decision: pure ordering, set unchanged (recommended).** Widget-less identities keep their
  RA seeds, ranked at the tail (tier 2, ordered by allMax). Rationale: (a) FIX-E's load-bearing
  "PURE ORDERING: the seed SET is unchanged" invariant (prewarm_engine_boot.go:606-609)
  survives — this ship changes only the SEQUENCE, so the lossless/leak-free proofs need no
  re-derivation; (b) excluding is a seed-SET change that buys ~nothing: on the observed
  topology the bench SA's 8 seeds ALL skip `empty_binding` in ~25ms each (fail-closed populate
  guard, phase1_pip_seed.go FIX-C A4) — the cost merely moves from the FRONT of boot to the
  tail; (c) proving "no RA-only cohort is ever customer-valuable" for ALL topologies is not
  needed if we don't remove them. Option B (exclude from boot ordering, keep for nothing) is
  strictly dominated and rejected.
- **#99b segKey scan: KEEP.** After Fix-3, ranked[0] is widget-capable by construction, so the
  scan binds segRank=0 in the boot600 topology — but "widget-capable" means ≥1 widget target
  ANYWHERE, not ≥1 RootIndex==0 widget target. On a multi-root config an identity present only
  in non-first-root widgets can still take ranked[0], and the scan (:816-844) is the guard
  that walks down to the first identity with a genuine first-nav target. It is cheap, already
  RED-proven, and becomes a safety net rather than dead code.

## 4. Q3 — keepwarm repair, no keepwarm code change (TRACED)

`rePrewarmKeepwarm` → `rePrewarmBootScoped(rank1Only=true)` (prewarm_engine_boot.go:214-219)
→ `seedScopeYielding` (:375) recomputes the ranking from a FRESH harvest+index snapshot on
every sweep; the sweep bound is `if rank1Only && ri > 0 { break }` (:872-874) over that
recomputed `ranked`. Fixing the ranking function therefore fixes the sweep automatically:
ranked[0] = the dominant widget-capable cohort (devs), which is exactly #102's stated scope
("dominant-CollapsedBindings cohort's cells", :867-871). Zero keepwarm-side edits; the
observed 16-empty-binding-skip sweep waste disappears as a direct consequence. The
ARM-KEEPWARM-SEGRANK ruling (prewarm_first_nav_latch_segment_test.go:204-263 — "segRank must
NOT retarget the keepwarm bound; the ranking is the thing to fix") is honored: we fix the
ranking, not the loop bound.

## 5. Q4 — FIX-E / FIX-F / F.4 interactions

- **FIX-E order properties are preserved, one fixture's rank assignment changes.** Rank-major,
  class-interleave, within-rank NavOrder, RA ascending-len tiebreak (all asserted in
  prewarm_engine_seed_order_test.go) are untouched — only WHICH identity holds which rank
  changes when a widget-less identity previously out-counted a widget-capable one. Test-impact
  enumeration in §7.
- **segKey/segRank semantics unchanged**; expected steady-state becomes segRank==0 (new
  telemetry assertion opportunity: a boot logging segRank>0 now signals a multi-root/partial-
  visibility topology, not pollution).
- **F.4 chunk resume gains a stability property.** Ranking is recomputed per scope execution;
  today two chunks of the same boot could disagree on ranked order (map-order value), so a
  deadline-cut + requeue could resume in a DIFFERENT order. Post-Fix-3: same snapshot →
  identical order (stated as R3-C1); across chunks the snapshot may still legitimately change
  (index rebuild after re-walk) — that residual is bounded by the F.4 boot-only fresh-skip
  (already-seeded cells are cheap re-encounters), and is a property statement, not a code
  change.

## 6. Mechanism + LOC bound

Single site: `seedScopeYielding` step (3), prewarm_engine_boot.go:674-706. ~25 LOC delta.

- `rankedIdentity{key, widgetMax, allMax int}` (replaces `collapsed`).
- `noteIdentity(c seedTarget, isWidget bool)`: max-fold both fields (unconditional write of
  `rankOf[k]` with folded maxima; no first-seen guard).
- Widget loop passes `isWidget=true`, restaction loop `false`.
- Comparator: widgetMax DESC, then allMax DESC, then key ASC.
- Ride-along observability (~6 LOC): one `prewarm.engine.seed.rank` Info line per ranked
  identity at boot scope (rank, cohort label, widget_max, all_max) — bounded by identity count
  (16–52 observed; same order as the existing 175 widget_targets lines), emitted only when
  `!rank1Only` to keep sweep logs quiet. The latch line already carries segKey/segRank (#99b).

No knobs, no static lists, no caps; rank metric stays data-derived CollapsedBindings
(feedback_no_special_cases, feedback_dynamic_cohort_prewarm_no_static_no_cold_fill). Cost:
same single O(T) fold + O(I log I) sort per scope execution (R3-C7).

## 7. Test impact (enumerated — R3-C6)

| Test | Impact |
|---|---|
| prewarm_seed_identity_rank_test.go `TestFixD_IdentityRankMajorSeedOrder` | GREEN unchanged (all fixture identities appear in widget sets with one count each; fold = identity) |
| prewarm_engine_seed_order_test.go `TestFixE_RankMajorClassInterleaveFirstNavOrder` | GREEN unchanged (devs/ops both widget-capable; order properties orthogonal to rank values) |
| prewarm_first_nav_latch_segment_test.go `TestFirstNavLatch_SegmentIdentity_RestactionOnlyRank0_FiresOnWidgetSegment` | Premise inverts: M (RA-only, 50) now ranks BELOW U1/U2 → segRank binds 0. Keep the arm (asserts fire-on-widget-segment) with updated rank expectation; ADD a new multi-root arm that keeps the #99b scan discriminating (ranked[0] widget-capable but RootIndex==1-only → segRank>0) — see falsifier F5 |
| prewarm_first_nav_latch_segment_test.go `TestKeepwarmSweep_RankOne_SeedsRankZeroNotSegment` | Expectation INVERTS by design: sweep must now seed U1 (widget-capable dominant), not M. Rewrite as the R3-C4 keepwarm-repair arm; the ruling it encoded (don't retarget the loop bound via segRank) stays honored — the loop bound is untouched |
| prewarm_keepwarm_sweep_test.go (cadence/enqueue/rank1-bound) | GREEN unchanged (mechanism, not rank values) |
| prewarm_engine_seed_latch_test.go | GREEN unchanged (counter/queue semantics) |

## 8. PM-gateable conditions

- **R3-C1 (determinism).** Ranked order is a pure function of the target multiset: same
  harvest+index snapshot → byte-identical ranked order across N≥20 in-process runs with input
  orders shuffled per run (widget list permuted, restaction list permuted — the map-order
  proxy). Mutation arm: restore first-seen fold → RED (some permutation flips ranked[0]).
- **R3-C2 (pollution).** boot600 shape (M: RA-only, CollapsedBindings 1344; U1: widget 5;
  U2: widget 2) → ranked = [U1, U2, M]. Mutation: drop the widgetMax primary key → RED
  (M first).
- **R3-C3 (pure ordering / set unchanged).** The seeded (unit×identity) event MULTISET is
  identical pre/post-change on the same fixture — widget-less identities still seed their RA
  targets, at the tail. RED if any seed disappears.
- **R3-C4 (keepwarm repair).** rank1Only sweep on the boot600 shape seeds U1's targets and
  none of M's; diff scope check: no edits outside the step-(3) ranking block (keepwarm loop
  bound untouched).
- **R3-C5 (segKey alignment + scan retention).** Boot600 shape → segRank==0, segKey==U1;
  multi-root arm (ranked[0] widget-capable with RootIndex==1 targets only) → segRank>0 and the
  latch still fires on the true first-nav segment (the #99b scan stays live and discriminating).
- **R3-C6 (falsifier re-run).** Every §7 row runs and is green (=== RUN verified, no
  bare-green "no tests to run" — feedback_falsifier_must_actually_run_under_gate_tag_env);
  FixD/FixE order arms unmodified except the two enumerated latch-file expectation changes.
- **R3-C7 (cost).** No per-seed work added; ranking stays one fold + one sort per scope
  execution (code-inspection + existing bench unaffected).
- **R3-C8 (on-cluster symptom-disappear, next 60K boot).** (a) the first `site=seed` diag
  lines after `rewalk_complete` are WIDGET seeds under a login cohort (not the bench SA), zero
  `empty_binding` skips in the rank-1 prefix; (b) `prewarm.first_nav.latch` shows
  `reason=segment-complete` with segRank=0 and a login-cohort segKey; (c) the first keepwarm
  sweep window shows re-seeds under that same cohort — the 16-empty-binding-skip pattern is
  gone; (d) rank order identical across two consecutive boots of the same cluster state
  (`prewarm.engine.seed.rank` lines diff clean).

## 9. Falsifier plan (hermetic, existing seams — enumeratePrewarmTargetsForGVRFn /
restActionTargetGVRFn / seedOne*Fn / resetFirstNavLatchForTest / firstNavFireObserver)

- **F1 determinism arm** = R3-C1 (N-run shuffle, mutation RED).
- **F2 pollution arm** = R3-C2 (revert-to-first-seen mutation RED on the boot600 shape).
- **F3 tie-break arm**: two identities with equal widgetMax+allMax → order by identityKey ASC,
  stable across runs.
- **F4 keepwarm-repair arm** = R3-C4.
- **F5 segKey arms** = R3-C5 (both fixtures).
- **F6 set-equality arm** = R3-C3.
- **On-cluster proof** = R3-C8 grep set.

## 10. Options surfaced (strategic)

- **Q2 widget-less handling — recommended: order-last, keep seeding (chosen above).**
  Alternative (exclude from seed set) rejected: set change, FIX-E proof re-derivation, zero
  observed payoff. If Diego later wants machine-SA seeds gone entirely, that is a separate
  seed-SET ship with its own lossless proof — not rank hygiene.
- **Q1 tier-1 metric — recommended: widgetMax primary.** Alternative (allMax primary, single
  metric) is simpler by one field but re-admits within-tier pollution; called out in §2 for
  the gate to overrule if simplicity is preferred.
