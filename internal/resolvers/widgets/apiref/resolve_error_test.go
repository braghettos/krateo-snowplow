// resolve_error_test.go — Task #272 / 0.30.251 falsifier for the apiref
// error-type preservation fix (architect doc:
// docs/task-262-s8-cj-tablist-trace-2026-06-09.md §3.3, §7.3).
//
// Pre-fix, `apiref.Resolve` stripped the upstream apiserver status code
// with `fmt.Errorf("%s", res.Err.Message)`. The downstream dispatcher's
// `errors.As(err, *apierrors.StatusError)` (widgets.go:228-234) then
// failed and every apiRef-resolve error landed in
// `response.InternalError` → HTTP 500, regardless of the apiserver's
// real response code. Customer impact: cj's S8 panel render fired
// `restactions:get` against the apiserver under narrow RBAC, got back
// a real 403, the SPA saw 500, and rendered `.ant-result-error`
// instead of a meaningful denial.
//
// Fix: at the apiref boundary, reconstruct an `*apierrors.StatusError`
// from the code preserved on `res.Err` (objects.Get's apiserver branch
// already sets res.Err.Code per apierrors.IsForbidden / IsNotFound —
// see internal/objects/get.go:209-214) and wrap with `%w` so the
// dispatcher's `errors.As` recovers the code.
//
// These tests exercise the wrapping helper directly so the contract
// (errors.As recovers Code=403, Code=404, etc.) is provable in
// isolation, without spinning up an envtest cluster.

package apiref

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/krateoplatformops/plumbing/http/response"
	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestStatusErrorFromResponse_Forbidden — the load-bearing case.
// Asserts that a 403 from `objects.Get` (wrapped in
// `*response.Status`) round-trips through the helper as an
// `*apierrors.StatusError` with Code=403, recoverable via errors.As
// from a `%w`-wrapped fmt.Errorf chain.
//
// This is the falsifier for the customer-visible S8 cj symptom: pre-
// fix, errors.As returns false on the wrapped chain and the dispatcher
// emits HTTP 500. Post-fix, errors.As returns true and recovers
// Code=403.
func TestStatusErrorFromResponse_Forbidden(t *testing.T) {
	ref := templatesv1.ObjectReference{
		Reference: templatesv1.Reference{
			Name:      "bench-app-01-02-composition-values",
			Namespace: "bench-ns-01",
		},
		APIVersion: "templates.krateo.io/v1",
		Resource:   "restactions",
	}
	upstream := response.New(http.StatusForbidden,
		fmt.Errorf("restactions.templates.krateo.io %q is forbidden: "+
			"User \"cyberjoker\" cannot get resource \"restactions\" in "+
			"API group \"templates.krateo.io\" in the namespace \"bench-ns-01\"",
			ref.Name))

	statusErr := statusErrorFromResponse(upstream, ref)
	if statusErr == nil {
		t.Fatalf("statusErrorFromResponse(403): want non-nil StatusError, got nil")
	}
	if got, want := int(statusErr.ErrStatus.Code), http.StatusForbidden; got != want {
		t.Errorf("StatusError.Code: got %d, want %d", got, want)
	}
	if got, want := string(statusErr.ErrStatus.Reason),
		string(metav1.StatusReasonForbidden); got != want {
		t.Errorf("StatusError.Reason: got %q, want %q", got, want)
	}
	if !apierrors.IsForbidden(statusErr) {
		t.Errorf("apierrors.IsForbidden(statusErr) = false; want true")
	}

	// Now wrap as `apiref.Resolve` does and verify the chain
	// preserves the type for errors.As — the exact contract the
	// dispatcher checks at widgets.go:228-234.
	wrapped := fmt.Errorf("apiref resolve %s/%s/%s: %w",
		"templates.krateo.io", "restactions", ref.Name, statusErr)

	var recovered *apierrors.StatusError
	if !errors.As(wrapped, &recovered) {
		t.Fatalf("errors.As(wrapped, *StatusError): got false; chain broken")
	}
	if got, want := int(recovered.Status().Code), http.StatusForbidden; got != want {
		t.Errorf("recovered.Status().Code: got %d, want %d", got, want)
	}
	// The wrapped error's full message includes the prefix +
	// the upstream message — observable from a single .Error() call.
	if !strings.Contains(wrapped.Error(), "apiref resolve templates.krateo.io/restactions/") {
		t.Errorf("wrapped.Error() missing context prefix: %q", wrapped.Error())
	}
	if !strings.Contains(wrapped.Error(), "is forbidden") {
		t.Errorf("wrapped.Error() missing upstream message: %q", wrapped.Error())
	}
}

// TestStatusErrorFromResponse_NotFound — sister case for objects.Get's
// IsNotFound branch (internal/objects/get.go:212-213). Same chain
// contract.
func TestStatusErrorFromResponse_NotFound(t *testing.T) {
	ref := templatesv1.ObjectReference{
		Reference: templatesv1.Reference{
			Name:      "does-not-exist",
			Namespace: "bench-ns-01",
		},
		APIVersion: "templates.krateo.io/v1",
		Resource:   "restactions",
	}
	upstream := response.New(http.StatusNotFound,
		fmt.Errorf("restactions.templates.krateo.io %q not found", ref.Name))

	statusErr := statusErrorFromResponse(upstream, ref)
	if statusErr == nil {
		t.Fatalf("statusErrorFromResponse(404): want non-nil StatusError, got nil")
	}
	if got, want := int(statusErr.ErrStatus.Code), http.StatusNotFound; got != want {
		t.Errorf("StatusError.Code: got %d, want %d", got, want)
	}
	if !apierrors.IsNotFound(statusErr) {
		t.Errorf("apierrors.IsNotFound(statusErr) = false; want true")
	}

	wrapped := fmt.Errorf("apiref resolve %s/%s/%s: %w",
		"templates.krateo.io", "restactions", ref.Name, statusErr)
	var recovered *apierrors.StatusError
	if !errors.As(wrapped, &recovered) {
		t.Fatalf("errors.As(wrapped, *StatusError): got false; chain broken")
	}
	if got, want := int(recovered.Status().Code), http.StatusNotFound; got != want {
		t.Errorf("recovered.Status().Code: got %d, want %d", got, want)
	}
}

// TestStatusErrorFromResponse_NilInput — defence-in-depth.
// `objects.Get` always sets res.Err on the error path, but the helper
// must never panic on a nil input (e.g. if a future caller passes a
// nil response.Status). Returns nil.
func TestStatusErrorFromResponse_NilInput(t *testing.T) {
	ref := templatesv1.ObjectReference{
		Reference: templatesv1.Reference{Name: "x", Namespace: "y"},
		Resource:  "z",
	}
	if got := statusErrorFromResponse(nil, ref); got != nil {
		t.Errorf("statusErrorFromResponse(nil): got %#v, want nil", got)
	}
}

// TestStatusErrorFromResponse_ZeroCodeFallsBackToInternal — if a
// future objects.Get branch sets res.Err.Code = 0 (missing field on
// the wire), the helper must still emit a clean status code rather
// than a zero / undefined response. Falls back to 500.
func TestStatusErrorFromResponse_ZeroCodeFallsBackToInternal(t *testing.T) {
	ref := templatesv1.ObjectReference{
		Reference: templatesv1.Reference{Name: "x", Namespace: "y"},
		Resource:  "z",
	}
	upstream := &response.Status{
		Code:    0,
		Message: "code-field-unset",
	}
	statusErr := statusErrorFromResponse(upstream, ref)
	if statusErr == nil {
		t.Fatalf("statusErrorFromResponse(zero-code): want non-nil StatusError")
	}
	if got, want := int(statusErr.ErrStatus.Code), http.StatusInternalServerError; got != want {
		t.Errorf("StatusError.Code: got %d, want %d (default-on-zero)", got, want)
	}
}

// TestStatusErrorFromResponse_PreservesDetails — the helper must
// populate StatusDetails so downstream consumers reading
// `err.Status().Details.Name` / `Kind` get a useful resource handle.
// This is what an apiserver-shaped 403 carries; we faithfully
// reproduce it from the ObjectReference.
func TestStatusErrorFromResponse_PreservesDetails(t *testing.T) {
	ref := templatesv1.ObjectReference{
		Reference: templatesv1.Reference{Name: "panel-foo", Namespace: "ns"},
		Resource:  "panels",
	}
	upstream := response.New(http.StatusForbidden, fmt.Errorf("denied"))
	statusErr := statusErrorFromResponse(upstream, ref)
	if statusErr == nil {
		t.Fatalf("statusErrorFromResponse: want non-nil StatusError")
	}
	if statusErr.ErrStatus.Details == nil {
		t.Fatalf("Status.Details: want non-nil; downstream cannot recover resource handle")
	}
	if got, want := statusErr.ErrStatus.Details.Name, ref.Name; got != want {
		t.Errorf("Details.Name: got %q, want %q", got, want)
	}
	if got, want := statusErr.ErrStatus.Details.Kind, ref.Resource; got != want {
		t.Errorf("Details.Kind: got %q, want %q", got, want)
	}
}
