// admission_ceiling.go — the SHARED, zero-knob adaptive-headroom calc used by
// BOTH memory-admission gates: the (c) nested-resolve aggregate bound
// (internal/resolvers/restactions/api/nested_resolve_bound.go) and the seed-unit
// bound (internal/handlers/dispatchers/seed_bound.go).
//
// DRY EXTRACTION (fold 2026-07-03, docs/prewarm-engine-implicit-on-cache-2026-07-03.md §3.2).
// This file holds ONLY the PURE calc + the two runtime samplers + the
// reserve-fraction constant — the pieces that must be defined ONCE:
//   - AdmissionCeiling() (ceiling int64, unlimited bool): (GOMEMLIMIT − liveHeap)
//     − GOMEMLIMIT/8, the max aggregate in-flight weight admittable right now.
//   - the two prod samplers (GOMEMLIMIT via debug.SetMemoryLimit(-1); live heap
//     via runtime/metrics /memory/classes/heap/objects:bytes) + their test seams.
//
// C1 (NON-NEGOTIABLE, deadlock-safety): this file DELIBERATELY holds NO
// counters, NO cond, NO inFlightWeight, NO inFlightCount. Each gate keeps its
// OWN independent bound instance (its own mutex/cond/counters). Sharing the
// COUNTER/COND across the two gates would reintroduce self-deadlock for the
// stacked seed→nested case (one goroutine holding the seed admission then
// synchronously entering the nested admission on the SAME semaphore would wait
// on itself). The two gates are INDEPENDENT semaphores reading the SAME
// headroom denominator — over-reservation when they stack, which is
// conservative-safe (admits LESS aggregate, never more) and deadlock-free
// (each has its own inFlightCount==0 unconditional escape).
//
// ZERO-KNOB: no env. reserve is a documented code-constant FRACTION of
// GOMEMLIMIT; GOMEMLIMIT is read via debug.SetMemoryLimit(-1) (respects the
// cgroup/env-derived value). Transparent when GOMEMLIMIT is unset (the runtime
// default math.MaxInt64) → unlimited==true, both gates no-op
// (project_caching_is_provisional: cleanly removable).
package cache

import (
	"math"
	"runtime/debug"
	"runtime/metrics"
	"sync"
)

const (
	// reserveFractionNum/reserveFractionDen express `reserve` as a FRACTION of
	// GOMEMLIMIT held back for NON-resolve work (informer caches, HTTP buffers,
	// the L1/apistage stores, GC working set). 1/8 = 12.5% headroom-share — a
	// dimensionless share of the pod's own limit that scales with pod size,
	// defensible as "keep headroom for the rest of the process" (unlike an
	// absolute byte budget). Documented code constant (zero-knob, Diego
	// 2026-07-02). Kept identical to the value nested_resolve_bound.go used.
	admissionReserveFractionNum int64 = 1
	admissionReserveFractionDen int64 = 8
)

// admissionRuntimeLimitFn / admissionLiveHeapFn are the runtime-headroom TEST
// SEAMS for the SHARED calc. Production wires them to debug.SetMemoryLimit(-1)
// and the runtime/metrics live-heap sample; falsifiers in EITHER gate's package
// inject deterministic GOMEMLIMIT / liveHeap via SetAdmissionRuntimeSeamsForTest
// WITHOUT a resurrected env. Guarded by admissionSeamMu when swapped in tests.
var (
	admissionSeamMu     sync.Mutex
	admissionRuntimeLimitFn func() int64 = prodRuntimeMemoryLimit
	admissionLiveHeapFn     func() int64 = prodLiveHeapBytes
)

// prodRuntimeMemoryLimit reads the effective GOMEMLIMIT (soft limit) via
// debug.SetMemoryLimit(-1), which returns the current limit without changing it
// (respects the env/cgroup-derived value). The runtime default when unset is
// math.MaxInt64 (effectively unlimited).
func prodRuntimeMemoryLimit() int64 {
	return debug.SetMemoryLimit(-1)
}

// prodLiveHeapBytes samples live heap objects via runtime/metrics
// (/memory/classes/heap/objects:bytes) — a cheap read, no stop-the-world
// (unlike runtime.ReadMemStats). This is the live (in-use) heap, the quantity
// GC cannot shrink while units are in flight — the OOM-relevant denominator.
func prodLiveHeapBytes() int64 {
	const key = "/memory/classes/heap/objects:bytes"
	sample := []metrics.Sample{{Name: key}}
	metrics.Read(sample)
	if sample[0].Value.Kind() != metrics.KindUint64 {
		return 0
	}
	v := sample[0].Value.Uint64()
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}

// AdmissionCeiling returns (ceiling, unlimited). ceiling = headroom − reserve =
// (GOMEMLIMIT − liveHeap) − GOMEMLIMIT/8, the max aggregate in-flight weight a
// gate may admit right now. unlimited==true when GOMEMLIMIT is the runtime's
// unset default (math.MaxInt64) → no soft ceiling to protect, the gate is
// transparent. Recompute-per-admission: each caller samples fresh at admission
// (no cached ceiling).
//
// PURE calc — reads only the (seamable) runtime samplers, mutates nothing. Both
// gates call it; neither shares state THROUGH it.
func AdmissionCeiling() (ceiling int64, unlimited bool) {
	admissionSeamMu.Lock()
	limitFn := admissionRuntimeLimitFn
	liveFn := admissionLiveHeapFn
	admissionSeamMu.Unlock()

	limit := limitFn()
	if limit <= 0 || limit == math.MaxInt64 {
		return 0, true // GOMEMLIMIT unset/unlimited — transparent pass-through.
	}
	reserve := limit / admissionReserveFractionDen * admissionReserveFractionNum
	headroom := limit - liveFn()
	ceiling = headroom - reserve
	if ceiling < 0 {
		ceiling = 0
	}
	return ceiling, false
}

// AdmissionLiveHeapSample exposes the shared live-heap sampler so a gate's
// per-unit calibration (measured live-heap delta) reads the SAME denominator as
// AdmissionCeiling. Seamable in tests via SetAdmissionRuntimeSeamsForTest.
func AdmissionLiveHeapSample() int64 {
	admissionSeamMu.Lock()
	liveFn := admissionLiveHeapFn
	admissionSeamMu.Unlock()
	return liveFn()
}

// SetAdmissionRuntimeSeamsForTest injects deterministic GOMEMLIMIT + live-heap
// samplers so a falsifier in EITHER gate's package drives admission by BYTE
// headroom without a resurrected env. limitFn/liveFn may close over test state
// (e.g. a live-heap counter that tracks admitted weight). Test-only. Returns a
// restore closure.
func SetAdmissionRuntimeSeamsForTest(limitFn, liveFn func() int64) func() {
	admissionSeamMu.Lock()
	prevLimit := admissionRuntimeLimitFn
	prevLive := admissionLiveHeapFn
	admissionRuntimeLimitFn = limitFn
	admissionLiveHeapFn = liveFn
	admissionSeamMu.Unlock()
	return func() {
		admissionSeamMu.Lock()
		admissionRuntimeLimitFn = prevLimit
		admissionLiveHeapFn = prevLive
		admissionSeamMu.Unlock()
	}
}

// ResetAdmissionRuntimeSeamsForTest restores the production samplers. Test-only.
func ResetAdmissionRuntimeSeamsForTest() {
	admissionSeamMu.Lock()
	admissionRuntimeLimitFn = prodRuntimeMemoryLimit
	admissionLiveHeapFn = prodLiveHeapBytes
	admissionSeamMu.Unlock()
}
