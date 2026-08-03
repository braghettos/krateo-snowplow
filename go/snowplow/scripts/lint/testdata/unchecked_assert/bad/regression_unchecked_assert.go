// scripts/lint/testdata/unchecked_assert/bad/regression_unchecked_assert.go
// — Ship H6 lint-gate BAD fixture (FAIL side of the dual-state proof for
// scripts/lint/no_unchecked_unstructured_assert.go).
//
// PURPOSE: this file deliberately reproduces the Ship 0.30.233 defect
// class the lint must catch: a raw `obj.(*unstructured.Unstructured)`
// content-assert inside a LITERAL informer event-handler body, with NO
// decodeBytesObject / fallbackUnstructuredFromIndexer routing. Post
// Ship-H5 the streaming-listwatch delivers *bytesObject here, NOT
// *unstructured.Unstructured — the assertion silently fails on every
// event.
//
// It is the isolated-root twin of the historical flat fixture at
// ../../regression_unchecked_assert.go (kept in place for the lint's own
// documented `--root=$(pwd)/scripts/lint/testdata` usage). This copy
// lives under bad/ so the H6 gate test can point --root at a directory
// that contains ONLY the BAD fixture, keeping the GOOD/BAD arms cleanly
// separated.
//
// The lint's BAD arm invokes:
//
//   go run scripts/lint/no_unchecked_unstructured_assert.go \
//     --root=$(pwd)/scripts/lint/testdata/unchecked_assert/bad
//   # exit 1; violations reported for each raw assert below.
//
// `//go:build ignore` keeps `go build ./...` + `go vet ./...` away.

//go:build ignore

package bad

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	clientcache "k8s.io/client-go/tools/cache"
)

// installBadHandler reproduces the Ship 0.30.233 defect class: a raw
// *unstructured.Unstructured content-assert inside a literal informer
// AddFunc / UpdateFunc / DeleteFunc body with no decode-helper routing.
//
// THE LINT MUST FLAG THIS FUNCTION.
func installBadHandler() clientcache.ResourceEventHandlerFuncs {
	return clientcache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			// REGRESSION: raw content-assert inside a literal AddFunc body.
			u, ok := obj.(*unstructured.Unstructured)
			if !ok || u == nil {
				return
			}
			_ = u.GetName()
		},
		UpdateFunc: func(_, newObj interface{}) {
			// REGRESSION: same defect class, UpdateFunc variant.
			u, ok := newObj.(*unstructured.Unstructured)
			if !ok || u == nil {
				return
			}
			_ = u.GetName()
		},
		DeleteFunc: func(obj interface{}) {
			// REGRESSION: same defect class, DeleteFunc variant.
			u, ok := obj.(*unstructured.Unstructured)
			if !ok || u == nil {
				return
			}
			_ = u.GetName()
		},
	}
}
