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
