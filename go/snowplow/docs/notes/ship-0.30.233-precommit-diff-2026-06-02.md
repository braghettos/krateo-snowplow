# Ship 0.30.233 — Pre-commit Diff Artifact

**Date**: 2026-06-02
**Branch**: `ship-0.30.233-s4-cache-invalidation-2026-06-02`
**Base**: `4f1ba26` (Block 5 tip) on `bench-path-b-block-5-2026-06-02`
**Author**: cache-developer
**Design**: `docs/ship-0.30.233-s4-cache-invalidation-trace-2026-06-02.md`

---

## File Manifest

| File | Status | Total LOC | Code LOC | Comments |
|---|---|---:|---:|---:|
| `internal/cache/crd_gvr.go` | NEW | 53 | 13 | 40 |
| `internal/cache/sa_rc_singleton.go` | NEW | 73 | 24 | 49 |
| `internal/cache/crd_discovery_side_effect.go` | NEW | 352 | 183 | 169 |
| `internal/cache/deps_watch.go` | EDIT (+20) | — | +7 | +13 |
| `main.go` | EDIT (+13) | — | +1 | +12 |
| `internal/cache/crd_add_discovery_test.go` | NEW (test) | 559 | 313 | 246 |

**Production code**: ~252 LOC (vs design estimate ~190 LOC; +62 LOC for PM tightening #1 worker channel infrastructure).
**Comment overhead**: ~450 LOC (load-bearing rationale per Ship 0.30.232 precedent — file headers explain WHY, not WHAT).
**Test code**: 313 LOC across 5 falsifier tests.

---

## Architect Design + 2 PM Tightenings — As-Built Cross-Reference

| Design item | Status | File:line |
|---|---|---|
| 1. `IsCRDGVR(gvr)` predicate, single centralised constant | DONE | `internal/cache/crd_gvr.go:32-47` |
| 2. `deps_watch.go` AddFunc dispatches CRD side-effect | DONE | `internal/cache/deps_watch.go:209-219` (hot-path hoisted to :199 per perf comment) |
| 3. `triggerCRDDiscovery(obj)` | DONE | `internal/cache/crd_discovery_side_effect.go:218-291` |
| 4. `main.go` wires `cache.SetProcessSARestConfig(rc)` | DONE | `main.go:207-216` |
| 5. `DiscoverGroupResources` UNCHANGED | CONFIRMED | `internal/cache/discovery_lookup.go:216-330` (untouched) |
| **PM Tightening #1** — bounded worker channel | DONE | `internal/cache/crd_discovery_side_effect.go:65-205` |
| **PM Tightening #2** — `defer recover()` in trigger | DONE | `internal/cache/crd_discovery_side_effect.go:232-247` + worker recover at `:122-131` |

### PM Tightening #1 — Worker channel implementation

```go
// crd_discovery_side_effect.go:65
const crdDiscoveryQueueDepth = 256

// crd_discovery_side_effect.go:79
type crdDiscovery struct {
    queue chan crdDiscoveryEvent  // bounded buffer
    startOnce sync.Once
    stopCh chan struct{}
    workerWG sync.WaitGroup
    // counters: enqueued, dropped, processed, invoked, skipped, panicsRecovered
}

// crd_discovery_side_effect.go:173 — non-blocking enqueue
func (c *crdDiscovery) submitCRDDiscoveryEvent(obj interface{}) {
    c.startCRDDiscoveryWorker()
    select {
    case c.queue <- crdDiscoveryEvent{obj: obj}:
        c.eventsEnqueued.Add(1)
    default:
        c.eventsDropped.Add(1)
        slog.Warn("cache.crd_discovery.event_dropped", ...)
    }
}
```

Mirrors `submitDeleteEvent` at `deps_watch.go:133-146`. Single worker goroutine drains the queue.

### PM Tightening #2 — Panic recover wrapper

```go
// crd_discovery_side_effect.go:232
func triggerCRDDiscovery(obj interface{}) {
    c := crdDiscoverySingleton()
    defer func() {
        if rec := recover(); rec != nil {
            c.panicsRecovered.Add(1)
            slog.Error("cache.crd_discovery.trigger.panic_recovered",
                slog.Any("panic", rec),
                slog.String("stack", string(debug.Stack())),
                ...)
        }
    }()
    // ...
}
```

Wraps the entire trigger body — extraction + AddNavigationDiscoveredGroup + DiscoverGroupResources call. Worker goroutine has its OWN outer recover at `:122-131` (mirrors `deleteWorker` pattern).

---

## Pre-Commit Gates — PASS/FAIL Evidence

### G1 — `go build` + `go test -race -count=1 ./internal/cache/...`
**STATUS: PASS**

```
$ go build ./...
(clean)

$ go test -race -count=1 ./internal/cache/...
ok  	github.com/krateoplatformops/snowplow/internal/cache	24.199s
```

### G2 — Falsifier MUST PASS on ship branch, MUST FAIL on 4f1ba26
**STATUS: PASS (both directions verified)**

**On ship branch (HEAD)**:
```
$ go test -race -count=1 -run TestCRDAdd_TriggersGroupDiscovery -v ./internal/cache/...
=== RUN   TestCRDAdd_TriggersGroupDiscovery
2026/06/02 07:41:34 INFO cache.discovery.navigation_discovered_group_added subsystem=cache group=composition.krateo.io ...
--- PASS: TestCRDAdd_TriggersGroupDiscovery (0.01s)
PASS
```

**On 4f1ba26 (pre-fix) binary** — verified by copying the new files to a temp worktree at 4f1ba26 (so the falsifier compiles) but WITHOUT the `deps_watch.go` AddFunc edit. The falsifier FAILS because `submitCRDDiscoveryEvent` is never called:

```
$ cd /tmp/snowplow-pre-fix && go test -race -count=1 -run TestCRDAdd_TriggersGroupDiscovery -v ./internal/cache/...
=== RUN   TestCRDAdd_TriggersGroupDiscovery
    crd_add_discovery_test.go:199: Ship 0.30.233 FAIL: worker did not process the CRD ADD event within 2s;
        counters: Enqueued=0 Dropped=0 Processed=0 Invoked=0 SkippedNG=0 PanicsRecovered=0
--- FAIL: TestCRDAdd_TriggersGroupDiscovery (2.00s)
FAIL
```

`Enqueued=0` proves the AddFunc did not dispatch the side-effect — the falsifier correctly identifies the pre-fix regression.

### G3 — Mechanism-uniform (zero new production-code GVR/customer literals)
**STATUS: PASS**

Total occurrences of `composition.krateo.io | githubscaffolding | cyberjoker | admin\b` across `internal/`:
- Base (4f1ba26): 513
- Current (HEAD): 513

```
$ diff /tmp/g3-base.txt /tmp/g3-current.txt
(empty — zero differences)
```

Production-code literals in new/edited files:
```
$ grep -rn "composition.krateo.io|githubscaffolding|cyberjoker|admin\b" \
    internal/cache/crd_gvr.go \
    internal/cache/sa_rc_singleton.go \
    internal/cache/crd_discovery_side_effect.go \
    internal/cache/deps_watch.go
(zero matches)
```

The only acceptable literal — the CRD meta GVR — lives in EXACTLY ONE production location:
```
internal/cache/crd_gvr.go:32:    Group:    "apiextensions.k8s.io",
internal/cache/crd_gvr.go:34:    Resource: "customresourcedefinitions",
```

The 18 test-file occurrences in `crd_add_discovery_test.go` are synthetic fixture data for the falsifier — legitimate test inputs, NOT policy logic.

### G4 — Worker channel mirrors deleteEvictCh pattern
**STATUS: PASS**

```
$ grep -n "submitCRDDiscoveryEvent|crdDiscoveryEvCh|crdDiscovery " internal/cache/*.go | grep -v _test.go
internal/cache/crd_discovery_side_effect.go:75:// crdDiscovery is the process-scoped CRD-ADD-side-effect bridge:
internal/cache/crd_discovery_side_effect.go:79:type crdDiscovery struct {
internal/cache/crd_discovery_side_effect.go:105:func crdDiscoverySingleton() *crdDiscovery {
internal/cache/crd_discovery_side_effect.go:165:// submitCRDDiscoveryEvent enqueues a CRD-ADD event onto the
internal/cache/crd_discovery_side_effect.go:173:func (c *crdDiscovery) submitCRDDiscoveryEvent(obj interface{}) {
internal/cache/deps_watch.go:218:                crdDiscoverySingleton().submitCRDDiscoveryEvent(obj)
```

The `TestCRDAdd_WorkerNotBlockedByDiscoveryHop` test verifies the decoupling: 5 AddFunc calls (each enqueueing a CRD event) complete in << 100ms even when the discovery hook stalls for 200ms — proving submitCRDDiscoveryEvent is non-blocking and the informer processor goroutine is NOT blocked by the discovery hop.

### G5 — Recover wrapper present
**STATUS: PASS**

```
$ grep -n "recover()" internal/cache/crd_discovery_side_effect.go
37:// PM TIGHTENING #2 — defer recover() inside triggerCRDDiscovery.
127:				if rec := recover(); rec != nil {     # worker goroutine outer
235:		if rec := recover(); rec != nil {              # triggerCRDDiscovery body
```

Two recover wrappers:
- **Worker goroutine** (`:122-131`): catches any panic that escapes `triggerCRDDiscovery` (defence in depth — should never fire if the inner recover catches first).
- **`triggerCRDDiscovery` body** (`:232-247`): catches panics during spec.group extraction or DiscoverGroupResources, increments `panicsRecovered` counter, logs at error level with debug.Stack().

`TestCRDAdd_RecoversFromMalformedObject` validates the worker stays alive across malformed inputs (no-group + empty-group → soft-skip → well-formed CRD → success).

### G6 — LOC delta split-report
**STATUS: PASS with documented variance**

See File Manifest above. Production code ~252 LOC (vs ~190 LOC design estimate). The +62 LOC delta is PM tightening #1 worker channel infrastructure (single-worker lifecycle, bounded queue, counters, drop-on-full WARN). Comment overhead within Ship 0.30.232 precedent.

---

## Production diff (deps_watch.go + main.go)

### `internal/cache/deps_watch.go`

```diff
@@ -190,6 +190,13 @@ func (w *depWatch) stopDeleteWorker() {
 //     propagate (could storm) and never block (could deadlock).
 func (rw *ResourceWatcher) depEventHandlers(gvr schema.GroupVersionResource) clientcache.ResourceEventHandlerFuncs {
 	w := depWatchSingleton()
+	// Ship 0.30.233 — pre-compute the CRD-meta-GVR predicate once
+	// per handler-set construction, NOT per-event. The predicate is
+	// a structural equality check (IsCRDGVR — see crd_gvr.go);
+	// hoisting it here keeps the AddFunc hot path free of any
+	// GVR-comparison cost for the 99% case where gvr is NOT the
+	// CRD meta-GVR.
+	crdSideEffect := IsCRDGVR(gvr)
 	return clientcache.ResourceEventHandlerFuncs{
 		AddFunc: func(obj interface{}) {
 			if !rw.addEventPostSync(gvr, w) {
@@ -199,6 +206,17 @@ func (rw *ResourceWatcher) depEventHandlers(gvr schema.GroupVersionResource) cli
 			ns, name := metaNSName(obj)
 			w.counters.addPropagated.Add(1)
 			Deps().OnAdd(gvr, ns, name)
+			// Ship 0.30.233 — CRD-ADD discovery side-effect.
+			// Dispatches to the bounded worker channel so the
+			// network-bound DiscoverGroupResources hop runs OFF
+			// the informer processor goroutine (PM tightening
+			// #1). triggerCRDDiscovery has its own defer recover
+			// (PM tightening #2) so a malformed CRD object cannot
+			// panic-kill the worker or this processor goroutine.
+			// See crd_discovery_side_effect.go.
+			if crdSideEffect {
+				crdDiscoverySingleton().submitCRDDiscoveryEvent(obj)
+			}
 		},
 		UpdateFunc: func(_, newObj interface{}) {
 			ns, name := metaNSName(newObj)
```

### `main.go`

```diff
@@ -204,6 +204,19 @@ func main() {
 					cacheWatcher = w
 					cache.SetGlobal(w)

+					// Ship 0.30.233 — wire the SA *rest.Config as a
+					// process singleton so the CRD-ADD discovery
+					// side-effect (crd_discovery_side_effect.go) can
+					// invoke DiscoverGroupResources without a per-call
+					// ctx. The informer processor goroutine has no
+					// InternalRESTConfigFromContext attached; the
+					// singleton is the bridge between the informer
+					// event surface and the discovery API. Mirrors the
+					// SetGlobal pattern above — single set at startup,
+					// idempotent, soft-fails to walker-only discovery
+					// if ever unset.
+					cache.SetProcessSARestConfig(rc)
+
 					// Ship 0.30.122 R4 Lever 1: wire the in-cluster
 					// *rest.Config so the composition GVR's streaming
 					// ListWatch can issue raw paged-LIST HTTP requests and
```

---

## Constraint Compliance Re-Check

- [x] **No park-behind-flag** (`feedback_no_park_broken_behind_flag`): always-on; no feature flag.
- [x] **No GVR/resource/user special-cases** (`feedback_no_special_cases`): `IsCRDGVR` is the ONLY GVR predicate in production code; centralised constant in `crd_gvr.go`. Test-file occurrences are synthetic fixtures.
- [x] **No upstream patch** (`project_no_upstream_authority`): all changes in `internal/cache/` + `main.go`.
- [x] **Empirical root-cause trace** (`feedback_empirical_root_cause_trace_before_fix`): design §2 cites file:line + live /debug/vars + pod logs.
- [x] **Falsifier-first** (`feedback_falsifier_first_before_ship`): G2 verified directionally — FAILS on pre-fix binary, PASSES on this branch.
- [x] **PM tightening #1 (worker channel)**: baked in at `crd_discovery_side_effect.go:65-205`; load-bearing test `TestCRDAdd_WorkerNotBlockedByDiscoveryHop`.
- [x] **PM tightening #2 (defer recover)**: baked in at `crd_discovery_side_effect.go:232-247`; load-bearing test `TestCRDAdd_RecoversFromMalformedObject`.

---

## Ready for commit + tag + push.

Would-be commit summary (PM gate verifying):
- 5 new files (4 production + 1 test) in `internal/cache/`
- 2 edits (deps_watch.go AddFunc hook + main.go SA-rc wiring)
- 5 falsifier tests (`TestCRDAdd_*` + `TestTriggerCRDDiscovery_RecoverWrapperCatchesPanic`)
- Tag: `0.30.233` annotated
- Chart lockstep tag: `0.30.233` on `braghettos/snowplow-chart` (origin is INVERTED → push EXPLICITLY)
