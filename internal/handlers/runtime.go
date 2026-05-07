package handlers

import (
	"encoding/json"
	"net/http"
	"runtime"

	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/handlers/dispatchers"
)

// RuntimeMetrics holds the JSON structure returned by /metrics/runtime.
type RuntimeMetrics struct {
	HeapAllocMB    float64          `json:"heap_alloc_mb"`
	HeapSysMB      float64          `json:"heap_sys_mb"`
	GoroutineCount int              `json:"goroutine_count"`
	NumGC          uint32           `json:"num_gc"`
	ActiveUsers    int              `json:"active_users"`
	CacheKeyCount  int64            `json:"cache_key_count"`
	ClusterDep     ClusterDepInfo   `json:"cluster_dep"`
	WatchEvents    WatchEventsInfo  `json:"watch_events"`
	WorkQueues     WorkQueuesInfo   `json:"work_queues"`
	L2             L2Info           `json:"l2"`
	Prewarm        *PrewarmInfo     `json:"prewarm,omitempty"`
}

// PrewarmInfo exposes the heap-alloc trajectory of the most recent
// WarmL1FromEntryPoints run (Lever A peak-alloc instrumentation,
// Q-COLD-1 PM gate G3, 2026-05-07). Nil when prewarm has not yet
// completed. All sizes in MB, duration in ms.
type PrewarmInfo struct {
	HeapStartMB float64 `json:"heap_start_mb"`
	HeapPeakMB  float64 `json:"heap_peak_mb"`
	HeapEndMB   float64 `json:"heap_end_mb"`
	HeapDeltaMB float64 `json:"heap_delta_mb"`
	DurationMs  int64   `json:"duration_ms"`
}

// WorkQueueLens is the read-side observability surface of the priority
// workqueue (HOT > WARM > COLD). Implemented by *cache.ResourceWatcher.
type WorkQueueLens interface {
	HotQueueLen() int
	WarmQueueLen() int
	ColdQueueLen() int
}

// WorkQueuesInfo exposes the current depth of the three L1 refresh
// priority queues. Non-zero hot_len under load indicates HOT-tier
// worker saturation (architect report 2026-05-02 §6/§7 #1).
type WorkQueuesInfo struct {
	HotLen  int `json:"hot_len"`
	WarmLen int `json:"warm_len"`
	ColdLen int `json:"cold_len"`
}

// WatchEventsInfo exposes informer event delivery counters. DeleteTombstone
// is the smoking-gun signal: when non-zero, the reflector synthesized a
// Delete via relist because the original watch event was lost.
type WatchEventsInfo struct {
	Add             int64 `json:"add"`
	Update          int64 `json:"update"`
	Delete          int64 `json:"delete"`
	DeleteTombstone int64 `json:"delete_tombstone"`
}

// L2Info exposes the L2 post-refilter cache (Q-RBACC-L2-1) counters at
// /metrics/runtime so canary observers can compute hit ratio + budget
// utilisation without needing /metrics/cache. All fields are sampled
// from cache.GlobalMetrics.Snapshot() (atomic loads) — safe under
// concurrent /metrics/runtime requests.
//
// Hits/Misses/Writes/Skipped/Evictions are monotonic counters (compute
// rate via successive samples). HitRate is the cumulative percentage
// (0–100) computed in the snapshot. ResidentBytes/Count are gauge
// snapshots of the current L2 budget consumption.
type L2Info struct {
	Hits                int64   `json:"hits"`
	Misses              int64   `json:"misses"`
	Writes              int64   `json:"writes"`
	SkippedHighRatio    int64   `json:"skipped_high_ratio"`
	SkippedSizeCap      int64   `json:"skipped_size_cap"`
	EvictionsL1Delete   int64   `json:"evictions_l1_delete"`
	EvictionsIdentity   int64   `json:"evictions_identity"`
	EvictionsRA         int64   `json:"evictions_ra"`
	EvictionsTotal      int64   `json:"evictions_total"`
	HitRate             float64 `json:"hit_rate"`
	ResidentBytes       int64   `json:"resident_bytes"`
	EntryCount          int64   `json:"entry_count"`
}

// ClusterDepInfo mirrors the cluster-wide dep instrumentation counters from
// MetricsSnapshot. The two *_per_namespace_list_* fields drive the Option A
// go/no-go signal (W_ns / W headline ratio per design doc §3.4).
type ClusterDepInfo struct {
	SAddTotal                 int64   `json:"sadd_total"`
	SAddByResolve             int64   `json:"sadd_by_resolve"`
	SAddByResolveNSList       int64   `json:"writes_per_namespace_list_resolve"`
	SAddByRegister            int64   `json:"sadd_by_register"`
	SAddByRegisterNSList      int64   `json:"writes_per_namespace_list_register"`
	SAddDeduped               int64   `json:"sadd_deduped"`
	SetSizeMax                int64   `json:"set_size_max"`
	SetSizeAvg                float64 `json:"set_size_avg"`
	SMembersTotal             int64   `json:"smembers_total"`
	SMembersBytes             int64   `json:"smembers_bytes"`
	WritesPerNamespaceListSum int64   `json:"writes_per_namespace_list"`
}

// RuntimeMetricsHandler returns an http.Handler that serves /metrics/runtime.
// It collects Go runtime stats, active user count, and total cache key count.
// queues may be nil before the ResourceWatcher is wired in startBackgroundServices.
func RuntimeMetricsHandler(c cache.Cache, queues WorkQueueLens) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)

		ctx := r.Context()

		activeUsers := 0
		if c != nil {
			members, err := c.SMembers(ctx, cache.ActiveUsersKey)
			if err == nil {
				activeUsers = len(members)
			}
		}

		var redisKeyCount int64
		if c != nil {
			redisKeyCount = c.DBSize(ctx)
		}

		var wqInfo WorkQueuesInfo
		if queues != nil {
			wqInfo = WorkQueuesInfo{
				HotLen:  queues.HotQueueLen(),
				WarmLen: queues.WarmQueueLen(),
				ColdLen: queues.ColdQueueLen(),
			}
		}

		snap := cache.GlobalMetrics.Snapshot()

		// Lever A peak-alloc instrumentation (Q-COLD-1 PM gate G3).
		var prewarmInfo *PrewarmInfo
		if ps := dispatchers.LoadPrewarmHeapStats(); ps != nil {
			deltaBytes := float64(int64(ps.HeapPeakBytes) - int64(ps.HeapStartBytes))
			prewarmInfo = &PrewarmInfo{
				HeapStartMB: float64(ps.HeapStartBytes) / (1024 * 1024),
				HeapPeakMB:  float64(ps.HeapPeakBytes) / (1024 * 1024),
				HeapEndMB:   float64(ps.HeapEndBytes) / (1024 * 1024),
				HeapDeltaMB: deltaBytes / (1024 * 1024),
				DurationMs:  ps.DurationMs,
			}
		}

		m := RuntimeMetrics{
			HeapAllocMB:    float64(ms.HeapAlloc) / (1024 * 1024),
			HeapSysMB:      float64(ms.HeapSys) / (1024 * 1024),
			GoroutineCount: runtime.NumGoroutine(),
			NumGC:          ms.NumGC,
			ActiveUsers:    activeUsers,
			CacheKeyCount:  redisKeyCount,
			ClusterDep: ClusterDepInfo{
				SAddTotal:                 snap.ClusterDepSAddTotal,
				SAddByResolve:             snap.ClusterDepSAddByResolve,
				SAddByResolveNSList:       snap.ClusterDepSAddByResolveNSList,
				SAddByRegister:            snap.ClusterDepSAddByRegister,
				SAddByRegisterNSList:      snap.ClusterDepSAddByRegisterNSList,
				SAddDeduped:               snap.ClusterDepSAddDeduped,
				SetSizeMax:                snap.ClusterDepSetSizeMax,
				SetSizeAvg:                snap.ClusterDepSetSizeAvg,
				SMembersTotal:             snap.ClusterDepSMembersTotal,
				SMembersBytes:             snap.ClusterDepSMembersBytes,
				WritesPerNamespaceListSum: snap.ClusterDepSAddByResolveNSList + snap.ClusterDepSAddByRegisterNSList,
			},
			WatchEvents: WatchEventsInfo{
				Add:             snap.WatchEventsAdd,
				Update:          snap.WatchEventsUpdate,
				Delete:          snap.WatchEventsDelete,
				DeleteTombstone: snap.WatchEventsDeleteTombstone,
			},
			WorkQueues: wqInfo,
			L2: L2Info{
				Hits:              snap.L2Hits,
				Misses:            snap.L2Misses,
				Writes:            snap.L2Writes,
				SkippedHighRatio:  snap.L2SkippedHighRatio,
				SkippedSizeCap:    snap.L2SkippedSizeCap,
				EvictionsL1Delete: snap.L2EvictionsL1Delete,
				EvictionsIdentity: snap.L2EvictionsIdentity,
				EvictionsRA:       snap.L2EvictionsRA,
				EvictionsTotal:    snap.L2EvictionsTotal,
				HitRate:           snap.L2HitRate,
				ResidentBytes:     snap.L2ResidentBytes,
				EntryCount:        snap.L2ResidentCount,
			},
			Prewarm: prewarmInfo,
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(m)
	})
}
