# Observability

Snowplow's runtime observability surface is three things, all on the single HTTP
port the server listens on (`main.go` `server.Addr = :<port>`):

1. **expvars** at `GET /debug/vars` — the metric surface (`main.go:869`).
2. **structured `slog` events** on stdout — the event log.
3. **pprof** at `GET /debug/pprof/*` — runtime profiling (`main.go:837-841`).

Plus two probe endpoints used by the chart:

| Path | Handler | Returns | Meaning |
|---|---|---|---|
| `GET /health` | `internal/handlers/health.go:34` | always `200 {"status":"alive"}` | liveness only — process is up; a still-warming pod is alive and must NOT be restarted |
| `GET /readyz` | `internal/handlers/readyz.go:49` | `200 {"status":"ready"}` once `cache.IsPhase1Done()`, else `503 {"status":"warming"}` | readiness — safe to receive traffic (prewarm Phase 1 has completed) |

The chart wires the **livenessProbe → `/health`** and **readinessProbe + startupProbe → `/readyz`** (`readyz.go:38-40`).

---

## Cache-off contract (read before interpreting any expvar)

Under `CACHE_ENABLED=false` the cache subsystem does not exist (transparent-fallback,
`project_cache_off_is_transparent_fallback`). Almost every `snowplow_*` expvar is registered
in an `init()` guarded by `if cache.Disabled() { return }`, so under cache-off **the key is
absent from `/debug/vars` entirely** — not present-but-zero. An absent key under cache-off is
expected, not a defect.

The exceptions, **registered unconditionally** in `main.go`'s HTTP bootstrap so a bench probe
gets `0` rather than a missing-key error under cache-off:

- `snowplow_rbac_publish_seq` — `cache.RegisterRBACSnapshotExpvar()` (`main.go:862`)
- `snowplow_authz_memo_*` — `rbac.RegisterAuthzMemoExpvar()` (`main.go:868`)

---

## expvars at `/debug/vars`

Every value is an `expvar.Func` evaluated lazily at scrape time (zero per-`/call` cost). Names
are stable for grep/Prometheus tooling. Grouped by subsystem.

### Fallthrough meter — "is the cache actually serving, or punting to the apiserver?"
Defined in `internal/cache/fallthrough_meter_expvar.go`.

| expvar | meaning | healthy range |
|---|---|---|
| `snowplow_apiserver_fallthrough_total` (`:57`) | grand-total `uint64` of read requests that bypassed the cache and hit the apiserver directly | climbs during boot/cold; should plateau once warm. A steadily climbing total on a warm pod = cache not covering the live request mix |
| `snowplow_apiserver_fallthrough_cells` (`:68`) | per-cell `map["path\|gvr\|reason"]→uint64` breakdown of the above | use to attribute fallthrough to a specific path/GVR/reason |
| `snowplow_assertion_violations_total` (`:60`) | `map["read_paths_scoped"]→uint64` — architectural-invariant breaches (a `/call`-class route not wrapped with `FallthroughScopeMiddleware`) | **0**. Non-zero = an invariant is broken in prod (logged ERROR, asserted at boot by `cache.AssertReadPathsScoped()`, `main.go:908`) |

### Dispatch L1 lookups — resolved-output cache hit rate
Defined in `internal/handlers/dispatchers/l1_lookup_metrics.go`.

| expvar | meaning | healthy range |
|---|---|---|
| `snowplow_dispatch_l1_lookups` (`:116`) | `map["<handlerKind>\|<gvr>"→{"hit_total","miss_total"}]`; handlerKind ∈ {restactions, widgets, widgetContent} | high hit_total/(hit+miss) on a warm pod. Sustained low hit ratio = prewarm not covering the served mix |

### RAFullList serve path — the cheap Go-slice serve for big LISTs
Defined in `internal/cache/bindings_by_gvr_metrics.go`.

| expvar | meaning | healthy range |
|---|---|---|
| `snowplow_ra_full_list_serve` (`:50`) | `map{hit, repopulate, verified_slice, fallback}` serve-outcome counters for the RAFullList cell | admin's first compositions `/call` should drive `hit`+1 over a warm prewarm-pinned cell; rising `fallback` = the cheap path is not engaging |
| `snowplow_ra_full_list_memo` (`:71`) | per-(RA × sliceShape) sliceability verdict snapshot | diagnostic for the three RAFullList failure modes (boot empty-full self-heal, prewarm not reaching widget, first-sight byte mismatch) |
| `snowplow_sliceability_reverify` (`:78`) | async sliceability-reverify worker counters | evidence the stuck-false reverify path is firing (informer event → re-verify within ≤60s) |
| `snowplow_bindings_by_gvr_delta_skipped_non_typed` (`:60`) | `uint64` — delta-event objects neither typed nor convertible, and DROPPED | **0**. Non-zero = the bindings-by-GVR index is drifting until the next boot rebuild (a silent-data-staleness canary) |

### Prewarm — boot warm-up completion + walk coverage
| expvar | meaning | healthy range |
|---|---|---|
| `snowplow_prewarm_complete` (`internal/cache/prewarm_complete_metric.go:106`) | `map{done:0/1, elapsed_ms}` — `done=1` once `Phase1Done` flips (same atomic `/readyz` reads); `elapsed_ms` = process-start→done, `-1` until flip | `done` reaches `1`; `elapsed_ms` is the cold-start-to-ready time |
| `snowplow_phase1_units_planned` (`internal/handlers/dispatchers/phase1_walk_pagination_metrics.go:133`) | `uint64` — widgetContent cells the apiRef pagination walk planned to seed | at 50K must climb toward the reachable widget-CR count; pinned near `500 × widget-count` = page cap still binding |
| `snowplow_phase1_units_seeded` (`:136`) | `uint64` — page cells handed to `populateWidgetContentL1` with a non-nil envelope (lower bound on L1 writes) | reconciles with units_planned minus the skip counters below |
| `snowplow_phase1_apiref_pages_total` (`:139`) | `uint64` — extra apiRef pages (page 2..N) resolved across all paginated widgets | ≫ `500 × widget-count` confirms the page backstop raise took effect |
| `snowplow_phase1_eligible_no_continue_total` (`:142`) | `uint64` — distinct eligible widgets whose page-1 resolve produced no continuation | tracks genuinely single-page apiRef widgets; a SPIKE on a post-storm boot = re-collection had retry work |
| `snowplow_phase1_walk_children` (`internal/handlers/dispatchers/phase1_walk_metrics.go:175`) | `map[root→{children-count entry}]` per-root boot walk fan-out | diagnostic for which navigation roots produced children |
| `snowplow_phase1_walk_zero_children_total` (`:186`) | `uint64` — walk observations that found zero children | a high count = roots resolving empty (possible RBAC/data gap) |
| `snowplow_phase1_walk_observations_total` (`:189`) | `uint64` — total walk children-count observations | denominator for the zero-children ratio |

### Prewarm engine — the unified walk/seed worker (the production path)
Defined in `internal/handlers/dispatchers/prewarm_engine_metrics.go`.

| expvar | meaning | healthy range |
|---|---|---|
| `snowplow_prewarm_engine_enqueued_total` (`:56`) | `uint64` — cumulative `enqueueScope` calls (every enqueue, even dedup-coalesced) | climbs during boot/re-walk |
| `snowplow_prewarm_engine_processed_total` (`:59`) | `uint64` — scopes fully processed by the worker | `processed ≈ enqueued − dedups` means the queue drained |
| `snowplow_prewarm_engine_yield_total` (`:62`) | `uint64` — worker parked because a customer `/call` was in flight (customer-priority yield) | >0 under customer load = the yield hook is working |
| `snowplow_prewarm_engine_pending_depth` (`:65`) | live `len(e.pending)` | **0** once the worker drains; sustained non-zero across many scrapes = worker dead |

### Phase-1 seed — per-cohort/per-target seed outcomes
Defined in `internal/handlers/dispatchers/phase1_pip_metrics.go`.

| expvar | meaning | healthy range |
|---|---|---|
| `snowplow_phase1_bindingset_seed_resolves_total` (`:181`) | `uint64` — per-binding-target seed resolves | climbs during seed |
| `snowplow_phase1_bindingset_seed_failures_total` (`:184`) | `uint64` — grand-total seed failures (= rbac_deny + operational; back-compat) | interpret via the split below |
| `snowplow_phase1_seed_rbac_deny_total` (`:192`) | `uint64` — EXPECTED narrow-RBAC denies (403/401); cohort genuinely can't read the target | non-zero is **normal**; these need no L1 entry, not re-enqueued |
| `snowplow_phase1_seed_operational_fail_total` (`:195`) | `uint64` — UNEXPECTED failures (ctx timeout/cancel, 5xx, transport, panic) | **0**. Non-zero = a real hole; these ARE re-enqueued |
| `snowplow_phase1_widget_seed_failure_total` (`:203`) | `map["cohort\|name\|gvr"→uint64]` — which widget broke which cohort | pinpoints a per-widget per-cohort seed failure |
| `snowplow_phase1_restaction_seed_failure_total` (`:211`) | `map["cohort\|namespace/name"→uint64]` — same for RESTActions | pinpoints a per-RESTAction per-cohort seed failure |
| `snowplow_phase1_cohort_seed_status` (`:220`) | `map[cohort→"success"\|"partial"\|"failed"]` | all `success` on a clean boot; `partial`/`failed` flags the affected cohort |

### Refresher — background re-resolve worker pool
Defined in `internal/cache/refresher_metrics.go`.

| expvar | meaning | healthy range |
|---|---|---|
| `snowplow_refresher_enqueue_total` (`:67`) | `uint64` — re-resolve tasks enqueued | climbs with CRUD churn |
| `snowplow_refresher_completed_total` (`:70`) | `uint64` — tasks completed | tracks enqueue under steady state |
| `snowplow_refresher_failed_total` (`:73`) | `uint64` — tasks failed | low/stable |
| `snowplow_refresher_retried_total` (`:76`) | `uint64` — tasks retried | low |
| `snowplow_refresher_dropped_total` (`:79`) | `uint64` — tasks dropped | low |
| `snowplow_refresher_skipped_no_entry_total` (`:82`) | `uint64` — skipped, no L1 entry to refresh | informational |
| `snowplow_refresher_skipped_no_handler_total` (`:85`) | `uint64` — skipped, no handler | informational |
| `snowplow_refresher_skipped_stage_error_total` (`:88`) | `uint64` — skipped due to stage error | low |
| `snowplow_refresher_queue_depth` (`:95`) | live workqueue `Len()` | near 0; climbing depth with stagnant `completed_total` = workers stuck (back-pressure) |
| `snowplow_refresher_yielded_total` (`:111`) | `uint64` — worker yield-parked for a customer `/call` | >0 under customer burst (if 0, hook broken) |
| `snowplow_refresher_capped_total` (`:114`) | `uint64` — yield max-parked cap fired (proceeded anyway) | **near 0**; steady climb = inflight counter leaking or sustained pressure |
| `snowplow_refresher_floored_total` (`:126`) | `uint64` — dequeue rate-floor deferred a key (entry younger than floor) | >0 under install-churn storm = the floor gate is protecting against re-resolve storms |

### RBAC snapshot + authz memo — subject-index freshness and serve-time eval cache
| expvar | meaning | healthy range |
|---|---|---|
| `snowplow_rbac_publish_seq` (`internal/cache/rbac_snapshot_expvar.go:54`) | `uint64` — incremented once per successful RBAC-snapshot publish | bumps within ~30s of a RoleBinding ADD/DELETE; `0` = no snapshot published (cache-off or pre-readiness) |
| `snowplow_authz_memo_hits` (`internal/rbac/snapshot_authz_memo.go:236`) | `uint64` — authz-memo hits | hit rate = hits/(hits+misses); target ≥0.85 warm |
| `snowplow_authz_memo_misses` (`:237`) | `uint64` — authz-memo misses | — |
| `snowplow_authz_memo_swaps` (`:238`) | `uint64` — generation shard swaps | bumps on RBAC-snapshot generation change |
| `snowplow_authz_memo_refused` (`:239`) | `uint64` — cap-breach refused inserts | low; sustained climb = memo cap pressure |
| `snowplow_authz_memo_entries` (`:240`) | `int` — live entry count of the current shard | bounded by cap |
| `snowplow_authz_memo_deny_uncached_total` (`:242`) | `uint64` — denies (never cached) | informational; denies are deliberately not memoized |

### Informer / discovery surface
| expvar | meaning | healthy range |
|---|---|---|
| `snowplow_plurals_registered_gvrs` (`internal/cache/registered_gvrs_expvar.go:97`) | `{count, gvrs:[group/version/resource…], last_register_unix_ns}` — live set of GVRs with a registered informer | `count` tracks the cluster's served GVR set (~≤50); two scrapes with identical `last_register_unix_ns` = informer set quiesced |
| `snowplow_crd_discovery` (`internal/cache/crd_discovery_expvar.go:51`) | `map{events_enqueued, events_dropped, events_processed, discovery_invoked, discovery_skipped_ng, deletes_processed, delete_skipped_ng, panics_recovered}` | `events_dropped`/`*_skipped_ng`/`panics_recovered` should be **0** on a healthy cluster — a non-zero `discovery_skipped_ng` is a flashing red flag (silent-skip defect class) |
| `snowplow_crd_schema_memo_hits_total` (`internal/resolvers/crds/schema/schema_cache_metrics.go:59`) | `uint64` — compiled-CRD-schema memo hits | high hit ratio warm |
| `snowplow_crd_schema_memo_misses_total` (`:62`) | `uint64` — memo misses | — |
| `snowplow_crd_schema_memo_stale_dropped_total` (`:70`) | `uint64` — generation-fence drops (CRD lifecycle moved gen during GET+compile) | expected under concurrent CRD install; steady climb without installs = generation churn |
| `snowplow_crd_schema_memo_invalidations_total` (`:78`) | `uint64` — full-reset count (CRD-lifecycle bridge clears) | bumps on CRD lifecycle events |
| `snowplow_sa_discovery_builds_total` (`internal/dynamic/cached_client_metrics.go:59`) | `uint64` — SA-discovery client builds | informational |
| `snowplow_sa_discovery_invalidations_total` (`:62`) | `uint64` — SA-discovery cache invalidations | informational |
| `snowplow_sa_discovery_fallbacks_total` (`:65`) | `uint64` — SA-discovery fallbacks | low; climb = discovery degrading to fallback |

### Upstream controller health — "is snowplow broken, or is an upstream controller crash-looping?"
Defined in `internal/cache/controller_health_expvar.go`; entry shapes in `internal/cache/controller_health.go:78-94`.

| expvar | meaning | healthy range |
|---|---|---|
| `snowplow_upstream_controller_health` (`:58`) | `map["<ns>/<name>"→{Healthy, Reason, PodRestartCount, EndpointReadyCount, Namespace, Name, LastObserved}]` for auto-discovered controllers | every entry `Healthy=1`, `Reason=""`. `Reason` enum (`controller_health.go:108-114`): `pod-restart-within-window`, `endpoints-zero-ready`, `both`, `unwired` |
| `snowplow_upstream_webhook_failurepolicy` (`:59`) | `map["<webhookName>"→{Policy:"Fail"/"Ignore", Configuration, Type:"Mutating"/"Validating"}]` | a `Fail`-policy webhook on a crash-looping controller explains apiserver pressure / write hangs |

---

## Key `slog` events

JSON structured logs on stdout. Message strings are stable, dotted, and greppable. The
operator-notable ones:

| Event (message) | Level | Site | What it tells an operator |
|---|---|---|---|
| `phase1.warmup.completed` | Info | `internal/handlers/dispatchers/phase1_walk.go:818` | the prewarm walk finished — the boundary `/readyz` and `snowplow_prewarm_complete` track; carries the elapsed and counts |
| `cache.prewarm.completed` | Info | `internal/cache/prewarm.go:168` | the cache-side prewarm seed finished |
| `prewarm.engine.boot.complete` / `.started` | Info | `internal/handlers/dispatchers/` (prewarm engine) | engine boot lifecycle |
| `phase1.seed.cohort.operational_failure` | (failure) | dispatchers phase1 seed | a cohort hit an UNEXPECTED seed failure — pairs with `snowplow_phase1_seed_operational_fail_total`; actionable |
| `phase1.seed.cohort.expected_deny` | (info) | dispatchers phase1 seed | EXPECTED narrow-RBAC deny — normal, pairs with `snowplow_phase1_seed_rbac_deny_total` |
| `phase1.walk.apiref_pagination.backstop_hit` | Warn | `internal/handlers/dispatchers/phase1_walk_pagination.go:597` | a widget's apiRef pagination hit the anti-runaway page ceiling — coverage may be capped |
| `apiserver_fallthrough` | Warn | `internal/cache/fallthrough_meter.go:359` | a read punted to the apiserver — the log companion to `snowplow_apiserver_fallthrough_total`; includes path/gvr/reason |
| `cache.read_paths_scoped.violation` | Error | `internal/cache/fallthrough_assert.go:123` | architectural invariant breach — a `/call` route is not scope-wrapped; bumps `snowplow_assertion_violations_total` |
| `cache.bindings_by_gvr.delta_skipped_non_typed` | Warn | `internal/cache/bindings_by_gvr_delta.go:117` | an index delta event was dropped — index is drifting; pairs with the same-named expvar |
| `cache.crd_discovery.event_dropped` | Warn | `internal/cache/crd_discovery_side_effect.go:253` | a CRD-discovery event was dropped (queue full / shutdown); pairs with `snowplow_crd_discovery.events_dropped` |
| `cache.controller_health.watch.broken` | Warn | `internal/cache/controller_health.go:369` | an upstream controller-health watch broke — the health gauge may go stale |
| `cache.rbac.snapshot.published` | Info | `internal/cache/` rbac snapshot | a new RBAC subject-index snapshot published; pairs with `snowplow_rbac_publish_seq` |
| `cache.secrets.informer.assertion_violation` | Error | `internal/cache/secrets_informer.go:377,392` | secrets-informer invariant breach |

There are many more dotted events in the `phase1.*`, `prewarm.engine.*`, `cache.crd_discovery.*`,
`cache.rbac.snapshot.*`, and `cache.discovery.*` families (full set is greppable with
`grep -rhoE '"(phase1|prewarm|cache)\.[a-z_.]+"' internal main.go`); the table above lists the
ones that map to an alarm-worthy condition or pair directly with an expvar.

---

## pprof

Registered on the custom server mux (the server does **not** use `http.DefaultServeMux`),
`main.go:837-841`:

| Path | Profile |
|---|---|
| `GET /debug/pprof/` | index (links to goroutine, heap, allocs, threadcreate, block, mutex, …) |
| `GET /debug/pprof/cmdline` | process command line |
| `GET /debug/pprof/profile` | 30s CPU profile |
| `GET /debug/pprof/symbol` | symbol lookup |
| `GET /debug/pprof/trace` | execution trace |

The index path also serves the goroutine/heap/allocs/mutex/block/threadcreate sub-profiles (the
comment at `main.go:836` enumerates them). `main.go:101` notes mutex + block profiling fractions
are set at startup so `/debug/pprof/mutex` and `/debug/pprof/block` return non-empty data.

Typical use:

```
go tool pprof http://<pod>:<port>/debug/pprof/heap          # memory
go tool pprof http://<pod>:<port>/debug/pprof/profile       # CPU (30s)
curl http://<pod>:<port>/debug/pprof/goroutine?debug=2      # goroutine dump (deadlock/leak)
```

---

## Notes / discrepancies vs. the documentation plan

- The plan (§4 / §3) names a single `observability.md`; this is it. The expvar/event tables
  above are built from the code, not from prose.
- The plan's `ARCHITECTURE.md` trace-anchor list (§4) cites `/debug/vars` only and does not
  enumerate the expvars — that enumeration is this document.
- Several phase-1 seed expvars referenced in older notes (`snowplow_phase1_seed_restactions_total`,
  `snowplow_phase1_seed_widgets_total` and their `_by_cohort` maps; the binding-set
  classes / powerset-skipped counters) were **deleted** as always-zero in production and are
  intentionally NOT in the code today (`phase1_pip_metrics.go:156-180`) — they are excluded here.
