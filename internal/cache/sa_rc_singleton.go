// sa_rc_singleton.go — Ship 0.30.233. Process-scoped accessor for
// the snowplow SERVICE-ACCOUNT *rest.Config, wired ONCE at main.go
// startup (immediately after rest.InClusterConfig() succeeds).
//
// WHY a singleton, not a per-call ctx — the CRD-ADD discovery side-
// effect added by Ship 0.30.233 runs on the informer processor
// goroutine, which has no per-call ctx carrying the
// InternalRESTConfigFromContext value. Pre-Ship-0.30.233 the only
// site that called DiscoverGroupResources was resolve.go:990 inside
// the walker — that site DOES have a ctx with an attached rc. The
// CRD-ADD-side-effect call site does NOT, so we route through this
// process singleton.
//
// MIRRORS the existing patterns:
//   - cache.SetGlobal / cache.Global() (watcher singleton, main.go:205)
//   - watcher.SetRESTConfig (per-watcher rc, main.go:215)
//   - watcher.SetMetadataClient (metadata client, similar lifecycle)
//
// All wired at the same main.go startup block. The setter is
// idempotent; production calls it exactly once. Test code may call
// it from a setup helper — the RWMutex makes that race-safe.

package cache

import (
	"sync"

	"k8s.io/client-go/rest"
)

// processSARC holds the process-wide service-account *rest.Config
// wired by main.go. nil until SetProcessSARestConfig is called.
//
// Guarded by processSARCMu (not Global()'s mutex — that mutex
// already serialises ResourceWatcher singleton access; layering
// this onto the same lock would create lock-ordering risk).
var (
	processSARCMu sync.RWMutex
	processSARC   *rest.Config
)

// SetProcessSARestConfig records the process-wide SA *rest.Config.
// Called ONCE by main.go's cache-init block, immediately after
// rest.InClusterConfig() returns successfully. Subsequent calls
// overwrite — production never re-invokes; tests may.
//
// Nil rc is accepted (stores nil) — a future cache-off transition
// can clear the singleton without panicking the CRD-ADD path
// (ProcessSARestConfig returns nil, triggerCRDDiscovery soft-fails).
func SetProcessSARestConfig(rc *rest.Config) {
	processSARCMu.Lock()
	processSARC = rc
	processSARCMu.Unlock()
}

// ProcessSARestConfig returns the process-wide SA *rest.Config or
// nil if not yet wired. Read by triggerCRDDiscovery on the
// CRD-ADD path — soft-fails (skips discovery) when nil.
//
// Cheap: RLock + pointer read. Safe for concurrent use.
func ProcessSARestConfig() *rest.Config {
	processSARCMu.RLock()
	defer processSARCMu.RUnlock()
	return processSARC
}

// ResetProcessSARestConfigForTest zeros the singleton. TEST-ONLY —
// production lifecycle is set-once at startup.
func ResetProcessSARestConfigForTest() {
	processSARCMu.Lock()
	processSARC = nil
	processSARCMu.Unlock()
}
