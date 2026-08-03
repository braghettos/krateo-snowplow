// otel_accessors.go — exported, read-only accessor over the per-GVR
// compiled-CRD-schema memo counters (package-private crdSchemaMemoStats),
// so the internal/metrics OTLP mirror (a separate package) can observe the
// SAME live snapshot the snowplow_crd_schema_memo_* expvar closures read.
//
// ADDITIVE: a pure read accessor. It changes no incrementer and leaves the
// expvar surface untouched.

package schema

// CRDSchemaMemoSnapshot returns the compiled-CRD-schema memo counters,
// mirroring snowplow_crd_schema_memo_hits_total / _misses_total /
// _stale_dropped_total / _invalidations_total (Resets is published as
// _invalidations_total). It is a four-atomic Load read on demand.
func CRDSchemaMemoSnapshot() (hits, misses, staleDropped, invalidations uint64) {
	s := crdSchemaMemoStats()
	return s.Hits, s.Misses, s.StaleDropped, s.Resets
}
