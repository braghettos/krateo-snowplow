package main

import (
	"context"
	"expvar"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/env"
	"github.com/krateoplatformops/plumbing/kubeutil"
	"github.com/krateoplatformops/plumbing/server/use"
	"github.com/krateoplatformops/plumbing/server/use/cors"
	"github.com/krateoplatformops/plumbing/slogs/pretty"
	_ "github.com/krateoplatformops/snowplow/docs"
	"github.com/krateoplatformops/snowplow/internal/cache"
	idynamic "github.com/krateoplatformops/snowplow/internal/dynamic"
	"github.com/krateoplatformops/snowplow/internal/handlers"
	"github.com/krateoplatformops/snowplow/internal/handlers/dispatchers"
	"github.com/krateoplatformops/snowplow/internal/handlers/middleware"
	"github.com/krateoplatformops/snowplow/internal/rbac"
	crdschema "github.com/krateoplatformops/snowplow/internal/resolvers/crds/schema"
	restactionsapi "github.com/krateoplatformops/snowplow/internal/resolvers/restactions/api"
	jqsupport "github.com/krateoplatformops/snowplow/internal/support/jq"
	httpSwagger "github.com/swaggo/http-swagger"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/rest"
)

const (
	serviceName = "snowplow"
)

var (
	build string
)

// @title SnowPlow API
// @version 0.1.0
// @description This the total new Krateo backend.
// @BasePath /
func main() {
	debugOn := flag.Bool("debug", env.Bool("DEBUG", false), "enable or disable debug logs")
	blizzardOn := flag.Bool("blizzard", env.Bool("BLIZZARD", false), "dump verbose output")
	prettyLog := flag.Bool("pretty-log", env.Bool("PRETTY_LOG", true), "print a nice JSON formatted log")
	port := flag.Int("port", env.ServicePort("PORT", 8081), "port to listen on")
	authnNS := flag.String("authn-namespace", env.String("AUTHN_NAMESPACE", ""),
		"krateo authn service clientconfig secrets namespace")
	signKey := flag.String("jwt-sign-key", env.String("JWT_SIGN_KEY", ""), "secret key used to sign JWT tokens")
	jqModPath := flag.String("jq-modules-path", env.String(jqsupport.EnvModulesPath, ""),
		"loads JQ custom modules from the filesystem")
	// Ship Resilience-1 (0.30.162) — comma-separated list of
	// namespaces whose Deployment + Endpoints are watched for the
	// snowplow_upstream_controller_health gauge. Default
	// "krateo-system". Empty string → per-namespace Deployment +
	// Endpoints watches inert; cluster-scoped webhook-config
	// watches still run (so webhook gauge is populated). Discovery
	// of WHICH Deployments to watch is automatic via
	// MutatingWebhookConfiguration / ValidatingWebhookConfiguration
	// introspection — no hardcoded controller-name list
	// (feedback_no_special_cases).
	controllerHealthNS := flag.String("controller-health-namespaces",
		env.String("CONTROLLER_HEALTH_NAMESPACES", "krateo-system"),
		"comma-separated list of namespaces whose controllers are watched for Resilience-1 expvar gauges")

	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), "Flags:")
		flag.PrintDefaults()
	}

	flag.Parse()

	// Ship 0.30.170-debug — enable Go runtime off-CPU profilers at boot
	// so /debug/pprof/mutex + /debug/pprof/block return non-empty data
	// under Chrome MCP cold-nav load. Required for the parallelism
	// regression investigation after Ship 0.30.169 RBAC index proved the
	// 2× ceiling is OFF-CPU (pod CPU utilization 13.43% during burst).
	// fraction=1 / rate=1 = sample every event; overhead is noise at
	// today's utilization. DEBUG BUILD — roll back to 0.30.169 once
	// profiles are captured.
	runtime.SetMutexProfileFraction(1)
	runtime.SetBlockProfileRate(1)

	os.Setenv("DEBUG", strconv.FormatBool(*debugOn))
	os.Setenv("TRACE", strconv.FormatBool(*blizzardOn))
	os.Setenv("AUTHN_NAMESPACE", *authnNS)
	os.Setenv(jqsupport.EnvModulesPath, *jqModPath)

	logLevel := slog.LevelInfo
	if *debugOn {
		logLevel = slog.LevelDebug
	}

	var lh slog.Handler
	if *prettyLog {
		lh = pretty.New(&slog.HandlerOptions{
			Level:     logLevel,
			AddSource: false,
		},
			pretty.WithDestinationWriter(os.Stderr),
			pretty.WithColor(),
			pretty.WithOutputEmptyAttrs(),
		)
	} else {
		lh = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
			Level:     logLevel,
			AddSource: false,
		})
	}

	log := slog.New(lh)
	// 0.30.172: route package-level slog calls (slog.InfoContext / .Info /
	// .DebugContext etc.) through the configured handler. Without this, any
	// emission via slog.* package-level functions goes to Go's uninitialised
	// default handler (text format to stderr) and is silently filtered out
	// by the JSON-only stdout log pipeline. Specifically caught the
	// dispatcher.call.complete log from per_call_log.go (0.30.171-debug)
	// that uses slog.InfoContext(ctx, ...) and never appeared in pod logs.
	slog.SetDefault(log)
	if *debugOn {
		log.Debug("environment variables", slog.Any("env", os.Environ()))
	}

	chain := use.NewChain(
		use.TraceId(),
		use.Logger(log),
	)

	// Ship 0.30.123 (#155) — wire the in-process nested-/call resolver
	// seam. When a RESTAction stage's path is a /call?resource=... loopback
	// into snowplow's own /call endpoint, the api resolver invokes this
	// IN-PROCESS instead of issuing an HTTP request — so a JWT-less /
	// SA-credentialed resolve can complete an exportJwt loopback stage.
	// Wired unconditionally at startup (not cache-gated): the seam itself
	// is cache-agnostic; ResolveNestedCall internally gates its RBAC check
	// on !cache.Disabled(). Mirrors the api.resolveOnceFn seam pattern.
	// RESOLVER_INPROCESS_NESTED_CALL (default true) is the runtime gate;
	// a nil resolver (this wiring skipped) is the structural fallback.
	restactionsapi.RegisterNestedCallResolver(dispatchers.ResolveNestedCall)

	// #57 — retired-flag startup audit. PREWARM_ENABLED and
	// RESOLVER_USE_INFORMER were folded into the single CACHE_ENABLED
	// master gate (prewarm + informer-pivot are now implicit-on-cache).
	// A stale value of either name in the env is functionally ignored
	// (the helpers no longer read them); this emits one audit line per
	// retired flag still present — Warn when set to "false" (a silent
	// behavior change: the operator asked for OFF and now gets ON) and
	// Info otherwise (a harmless no-op). Absent keys emit nothing. Placed
	// before the cache banner so the audit reads alongside the cache-mode
	// determination. See cache.AuditRetiredFlags (retired_flags.go).
	cache.AuditRetiredFlags(log)

	// Cache subsystem — Tag 0.30.4 (cache=on activation).
	//
	// When CACHE_ENABLED is unset / false / 0 / no, cache.Disabled()
	// returns true and cache.NewResourceWatcher returns (nil, nil)
	// without instantiating the dynamicinformer factory. No goroutines
	// spawn. Every consumer (objects.Get, dynamic.ListObjects,
	// rbac.UserCan, EvaluateRBAC) checks cache.Disabled() at the top
	// and falls back to the apiserver / SubjectAccessReview path.
	//
	// When CACHE_ENABLED=true:
	//   - the dynamic informer factory is constructed,
	//   - the four Role-Based Access Control GVRs are eagerly
	//     registered (Role, RoleBinding, ClusterRole, ClusterRoleBinding),
	//   - factory.Start() is invoked inside NewResourceWatcher,
	//   - cache.SetGlobal(rw) publishes the watcher for EvaluateRBAC.
	//
	// We then block (with a timeout) on WaitForCacheSync so the first
	// /call dispatched at startup does not race informer LISTs.
	cacheCtx, cacheCancel := context.WithCancel(context.Background())
	defer cacheCancel()
	// 0.30.207 — honor DEBUG on the background path. cacheCtx is a bare
	// context.Background() derivative; unlike the HTTP request path (which
	// runs use.Logger(log) → xcontext.WithLogger), it carries NO logger.
	// Every background-resolve site (refresher → resolveAndPopulateL1 →
	// resolver, prewarm walk, discovery refresher) calls
	// xcontext.Logger(ctx), which on a logger-less context falls back to a
	// HARDCODED slog.LevelDebug handler (plumbing/context.Logger) that
	// ignores the DEBUG env var entirely — so the hot L1-refresher loop
	// emits full-dict DEBUG lines even with DEBUG=false. Inject the
	// already-level-configured logger (Info when DEBUG=false, Debug when
	// DEBUG=true) so the background path obeys the flag. WithLogger wraps a
	// non-nil root verbatim (preserving its level), so this changes log
	// LEVEL gating only — never log content.
	cacheCtx = xcontext.BuildContext(cacheCtx, xcontext.WithLogger(log))

	var cacheWatcher *cache.ResourceWatcher
	if !cache.Disabled() {
		rc, rcErr := rest.InClusterConfig()
		if rcErr != nil {
			log.Warn("cache: rest.InClusterConfig failed; staying on apiserver branch",
				slog.Any("err", rcErr))
		} else {
			dynCli, dynErr := dynamic.NewForConfig(rc)
			if dynErr != nil {
				log.Warn("cache: dynamic.NewForConfig failed; staying on apiserver branch",
					slog.Any("err", dynErr))
			} else {
				w, wErr := cache.NewResourceWatcher(cacheCtx, dynCli)
				if wErr != nil {
					log.Warn("cache: NewResourceWatcher failed; staying on apiserver branch",
						slog.Any("err", wErr))
				} else {
					cacheWatcher = w
					cache.SetGlobal(w)

					// Ship 0.30.233 — wire the SA *rest.Config as a
					// process singleton so the CRD-ADD discovery
					// side-effect (crd_discovery_side_effect.go) can
					// invoke DiscoverGroupResources without a per-call
					// ctx. The informer processor goroutine has no
					// InternalRESTConfigFromContext attached; the
					// singleton is the bridge between the informer
					// event surface and the discovery API. Mirrors the
					// SetGlobal pattern above — single set at startup,
					// idempotent, soft-fails to walker-only discovery
					// if ever unset.
					cache.SetProcessSARestConfig(rc)

					// Task #322 (#318-R2) Commit 1 — wire the SA-singleton
					// cached-discovery invalidation callback so the
					// CRD-lifecycle bridge (crd_discovery_side_effect.go)
					// can invalidate the dynamic-package discovery
					// singleton AFTER DiscoverGroupResources / teardown
					// without an import cycle (cache is below dynamic).
					// Same set-once-at-startup lifecycle as
					// SetProcessSARestConfig above; soft no-op if unset.
					cache.SetSADiscoveryInvalidator(idynamic.InvalidateSADiscovery)

					// Task #323 (#318-R2 Commit 2-B) — wire the per-GVR
					// compiled-CRD-schema memo reset callback so the SAME
					// CRD-lifecycle bridge resets the schema memo in lockstep
					// with the SA-discovery cache (fired right after
					// invalidateSADiscovery in triggerCRDDiscovery/Delete).
					// Same set-once-at-startup lifecycle; soft no-op if unset.
					// crds/schema is above cache in the import graph, so the
					// trampoline lives in cache and main.go injects the impl.
					cache.SetCRDSchemaInvalidator(crdschema.InvalidateCRDSchemaMemo)

					// Ship 0.30.122 R4 Lever 1: wire the in-cluster
					// *rest.Config so the composition GVR's streaming
					// ListWatch can issue raw paged-LIST HTTP requests and
					// stream the response body item-by-item (the dynamic
					// client only returns a fully-materialised list). Same
					// post-construction wiring pattern as SetMetadataClient.
					// When unset the composition GVR falls back to the
					// standard NewFilteredDynamicInformer.
					w.SetRESTConfig(rc)

					// §0.30.93 (Revision 18): wire the metadata client
					// for the metadata-only informer routing path.
					// Composition GVRs (~50K objects at production
					// scale) take this path to stay within the 1.8 GiB
					// RSS budget; RBAC GVRs and customer CRDs without
					// the `krateo.io/cache-mode: metadata` annotation
					// continue on the dynamic full-informer path.
					//
					// Failure to construct the metadata client leaves
					// rw.metaClient == nil — `EnsureResourceType` then
					// emits `cache.lazy_register.metadata_only_unwired`
					// for every Composition GVR touch (loud SRE signal
					// without crash-looping the pod).
					metaCli, metaErr := metadata.NewForConfig(rc)
					if metaErr != nil {
						log.Warn("cache: metadata.NewForConfig failed; metadata-only routing offline (full-informer fallback)",
							slog.Any("err", metaErr))
					} else {
						w.SetMetadataClient(metaCli)
					}

					// 0.30.98 Tag A: wire the discovery client for the
					// four-conjunct servability gate's conjunct 4
					// (resourceTypeConfirmed — the S4 fix). We use a RAW
					// (uncached) discovery.DiscoveryClient deliberately:
					// the discovery-refresh ticker MUST observe a
					// post-startup CRD's group/version transitioning from
					// un-served to served, and a memcache-backed client
					// would mask that transition until an explicit
					// Invalidate(). The ticker calls
					// ServerResourcesForGroupVersion once per ~30s per
					// registered group/version (deduped) — negligible
					// apiserver load.
					//
					// Failure to construct the discovery client leaves
					// rw.disco == nil; resourceTypeConfirmedLocked then
					// defaults to true (the pivot keeps its pre-0.30.98
					// HasSynced-only behaviour). The S4 fix is degraded
					// but not crash-looping — a loud WARN flags it.
					discoCli, discoErr := discovery.NewDiscoveryClientForConfig(rc)
					if discoErr != nil {
						log.Warn("cache: discovery.NewDiscoveryClientForConfig failed; resource-type confirmation offline (S4 gate degrades to HasSynced-only)",
							slog.Any("err", discoErr))
					} else {
						w.SetDiscoveryClient(discoCli)
						// Launch the ~30s discovery-refresh ticker. It
						// primes confirmation once immediately, then
						// re-confirms every registered GVR's resource
						// type on each tick — flipping post-startup CRDs
						// unconfirmed->confirmed and clearing watchBroken
						// on a successful relist. Bound by cacheCtx +
						// rw.stopCh.
						w.StartDiscoveryRefresher(cacheCtx)
					}

					// §0.30.93 annotation discovery: one apiextensions
					// LIST at startup to find CRDs carrying
					// `krateo.io/cache-mode: metadata`. Bounded by
					// 30 s; soft-fail (annotation set stays empty, the
					// static seed in `internal/cache/cache_mode.go`
					// still routes Composition GVRs to metadata-only).
					discoCtx, discoCancel := context.WithTimeout(cacheCtx, 30*time.Second)
					cache.DiscoverMetadataOnlyAnnotations(discoCtx, rc)
					discoCancel()

					// 0.30.8: wire the L1 resolved-output cache refresher.
					// Order matters: register dispatcher handlers BEFORE
					// StartRefresher so the worker pool sees them on
					// first dequeue, and BEFORE the watcher starts
					// emitting UPDATE events (which it already may be
					// doing — NewResourceWatcher calls factory.Start
					// internally for the RBAC GVRs). Idempotent on
					// duplicate calls.
					//
					// 0.30.113 Part B: pass the in-cluster *rest.Config as
					// the background-refresh SA transport — a refresh has
					// no live per-user token; the widget resolver needs an
					// apiserver client-config on the context.
					dispatchers.RegisterRefreshHandlers(rc)
					// Ship 2 Stage 2.5 / 0.30.248 (Fix v2 engine ctx
					// decoupling). The prewarm engine worker reads this
					// ctx as its long-lived runtime — it stops only on
					// process shutdown, so post-boot
					// scopeKindGVRDiscovered enqueues (from
					// cache.DiscoverGroupResources's `if added` branch)
					// are actually consumed. Pre-Fix-v2 the worker
					// inherited the boot-seed goroutine's bounded ctx
					// and died at boot-done, leaving post-boot enqueues
					// unprocessed (Trace v2 §1.5).
					//
					// MUST be wired BEFORE Phase1Warmup runs (line 594
					// below — Phase1Warmup's engineSeed closure reads
					// engineProcessCtx). Mirrors cache.StartRefresher's
					// cacheCtx-as-process-lifetime pattern (line 320).
					dispatchers.SetEngineProcessContext(cacheCtx)
					// Ship #98 / 0.30.215 — wire the customer-priority
					// yield hook BEFORE StartRefresher so the worker pool
					// sees a populated hook on its first processNext.
					// Mirrors the prewarm engine's customer-inflight signal
					// (prewarm_engine.go:88-105) — one atomic-int64 read per
					// refresher yield-poll tick. Cache-off is a no-op
					// (StartRefresher returns early). The hook is the ONE
					// seam between cache and dispatchers; cache cannot
					// import dispatchers (cycle), so dispatchers injects
					// its predicate via cache.SetCustomerInflightHook.
					cache.SetCustomerInflightHook(dispatchers.CustomerInFlight)
					cache.StartRefresher(cacheCtx)
					// Ship #91 / 0.30.211 — Lever C async invalidator worker.
					// Bounded queue, drop-on-full. Receives raKey enqueues
					// from the deps refresh hook for stuck-false memo
					// invalidation. NOT on the refresher workqueue
					// (feedback_refresher_populate_amplification). Cache-off
					// is a no-op inside the Start fn.
					cache.StartSliceabilityReverifier(cacheCtx)

					// Block until RBAC informer LISTs complete so the
					// first dispatch is not racing the initial sync.
					// Bounded at 60s — soft failure (log + continue),
					// not fatal.
					syncCtx, syncCancel := context.WithTimeout(cacheCtx, 60*time.Second)
					if err := w.WaitForCacheSync(syncCtx, 60*time.Second); err != nil {
						log.Warn("cache: initial WaitForCacheSync incomplete; first dispatches may evaluate against partial RBAC index",
							slog.Any("err", err))
					} else {
						log.Info("cache: RBAC informers fully synced")
					}
					syncCancel()

					// Ship 0.30.189 — sentinel-collision sanity check.
					// `groupOnlyCohortSentinel` ("system:cohort:group-only:v1")
					// is the synthetic identity used to normalise the
					// authenticated-group-only cohort so the PIP seed and
					// the request-time dispatcher hash to the same L1
					// cell. Invariant: CRBsByUser[sentinel] == ∅ and
					// RBsByUserByNS[ns][sentinel] == ∅ for all ns. A real
					// User-kind subject literally named that string would
					// break the invariant (the sentinel would gain real
					// bindings → group-only cohort would leak admin-like
					// access at hash time). Standard k8s admission
					// rejects `system:*` user names; this check guards
					// against misconfigured clusters / test fixtures by
					// failing loud at boot rather than silently at
					// request time.
					if snap := cache.LiveRBACSnapshot(); snap != nil {
						const sentinel = "system:cohort:group-only:v1"
						if n := len(snap.CRBsByUser[sentinel]); n > 0 {
							log.Error("cache: sentinel collision — a ClusterRoleBinding has subject Name="+sentinel,
								slog.Int("crb_count", n),
								slog.String("hint", "rename the offending subject or change groupOnlyCohortSentinel; sentinel must be unique"),
							)
							os.Exit(1)
						}
						for ns, byUser := range snap.RBsByUserByNS {
							if n := len(byUser[sentinel]); n > 0 {
								log.Error("cache: sentinel collision — a RoleBinding has subject Name="+sentinel,
									slog.String("namespace", ns),
									slog.Int("rb_count", n),
									slog.String("hint", "rename the offending subject or change groupOnlyCohortSentinel; sentinel must be unique"),
								)
								os.Exit(1)
							}
						}
					}

					// Ship D.2 (0.30.143) — F-3 cache. Start the
					// AUTHN_NAMESPACE-scoped Secrets informer so the
					// per-user `<user>-clientconfig` lookup the
					// resolver mapper runs (endpoints.go:67) is
					// served from in-process instead of the
					// apiserver. Soft-fail by design — a wiring error
					// leaves the cache inert and the resolver mapper
					// falls back to plumbing's endpoints.FromSecret
					// (the pre-D.2 path).
					//
					// AUTHN_NAMESPACE empty → no-op (production sets
					// it via the chart values; dev/test may leave
					// blank). AssertSecretsInformerWired runs after
					// to surface the misconfiguration.
					if *authnNS != "" {
						if err := cache.StartSecretsInformer(cacheCtx, rc, *authnNS); err != nil {
							log.Warn("cache: StartSecretsInformer failed; F-3 cache offline (upstream fallback)",
								slog.String("authn_ns", *authnNS),
								slog.Any("err", err))
						}
					}
					cache.AssertSecretsInformerWired()

					// Ship Resilience-1 (0.30.162) — upstream-controller
					// health watch surface. Default scope is
					// krateo-system; multi-namespace via comma-
					// separated chart override. Soft-fail by design —
					// wiring errors leave the gauges publishing empty
					// maps and the rest of snowplow runs byte-
					// identical to pre-Resilience-1. CACHE_ENABLED=false
					// has already short-circuited inside Start (no
					// goroutine, no client built) — this whole block
					// only runs when cache is on.
					controllerHealthNamespaces := splitCommaList(*controllerHealthNS)
					if err := cache.StartControllerHealthInformer(cacheCtx, rc, controllerHealthNamespaces); err != nil {
						log.Warn("cache: StartControllerHealthInformer failed; Resilience-1 gauges offline",
							slog.Any("err", err))
					}

					// Tag 0.30.6 binding: walk every RestAction in the
					// cluster, derive the GVR set referenced by
					// spec.api[*].path, and eager-register the lot.
					// Bound by STARTUP_TIMEOUT_SECONDS (default 120);
					// fan-in by STARTUP_INFORMER_FANIN (default 8).
					// Soft failure: log + continue; the lazy fallback
					// in AddResourceType handles the gap.
					//
					// As of 2026-05-13 post-mortem (tag 0.30.61): gated off
					// by default because no consumer reads from the
					// eagerly-registered informers at this tag — the
					// resolver still calls apiserver directly, so eager
					// registration is pure apiserver pressure. Bench at
					// 0.30.6 showed 3× S7/S8 convergence regression + new
					// S6b VERIFY TIMEOUT vs 0.30.5. Re-enable when
					// resolver-cache wiring lands. Set
					// EAGER_REGISTER_ENABLED=true to opt-in (will cause
					// apiserver pressure at 50K scale per bench data;
					// see project_regression_journal.md 2026-05-13).
					//
					// The inventory walker (inventory.go), eager.go, and
					// the watcher.go eagerSet plumbing are intentionally
					// preserved as dormant library code.
					if os.Getenv("EAGER_REGISTER_ENABLED") == "true" {
						log.Info("eager-register: enabled via EAGER_REGISTER_ENABLED=true",
							slog.String("subsystem", "cache"))
						fanin := env.Int("STARTUP_INFORMER_FANIN", 8)
						startupTimeout := time.Duration(env.Int("STARTUP_TIMEOUT_SECONDS", 120)) * time.Second
						invCtx, invCancel := context.WithTimeout(cacheCtx, startupTimeout)
						inv, invErr := cache.CollectResourceTypesFromRestActions(invCtx, dynCli)
						if invErr != nil {
							log.Warn("cache: RestAction inventory walk failed; falling through to lazy registration",
								slog.Any("err", invErr))
							// Mark eager-done with an empty set so any
							// post-startup AddResourceType is treated as
							// expected-lazy (no WARN spam).
							w.MarkEagerSet([]schema.GroupVersionResource{})
						} else {
							n, regErr := cache.EagerRegisterAll(invCtx, w, inv, fanin, startupTimeout)
							if regErr != nil {
								log.Warn("cache: eager registration WaitForCacheSync incomplete",
									slog.Int("resource_types", n),
									slog.Any("err", regErr))
							}
							w.MarkEagerSet(inv)
						}
						invCancel()
					} else {
						// 0.30.9 Sub-scope B (Revision 17): lazy
						// registration of resolver-touched GVRs is
						// the production default for DELETE-evict.
						// The dispatcher hot path calls
						// cache.Global().EnsureResourceType(gvr) on
						// every dep-edge record, so the informer
						// (and the watcher.go UpdateFunc/DeleteFunc
						// handlers) comes online on first touch.
						// EAGER_REGISTER_ENABLED=true remains
						// available as an OPTIONAL warm-start knob
						// for bench/large-customer scenarios that
						// want cold-zero on the first request; the
						// cost is a startup memory burst (OOM'd at
						// 50K bench scale on 0.30.8 — see
						// project_regression_journal.md 2026-05-13).
						log.Info("eager-register: disabled (default); lazy-register-on-resolver-touch provides DELETE-evict",
							slog.String("subsystem", "cache"),
							slog.String("rationale", "0.30.9 Sub-scope B: informers wired on first dep-record via EnsureResourceType; bounded memory at production scale"),
							slog.String("override_hint", "set EAGER_REGISTER_ENABLED=true for warm-start (bench-only; costs startup memory)"),
						)
						// Mark eager-done with an empty set so any
						// post-startup AddResourceType is treated as
						// expected-lazy (no WARN spam from the eagerSet
						// gate in watcher.AddResourceType).
						w.MarkEagerSet([]schema.GroupVersionResource{})

						// 0.30.99 Tag B — startup navigation GVR-walk,
						// gated behind PREWARM_REGISTER_ENABLED (default
						// OFF, mirrors EAGER_REGISTER_ENABLED). Not in the
						// chart configmap — absent ⇒ OFF, so Tag B is
						// chart-change-free and behavior-neutral for
						// production.
						//
						// Why default OFF (architect REJECT of default-on,
						// adjudicated): when the resolver pivot is inactive
						// (historically RESOLVER_USE_INFORMER OFF; post-#57
						// the pivot is implicit-on-cache, so "inactive" ==
						// cache off) the resolver pivot does NOT consume the
						// informers a startup walk would register. A walk
						// would register N informers nobody reads — each
						// EnsureResourceType lands in the post-Start branch
						// and immediately spawns a LIST+WATCH against
						// apiserver. That is the exact "pure apiserver
						// pressure, no consumer" regression the 0.30.6 /
						// 0.30.61 post-mortems reverted (feature journal
						// 0.30.61: "no consumer reads from the eagerly-
						// registered informers ... eager-register = pure
						// apiserver overhead"). The 0.30.8 (rev 104) and
						// 0.30.92 OOM-at-50K modes are also unmitigated:
						// composition GVRs route to the FULL-Unstructured
						// informer because metadataOnlyGVRSeed is empty and
						// customer core-provider CRDs are not annotated
						// krateo.io/cache-mode: metadata.
						//
						// Promotion to ON-by-default requires a
						// PREWARM_REGISTER_ENABLED=true bench at 50K
						// measuring apiserver QPS + RSS-under-load, with the
						// cache subsystem on (CACHE_ENABLED=true) so the
						// pivot consumer is actually present (#57: the pivot
						// is implicit-on-cache; no separate
						// RESOLVER_USE_INFORMER flag to set).
						if os.Getenv("PREWARM_REGISTER_ENABLED") == "true" {
							log.Info("prewarm-register: enabled via PREWARM_REGISTER_ENABLED=true",
								slog.String("subsystem", "cache"),
								slog.String("hint", "startup navigation GVR-walk active; opt-in only — costs apiserver QPS when the resolver pivot is inactive (cache off, #57)"),
							)
							// Soft failure: a LIST error is logged +
							// ignored — the lazy register-on-navigation
							// fallback still covers every GVR on first
							// request. Bound by a fresh timeout so a
							// stalled apiserver cannot wedge boot.
							pwCtx, pwCancel := context.WithTimeout(cacheCtx,
								time.Duration(env.Int("STARTUP_TIMEOUT_SECONDS", 120))*time.Second)
							reg, present, pwErr := w.PrewarmRegisterFromNavigation(pwCtx, dynCli)
							pwCancel()
							if pwErr != nil {
								log.Warn("cache: startup navigation GVR-walk incomplete; lazy register-on-navigation covers the gap",
									slog.Any("err", pwErr))
							} else {
								log.Info("cache: startup navigation GVR-walk done",
									slog.String("subsystem", "cache"),
									slog.Int("registered", reg),
									slog.Int("already_present", present))
							}
						} else {
							log.Info("prewarm-register: disabled (default); set PREWARM_REGISTER_ENABLED=true to opt-in",
								slog.String("subsystem", "cache"),
								slog.String("rationale", "startup walk registers informers the pivot does not consume when the pivot is inactive (cache off, #57) — re-arms the 0.30.61 no-consumer apiserver-QPS regression + the unmitigated 0.30.8/0.30.92 OOM modes"),
							)
						}

						// 0.30.102 Tag B — Phase 1 SA-credentialed
						// resolution walk + CRD-watch + probe-gated
						// readiness. #57: implicit-on-cache —
						// cache.PrewarmEnabled() now folds to
						// !cache.Disabled(), so reaching here (inside the
						// !cache.Disabled() block) it is always true; the
						// standalone PREWARM_ENABLED flag was retired.
						// Distinct from the 0.30.99
						// PREWARM_REGISTER_ENABLED GVR-walk above: Tag B
						// resolves the routesloaders navigation roots
						// under SA identity (discovering GVRs by
						// resolution, not from a configured list) and
						// BLOCKS readiness on every navigated informer
						// reaching HasSynced.
						//
						// Phase1Warmup BLOCKS on the informer sync
						// barrier, so it runs on its own goroutine — the
						// HTTP server (incl. /readyz) must come up while
						// Phase 1 is still warming so the readinessProbe
						// observes 503 during warmup. The goroutine's
						// lifecycle is bounded by both the derived timeout
						// context AND cacheCtx; it terminates when
						// Phase1Warmup returns.
						//
						// When the cache subsystem is OFF, prewarm is
						// implicit-off: this branch is unreachable (the
						// enclosing !cache.Disabled() guard) and the
						// readiness safety-net below calls
						// cache.MarkPhase1Done() immediately — /readyz
						// then returns 200 from the first probe (no-op
						// gate; transparent cache-off fallback).
						if cache.PrewarmEnabled() {
							log.Info("prewarm: Phase 1 startup warmup enabled (implicit-on-cache, #57)",
								slog.String("subsystem", "cache"),
								slog.String("hint", "SA-credentialed routesloaders resolution walk + CRD-watch; /readyz gates on Phase1Done"),
							)
							// PHASE1_TIMEOUT_SECONDS bounds the whole walk +
							// sync barrier. Default 900s — aligned with the
							// chart's startupProbe budget (failureThreshold
							// 90 * periodSeconds 10). A separate knob from
							// STARTUP_TIMEOUT_SECONDS (120s) because Phase 1
							// resolves the full navigation surface and may
							// legitimately take minutes at production scale.
							phase1Timeout := time.Duration(env.Int("PHASE1_TIMEOUT_SECONDS", 900)) * time.Second
							go func() {
								p1Ctx, p1Cancel := context.WithTimeout(cacheCtx, phase1Timeout)
								defer p1Cancel()
								if err := dispatchers.Phase1Warmup(p1Ctx, rc, *authnNS); err != nil {
									log.Warn("cache: Phase 1 startup warmup incomplete",
										slog.String("subsystem", "cache"),
										slog.Any("err", err))
								}
								// Phase1Warmup always calls
								// cache.MarkPhase1Done internally before it
								// returns — /readyz is now 200.
							}()
						} else {
							// #57: PrewarmEnabled() is implicit-on-cache, so
							// inside this !cache.Disabled() block it is always
							// true — this else is defensively retained (the
							// gate symbol is preserved) but unreachable under
							// cache-on. The cache-off no-op path lives in the
							// readiness safety-net below.
							log.Info("prewarm: Phase 1 startup warmup not scheduled",
								slog.String("subsystem", "cache"),
								slog.String("rationale", "prewarm is implicit-on-cache (#57); the cache-off no-op flips Phase1Done via the readiness safety-net"),
							)
							// Nothing to warm — flip the readiness gate now
							// so /readyz is an immediate-200 no-op.
							cache.MarkPhase1Done()
						}
					}
				}
			}
		}
	} else {
		// 0.30.71 — "true cache-off" diagnostic mode.
		//
		// CACHE_ENABLED=false unconditionally disables the L1
		// resolved-output cache, the typed-RBAC indexer, and the
		// informer factory. We still construct a passthrough
		// ResourceWatcher (when in-cluster config is available) so
		// the watcher API stays callable; every Get/List call routes
		// to apiserver via the dynamic client. NewResourceWatcher
		// emits the loud WARN diagnostic-mode banner so operators
		// see immediately that ALL caching is off.
		//
		// When in-cluster config or dynamic.NewForConfig fails (e.g.
		// running outside a cluster for unit tests), we fall back to
		// the pre-0.30.71 nil-watcher shape — consumers nil-check
		// cache.Global() and take their own apiserver branch.
		rc, rcErr := rest.InClusterConfig()
		if rcErr != nil {
			log.Info("cache: rest.InClusterConfig unavailable in disabled mode; watcher will be nil",
				slog.Any("err", rcErr))
			_, _ = cache.NewResourceWatcher(cacheCtx, nil)
		} else {
			dynCli, dynErr := dynamic.NewForConfig(rc)
			if dynErr != nil {
				log.Warn("cache: dynamic.NewForConfig failed in disabled mode; watcher will be nil",
					slog.Any("err", dynErr))
				_, _ = cache.NewResourceWatcher(cacheCtx, nil)
			} else {
				w, wErr := cache.NewResourceWatcher(cacheCtx, dynCli)
				if wErr != nil {
					log.Warn("cache: NewResourceWatcher failed in disabled mode; watcher will be nil",
						slog.Any("err", wErr))
				} else if w != nil {
					cacheWatcher = w
					cache.SetGlobal(w)
				}
			}
		}
	}
	defer func() {
		if cacheWatcher != nil {
			cacheWatcher.Stop()
			cache.SetGlobal(nil)
		}
	}()

	// 0.30.102 Tag B — readiness-gate safety net. The Tag B block above
	// flips the Phase1Done gate (immediately when prewarm is off, or
	// asynchronously at the tail of Phase1Warmup when on). #57: prewarm is
	// implicit-on-cache, so "prewarm off" == cache off. Several startup
	// paths bypass that block entirely: CACHE_ENABLED=false (diagnostic
	// passthrough) or a cache-setup failure (nil watcher). On any such
	// path there is nothing to warm, so /readyz must still return 200.
	// When the cache subsystem is ON AND a watcher exists, the Phase1Warmup
	// goroutine owns the flip — do NOT pre-flip here, or the
	// premature-Ready invariant breaks.
	//
	// 0.30.153 — the three-disjunct invariant is encoded in
	// cache.ShouldFlipPhase1DoneOnStartup so the cache-off case
	// (CACHE_ENABLED=false + non-nil passthrough watcher) is covered. The
	// prior 2-disjunct condition missed that case; pod was stuck
	// `{"status":"warming","phase1Done":false}` forever, Service endpoints
	// empty, snowplow LB unroutable. #57 preserves the 3-arg signature
	// (cache.PrewarmEnabled() folds to !Disabled(), so the middle disjunct
	// now equals the first) — the named helper is the regression's encoded
	// falsifier.
	if cache.ShouldFlipPhase1DoneOnStartup(
		!cache.Disabled(),
		cache.PrewarmEnabled(),
		cacheWatcher == nil,
	) {
		cache.MarkPhase1Done()
	}

	mux := http.NewServeMux()

	mux.Handle("GET /swagger/", httpSwagger.WrapHandler)
	//mux.Handle("POST /convert", chain.Then(handlers.Converter()))

	mux.Handle("GET /health", handlers.HealthCheck(serviceName, build, kubeutil.ServiceAccountNamespace))
	mux.Handle("GET /readyz", handlers.ReadyCheck())
	// Ship D (0.30.141, architectural-consistency invariant) —
	// /api-info/names is `/call`-class read; scope as `plurals` so the
	// invariant counter classifies any apiserver fall-through from this
	// route. cache.FallthroughScopeMiddleware is a no-op when
	// CACHE_ENABLED=false (project_caching_is_provisional).
	mux.Handle("GET /api-info/names", chain.Append(
		cache.FallthroughScopeMiddleware(cache.ScopePlurals)).
		Then(handlers.Plurals()))
	cache.RegisterScopedRoute("GET /api-info/names", cache.ScopePlurals)

	mux.Handle("GET /list", chain.Append(
		middleware.UserConfig(*signKey, *authnNS),
		cache.FallthroughScopeMiddleware(cache.ScopeList)).
		Then(handlers.List()))
	cache.RegisterScopedRoute("GET /list", cache.ScopeList)

	// Ship D — GET /call is the canonical `/call` read path. Dispatcher
	// routes into the per-group restactions / widgets handlers; for the
	// scope label we use call-generic (the dispatcher's fallthrough lane
	// is the F-1 site). The restactions / widgets dispatcher entries
	// inherit the scope via the same ctx.
	mux.Handle("GET /call", chain.Append(
		middleware.UserConfig(*signKey, *authnNS),
		cache.FallthroughScopeMiddleware(cache.ScopeCallGeneric),
		handlers.Dispatcher(dispatchers.All())).
		Then(handlers.Call()))
	cache.RegisterScopedRoute("GET /call", cache.ScopeCallGeneric)

	// Ship D — write-verb `/call` routes also get the middleware (PM
	// explicit). Write verbs are out of the read-path invariant (F-11
	// in the design), but centralizing classification prevents silent
	// escapes: a future GET-class route mistakenly registered under
	// `call-write-*` would still trip the counter on the wrong cell.
	mux.Handle("POST /call", chain.Append(
		middleware.UserConfig(*signKey, *authnNS),
		cache.FallthroughScopeMiddleware(cache.ScopeCallWritePost)).
		Then(handlers.Call()))
	cache.RegisterScopedRoute("POST /call", cache.ScopeCallWritePost)

	mux.Handle("PUT /call", chain.Append(
		middleware.UserConfig(*signKey, *authnNS),
		cache.FallthroughScopeMiddleware(cache.ScopeCallWritePut)).
		Then(handlers.Call()))
	cache.RegisterScopedRoute("PUT /call", cache.ScopeCallWritePut)

	mux.Handle("PATCH /call", chain.Append(
		middleware.UserConfig(*signKey, *authnNS),
		cache.FallthroughScopeMiddleware(cache.ScopeCallWritePatch)).
		Then(handlers.Call()))
	cache.RegisterScopedRoute("PATCH /call", cache.ScopeCallWritePatch)

	mux.Handle("DELETE /call", chain.Append(
		middleware.UserConfig(*signKey, *authnNS),
		cache.FallthroughScopeMiddleware(cache.ScopeCallWriteDelete)).
		Then(handlers.Call()))
	cache.RegisterScopedRoute("DELETE /call", cache.ScopeCallWriteDelete)

	mux.Handle("POST /jq", chain.Append(middleware.UserConfig(*signKey, *authnNS)).Then(handlers.JQ()))

	// /debug/pprof/* — registered on the custom mux (server does NOT use http.DefaultServeMux).
	// Exposes goroutine, heap, profile, allocs, mutex, block, cmdline, symbol, threadcreate, trace.
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)

	// Ship D.1 (0.30.142) — mount expvar.Handler on the snowplow mux so
	// Ship D's counters (snowplow_apiserver_fallthrough_total,
	// snowplow_apiserver_fallthrough_cells,
	// snowplow_assertion_violations_total) are reachable over HTTP.
	// Without this mount the counters published in
	// internal/cache/fallthrough_meter_expvar.go are visible only via
	// the process expvar registry — not on the production pod's HTTP
	// surface — which is why Ship D's tester gate appeared to fail
	// observability (the architect's audit confirmed the wrappers WERE
	// firing; the counter simply wasn't reachable). One-liner alongside
	// the existing pprof registrations.
	//
	// Ship 0.30.249 / Task #250 Block 2a — register
	// snowplow_rbac_publish_seq (RBAC snapshot publish-sequence counter)
	// here BEFORE the mux mount accepts scrapes. Unconditionally wired
	// (cache mode-agnostic) so the Phase 6 #250 bench S8/S9 inner gate
	// can poll the value under both CACHE_ENABLED=true and =false;
	// under cache-off the value remains 0 and the bench's
	// _wait_rbac_propagation_to_snowplow times out as expected.
	cache.RegisterRBACSnapshotExpvar()
	// Ship L2 (0.30.253) / Task #291 — register the snapshot authz memo
	// counters (hits/misses/swaps/refused/entries) next to the publish-seq
	// expvar, BEFORE the mux mount accepts scrapes, so the F1/F5 falsifiers
	// can read hit-rate + entry count over /debug/vars. Cache mode-agnostic
	// registration; the memo itself is only populated on the cache=on path.
	rbac.RegisterAuthzMemoExpvar()
	mux.Handle("GET /debug/vars", expvar.Handler())

	ctx, stop := signal.NotifyContext(context.Background(), []os.Signal{
		os.Interrupt,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGKILL,
		syscall.SIGHUP,
		syscall.SIGQUIT,
	}...)
	defer stop()

	server := &http.Server{
		Addr: fmt.Sprintf(":%d", *port),
		Handler: use.CORS(cors.Options{
			AllowedOrigins: []string{"*"},
			AllowedMethods: []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
			AllowedHeaders: []string{
				"Accept",
				"Authorization",
				"Content-Type",
				"X-Auth-Code",
				"X-Krateo-TraceId",
			},
			ExposedHeaders:   []string{"Link"},
			AllowCredentials: true,
			MaxAge:           300, // Maximum value not ignored by any of major browsers
		})(mux),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 50 * time.Second,
		IdleTimeout:  30 * time.Second,
	}

	// Ship D (0.30.141) — architectural-consistency invariant boot
	// assert. Verifies every /call-class route is wrapped with
	// FallthroughScopeMiddleware. Test mode panics on missing routes;
	// prod logs ERROR + bumps snowplow_assertion_violations_total. The
	// invariant has no kill-switch in prod — a missing route degrades
	// invariant visibility, not service correctness.
	cache.AssertReadPathsScoped()

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("server cannot run",
				slog.String("addr", server.Addr),
				slog.Any("err", err))
		}
	}()

	// Listen for the interrupt signal.
	log.Info("server is ready to handle requests", slog.String("addr", server.Addr))
	<-ctx.Done()

	// Restore default behavior on the interrupt signal and notify user of shutdown.
	stop()
	log.Info("server is shutting down gracefully, press Ctrl+C again to force")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	server.SetKeepAlivesEnabled(false)
	if err := server.Shutdown(ctx); err != nil {
		log.Error("server forced to shutdown", slog.Any("err", err))
	}

	log.Info("server gracefully stopped")
}

// splitCommaList parses a comma-separated env var into a string
// slice, dropping empty entries and trimming whitespace. Used by
// the Ship Resilience-1 wiring (0.30.162) to parse
// CONTROLLER_HEALTH_NAMESPACES into the watch-scope slice. Empty
// input → empty slice (subsystem-defined behavior: the per-
// namespace Deployment + Endpoints watches stay inert; cluster-
// scoped webhook watches still run).
func splitCommaList(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}
