// servable.go — Tag 0.30.98 (Tag A of the unified prewarm+pivot-
// servability design): the machinery behind conjuncts 3 and 4 of the
// four-conjunct servability gate.
//
//	servable(gvr) := registered(gvr)        // conjunct 1 — rw.informers
//	  AND HasSynced(gvr)                     // conjunct 2 — client-go latch
//	  AND watchHealthy(gvr)                  // conjunct 3 — this file
//	  AND resourceTypeConfirmed(gvr)         // conjunct 4 — this file (S4 fix)
//
// The predicate itself (servableLocked) lives in watcher.go alongside
// IsServable / ListObjectsServable. This file holds:
//
//   - ResourceTypeDiscovery: the narrow discovery interface conjunct 4
//     depends on (satisfied by k8s.io/client-go discovery.DiscoveryInterface).
//   - SetDiscoveryClient: startup wiring (main.go), mirrors SetMetadataClient.
//   - installWatchErrorHandler: SetWatchErrorHandler wiring for conjunct 3.
//   - RefreshDiscovery + StartDiscoveryRefresher: the ~30s ticker that
//     confirms resource types (flips a post-startup CRD unconfirmed->
//     confirmed) and clears watchBroken on a successful relist.
//
// CRITICAL — feedback_no_special_cases.md: there is ZERO hardcoded
// business-GVR list in this file. The discovery refresh iterates
// rw.informers (the set of registered GVRs) and runs the SAME
// ServerResourcesForGroupVersion check for every one. The ticker does
// NOT register GVRs — it only confirms ones already registered.

package cache

import (
	"context"
	"log/slog"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/informers"
	clientcache "k8s.io/client-go/tools/cache"
)

// discoveryRefreshInterval is the cadence of the conjunct-4 discovery
// ticker. ~30s is the design figure: short enough that a post-startup
// CRD flips confirmed within a single human-perceptible window, long
// enough that the periodic discovery LIST is negligible apiserver load
// (one ServerResourcesForGroupVersion call per registered group/version,
// deduped per refresh).
const discoveryRefreshInterval = 30 * time.Second

// ResourceTypeDiscovery is the narrow discovery surface conjunct 4
// (resourceTypeConfirmed) depends on. k8s.io/client-go's
// discovery.DiscoveryInterface satisfies it directly, so production
// wires a real (cached) discovery client; unit tests inject a double.
//
// ServerResourcesForGroupVersion returns the resources the apiserver
// currently serves for a group/version. A post-startup CRD's
// group/version returns an empty / 404 result until the CRD is
// installed and the apiextensions controller publishes its API — that
// transition is exactly what flips a GVR confirmed.
type ResourceTypeDiscovery interface {
	ServerResourcesForGroupVersion(groupVersion string) (*metav1.APIResourceList, error)
}

// SetDiscoveryClient wires the discovery client used by conjunct 4
// (resourceTypeConfirmed). Called once at startup from main.go AFTER
// NewResourceWatcher succeeds — mirrors SetMetadataClient.
//
// Passing nil clears the client. With no discovery client wired,
// resourceTypeConfirmedLocked returns true (degraded mode: the pivot
// keeps its pre-0.30.98 HasSynced-only behaviour). In production the
// discovery client MUST be wired so a post-startup CRD is correctly
// gated unconfirmed until the apiserver serves it.
//
// Nil-receiver safe (test path under CACHE_ENABLED=false).
func (rw *ResourceWatcher) SetDiscoveryClient(d ResourceTypeDiscovery) {
	if rw == nil {
		return
	}
	rw.mu.Lock()
	rw.disco = d
	rw.mu.Unlock()
}

// installWatchErrorHandler wires the conjunct-3 WATCH-error handler onto
// gi's informer. Callers MUST hold rw.mu.Lock() and MUST call this
// BEFORE Informer().Run — SetWatchErrorHandler returns an error once the
// informer has started.
//
// The closure fires whenever the reflector's ListAndWatch drops its
// connection with an error. It sets rw.watchBroken[gvr] so the next
// servability check falls through to apiserver. The flag is cleared by
// the discovery-refresh ticker once the informer's
// LastSyncResourceVersion advances (a successful relist) — see
// refreshOnceLocked. Rationale for that recovery hook: client-go re-arms
// ListAndWatch automatically after a watch error and the handler stays
// installed; there is no "watch healthy again" callback, so the cleanest
// in-process signal of a recovered relist is an advancing
// LastSyncResourceVersion.
//
// The closure captures gvr by value and references rw — it takes rw.mu
// itself, so it MUST NOT be invoked while the caller still holds the
// lock. client-go invokes it from the reflector goroutine, never under
// rw.mu, so that is safe in production. The FireWatchError test hook
// likewise invokes the closure without holding rw.mu.
func (rw *ResourceWatcher) installWatchErrorHandler(gvr schema.GroupVersionResource, gi informers.GenericInformer) {
	if gi == nil {
		return
	}
	handler := func(_ *clientcache.Reflector, err error) {
		rw.mu.Lock()
		if rw.watchBroken == nil {
			rw.watchBroken = map[schema.GroupVersionResource]struct{}{}
		}
		_, already := rw.watchBroken[gvr]
		rw.watchBroken[gvr] = struct{}{}
		rw.mu.Unlock()
		if !already {
			slog.Warn("cache.watch.broken",
				slog.String("subsystem", "cache"),
				slog.String("gvr", gvr.String()),
				slog.Any("err", err),
				slog.String("effect", "servable=false until reflector relists; pivot falls through to apiserver"),
			)
		}
	}
	if err := gi.Informer().SetWatchErrorHandler(handler); err != nil {
		// Reachable only if the informer already started — which the
		// callers guarantee against (handler installed pre-Run). Log a
		// WARN so the regression is visible without failing the boot.
		slog.Warn("cache.watch.set_error_handler_failed",
			slog.String("subsystem", "cache"),
			slog.String("gvr", gvr.String()),
			slog.String("error", err.Error()),
		)
	}
}

// RefreshDiscovery runs ONE pass of the conjunct-4 discovery check over
// every currently-registered GVR and updates rw.confirmed. It also
// clears watchBroken for any GVR whose LastSyncResourceVersion has
// advanced since the previous pass (a successful relist).
//
// Exposed (not just ticker-internal) so main.go can prime confirmation
// once at startup before the first dispatch, and so the unit tests can
// drive the refresh deterministically. Safe for concurrent use; bounds
// every discovery call by ctx.
//
// Per feedback_no_special_cases.md: the GVR set is rw.informers — there
// is no parallel "prewarm set" and no hardcoded enumeration.
func (rw *ResourceWatcher) RefreshDiscovery(ctx context.Context) {
	if rw == nil || rw.mode == modePassthrough {
		return
	}

	// Snapshot the registered GVRs + the discovery client under the
	// lock; run the (potentially blocking) discovery calls WITHOUT the
	// lock; write results back under the lock.
	rw.mu.RLock()
	disco := rw.disco
	gvrs := make([]schema.GroupVersionResource, 0, len(rw.informers))
	gis := make([]informers.GenericInformer, 0, len(rw.informers))
	for gvr, gi := range rw.informers {
		gvrs = append(gvrs, gvr)
		gis = append(gis, gi)
	}
	rw.mu.RUnlock()

	if len(gvrs) == 0 {
		return
	}

	// Resolve resource-type existence per group/version. Multiple GVRs
	// can share a group/version (e.g. several CompositionDefinition
	// versions) — dedupe so we issue one discovery call per gv.
	type gvKey = string
	served := map[gvKey]bool{}
	if disco != nil {
		queried := map[gvKey]struct{}{}
		for _, gvr := range gvrs {
			if ctx.Err() != nil {
				return
			}
			gv := groupVersionString(gvr)
			if _, done := queried[gv]; done {
				continue
			}
			queried[gv] = struct{}{}
			served[gv] = resourceTypeServed(disco, gvr)
		}
	}

	// Write confirmed + clear watchBroken on advanced RV, under the lock.
	rw.mu.Lock()
	defer rw.mu.Unlock()
	if rw.confirmed == nil {
		rw.confirmed = map[schema.GroupVersionResource]struct{}{}
	}
	if rw.lastSyncRV == nil {
		rw.lastSyncRV = map[schema.GroupVersionResource]string{}
	}
	for i, gvr := range gvrs {
		// Conjunct 4: confirm the resource type. With no discovery
		// client, disco==nil ⇒ resourceTypeConfirmedLocked already
		// returns true, so we leave rw.confirmed untouched here.
		if disco != nil {
			if served[groupVersionString(gvr)] {
				rw.confirmed[gvr] = struct{}{}
			} else {
				// Resource type not served — un-confirm it. This is
				// what gates a post-startup CRD until the apiserver
				// publishes its API, and also correctly retracts a
				// confirmation if a CRD is deleted.
				delete(rw.confirmed, gvr)
			}
		}

		// Conjunct-3 recovery: a successful relist advances the
		// informer's LastSyncResourceVersion. If it advanced since the
		// last refresh AND the informer is currently synced, the WATCH
		// is healthy again — clear watchBroken.
		rv := gis[i].Informer().LastSyncResourceVersion()
		prev := rw.lastSyncRV[gvr]
		if rv != "" && rv != prev && gis[i].Informer().HasSynced() {
			if _, broken := rw.watchBroken[gvr]; broken {
				delete(rw.watchBroken, gvr)
				slog.Info("cache.watch.recovered",
					slog.String("subsystem", "cache"),
					slog.String("gvr", gvr.String()),
					slog.String("reason", "LastSyncResourceVersion advanced — reflector relisted"),
				)
			}
		}
		if rv != "" {
			rw.lastSyncRV[gvr] = rv
		}
	}
}

// resourceTypeServed reports whether the apiserver currently serves
// gvr's resource *type*. It asks discovery for the group/version's
// APIResourceList and checks the Resource name appears. A discovery
// error or an empty list both mean "not served" — the conservative
// direction: an unconfirmed GVR falls through to apiserver, which is
// always safe.
func resourceTypeServed(disco ResourceTypeDiscovery, gvr schema.GroupVersionResource) bool {
	list, err := disco.ServerResourcesForGroupVersion(groupVersionString(gvr))
	if err != nil || list == nil {
		return false
	}
	for _, r := range list.APIResources {
		if r.Name == gvr.Resource {
			return true
		}
	}
	return false
}

// groupVersionString renders gvr's group/version the way discovery keys
// it: "v1" for the core group, "group/version" otherwise.
func groupVersionString(gvr schema.GroupVersionResource) string {
	if gvr.Group == "" {
		return gvr.Version
	}
	return gvr.Group + "/" + gvr.Version
}

// StartDiscoveryRefresher launches the conjunct-4 discovery-refresh
// ticker goroutine. It runs RefreshDiscovery every discoveryRefreshInterval
// until ctx is cancelled or the watcher is Stop()ed — whichever fires
// first. Idempotent-safe to call once; main.go owns the single call.
//
// The goroutine's lifecycle is bound by BOTH ctx and rw.stopCh, so it is
// reliably reaped (no NEVER-use-go-func-without-lifecycle violation).
//
// In modePassthrough there is no informer to confirm — the goroutine is
// not spawned.
func (rw *ResourceWatcher) StartDiscoveryRefresher(ctx context.Context) {
	if rw == nil || rw.mode == modePassthrough {
		return
	}
	go func() {
		// Prime confirmation once immediately so the first dispatch
		// after startup does not wait a full interval.
		rw.RefreshDiscovery(ctx)

		t := time.NewTicker(discoveryRefreshInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-rw.stopCh:
				return
			case <-t.C:
				rw.RefreshDiscovery(ctx)
			}
		}
	}()
}

// --- Test hooks -----------------------------------------------------------
//
// FireWatchError / MarkWatchRecovered drive conjunct 3 deterministically
// from unit tests. A fake dynamic client never drops a real WATCH, so
// these exercise the SAME state transitions the production handler and
// the discovery-ticker recovery path perform — they do not bypass the
// gate, they trigger it.

// FireWatchError simulates the reflector dropping gvr's WATCH connection
// — the exact effect of the production SetWatchErrorHandler closure.
// After this call, servableLocked's conjunct 3 fails for gvr.
func (rw *ResourceWatcher) FireWatchError(gvr schema.GroupVersionResource) {
	if rw == nil {
		return
	}
	rw.mu.Lock()
	if rw.watchBroken == nil {
		rw.watchBroken = map[schema.GroupVersionResource]struct{}{}
	}
	rw.watchBroken[gvr] = struct{}{}
	rw.mu.Unlock()
}

// MarkWatchRecovered simulates a successful relist clearing gvr's broken
// WATCH — the exact effect of the discovery-ticker recovery branch in
// RefreshDiscovery. After this call, servableLocked's conjunct 3 holds
// again for gvr (assuming the other three conjuncts hold).
func (rw *ResourceWatcher) MarkWatchRecovered(gvr schema.GroupVersionResource) {
	if rw == nil {
		return
	}
	rw.mu.Lock()
	delete(rw.watchBroken, gvr)
	rw.mu.Unlock()
}
