// crd_discovery_side_effect.go — Ship 0.30.233. The CRD-ADD
// discovery side-effect bridge.
//
// PURPOSE — restore the pre-Ship-0.5 invariant "a CRD ADD event
// drives discovery for the new CRD's group", in a SIMPLER form
// than the deleted CRD-watch backplane: ONE side-effect hook on
// the EXISTING customresourcedefinitions informer's AddFunc, NOT a
// separate informer.
//
// PRE-Ship-0.5 — discovery was driven by an in-process CRD
// informer + an AddFunc that called OnResourceTypeAvailable +
// register-resource-type. Ship 0.5 / 0.30.223 (v6) deleted that
// informer and routed discovery through the walker
// (lazyRegisterInnerCallPaths → DiscoverGroupResources). The TRACE
// in docs/ship-0.30.233-s4-cache-invalidation-trace-2026-06-02.md
// proved the walker-only chain has a stuck-zero-state race when a
// CRD is created at runtime: stage 1 of compositions-list serves
// the cached `crds` LIST result (which doesn't yet include the new
// CRD), stage 2 iterator is empty, the discovery hop is never
// reached for the new group, and the composition informer is
// never registered.
//
// Ship 0.30.233 fixes this by handing every CRD-ADD event to a
// bounded worker channel that calls cache.AddNavigationDiscoveredGroup
// + cache.DiscoverGroupResources for the new CRD's spec.group on a
// dedicated goroutine — OFF the informer processor goroutine.
//
// PM TIGHTENING #1 — bounded worker channel (NOT inline).
// DiscoverGroupResources does network hops (disco.ServerGroups +
// disco.ServerResourcesForGroupVersion); running it on the
// informer processor goroutine would stall ADD delivery for every
// other informer sharing that processor during the discovery hop
// (~tens of ms × N versions). The pattern mirrors deps_watch.go's
// existing deleteEvictCh — single bounded worker, drop-on-full
// with WARN log.
//
// PM TIGHTENING #2 — defer recover() inside triggerCRDDiscovery.
// The informer processor goroutine (or worker goroutine here)
// must NEVER panic-kill the pod under a malformed CRD object.
// The recover wrapper logs at error level with debug.Stack() so a
// regression surfaces in pod logs without taking the pod down.

package cache

import (
	"context"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// crdDiscoveryQueueDepth bounds the worker channel. 256 buffered
// slots is ample headroom for a realistic CRD-CREATE burst (a
// blueprint install creates ~10-30 CRDs in a few hundred ms; 256
// covers the largest customer scenario observed). A full channel
// falls back to drop-with-WARN — DiscoverGroupResources is per-
// group singleflighted and idempotent, so a dropped event for a
// group ALREADY being discovered is harmless; a dropped event for
// a NEW group means a delayed discovery (the next CRD ADD for
// that group, or the next walker pass, will retry).
const crdDiscoveryQueueDepth = 256

// crdDiscoveryEvent is one queued CRD-lifecycle event handed to the
// discovery worker. The bridge captures the informer-delivered object
// at enqueue time so the worker reads spec.* from the snapshot the
// informer delivered — no late reads against a mutating store.
type crdDiscoveryEvent struct {
	// obj is the CRD event payload. Production delivery shape post-Ship-H5
	// is *bytesObject (streaming-listwatch is the default for every dynamic
	// informer per watcher.go:1035-1047). The stock-informer fallback path
	// delivers *unstructured.Unstructured. triggerCRDDiscovery + the §3b
	// DELETE handler route both through decodeBytesObject
	// (bytesobject.go:394) — see Ship L / 0.30.246. DELETE events may
	// arrive wrapped in clientcache.DeletedFinalStateUnknown; the AddFunc/
	// UpdateFunc/DeleteFunc literal bodies in deps_watch.go unwrap that
	// shape BEFORE handing obj to submitCRDLifecycleEvent.
	obj interface{}
	// kind discriminates ADD vs UPDATE vs DELETE so the worker dispatches
	// to the right side-effect (Ship L / 0.30.246). UPDATE re-runs
	// discovery against the new spec; DELETE tears down the per-resource
	// informer + dirty-marks dependent L1.
	kind crdLifecycleKind
}

// crdLifecycleKind discriminates the three CRD events the bridge handles
// (Ship L / 0.30.246).
type crdLifecycleKind uint8

const (
	crdLifecycleAdd    crdLifecycleKind = iota // CRD CREATE
	crdLifecycleUpdate                         // CRD UPDATE (spec.versions[] / served[] / group changes)
	crdLifecycleDelete                         // CRD DELETE (group no longer served by the apiserver)
)

// crdDiscovery is the process-scoped CRD-ADD-side-effect bridge:
// counters + the worker channel + the worker-goroutine lifecycle.
// Mirrors depWatch (deps_watch.go) — sibling pattern, distinct
// state to keep the falsifier surface auditable.
type crdDiscovery struct {
	// queue is the bounded ADD-event channel. Drained by exactly
	// one worker goroutine spawned via startOnce.
	queue chan crdDiscoveryEvent

	startOnce sync.Once
	stopCh    chan struct{}
	workerWG  sync.WaitGroup

	// Counters — observability for the falsifier + ops dashboards.
	// All atomic for lock-free reads.
	eventsEnqueued     atomic.Uint64 // lifecycle events accepted into the queue (ADD + UPDATE + DELETE)
	eventsDropped      atomic.Uint64 // lifecycle events dropped (queue full)
	eventsProcessed    atomic.Uint64 // lifecycle events drained by the worker
	discoveryInvoked   atomic.Uint64 // ADD+UPDATE calls that reached DiscoverGroupResources
	discoverySkippedNG atomic.Uint64 // ADD+UPDATE calls skipped (no group / decode-fail / no SA rc)
	deletesProcessed   atomic.Uint64 // Ship L — DELETE calls that completed teardown (>=1 GVR torn down)
	deleteSkippedNG    atomic.Uint64 // Ship L — DELETE calls skipped (decode-fail / no plural / no served versions)
	panicsRecovered    atomic.Uint64 // recover-wrapper panic catches across all lifecycle handlers
}

var (
	crdDiscoveryInstance *crdDiscovery
	crdDiscoveryOnce     sync.Once
)

// crdDiscoverySingleton returns the process-scoped bridge, lazily
// constructing it on first access. Always non-nil.
func crdDiscoverySingleton() *crdDiscovery {
	crdDiscoveryOnce.Do(func() {
		crdDiscoveryInstance = &crdDiscovery{
			queue:  make(chan crdDiscoveryEvent, crdDiscoveryQueueDepth),
			stopCh: make(chan struct{}),
		}
	})
	return crdDiscoveryInstance
}

// startCRDDiscoveryWorker spawns the single worker goroutine
// exactly once (sync.Once-bounded). The worker drains the queue
// and invokes triggerCRDDiscovery per event OFF the informer
// processor goroutine. It exits on stopCh close (test cleanup);
// production never stops it — its lifetime is the process
// lifetime.
func (c *crdDiscovery) startCRDDiscoveryWorker() {
	c.startOnce.Do(func() {
		c.workerWG.Add(1)
		go func() {
			defer c.workerWG.Done()
			defer func() {
				if rec := recover(); rec != nil {
					slog.Error("cache.crd_discovery.worker.panic",
						slog.String("subsystem", "cache"),
						slog.Any("panic", rec),
						slog.String("stack", string(debug.Stack())),
					)
				}
			}()
			for {
				select {
				case <-c.stopCh:
					// Drain queued events before exit so test
					// teardown is deterministic.
					for {
						select {
						case ev := <-c.queue:
							c.processEvent(ev)
						default:
							return
						}
					}
				case ev := <-c.queue:
					c.processEvent(ev)
				}
			}
		}()
	})
}

// processEvent is the worker's per-event entry point. Bumps the
// processed counter then dispatches on the event kind. The recover
// wrappers (PM tightening #2) are INSIDE each trigger* function so a
// single bad CRD object cannot crash the worker.
//
// Ship L (0.30.246): dispatches on crdLifecycleKind. ADD + UPDATE
// share the discovery code path (triggerCRDDiscovery); DELETE branches
// to triggerCRDDelete (wired in commit-4).
func (c *crdDiscovery) processEvent(ev crdDiscoveryEvent) {
	switch ev.kind {
	case crdLifecycleAdd, crdLifecycleUpdate:
		triggerCRDDiscovery(ev.obj, ev.kind)
	case crdLifecycleDelete:
		triggerCRDDelete(ev.obj)
	default:
		// Ship L review note (#200) — defensive default for the closed
		// crdLifecycleKind enum. submitCRDLifecycleEvent is the only
		// enqueue site and always passes a named constant, so an unknown
		// kind here is structurally unreachable today. The default makes
		// that contract explicit: an out-of-range kind from a future
		// mis-wiring is logged (not silently no-op'd) while the worker
		// stays alive and the event still counts as processed below. No
		// side-effect fires — there is no safe default action for an
		// unknown lifecycle event.
		slog.Error("cache.crd_discovery.unknown_kind",
			slog.String("subsystem", "cache"),
			slog.String("kind", crdLifecycleKindString(ev.kind)),
			slog.String("hint", "processEvent received an out-of-range crdLifecycleKind — "+
				"submitCRDLifecycleEvent must always enqueue a named constant. "+
				"This indicates a wiring regression; the worker continues."),
		)
	}
	// Task #85: bump eventsProcessed AFTER the side-effect (discovery /
	// delete) completes, not before. "Processed" must mean "the
	// side-effect has run" — so eventsProcessed is a valid happens-before
	// signal for the side-effect's observable state (e.g.
	// navDiscoveredGroups via AddNavigationDiscoveredGroup at
	// triggerCRDDiscovery -> discovery_lookup.go:352). Pre-0.30.252 the
	// bump preceded the dispatch, so WaitCRDDiscoveryProcessedForTest
	// (which polls eventsProcessed) could return BEFORE
	// AddNavigationDiscoveredGroup ran, racing the assertions in
	// TestCRDAdd_TriggersGroupDiscovery (and siblings) under -count load.
	// The final per-event count is unchanged (still exactly one Add per
	// event), so every EventsProcessed==N assertion still holds; the only
	// difference is a sub-microsecond window where a dequeued event is not
	// yet counted — benign for the /debug/vars `events_processed` gauge,
	// and the gauge now reports fully-processed events, which is the more
	// honest semantics.
	c.eventsProcessed.Add(1)
}

// submitCRDLifecycleEvent enqueues a CRD lifecycle event (ADD / UPDATE
// / DELETE) onto the worker queue. Non-blocking with bounded buffer.
// Called from the deps_watch.go AddFunc/UpdateFunc/DeleteFunc when
// IsCRDGVR(gvr) is true.
//
// On full queue: drop + WARN + counter bump. DiscoverGroupResources
// is per-group singleflighted; a dropped event for an in-flight
// group is harmless. A dropped event for a NEW group means the
// next event for that group (or the next walker pass) retries.
//
// Ship L (0.30.246) — renamed from submitCRDDiscoveryEvent. The kind
// parameter lets the worker dispatch to ADD/UPDATE/DELETE paths.
func (c *crdDiscovery) submitCRDLifecycleEvent(obj interface{}, kind crdLifecycleKind) {
	c.startCRDDiscoveryWorker()
	select {
	case c.queue <- crdDiscoveryEvent{obj: obj, kind: kind}:
		c.eventsEnqueued.Add(1)
	default:
		c.eventsDropped.Add(1)
		slog.Warn("cache.crd_discovery.event_dropped",
			slog.String("subsystem", "cache"),
			slog.String("kind", crdLifecycleKindString(kind)),
			slog.String("hint", "CRD lifecycle burst outran the discovery worker — "+
				"DiscoverGroupResources is singleflighted per-group so a duplicate "+
				"for an in-flight group is harmless; a new group will be retried "+
				"on the next CRD event or walker pass."),
		)
	}
}

// crdLifecycleKindString renders the enum as a human-readable label for
// log lines + WARNs. Closed-set; default falls back to "unknown" rather
// than panicking on an invalid value.
func crdLifecycleKindString(k crdLifecycleKind) string {
	switch k {
	case crdLifecycleAdd:
		return "ADD"
	case crdLifecycleUpdate:
		return "UPDATE"
	case crdLifecycleDelete:
		return "DELETE"
	default:
		return "unknown"
	}
}

// stopCRDDiscoveryWorker closes the worker stop channel and blocks
// until the worker goroutine has exited (and drained pending
// events). Used by the _test.go shim; production code MUST NOT
// call it.
func (c *crdDiscovery) stopCRDDiscoveryWorker() {
	select {
	case <-c.stopCh:
		// already stopped
	default:
		close(c.stopCh)
	}
	c.workerWG.Wait()
}

// triggerCRDDiscovery is the actual side-effect: extract spec.group
// from the CRD object, add it to the navigation-discovered set,
// and invoke DiscoverGroupResources. Soft-fails on every error
// path — the recover wrapper (PM tightening #2) catches panics so
// a malformed CRD cannot kill the worker / pod.
//
// Identity invariants:
//   - The SA *rest.Config comes from ProcessSARestConfig (set once
//     at main.go startup). nil → soft-skip + counter bump.
//   - The CRD object decodes via decodeBytesObject (bytesobject.go:394).
//     Production delivery shape post-Ship-H5 is *bytesObject (streaming-
//     listwatch is the default per watcher.go:1035-1047); the stock-
//     informer fallback delivers *unstructured.Unstructured. Anything
//     else (PartialObjectMetadata, nil, etc.) → soft-skip.
//   - spec.group is read via unstructured.NestedString — empty /
//     missing / non-string soft-skips.
//
// ASYNC — runs on the discovery worker goroutine, NOT the informer
// processor. DiscoverGroupResources blocks on the apiserver
// discovery hop (~tens of ms); the worker queues serialize CRD
// events so concurrent CRD ADDs do not parallelise discovery hops
// (singleflight inside DiscoverGroupResources serialises per-group
// anyway; the worker queue serialises across groups too, which is
// fine for the realistic CRD-CREATE burst rate).
//
// Ship L (0.30.246): handles both crdLifecycleAdd and crdLifecycleUpdate.
// AddNavigationDiscoveredGroup is idempotent (discovery_lookup.go:87-102);
// DiscoverGroupResources is per-group singleflighted, so UPDATE re-firing
// is cheap when nothing has changed.
func triggerCRDDiscovery(obj interface{}, kind crdLifecycleKind) {
	c := crdDiscoverySingleton()

	// PM TIGHTENING #2: panic-recover wrapper. The informer
	// processor goroutine (via the worker) must never panic-kill
	// the pod under a malformed CRD object. Logs at error level
	// with debug.Stack() so a regression is visible in pod logs.
	defer func() {
		if rec := recover(); rec != nil {
			c.panicsRecovered.Add(1)
			slog.Error("cache.crd_discovery.trigger.panic_recovered",
				slog.String("subsystem", "cache"),
				slog.String("kind", crdLifecycleKindString(kind)),
				slog.Any("panic", rec),
				slog.String("stack", string(debug.Stack())),
				slog.String("hint", "triggerCRDDiscovery panicked on a CRD object — "+
					"continuing (the worker stays alive). Inspect the stack trace "+
					"to identify the malformed CRD shape."),
			)
		}
	}()

	// Ship L / 0.30.246 — decode-on-access. Post Ship H5 the
	// streaming-listwatch is the default for every dynamic informer
	// (watcher.go:1035-1047), so the customresourcedefinitions
	// informer delivers *bytesObject here, NOT
	// *unstructured.Unstructured. decodeBytesObject is the established
	// H5-aware decode dance: *bytesObject → fresh Unstructured via
	// .Decode(); *unstructured.Unstructured → returned as-is. Anything
	// else (PartialObjectMetadata, nil, etc.) → (nil, false) and we
	// soft-skip + one-shot WARN per unique go-type.
	u, ok := decodeBytesObject(obj)
	if !ok || u == nil {
		c.discoverySkippedNG.Add(1)
		warnOnceCRDDecodeSkip(obj, kind)
		return
	}

	group, found, err := unstructured.NestedString(u.Object, "spec", "group")
	if err != nil || !found || group == "" {
		c.discoverySkippedNG.Add(1)
		return
	}

	saRC := ProcessSARestConfig()
	if saRC == nil {
		c.discoverySkippedNG.Add(1)
		slog.Warn("cache.crd_discovery.no_sa_rc",
			slog.String("subsystem", "cache"),
			slog.String("group", group),
			slog.String("hint", "SetProcessSARestConfig was not called at startup — "+
				"CRD-ADD discovery is degraded to walker-only. Check main.go wiring."),
		)
		return
	}

	c.discoveryInvoked.Add(1)

	// Add to navigation-discovered set FIRST so the watcher's
	// removable-discriminator (watcher.go:749/:1064) sees the
	// group as nav-discovered when EnsureResourceType inside
	// DiscoverGroupResources spawns the composition GVR informer.
	AddNavigationDiscoveredGroup(group)

	// Fire-and-forget discovery hop. DiscoverGroupResources is
	// per-group singleflighted (discovery_lookup.go:228-232) and
	// idempotent (EnsureResourceType is itself singleflighted via
	// rw.mu). Soft-fails on apiserver errors (warn-logged inside
	// DiscoverGroupResources at discovery_lookup.go:255-258 +
	// :270-275).
	ctx := context.Background()
	if _, derr := DiscoverGroupResources(ctx, saRC, group); derr != nil {
		slog.Warn("cache.crd_discovery.discover_group_failed",
			slog.String("subsystem", "cache"),
			slog.String("group", group),
			slog.Any("err", derr),
		)
	}

	// Task #322 (#318-R2) Commit 1 — invalidate the SA-singleton cached
	// discovery client AFTER DiscoverGroupResources, so the next
	// ValidateObjectStatus for the new/changed GVR rebuilds the mapper
	// and sees the new CRD's schema. STRICTLY ordered after discovery
	// (F-4 safety): a stale discovery cache cannot persist past this CRD
	// ADD/UPDATE. Soft no-op when the dynamic singleton is unwired
	// (discovery_invalidation_hook.go).
	invalidateSADiscovery()

	// Task #323 (#318-R2 Commit 2-B) — reset the per-GVR compiled-CRD-schema
	// memo (crds/schema) in lockstep with the discovery cache, AFTER
	// DiscoverGroupResources, so the next ValidateObjectStatus for the
	// new/changed GVR recompiles from fresh CRD bytes (a CRD UPDATE that
	// changes the schema MUST invalidate; this is that path). Soft no-op when
	// the schema-memo invalidator is unwired (discovery_invalidation_hook.go).
	invalidateCRDSchemaMemo()
}

// triggerCRDDelete handles a CRD DELETE event: derive the GVRs that
// were served, tear down each per-resource informer via
// RemoveResourceType, and dirty-mark dependent L1 entries via
// OnResourceTypeRemoved (Ship L / 0.30.246, spec §3b).
//
// IDENTITY INVARIANTS — same shape as triggerCRDDiscovery:
//   - obj is decoded via decodeBytesObject (H5-aware). Production
//     delivery shape is *bytesObject; stock fallback is
//     *unstructured.Unstructured. Anything else soft-skips + WARN.
//   - spec.group + spec.names.plural + each served spec.versions[].name
//     produce the GVRs that need teardown — same derivation as
//     cache_mode.go:310-321.
//   - RemoveResourceType is idempotent (watcher.go:1292): unknown GVR
//     is a no-op. Safe under double-fire (DELETE storm).
//   - OnResourceTypeRemoved is no-op-on-empty (deps.go:716-722).
//
// FAILURE MODES (see spec §10.4):
//   - decodeBytesObject fails -> soft-skip + WARN. The informer keeps
//     running until it WATCH-404s and the controller-health snapshot
//     re-establishes via OnResourceTypeRemoved on the next sync.
//   - spec.versions[] is empty -> no GVR to tear down. Counter + skip.
//   - cache.Global() returns nil (test path) -> RemoveResourceType is
//     itself nil-receiver-safe (watcher.go:1318).
//
// NOTE: navDiscoveredGroups stays APPEND-ONLY on DELETE (per OQ1
// ratified — spec §11.2). The set's removable-discriminator predicate
// at watcher.go:749/:1074 is dead-code under the H5 streaming default
// in production, but the append-only contract keeps the contract
// surface bounded. Ship L+1 will retire the dead predicate use under
// task #196.
func triggerCRDDelete(obj interface{}) {
	c := crdDiscoverySingleton()

	defer func() {
		if rec := recover(); rec != nil {
			c.panicsRecovered.Add(1)
			slog.Error("cache.crd_discovery.delete.panic_recovered",
				slog.String("subsystem", "cache"),
				slog.Any("panic", rec),
				slog.String("stack", string(debug.Stack())),
				slog.String("hint", "triggerCRDDelete panicked on a CRD object — "+
					"continuing (the worker stays alive). Inspect the stack trace "+
					"to identify the malformed CRD shape."),
			)
		}
	}()

	u, ok := decodeBytesObject(obj)
	if !ok || u == nil {
		c.deleteSkippedNG.Add(1)
		warnOnceCRDDecodeSkip(obj, crdLifecycleDelete)
		return
	}

	group, _, _ := unstructured.NestedString(u.Object, "spec", "group")
	plural, _, _ := unstructured.NestedString(u.Object, "spec", "names", "plural")
	if group == "" || plural == "" {
		c.deleteSkippedNG.Add(1)
		slog.Warn("cache.crd_discovery.delete.no_group_or_plural",
			slog.String("subsystem", "cache"),
			slog.String("group", group),
			slog.String("plural", plural),
			slog.String("hint", "CRD DELETE event has empty spec.group or "+
				"spec.names.plural — cannot derive GVRs to tear down."),
		)
		return
	}

	versions, found, vErr := unstructured.NestedSlice(u.Object, "spec", "versions")
	if vErr != nil || !found || len(versions) == 0 {
		c.deleteSkippedNG.Add(1)
		slog.Warn("cache.crd_discovery.delete.no_versions",
			slog.String("subsystem", "cache"),
			slog.String("group", group),
			slog.String("plural", plural),
			slog.String("hint", "CRD DELETE event has empty spec.versions[] — "+
				"nothing to tear down."),
		)
		return
	}

	rw := Global()
	torn := 0
	for _, v := range versions {
		vm, vok := v.(map[string]any)
		if !vok {
			continue
		}
		name, _, _ := unstructured.NestedString(vm, "name")
		served, _, _ := unstructured.NestedBool(vm, "served")
		if name == "" || !served {
			// Not-served versions had no informer wired (per
			// cache_mode.go:312-316); nothing to tear down.
			continue
		}
		gvr := schema.GroupVersionResource{
			Group:    group,
			Version:  name,
			Resource: plural,
		}
		if rw != nil {
			rw.RemoveResourceType(gvr) // idempotent, nil-safe
		}
		Deps().OnResourceTypeRemoved(gvr) // dirty-mark dependent L1
		torn++
	}

	c.deletesProcessed.Add(1)
	slog.Info("cache.crd_discovery.delete.processed",
		slog.String("subsystem", "cache"),
		slog.String("group", group),
		slog.String("plural", plural),
		slog.Int("gvrs_torn_down", torn),
	)

	// Task #322 (#318-R2) Commit 1 — invalidate the SA-singleton cached
	// discovery client AFTER teardown (RemoveResourceType +
	// OnResourceTypeRemoved), so the next ValidateObjectStatus for a
	// just-deleted GVR rebuilds the mapper WITHOUT the removed GVR
	// (KindFor then misses -> error returned to the caller, not a stale
	// positive). STRICTLY ordered after teardown (F-4 safety). Soft
	// no-op when the dynamic singleton is unwired
	// (discovery_invalidation_hook.go).
	invalidateSADiscovery()

	// Task #323 (#318-R2 Commit 2-B) — reset the per-GVR compiled-CRD-schema
	// memo (crds/schema) in lockstep, AFTER teardown, so the next
	// ValidateObjectStatus for a just-deleted GVR recompiles (and then misses
	// at the CRD GET -> error to the caller, not a stale-positive schema).
	// Soft no-op when the schema-memo invalidator is unwired.
	invalidateCRDSchemaMemo()

	// NOTE: navDiscoveredGroups stays append-only. See doc-comment
	// above + spec §11.2 OQ1 worked-examples deep-dive — the
	// predicate's "remove on DELETE" hazard is documentation-preserved
	// but dead-code under H5 streaming default. Ship L+1 / task #196
	// addresses the dead predicate.
}

// warnOnceCRDDecodeSkip emits a single WARN per unique go-type observed
// at the decode-skip path. Bounded by a sync.Map keyed on the type name
// so log volume is bounded by the number of distinct delivery shapes
// (1-2 in practice). Ship L / 0.30.246.
//
// The silent-skip behaviour of the pre-Ship-L bridge is what hid the
// 0.30.233 bytesObject regression for 13 ships. This WARN is the
// observability surface so any future routing change surfaces in pod
// logs the moment the new shape is observed — not 5 months later in a
// bench failure.
var crdDecodeSkipWarnedTypes sync.Map // map[string]struct{}

func warnOnceCRDDecodeSkip(obj interface{}, kind crdLifecycleKind) {
	typeName := goTypeOf(obj)
	if _, loaded := crdDecodeSkipWarnedTypes.LoadOrStore(typeName, struct{}{}); loaded {
		return
	}
	slog.Warn("cache.crd_discovery.decode_skipped",
		slog.String("subsystem", "cache"),
		slog.String("kind", crdLifecycleKindString(kind)),
		slog.String("got_type", typeName),
		slog.String("hint", "CRD lifecycle event arrived in an undecodable shape — "+
			"decodeBytesObject returned (nil,false). If got_type is *bytesObject, "+
			"the raw bytes are malformed (rare); otherwise decodeBytesObject "+
			"(bytesobject.go:394) needs a new case for this shape."),
	)
}

// CRDDiscoveryStats is a read-only snapshot of the CRD-discovery
// bridge counters. Consumed by the Ship 0.30.233 falsifier and the
// /debug/vars surface (followup #143).
//
// Ship L (0.30.246) added DeletesProcessed + DeleteSkippedNG for the
// CRD DELETE lifecycle path. Both fields stay zero in test fixtures
// that exercise only the ADD/UPDATE paths.
type CRDDiscoveryStats struct {
	EventsEnqueued     uint64
	EventsDropped      uint64
	EventsProcessed    uint64
	DiscoveryInvoked   uint64 // ADD + UPDATE (DiscoverGroupResources calls)
	DiscoverySkippedNG uint64 // ADD + UPDATE decode-skip / no-group / no-SA-rc
	DeletesProcessed   uint64 // Ship L — successful DELETE teardowns
	DeleteSkippedNG    uint64 // Ship L — DELETE decode-skip / no-served-versions / no-plural
	PanicsRecovered    uint64
}

// CRDDiscoveryStatsSnapshot returns the current bridge counters.
func CRDDiscoveryStatsSnapshot() CRDDiscoveryStats {
	c := crdDiscoverySingleton()
	return CRDDiscoveryStats{
		EventsEnqueued:     c.eventsEnqueued.Load(),
		EventsDropped:      c.eventsDropped.Load(),
		EventsProcessed:    c.eventsProcessed.Load(),
		DiscoveryInvoked:   c.discoveryInvoked.Load(),
		DiscoverySkippedNG: c.discoverySkippedNG.Load(),
		DeletesProcessed:   c.deletesProcessed.Load(),
		DeleteSkippedNG:    c.deleteSkippedNG.Load(),
		PanicsRecovered:    c.panicsRecovered.Load(),
	}
}

// resetCRDDiscoveryForTest tears the singleton down so each test
// starts clean (counters zeroed, worker stopped). TEST-ONLY.
func resetCRDDiscoveryForTest() {
	if crdDiscoveryInstance != nil {
		crdDiscoveryInstance.stopCRDDiscoveryWorker()
	}
	crdDiscoveryInstance = nil
	crdDiscoveryOnce = sync.Once{}
}

// WaitCRDDiscoveryProcessedForTest blocks until at least `n`
// events have been processed by the worker, or `pollTimeoutMs`
// elapses. TEST-ONLY helper for the falsifier — the worker is
// async so the test cannot assert post-AddFunc state synchronously.
//
// Returns true on success, false on timeout.
func WaitCRDDiscoveryProcessedForTest(n uint64, pollTimeoutMs int) bool {
	c := crdDiscoverySingleton()
	deadline := time.Now().Add(time.Duration(pollTimeoutMs) * time.Millisecond)
	for time.Now().Before(deadline) {
		if c.eventsProcessed.Load() >= n {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return c.eventsProcessed.Load() >= n
}
