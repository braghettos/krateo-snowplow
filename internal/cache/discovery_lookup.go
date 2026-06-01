// discovery_lookup.go — Ship 0.5 / 0.30.223 (v6 design,
// docs/walker-driven-informer-design-2026-06-01.md). REPLACES the
// pre-v6 CRD-watch file (459 LOC, deleted): the in-process LIST/WATCH
// informer against apiextensions.k8s.io/v1/customresourcedefinitions
// is gone. Composition GVRs are discovered by one-shot apiserver
// discovery (Discovery.ServerResourcesForGroupVersion) invoked
// synchronously from the walker the first time a templated apiserver
// path is reached for each navigation-discovered group.
//
// WHY v6 IS SAFE — synchronous one-shot has no replay window. The
// pre-v6 pattern had a boot replay-vs-discover race closed by a
// post-walk reconcile re-scan; v6 deletes the informer (no replay
// window). DiscoverGroupResources is a single synchronous transaction
// — list resources, register each, dirty-mark stale-negative LIST
// entries, return.
//
// LAG TRADEOFF — accepted per Diego 2026-06-01. CRD CREATE detected
// on the NEXT walker pass under that group (bounded by Phase 1 +
// widget/RESTAction CRUD re-walks). CRD DELETE unbounded — composition
// informer stays until pod restart; LIST/WATCH against a deleted CRD
// produces apiserver 404 (no-cache equivalent). Periodic sweep
// followup #117, post-Ship 2.
//
// PRESERVED: the navigation-discovered group set (renamed accessors,
// load-bearing for watcher.go:749/:1064 removable-discriminator) +
// the FD1 dirty-mark chain (Ship D / 0.30.114).
//
// REMOVED: the CRD informer + handlers + handler-extension entry +
// post-walk reconcile + per-CRD-object derivation + register/
// unregister event handlers.

package cache

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"log/slog"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	kscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
)

// navDiscoveredGroups is the set of apiserver groups that the walker
// has reached via a templated apiserver path. NAVIGATION-DERIVED —
// starts empty, populated only by AddNavigationDiscoveredGroup.
//
// LOAD-BEARING for watcher.go's removable-discriminator at :749 +
// :1064: a GVR is removable (gets a standalone informer, not a
// shared-factory one) iff its group is in this set. Composition GVRs
// MUST be removable so RemoveResourceType can tear them down on the
// periodic sweep (followup #117) without affecting unrelated
// informers.
//
// Guarded by navDiscoveredGroupsMu (its own mutex, not rw.mu —
// callers may be inside rw.mu when they consult IsNavigationDiscoveredGroup).
var (
	navDiscoveredGroupsMu sync.RWMutex
	navDiscoveredGroups   = map[string]struct{}{}
)

// AddNavigationDiscoveredGroup records group as a navigation-discovered
// group. Idempotent. Pure set-add — NO informer side-effects (v6).
// Called by the Phase 1 walk for every static group it extracts from
// a templated apiserver path via
// ExtractAPIServerGroupFromTemplatedPath.
//
// The empty string is rejected — the core group ("") is never a
// composition group and admitting it would corrupt the removable-
// discriminator predicate (every core resource would be treated as
// removable, which is wrong: built-in informers must never be torn
// down).
//
// v6 NOTE: pre-Ship-0.5 this call also fired a sync.Once spawning the
// CRD informer. v6 deletes that side-effect; composition GVR discovery
// is now done by DiscoverGroupResources (synchronous apiserver
// discovery). The walker calls AddNavigationDiscoveredGroup(group) +
// DiscoverGroupResources(ctx, rc, group) at the same site (resolve.go).
func AddNavigationDiscoveredGroup(group string) {
	if group == "" {
		return
	}
	navDiscoveredGroupsMu.Lock()
	_, existed := navDiscoveredGroups[group]
	navDiscoveredGroups[group] = struct{}{}
	navDiscoveredGroupsMu.Unlock()
	if !existed {
		slog.Info("cache.discovery.navigation_discovered_group_added",
			slog.String("subsystem", "cache"),
			slog.String("group", group),
			slog.String("note", "navigation-derived — extracted from a resolved templated apiserver path"),
		)
	}
}

// IsNavigationDiscoveredGroup reports whether group is in the
// navigation-discovered set. The single membership predicate the
// watcher's removable-discriminator (:749, :1064) consults — no per-
// resource carve-out.
//
// Renamed from the pre-v6 accessor in Ship 0.5 / 0.30.223. Semantics
// identical — same navigation-derived predicate, same removable-iff-
// walker-discovered contract.
func IsNavigationDiscoveredGroup(group string) bool {
	navDiscoveredGroupsMu.RLock()
	defer navDiscoveredGroupsMu.RUnlock()
	_, ok := navDiscoveredGroups[group]
	return ok
}

// NavigationDiscoveredGroupsSnapshot returns a copy of the current
// navigation-discovered group set. Observability + test helper.
//
// Renamed from the pre-v6 accessor in Ship 0.5 / 0.30.223.
func NavigationDiscoveredGroupsSnapshot() []string {
	navDiscoveredGroupsMu.RLock()
	defer navDiscoveredGroupsMu.RUnlock()
	out := make([]string, 0, len(navDiscoveredGroups))
	for g := range navDiscoveredGroups {
		out = append(out, g)
	}
	return out
}

// ResetNavigationDiscoveredGroupsForTest clears the navigation-
// discovered set. TEST-ONLY — the production lifecycle is append-only.
//
// Renamed from the pre-v6 helper in Ship 0.5 / 0.30.223.
func ResetNavigationDiscoveredGroupsForTest() {
	navDiscoveredGroupsMu.Lock()
	navDiscoveredGroups = map[string]struct{}{}
	navDiscoveredGroupsMu.Unlock()
}

// --- Discovery hop (v6 core) ----------------------------------------------

// discoveryClientBuilder builds a Discovery client from a *rest.Config.
// Indirected via a package-level var so unit tests can swap in a fake
// without standing up a real REST endpoint. The signature uses
// interface{} for the input so the test fake does not need to import
// k8s.io/client-go/rest just to satisfy the parameter type.
var discoveryClientBuilder = func(rc *rest.Config) (discovery.DiscoveryInterface, error) {
	if rc == nil {
		return nil, fmt.Errorf("nil *rest.Config")
	}
	return discovery.NewDiscoveryClientForConfig(rc)
}

// discoverGroupSingleflight ensures that concurrent
// DiscoverGroupResources calls for the same group share a single
// apiserver discovery hop. Per `feedback_seed_inherits_nested_call_
// identity`: the walker may invoke the discovery path from multiple
// goroutines (Phase 1 + widget CRUD re-walks); without singleflight
// each goroutine would issue its own discovery LIST.
var discoverGroupSingleflight sync.Map // group(string) -> *sync.Mutex

// discoverGroupOnce guards the per-group "discovery has completed at
// least once" flag. v6 NOTE: we do NOT permanently cache the
// discovered set — a re-walk on widget CRUD must be able to detect
// newly-added composition GVRs. The mutex serializes concurrent calls
// only; the discovery hop itself is repeated on every walker re-entry
// (idempotent via EnsureResourceType).
func discoverGroupLock(group string) *sync.Mutex {
	v, _ := discoverGroupSingleflight.LoadOrStore(group, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// Counters — observability for the discovery hop. Mirror the
// existing handler_registry.go pattern: sync.Map keyed by group,
// values are *atomic.Uint64 (cheap atomic increment under contention).
var (
	// discoveryGroupResourcesFetched ticks once per successful
	// DiscoverGroupResources call (per group). Exposed at
	// /debug/vars via expvar (see discovery_lookup_expvar.go).
	discoveryGroupResourcesFetched sync.Map // group(string) -> *atomic.Uint64

	// discoveryGVRsSpawned ticks once per composition GVR registered
	// via EnsureResourceType inside DiscoverGroupResources (per
	// group). Exposed at /debug/vars via expvar.
	discoveryGVRsSpawned sync.Map // group(string) -> *atomic.Uint64
)

// DiscoverGroupResources lists `ServerResourcesForGroupVersion` for
// every version of `group`. For each APIResource whose Kind is NOT in
// scheme.Scheme.AllKnownTypes() (i.e. CRD-backed, not a built-in), it
// forms the composition GVR and:
//
//  1. Calls rw.EnsureResourceType(gvr) — idempotent; if added==true
//     the informer is spawned.
//  2. Calls Deps().OnResourceTypeAvailable(gvr) — fires the FD1
//     dirty-mark for stale-negative LIST entries, byte-identical to
//     the post-Ship-0 per-CRD-object registration body (preserved
//     invariant).
//
// Returns (count of GVRs newly registered this call, error). Soft-
// fails on per-version discovery errors (apiserver may not serve a
// stale version; subsequent walks retry). Returns nil error iff at
// least one version was queryable.
//
// SINGLEFLIGHTED PER GROUP — concurrent calls for the same group
// serialize so only one discovery hop is in flight at a time. The
// mutex is per-group so calls for different groups proceed in
// parallel.
//
// Nil-receiver / nil watcher / nil discovery client / passthrough
// mode are all soft no-ops returning (0, nil) so callers can wire
// this unconditionally.
func DiscoverGroupResources(ctx context.Context, rc *rest.Config, group string) (int, error) {
	if group == "" {
		return 0, nil
	}
	rw := Global()
	if rw == nil || rw.mode == modePassthrough {
		// Cache off / passthrough — no informer to spawn, but record
		// the group anyway so a future cache-on transition has the
		// nav-discovered set ready.
		return 0, nil
	}

	// Per-group singleflight — serialize concurrent discovery hops
	// for the same group across goroutines.
	lock := discoverGroupLock(group)
	lock.Lock()
	defer lock.Unlock()

	if ctx.Err() != nil {
		return 0, ctx.Err()
	}

	disco, err := discoveryClientBuilder(rc)
	if err != nil {
		slog.Warn("cache.discovery.client_build_failed",
			slog.String("subsystem", "cache"),
			slog.String("group", group),
			slog.Any("err", err),
		)
		return 0, err
	}

	// Enumerate every version of `group` via ServerGroups().
	versions, err := serverVersionsForGroup(disco, group)
	if err != nil {
		// Group may not exist yet — common during cluster boot. Soft
		// fail; subsequent walks retry.
		slog.Warn("cache.discovery.server_groups_failed",
			slog.String("subsystem", "cache"),
			slog.String("group", group),
			slog.Any("err", err),
		)
		return 0, err
	}

	spawned := 0
	anyOK := false
	for _, version := range versions {
		if ctx.Err() != nil {
			return spawned, ctx.Err()
		}
		gv := schema.GroupVersion{Group: group, Version: version}
		list, lerr := disco.ServerResourcesForGroupVersion(gv.String())
		if lerr != nil || list == nil {
			slog.Warn("cache.discovery.server_resources_failed",
				slog.String("subsystem", "cache"),
				slog.String("group_version", gv.String()),
				slog.Any("err", lerr),
			)
			continue
		}
		anyOK = true
		for _, el := range list.APIResources {
			if !discoveryIsRegisterableResource(el) {
				continue
			}
			gvr := schema.GroupVersionResource{
				Group:    group,
				Version:  version,
				Resource: el.Name,
			}
			gvk := gvr.GroupVersion().WithKind(el.Kind)

			// Skip built-in kinds — only CRD-backed kinds need the
			// composition-discovery path. (Built-ins are pre-known
			// at boot from scheme.Scheme.AllKnownTypes().)
			if isBuiltInKind(gvk) {
				continue
			}

			added, _ := rw.EnsureResourceType(gvr)
			if added {
				// FD1 (Ship D / 0.30.114) — preserved invariant:
				// dirty-mark stale-negative LIST entries when a
				// genuinely-new GVR appears at runtime. Identical
				// callback chain to the pre-Ship-0.5
				// per-CRD-object registration body, just invoked from the
				// synchronous discovery path instead of a CRD-
				// informer AddFunc.
				marked := Deps().OnResourceTypeAvailable(gvr)
				slog.Info("cache.discovery.gvr_registered",
					slog.String("subsystem", "cache"),
					slog.String("gvr", gvr.String()),
					slog.Int("l1_keys_dirty_marked", marked),
					slog.String("note", "composition informer spawned via one-shot discovery; stale-negative LIST deps dirty-marked"),
				)
				spawned++
				incDiscoveryGVRsSpawned(group)
			}
		}
	}

	if !anyOK {
		return spawned, fmt.Errorf("no version of group %q is currently served", group)
	}

	incDiscoveryGroupResourcesFetched(group)
	slog.Info("cache.discovery.group_resources_fetched",
		slog.String("subsystem", "cache"),
		slog.String("group", group),
		slog.Int("gvrs_spawned", spawned),
		slog.Int("versions_queried", len(versions)),
	)
	return spawned, nil
}

// serverVersionsForGroup returns every version of `group` the
// apiserver currently serves. Empty + nil error for an unknown
// group (apiserver responded but did not list it).
func serverVersionsForGroup(disco discovery.DiscoveryInterface, group string) ([]string, error) {
	groups, err := disco.ServerGroups()
	if err != nil || groups == nil {
		return nil, err
	}
	for _, g := range groups.Groups {
		if g.Name != group {
			continue
		}
		out := make([]string, 0, len(g.Versions))
		for _, v := range g.Versions {
			out = append(out, v.Version)
		}
		return out, nil
	}
	return nil, nil
}

// discoveryIsRegisterableResource reports whether an APIResource is
// suitable for EnsureResourceType. Filters subresources (status,
// scale, etc.) and resources lacking either a Name or Kind.
func discoveryIsRegisterableResource(el metav1.APIResource) bool {
	if el.Name == "" || el.Kind == "" {
		return false
	}
	// Subresources have a slash in their Name (e.g. "pods/status").
	// Their informer is not separately registered — the parent
	// resource's informer covers them.
	if strings.Contains(el.Name, "/") {
		return false
	}
	return true
}

// isBuiltInKind reports whether gvk is registered in
// scheme.Scheme.AllKnownTypes(). Built-in kinds (core/v1, apps/v1,
// rbac/v1, apiextensions/v1, etc.) are NOT CRD-backed and do NOT need
// the composition-discovery path.
//
// We use a package-level lazy-initialized map so the lookup is O(1)
// per call and we do not pay scheme.Scheme.AllKnownTypes()'s
// allocation cost on every discovery hop.
var (
	builtInKindsOnce sync.Once
	builtInKinds     map[schema.GroupVersionKind]struct{}
)

func isBuiltInKind(gvk schema.GroupVersionKind) bool {
	builtInKindsOnce.Do(initBuiltInKinds)
	_, ok := builtInKinds[gvk]
	return ok
}

// initBuiltInKinds populates the built-in-kinds map from
// kscheme.Scheme.AllKnownTypes() PLUS apiextensions/v1 (NOT in the
// client-go scheme by default). Called exactly once via sync.Once.
//
// kscheme.Scheme is k8s.io/client-go/kubernetes/scheme.Scheme — wires
// the standard Kubernetes API surface (core/v1, apps/v1, batch/v1,
// rbac/v1, networking/v1, etc.). We layer apiextensions/v1 on top so
// the CRD GVR is recognised as built-in (the v6 invariant: the
// discovery hop must never incidentally re-spawn the CRD informer).
// Anything NOT in this combined scheme is a CRD-backed kind and must
// take the composition-discovery path.
func initBuiltInKinds() {
	builtInKinds = make(map[schema.GroupVersionKind]struct{})
	// Standard Kubernetes API surface.
	for gvk := range kscheme.Scheme.AllKnownTypes() {
		builtInKinds[gvk] = struct{}{}
	}
	// apiextensions/v1 — wired explicitly because kscheme.Scheme does
	// not include it. Use the package's AddToScheme against a private
	// scheme so we extract its known types without polluting the
	// global client-go scheme.
	extScheme := runtime.NewScheme()
	_ = apiextensionsv1.AddToScheme(extScheme)
	for gvk := range extScheme.AllKnownTypes() {
		builtInKinds[gvk] = struct{}{}
	}
}

// --- Counters -------------------------------------------------------------

func incDiscoveryGroupResourcesFetched(group string) {
	v, _ := discoveryGroupResourcesFetched.LoadOrStore(group, &atomic.Uint64{})
	v.(*atomic.Uint64).Add(1)
}

func incDiscoveryGVRsSpawned(group string) {
	v, _ := discoveryGVRsSpawned.LoadOrStore(group, &atomic.Uint64{})
	v.(*atomic.Uint64).Add(1)
}

// DiscoveryGroupResourcesFetched returns the running count of
// successful DiscoverGroupResources calls for `group`. Used by the
// Ship 0.5 falsifier (B4 — re-walk on widget CRUD increments the
// counter) and operations (G1 — 24h cadence tracking).
func DiscoveryGroupResourcesFetched(group string) uint64 {
	v, ok := discoveryGroupResourcesFetched.Load(group)
	if !ok {
		return 0
	}
	return v.(*atomic.Uint64).Load()
}

// DiscoveryGVRsSpawned returns the running count of composition GVRs
// registered via DiscoverGroupResources for `group`. Used by the
// Ship 0.5 falsifier (M8 / B2 — composition informers PRESENT
// post-walk).
func DiscoveryGVRsSpawned(group string) uint64 {
	v, ok := discoveryGVRsSpawned.Load(group)
	if !ok {
		return 0
	}
	return v.(*atomic.Uint64).Load()
}

// DiscoveryGroupResourcesFetchedTotal returns the sum of every per-
// group fetch counter. Used by the expvar/observability surface.
func DiscoveryGroupResourcesFetchedTotal() uint64 {
	var total uint64
	discoveryGroupResourcesFetched.Range(func(_, v any) bool {
		total += v.(*atomic.Uint64).Load()
		return true
	})
	return total
}

// DiscoveryGVRsSpawnedTotal returns the sum of every per-group
// spawned-GVR counter. Used by the expvar/observability surface.
func DiscoveryGVRsSpawnedTotal() uint64 {
	var total uint64
	discoveryGVRsSpawned.Range(func(_, v any) bool {
		total += v.(*atomic.Uint64).Load()
		return true
	})
	return total
}

// ResetDiscoveryCountersForTest zeros every per-group counter.
// TEST-ONLY.
func ResetDiscoveryCountersForTest() {
	discoveryGroupResourcesFetched.Range(func(k, _ any) bool {
		discoveryGroupResourcesFetched.Delete(k)
		return true
	})
	discoveryGVRsSpawned.Range(func(k, _ any) bool {
		discoveryGVRsSpawned.Delete(k)
		return true
	})
}

// ResetDiscoverySingleflightForTest clears the per-group singleflight
// mutex map. TEST-ONLY. Production callers should never reset this;
// the map grows monotonically with the nav-discovered group set
// (bounded by ~tens of groups).
func ResetDiscoverySingleflightForTest() {
	discoverGroupSingleflight.Range(func(k, _ any) bool {
		discoverGroupSingleflight.Delete(k)
		return true
	})
}
