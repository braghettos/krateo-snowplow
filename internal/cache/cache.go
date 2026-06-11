// Package cache provides the snowplow informer-backed cache subsystem.
//
// At 0.30.4 cache=on (CACHE_ENABLED=true) eagerly registers the four
// Role-Based Access Control GVRs in the dynamic informer factory,
// starts it, and publishes the watcher via SetGlobal so EvaluateRBAC
// can serve in-process Role-Based Access Control decisions without
// ever calling SubjectAccessReview against apiserver (Revision 1
// binding).
//
// Per project_redis_removal.md the cache subsystem MUST stay removable
// via the CACHE_ENABLED env toggle. Disabled() is the single read of
// that toggle; when true the package returns nil watchers and every
// consumer falls back to the apiserver / SubjectAccessReview path.
//
// #57 — CACHE_ENABLED is the single master gate. Two formerly-standalone
// flags were folded into it (project_single_cache_flag_direction):
// startup prewarm (PrewarmEnabled, was PREWARM_ENABLED) and the
// resolver/objects informer-serve pivot (resolverUseInformer / useInformer,
// was RESOLVER_USE_INFORMER) are now IMPLICIT under this gate — on iff the
// cache subsystem is on, off iff it is off. There is no separate prewarm
// or informer-pivot env flag; stale values of the retired names are
// ignored (main.go's retired-flag audit warns once). Several sub-layer
// back-out knobs remain explicit (RESOLVED_CACHE_ENABLED,
// RESOLVED_CACHE_APISTAGE_ENABLED, WIDGET_CONTENT_L1_ENABLED, etc.).
package cache

import "os"

// Disabled reports whether the cache subsystem is disabled. When true,
// every consumer takes the apiserver branch; the informer factory is
// never instantiated; no goroutines start.
//
// Default is disabled — CACHE_ENABLED must be explicitly set to a
// truthy value ("true", "1", "yes") to enable cache plumbing. Any
// other value (including unset, empty, "false", "0", "no") is treated
// as disabled.
func Disabled() bool {
	switch os.Getenv("CACHE_ENABLED") {
	case "true", "1", "yes":
		return false
	default:
		return true
	}
}
