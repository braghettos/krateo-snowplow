package dispatchers

import (
	"context"
	"net/http"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

func All() map[string]http.Handler {
	return map[string]http.Handler{
		"restactions.templates.krateo.io": RESTAction(),
		"widgets.templates.krateo.io":     Widgets(),
	}
}

// RegisterRefreshHandlers wires the L1 resolved-output cache refresher
// callbacks for the two dispatcher kinds. MUST be called AFTER
// ResolvedCache() is built (so the cache singleton exists) and BEFORE
// cache.StartRefresher (so the worker pool sees populated handlers on
// first dequeue).
//
// Ship C (0.30.112): the handlers are REAL — each delegates to the
// shared resolveAndPopulateL1, which re-resolves the entry from its own
// ResolvedKeyInputs under the entry's own identity and Put()s the fresh
// bytes back into L1. resolveAndPopulateL1 installs WithL1KeyContext
// internally so the resolver re-records dep edges (the inner object set
// may have changed since the original resolve). It only ever Put()s —
// never evicts — so the stale-while-revalidate contract
// (feedback_l1_invalidation_delete_only.md) holds.
//
// A non-nil error from resolveAndPopulateL1 propagates to the refresher,
// which requeues the key with bounded exponential backoff. A legacy
// nil-Inputs entry never reaches here — the refresher skips it before
// dispatching a handler.
func RegisterRefreshHandlers() {
	refreshFunc := func(ctx context.Context, _ string, in cache.ResolvedKeyInputs) error {
		// resolveAndPopulateL1 computes the canonical key from `in`,
		// installs WithL1KeyContext, builds the entry's own identity
		// context, re-resolves, re-checks liveness, and Put()s. The
		// `key` argument is redundant with ComputeKey(in) — the shared
		// path recomputes it so prewarm (Ship F) can reuse the body
		// without a pre-known key.
		return resolveAndPopulateL1(ctx, in)
	}
	cache.RegisterRefreshFunc("restactions", refreshFunc)
	cache.RegisterRefreshFunc("widgets", refreshFunc)
}
