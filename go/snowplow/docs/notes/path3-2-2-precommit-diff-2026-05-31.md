# Path 3.2.2 — Pre-commit Diff (0.30.220)

Branch: `ship-0.30.220-path3-2-2-walker-pagination`
Base: `7fc2b61 docs(path3-2-1): attach final precommit + bench probe results`

## Files changed

```
 M internal/handlers/dispatchers/phase1_walk.go             (+12 LOC, 1 call site)
?? internal/handlers/dispatchers/phase1_walk_pagination.go  (NEW, 312 LOC ~75% comments)
?? internal/handlers/dispatchers/phase1_walk_pagination_test.go (NEW, 178 LOC)
?? docs/path3-2-2-design-2026-05-31.md
?? docs/path3-2-2-phase0-baselines-2026-05-31.md
?? docs/path3-2-2-precommit-diff-2026-05-31.md (this file)
```

## Architectural intent

Dispatched (B)+(C) HALTED mid-execution per Diego 2026-05-31
"walking INIT is how prewarm works" + `feedback_prewarm_follows_frontend`.
Pivot to walker extension.

**Mechanism**: when the F2 INIT walker resolves an apiRef+
resourcesRefsTemplate-driven widget AND the resolver signals
`status.resourcesRefs.slice.continue == true`, the walker iterates
pages 2..N until the resolver flips `.continue` to false OR a safety
cap (`phase1MaxApiRefPages = 500`) is hit. Each additional page Puts
its envelope into the identity-free `widgetContent` L1 cell and
recurses into the new per-page child refs — exactly the page-1 path,
extended.

**Data-driven (no hardcoded GVRs)**:
- Predicate `isApiRefTemplateDriven` keys on the widget's SHAPE
  (`spec.apiRef.name != ""` AND `len(spec.resourcesRefsTemplate) > 0`)
  — same shape `widget_content.go` already uses for
  `isRBACSensitiveApiRefWidget` and `shouldSkipEmptyWidgetShell`.
  No widget-name. No GVR literal.
- Per-page children discovered by parsing each child ref's
  `/call?` Path via the existing `util.ParseCallPathToObjectRef` —
  the same machinery the page-1 recursion already uses.
- Continuation signal read from the resolver's own
  `status.resourcesRefs.slice.continue` flag (resolve.go:124-134).

## No new env vars / flags (Diego mandate)

Pagination rides on the EXISTING `cache.PrewarmEnabled()` gate
(prewarm off ⇒ walker doesn't run at all). The `phase1MaxApiRefPages`
safety cap is a Go constant — raising it is a code edit + ship,
consistent with `project_single_cache_flag_direction`.

## Cost bounds

- **Total widgets paginated**: only apiRef+template widgets reachable
  from INIT roots (today: compositions-page-datagrid,
  blueprints-page-datagrid, etc.).
- **Per-widget pages**: capped at 500 ⇒ at perPage=5, materialises
  up to 2,500 rows per apiRef widget. For ~50K compositions this
  covers ~5% on first ship. We instrument refresher amplification +
  raise the cap in a follow-up if Phase 3 falsifier shows headroom.
- **Refresher amplification** (0.30.185 HARD REVERT lesson): each
  populated entry adds to refresher load. Phase 3 bench probe must
  verify steady-state refresher queue depth + completed rate vs
  0.30.219 baseline.

## Falsifier targets (Phase 3 bench probe — 30 min steady state)

- `widgets|buttons widget-content-hit / (hit + miss-per-user-fallback)` >= 0.95
- `widgets|markdowns widget-content-hit / (hit + miss-per-user-fallback)` >= 0.95
- `widget-content-miss-per-user-fallback buttons` delta < 50 over 30 min
- snowplow pod restarts = 0
- p50 portal cyberjoker `/dashboard` <= 1.5× 0.30.219 baseline
  (no Phase B-style 5.9× wall-clock regression)
- refresher queue depth steady-state < 2× 0.30.219 baseline (5K-ish)

## HARD REVERT plan

- helm rollback snowplow to rev 378 (0.30.219).
- Update regression journal with empirical signal that triggered revert.

## Unit test pass

```
$ go test -race ./internal/handlers/dispatchers/...
ok  	github.com/krateoplatformops/snowplow/internal/handlers/dispatchers	8.227s
```

The pre-existing `TestMapVerbs` failure in
`internal/resolvers/widgets/resourcesrefs/...` was confirmed UNRELATED
(reproduces on base ref `7fc2b61` before any 3.2.2 edits — patch verb
mapping issue, pre-dates this branch).

## Three-way ACK requested

- **Architect** — confirm walker extension is the correct mechanism
  per `feedback_prewarm_follows_frontend`. Confirm the data-driven
  shape predicate is sound (no hardcoded GVRs in the path I cited).
- **PM** — confirm falsifier acceptance criteria are sufficient and
  the cap=500 first-ship is acceptable given 5% coverage of 50K.

Phase 3 bench probe will fill in the empirical numbers (post-deploy
30-min window) before Phase 4 tag + production deploy.
