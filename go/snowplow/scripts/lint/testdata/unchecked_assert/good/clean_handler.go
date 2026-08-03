// scripts/lint/testdata/unchecked_assert/good/clean_handler.go — Ship H6
// lint-gate GOOD fixture (PASS side of the dual-state proof for
// scripts/lint/no_unchecked_unstructured_assert.go).
//
// PURPOSE: this file is the H5-SAFE counterpart to the BAD fixture at
// ../bad/regression_unchecked_assert.go. It constructs the SAME
// clientcache.ResourceEventHandlerFuncs{} shape the lint inspects, but
// every content-reading handler body routes through decodeBytesObject
// (the H5-aware decode helper) — so the lint MUST NOT flag it. The
// all-`_` handler (OQ2) is content-free by construction and equally
// clean.
//
// The lint's GOOD arm invokes:
//
//   go run scripts/lint/no_unchecked_unstructured_assert.go \
//     --root=$(pwd)/scripts/lint/testdata/unchecked_assert/good
//   # exit 0; no violations
//
// `//go:build ignore` keeps `go build ./...` + `go vet ./...` away from
// this fixture (mirrors the sibling fixture + the lint programs
// themselves). parser.ParseFile reads it regardless of the build tag —
// that's how the lint sees it.

//go:build ignore

package good

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	clientcache "k8s.io/client-go/tools/cache"
)

// decodeBytesObject stands in for the real internal/cache helper of the
// same name. The lint matches decode helpers by IDENTIFIER NAME (see
// no_unchecked_unstructured_assert.go:112-116), so a local declaration
// with the canonical name is sufficient for the fixture — no dependency
// on the internal package.
func decodeBytesObject(obj interface{}) (*unstructured.Unstructured, bool) {
	u, ok := obj.(*unstructured.Unstructured)
	return u, ok
}

// installGoodHandler is the H5-safe pattern: content access goes through
// decodeBytesObject, so even though a raw *unstructured.Unstructured
// assert exists inside decodeBytesObject, the handler bodies themselves
// route through the helper. The lint MUST NOT flag any of these.
func installGoodHandler() clientcache.ResourceEventHandlerFuncs {
	return clientcache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			// H5-safe: route through the decode helper.
			u, ok := decodeBytesObject(obj)
			if !ok || u == nil {
				return
			}
			_ = u.GetName()
		},
		UpdateFunc: func(_, newObj interface{}) {
			u, ok := decodeBytesObject(newObj)
			if !ok || u == nil {
				return
			}
			_ = u.GetName()
		},
		// OQ2: all-blank params — content-free by construction, never flagged.
		DeleteFunc: func(_ interface{}) {},
	}
}
