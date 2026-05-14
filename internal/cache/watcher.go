package cache

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	clientcache "k8s.io/client-go/tools/cache"
)

// listPageLimit is the chunk size used by every informer LIST call.
// Bounded paging keeps the apiserver from streaming an unbounded
// response — matches the policy from earlier ResourceWatcher iterations
// (Q-OOM-WARMER, ship/0.25.320).
const listPageLimit int64 = 500

// watcherMode discriminates the two ResourceWatcher operational modes
// introduced at 0.30.71 ("extended CACHE_ENABLED"):
//
//   - modeInformer (CACHE_ENABLED=true, operational/production): the
//     factory is constructed, RBAC GVRs are eager-registered, the
//     SetTransform typed-RBAC pipeline runs, and every Get/List is
//     served from the informer indexer in O(1).
//
//   - modePassthrough (CACHE_ENABLED=false, diagnostic/measurement):
//     NO factory, NO informers, NO goroutines. Every Get/List call
//     reaches apiserver via the dynamic client. This is the "true
//     cache-off" baseline used to measure the L1+typed-RBAC+informer
//     stack's compound effect on warm-p50 latency. Operational
//     customers MUST NOT run in this mode (informer cache savings
//     vanish; apiserver pressure spikes).
type watcherMode int

const (
	modeInformer    watcherMode = 0 // cache=on, factory-backed
	modePassthrough watcherMode = 1 // cache=off + dyn provided, apiserver-routed
)

// RBACResourceTypes is the eager-registered Role-Based Access Control
// resource-type set (0.30.4 binding, plan §"Tag 0.30.4 What's implemented"
// bullet 1). The four GVRs are eagerly informer-registered by
// NewResourceWatcher when CACHE_ENABLED=true so EvaluateRBAC can serve
// in-process Role-Based Access Control decisions without ever calling
// SubjectAccessReview against apiserver (Revision 1 binding).
//
// Per feedback_no_special_cases.md: NO per-resource policy lives in this
// set — it is the bare minimum required by EvaluateRBAC.
var RBACResourceTypes = []schema.GroupVersionResource{
	{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"},
	{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"},
	{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"},
	{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"},
}

// ResourceWatcher is the cluster-wide informer cache. At 0.30.4 the
// factory is instantiated AND started by NewResourceWatcher when
// CACHE_ENABLED=true; the four Role-Based Access Control GVRs are
// eagerly registered. At 0.30.6 the RestAction-derived inventory is
// also eager-registered post-construction via EagerRegisterAll.
//
// All methods are safe for concurrent use. AddResourceType registers an
// informer for a GVR; Start launches them; Get/List read from the
// in-memory store; Stop cancels the underlying context and waits for
// graceful shutdown.
//
// Per feedback_no_special_cases.md the type does not embed any
// per-resource policy — every consumer treats Disabled() uniformly.
type ResourceWatcher struct {
	// mode is the operational discriminator (0.30.71). modeInformer
	// is the production path; modePassthrough routes every Get/List
	// straight to apiserver via dyn. mode is set once at construction
	// and never mutated.
	mode watcherMode

	dyn     dynamic.Interface
	factory dynamicinformer.DynamicSharedInformerFactory

	mu        sync.RWMutex
	informers map[schema.GroupVersionResource]informers.GenericInformer
	started   bool

	// syncCh stores per-GVR sync channels. Closed when that GVR's
	// informer has completed its initial WaitForCacheSync. Used by
	// EnsureResourceType so concurrent first-readers can block until
	// the informer is live, or proceed via the apiserver-fallback
	// branch in the resolver hot path. Tag 0.30.9 Sub-scope B
	// (lazy-register-on-resolver-touch).
	syncCh map[schema.GroupVersionResource]chan struct{}

	// eagerSet is the set of GVRs the caller passed to MarkEagerSet —
	// the post-startup expectation is that NO AddResourceType call
	// fires for a GVR in this set (because eager already registered
	// it). When one does, addResourceTypeLocked emits the WARN
	// "lazy-AddResourceType-unexpected" so the regression is visible.
	// nil eagerSet = "eager registration not yet completed" — no
	// WARNs fire (the constructor's own RBAC registrations are not
	// "lazy").
	eagerSet     map[schema.GroupVersionResource]struct{}
	eagerDone    bool

	stopCh chan struct{}
}

// NewResourceWatcher constructs a cluster-wide ResourceWatcher.
//
// Mode selection (0.30.71 extended CACHE_ENABLED semantics):
//
//   - CACHE_ENABLED=true (production, modeInformer): the dynamic
//     informer factory is constructed, the four Role-Based Access
//     Control GVRs are eagerly registered, the SetTransform pipeline
//     runs, and factory.Start fires. Every Get/List is served from
//     the informer indexer in O(1).
//
//   - CACHE_ENABLED=false + dyn != nil (diagnostic, modePassthrough):
//     NO factory is constructed, NO informers run, NO goroutines
//     spawn. Every Get/List is routed to apiserver via dyn. This is
//     the "true cache-off" measurement mode introduced at 0.30.71.
//     A loud WARN log is emitted so operators see immediately that
//     ALL caching layers (L1, typed-RBAC, informer) are dead.
//
//   - CACHE_ENABLED=false + dyn == nil: returns (nil, nil) for
//     backward compatibility with PM-amendment-1 tests that asserted
//     "dormant when disabled" before 0.30.71 existed.
//
// Callers MUST nil-check the return value: when nil, every consumer
// takes the apiserver branch. When non-nil in passthrough mode,
// Get/List still route to apiserver — the difference is that the
// watcher API stays callable instead of forcing every consumer to
// nil-check + duplicate the apiserver branch.
//
// At 0.30.4 (Revision 1 binding) cache=on mode eagerly registers the
// Role-Based Access Control GVRs (Role, RoleBinding, ClusterRole,
// ClusterRoleBinding) and starts the factory so EvaluateRBAC can serve
// in-process Role-Based Access Control decisions without ever calling
// SubjectAccessReview against apiserver.
func NewResourceWatcher(ctx context.Context, dyn dynamic.Interface) (*ResourceWatcher, error) {
	if Disabled() {
		// 0.30.71 split: with dyn=nil we preserve the pre-0.30.71
		// (nil, nil) contract that watcher_test.go's dormancy tests
		// rely on. With dyn != nil we build a passthrough watcher
		// so consumers that hold the watcher pointer can still call
		// Get/List; every call routes to apiserver.
		if dyn == nil {
			slog.Info("cache.disabled=true",
				slog.String("subsystem", "cache"),
				slog.Bool("plumbing_present", true),
				slog.Bool("routed", false),
				slog.String("mode", "dormant-no-dyn"),
			)
			return nil, nil
		}

		slog.Warn(
			"CACHE_ENABLED=false — typed-RBAC + informer cache + L1 ALL disabled (diagnostic mode; do not run in production)",
			slog.String("subsystem", "cache"),
			slog.Bool("plumbing_present", true),
			slog.Bool("routed", true),
			slog.String("mode", "passthrough"),
			slog.String("rationale", "0.30.71: true cache-off baseline for warm-p50 measurement"),
			slog.String("rbac.evaluate_path", "SubjectAccessReview"),
			slog.String("informer.get_list_path", "apiserver"),
			slog.String("l1.resolved_cache_path", "disabled"),
		)
		_ = ctx
		return &ResourceWatcher{
			mode:      modePassthrough,
			dyn:       dyn,
			informers: map[schema.GroupVersionResource]informers.GenericInformer{},
			syncCh:    map[schema.GroupVersionResource]chan struct{}{},
			stopCh:    make(chan struct{}),
		}, nil
	}

	if dyn == nil {
		return nil, fmt.Errorf("cache: NewResourceWatcher requires non-nil dynamic.Interface")
	}

	tweak := func(opts *metav1.ListOptions) {
		opts.Limit = listPageLimit
	}

	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		dyn,
		0, // resync period 0 — pure event-driven; no periodic full re-list
		metav1.NamespaceAll,
		tweak,
	)

	rw := &ResourceWatcher{
		mode:      modeInformer,
		dyn:       dyn,
		factory:   factory,
		informers: map[schema.GroupVersionResource]informers.GenericInformer{},
		syncCh:    map[schema.GroupVersionResource]chan struct{}{},
		stopCh:    make(chan struct{}),
	}
	_ = ctx // reserved for future wiring (0.30.6 eager-registration caller may pass-through)

	// Revision 1 binding: register the four Role-Based Access Control
	// GVRs eagerly and start the factory. This is the single set of
	// types EvaluateRBAC reads from; without these we cannot meet the
	// "zero SubjectAccessReview in cache=on" rule.
	for _, gvr := range RBACResourceTypes {
		rw.addResourceTypeLocked(gvr)
	}

	// 0.30.5: install the SetTransform strip BEFORE factory.Start
	// (primer §4.7). The TransformFunc drops managedFields and the
	// last-applied-configuration annotation from every object before
	// it lands in the indexer. SetTransform returns an error only when
	// the informer has already started — at this point we have not
	// called Start yet, so the error path is unreachable. We log a
	// WARN if it ever happens to surface the regression rather than
	// failing the boot.
	for gvr, gi := range rw.informers {
		resourceType := gvrResourceTypeString(gvr)
		tf := StripBulkyFieldsForResourceType(resourceType, gvr)
		if err := gi.Informer().SetTransform(tf); err != nil {
			slog.Warn("cache.strip.set_transform_failed",
				slog.String("subsystem", "cache"),
				slog.String("resource_type", resourceType),
				slog.String("error", err.Error()),
			)
		}
	}

	// 0.30.6 plan §Risks bullet 1 — startup assertion. The typed
	// RBAC overrides MUST be registered BEFORE factory.Start so
	// every Add/Update event fires through the typed transform.
	// Registration happens at package init() in strip.go; if it
	// regressed (someone deletes the init or renames the GVR), this
	// panics with the missing GVR so the regression cannot ship
	// silently.
	AssertRBACTypedOverridesRegistered()

	rw.factory.Start(rw.stopCh)
	rw.started = true

	// 0.30.9 Sub-scope B: now that the factory has started the
	// constructor-registered informers (the four RBAC GVRs),
	// spawn one sync-watcher per GVR so EnsureResourceType callers
	// for those GVRs (rare — only the test path; production callers
	// go through ListTypedObjects / GetTypedObject) see a closed
	// channel as soon as HasSynced flips.
	for gvr, gi := range rw.informers {
		ch := rw.syncCh[gvr]
		if ch == nil {
			continue
		}
		go waitInformerSync(gi.Informer().HasSynced, ch, rw.stopCh)
	}

	slog.Info("cache.plumbing_present=true cache.routed=true rbac.informer_started=true",
		slog.String("subsystem", "cache"),
		slog.Int64("list_page_limit", listPageLimit),
		slog.Int("resource_types_registered", len(rw.informers)),
		slog.String("rbac.evaluate_path", "in-process"),
		slog.String("subject_access_review_calls_in_cache_on_path", "banned"),
	)

	return rw, nil
}

// Mode returns the operational discriminator (modeInformer or
// modePassthrough). Exposed for falsifier-log diagnostics and unit
// tests; production callers should rely on Disabled() / Get-List
// behaviour instead.
func (rw *ResourceWatcher) Mode() watcherMode {
	if rw == nil {
		return modeInformer // unreachable: callers nil-check upstream
	}
	return rw.mode
}

// IsPassthrough reports whether the watcher routes Get/List to
// apiserver (0.30.71 diagnostic mode). A nil receiver returns false
// so call sites stay terse.
func (rw *ResourceWatcher) IsPassthrough() bool {
	return rw != nil && rw.mode == modePassthrough
}

// AddResourceType registers an informer for gvr. Idempotent: calling
// twice for the same GVR is a no-op. Safe for concurrent use.
//
// In modePassthrough (0.30.71) this is a no-op: every Get/List call
// is already apiserver-routed via the dynamic client, so there is
// nothing to register and no informer to start.
//
// At 0.30.4 the constructor eagerly registers the four Role-Based
// Access Control GVRs. At 0.30.6 the inventory walker (gated behind
// EAGER_REGISTER_ENABLED at 0.30.61) covered the RestAction-derived
// inventory. At 0.30.9 (Sub-scope B), the canonical lazy-registration
// API for resolver-hot-path callers is EnsureResourceType — it returns
// the per-GVR sync channel so callers can singleflight first-reads.
// AddResourceType is preserved for back-compat with EagerRegisterAll.
func (rw *ResourceWatcher) AddResourceType(gvr schema.GroupVersionResource) {
	if rw.mode == modePassthrough {
		return
	}
	rw.mu.Lock()
	defer rw.mu.Unlock()

	rw.addResourceTypeLocked(gvr)
}

// EnsureResourceType is the idempotent, sync-channel-returning lazy
// registration API introduced at 0.30.9 (Sub-scope B). Behaviour:
//
//   - If gvr is already registered: returns (false, sync), where sync
//     is the channel that was created on first registration. The
//     channel is closed once that informer's initial WaitForCacheSync
//     completes. Callers may either block on it (to wait for the
//     informer cache to be live) or proceed via apiserver-fallback.
//   - If gvr is not yet registered: registers it under rw.mu (which
//     serves as the singleflight primitive — no separate sync.Once
//     needed), kicks off the informer goroutine (late-registration
//     branch), spawns a sync-watcher that closes sync on
//     WaitForCacheSync completion, and returns (true, sync).
//
// In modePassthrough (0.30.71) this is a no-op: returns (false, nil).
// Callers in passthrough mode never hit the informer code path —
// every Get/List routes to apiserver via the dynamic client — so
// there is no informer to register and no sync channel to honour.
//
// Per plan §"Singleflight on EnsureResourceType" (binding): rw.mu IS
// the singleflight primitive. Concurrent first-reads for the same GVR
// see exactly one factory.ForResource + Informer().Run call; every
// subsequent caller receives the same sync channel and the same
// informer (via the existing AddResourceType idempotence).
//
// Per feedback_l1_invalidation_delete_only.md + Revision 17 plan
// constraint: the dep-tracker handlers (UpdateFunc/DeleteFunc) are
// wired by addResourceTypeLocked at registration time. The sync
// channel exists so callers in the resolver hot path can know when
// the informer is live; the dep-tracker handlers themselves fire
// independently of the channel state.
//
// Safe for concurrent use.
func (rw *ResourceWatcher) EnsureResourceType(gvr schema.GroupVersionResource) (added bool, sync <-chan struct{}) {
	if rw == nil {
		return false, nil
	}
	if rw.mode == modePassthrough {
		return false, nil
	}

	rw.mu.Lock()
	defer rw.mu.Unlock()

	if _, exists := rw.informers[gvr]; exists {
		// Hit: return the existing sync channel. Defensive nil-check
		// — the constructor's RBAC registrations always allocate a
		// channel, but a future refactor could break this invariant.
		if ch, ok := rw.syncCh[gvr]; ok {
			return false, ch
		}
		// Defensive: invariant broken. Return a pre-closed channel
		// so callers don't deadlock.
		closed := make(chan struct{})
		close(closed)
		return false, closed
	}

	// Miss: register + allocate the sync channel + spawn the
	// sync-watcher goroutine. addResourceTypeLocked allocates the
	// channel and stores it in rw.syncCh; we read it here and
	// spawn the watcher so the channel closes on HasSynced.
	rw.addResourceTypeLocked(gvr)
	ch := rw.syncCh[gvr]

	// Falsifier per plan §"Pre-flight RUN protocol" step 2: emit a
	// log line `cache.lazy_register fired gvr=...` on first
	// registration. Distinct from `lazy-AddResourceType` (the
	// 0.30.6 falsifier triggered by post-eager-done lazy adds).
	slog.Info("cache.lazy_register",
		slog.String("subsystem", "cache"),
		slog.String("gvr", gvr.String()),
		slog.String("hint", "first resolver touch — informer registered + dep-tracker handlers wired"),
	)

	return true, ch
}

// addResourceTypeLocked is the lock-held implementation of
// AddResourceType. Callers MUST hold rw.mu.Lock().
//
// 0.30.5: every newly-added informer also has the SetTransform strip
// installed — including informers added lazily after Start(). For
// post-Start registration SetTransform returns an error (cannot mutate
// a running informer); we log it as a WARN so the regression is
// observable but do NOT fail the registration (the cache still works,
// just at higher memory cost for that GVR).
//
// 0.30.6: post-eager-registration calls into AddResourceType are
// expected to be rare — the inventory walker is supposed to cover the
// full RestAction-derived GVR set. When a lazy registration fires AND
// eager registration has completed AND the GVR was in the eager
// inventory set, we emit `lazy-AddResourceType-unexpected` so the
// regression is loud. Lazy registration for a GVR NOT in the eager
// inventory is normal (e.g. customer-added RestAction post-startup).
func (rw *ResourceWatcher) addResourceTypeLocked(gvr schema.GroupVersionResource) {
	// 0.30.71 + 0.30.8: defensive guard. AddResourceType already
	// early-returns in modePassthrough, and NewResourceWatcher's
	// passthrough branch never reaches addResourceTypeLocked (no
	// factory exists in passthrough mode). This re-asserts the
	// invariant in case a future caller routes around AddResourceType
	// — without it, rw.factory.ForResource(gvr) on the next line
	// would nil-panic.
	if rw.mode == modePassthrough {
		return
	}
	if _, exists := rw.informers[gvr]; exists {
		return
	}

	gi := rw.factory.ForResource(gvr)
	rw.informers[gvr] = gi

	// 0.30.9 Sub-scope B: allocate the sync channel BEFORE we spawn
	// any goroutine that could close it. The channel is closed by
	// the late-registration sync-watcher (below, in the rw.started
	// branch) or by the constructor-driven post-Start sync watcher
	// (registered in NewResourceWatcher's terminal block). We
	// allocate the channel here unconditionally so EnsureResourceType
	// can return it for either path.
	if rw.syncCh == nil {
		rw.syncCh = map[schema.GroupVersionResource]chan struct{}{}
	}
	rw.syncCh[gvr] = make(chan struct{})

	resourceType := gvrResourceTypeString(gvr)
	tf := StripBulkyFieldsForResourceType(resourceType, gvr)
	if err := gi.Informer().SetTransform(tf); err != nil {
		slog.Warn("cache.strip.set_transform_failed",
			slog.String("subsystem", "cache"),
			slog.String("resource_type", resourceType),
			slog.String("error", err.Error()),
			slog.Bool("post_start", rw.started),
		)
	}

	// 0.30.8: dep-tracker event hooks for the L1 resolved-output
	// cache. Installed at registration time so every newly-added
	// informer gains wiring on first use (covers both eager + lazy
	// AddResourceType paths). Per
	// feedback_l1_invalidation_delete_only.md, DELETE evicts, UPDATE
	// enqueues refresh, ADD is a deliberate no-op for dep-tracking
	// (the informer still consumes ADD for its own LIST/WATCH state).
	//
	// Mode-gating (0.30.71 + 0.30.8): these handlers are wired ONLY
	// in modeInformer. In modePassthrough the early-return at the
	// top of this function fires; in modePassthrough L1 is also off
	// (ResolvedCache() returns nil) so a dep tracker without a store
	// would record forward edges that the watcher could never
	// invalidate — wiring them at all in passthrough is wasted work.
	//
	// The handlers run on the informer's processor goroutine. Both
	// OnDelete and OnUpdate complete in O(deps-for-this-tuple); the
	// hot path is sync.Map operations.
	if _, regErr := gi.Informer().AddEventHandler(clientcache.ResourceEventHandlerFuncs{
		UpdateFunc: func(_, newObj interface{}) {
			ns, name := metaNSName(newObj)
			Deps().OnUpdate(gvr, ns, name)
		},
		DeleteFunc: func(obj interface{}) {
			// DeletedFinalStateUnknown wraps the last-known object
			// when the watcher missed the explicit DELETE. Unwrap
			// so we still get the (ns, name) tuple.
			if tomb, ok := obj.(clientcache.DeletedFinalStateUnknown); ok {
				obj = tomb.Obj
			}
			ns, name := metaNSName(obj)
			Deps().OnDelete(gvr, ns, name)
		},
		// AddFunc is intentionally NOT wired. Pre-flight falsifier
		// (probe.log 2026-05-13) showed the scale-up CREATE-event
		// transient does not reproduce on 0.30.7; the Revision 16
		// scope expansion was rolled back.
	}); regErr != nil {
		slog.Warn("cache.deps.add_event_handler_failed",
			slog.String("subsystem", "cache"),
			slog.String("resource_type", resourceType),
			slog.String("error", regErr.Error()),
		)
	}

	if rw.started {
		// Late registration after Start(): kick the new informer.
		go gi.Informer().Run(rw.stopCh)

		// 0.30.9 Sub-scope B: spawn the sync-watcher for this GVR.
		// The watcher polls HasSynced (cheap atomic load in
		// client-go) and closes the sync channel as soon as the
		// informer's initial LIST is reconciled. We use a polling
		// loop bounded by rw.stopCh so the goroutine exits when
		// the watcher Stop()s. WaitForCacheSync (client-go) uses
		// the same polling primitive internally — we re-implement
		// it here so we don't need to allocate a context.
		ch := rw.syncCh[gvr]
		go waitInformerSync(gi.Informer().HasSynced, ch, rw.stopCh)

		// 0.30.6 falsifier (plan §"Code-path falsifier"). If eager
		// registration has already completed AND this GVR was in
		// the eager set, the inventory walker missed it OR an
		// upstream caller is double-registering — either way the
		// SRE wants to see it.
		if rw.eagerDone {
			if _, wasInEager := rw.eagerSet[gvr]; wasInEager {
				slog.Warn("lazy-AddResourceType-unexpected",
					slog.String("subsystem", "cache"),
					slog.String("resource_type", resourceType),
					slog.String("hint", "was in eager inventory but registered lazily"),
				)
			} else {
				slog.Info("lazy-AddResourceType",
					slog.String("subsystem", "cache"),
					slog.String("resource_type", resourceType),
					slog.String("hint", "not in eager inventory — likely post-startup RestAction"),
				)
			}
		}
	}
}

// waitInformerSync polls the informer's HasSynced predicate and
// closes ch when it returns true OR stopCh is closed. Polled at
// 50ms — same cadence client-go uses internally (cache.WaitForCacheSync
// polls at 100ms; we use half that so callers see the live state a
// tick earlier on the first read). The goroutine is bounded by
// stopCh so Stop() reliably reaps it.
//
// Idempotent on ch close — we only close once. If stopCh fires before
// the informer syncs, ch is closed anyway so callers blocked on it
// unblock (they will see HasSynced()==false on a follow-up read and
// fall back to the apiserver path).
func waitInformerSync(hasSynced func() bool, ch chan struct{}, stopCh <-chan struct{}) {
	defer func() {
		select {
		case <-ch:
			// Already closed (e.g., constructor's bulk-sync path).
		default:
			close(ch)
		}
	}()
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	if hasSynced() {
		return
	}
	for {
		select {
		case <-stopCh:
			return
		case <-t.C:
			if hasSynced() {
				return
			}
		}
	}
}

// MarkEagerSet records the set of GVRs that were registered via the
// eager-registration pathway (Tag 0.30.6). After this call, lazy
// AddResourceType for any GVR in `eagerSet` emits the
// `lazy-AddResourceType-unexpected` WARN — the gap is a falsifier the
// PM gate verifies via `kubectl logs`.
//
// Calling MarkEagerSet with a nil slice is permitted (resets the
// eager-done flag back to false — used by tests).
//
// Safe for concurrent use.
func (rw *ResourceWatcher) MarkEagerSet(eagerSet []schema.GroupVersionResource) {
	rw.mu.Lock()
	defer rw.mu.Unlock()

	if eagerSet == nil {
		rw.eagerSet = nil
		rw.eagerDone = false
		return
	}
	m := make(map[schema.GroupVersionResource]struct{}, len(eagerSet))
	for _, gvr := range eagerSet {
		m[gvr] = struct{}{}
	}
	rw.eagerSet = m
	rw.eagerDone = true
}

// metaNSName extracts (namespace, name) from an informer-event object.
// Returns ("", "") if obj is not a metav1-conforming runtime object.
//
// The function tolerates both *unstructured.Unstructured and any typed
// object that implements GetNamespace/GetName (the four RBAC types are
// typed; everything else is unstructured at 0.30.5+).
func metaNSName(obj interface{}) (string, string) {
	type nsNameAccessor interface {
		GetNamespace() string
		GetName() string
	}
	if a, ok := obj.(nsNameAccessor); ok {
		return a.GetNamespace(), a.GetName()
	}
	return "", ""
}

// gvrResourceTypeString renders gvr as "group/version/Resource" for the
// strip.applied falsifier log line. The core group renders as
// "core/v1/Resource" (rather than "/v1/Resource") so log readers don't
// need to special-case the empty-group case.
func gvrResourceTypeString(gvr schema.GroupVersionResource) string {
	group := gvr.Group
	if group == "" {
		group = "core"
	}
	return group + "/" + gvr.Version + "/" + gvr.Resource
}

// Start launches every registered informer and begins serving from the
// in-memory cache. Idempotent.
//
// At 0.30.4 NewResourceWatcher invokes Start() automatically after
// eager RBAC registration — callers normally do not need to call this
// directly. Future tags may use it for lazy GVR registration scenarios.
func (rw *ResourceWatcher) Start() {
	if rw.mode == modePassthrough {
		return
	}
	rw.mu.Lock()
	defer rw.mu.Unlock()

	if rw.started {
		return
	}
	rw.started = true
	rw.factory.Start(rw.stopCh)
}

// WaitForCacheSync blocks until every registered informer's local
// store is in sync with apiserver, or the timeout elapses. Returns nil
// on success, error on timeout or context cancellation.
//
// In modePassthrough (0.30.71) there is no informer to sync — every
// read goes to apiserver — so the function returns nil immediately.
func (rw *ResourceWatcher) WaitForCacheSync(ctx context.Context, timeout time.Duration) error {
	if rw.mode == modePassthrough {
		return nil
	}
	rw.mu.RLock()
	syncs := make([]clientcache.InformerSynced, 0, len(rw.informers))
	for _, gi := range rw.informers {
		syncs = append(syncs, gi.Informer().HasSynced)
	}
	rw.mu.RUnlock()

	if len(syncs) == 0 {
		return nil
	}

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if !clientcache.WaitForCacheSync(cctx.Done(), syncs...) {
		return fmt.Errorf("cache: sync timeout after %s", timeout)
	}
	return nil
}

// passthroughGetTimeout bounds the apiserver Get/List call in
// modePassthrough so a stalled apiserver cannot wedge a caller
// indefinitely. 30s mirrors the dynamic.Client default behaviour
// elsewhere in snowplow.
const passthroughGetTimeout = 30 * time.Second

// GetObject returns the unstructured object for (gvr, namespace,
// name) or (nil, false) when missing.
//
// In modeInformer (cache=on) the lookup is served from the informer
// indexer in O(1).
//
// In modePassthrough (cache=off + dyn provided, 0.30.71) the lookup
// is routed to apiserver via the dynamic client. Each call is a fresh
// apiserver Get; there is NO in-process caching. This is the "true
// cache-off" path the diagnostic mode promises.
func (rw *ResourceWatcher) GetObject(gvr schema.GroupVersionResource, namespace, name string) (*unstructured.Unstructured, bool) {
	if rw.mode == modePassthrough {
		ctx, cancel := context.WithTimeout(context.Background(), passthroughGetTimeout)
		defer cancel()
		var uns *unstructured.Unstructured
		var err error
		if namespace == "" {
			uns, err = rw.dyn.Resource(gvr).Get(ctx, name, metav1.GetOptions{})
		} else {
			uns, err = rw.dyn.Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		}
		if err != nil || uns == nil {
			return nil, false
		}
		return uns, true
	}

	rw.mu.RLock()
	gi, ok := rw.informers[gvr]
	rw.mu.RUnlock()

	if !ok {
		return nil, false
	}

	key := name
	if namespace != "" {
		key = namespace + "/" + name
	}

	obj, exists, err := gi.Informer().GetIndexer().GetByKey(key)
	if err != nil || !exists {
		return nil, false
	}

	uns, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil, false
	}
	return uns, true
}

// ListObjects returns every object for gvr scoped to namespace.
// Pass empty string for cluster-wide listing.
//
// In modeInformer (cache=on) the list is served from the informer
// indexer in O(N) over the namespace partition.
//
// In modePassthrough (cache=off + dyn provided, 0.30.71) the list is
// routed to apiserver via the dynamic client with the same
// listPageLimit bounded paging policy used by the informer factory.
// Each call is a fresh apiserver LIST; there is NO in-process
// caching. Paging is iterated until Continue is empty so callers see
// the full set.
func (rw *ResourceWatcher) ListObjects(gvr schema.GroupVersionResource, namespace string) []*unstructured.Unstructured {
	if rw.mode == modePassthrough {
		return rw.listPassthrough(gvr, namespace)
	}
	rw.mu.RLock()
	gi, ok := rw.informers[gvr]
	rw.mu.RUnlock()

	if !ok {
		return nil
	}

	store := gi.Informer().GetIndexer()
	var items []interface{}
	if namespace == "" {
		items = store.List()
	} else {
		idx, err := store.ByIndex(clientcache.NamespaceIndex, namespace)
		if err != nil {
			items = filterByNamespace(store.List(), namespace)
		} else {
			items = idx
		}
	}

	out := make([]*unstructured.Unstructured, 0, len(items))
	for _, it := range items {
		if uns, ok := it.(*unstructured.Unstructured); ok {
			out = append(out, uns)
		}
	}
	return out
}

// listPassthrough is the modePassthrough implementation of ListObjects.
// Iterates apiserver LIST with bounded paging until Continue is empty.
// Errors are swallowed (logged at debug) and a possibly-partial slice
// is returned — same contract the informer indexer gives on a partial
// sync: callers MUST be tolerant of empty / partial results.
func (rw *ResourceWatcher) listPassthrough(gvr schema.GroupVersionResource, namespace string) []*unstructured.Unstructured {
	ctx, cancel := context.WithTimeout(context.Background(), passthroughGetTimeout)
	defer cancel()

	var out []*unstructured.Unstructured
	var continueToken string
	for {
		opts := metav1.ListOptions{Limit: listPageLimit, Continue: continueToken}
		var list *unstructured.UnstructuredList
		var err error
		if namespace == "" {
			list, err = rw.dyn.Resource(gvr).List(ctx, opts)
		} else {
			list, err = rw.dyn.Resource(gvr).Namespace(namespace).List(ctx, opts)
		}
		if err != nil || list == nil {
			slog.Debug("cache.passthrough.list_failed",
				slog.String("subsystem", "cache"),
				slog.String("gvr", gvr.String()),
				slog.String("namespace", namespace),
				slog.Any("err", err),
			)
			return out
		}
		for i := range list.Items {
			item := list.Items[i]
			out = append(out, &item)
		}
		continueToken = list.GetContinue()
		if continueToken == "" {
			return out
		}
	}
}

func filterByNamespace(items []interface{}, ns string) []interface{} {
	out := make([]interface{}, 0, len(items))
	for _, it := range items {
		uns, ok := it.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		if uns.GetNamespace() == ns {
			out = append(out, it)
		}
	}
	return out
}

// GetTypedObject returns the cached object for (gvr, namespace, name)
// as a runtime.Object — without the *unstructured.Unstructured
// type-assert that GetObject performs. Used by callers that opt into
// the 0.30.6 typed-converting transform: the indexer entry is a typed
// pointer (e.g. *rbacv1.ClusterRoleBinding) and the caller does the
// final type-assert at the call-site.
//
// Returns (nil, false) when the GVR is not registered, the key is
// missing, or the underlying object is nil.
//
// In modeInformer (cache=on) this serves typed pointers in O(1) with
// zero per-call FromUnstructured cost. In modePassthrough (cache=off
// + dyn provided, 0.30.71) the call routes to apiserver and returns
// the resulting *unstructured.Unstructured (which IS a runtime.Object)
// — the caller's as{Kind} helper in internal/rbac/evaluate.go falls
// through to the Unstructured fallback path (FromUnstructured per
// call). That is exactly the "original FromUnstructured-based RBAC
// evaluation" the diagnostic mode promises.
//
// For non-RBAC callers that still want the Unstructured (e.g.
// resolver-side reads), GetObject is preserved unchanged.
func (rw *ResourceWatcher) GetTypedObject(gvr schema.GroupVersionResource, namespace, name string) (runtime.Object, bool) {
	if rw.mode == modePassthrough {
		uns, ok := rw.GetObject(gvr, namespace, name)
		if !ok || uns == nil {
			return nil, false
		}
		return uns, true
	}
	rw.mu.RLock()
	gi, ok := rw.informers[gvr]
	rw.mu.RUnlock()

	if !ok {
		return nil, false
	}

	key := name
	if namespace != "" {
		key = namespace + "/" + name
	}

	obj, exists, err := gi.Informer().GetIndexer().GetByKey(key)
	if err != nil || !exists {
		return nil, false
	}

	robj, ok := obj.(runtime.Object)
	if !ok {
		return nil, false
	}
	return robj, true
}

// ListTypedObjects returns every object for gvr scoped to namespace
// as []runtime.Object — without the *unstructured.Unstructured
// type-assert that ListObjects performs. Caller does the final type
// assert at the call-site.
//
// Pass empty namespace for cluster-wide listing.
//
// In modeInformer (cache=on) the list is served from the informer
// indexer with zero per-call FromUnstructured cost.
//
// In modePassthrough (cache=off + dyn provided, 0.30.71) the list is
// routed to apiserver and the returned []runtime.Object holds
// *unstructured.Unstructured values — callers' as{Kind} helpers fall
// through to FromUnstructured (the "original" RBAC path).
func (rw *ResourceWatcher) ListTypedObjects(gvr schema.GroupVersionResource, namespace string) []runtime.Object {
	if rw.mode == modePassthrough {
		uns := rw.listPassthrough(gvr, namespace)
		out := make([]runtime.Object, 0, len(uns))
		for _, u := range uns {
			out = append(out, u)
		}
		return out
	}
	rw.mu.RLock()
	gi, ok := rw.informers[gvr]
	rw.mu.RUnlock()

	if !ok {
		return nil
	}

	store := gi.Informer().GetIndexer()
	var items []interface{}
	if namespace == "" {
		items = store.List()
	} else {
		idx, err := store.ByIndex(clientcache.NamespaceIndex, namespace)
		if err != nil {
			items = filterRuntimeByNamespace(store.List(), namespace)
		} else {
			items = idx
		}
	}

	out := make([]runtime.Object, 0, len(items))
	for _, it := range items {
		if robj, ok := it.(runtime.Object); ok {
			out = append(out, robj)
		}
	}
	return out
}

// filterRuntimeByNamespace is the runtime.Object analogue of
// filterByNamespace. Used by ListTypedObjects only when the indexer
// is missing the NamespaceIndex (rare; defensive).
func filterRuntimeByNamespace(items []interface{}, ns string) []interface{} {
	out := make([]interface{}, 0, len(items))
	for _, it := range items {
		// metav1.Object interface gives us GetNamespace without type
		// assertion against either Unstructured or typed kinds.
		type nsAccessor interface{ GetNamespace() string }
		if a, ok := it.(nsAccessor); ok && a.GetNamespace() == ns {
			out = append(out, it)
		}
	}
	return out
}

// MatchingObjects returns cached objects in namespace whose labels
// match selector. Use this for label-selected reads instead of
// post-filtering ListObjects, when the indexer can short-circuit.
func (rw *ResourceWatcher) MatchingObjects(gvr schema.GroupVersionResource, namespace string, selector labels.Selector) []*unstructured.Unstructured {
	all := rw.ListObjects(gvr, namespace)
	if selector == nil || selector.Empty() {
		return all
	}
	out := make([]*unstructured.Unstructured, 0, len(all))
	for _, uns := range all {
		if selector.Matches(labels.Set(uns.GetLabels())) {
			out = append(out, uns)
		}
	}
	return out
}

// Stop signals every informer goroutine to exit. Idempotent.
func (rw *ResourceWatcher) Stop() {
	rw.mu.Lock()
	defer rw.mu.Unlock()

	select {
	case <-rw.stopCh:
		// Already closed.
	default:
		close(rw.stopCh)
	}
}

// global holds the cluster-wide ResourceWatcher singleton wired in
// main.go. Cache=on consumers read it via Global(); a nil return is the
// canonical cache=off branch signal.
//
// We accept a package-level singleton here because:
//   - the watcher is genuinely process-scoped (one factory per pod);
//   - threading it through every resolver call site would touch ~30
//     unrelated files for no behavioural gain;
//   - the cache=off branch is encoded as nil — there is no other
//     "disabled" state to model.
//
// Per feedback_no_special_cases.md the singleton holds no per-resource
// or per-user policy: it is a pointer or it is nil.
var (
	globalMu      sync.RWMutex
	globalWatcher *ResourceWatcher
)

// SetGlobal wires rw as the process-scoped ResourceWatcher. Called once
// from main.go after NewResourceWatcher succeeds. Passing nil clears
// the singleton — used by tests and by the cache=off path.
func SetGlobal(rw *ResourceWatcher) {
	globalMu.Lock()
	globalWatcher = rw
	globalMu.Unlock()
}

// Global returns the process-scoped ResourceWatcher or nil when the
// cache subsystem is disabled / not yet wired. Cache=on consumers MUST
// nil-check the return value.
func Global() *ResourceWatcher {
	globalMu.RLock()
	defer globalMu.RUnlock()
	return globalWatcher
}
