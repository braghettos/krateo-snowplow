# Snowplow Test-Coverage Audit — 2026-07-30

Feature × existing-coverage × gap inventory across all packages, produced to drive an extensive
test-suite build. Full per-feature matrix (131 rows) is in the workflow output
(`wwdxzny7r`); this doc carries the actionable parts: summary, prioritized gap list, and the
conflict-free suite-organization plan.

## Summary

- Features audited: **131**
- Covered — discriminating (RED/neuter arm, -race): **89**
- Covered — shallow (happy-path only): **26**
- Uncovered (no test): **16**
- Standing CI gates: 1 wired (`-race` suite in `release-pullrequest.yaml`); 1 **orphaned** (the
  `scripts/lint/*` AST gates — present but NOT invoked by any workflow).

**8 of the top-10 gaps are security** (RBAC / identity / cross-user-leak / forgery). The
dispatcher serve path and the identity/key-derivation seams carry the most under-tested
correctness-critical surface.

## Prioritized gap list

### HIGH
- **H1** [SEC cross-user] — RESTAction & Widget `ServeHTTP` end-to-end orchestration. No httptest
  drives the branch ordering (RBAC before L1 Get; serve-eligible before Get; Put only on genuine
  else-if; stage-error/external precedence). Arms a–k incl warm≡cache-off, RBAC-deny→Get-never-called,
  RBAC-sensitive-widget skips content cell, both Put-gates. RED: L1-serve-before-RBAC / reads shared
  content cell for RBAC-sensitive widget / Puts regardless of sink.Count().
- **H2** [SEC cross-user] — `resolveWidgetData` #69 fail-soft: read-error must return STATIC src, build
  NO cross-ns aggregate. RED: on read-error proceeds to build SA-maximal aggregate.
- **H3** [SEC stale-permit] — L2 authz-memo invalidation on **role-RULE edit** (not just binding-set).
  RED: memo keyed only on binding-set / rule Update fails to bump PublishSeq → serves stale allow.
- **H4** [SEC forgery/arming] — CORS `ExposedHeaders` wiring: `X-Snowplow-Refresh-Key`/`-Class` must be
  exposed cross-origin (the whole #48/#67 arming contract). RED: drop either header.
- **H5** [SEC under-grant] — `/rbac` handler non-auth contracts: 422 partial-read-set refusal
  (`formatUnresolved`, zero coverage), 400 missing param, `[]` not null. RED: partial read-set as 200.
- **H6** [SEC-adjacent forgery-single-source] — orphaned AST lint gates (`no_parallel_binding_derivation`,
  `no_unchecked_unstructured_assert`) not in CI → a BindingUID derivation outside the match_subject
  allowlist would not fail. Wire into CI + regression fixture.

### MEDIUM
- **M1** [SEC/mem] — `Representative*` EXCLUDED from ComputeKey (per-binding equivalence unpinned).
- **M2** [SEC] — widgetContent OMITS BindingUID / other 4 classes fold it.
- **M3** [SEC identity] — `GetIdentityContext` D1 enum filter (drops displayName/typo/non-string).
- **M5** [SEC fail-closed] — `checkDispatchRBAC` direct table (fail-closed on UserInfo/eval error).
- **M6** [SEC stale-forever] — external-touched skip on RA full-list serve surface (#4).
- **M7** — `/readyz` C2 backstop via injected panicking/blocking/erroring `pipSeedFn`.
- **M8** — L2 authz-memo cap-breach (16384) REFUSE path (not LRU-evict; correct verdict on refused).
- **M9** [SEC-adjacent] — `canonicalGroupsHash` isolated unit (length-prefix anti-alias, no in-place sort).
- **M10** — `WidgetContentL1Enabled` gate truth table.
- **M11** — `SeededAtBoot` seed→traffic re-classification on re-Put.
- **M12** — SA transport capture at construction + out-of-cluster nil-guard (#18/#19).
- **M13** — `HashExtras` direct + parity with ComputeKey (single-derivation anti-drift).
- **M14** — Seed footprint budget PROD branch (ERROR+counter+proceed, no panic).
- **M15** — `hasTemplatedEndpointRef` seed-skip (no truncated no-extras Put).
- **M16** — X-Snowplow-Refresh-Class stamped-class AST scan (serve-site literal ∈ allowlist).
- **M17** [SEC auth] — RefreshAuth cookie-name override + **no-token-in-URL** + expired→401.
- **M18** [SEC-adjacent DoS] — /refreshes URL-safe base64 (EventSource path) + 512-cap + informer-only skip.
- **M19** — delivery dep-edges list-scope path (RecordList `*`) + action-ref skip.
- **M20** — widgets.Resolve orchestration hermetic unit + rrt-extras fold timing (rrt key must not leak
  into widgetData jq).

### LOW
L1 inlineParentIdentityForKey partial-identity shapes [SEC]; L2 Extras marshal-failure fallback;
L3 resolvedKeyVersion golden anchor; L4 SeedDeclinedExternalSet cache-pkg direct; L5 cache=off SAR
permit branch [SEC]; L6 authn Token -race + clock-skew; L7 SA synthetic-groups probe→assertion;
L8 Stats widgetContent/raFullList evict-pressure; L9 /debug/refreshes structural aggregate-only guard
[SEC-adjacent]; L10 /debug/servable handler; L11 refresh coalesce-window disable; L12 F6 undeclared-extra
reaches resolve-INPUT-not-key; L13 UAF refilter -race + SA-endpoint fail-closed + multi-yield; L14
beginPerCall / emitDispatchCacheKeyDiag diagnostics.

## Suite organization (conflict-free; one coherent new file per concern, by owning package)

**internal/handlers/dispatchers/**: `servehttp_orchestration_test.go` (H1), `check_dispatch_rbac_test.go`
(M5), `sa_transport_capture_test.go` (M12), `phase1_readyz_backstop_test.go` (M7),
`refresh_class_stamp_ast_test.go` (M16), `inline_parent_identity_key_test.go` (L1),
`f6_resolve_input_reach_test.go` (L12), + extend templated-endpointref (M15).

**internal/cache/**: `compute_key_identity_invariants_test.go` (M1+M2), `hash_extras_test.go` (M13+L2),
`gates_widget_content_test.go` (M10), `seeded_at_boot_test.go` (M11),
`seed_assert_prod_mode_test.go` (M14), `resolved_key_version_golden_test.go` (L3),
`seed_declined_external_set_test.go` (L4), stats extension (L8), refresh_broadcaster coalesce (L11).

**internal/resolvers/widgets/**: `resolve_orchestration_test.go` (M20+H2),
`identity_key_extras_accessors_test.go` (M3). **apiref/**: extend ra_full_list (M6).
**restactions/api/**: refilter concurrency (L13).

**internal/rbac/evaltest/**: extend snapshot_authz_memo (H3+M8), `groups_hash_test.go` (M9),
extend evaluate (L5+L7). **internal/authn/**: extend authn_test (L6).

**internal/handlers/ & main**: `main_cors_test.go` (H4 — extract cors.Options into a package var),
`rbac_handler_body_test.go` (H5), extend refreshes (M18), `debug_refreshes_structural_test.go` (L9),
`debug_servable_test.go` (L10). **middleware/**: extend refreshauth (M17).

**CI/lint**: wire `scripts/lint/*` into `release-pullrequest.yaml` + regression fixtures (H6).
**Extend existing** `deps_refresh_delivery_falsifier_test.go` (M19).

Every addition is a distinct new file or a named extension in its owning package — keeps the
`-p 1 -race` serialized run conflict-free.

## Build plan

Author in priority waves, each built + `go test -race` verified before the next:
- **Wave 1 (security/HIGH + security-MEDIUM):** dispatchers, cache, widgets, rbac/evaltest,
  handlers+main+middleware. Covers H1–H5, M1–M3, M5, M7, M10–M14, M16–M18, L1–L3, L8, L9, L12.
- **Wave 2 (remaining + CI):** apiref (M6), restactions/api (L13), authn (L6), CI lint wiring (H6),
  M6/M15/M19 extensions, L4–L7, L10, L11, L14.
