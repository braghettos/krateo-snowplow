// controller_health_expvar.go — Ship Resilience-1 (0.30.162). expvar
// publication for the two new gauges:
//
//   - snowplow_upstream_controller_health  → map[ns/name]ControllerHealthEntry
//   - snowplow_upstream_webhook_failurepolicy → map[webhookName]WebhookFailurePolicyEntry
//
// Mirrors fallthrough_meter_expvar.go shape: register handles in
// init(); each handle's value is a closure that loads the latest
// immutable snapshot via atomic.Pointer and returns map(s) suitable
// for JSON marshalling.
//
// HG-5 discharge (legacy, pre-CFG-1): when Disabled() the closures
// returned an empty map. This was "gauges present but empty".
//
// CFG-1 (Ship 0.30.163) — cache-off compliance per project memory
// `project_cache_off_is_transparent_fallback`. Diego's 2026-05-22
// contract supersedes HG-5: "there is no cache with cache_enabled=false".
// Under CACHE_ENABLED=false the cache subsystem does not exist and
// these gauges MUST NOT be registered (so they don't appear at
// /debug/vars at all). The closure-level Disabled() guards are kept
// as defense-in-depth in case of runtime flip (rare); the boot-time
// guard is the primary mechanism.
//
// HG-6 discharge: the values published are NAME + COUNT only — no
// raw object bodies, no .clientConfig.caBundle, no auth tokens. The
// closures construct fresh maps each call; readers cannot mutate
// the snapshot.
package cache

import (
	"expvar"
	"sync"
)

// controllerHealthExpvarOnce guards registerControllerHealthExpvar so
// the registration body runs at most once per process. See the
// matching sync.Once in fallthrough_meter_expvar.go for rationale.
var controllerHealthExpvarOnce sync.Once

func init() {
	// CFG-1: under CACHE_ENABLED=false, no cache subsystem exists →
	// gauges must not be registered. init() runs once per process so
	// this branch cannot be unit-tested in-process; falsifier is
	// HG-321 (4-env-value matrix process spawn, see
	// e2e/bench/cfg1_falsifier.sh).
	if Disabled() {
		return
	}
	registerControllerHealthExpvar()
}

// registerControllerHealthExpvar performs the two expvar.Publish
// calls for the controller-health gauges. Guarded by
// controllerHealthExpvarOnce so it is safe to call from both init()
// and the test helper.
func registerControllerHealthExpvar() {
	controllerHealthExpvarOnce.Do(func() {
		expvar.Publish("snowplow_upstream_controller_health", expvar.Func(controllerHealthExpvarValue))
		expvar.Publish("snowplow_upstream_webhook_failurepolicy", expvar.Func(webhookFailurePolicyExpvarValue))
	})
}

// controllerHealthExpvarValue is the closure body for the
// controller_health gauge. Returns an empty map when:
//   - cache=off (Disabled()=true),
//   - no snapshot has been published yet (pre-readiness),
//   - the subsystem is not started.
//
// Every call constructs a fresh map; the snapshot's underlying map
// is never exposed to the marshaller (defense-in-depth — readers
// can't accidentally mutate the immutable snapshot).
func controllerHealthExpvarValue() any {
	if Disabled() {
		return map[string]ControllerHealthEntry{}
	}
	s := controllerHealthSnap.Load()
	if s == nil || s.Controllers == nil {
		return map[string]ControllerHealthEntry{}
	}
	out := make(map[string]ControllerHealthEntry, len(s.Controllers))
	for k, v := range s.Controllers {
		out[k] = v
	}
	return out
}

// webhookFailurePolicyExpvarValue is the closure body for the
// webhook_failurepolicy gauge. Empty map when cache=off / no
// snapshot.
func webhookFailurePolicyExpvarValue() any {
	if Disabled() {
		return map[string]WebhookFailurePolicyEntry{}
	}
	s := controllerHealthSnap.Load()
	if s == nil || s.Webhooks == nil {
		return map[string]WebhookFailurePolicyEntry{}
	}
	out := make(map[string]WebhookFailurePolicyEntry, len(s.Webhooks))
	for k, v := range s.Webhooks {
		out[k] = v
	}
	return out
}
