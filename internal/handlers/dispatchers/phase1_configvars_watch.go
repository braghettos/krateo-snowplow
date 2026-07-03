// phase1_configvars_watch.go — boot-race-tolerant prewarm (shape A,
// docs/prewarm-boot-race-tolerant-2026-07-03.md §2.2).
//
// THE TRIGGER INVERSION. Phase-1 prewarm used to start eagerly at boot and
// read the frontend `*-config-vars` ConfigMap exactly once (phase1_roots.go
// readFrontendConfig). On a fresh install where snowplow boots BEFORE the
// frontend has created that ConfigMap, the one-shot read finds nothing, the
// walk gives up warming, and the pod serves the first navigation COLD for
// its whole lifetime (the rt8rv 2026-07-03 boot race).
//
// The fix makes the APPEARANCE of the config-vars ConfigMap the event that
// DRIVES the prewarm walk. A namespaced, single-object (field-selected)
// ConfigMap informer is registered at Phase-1 start on the process-lifetime
// cacheCtx; its AddFunc (ConfigMap appeared) and UpdateFunc (config.json
// changed) enqueue the engine's scopeKindBoot re-drive. rePrewarmBoot then
// reads config-vars (now present), walks the nav roots, harvests, and seeds
// — the whole path already exists (prewarm_engine_boot.go). Idempotent by
// the "boot" dedup key (prewarm_engine.go:206), and the engine worker
// outlives boot (bound to cacheCtx via SetEngineProcessContext), so the
// re-drive fires whether the ConfigMap arrives before OR after the readiness
// backstop — self-heal with zero pod restart.
//
// NO NEW ENV, NO HARDCODED NAME. The watched ConfigMap name/namespace are
// the SAME config the eager read uses: name = FRONTEND_CONFIG_CONFIGMAP
// (default frontend-config-vars, phase1_roots.go:66-71), namespace = the
// AUTHN_NAMESPACE passed to Phase1Warmup. The name/namespace NEVER appear as
// Go literals here (feedback_no_special_cases): a single-object field
// selector is built from frontendConfigConfigMapName() + authnNS.
//
// HOOK-MUST-NOT-BLOCK. AddFunc/UpdateFunc ONLY call the O(1) non-blocking
// enqueueScope (prewarm_engine.go:273-282) — no inline walk work. The walk
// runs on the engine worker goroutine under its customer-priority yield +
// bounded queue (prewarm_engine_boot.go:105-108 contract).
//
// RBAC. The snowplow SA holds native `*/*` get/list/watch (phase1_walk.go:50
// cites the ClusterRoleBinding), so a namespaced ConfigMap WATCH is already
// authorized. No chart/RBAC change.

package dispatchers

import (
	"context"
	"log/slog"
	"sync/atomic"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	clientcache "k8s.io/client-go/tools/cache"

	corev1 "k8s.io/api/core/v1"
)

// configVarsWatchStarted guards StartConfigVarsWatch against a double start
// (idempotent per process). The first successful start wins; later calls are
// no-ops. Kept package-level (not on a struct) to mirror the other Phase-1
// singletons (seedTokenProvider, engineProcessCtx).
var configVarsWatchStarted atomic.Bool

// configVarsWatchClientForTest injects a fake typed clientset so the
// falsifier can drive AddFunc/UpdateFunc off a real client-go informer
// without a live apiserver. nil in production. Mirrors
// cache.SetSecretsClientForTest.
var configVarsWatchClientForTest atomic.Pointer[kubernetes.Interface]

// SetConfigVarsWatchClientForTest installs a fake kubernetes.Interface that
// StartConfigVarsWatch uses INSTEAD of kubernetes.NewForConfig(rc). TEST-ONLY
// — production MUST NOT call it. Returns a restore closure.
func SetConfigVarsWatchClientForTest(cli kubernetes.Interface) func() {
	configVarsWatchClientForTest.Store(&cli)
	return func() {
		var none kubernetes.Interface
		configVarsWatchClientForTest.Store(&none)
	}
}

// ResetConfigVarsWatchForTest clears the started guard. TEST-ONLY.
func ResetConfigVarsWatchForTest() {
	configVarsWatchStarted.Store(false)
	var none kubernetes.Interface
	configVarsWatchClientForTest.Store(&none)
}

// configVarsEnqueuedTotal counts scopeKindBoot enqueues driven by a
// config-vars ConfigMap event (Add or Update). The falsifier reads this to
// prove the informer — not the eager read — drove the re-walk. Distinct from
// the engine's own enqueuedTotal (which also counts the boot + GVR-discovered
// enqueues).
var configVarsEnqueuedTotal atomic.Uint64

// ConfigVarsEnqueuedTotal exposes the config-vars-driven boot re-enqueue
// count (falsifier + telemetry).
func ConfigVarsEnqueuedTotal() uint64 { return configVarsEnqueuedTotal.Load() }

// buildConfigVarsWatchClient builds the typed clientset the informer uses —
// the injected fake in tests, else kubernetes.NewForConfig(rc). Mirrors
// cache.buildSecretsClient.
func buildConfigVarsWatchClient(rc *rest.Config) (kubernetes.Interface, error) {
	if cli := configVarsWatchClientForTest.Load(); cli != nil && *cli != nil {
		return *cli, nil
	}
	return kubernetes.NewForConfig(rc)
}

// matchesConfigVars reports whether an informer event object IS the config-vars
// ConfigMap (name match). The field selector already scopes the LIST/WATCH to
// metadata.name==cmName at the apiserver (production), so this is a cheap
// defense-in-depth guard, NOT walk work — it keeps the trigger correct even if
// the field selector is ever weakened or the informer is backed by a client
// that does not enforce field selectors. O(1) name compare, non-blocking.
func matchesConfigVars(obj interface{}, cmName string) bool {
	cm, ok := obj.(*corev1.ConfigMap)
	if !ok {
		// A DeletedFinalStateUnknown or unexpected type — we do not enqueue on
		// it (no DeleteFunc anyway). Only Add/Update carry *ConfigMap here.
		return false
	}
	return cm.Name == cmName
}

// enqueueBootReDrive is the AddFunc/UpdateFunc body: it ONLY calls the
// engine's non-blocking enqueueScope(scopeKindBoot) (after the O(1) name
// guard). The scopeKindBoot key coalesces (prewarm_engine.go:206), so a burst
// of ConfigMap events collapses to at most one pending boot scope — behavioral
// neutrality when the object is already present at boot (one Add at sync → one
// coalescing enqueue).
//
// HOOK-MUST-NOT-BLOCK: no walk work here — enqueueScope is O(1).
func enqueueBootReDrive(reason string) {
	configVarsEnqueuedTotal.Add(1)
	prewarmEngineSingleton().enqueueScope(prewarmScope{kind: scopeKindBoot})
	slog.Info("prewarm.configvars.boot_redrive_enqueued",
		slog.String("subsystem", "cache"),
		slog.String("reason", reason),
		slog.String("effect", "config-vars ConfigMap event drove a scopeKindBoot re-walk (coalesces on the boot key)"),
	)
}

// StartConfigVarsWatch registers a single-object, namespaced, field-selected
// ConfigMap informer for (authnNS, frontendConfigConfigMapName()). On the
// ConfigMap's appearance (AddFunc) or a config.json change (UpdateFunc) it
// enqueues a scopeKindBoot re-drive on the prewarm engine.
//
// LIFETIME. ctx MUST be the process-lifetime cacheCtx (NOT p1Ctx): the
// informer must stay live so a ConfigMap that lands LONG after the Phase-1
// budget / readiness backstop still drives a self-heal re-walk (§2.2, §2.6).
// The engine worker it enqueues to is also cacheCtx-bound
// (SetEngineProcessContext), so the enqueue always targets a live worker.
//
// Idempotent (configVarsWatchStarted). Soft-fail: a client-build error leaves
// the eager one-shot read as the only trigger — degraded, not fatal (the pod
// still serves cold and the eager read still covers the deps-warm-at-boot
// case).
//
// MUST be called only under cache.PrewarmEnabled() + PrewarmEngineEnabled()
// (main.go enforces) — the enqueue is meaningless without a running engine.
func StartConfigVarsWatch(ctx context.Context, rc *rest.Config, authnNS string) {
	log := slog.Default()
	if authnNS == "" {
		// No namespace to scope the informer to — the eager read has the
		// same dependency, so this mirrors that degrade. Production sets
		// AUTHN_NAMESPACE via the chart.
		log.Info("prewarm.configvars.watch_skipped",
			slog.String("subsystem", "cache"),
			slog.String("reason", "authn namespace empty — no config-vars namespace to watch"),
		)
		return
	}
	if !configVarsWatchStarted.CompareAndSwap(false, true) {
		return // idempotent re-call
	}

	client, err := buildConfigVarsWatchClient(rc)
	if err != nil {
		log.Warn("prewarm.configvars.watch_client_failed",
			slog.String("subsystem", "cache"),
			slog.Any("err", err),
			slog.String("effect", "config-vars watch not wired; eager one-shot read remains the only trigger (degraded, not fatal)"),
		)
		configVarsWatchStarted.Store(false) // allow a later retry
		return
	}

	cmName := frontendConfigConfigMapName()

	// Single-object field selector: metadata.name == cmName. Combined with
	// the namespace scope below this is the smallest possible watch — ONE
	// object in ONE namespace, not a cluster-wide ConfigMap watch. The name
	// is config (frontendConfigConfigMapName), never a Go literal.
	factory := informers.NewSharedInformerFactoryWithOptions(
		client,
		0, // resync period 0 — pure event-driven (matches the dynamic + secrets factories)
		informers.WithNamespace(authnNS),
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.FieldSelector = fields.OneTermEqualSelector("metadata.name", cmName).String()
		}),
	)

	inf := factory.Core().V1().ConfigMaps().Informer()

	if _, err := inf.AddEventHandler(clientcache.ResourceEventHandlerFuncs{
		// AddFunc: config-vars appeared (or the initial-list replay when it
		// is already present at boot). Drives the boot re-walk.
		AddFunc: func(obj interface{}) {
			if matchesConfigVars(obj, cmName) {
				enqueueBootReDrive("configmap_added")
			}
		},
		// UpdateFunc: config.json changed (e.g. the frontend rewrote its
		// INIT/ROUTES_LOADER entry points). Re-drives so the new roots warm.
		UpdateFunc: func(_, newObj interface{}) {
			if matchesConfigVars(newObj, cmName) {
				enqueueBootReDrive("configmap_updated")
			}
		},
		// No DeleteFunc: a deleted config-vars ConfigMap has no roots to
		// re-walk — nothing to enqueue.
	}); err != nil {
		log.Warn("prewarm.configvars.add_event_handler_failed",
			slog.String("subsystem", "cache"),
			slog.Any("err", err),
		)
	}

	// Start the factory bound to ctx (cacheCtx) — process shutdown closes
	// it. Non-blocking; the LIST/WATCH runs on spawned goroutines. We do NOT
	// WaitForCacheSync here: the AddFunc replay on first sync is exactly the
	// signal we want, and blocking boot on it would re-introduce a startup
	// stall (the 0.30.220 lesson) — the whole point is that prewarm start is
	// event-driven, not boot-blocking.
	factory.Start(cacheStopCh(ctx))

	log.Info("prewarm.configvars.watch_started",
		slog.String("subsystem", "cache"),
		slog.String("namespace", authnNS),
		slog.String("configmap", cmName),
		slog.String("lifetime", "process (cacheCtx)"),
		slog.String("selector", "metadata.name single-object"),
	)
}

// cacheStopCh returns a stop channel that closes when ctx is cancelled, for
// SharedInformerFactory.Start (which takes a <-chan struct{}). Local sibling
// of cache.stopCh (unexported there; the dispatchers package cannot reach it
// without a new export, so we keep a tiny local copy).
func cacheStopCh(ctx context.Context) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch
}
