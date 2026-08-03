// scripts/lint/testdata/regression_unchecked_assert.go — Ship L lint
// regression fixture (FAIL side of the dual-state proof).
//
// PURPOSE: this file deliberately contains the regression class the
// Ship-L lint at scripts/lint/no_unchecked_unstructured_assert.go must
// catch: a raw `obj.(*unstructured.Unstructured)` assertion inside a
// LITERAL informer event-handler body (AddFunc / UpdateFunc / DeleteFunc).
//
// Production code (post Ship L) MUST pass the lint with zero violations.
// THIS file MUST trip the lint with at least one violation. The lint
// invokes itself with --root pointing here for the FAIL-side dual-state
// proof:
//
//   # PASS side — production code
//   go run scripts/lint/no_unchecked_unstructured_assert.go \
//     --root=$(pwd)/internal/cache
//   # exit 0
//
//   # FAIL side — this fixture
//   go run scripts/lint/no_unchecked_unstructured_assert.go \
//     --root=$(pwd)/scripts/lint/testdata
//   # exit 1; violation reported for the AddFunc raw assert below.
//
// `//go:build ignore` keeps `go build ./...` + `go vet ./...` away from
// this file — testdata/ is also excluded by the Go toolchain's package
// discovery (per cmd/go/internal/load/pkg.go) but the build tag is the
// defence-in-depth guard against a future refactor moving the fixture
// out of testdata/. This mirrors the established codebase pattern at
// scripts/lint/no_parallel_binding_derivation.go:60 and
// scripts/sa-endpoint-shape-proof.go:26.

//go:build ignore

package testdata

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	clientcache "k8s.io/client-go/tools/cache"
)

// installBadHandler reproduces the Ship 0.30.233 defect class: a raw
// *unstructured.Unstructured content-assert inside a literal informer
// AddFunc body. Post-Ship-H5 the streaming-listwatch path delivers
// *bytesObject here, NOT *unstructured.Unstructured — the assertion
// silently fails on every event.
//
// THE LINT MUST FLAG THIS FUNCTION.
func installBadHandler() clientcache.ResourceEventHandlerFuncs {
	return clientcache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			// REGRESSION: raw content-assert inside a literal AddFunc
			// body. The lint flags this exact pattern.
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
