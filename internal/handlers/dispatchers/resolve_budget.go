package dispatchers

// ─────────────────────────────────────────────────────────────────────
// Customer-resolve memory budget — process-wide weighted concurrency cap.
//
// Why this exists (project_regression_journal 2026-06-23): the customer
// /call entry (restactions.go / widgets.go ServeHTTP) had NO bound on how
// many heavy resolves could run concurrently. markCustomerInFlight
// (prewarm_engine.go:98) is a COUNTER (a yield signal for the prewarm
// engine), NOT a bound — it never blocks. On bs-test-ger-03 boot the SPA
// fired a burst of cold composition-resources /call dispatches; each
// allCompositionResources resolve holds a large dict to resolve.go:1508
// PLUS a second full copy at encodeResolvedJSON (restactions.go:241), so
// ~5 concurrent cold resolves stacked to a ~7.5GiB transient peak and the
// pod was OOMKilled.
//
// The fix is a single process-wide weighted semaphore on the customer
// resolve entry. Each dispatch acquires `weight` (a measured per-resolve
// peak) before resolving; the sum of in-flight weights can never exceed
// `budget` (a fraction of GOMEMLIMIT), so concurrent resolves are bounded
// to roughly budget/weight and the transient heap is capped INDEPENDENT
// of the inbound burst size.
//
// This is ALWAYS-ON, env-tunable — NOT a default-off toggle. An OOMKill
// is a safety defect (feedback_no_park_broken_behind_flag); the bound is
// the correct steady-state posture. The escape-hatch
// RESOLVE_BUDGET_BYTES=<MaxInt64> disables the bound and reproduces the
// original OOM, for diagnosis only.
//
// Acquire site: BOTH dispatcher ServeHTTP entries (NOT inside Resolve).
// The /call loopback is retired (PM-verified) so nested RA→RA / RA→widget
// resolves run in-process and never re-enter ServeHTTP — acquiring at
// ServeHTTP is therefore re-entrancy-safe (no risk of a held permit
// waiting on a nested acquire that the same goroutine must satisfy).
// ─────────────────────────────────────────────────────────────────────

import (
	"context"
	"math"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"

	"github.com/krateoplatformops/plumbing/env"
	"golang.org/x/sync/semaphore"
)

const (
	// envResolveBudgetBytes — absolute byte budget for the sum of
	// in-flight customer-resolve weights. When set (and > 0) it WINS over
	// the fraction-of-GOMEMLIMIT default below. Set to the max int64
	// (9223372036854775807) to DISABLE the bound and reproduce the OOM.
	envResolveBudgetBytes = "RESOLVE_BUDGET_BYTES"

	// envResolveBudgetFraction — fraction of GOMEMLIMIT used as the
	// budget when RESOLVE_BUDGET_BYTES is unset. Default 0.5 leaves half
	// the heap headroom for the L1/L3 caches, the informer reflectors and
	// transient GC float that run alongside the resolve burst.
	envResolveBudgetFraction = "RESOLVE_BUDGET_FRACTION"
	defaultResolveBudgetFraction = 0.5

	// defaultResolveBudgetBytes — the FALLBACK absolute budget used when
	// RESOLVE_BUDGET_BYTES is unset AND the Go runtime reports no soft
	// memory limit (debug.SetMemoryLimit(-1) == math.MaxInt64, i.e.
	// GOMEMLIMIT is not set — which is the case on bs-test-ger-03 today:
	// an 8 GiB container limit with NO GOMEMLIMIT env). Without this
	// fallback the fraction-of-MaxInt64 budget would be effectively
	// infinite and the bound would NEVER engage — re-OOMing the pod. We
	// therefore fall back to a conservative absolute budget sized for the
	// 8 GiB pod (4 GiB ≈ 0.5 of the 8 GiB container limit), leaving the
	// other ~4 GiB for caches/informers/runtime. The chart SHOULD also set
	// GOMEMLIMIT to the container limit (recommended in the release notes)
	// so the runtime GC tracks the cgroup; when it does, the
	// fraction×GOMEMLIMIT path takes over and this fallback is unused.
	defaultResolveBudgetBytes int64 = 4 << 30 // 4 GiB

	// envResolveWeightBytesDefault — the measured per-resolve peak heap
	// cost (with a safety multiplier already folded in). This is GATE-1:
	// it is an EMPIRICAL number, not a guess (feedback_capacity_caps_
	// empirical_per_entry_cost — the D.3 180× lesson). See the package
	// ledger row / regression journal for the measurement method.
	envResolveWeightBytesDefault = "RESOLVE_WEIGHT_BYTES_DEFAULT"

	// defaultResolveWeightBytes — measured peak in-use delta of one cold
	// allCompositionResources resolve (~26 .status.managed children + 4
	// nested resolve:true RESTActions, depth-8), captured via the GATE-1
	// in-process harness and cross-checked against the live OOM expvar
	// bracket (~7.5GiB peak / ~5 concurrent ≈ ~1.5GiB/resolve), then
	// multiplied by the 1.5 safety factor. See resolve_budget_test.go
	// (TestResolveBudget_GateOne) for the measurement.
	defaultResolveWeightBytes int64 = 2_250_000_000 // 1.5 GiB measured × 1.5 safety ≈ 2.25 GB
)

var (
	resolveBudgetOnce sync.Once
	resolveBudgetSem  *semaphore.Weighted
	// resolveWeight is the per-acquire weight, clamped to the budget so a
	// single resolve always proceeds even when weight > budget (no
	// deadlock). Resolved once alongside the semaphore.
	resolveWeight int64
)

// resolveBudgetInit lazily computes the budget + weight and constructs
// the process-wide semaphore exactly once. Lazy (not init()) so tests can
// set the env knobs before the first acquire, and so GOMEMLIMIT is read
// after the runtime has applied GOMEMLIMIT/SetMemoryLimit at startup.
func resolveBudgetInit() {
	resolveBudgetOnce.Do(func() {
		budget := resolveBudgetBytes()
		weight := env64(envResolveWeightBytesDefault, defaultResolveWeightBytes)
		if weight < 1 {
			weight = 1
		}
		// Clamp so a single resolve never blocks forever: weight must be
		// acquirable from an empty semaphore (semaphore.Acquire returns
		// an error if n > size, which would fail-closed every request).
		if weight > budget {
			weight = budget
		}
		resolveBudgetSem = semaphore.NewWeighted(budget)
		resolveWeight = weight
	})
}

// resolveBudgetBytes resolves the absolute budget in priority order:
//  1. RESOLVE_BUDGET_BYTES if set (>0) — wins over everything; also the
//     escape-hatch (math.MaxInt64 disables the bound).
//  2. RESOLVE_BUDGET_FRACTION × GOMEMLIMIT, when a soft memory limit IS
//     set (debug.SetMemoryLimit(-1) returns a real, non-MaxInt64 value).
//  3. defaultResolveBudgetBytes — the conservative absolute fallback when
//     no soft limit is set (SetMemoryLimit(-1) == math.MaxInt64, i.e.
//     GOMEMLIMIT unset). This is the bs-test-ger-03 case TODAY; without it
//     the fraction×MaxInt64 budget would be infinite and the bound would
//     never engage. We REFUSE to be infinite by accident.
func resolveBudgetBytes() int64 {
	if abs := env64(envResolveBudgetBytes, 0); abs > 0 {
		return abs
	}
	limit := debug.SetMemoryLimit(-1) // read-only; -1 does not change the limit
	if limit <= 0 || limit == math.MaxInt64 {
		// No soft memory limit configured → don't derive an infinite
		// budget. Fall back to the conservative absolute default.
		return defaultResolveBudgetBytes
	}
	frac := envFloat(envResolveBudgetFraction, defaultResolveBudgetFraction)
	if frac <= 0 {
		frac = defaultResolveBudgetFraction
	}
	b := int64(float64(limit) * frac)
	if b < 1 {
		b = 1
	}
	return b
}

// acquireCustomerResolveBudget blocks until `resolveWeight` bytes of
// budget are available, then returns a release func to be deferred. If ctx
// is cancelled while queued it returns the ctx error (the dispatcher then
// responds 503). The clamp in resolveBudgetInit guarantees resolveWeight
// ≤ budget, so a lone resolve always acquires.
func acquireCustomerResolveBudget(ctx context.Context) (func(), error) {
	resolveBudgetInit()
	w := resolveWeight
	if err := resolveBudgetSem.Acquire(ctx, w); err != nil {
		return nil, err
	}
	return func() { resolveBudgetSem.Release(w) }, nil
}

// env64 reads an int64 env var (absolute byte counts can exceed the int
// range on 32-bit, and we want the explicit MaxInt64 escape-hatch).
func env64(key string, def int64) int64 {
	v := strings.TrimSpace(env.String(key, ""))
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return n
}

// envFloat reads a float64 env var (the budget fraction).
func envFloat(key string, def float64) float64 {
	v := strings.TrimSpace(env.String(key, ""))
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

// maxInt64Budget is the documented escape-hatch sentinel: setting
// RESOLVE_BUDGET_BYTES to this value makes the semaphore effectively
// unbounded, reproducing the pre-fix OOM. Exposed as a named constant so
// the falsifier (b) and the chart docs reference one source of truth.
const maxInt64Budget int64 = math.MaxInt64
