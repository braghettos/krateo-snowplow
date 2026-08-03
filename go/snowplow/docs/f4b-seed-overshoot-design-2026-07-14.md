# F4b — the single-pass seed overshoot (TASK #132)

Date: 2026-07-14
Author: cache-architect
Repo: main @ f2c7c86 (1.7.11)
Artifacts (read-only): `/tmp/f6-deploy/boot-1.7.10-milestone-t0.log` (the 50K milestone boot — the ONLY log that carries the 50K seed segment; the two west4 logs are the tiny 27-composition corpus, latch ~37s, and do NOT reproduce the overshoot).
Code: `internal/handlers/dispatchers/phase1_pip_seed.go`, `internal/cache/seed_resolve_memo.go`, `internal/handlers/dispatchers/prewarm_engine_boot.go`.

## Headline

**The "654s vs 370s same-config variance" and the "~500s single-pass overshoot" are a MEASUREMENT ARTIFACT, not two draws of one random variable.** One `prewarm.engine.boot.complete elapsed_ms` line sums two structurally different phases whose mix differs by pass, so comparing raw `elapsed_ms` across passes compares apples to oranges. Decomposed against the milestone log:

- **Pass 1** (the genuine first boot seed): 1000 widget resolves = **459.6s** of seed jq, `first_nav.latch` at 376s, scope CUT at the 480s `pipGlobalTimeout` → `scope_incomplete "context deadline exceeded"` → `scope_requeued attempt:1`. **This is the real overshoot.**
- **Passes 2–6** (F.4 deadline-cut resume): each `boot.complete` reports `elapsed_ms` 340–383s, but only **~80s** of that is seed work (9 distinct widgets re-resolved × ~5 cohorts = 45 `widget.timing` lines). The remaining **~255s is the DISCOVERY RE-WALK** (`rewalk_complete elapsed_ms` = 253–261s), which re-runs every resume pass and is not seed jq at all.

So the fix is **not** "extend the F4 memo to widget-data" and **not** "bump PHASE1." The F4 memo already works (`memo_hits:492/misses:90` on pass 1). The two real levers are (§4): stop the **external-backed whale widgets re-resolving on every pass** (a self-inflicted infinite loop, TRACED below), and get the **255s discovery re-walk out of the resume hot path**.

## 1. Variance root cause (TRACED)

### 1.1 The two phases inside one `boot.complete elapsed_ms`

Bucketing every `phase1.seed.widget.timing` line by the `boot.complete` interval it falls in (milestone log):

| pass | scope | `boot.complete elapsed_ms` | widget.timing lines | Σ widget seed (s) | `rewalk_complete elapsed_ms` |
|---|---|---|---|---|---|
| 1 | boot | 340815 | 1000 | **459.6** | 84295 |
| 2 | boot | 377963 | 45 | 85.3 | 207731 |
| 3 | boot | 381963 | 45 | 88.8 | 254059 |
| 4 | boot | 367049 | 45 | 73.7 | 253081 |
| 5 | boot | 377765 | 45 | 80.2 | 255492 |
| 6 | boot | 382890 | 45 | 85.1 | 261112 |

`boot.complete elapsed_ms` is the scope's total wall-clock. Pass 1's is dominated by 459.6s of seed jq (cut at 480s). Passes 2–6's is dominated by the ~255s rewalk plus ~80s of whale re-seed. **Two passes with the "same config" therefore legitimately measure very different `elapsed_ms` — because they run a different phase mix, not because the same work took a random amount of time.** The "654 vs 370" pair the brief cites is this exact confound (a pass-1-like number vs a resume-like number).

Evidence lines (milestone log):
- `first_nav.latch ... reason:"segment-complete" elapsed_ms:376346` (22:49:52) — the seed's first-nav segment finished at 376s.
- `scope_incomplete scope:"boot" err:"context deadline exceeded"` (22:50:13) — 480s budget hit ~21s later.
- `scope_requeued scope:"boot" attempt:1` — F.4 resume.
- `boot.complete scope:"boot" elapsed_ms:340815` — first `boot.complete` (the resume chunk that continued after the cut, NOT pass 1's full wall-clock).

### 1.2 Why the widget.timing count collapses 1000 → 45 on resume (F.4 fresh-skip IS working)

`seedSkipDecision(seedModeBoot, ...)` returns `true` for a live L1 cell and the caller returns *before* the timing sink is installed (`phase1_pip_seed.go:870` skip returns before the `defer` at `:908`). **A skipped widget emits no `widget.timing` line.** So the drop from 1000 (pass 1) to 45 (passes 2–6) is direct proof the F.4 boot fresh-skip skipped the 955 already-warm internal widgets — exactly its designed behavior. (Note: `phase1.seed.fresh_skip` is `slog.Debug` and the milestone boot logs at INFO — zero DEBUG lines — so the fresh_skip *log* is silent; the absence of the timing line is the correct falsifier, not the fresh_skip line.)

## 2. Overshoot decomposition — pass 1's 459.6s (TRACED)

Cost histogram of the 1000 pass-1 widget resolves (`elapsed_ms_total`):

| bucket | count | note |
|---|---|---|
| < 100 ms | 749 | trivial (memo-hit / cheap / no-apiRef widgets) |
| 100–500 ms | 28 | |
| 500–1000 ms | 38 | |
| 1000–3000 ms | 120 | **the mass** |
| > 3000 ms | 20 | 116s total |

- **The residual is NOT the heavy-jq tail.** The 20 widgets > 3s sum to only 116s, and the F4 memo already collapses them (`memo_hits:492`). The mass is the **mid bucket: 120 widgets at 1–3s + 38 at 0.5–1s ≈ 259s.**
- **The mid bucket is the per-cohort fan-out, not any hot widget.** Aggregating pass-1 widget cost by name shows every heavy widget appears with `cnt=5` (once per cohort). There are **6 cohorts** (`group:admins`, `group:devs`, `group:krateo:snowplow-seed`, `group:system:masters`, `system:gke-common-webhooks`, `system:kubestore-collector` — from `restaction.timing` cohort fields). Each widget's per-cohort resolve pays a baseline (`allCompositions` iterates 29 GVRs even when informer-served: `stage=allCompositions iter_calls:29 el_ms:~550` × 2 passes/widget = ~1.1s floor per widget-cohort even for a *cheap* widget like `greeting-subtitle`). **1000 widget-cohort resolves × ~0.3–1.1s floor = the 259s mid mass.** The memo can't touch this: it collapses *identical* (RA, identity, page) resolves, but each cohort is a distinct identity, so the per-cohort baseline is genuinely distinct work.
- Restactions add 15.8s (57 resolves), negligible.

**So pass 1 = ~116s heavy (memo-reduced) + ~259s per-cohort-baseline mid + ~85s small/trivial ≈ 460s.** The dominant term is the 6× per-cohort re-execution of the ~166 non-trivial widgets, each carrying the `allCompositions`-over-29-GVRs double-pass floor.

## 3. The resume-loop root cause — external whales re-resolve forever (TRACED, this is the real budget-burner)

The 9 widgets that re-resolve on **every** pass (passes 2–6, `cnt=5` each) are precisely the ones the #102 GTTL-1 `declineSeedPutOnError` gate declines:

```
"msg":"phase1.seed.skip.external_touch" class:"widgets" target:"krateo-system/list-activity-events" external_touches:2
```

Decline counts over the boot (milestone log): `search-results` 37, `obs-throughput-card` 37, `obs-resource-card` 37, `obs-reconcile-by-composition` 37, `obs-perf-p50` 37, `obs-log-stream` 37, `obs-errors-card` 37, `marketplace-source-toggle` 37, `list-activity-events` 37 — **all via `external_touch`, zero via `stage_error`.** These are the observability/ClickHouse + marketplace widgets (the #129 external-widget class; `endpointRef≠None`).

**The self-inflicted loop (file:line chain):**
1. `seedOneWidget` resolves the whale; the resolve touches an external endpoint (`WithExternalTouchedSink` records it).
2. `declineSeedPutOnError` (`phase1_pip_seed.go:950` → `:1178`) sees `extTouchedSink.Count() > 0`, **declines the `handle.Put`**, returns nil. The cell is **never warmed**. (Correct by design — an external touch has no dep edge to invalidate it, so a warm entry would go stale silently.)
3. Next pass, `seedSkipDecision(seedModeBoot, ...)` (`:870` → `:529`) does `handle.Get(key)` → **MISS** (never Put) → returns false → **the whale is re-resolved from scratch**, paying its full external round-trip (`search-results` = 42.5s/pass).
4. GOTO 2, forever, every resume pass.

`search-results` alone is 42.5s per pass × 5 resume passes = **~210s of pure repeated external work** the seed can never make progress on, because the Put is (correctly) declined and the skip (correctly) misses. This is the loop that keeps the boot scope requeuing and re-walking.

**Prior-art check:** k8s/client-go has no analogue — this is snowplow's own seed↔decline interaction. The refresher (`resolve_populate.go`) has the same decline gate but is TTL-timer-driven, not a tight resume loop, so it doesn't spin. The fix must break the seed's decline→miss→re-resolve cycle without warming an un-invalidatable external cell.

## 4. Fix design (chosen levers, with LOC bound + file:line)

Two independent levers. **Lever A is the primary (breaks the resume loop); Lever B removes the redundant rewalk cost.** Neither touches the F4 memo (working) nor the budget (a concession, rejected §6).

### Lever A — seed-skip declined-external widgets on resume (breaks the §3 loop). PRIMARY.

**Root cause it targets:** the whale is re-resolved every pass because the Put is declined and the skip Gets a miss. The seed has no memory that "this (widget, cohort) was resolved and *intentionally* not Put (external)." Give it that memory, scoped to the boot pass set, so a resume pass skips a widget it already resolved-and-declined this boot.

**Design:** record a per-(class,key) "resolved-but-declined-external" marker in an **engine-lived set, keyed per boot-scope-key** (mirror the `SeedResolveMemo` type/idiom, but with ENGINE — not per-pass-context — OWNERSHIP; see the lifetime correction below). In `seedSkipDecision(seedModeBoot, ...)`, before the `handle.Get`, consult the marker: if this key was already resolved-and-declined *this boot scope* (across any of its resume passes), skip (the cell is intentionally cold; re-resolving it every resume pass makes zero forward progress and only burns budget). Set the marker in `declineSeedPutOnError`'s external-touch branch.

- **LIFETIME (corrected as-built — the original "boot-scope-lived / per-context" wording was WRONG and made the fix inert; the arch owns this ambiguity).** The §3 loop is **CROSS-PASS**: a boot RESUME is a *fresh* `seedScopeYielding` invocation (`AddRateLimited` requeue → `processScope` → `rePrewarmBoot` → `rePrewarmBootScoped` → `seedScopeYielding`), and within one pass each (widget,cohort) is visited exactly once. A set `new`ed per `seedScopeYielding` pass therefore starts empty every resume and `Marked()` can never fire in prod — the whales re-resolve every pass exactly as pre-fix (this is what the first build, 2dc46ae, did — correctness-inert). **The set must be ENGINE-LIVED, keyed by the scope key (`"boot"`):** the `prewarmEngine` holds one set per scope key, created on the scope's first `processScope`, **REUSED across the scope's `AddRateLimited` requeues**, and **CLEARED when the scope genuinely completes** (`err==nil` → `Forget`) **OR on a config-vars redrive** (new topology → whales re-resolve once under the new nav set). The engine installs the set onto the scope ctx in `processScope` before invoking the handler; `seedScopeYielding` / `seedSkipDecision` / `declineSeedPutOnError` read/write it off ctx via `cache.SeedDeclinedExternalSetFromContext` (still the context-carried access idiom — only the OWNERSHIP moves to the engine).
- **Placement (as-built):** the set type + accessors live in `internal/cache/seed_declined_external_set.go`; the engine holds `declinedExtSets map[string]*SeedDeclinedExternalSet` + `declinedExternalSetFor(key)` (get-or-create, reuse) + `clearDeclinedExternalSet(key)` (`prewarm_engine.go`); installed onto `scopeCtx` in `processScope` (boot scope only), cleared in the `Forget`-on-success branch; cleared on config-vars redrive in `enqueueBootReDrive` (`phase1_configvars_watch.go`); consulted in `seedSkipDecision` case `seedModeBoot` before `handle.Get`; populated in `declineSeedPutOnError` external branch. Key = the same `key` the Put/Get use (single-derivation, no drift — `feedback_consultation_mutation_is_not_key_correctness`).
- **Why this is safe / correct:** it only skips on *resume passes within the same live boot scope* (the set is cleared on genuine completion, so a later fresh boot re-resolves each whale once; cleared on redrive, so a new topology re-resolves once). The FIRST boot pass still resolves each external whale once. It does not warm an un-invalidatable cell (the whole point of the #102 decline is preserved). It changes only whether the seed *wastes budget re-resolving a cell it will decline again on the same boot's resume passes*.
- **R2/R3/R4 as-built (PM re-gate specifics):**
  - **R2 clear on BOTH triggers (load-bearing):** `clearDeclinedExternalSet` fires on genuine `boot.complete` (Forget branch) AND on config-vars redrive (`enqueueBootReDrive`). A `mark → redrive → next-invocation Marked()==false` arm proves the redrive clear takes; omitting the clear leaves a stale skip across the topology change (the RED).
  - **R3 TEARDOWN, not empty-in-place:** the clear does `delete(map, key)` (drops the entry), NOT re-assign an empty set — so the engine map cannot accumulate one pinned entry per scope key across unrelated boots. Since an engine-held field does not get off-boot nil-ness for free, nil-off-boot is EARNED: the set is installed onto ctx ONLY in the `s.kind == scopeKindBoot` branch of `processScope`. A grep arm asserts the sole `WithSeedDeclinedExternalSet` install site is that boot-gated branch (never a /call or keepwarm path), and a runtime arm asserts a non-boot (gvr-discovered) scope's handler ctx carries no set.
  - **R4 whole-boot cross-pass counter:** the `phase1.seed.declined_external.summary` line is emitted ONCE at teardown (inside `clearDeclinedExternalSet`, reading `Marks()` off the engine-lived set before delete), so it reports the CUMULATIVE count across all of the boot's resume passes — NOT a per-`seedScopeYielding`-pass partial (the per-pass emit was removed).
- **Effect on symptom:** removes ~210s (`search-results`) + the other 8 whales' repeated external cost from every resume pass → resume passes drop from ~80s seed to near-0 seed. The requeue loop stops thrashing on external widgets.
- **LOC:** ~35 (a `sync.Map`-backed `boot-scope declined set` type mirroring `SeedResolveMemo` + context accessor + 3 call-site hooks). Reuses the existing context-scoped-sink pattern verbatim.
- **Toggle/inert:** installed only under the boot-scope context (never /call, never keepwarm, never cache-off) — a strict no-op everywhere else, same as the memo (C-F4-8 pattern).
- **Self-retiring, no #129 conflict (C-F4B-5):** the marker is set ONLY in `declineSeedPutOnError`'s external-touch branch (never the stage-error branch — a transient stage error should retry on resume). It is therefore fully coupled to the decline firing: if #129 later gives external widgets a warmable, invalidatable TTL cell, the seed's Put stops being declined for those widgets → the external branch stops firing → the marker is never set → Lever A silently self-retires with zero code change and no conflict with the #129 cache.

### Lever B — skip the discovery re-walk on a boot RESUME chunk (removes ~255s/pass). SECONDARY.

**Root cause it targets:** `rePrewarmBootScoped` re-runs the full discovery walk (`rewalk_complete elapsed_ms:253–261s`) on every resume attempt, even though the walk output (the harvested nav-widget set + RA set) is **identical** to the first pass — the resume only needs to continue seeding the *tail the deadline cut*, not re-discover targets. The rewalk is 255s of the resume pass's wall-clock and produces the same 191 widgets / 34 restactions every time (`rewalk_complete widgets:191 restactions:34` identical across passes).

**Design:** the boot scope should harvest the walk ONCE (first attempt) and **reuse the harvested target snapshot on resume attempts** rather than re-walking. The harvester (`navWidgetHarvester`) already dedupes and snapshots (`snapshot()`); the resume path should feed the prior snapshot into the seed loop instead of re-driving `walk()`. Gate on `attempt > 0` (resume) — a first attempt still walks. This is squarely a `feedback_no_special_cases`-clean data reuse (the snapshot is the walk's own output, not a hardcoded target list).

- **Placement:** `rePrewarmBootScoped` (`prewarm_engine_boot.go:228`) — cache the harvested snapshot on the boot-scope engine state after the first successful walk; on a requeued attempt, skip the `walk()` call and seed from the cached snapshot.
- **Effect on symptom:** removes ~255s from every resume pass. Combined with Lever A (resume seed → ~0), a resume pass goes from ~340s to well under a minute, so the boot scope converges instead of thrashing.
- **LOC:** ~40–60 (snapshot cache field + reuse branch + invalidation on a genuine config-vars redrive so a real topology change still re-walks).
- **Risk/caveat:** must invalidate the cached snapshot when the config-vars watcher signals a real INIT/nav change (else a resume after a topology change seeds a stale target set). The config-vars redrive path (`phase1_configvars_watch.go`) is the natural invalidation hook. **This is the load-bearing correctness condition for Lever B** and is why it's secondary — it needs a soundness pass on the invalidation seam. If that seam proves fiddly, Lever A alone already stops the thrash (the rewalk only hurts because the resume keeps firing; kill the resume-firing driver and the rewalk cost amortizes away).

### Recommended sequencing

**Ship Lever A first (self-contained, ~35 LOC, breaks the loop that drives the requeue thrash).** Measure. If the boot scope still can't fit pass 1 into 480s after A (pass 1 is a genuine 460s of first-time work — A doesn't help pass 1, only resumes), then Lever B removes the resume rewalk so the requeue continuations are cheap enough that the F.4 cost-proportional resume actually converges the tail quickly. A + B together make the *aggregate* boot converge; neither shrinks the irreducible pass-1 460s, which is addressed by the §6 note on per-cohort baseline (a larger, separate lever surfaced to Diego).

## 5. Falsifiers + PM-gateable acceptance

Each ties to a runtime artifact, not a unit test.

1. **Lever A primary (symptom):** re-boot the fixed image at 50K; grep `phase1.seed.widget.timing` bucketed by `boot.complete` interval. **The 9 external whales (`search-results` et al.) must appear on pass 1 only, NOT on resume passes 2+.** Pre-fix they appear on all passes (`cnt=5`). Corollary: `phase1.seed.skip.external_touch` decline count for those targets drops from ~37 to ~6 (one per cohort, pass 1 only).
2. **Lever A RED arm (hermetic, gate-blocking):** a boot-scope seed pass that resolves→declines an external widget, then a second (resume) pass over the same target set: assert the second pass does NOT re-invoke `widgets.Resolve` for that widget (skip fires). RED arm = remove the declined-set consult → the second pass re-resolves → test fails. This is the discriminating arm (proves the marker, not just a skip).
3. **Lever A safety:** the declined-set marker is nil on a /call context and on a keepwarm context (grep: no marker install outside `withCohortSeedContext`; the /call path re-resolves the external widget fresh, unaffected). Mirrors the memo's C-F4-8.
4. **Lever B (symptom):** `rewalk_complete` fires ONCE per boot scope, not once per resume attempt. Pre-fix: 6+ rewalk lines per boot; post-fix: 1 (first attempt) + re-walks only on a genuine config-vars redrive.
5. **Lever B safety (load-bearing):** after a simulated config-vars INIT change during a boot resume, the next seed pass uses the NEW target set (re-walks). RED arm = a stale-snapshot reuse serves the old nav set → test fails.
6. **Aggregate milestone:** `boot.complete` for the boot scope converges (no `scope_requeued attempt≥2` churn over 10 min); `first_nav.latch` still fires via `segment-complete` (not the F5 backstop); Ready via segment-complete. Per `project_current_state` the 50K canonical bar is warm 911 / cold 2053 / conv ~24s — the seed convergence must not regress those.

## 6. Rejected alternatives

- **Extend the F4 memo to widget-data resolves (brief lever a).** REJECTED — misdiagnosis. The memo already collapses the heavy shared-RA jq (`memo_hits:492` on pass 1). The residual mass (§2) is the **per-cohort** baseline of the mid-bucket widgets; those resolves are *not* identical across cohorts (distinct RBAC identity → distinct memo key by C-F4-4), so a wider memo cannot collapse them without violating per-user keying (`feedback_l1_per_user_keyed_never_cohort`). Extending the memo to widget-data would add surface for near-zero gain.
- **Bump `PHASE1_TIMEOUT_SECONDS` / `pipGlobalTimeout` (brief lever c).** REJECTED per `feedback_no_shortcuts_or_workarounds` — a budget bump hides the §3 external-whale re-resolve loop (which would keep thrashing forever, just with a longer leash) and the §4B redundant rewalk. It treats the symptom (overshoot) not the cause (infinite re-resolve + repeated rewalk). Explicitly last-resort per the brief; not needed once A+B land.
- **Widen seed cohort concurrency (F4 doc Option 3).** REJECTED as primary (unchanged from F4 §Option 3) — parallelism after removing redundancy, not instead of it. It would let the external whales spin in parallel, wasting more CPU, not less.
- **Warm the external-whale cell anyway (drop the #102 decline for the seed).** REJECTED — a hard safety regression. An external-touch cell has no dep edge, so a warmed entry goes stale silently and serves stale observability data forever until TTL. The decline is correct; the fix is to stop *re-resolving* it, not to *warm* it.
- **Skip external widgets from the seed entirely (never resolve them).** REJECTED as first choice — pass 1 still wants to resolve each external widget once so its content is available for the /call cold path pin, and skipping them wholesale would also skip the first-paint. Lever A skips only the *redundant resume re-resolves*, which is the minimal correct cut. (If the tester later shows even the single pass-1 external resolve blows the budget, the #129 bounded-TTL-external-cache is the right home for those widgets — cross-referenced, not folded here.)

## 7. Strategic choice to surface (Diego / TL)

The **irreducible pass-1 460s** is a per-cohort-fan-out cost (§2), not something A or B shrinks. At 6 cohorts it fits *after* A stops the thrash (pass 1 latch was 376s, under 480s — the milestone actually latched fine; the requeue churn is what looked like failure). But **cohort count is data-derived and scales the pass-1 cost linearly** — the F4 doc's C-F4-1 correction already flagged this. If a customer topology has materially more than ~6 seed cohorts, pass 1 alone could exceed 480s and neither A nor B helps. The strategic lever for *that* case is reducing the per-cohort baseline (e.g. sharing the `allCompositions`-over-29-GVRs substrate read across cohorts, since the informer-served bytes are cohort-independent even though the RBAC filter is not — a "resolve substrate once, filter per cohort" split). That is a larger design and is **out of scope for F4b**; recommend booking it as a follow-up if the tester's on-cluster cohort count exceeds ~6, and shipping A (+B) now as the loop/rewalk fix.
