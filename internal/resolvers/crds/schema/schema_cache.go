// schema_cache.go — Task #323 (#318-R2 Commit 2-B). Per-GVR compiled-CRD-
// schema memo for ValidateObjectStatus.
//
// WHY — for every child GET, ValidateObjectStatus (schema.go:126) re-runs
// extractOpenAPISchemaFromCRD (extract.go:11) + buildValidationFromSchemaData
// (validate.go:13: a YAML marshal + YAML unmarshal into apiextv1.JSONSchema
// Props + Convert_v1_..._To_apiextensions_... convert) to recompile the SAME
// CRD's OpenAPI schema. TRACED 6.28s / 0.91% of the 0.30.258 Step-2 drain
// profile (docs/task-318-r2-drain-cost-trim-design-2026-06-11.md §1.1 cost
// center B). The compiled *apiextensions.CustomResourceValidation is a pure
// function of (CRD bytes, version) and is INVARIANT per GVR across a window:
// every compositions-panels child re-fetches + re-parses the same CRD. This
// file memoises the compiled CRV per composition GVR so the parse is paid
// ONCE per GVR per process lifetime (until invalidated), and a warm
// ValidateObjectStatus is just the per-object validateCustomResource
// (NewSchemaValidator + ValidateCustomResource against THIS object's
// status.widgetData — kept per-call, see below) plus a map lookup.
//
// WHAT IS MEMOISED / WHAT STAYS PER-CALL — only the compiled CRV (steps 2+3
// of the design §2: the CRD bytes + the v1->internal validation build). The
// per-object validateCustomResource (validate.go:104) — NewSchemaValidator(
// crv.OpenAPIV3Schema) + ValidateCustomResource(doc) against the object's own
// widgetData — STAYS per-call: the widgetData differs per child and is the
// only genuinely per-object step (TRACED 0.69s, design §2 step 4). We do NOT
// cache the compiled validation.SchemaValidator (a Redis-era cache at
// 6a0877d did; this is deliberately narrower — the SchemaValidator build is
// cheap inside the 0.69s and a per-call rebuild keeps the never-change-output
// contract trivially honest: identical CRV in => identical validation out).
//
// KEY SHAPE — keyed per composition GVR (schema.GroupVersionResource). One
// CRD per GVR; the CRD's OpenAPI schema is K8s-immutable for the CRD-object
// lifetime (same invariant plurals_resolver.go relies on). The natural,
// structural key — no path/resource/user literal (feedback_no_special_cases).
//
// INVALIDATION — bridge-keyed FULL RESET + a generation fence against
// inflight re-installs. InvalidateCRDSchemaMemo() (a) bumps a monotonic
// generation counter then (b) clears the ENTIRE memo. It is wired (in
// main.go) to the EXISTING 0.30.233 CRD-lifecycle bridge
// (internal/cache/crd_discovery_side_effect.go) via the SAME trampoline as
// Commit 1's SA-discovery invalidator (cache.SetCRDSchemaInvalidator, fired
// right after cache.invalidateSADiscovery() at the END of triggerCRDDiscovery
// [ADD/UPDATE] + triggerCRDDelete [DELETE], AFTER DiscoverGroupResources /
// teardown). Rationale for a FULL reset over a per-key selective evict (the
// design's stated rule): a CRD spec UPDATE that changes the schema MUST
// invalidate, and the bridge fires on UPDATE (crdLifecycleUpdate ->
// triggerCRDDiscovery, deps_watch.go:234); a bridge-keyed full reset fires
// exactly on the events that mutate any CRD schema (ADD/UPDATE/DELETE), needs
// no CRD UID/resourceVersion read to scope the evict, and the memo is small
// (one CRV per composition GVR class — a handful at customer scale) so a
// rebuild-on-next-miss is cheap. The ONLY runtime mutation of the GVR set /
// CRD schemas snowplow resolves through ValidateObjectStatus is the CRD
// lifecycle; snowplow serves no aggregated APIs (no apiregistration /
// APIService handling anywhere in internal/) — same Q3-a narrowing as
// cached_client.go.
//
// THE GENERATION FENCE — why a full reset alone is NOT sufficient, and how
// this differs from Commit 1's mapper.Reset() (these mechanisms are NOT
// equivalent — the earlier "mirroring mapper.Reset()" claim was inaccurate):
// Commit 1's reset NILs the restmapper delegate, and the fill is done LAZILY
// BY CLIENT-GO inside the next KindFor (getDelegate re-downloads when
// delegate==nil, under client-go's initMu) — so after a reset the next access
// always re-pulls fresh, regardless of interleaving (self-healing). This
// memo's fill is an APPLICATION-level store on the miss path, which can RACE
// the async bridge reset: if InvalidateCRDSchemaMemo() fires AFTER an inflight
// ValidateObjectStatus has read OLD CRD bytes (its CRD GET reached the
// apiserver before the UPDATE landed) but BEFORE that call stores, a sticky
// LoadOrStore would re-install the STALE CRV and — with no guaranteed later
// reset for that GVR — it would persist for the PROCESS LIFETIME. That is a
// real, non-self-healing staleness hole (architect A1). The fence closes it:
// the miss path snapshots the generation BEFORE the CRD GET (currentSchemaGen),
// and storeCRDSchema installs ONLY if the generation is UNCHANGED at store
// time. A reset between snapshot and store moves the generation -> the store
// is DROPPED, and the next call misses and recompiles from now-fresh bytes
// (self-healing, like Commit 1, but via the application-level fence instead of
// client-go's lazy re-pull). This makes "a hit cannot serve a stale schema for
// a changed GVR" actually hold under concurrent /call during a CRD install.
//
// IDENTITY — the cached CRV is compiled FROM the CRD schema: cluster-shape
// metadata, identity-independent. The CRD GET that feeds it is a cluster-
// scoped read of apiextensions.k8s.io/v1/customresourcedefinitions performed
// with the SA identity for every user already; the compiled schema is the
// envelope shape snowplow validates against, NOT per-user data. It sits BELOW
// the per-user RBAC layer. Sharing one compiled CRV across users changes no
// user's visible data (same RBAC argument as Commit 1's header; this is NOT a
// per-user data cache — feedback_l1_per_user_keyed_never_cohort guards
// user-data L1, this is shape metadata).
//
// CONCURRENCY — a sync.Map read concurrently by every ValidateObjectStatus
// goroutine (drain walker + N customer /call) and reset by the CRD-lifecycle
// bridge worker (InvalidateCRDSchemaMemo, on a SEPARATE goroutine). sync.Map is
// race-safe by construction; the generation counter is atomic. The
// shared-vs-private conversion (the 0.30.128 hazard class — per-call
// recompiled CRV -> one shared cached CRV) is gated by two concurrent -race
// tests (schema_cache_race_test.go): a readers-vs-reset test and an
// inflight-fill-vs-reset test, both running InvalidateCRDSchemaMemo() on a
// SEPARATE goroutine (feedback_shared_vs_copy_is_a_concurrency_change). The
// cached *CRV is treated as IMMUTABLE after store: validateCustomResource only
// READS crv.OpenAPIV3Schema (NewSchemaValidator), never mutates it, so
// concurrent readers of one shared *CRV need no extra lock. The generation
// fence (above) handles the one mutation surface a plain sync.Map cannot: an
// inflight miss-fill re-installing a value compiled from pre-reset bytes.
//
// REMOVABLE — pure acceleration. On miss, ValidateObjectStatus computes the
// CRV exactly as before (the extract + build path is unchanged); the memo
// only short-circuits the recompute on a hit. With the memo emptied (cache-
// off / never-warmed), every call recomputes — identical behaviour to today
// (project_caching_is_provisional).

package schema

import (
	"sync"
	"sync/atomic"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	runtimeschema "k8s.io/apimachinery/pkg/runtime/schema"
)

// crdSchemaMemo maps a composition GVR -> its compiled, immutable-after-store
// *apiextensions.CustomResourceValidation. sync.Map fits the workload: write-
// once-per-GVR then read-mostly, with a rare full clear on a CRD lifecycle
// event. Keyed structurally on the GVR (no special-case literal).
var crdSchemaMemo sync.Map // map[runtimeschema.GroupVersionResource]*apiextensions.CustomResourceValidation

// crdSchemaMemoGen is the monotonic generation counter that fences inflight
// fills against a concurrent reset (architect A1). InvalidateCRDSchemaMemo()
// bumps it BEFORE clearing the map; the miss path snapshots it via
// currentSchemaGen() BEFORE the CRD GET, and storeCRDSchema drops/undoes a
// store whose snapshot is stale. This closes the invalidate-vs-inflight-fill
// staleness hole that a sticky LoadOrStore alone leaves open.
var crdSchemaMemoGen atomic.Uint64

// crdSchemaMemo counters — observability for the falsifier (hit/miss ratio,
// reset count) and the fenced-store drop count. atomic for lock-free reads;
// PROFILE-ONLY this ship — NOT exposed via expvar/'/debug/vars'. Test-only
// snapshot below.
var (
	crdSchemaMemoHits         atomic.Uint64
	crdSchemaMemoMisses       atomic.Uint64
	crdSchemaMemoResets       atomic.Uint64
	crdSchemaMemoStaleDropped atomic.Uint64 // fenced stores dropped because a reset moved the generation
)

// currentSchemaGen snapshots the memo generation. The miss path calls this
// BEFORE the CRD GET so storeCRDSchema can detect a reset that landed during
// the GET+compile window and drop the (potentially stale) store.
func currentSchemaGen() uint64 {
	return crdSchemaMemoGen.Load()
}

// lookupCRDSchema returns the cached compiled CRV for gvr, or (nil, false) on
// a miss. Hot-path read on every ValidateObjectStatus.
func lookupCRDSchema(gvr runtimeschema.GroupVersionResource) (*apiextensions.CustomResourceValidation, bool) {
	v, ok := crdSchemaMemo.Load(gvr)
	if !ok {
		crdSchemaMemoMisses.Add(1)
		return nil, false
	}
	crdSchemaMemoHits.Add(1)
	crv, _ := v.(*apiextensions.CustomResourceValidation)
	return crv, crv != nil
}

// storeCRDSchema memoises the compiled CRV for gvr UNDER THE GENERATION FENCE
// (architect A1). gen is the generation snapshot the caller took via
// currentSchemaGen() BEFORE the CRD GET that produced crv. The CRV MUST be
// treated as immutable after this call (validateCustomResource only reads it).
//
// Fence semantics — a store is valid only if NO reset happened between the
// caller's snapshot and the store:
//   - If the generation already moved at entry (gen != current), the crv may
//     be compiled from PRE-reset (stale) bytes -> DROP the store entirely.
//   - Otherwise LoadOrStore (a concurrent miss-then-build race for the same
//     gen converges on one CRV — identical input => identical CRV), THEN
//     re-check the generation. If a reset raced in AFTER our LoadOrStore, the
//     reset's clear may have run before OR after our store; either way our
//     snapshot is now stale, so DELETE the entry we just (maybe) installed.
//     The next miss recompiles from fresh bytes (self-healing).
//
// This is airtight against InvalidateCRDSchemaMemo()'s bump-then-clear order:
// the reset bumps the generation BEFORE clearing, so any store that observes
// the old generation at the post-store re-check is guaranteed to undo itself,
// and any store that runs before the clear is removed by the clear.
func storeCRDSchema(gvr runtimeschema.GroupVersionResource, crv *apiextensions.CustomResourceValidation, gen uint64) {
	if crv == nil {
		return
	}
	if crdSchemaMemoGen.Load() != gen {
		// A reset landed during the GET+compile window -> the crv may be
		// stale. Do NOT install it.
		crdSchemaMemoStaleDropped.Add(1)
		return
	}
	crdSchemaMemo.LoadOrStore(gvr, crv)
	// Re-check AFTER the store: a reset that raced in between the gen check
	// above and the LoadOrStore would otherwise leave our (possibly stale)
	// entry installed past the reset. Undo it.
	if crdSchemaMemoGen.Load() != gen {
		crdSchemaMemo.Delete(gvr)
		crdSchemaMemoStaleDropped.Add(1)
	}
}

// InvalidateCRDSchemaMemo clears the ENTIRE per-GVR compiled-schema memo so
// the next ValidateObjectStatus per GVR recompiles from fresh CRD bytes.
// Wired (in main.go, via cache.SetCRDSchemaInvalidator) to the END of the
// CRD-lifecycle bridge's triggerCRDDiscovery (ADD/UPDATE) and triggerCRDDelete
// (DELETE) — AFTER DiscoverGroupResources / teardown — so a mid-run CRD
// install/change/removal cannot leave a stale compiled schema. Full reset
// (not per-key) is the simple-correct shape: it fires exactly on the events
// that mutate any CRD schema and needs no per-CRD identity read to scope the
// evict (see file header).
//
// GENERATION FENCE ORDER (architect A1) — bump the generation FIRST, THEN
// clear the map. The bump-before-clear order is load-bearing: it guarantees an
// inflight storeCRDSchema (which snapshotted the OLD generation before its CRD
// GET) sees the moved generation at its post-store re-check and undoes its
// (possibly stale) install — regardless of whether that store ran before or
// after the clear below. This is what makes the reset self-healing for inflight
// fills (a sticky LoadOrStore alone is not — see the file header).
//
// No-op-safe before any entry exists. Goroutine-safe (atomic bump +
// sync.Map.Range + Delete). Bumps crdSchemaMemoResets for the falsifier.
func InvalidateCRDSchemaMemo() {
	crdSchemaMemoResets.Add(1)
	crdSchemaMemoGen.Add(1) // BEFORE the clear — fences inflight stores
	crdSchemaMemo.Range(func(k, _ any) bool {
		crdSchemaMemo.Delete(k)
		return true
	})
}

// CRDSchemaMemoStats is a read-only snapshot of the memo counters. TEST-ONLY
// surface (the falsifier asserts hit/miss/reset/stale-drop transitions); NOT
// wired to expvar/'/debug/vars' in this ship — observability is profile-only
// (the validation run asserts extractOpenAPISchemaFromCRD ≈ 0 under the drain
// focus, NOT an expvar hit-rate). Wiring snowplow_crd_schema_memo_* counters
// is a tracked follow-up.
type CRDSchemaMemoStats struct {
	Hits         uint64
	Misses       uint64
	Resets       uint64
	StaleDropped uint64
}

// crdSchemaMemoStats returns the current counters. Package-private — exposed
// to tests via the same package.
func crdSchemaMemoStats() CRDSchemaMemoStats {
	return CRDSchemaMemoStats{
		Hits:         crdSchemaMemoHits.Load(),
		Misses:       crdSchemaMemoMisses.Load(),
		Resets:       crdSchemaMemoResets.Load(),
		StaleDropped: crdSchemaMemoStaleDropped.Load(),
	}
}

// resetCRDSchemaMemoForTest clears the memo, the generation, AND the counters
// so each test sees a fresh state. TEST-ONLY — production lifecycle is
// full-reset-on-bridge-event only. Mirrors resetSADiscoveryForTest
// (dynamic/cached_client.go).
func resetCRDSchemaMemoForTest() {
	crdSchemaMemo.Range(func(k, _ any) bool {
		crdSchemaMemo.Delete(k)
		return true
	})
	crdSchemaMemoGen.Store(0)
	crdSchemaMemoHits.Store(0)
	crdSchemaMemoMisses.Store(0)
	crdSchemaMemoResets.Store(0)
	crdSchemaMemoStaleDropped.Store(0)
}
