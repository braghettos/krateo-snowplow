// Q-PREWARM-R2R5 PR-B (R5) — event-driven prewarm via UserSecretWatcher.
//
// Replaces the synchronous `WarmL1FromEntryPoints` per-binding-group serial
// loop with an event-driven worker pool that consumes per-user jobs enqueued
// by the UserSecretWatcher informer. ADD events on `<user>-clientconfig`
// secrets become prewarm jobs; DELETE events trigger L1 + L2 evict; the pool
// drains at K-parallel without monopolising any single OS thread for minutes
// at a time.
//
// Spec: /tmp/snowplow-runs/q-cold-1-cd-v6-phase6-50k-postPR2PR3-2026-05-05/
//       Q-PREWARM-R2R5-SPEC.md §2.
//
// Activation gate: env PREWARM_MODE.
//   - "event-driven" (default): worker pool is started; the synchronous
//     entry-point loop in main.go is skipped.
//   - "legacy": worker pool is NOT started; UserSecretWatcher.OnUserReady
//     is wired to a no-op (current behaviour pre-R5); main.go runs the
//     synchronous WarmL1FromEntryPoints loop.
package dispatchers

import (
	"context"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/jwtutil"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/dynamic"
	k8sdynamic "k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

// poolHeapSampleInterval is how often the peak-tracking goroutine samples
// HeapAlloc while the pool is draining (Lever A G3, R5 followup).
//
// 200ms matches the legacy WarmL1FromEntryPoints sampler so peak readings
// are directly comparable across the two prewarm code paths.
const poolHeapSampleInterval = 200 * time.Millisecond

// poolDrainQuietWindow is how long the queue must remain empty (with at
// least one job processed) before the pool is considered "drained" for
// the purpose of capturing the G3 heap snapshot.
//
// In R5 the pool is event-driven and never reaches a permanent terminal
// state — new users could log in hours later. We declare drain when the
// initial LIST-fed cohort has finished, so the snapshot reflects the
// peak under cold-start prewarm load (the gate G3 actually targets).
//
// 5s is generous enough to cover gaps between queued jobs at K=8 workers
// without prematurely sampling mid-burst.
const poolDrainQuietWindow = 5 * time.Second

// PrewarmJob is a single per-user prewarm task. The worker pulls it from
// the bounded queue and runs the entry-point widget-tree walk for that
// user's binding-identity cohort.
type PrewarmJob struct {
	Username string
	Token    string // short-lived JWT minted at enqueue time
	Endpoint endpoints.Endpoint
	Groups   []string
}

// PrewarmWorkerPool consumes PrewarmJobs from a bounded channel and runs
// the widget-tree walk for each user. K workers drain the queue
// concurrently. The pool is started exactly once via Start; subsequent
// calls to Start are no-ops.
//
// Concurrency contract:
//   - All exported fields must be set BEFORE Start is called.
//   - Enqueue is safe from any goroutine after Start has been called.
//   - The pool stops cleanly when its parent context is cancelled.
//
// Per spec §2.6 (pod restart): the pool does NOT pre-discover users.
// The UserSecretWatcher informer's initial LIST emits ADD events for
// every existing -clientconfig secret on pod start; those events feed
// the queue. No separate cold-start enumeration is required.
type PrewarmWorkerPool struct {
	// Workers is the target worker count (clamped to [1, 32] in Start).
	Workers int
	// QueueCap is the bounded channel capacity (clamped to [16, 65536]).
	QueueCap int

	// Cache is the in-process L1/L2 store.
	Cache cache.Cache
	// AuthnNS is the namespace where -clientconfig secrets live.
	AuthnNS string
	// EntryPoints is the frontend-config widget-tree starting set.
	EntryPoints []cache.EntryPoint
	// RBACWatcher is consulted for the binding identity of each user.
	RBACWatcher *cache.RBACWatcher
	// SnowplowEndpointFn provides the elevated-call endpoint for
	// userAccessFilter resolution (same wiring as WarmL1FromEntryPoints).
	SnowplowEndpointFn func() (*endpoints.Endpoint, error)
	// SnowplowK8sClient provides the in-cluster SA dynamic client used
	// for the Path B dispatch (Q-RBAC-DECOUPLE C(d) v6).
	SnowplowK8sClient dynamic.Client
	// DynClient is the unthrottled SA-backed dynamic client used for L2
	// miss fallback during prewarm. Built once by main.go and shared.
	DynClient k8sdynamic.Interface

	// JobTimeout caps a single per-user widget-tree walk. Defaults to
	// preWarmTimeout (30s) when zero.
	JobTimeout time.Duration

	queue   chan PrewarmJob
	startMu sync.Mutex
	started atomic.Bool
	stopped atomic.Bool

	// inflight tracks observable progress. Read-only outside the pool.
	processed atomic.Int64
	dropped   atomic.Int64
	enqueued  atomic.Int64

	// inflightUsers prevents two workers from racing on the same
	// username. Mirrors the per-user tryLock semantics of
	// UserSecretWatcher.warmingUsers, kept independent here so the
	// worker pool is robust even if the watcher's lock semantics change.
	inflightUsers sync.Map // username -> struct{}
}

// Start launches the worker goroutines. Safe to call from main wiring.
// Idempotent: subsequent calls are no-ops. Workers exit when ctx is
// cancelled. Returns the pool itself for chained wiring.
func (p *PrewarmWorkerPool) Start(ctx context.Context) *PrewarmWorkerPool {
	p.startMu.Lock()
	defer p.startMu.Unlock()
	if p.started.Load() {
		return p
	}

	// Clamp inputs. Refuse to start with degenerate config; instead pin
	// to documented defaults so a typo in env doesn't silently disable
	// the path.
	if p.Workers < 1 {
		p.Workers = 1
	}
	if p.Workers > 32 {
		p.Workers = 32
	}
	if p.QueueCap < 16 {
		p.QueueCap = 16
	}
	if p.QueueCap > 65536 {
		p.QueueCap = 65536
	}
	if p.JobTimeout <= 0 {
		p.JobTimeout = preWarmTimeout
	}

	p.queue = make(chan PrewarmJob, p.QueueCap)
	p.started.Store(true)

	for i := 0; i < p.Workers; i++ {
		workerID := i
		go p.workerLoop(ctx, workerID)
	}

	// Lever A peak-alloc instrumentation (Q-COLD-1 PM gate G3, R5 followup
	// 0.25.308). The legacy WarmL1FromEntryPoints sampler at prewarm.go:451
	// never fires under PREWARM_MODE=event-driven (default). Without this,
	// the prewarm.* block in /metrics/runtime stays empty in production.
	//
	// Both paths target the same prewarmHeapStats atomic.Pointer. They are
	// mutually exclusive per pod lifetime (legacy is wired to a no-op when
	// the pool is started) so there is no race on the publish.
	go p.runHeapInstrumentation(ctx)

	slog.Default().Info("prewarm-pool: started",
		slog.Int("workers", p.Workers),
		slog.Int("queueCap", p.QueueCap),
		slog.Int("entryPoints", len(p.EntryPoints)),
		slog.Duration("jobTimeout", p.JobTimeout),
	)
	return p
}

// runHeapInstrumentation samples HeapAlloc at pool start, tracks the
// peak via a 200ms-tick goroutine, and publishes the G3 snapshot once
// the initial cohort has drained (queue empty for poolDrainQuietWindow
// with processed > 0).
//
// Mirrors the pattern in WarmL1FromEntryPoints (prewarm.go:451-548) so
// the legacy and R5 paths produce comparable numbers.
//
// Lifecycle: one-shot per pool. Exits on ctx cancel even if drain never
// completes (e.g. pod has zero -clientconfig secrets).
func (p *PrewarmWorkerPool) runHeapInstrumentation(ctx context.Context) {
	var startMs runtime.MemStats
	runtime.ReadMemStats(&startMs)
	heapStart := startMs.HeapAlloc
	var heapPeak atomic.Uint64
	heapPeak.Store(heapStart)

	prewarmStart := time.Now()
	sampleTicker := time.NewTicker(poolHeapSampleInterval)
	defer sampleTicker.Stop()

	// drainCheck fires more sparsely than the sample tick — we don't
	// need millisecond precision on drain detection. 500ms is small
	// relative to the 5s quiet-window so jitter on detection is bounded.
	drainCheck := time.NewTicker(500 * time.Millisecond)
	defer drainCheck.Stop()

	var ms runtime.MemStats
	var quietSince time.Time

	for {
		select {
		case <-ctx.Done():
			// Pod shutdown before drain. Publish what we have so the
			// canary observer can still distinguish "pool ran but did
			// not drain" from "pool never started". Use the last peak
			// we observed and the current HeapAlloc as end.
			runtime.ReadMemStats(&ms)
			peak := heapPeak.Load()
			if ms.HeapAlloc > peak {
				peak = ms.HeapAlloc
			}
			p.publishHeapStats(heapStart, peak, ms.HeapAlloc, time.Since(prewarmStart).Milliseconds(), "ctx-cancel")
			return

		case <-sampleTicker.C:
			runtime.ReadMemStats(&ms)
			cur := ms.HeapAlloc
			for {
				prev := heapPeak.Load()
				if cur <= prev {
					break
				}
				if heapPeak.CompareAndSwap(prev, cur) {
					break
				}
			}

		case <-drainCheck.C:
			// Drain criterion: at least one user processed AND the queue
			// has been empty for poolDrainQuietWindow.
			processed, _, _ := p.processedEnqueuedDropped()
			if processed == 0 || p.QueueDepth() != 0 {
				quietSince = time.Time{}
				continue
			}
			if quietSince.IsZero() {
				quietSince = time.Now()
				continue
			}
			if time.Since(quietSince) < poolDrainQuietWindow {
				continue
			}

			// Drained. Capture end state and publish.
			runtime.ReadMemStats(&ms)
			peak := heapPeak.Load()
			if ms.HeapAlloc > peak {
				// Final read may exceed last sample; preserve the true
				// peak.
				peak = ms.HeapAlloc
			}
			p.publishHeapStats(heapStart, peak, ms.HeapAlloc, time.Since(prewarmStart).Milliseconds(), "drained")
			return
		}
	}
}

// processedEnqueuedDropped returns the same triple as Stats() but in
// (processed, enqueued, dropped) order so the drain detector can name
// the first field "processed" without aliasing the public API.
func (p *PrewarmWorkerPool) processedEnqueuedDropped() (processed, enqueued, dropped int64) {
	return p.processed.Load(), p.enqueued.Load(), p.dropped.Load()
}

// publishHeapStats writes the snapshot to the package-global atomic and
// emits a final log line. Reason is "drained" on normal completion or
// "ctx-cancel" on early shutdown.
func (p *PrewarmWorkerPool) publishHeapStats(start, peak, end uint64, durationMs int64, reason string) {
	stats := &PrewarmHeapStats{
		HeapStartBytes: start,
		HeapPeakBytes:  peak,
		HeapEndBytes:   end,
		DurationMs:     durationMs,
	}
	prewarmHeapStats.Store(stats)

	processed, enqueued, dropped := p.processedEnqueuedDropped()
	slog.Default().Info("prewarm-pool: heap-alloc snapshot published",
		slog.String("reason", reason),
		slog.Int64("prewarm_duration_ms", durationMs),
		slog.Float64("prewarm_heap_alloc_start_mb", float64(start)/(1024*1024)),
		slog.Float64("prewarm_heap_alloc_peak_mb", float64(peak)/(1024*1024)),
		slog.Float64("prewarm_heap_alloc_end_mb", float64(end)/(1024*1024)),
		slog.Float64("prewarm_heap_delta_mb", float64(int64(peak)-int64(start))/(1024*1024)),
		slog.Int64("processed", processed),
		slog.Int64("enqueued", enqueued),
		slog.Int64("dropped", dropped),
	)
}

// Enqueue offers a job to the pool. Returns true on success; returns
// false (and increments the dropped counter) when the bounded queue is
// full. Per spec §2.2, drops are non-fatal — the informer re-emits ADD
// on resync, so dropped users get a second chance.
//
// Non-blocking by contract: the caller is the informer ADD handler,
// which must not stall client-go's shared workqueue. A blocking send
// here would back-pressure into the informer factory and freeze RBAC
// + secret event delivery for everyone.
func (p *PrewarmWorkerPool) Enqueue(job PrewarmJob) bool {
	if !p.started.Load() || p.stopped.Load() {
		return false
	}
	if job.Username == "" {
		return false
	}
	select {
	case p.queue <- job:
		p.enqueued.Add(1)
		return true
	default:
		p.dropped.Add(1)
		slog.Default().Warn("prewarm-pool: queue full, dropping job (informer will re-enqueue on resync)",
			slog.String("user", job.Username),
			slog.Int64("dropped", p.dropped.Load()),
		)
		return false
	}
}

// Stats returns observable counters. Used by tests and operator probes.
func (p *PrewarmWorkerPool) Stats() (enqueued, processed, dropped int64) {
	return p.enqueued.Load(), p.processed.Load(), p.dropped.Load()
}

// QueueDepth returns the number of jobs currently buffered. Tests use
// this to wait for the pool to drain.
func (p *PrewarmWorkerPool) QueueDepth() int {
	if p.queue == nil {
		return 0
	}
	return len(p.queue)
}

// QueueCapacity returns the configured queue capacity (post-clamp).
func (p *PrewarmWorkerPool) QueueCapacity() int {
	return p.QueueCap
}

// workerLoop is the per-worker drain. Exits on ctx cancellation.
func (p *PrewarmWorkerPool) workerLoop(ctx context.Context, workerID int) {
	for {
		select {
		case <-ctx.Done():
			return
		case job, ok := <-p.queue:
			if !ok {
				return
			}
			p.processOne(ctx, workerID, job)
		}
	}
}

// processOne executes a single per-user prewarm walk. Wraps the per-user
// timeout, the per-user inflight lock, and the panic-recovery boundary
// so that a single user's failure cannot wedge the worker.
func (p *PrewarmWorkerPool) processOne(parentCtx context.Context, workerID int, job PrewarmJob) {
	// Per-user inflight lock: skip if another worker is already
	// processing this user. Same semantics as UserSecretWatcher's
	// warmingUsers but local to the pool.
	if _, loaded := p.inflightUsers.LoadOrStore(job.Username, struct{}{}); loaded {
		slog.Debug("prewarm-pool: user already inflight, skipping",
			slog.String("user", job.Username),
		)
		return
	}
	defer p.inflightUsers.Delete(job.Username)

	defer func() {
		if r := recover(); r != nil {
			slog.Error("prewarm-pool: panic in worker (recovered)",
				slog.Int("worker", workerID),
				slog.String("user", job.Username),
				slog.Any("panic", r),
			)
		}
	}()

	jobCtx, cancel := context.WithTimeout(parentCtx, p.JobTimeout)
	defer cancel()

	start := time.Now()
	visited := p.runPerUser(jobCtx, job)
	p.processed.Add(1)

	slog.Default().Info("prewarm-pool: user done",
		slog.Int("worker", workerID),
		slog.String("user", job.Username),
		slog.Int("warmed", visited),
		slog.Duration("elapsed", time.Since(start)),
		slog.Int64("processed", p.processed.Load()),
	)
}

// runPerUser builds the per-user warm context and runs the entry-point
// widget-tree walk. Returns the number of unique widgets visited (used
// for observability). Mirrors the inner block of WarmL1FromEntryPoints
// for one binding group.
func (p *PrewarmWorkerPool) runPerUser(ctx context.Context, job PrewarmJob) int {
	// Inject elevated-call endpoint provider (same as WarmL1FromEntryPoints).
	if p.SnowplowEndpointFn != nil {
		ctx = cache.WithSnowplowEndpoint(ctx, func() (any, error) {
			return p.SnowplowEndpointFn()
		})
	}
	// Inject Path B SA k8s client (Q-RBAC-DECOUPLE C(d) v6).
	if p.SnowplowK8sClient != nil {
		ctx = cache.WithSnowplowK8s(ctx, p.SnowplowK8sClient)
	}

	user := jwtutil.UserInfo{Username: job.Username, Groups: job.Groups}

	// Compute binding identity for this user. When the RBAC watcher is
	// not yet synced (e.g. very first ADD before WaitForSync resolves),
	// the helper returns "" and we fall back to the username — same
	// behaviour as the synchronous loop at prewarm.go:381-384.
	bid := ""
	if p.RBACWatcher != nil {
		bid = p.RBACWatcher.CachedBindingIdentity(job.Username, job.Groups)
	}
	if bid == "" {
		bid = job.Username
	}

	// Q-RBAC-DECOUPLE C(d) v3 — prewarm fills the UNFILTERED L1 shape
	// (cachedRESTAction wrapper). Per-user refilter happens at HTTP-time.
	warmCtx := cache.WithBindingIdentity(ctx, bid)
	if p.RBACWatcher != nil {
		warmCtx = cache.WithRBACWatcher(warmCtx, p.RBACWatcher)
	}

	if len(p.EntryPoints) == 0 {
		// Nothing to walk; treat as success so the user is marked done.
		return 0
	}

	epRefs := make([]l1Ref, 0, len(p.EntryPoints))
	for _, ep := range p.EntryPoints {
		epRefs = append(epRefs, l1Ref{gvr: ep.GVR, ns: ep.Namespace, name: ep.Name})
	}

	visited := make(map[string]bool)

	// Build the per-request rctx the same way warmL1RestActionsForUser
	// does, so child resolution gets WithUserConfig / WithUserInfo /
	// WithAccessToken in scope.
	rctx := xcontext.BuildContext(warmCtx,
		xcontext.WithUserConfig(job.Endpoint),
		xcontext.WithUserInfo(user),
		xcontext.WithAccessToken(job.Token),
		xcontext.WithLogger(slog.Default()),
	)
	rctx = cache.WithCache(rctx, p.Cache)

	recursivePreWarm(rctx, user, job.Endpoint, job.Token, p.Cache, p.DynClient, epRefs, p.AuthnNS, visited, 1)
	return len(visited)
}

// MakeEventDrivenPrewarmer returns a UserReadyFunc that enqueues a
// prewarm job into the worker pool whenever the UserSecretWatcher
// informer ADDs or UPDATEs a -clientconfig secret. Per spec §2.1.
//
// The closure resolves the per-user endpoint + groups by reading the
// secret via endpoints.FromSecret (same path as discoverUsers in the
// synchronous loop). It mints a short-lived JWT for the worker to use
// when child RESTAction calls hit /call internally with exportJwt:true.
//
// Per `feedback_no_special_cases.md`: nothing user-specific is hardcoded
// here. The walk plan is the entry-point list passed at pool
// construction; per-user data is pulled from the secret.
func MakeEventDrivenPrewarmer(pool *PrewarmWorkerPool, rc *rest.Config, signKey string) cache.UserReadyFunc {
	return func(ctx context.Context, username string) {
		if pool == nil {
			return
		}
		if !pool.started.Load() {
			return
		}
		// Build the per-user endpoint from the freshly-added secret.
		// Use a short bounded timeout so a slow API server cannot wedge
		// the informer ADD goroutine indefinitely.
		fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		secretName := username + clientConfigSecretSuffix
		ep, err := endpoints.FromSecret(fetchCtx, rc, secretName, pool.AuthnNS)
		if err != nil {
			slog.Warn("prewarm-pool: failed to load endpoint for user; skipping enqueue",
				slog.String("user", username),
				slog.Any("err", err),
			)
			return
		}
		groups := extractGroupsFromClientCert(ep.ClientCertificateData)

		token := mintJWT(jwtutil.UserInfo{Username: username, Groups: groups}, signKey)

		_ = pool.Enqueue(PrewarmJob{
			Username: username,
			Token:    token,
			Endpoint: ep,
			Groups:   groups,
		})
	}
}
