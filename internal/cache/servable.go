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
		return
	}
	// 0.30.99 Tag B — watch-handler coverage guard. Record the
	// successful install so assertWatchHandlerCoverageLocked (run from
	// the constructor's terminal block) can verify every registered GVR
	// has a handler. Caller holds rw.mu, so this write is safe.
	if rw.watchHandlerInstalled == nil {
		rw.watchHandlerInstalled = map[schema.GroupVersionResource]struct{}{}
	}
	rw.watchHandlerInstalled[gvr] = struct{}{}
}

// assertWatchHandlerCoverageLocked verifies the 0.30.99 Tag B invariant:
// every GVR in rw.informers has had its conjunct-3 WATCH-error handler
// installed (recorded in rw.watchHandlerInstalled). Callers MUST hold
// rw.mu.
//
// Why a guard at all (architect's Tag B note): Tag A wires
// SetWatchErrorHandler on the three informer-creation paths, but its
// constructor install-loop only covers informers PRESENT at construction
// (the RBAC bootstrap set). A future pre-Start lazy-register path that
// routes around the constructor loop — e.g. if EnsureResourceType were
// ever invoked before NewResourceWatcher calls factory.Start — would
// register an informer the loop never sees. Without conjunct 3 that
// informer's pivot reads could serve a stale store after a dropped
// WATCH.
//
// 0.30.99 closes the gap STRUCTURALLY: addResourceTypeLocked now calls
// installWatchErrorHandler unconditionally at registration time (not
// only in the post-Start branch); addResourceTypeMetadataOnlyLocked
// already did. This assertion is the belt-and-braces check that the
// structural fix held — it runs once at startup and logs a loud WARN
// naming any GVR that slipped through, so a regression that reintroduces
// a coverage gap is visible at every boot rather than only under a
// dropped-WATCH incident.
//
// Logged-assertion (not panic): a missing handler degrades conjunct 3
// for one GVR but does not corrupt state — failing the boot would be a
// disproportionate response. The WARN is the SRE signal.
func (rw *ResourceWatcher) assertWatchHandlerCoverageLocked() {
	var missing []string
	for gvr := range rw.informers {
		if _, ok := rw.watchHandlerInstalled[gvr]; !ok {
			missing = append(missing, gvr.String())
		}
	}
	if len(missing) > 0 {
		slog.Warn("cache.watch.handler_coverage_gap",
			slog.String("subsystem", "cache"),
			slog.Int("missing_count", len(missing)),
			slog.Any("missing_gvrs", missing),
			slog.String("effect", "conjunct 3 (watchHealthy) cannot fire for these GVRs — a dropped WATCH would not gate the pivot"),
			slog.String("hint", "a registration path bypassed installWatchErrorHandler — audit addResourceTypeLocked / addResourceTypeMetadataOnlyLocked"),
		)
		return
	}
	slog.Info("cache.watch.handler_coverage_ok",
		slog.String("subsystem", "cache"),
		slog.Int("informers", len(rw.informers)),
		slog.String("invariant", "every registered GVR has a WATCH-error handler (conjunct 3)"),
	)
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
	rw.ensureConfirmMapsLocked()
	for i, gvr := range gvrs {
		rw.applyConfirmLocked(gvr, gis[i], disco != nil, served[groupVersionString(gvr)])
	}
}

// ensureConfirmMapsLocked lazily allocates the conjunct-3/4 state maps.
// Callers MUST hold rw.mu.Lock(). Shared by RefreshDiscovery and the
// scoped ConfirmResourceType so both write through the SAME maps.
func (rw *ResourceWatcher) ensureConfirmMapsLocked() {
	if rw.confirmed == nil {
		rw.confirmed = map[schema.GroupVersionResource]struct{}{}
	}
	if rw.lastSyncRV == nil {
		rw.lastSyncRV = map[schema.GroupVersionResource]string{}
	}
}

// applyConfirmLocked is the per-GVR conjunct-3/4 update body extracted from
// RefreshDiscovery so the scoped ConfirmResourceType reuses the EXACT same
// confirm/recover logic — it does NOT fork a parallel predicate
// (feedback_no_special_cases / the architect's "reuse RefreshDiscovery's
// confirm logic" constraint). Callers MUST hold rw.mu.Lock() and MUST have
// called ensureConfirmMapsLocked.
//
//   - haveDisco: a discovery client is wired (disco != nil). With no
//     discovery client, conjunct 4 is degraded-true (resourceTypeConfirmedLocked
//     returns true) so rw.confirmed is left untouched — identical to the
//     pre-extraction RefreshDiscovery branch.
//   - typeServed: whether the apiserver currently serves gvr's resource
//     type (the result of resourceTypeServed for gvr's group/version).
func (rw *ResourceWatcher) applyConfirmLocked(
	gvr schema.GroupVersionResource,
	gi informers.GenericInformer,
	haveDisco bool,
	typeServed bool,
) {
	// Conjunct 4: confirm the resource type. With no discovery client,
	// haveDisco==false ⇒ resourceTypeConfirmedLocked already returns true,
	// so we leave rw.confirmed untouched here.
	if haveDisco {
		if typeServed {
			rw.confirmed[gvr] = struct{}{}
		} else {
			// Resource type not served — un-confirm it. This is what
			// gates a post-startup CRD until the apiserver publishes its
			// API, and also correctly retracts a confirmation if a CRD is
			// deleted.
			delete(rw.confirmed, gvr)
		}
	}

	// Conjunct-3 recovery: a successful relist advances the informer's
	// LastSyncResourceVersion. If it advanced since the last refresh AND
	// the informer is currently synced, the WATCH is healthy again — clear
	// watchBroken.
	rv := gi.Informer().LastSyncResourceVersion()
	prev := rw.lastSyncRV[gvr]
	if rv != "" && rv != prev && gi.Informer().HasSynced() {
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

// ConfirmResourceType runs ONE conjunct-3/4 confirmation pass SCOPED to a
// single gvr, reusing RefreshDiscovery's exact confirm/recover body
// (applyConfirmLocked) — it does NOT fork a parallel confirm predicate.
//
// Fix #1 (1b): primed on the api-step LIST register path (informer_dispatch.go
// Gate 6, after EnsureResourceType) and on the apistage content-HIT
// re-touch (1a) so the FIRST post-boot LIST of a registered GVR does not
// wait a full discoveryRefreshInterval (30s) tick to become servable, and a
// transient boot-time discovery flap self-corrects. Without this, a
// registered-but-unconfirmed GVR served from the apistage content cache
// stays latched not-servable to TTL — the stale-delete root cause
// (docs/rca-stale-delete-compositiondefinitions-informer-2026-06-25.md §4.3).
//
// Lazy + idempotent (feedback_bounding_mechanism_discipline): a no-op for
// an UNregistered gvr (nothing to confirm — the LIST register path calls
// EnsureResourceType first, so by the time this runs the informer exists or
// the call is a cheap miss) and re-runnable any number of times (confirming
// an already-confirmed GVR is a map write of an existing key). One discovery
// round-trip (ServerResourcesForGroupVersion) for gvr's group/version, off
// the lock; bounded by ctx.
//
// Concurrency: writes rw.confirmed / rw.watchBroken / rw.lastSyncRV under
// rw.mu.Lock() — the SAME maps + lock the discovery-refresh ticker uses, so
// the two are serialised (feedback_shared_vs_copy_is_a_concurrency_change;
// covered by TestFalsifierHealA_ScopedConfirmRace under -race).
//
// Nil-receiver / passthrough safe.
func (rw *ResourceWatcher) ConfirmResourceType(ctx context.Context, gvr schema.GroupVersionResource) {
	if rw == nil || rw.mode == modePassthrough {
		return
	}

	// Snapshot registration + discovery client under RLock; run the
	// (blocking) discovery call WITHOUT the lock; re-read the informer + write
	// back under Lock (N2 — the informer handle is re-read under the write
	// lock, not captured here, to survive a delete+recreate interleave).
	rw.mu.RLock()
	_, registered := rw.informers[gvr]
	disco := rw.disco
	rw.mu.RUnlock()
	if !registered {
		// Nothing to confirm — the GVR has no informer. Lazy: the LIST
		// register path (EnsureResourceType) runs first; a miss here is a
		// cheap no-op, not a registration.
		return
	}
	if ctx != nil && ctx.Err() != nil {
		return
	}

	typeServed := false
	if disco != nil {
		typeServed = resourceTypeServed(disco, gvr)
	}

	rw.mu.Lock()
	defer rw.mu.Unlock()
	// Re-check registration under the write lock: a concurrent
	// RemoveResourceType (CRD-DELETE teardown) could have unregistered gvr
	// between the RUnlock and here. Confirming a torn-down GVR would
	// resurrect a stale rw.confirmed entry the teardown meant to drop.
	//
	// N2 (architect): re-READ gi from rw.informers under the write lock
	// rather than trusting the gi captured under the earlier RLock. On a
	// delete+recreate interleave (RemoveResourceType then EnsureResourceType
	// of a fresh informer) the captured gi is stale; reading its
	// LastSyncResourceVersion in applyConfirmLocked would write rw.lastSyncRV
	// off the OLD reflector. The re-read binds the conjunct-3 recovery to the
	// CURRENT informer.
	curGI, stillRegistered := rw.informers[gvr]
	if !stillRegistered {
		return
	}
	rw.ensureConfirmMapsLocked()
	rw.applyConfirmLocked(gvr, curGI, disco != nil, typeServed)
}

// ConfirmResourceTypes runs the scoped conjunct-3/4 confirmation pass over a
// caller-supplied SET of GVRs in one shot, reusing RefreshDiscovery's exact
// per-GV dedup + applyConfirmLocked body (it does NOT fork a parallel
// predicate). It is ConfirmResourceType's plural sibling: same confirm/recover
// semantics, but it issues ONE ServerResourcesForGroupVersion per distinct
// group/version across the whole set rather than one-per-GVR — so a walk that
// registers many GVRs sharing a group/version (e.g. several CompositionDefinition
// versions) costs one discovery round-trip per GV, not per GVR.
//
// Fix #130 F1: primed by the discovery-walk registration path
// (PrewarmRegisterFromNavigation, prewarm.go) so a GVR the walk lazily
// registers becomes conjunct-4 typeConfirmed within one discovery round-trip
// instead of waiting a full discoveryRefreshInterval (30s) ticker tick. The
// 1.7.3 serve_miss readout showed EVERY walk-registered GVR latched
// typeConfirmed:false (registered/hasSynced/watchHealthy all true) until the
// ticker — conjunct 4 was the sole universal blocker of informer-serve at boot.
//
// Only the SUBSET of gvrs currently registered is confirmed — an unregistered
// gvr in the set is a cheap no-op (nothing to confirm; the walk's
// EnsureResourceType runs first, so a walk-registered GVR is registered by the
// time this runs). Idempotent + re-runnable: confirming an already-confirmed
// GVR is a map write of an existing key. Discovery calls run OFF the lock and
// are bounded by ctx.
//
// Concurrency: writes rw.confirmed / rw.watchBroken / rw.lastSyncRV under
// rw.mu.Lock() — the SAME maps + lock the discovery-refresh ticker and the
// scoped ConfirmResourceType use, so all three are serialised. Informer handles
// are re-read under the write lock (N2), not captured under the earlier RLock,
// to survive a delete+recreate interleave.
//
// Nil-receiver / passthrough safe. Empty / nil set = no-op.
func (rw *ResourceWatcher) ConfirmResourceTypes(ctx context.Context, gvrs []schema.GroupVersionResource) {
	if rw == nil || rw.mode == modePassthrough || len(gvrs) == 0 {
		return
	}

	rw.mu.RLock()
	disco := rw.disco
	rw.mu.RUnlock()
	if ctx != nil && ctx.Err() != nil {
		return
	}

	// Resolve resource-type existence per group/version, deduped — identical
	// to RefreshDiscovery's dedup loop, run OFF the lock. One discovery call
	// per distinct gv across the whole set (the cost bound), not per GVR.
	served := map[string]bool{}
	if disco != nil {
		queried := map[string]struct{}{}
		for _, gvr := range gvrs {
			if ctx != nil && ctx.Err() != nil {
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

	rw.mu.Lock()
	defer rw.mu.Unlock()
	rw.ensureConfirmMapsLocked()
	for _, gvr := range gvrs {
		// Re-read the informer under the write lock (N2): only confirm a GVR
		// that is CURRENTLY registered. A GVR unregistered between the walk's
		// EnsureResourceType and here (CRD-DELETE teardown) is skipped, so we
		// never resurrect a stale rw.confirmed entry.
		curGI, stillRegistered := rw.informers[gvr]
		if !stillRegistered {
			continue
		}
		rw.applyConfirmLocked(gvr, curGI, disco != nil, served[groupVersionString(gvr)])
	}
}

// ServableGVRStatus is a read-only per-GVR servability snapshot row,
// surfaced by the /debug/servable diagnostic. It exposes the four
// servability conjuncts so an operator can see WHY a GVR is (not) servable
// without a kubectl exec into the pod.
type ServableGVRStatus struct {
	GVR         string `json:"gvr"`
	HasSynced   bool   `json:"hasSynced"`   // conjunct 2
	WatchBroken bool   `json:"watchBroken"` // conjunct 3 (true ⇒ not servable)
	Confirmed   bool   `json:"confirmed"`   // conjunct 4
	Servable    bool   `json:"servable"`    // all four conjuncts
}

// ServableSnapshot returns a read-only per-GVR servability snapshot over
// every registered informer. READ-ONLY: it takes rw.mu in READ mode and
// mutates NO state (no confirm, no register, no watch-flag change) — it is
// safe to call from a diagnostic HTTP handler on every request.
//
// Returns nil for a nil receiver or passthrough mode (no informers to
// report). Deterministically ordered is NOT guaranteed (map iteration); the
// handler sorts for stable output.
func (rw *ResourceWatcher) ServableSnapshot() []ServableGVRStatus {
	if rw == nil || rw.mode == modePassthrough {
		return nil
	}
	rw.mu.RLock()
	defer rw.mu.RUnlock()
	out := make([]ServableGVRStatus, 0, len(rw.informers))
	for gvr, gi := range rw.informers {
		_, broken := rw.watchBroken[gvr]
		_, confirmed := rw.confirmed[gvr]
		// servableLocked is the single source of truth for the composite —
		// reuse it rather than re-deriving the conjunction here, so a future
		// conjunct change cannot make the diagnostic disagree with the gate.
		_, servable := rw.servableLocked(gvr)
		out = append(out, ServableGVRStatus{
			GVR:         gvr.String(),
			HasSynced:   gi.Informer().HasSynced(),
			WatchBroken: broken,
			Confirmed:   confirmed,
			Servable:    servable,
		})
	}
	return out
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

// HasWatchHandlerForTest reports whether gvr's informer had its
// conjunct-3 WATCH-error handler installed (recorded in
// rw.watchHandlerInstalled). It is the test surface for the 0.30.99
// Tag B watch-handler coverage guard — a unit test asserts every
// registered GVR, RBAC bootstrap and lazily-registered alike, returns
// true here, proving installWatchErrorHandler ran on its registration
// path. A fake dynamic client never drops a real WATCH, so this is the
// only deterministic way to assert install-time coverage (FireWatchError
// proves conjunct-3 GATING but writes watchBroken directly, bypassing
// the installed handler).
//
// Returns false for nil receivers, passthrough mode, or unknown GVRs.
// Safe for concurrent use; takes rw.mu in read mode.
func (rw *ResourceWatcher) HasWatchHandlerForTest(gvr schema.GroupVersionResource) bool {
	if rw == nil {
		return false
	}
	rw.mu.RLock()
	defer rw.mu.RUnlock()
	_, ok := rw.watchHandlerInstalled[gvr]
	return ok
}
