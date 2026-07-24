# F4b Lever B — reuse the harvested discovery snapshot on boot RESUME passes (#135)

Date: 2026-07-22
Author: cache-architect
Repo: main
Source design: `docs/f4b-seed-overshoot-design-2026-07-14.md` §"Lever B" (~:105), §6, §7, falsifiers 4+5 (~:127-128).
Prior ship (context, do NOT re-scope): Lever A landed 1.7.12 (#132) — engine-lived declined-external set, `declined_external_keys=63` live.

---

## HEADLINE VERDICTS (read these first)

**1. Invalidation-seam verdict: SOUND, and it is ALREADY WIRED — Lever B needs NO new invalidation code.**
The load-bearing worry ("a resume after a topology change seeds a stale target set") does not arise, for a reason the source design did not fully account for: the `navWidgetHarvester` + `contentPrewarmHarvester` are **already process-lived, monotonic, first-write-wins accumulators** shared by reference across every walk pass (they are NEVER cleared in production — TRACED below). Lever B does not add a new cached snapshot; it **stops re-driving `walk()` on resume** and reads the harvester the walk already filled. The only genuine question is: on a real config-vars INIT change, does the resume re-walk so the NEW nav set is discovered? Answer: **yes, for free** — a config-vars redrive is NOT a `NumRequeues>0` resume; it is a fresh `enqueueScope` with backoff reset, so the `attempt==0` gate re-walks it. The seam is a one-line `NumRequeues(s)==0` read of state client-go already tracks, not a new invalidation subscription. **Recommend: BUILD.**

**2. #123-framing verdict: Lever B does NOT widen the #123 margin. It hardens aggregate boot convergence under resume-thrash only.**
Lever B removes ~255s of rewalk from RESUME passes. Pass 1 (the #123 ceiling, 459.6s of per-cohort seed jq, latch at 376s) has `attempt==0` so it STILL walks — Lever B touches zero of pass-1's cost. The true #123 reducer is the §6 per-cohort-baseline split (resolve the `allCompositions` substrate once, filter per cohort). **Recommend: book that as its own task (#123/#136), higher #123 leverage, out of scope here.** Lever B is worth shipping regardless because Lever A alone does NOT remove the rewalk (see §3.3) — a resume pass that Lever A shrank to ~0 seed still pays 255s of rewalk, so aggregate resume passes stay ~255s each and the boot scope keeps burning the requeue budget on rediscovery.

---

## 1. Root cause (TRACED)

`rePrewarmBootScoped` (`internal/handlers/dispatchers/prewarm_engine_boot.go:228`) runs the FULL discovery re-walk on EVERY invocation, including every F.4 deadline-cut resume:

- **The re-walk loop** — `prewarm_engine_boot.go:291-328`: `deps.navHarv.BeginWalk()` then per-root a FRESH `newPhase1Walker` + `w.walk(...)`. This is the 253-261s `rewalk_complete elapsed_ms` (source design §1.1 table).
- **`rewalk_complete` emitted** — `prewarm_engine_boot.go:364-371`, AFTER the walk, reporting `widgets:191 restactions:34` identical every pass.
- **The resume path that re-enters it** — `processScope` (`prewarm_engine.go:571`) → on `err!=nil` with live ctx → `queue.AddRateLimited(s)` (`:621`) → worker re-`Get`s → `processScope` → `scopeHandler` → `rePrewarmBoot` → `rePrewarmBootScoped`. Every resume re-runs the whole function top-to-bottom, walk included.

**Why the walk output is identical across resume passes (the reuse is provably safe):** the harvesters are process-lived accumulators, not per-pass state.

- `newNavWidgetHarvester()` is called EXACTLY ONCE in production — `phase1_walk.go:379` inside `Phase1Warmup`. The instance is stored on `rePrewarmDeps.navHarv` (`phase1_walk.go:426`) and closed into the boot-scope handler; `deps` outlives every pass.
- `harvestNavWidget` (`phase1_pip_seed.go:300`) is **first-write-wins** (`:308` `if _, seen := h.entries[key]; seen { return }`). Entries are never deleted.
- **There is NO clear/reset of `h.entries` anywhere in production code** (verified: grep for `h.entries =` / `delete(h.entries` / `clear(` returns only the constructor at `phase1_pip_seed.go:244`). `BeginWalk`/`BeginRoot` reset only `curRoot` (the RootIndex stamp counter), NOT the entry map.
- `snapshot()` (`phase1_pip_seed.go:387`) returns a copy of the accumulated map.

So on resume pass N, `deps.navHarv.snapshot()` already returns the union of every widget harvested on passes 0..N — the re-walk on pass N adds nothing new (first-write-wins drops every re-harvest). **The re-walk on a resume pass is 255s of pure waste: it re-discovers a set the shared harvester already holds.** `contentPrewarmHarvester` (the RA harvester, `deps.harvester`) has the identical process-lived first-write-wins shape.

This is the root cause: `rePrewarmBootScoped` unconditionally re-drives `walk()` even when the harvester it feeds is already fully populated from a prior pass of the same live boot scope.

---

## 2. Design + placement

**Gate the re-walk (and the two post-walk settle/index steps that depend on it) on `attempt == 0`; on a resume pass, skip straight to the seed over the already-populated harvester snapshot.**

- **Placement:** `rePrewarmBootScoped` (`prewarm_engine_boot.go:228`), guarding the block `:254-345` (the `BeginWalk`/per-root `walk()` loop `:291-328`, `settleRegisteredSet` `:333`, and `BuildBindingsByGVRIndex` `:340`). The seed block `:347-391` (snapshot → `seedScopeYielding`) runs UNCHANGED on both first and resume passes — it already reads the shared harvester's `snapshot()`, which on a resume is the accumulated set.
- **The attempt signal (no new state):** the workqueue already tracks per-item requeue count. `processScope` reads it today at `prewarm_engine.go:626` (`e.queue.NumRequeues(s)` for the `scope_requeued attempt:` log). Thread it into the scope handler. Cleanest seam: compute `attempt := e.queue.NumRequeues(s)` in `processScope` BEFORE calling the handler and carry it on `scopeCtx` via a new tiny `cache.WithBootResumeAttempt(ctx, attempt)` (mirror the existing `WithSeedDeclinedExternalSet` install at `prewarm_engine.go:602-604`, boot-scope-only). `rePrewarmBootScoped` reads it: `attempt == 0` (or absent → 0) walks; `attempt > 0` reuses.
  - Alternative seam (no ctx plumbing): a `bool walk` parameter on `rePrewarmBootScoped`, set by a new `makeBootScopeHandler` branch that inspects `NumRequeues`. Ctx-carry is preferred — it mirrors the Lever A install pattern already in `processScope` and keeps the handler signature stable.
- **Reuse-branch behavior on a resume pass:**
  - SKIP `BeginWalk` + the per-root `walk()` loop (the 255s).
  - `settleRegisteredSet` (`:333`) and `BuildBindingsByGVRIndex` (`:340`): these depend on the walk's discovered GVRs. On a resume, the GVRs were already registered + indexed on pass 0, and the informer set only grows. **Keep `BuildBindingsByGVRIndex` on resume** (it is cheap — an index rebuild over already-registered GVRs, source design measures the 255s as the WALK, not the index; the index build logs its own `index_built` line separately) OR skip it as a further optimization once measured. **Recommend: keep both settle+index on resume for pass-0 parity of the substrate the seed reads; only skip the `walk()` loop.** This is the minimal, provably-safe cut — the 255s is the walk, and skipping only the walk removes the entire measured cost with zero risk to the informer/index substrate.
  - RUN the seed (`:347-391`) exactly as today over `deps.navHarv.snapshot()` — now the accumulated set.
- **feedback_no_special_cases CLEAN (verdict 4):** the reused snapshot is the WALK'S OWN output (`deps.navHarv.snapshot()`), harvested from live config.json roots — not a hardcoded target list, no resource/name/path literal. The `attempt` gate is a data-derived requeue count from client-go, not a magic number. This is a pure "don't recompute an idempotent result" reuse, structurally identical to the F4 memo (`seedScopeYielding:499`) and to the harvester's own first-write-wins dedup. No special case introduced.

---

## 3. The invalidation seam — soundness resolution (THE CRUX)

### 3.1 The two ways the boot scope re-enters, and how they differ

There are exactly two paths that cause `rePrewarmBootScoped` to run again for the `"boot"` key:

**(A) F.4 deadline-cut RESUME** — `processScope:621` `queue.AddRateLimited(s)`. This INCREMENTS `NumRequeues(s)` (client-go's rate-limiter tracks per-item requeues). So the resume pass sees `attempt > 0` → REUSE snapshot. Correct: a deadline-cut is the SAME topology, just an unfinished seed tail; re-discovering the identical set is the waste Lever B kills.

**(B) config-vars redrive (genuine topology change)** — `enqueueBootReDrive` (`phase1_configvars_watch.go:181`) → `enqueueScope(bootScope)` → `queue.Add(s)` (`prewarm_engine.go:357`, the IMMEDIATE add, NOT `AddRateLimited`). Critically: on the PRIOR scope's genuine completion, `processScope:637` called `queue.Forget(s)`, which **resets `NumRequeues(s)` to 0**. And even mid-flight, a config-vars change fires `clearDeclinedExternalSet` + a fresh `Add` — the new dequeue of the boot scope is `attempt == 0` unless it is itself subsequently deadline-cut. So a config-vars redrive re-walks the NEW config.json roots → discovers the new nav set → first-write-wins ADDS the new widgets to the harvester → seed covers them. **The new topology is walked, exactly as required by falsifier 5.**

**This is the seam, and it is already correct by construction of the existing queue semantics** — `Forget`-on-success (`:637`) and immediate-`Add` (not rate-limited) for config-vars redrive (`:357`) mean the requeue counter distinguishes "deadline-cut resume of the same topology" (counter > 0) from "genuine redrive" (counter reset to 0 by the prior Forget). Lever B keys on exactly that distinction.

### 3.2 The ONE residual edge — a config-vars change DURING an unfinished (already-requeued) boot scope

Consider: pass 0 deadline-cuts → `AddRateLimited` → `NumRequeues==1`. Before the resume runs, config.json changes → `enqueueBootReDrive` → `queue.Add(boot)`. Because the boot key is already in the queue (coalesces), the pending item still carries `NumRequeues==1`. The resume then runs with `attempt==1` → REUSE → **would seed the OLD nav set, missing the new topology until the next genuine completion+redrive.** This is the stale-snapshot risk the source design flagged as "the load-bearing correctness condition."

**Resolution — two options, recommend (i):**

**(i) RECOMMENDED — clear the requeue counter on config-vars redrive (1 line, reuses the pattern Lever A already established at the same call site).** `enqueueBootReDrive` ALREADY does `prewarmEngineSingleton().clearDeclinedExternalSet(bootScope.key(), "config-vars-redrive")` at `phase1_configvars_watch.go:192` — for exactly the analogous reason (Lever A's set must be dropped on a topology change). Add a sibling `prewarmEngineSingleton().queue.Forget(bootScope)` (or a thin `e.resetBootAttempt(key)` wrapper) BEFORE the `enqueueScope`. `Forget` resets `NumRequeues` to 0, so the coalesced pending boot item is re-dequeued as `attempt==0` → RE-WALKS the new config. This is byte-for-byte the same "clear-on-redrive" hardening Lever A required, in the same function, and it composes: the config-vars watcher becomes the single invalidation point for BOTH Lever A's declined set AND Lever B's walk-skip. Clean, no new subscription, one line.
  - Caveat to verify in impl: `workqueue.Forget` on an item currently in the queue (not being processed) is safe (it only touches the rate-limiter's failure map, not queue membership) — confirm against the client-go version; if `Forget` semantics on a queued-but-not-processing item are ambiguous, use option (ii).

**(ii) Fallback — snapshot-generation counter.** Bump an engine-lived `bootTopologyGen atomic.Uint64` in `enqueueBootReDrive`; stamp the gen the walk-skip is valid for; reuse only if `attempt>0 AND gen unchanged since the last walk`. More explicit but adds a counter + a compare; (i) reuses existing queue state and the existing clear-on-redrive call site, so it is strictly less surface.

### 3.3 Interaction with the existing self-heal (`TestBootRace_ConfigVarsInformerDrivesReWalk`)

The self-heal falsifier (`phase1_boot_race_selfheal_falsifier_test.go`) drives: config-vars ABSENT → boot scope gives up (`roots_list_failed`, returns error at `prewarm_engine_boot.go:277`) → later the ConfigMap appears → AddFunc → `enqueueBootReDrive` → boot re-walk finds roots. Two facts make Lever B compatible:

- The give-up on absent config returns an ERROR at `:271-278` (`roots_list_failed`) — that is BEFORE the walk. Under Lever B, an `attempt>0` resume would try to reuse an EMPTY harvester (nothing was ever walked). **Guard: reuse ONLY when the harvester snapshot is non-empty** (`len(deps.navHarv.snapshot()) > 0`); an empty snapshot means pass 0 never successfully harvested (config was absent / walk failed) → fall through to a real walk regardless of `attempt`. This makes the give-up→appear→heal sequence still re-walk on the config-vars AddFunc (option (i) resets attempt to 0 anyway; the non-empty guard is defense-in-depth for the deadline-cut-before-any-harvest corner).
- The AddFunc path uses `queue.Add` (immediate) and, with option (i), `Forget` first — so the self-heal re-walk is always `attempt==0` → walks. The falsifier's `bootRuns>=2` assertion (t0 give-up + self-heal re-walk) holds; both runs walk.

**Verdict on the seam: SOUND.** The distinction Lever B needs (resume-of-same-topology vs genuine-redrive) is already materialized in the queue's `NumRequeues`/`Forget` semantics; the one residual edge (§3.2) is closed by a 1-line `Forget`-on-redrive that mirrors Lever A's existing clear-on-redrive at the identical call site; the non-empty-snapshot guard makes the boot-race give-up path re-walk safely. No new invalidation subscription, no new watcher, no topology-diff logic.

---

## 4. Lifetime / ownership

The snapshot Lever B reuses is NOT a new field — it is the existing `deps.navHarv` (+ `deps.harvester`), which is:

- **Created once** (`phase1_walk.go:379`), **engine/deps-lived** (closed into the boot handler via `rePrewarmDeps`), REUSED across every requeue and every config-vars redrive.
- **Grows monotonically** (first-write-wins, never cleared). A config-vars redrive that walks a NEW config ADDS the new roots' widgets to the SAME harvester (correct — a superset seed is safe; the seed's per-target RBAC scoping filters, and over-inclusion is benign per the existing `unionProactiveRARefs` no-special-case note at `prewarm_engine_boot.go:1129`). A widget that DISAPPEARS from the new config remains in the harvester and is re-seeded (a harmless stale seed of a now-unreferenced widget — its cell is per-user-keyed and simply unused; NOT a correctness or leak issue). If a future requirement needs the harvester to SHRINK on topology change, that is a separate concern that predates and is orthogonal to Lever B (today's harvester never shrinks either).
- **The only NEW lifetime element** is the per-scope `attempt` read, which is:
  - OWNED by the workqueue (client-go), not by Lever B — Lever B only reads `NumRequeues`.
  - Reset on genuine completion (`Forget` at `prewarm_engine.go:637`) and, per §3.2 option (i), on config-vars redrive (`enqueueBootReDrive`). So a fresh boot / new topology re-walks once, exactly mirroring how Lever A's declined set is cleared on completion + redrive (`prewarm_engine.go:644` + `phase1_configvars_watch.go:192`).

Ownership summary: no engine-held snapshot field is added (unlike Lever A's `declinedExtSets` map — the harvester already IS the engine-lived accumulator). The clear-on-completion + clear-on-redrive points are the SAME two points Lever A uses, so the two levers share one invalidation story.

---

## 5. Falsifier set (PM-gateable, each ties to a runtime artifact / real re-invocation)

**B-symptom (positive) — `rewalk_complete` fires ONCE per boot scope, not per attempt.**
- Runtime: re-boot the fixed image at 50K; grep `prewarm.engine.boot.rewalk_complete` bucketed against `scope_requeued attempt:` lines. Pre-fix: 6+ `rewalk_complete` lines (one per pass), each `elapsed_ms:253000-261000`. Post-fix: exactly 1 `rewalk_complete` (pass 0) + N `boot.complete` lines whose gap to their `scope_requeued` predecessor is dominated by SEED not rewalk (resume `boot.complete elapsed_ms` collapses from ~340-383s toward the resume seed cost alone, low tens of seconds once Lever A also elides the whale re-seed).
- Corollary: a NEW `prewarm.engine.boot.rewalk_reused` (or a field on `boot.complete`, e.g. `walked:false attempt:N`) INFO line on resume passes, so the skip is greppable and the count of skips == number of resume passes.

**B-safety (load-bearing, RED-blocking) — config-vars INIT change DURING a boot resume → the next seed pass uses the NEW target set (re-walks); driven through the REAL ≥2× resume boundary.**
Per `feedback_falsifier_must_drive_real_boundary_not_install_crossed_state`: the arm MUST drive the actual resume re-invocation twice with a genuine config-vars change between — NOT hand-install a stale snapshot.
- Harness (extends `phase1_boot_race_selfheal_falsifier_test.go`'s real-informer + real-engine pattern): 
  1. Config-vars present with roots {A}. Drive boot scope pass 0 (`attempt==0`) → walks → harvester holds {A}. Force a deadline-cut (via `prewarmScopeTimeoutFn` test seam, `prewarm_engine.go:522`) so `processScope` `AddRateLimited`s → `NumRequeues==1`.
  2. **Genuine config-vars change**: update the ConfigMap Data to roots {A,B} → REAL UpdateFunc → `configVarsDataChanged==true` → `enqueueBootReDrive` (with §3.2 option (i): `Forget` resets attempt to 0).
  3. Drive the NEXT dequeue through the REAL worker/`processScope`. Assert: it WALKED (`rewalk_complete` fired again) and the harvester/seed now covers {B} (new root). 
  - **RED arm** = remove the §3.2 `Forget`-on-redrive (or the non-empty guard): the resume runs at `attempt==1` → REUSE → seeds only {A} → B is missing → assert-B-seeded FAILS. This discriminates the stale-snapshot bug precisely.
  - Second RED arm (the pure symptom guard): with NO config change, drive pass 0 (walk) → deadline-cut → resume: assert the resume did NOT call `walk()` (a `walkCalled` counter via the existing `deps.lister`/walker seam stays at 1). RED = gate always-walks → counter==2.

**B-safety corollary — boot-race give-up path still self-heals under Lever B.** Reuse `TestBootRace_ConfigVarsInformerDrivesReWalk` unchanged: config absent → give-up (empty harvester) → ConfigMap appears → AddFunc → re-walk. Assert `rewalk_complete` fires on the self-heal pass (non-empty guard + attempt-reset both force the walk). RED = a naive `attempt>0→reuse` with no non-empty guard would try to reuse the empty harvester and seed nothing → `roots>0` assertion (`selfheal_falsifier_test.go:241`) fails.

**Aggregate milestone (unchanged from source §5.6):** `boot.complete` for the boot scope converges (no `scope_requeued attempt≥2` churn over 10 min); `first_nav.latch` still fires via `segment-complete` on pass 0 (Lever B does not touch pass-0 latch timing — pass 0 walks). 50K canonical bar (warm 911 / cold 2053 / conv ~24s per `project_current_state`) must not regress.

---

## 6. Implementation LOC estimate

~25-40 LOC:
- `rePrewarmBootScoped` gate: read attempt off ctx, `if attempt==0 || len(navHarv.snapshot())==0 { walk block } else { skip-walk, log rewalk_reused }` — ~12 LOC (wrap `:254-333`, keep `:340` index build).
- `processScope` (`prewarm_engine.go:602-604` area): compute `attempt := e.queue.NumRequeues(s)` and `scopeCtx = cache.WithBootResumeAttempt(scopeCtx, attempt)` in the boot-scope branch — ~3 LOC.
- `cache.WithBootResumeAttempt` / `BootResumeAttemptFromContext` (mirror `WithSeedDeclinedExternalSet`) — ~10 LOC in `internal/cache`.
- `enqueueBootReDrive` §3.2 fix: `prewarmEngineSingleton().queue.Forget(bootScope)` (or `e.resetBootAttempt`) before `enqueueScope` — ~1-3 LOC, adjacent to the existing `clearDeclinedExternalSet` call at `phase1_configvars_watch.go:192`.
- New `rewalk_reused` log line — ~5 LOC.

No production code written here (design only). This is well inside the source design's ~40-60 LOC estimate; it comes in LOWER because the harvester reuse is free (no new snapshot field) — the source design assumed a new cached-snapshot field that turned out to be redundant with the existing accumulator.

---

## 7. #123 framing verdict (explicit, data-driven) + per-cohort-baseline recommendation

**Does Lever B widen the #123 margin? NO.** #123 is the pass-1 seed ceiling: 459.6s of per-cohort seed jq, `first_nav.latch` at 376s (< 480s — it fit), scale-linear in cohort count (source §2, §7; #136 re-grounds cohort count at ~20 not 6). Pass 1 runs at `attempt==0` → Lever B walks it → Lever B removes ZERO of pass-1's cost. Lever B removes ~255s of rewalk from RESUME passes only. So Lever B's contribution is **aggregate boot convergence under resume-thrash**, not #123 margin: with the rewalk gone AND Lever A's whale re-seed gone, a resume pass drops from ~340s to the seed-tail cost, so the F.4 cost-proportional resume actually converges the deadline-cut tail instead of each requeue paying 255s of rediscovery and re-thrashing. That is real and worth shipping (it is what makes the boot scope stop requeuing), but it must not be sold as a #123 fix.

**The true #123 reducer (§6 of the source design): resolve the `allCompositions` substrate ONCE across cohorts, filter per cohort.** Source §2 traces the pass-1 mid-mass (259s) to the per-cohort `allCompositions`-over-29-GVRs double-pass floor (`stage=allCompositions iter_calls:29`), paid once per (widget × cohort). The informer-served substrate bytes are cohort-INDEPENDENT; only the RBAC filter is per-cohort. A "resolve substrate once, filter N times" split attacks the dominant pass-1 term directly and scales sub-linearly in cohort count — exactly the lever #123/#136 need. It is a larger design (touches the resolve stage's substrate/filter boundary, cache-key implications, RBAC-filter correctness under a shared substrate) and is genuinely the higher-leverage #123 item.

**Recommendation to TL/Diego:** ship Lever B for resume-thrash hardening (it is the necessary complement to Lever A — A alone leaves 255s of rewalk on every resume, so the boot scope keeps churning). **Book the per-cohort-baseline substrate-share as its own design task under #123/#136** — it is the real pass-1/#123 reducer and should be surfaced to Diego as the next boot-budget item, NOT folded into Lever B. Sequencing (source §7): A shipped → B (this) → then the substrate-share design if the on-cluster cohort count (#136, ~20) pushes pass-1 toward 480s.

---

## Appendix — file:line index (all TRACED against current main)

- Re-walk loop + `rewalk_complete`: `prewarm_engine_boot.go:254-371` (gate target).
- Seed block (unchanged by Lever B): `prewarm_engine_boot.go:347-391`.
- Resume re-entry (AddRateLimited): `prewarm_engine.go:609-632`; `Forget`-on-success (attempt reset): `:637`.
- Attempt signal source: `prewarm_engine.go:626` `e.queue.NumRequeues(s)`.
- Lever A install pattern to mirror: `prewarm_engine.go:602-604` (`WithSeedDeclinedExternalSet`, boot-scope-only).
- config-vars redrive (invalidation call site + existing Lever A clear): `phase1_configvars_watch.go:181-200`, esp. `:192`.
- config-vars DATA-change gate (#106): `phase1_configvars_watch.go:163-171`, `:283-299`.
- Harvester single-construction: `phase1_walk.go:379`; stored on deps `:426`.
- Harvester first-write-wins (never cleared): `phase1_pip_seed.go:300-344`; snapshot `:387`.
- Boot-race self-heal falsifier (compat + reuse): `phase1_boot_race_selfheal_falsifier_test.go` (esp. `:241` roots>0 assert, `:272` bootRuns>=2).
- Deadline-cut test seam (drive resume without a live cluster): `prewarm_engine.go:522` `prewarmScopeTimeoutFn`.
