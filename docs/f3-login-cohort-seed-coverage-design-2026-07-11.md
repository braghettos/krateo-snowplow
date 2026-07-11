# F3 — Login-cohort seed coverage (Fix #130 F3)

Date: 2026-07-11
Author: cache architect
Repo state: main @ 8b5163a (measured build 1.7.6)
Evidence: /tmp/f2-deploy/admin-hit-count/{admin-hit-ledger.txt, boot-t0.log, nav-full.log, pass2-full.log}

## TL;DR

Admin first-nav is 56.8% (76 cold cacheable widgets), 0 hits seed-attributable. The
admins cohort is NOT under-enumerated — it appears in EVERY widget target set (all
537 widget entries carry `targets:16` including `\x1fadmins`). It is
**enumerated-but-starved**: the rank-major seed (FIX-E) seeds ALL of rank-0 (devs,
widget_max 1293) first, and the serial per-widget resolve is so slow that the seed
processed only 169 widget targets before its deadline — never reaching admins at
rank 2. The `/readyz` first-nav latch (FIX-F) fires on the DEVS segment only, so the
pod goes Ready with admins' cells cold. attribution=0 is a coverage gap, NOT a
key-divergence bug (the 76 miss widgets are identity-UNDECLARED → their key does not
fold username → a completed admins seed WOULD hit). Fix: a UNIFORM login-cohort-first
rank discriminator + a first-nav latch that waits for EVERY login cohort's first-nav
segment, not just rank-0's.

## 1. TRACE — admins enumeration (Q1): enumerated-but-starved, NOT under-enumerated

### The dedup + rank mechanism (file:line)
- `EnumeratePrewarmTargetsForGVR` (internal/cache/prewarm_enumeration.go:128) collects
  every binding for a GVR (per-GVR bucket ∪ wildcard), then DEDUPS by representative
  identity: key = `rep.Username + \x1f + join(sorted rep.Groups)` (:190). For a
  GROUP-only cohort (admins granted via a ClusterRoleBinding to `Group=admins`),
  `pickRepresentativeFromSubjectKeys` (:249) picks the group subject → rep =
  {Username:"", Groups:["admins"]} → key `\x1fadmins`. `CollapsedBindings` counts how
  many raw bindings fold into that representative (:195/:207).
- The engine ranks identities over BOTH classes' target sets by
  (widgetMax DESC, allMax DESC, key ASC), where widgetMax/allMax are MAX-FOLDS of
  `CollapsedBindings` (prewarm_engine_boot.go:718-758). widgetMax = the collapsed
  count observed in some WIDGET target set.

### Why admins = widget_max 1 (TRACED, and why it is CORRECT)
`widget_max=1` for `\x1fadmins` means: admins is represented by ONE collapsed binding
(one CRB to Group=admins) and appears in widget target sets with CollapsedBindings=1.
devs = widget_max 1293 because the 1000-user + per-user-RoleBinding topology folds
1293 raw bindings into the single `\x1fdevs` representative. **widget_max is the RANK
METRIC (binding multiplicity), NOT the count of widgets the cohort can render.**

The decisive evidence that admins is fully enumerated (NOT blind to CRB-only cohorts):
- `prewarm.engine.seed.widget_targets`: 537 lines, EVERY ONE `"targets":16`
  (boot-t0.log). Every widget's per-binding target set has 16 representative
  identities — and `\x1fadmins` is one of them (it is rank 2 = present).
- `prewarm.enumerate.dedup`: every widget GVR shows `bindings:1308 identities:16`.
  admins is one of those 16 for EVERY widget GVR.

So the binding→widget mapping is NOT blind to the cluster-scoped/CRB-only admins
cohort. Admins is a first-class rank-2 identity across all 537 widgets. **Verdict:
enumerated-but-STARVED by rank + deadline, not under-enumerated.** (This falsifies the
"binding→widget mapping blind to CRB-only cohorts" hypothesis in the brief — the
enumeration is correct; the SCHEDULING starves it.)

### The starvation (TRACED, boot-t0.log timeline)
The seed is RANK-MAJOR (prewarm_engine_boot.go:970-1018): the outer loop walks
`ranked` in order; each rank's inner loops iterate all widget/RA seeds but only
process targets whose `identityKey(c)==rankKey`. So it seeds devs (rank 0: all its
widget targets + RAs), THEN portals (rank 1), THEN admins (rank 2).
- 14:20:54 Phase-1 warmup starts.
- 14:24:47 ranks emitted (devs r0, portals r1, admins r2, then 222 system-SA ranks).
- 14:25:54 `phase1.seed.sync_incomplete` err="context deadline exceeded"
  elapsed_ms=150746 → readiness flips as backstop.
- `prewarm.seed.abort phase=widgets targets_processed=169 elapsed_ms=396033` (14:31:23)
  — the widgets phase processed only **169** targets before the ctx cancelled.
  rank-0 devs alone is 537 widget targets; the seed never finished devs, so **admins
  (rank 2) was never reached.** The abort repeats (14:39:40 tp=227, 14:47:44 tp=264)
  — the boot re-walk re-enqueues and re-aborts, never converging.

### Why it is so slow (TRACED)
Each widget seed is a full nested resolve under `WithPrewarmIterSerial`
(withCohortSeedContext, phase1_pip_seed.go:440) — one resolve in flight, no
parallelism. `phase1.seed.widget.phase.timing` shows per-widget apiref_ms up to 669ms
(list-activity-events), and the discovery re-walk itself is 84-327s
(`prewarm.engine.boot.rewalk_complete`). 169 targets / 396s ≈ 2.3s/target. A
`marketplace-detail` operational_failure (null-jq, #128-class) at 14:45:50 triggers a
coalesced re-walk (prewarm_engine_boot.go:548-551) that resets progress — the seed
never converges past the early ranks.

## 2. TRACE — the 150.7s deadline (Q2)

There are TWO distinct bounds, and the "150.7s" is the pipSeed return, NOT
pipGlobalTimeout:
- `pipCohortTimeout = 120s` (phase1_pip_seed.go:132) — per-target-cohort ctx
  (seedOneTarget, prewarm_engine_boot.go:591).
- `pipGlobalTimeout = 8min = 480s` (phase1_pip_seed.go:141) — the seedCtx budget the
  SYNC seed runs under (phase1_walk.go:826).
- `PHASE1_TIMEOUT_SECONDS` — the outer Phase-1 backstop (p1Ctx, cache/phase1.go:323).

The `sync_incomplete` WARN (phase1_walk.go:831) fires when `pipSeed(seedCtx)` returns
a non-nil err — here `context deadline exceeded` at elapsed 150746ms. This is a CHILD
ctx cancel, not pipGlobalTimeout (480s) — the seed's parent Phase1Warmup ctx
(bounded by PHASE1_TIMEOUT_SECONDS) cancelled at ~150s. **Config-drift finding: the
deployed pod's PHASE1_TIMEOUT is ~150s, well short of the 8-min pipGlobalTimeout the
seed budget assumes** (chart default has historically been 180/300; this pod ran
~150s — the exact env is not dumped in the boot log; the developer must read the
live pod env at build time). The seed budget (480s) is dead because its PARENT
(PHASE1_TIMEOUT) cancels first.

What work was cut: the entire rank-1+ tail — portals, admins, and all system SAs.
The MarkPhase1Done backstop (phase1_walk.go:811 defer) fires on the cancel, flipping
Ready-degraded. **Did even the 1 admin widget land? NO** — the seed never reached
rank 2; admins got 0 widget cells (confirmed by attribution=0 + all 76 admin widgets
cold). The "1" in widget_max is a binding count, not a seeded-widget count.

**Q4 answer — intended best-effort or bug?** The "first /call per affected cohort
falls back to per-user resolve" fallback is the INTENDED C2 liveness backstop
(never-not-Ready-forever, phase1_walk.go:792-799). It is doing its job. But the
DESIGN INTENT that made it acceptable — that the first-nav segment for the served
cohort is warm before Ready — holds ONLY for rank-0 (devs). For every OTHER login
cohort the backstop degenerates to "always cold on first nav." So it is a
best-effort fallback that has become the DEFAULT path for admins = a coverage
defect, not a code bug in the fallback itself.

## 3. TRACE — attribution=0 sanity (Q3): NOT key-divergence; it is coverage

The seed cohort identity for admins is Username="" + Groups=["admins"]
(withCohortSeedContext sets `WithUserInfo{Username: cohort.Username, Groups:
cohort.Groups}`, phase1_pip_seed.go:432-435; cohort.Username="" for the group-rep).
The real admin browser is `{name:"admin", groups:"admins"}` (nav-full.log
resolved_cache.lookup `user:{name:"admin",groups:"admins"}`). These differ in
Username.

**But the key does not fold Username for the 76 miss widgets.** `DeclaredIdentity`
(internal/resolvers/widgets/widgets.go:110) returns nil unless the widget declares an
`identityContext` — the ~99% corpus path (:103-113). It is the SINGLE identity fold
site for both the key path (`effectiveKeyExtras` → `declaredIdentityForKey`) and the
resolve-input path. For an UNDECLARED widget the fold is a no-op → the cache key is
identity-INVARIANT (BindingUID "" — the A2/#95 mechanism) → seed key (Username="")
== browser key (Username="admin"). The 76 misses are all
statistics/cards/flexes/tags/buttons/listies/selects/layouts/images/menus/
paragraphs/linecharts/tables (ledger MISS breakdown) — none of which declare
identity. So a COMPLETED admins seed WOULD produce browser-hittable cells.

Therefore attribution=0 is fully explained by "the seed never ran for admins," NOT by
the A7 key-divergence class. The A7 definitive-arch cure holds for the admins shape:
undeclared widgets are group/user-invariant, so the group-rep seed key matches the
real admin key. (This must still be PROVEN by falsifier — see §7 arm (c) — because a
key-parity claim on the admins shape must be verified through REAL derivation on both
sides, per feedback_consultation_mutation_is_not_key_correctness.)

CAVEAT (identity-declared widgets): IF any first-nav admin widget declares
`identityContext:[username]`, its key DOES fold username → the group-rep seed
(Username="") would write a key the admin (Username="admin") cannot hit. The design
below handles this uniformly (§4 note) — the seed must resolve each cohort under a
representative that carries the key-folded identity dimensions, OR such widgets are
knowingly per-user and out of cohort-seed scope. Empirically the 76 misses are
undeclared, so this caveat is not the current gap — but the falsifier must include a
declared-widget arm to prevent a future regression.

## 4. DESIGN F3

Two levers, both uniform, no static lists, no "if admins":

### Lever 1 — LOGIN-COHORT-FIRST rank discriminator (the root fix)
Today rank = (widgetMax DESC, allMax DESC, key ASC). widgetMax is binding
multiplicity, so devs (1293 bindings) outranks admins (1 binding) — but BOTH are
login cohorts that render the dashboard, and the 222 system SAs that never touch the
frontend are interleaved by binding count too. The milestone ("ANY user first-nav =
100%") needs EVERY login cohort's first-nav segment warmed before the
non-frontend-reachable tail.

Add a PRIMARY rank key: **is this identity frontend-reachable?** = does it have ≥1
RootIndex==0 (first-nav) widget target? This is already computed for the latch
(prewarm_engine_boot.go:894-920). Promote it to a rank tier:

    sort by (isFirstNavReachable DESC, widgetMax DESC, allMax DESC, key ASC)

- UNIFORM discriminator: "cohort with a frontend-reachable first-nav nav segment,"
  derived from the SAME RootIndex==0 widget-target data the latch uses — not a
  hardcoded cohort list (satisfies feedback_no_special_cases +
  feedback_dynamic_cohort_prewarm_no_static_no_cold_fill).
- Effect: devs, portals(if reachable), admins, system:masters — every cohort that
  renders the dashboard — sort ABOVE the ~222 system SAs. The PURE-ORDERING invariant
  (FIX-E) is preserved: the seeded (unit×identity) SET is unchanged; only the SEQUENCE
  changes. No caps, no skips.
- Within the reachable tier, keep widgetMax DESC (devs first — the largest cohort is
  still highest-value), so this does not regress the devs-first property that made the
  95%-narrow mix fast.

Lever 1 ALONE is insufficient at this cluster because even the reachable tier
(devs+portals+admins+masters) exceeds the ~150s deadline at 2.3s/target serial. So:

### Lever 2 — first-nav latch waits for EVERY reachable cohort's first-nav segment
Today FIX-F fires `/readyz` the instant the FIRST reachable identity's (segKey at
segRank) RootIndex==0 segment completes (prewarm_engine_boot.go:1000-1005). Post-Lever-1
that is still just devs. Change the latch to require ALL first-nav-reachable
identities' RootIndex==0 segments to complete before firing — i.e. arm
`firstNavRemaining` as the SUM over every reachable cohort's RootIndex==0 targets, and
fire when it reaches 0 (or the PHASE1_TIMEOUT/pipGlobalTimeout backstop, unchanged).

- This makes readiness mean "every login cohort's dashboard is warm," which IS the
  milestone. Combined with Lever 1's ordering, the reachable cohorts seed FIRST and
  the pod does not go Ready until they are all warm.
- Backstop preserved: MarkPhase1Done still fires regardless on
  PHASE1_TIMEOUT/pipGlobalTimeout expiry (C2 liveness) — so a pathological cohort
  cannot hang readiness forever; it just goes Ready-degraded for that cohort (today's
  behavior, but now the exception not the rule).
- The reachable-cohort segment count is small: 16 identities × ~179 first-nav widgets
  is the ceiling, but only the ~4 reachable cohorts × ~76 RootIndex==0 widgets ≈ 300
  targets matter. That is the readiness-gating work.

### Ordering / boot-budget interaction (MANDATORY — the deadline is real)
Lever 1+2 reorder work but do not make it faster. At 2.3s/target serial, ~300
reachable-cohort first-nav targets ≈ 690s — OVER the ~150s PHASE1_TIMEOUT AND the
480s pipGlobalTimeout. Two required companions (NOT new mechanisms — existing knobs):
1. **Raise PHASE1_TIMEOUT so pipGlobalTimeout (480s) is actually the seed's bound**
   (Q2 config-drift). The seed budget was designed as 480s; the parent cancelling at
   ~150s is the drift. This is a CHART VALUE change, not code (route to the chart
   session). Without it the seed budget is dead.
2. **The serial per-target cost is the structural ceiling** (this is #123 — "seed is
   the next boot-budget ceiling, ~339s of 480s at 50K"). F3 does NOT solve #123; it
   makes the reachable cohorts WIN the budget they have. If even the reachable tier
   overruns the raised budget, the residual is bounded per-cohort (Lever-2 latch fires
   at backstop with whatever seeded) and the remaining cohorts fall back to per-user
   resolve — strictly better than today (devs-only). A follow-up (bounded seed
   PARALLELISM for the reachable tier, or a per-cohort budget slice) is the #123 lever;
   flag it, do not fold it into F3.

### The marketplace-detail re-walk churn (contributing, route separately)
The operational_failure (null-jq, #128) re-enqueues the boot re-walk
(prewarm_engine_boot.go:548-551), resetting seed progress and burning budget. This is
the #128 portal-RA null-jq issue, already tracked. Note it as a budget amplifier that
F3's reordering does not fix; #128's resolution removes the re-walk thrash.

## 5. Seed-attribution observable (brief requirement)

Add a counter/log tag so this measurement stops needing forensics. At the resolved
serve-hit site (resolved_cache.lookup), when `hit==true`, compare the hit entry's
provenance: stamp a `seeded_at_boot bool` on the ResolvedEntry at seed Put time
(seedOneWidget/seedOneRestaction Put) and surface it in the lookup log as
`hit_source:"seed"|"traffic"`. Aggregate a `resolved_cache.hits_seed_attributable`
expvar. This is leak-safe (boolean provenance, no per-user data) and directly answers
"did the seed warm this cell." Place the stamp at the two seed Put primitives (single
insertion each, feedback_no_special_cases). LOC ~15.

## 6. Budget analysis (50K, #123)

At this cluster (~5K deployments, 60K benchapps) the reachable-tier first-nav seed is
~300 targets × 2.3s ≈ 690s serial. At the canonical 50K/1.3M-object scale the per-
target resolve cost rises (larger LISTs, more RBAC bindings → 1293→higher collapsed
counts, more per-cohort fan-out). #123 already pins seed at ~339s of a 480s budget at
50K with the CURRENT devs-only-effective seeding. F3 does NOT increase the seeded SET
(pure reorder), so aggregate seed cost is unchanged — but the READINESS-GATING subset
grows from "devs first-nav segment" to "all reachable cohorts' first-nav segments."
That raises the time-to-Ready. Mitigations in priority order:
1. Raise PHASE1_TIMEOUT to ≥ pipGlobalTimeout (chart) — reclaims the dead 330s.
2. Bounded reachable-tier parallelism (the #123 lever; NOT F3) if the raised budget
   still overruns. Must respect the #46 SEED_FOOTPRINT_BUDGET_BYTES aggregate bound
   (feedback_bounding_mechanism_discipline: cost-proportional, after cache-hit,
   >cap concurrency in the falsifier).
State clearly: F3 targets the 0.05 admin mix + every non-devs login cohort; it trades
a longer time-to-Ready for correct first-nav coverage. If Diego prefers time-to-Ready
over admin-cohort coverage, Lever-2 can be scoped to "reachable cohorts up to a budget
fraction" — a strategic call (§8).

## 7. Falsifiers (PM gate)

- **(a) Hermetic RED — rank discriminator (K>1 cohorts × M>1 widgets).** Build a
  binding index with ≥2 login cohorts (devs 1000-binding, admins 1-binding CRB) + ≥2
  system SAs, ≥2 first-nav widgets. Assert post-Lever-1 rank order places BOTH login
  cohorts above BOTH system SAs, regardless of widgetMax. RED arm = pre-fix rank
  (widgetMax DESC only) interleaves a system SA above admins. Degenerate K=1/M=1 is
  inadmissible (feedback_falsifier_shape_must_discriminate).
- **(b) Hermetic RED — latch waits for all reachable cohorts.** ≥2 reachable cohorts;
  assert the latch does NOT fire until BOTH cohorts' RootIndex==0 segments complete.
  RED = pre-fix latch fires on the first cohort (segRank) with the second still cold.
- **(c) Key-parity through REAL derivation (attribution proof, no hand-fed keys).**
  Drive an UNDECLARED first-nav widget through the REAL seed derivation (cohort
  Username="", Groups=["admins"]) AND the REAL browser derivation (Username="admin",
  Groups=["admins"]); assert the pre-hash ResolvedKeyInputs are FIELD-equal (BindingUID
  "" both sides), then that ComputeKey digests match. PLUS a DECLARED-widget arm
  (identityContext:[username]) proving the keys DIVERGE (guards the §4 caveat).
  curl probes INADMISSIBLE (feedback_curl_probes_inadmissible_for_seed_hit_acceptance);
  both sides must be real derivation (feedback_key_parity_golden_real_inputs_prehash_diff).
- **(d) Acceptance — repeat the exact reboot+counted admin first-nav.** Fresh boot,
  real Chrome admin session, per-/call resolved_cache.lookup counted, first-touch per
  key_hash. REQUIRE: `hits_seed_attributable > 0` (the new observable) AND admin
  first-nav hit-rate ≥ **85%** (justification below). Contamination control: the
  ledger notes prior admin traffic inflated the HIT side; run from a genuinely cold
  boot with NO prior admin session.

### Achievable target justification (the 85%)
100% first-nav is the milestone aspiration but not guaranteed on the FIRST reboot at
this cluster because (i) the marketplace-detail null-jq re-walk (#128) still burns
budget until fixed, and (ii) a few first-nav widgets resolve external/ClickHouse data
(#129) that the seed cannot warm identically. 85% is the floor that PROVES the fix
works: it requires the admins cohort seed to have RUN (attribution>0) and covered the
bulk of the 76 currently-cold widgets. Steady-state stays ~97.7% (measured). If
PHASE1_TIMEOUT is raised AND #128 lands, 100% is reachable — set 85% as the F3 gate,
100% as the milestone-close target after #128/#129.

## 8. Options with recommendation (strategic — surface to Diego/TL)

- **Option A (recommended): Lever 1 + Lever 2 + PHASE1_TIMEOUT raise.** Correct
  milestone semantics (Ready = every login cohort warm). Cost: longer time-to-Ready.
- **Option B: Lever 1 only (reorder, keep devs-only latch).** Admins seeds sooner
  (before system SAs) but readiness still flips on devs; admins may still be cold if
  the budget cuts before rank-2 completes. Weaker, but zero time-to-Ready regression.
- **Option C: Lever 1+2 but Lever-2 bounded to a budget fraction.** Ready when
  reachable cohorts up to X% of budget are warm; residual cohorts fall back. Balances
  coverage vs time-to-Ready.
Recommend A (the milestone is explicit: "ANY user first-nav = 100%"), with the
PHASE1_TIMEOUT raise as a hard prerequisite and #128 as a fast-follow.

## 9. REACHABILITY (the F1 lesson — prove the deployed binary reaches the new code)

- The seed engine ran on the measured 1.7.6 boot: `prewarm.engine.seed.rank` (225
  lines), `prewarm.seed.abort`, `prewarm.first_nav.latch reason=segment-complete
  segment_identity=\x1fdevs` all present in boot-t0.log. So the deployed binary
  reaches the rank sort (prewarm_engine_boot.go:750) and the latch arming (:894-920,
  :1000-1005) on every boot — both Lever-1 and Lever-2 sit on the unconditional seed
  path, no flag gates them.
- Lever 1 changes the existing `sort.SliceStable(ranked, ...)` comparator (:750) — the
  same call that emitted the 225 rank lines. Lever 2 changes the existing
  `firstNavRemaining` arming (:894-920) + fire condition (:1000-1005) — the same code
  that fired the observed segment-complete latch.
- Production call chain: main boot → Phase1Warmup → phase1WarmupWith Step 7.6 →
  pipSeed(seedCtx) → seedScopeYielding → the rank sort (Lever 1) + the rank-major loop
  + latch (Lever 2). No env gate.
- Falsifier (d)'s on-boot `hits_seed_attributable > 0` is the deployed-binary reach
  proof: a non-zero seed-attributable admin hit on a real reboot proves the reordered
  seed reached admins and wrote a browser-hittable cell.
- The seed-attribution observable (§5) is itself the standing reachability probe —
  every future boot reports whether the seed warmed admin cells, so this never needs
  forensics again.

---

## 10. AMENDMENT (2026-07-11, Diego directive) — SA-cohort seed EXCLUSION

Diego amended the F3 scope mid-build: rather than only RE-RANKING ServiceAccount
cohorts below login cohorts (Lever 1's "system SAs sort last" half), **exclude
ServiceAccount-subject cohorts from the seed target set entirely.** They are
machine/controller cohorts that never render the frontend, and seeding them
starved the login cohorts out of the boot budget.

### Discriminator (arch-reviewed, C-F3-6 paths corrected)
- Implemented in `EnumeratePrewarmTargetsForGVR`
  (**internal/cache/prewarm_enumeration.go**) via `allSubjectsAreServiceAccountKind`:
  a binding is excluded iff its projected subject SET is non-empty and contains
  ZERO User and ZERO Group subjects (every subject is `rbacv1.ServiceAccountKind`).
- **SET predicate, NOT the picked representative's kind (arch-caught latent bug):**
  `pickRepresentativeFromSubjects` (match_subject.go) is first-non-empty-BY-SLICE-ORDER,
  not kind-priority — a MIXED `[SA-first, Group-second]` binding would pick the SA
  as representative. Gating on the winner's kind would WRONGLY drop that
  login-cohort binding. The set predicate keeps any binding with a User/Group
  subject. Falsifier `TestF3SAExclusion_MixedSubjects` is the RED-catcher (the
  naive winner-kind gate drops the mixed binding — verified RED).
- **Subject KIND, not a name allowlist:** `system:masters` is a GROUP subject and
  is KEPT despite the `system:` name (feedback_no_special_cases). A `system:`
  name-prefix filter would wrongly exclude that login cohort.

### portals-SA safety (safety-check #2 — CLOSED)
`system:serviceaccount:krateo-system:portals-v1-5-1` has widget_max=1 (it renders
a widget) and is now excluded. CLOSED as safe: it is a CDC/loopback mechanism SA,
not a browser login identity — no human session depends on its seeded cell. If it
issues a real /call its cell cold-fills as a TRAFFIC entry (never-worse arm
`TestF3SAExclusion_NeverWorse_SATrafficStillServes`). The #57 snowplow loopback SA
is a seed MECHANISM identity (it RUNS the seed), never itself a seed TARGET in the
index, so the exclusion does not touch it.

### Interaction with Levers 1+2
- The exclusion SUPERSEDES the "system SAs sort last" half of Lever 1: SAs are gone
  from the seed set, so there is no SA tail to sort below. Lever 1's
  `firstNavReachable` primary rank tier STILL applies — it now orders the surviving
  LOGIN cohorts (a login cohort with a first-nav widget above one with only
  non-first-nav widgets).
- Lever 2 (latch waits ALL reachable cohorts) is unchanged — the reachable set is
  now login cohorts only.

## 11. C-F3-6 path corrections (applied)
- `prewarm_engine_boot.go`, `phase1_pip_seed.go` — **internal/handlers/dispatchers/**.
- `prewarm_enumeration.go` — **internal/cache/**.
- `DeclaredIdentity` — **internal/resolvers/widgets/widgets.go**.

## 12. Seed-budget mechanism (Q2 finalized) — the chart companion is a RE-RAISE
TRACED on the 1.7.6 boot: the Step-7.6 seed runs under
`seedCtx = context.WithTimeout(phase1Ctx, pipGlobalTimeout=480s)`
(phase1_walk.go:826) — it INHERITS the phase-1 deadline. Effective seed budget =
`min(PHASE1_TIMEOUT − preSeedElapsed, 480s)`. Pre-seed phases (roots-walk ~150s +
content-prewarm ~11s + discovery re-walk ~100s) consumed ~251s before the seed
started. So at PHASE1_TIMEOUT=300 the seed got ~49s (starved). Chart companion:
PHASE1_TIMEOUT_SECONDS 180→900 (snowplow-chart values.yaml) so 900−251=649 > 480 →
the seed's own 480s cap is the real bound. Re-measure preSeedElapsed at 50K and
re-raise if 900−preSeed < 480. Trade (Option A): longer worst-case time-to-Ready;
the Lever-2 latch flips Ready at min(all-reachable-warm, backstop) so the common
case is unaffected.
