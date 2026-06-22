// nested_call.go — the in-process RA/widget resolve implementation behind
// the api.NestedCallResolverFunc seam.
//
// HISTORY: introduced at Ship 0.30.123 (#155) as the in-process resolver for
// the /call?resource=... LOOPBACK stage. The /call loopback DISPATCH BRANCH
// was RETIRED in the 2026-06-22 unified ship (corpus audit confirmed zero
// live loopback paths). The resolve LOGIC here SURVIVES that retirement: it
// is now the shared in-process resolver behind the DIRECT-APISERVER-PATH +
// `resolve: true` mechanism (the recommended replacement for the loopback).
// The seam name (ResolveNestedCall / api.RegisterNestedCallResolver) is kept
// for continuity; what changed is the TRIGGER (a direct apiserver path with
// resolve:true, not a /call?resource=... path) and the addition of a WIDGET
// arm (the loopback was RESTAction-only).
//
// This is the IMPL behind the api.NestedCallResolverFunc seam. It lives in
// the dispatchers package because it needs objects.Get, checkDispatchRBAC,
// restactions.Resolve, AND widgets.Resolve — the api package cannot import
// restactions/widgets/dispatchers (import cycle), so it declares only the
// seam (api/nested_call_seam.go) and main.go wires this implementation in via
// api.RegisterNestedCallResolver(dispatchers.ResolveNestedCall).
//
// ResolveNestedCall replicates restActionHandler.ServeHTTP / widgetsHandler.
// ServeHTTP MINUS the HTTP edge: objects.Get the referenced CR, gate it with
// checkDispatchRBAC, branch on the fetched GVR, resolve it in-process, and
// return the FULL resolved envelope via encodeResolvedJSON — the exact bytes
// the HTTP /call response body carries. The identity is whatever WithUserInfo
// the inbound ctx already carries — so a JWT-less / SA-credentialed resolve
// completes a resolve a per-user HTTP edge could not.
//
// THE checkDispatchRBAC CALL IS THE SINGLE MOST IMPORTANT CORRECTNESS LINE.
// The in-process path bypasses the HTTP edge and with it the per-user
// apiserver RBAC enforcement an HTTP /call would pay. Omitting the explicit
// gate would make every in-process resolve an RBAC-bypass / cross-user-leak
// vector. It is NOT optional. It is GVR-parameterised (kind-agnostic) so the
// widget arm is gated identically to the RESTAction arm.

package dispatchers

import (
	"context"
	"fmt"
	"log/slog"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/env"
	"github.com/krateoplatformops/snowplow/apis"
	v1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/objects"
	"github.com/krateoplatformops/snowplow/internal/resolvers/restactions"
	"github.com/krateoplatformops/snowplow/internal/resolvers/widgets"
	"k8s.io/apimachinery/pkg/runtime"
)

// nestedResolveRestActionsResource / nestedResolveWidgetsResource are the
// plural resource names that select the resolve arm. They are matched on the
// fetched object's GVR.Resource (objects.Get fills the canonical GVR), so the
// branch is data-driven off the apiserver-discovered GVR — NOT a path/name
// literal (feedback_no_special_cases). Any other resource → raw no-op.
const (
	nestedResolveRestActionsResource = "restactions"
	nestedResolveWidgetsResource     = "widgets"
)

// ResolveNestedCall resolves a referenced RA/widget CR IN-PROCESS. It is the
// implementation wired into the api.nestedCallResolver seam at startup, and
// is invoked by the api resolver's direct-apiserver-path + `resolve: true`
// branch (resolve.go maybeResolveInProcess).
//
// Pipeline (= restActionHandler/widgetsHandler.ServeHTTP minus the HTTP edge):
//  1. recursion-depth guard — at cache.NestedCallMaxDepth() return a bounded
//     ERROR (never empty, never panic);
//  2. objects.Get the referenced CR under ctx's identity;
//  3. checkDispatchRBAC — the load-bearing RBAC gate (cache=on); a denied
//     identity gets a 403-class error, NOT empty content;
//  4. branch on the fetched GVR.Resource:
//       restactions → restactions.Resolve(typed RESTAction);
//       widgets     → widgets.Resolve(the Unstructured widget);
//       else        → return the RAW fetched object (no-op resolve) so a
//                     resolve:true on a non-RA/widget path is harmless;
//     under a ctx whose nested-call depth is incremented by 1 (so an inner
//     RA-resolves-RA chain is bounded);
//  5. return encodeResolvedJSON(res) — the FULL resolved envelope,
//     byte-identical to what the HTTP /call response body carries.
func ResolveNestedCall(
	ctx context.Context,
	ref v1.ObjectReference,
	perPage, page int,
	extras map[string]any,
) ([]byte, error) {
	log := xcontext.Logger(ctx)

	// Step 1 — recursion-depth guard. depth is the number of nested resolve
	// hops already taken; the outermost request-path resolve carries 0, so
	// its first nested resolve enters here at depth 0 and we cap at
	// NestedCallMaxDepth. A self-referential or cyclic graph terminates here
	// with a bounded error — NOT a panic, NOT empty.
	depth := cache.NestedCallDepthFromContext(ctx)
	if depth >= cache.NestedCallMaxDepth() {
		return nil, fmt.Errorf("nested resolve depth limit exceeded (%d): "+
			"resource=%s name=%s namespace=%s — refusing to recurse further "+
			"(cyclic or pathologically deep resolve graph)",
			cache.NestedCallMaxDepth(), ref.Resource, ref.Name, ref.Namespace)
	}

	// Step 2 — fetch the referenced CR under ctx's identity.
	got := objects.Get(ctx, ref)
	if got.Err != nil {
		return nil, fmt.Errorf("nested resolve: fetch %s/%s: %s",
			ref.Resource, ref.Name, got.Err.Message)
	}
	if got.Unstructured == nil {
		return nil, fmt.Errorf("nested resolve: fetch %s/%s: nil object",
			ref.Resource, ref.Name)
	}

	// Step 3 — THE RBAC GATE. In cache=on mode objects.Get is informer-served
	// and does NOT enforce per-user RBAC for this GET; the HTTP /call path
	// would have enforced it via the per-user apiserver call. The in-process
	// path MUST run the explicit gate. GVR-parameterised (kind-agnostic) so
	// the RESTAction and widget arms are gated identically. A denied identity
	// gets a 403-class error — never empty content.
	if !cache.Disabled() {
		if !checkDispatchRBAC(ctx, got.GVR, got.Unstructured.GetNamespace()) {
			log.Warn("nested resolve dispatch denied by EvaluateRBAC",
				slog.String("name", got.Unstructured.GetName()),
				slog.String("namespace", got.Unstructured.GetNamespace()),
				slog.String("gvr", got.GVR.String()),
			)
			return nil, fmt.Errorf("forbidden: cannot get %s in namespace %q",
				got.GVR.Resource, got.Unstructured.GetNamespace())
		}
	}

	// Step 4/5 — resolve the inner object under a ctx whose nested-call depth
	// is incremented by 1 (so a resolve WITHIN this inner object enters one
	// level deeper and the depth cap bounds the whole recursion). The
	// L1KeyFromContext on `ctx` (the outer entry's key) is preserved by
	// WithNestedCallDepth (context.WithValue keeps parent values), so the
	// inner resolve's apiserver-call dep edges land on the OUTER L1 key —
	// transitive data invalidation works (proposal §5).
	innerCtx := cache.WithNestedCallDepth(ctx, depth+1)

	switch got.GVR.Resource {
	case nestedResolveRestActionsResource:
		// RESTAction arm — decode to a typed RESTAction and resolve.
		scheme := runtime.NewScheme()
		if err := apis.AddToScheme(scheme); err != nil {
			return nil, fmt.Errorf("nested resolve: add apis to scheme: %w", err)
		}
		var cr v1.RESTAction
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(
			got.Unstructured.Object, &cr); err != nil {
			return nil, fmt.Errorf("nested resolve: unstructured -> RESTAction %s/%s: %w",
				ref.Resource, ref.Name, err)
		}
		// Ship 0.30.230 fix-at-root: thread the *rest.Config from ctx when
		// the resolve runs under an internal driver (Phase 1 walker, PIP
		// seed, refresher). Per-user resolves have no internal rc on ctx;
		// rcFromCtx returns nil and the api resolver falls back to the
		// per-user kubeconfig path (unchanged from pre-fix behaviour).
		res, err := restactions.Resolve(innerCtx, restactions.ResolveOptions{
			In:      &cr,
			SArc:    rcFromCtx(innerCtx),
			AuthnNS: env.String("AUTHN_NAMESPACE", ""),
			PerPage: perPage,
			Page:    page,
			Extras:  extras,
		})
		if err != nil {
			return nil, fmt.Errorf("nested resolve: resolve RESTAction %s/%s: %w",
				ref.Resource, ref.Name, err)
		}
		if res == nil {
			// A resolve that produced no RESTAction at all — return an empty
			// JSON object so the consuming stage sees well-formed JSON.
			return []byte("{}"), nil
		}
		// encodeResolvedJSON is the SAME encoder restActionHandler.ServeHTTP
		// uses — calling it here guarantees the in-process bytes are
		// byte-identical to the HTTP /call bytes (Ship 0.30.124 content-shape
		// fix: the WHOLE RESTAction envelope {kind,apiVersion,metadata,spec,
		// status}, NOT the bare Status.Raw).
		encoded, err := encodeResolvedJSON(res)
		if err != nil {
			return nil, fmt.Errorf("nested resolve: encode RESTAction envelope %s/%s: %w",
				ref.Resource, ref.Name, err)
		}
		return encoded, nil

	case nestedResolveWidgetsResource:
		// Widget arm (added by the 2026-06-22 ship — serves the direct-path
		// resolve:true form; the legacy loopback never routed widgets here).
		// widgets.Resolve takes the Unstructured widget directly (no typed
		// decode) and returns the resolved *Widget (an *unstructured.
		// Unstructured), the EXACT shape widgetsHandler.ServeHTTP +
		// resolveWidgetForRefresh use (resolve_populate.go).
		res, err := widgets.Resolve(innerCtx, widgets.ResolveOptions{
			In:      got.Unstructured,
			RC:      rcFromCtx(innerCtx),
			AuthnNS: env.String("AUTHN_NAMESPACE", ""),
			PerPage: perPage,
			Page:    page,
			Extras:  extras,
		})
		if err != nil {
			return nil, fmt.Errorf("nested resolve: resolve Widget %s/%s: %w",
				ref.Resource, ref.Name, err)
		}
		if res == nil {
			return []byte("{}"), nil
		}
		encoded, err := encodeResolvedJSON(res)
		if err != nil {
			return nil, fmt.Errorf("nested resolve: encode Widget envelope %s/%s: %w",
				ref.Resource, ref.Name, err)
		}
		return encoded, nil

	default:
		// Any other kind (e.g. a configmap path with resolve:true) — resolve
		// is meaningless, so feed the RAW fetched object back unchanged. This
		// is a harmless no-op consistent with the proposal (#1 step 2): a
		// resolve:true on a non-RA/widget path is a plain raw fetch. The
		// caller substitutes these bytes for the stage output, which equals
		// the raw-CR feed it would have done with resolve:false.
		encoded, err := encodeResolvedJSON(got.Unstructured.Object)
		if err != nil {
			return nil, fmt.Errorf("nested resolve: encode raw object %s/%s: %w",
				ref.Resource, ref.Name, err)
		}
		return encoded, nil
	}
}

// Compile-time assertion that ResolveNestedCall satisfies the
// api.NestedCallResolverFunc signature. If the seam type ever drifts, this
// fails the build at the dispatchers package rather than silently at the
// main.go wiring site.
var _ func(context.Context, v1.ObjectReference, int, int, map[string]any) ([]byte, error) = ResolveNestedCall
