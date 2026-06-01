// phase1.go — 0.30.102 Tag B: startup informer-warmup state + the
// hardcoded meta-query seed budget + the all-informer sync-wait.
//
// Tag B premise (resting on Tag A 0.30.100): the resolver pivot
// (RESOLVER_USE_INFORMER=true) can only serve a GVR whose informer is
// registered AND synced. 0.30.99's bench failed because the navigated
// informers were registered lazily-late and never synced inside the
// navigation window — the pivot served nothing cold.
//
// Tag B closes that cold window with a startup PHASE 1: at boot, before
// traffic, the TWO navigation roots (the `routesloaders` and `navmenus`
// widget CRs) are LISTed cluster-wide and every CR is RECURSIVELY
// resolved with the snowplow SERVICE-ACCOUNT identity through the
// standard widget/RESTAction resolver (0.30.105: the walk recurses
// Root -> Route -> Page -> Row/Column -> DataGrid/Table leaf via each
// resolved widget's status.resourcesRefs.items[]). As the resolver walks
// inner-call paths it auto-registers an informer for every GVR it
// touches via the flag-independent `lazyRegisterInnerCallPaths` hook —
// including the heavy `composition.krateo.io` informer behind the
// Compositions Page DataGrid. After the walk, Phase 1 BLOCKS until every
// registered informer reaches HasSynced, then signals Phase1Done. The
// /readyz probe gates pod readiness on Phase1Done so traffic only
// arrives once the navigated informers are warm.
//
// CRITICAL — feedback_no_special_cases.md: Phase 1 does NOT consult any
// configured GVR / RESTAction list. The ONLY hardcoded budget is the 7
// meta-query seeds below — bare anchors needed to bootstrap discovery,
// not per-resource policy. Every BUSINESS GVR (widgets, panels,
// compositions) is discovered by recursively resolving the two
// navigation roots.
//
// Ship 0 / 0.30.222: the customresourcedefinitions GVR was REMOVED from
// the seed set. Ship 0.5 / 0.30.223 (v6): the CRD informer is DELETED
// entirely. Composition GVRs are discovered by one-shot apiserver
// discovery (cache.DiscoverGroupResources, invoked synchronously from
// the walker) instead of via an in-process CRD-informer event stream.
// The 4 RBAC GVRs + restactions + routesloaders + navmenus remain
// primordial because they have justified chicken-and-egg semantics
// (walker queries them to start the walk); the CRD GVR has none — by
// the time the walker encounters a templated path it is already
// running.
//
// BEHAVIOR-NEUTRAL — PrewarmEnabled() gates the whole feature behind
// PREWARM_ENABLED (default OFF), mirroring PREWARM_REGISTER_ENABLED.
// When OFF: Phase 1 never runs and Phase1Done is pre-set true at startup
// so /readyz is an immediate-200 no-op. The flag is NOT in the chart
// configmap — absent => OFF.

package cache

import (
	"context"
	"os"
	"sync/atomic"
	"time"

	"log/slog"

	"k8s.io/apimachinery/pkg/runtime/schema"
	clientcache "k8s.io/client-go/tools/cache"
)

// envPrewarmEnabled is the opt-in gate for the Tag B startup warmup
// (Phase 1 + walker-driven discovery). Default OFF — absent / "" /
// anything but "true" => the feature is dormant and behavior-neutral.
const envPrewarmEnabled = "PREWARM_ENABLED"

// PrewarmEnabled reports whether the Tag B startup warmup is opted in.
// Read once at startup by main.go; cheap enough to read per call.
func PrewarmEnabled() bool {
	return os.Getenv(envPrewarmEnabled) == "true"
}

// phase1Done is the process-wide atomic that flips true exactly once,
// when the Phase 1 SA-credentialed resolution walk has finished AND
// every registered informer (including composition informers spawned
// via cache.DiscoverGroupResources during the walk) has reached
// HasSynced.
//
// When PrewarmEnabled()==false the startup sequence calls
// MarkPhase1Done immediately (nothing to wait for) so /readyz is a
// no-op. When true, MarkPhase1Done is called only at the END of
// Phase1Warmup. /readyz returns 200 iff phase1Done.Load()==true.
var phase1Done atomic.Bool

// MarkPhase1Done flips the process-wide Phase1Done signal to true. Safe
// to call multiple times — atomic store is idempotent. Called once by
// the startup sequence (immediately when PREWARM_ENABLED is OFF, or at
// the tail of Phase1Warmup when ON).
func MarkPhase1Done() {
	phase1Done.Store(true)
}

// IsPhase1Done reports whether the Tag B startup warmup has completed.
// The /readyz probe handler returns 200 iff this is true. Liveness
// (/health) does NOT consult this — a not-yet-warm pod is alive.
func IsPhase1Done() bool {
	return phase1Done.Load()
}

// ShouldFlipPhase1DoneOnStartup reports whether the startup safety-net
// at main.go's readiness-gate block should flip Phase1Done immediately
// (because there is nothing to warm), rather than wait for the
// Phase1Warmup goroutine to signal completion.
//
// The invariant: /readyz must return 200 once the pod is ready to serve
// traffic. When the cache subsystem is OFF, prewarm is OFF, or the
// watcher failed to construct, NO informer-warming work exists — the
// gate must flip at boot or the pod is stuck "warming" forever, the
// Service drops it from Endpoints, and the LB has 0 backends. The
// healthy CACHE_ENABLED=true + PREWARM_ENABLED=true path returns false
// here so Phase1Warmup retains ownership of the flip.
//
// 0.30.153 — Ship: introduced as a named helper to make the four-disjunct
// invariant testable and to retire the inline conditional at main.go that
// missed the CACHE_ENABLED=false + PREWARM_ENABLED=true + watcher-non-nil
// case (incident: pod stuck `{"status":"warming","phase1Done":false}`,
// Service endpoints empty, snowplow LB unroutable).
//
// Three reasons to flip (any one suffices):
//   - cacheEnabled == false — cache subsystem off, no informers exist
//   - prewarmEnabled == false — prewarm disabled, no warmup goroutine runs
//   - watcherIsNil == true — watcher construction failed, no informers exist
//
// MarkPhase1Done is idempotent (atomic store) so a caller may invoke it
// unconditionally when this returns true.
func ShouldFlipPhase1DoneOnStartup(cacheEnabled, prewarmEnabled, watcherIsNil bool) bool {
	return !cacheEnabled || !prewarmEnabled || watcherIsNil
}

// ResetPhase1DoneForTest clears the Phase1Done signal. TEST-ONLY — the
// production lifecycle is set-once. Exported so the readyz handler test
// in another package can drive the gate deterministically.
func ResetPhase1DoneForTest() {
	phase1Done.Store(false)
}

// Ship 0.5 / 0.30.223 (v6): the apiextensions CRD GVR constant AND
// its accessor were DELETED. The CRD informer is no longer spawned
// anywhere in the cache codebase; the composition-GVR-discovery
// semantics it used to back are now satisfied by cache.
// DiscoverGroupResources (one-shot apiserver discovery, synchronous,
// invoked from the walker — see discovery_lookup.go).

// routesLoadersGVR is the GVR of the `routesloaders` widget CR.
//
// 0.30.107 — this is NO LONGER a root-SELECTION driver. The navigation
// roots Phase 1 walks are read from the frontend ConfigMap at runtime
// (config.json .api.INIT / .api.ROUTES_LOADER — see
// dispatchers/phase1_roots.go); the resource name `routesloaders` is
// never a Go literal in that selection path. This GVR remains ONLY as a
// meta-query INFORMER-ANCHOR seed: the watcher pre-registers an informer
// for this resource type so that a `/call` to a routesloaders CR can be
// served from cache rather than the apiserver. It is the informer-warming
// anchor, not "where navigation starts".
//
// Per feedback_no_special_cases.md: a bare informer-anchor seed for a
// well-known navigation resource type, not a per-resource carve-out and
// not a root-selection special-case.
var routesLoadersGVR = schema.GroupVersionResource{
	Group:    "widgets.templates.krateo.io",
	Version:  "v1beta1",
	Resource: "routesloaders",
}

// navMenusGVR is the GVR of the `navmenus` widget CR.
//
// 0.30.107 — like routesLoadersGVR, this is NO LONGER a root-SELECTION
// driver: the navigation roots come from the frontend ConfigMap's
// config.json (.api.INIT). This GVR remains ONLY as a meta-query
// INFORMER-ANCHOR seed so a `/call` to a navmenus CR can be served from
// the informer cache. The resource name `navmenus` is never a Go literal
// in the root-selection path.
//
// Per feedback_no_special_cases.md: a bare informer-anchor seed, not a
// per-resource carve-out.
var navMenusGVR = schema.GroupVersionResource{
	Group:    "widgets.templates.krateo.io",
	Version:  "v1beta1",
	Resource: "navmenus",
}

// RoutesLoadersGVR exposes the routesloaders meta-query informer-anchor
// seed. Read-only accessor. 0.30.107: no longer consumed by the Phase 1
// root-selection path (roots come from the frontend ConfigMap) — retained
// for the seed-set and its falsifier test.
func RoutesLoadersGVR() schema.GroupVersionResource {
	return routesLoadersGVR
}

// NavMenusGVR exposes the navmenus meta-query informer-anchor seed.
// Read-only accessor. 0.30.107: no longer consumed by the Phase 1
// root-selection path.
func NavMenusGVR() schema.GroupVersionResource {
	return navMenusGVR
}

// MetaQuerySeeds returns the COMPLETE hardcoded seed budget for Tag B —
// EXACTLY these 7 GVRs, nothing else (feedback_no_special_cases.md is a
// hard requirement here). Every entry is a meta-query INFORMER-ANCHOR
// seed: the watcher pre-registers an informer for the resource type so a
// `/call` to one of these can be served from cache. None of them is a
// root-SELECTION driver — the navigation roots come from the frontend
// ConfigMap (config.json .api.INIT / .api.ROUTES_LOADER; see
// dispatchers/phase1_roots.go).
//
//  1. routesloaders            — informer-anchor for the routesloaders
//     widget type. 0.30.107: no longer a root-selection literal.
//  2. navmenus                 — informer-anchor for the navmenus widget
//     type. 0.30.107: no longer a root-selection literal.
//  3. restactions              — the restActionGVR anchor (already cited
//     by inventory.go; the resolver's apiRef edges target it).
//  4-7. the 4 RBACResourceTypes — roles / rolebindings / clusterroles /
//     clusterrolebindings (already bootstrap-registered in
//     NewResourceWatcher; included here so the seed set is the single
//     auditable source of truth).
//
// Ship 0 / 0.30.222: customresourcedefinitions is NO LONGER a seed
// (Diego invariant: "no CRD informer if the CRD object itself is not
// walked"). Ship 0.5 / 0.30.223 (v6): the CRD informer was deleted
// entirely; composition GVRs are discovered by one-shot apiserver
// discovery (cache.DiscoverGroupResources) invoked synchronously from
// the walker.
//
// Every BUSINESS GVR — widgets, panels, compositions — is ABSENT from
// this set by construction. Those are discovered by RESOLVING the
// ConfigMap-derived navigation roots, never named in code. A test
// asserts this slice has exactly 7 entries and that none of them is a
// composition/widget/panel business GVR.
func MetaQuerySeeds() []schema.GroupVersionResource {
	seeds := []schema.GroupVersionResource{
		routesLoadersGVR,
		navMenusGVR,
		restActionGVR,
	}
	seeds = append(seeds, RBACResourceTypes...)
	return seeds
}

// RegisterMetaQuerySeeds registers an informer for each of the 3
// non-RBAC meta-query seeds (routesloaders, navmenus, restactions) plus
// re-confirms the 4 RBAC GVRs (already registered by NewResourceWatcher
// — EnsureResourceType observes added=false for those) — 7 seeds total.
// Idempotent + singleflighted under rw.mu.
//
// Ship 0 / 0.30.222: the CRD GVR is no longer in this list. Ship 0.5
// / 0.30.223 (v6): the CRD informer is deleted; composition GVRs are
// discovered via cache.DiscoverGroupResources (one-shot apiserver
// discovery) invoked synchronously from the walker the first time a
// templated apiserver path is reached.
//
// This is the ONLY code that hands a hardcoded GVR to EnsureResourceType
// at startup. The Phase 1 walk registers everything else by resolution.
//
// Returns the count newly registered. Nil-receiver / passthrough are
// no-ops.
func (rw *ResourceWatcher) RegisterMetaQuerySeeds() int {
	if rw == nil || rw.mode == modePassthrough {
		return 0
	}
	registered := 0
	for _, gvr := range MetaQuerySeeds() {
		added, _ := rw.EnsureResourceType(gvr)
		if added {
			registered++
		}
	}
	slog.Info("cache.phase1.meta_query_seeds_registered",
		slog.String("subsystem", "cache"),
		slog.Int("seed_count", len(MetaQuerySeeds())),
		slog.Int("newly_registered", registered),
		slog.String("note", "bare meta-query anchors only — every business GVR is discovered by resolution"),
	)
	return registered
}

// WaitAllInformersSynced blocks until EVERY registered informer reaches
// HasSynced AND no new informer was registered DURING the wait, or ctx
// is cancelled. This is the Phase 1 sync barrier: after the SA-credentialed
// resolution walk has fanned out (registering an informer per touched
// GVR via lazyRegisterInnerCallPaths) AND every templated apiserver
// path has invoked cache.DiscoverGroupResources to spawn composition
// informers (v6, Ship 0.5), this call guarantees the navigated set is
// warm before Phase1Done flips.
//
// RE-SNAPSHOT LOOP — the load-bearing concurrency property. A single
// snapshot+wait has a race: a late EnsureResourceType (a composition
// informer spawned by a still-running DiscoverGroupResources, or by a
// late resolver inner-call touch) that lands AFTER the snapshot is
// taken but while WaitForCacheSync is blocked would NOT be in the sync
// set — Phase1Done could then flip while that composition informer is
// still cold, the exact premature-Ready failure /readyz exists to
// prevent. So this loop re-snapshots after every WaitForCacheSync pass
// and only returns when a full pass completed with the registered-
// informer count UNCHANGED across it (no registration occurred during
// the wait). client-go's HasSynced is monotonic — once true it stays
// true — so a stable count across a pass means every informer observed
// at the start of the pass is synced AND nothing new appeared, hence
// every informer is synced.
//
// It does NOT layer its own timeout — the caller (Phase1Warmup) owns the
// deadline via ctx so the PHASE1_TIMEOUT_SECONDS budget is the single
// source of truth and also bounds a pathological never-stabilizing loop.
//
// INVARIANT the count-equality test depends on: the registered-informer
// set is append-only during Phase 1 — RemoveResourceType is wired only
// to the CRD-DELETE path (followup #117, post-Ship 2), which is dormant
// during Phase 1's bounded walk. So an unchanged COUNT across a pass
// implies an unchanged SET. If a future change adds an in-Phase-1
// de-registration path, this proxy breaks and the loop must compare
// the GVR set, not the count.
//
// Returns nil on success, ctx.Err()/DeadlineExceeded on cancellation. In
// modePassthrough there are no informers — returns nil immediately.
func (rw *ResourceWatcher) WaitAllInformersSynced(ctx context.Context) error {
	if rw == nil || rw.mode == modePassthrough {
		return nil
	}

	start := time.Now()
	for pass := 1; ; pass++ {
		if ctx.Err() != nil {
			slog.Warn("cache.phase1.sync_wait_incomplete",
				slog.String("subsystem", "cache"),
				slog.Int("pass", pass),
				slog.Int64("waited_ms", time.Since(start).Milliseconds()),
				slog.Any("err", ctx.Err()),
			)
			return ctx.Err()
		}

		// Snapshot the informer set + count under the lock.
		rw.mu.RLock()
		countBefore := len(rw.informers)
		syncs := make([]clientcache.InformerSynced, 0, countBefore)
		for _, gi := range rw.informers {
			syncs = append(syncs, gi.Informer().HasSynced)
		}
		rw.mu.RUnlock()

		if len(syncs) == 0 {
			// Nothing registered — vacuously synced.
			return nil
		}

		// Wait for this snapshot's informers to sync (outside the lock,
		// so concurrent registrations are not blocked).
		if !clientcache.WaitForCacheSync(ctx.Done(), syncs...) {
			slog.Warn("cache.phase1.sync_wait_incomplete",
				slog.String("subsystem", "cache"),
				slog.Int("pass", pass),
				slog.Int("informer_count", countBefore),
				slog.Int64("waited_ms", time.Since(start).Milliseconds()),
				slog.Any("err", ctx.Err()),
			)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return context.DeadlineExceeded
		}

		// Re-snapshot: if the count is unchanged, NO informer was
		// registered during the WaitForCacheSync pass — the barrier is
		// genuinely complete. If it grew, a CRD-add (or a late resolver
		// touch) landed mid-wait; loop and re-wait so the new informer
		// is included.
		countAfter := rw.RegisteredCount()
		if countAfter == countBefore {
			slog.Info("cache.phase1.sync_wait_complete",
				slog.String("subsystem", "cache"),
				slog.Int("passes", pass),
				slog.Int("informer_count", countAfter),
				slog.Int64("waited_ms", time.Since(start).Milliseconds()),
			)
			return nil
		}
		slog.Info("cache.phase1.sync_wait_repass",
			slog.String("subsystem", "cache"),
			slog.Int("pass", pass),
			slog.Int("count_before", countBefore),
			slog.Int("count_after", countAfter),
			slog.String("reason", "informer registered mid-wait — re-snapshotting so the new informer is in the barrier"),
		)
	}
}

// RegisteredGVRs returns a snapshot of every GVR with a registered
// informer. The no-hardcode falsifier asserts over this full set that
// the orphan GVR is absent — a stronger check than a single-GVR probe.
func (rw *ResourceWatcher) RegisteredGVRs() []schema.GroupVersionResource {
	if rw == nil || rw.mode == modePassthrough {
		return nil
	}
	rw.mu.RLock()
	defer rw.mu.RUnlock()
	out := make([]schema.GroupVersionResource, 0, len(rw.informers))
	for gvr := range rw.informers {
		out = append(out, gvr)
	}
	return out
}

// IsRegistered reports whether an informer exists for gvr. Convenience
// over RegisteredGVRs for single-GVR assertions (falsifier tests).
func (rw *ResourceWatcher) IsRegistered(gvr schema.GroupVersionResource) bool {
	if rw == nil || rw.mode == modePassthrough {
		return false
	}
	rw.mu.RLock()
	defer rw.mu.RUnlock()
	_, ok := rw.informers[gvr]
	return ok
}

// RegisteredCount returns the number of registered informers without
// allocating a slice. The Phase 1 walk driver polls this to detect when
// the CRD-watch + resolution fan-out has settled.
func (rw *ResourceWatcher) RegisteredCount() int {
	if rw == nil || rw.mode == modePassthrough {
		return 0
	}
	rw.mu.RLock()
	defer rw.mu.RUnlock()
	return len(rw.informers)
}

// Ship 0.30.127: WithPhase1Resolution / IsPhase1Resolution and their
// context key were REMOVED. The marker's sole consumer was the
// phase1IteratorCap in the RESTAction resolver (setup.go), deleted this
// ship — the cap truncated the Phase-1 navmenu walk's per-namespace
// iterator to namespaces holding no navmenuitems, so the walk descended
// nothing past the roots. With the cap gone the marker has zero
// consumers; it is swept rather than left as dead code.

// ctxKeyInternalEndpointType is the typed empty-struct context key for
// WithInternalEndpoint / InternalEndpointFromContext.
type ctxKeyInternalEndpointType struct{}

var ctxKeyInternalEndpoint = ctxKeyInternalEndpointType{}

// WithInternalEndpoint attaches an internal-dispatch apiserver endpoint
// to ctx. The RESTAction resolver's endpoint-resolution step consults
// this when a non-UAF api[] stage has NO EndpointRef AND the request is
// driven by an internal/startup path that has no per-user `-clientconfig`
// Secret to read.
//
// This is a GENERAL mechanism, not a per-resource carve-out
// (feedback_no_special_cases.md): any internal driver — Phase 1's
// SA-credentialed resolution walk today, a future background refresher
// tomorrow — can tell the standard resolver which endpoint to dispatch
// against instead of the per-user clientconfig lookup. The resolver
// stays one code path; only the endpoint SOURCE is parameterised.
//
// ep is carried as `any` so the cache package does not couple to the
// plumbing endpoints type; the resolver type-asserts to its endpoint
// type. nil ep returns the parent context unchanged.
func WithInternalEndpoint(ctx context.Context, ep any) context.Context {
	if ctx == nil || ep == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyInternalEndpoint, ep)
}

// InternalEndpointFromContext returns the internal-dispatch endpoint
// attached by WithInternalEndpoint, or (nil, false) when none was set
// (the ordinary per-user request path — the resolver then takes its
// standard per-user clientconfig lookup).
func InternalEndpointFromContext(ctx context.Context) (any, bool) {
	if ctx == nil {
		return nil, false
	}
	v := ctx.Value(ctxKeyInternalEndpoint)
	if v == nil {
		return nil, false
	}
	return v, true
}

// ctxKeyInternalRESTConfigType is the typed empty-struct context key for
// WithInternalRESTConfig / InternalRESTConfigFromContext.
type ctxKeyInternalRESTConfigType struct{}

var ctxKeyInternalRESTConfig = ctxKeyInternalRESTConfigType{}

// WithInternalRESTConfig attaches a ready-built apiserver *rest.Config to
// ctx. The snowplow object/resourceRef client-construction sites consult
// this when an internal/startup driver (Phase 1's SA-credentialed
// resolution walk) is in flight, and use it directly instead of rebuilding
// a client from the context endpoint via kubeconfig.NewClientConfig.
//
// 0.30.103 bug fix — WHY a *rest.Config and not just the endpoint:
// kubeconfig.NewClientConfig(ctx, ep) marshals the endpoint into a
// kubeconfig document and hands it to client-go's clientcmd loader. That
// path is CERT-AUTH-ONLY and base64-aware:
//   - it has NO token field — a token-bearing endpoint loses its only
//     credential (the SA client would then be unauthenticated);
//   - clientcmd base64-DECODES certificate-authority-data, so it requires
//     the CA to be base64-encoded. The per-user `<user>-clientconfig`
//     Secret stores credentials base64-encoded (the authn signup flow
//     base64-encodes them), so the per-user path works. But the snowplow
//     service account's in-cluster credentials — the projected
//     /var/run/secrets/.../token (a raw JWT) and ca.crt (raw PEM) — are
//     NOT base64-encoded. Feeding the raw-PEM SA CA through that path
//     fails with "illegal base64 data at input byte 0".
//
// So the SA cannot be expressed as a kubeconfig-loadable endpoint at all.
// The SA *rest.Config must be built directly from the raw in-cluster
// credentials (rest.InClusterConfig), then carried here so the resolver's
// client-construction sites use it verbatim — bypassing the
// base64/cert-only kubeconfig path entirely.
//
// This is a GENERAL mechanism, not a per-resource carve-out
// (feedback_no_special_cases.md): any internal driver can hand the
// resolver a pre-built *rest.Config; only the client SOURCE is
// parameterised, the resolver stays one code path. Ordinary per-user
// requests never set it and fall through to the unchanged
// kubeconfig.NewClientConfig path.
//
// rc is carried as `any` so the cache package does not couple to the
// k8s.io/client-go/rest type; the consuming site type-asserts to
// *rest.Config. nil rc returns the parent context unchanged.
func WithInternalRESTConfig(ctx context.Context, rc any) context.Context {
	if ctx == nil || rc == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyInternalRESTConfig, rc)
}

// InternalRESTConfigFromContext returns the internal-dispatch *rest.Config
// attached by WithInternalRESTConfig, or (nil, false) when none was set
// (the ordinary per-user request path — the caller then builds a client
// from the context endpoint via kubeconfig.NewClientConfig).
func InternalRESTConfigFromContext(ctx context.Context) (any, bool) {
	if ctx == nil {
		return nil, false
	}
	v := ctx.Value(ctxKeyInternalRESTConfig)
	if v == nil {
		return nil, false
	}
	return v, true
}
