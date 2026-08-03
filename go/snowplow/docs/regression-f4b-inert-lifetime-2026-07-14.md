# Regression journal — #132 F4b Lever A first build (2dc46ae) was correctness-inert (caught at gate, never shipped)

Date: 2026-07-14
Scope: caught at the architect gate on a frozen-but-parked SHA; NEVER deployed. Ship artifact: fix/132-f4b-external-decline-marker @ acf7a7d (dual-gate GREEN).

## Summary

The first build of F4b Lever A (commit 2dc46ae) placed the "resolved-but-declined-external" marker set in a **pass-lived** context (newed inside `seedScopeYielding`), which is torn down when that pass returns. Because the §3 external-whale re-resolve loop is **cross-pass** (a boot resume is a fresh `seedScopeYielding` invocation) and each `(widget,cohort)` is visited exactly once per pass, `Marked()` could never return true in production — the whales re-resolved every resume pass exactly as pre-fix. The fix was mechanically well-formed (build green, unit arms green) but **correctness-inert**: it produced zero behavior change against the symptom it targeted.

## How found

Architect gate on 2dc46ae (design-soundness review, pre-merge). The arch traced the real requeue chain — `AddRateLimited` requeue → `processScope` (prewarm_engine.go) → `rePrewarmBoot` → `rePrewarmBootScoped` → `seedScopeYielding` → **new set** — and observed that each resume pass starts with a fresh empty set, so a whale marked-declined on pass 1 is unmarked on pass 2 and re-resolves. All individual building blocks PASSED (placement-as-memo-mirror, external-only marking, full-key discipline, no-special-case); the defect was purely the **lifetime scope** of the set.

## The falsifier gap (the actual defect class)

The first-build multi-cohort arm (`TestF4bLeverA_MultiCohort_ResumeSkipsDeclinedExternal`) hand-installed ONE long-lived set on `bootCtx` and reused it across simulated passes. It therefore tested the marker MECHANISM against a persistence that **production does not provide** — it never drove the real `seedScopeYielding`/`processScope` lifecycle that news the set per pass. A green-and-discriminating unit arm on a mechanism, exercised against test-only persistence the deployed path lacks, is the reachability/faithfulness trap of `feedback_falsifier_must_actually_run_under_gate_tag_env` one level up: the arm proved "if the set persists, the skip fires" but not "the set persists in prod across a resume."

## Root cause

Marker set OWNERSHIP was pass-scoped (per `seedScopeYielding` context) while the loop it targets is boot-scope-scoped (cross-pass, across requeues). The design doc's original "boot-scope-lived / per-context" wording was itself ambiguous and seeded the mistake (arch owns that ambiguity, corrected as-built in §4).

## Fix (87c3783 rework + acf7a7d R2/R3/R4)

Move the set OWNERSHIP to the `prewarmEngine` struct, keyed per boot-scope-key:
- `declinedExtSets map[string]*cache.SeedDeclinedExternalSet` + `declinedExternalSetFor(key)` (get-or-create, REUSE across the scope's `AddRateLimited` requeues) + `clearDeclinedExternalSet(key, reason)` (prewarm_engine.go).
- `processScope` installs the engine-held set onto `scopeCtx` in the `s.kind == scopeKindBoot` branch ONLY (nil off the boot path — earned, since a struct field doesn't get off-boot nil-ness for free), and TEARS DOWN (`delete`, not empty-in-place) the entry on genuine completion (`err==nil` → Forget) so the map can't accumulate across unrelated boots.
- `enqueueBootReDrive` (config-vars redrive = new topology) clears the set so whales re-resolve once under the new nav set.
- The whole-boot cross-pass summary (`phase1.seed.declined_external.summary`) is emitted ONCE at teardown, reading cumulative `Marks()` — not a per-pass partial.
- `seedScopeYielding` no longer news a set; consult (`seedSkipDecision`) + mark (`declineSeedPutOnError` external branch) read/write it off ctx via `cache.SeedDeclinedExternalSetFromContext` (unchanged).

## Falsifier re-gated (the discriminator)

`TestF4bLeverA_MultiCohort_ResumeSkipsDeclinedExternal` now drives TWO REAL `processScope` invocations sharing the engine-lived set: pass 1 re-resolves K=2 cohort whales + requeues; pass 2 (the real requeue, reused set) skips both → 0 re-resolves → boot converges. RED arm = new-the-set-per-pass (the 2dc46ae behavior) → pass 2 re-resolves → never converges. The arch independently ran all 11 arms and flipped the never-reuse RED: exactly the 6 persistence arms failed, the 3 mechanism arms stayed green.

## Second regression in the same cycle (freeze-discipline)

Between 2dc46ae and acf7a7d, an intermediate SHA (87c3783) was reported as frozen while its `_test.go` did not compile (a widened prod signature `clearDeclinedExternalSet(key)` → `(key, reason)` left one test call at the old arity). `go build` passed (it does not compile `_test.go`); `go test` failed → zero arms ran, so the "3× green" claim described an untested tree. The PM gated the broken SHA; the arch stashed the dirty tree and gated the committed blob → the two gaters disagreed. Both are freeze-discipline violations (`feedback_freeze_to_committed_ref_for_multiagent_gating`, and reporting results from a state never tested = `feedback_falsifier_must_actually_run_under_gate_tag_env`).

## Prevention

- A bounding/loop-breaking mechanism must have its STATE LIFETIME matched to the SCOPE of the event it bounds. If the event is cross-pass (survives a requeue), the state must survive the requeue too — pass-lived state is inert against a cross-pass loop. Prove the bounded event still happens after the fix at the REAL scope (`feedback_bounding_mechanism_discipline`).
- A multi-pass/resume falsifier MUST drive the REAL pass lifecycle (here `processScope` requeue), sharing the same state the prod path shares — never hand-install persistence the deployed path doesn't provide. RED arm = the actual prod-inert variant (new-per-pass), asserting it does NOT converge.
- Pre-freeze verification is `go test` (compiles `_test.go` AND runs), never `go build` alone. One SHA = one clean committed tree that `go test` passed AFTER the commit; touch one character → re-run. Never report results for a tree state you did not test, and never "the fix is in a later commit."

Links: [[feedback_bounding_mechanism_discipline]] [[feedback_falsifier_must_actually_run_under_gate_tag_env]] [[feedback_freeze_to_committed_ref_for_multiagent_gating]] [[feedback_one_sha_one_clean_tested_tree]] [[feedback_dev_review_with_architect_pm_before_commit]].

Dual-gate GREEN on acf7a7d: arch PASS (ran all 11 arms itself, flipped the never-reuse RED — 6 persistence arms failed / 3 mechanism arms green; teardown re-ruled leak-free + race-safe) + PM FINAL-ACCEPT (own RED re-derivations R1/R2/R3 in an isolated worktree at the frozen SHA).
