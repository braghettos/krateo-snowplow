# #105 — marketplace-detail null-jq re-enqueues the whole boot re-walk, unboundedly

TRACE + FIX-DESIGN. Read-only. Frozen ref: origin/main `8fc4d87` (= 1.7.17 + #119 PARKED).
Log artifacts `/tmp/f3-deploy/boot-1.7.7-900-t0.log` + `retained-1.7.7-900.log`: **ABSENT**
in this worktree (different session). All claims below are TRACED against code at the frozen
ref + the issue's verbatim quoted log lines; runtime cadence (~8 min, ~320 s/rewalk) is
grounded in the code's timeout literals (TRACED) with the issue's field observations noted.

---

## 1. ROOT CAUSE (TRACED)

**The amplifier is a two-part defect, both snowplow-side, both readable:**

### 1a. The classifier has no "deterministic-input-failure" verdict — a null-jq failure defaults to `seedFailOperational` (retryable)

`classifySeedErr` (`internal/handlers/dispatchers/phase1_seed_classify.go:78-110`) is a
closed taxonomy of exactly three verdicts: `seedFailNone` / `seedFailRBACDeny` /
`seedFailOperational`. Its match ladder recognises only:
- ctx cancel/deadline → operational (`:86-88`)
- `apierrors.IsForbidden/IsUnauthorized` → RBAC-deny (`:94-96`)
- `apierrors.IsServiceUnavailable/IsServerTimeout/IsTimeout/IsInternalError` → operational (`:99-104`)
- **everything else → operational (FAIL-LOUD default, `:106-109`).**

The marketplace-detail failure is `fmt.Errorf("resolve RESTAction %s/%s: %w", …)` wrapping
`fmt.Errorf("unable to resolve filter: %w", err)` where `err` is a **jqutil.Eval error**
(`internal/resolvers/restactions/restactions.go:69-70`, surfaced up through the seed at
`internal/handlers/dispatchers/phase1_pip_seed.go:788-800`). A jq "expected a string" error
is a plain `*errors.errorString`/gojq error — **not** a ctx error, **not** an
`*apierrors.StatusError`. So it falls straight through to the fail-loud default and is
classified `seedFailOperational` — i.e. *"transient, retry it."*

This is a **mis-classification, not a bug in the default.** The fail-loud default is correct
for genuinely-unknown errors (a 5xx that didn't match a predicate SHOULD retry). But a
malformed-input / deterministic-jq failure is *not* transient: it will fail identically on
every retry because the RA's jq and the (null) input are unchanged. The taxonomy has no
verdict for "the seed will never succeed for this item — stop retrying it."

### 1b. `seedFailOperational` unconditionally re-enqueues the WHOLE boot re-walk (per pass)

`classifyEngineSeedErr` (`prewarm_engine_boot.go:570-598`) is the per-target error sink the
seed loop calls. On an operational verdict it does (`:594-597`):

```go
if !reEnqueued {
    reEnqueued = true
    prewarmEngineSingleton().enqueueScope(prewarmScope{kind: scopeKindBoot})
}
```

`enqueueScope` is an **immediate `queue.Add`** (`prewarm_engine.go:356-359`) — NOT
`AddRateLimited`. The `reEnqueued bool` bounds it to **at most one enqueue per
`seedScopeYielding` pass** (the `key()=="boot"` dedup, `prewarm_engine.go:265-269`, coalesces
concurrent enqueues within a pass to one pending re-walk — the design §3.1 "storm bound").

**But that bound is per-pass, not per-lifetime.** Each re-walk *is a fresh
`seedScopeYielding` pass* (`processScope` → `makeBootScopeHandler` → `rePrewarmBoot` →
`rePrewarmBootScoped` → `seedScopeYielding`, boot.go:79-384). marketplace-detail is seeded
again in that pass, fails again deterministically (same RA, same null input), and enqueues
the boot scope again. **The bound resets every pass.** There is no lifetime counter, no
per-item futility bound, no poison-pill. The loop is therefore **unbounded across passes** —
exactly the issue's account.

Critically: the per-target failure is *swallowed* — `classifyEngineSeedErr` returns `void`,
the seed loop does `targetsProcessed++; return false` (does not abort, boot.go:897-902), and
`seedScopeYielding` returns **nil** (success) at the pass level. So the re-walk that the
failure triggers is NOT the `processScope` error-requeue path (`prewarm_engine.go:609-632`,
`AddRateLimited` on ctx-error) — it is the **immediate re-Add** from the classifier. This
matters for the cadence (§2).

### The chain, end to end

```
seedOneRestaction(marketplace-detail)                       phase1_pip_seed.go:620
  → restactions.Resolve → jqutil.Eval("expected a string")  restactions.go:65-70
  → return fmt.Errorf("resolve RESTAction …: %w", err)      phase1_pip_seed.go:800
seedRestactionTarget: err != nil, ctx.Err()==nil            prewarm_engine_boot.go:897
  → classifyEngineSeedErr("restaction", …, err)             prewarm_engine_boot.go:898
    → classifySeedErr(err) == seedFailOperational (default)  phase1_seed_classify.go:109
    → slog.Warn "prewarm.engine.seed.operational_failure"    prewarm_engine_boot.go:585
    → enqueueScope(scopeKindBoot)  [immediate Add]           prewarm_engine_boot.go:596
worker.processScope dequeues boot again                      prewarm_engine.go:562/571
  → rePrewarmBoot → …→ seedScopeYielding (FRESH pass)        prewarm_engine_boot.go:79/364
  → slog.Info "prewarm.engine.boot.rewalk_complete"          prewarm_engine_boot.go:364
  → seeds marketplace-detail again → fails again → GOTO top  [∞]
```

`scope_incomplete` + `scope_requeued` (`prewarm_engine.go:610/623`) are the *other* requeue
path (fired only when the scope RETURNS an error to `processScope`, i.e. a ctx-cut / abort).
The issue quotes them because at 50K the boot scope ALSO gets deadline-cut at its 8-min
per-scope budget — but the **primary** driver of the marketplace-detail loop is the immediate
re-Add at boot.go:596, which needs no deadline-cut to fire.

---

## 2. WHY ~320 s / rewalk AND ~8 min cadence (TRACED to literals)

- **~320 s per rewalk**: a re-walk is a full `rePrewarmBootScoped` — re-walk every nav root
  (fresh walker/root, boot.go:292-328) + settle registered set + build BindingsByGVR +
  `seedScopeYielding` over all harvested restactions × widgets × per-binding cohorts. This is
  the same shape as the boot seed, whose empirical wall-clock is the #123/#121 ~314-339 s at
  50K (`docs/f4-boot-scope-budget-design-2026-07-07.md`, task #123). The issue's 314-331 s is
  this pass cost. TRACED to the mechanism; exact seconds are the 50K empirical.

- **~8 min cadence**: `prewarmScopeTimeout` returns `pipGlobalTimeout = 8m`
  (`prewarm_engine.go:510-512`, `phase1_pip_seed.go:135-142`). The per-scope
  `context.WithTimeout(ctx, 8m)` (`prewarm_engine.go:592`) caps ONE re-walk pass at 8 min.
  A pass runs ~320 s (< 8 min), completes, marketplace-detail has already re-Added the boot
  scope during the pass, the worker immediately dequeues it (the queue had it pending), and
  the next pass starts. So the OBSERVED period between `rewalk_complete` lines ≈ one pass
  wall-clock (~320 s) UNLESS a pass gets deadline-cut at 8 min (larger cohorts / customer
  yield contention), in which case the period ≈ 8 min. The issue's "~every 8 min" is the
  deadline-cut-dominated cadence; the floor is the ~320 s pass cost.

- **`prewarm.seed.abort` ~170 s**: `emitSeedAbort` (`prewarm_engine_boot.go:544-554`) fires
  when a seed target sees `ctx.Err() != nil` mid-pass — the scope ctx was cancelled (parent
  cut / 8-min deadline). 170 s is a mid-pass cut, consistent with a re-walk that started
  late in the 8-min window. Log-only, best-effort — NOT the amplifier, a *symptom* of it.

- **Note the workqueue backoff is NOT the throttle here.** The client-go
  `DefaultTypedControllerRateLimiter` (`prewarm_engine.go:339-341`) uses
  `ItemExponentialFailureRateLimiter(5ms, 1000s)` — but that governs only `AddRateLimited`
  (the ctx-error requeue). The classifier's `enqueueScope` is a plain `Add` with **zero
  delay**, and each genuine pass-completion `Forget`s the item (`prewarm_engine.go:637`)
  resetting any accumulated backoff. So there is no exponential slow-down protecting the
  system — the loop runs as fast as a pass completes, forever.

---

## 3. SNOWPLOW-FIXABLE vs PORTAL-OWNED (precise)

| Element | Owner | Fixable here? |
|---|---|---|
| The marketplace-detail RA's jq (`expected a string`) | **PORTAL** — krateo-system portal-chart/composition-controller-generated (same constraint as #32; manual edit reverts) | **NO** |
| The null input the jq chokes on (upstream null-path / cluster-list-collapse producing a scalar) | mixed — #117/#118 class; the collapse is snowplow, but the RA authored the path | partial — see §5 |
| **A deterministic seed failure re-enqueuing the full boot re-walk unboundedly** | **SNOWPLOW** — `phase1_seed_classify.go` + `prewarm_engine_boot.go` | **YES — this is the fix** |

**The architecturally load-bearing defect is the third row.** Even if the marketplace-detail
jq were fixed tomorrow, the next portal RA with a bad jq (or a genuinely-null upstream, or a
mis-typed filter) re-opens the exact same amplifier. A marketplace-detail-specific null-guard
is whack-a-mole and violates `feedback_no_special_cases`. **The blast-radius bound is the
right-altitude fix; the jq is a portal hand-off (§6).**

---

## 4. #117 / #118 CROSS-CHECK (TRACED)

- **Same resolve op-failure class?** YES. #117 ("split cannot be applied to: null") and
  #105 ("expected a string") are BOTH `jqutil.Eval` errors surfaced through
  `restactions.go:69-70` → `phase1_pip_seed.go:800`. Same wrapper, same non-apierror /
  non-ctx shape, same fail-loud-default classification → same `seedFailOperational` →
  same re-enqueue. They are one class: **deterministic jq/resolve-input failure mis-typed as
  transient.**

- **Did #118's WARN change anything here?** NO. #118 added a WARN on the null-path →
  cluster-LIST collapse *inside the resolver* (`internal/resolvers/restactions/api/refilter.go:64-67`
  — "other shapes passed through unchanged with a WARN"). It is a **diagnostic log only**; it
  does not change the collapse's OUTPUT and does not change whether the downstream jq
  succeeds or errors. The marketplace-detail filter still receives the same (scalar/null)
  input and still errors identically. **#118 is orthogonal to the amplifier.**

- **The amplifier is independent of whether the jq is fixed.** PROVEN by construction: the
  re-enqueue at boot.go:596 fires on ANY `seedFailOperational` verdict from ANY seed target.
  The marketplace-detail jq is one instance; fixing it removes that instance but not the
  mechanism. The falsifier (§7) drives a *synthetic* deterministic-failing item, not
  marketplace-detail, precisely to prove the fix is jq-agnostic.

---

## 5. FIX DESIGN

### 5.0 CONFIRMED re-walk seeding semantics (the progress signal DEPENDS on this — TRACED)

A stable boot re-walk **fresh-skips already-warm cells** — it does NOT re-seed the full set.
TRACED at `seedSkipDecision` seedModeBoot (`phase1_pip_seed.go:530-567`): before resolving a
target the seed does `entry, live := handle.Get(key)`; if the cell is live (non-expired under
the exact serve-path TTL, `:554-556`) it **returns true → resolve + Put ELIDED**, bumping
`pipSeedFreshSkipTotal` (a fresh-skip), NOT `pipBindingSetSeedResolvesTotal` (a Put). The
counter doc is explicit (`phase1_pip_metrics.go:102-114`): *"A boot re-drive over a
fully-warm set drives this [fresh_skip] ≈ the chunk-1 seeded count while seed_resolves stays
flat."*

**Consequence for the progress signal (this is gate item 1, and my earlier formula was
WRONG):** in the pure-marketplace-detail shape, every healthy cohort cell is already warm from
the FIRST successful re-walk, so on subsequent stable re-walks they **fresh-skip (no Put)** and
only marketplace-detail is re-attempted and re-fails. So `seededThisPass` (= actual Puts /
`seed_resolves` delta) is **≈ 0 on a stable re-walk** — meaning a `seededThisPass > 0` progress
test would NOT be true-forever… BUT it is not reliably 0 either: cells lazy-expire at TTL, so
the re-walk *after* an expiry re-seeds them (Put, `seededThisPass > 0` again), oscillating with
TTL. **Neither a count nor a "seeded>0" flag is a sound progress signal.** The sound signal is
a SET-DELTA: *did the set of successfully-seeded targets GROW, or did the set of failing
targets SHRINK, versus the prior pass?* A TTL-driven re-Put of an already-seeded target does
NOT grow the seeded SET (same target key, already a member) — so it correctly reads as "no net
progress," while a genuinely-newly-reached cohort (the #123 admin-cohort the loop was starving)
DOES grow the set. This is exactly the redefinition gate item 1 prescribes, and it is required:
the count-based formula false-negatives the real shape.

### Prior-art check (opens the design)

client-go's workqueue ALREADY has the primitive: `RateLimitingInterface.NumRequeues(item)`
returns the per-item failure count (`default_rate_limiters.go:110-115`), and the controller
idiom is `if q.NumRequeues(item) < N { AddRateLimited } else { Forget + log-drop }` (the
standard "give up after N" pattern). We are NOT reinventing — we thread the SAME counter the
engine already owns. The gap is only that the engine's re-enqueue for a *swallowed per-target
seed failure* bypasses the queue's requeue accounting entirely (it's a fresh `Add`, not
`AddRateLimited`), so `NumRequeues` never counts it.

### Options

**(i) Distinguish deterministic-jq-failure from transient at the classifier + stop
re-enqueueing the former.** Add a `seedFailDeterministic` verdict to `classifySeedErr` that
matches the resolve/jq-input family (a snowplow-side sentinel error type wrapping
`jqutil.Eval` failures — NOT a string match). `classifyEngineSeedErr` logs it (new
`prewarm.engine.seed.deterministic_failure` WARN + counter) but does **NOT** re-enqueue.
- Pro: surgical, targets the exact mis-classification; jq-agnostic (any resolve-input error).
- Con: requires a typed sentinel from the resolver (touches `restactions.go` +
  `jqutil`/`jqsupport` to wrap eval errors in a `ResolveInputError` type) so the classifier
  can `errors.As` it — a cross-package error-contract change. Risk: mis-scoping the sentinel
  (a genuinely-transient error that happens to flow through Eval would be wrongly given up).

**(ii) Per-boot-scope-lifetime re-enqueue bound (poison-pill on the boot scope itself).**
Track the boot re-walk's consecutive-failed-with-same-failure-set count; after N consecutive
re-walks that made **no forward progress** (same set of failing targets, no newly-seeded
target), stop re-enqueueing and emit `prewarm.engine.boot.converged_with_skips` (boot is
"done, N items given up"). Keepwarm/config-vars redrive still re-arms (a topology change
gets a fresh attempt).
- Pro: mechanism-level, jq-agnostic, no resolver contract change, no error-taxonomy risk.
  Directly answers "boot must converge." Bounds ANY deterministically-failing item, not just
  jq (RBAC-deny is already not-re-enqueued at `:572-581`, so this catches the residual
  operational-but-deterministic class).
- Con: needs a SOUND progress signal (§5.0) — a SET-DELTA (seeded-SET grew OR failed-SET
  shrank), NOT a count/flag. A count-based "seeded>0" false-negatives the pure-marketplace-
  detail shape (§5.0: healthy cohorts fresh-skip on stable re-walks, and TTL re-Puts oscillate
  the count without growing the seeded set). The set-delta is derivable from state the pass
  already touches (§As-built).

**(iii) Per-item bounded retry via `NumRequeues`.** Route the classifier's re-enqueue through
`AddRateLimited` (so `NumRequeues` counts) and give up a *specific target* after N. Problem:
the queue item is the BOOT SCOPE (one key, no per-target payload) — `NumRequeues("boot")`
counts boot re-walks, not per-target failures. To bound per-target you'd need per-target queue
items, a much larger refactor (Ship 2's `scopeKindWidgetCR` shape, not built). Rejected as
disproportionate.

### RECOMMENDATION: **(ii) primary, with (i)'s counter as telemetry.**

Option (ii) is the right altitude: it bounds the **blast radius** (the full re-walk
re-enqueue) at the mechanism that owns it (the boot scope), is jq-agnostic and
special-case-free, needs no cross-package error-contract change, and directly makes the
symptom (unbounded re-walk, seed budget reset) disappear — boot converges with the failing
item recorded as given-up. It composes cleanly with the existing `reEnqueued` per-pass bound
(that stays; (ii) adds the *cross-pass lifetime* bound the current code lacks).

Do NOT do (i) as the primary: a null-degradation / swallow-the-error path risks masking a
real transient (the exact `feedback` hazard — a null-guard that swallows real errors is
unsound). (ii) preserves the WARN + counter (the operator still SEES marketplace-detail fail
every pass until convergence-with-skips fires) — it stops the *re-walk amplification*, not
the *visibility*.

### As-built target + LOC bound

- **`prewarm_engine_boot.go` `classifyEngineSeedErr` (:570-598)** — instead of the
  unconditional `enqueueScope`, RECORD the failing target into this pass's `failedSet` (a
  `map[string]struct{}` keyed on kind+"/"+label, the same identity the WARN already logs).
  Reuse the engine-lived-per-scope-key map pattern ALREADY present for the declined-external
  set (`prewarm_engine.go:299-311/368-380/398-411`) — add a sibling
  `map[string]*bootConvergenceState` keyed on `s.key()` holding
  {seededSet, priorFailedSet, consecutiveNoProgressPasses}. The re-enqueue decision MOVES OUT
  of the classifier to the END of `seedScopeYielding` (where the whole pass's failedSet is
  known and the cumulative seededSet is available).
- **The seededSet must be recorded at the SUCCESS site, not inferred from a count.** At each
  genuine Put (`phase1_pip_seed.go:836` restaction, the widget mirror) — i.e. the same site
  that bumps `pipBindingSetSeedResolvesTotal` — add the target key to the boot-scope
  `seededSet`. Fresh-SKIPPED targets (`seedSkipDecision`→true, §5.0) are ALREADY-seeded
  members and are added too (a fresh-skip means "this target IS warm this boot" — it belongs
  in the seeded SET even though it did no Put this pass). This is the crux of the set-delta:
  the seeded SET is the union of "Put this pass" ∪ "fresh-skipped this pass" = "warm targets
  reached this boot," which GROWS only when a genuinely-new cohort/target is reached, and does
  NOT grow on a TTL re-Put of an already-member target.
- **End of `seedScopeYielding` / `rePrewarmBootScoped`** — compute the SET-DELTA (NOT a count):
  `madeProgress = (len(seededSet) grew vs prior) || (failedSet ⊊ priorFailedSet)`. Concretely:
  `grew := seededCountThisPass > priorSeededCount; shrank := len(failedSet) < len(priorFailedSet) && failedSet ⊆ priorFailedSet`.
  If `!(grew || shrank)` increment `consecutiveNoProgressPasses`, else reset to 0. Snapshot
  `priorSeededCount = len(seededSet)` and `priorFailedSet = failedSet` for the next pass.
  Re-enqueue only while `consecutiveNoProgressPasses < bootMaxNoProgressPasses` (const, e.g. 2).
  On the bound emit `prewarm.engine.boot.converged_with_skips{given_up_targets, passes}`.
  **This formula does NOT false-negative the pure-marketplace-detail shape** (§5.0): once every
  healthy cohort is warm, `seededSet` is stable (fresh-skips keep them members, no growth) and
  `failedSet` is stably `{marketplace-detail}` (no shrink) → no progress → the bound fires. A
  TTL re-Put re-adds an already-present member (no growth) → still no-progress → still fires.
- **Cleared** on genuine boot completion (Forget) and config-vars redrive — REUSE the
  existing `clearDeclinedExternalSet` teardown point (`prewarm_engine.go:398`,
  boot.go:644) so the state doesn't leak across boots and a topology change re-arms with a
  fresh empty seededSet/failedSet (a new nav topology gets a genuine fresh attempt).

**LOC bound: ~55-75 LOC** (the state struct + the map-plumbing mirrored on the existing
declined-external pattern + seededSet recording at the Put + fresh-skip sites + the set-delta
compare + one gated re-enqueue). No resolver change, no new env knob (const bound, F4-C6
style), no special-case. Slightly above my earlier ~40-60 estimate because the sound signal
needs seededSet recording at two success sites (Put + fresh-skip), not a free counter delta.

---

## 6. PORTAL HAND-OFF (for Diego, not snowplow scope)

marketplace-detail's jq should be null-hardened at the portal source
(composition-controller/portal-chart template that generates the krateo-system
marketplace-detail RESTAction). This is the #32-class hand-off: snowplow cannot edit it (edits
revert). Surface to the portal-chart/composition-blueprint owner. Fixing it removes the
marketplace-detail INSTANCE but the §5 fix is still required (it bounds the CLASS).

---

## 7. FALSIFIER (discriminating, hermetic)

Boot-walk dynamics → drive the REAL `seedScopeYielding` re-invocation path (per
`feedback_falsifier_must_drive_real_boundary_not_install_crossed_state`: hand-installing the
crossed-over state is inadmissible — the re-walk must re-invoke ≥N times for real).

Hermetic (no kind cluster needed — the #47 harness is NOT required): the seed primitives are
already seamed (`seedOneRestactionFn`/`seedOneWidgetFn`/`restActionTargetGVRFn`,
boot.go:445-446) and `prewarmScopeTimeoutFn` (prewarm_engine.go:522) can shrink the budget.

> **Every arm MUST include ≥1 target that SUCCESSFULLY seeds on EVERY pass alongside the
> failing one** (gate item 2, `feedback_falsifier_shape_must_discriminate`). This is the
> discriminator that RED-fails a `seededThisPass > 0` / count-based impl (which would see the
> healthy target's per-pass activity as "progress forever" and never fire the bound — the
> §5.0 false-negative) and only passes the SET-DELTA impl. A total-failure toy (failing target
> alone, `seededThisPass` trivially 0) is BLIND to that bug and is INADMISSIBLE as the primary
> arm.

**Arm A (the shape-of-the-real-bug arm; RED = current unbounded AND RED = a count-based fix):**
a boot scope with (i) a **healthy** target whose `seedOneRestactionFn` SUCCEEDS every pass
(and, to model the real §5.0 dynamics, is FRESH-SKIPPED on passes ≥2 since it's now warm) PLUS
(ii) a target whose `seedOneRestactionFn` returns a **deterministic** non-apierror, non-ctx
error every pass (a synthetic "resolve filter: expected a string"-shaped `errors.New`, NOT
marketplace-detail — jq-agnostic). The healthy target is the discriminator: its seededSet
membership is stable from pass 2 (no growth), the failedSet is stably `{deterministic}` (no
shrink) → no-progress → bound fires. Assert with the fix: `rewalk_complete` count ≤
`bootMaxNoProgressPasses`+1 then STOPS, and `converged_with_skips` fires with ONLY the
deterministic target in `given_up_targets` (the healthy target is NOT given up). **Double RED:**
(a) current code — re-walk count unbounded → RED; (b) a `seededThisPass > 0` impl — the healthy
target's pass-1 Put + any TTL re-Put keeps `seededThisPass > 0` intermittently, so a count-based
"progress" reading NEVER converges (or converges only by luck of TTL timing) → assert it does
NOT reliably fire within the bound → RED. Only the set-delta impl passes deterministically.

**Arm B (transient still retries — the fix doesn't kill legitimate retry):** a healthy
always-succeeds target PLUS a target that fails on the first pass with a **ctx-deadline /
IsServiceUnavailable** error but the seam is flipped to succeed on a later pass. Assert the
boot scope IS re-enqueued and the transient target eventually seeds — its move from failedSet
to seededSet is a set-delta (failedSet shrank AND seededSet grew) → `consecutiveNoProgressPasses`
resets → `converged_with_skips` does NOT fire. Discriminator: a degenerate "give up after N
unconditionally" kills this arm; only "give up after N *consecutive-no-set-progress*" passes it.

**Arm C (mixed, all three classes):** one always-succeeds + one deterministic-failing + one
transient-then-succeeds target in the same scope. Assert boot converges with EXACTLY the
deterministic target in `given_up_targets` (always-succeeds seeded, transient eventually
seeded, deterministic given up). Proves the bound is per-failing-target-set-progress, not
whole-scope, AND that a still-making-progress scope (transient not yet resolved) is not
prematurely given up while the deterministic one is pending.

**Falsifier ground-truth for the ON-CLUSTER proof:** on a deploy carrying the fix, the log
shows a BOUNDED number of `prewarm.engine.boot.rewalk_complete` lines followed by ONE
`prewarm.engine.boot.converged_with_skips{given_up_targets:[krateo-system/marketplace-detail]}`,
and `prewarm.engine.boot.complete` fires (seed reaches admin cohorts — the #123 progress the
loop was resetting). Pre-fix: `rewalk_complete` recurs indefinitely, `boot.complete` for the
full cohort set never lands. This is the specific wire-signal that proves symptom-disappear.

---

## 8. "Would this fix make the symptom disappear?" (self-check)

Symptom = (a) unbounded re-walks, (b) seed never reaches admin cohorts because each re-walk
resets progress. The fix stops re-enqueueing after N no-progress passes → (a) bounded by
construction. Once re-enqueue stops, the LAST pass runs to `boot.complete` without being
interrupted/reset by a fresh re-walk → the seed proceeds through the full cohort set → (b)
resolved. The failing target is recorded given-up (WARN+counter preserved) so the operator
still sees it — no silent swallow. YES, symptom disappears, and it disappears for the CLASS
(any deterministic seed failure), not just marketplace-detail.

Residual: marketplace-detail's own cell stays cold (falls back to per-user resolve at /call —
which will ALSO error on the bad jq, but that's the portal jq defect, §6, not the amplifier).
The amplifier — the thing that was starving admin-cohort seed budget forever — is gone.
