# F.4 — Resumable boot scope: surviving the per-scope budget without knobs

Date: 2026-07-07 · Author: cache-architect · Ref: main @ 701c2e3 (1.6.1)
Status: DESIGN (no build). PM conditions F4-C1..C8 below.
Live evidence: fresh2 @ 60K, 1.6.1, PHASE1_TIMEOUT=900 — boots xkpb7 (scope cut
at engine+480s at 168/175 segment widgets, no latch fire, Ready 822s not
gate-driven) and t72l5 (segment completed in-scope, latch segment-complete at
scope+303s). Segment ≈ 2.3min walk/enum preamble + 5–6.5min seeding — straddles
the 8m budget nondeterministically.

---

## 1. Problem and root cause (TRACED)

The engine worker bounds every scope execution with
`context.WithTimeout(ctx, prewarmScopeTimeout(s))` where
`prewarmScopeTimeout` returns `pipGlobalTimeout` = 8m uniformly
(prewarm_engine.go:392-394, applied at :440; pipGlobalTimeout defined at
phase1_pip_seed.go:141). At 60K the boot scope's first-nav segment alone now
straddles that budget. When the deadline fires:

1. `seedScopeYielding` aborts via `emitSeedAbort` + `return ctx.Err()`
   (prewarm_engine_boot.go:712-716 widget path) → the scope returns
   DeadlineExceeded → `prewarm.engine.scope_incomplete` (prewarm_engine.go:444).
2. **Nothing engine-side re-enqueues the cut scope.** The only paths that
   enqueue `scopeKindBoot` are the one-shot boot enqueue
   (phase1_walk.go:478), the #158 per-target *operational-failure* re-enqueue
   inside `classifyEngineSeedErr` (prewarm_engine_boot.go:512-515 — not
   reached on a ctx-deadline abort, which exits via `emitSeedAbort`), and the
   config-vars ConfigMap Add/Update events
   (phase1_configvars_watch.go:131-139). So resumption depends on an
   **external, nondeterministic** ConfigMap event.
   NOTE (F.4 as-built §1.2 clarification): the #158 per-target re-enqueue
   inside `classifyEngineSeedErr` exists ONLY for per-target *operational
   failures* (5xx / transport / timeout, classified by `classifySeedErr`) and
   is reached via the normal target-error path — NEVER on a ctx-deadline
   abort, which exits earlier via `emitSeedAbort` + `return ctx.Err()`. F.4
   GENERALIZES that requeue to the deadline path: the ENGINE worker
   (`processScope`) requeues ANY scope that returns an error while the process
   ctx is alive, so a boot deadline-cut resumes deterministically. The two
   mechanisms compose — #158 heals a single flaky target mid-scope; F.4 heals
   the whole cut scope.

3. A re-run **redoes all completed seed work at full cost**: neither
   `seedOneRestaction` (phase1_pip_seed.go:460-639) nor `seedOneWidget`
   (:666+) has any warm-cell skip — both unconditionally `objects.Get` →
   `Resolve` → `Put`. (This is *required* by keepwarm, whose Puts must reset
   CreatedAt — prewarm_keepwarm_sweep.go:19-22 — and by gvr-discovered, whose
   whole purpose is re-resolving already-warm cells so the new GVR's dep edge
   gets recorded — prewarm_engine_boot.go:137-147.) Consequence: a redrive of
   a cut boot scope re-pays the full segment and can be cut again at 8m —
   the live `scope_incomplete ×2` successive-cut boot is exactly this
   livelock at the straddle point.
4. Queue fairness is accidental: `dequeueScope` iterates a Go map
   (prewarm_engine.go:291-299) → **random** dequeue order. The header comment
   (:42-46) claims a `workqueue.TypedRateLimitingInterface`; the as-built is a
   hand-rolled map + signal channel. Stale doc, and no ordering guarantee for
   the keepwarm/CRUD starvation bound.

Why this produces the symptom: at 60K the segment cost crossed the single-shot
budget, so whether the first-nav latch fires is a coin flip on walk/seed
variance (t72l5 yes, xkpb7 no), and when it loses, the dashboard cells stay
cold until an unrelated ConfigMap event happens to redrive — which then repays
the full cost and can lose again.

## 2. Prior-art check (opens the design, per team rule)

- **client-go `workqueue.TypedRateLimitingInterface`** is Kubernetes' answer
  to "a work item is bigger than one attempt": FIFO + coalescing dedup +
  `AddRateLimited` exponential-backoff requeue + never-drop, paired with
  **level-triggered idempotent reconcile**. Controllers never checkpoint
  partial progress inside an item; they make the item cheap to re-run and
  requeue it. This maps to the chosen shape below.
- There is **no chunk-cursor prior art** in client-go/controller-runtime for
  resuming mid-reconcile; `pager.ListPager` (Limit/Continue) chunks LIST
  pagination, not resolve work — not applicable.
- Verdict: don't invent a cursor. Make the engine queue behave like a real
  workqueue (requeue-on-failure, FIFO, backoff) and make the boot re-run
  **cost-proportional** so idempotent re-run is cheap — which the as-built
  seed primitives currently are not (§1.3).

## 3. Chosen design — shape (b″): engine-driven requeue + boot-only fresh-skip

Two small, composable mechanisms. The 8m budget, the readyz wiring
(engineSeed select / bootDone / MarkPhase1Done, phase1_walk.go:431-513), and
the seed ORDER are all untouched.

### 3.1 Failure-requeue on the engine queue (the "resume" trigger)

When a scope returns an error and the worker's process ctx is still alive
(i.e. not shutdown), the engine itself requeues the same scope with
rate-limited backoff; on success it forgets the failure history. Uniform for
all scope kinds — no special case:

- `scopeKindBoot` deadline-cut → requeued → the continuation chunk. This is
  the F.4 fix proper. Also heals `roots_list_failed` (transient apiserver at
  boot), today healed only by config-vars luck.
- `scopeKindGVRDiscovered` cut/error → requeued. Today an incomplete
  gvr-discovered scope is silently dropped (prewarm_engine_boot.go:169-175)
  and the S4 dep-edge repair is lost until another discovery event — F.4
  fixes this for free.
- `scopeKindKeepwarm` error → requeued; coalesces with the next tick.

Coalescing is **correct by construction** because there is no cursor: a
continuation and a fresh config-vars redrive are the same level-triggered
work item, same `key()=="boot"` — nothing to swallow or reset (the brief's
shape-(a) coalescing hazard evaporates).

New greppable log line `prewarm.engine.scope_requeued` (scope, err,
attempt/backoff) at the requeue site.

Exponential backoff bounds the pathological zero-progress case (e.g. a walk
that can never finish inside the budget): retries never stop (never-drop) but
space out — no hot loop, and informer convergence makes later attempts
cheaper. Backoff parameters are client-go's stock defaults, not our knob.

**Queue implementation — the one strategic choice (see §7):**
- **R1 (recommended):** replace the hand-rolled pending-map/signal with
  `workqueue.TypedRateLimitingInterface[prewarmScope]` (`prewarmScope` is
  comparable — string kind + GVR struct — so the item is its own key; the
  per-key payload-coalescing semantics of the map are preserved 1:1 because
  `key()` and item identity coincide). Gets FIFO (F4-C5), `AddRateLimited`
  (F4-C8), coalescing, and ShutDown-on-ctx from tested client-go code, and
  makes the stale header comment at prewarm_engine.go:42-46 true.
- **R2 (minimal delta):** keep the map, add a FIFO key-order slice + explicit
  requeue via `workqueue.NewTypedItemExponentialFailureRateLimiter` +
  `time.AfterFunc`. Smaller diff, but hand-rebuilds more of workqueue —
  against the prior-art rule.

### 3.2 Boot-only fresh-skip (makes re-run cost-proportional)

Give `scopeKindBoot` seeds an explicit fast-forward: before resolving a
(unit × identity) target, if a **live** (non-expired) L1 cell already exists
under the exact production cell key, skip the resolve and count the target as
processed. This is what makes chunk N+1 cheap (≈ preamble + remaining
targets) and the chunk count provably finite: every chunk seeds strictly more
targets than the last (cells persist across chunks; TTL 1h ≫ chunk cadence).

**Scope-kind boundary (F4-C3) — load-bearing, traced:**
- boot: fresh-skip ON. Boot semantics = "make warm"; a live cell is done.
- keepwarm: OFF — its Puts must re-resolve fresh bytes and reset CreatedAt
  (prewarm_keepwarm_sweep.go:19-22); a fresh-skip would make the sweep a no-op.
- gvr-discovered: OFF — it exists to re-resolve *already-warm* cells so the
  dep edge against the new GVR is recorded (prewarm_engine_boot.go:137-147);
  a fresh-skip would reintroduce the S4 defect.

Plumbed exactly like `rank1Only` (a scope-kind-derived bool through
`rePrewarmBootScoped` → `seedScopeYielding` → the per-target path). Not a
knob: it is a structural property of the scope kind.

**Single-derivation-site constraint (the #64/#317/FIX-G lesson,
feedback_consultation_mutation_is_not_key_correctness):** the skip MUST
consume the SAME key derivation the Put site uses — no parallel re-derivation
anywhere. Concretely: the check lives inside `seedOneWidget` /
`seedOneRestaction` immediately after their existing `dispatchCacheLookupKey`
call (phase1_pip_seed.go:480-483 restactions; the widget mirror incl. the
inline-extras union), after the empty-BindingUID guard, **before**
`enterSeedUnit` (skipped units never consume the #46 memory gate — the bound
composes trivially, strictly fewer admissions). By construction there is one
derivation body; skip-key correctness cannot drift from Put-key correctness.
Note: the restaction skip still pays one informer-backed `objects.Get` (the
key needs GVR/ns/name from the CR) — µs-scale, acceptable.

**Fresh predicate (F4-C2a, PM-required, as-built):** skip-eligibility ≡ the
production-key lookup `handle.Get(key)` returns a NON-EXPIRED entry per the
store's OWN TTL-expiry check — the SAME `ResolvedCacheStore.Get`
(internal/cache/resolved.go) the serve path and refresher use (it drops a
TTL-expired entry and returns `(nil,false)`, honoring any per-entry
`TTLOverride`). There is NO "TTL-remaining ≥ X" literal and NO put-since-chunk
bookkeeping: the skip reuses the store's existing effective-TTL logic verbatim,
so a boot chunk skips exactly the cells a `/call` would HIT and re-resolves
exactly the cells a `/call` would MISS. `key` is the SAME value
`dispatchCacheLookupKey` derived one line above (the Put key), so skip-key
correctness cannot drift from Put-key correctness.

Skip observability: `pipSeedFreshSkipTotal` expvar counter + a Debug-level
per-target line; the fire-log and seed telemetry are otherwise unchanged.
If the store exposes a metrics-neutral peek, use it; otherwise the Get's
hit-count contribution is accepted and documented (seed-attributable).

The residual re-run tax is the ~2.3min walk/enum preamble per chunk. That is
the price of level-triggered soundness (config.json / RBAC / informer state
may have changed between chunks) and is exactly what every config-vars
redrive already pays today. A cursor could avoid it only by freezing a stale
snapshot (§5 ALT-A).

### 3.3 What F.4 deliberately does NOT change

- `prewarmScopeTimeout` stays `pipGlobalTimeout` (8m) for every kind — the
  liveness bound protecting keepwarm/CRUD from a runaway scope is preserved
  verbatim (arch O1 on #102).
- engineSeed's select, bootDone/scopeDone, MarkPhase1Done, PHASE1_TIMEOUT
  backstop: byte-identical. On a chunk-1 cut, bootDone still closes with the
  deadline error and Ready still flips at ≈ min(latch, 8m seed budget,
  PHASE1 backstop) exactly as today. F.4's contribution is that the warm
  state then **completes deterministically** one short chunk later, instead
  of never/by luck. (Closing the residual Ready→fully-warm gap at 60K is a
  seed-throughput problem — FIX-G-class work — not a budget problem; stated
  honestly, out of scope.)
- Seed order, latch arming/decrement logic, #158 classification: untouched.

### 3.4 Invariant walk-through

- **#99b latch across chunks:** the latch is a process fire-once singleton
  (prewarm_first_nav_latch.go:57-100); each chunk re-arms per-scope counting
  from its own fresh walk+enum (prewarm_engine_boot.go:782-825). In the
  completing chunk, already-warm segment targets fast-forward through
  `seedWidgetTarget` (processed → decrement, same PROCESSED-not-SUCCEEDED
  contract as :874-882) and the remaining ones seed for real; the latch fires
  once, on the chunk where the segment finishes. If chunk 1 already fired
  and was cut in the tail, chunk 2's re-fire hits `sync.Once` — a no-op.
- **F-C2 ARM-TAIL:** within the firing chunk the segment completes mid-pass,
  before that rank's RootIndex>0 widgets and restactions (order unchanged) —
  the tail is still pending at fire time.
- **Keepwarm/CRUD starvation:** per-chunk bound stays 8m; between chunks the
  FIFO queue runs any pending keepwarm/gvr-discovered scope before the boot
  continuation (which re-enters at the back, after backoff). Worst-case wait
  behind boot = one budget — the existing bound, now guaranteed rather than
  map-random.
- **#46 footprint:** unchanged per unit; fresh-skip only removes admissions.
- **GTTL-1 interplay:** a declined Put (degraded resolve) leaves no fresh
  cell → the target is NOT skipped next chunk → retried. A prior good cell →
  skipped → good bytes kept. Consistent with the Put-gate's intent.
- **Dirty cells:** a chunk-1-seeded cell dirty-marked before chunk 2 is
  skipped by the seed; the event-driven refresher owns dirty re-resolves —
  same division of labor as steady state.

## 4. Would this make the symptom disappear?

Yes. xkpb7 replayed under F.4: chunk 1 cut at engine+480s with 168/175 →
worker requeues boot (backoff ~ms) → chunk 2 = ~2.3min preamble + 168 skips
(µs each) + 7 real seeds → latch fires `segment-complete` ≈ cut+3min,
deterministically, with zero dependence on config-vars events. Every boot
topology terminates in ⌈seed_time / (budget − preamble)⌉ chunks; the
scope_incomplete ×2 livelock class is structurally gone because chunk work is
monotone.

## 5. Alternatives rejected

- **ALT-A — cursor/checkpoint chunking (brief shape a):** a positional cursor
  (rank r, widget i, target j) over lists that are RE-DERIVED each run
  (re-walk + re-enum) is unsound — informer catch-up and RBAC deltas reorder
  and reshape `widgetSeeds`/`restactionSeeds`/`ranked` between chunks. Making
  it sound requires freezing the first chunk's snapshot in the scope payload:
  serves stale enumeration, holds MBs across chunks, breaks the no-payload
  boot-key coalescing contract (a fresh config-vars redrive must RESET the
  cursor — new failure modes), and contradicts the k8s level-triggered idiom
  (§2). Buys only the ~2.3min preamble over fresh-skip — bad trade.
- **ALT-B — status-quo idempotent re-run (brief shape b, formalized):** the
  trigger is an external nondeterministic ConfigMap event, and the "warm
  fast-forward ~0ms" premise is NOT as-built — TRACED at
  phase1_pip_seed.go:460-639/666+ (unconditional Resolve+Put, no warm check;
  keepwarm depends on exactly that). Without fresh-skip each re-run repays
  the full segment → livelock at the straddle point (live scope_incomplete
  ×2). (b′) alone (self-re-enqueue, no skip) inherits the same livelock.
- **ALT-C — class-differentiated derived budget (brief shape c):** still a
  single-shot cliff — a 20% bigger topology fails identically; "remaining
  PHASE1 window" is undefined for post-boot boot-kind redrives (config-vars
  events fire hours after boot), forcing a two-regime rule; it conflates the
  readiness backstop with the engine liveness bound; and at 60K the derived
  window (~900s − ~330s engine start ≈ 9.5min) barely clears the observed
  7.3–8.8min straddle — no margin. It is the rejected "bigger timeout knob"
  in derived clothing, and it delays any pending keepwarm/CRUD scope for the
  whole enlarged window.
- **ALT-D — raise `pipGlobalTimeout`:** explicitly rejected by Diego
  (magic-number knob; extends worst-case starvation of every other scope).

## 6. PM-gateable conditions

- **F4-C1 (engine-owned resume, never-drop):** a boot scope cut by the
  per-scope budget is requeued by the ENGINE itself — zero dependence on
  config-vars events — coalescing on the boot key, retrying until success
  with rate-limited backoff. Mutation-RED: remove the requeue → straddle
  falsifier fails.
- **F4-C2 (cost-proportional resume):** a continuation chunk does NOT
  re-resolve already-seeded targets. Fresh-skip consumes the SAME
  `dispatchCacheLookupKey` derivation as the Put site (single body, no
  parallel derivation, no hand-fed keys in the falsifier — both sides run the
  real primitive). Observable via `pipSeedFreshSkipTotal`. Mutation-RED:
  remove the skip → chunk-2 resolve count equals the full set.
- **F4-C3 (scope-kind boundary):** fresh-skip applies ONLY to scopeKindBoot.
  Falsifier arms prove keepwarm still re-Puts a live cell (CreatedAt reset)
  and gvr-discovered still re-resolves a live cell (dep-edge repair).
- **F4-C4 (latch exactly-once + ARM-TAIL across chunks):** a segment larger
  than one budget produces exactly ONE `segment-complete` fire, in the
  completing chunk, while ≥1 non-segment tail unit is still unseeded; a
  latch already fired in chunk 1 is not re-fired by chunk 2.
- **F4-C5 (starvation bound preserved, now guaranteed):** a
  keepwarm/gvr-discovered scope enqueued during a boot chunk runs BEFORE the
  boot continuation (FIFO); no scope waits more than one budget behind boot.
- **F4-C6 (no new knobs):** no new env, no new duration/count literal;
  budget stays `pipGlobalTimeout`; backoff = client-go stock rate-limiter
  defaults; chunk count emergent from topology.
- **F4-C7 (readyz wiring untouched):** engineSeed select / bootDone /
  MarkPhase1Done / PHASE1_TIMEOUT byte-identical; existing FIX-F/#99b/shape-A
  falsifiers stay green unmodified (except where they assert queue internals).
- **F4-C8 (futility bound):** repeated zero-progress failures back off
  exponentially — no hot loop; retries never stop (never-drop).

## 7. Strategic choice surfaced (needs TL/Diego pick)

R1 (adopt `workqueue.TypedRateLimitingInterface[prewarmScope]`, ~+60/−40 LOC
in prewarm_engine.go, makes the stale :42-46 comment true, gets
FIFO/backoff/coalescing from tested client-go code) vs R2 (keep the map, add
FIFO slice + client-go rate limiter + AfterFunc, ~40-60 LOC, but hand-rebuilds
workqueue semantics we then own forever). **Recommendation: R1** — the
prior-art rule exists for exactly this; the current map+signal is already a
half-reimplementation that just failed us on ordering.

## 8. Falsifier plan

Hermetic (package dispatchers, -race, no build tags — verify arms ACTUALLY RAN
per feedback_falsifier_must_actually_run_under_gate_tag_env):

1. **Straddle arm (F4-C1+C4):** stubbed primitives (`seedOneWidgetFn` /
   `seedOneRestactionFn` / `enumeratePrewarmTargetsForGVRFn` seams, existing
   pattern) with per-target simulated cost + a stub-side seeded-set emulating
   fresh-skip monotonicity; per-scope budget shrunk via a new 1-LOC test seam
   `var prewarmScopeTimeoutFn = prewarmScopeTimeout` (same pattern as
   seedCohortFn). K=2 identities × RootIndex∈{0,1} widgets + restactions.
   Chunk 1 deadline-cuts mid-segment → assert `scope_requeued` + no latch
   fire; chunk 2 completes → assert exactly ONE `segment-complete`
   (firstNavFireObserver), fired while ≥1 tail unit unprocessed (ARM-TAIL),
   and total processed monotone. RED arm: requeue removed → test hangs/fails.
2. **Fairness arm (F4-C5):** enqueue keepwarm while boot chunk 1 runs; after
   the cut, assert dequeue order keepwarm → boot-continuation (FIFO). RED
   under the old map queue (random order) — this arm also discriminates
   R1/R2 from status quo.
3. **Fresh-skip real-primitive arm (F4-C2, divergent-derivation-proof):** run
   the REAL `seedOneWidget` twice against a real in-memory L1 (existing
   real-primitive test fixtures: prewarm_seed_empty_binding_skip_test.go
   pattern). First call resolves+Puts; second call must skip: assert
   `pipSeedFreshSkipTotal` +1 and `pipBindingSetSeedResolvesTotal` unchanged.
   No hand-constructed key anywhere — both sides run production derivation.
   Restaction mirror arm. RED: skip removed → second call re-resolves.
4. **Boundary arms (F4-C3):** keepwarm path (rank1Only) over a live cell →
   asserts a REAL re-Put happened (CreatedAt/bytes replaced);
   gvr-discovered path over a live cell → asserts re-resolve invoked
   (dep-edge recording preserved). RED: fresh-skip wrongly applied to either
   kind → these fail.
5. **Exactly-once cross-chunk arm:** latch fires in chunk 1, scope cut in the
   tail, chunk 2 completes → assert no second fire log/observer event.

Live proof at 60K (fresh2), post-deploy grep sequence on one boot:
`prewarm.engine.scope_incomplete scope=boot` → engine-driven
`prewarm.engine.scope_requeued` (with `ConfigVarsEnqueuedTotal` flat over the
window — proves engine-owned resume) → next chunk's
`prewarm.first_nav.latch reason=segment-complete` with
`first_nav_targets`==segment size, plus expvar `pipSeedFreshSkipTotal` ≈
chunk-1 seeded count. Symptom-disappear check: zero boots where the latch
never fires; no permanently-cold first-nav targets.

## 9. Touch points + LOC bound

- `internal/handlers/dispatchers/prewarm_engine.go` — queue swap (R1) +
  requeue/Forget + `scope_requeued` log + timeout seam: ~100 LOC net.
- `internal/handlers/dispatchers/prewarm_engine_boot.go` — freshSkip plumb
  (scope-kind → `rePrewarmBootScoped` → `seedScopeYielding` → per-target
  ctx/param): ~20 LOC.
- `internal/handlers/dispatchers/phase1_pip_seed.go` — skip check in the two
  primitives after key derivation + counter: ~40 LOC.
- `internal/handlers/dispatchers/prewarm_engine_metrics.go` — expose
  `pipSeedFreshSkipTotal` (+ requeue counter): ~10 LOC.
- Tests: ~350-450 LOC across the five arms.
- Total prod delta ≈ 170 LOC. No CRD/chart/env change.
