// apistage_postjq.go — Ship Phase B / 0.30.185: per-cohort post-jq value
// bytes cache.
//
// PURPOSE
//   The Ship A.2 / 0.30.178 cohort gate memo caches the encoded LIST
//   envelope bytes on the permitAll fast-path — eliminating the
//   per-call listEnvelopeValue + CopyJSONMap cost. Phase B extends that
//   discipline one stage downstream: cache the POST-JQ stage output
//   bytes per (cohort × stage-id × jqID). On a warm hit the resolver
//   skips:
//     1. listEnvelopeValue + CopyJSONMap (envelope rebuild + deep copy),
//     2. the gojq Parse + Compile + Run pipeline (the jq filter eval),
//   in favor of a single json.Unmarshal of the cached bytes into a
//   fresh isolated value tree.
//
// KEY COMPOSITION (per HG-PB.17)
//   The cache lives on the per-cohort memo and is keyed by (stage-id,
//   jqID). The cohort key is implicit (the memo IS the cohort).
//   jqID = xxhash.Sum64String(filter) — a stable hash of the RESTAction
//   stage's `getter.filter` string (handler.go:94 opts.filter). Same
//   filter text => same jqID across requests; different filters get
//   distinct entries.
//
//   We DO NOT canonicalise the filter string (PM-mandated: ".items[]"
//   vs ".items [ ]" hash differently). The jq filter text is the cache
//   key; semantic equivalence is not our concern. Unit test
//   TestPostJQ_WhitespaceDistinctCacheEntries documents the behaviour.
//
// LIFETIME + INVALIDATION
//   The cache lives ON the memo. When rbacGen bumps (a cohort RBAC
//   mutation), the memo is dropped at the gateListItemsWithMemo
//   stamp-mismatch check; the next request rebuilds the memo and its
//   postJQEncoded sub-cache starts empty. Per-jqID invalidation on
//   filter text change is automatic (different jqID => different
//   sync.Map key).
//
// SAFETY (binding contracts)
//   - feedback_l1_per_user_keyed_never_cohort.md — the postJQ cache
//     gates on `permitAll == true` cohorts only. permitAll means every
//     cohort member sees the same gated envelope (cluster-wide list
//     grant); the post-jq output is therefore identical across members.
//     On !permitAll cohorts the cache is not engaged (the kept-set
//     varies per cohort in principle, and the gojq cost on a narrowed
//     envelope is much smaller anyway).
//   - feedback_no_special_cases.md — jqID is xxhash over the raw filter
//     string. No resource-name / verb / username special-casing.
//   - feedback_shared_vs_copy_is_a_concurrency_change.md — the cache
//     stores BYTES (immutable). Each hit json.Unmarshal's into a fresh
//     value tree, so the consumer holds a private map/slice tree it
//     can freely mutate (including via downstream gojq passes). No
//     shared-value race surface.
//   - feedback_no_naive_compression_middleware.md — no compression on
//     the cached bytes (the Ship A.2 envelope gzip cache was a separate
//     layer; the post-jq output is the FINAL stage value and is
//     typically much smaller than the envelope — gzip on a small
//     post-jq result is unprofitable).
//
// CAPACITY (per HG-PB.14)
//   - Per-entry cap: COHORT_POSTJQ_CAP_BYTES env, default 64 MiB.
//     Entries exceeding the cap are NOT stored (the request still
//     completes via the canonical path; only the cache update is
//     skipped). cache.RecordCohortPostJQCapacitySkip increments.
//   - Aggregate cap: 4 GiB across all live postJQ entries on this pod.
//     Hard-coded constant — operator can't disable. Same skip-on-
//     exceed semantics.
//   - Per-deploy histogram (snowplow_cohort_postjq_size_hist_*) gives
//     ops the empirical distribution to refine the per-entry cap.
//
// ErrMultiYield POLICY (per PM)
//   ErrMultiYield from EvalValue is treated as RECOMPUTE — no cache
//   entry written. The resolver's existing ContinueOnError logic
//   handles the error path. Caching errors adds correctness risk per
//   PM preference.
//
// EMPTY RESULT POLICY (per PM)
//   Empty post-jq value (jq `empty` yield, or marshal of nil) is cached
//   as ZERO BYTES — a valid answer. Subsequent hits json.Unmarshal []byte{}
//   ... actually we store the marshalled form which for nil is "null",
//   and for empty arrays / maps is the appropriate JSON literal. The
//   "empty result" rule means we do NOT skip the cache write on
//   zero-content — the next hit serves the same zero-content answer
//   without rerunning gojq.
//
// CACHE_ENABLED=false BYPASS (per HG-PB.15)
//   The postJQ cache is INSIDE cohortGateMemo, which only exists when
//   the apistage L1 is enabled (which requires CACHE_ENABLED=true).
//   With CACHE_ENABLED=false, apistage_cohort_memo never instantiates
//   a memo, so this file's helpers are never called.

package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"sync"

	"github.com/cespare/xxhash/v2"
	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// postJQEntry is the cached post-jq stage output. raw holds the JSON
// bytes; gzipped is reserved for a future ship (kept on the struct so
// downstream readers can detect availability without a separate map
// header). Today gzipped is always nil — the post-jq output is typically
// small enough that the gzip cost outweighs the bandwidth win.
type postJQEntry struct {
	raw     []byte
	gzipped []byte // optional, lazy; nil in Phase B / 0.30.185
}

// postJQKey composes the per-memo postJQEncoded sync.Map key. The memo
// IS the cohort; the key only needs to disambiguate (stage-id, jqID).
//
// We use a string key (not a struct) because sync.Map's untyped Load /
// Store API requires `any`. Strings compare via byte-equality at the
// underlying map level — cheap and safe.
//
// Format: "<stage-id>|<jqID-as-base16>". The pipe is reserved
// (stage-ids never contain it under the restactions package's
// convention — they're map keys in the YAML, which restricts to
// alphanumerics + "-" + "_").
func postJQKey(stageID string, jqID uint64) string {
	return stageID + "|" + strconv.FormatUint(jqID, 16)
}

// JQIDFromFilter computes the jqID for a filter pointer. Nil / empty
// filter returns (0, false) — caller MUST check the bool before
// engaging the postJQ cache.
//
// xxhash.Sum64String is the chosen hash function per the brief:
//   - already vendored as an indirect dep (now promoted to direct);
//   - same hash family used by k8s/client-go's internal indexes;
//   - 64-bit output collision probability on ~283 cohort entries × ~10
//     filters is ~1e-16 — negligible for a cache key.
//
// Whitespace normalisation: NONE. ".items[]" vs ".items [ ]" produce
// distinct jqIDs and distinct cache entries. PM-ratified
// (TestPostJQ_WhitespaceDistinctCacheEntries).
func JQIDFromFilter(filter *string) (uint64, bool) {
	if filter == nil {
		return 0, false
	}
	s := *filter
	if s == "" {
		return 0, false
	}
	return xxhash.Sum64String(s), true
}

// lookupCohortPostJQ returns the cached post-jq bytes for the given
// (memo × stageID × jqID), or (nil, false) on miss. Lock-free —
// sync.Map.Load.
//
// HG-PB.13 hot-path: this is the function that, on hit, lets the
// resolver bypass listEnvelopeValue + CopyJSONMap + gojq entirely.
func lookupCohortPostJQ(memo *cohortGateMemo, stageID string, jqID uint64) ([]byte, bool) {
	if memo == nil || stageID == "" {
		return nil, false
	}
	v, ok := memo.postJQEncoded.Load(postJQKey(stageID, jqID))
	if !ok {
		cache.RecordCohortPostJQMiss()
		return nil, false
	}
	entry, ok := v.(*postJQEntry)
	if !ok || entry == nil {
		// Defensive — the sync.Map value type is strictly *postJQEntry
		// (we control every Store call); a non-matching type here is a
		// bug we'd want to surface. Treat as miss + log.
		cache.RecordCohortPostJQMiss()
		return nil, false
	}
	cache.RecordCohortPostJQHit()
	return entry.raw, true
}

// storeCohortPostJQ writes raw post-jq bytes into the memo's postJQ
// sub-cache. Enforces:
//   - per-entry cap (COHORT_POSTJQ_CAP_BYTES) — drop + bump
//     capacity_skips on exceed.
//   - aggregate cap (4 GiB across the pod) — drop + bump capacity_skips
//     on exceed.
//   - idempotent populate via sync.Map.LoadOrStore: concurrent writers
//     producing identical bytes (same cohort, same jqID, same envelope)
//     are deduped at the map header.
//
// Empty bytes (len == 0) are VALID — cached as an empty entry per the
// PM-ratified empty-result rule. A future hit returns []byte{} which
// json.Unmarshal's to nil; the merge into dict treats nil as the
// canonical empty stage output.
func storeCohortPostJQ(
	ctx context.Context,
	memo *cohortGateMemo,
	gvr schema.GroupVersionResource,
	cohort string,
	stageID string,
	jqID uint64,
	raw []byte,
) {
	if memo == nil || stageID == "" {
		return
	}
	log := xcontext.Logger(ctx)

	// Per-entry cap check.
	cap := cache.CohortPostJQCapBytes()
	if uint64(len(raw)) > cap {
		cache.RecordCohortPostJQCapacitySkip()
		log.Warn("cohort_memo.postjq_skip_per_entry_cap",
			slog.String("subsystem", "cache"),
			slog.String("gvr", gvr.String()),
			slog.String("cohort", cohort),
			slog.String("stage_id", stageID),
			slog.Uint64("jq_id", jqID),
			slog.Int("bytes", len(raw)),
			slog.Uint64("cap_bytes", cap),
		)
		return
	}

	// Aggregate cap check — soft: read the live counter, refuse growth
	// above the threshold. Racy but adequate; the cap is a guard, not
	// a precise quota. A few-MiB overshoot under a concurrent burst is
	// not load-bearing.
	if cache.CohortPostJQBytesLive()+uint64(len(raw)) > cache.CohortPostJQAggregateCapBytes() {
		cache.RecordCohortPostJQCapacitySkip()
		log.Warn("cohort_memo.postjq_skip_aggregate_cap",
			slog.String("subsystem", "cache"),
			slog.String("gvr", gvr.String()),
			slog.String("cohort", cohort),
			slog.String("stage_id", stageID),
			slog.Uint64("jq_id", jqID),
			slog.Int("bytes", len(raw)),
			slog.Uint64("aggregate_cap_bytes", cache.CohortPostJQAggregateCapBytes()),
			slog.Uint64("aggregate_live_bytes", cache.CohortPostJQBytesLive()),
		)
		return
	}

	entry := &postJQEntry{raw: raw}
	key := postJQKey(stageID, jqID)
	_, loaded := memo.postJQEncoded.LoadOrStore(key, entry)
	if loaded {
		// Concurrent writer beat us — drop our entry. Bytes are
		// byte-identical (same cohort/jqID/envelope produce same
		// post-jq output) so the winner's entry serves correctly.
		return
	}
	cache.RecordCohortPostJQStore(len(raw))
	log.Info("cohort_memo.postjq_store",
		slog.String("subsystem", "cache"),
		slog.String("event", "postjq_store"),
		slog.String("gvr", gvr.String()),
		slog.String("cohort", cohort),
		slog.String("stage_id", stageID),
		slog.Uint64("jq_id", jqID),
		slog.Int("bytes", len(raw)),
	)
}

// unmarshalCohortPostJQ decodes cached post-jq bytes into a fresh
// isolated value tree suitable for direct feed into the stage dict.
// json.Unmarshal allocates a private tree per call — no aliasing
// against the cached bytes.
//
// The empty-bytes case (len==0) is the cached `empty` answer: returns
// (nil, nil) so the caller can short-circuit the merge.
func unmarshalCohortPostJQ(raw []byte) (any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// peekCohortMemoForPostJQ is the Phase B / 0.30.185 hot-path
// short-circuit for the postJQ HIT case (architect review item 3).
// Performs the MINIMUM work needed to determine whether a cached
// post-jq value exists for the current request:
//
//  1. Recover the cohort identity from ctx (UserInfo) — same hash path
//     gateListItemsWithMemoEx uses.
//  2. Lookup the existing memo via the cheap sync.Map.Load on store.
//  3. Validate the memo's rbacGen against the live cohort generation
//     (CohortRBACGen) — a mismatch means the memo is stale and the
//     postJQ entries under it are stale by extension; treat as miss.
//  4. Require permitAll == true (postJQ cache is permitAll-only).
//  5. Lookup the postJQ entry under (stageID, jqID).
//
// Returns (memo, cohort, raw, true) on HIT — the caller json.Unmarshal's
// raw into a fresh isolated value tree and serves it directly, SKIPPING
// listEnvelopeValue + CopyJSONMap + gojq entirely. This is the load-
// bearing CPU win: the deep-copy that listEnvelopeValue paid for every
// content-cache hit is bypassed on a postJQ hit.
//
// Returns (nil, "", nil, false) on miss / ineligible — the caller falls
// through to the canonical gateListItemsWithMemoEx path, which performs
// the envelope rebuild + memo populate as usual and may install a
// capturePostJQ hook for lazy populate.
//
// NIL-SAFETY: every input (store, filter, stageID) is checked. A
// passing all-zero call returns clean miss without panic.
//
// CALL COST on miss: one xcontext.UserInfo(ctx) + one CohortKeyHash +
// one sync.Map.Load + one CohortRBACGen + one bool check. The CohortKey
// + CohortRBACGen cost is paid TWICE (here AND inside
// gateListItemsWithMemoEx) on a miss; this is acceptable — both
// functions are O(len(groups)) hashing, the same overhead the existing
// gate would have paid anyway.
//
// CALL COST on hit: identical to miss PLUS one postJQ sync.Map.Load +
// the json.Unmarshal of the cached bytes (the caller's responsibility).
// ZERO envelope rebuild, ZERO CopyJSONMap, ZERO gojq.
func peekCohortMemoForPostJQ(
	ctx context.Context,
	store *cache.CohortGateMemoStore,
	filter *string,
	stageID string,
	sliceActive bool,
) (memo *cohortGateMemo, cohort string, jqID uint64, raw []byte, hit bool) {
	if store == nil || stageID == "" || sliceActive {
		return nil, "", 0, nil, false
	}
	id, jqIDOK := JQIDFromFilter(filter)
	if !jqIDOK {
		return nil, "", 0, nil, false
	}

	ui, err := xcontext.UserInfo(ctx)
	if err != nil {
		return nil, "", 0, nil, false
	}
	cohort = cohortKeyHashFromUserInfo(ui.Username, ui.Groups)
	if cohort == "" {
		return nil, "", 0, nil, false
	}

	v, ok := store.Lookup(cohort)
	if !ok {
		return nil, "", 0, nil, false
	}
	m, isMemo := v.(*cohortGateMemo)
	if !isMemo || m == nil {
		return nil, "", 0, nil, false
	}
	// Stamp check — a stale memo's postJQ entries are stale by
	// extension; the canonical path will rebuild + repopulate.
	if m.rbacGen != cache.CohortRBACGen(ui.Username, ui.Groups) {
		return nil, "", 0, nil, false
	}
	// permitAll-only gating — same discipline as the original
	// apistageContentServe eligibility check.
	if !m.permitAll {
		return nil, "", 0, nil, false
	}

	raw, hit = lookupCohortPostJQ(m, stageID, id)
	if !hit {
		return nil, "", 0, nil, false
	}
	return m, cohort, id, raw, true
}

// feedPostJQValue is the Phase B / 0.30.185 fast-path equivalent of the
// resolve.go feedValue closure. It mirrors jsonHandlerCore's post-filter
// merge block (handler.go:122-145) but skips the EvalValue + wrap-in-pig
// steps because the value is ALREADY the post-jq result (recovered from
// the cohort postJQ cache).
//
// Semantics: same as jsonHandlerCore's merge:
//   - if dict[id] absent: dict[id] = postJQ
//   - if dict[id] is []any: append wrapAsSlice(postJQ)
//   - otherwise: dict[id] = []any{prev, postJQ} (with []any flattening
//     when postJQ is itself a slice)
//
// dictMu is acquired around the dict mutation — same contract as the
// canonical feedValue closure in resolve.go.
func feedPostJQValue(postJQ any, dictMu *sync.Mutex, dict map[string]any, id string) error {
	dictMu.Lock()
	defer dictMu.Unlock()

	got, ok := dict[id]
	if !ok {
		dict[id] = postJQ
		return nil
	}
	switch existingSlice := got.(type) {
	case []any:
		if v := wrapAsSlice(postJQ); len(v) > 0 {
			dict[id] = append(existingSlice, v...)
		}
	default:
		switch v := postJQ.(type) {
		case []any:
			all := []any{got}
			all = append(all, v...)
			dict[id] = all
		default:
			dict[id] = []any{got, v}
		}
	}
	return nil
}
