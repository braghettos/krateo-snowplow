// crd_gvr.go — Ship 0.30.233. The single, centralised CRD-meta-GVR
// predicate consulted by the deps_watch.go AddFunc when deciding
// whether to dispatch a CRD-discovery side-effect.
//
// WHY a predicate, not an inline literal — per
// `feedback_no_special_cases.md`: the rule forbids hardcoded
// path/resource/user CASES in resolver Go. Recognising the CRD-meta
// GVR (the ONLY GVR whose Add event needs to drive group-discovery
// because no other event source bridges the informer-event API to
// the discovery API) is necessary plumbing that the type system
// requires SOME literal for. We park that literal in ONE place so a
// future audit is single-point — matching how `apiextensionsv1` is
// already referenced at discovery_lookup.go:43, phase1.go (header),
// plurals_resolver.go:63, cache_mode.go:58 — every other reference
// to the CRD GVR in the cache package consumes IsCRDGVR rather than
// reconstructing the literal.
//
// This file MUST NOT grow other "special-case" predicates. If a
// future ship needs another GVR-specific behaviour, it goes through
// the declarative handler-extension registry
// (handler_registry.go) — not a new predicate here.

package cache

import "k8s.io/apimachinery/pkg/runtime/schema"

// crdGVR is the apiextensions.k8s.io/v1 CustomResourceDefinition
// GroupVersionResource. The CRD-meta informer is registered lazily
// by the walker (lazyRegisterInnerCallPaths) — this constant is
// the predicate match-target, NOT a registration driver.
var crdGVR = schema.GroupVersionResource{
	Group:    "apiextensions.k8s.io",
	Version:  "v1",
	Resource: "customresourcedefinitions",
}

// IsCRDGVR reports whether gvr is the CRD-meta GVR. The single
// predicate the deps_watch.go AddFunc consults when routing a
// CRD-ADD event to the discovery side-effect.
//
// Structural — equality against a typed schema.GroupVersionResource
// value. No string-build / no resource-name regex / no group-prefix
// substring match — the predicate is byte-equal or false.
func IsCRDGVR(gvr schema.GroupVersionResource) bool {
	return gvr == crdGVR
}

// CRDGVRForTest returns the CRD-meta GVR. TEST-ONLY accessor —
// production code MUST consume IsCRDGVR. The exported test helper
// keeps the literal out of test files.
func CRDGVRForTest() schema.GroupVersionResource {
	return crdGVR
}
