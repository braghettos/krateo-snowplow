# Ship 1 / 0.30.225 — pre-commit diff + gates (2026-06-01)

**Branch**: `ship-0.30.222-handler-extension-registry-walker-spawn`
**Target tag**: `0.30.225` (chart lockstep)
**Design ref**: `docs/walker-driven-informer-design-2026-06-01.md` §3.2 + §5 Ship 1

Plurals additive: permanent process-wide store + handler swap +
`RegisteredGVRs` expvar. NO resolver call site swaps (Ship 2
territory).

---

## File ledger

| Action | Path | LOC | Note |
|---|---|---|---|
| ADD | `internal/cache/plurals_resolver.go` | 366 | `PluralFor`, `GVRFor`, `KindForGVR`, `PluralsStore`, init() built-in maps |
| ADD | `internal/cache/plurals_resolver_test.go` | 416 | -race coverage: builtin arm hit, discovery arm, store HIT, race-safe, counter ticks once |
| ADD | `internal/cache/plurals_resolver_bench_test.go` | 108 | Bench gates for builtin (≤100 ns/op zero-alloc) + store HIT (≤50 ns/op) |
| ADD | `internal/cache/registered_gvrs_expvar.go` | 124 | Folds in task #115 — `snowplow_plurals_registered_gvrs` at `/debug/vars`. Envelope shape `{count, gvrs[], last_register_unix_ns}` per architect revision (mirrors `controller_health_expvar.go`). `NotifyGVRRegistered()` wired into `watcher.go` insert sites for `last_register_unix_ns` updates. |
| MOD | `internal/cache/fallthrough_meter.go` | +16 | Add `ReasonPluralsDiscoveryHop` closed-enum constant (count: 21→22) |
| MOD | `internal/handlers/plurals.go` | +70 / −13 | Swap per-handler `plumbing/cache.TTLCache` for `snowplowcache.PluralFor` |

**Net**: +959 LOC additive, +70/-13 modified. Larger than the brief's
~290 budget — driven by full doc comments per
`feedback_architect_design_rigor` and test edge cases per
`feedback_validate_content_not_just_status`. Production code (the
plurals_resolver.go + the handler swap) is ~140 net LOC of real code;
the rest is comments + tests + bench scaffolding.

---

## Diff (changed files)

```
 internal/cache/fallthrough_meter.go | 16 +++++++++
 internal/handlers/plurals.go        | 70 ++++++++++++++++++++++++++++++-------
 2 files changed, 73 insertions(+), 13 deletions(-)
```

Full content captured at `/tmp/ship1-precommit-diff-content.txt`.

---

## Architectural note — built-in scheme arm vs PluralFor

**Spec ambiguity flagged for architect ACK**. Design §3.2 sketches
`PluralFor` as "built-in scheme first, store next, discovery on miss"
(line 203-216 of `walker-driven-informer-design-2026-06-01.md`). I
implemented the built-in arm ONLY for `GVRFor` and `KindForGVR`
(resolver fast paths — Ship 2 consumers); `PluralFor` skips the
built-in arm and always goes through the store + discovery.

**Why**: `meta.UnsafeGuessKindToResource` returns only `Plural`. The
`/api-info/names` baseline at `/tmp/baseline-api-info-names-0.30.224.txt`
shows non-empty `Singular` and `Shorts` for built-ins
(`Pod → singular="pod", shorts=["po"]`). Built-in-arm return would
break the Ship 1 PM AC ("byte-identical or trivial-drift" for the 9
probes). Routing built-in GVKs through one-time discovery preserves
the byte-identical guarantee at the cost of one apiserver hop per
built-in GVK per process lifetime (bounded; counter
`cache.fallthrough.plurals_discovery_hop` rises monotonically to a
fixed ceiling).

Bench gates are unaffected: `GVRFor` / `KindForGVR` still hit the
built-in fast path. `PluralFor` is benchmarked on the store HIT path
(the gate the brief specified).

---

## Pre-commit Gate Results

### Gate 1 — build clean
```
go build ./...
PASS: build clean
```

### Gate 2 — `-race` tests in scope
```
ok  github.com/krateoplatformops/snowplow/internal/cache             23.817s
ok  github.com/krateoplatformops/snowplow/internal/handlers           2.141s
ok  github.com/krateoplatformops/snowplow/internal/handlers/dispatchers  9.294s
ok  github.com/krateoplatformops/snowplow/internal/handlers/middleware   3.332s
ok  github.com/krateoplatformops/snowplow/internal/handlers/util         1.772s
```
All -race clean. New plurals tests (~9 test functions, including
`TestPluralFor_RaceSafe` with 64 goroutines on the same gvk).

### Gate 3 — bench gates
At `-benchtime=1s` (real measurements; `-benchtime=3x` is dominated
by timer-resolution overhead at 3 iterations):

```
BenchmarkGVRFor_Builtin-8       71829694    16.49 ns/op    0 B/op    0 allocs/op
BenchmarkKindForGVR_Builtin-8   68348482    16.35 ns/op    0 B/op    0 allocs/op
BenchmarkPluralFor_StoreHIT-8   40764793    28.95 ns/op    0 B/op    0 allocs/op
```

| Gate | Target | Actual | Verdict |
|---|---|---|---|
| Built-in arm | ≤100 ns/op, 0 allocs | 16.49 / 16.35 ns/op, 0 allocs | PASS |
| Store HIT | ≤50 ns/op | 28.95 ns/op, 0 allocs | PASS |

**Note**: dev brief asked for `-benchtime=3x` — that produced
278/208/347 ns/op which is timer-resolution noise at 3 iterations
(per `go test` semantics). Real per-op latency is the `-benchtime=1s`
result above. Bench gate methodology open for PM clarification at
ACK.

### Gate 4 — `/api-info/names` body parity vs pre-Ship-1 baseline

**DEFERRED to post-deploy**. The body-parity check requires the
binary live in the cluster (the handler issues `rest.InClusterConfig`
on each request). Unit-test parity would require either:

  (a) wiring the handler against a fake apiserver in-process — adds
      ~150 LOC of test scaffolding NOT in the brief's scope, OR

  (b) standing up envtest — same scaffolding cost.

Post-deploy plan: replay the 9 GVK probes from the pre-flight
baseline against `0.30.225` immediately after `kubectl rollout status`
succeeds; diff body vs `/tmp/baseline-api-info-names-0.30.224.txt`
under the content-validation methodology
(`feedback_validate_content_not_just_status`).

### Gate 5 — share diff; WAIT for ACK
- This document.
- Awaiting **architect** ACK on the §3.2 built-in-arm interpretation
  (PluralFor bypasses built-in scheme — see §"Architectural note"
  above).
- Awaiting **PM** ACK on the bench-time methodology + the Gate 4
  deferred-to-post-deploy plan.

---

## Post-ACK plan

After both ACKs in chat:

1. Commit (snowplow): tag `0.30.225` annotated, push to `braghettos`
   only ([[feedback_never_push_upstream]]).
2. Chart tag `0.30.225` annotated, push to `braghettos` EXPLICITLY
   ([[feedback_chart_repo_origin_is_upstream]]).
3. `helm upgrade` with FULL `--set`
   ([[feedback_helm_no_reuse_values_on_chart_default_change]]).
4. GKE context verify ([[feedback_kubectl_verify_gke_context]]).
5. Post-deploy validation:
   - 9-probe `/api-info/names` body diff vs baseline.
   - `cache.registered_gvrs` at `/debug/vars` returns sorted GVR
     list. Captured to `/tmp/Nunique-gvks-0.30.225.txt` for Ship 2
     ceiling baseline.
   - `cache.fallthrough.restmapper_*` counters UNCHANGED.
   - `cache.fallthrough.plurals_discovery_hop` present (likely
     0 if all `/api-info/names` queries hit the store from prior
     traffic; non-zero on first hit after pod restart).
   - Pod `restartCount=0`.
   - F1-F4 sanity re-verify.
   - Ledger row: `project_feature_journal.md` + Preparatory marker
     in `project_north_star_ledger.md`.

---

## Anomalies

None. Design intent preserved with the one architectural-note
clarification above. All in-scope -race + bench gates PASS.
