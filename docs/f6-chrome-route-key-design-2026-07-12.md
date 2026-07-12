# F6 — Chrome-widget route-context cache-key gap (#130 final gap-chain)

Date: 2026-07-12 · snowplow 1.7.9 · repo @ origin/main 26b48b3
Run-dir / artifacts:
- Boot+window log: `/tmp/f4-deploy/boot-1.7.9-milestone-t0.log`
- Tester milestone report: `/tmp/f4-deploy/c-r2-4-milestone-report.md`
- SPA source: `/Users/diegobraga/krateo/frontend-draganddrop/frontend` (current, not the stale `~/krateo/frontend`)

## TL;DR

The 8 chrome widgets miss on the first non-dashboard nav because the **frontend folds the
current route's params (`namespace`, `name`) into the `?extras=` of EVERY widget /call**, and
those params partition the per-cohort `widgets` cache key. On `/compositions/:namespace/:name`
the route params are non-empty → a route-specific key that the seed (extras-less) never warmed
and no prior nav touched. The rendered content of these chrome widgets is **byte-identical across
routes** (all three admin app-shell keys serve `resident_bytes:1538`), so the partitioning is
**SPURIOUS over-keying**, not a semantic requirement.

This is the **F-ARCH-1 seed-key-divergence class one level up**: the seed folds no request
extras (`extras_len:0`), the browser folds one (`extras_len:1`) → the seeded cell is not
browser-reachable, exactly the `TestFARCH1_RED_OldFrontendRequest_Misses` shape but with route
params instead of identity.

Recommended fix = **author-declared extras-dependence** (`spec.keyExtras`), a byte-for-byte mirror
of the existing A2 `spec.identityContext` contract: a widget only folds the request extras it
actually consumes. Chrome/layout widgets declare none → their key stops partitioning by route →
one seeded cell serves all routes. No static route list, no name-table, no cross-user sharing
change, per-user/cohort keying untouched.

## 1. Mechanism (TRACED)

### 1.1 Where route context enters the key

- Frontend: `useWidgetQuery.ts:126` reads `useParams()` (React-Router route params);
  `useWidgetQuery.ts:135` passes them to `buildExtrasParam`; `buildExtrasParam`
  (`useWidgetQuery.ts:84-101`) merges **all** route params into the extras JSON
  (`useWidgetQuery.ts:92-94`) and serializes to `?extras=<json>`. This is unconditional for every
  widget the SPA fetches — there is no per-widget filter for which params a widget consumes.
- snowplow ingest: `util.ParseExtras` (`internal/handlers/util/extras.go:13-27`) JSON-decodes
  `?extras=` into `map[string]any` (the request extras).
- Key fold: `widgets.go:152` `keyExtras := effectiveKeyExtras(req.Context(), got.Unstructured.Object, extras)`.
  `effectiveKeyExtras` (`helpers.go:368-399`) unions the request extras into the key material via
  `unionForKey` (`helpers.go:322-338`) — request wins. That `keyExtras` feeds BOTH:
  - the per-cohort `widgets` key: `dispatchCacheLookupKey` → `ComputeKey` (`helpers.go:228-270`,
    `Extras: extras` at :268), AND
  - the identity-free `widgetContent` key: `dispatchWidgetContentKey` (`helpers.go:175-195`,
    `Extras: extras` at :193).

So the route params fold into the ComputeKey digest for both layers.

### 1.2 Same widget, differing key inputs, dashboard vs compositions (TRACED, from the log)

`app-shell` (Layout), admin/[admins], per-cohort `widgets` layer — three distinct
`dispatch.cache_key.computed` `key_hash` values, all `extras_len:1`, all same user/groups/gvr/pagination:

| key_hash (12) | route (by timestamp) | hit? | resident_bytes |
|---|---|---|---|
| `75adfc21…` | dashboard (11:57, 12:03:49) | hit@2nd | 1538 |
| `cdad05b9…` | compositions-A (11:57:55, 12:03:58, 12:04:14) | hit | 1538 |
| `008d0cbe…` | compositions-B (12:04:07, 12:04:24) | **MISS**@12:04:07 (first-nav) | 1538 |

The seed computed `app-shell` only at `extras_len:0` (`handler_kind:widgets`, keys
`0f3c6032…/221b882b…/…` per cohort) — none of which equals any browser key (browser is
`extras_len:1`). The miss at 12:04:07.015 is `l1:miss` at the `widgets` handler, followed
114ms later by a `widgetContent` HIT (`8a7a3acc…`, `l1:content-hit`) — i.e. the SPA fetches the
chrome widget both ways; only the per-cohort `widgets` fetch (route-keyed) misses.

### 1.3 The base key vs dashboard key vs widgetContent key (answers brief Q3)

Three distinct cells exist for one chrome widget:
- **Per-cohort `widgets` cells** — keyed by BindingUID + extras. The browser's carry
  `extras_len:1` (route params). The seed's carry `extras_len:0`. **They never match** → the
  seed's `widgets` cell is dead weight for chrome widgets (the "base key" the tester saw seeded).
- **`widgetContent` cell** (`8a7a3acc…`, `resident_bytes:1564`) — identity-free, single stable
  cell, HITs on every nav. **NOT seeded by boot** (seed emits no widgetContent for app-shell); it
  self-warms on the first dashboard traffic and then serves all routes. Its extras_len is
  effectively 0 on the wire path that the SPA uses for it (the tester's stable hit), which is why
  it does NOT partition by route.

So the "seed warmed a base key + dashboard key" observation = seed warmed the extras-less
per-cohort `widgets` cells (unreachable by the browser) and dashboard traffic warmed the
route-A `widgets` cell + the shared `widgetContent` cell. The compositions-route `widgets` cell
is the only cold one on first nav.

## 2. Required-vs-spurious verdict: **SPURIOUS** (EVIDENCE)

Falsifiable content check from the log: all three route-partitioned `widgets` keys for `app-shell`
serve `resident_bytes:1538` — identical size (a change in rendered content would change the
serialized byte length; app-shell/sidebar-nav/brand-logo render fixed chrome, they do not consume
`namespace`/`name` in their jq). The route context changes the KEY but not the resolved body.
This is over-keying: the cell is needlessly partitioned by inputs that do not affect resolution.

Caveat (bounds the fix): SOME widgets on a composition route genuinely DO consume `namespace`/`name`
(the composition-detail / resources widgets — that's what those params are FOR). For those the
route param is semantically required and MUST stay in the key. So a blanket "strip route extras"
is wrong; the fix must be **per-widget, author-declared**, matching the A2 identity precedent.

## 3. Prior art

- k8s/client-go: no applicable primitive — this is a cache-key-composition policy, not an
  informer/lister concern.
- **In-repo A2 contract IS the prior art**: `spec.identityContext`
  (`internal/resolvers/widgets/widgets.go:29-150`, `GetIdentityContext` + `DeclaredIdentity`) is
  an author-declared enum of which IDENTITY dimensions a widget's resolution depends on; only
  declared dimensions fold into the key (`effectiveKeyExtras` merges `declaredIdentityForKey`).
  The route-extras problem is the identical shape for REQUEST-EXTRAS dimensions. Reuse the pattern;
  do not invent a new mechanism.

## 4. Design — ranked options

### Option A (RECOMMENDED): author-declared `spec.keyExtras` allowlist (snowplow-side, mirrors A2)

Add a widget CR field `spec.keyExtras: []string` — the enumerated request-extras keys whose values
this widget's resolution depends on. `effectiveKeyExtras` folds ONLY the declared request-extras
keys into the key (identity injection and the two inline maps are unchanged). Undeclared keys are
still passed to the RESOLVE input (the jq dict) — they just don't PARTITION the cache. Absent
declaration = fold NO request extras (prod-inert for chrome widgets; the ~99% path).

- File:line target: `effectiveKeyExtras` (`internal/handlers/dispatchers/helpers.go:368-399`) —
  filter the raw `requestExtras` arg into `unionForKey` via
  `filterDeclaredKeyExtras(cr, requestExtras)`; add `GetKeyExtras(obj)` accessor in
  `internal/resolvers/widgets/widgets.go` next to `GetIdentityContext`. LOC bound: ~40
  (accessor ~20, filter+wiring ~15, one shared call site).
  NOTE (as-built, PM-caught doc correction): `effectiveKeyExtras` / `unionForKey` / the
  key-builders live in `internal/handlers/dispatchers/helpers.go` — NOT in a
  `resolvers/widgets/helpers.go` (there is none). The `GetIdentityContext`-style accessors
  (`GetKeyExtras`) live in `internal/resolvers/widgets/widgets.go`. Every `helpers.go` file
  reference in this doc means `internal/handlers/dispatchers/helpers.go`.
- Composition-detail / resources widgets declare `keyExtras: [namespace, name]` (portal-chart
  widget YAML) → their key still partitions correctly. Chrome widgets declare nothing → single cell.
- Anti-drift: the filter is applied in the SINGLE shared `effectiveKeyExtras`, so all four key
  consumers (dispatch, widgetContent, subscription, seed) fold identically — same guarantee A1
  gave. The subscription-arming path (`refresh_subscription.go:137`) and seed
  (`phase1_pip_seed.go:817`) both go through `effectiveKeyExtras`, so parity holds by construction.
- **DEFAULT/ROLLOUT CARE**: default-absent = fold-nothing is the RIGHT default for chrome, but it
  is a KEY CHANGE for widgets that TODAY rely on route params partitioning correctly WITHOUT
  declaring them. Those widgets would start sharing a cell across routes and serve stale/wrong
  content. Two sub-options:
  - **A-strict** (safer rollout): default = fold ALL request extras (today's behavior); a widget
    OPTS OUT of a key by NOT... no — inverted, unsafe. Reject.
  - **A-declare** (recommended): default = fold NOTHING; widgets that consume route params MUST
    declare `keyExtras`. Requires a one-time audit of the portal-chart widget corpus (which widgets
    read `.namespace`/`.name`/query params in their jq) and adding the declaration to those.
    This is a coordinated frontend-chart + snowplow change (like A6). The audit is bounded — grep
    the widget RA/widget YAML for `extras.namespace` / `extras.name` / query-param reads.

Falsifier:
- RED: a widget that DOES read `extras.namespace` in its jq, with NO `keyExtras` declaration, must
  produce byte-identical keys across two different namespaces AND (the bug arm) serve the wrong
  body — proving why the audit+declaration is mandatory. GREEN once it declares `keyExtras:[namespace]`.
- GREEN (the win): a chrome widget (no declaration) produces ONE key regardless of route params;
  seed key == browser key (extends `TestFARCH1_SeedParity_*` with a route-param-bearing serve
  request that now folds nothing). On-cluster: compositions first-nav app-shell = `l1:*hit` (seed
  or content), `dispatch.cache_key.computed extras_len:0` on the widgets layer for chrome widgets.

### Option B: seed enumerates route contexts (walk-derived, NO static list)

Keep the key as-is; make the seed replay the per-route extras for each nav-widget. The walk already
discovers routes via routesloaders/ROUTES_LOADER roots (`feedback_prewarm_follows_frontend`); the
seed would, for each discovered route, re-seed the chrome widgets with that route's params folded.

- Rejected as primary because: (1) it seeds SPURIOUS cells (byte-identical bodies under N keys) —
  wasted memory + wasted seed budget; (2) cost multiplies: N routes × 8 chrome × 7 cohorts. From
  the milestone, the single seed pass is 369.8s of 480s (≈110s headroom); even a handful of routes
  risks the F4b overshoot (#132). (3) It does not fix the STRUCTURE — a route the walk didn't see
  (deep-linked composition) still cold-misses. Option A makes the cell route-invariant so no route
  enumeration is needed.
- Keep B as the fallback ONLY for widgets that legitimately vary per route (the composition-detail
  class) — but those are already reached by normal nav and their route set is unbounded (any
  namespace/name), so seeding them per-route is infeasible anyway. So B is not a general answer.

### Option C: frontend stops sending route params to chrome widgets

Mirror the `injectIdentity` capability-flag pattern: the SPA folds route params into extras only
for widgets that consume them. Rejected as PRIMARY: the frontend cannot know which widgets consume
which params without the same author declaration Option A introduces server-side — so this is
Option A's data, enforced on the wrong side, and it leaves the seed/subscription parity to the
frontend. Option A centralizes the policy in the widget CR (single source of truth, both sides read
it). C could be a companion (skip sending undeclared extras to cut wire size) but is not required.

## 5. Recommendation

**Option A-declare.** It is the exact A2 pattern one dimension over, keeps the anti-drift single
site, honors every invariant (no static list, no name-table, per-user/cohort keying inviolable,
layers removable, prod-inert default for the identity-free corpus). It requires a coordinated
portal-chart widget audit + declaration for the route-consuming widgets — surface this to Diego as
the one strategic dependency (same shape as the A6 cross-repo landing).

Strategic choice for TL/Diego: **A-declare (default fold-nothing, audit route-consumers) vs. keep
the gap.** The gap is 83.3% vs 98.4% first-nav on non-dashboard routes; it is a real but bounded
UX miss (self-warms on first touch, P2=100%). If the audit is deemed too risky pre-1.0, the gap is
shippable as a known-corner and A-declare lands as a fast-follow.

## 6. Cost estimate

- Option A: ~40 LOC snowplow + N widget-YAML declarations (N = count of route-consuming widgets in
  the portal chart, expected small — composition-detail/resources family). Zero seed-budget impact
  (fewer cells, not more). Memory: chrome widgets collapse from up-to-N-route cells to 1.
- Option B (rejected): +(routes × 8 × 7) seed targets against a ~110s headroom — high F4b-overshoot
  risk.

## 7. Falsifiers (summary)

1. Unit (permanent, extends farch_seed_parity): chrome widget (no `keyExtras`) — seed key
   (`effectiveKeyExtras(ctx, cr, nil)`) == serve key with a route-param-bearing request
   (`effectiveKeyExtras(ctx, cr, {"namespace":"x","name":"y"})`); L1 HIT, zero resolver calls.
2. Unit RED (mandatory, proves the audit necessity): route-consuming widget with NO declaration —
   keys collide across namespaces AND wrong body served; GREEN after declaring `keyExtras:[namespace]`.
3. On-cluster (the milestone re-run): admin (or any cohort) FIRST nav to `/compositions` — the 8
   chrome widgets show `l1:*hit` (not `l1:miss`); `dispatch.cache_key.computed` for chrome widgets
   on the `widgets` layer shows `extras_len:0`; compositions first-nav hit-rate → ~100% (was 83.3%).
4. CI invariant (#67 generalization): the emit/sub/seed parity test asserts the declared-keyExtras
   filter is applied identically on all three paths (guards the anti-drift property).
