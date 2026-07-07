# keepwarm c2 — cohort-coverage extension of the G-TTL sweep (design, 2026-07-07)

Repo: main @ 6d8af79 (1.6.3 line, Fix-3 rank hygiene at tip). Design + BUILD (as-built note §9). Prior docs: docs/g-ttl-quiet-page-keepwarm-design-2026-07-04.md (c1),
docs/fix3-rank-hygiene-design-2026-07-07.md (Fix-3). Live motivating evidence: fresh2 60K,
1.6.2 pod at 3.4h uptime served the ADMIN user 92 misses / 108 calls with 12–15s cold
statistic/listy resolves (team-lead observation 2026-07-07).

## 0. Prior-art check

- This is still refresh-ahead caching (Caffeine `refreshAfterWrite`); client-go has no L1
  facility. What client-go DOES already give us is the completion machinery: the engine's
  `workqueue.TypedRateLimitingQueue` (prewarm_engine.go:280-284) with AddRateLimited requeue +
  Forget-on-success (prewarm_engine.go:492-508, F.4) is exactly the "cut chunk resumes
  deterministically" primitive c2 needs at scale. Nothing new to import; c2 composes with it.
- No client-go primitive tracks "identities recently served" — predicate (c) would be
  hand-built state (§2.3).

## 1. Root cause of the motivating symptom (TRACED)

1. The #102 c1 sweep bound is `if rank1Only && ri > 0 { break }` over the Fix-3 ranked slice
   (prewarm_engine_boot.go:926-928) — ranked[0] ONLY. Post-Fix-3 ranked[0] is the dominant
   widget-capable cohort (devs on fresh2), deterministically (prewarm_engine_boot.go:737-745).
2. The admin identity is never ranked[0]: admin's wildcard binding lands in every GVR bucket
   (prewarm_enumeration.go:145-147 wildcard union) so admin IS widget-capable (widgetMax≥1),
   but its CollapsedBindings collapse is far below devs' per-composition RoleBinding
   population, so admin sits at rank ≥1 and the c1 bound never reaches it.
3. L1 TTL expiry is lazy-on-Get with CreatedAt sliding only on Put (resolved.go:787-791,
   :807-808); the refresher never touches quiet cells (g-ttl design §1.3). So every admin cell
   whose data goes quiet dies at TTL=3600s and NOTHING re-creates it → each admin
   return-after-idle pays the full 60K cold resolve set. That is the observed 92-miss window.
4. Symptom-disappear check: putting admin (and every widget-capable login cohort) inside the
   sweep set means its cells are re-Put each cycle before expiry → a return-after-idle is
   l1:HIT. The fix targets the exact mechanism producing the misses.

c1's own design scoped this deliberately ("c1 now, c2 refinement", g-ttl §3c); c2 is the
data-justified extension, not a defect fix in c1.

## 2. Q1 — THE SWEEP SET predicate

Candidate predicates, adjudicated:

**(a) all widget-capable identities (widgetMax ≥ 1)** — the Fix-3 tier boundary
(prewarm_engine_boot.go:688-696: CollapsedBindings ≥ 1 per observation, so widgetMax ≥ 1 ⇔
the identity appears in some widget target set). Structural, derived, deterministic, and —
decisive — a CONTIGUOUS PREFIX of `ranked` post-Fix-3 (widgetMax DESC is the primary sort
key), so the sweep bound stays a pure loop bound: `break` at the first widgetMax==0 entry.
Caveat the brief raised: widget-capable ⊅ login cohort — a machine SA CAN hold widget
bindings in some topologies and would be swept. Adjudication: snowplow has no login-ness
oracle (login identity lives in authn; the frontend consumes RAs only widget-mediated, Fix-3
§3 TRACED against the SPA source) — any "login cohort" predicate would be either a name
heuristic (feedback_no_special_cases violation) or an authn coupling (new seam). Capability
is the only sound derived predicate available; the waste for a hypothetical widget-capable
machine SA is bounded by its segment cost and is exactly what refinement (c) would trim.

**(b) top-N ranks — REJECTED.** N is a magic count with no data derivation; the brief and
feedback_prewarm_walk_no_sampling_caps both reject it.

**(c) activity-driven (identities that SERVED a real /call within the last TTL window) —
DEFER, data-gated.** Cost-proportional to real usage, no magic count — attractive on paper.
Traced reality:
  - NO served-identity recency record exists anywhere (grep lastServed/recency/servedIdentity
    over internal/ = empty; the dispatcher tracks only the in-flight counter
    prewarm_engine.go:118 and aggregate counters). It would be NEW state: a per-identity
    last-served map + locking + staleness eviction + restart semantics.
  - The bridge problem is worse than the state. The serve side's identity is the JWT tuple
    and the cell key folds a RE-DERIVED first-match BindingUID
    (helpers.go:227-270 dispatchCacheLookupKey → rbac.EvaluateRBAC); the sweep's rank unit is
    the binding REPRESENTATIVE tuple (prewarm_enumeration.go:178-190), and PrewarmTarget's
    BindingUID field is explicitly DIAGNOSTIC-ONLY, not the cell-key value
    (prewarm_enumeration.go:67-73). Serve tuple (cyberjoker, [devs]) ≠ seed tuple ("", [devs]).
    Matching them requires re-deriving EvaluateRBAC per (identity × GVR) at sweep-plan time
    and comparing against recorded served BindingUIDs — a divergent-producer/consumer
    derivation pair, the exact class feedback_consultation_mutation_is_not_key_correctness and
    the #64 saga warn about. Provable, but it needs its own G6-style subset lemma + REAL-
    derivation falsifier arms.
  - Restart cold-hole: the record is in-memory; after a deploy no cohort is "active" until it
    navigates once — the motivating admin symptom would recur once per deploy per cohort.
  - Payoff check against the customer mix (0.95 narrow + 0.05 admin): both dominant cohorts
    are active essentially always, so (c)'s savings accrue only on the singleton-user /
    machine-SA tail — the cheapest segments (narrow RBAC → small filtered lists). High
    machinery, small win, at odds with project_caching_is_provisional (keep layers removable).
  Verdict: hold (c) as the refinement, gated on live data (§7 trigger), with the bridge lemma
  as its entry cost.

**(d) all widget-capable, rank-ordered, budget staggers the tail — CHOSEN, as (a)+completion.**
With the keepwarm-mode age-skip (§4.2) and F.4 requeue, the budget does not CUT the tail —
it staggers it across chunks and the sweep completes cost-proportionally. So (d) degenerates
to "(a), swept in the already-deterministic ranked order, chunked by the existing 8m scope
budget". No new ordering, no new knob.

**Chosen: sweep set = the widget-capable prefix of `ranked` (widgetMax ≥ 1), in ranked order,
single keepwarm scope, F.4-chunked.** Machine-SA-with-widget-bindings inclusion is accepted
and documented (falsifier arm F-C2c makes the inclusion explicit); (c) is the deferred trim.

## 3. Q2 — cost model

Identity cardinality: the rank unit is the representative (Username, sorted-Groups) tuple per
binding (prewarm_enumeration.go:188-190). Group-subject RBAC collapses the user population
into group cohorts — 50K bindings → ~16 identities observed on fresh2
(project_50k_widget_gvr_uniform_wildcard_floor). Two regimes at the 1000-user / 50K canon:

- **Group-cohort RBAC (the observed customer shape, 0.95 narrow mix):** identities stay
  O(dozens) regardless of user count (users collapse into devs-like groups). Fresh2 60K
  estimate: devs pass ≈ 190s measured (c1); admin (broad RBAC, 12–15s heavy widgets) ≈
  200–400s INFERRED; ~14 narrow singleton cohorts ≈ 10–30s each INFERRED (uniform wildcard
  floor puts every identity in every widget bucket, but narrow RBAC filters lists small).
  First full sweep ≈ 12–17 min. STEADY-STATE sweeps are strictly cheaper: the age-skip (§4.2)
  skips every cell the refresher or a customer Put already refreshed this window, so a cycle
  re-resolves only genuinely-quiet cells. Duty ≈ 13min/34min ≈ 30–40% of ONE background
  worker worst-case first cycle, engine-yielded (yield-on-inflight before every target,
  prewarm_engine.go:521-535 + seedScopeYielding checkpoints), memory-bounded per unit by #46
  enterSeedUnit (phase1_pip_seed.go:559) — customers keep absolute priority; the falsifier
  includes a p95-flat arm.
- **Per-user-binding worst case (every user individually bound + widget-capable):** identities
  ≈ users (1000). Σ segment costs exceeds the sweep window → the cycle can no longer cover
  everyone. Behavior is GRACEFUL by construction: ranked order = population DESC, so the mass
  cohorts stay covered; tail cohorts degrade to best-effort (re-Put interval grows beyond
  TTL → occasional cold, never worse than today). This regime is the explicit (c) trigger
  (§7). No flat cap is added — cost stays proportional to the topology's real cohort count
  (feedback_bounding_mechanism_discipline: cost-proportional, not flat-worst-case).

Bounding stack (all existing, composed, no additions): engine yield (per target) → #46
per-unit adaptive memory gate → per-target pipCohortTimeout → 8m per-scope chunk budget →
F.4 rate-limited requeue → queue coalescing on "keepwarm".

## 4. Mechanism

### 4.1 Sweep-set bound (the Q5 plumbing)

Replace the two accreted bools (`rank1Only`, `bootScoped`) threading through
rePrewarmBootScoped → seedScopeYielding → seedOneWidget/seedOneRestaction with ONE mode enum:

```go
type seedScopeMode int
const (
    seedModeBoot          seedScopeMode = iota // all ranks; F.4 live-cell fresh-skip
    seedModeKeepwarm                            // widget-capable prefix; age-skip (§4.2)
    seedModeGVRDiscovered                       // all ranks; NO skip (must record dep edges)
)
```

The set is DERIVED, not configured — no env, no list. Loop bound
(prewarm_engine_boot.go:926-928) becomes:

```go
if mode == seedModeKeepwarm && ranked[ri].widgetMax == 0 {
    break // widget-capable tier exhausted (Fix-3 prefix property)
}
```

c1's rank-1 bound is deleted (superseded, not parameterised). The enum also retires the
illegal 4th state the two bools admitted (rank1Only && bootScoped) and pins each mode's skip
semantics at the type level. `rePrewarmKeepwarm` (prewarm_engine_boot.go:214-219) passes
seedModeKeepwarm; handlers for boot/gvr-discovered pass their modes; ticker, scope kind, queue
key ("keepwarm"), cadence (TTL×3/4, prewarm_keepwarm_sweep.go:60-71) all UNCHANGED.

### 4.2 Keepwarm age-skip — completion at scale without new state

Problem (TRACED): prewarmScopeTimeout returns pipGlobalTimeout=8m for EVERY scope kind
(prewarm_engine.go:393-395; phase1_pip_seed.go:141). A full-cohort sweep at 60K (§3, ~13min)
always overruns one chunk → deadline-cut → F.4 AddRateLimited requeue → today's keepwarm
(bootScoped=false, no skip) would restart from rank-1 and re-pay the whole prefix every
chunk — a non-completing loop. c1 never hit this (190s < 8m); c2 structurally does.

Fix: a keepwarm-mode AGE-SKIP in the shared seed primitives, at the same site as the F.4
boot fresh-skip (phase1_pip_seed.go:538-551 restaction / :778-792 widget — before
enterSeedUnit, consuming the SAME production key the Put uses, single-derivation-site rule
F4-C2a):

```go
skip iff entry live AND time.Since(entry.CreatedAt) < ResolvedCacheTTL() - keepwarmSweepInterval()
```

The threshold is TTL − TTL×3/4 = TTL/4 — derived from the two existing constants, no knob;
if the cadence ratio ever changes the threshold follows. Properties:

- **Guarantee.** A skipped cell has age < TTL−P (P = sweep period), so it survives to the
  next cycle's examination at age < TTL — never expires between sweeps. A non-skipped cell is
  re-resolved and re-Put now. Under Fix-3-deterministic sweep positions each covered cell's
  re-Put interval ≈ P < TTL; worst-case jitter (chunk backoff, yields) is one cold window,
  self-healing next cycle — stated honestly, not hidden.
- **Chunk resume is cost-proportional.** Cells re-Put by an earlier chunk of the same cycle
  have age ≈ minutes ≪ TTL/4 → skipped → the requeued continuation pays preamble + remainder
  only. This is F.4's boot property, recovered for keepwarm with zero cycle-tracking state
  (no cycle timestamp, no scope payload — dedup on the bare "keepwarm" key is preserved).
- **Refresher/customer dedup for free.** A cell the event-driven refresher (or a customer
  Put) refreshed within the last TTL/4 is skipped — the sweep stops redundantly re-resolving
  churny cells, recovering most of option (b)'s cost-proportionality argument (g-ttl §3b)
  with none of its machinery. This tightens c1's behavior too (today c1 re-Puts rank-1
  unconditionally each sweep).
- **Backstop intact (GTTL-1 restated).** The skip never extends a lifetime — it only elides a
  redundant re-resolve of a cell that is provably young. Every sweep Put remains a fresh
  RE-RESOLVE, and declineSeedPutOnError (phase1_pip_seed.go:647, :868, :1030) still declines
  degraded re-resolves, so a persistently-failing cell TTL-expires — TTL stays the load-
  bearing staleness outer net (g-ttl §1.6). Nothing here is TTL-extend-on-read (option (a)
  stays rejected).

The entry's CreatedAt is Put-time-stamped (resolved.go:807-808); handle.Get already applies
the exact effectiveTTL liveness the serve path uses (resolved.go:787-791). If the cacheHandle
seam doesn't expose CreatedAt on the returned entry, a read-only accessor is the only cache-
side touch (≤5 LOC).

### 4.3 Q3 — staggering, adjudicated

- **Per-cohort scopes (one queue key per identity) — REJECTED.** Needs a planner tick handler
  that pre-computes the ranking to fan out scopes (the ranking today is computed inside the
  seed from a fresh snapshot — prewarm_engine_boot.go:733-745); every cohort scope re-pays
  the full preamble (nav re-walk + index rebuild + per-unit targetsFor precompute) × N
  cohorts; dynamic key cardinality tracks identity count (stale keys on RBAC shifts). More
  machinery, worse cost.
- **One scope, larger keepwarm budget (e.g. TTL/2) — REJECTED.** A monolithic multi-10-minute
  scope occupancy makes a CRUD boot/gvr-discovered scope wait the whole sweep (single worker,
  FIFO) — breaks the one-budget-max-delay fairness c1 relied on.
- **One scope + 8m chunks + age-skip resume — CHOSEN.** The workqueue is the stagger: a
  deadline-cut chunk requeues with backoff (prewarm_engine.go:491-503), and any boot /
  gvr-discovered scope enqueued meanwhile runs FIRST (FIFO + the requeue's rate-limit delay)
  — CRUD re-prewarm fairness is preserved with max delay = ONE chunk (≤8m), same as today.
  Ticks arriving mid-sweep still coalesce on the "keepwarm" key. Expected steady-state logs:
  `prewarm.engine.scope_incomplete`/`scope_requeued` lines per chunk are NORMAL for keepwarm
  at scale — the completion signal is the final chunk's `prewarm.engine.boot.complete
  scope=keepwarm` (observability note, R3-C8-style grep set in §6).

### 4.4 Q4 — TTL interplay (intent confirmed)

Yes, deliberately: for widget-capable cohorts the L1 becomes effectively never-expiring —
BUT freshness is maintained by RE-RESOLUTION each cycle (fresh bytes, error-gated), and TTL
remains the staleness backstop for anything the sweep can't refresh (persistent resolve
failures, #36 short-TTL overrides, widget-less identities' RA cells, cells beyond a degraded
tail). GTTL-1 invariant restated verbatim: the sweep NEVER extends the life of existing
bytes. Provisional-cache posture preserved: everything rides the existing implicit-on-cache
gate (PrewarmEngineEnabled → cache.PrewarmEnabled, prewarm_engine.go:89-91); back-out =
CACHE_ENABLED=false kills prewarm+engine+ticker together; no new flag to strand.

### 4.5 LOC bound + touched sites

~70–90 LOC total (excl. tests):
- seedScopeMode enum + threading (rePrewarmBoot/rePrewarmKeepwarm/rePrewarmGVRDiscovered,
  rePrewarmBootScoped, seedScopeYielding, seedOneWidget, seedOneRestaction signatures) —
  prewarm_engine_boot.go:202-221, :468-470; phase1_pip_seed.go:460, :699. ~30 LOC net.
- Loop bound swap (prewarm_engine_boot.go:926-928) — ~4 LOC.
- Age-skip blocks (mirror of :538-551 / :778-792, keepwarm branch) — ~20 LOC + counter
  (keepwarmAgeSkipTotal expvar alongside pipSeedFreshSkipTotal).
- Optional CreatedAt accessor on cacheHandle — ≤5 LOC.
- Comment/doc updates (c1 header prewarm_keepwarm_sweep.go:13-38 rank-1 wording → widget-
  capable tier).
No prewarm_keepwarm_sweep.go mechanism change (ticker/cadence/enqueue untouched); no
resolved.go store change beyond the possible accessor; no scope-key change.

## 5. PM-gateable conditions

- **C2-C1 (sweep set).** On a fixture with widget-capable identities W1(count 200), W2(5) and
  a widget-less identity M (RA-only, count 1344): keepwarm mode seeds W1 AND W2's targets and
  NONE of M's. Mutation arms: (i) restore rank-1 bound → RED (W2 unseeded); (ii) drop the
  widgetMax==0 break → RED (M seeded = boot behavior leaking into keepwarm).
- **C2-C2 (machine-SA shape, predicate honesty).** A machine SA WITH a widget binding
  (widgetMax≥1) IS swept — asserted explicitly so the capability-not-login-ness semantics are
  pinned, not accidental. Documented as the (c) refinement's target.
- **C2-C3 (age-skip correctness).** Cells Put < TTL/4 ago are skipped (primitive not entered,
  keepwarmAgeSkipTotal++); cells older are re-resolved and re-Put with fresh CreatedAt.
  Mutation: remove the age term (skip on bare liveness, the boot predicate) → RED (nothing
  re-Puts; the sweep is a no-op — the F4-C3 boundary c1 stated).
- **C2-C4 (completion under chunking).** With prewarmScopeTimeoutFn shrunk (existing F.4
  seam, prewarm_engine.go:405) so the sweep deadline-cuts mid-cohort: the requeued
  continuation age-skips the already-swept prefix and completes the remaining cohorts; total
  seed-primitive invocations across chunks = one full sweep set (no re-pay, no loss).
  Mutation: force no-skip in keepwarm mode → RED (chunk 2 re-seeds the prefix; tail starved).
- **C2-C5 (fairness).** A scopeKindBoot enqueued between keepwarm chunks runs BEFORE the
  requeued keepwarm continuation (max CRUD delay = one chunk). Reuses the F.4 straddle
  falsifier machinery.
- **C2-C6 (backstop, GTTL-1 evolution).** A cell whose re-resolve persistently stage-errors is
  Put-declined (declineSeedPutOnError) every sweep and TTL-expires — the outer net survives.
  (c1 ARM-BACKSTOP re-run under the new mode.)
- **C2-C7 (cost / no-knob).** Diff-scope check: no new env var, no static list, no numeric
  cap; sweep-set = widgetMax tier, threshold = TTL − interval, both derived. Per-cycle
  seed-primitive invocation count on a K-identity fixture = Σ widget-capable quiet-cell
  segments only (churny/fresh cells skipped).
- **C2-C8 (per-user-keyed L1 untouched).** No change to key derivation or cell sharing —
  cells stay BindingUID-keyed per identity (grep: dispatchCacheLookupKey/ComputeKey diff-
  clean; feedback_l1_per_user_keyed_never_cohort).
- **C2-C9 (c1 arms' evolution — every row runs, === RUN verified).** Enumerated in §6.
- **C2-C10 (on-cluster symptom-disappear, fresh2 60K).** After one full sweep cycle: (a) an
  admin /call replay after >1h idle is l1:HIT with fresh content (the 92-miss window shape
  gone); (b) sweep-window logs show re-seeds under admin + the narrow cohorts, not devs only;
  (c) evictTTLTotal for widget-capable cohorts' cells ≈ 0 over a multi-hour soak; (d) /call
  p95 flat during sweep windows (storm band); (e) final chunk logs `scope=keepwarm ...
  complete` each cycle (completion, not thrash).

## 6. Falsifier plan (hermetic seams: enumeratePrewarmTargetsForGVRFn, seedOne*Fn,
prewarmScopeTimeoutFn, runKeepwarmSweepLoop short-interval driver — all existing)

New arms: F-C2a sweep-set + both mutations (=C2-C1); F-C2b widget-less RED boundary (in
C2-C1); F-C2c machine-SA-with-widget-bindings inclusion (=C2-C2); F-C2d age-skip GREEN +
no-op mutation RED (=C2-C3); F-C2e chunk-completion straddle (=C2-C4); F-C2f fairness
interleave (=C2-C5); F-C2g cost-count arm (=C2-C7).

c1 arm evolution (prewarm_keepwarm_sweep_test.go + prewarm_first_nav_latch_segment_test.go +
resolved_keepwarm_reresolve_test.go):

| c1 arm | c2 fate |
|---|---|
| ARM-KEEPWARM (ticker cadence → enqueue → re-Put resets CreatedAt) | GREEN unchanged (mechanism untouched); the re-Put assertion must seed a cell OLDER than TTL/4 or the new age-skip elides it — fixture ages adjusted, semantics identical |
| GTTL-3 / ARM-SCOPE (rank1Only seeds devs only; mutation rank1Only=false touches ops → RED) | EXPECTATION INVERTS BY DESIGN: both devs and ops are widget-capable → both swept. Rewritten as C2-C1 (the RED boundary moves from rank-2 to widgetMax==0) |
| ARM-BACKSTOP (declined Put → TTL expiry survives) | GREEN re-run under seedModeKeepwarm (=C2-C6) |
| TestKeepwarmSweep_RankOne_SeedsRankZeroNotSegment (post-Fix-3: sweep seeds ranked[0] W only) | Widens: sweep seeds W AND V (both widget-capable); the #99b ruling it encodes (segRank never retargets the sweep bound) stays honored — the bound is the widgetMax tier, still not segRank |
| F.4 boot fresh-skip arms | GREEN unchanged (boot mode untouched); NEW sibling arms assert keepwarm uses the AGE predicate, gvr-discovered still no-skip (F4-C3 boundary re-pinned per mode) |

All arms run under `go test` with === RUN verification
(feedback_falsifier_must_actually_run_under_gate_tag_env); kind-backed arms serialized
(feedback_serialize_kind_test_runs).

## 7. Deferred refinement trigger (predicate (c))

Adopt activity-driven trimming ONLY when live data shows: per-cycle sweep duration
approaching TTL/4 at a customer topology (the graceful-degradation boundary, §3 regime 2), OR
sweep duty measurably moving /call p95 (C2-C10d violation). Entry cost is stated up front: a
served-BindingUID recency record + the serve↔seed identity bridge lemma with real-derivation
falsifier arms on BOTH sides (no hand-fed keys), + restart-cold-hole semantics. Until then,
capability-set + age-skip is strictly less machinery for the observed mix.

## 8. Options surfaced (strategic)

- **Sweep set — recommended (d′): widget-capable tier, staggered-complete.** Alternative (c)
  activity-driven is the principled cost trim but is new stateful machinery with a divergent-
  derivation bridge; deferred with an explicit data trigger (§7). Alternative (b) top-N
  rejected outright (magic number).
- **Mode plumbing — recommended: seedScopeMode enum replacing rank1Only+bootScoped.**
  Alternative (third bool) keeps signatures smaller per-call but admits illegal states and
  scatters skip semantics; rejected.
- **Chunk budget — recommended: keep 8m uniform.** Alternative (TTL-derived keepwarm budget)
  is knob-free but sacrifices CRUD fairness to monolithic occupancy; rejected (§4.3).

## 9. As-built (2026-07-07, feat/keepwarm-c2-cohorts)

Implemented per §4 shape (d′), ~90 LOC net excl. tests:

- **seedScopeMode enum** (prewarm_engine.go) — `seedModeBoot` / `seedModeKeepwarm` /
  `seedModeGVRDiscovered`, `String()`. Replaces the (rank1Only, bootScoped) bool pair;
  retires the illegal 4th state. Threaded through makeBootScopeHandler → rePrewarmBoot /
  rePrewarmKeepwarm / rePrewarmGVRDiscoveredSeed → rePrewarmBootScoped → seedScopeYielding →
  seedOneWidget/seedOneRestaction (all signatures now take `mode seedScopeMode`).
- **Sweep-set bound** (prewarm_engine_boot.go seedScopeYielding loop) — the c1
  `rank1Only && ri>0 break` is DELETED; the keepwarm bound is now
  `mode == seedModeKeepwarm && ranked[ri].widgetMax == 0 { break }` (the widget-capable
  contiguous prefix, Fix-3 property). Rank-log suppressed for keepwarm.
- **Per-mode seed skip** (phase1_pip_seed.go `seedSkipDecision`, single derivation site) —
  boot = bare-liveness fresh-skip (F.4, `pipSeedFreshSkipTotal`); keepwarm = age-skip
  (`keepwarmAgeSkipTotal`) with `time.Since(entry.CreatedAt) < keepwarmAgeSkipThreshold()`;
  gvr-discovered = never skip. Both primitives call it before enterSeedUnit, consuming the
  SAME production key the Put uses.
- **Threshold** (prewarm_keepwarm_sweep.go `keepwarmAgeSkipThreshold`) = TTL − sweepInterval
  = TTL − TTL×3/4 = TTL/4, derived from cache.ResolvedCacheTTL + the existing cadence
  fraction. No new knob, no literal. Strict `<` preserves the store's strict `>` expiry
  boundary interplay (resolved.go:787).
- **Counter** `keepwarmAgeSkipTotal` (phase1_pip_metrics.go) — expvar
  `snowplow_phase1_keepwarm_age_skip_total`.
- Ticker/cadence/scope-key/queue-coalescing UNCHANGED (prewarm_keepwarm_sweep.go). GTTL-1
  intact (every sweep Put behind declineSeedPutOnError = fresh re-resolve).

Falsifiers (all -race, === RUN verified; mutation transcripts /tmp/c2/):
prewarm_keepwarm_c2_test.go (C2-C3 age-skip crux + age-term-discriminates, C2-C4 chunked
completion + per-cell interval < TTL via real engine worker + F.4 requeue, C2-C7 cost, plus
the boot-old-cell arm that catches mutation (a)); prewarm_keepwarm_sweep_test.go (C2-C1
widget-capable-prefix + both mutations, C2-C2 machine-SA capability inclusion); F.4 boundary
arm retargeted to gvr-discovered no-skip; prewarm_first_nav_latch_segment_test.go keepwarm
arm widened to the widget-capable prefix. Five RED mutations proven by source-revert-rerun:
(a) boot gains age-skip, (b) keepwarm loses age-skip, (c) gvr-discovered gains a skip, (i)
c1 rank-1 bound restored, (ii) widgetMax==0 break dropped.
