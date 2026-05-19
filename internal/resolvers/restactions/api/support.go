package api

import (
	"context"
	"log/slog"
	"sync"
)

// depthSentinelNotComputed is the value the "depth" log field carries
// when mapDepth was NOT run — i.e. on the common (non-debug) path where
// Ship #6 gates the recursive full-tree walk out. A negative sentinel
// is unambiguously "not measured" (a real depth is always >= 0), so a
// log reader never mistakes it for a genuine depth.
const depthSentinelNotComputed = -1

// depthForLog returns mapDepth(dict) for the "api successfully resolved"
// log line's `depth` field — but ONLY when the logger's Debug level is
// enabled. Ship #6 (0.30.136).
//
// THE FIX: mapDepth is a recursive full-tree walk of the resolved dict;
// the resolver called it on EVERY /call solely to populate one log
// field, costing ~23% of pod CPU. The "api successfully resolved" line
// emits at Info, but `depth` is a diagnostic detail that belongs at
// Debug — so the walk runs only when Debug is enabled (the pod default
// is Info; Debug is opt-in via --debug, main.go). On the common path
// depthForLog returns the sentinel and does NO work; the Info line
// still emits with all its other fields. The diagnostic is DEFERRED,
// not lost (AC-2): under --debug the walk runs and `depth` is exact.
//
// CONCURRENCY (AC-3 — gating a lock is a concurrency change,
// feedback_shared_vs_copy_is_a_concurrency_change): the dictMu lock is
// gated out TOGETHER WITH mapDepth. On the non-debug path neither the
// walk nor the Lock/Unlock runs — there is no call to protect. On the
// debug path mapDepth runs UNDER mu exactly as before, serialising the
// full-tree read against concurrent jsonHandler writes to dict. The
// caller passes the same *sync.Mutex (dictMu) that guards dict.
func depthForLog(ctx context.Context, log *slog.Logger, mu *sync.Mutex, dict any) int {
	if log == nil || !log.Enabled(ctx, slog.LevelDebug) {
		return depthSentinelNotComputed
	}
	mu.Lock()
	d := mapDepth(dict)
	mu.Unlock()
	return d
}

func mapDepth(data any) int {
	switch v := data.(type) {
	case map[string]any:
		maxDepth := 1
		for _, val := range v {
			d := mapDepth(val) + 1
			if d > maxDepth {
				maxDepth = d
			}
		}
		return maxDepth
	case []any:
		maxDepth := 0
		for _, elem := range v {
			d := mapDepth(elem)
			if d > maxDepth {
				maxDepth = d
			}
		}
		return maxDepth
	default:
		return 0
	}
}
