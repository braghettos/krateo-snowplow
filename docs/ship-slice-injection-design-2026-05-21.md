# Ship H7 / 0.30.16x — `.slice` injection into widget data source

**Status:** DESIGN-READY for dev. ARCHITECT-AUTHORED 2026-05-21,
pre-PM-gate. **10 ACs**, **6 HARD GATES**, **3 OQs**. Tag number
left blank: dev picks `0.30.16x` (likely `0.30.161` per sequential
numbering) at commit time per `feedback_tag_commits.md` — the tag
MUST point to the meaningful "slice-injection / zero-cold dashboard"
commit, not a downstream merge.

Ship-letter "H7" continues the H-series (no other H ships yet
queued); team-lead may substitute "Ship I" if preferred. The
mechanism — not the letter — is load-bearing for this row.

**Lineage.** This design is the direct implementation of
Recommendation A from today's RCA at
`docs/dashboard-compositions-panel-row-table-rca-2026-05-21.md`
(RCA #299, §5.A, expected delta −5.0 to −5.5s). The RCA was
dispatched in-turn by team-lead at 12:50 CEST 2026-05-21 in response
to the 0.30.160 paged-table cold of 7.4s after Ship G shipped and
HG-1 came in red. The §5.A recommendation was ranked HIGHEST expected
delta of FIVE candidates; this ship implements it in isolation.

**One-liner.** When the widget resolver calls
`resolveWidgetData(ctx, opts.In, ds)` at `internal/resolvers/widgets/resolve.go:47`,
inject the existing pagination triple into `ds["slice"]` IFF
`opts.PerPage > 0 && opts.Page > 0`. The Table widget's
`widgetDataTemplate[0].expression` ALREADY references
`.slice.page * .slice.perPage`; today that reference evaluates to
`null * null` and the jq falls into the `else $sorted end` branch,
returning all 48,999 rows. After this ship the `if .slice then …`
branch fires and the widget trims to 50 rows at the resolver layer.

This is the actual **zero-cold ship** per Diego's prompt of 2026-05-21
following the RCA closeout. Ship G shipped the cache-layer plumbing;
this ship eliminates the cache-eviction-pressure root cause by
shrinking the cached envelope from 60 MiB to ~40 KiB. After this
ship: Ship G entries survive their TTL, D.5 becomes structurally
dormant on the dashboard path (no 48,999-row jq), and the projected
admin Dashboard cold drops to under 1s = north-star achieved per
`project_north_star.md`.

---

## §1. Header status line & RCA lineage

| Field | Value |
|---|---|
| Ship name | Ship H7 (placeholder; team-lead may rename) |
| Tag | dev picks at commit; expected `0.30.161` |
| Source RCA | `docs/dashboard-compositions-panel-row-table-rca-2026-05-21.md` |
| Recommendation implemented | RCA §5 Recommendation A — "inject `.slice` into ds" |
| Expected delta (per RCA §5 ranking) | −5.0 to −5.5s admin Dashboard cold |
| Effort | LOW — single-file widget resolver change |
| Risk | LOW — additive injection; widgets not referencing `.slice` are byte-identical |
| Predecessor ships | Ship G (`0.30.160`, identity-free widget content layer); Ship D.5 (`0.30.152`, cluster-list-when-allowed); Ship D.4.1 / D.4 (`0.30.151`); Ship F1/F2 (`0.30.119/0.30.120+`) |
| North-star anchor | `project_north_star.md` — 1s cold / 500ms warm / 1s fresh at 50K |

Mechanism is structural fix, not feature toggle — no new env flag.
Reverts cleanly by removing the four-line injection if anomalies
surface. Cache toggle invariant is preserved
(`CACHE_ENABLED=false` reverts to ~10s admin via 0.30.6 path
unchanged; see HG-5).

---

## §2. Mechanism — TRACED

### §2.0 Prior-art check (per `feedback_check_k8s_clientgo_prior_art`)

client-go ships shared informers, indexers, and apiserver-side
pagination (`?limit=` / `?continue=`). It does NOT ship any jq
variable-injection primitive, any widget-resolver, or any jq data-
source assembly. This ship is a snowplow-internal jq evaluation-
context injection: `widgets.Resolve` constructs a `map[string]any`
that becomes `Data` to `jqutil.Eval`. There is no client-go primitive
to lean on; the prior-art check is acknowledged and confirmed
inapplicable. Verified.

### §2.1 The defect — where `ds` is constructed and handed to widgetDataTemplate

**File:** `internal/resolvers/widgets/resolve.go:37-52`

TRACED — current code:

```go
func Resolve(ctx context.Context, opts ResolveOptions) (*Widget, error) {
    log := xcontext.Logger(ctx).With(loggerAttr(opts.In.Object))

    ds, err := resolveApiRef(ctx, opts)            // line 40
    if err != nil { … }

    widgetData, err := resolveWidgetData(ctx, opts.In, ds)   // line 47 ← THE INJECTION SITE
    if err != nil { … }
    …
}
```

`ds` is produced by `resolveApiRef` at line 40, which delegates to
`apiref.Resolve` (`internal/resolvers/widgets/apiref/resolve.go:22-51`).
The full chain that produces `ds`:

1. `widgets/resolve.go:40` → `resolveApiRef(ctx, opts)` →
2. `widgets/apiref/resolve.go:46-48` → `restactions.Resolve(ctx, raopts)` with `PerPage` and `Page` →
3. `restactions/restactions.go:36-49` → `api.Resolve(ctx, …)` returning `dict map[string]any` →
4. `restactions/api/resolve.go:205-216` — `dict["slice"]` IS set here when `opts.PerPage>0 && opts.Page>0`:

```go
dict := map[string]any{}
if opts.Extras != nil {
    dict = maps.DeepCopyJSON(opts.Extras)
}

if opts.PerPage > 0 && opts.Page > 0 {
    dict["slice"] = map[string]any{
        "page":    opts.Page,
        "perPage": opts.PerPage,
        "offset":  (opts.Page - 1) * opts.PerPage,
    }
}
```

5. After all api stages run, `restactions/restactions.go:58-75` —
   the RESTAction's `spec.Filter` is applied to `dict`:

```go
if opts.In.Spec.Filter != nil {
    q := ptr.Deref(opts.In.Spec.Filter, "")
    s, err := jqutil.Eval(context.TODO(), jqutil.EvalOptions{
        Query: q, Data: dict, …
    })
    raw = []byte(s)
} else {
    raw, _ = json.Marshal(dict)   // slice preserved on this branch
}
opts.In.Status = &runtime.RawExtension{Raw: raw}
```

6. `widgets/apiref/resolve.go:50` → `rawExtensionToMap(ra.Status)` —
   unmarshal back to `map[string]any` → returned as `ds`.

**The bug:** the `compositions-list` RA's filter is
`{ list: (.allCompositions // []) }` (TRACED — captured RA at
`/tmp/compositions_list_ra.yaml`, also cited in RCA §1). This filter
produces a shape `{list: [...]}` — `dict["slice"]` is **DROPPED** by
the filter projection. Therefore `ds` arriving at line 47 is
`{list: [48,999 rows]}` with NO `slice` field. The widget's jq
references `.slice.page` and `.slice.perPage`, both evaluate to
`null`, the conditional `if .slice then …` is false, and the
`else $sorted end` branch returns all 48,999 rows.

**Diff sketch — minimal-invasive fix at line 47:**

```go
// CURRENT (line 47):
widgetData, err := resolveWidgetData(ctx, opts.In, ds)

// PROPOSED — inject pagination triple uniformly when client requested it:
if opts.PerPage > 0 && opts.Page > 0 {
    // Re-inject the same triple that api.Resolve built at resolve.go:211-215.
    // Identical shape; the RA filter (e.g. `{list: (.allCompositions // [])}`)
    // may have dropped it from `ds`. Mechanism-uniform — no widget-name
    // special-case, no enum, no flag (per feedback_no_special_cases).
    if _, present := ds["slice"]; !present {
        ds["slice"] = map[string]any{
            "page":    opts.Page,
            "perPage": opts.PerPage,
            "offset":  (opts.Page - 1) * opts.PerPage,
        }
    }
}
widgetData, err := resolveWidgetData(ctx, opts.In, ds)
```

Rationale for `if _, present := ds["slice"]; !present`: if the RA's
filter explicitly emits `.slice` (preserving it through a pass-
through filter, or actively re-emitting), do NOT overwrite — let
the RA's authored shape win. This is the conservative default.
See OQ-1 for the alternative (always-overwrite) policy.

### §2.2 The jq template — empirical verification

TRACED — `/tmp/table_widget.yaml`, captured 2026-05-21 11:48 CEST
from live pod `snowplow-6dbc65bf85-swhb8` (image 0.30.160):

```yaml
  widgetDataTemplate:
  - expression: |
      ${
        (
          ((.list | sort_by(.ts // "") | reverse)) as $sorted
          | (if .slice then $sorted[0 : (.slice.page * .slice.perPage)] else $sorted end)
          | map([
              {
                valueKey: "key",
                kind: "jsonSchemaType",
                type: "string",
                …
```

Verified — the widget jq **explicitly references** `.slice`,
`.slice.page`, `.slice.perPage`. The conditional shape proves the
widget author EXPECTED `.slice` to be present and authored a clean
fall-through for the absence case. The fall-through is what we are
firing today (and is the source of the 48,999-row pathology). The
RCA's claim at RCA §5.A is empirically validated.

### §2.3 Effect chain — propagation after the fix

TRACED hops, each cited:

1. **Injection site:** `widgets/resolve.go:47` — `ds["slice"] = {page, perPage, offset}`.
2. **widgetDataTemplate evaluation:** `widgets/widgetdatatemplate/resolve.go:48-50` — `jqutil.Eval` runs with `Data: opts.DataSource` which is the post-injection `ds`. The widget jq's `if .slice then $sorted[0 : (.slice.page * .slice.perPage)] else …` now evaluates `.slice` as truthy and slices to `page*perPage` rows (= 1×50 = 50 for the dashboard cold call).
3. **widgetData assembly:** `widgets/widgetdatatemplate/resolve.go:57-59` — the sliced 50-row array is set into `src["data"]` at `widgets/resolve.go:163`.
4. **CRD validation:** `widgets/resolve.go:95-98` — `crdschema.ValidateObjectStatus` is called with `opts.In.Object` whose `.status.widgetData.data` now contains 50 rows instead of 48,999. The kube-openapi `objectValidator.Validate` recursion shrinks ~1000×. (RCA §2.5 measured 1.5s on 48,999 rows; INFERRED ~1.5ms on 50 rows.)
5. **JSON marshal + SetIndent:** the Status assembly at `widgets/resolve.go:54-59` and the downstream `writeJSON`/`writeResolvedJSON` at `internal/handlers/dispatchers/widgets.go` encode 50 rows instead of 48,999 → body 37.7 MB → ~40 KiB. (INFERRED proportional — 770 B/row × 50 = 38.5 KiB, observed per-row size in RCA §3.1.)
6. **Cache Put:** `internal/handlers/dispatchers/widget_content.go` — Ship G's widget-content Put now stores a ~40 KiB envelope instead of 60 MiB. The 2 GiB cap holds ~50,000 such entries instead of ~30 — eviction pressure structurally vanishes.

### §2.4 Why this works structurally

The `slice` map is already being computed at `api/resolve.go:211-215`
on every paged request. The data is ALREADY available; it is simply
being lost between the api-layer assembly and the widget-data jq
evaluation because the RESTAction's intermediate filter projection
strips it. The fix re-introduces it at the widget layer, where the
widget jq expects it.

This is a one-line bug — the `slice` triple is identical in shape to
the one already at `api/resolve.go:211-215`; we are propagating a
piece of state through one extra hop. There is no NEW state. There
is no NEW code path. There is no NEW gate.

Per `feedback_no_special_cases` — the injection is mechanism-uniform:
"always inject `.slice` if computed (i.e. perPage>0 && page>0)". No
widget-name table, no GVR table, no allowlist. Any widget whose jq
references `.slice` benefits; any widget that doesn't is byte-
identical.

### §2.5 Interaction with Ship G — cache eviction pressure relief

TRACED — RCA §2.4:

```
widget_content_store_total=87
widget_content_evict_total=67
widget_content_evict_pressure=0.770   ← 77% of widget envelopes evicted before re-read
```

Average widget envelope on the dashboard table: ~60 MiB; cap 2 GiB
⇒ ~30-entry ceiling ⇒ steady-state eviction pressure 0.77.

After this ship: envelope ~40 KiB; cap 2 GiB ⇒ ~50,000-entry
ceiling. The dashboard's full widget set is ~10 distinct envelopes
(routes-loader, nav, dashboard panels, dashboard tables, …) ×
~10 paged variants ≈ 100 entries. Working set drops 4 orders of
magnitude below cap.

**Effect on Ship G:** Ship G's identity-free Put / per-user gate
pattern is correct by construction (verified in RCA §3.3) but
defeated by capacity. After this ship, Ship G entries SURVIVE their
TTL → cold navigations served from L1 → admin Dashboard from F2-
populated entries → north-star ≤1s cold becomes empirically
reachable. Ship G's effectiveness is unlocked AS A SIDE EFFECT of
this ship; no Ship G code is touched.

### §2.6 Interaction with Ship D.5 — dashboard path becomes structurally D.5-irrelevant

TRACED — RCA §3.2: D.5's cluster-list collapse is dormant on
`compositions-list` (no `clusterListWhenAllowed: true` opt-in). The
D.5-anchored work at scaling-inverted 48,999 rows was projected to
yield −0.3 to −0.5s of the 7.4s cold; AFTER this ship, the resolver
NEVER materialises 48,999 rows on the dashboard cold path (the
widget jq slices to 50 BEFORE the row count reaches validation /
marshal / cache).

D.5 remains valuable for FORWARD-LOOKING use cases (any RA whose
filter does NOT slice, or whose widget jq does not reference `.slice`)
and for non-dashboard RAs that fan out per-namespace. D.5 simply
becomes structurally irrelevant for THIS path. No D.5 code is
touched; D.5 simply stays correctly dormant on the dashboard RA
until/unless a portal-chart change opts the RA in (RCA §5
Recommendation B, queued).

---

## §3. Decision tree / fix shapes

This is a one-file fix. The decision tree has only TWO branches:

### Branch A — single injection at `widgets/resolve.go:47`, gated by `(perPage>0 && page>0)` AND `ds["slice"] not already present`

**Recommended.** Most conservative. Honours an RA author who
deliberately emits `.slice` via their filter; injects only when
absent. Code shape per §2.1 diff sketch.

ACs are written against this branch.

### Branch B — unconditional overwrite at `widgets/resolve.go:47`, gated only by `(perPage>0 && page>0)`

Same code without the `if _, present := ds["slice"]; !present` guard.
Always overwrites whatever the RA filter emitted under `.slice`.

Simpler. Removes one conditional. Loses the "RA author intent" honour
property. PM should choose Branch A or Branch B explicitly pre-dev.

Recommend **Branch A** absent evidence of an RA in production that
emits a meaningfully-different `.slice` from the
`{page, perPage, offset}` triple. OQ-2 surfaces any such RA before
landing.

### Empirical confirmation prerequisite

Per the RCA's verified `.slice` reference in the widget jq, no
further confirmation is required pre-design. However, OQ-2's grep
across portal RestActions for `.slice` references is the falsifier
that confirms NO widget in production was relying on the absence-
of-`.slice` semantic as a "no pagination" flag. If OQ-2 surfaces any
such pattern, reframe as a per-widget gate (still mechanism-uniform —
e.g. `if widget declares perPage in widgetData then inject` —
NOT a widget-name table).

---

## §4. Acceptance criteria

**AC-H7.1 — Injection site.** A `ds["slice"] = {page, perPage, offset}`
write exists at `internal/resolvers/widgets/resolve.go` between line
45 (return-from-resolveApiRef) and line 47 (call to
`resolveWidgetData`). Cited as TRACED on the dev branch diff.

**AC-H7.2 — Re-uses computed triple (no duplication).** The injected
triple is identical in shape and values to the one constructed at
`internal/resolvers/restactions/api/resolve.go:211-215`. Same
`page`, `perPage`, `offset = (page-1)*perPage`. NOT re-derived from
request query strings; uses `opts.PerPage`/`opts.Page` from the
widget resolver's options struct (which is sourced from the request
handler at `internal/handlers/dispatchers/widgets.go:155-157`).

**AC-H7.3 — Unit test confirming `slice` presence in `ds` keys.**
Add a test in
`internal/resolvers/widgets/resolve_test.go` (or
`widget_slice_injection_test.go` per project test-file conventions —
dev picks) that constructs a fake `apiref.Resolve`-returning
`ds = {list: []}`, calls `Resolve` with `PerPage=50, Page=1`, and
asserts `ds["slice"]` is set in the data passed to `resolveWidgetData`.
A simple approach: dependency-inject a shim that captures `ds` at
the widgetdatatemplate boundary.

**AC-H7.4 — Integration test: widget jq executed with the new `ds`
returns 50 rows on a 48,999-row fixture.** Add an integration test
that fixtures a 48,999-element `.list` and a widget jq identical to
the dashboard table's. Assert `len(result) == 50` post-resolve. This
test is the ground-truth falsifier — if it passes, the dashboard
cold-path effect is structurally guaranteed.

**AC-H7.5 — Byte-identical output for widgets not referencing `.slice`.**
Add a regression test: a widget whose widgetDataTemplate jq does NOT
reference `.slice`. Resolve with `PerPage=50, Page=1`. Assert the
output is byte-identical to a Resolve with `PerPage=0, Page=0` (or
to the pre-ship output). Confirms the injection is harmless to
widgets that ignore `.slice`.

**AC-H7.6 — No new env flag.** No `WIDGET_SLICE_INJECT_ENABLED`-style
gate. Mechanism is a structural fix; per
`feedback_no_park_broken_behind_flag` a confirmed correctness defect
must be fixed, not parked behind a flag. The fix is correctness
(widget jq author intent honoured) and performance (envelope drops
1000×); no flag is appropriate.

**AC-H7.7 — No new closed-enum FallthroughReason needed.** This is
not a cache-shape fix; no new fallthrough cell is exposed. (Confirm
via `grep -rn "FallthroughReason\|fallthrough_cells" internal/`
post-edit returns the existing 0.30.160 reason set unchanged.)

**AC-H7.8 — Pre-flight falsifier baseline + post-deploy validation**
per `feedback_falsifier_first_before_ship`. Pre-flight capture at
`/tmp/snowplow-runs/0.30.16x/before/`; post-deploy capture and diff
at `/tmp/snowplow-runs/0.30.16x/after/`. See §6.

**AC-H7.9 — Tag at commit time** per `feedback_tag_commits.md`. Dev
picks tag (expected `0.30.161`) pointing at the meaningful "slice
injection" commit. No generic-merge tagging.

**AC-H7.10 — Pre-commit dev review by architect + PM** per
`feedback_dev_review_with_architect_pm_before_commit.md`. Dev shares
diff with architect (design soundness — confirm Branch A vs B per
§3) and PM (AC + falsifier coverage) BEFORE commit/tag/push.

---

## §5. HARD GATES

Methodology: session-cold-warm-pod, identical to D.5 / Ship G. Three
runs at n=3 each (cold-cold-cold pod-restart pattern). Admin and
cyberjoker (cj) corpora; 4 canary SHAs.

### HG-1 — admin Dashboard session-cold ≤ 1.0s median

PROJECTION RATIONALE: Ship G's pre-this-ship cold at 0.30.160 was
9.17s wall (3 paged admin requests, median 7.99s server +
~1.2s network at 37.7 MB body). After this ship: body drops to
~40 KiB (~0.005s network), TTFB drops from 5.7s to ~0.1-0.3s per
RCA §2.5 line-item decomposition (1.5s validation removed, 0.5s
gojq removed, 0.4s marshal removed, 0.3s CopyJSON removed, 1.0s GC
mark-assist removed). Conservative target ≤1.0s median admin cold
= north-star achieved.

REVERT trigger: > 1.5s median (i.e. fix DID NOT deliver the projected
gain; investigate before next ship).

### HG-2 — cyberjoker no-regression (≤1.25× baseline)

Per prior ships' framing (Ship G, D.5). Baseline: cj cold median
on 0.30.160 pre-this-ship. Threshold: post-ship cj cold median ≤
1.25× baseline. Note the RCA §4 finding that cj's 9.97s on 0.30.160
is a SCOPE EXPANSION (cj traversing admin's full Dashboard via Ship
G's identity-free routes-loader); this ship will also benefit cj on
that traversal proportionally (cj's compositions-LIST is filtered to
0 rows BUT the resolver still runs the per-NS iterator AND the jq
AND the validate today; after this ship the validate / marshal /
encode portion is on a 50-row ceiling, halving cj's cost).

### HG-3 — byte-identical wire output for widgets NOT referencing `.slice`

Regression guard. 4-canary SHA framework: anchor 4 SHAs on
non-paginated, non-slice-referencing widget URLs pre-deploy. Post-
deploy: same 4 URLs hit; SHA1 of response bodies must match anchors.

Anchor selection: choose 4 widgets from `phase1_walk_traversal_falsifier_test.go`'s
fixture set whose widgetDataTemplate jq does NOT contain `.slice`.
Dev or tester captures the SHAs pre-deploy.

### HG-4 — 4 canary SHAs match the pre-ship anchor

(Same framework as HG-3; restated here as the "anchor canary"
guard. Includes both `.slice`-referencing AND non-referencing widget
SHAs. For `.slice`-referencing widgets the post-deploy SHA WILL
differ — that is THE FIX. Document expected vs unexpected SHA
mismatches in the ledger row.)

### HG-5 — `CACHE_ENABLED=false` reverts to ~10s admin

Mechanism toggle invariant (per `project_redis_removal.md` and
`project_caching_is_provisional.md`). This ship is NOT cache-
toggle-dependent — the injection runs on every Resolve. Verify
that `CACHE_ENABLED=false` still produces correct (sliced) output
AND that the wall-time matches the pre-ship cache-off baseline within
±20%. (i.e. confirm the injection itself adds zero measurable cost.)

### HG-6 — clean-wire audit ZERO matches

Per `feedback_byte_identical_baselines_clean_wire_shape.md`. Audit
the post-deploy wire shape for:
- token-like strings (`Bearer`, `eyJ` JWT prefix, ServiceAccount
  token shape)
- internal-field exposure (managed-fields residue, kubectl-apply
  annotations, runtime-only fields)
- ZERO matches required.

This ship does NOT touch the wire shape beyond reducing row count;
HG-6 should pass trivially.

---

## §6. Falsifier-first capture (per `feedback_falsifier_first_before_ship`)

### Pre-flight baseline at `/tmp/snowplow-runs/0.30.16x/before/`

Capture BEFORE coding (per the feedback rule). Anchor against current
`0.30.160` live pod (`snowplow-6dbc65bf85-swhb8` or successor; helm
rev to be confirmed at capture time).

Required artifacts:

1. **admin Dashboard cold n=3** — same 3-curl pattern as RCA §2.1.
   URL `/call?…&resource=tables&name=dashboard-compositions-panel-row-table&namespace=krateo-system&page=1&perPage=50`.
   Captured: HTTP code, body size, time_total, ttfb. Median computed.
   Captured to `before/admin_dashboard_table_cold_{1,2,3}.json` and
   `before/admin_dashboard_table_cold_timings.txt`.

2. **cyberjoker Dashboard cold n=3** — same shape with cj token.
   Captured to `before/cj_dashboard_table_cold_{1,2,3}.json`.

3. **Widget envelope byte sizes on the table widget** —
   `before/widget_entry_byte_sizes.txt` capturing
   `widget_content_store_total`, the per-entry size from cache stats
   (if reachable via expvar), and the raw body byte sizes of the 3
   admin requests.

4. **4 canary SHAs** — chosen non-`.slice`-referencing widgets.
   Captured to `before/canary_shas.txt` with URL + body SHA1 + body
   size.

5. **clean-wire audit** — run the audit harness (whatever today's
   form is — see Ship G §10 for the exact shell pipeline against
   the live pod) against admin + cj corpora. Output to
   `before/clean_wire_audit.txt`. EXPECT zero matches (baseline
   property).

6. **widget_content cache stats** — capture expvar snapshot of
   `widget_content_store_total`, `widget_content_evict_total`,
   `widget_content_evict_pressure`, `apistage_store_total`,
   `apistage_evict_total`, `apistage_evict_pressure`. Output to
   `before/cache_stats.json`. Anchor on the 0.30.160 RCA-reported
   eviction-pressure regime (~0.77 widget, ~0.89 apistage).

7. **expvar snapshot** — `before/expvar.json` (HeapAlloc, NumGC,
   NextGC; resolved-cache entries/bytes; refresher counters; per-
   GVR fallthrough cells).

### Post-deploy capture at `/tmp/snowplow-runs/0.30.16x/after/`

Identical artifact set. Diff:

- **`admin_dashboard_table_cold_timings.txt`**: must show median
  drop from ~9.17s to ≤1.0s (HG-1 pass).
- **`widget_entry_byte_sizes.txt`**: must show body size drop from
  ~37.7 MB to ~40 KiB.
- **`canary_shas.txt`**: 4-canary SHA match against `before/` for
  the 4 non-`.slice`-referencing widgets (HG-3/4).
- **`clean_wire_audit.txt`**: zero matches (HG-6).
- **`cache_stats.json`**: eviction pressures should drop materially
  (widget_content_evict_pressure expected < 0.20; apistage may stay
  higher initially and decline as the working set stabilises).

### Empirical validation post-deploy

Pass criteria: ALL 6 HGs PASS. Append outcome to
`project_north_star_ledger.md` and `project_feature_journal.md`
(per `feedback_maintain_feature_journal`).

Fail criteria: any HG FAIL → revert per `feedback_failure_mode_data`,
capture residual data, append to
`project_regression_journal.md` per
`feedback_maintain_regression_journal`.

---

## §7. Test plan

### §7.1 Unit tests

**`internal/resolvers/widgets/resolve_test.go`** (extend existing file
OR create `resolve_slice_injection_test.go` per dev preference):

- **`TestResolve_InjectsSliceWhenPaginated`** — Resolve with
  `PerPage=50, Page=1`; assert the `ds` handed to `resolveWidgetData`
  contains `slice: {page:1, perPage:50, offset:0}`.
- **`TestResolve_DoesNotInjectSliceWhenUnpaginated`** — Resolve with
  `PerPage=0, Page=0`; assert `ds["slice"]` is unset.
- **`TestResolve_DoesNotOverwritePreExistingSlice`** — Resolve where
  `resolveApiRef` returns `ds` with `slice` already present; assert
  the original `slice` survives (Branch A property — see §3).
- **`TestResolve_SlicedJqProducesPerPageRows`** — Resolve a widget
  whose widgetDataTemplate jq slices `.list` by `.slice.perPage`,
  on a fixture `.list` of 1000 rows with `PerPage=50, Page=1`;
  assert output `len == 50`.
- **`TestResolve_NonSlicedWidgetIsByteIdentical`** — Resolve a widget
  whose widgetDataTemplate jq does NOT reference `.slice`; compare
  output byte-for-byte against the same call with `PerPage=0,
  Page=0`.

### §7.2 Integration tests

Extend `internal/resolvers/widgets/widgets_test.go` (or the closest
extant integration suite) with a 48,999-row fixture mirroring the
dashboard table shape:

- **`TestDashboardTable_ColdPath_Slices_To_50`** — fixture
  `.allCompositions = [48,999 entries]`; RA filter
  `{list: (.allCompositions // [])}`; widget jq from
  `/tmp/table_widget.yaml`; Resolve with `PerPage=50, Page=1`.
  Assert resulting `status.widgetData.data` has exactly 50 entries
  AND the resolver wall-time is < 1s on the test fixture (test-side
  HG-1 echo). This is the load-bearing falsifier — if this passes,
  the production HG-1 passes structurally.

### §7.3 Race-condition tests

`ds` is a `map[string]any`. The injection at line 47 is a single
write into a map that is owned by the goroutine running `Resolve`
(local var, not shared). Per `feedback_shared_vs_copy_is_a_concurrency_change`,
no concurrency change is introduced. No `-race` flag test required
specifically for this ship; the existing
`internal/resolvers/restactions/api/apistage_concurrent_isolation_test.go`
covers the broader resolver concurrency.

### §7.4 No-fake-production-scenarios

Per `feedback_no_fake_production_scenarios`. The integration test
uses a 48,999-row fixture (the actual production scale). Composition
controllers stay enabled in the live-pod validation; if the cold-
path remains > 1s under load = architectural finding, NOT test
setup issue.

---

## §8. Open questions

### OQ-1 — Conditional injection: `(perPage>0 && page>0)` or always-inject-when-passed?

**Site:** `internal/resolvers/widgets/resolve.go:47` (the injection
site introduced by this ship).

**Question:** Today's `api/resolve.go:211` gates injection on
`opts.PerPage > 0 && opts.Page > 0`. Should the widget-layer
injection follow the same gate, OR a looser one (e.g. inject
whenever the request carried ANY pagination signal)?

**Recommendation:** mirror the `api/resolve.go:211` gate exactly —
`(opts.PerPage > 0 && opts.Page > 0)`. Mechanism-uniform across
the two injection sites. No new policy.

**Closure:** PM ratifies pre-dev.

### OQ-2 — Are other widgets in production also broken?

**Site:** grep `\.slice` across all RestAction widget jq pipelines
in the portal chart and live cluster.

**Action:** dev runs (against a snapshot or live cluster):

```bash
kubectl get widgetdefinitions.widgets.templates.krateo.io -A -o yaml \
  | grep -B 2 -A 5 '\.slice'
# Also walk the portal chart values for widget templates referencing .slice.
```

**Expected outcome:** the dashboard-compositions-panel-row-table is
NOT the only widget. Per RCA §1, the panel-row widget propagates
`slice: {page, perPage}` via `resourcesrefs/resolve.go:145-148` to
child widgets. Any child widget whose jq references `.slice` was
similarly broken — and is fixed by THIS ship as a side effect.

**Closure:** dev surfaces the grep result in the PR description.
ANY widget surfaced gains the fix uniformly (per
`feedback_no_special_cases` — no widget-name table). If grep
surfaces a widget that EMITTED `.slice` from the RA filter to mean
"no pagination" (i.e. relied on the absent-when-unpaged semantic),
escalate to architect — Branch A's "do not overwrite pre-existing"
guard protects this case; Branch B does not.

### OQ-3 — Is there a corresponding `.perPage` / `.page` injection that's also missing?

**Site:** the full request-context injection list — i.e. what other
keys does the widget jq expect from the request context that may
similarly be stripped by the RA filter?

**Action:** grep widget jq pipelines for `.perPage`, `.page`,
`.offset`, `.continue`, `.cursor`, `.q` (search query), `.filter`
(client-side filter param), `.sortBy`, etc.

**Expected outcome:** if other request-context keys ARE referenced
by widget jq, document them as Ship H7-FOLLOWUP candidates. They
are NOT in scope for THIS ship. THIS ship injects ONLY `.slice`
(the empirically-load-bearing case for the dashboard).

**Closure:** dev surfaces the grep result. Any followup goes to a
NEW design doc, NOT this ship.

---

## §9. Tag / commit / ledger template

Per `feedback_tag_commits.md` — tag points at the meaningful
"slice-injection" commit, not a downstream merge.

**Commit message template:**

```
fix(widgets): inject .slice into widget data source (Ship H7, 0.30.16x)

Restores widget-jq access to pagination triple {page, perPage, offset}.
The RESTAction's `spec.Filter` projection strips `.slice` from the
intermediate dict (e.g. compositions-list emits `{list: ...}`); the
widget's widgetDataTemplate jq references `.slice` and falls into the
else branch, returning the full unsliced list.

Effect on dashboard-compositions-panel-row-table cold path:
  widgetData.data rows: 48,999 → 50
  body bytes:           37.7 MB → ~40 KiB
  kube-openapi validate: ~1.5s → ~1.5ms
  admin Dashboard cold (projected): 9.17s → ≤1.0s (HG-1)

Refs:
  RCA: docs/dashboard-compositions-panel-row-table-rca-2026-05-21.md §5.A
  Design: docs/ship-slice-injection-design-2026-05-21.md
```

**Ledger row template** (append to
`project_north_star_ledger.md`):

```
| 0.30.16x | Ship H7 — .slice injection | admin cold (median) | <BEFORE>s → <AFTER>s |
  cj cold (median) | <BEFORE>s → <AFTER>s | HGs: HG-1 <P/F>, HG-2 <P/F>,
  HG-3 <P/F>, HG-4 <P/F>, HG-5 <P/F>, HG-6 <P/F> | Artifacts:
  /tmp/snowplow-runs/0.30.16x/{before,after}/ | Notes: zero-cold ship; Ship G
  cache-eviction unlocked by 1000× envelope shrink |
```

**Feature journal row template** (append to
`project_feature_journal.md`):

```
2026-05-21 — Ship H7 / 0.30.16x — .slice injection (widgets/resolve.go:47)
  Expected:  admin Dashboard cold 9.17s → ≤1.0s (HG-1); widget envelope 60 MiB → 40 KiB
  Test:      TestResolve_InjectsSliceWhenPaginated (unit); TestDashboardTable_ColdPath_Slices_To_50 (integration)
  Actual:    [filled in post-deploy]
  Delta:     [filled in post-deploy]
```

---

## §10. Architect summary

This is the load-bearing zero-cold ship surfaced by today's RCA loop
(`docs/dashboard-compositions-panel-row-table-rca-2026-05-21.md`
§5 Recommendation A). The dashboard-compositions-panel-row-table
widget's widgetDataTemplate jq explicitly references `.slice.page` and
`.slice.perPage` and authored a clean fall-through to "return all rows"
when `.slice` is absent. Today `.slice` is ALWAYS absent at the
widget-jq layer, because the compositions-list RA's filter projection
`{list: (.allCompositions // [])}` strips `.slice` from the dict that
`api.Resolve` originally constructed. The fix is to re-inject the
pagination triple into `ds` between the apiRef-resolve return and the
widget-data resolve call at `internal/resolvers/widgets/resolve.go:47`,
gated on `(opts.PerPage > 0 && opts.Page > 0)` — mechanism-uniform,
no widget-name special-case, no env flag. Combined with Ship G now
EFFECTIVE (because widget envelopes shrink 1000× → cache eviction
pressure drops from 0.77 to negligible → Ship G entries finally
survive their TTL) and Ship D.5 cleanly DORMANT on this path (no
48,999-row resolver fanout means D.5's per-NS iterator collapse is
moot for the dashboard), the projected admin Dashboard cold drops
from 9.17s to ≤1.0s — north-star achieved per
`project_north_star.md` for the dashboard path. Smaller than D.5 or
Ship G in code surface (one file, ~6 lines); larger in expected
delta (RCA §5 ranks it #1 of FIVE candidates at −5.0 to −5.5s).
PM-gated and dev-implemented same-day per the prompt; pre-flight
falsifier capture per §6 BEFORE coding, no exceptions.
