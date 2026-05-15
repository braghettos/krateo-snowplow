// prewarm.go — Tag 0.30.99 (Tag B of the unified prewarm+pivot-
// servability design): the startup navigation GVR-walk.
//
// Tag B premise (resting on Tag A): the resolver pivot can only serve a
// GVR whose informer is registered. Today the registered set at boot is
// just the four RBAC bootstrap GVRs (NewResourceWatcher) — every other
// GVR is registered lazily on first navigation touch (the resolver
// inner-call path `lazyRegisterInnerCallPaths`, the dispatcher
// dep-record path `ensureWatcherInformerForGVR`). That lazy path is
// correct but means the FIRST request for each GVR pays a cold informer
// (servable=false → apiserver fallthrough) until its informer syncs.
//
// PrewarmRegisterFromNavigation closes that cold window: at boot it
// walks the cluster's RESTAction inventory ONCE, derives the GVR set the
// navigation surface reaches (`spec.api[*].path` → GVR), and registers
// an informer for each via EnsureResourceType. By first user login the
// navigated set is registered and likely synced, so the pivot serves
// from a warm informer instead of falling through.
//
// CRITICAL — feedback_no_special_cases.md: there is ZERO hardcoded
// business-GVR list here. The GVR set is whatever
// CollectResourceTypesFromRestActions derives from the cluster's
// RESTActions — pure navigation-derived discovery. The only constant is
// the `restActionGVR` anchor needed to LIST RESTActions in the first
// place (the same anchor the inventory walker already uses) — that is
// the bare meta-query seed, not a per-resource policy.
//
// GATED DEFAULT-OFF — PREWARM_REGISTER_ENABLED (main.go). This walk does
// NOT run unless PREWARM_REGISTER_ENABLED=true. The gate exists because
// a startup GVR-walk RE-ARMS multiple documented regressions while the
// pivot consumer is off:
//
//   - No-consumer apiserver pressure (0.30.6 / 0.30.61). With
//     RESOLVER_USE_INFORMER OFF by default the resolver pivot does not
//     read from these informers. Each EnsureResourceType the walk fires
//     lands in the post-Start late-registration branch and immediately
//     spawns a LIST+WATCH against the apiserver — N informers nobody
//     reads. The 0.30.61 feature-journal entry recorded exactly this:
//     "no consumer reads from the eagerly-registered informers ...
//     eager-register = pure apiserver overhead", and that is why
//     EAGER_REGISTER_ENABLED was reverted to default-OFF.
//
//   - OOM-at-50K (0.30.8 rev 104, 0.30.92), UNMITIGATED. Composition
//     GVRs (~50K objects at production scale) take the FULL-Unstructured
//     informer, not the §0.30.93 metadata-only PartialObjectMetadata
//     path: `metadataOnlyGVRSeed` is empty (`[]gvrPattern{}`) and
//     customer core-provider CRDs are not annotated
//     `krateo.io/cache-mode: metadata`, so `shouldUseMetadataOnly`
//     returns false for them. A startup walk that registers those GVRs
//     up front incurs the same RSS burst the OOM post-mortems recorded.
//
// FIRE-AND-FORGET (the distinction from EagerRegisterAll): the walk
// calls EnsureResourceType per GVR and returns immediately — it does NOT
// WaitForCacheSync over the inventory. Each informer's initial LIST runs
// asynchronously on its own goroutine (the standard EnsureResourceType
// late-registration branch). So the walk does not add the blocking
// bulk-sync startup-latency burst EagerRegisterAll has — but
// fire-and-forget removes ONLY the blocking-sync mode; it does NOT
// remove the no-consumer apiserver-QPS or the OOM modes above. That is
// why the gate, not fire-and-forget alone, is the safety mechanism.
//
// Promotion to default-ON requires a PREWARM_REGISTER_ENABLED=true bench
// at 50K scale measuring apiserver QPS + RSS-under-load, run alongside
// RESOLVER_USE_INFORMER=true so the pivot consumer is actually present.

package cache

import (
	"context"
	"log/slog"
	"time"

	"k8s.io/client-go/dynamic"
)

// PrewarmRegisterFromNavigation walks the cluster's RESTAction inventory
// and registers an informer for every GVR the navigation surface
// reaches. Fire-and-forget: it returns once every GVR has been handed to
// EnsureResourceType — it does NOT block on informer sync.
//
// Returns the number of distinct GVRs the walk registered (added=true
// from EnsureResourceType) and the number it found already registered
// (added=false — e.g. a GVR that overlaps the RBAC bootstrap set, or a
// duplicate within the inventory). A walk-error (LIST failure) is
// returned to the caller; the caller treats it as soft (log + continue,
// the lazy register-on-navigation fallback still covers every GVR on
// first request).
//
// Nil-receiver / passthrough / cache-disabled are no-ops: there is no
// informer factory to register against. dynClient must be the same
// dynamic client used to build the watcher.
//
// Concurrency: EnsureResourceType is idempotent + singleflighted under
// rw.mu; calling it once per inventory GVR here, and again later from
// the lazy paths, is safe — the later calls observe added=false.
func (rw *ResourceWatcher) PrewarmRegisterFromNavigation(ctx context.Context, dynClient dynamic.Interface) (registered, alreadyPresent int, err error) {
	if rw == nil {
		return 0, 0, nil
	}
	if rw.mode == modePassthrough {
		// No informers in passthrough mode — every read is apiserver-
		// routed, so there is nothing to prewarm.
		slog.Info("cache.prewarm.skipped",
			slog.String("subsystem", "cache"),
			slog.String("reason", "passthrough mode — no informer factory"),
		)
		return 0, 0, nil
	}
	if dynClient == nil {
		slog.Warn("cache.prewarm.skipped",
			slog.String("subsystem", "cache"),
			slog.String("reason", "nil dynamic client"),
		)
		return 0, 0, nil
	}

	start := time.Now()

	// Navigation fan-out: the RESTAction inventory IS the navigation-
	// derived GVR set. Every RESTAction's spec.api[*].path is an
	// apiserver REST path the resolver dispatches an inner call against;
	// CollectResourceTypesFromRestActions parses each to a GVR. Widget
	// CRs reach K8s reads exclusively via apiRef→RESTAction, so the
	// RESTActions' inner-call GVRs are precisely the set widget
	// navigation touches — no separate widget-tree recursion is needed
	// to enumerate read GVRs (widget CR GVRs themselves register at
	// runtime on first dispatch via recordWidgetDeps's self-dep edge).
	inv, walkErr := CollectResourceTypesFromRestActions(ctx, dynClient)
	if walkErr != nil {
		slog.Warn("cache.prewarm.inventory_walk_failed",
			slog.String("subsystem", "cache"),
			slog.Any("err", walkErr),
			slog.String("effect", "no startup prewarm; lazy register-on-navigation still covers every GVR on first request"),
		)
		return 0, 0, walkErr
	}

	for _, gvr := range inv {
		if ctx.Err() != nil {
			// Boot context cancelled — stop early. Whatever we did not
			// reach is covered by the lazy register-on-navigation path.
			slog.Warn("cache.prewarm.context_cancelled",
				slog.String("subsystem", "cache"),
				slog.Int("registered", registered),
				slog.Int("already_present", alreadyPresent),
				slog.Int("inventory_size", len(inv)),
			)
			return registered, alreadyPresent, ctx.Err()
		}
		// Fire-and-forget: EnsureResourceType registers + spawns the
		// informer's run-loop and sync-watcher, then returns. We
		// deliberately discard the sync channel — the boot sequence does
		// not block on these informers syncing. Tag A's four-conjunct
		// servable() gate ensures any request that lands before sync
		// falls through to apiserver.
		added, _ := rw.EnsureResourceType(gvr)
		if added {
			registered++
		} else {
			alreadyPresent++
		}
	}

	slog.Info("cache.prewarm.completed",
		slog.String("subsystem", "cache"),
		slog.Int("inventory_size", len(inv)),
		slog.Int("registered", registered),
		slog.Int("already_present", alreadyPresent),
		slog.Int64("walk_ms", time.Since(start).Milliseconds()),
		slog.String("mode", "fire-and-forget — informers sync asynchronously; boot not blocked"),
	)
	return registered, alreadyPresent, nil
}
