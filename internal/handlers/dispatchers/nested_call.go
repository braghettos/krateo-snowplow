// nested_call.go — Ship 0.30.123 (#155): the in-process nested-/call
// resolver implementation.
//
// This is the IMPL behind the api.NestedCallResolverFunc seam. It lives
// in the dispatchers package because it needs objects.Get,
// checkDispatchRBAC, AND restactions.Resolve — the api package cannot
// import restactions/dispatchers (import cycle), so it declares only the
// seam (api/nested_call_seam.go) and main.go wires this implementation
// in via api.RegisterNestedCallResolver(dispatchers.ResolveNestedCall).
//
// ResolveNestedCall replicates restActionHandler.ServeHTTP MINUS the HTTP
// edge: objects.Get the referenced RESTAction CR, gate it with
// checkDispatchRBAC, FromUnstructured-decode it, restactions.Resolve it,
// and return the FULL RESTAction envelope via encodeResolvedJSON — the
// exact bytes restActionHandler.ServeHTTP writes as the HTTP /call
// response body (Ship 0.30.124 content-shape fix; 0.30.123 wrongly
// returned the bare Status.Raw). The identity is whatever WithUserInfo
// the inbound ctx already carries — so a JWT-less / SA-credentialed
// resolve completes a /call-loopback stage that the HTTP path could not
// (no Authorization header to forward).
//
// THE checkDispatchRBAC CALL IS THE SINGLE MOST IMPORTANT CORRECTNESS
// LINE. The in-process path bypasses the HTTP edge and with it the
// per-user apiserver RBAC enforcement an HTTP /call would pay. Omitting
// the explicit gate would make every in-process nested /call an
// RBAC-bypass / cross-user-leak vector. It is NOT optional.

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
	"k8s.io/apimachinery/pkg/runtime"
)

// ResolveNestedCall resolves a /call-loopback stage IN-PROCESS. It is the
// implementation wired into the api.nestedCallResolver seam at startup.
//
// Pipeline (= restActionHandler.ServeHTTP minus the HTTP edge):
//  1. recursion-depth guard — at cache.NestedCallMaxDepth() return a
//     bounded ERROR (never empty, never panic);
//  2. objects.Get the referenced RESTAction CR under ctx's identity;
//  3. checkDispatchRBAC — the load-bearing RBAC gate (cache=on); a
//     denied identity gets a 403-class error, NOT empty content;
//  4. FromUnstructured-decode to a typed RESTAction;
//  5. restactions.Resolve under a ctx whose nested-call depth is
//     incremented by 1 (so an inner /call-loopback hop is bounded);
//  6. return encodeResolvedJSON(res) — the FULL RESTAction envelope,
//     byte-identical to what restActionHandler.ServeHTTP writes as the
//     HTTP /call response body (Ship 0.30.124 content-shape fix).
func ResolveNestedCall(
	ctx context.Context,
	ref v1.ObjectReference,
	perPage, page int,
	extras map[string]any,
) ([]byte, error) {
	log := xcontext.Logger(ctx)

	// Step 1 — recursion-depth guard. depth is the number of nested-/call
	// hops already taken; the outermost request-path resolve carries 0,
	// so its first nested /call enters here at depth 0 and we cap at
	// NestedCallMaxDepth. A self-referential or cyclic RESTAction
	// terminates here with a bounded error — NOT a panic, NOT empty.
	depth := cache.NestedCallDepthFromContext(ctx)
	if depth >= cache.NestedCallMaxDepth() {
		return nil, fmt.Errorf("nested /call depth limit exceeded (%d): "+
			"resource=%s name=%s namespace=%s — refusing to recurse further "+
			"(cyclic or pathologically deep /call graph)",
			cache.NestedCallMaxDepth(), ref.Resource, ref.Name, ref.Namespace)
	}

	// Step 2 — fetch the referenced RESTAction CR under ctx's identity.
	got := objects.Get(ctx, ref)
	if got.Err != nil {
		return nil, fmt.Errorf("nested /call: fetch %s/%s: %s",
			ref.Resource, ref.Name, got.Err.Message)
	}
	if got.Unstructured == nil {
		return nil, fmt.Errorf("nested /call: fetch %s/%s: nil object",
			ref.Resource, ref.Name)
	}

	// Step 3 — THE RBAC GATE. In cache=on mode objects.Get is informer-
	// served and does NOT enforce per-user RBAC for this GET; the HTTP
	// /call path would have enforced it via the per-user apiserver call.
	// The in-process path MUST run the explicit gate, exactly as
	// restActionHandler.ServeHTTP does. A denied identity gets a
	// 403-class error — never empty content (which would mask the
	// denial and is an under-serve, not a leak, but still wrong).
	if !cache.Disabled() {
		if !checkDispatchRBAC(ctx, got.GVR, got.Unstructured.GetNamespace()) {
			log.Warn("nested /call dispatch denied by EvaluateRBAC",
				slog.String("name", got.Unstructured.GetName()),
				slog.String("namespace", got.Unstructured.GetNamespace()),
				slog.String("gvr", got.GVR.String()),
			)
			return nil, fmt.Errorf("forbidden: cannot get %s in namespace %q",
				got.GVR.Resource, got.Unstructured.GetNamespace())
		}
	}

	// Step 4 — decode to a typed RESTAction.
	scheme := runtime.NewScheme()
	if err := apis.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("nested /call: add apis to scheme: %w", err)
	}
	var cr v1.RESTAction
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(
		got.Unstructured.Object, &cr); err != nil {
		return nil, fmt.Errorf("nested /call: unstructured -> RESTAction %s/%s: %w",
			ref.Resource, ref.Name, err)
	}

	// Step 5 — resolve the inner RESTAction. The ctx passed in carries
	// the depth INCREMENTED by 1, so any /call-loopback stage WITHIN this
	// inner RESTAction enters ResolveNestedCall one level deeper and the
	// depth cap bounds the whole recursion.
	innerCtx := cache.WithNestedCallDepth(ctx, depth+1)
	res, err := restactions.Resolve(innerCtx, restactions.ResolveOptions{
		In:      &cr,
		AuthnNS: env.String("AUTHN_NAMESPACE", ""),
		PerPage: perPage,
		Page:    page,
		Extras:  extras,
	})
	if err != nil {
		return nil, fmt.Errorf("nested /call: resolve RESTAction %s/%s: %w",
			ref.Resource, ref.Name, err)
	}

	// Step 6 — return the FULL RESTAction envelope, byte-shaped exactly as
	// the HTTP /call endpoint delivers it.
	//
	// Ship 0.30.124 content-shape fix: 0.30.123 returned res.Status.Raw —
	// the inner RESTAction's BARE status. That was wrong. The real HTTP
	// GET /call?resource=restactions is intercepted by the handlers.Dispatcher
	// middleware and routed to restActionHandler.ServeHTTP, whose response
	// body is encodeResolvedJSON(res) — the WHOLE RESTAction envelope
	// {kind, apiVersion, metadata, spec, status}. A consuming stage with
	// `dependsOn.iterator: ".<id>.status"` does `.status` on the nested
	// result, expecting that envelope. 0.30.123's bare-status array made
	// `.status` on an array fail ("expected an object but got: array"),
	// the iterator got no options, and the result was empty.
	//
	// encodeResolvedJSON is the SAME encoder restActionHandler.ServeHTTP
	// uses (helpers.go) — calling it here guarantees the in-process bytes
	// are byte-identical to the HTTP /call bytes; no hand-replicated
	// marshal.
	if res == nil {
		// A resolve that produced no RESTAction at all — return an empty
		// JSON object so the stage's ResponseHandler sees well-formed JSON
		// rather than a nil reader.
		return []byte("{}"), nil
	}
	encoded, err := encodeResolvedJSON(res)
	if err != nil {
		return nil, fmt.Errorf("nested /call: encode RESTAction envelope %s/%s: %w",
			ref.Resource, ref.Name, err)
	}
	return encoded, nil
}

// Compile-time assertion that ResolveNestedCall satisfies the
// api.NestedCallResolverFunc signature. If the seam type ever drifts,
// this fails the build at the dispatchers package rather than silently
// at the main.go wiring site.
var _ func(context.Context, v1.ObjectReference, int, int, map[string]any) ([]byte, error) = ResolveNestedCall
