# #106 — Config-vars watcher data-change redrive gate (design memo v2)

Author: architect (archC2d), 2026-07-07. PM gate: ACCEPT (pmC2d, C106-1..4; INFO skip line binding).
Base: main @ bcd6f286 (1.6.4). Target: 1.6.5. Task #106.

## Symptom mechanism (TRACED)

`UpdateFunc` enqueues unconditionally on ANY object write: `phase1_configvars_watch.go:213-217`
→ `enqueueBootReDrive` :131-139 → `enqueueScope(scopeKindBoot)` :133. The KrateoFrontend CDC
re-apply bumps annotations/managedFields (fresh `krateo.io/traceparent` span each reconcile) →
new ResourceVersion → informer UPDATE → boot scope ~1/min, each paying the full walk (~2.3 min
at 60K). `config.json` is byte-identical across these events (sha-verified live on fresh2,
2026-07-07). Keepwarm scopes queue behind the permanent boot stream (observed delaying the
first c2 sweep).

## Prior-art check

client-go has no built-in data-diff predicate, but the informer `UpdateFunc` already receives
`(old, new)`. Canonical filter tiers: (1) `old.ResourceVersion == new.ResourceVersion` skip —
filters only resync replays, useless here (CDC writes genuinely bump RV; our factory runs
resync=0, :194); (2) content compare of the consumed fields via
`apiequality.Semantic.DeepEqual` — the standard controller pattern for ConfigMap-driven reload
(GenerationChangedPredicate does not apply: ConfigMaps have no spec/generation; Helm's
checksum/config pattern hashes full Data for the same reason). We take tier 2.

## Design — stateless Data-equality predicate in UpdateFunc (C106-1..3)

**Placement.** One function `configVarsDataChanged(oldObj, newObj any) bool` in
`phase1_configvars_watch.go` beside `matchesConfigVars` (:113), called from `UpdateFunc` after
the name guard. Casts both to `*corev1.ConfigMap`; returns
`!apiequality.Semantic.DeepEqual(old.Data, new.Data) || !apiequality.Semantic.DeepEqual(old.BinaryData, new.BinaryData)`.
If EITHER cast fails → return true (fail OPEN = pre-fix behavior; never suppress on doubt).
The hook stays O(1)-ish (map compare over a 1-key CM) and non-blocking — the
HOOK-MUST-NOT-BLOCK contract (:30-33) holds. NOTE: the current UpdateFunc is
`func(_, newObj interface{})` and discards `old` — wiring `old` in is part of the change.

**Scope: FULL Data + BinaryData (superset), never narrower than consumed.** The walker consumes
ONLY `Data["config.json"]` (`frontendConfigDataKey`, phase1_roots.go:75; read :248 — TRACED).
Key-only would be tightest but fails SILENT-STALE if a future walker change consumes a sibling
key and the gate isn't widened in lockstep — a suppressed genuine redrive is the exact
cold-first-nav class this watcher exists to kill, and it fails quietly. Full-Data fails only
with an occasional redundant coalesced walk on a frontend deploy touching a sibling key —
self-limiting and arguably desirable. Matches Helm checksum/config prior-art. NO parse in the
hook (no parse-error path; the only failure mode is the type-cast, which fails OPEN). Add a
pointer comment at `frontendConfigDataKey` (phase1_roots.go:73-75) noting the gate so the
coupling is discoverable.

**No stored hash; the staleness dilemma dissolves (C106-2c).** Semantics = compare against
LAST-SEEN, implemented via the informer's own delivered `(old, new)` pair: client-go guarantees
each UpdateFunc `old` is the previously delivered state, and `enqueueScope` never drops
(workqueue.Add, prewarm_engine.go:344-346 — TRACED), so last-seen ≡ last-redriven with no
divergence window. The REJECTED trap (fixed boot-baseline hash missing a revert-to-baseline
change) is structurally absent — nothing captured at boot, nothing persisted. Flap A→B→A as two
updates: event 1 (old=A,new=B) differs → redrive; event 2 (old=B,new=A) differs → redrive.
Both fire BY CONSTRUCTION.

**AddFunc UNGATED (C106-3a).** Verbatim unchanged (:206-210) — first appearance / initial-LIST
replay always redrives; it is the boot-race self-heal trigger itself; there is no prior state
to diff. A design that gates the ADD path is wrong.

**DELETE semantics.** Keep NO DeleteFunc (unchanged :218-219). A deleted config-vars CM has
nothing to walk; delete→recreate is covered by the unconditional AddFunc (recreate = ADD).
Stateless dividend: a stored last-hash would survive the delete and could suppress the recreate
under a naive compare — no such state exists.

**Coalescing + F.4 (C106-3b): NO behavior change.** The gate sits BEFORE `enqueueScope` inside
the hook; the boot dedup key (:335-341), the engine worker, and the F.4 engine-owned requeue
(post-dequeue, prewarm_engine.go:541) are untouched. Net effect on c2: keepwarm scopes stop
queueing behind a permanent 1/min boot stream. Two rapid data changes may coalesce to ONE walk
run — correct: the walk reads the LIVE ConfigMap at run time (readFrontendConfig,
phase1_roots.go:235+), so the single run observes the final state; later events re-enqueue if
anything changed after dequeue. Converges to last-written state.

## Observability (C106-4 — PM ruling: BOTH, INFO binding)

- Expvar counter `configVarsSkippedTotal` + `ConfigVarsSkippedTotal()` accessor + expvar
  `snowplow_phase1_configvars_skipped_total`, exact mirror of the enqueued counter
  (phase1_configvars_watch.go:91-95). Steady-state rate = the churn rate — proves the gate is
  actively firing (distinguishes gate-working from informer-dead/churn-stopped).
- Skip line at INFO: `prewarm.configvars.redrive_skipped` with
  reason="data_unchanged_metadata_only" + CM name + effect string, greppable under
  PRETTY_LOG:false. Volume bounded by the CDC cadence (~1/min on fresh2) — that IS the live
  evidence stream for the C106-1(ii) ledger row, and it disappears if the CDC churn is ever
  fixed upstream.
- Enqueue reason string split: "configmap_added" / "configmap_data_changed".

## Falsifier spec (C106-1(i), C106-2)

Substrate: the existing real-informer-over-fake-clientset harness
(phase1_boot_race_selfheal_falsifier_test.go pattern, SetConfigVarsWatchClientForTest), reading
ConfigVarsEnqueuedTotal / ConfigVarsSkippedTotal.

- **ARM-1 annotation-only (crux), paired in ONE test with ARM-2:** Update changing ONLY a
  traceparent-shaped annotation → EnqueuedTotal FLAT + no boot_redrive_enqueued + SkippedTotal
  +1; then Update changing Data["config.json"] → EnqueuedTotal +1,
  reason="configmap_data_changed". Discriminator = the data-diff (gate-less build passes the
  data arm but fails the annotation arm; gate-everything build fails the data arm).
- **ARM-3 flap:** A→B→A two updates → EnqueuedTotal +2 (pins per-event-delta semantics).
- **ARM-4 add-always:** existing TestBootRace_ConfigVarsInformerDrivesReWalk stays GREEN
  unchanged (ADD path ungated); first ADD redrives regardless of data.
- **ARM-5 delete→recreate:** Delete then Create same Data → +1 via ADD.
- **RED mutation (source-revert, byte-clean restore):** drop the predicate (UpdateFunc calls
  enqueueBootReDrive unconditionally, the pre-fix :213-217 form) → ARM-1 annotation arm goes
  RED (counter increments).

## On-cluster ledger row (C106-1(ii), post-deploy)

Fresh2 60K, ≥15 min under CDC churn: `boot_redrive_enqueued` delta 0 (pre-fix ~15) while the
CM's traceparent keeps changing (resourceVersion bumping) + `snowplow_phase1_configvars_skipped_total`
climbing ~1/min + `prewarm.configvars.redrive_skipped` INFO lines at the churn cadence; then
ONE live config.json edit → exactly one redrive + a completed walk; c2 keepwarm cohort_summary
lines appear on cadence (no boot-scope queue delay).

## Touched surface (~28 LOC)

`phase1_configvars_watch.go`: predicate helper (~12), UpdateFunc wiring (~3),
counter+accessor+expvar (~7), INFO line (~4), reason split (~2). Plus ONE comment-only pointer
line at phase1_roots.go:73-75. NO env knob (unconditional-on; pure correctness fix), NO
engine/queue/F.4 change, NO stored state, no new apiserver reads.

## Option surfaced and rejected

(B) stored sha256-of-Data compared against last-REDRIVEN hash — needed only to gate the ADD
path too (suppress the redundant boot redrive when a restart replays an unchanged CM).
Rejected: ADD-always is load-bearing for the boot race; stored state reintroduces the
drift/delete-staleness holes the stateless design closes. Revisit only if ADD-path noise is
ever observed (it isn't — ADD fires once per process).
