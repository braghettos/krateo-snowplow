// discovery_invalidation_hook.go — Task #322 (#318-R2) Commit 1. The
// cache->dynamic invalidation indirection for the SA-singleton cached
// discovery client.
//
// WHY an indirection, not a direct call — layering. internal/cache is
// BELOW internal/dynamic (dynamic imports cache via schema.go, never the
// reverse). The CRD-lifecycle bridge (crd_discovery_side_effect.go) lives
// in cache and must invalidate the dynamic-package SA discovery singleton
// AFTER it runs DiscoverGroupResources / teardown. We cannot import
// dynamic from cache without an import cycle, so main.go injects the
// invalidator at startup:
//
//	cache.SetSADiscoveryInvalidator(dynamic.InvalidateSADiscovery)
//
// The bridge then calls invalidateSADiscovery() (the package-private
// trampoline) at the end of triggerCRDDiscovery + triggerCRDDelete.
//
// MIRRORS the existing cache->dispatchers hook indirection
// (notifyGVRDiscoveredForReprewarm, discovery_lookup.go) and the
// process-singleton storage shape of sa_rc_singleton.go (RWMutex-guarded
// pointer, set-once at startup, soft no-op when unset).
//
// ORDERING CONTRACT (F-4 safety) — the bridge invalidates AFTER
// DiscoverGroupResources (ADD/UPDATE) and AFTER teardown (DELETE), so the
// next ValidateObjectStatus for a new/changed/removed GVR rebuilds the
// mapper against fresh cluster shape. A stale discovery cache CANNOT
// persist past a CRD lifecycle event — which is the ONLY runtime mutation
// of the GVR set snowplow resolves through ValidateObjectStatus (snowplow
// serves no aggregated APIs). This does NOT re-create the S4/F-4
// stuck-zero class: discovery FIRES (via the bridge) AND the cache is
// invalidated in lockstep.

package cache

import (
	"log/slog"
	"sync"
)

// saDiscoveryInvalidator is the process-wide invalidation callback wired
// by main.go to dynamic.InvalidateSADiscovery. nil until
// SetSADiscoveryInvalidator is called — invalidateSADiscovery soft-no-ops
// when unset (cache-off / test paths that do not stand up the dynamic
// singleton).
//
// Guarded by its own mutex (not Global()'s) to avoid lock-ordering risk —
// same rationale as processSARCMu (sa_rc_singleton.go:37-40).
var (
	saDiscoveryInvalidatorMu sync.RWMutex
	saDiscoveryInvalidator   func()
)

// SetSADiscoveryInvalidator records the process-wide SA-discovery
// invalidation callback. Called ONCE by main.go's cache-init block, next
// to SetProcessSARestConfig. Subsequent calls overwrite — production
// never re-invokes; tests may.
//
// nil f is accepted (stores nil) so a future cache-off transition can
// clear the hook without the bridge panicking.
func SetSADiscoveryInvalidator(f func()) {
	saDiscoveryInvalidatorMu.Lock()
	saDiscoveryInvalidator = f
	saDiscoveryInvalidatorMu.Unlock()
}

// saDiscoveryNilHookWarnOnce rate-limits the unwired-hook warning to one
// line per process life.
var saDiscoveryNilHookWarnOnce sync.Once

// invalidateSADiscovery fires the wired SA-discovery invalidation
// callback. Called at the END of triggerCRDDiscovery (ADD/UPDATE, after
// DiscoverGroupResources) and triggerCRDDelete (DELETE, after teardown).
//
// A nil hook here means main.go's wiring line was dropped: the bridge
// only runs with the cache subsystem up, where the invalidator MUST be
// wired — otherwise the SA discovery cache goes silently stale on CRD
// lifecycle events (the F-4/S4 class this mechanism forecloses). Warn
// loudly (once), mirroring the no_sa_rc sibling warning above.
//
// Cheap: RLock + nil-check + call. Safe for concurrent use.
func invalidateSADiscovery() {
	saDiscoveryInvalidatorMu.RLock()
	f := saDiscoveryInvalidator
	saDiscoveryInvalidatorMu.RUnlock()
	if f == nil {
		saDiscoveryNilHookWarnOnce.Do(func() {
			slog.Warn("cache.crd_discovery.sa_discovery_invalidator_unwired",
				slog.String("subsystem", "cache"),
				slog.String("hint", "SetSADiscoveryInvalidator was not called at "+
					"startup — the SA discovery cache will NOT invalidate on CRD "+
					"lifecycle events. Check main.go wiring "+
					"(cache.SetSADiscoveryInvalidator(dynamic.InvalidateSADiscovery))."))
		})
		return
	}
	f()
}

// ResetSADiscoveryInvalidatorForTest zeros the hook. TEST-ONLY —
// production lifecycle is set-once at startup.
func ResetSADiscoveryInvalidatorForTest() {
	saDiscoveryInvalidatorMu.Lock()
	saDiscoveryInvalidator = nil
	saDiscoveryInvalidatorMu.Unlock()
}
