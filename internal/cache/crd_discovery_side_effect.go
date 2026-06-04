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

// crdDiscoveryEvent is one queued CRD-ADD event handed to the
// discovery worker. The bridge captures the *unstructured.Unstructured
// at enqueue time so the worker reads spec.group from the snapshot
// the informer delivered — no late reads against a mutating store.
type crdDiscoveryEvent struct {
	obj interface{} // *unstructured.Unstructured (or bytesObject — see decode below)
}

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
// processed counter then dispatches to triggerCRDDiscovery — the
// recover wrapper (PM tightening #2) is INSIDE triggerCRDDiscovery
// so a single bad CRD object cannot crash the worker.
func (c *crdDiscovery) processEvent(ev crdDiscoveryEvent) {
	c.eventsProcessed.Add(1)
	triggerCRDDiscovery(ev.obj)
}

// submitCRDDiscoveryEvent enqueues a CRD-ADD event onto the
// worker queue. Non-blocking with bounded buffer. Called from the
// deps_watch.go AddFunc when IsCRDGVR(gvr) is true.
//
// On full queue: drop + WARN + counter bump. DiscoverGroupResources
// is per-group singleflighted; a dropped event for an in-flight
// group is harmless. A dropped event for a NEW group means the
// next event for that group (or the next walker pass) retries.
func (c *crdDiscovery) submitCRDDiscoveryEvent(obj interface{}) {
	c.startCRDDiscoveryWorker()
	select {
	case c.queue <- crdDiscoveryEvent{obj: obj}:
		c.eventsEnqueued.Add(1)
	default:
		c.eventsDropped.Add(1)
		slog.Warn("cache.crd_discovery.event_dropped",
			slog.String("subsystem", "cache"),
			slog.String("hint", "CRD-ADD burst outran the discovery worker — "+
				"DiscoverGroupResources is singleflighted per-group so a duplicate "+
				"for an in-flight group is harmless; a new group will be retried "+
				"on the next CRD ADD or walker pass."),
		)
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
//   - The CRD object is *unstructured.Unstructured (the dynamic
//     informer's standard delivery shape). bytesObject / other
//     shapes soft-skip (we expect Unstructured for the CRD GVR; a
//     future routing change would surface here via skip counter).
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
func triggerCRDDiscovery(obj interface{}) {
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
				slog.Any("panic", rec),
				slog.String("stack", string(debug.Stack())),
				slog.String("hint", "triggerCRDDiscovery panicked on a CRD object — "+
					"continuing (the worker stays alive). Inspect the stack trace "+
					"to identify the malformed CRD shape."),
			)
		}
	}()

	u, ok := obj.(*unstructured.Unstructured)
	if !ok || u == nil {
		// bytesObject / PartialObjectMetadata / nil — the CRD
		// informer is dynamic-full-Unstructured by construction
		// (see watcher.go addResourceTypeLocked path). A non-
		// Unstructured arrival means a routing change that future
		// editors should be aware of.
		c.discoverySkippedNG.Add(1)
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
