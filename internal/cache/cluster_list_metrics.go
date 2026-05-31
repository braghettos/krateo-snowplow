// cluster_list_metrics.go — Path 3.2 / 0.30.218 — counters for the
// cell-warm vs cold-fallback observability surface introduced by the
// cluster_list collapse mechanism's customer-priority guard.
//
// AC-P3.2.10 anchors on the ABSENCE of `cluster_list.cell.cold_fallback`
// log markers once the system reaches steady state (PIP boot pre-warm
// populated every cell). The counters expose the same evidence
// programmatically for the post-deploy mechanism gate.

package cache

import "sync/atomic"

// clusterListCellWarmTotal counts customer /call paths that hit the
// cell-warm fast-path in attemptClusterListCollapse — sync.Map.Load
// returned the cached apistage entry, customer keeps the cluster-scope
// call, no decode on the customer goroutine.
var clusterListCellWarmTotal atomic.Uint64

// clusterListCellColdFallbackTotal counts customer /call paths that hit
// an unpopulated cluster_list cell — the customer falls back to the
// per-NS iterator path for THAT request and an async populate goroutine
// is scheduled to fill the cell for the NEXT customer.
var clusterListCellColdFallbackTotal atomic.Uint64

// RecordClusterListCellWarm bumps the cell-warm counter. Path 3.2.
func RecordClusterListCellWarm() {
	clusterListCellWarmTotal.Add(1)
}

// RecordClusterListCellColdFallback bumps the cold-fallback counter. Path 3.2.
func RecordClusterListCellColdFallback() {
	clusterListCellColdFallbackTotal.Add(1)
}

// ClusterListCellCounters returns the (warm, cold_fallback) totals for
// post-deploy mechanism inspection. Read-only — used by /debug/vars and
// tests. AC-P3.2.10 falsifier consults this; cold_fallback should be
// zero post-Step-7.5 + 60s settle.
func ClusterListCellCounters() (warm, coldFallback uint64) {
	return clusterListCellWarmTotal.Load(),
		clusterListCellColdFallbackTotal.Load()
}

// ResetClusterListCellCountersForTest zeroes the counters. Test-only —
// production code MUST NOT call this.
func ResetClusterListCellCountersForTest() {
	clusterListCellWarmTotal.Store(0)
	clusterListCellColdFallbackTotal.Store(0)
}
