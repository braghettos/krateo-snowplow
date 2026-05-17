// resolve_populate.go — Ship C (0.30.112): the single resolve-and-store
// path for the L1 resolved-output cache.
//
// resolveAndPopulateL1 re-resolves an L1 entry from its own
// ResolvedKeyInputs and writes the fresh bytes back under the canonical
// key. It is the body the runtime refresher's RefreshFunc invokes on a
// dirty-mark (Ship C) and the body Ship F's prewarm will reuse — one
// resolve path, no duplication.
//
// IDENTITY (PM directive, AC-C7): the re-resolve runs under the entry's
// OWN Inputs identity — Username + Groups from the ResolvedKeyInputs.
// There is NO ServiceAccount fallback and NO shared identity: a refresh
// of user U's entry resolves as U, so RBAC narrowing and the resolved
// content stay user-correct. The re-resolve context also carries
// WithL1KeyContext(key) so the resolver re-records dep edges (the inner
// object set may have changed since the original resolve).
//
// Per feedback_l1_invalidation_delete_only.md: this path only ever
// Put()s — it never evicts. A refresh that lands after the entry was
// evicted must not resurrect it (see the post-resolve liveness re-check).

package dispatchers

import (
	"context"
	"fmt"
	"log/slog"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/env"
	"github.com/krateoplatformops/plumbing/jwtutil"
	"github.com/krateoplatformops/snowplow/apis"
	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/objects"
	"github.com/krateoplatformops/snowplow/internal/resolvers/restactions"
	"github.com/krateoplatformops/snowplow/internal/resolvers/widgets"
	"k8s.io/apimachinery/pkg/runtime"
)

// resolveOnceFn is the resolve-and-encode seam. It re-fetches the CR
// named by inputs (under ctx's identity) and returns the encoded
// resolver output. Production wires it to resolveOnceProd; tests stub
// it to exercise resolveAndPopulateL1's queue/identity/Put plumbing
// without a live cluster.
//
// A package var rather than a parameter so the refresher's RefreshFunc
// signature (cache.RefreshFunc) is untouched. Swapped only by the
// _test.go shim; production never reassigns it.
var resolveOnceFn = resolveOnceProd

// resolveAndPopulateL1 is the single resolve-and-store path. It:
//
//  1. computes the canonical L1 key from inputs (must equal the key the
//     entry is filed under — ComputeKey is deterministic);
//  2. builds the re-resolve context: the entry's own Username+Groups
//     identity, plus WithL1KeyContext(key) so dep edges are re-recorded;
//  3. re-resolves + encodes via the resolveOnce seam;
//  4. re-checks the entry is still live (a DELETE-evict may have raced
//     the refresh) and, if so, Put()s the fresh bytes.
//
// Returns an error on resolve failure so the refresher can retry with
// backoff; returns nil (no-op) for an Inputs the path cannot drive.
func resolveAndPopulateL1(ctx context.Context, inputs cache.ResolvedKeyInputs) error {
	log := xcontext.Logger(ctx)

	c := cache.ResolvedCache()
	if c == nil {
		// L1 disabled — nothing to populate. Not an error.
		return nil
	}

	key := cache.ComputeKey(inputs)

	// AC-C7: re-resolve under the entry's OWN identity. Username+Groups
	// come straight off the cached Inputs — no SA, no shared identity.
	// WithL1KeyContext threads the L1 key so the resolver's inner-call
	// recording site re-records dep edges for this refresh.
	rctx := xcontext.BuildContext(ctx,
		xcontext.WithUserInfo(jwtutil.UserInfo{
			Username: inputs.Username,
			Groups:   inputs.Groups,
		}),
	)
	rctx = cache.WithL1KeyContext(rctx, key)

	encoded, err := resolveOnceFn(rctx, inputs)
	if err != nil {
		return fmt.Errorf("resolveAndPopulateL1 %s/%s: %w",
			inputs.HandlerKind, inputs.Name, err)
	}
	if encoded == nil {
		// The seam declined to resolve (e.g. unknown handler kind) —
		// skip-to-TTL, not an error.
		return nil
	}

	// A refresh that lands AFTER the entry was DELETE-evicted must not
	// resurrect it. Re-Get under the key: if it is gone, drop the fresh
	// bytes on the floor (the eviction is authoritative).
	if _, alive := c.Get(key); !alive {
		log.Debug("resolveAndPopulateL1: entry evicted during refresh; not resurrecting",
			slog.String("subsystem", "cache"),
			slog.String("key_hash", key),
		)
		return nil
	}

	c.Put(key, &cache.ResolvedEntry{
		RawJSON: encoded,
		Inputs:  &inputs,
	})
	log.Debug("resolveAndPopulateL1: re-resolved + stored",
		slog.String("subsystem", "cache"),
		slog.String("key_hash", key),
		slog.String("handler", inputs.HandlerKind),
		slog.String("user", inputs.Username),
	)
	return nil
}

// resolveOnceProd is the production resolve-and-encode implementation.
// It re-fetches the dispatch CR named by inputs (objects.Get, under
// ctx's identity) and dispatches the matching resolver, returning the
// encoded output byte-identical to the request-path encode
// (encodeResolvedJSON — same encoder settings as a cold dispatch).
func resolveOnceProd(ctx context.Context, inputs cache.ResolvedKeyInputs) ([]byte, error) {
	authnNS := env.String("AUTHN_NAMESPACE", "")

	ref := templatesv1.ObjectReference{
		Reference: templatesv1.Reference{
			Name:      inputs.Name,
			Namespace: inputs.Namespace,
		},
		APIVersion: schemaGroupVersion(inputs.Group, inputs.Version),
		Resource:   inputs.Resource,
	}
	got := objects.Get(ctx, ref)
	if got.Err != nil {
		return nil, fmt.Errorf("re-fetch %s/%s: %s",
			inputs.Resource, inputs.Name, got.Err.Message)
	}
	if got.Unstructured == nil {
		return nil, fmt.Errorf("re-fetch %s/%s: nil object", inputs.Resource, inputs.Name)
	}

	switch inputs.HandlerKind {
	case "restactions":
		return resolveRestActionForRefresh(ctx, got, inputs, authnNS)
	case "widgets":
		return resolveWidgetForRefresh(ctx, got, inputs, authnNS)
	default:
		// Unknown handler kind — skip-to-TTL.
		return nil, nil
	}
}

// schemaGroupVersion renders the apiVersion string objects.Get expects.
// Core group ("") renders as just the version.
func schemaGroupVersion(group, version string) string {
	if group == "" {
		return version
	}
	return group + "/" + version
}

// resolveRestActionForRefresh converts the re-fetched CR to a typed
// RESTAction and dispatches restactions.Resolve, returning encoded
// output. Mirrors the restActionHandler.ServeHTTP resolve+encode path.
func resolveRestActionForRefresh(ctx context.Context, got objects.Result, inputs cache.ResolvedKeyInputs, authnNS string) ([]byte, error) {
	scheme := runtime.NewScheme()
	if err := apis.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("add apis to scheme: %w", err)
	}
	var cr templatesv1.RESTAction
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(got.Unstructured.Object, &cr); err != nil {
		return nil, fmt.Errorf("unstructured -> RESTAction: %w", err)
	}
	res, err := restactions.Resolve(ctx, restactions.ResolveOptions{
		In:      &cr,
		AuthnNS: authnNS,
		PerPage: inputs.PerPage,
		Page:    inputs.Page,
		Extras:  inputs.Extras,
	})
	if err != nil {
		return nil, fmt.Errorf("resolve RESTAction: %w", err)
	}
	return encodeResolvedJSON(res)
}

// resolveWidgetForRefresh dispatches widgets.Resolve on the re-fetched
// CR, returning encoded output. Mirrors the widgetsHandler.ServeHTTP
// resolve+encode path.
func resolveWidgetForRefresh(ctx context.Context, got objects.Result, inputs cache.ResolvedKeyInputs, authnNS string) ([]byte, error) {
	res, err := widgets.Resolve(ctx, widgets.ResolveOptions{
		In:      got.Unstructured,
		AuthnNS: authnNS,
		PerPage: inputs.PerPage,
		Page:    inputs.Page,
		Extras:  inputs.Extras,
	})
	if err != nil {
		return nil, fmt.Errorf("resolve widget: %w", err)
	}
	return encodeResolvedJSON(res)
}
