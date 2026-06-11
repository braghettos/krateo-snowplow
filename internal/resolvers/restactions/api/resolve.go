package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"sync"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/env"
	httpcall "github.com/krateoplatformops/plumbing/http/request"
	"github.com/krateoplatformops/plumbing/http/response"
	"github.com/krateoplatformops/plumbing/maps"
	"github.com/krateoplatformops/plumbing/ptr"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/dynamic"
	"github.com/krateoplatformops/snowplow/internal/objects"
	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

// iterParallelism returns the per-stage inner-call fan-out width.
//
// Default is GOMAXPROCS(0); env override RESOLVER_ITER_PARALLELISM
// (positive integer) takes precedence when set. The value is clamped
// to [1, 32] — the upper bound caps tail-latency damage from a
// pathological stage (e.g. 5000 inner calls × 100ms apiserver latency
// at unbounded width would saturate the apiserver, the snowplow pod's
// HTTP client pool, AND the Go scheduler simultaneously).
//
// Per architect's design 0.30.95: bound is hard-capped at the resolver
// boundary, NOT per-resource — no per-GVR carve-outs (feedback_no_special_cases.md).
//
// Ship F2 (0.30.125): a resolve under cache.WithPrewarmIterSerial — the
// SA content-population pass — gets parallelism 1. The content pass
// uncaps the iterator (full per-namespace fan-out, the #159 OOM
// territory) but runs behind the 503 readiness gate with no latency
// budget, so it forces the fan-out serial to hold peak RSS down. This
// is CONTEXT-SCOPED: every real /call carries no marker and keeps the
// GOMAXPROCS/env width.
func iterParallelism(ctx context.Context) int {
	if cache.PrewarmIterSerialFromContext(ctx) {
		return 1
	}
	n := runtime.GOMAXPROCS(0)
	if s := os.Getenv("RESOLVER_ITER_PARALLELISM"); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v > 0 {
			n = v
		}
	}
	if n > 32 {
		n = 32
	}
	if n < 1 {
		n = 1
	}
	return n
}

// lazyRegisterSlowThreshold is the ceiling above which EnsureResourceType
// emits a WARN log line. The call itself is just `rw.mu.Lock()` plus a
// `factory.ForResource` + goroutine spawn — sub-millisecond expected.
// Anything above this threshold means rw.mu is heavily contended or
// factory.ForResource is doing real work (rare client-go discovery
// path). The threshold is intentionally aggressive so the falsifier
// fires before 8s-class first-read symptoms accumulate.
const lazyRegisterSlowThreshold = 250 * time.Millisecond

const (
	//annotationKeyVerboseAPI = "krateo.io/verbose"
	headerAcceptJSON = "Accept: application/json"

	// envVerboseWireDump is the Ship 0.30.121 R1-b operator kill-switch
	// for httpcall's DumpResponse verbose wire-dump. When false (the
	// default) endpoint.Debug is NEVER set regardless of the per-RESTAction
	// krateo.io/verbose annotation — so a stray annotation on a heavy
	// RESTAction cannot OOM the pod. Only when this is explicitly "true"
	// does the per-call verbose decision (R1-a) get a chance to flip Debug.
	envVerboseWireDump = "RESOLVER_VERBOSE_WIRE_DUMP"
)

// verboseWireDumpEnabled reports whether the R1-b operator kill-switch
// permits the httpcall verbose wire-dump at all. Default false.
func verboseWireDumpEnabled() bool {
	return env.Bool(envVerboseWireDump, false)
}

// callWantsWireDump implements the Ship 0.30.121 R1-a per-call verbose
// decision. The verbose wire-dump (httpcall DumpResponse) stringifies the
// entire HTTP response body — for a K8s collection LIST that is the
// multi-MB compositions envelope, the dominant transient-memory cost. A
// K8s collection LIST is identified by ParseAPIServerPathToDep yielding a
// parseable apiserver path with an EMPTY object name. For every other
// call shape (a GET-by-name, an external endpoint, a nested /call) the
// body is small and the wire-dump is cheap — verbose stays available
// there for debugging. The decision is keyed on the parsed call shape
// ONLY — no resource/name literal (feedback_no_special_cases).
func callWantsWireDump(verbose bool, callPath string) bool {
	if !verbose {
		return false
	}
	if gvr, _, name, ok := cache.ParseAPIServerPathToDep(callPath); ok && name == "" {
		// A K8s collection LIST — suppress the wire-dump (this is the
		// ~1.94 GiB alloc_space line). gvr is parsed only to confirm the
		// apiserver-path shape; its value is not otherwise needed.
		_ = gvr
		return false
	}
	return true
}

type ResolveOptions struct {
	RC      *rest.Config
	AuthnNS string
	Verbose bool
	Items   []*templates.API
	PerPage int
	Page    int
	Extras  map[string]any

	// Watcher is the cluster-wide informer cache. When nil (the
	// default at 0.30.1, since CACHE_ENABLED defaults to false),
	// every API call takes the apiserver branch via httpcall.Do.
	// Routing for K8s-served endpoints flips on at 0.30.2.
	Watcher *cache.ResourceWatcher

	// RESTActionNamespace / RESTActionName identify the owning
	// RESTAction CR (Ship E, 0.30.116). They are folded into the
	// per-api-stage L1 key so a stage id is scoped to its RESTAction.
	// restactions.Resolve threads them from the RESTAction's
	// ObjectMeta. Empty when the caller did not supply them — the
	// api-stage key-swap then no-ops for that resolve (cache miss is
	// always safe), so a caller that forgets to thread them degrades
	// to the 0.30.115 path rather than mis-keying.
	RESTActionNamespace string
	RESTActionName      string
}

// accumulateErrorKey implements Ship 0.30.257 (#313) Option W-A: the shared
// dict[errorKey] is an ACCUMULATING slice rather than last-wins, so an
// iterator stage with K failing items surfaces ALL K errors (not just the
// last). All iterator items of a stage share ONE errorKey (setup.go:59), so
// the pre-0.30.257 `dict[errorKey] = value` clobbered earlier item errors —
// the long-standing ContinueOnError=true last-wins bug (trace §1.2 / §2.3).
//
//   - absent          → []any{value}
//   - already []any   → append(existing, value)
//   - a non-slice     → []any{existing, value}  (defensive promotion: a prior
//     scalar from any non-iterator path is preserved, never
//     dropped)
//
// CONCURRENCY: `dict` is the shared serve map written by parallel iterator
// workers; the caller MUST hold dictMu across this call (the three worker
// error branches do — resolve.go error sites). The append is NOT internally
// locked — keeping the lock at the call site mirrors the existing
// dict[ErrorKey]-write pattern and keeps the whole error-record + (in the
// httpcall branch) the asMap decision inside one critical section. Exercised
// concurrently by TestResolve_IteratorErrorCollection_Race under -race.
func accumulateErrorKey(dict map[string]any, key string, value any) {
	existing, present := dict[key]
	if !present {
		dict[key] = []any{value}
		return
	}
	if slice, ok := existing.([]any); ok {
		dict[key] = append(slice, value)
		return
	}
	// A present non-slice value (scalar/map from any path) — promote to a
	// slice so neither the prior value nor the new one is silently dropped.
	dict[key] = []any{existing, value}
}

func Resolve(ctx context.Context, opts ResolveOptions) map[string]any {
	if len(opts.Items) == 0 {
		return map[string]any{}
	}

	if opts.RC == nil {
		var err error
		opts.RC, err = rest.InClusterConfig()
		if err != nil {
			return map[string]any{}
		}
	}

	log := xcontext.Logger(ctx)
	log.Info("pagination options", slog.Int("page", opts.Page), slog.Int("perPage", opts.PerPage))

	// Cache routing gate. At 0.30.1 cache.Disabled() defaults to true
	// and Watcher is nil — every API call takes the apiserver branch.
	// The 0.30.2 ship lands the cache-served branch keyed off Watcher.
	if cache.Disabled() || opts.Watcher == nil {
		log.Debug("api.Resolve: cache disabled or watcher unset; using apiserver branch",
			slog.Bool("cache_disabled", cache.Disabled()),
			slog.Bool("watcher_nil", opts.Watcher == nil))
	}

	user, err := xcontext.UserInfo(ctx)
	if err != nil {
		log.Error("unable to fetch user info from context", slog.Any("err", err))
		return map[string]any{}
	}

	// Sort API by Depends
	names, err := topologicalSort(opts.Items)
	if err != nil {
		log.Error("unable to sorted api by deps", slog.Any("error", err))
		return map[string]any{}
	}
	log.Debug("sorted api by deps", slog.Any("names", names))

	apiMap := make(map[string]*templates.API, len(opts.Items))
	for _, id := range names {
		for _, el := range opts.Items {
			if el.Name == id {
				apiMap[id] = el
				break
			}
		}
	}
	log.Debug("created api map", slog.Int("total", len(apiMap)))

	// Endpoints reference mapper
	mapper := endpointReferenceMapper{
		authnNS:  opts.AuthnNS,
		username: user.Username,
		rc:       opts.RC,
	}

	dict := map[string]any{}
	if opts.Extras != nil {
		dict = maps.DeepCopyJSON(opts.Extras)
	}

	if opts.PerPage > 0 && opts.Page > 0 {
		dict["slice"] = map[string]any{
			"page":    opts.Page,
			"perPage": opts.PerPage,
			"offset":  (opts.Page - 1) * opts.PerPage,
		}
	}

	log.Info("base dict for api resolver", slog.Any("dict", dict))

	// Ship F1 (0.30.119): the content-keyed api-stage L1 is active only
	// when RESOLVED_CACHE_APISTAGE_ENABLED=true (default off). Read the
	// gate + the store handle ONCE before the loop; flag-off both stay
	// inert and every call runs the byte-identical 0.30.118 path.
	//
	// Unlike Ship E's per-stage, per-user key, the F1 content layer keys
	// each K8s CALL by its (gvr, namespace, name-or-empty) — identity-
	// free, shared. No owning-RESTAction scoping is needed (the call
	// tuple fully identifies the content unit), so RESTActionName is no
	// longer consulted.
	apistageEnabled := cache.ApistageL1Enabled()
	var apistageStore *cache.ResolvedCacheStore
	if apistageEnabled {
		apistageStore = cache.ResolvedCache()
		if apistageStore == nil {
			apistageEnabled = false
		}
	}

	// Ship 0.30.120 layer (b) — error-aware Put-gate sink. The background
	// refresher installs an *atomic.Int64 on ctx via WithStageErrorSink;
	// each dict[call.ErrorKey] write below bumps it so resolveAndPopulateL1
	// can decline to overwrite a good L1 entry with a result produced
	// under a swallowed (continueOnError'd) stage error. On the normal
	// request path no sink is installed — stageErrSink is nil, the bump
	// sites are no-ops, and this resolve is byte-identical to 0.30.119.
	stageErrSink := cache.StageErrorSinkFromContext(ctx)

	// Ship 0.30.192 — pure-additive per-stage timing sink. Installed by
	// phase1_pip_seed.go's seedOneRestaction so cost-attribution data
	// flows into a structured slog line. Production /call requests
	// install no sink; pipTimingSink is nil and every recorder branch
	// below is a nil-receiver no-op (no behavioural change).
	pipTimingSink := cache.PIPStageTimingSinkFrom(ctx)

	for _, id := range names {
		// Get the api with this identifier
		apiCall, ok := apiMap[id]
		if !ok {
			log.Warn("api not found in apiMap", slog.Any("name", id))
			continue
		}
		if apiCall.Headers == nil {
			apiCall.Headers = []string{headerAcceptJSON}
		}

		// Ship 0.30.192 — per-stage timing recorder. stageTiming is a
		// stack-local value; recordStageTiming closes over it and the
		// outer pipTimingSink. On a nil sink (production /call path)
		// recordStageTiming is still called (one nil-receiver no-op);
		// no behavioural change. Stage early-exits (continue / return
		// dict) MUST call recordStageTiming() first so the failed
		// stage's wall-clock is captured for cost attribution.
		//
		// Ship 0.30.193 — register the stage with the sink so workers'
		// AccumulateContentServe / AccumulateMemoPopulate /
		// AccumulateDefensive calls (called from concurrent goroutines
		// inside the iterator errgroup) can accumulate into THIS
		// stage's struct. BeginStage takes a *PIPStageTiming pointer;
		// recordStageTiming calls EndStage which COMMITS the in-flight
		// stage to the sink's stages slice. A nil sink makes both
		// BeginStage and EndStage no-ops.
		stageStart := time.Now()
		stageTiming := cache.PIPStageTiming{StageID: id}
		pipTimingSink.BeginStage(&stageTiming)
		recordStageTiming := func() {
			stageTiming.ElapsedMs = time.Since(stageStart).Milliseconds()
			if pipTimingSink != nil {
				// Ship 0.30.193 — sink-aware finalisation. EndStage
				// commits the in-flight stage; Append is no longer
				// invoked because the stage is already tracked under
				// the sink's current pointer. The previously-set
				// ElapsedMs / IteratorElapsedMs / ClusterListUsed /
				// ClusterListDenyGate fields are captured by the
				// EndStage copy.
				pipTimingSink.EndStage()
			}
		}

		// Ship 0.30.257 (#313) — genuine-cancellation guard (design
		// §2.1.1 Option C-A). The iterator errgroup no longer cancels
		// gctx on a per-ITEM hard error (workers record into itemErrs and
		// return nil — see the worker branches + post-g.Wait() join
		// below), so the ONLY thing that aborts the resolve early is a
		// GENUINE caller cancellation (client disconnect / deadline)
		// propagating through the request ctx. We removed the WORKER as a
		// cancellation SOURCE, not the parent-ctx machinery: a cancelled
		// caller still cancels gctx (errgroup.WithContext derives gctx from
		// ctx) so each in-flight dispatch fails fast, and THIS guard at the
		// stage-loop top makes the cancellation abort downstream STAGES
		// promptly instead of resolving them against a dead ctx. Cheap (one
		// atomic load) and inert on the happy path.
		if err := ctx.Err(); err != nil {
			log.Debug("api.Resolve: caller context cancelled; aborting remaining stages",
				slog.String("name", id), slog.Any("err", err))
			recordStageTiming()
			return dict
		}

		// Tag 0.30.9 Sub-scope A: detect userAccessFilter.
		// When set, the dispatch uses snowplow's ServiceAccount
		// endpoint (cluster-wide read) — NOT the per-user
		// clientconfig — and the response is in-process-refiltered
		// per object through EvaluateRBAC. When unset, the dispatch
		// path is unchanged from 0.30.8 (per-user-token via the
		// endpointReferenceMapper). Per Revision 5 (binding): atomic
		// ship — no gate flag. Portal RestActions opt in by adding
		// the userAccessFilter stanza; the resolver branches
		// per-stage.
		uafActive := apiCall.UserAccessFilter != nil

		// User-bearer-token append: only for non-UAF stages. When
		// UAF is active the SA endpoint carries the SA token (no
		// user-bearer override needed); appending the user token
		// here would route the call through the user's credentials
		// instead of the SA's — breaking the entire UAF mechanism.
		if !uafActive {
			if accessToken, _ := xcontext.AccessToken(ctx); accessToken != "" {
				if apiCall.EndpointRef == nil || ptr.Deref(apiCall.ExportJWT, false) {
					// 0.30.164: stage-local Headers — never write the user
					// bearer back into the shared CR slice (the CR is marshaled
					// into the /call response body at restactions.go:149; an
					// in-place append leaked the JWT to the wire — see
					// /tmp/snowplow-runs/ship-307/before/.../call-namespaces.json).
					stageHeaders := make([]string, len(apiCall.Headers), len(apiCall.Headers)+1)
					copy(stageHeaders, apiCall.Headers)
					stageHeaders = append(stageHeaders,
						fmt.Sprintf("Authorization: Bearer %s", accessToken))
					local := *apiCall
					local.Headers = stageHeaders
					apiCall = &local
				}
			}
		}

		// Resolve the endpoint. UAF stages use the snowplow-SA
		// endpoint; non-UAF stages go through the per-user
		// clientconfig (or the named EndpointRef) as before.
		var ep endpoints.Endpoint
		if uafActive {
			saEP, saErr := dynamic.ServiceAccountEndpoint()
			if saErr != nil {
				log.Error("userAccessFilter: cannot acquire ServiceAccount endpoint; falling through to per-user dispatch (degraded mode)",
					slog.String("name", id), slog.Any("err", saErr))
				// Fail-closed-but-respond: per Revision 5 atomic
				// ship there is no toggle to fall back to the
				// per-user path correctly (we'd leak the user
				// bearer token to a SA-marked stage). Returning
				// an empty result for this stage and continuing.
				dict[id] = map[string]any{"items": []any{}}
				recordStageTiming()
				continue
			}
			ep = *saEP
		} else {
			resolved, err := mapper.resolveOne(ctx, apiCall.EndpointRef)
			if err != nil {
				log.Error("unable to resolve api endpoint reference",
					slog.String("name", id), slog.Any("ref", apiCall.EndpointRef), slog.Any("error", err))
				recordStageTiming()
				return dict
			}
			ep = resolved
		}
		// Ship 0.30.121 R1 — the verbose wire-dump (httpcall's DumpResponse)
		// is the single largest transient-memory consumer (~1.94 GiB
		// alloc_space on the 50K bench: it stringifies every HTTP response
		// body, including the multi-MB compositions LIST). The blanket
		// `ep.Debug = opts.Verbose` set here is REMOVED — Debug is now
		// decided PER-CALL inside the g.Go worker (R1-a: never for a K8s
		// collection LIST) and additionally gated on RESOLVER_VERBOSE_WIRE_DUMP
		// (R1-b: an operator kill-switch, default off). See the worker below.
		log.Debug("resolved endpoint for api call",
			slog.String("name", id), slog.String("host", ep.ServerURL),
			slog.Bool("uaf", uafActive))

		tmp := createRequestOptions(ctx, log, apiCall, dict)
		if len(tmp) == 0 {
			log.Warn("empty request options for http call", slog.Any("name", id))
			recordStageTiming()
			continue
		}

		// Ship D.5 (0.30.152) — cluster-list-when-allowed iterator
		// collapse. When the RA opts in AND the requester holds
		// cluster-scope `list` on the target GVR AND
		// cache + Ship B snapshot are ready, attemptClusterListCollapse
		// pre-dispatches a SINGLE cluster-scope LIST, validates its
		// shape (AC-D5.14), Puts the envelope under the identity-free
		// apistage key, and returns a 1-element tmp slice. The existing
		// worker loop then runs apistageContentServe which Get-hits the
		// cache + applies the per-user gateContentEnvelope narrowing —
		// no double-dispatch, no special-case in the worker.
		//
		// Default-off + RBAC-deny + shape-fail all yield
		// useClusterList=false; tmp keeps its iterator fan-out and the
		// path is byte-identical to pre-D.5 (AC-D5.6).
		//
		// Ship 0.30.192 — capture cluster_list outcome + deny gate for
		// per-stage timing instrumentation. The third return value is
		// the deny-gate number (0 = no deny, 2-7 = which gate); see
		// cache/pip_stage_timing.go PIPStageTiming.ClusterListDenyGate
		// for the value table.
		//
		// Ship S.1 — the per-RA opt-in field (ClusterListWhenAllowed) was
		// removed. attemptClusterListCollapse is now reached for every
		// iterator stage (its own gates 2-5 decide servability), so
		// ClusterListAttempted is true whenever the resolver evaluates the
		// gate. The old `ptr.Deref(apiCall.ClusterListWhenAllowed, false)`
		// gate-attempt signal is gone with the field.
		stageTiming.ClusterListAttempted = true
		if newTmp, useClusterList, denyGate := attemptClusterListCollapse(
			ctx, log, apiCall, dict, ep, apistageStore, apistageEnabled); useClusterList {
			tmp = newTmp
			stageTiming.ClusterListUsed = true
			stageTiming.ClusterListDenyGate = denyGate // 0 on success
		} else {
			stageTiming.ClusterListUsed = false
			stageTiming.ClusterListDenyGate = denyGate
		}

		// 0.30.92 widening: lazy-register the informer for every
		// downstream apiserver GVR enumerated by this stage's request
		// options. Without this, the 0.30.91 hook only fired for the
		// entry-point RESTAction GVR (recorded in restactions.go's
		// dispatcher) — downstream GVRs the resolver dispatches inner
		// HTTP calls against (e.g. compositions, sidebar widgets,
		// resourcesRefs targets) never received an informer, so the
		// 0.30.8 dep-tracker DeleteFunc never fired and
		// `evict_delete_total` stayed 0 after deliberate DELETE events
		// (probe `/tmp/snowplow-runs/0.30.91/preflight/probe.log`,
		// gates 1 + 4 FAIL).
		//
		// `call.Path` is the JQ-evaluated apiserver REST path
		// (e.g. `/apis/composition.krateo.io/v1/namespaces/<ns>/
		// githubscaffoldingwithcompositionpages`). `ParseAPIServerPathToGVR`
		// extracts (Group, Version, Resource) and skips non-apiserver
		// paths (external endpoints, malformed templated fragments).
		// EnsureResourceType is idempotent + singleflight under rw.mu;
		// duplicate calls within the loop are sub-microsecond no-ops.
		//
		// Timing instrumentation: if EnsureResourceType ever blocks
		// longer than lazyRegisterSlowThreshold we emit a WARN so the
		// 0.30.92 first-read-latency follow-up has a falsifier.
		lazyRegisterInnerCallPaths(ctx, log, tmp)

		// 0.30.95 bounded-parallel inner-call iterator.
		//
		// Pre-0.30.95 the inner-call loop was sequential — N inner calls
		// against the apiserver paid N × per-call latency. The architect's
		// 0.30.95 design replaces it with a bounded errgroup whose width
		// is iterParallelism() (GOMAXPROCS default, env-overridable, hard
		// cap 32). dictMu serialises all writes against `dict` from
		// concurrent goroutines (jsonHandler closure + error-branch
		// inline). gctx flows into httpcall.Do so the first hard-error
		// cancels in-flight peers when ContinueOnError=false.
		//
		// Edge type 3 dep-recording stays INLINE before g.Go (per the
		// 0.30.94 contract — sync.Map under the hood, safe to call from
		// the parent goroutine, no need to record from inside the worker).
		//
		// The success-branch log's `depth` field is computed by
		// depthForLog (support.go) — a mapDepth full-tree walk of dict.
		// Ship #6 gates that walk behind the Debug level; when it DOES
		// run (Debug on) it reads dict under dictMu, serialising against
		// concurrent jsonHandler writes. On the common (Info) path
		// neither the walk nor the lock runs.
		var dictMu sync.Mutex
		g, gctx := errgroup.WithContext(ctx)
		g.SetLimit(iterParallelism(ctx))

		// Ship 0.30.257 (#313) — per-item error slots (design §2.1 / §3.2).
		// One slot per iterator item, index-aligned with tmp. Each worker
		// writes ONLY its own itemErrs[i] (disjoint indices) and returns nil
		// on a per-item hard error, so the errgroup ctx is never cancelled by
		// a per-item failure → the remaining items proceed (Option C-A; the
		// SetLimit bound is unchanged). The slice backing array is allocated
		// HERE, before any g.Go, and never grown — disjoint-index writes to a
		// pre-sized slice are data-race-free without a lock (same property as
		// a pre-sized array; proven by TestResolve_IteratorErrorCollection_Race
		// under -race). The shared dict[errorKey] accumulation (W-A) is a
		// SEPARATE concern and stays under dictMu.
		itemErrs := make([]error, len(tmp))

		// Ship 0.30.192 — record iterator fan-out cost. tmp slice
		// length is the call count (1 after cluster-list collapse,
		// N per-NS after the iterator path); iterStart anchors the
		// wall-clock that g.Wait() closes below.
		stageTiming.IteratorCalls = len(tmp)
		iterStart := time.Now()

		// Ship 0.30.121 R1-b — the operator kill-switch. Compute once per
		// stage: verbose is permitted ONLY when the RESTAction asked for it
		// (opts.Verbose) AND the env flag explicitly enables the wire-dump.
		// Default off => wireVerbose is false => no call ever gets Debug.
		wireVerbose := opts.Verbose && verboseWireDumpEnabled()

		for i := range tmp {
			call := tmp[i]
			// Ship 0.30.121 R1-a — per-call verbose decision. `ep` is shared
			// across every tmp[] call of this stage; setting ep.Debug in
			// place would race the concurrent g.Go workers. When this call
			// wants the wire-dump, take a SHALLOW COPY of the Endpoint value
			// and flip Debug on the copy, so peers keep the un-Debugged
			// shared Endpoint. A K8s collection LIST never wants it
			// (callWantsWireDump returns false) — that suppresses the
			// ~1.94 GiB DumpResponse alloc_space line.
			if callWantsWireDump(wireVerbose, call.Path) {
				epCopy := ep
				epCopy.Debug = true
				call.Endpoint = &epCopy
			} else {
				call.Endpoint = &ep
			}
			// Wrap jsonHandler so every dict[id] mutation goes through
			// dictMu. Three forms — Ship 0.30.128 P-CORE-1/2:
			//   * inner (io.ReadCloser) — the genuine httpcall.Do HTTP-body
			//     path; set as call.ResponseHandler.
			//   * innerBytes ([]byte) — an in-memory dispatch result whose
			//     bytes are already materialised (informer-served, nested
			//     /call, internal-rest-config); skips the redundant
			//     io.ReadAll copy.
			//   * innerValue (any) — an apistage content-cache hit whose
			//     gated envelope is already a decoded structured value;
			//     skips both the marshal and the unmarshal.
			// Ship 0.30.235 — UAF spec + stage name plumbed in so
			// jsonHandlerCore can run the refilter on the RAW envelope
			// BEFORE the stage filter projects items. The pre-0.30.235
			// post-g.Wait() applyUserAccessFilter call is DELETED.
			// Ship K / 0.30.245 — `dict` plumbs the resolver's
			// accumulated stage-output dict into jsonHandlerCore so the
			// UAF refilter's resolveUAFResources evaluates
			// resourcesFrom against UPSTREAM stage outputs (e.g.
			// `.crds` from a prior stage). Same map as `out`; refilter
			// reads BEFORE the merge so the current stage is absent
			// (correct — resourcesFrom never references the stage it
			// gates). Pre-Ship-K passed only `pig` (per-stage scope) →
			// resourcesFrom returned [] → 0-count regression on the
			// multi-stage compositions-list RA.
			hOpts := jsonHandlerOptions{
				key:         id,
				out:         dict,
				dict:        dict,
				filter:      apiCall.Filter,
				uaf:         apiCall.UserAccessFilter,
				apiCallName: apiCall.Name,
			}
			inner := jsonHandler(gctx, hOpts)
			innerBytesFn := jsonHandlerBytes(gctx, hOpts)
			innerValueFn := jsonHandlerValue(gctx, hOpts)
			call.ResponseHandler = func(r io.ReadCloser) error {
				dictMu.Lock()
				defer dictMu.Unlock()
				return inner(r)
			}
			// feedBytes / feedValue are the dictMu-protected in-memory
			// equivalents the readerFromBytes call sites below use instead
			// of call.ResponseHandler(readerFromBytes(...)).
			feedBytes := func(b []byte) error {
				dictMu.Lock()
				defer dictMu.Unlock()
				return innerBytesFn(b)
			}
			feedValue := func(v any) error {
				dictMu.Lock()
				defer dictMu.Unlock()
				return innerValueFn(v)
			}

			// Edge type 3 dep recording — see 0.30.94 ship for full
			// rationale. cache.Deps() is sync.Map-backed; idempotent
			// LoadOrStore; safe from this (parent-goroutine) site.
			//
			// Ship F1 (0.30.119): the dep edge attaches to whatever L1
			// key the request path threaded (the per-user resolved-output
			// key). The CONTENT-keyed api-stage entry records its OWN dep
			// edge inside the worker, keyed by the per-call content key
			// (gvr,ns,[name]) — so an informer event on a K8s call's GVR
			// dirty-marks the matching content entry and the refresher
			// re-dispatches that one call.
			if l1Key := cache.L1KeyFromContext(ctx); l1Key != "" && !cache.Disabled() {
				if ptr.Deref(call.Verb, http.MethodGet) == http.MethodGet {
					if gvr, ns, name, parseOK := cache.ParseAPIServerPathToDep(call.Path); parseOK {
						if name == "" {
							cache.Deps().RecordList(l1Key, gvr, ns)
						} else {
							cache.Deps().Record(l1Key, gvr, ns, name)
						}
						log.Debug("dep.recorded",
							slog.String("subsystem", "cache"),
							slog.String("edge_type", "innerCall"),
							slog.String("gvr", gvr.String()),
							slog.String("ns", ns),
							slog.String("name", name),
							slog.String("l1_key", l1Key),
						)
					}
				}
			}

			g.Go(func() error {
				log.Debug("calling api", slog.String("name", id),
					slog.String("host", call.Endpoint.ServerURL),
					slog.String("path", call.Path),
				)

				// Ship 0.30.123 (#155) — in-process nested /call. This is
				// the FIRST dispatch branch (before the informer pivot,
				// before httpcall.Do). When the stage's `path` is a
				// /call?resource=...&apiVersion=... loopback into snowplow's
				// OWN /call endpoint, resolve the referenced RESTAction
				// IN-PROCESS — no HTTP request, no Authorization header,
				// identity carried by the WithUserInfo already on ctx. This
				// lets a JWT-less / SA-credentialed resolve complete an
				// exportJwt loopback stage (the 0.30.120 poison) and is the
				// hard prerequisite for F2's startup SA-prewarm.
				//
				// Three structural gates, ALL must hold or the branch is
				// skipped and the call falls through to the informer pivot
				// / httpcall.Do exactly as 0.30.121:
				//   1. RESOLVER_INPROCESS_NESTED_CALL enabled (default true);
				//   2. the resolver seam is wired (nestedCallResolver != nil
				//      — the second structural fallback);
				//   3. the call is a GET whose path parses as a /call
				//      loopback (objects.ParseCallPathToObjectRef — SHAPE only,
				//      no resource/name/host literal).
				// On a nested error: honour ContinueOnError / ErrorKey
				// exactly as the HTTP path, AND bump the 0.30.120 stage-error
				// sink so layer (b)'s Put-gate still sees the failure.
				if inprocessNestedCallEnabled() && nestedCallResolver != nil &&
					ptr.Deref(call.Verb, http.MethodGet) == http.MethodGet {
					if ref, isLoopback := objects.ParseCallPathToObjectRef(call.Path); isLoopback {
						statusRaw, nerr := nestedCallResolver(gctx, ref,
							opts.PerPage, opts.Page, opts.Extras)
						if nerr != nil {
							log.Error("nested /call resolution failed",
								slog.String("name", id),
								slog.String("path", call.Path),
								slog.String("dispatch", "in-process-nested-call"),
								slog.String("error", nerr.Error()))
							dictMu.Lock()
							// Ship 0.30.257 (#313) W-A: accumulate per-item
							// errors under the shared errorKey (no last-wins).
							accumulateErrorKey(dict, call.ErrorKey, nerr.Error())
							dictMu.Unlock()
							// Layer (b) backstop (0.30.120): record the stage
							// error on the refresher's sink (nil on the
							// request path) so the error-aware Put-gate still
							// sees a nested-/call failure. #301: Bump captures
							// stage name + err for the decline-log sample
							// (nil-receiver-safe). UNCHANGED by #313 — the
							// refresher Put-gate (resolve_populate.go:242) and
							// the request-path Cache-A guard both key on this
							// Bump's Count(), so it MUST still fire.
							stageErrSink.Bump(id, nerr.Error())
							// Ship 0.30.257 (#313) Option C-A: a per-item hard
							// error NO LONGER cancels gctx. Record it into this
							// item's disjoint slot and return nil so the
							// remaining iterator items proceed (the errgroup
							// SetLimit bound is unchanged; only the per-item
							// cancel is removed). The %w-wrapped cause is
							// preserved — it now lands in itemErrs[i] (errors.Join
							// at post-g.Wait() makes it errors.Is-inspectable)
							// instead of g.Wait(). ContinueOnError no longer
							// changes the control flow here: BOTH cases record +
							// fall through (the historical ContinueOnError=false
							// fast-abort is intentionally retired per the #313
							// directive — iteration continues past per-item
							// errors regardless).
							if !call.ContinueOnError {
								itemErrs[i] = fmt.Errorf("api %s item %d failed: %w", id, i, nerr)
							}
							// Fall through to the success-log line either way,
							// mirroring the prior ContinueOnError contract.
						} else {
							// The in-process result IS the referenced
							// RESTAction's Status.Raw — byte-identical to the
							// HTTP /call response body. Feed the in-memory
							// bytes directly (Ship 0.30.128 P-CORE-1 — no
							// io.ReadAll copy).
							if err := feedBytes(statusRaw); err != nil {
								return err
							}
						}
						// Ship #6 — depthForLog runs mapDepth (under dictMu)
						// ONLY when Debug is enabled; on the common path it
						// returns the sentinel and does no work / takes no lock.
						depth := depthForLog(ctx, log, &dictMu, dict)
						dispatch := "in-process-nested-call"
						if nerr != nil {
							dispatch = "in-process-nested-call-error"
						}
						log.Info("api successfully resolved",
							slog.String("name", id),
							slog.String("host", call.Endpoint.ServerURL),
							slog.String("path", call.Path),
							slog.Int("depth", depth),
							slog.String("dispatch", dispatch),
						)
						return nil
					}
				}

				// 0.30.95 resolver pivot — dispatch GET reads to the
				// informer cache when the cache subsystem is on. #57:
				// implicit-on-cache — resolverUseInformer() now folds to
				// !cache.Disabled() (the standalone RESOLVER_USE_INFORMER
				// flag was retired). Cache OFF: this branch is byte-
				// identical to the apiserver path (R-FALSE-1 invariant).
				//
				// The pivot returns served=true ONLY when the call is
				// safely cache-servable (GET, parseable apiserver path,
				// cache=on, full-Unstructured informer, synced). All
				// other shapes (write verbs, subresources, external
				// URLs, metadata-only GVRs, pre-sync, 404) fall through
				// to the apiserver branch below unchanged.
				//
				// Ship F1 (0.30.119) — content-keyed api-stage L1. When
				// apistageEnabled, the pivot-served raw envelope is
				// cached identity-free under the per-call content key
				// (gvr, namespace, name-or-empty):
				//
				//   1. content Get(contentKey) — HIT: use the stored raw
				//      envelope, skip the dispatch entirely. MISS:
				//      dispatch UN-GATED (WithApistageContentResolve makes
				//      dispatchViaInformer skip its inline RBAC gate) and
				//      Put the raw envelope under contentKey.
				//   2. GATE the raw envelope (hit OR miss) with the
				//      REQUEST identity — gateContentEnvelope runs
				//      filterListByRBAC/filterGetByRBAC, the single F1
				//      gate site. served=false here is fail-closed (no
				//      identity / GET denied) → fall through to apiserver.
				//   3. feed the GATED envelope to call.ResponseHandler →
				//      jsonHandler/apiCall.Filter → dict[id], unchanged.
				//
				// The content entry holds only un-gated content; the
				// per-user narrowing is the fresh per-request gate at
				// step 2 — no cross-user leak, the hit path is gated too.
				// Flag-off (apistageEnabled false) this is byte-identical
				// to the 0.30.118 pivot path.
				if resolverUseInformer() {
					if apistageEnabled {
						if gatedVal, served, ok := apistageContentServe(gctx, apistageStore, call); ok {
							if served {
								// Ship 0.30.128 P-CORE-2: the gated envelope
								// is already a decoded structured value —
								// feed it direct (no marshal, no unmarshal).
								if err := feedValue(gatedVal); err != nil {
									return err
								}
								// Ship #6 — see depthForLog (support.go).
								depth := depthForLog(ctx, log, &dictMu, dict)
								log.Info("api successfully resolved",
									slog.String("name", id),
									slog.String("host", call.Endpoint.ServerURL),
									slog.String("path", call.Path),
									slog.Int("depth", depth),
									slog.String("dispatch", "apistage-content"),
								)
								return nil
							}
							// served=false — fail-closed (no identity / GET
							// denied): fall through to the apiserver branch,
							// whose per-user token narrows correctly.
						}
						// ok=false — the content layer could not serve this
						// call (not pivot-servable: write verb, external URL,
						// metadata-only GVR, pre-sync). Fall through.
					} else if raw, served := dispatchViaInformer(gctx, call); served {
						// Informer-served bytes are in memory — feed direct
						// (Ship 0.30.128 P-CORE-1 — no io.ReadAll copy).
						if err := feedBytes(raw); err != nil {
							return err
						}
						// Ship #6 — see depthForLog (support.go).
						depth := depthForLog(ctx, log, &dictMu, dict)
						log.Info("api successfully resolved",
							slog.String("name", id),
							slog.String("host", call.Endpoint.ServerURL),
							slog.String("path", call.Path),
							slog.Int("depth", depth),
							slog.String("dispatch", "informer"),
						)
						return nil
					}
				}

				// 0.30.104 Phase-1 TLS-CA fix — when an internal-dispatch
				// *rest.Config is on the context (Phase 1's SA-credentialed
				// startup walk attaches its rest.InClusterConfig() config
				// via cache.WithInternalRESTConfig), route apiserver-path
				// GET/LIST calls through a client-go dynamic client built
				// from that *rest.Config instead of plumbing's httpcall.Do.
				//
				// WHY: plumbing's httpcall.Do builds the HTTP client from
				// the Endpoint shape; its tlsConfigFor installs a custom CA
				// pool ONLY in the HasCertAuth() branch. The snowplow SA
				// endpoint is TOKEN-auth, so the SA's cluster CA is dropped
				// and the apiserver TLS handshake fails with
				// "x509: certificate signed by unknown authority" — Phase 1
				// never discovers the composition GVR. The context-carried
				// *rest.Config is the rest.InClusterConfig() value, which
				// carries the cluster CA verbatim; client-go's transport
				// installs it correctly. See internal_dispatch.go.
				//
				// BEHAVIOR-NEUTRAL: ordinary per-user requests never set
				// cache.WithInternalRESTConfig, so dispatchViaInternalRESTConfig
				// returns served=false for them and this block is a no-op —
				// the path is byte-identical to pre-0.30.104.
				//
				// A non-nil err here is the REAL apiserver error (a 403, a
				// genuine connectivity fault). We do NOT fall through to
				// httpcall.Do on error — that would just re-hit the broken
				// plumbing TLS path and mask the real error behind a second
				// x509 failure. We surface it exactly as an httpcall.Do
				// StatusFailure: write call.ErrorKey under dictMu, then
				// honour ContinueOnError.
				if raw, served, ierr := dispatchViaInternalRESTConfig(gctx, call); served || ierr != nil {
					if ierr != nil {
						log.Error("api call response failure", slog.String("name", id),
							slog.String("host", call.Endpoint.ServerURL),
							slog.String("path", call.Path),
							slog.String("dispatch", "internal-rest-config"),
							slog.String("error", ierr.Error()))
						dictMu.Lock()
						// Ship 0.30.257 (#313) W-A: accumulate per-item errors
						// under the shared errorKey (no last-wins).
						accumulateErrorKey(dict, call.ErrorKey, ierr.Error())
						dictMu.Unlock()
						// Ship 0.30.120 layer (b): record the stage error on
						// the refresher's sink (nil on the request path).
						// #301: Bump captures stage name + err for the
						// decline-log sample (nil-receiver-safe). UNCHANGED by
						// #313 — the refresher Put-gate + request-path Cache-A
						// guard both key on this Bump's Count().
						stageErrSink.Bump(id, ierr.Error())
						// Ship 0.30.257 (#313) Option C-A: a per-item hard error
						// no longer cancels gctx — record into the item's
						// disjoint slot and fall through so remaining items
						// proceed. The %w-wrapped cause moves from g.Wait() to
						// itemErrs[i]. We STILL must NOT fall through to
						// httpcall.Do here (the internal dispatcher OWNED this
						// call; re-hitting plumbing's broken TLS path would mask
						// the real error behind a second x509 failure) — the
						// success-log line + return below covers both the error
						// and the fed-bytes case.
						if !call.ContinueOnError {
							itemErrs[i] = fmt.Errorf("api %s item %d failed: %w", id, i, ierr)
						}
					} else {
						// internal-rest-config dispatch result is in memory
						// — feed direct (Ship 0.30.128 P-CORE-1).
						if err := feedBytes(raw); err != nil {
							return err
						}
					}
					// Ship #6 — see depthForLog (support.go).
					depth := depthForLog(ctx, log, &dictMu, dict)
					dispatch := "internal-rest-config"
					if ierr != nil {
						dispatch = "internal-rest-config-error"
					}
					log.Info("api successfully resolved",
						slog.String("name", id),
						slog.String("host", call.Endpoint.ServerURL),
						slog.String("path", call.Path),
						slog.Int("depth", depth),
						slog.String("dispatch", dispatch),
					)
					return nil
				}

				res := httpcall.Do(gctx, call)
				if res.Status == response.StatusFailure {
					log.Error("api call response failure", slog.String("name", id),
						slog.String("host", call.Endpoint.ServerURL),
						slog.String("path", call.Path),
						slog.String("error", res.Message))

					asMap, mapErr := response.AsMap(res)
					if mapErr != nil {
						log.Warn("unable to encode status as dict", slog.Any("err", mapErr))
					}

					dictMu.Lock()
					// Ship 0.30.257 (#313) W-A: accumulate per-item errors
					// under the shared errorKey (no last-wins). The recorded
					// VALUE is unchanged per item — the asMap (when AsMap
					// succeeded) else the raw message string; W-A only changes
					// the container scalar→accumulating-slice.
					if len(asMap) > 0 {
						accumulateErrorKey(dict, call.ErrorKey, asMap)
					} else {
						accumulateErrorKey(dict, call.ErrorKey, res.Message)
					}
					dictMu.Unlock()
					// Ship 0.30.120 layer (b): record the stage error on the
					// refresher's sink (nil on the request path) — covers both
					// the asMap and res.Message ErrorKey-write branches above.
					// #301: Bump captures stage name + err for the decline-log
					// sample (nil-receiver-safe). UNCHANGED by #313 — the
					// refresher Put-gate + request-path Cache-A guard both key
					// on this Bump's Count().
					stageErrSink.Bump(id, res.Message)

					// Ship 0.30.257 (#313) Option C-A: a per-item hard error no
					// longer cancels gctx — record it into this item's disjoint
					// slot and fall through so the remaining iterator items
					// proceed. res.Message is a string (not an error), so the
					// item error wraps it with %s (mirrors the pre-0.30.257
					// g.Wait()-returned shape); the error nonetheless aggregates
					// via errors.Join at post-g.Wait().
					if !call.ContinueOnError {
						itemErrs[i] = fmt.Errorf("api %s item %d failed: %s", id, i, res.Message)
					}
					// Fall through to the success-log line (preserves
					// pre-0.30.95 behaviour where the
					// "successfully resolved" line emitted on every
					// non-hard-error call).
				}

				// Ship #6 — depthForLog runs mapDepth (the full-tree walk)
				// under dictMu ONLY when Debug is enabled — serialising the
				// read against concurrent jsonHandler writes. On the common
				// (Info) path it does no work and takes no lock.
				depth := depthForLog(ctx, log, &dictMu, dict)
				log.Info("api successfully resolved",
					slog.String("name", id),
					slog.String("host", call.Endpoint.ServerURL),
					slog.String("path", call.Path),
					slog.Int("depth", depth),
				)
				return nil
			})
		}

		// Ship 0.30.257 (#313) — post-fan-out handling. With Option C-A
		// the three per-ITEM hard-error branches record into itemErrs[i]
		// and return nil, so g.Wait() NO LONGER returns non-nil for a
		// per-item dispatch failure (the stage runs all items + downstream
		// stages run — the central #313 behaviour change). g.Wait() can
		// still return non-nil for a NON-per-item worker error: a response
		// HANDLER failure (feedBytes / feedValue / inner returning a decode
		// error — NOT in #313's scope) or a genuine ctx-cancellation
		// surfacing through such a handler. For THOSE we preserve the
		// pre-0.30.257 short-circuit + truncated-dict return verbatim.
		if err := g.Wait(); err != nil {
			log.Debug("api stage short-circuited on hard error",
				slog.String("name", id), slog.Any("err", err))
			stageTiming.IteratorElapsedMs = time.Since(iterStart).Milliseconds()
			recordStageTiming()
			return dict
		}
		stageTiming.IteratorElapsedMs = time.Since(iterStart).Milliseconds()

		// Ship 0.30.257 (#313) — per-item errors recorded by the worker
		// branches above do NOT cancel the stage; they were collected into
		// itemErrs (disjoint indices) and the matching dict[errorKey]
		// accumulating-slice entries (W-A) already carry them on the wire.
		// errors.Join folds them into a single errors.Is-inspectable
		// aggregate (preserving the %w wraps the worker branches set) for a
		// Debug diagnostic line ONLY — api.Resolve still returns a map, never
		// an error (trace §2.2). The wire result is "all successful items +
		// the accumulated per-item errors under errorKey". The refresher /
		// request-path Cache-A guards (keyed on stageErrSink.Count(), bumped
		// above) decide whether the partial-with-errors result is PERSISTED.
		if joined := errors.Join(itemErrs...); joined != nil {
			log.Debug("api stage had per-item errors (continuing)",
				slog.String("name", id), slog.Any("err", joined))
		}

		// Ship 0.30.235 — the pre-0.30.235 post-g.Wait()
		// applyUserAccessFilter call was DELETED; UAF now runs per-worker
		// inside jsonHandlerCore on the raw envelope. See
		// refilter_layering_test.go for the permanent regression gate.

		// Ship F1 (0.30.119): the api-stage L1 is now CONTENT-keyed —
		// the per-K8s-call Put happens inside the g.Go worker (each call
		// stores its own raw envelope under its (gvr,ns,[name]) content
		// key). There is NO per-stage Put here — the Ship E per-stage
		// entry is gone; an iterator stage produces N content entries,
		// one per call, assembled into dict[id] by the N jsonHandler
		// merges exactly as before.

		// Ship 0.30.192 — emit per-stage timing on normal completion.
		// Records ElapsedMs (stage-total wall-clock) + IteratorElapsedMs
		// (g.Wait()-bounded fan-out cost) for the cohort timing log line.
		recordStageTiming()
	}

	// Ship 2a (0.30.209) — the per-serve removeManagedFields(dict) walk is
	// GONE. managedFields is now stripped ONCE at the four item-
	// materialisation sites (parseListEnvelope, validateClusterListShape,
	// gateGetEnvelope, gateListEnvelope→parseListEnvelope), where the item
	// map is still private. With the Ship 2a SHALLOW envelope, this walk
	// `delete(v, "managedFields")`'d the SHARED entry.Items maps in place,
	// racing concurrent serves' reads (the -race in
	// TestResolve_ConcurrentRequestsDoNotCrossPollinate). Stripping at
	// load means dict's only maps here are the fresh per-serve outer
	// envelope + jq-constructed objects (never carry managedFields), so
	// the walk is both unsafe (shared write) and unnecessary. Dropping it
	// also removes a per-serve O(nodes) full-tree traversal (perf win).
	//delete(dict, "slice")

	return dict
}

// lazyRegisterInnerCallPaths walks the per-stage RequestOptions slice
// (one entry per iterator dispatch — the iterator + non-iterator paths
// share this code) and calls cache.Global().EnsureResourceType for the
// GVR derived from each call.Path. Idempotent across paths that point
// at the same GVR (singleflight under rw.mu).
//
// Cache=off / watcher=nil branch is silently skipped — there is no
// informer to register and the apiserver-fallback path in
// `httpcall.Do` handles the call regardless.
//
// Paths that don't resolve to an apiserver GVR (external endpoints,
// JQ-evaluation failures that leak `${...}` to the final string) are
// also silently skipped — those have no informer counterpart and the
// dispatch will hit the external URL through `httpcall.Do` as before.
//
// Timing: per-call duration is measured; calls slower than
// lazyRegisterSlowThreshold emit a WARN log so a regression in
// rw.mu contention or factory.ForResource cost becomes visible.
func lazyRegisterInnerCallPaths(ctx context.Context, log *slog.Logger, opts []httpcall.RequestOptions) {
	rw := cache.Global()
	if rw == nil {
		return
	}
	seen := map[schema.GroupVersionResource]struct{}{}
	for i := range opts {
		path := opts[i].Path

		// 0.30.102 Tag B Part 2 — composition-group walker feed.
		// Composition apiserver paths are JQ-templated
		// (`/apis/<group>/${.v}/...`), so ParseAPIServerPathToGVR
		// (which rejects any `${`) cannot derive their GVR here. The
		// GROUP segment is static, though — extract it and:
		//
		//  1. Record it in the navigation-discovered set (so the
		//     watcher's removable-discriminator at watcher.go:749 +
		//     :1064 routes composition GVRs to the standalone-informer
		//     branch — the only branch RemoveResourceType can ever
		//     tear down).
		//
		//  2. Ship 0.5 / 0.30.223 (v6): invoke cache.DiscoverGroup-
		//     Resources synchronously. One-shot apiserver discovery
		//     enumerates every CRD-backed resource in `grp`, calls
		//     EnsureResourceType for each (spawning composition
		//     informers), and fires the FD1 dirty-mark chain via
		//     Deps().OnResourceTypeAvailable for genuinely-new GVRs.
		//     Replaces the deleted CRD-informer event-driven backplane
		//     (Ship 0 walker-spawn + the pre-v6 CRD-watch file).
		//     Soft-fails: a
		//     discovery error is logged and the walker continues
		//     (subsequent walks retry); the dispatch through the
		//     unregistered composition GVR will hit apiserver via
		//     fall-through.
		//
		// Gated by PrewarmEnabled() — #57 implicit-on-cache, so a
		// cache-OFF process is byte-identical (the nav-discovered set
		// stays empty and the discovery hop never runs). Non-templated
		// paths also flow through here harmlessly — their group is added
		// too (it IS navigation-reached).
		if cache.PrewarmEnabled() {
			if grp, grpOK := cache.ExtractAPIServerGroupFromTemplatedPath(path); grpOK {
				cache.AddNavigationDiscoveredGroup(grp)
				// The walker's *rest.Config is wired through
				// cache.WithInternalRESTConfig during Phase 1 (SA-
				// credentialed walk). When absent (e.g. a non-Phase-1
				// /call), the discovery hop is skipped — the only
				// caller pattern that requires it is the SA-credentialed
				// walker, which DOES set it.
				if rcAny, ok := cache.InternalRESTConfigFromContext(ctx); ok {
					if cfg, ok := rcAny.(*rest.Config); ok && cfg != nil {
						if _, err := cache.DiscoverGroupResources(ctx, cfg, grp); err != nil {
							log.Warn("cache.discovery.group_resources_fetch_failed",
								slog.String("subsystem", "cache"),
								slog.String("group", grp),
								slog.Any("err", err),
							)
						}
					}
				}
			}
		}

		gvr, ok := cache.ParseAPIServerPathToGVR(path)
		if !ok {
			continue
		}
		if _, dup := seen[gvr]; dup {
			continue
		}
		seen[gvr] = struct{}{}

		start := time.Now()
		added, _ := rw.EnsureResourceType(gvr)
		elapsed := time.Since(start)

		// Emit a one-shot INFO line on first registration of a GVR so
		// the gate-2 probe can count distinct lazy-registered GVRs.
		// The cache layer emits its own `cache.lazy_register` line —
		// we add a callsite-specific marker so post-mortems can tell
		// whether the entry came from a widget dep edge or an inner
		// resolver call.
		if added {
			log.Info("cache.lazy_register.inner_call",
				slog.String("subsystem", "cache"),
				slog.String("gvr", gvr.String()),
				slog.String("path", path),
				slog.Duration("ensure_elapsed", elapsed),
				slog.String("hint", "resolver inner-call first touch — informer registered + dep-tracker handlers wired"),
			)
		}
		if elapsed > lazyRegisterSlowThreshold {
			log.Warn("cache.lazy_register.slow",
				slog.String("subsystem", "cache"),
				slog.String("gvr", gvr.String()),
				slog.Duration("elapsed", elapsed),
				slog.Duration("threshold", lazyRegisterSlowThreshold),
				slog.String("hint", "EnsureResourceType blocked unexpectedly long — investigate rw.mu contention"),
			)
		}
	}
}

// removeManagedFields was the per-serve recursive managedFields stripper
// called at the end of Resolve. Ship 2a (0.30.209) removed it: with the
// shallow envelope it wrote the shared entry.Items in place (a data
// race), and managedFields is now stripped once at the item-
// materialisation sites (stripManagedFields in apistage.go /
// cluster_list.go). The function is intentionally deleted rather than
// parked — feedback_no_park_broken_behind_flag.
