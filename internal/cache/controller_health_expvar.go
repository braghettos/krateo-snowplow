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
// HG-5 discharge: when Disabled() (CACHE_ENABLED=false) every closure
// returns the EMPTY map — the gauges are present in /debug/vars but
// carry zero entries. The absence of any controllerHealthStarted log
// + the empty maps together signal inert.
//
// HG-6 discharge: the values published are NAME + COUNT only — no
// raw object bodies, no .clientConfig.caBundle, no auth tokens. The
// closures construct fresh maps each call; readers cannot mutate
// the snapshot.
package cache

import "expvar"

func init() {
	expvar.Publish("snowplow_upstream_controller_health", expvar.Func(controllerHealthExpvarValue))
	expvar.Publish("snowplow_upstream_webhook_failurepolicy", expvar.Func(webhookFailurePolicyExpvarValue))
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
