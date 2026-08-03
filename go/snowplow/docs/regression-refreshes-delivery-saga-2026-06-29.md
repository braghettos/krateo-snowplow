# Regression journal — `/refreshes` zero-delivery saga (4 cycles, #61 → #64)

Date: 2026-06-29. Final fix shipped **1.5.13** (snowplow `6a221d4`, chart `1.0.39`).
Sister rule: [[feedback_falsifier_shape_must_discriminate]], [[feedback_validate_content_not_just_status]].

## Symptom

Live-refresh SSE (`GET /refreshes`) delivered **zero** `event: refresh` frames
to a real browser subscription, despite:
- `published` (PublishRefresh fan-outs) climbing,
- `subscribers >= 1` (the connection armed successfully), and
- a guaranteed matching publish (the underlying resource was reconciling).

`/debug/refreshes` showed the exact tell: **`published > 0 && delivered == 0`**.

## How it was found

The **frontend's real armed-subscription smoke** on the enterprise-full
cluster — a browser EventSource arming the actual
`descriptions-composition-detail-metadata` widget and observing no refresh
frames. NOT caught by any hermetic test for **four** ship cycles. Every
hermetic golden was GREEN while production delivered nothing.

## Root cause (final, correct — #64 / 1.5.13)

**page/perPage fold divergence in the cache key.** The EMIT path (`/call`
dispatcher) runs page/perPage through `paginationInfo` (helpers.go), whose
non-paginated default is **-1, -1** → `ComputeKey` folds `"-1","-1"`. The
SUBSCRIPTION path (`DeriveSubscriptionKey`) folded the **raw coords 0, 0** —
the value the frontend `?sub=` contract sends (howto doc) / the json-absent
zero. `"-1" != "0"` → the SHA-256 `ComputeKey` digest diverges on the
page/perPage bytes → the armed key never matches the published key → zero
delivery. **Class-independent** (the fold is identical for every class) and
**extras-orthogonal**.

## The 3 prior misses — all dual-gate-green, all failed in production

1. **#61 / 1.5.8 — dep-edge (necessary, not sufficient).** Made the
   refresher dirty-mark the armed top-level key so `PublishRefresh` would
   FIRE for it (the dep edge read `status.resourcesRefs` path-decoded, not
   just `spec`). Correct and required — but the publish then fired against a
   key the subscription couldn't match, so delivery stayed 0. A necessary
   precondition mistaken for the whole fix.
2. **#64-inline-extras / 1.5.11 — WRONG root cause.** Hypothesized the
   subscription key omitted the inline-extras union (`spec.apiRef.extras` ∪
   `spec.resourcesRefsTemplateExtras`) the emit key folds, and reconstructed
   it server-side via `objects.Get`. But the REAL
   `descriptions-composition-detail-metadata` widget has **NO inline extras**,
   so the inline-union fix was a **no-op for the actual failing widget**. The
   golden passed because it CONSTRUCTED a widget WITH inline extras.
3. (The TTL-evict residual #62 / publish-on-cold-Put was a real adjacent
   hardening, not a miss — it shipped correctly but addressed a different
   corner.)

## MASKING meta-pattern (the key lesson)

Every golden across all four cycles used **CONSTRUCTED / synthetic inputs with
MATCHING values on both sides** → the test passed while the REAL widget
diverged on a field the test never exercised. Same failure class **four
times**:
- **#46** — degenerate K=1/M=1 harness masked the per-cohort-vs-per-unit
  granularity defect.
- **#61** — flat-field fixture masked the `status.resourcesRefs` path-decode
  requirement (the items are `ResourceRefResult{path,...}`, no inline gvr).
- **#64-inline** — a constructed widget WITH inline extras masked that the
  real widget had none.
- **#64-pagination** — goldens hand-set MATCHING page/perPage tuples on both
  sides, never the real **-1-vs-0 default gap**.

The unifying defect: a falsifier whose inputs are author-constructed to match
on both sides cannot discriminate a field that only diverges under the REAL
production shape. See [[feedback_falsifier_shape_must_discriminate]].

## Fix

**EXTRACT** `paginationInfo`'s normalization into a pure shared
`normalizePagination(perPage, page) (int, int)` (helpers.go) that BOTH
`paginationInfo` (emit) AND `DeriveSubscriptionKey` (sub, applied to coords for
ALL classes before the per-class switch) call. The bug was two hand-written
normalization defaults **drifting**; a single shared core cannot drift. Rules
mirrored exactly, including `perPage > 0 && page <= 0 → page = 1`.

## Prevention (durable)

(a) **Pre-hash input-string diff method.** For a hashed key, diff the
    **pre-hash INPUT field-by-field**, NOT the digest. The digest tells you
    "different" one bit at a time; the field diff surfaces **all** divergent
    fields **at once** — vs the one-field-at-a-time chase (extras, then
    pagination) that cost four cycles. The arch's pre-hash field-by-field diff
    is what finally isolated page/perPage as the sole divergence. Now a
    permanent hermetic guard: `TestFalsifier64Pagination_PreHashInputEquality`
    asserts every `ComputeKey`-folded field of the EMIT `ResolvedKeyInputs`
    equals the SUB inputs (BindingUID-independent — both sides derive the same
    test identity).
(b) **Goldens use the REAL emit-default path + REAL widget shape.** Drive the
    actual `paginationInfo` (no hand-passed tuple) and fetch the actual CR
    (no constructed widget). NEVER hand-set matching values on both sides —
    that is exactly the masking that recurred four times.
(c) **Ground-truth confirmation before declaring fixed.** The frontend's real
    `X-Snowplow-Refresh-Key` (here `2b924e9…`) is the acceptance gate; the
    absolute digest needs the on-cluster admin BindingUID, so it is verified by
    the redeploy + frontend re-smoke (`delivered > 0`), not a hermetic claim
    alone.
(d) **The pre-hash-equality arm is a permanent hermetic guard** against any
    future emit/subscription key-derivation drift on any folded field.

## Shipping note

Releases **1.5.5 through 1.5.12 all shipped with `/refreshes` delivery broken**
(the feature was non-functional for inline-extras-free composition-detail
widgets the whole time). The seam fast-follow (consolidate the emit + sub key
derivation behind one entry point so future fields cannot diverge) is tracked
as a separate task (#66-adjacent). 1.5.13 is the first release with working
delivery, pending the on-cluster `delivered > 0` confirmation.
