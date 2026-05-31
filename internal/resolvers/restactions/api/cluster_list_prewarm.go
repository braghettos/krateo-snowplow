// cluster_list_prewarm.go — Path 3.2 / 0.30.218 — PIP boot pre-warm
// for the cluster_list cell roster.
//
// At pod start, BEFORE MarkPhase1Done flips /readyz to 200, the
// dispatcher subsystem walks every harvested RESTAction, finds every
// stage with a per-NS iterator (DependsOn.Iterator non-empty +
// ns-scoped target GVR), dedupes by GVR, and populates the apistage L1
// cell for each one in parallel under SA identity. This eliminates the
// first-customer cold-fallback path: every customer /call after boot
// hits a warm cell.
//
// CELL ROSTER cardinality: ~10-15 distinct GVRs at 50K prod scale
// (cluster_list-eligible RAs across portal + krateo internal — the
// per-namespace iterators in compositions / panels / configmaps /
// widgets / apirefs etc.). Each cell costs ~50-2,000ms to populate
// (envelope-size-dependent). Total wall-clock with GOMAXPROCS=8
// parallelism: ~5s on the empirical 0.30.217 measurements.
//
// TIMEOUT: 60s hard cap. If the pre-warm overruns, MarkPhase1Done
// still fires (readiness not blocked); cold-fallback covers the gap
// until cells warm in the background.

package api

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/krateoplatformops/plumbing/endpoints"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// ClusterListPrewarmTimeout is the hard ceiling on Step 7.5 wall-clock.
// If pre-warm exceeds this, the caller proceeds anyway (MarkPhase1Done
// fires); per-NS iterator fallback covers the gap until cells warm via
// the cold-fallback async populate path. Exported so phase1 callers can
// reference it. Path 3.2 / 0.30.218.
const ClusterListPrewarmTimeout = 60 * time.Second

// ClusterListCellRoster enumerates the (apiCall, GVR) tuples that need
// pre-warm. Built by EnumerateClusterListCells; consumed by
// PrewarmClusterListCells.
type ClusterListCellRoster struct {
	Cells []ClusterListCellEntry
}

// ClusterListCellEntry is one cell in the roster — a single
// (apiCall, target GVR) tuple. The apiCall is needed for clusterCall
// construction (path template + endpoint resolution); the GVR is the
// key the cell is stored under.
type ClusterListCellEntry struct {
	APICall *templates.API
	GVR     schema.GroupVersionResource
}

// EnumerateClusterListCells walks every RESTAction in restActions, finds
// every stage with a per-NS iterator that derives to a ns-scoped target
// GVR, and returns the deduped (apiCall, GVR) roster. Identity-free —
// the cluster_list cell is shared across all cohorts (apistage L1 is
// identity-free per ResolvedKeyInputs CacheEntryClassApistage contract).
//
// ctx: SA-identity context with the SA endpoint + REST config attached
// (so deriveTargetGVRForClusterList's jq evaluation runs under SA
// scope). The pre-warm caller (phase1_clusterlist_prewarm.go in
// dispatchers) sets this up via withContentPrewarmSAContext or
// equivalent.
//
// Dedup: keyed on gvr.String(). Two RAs targeting the same GVR yield
// ONE roster entry (the first encountered apiCall wins; the GVR is the
// load-bearing field — apiCall is just the path-template carrier and
// the path is identical across same-GVR iterators by construction).
//
// Returns an empty roster on nil/empty input. Errors during per-RA
// inspection are LOGGED, NOT FATAL — one broken RA must not block
// the rest of the roster from pre-warm.
func EnumerateClusterListCells(
	ctx context.Context,
	log *slog.Logger,
	restActions []*templates.RESTAction,
) ClusterListCellRoster {
	roster := ClusterListCellRoster{}
	if len(restActions) == 0 {
		return roster
	}
	seenGVR := map[string]bool{}
	for _, ra := range restActions {
		if ra == nil {
			continue
		}
		for _, apiCall := range ra.Spec.API {
			if apiCall == nil {
				continue
			}
			if apiCall.DependsOn == nil || apiCall.DependsOn.Iterator == nil ||
				*apiCall.DependsOn.Iterator == "" {
				continue
			}
			// Build an empty dict for jq probe — the iterator's first
			// element shape is RA-specific but the GVR derivation
			// only consults the parsed apiserver path, which is a
			// constant string template after resolution. We invoke
			// deriveTargetGVRForClusterList with an empty dict; if
			// the iterator's template references parent-stage output
			// that isn't yet populated, the probe returns false and
			// we skip this entry. (Production refresher path will
			// populate it lazily via the cold-miss async populate.)
			//
			// IMPORTANT: pre-warm enumeration MUST tolerate iterator
			// templates whose first-element shape depends on parent
			// stages — those entries simply skip pre-warm. The
			// cold-miss fast-path handles them at first customer
			// touch.
			gvr, ok := deriveTargetGVRForClusterList(ctx, log, apiCall, map[string]any{})
			if !ok {
				continue
			}
			key := gvr.String()
			if seenGVR[key] {
				continue
			}
			seenGVR[key] = true
			roster.Cells = append(roster.Cells, ClusterListCellEntry{
				APICall: apiCall,
				GVR:     gvr,
			})
		}
	}
	return roster
}

// PrewarmClusterListCells populates every cell in the roster in
// parallel (bounded by parallelism arg). Each cell is populated via
// populateClusterListCellSync. Returns the count of successfully
// populated cells + total cells attempted (so the caller can log
// "warmed X of Y").
//
// ctx MUST carry the SA endpoint + REST config + a deadline (the
// 60s timeout cap, set by the caller via context.WithTimeout). On
// deadline exceeded, the errgroup returns and partially-populated
// state is acceptable — the cold-fallback path covers any unwarmed
// cell on first customer touch.
//
// parallelism is typically runtime.GOMAXPROCS(0); the same bound the
// PIP cohort errgroup uses.
//
// ep is the SA endpoint passed through to buildClusterListCall. NOT
// per-cohort (cluster_list cells are identity-free).
//
// apistageStore is the L1 store the populated entries are Put into.
// Required (panics on nil — production wires it from
// cache.ResolvedCache()).
//
// Returns (populated, attempted, err). err is set to ctx.Err() ONLY on
// deadline exceeded — per-cell failures are logged + counted in
// `attempted - populated` but NOT propagated (one broken cell must not
// abort the rest).
func PrewarmClusterListCells(
	ctx context.Context,
	log *slog.Logger,
	roster ClusterListCellRoster,
	ep endpoints.Endpoint,
	apistageStore *cache.ResolvedCacheStore,
	parallelism int,
) (populated int, attempted int, err error) {
	if apistageStore == nil {
		log.Warn("cluster_list.prewarm.skipped",
			slog.String("subsystem", "cache"),
			slog.String("reason", "nil apistage store — cache off?"),
		)
		return 0, 0, nil
	}
	if len(roster.Cells) == 0 {
		log.Info("cluster_list.prewarm.empty_roster",
			slog.String("subsystem", "cache"),
			slog.String("hint", "no iterator stages with ns-scope target GVR — nothing to pre-warm"),
		)
		return 0, 0, nil
	}

	if parallelism <= 0 {
		parallelism = 4
	}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(parallelism)

	var mu sync.Mutex
	popOK := 0

	for i := range roster.Cells {
		cell := roster.Cells[i] // capture loop var for closure
		g.Go(func() error {
			// Honor deadline.
			if gctx.Err() != nil {
				return gctx.Err()
			}
			contentKey := cache.ComputeKey(contentKeyInputs(cell.GVR, "", ""))
			// If the cell is already warm (from a prior boot's
			// refresher OR from a concurrent populate), skip.
			if existing, hit := apistageStore.Get(contentKey); hit && existing != nil {
				mu.Lock()
				popOK++
				mu.Unlock()
				// Register in cluster_list tier so future
				// dirty-marks route to the high-priority queue.
				cache.RegisterClusterListKey(contentKey)
				return nil
			}
			// Build the cluster-scope call.
			clusterCall := buildClusterListCall(cell.APICall, ep, cell.GVR)

			if populateClusterListCellSync(gctx, log, cell.APICall, cell.GVR, contentKey, clusterCall, apistageStore) {
				mu.Lock()
				popOK++
				mu.Unlock()
			}
			return nil // never propagate per-cell errors
		})
	}

	err = g.Wait()
	if err == context.DeadlineExceeded || err == context.Canceled {
		log.Warn("cluster_list.prewarm.timeout",
			slog.String("subsystem", "cache"),
			slog.Int("populated", popOK),
			slog.Int("attempted", len(roster.Cells)),
			slog.Any("err", err),
			slog.String("effect", "MarkPhase1Done fires regardless; remaining cells lazy-warm via cold-fallback async populate"),
		)
	}
	return popOK, len(roster.Cells), err
}
