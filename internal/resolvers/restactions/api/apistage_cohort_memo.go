// apistage_cohort_memo.go — Ship GMC / 0.30.174, A.2 / 0.30.178,
// Phase B / 0.30.185.
//
// PROBLEM
//   filterListByRBAC at apistage.go:176 runs per-item EvaluateRBAC over
//   every item of every content-Get hit. Two requests for the SAME LIST
//   from two users in the SAME RBAC cohort (identical Groups set +
//   identical Username admit rules) compute identical kept sets — yet
//   the resolver pays the per-item EvaluateRBAC fan-out on EACH request.
//   At admin-cohort scale (10K-50K compositions) the work dominates the
//   cold path.
//
// PHASE B / 0.30.185 EXTENSION
//   Even with the Ship A.2 permitAll envelope cache, the resolver still
//   pays the per-call EvalValue (gojq) + listEnvelopeValue + CopyJSONMap
//   cost on every warm /call. The pre-flight cyberjoker-cold-pprof-cpu
//   capture shows ~30-40% of cold-CPU concentrated in those three sites
//   for the admin/cyberjoker LIST stages. Phase B extends cohortGateMemo
//   with a `postJQEncoded` sub-cache keyed by (stage-id × jqID) — where
//   jqID = xxhash.Sum64String(filter). On a HIT, the cached post-jq
//   bytes are json.Unmarshal'd into a fresh isolated value tree and fed
//   direct into the stage dict, skipping gojq + listEnvelopeValue +
//   CopyJSONMap entirely. Population is LAZY: the first request for a
//   given (cohort, stage-id, jqID) runs the canonical path and captures
//   the post-jq value via a single-line hook in jsonHandlerCore.
//
// SOLUTION (Ship GMC / 0.30.174 + A.2 / 0.30.178)
//   Memoize the kept-name set per (content-entry × cohort). Cohort is
//   keyed by hash(sorted(Groups) || Username) — stable across requests
//   from the same identity. The memo stamps the RBAC publish generation
//   (cache.CohortRBACGen, Ship GMC.1) at write time; a stale memo (gen
//   mismatch) is discarded and re-filtered.
//
//   Ship A.2 / 0.30.178 extends the memo with a COHORT NAMESPACE ACL
//   pre-index (cache.CohortNSACL) that classifies the cohort's verdict
//   on the LIST at populate time:
//
//     - permitAll == true   — cluster-wide list grant; every item kept.
//                              ALSO populates encodedJSON + encodedGzip
//                              for zero-CPU serve on hit.
//     - permitAll == false  — RoleBinding-only grants; keptNames is
//                              filtered by the binding namespaces (no
//                              per-item EvaluateRBAC, just a map lookup
//                              per item against permittedNS).
//     - permitAll == false, permittedNS == empty — cohort cannot list;
//                              keptNames is empty.
//
//   On hit: walk entry.Items, keep items whose "namespace/name" is in
//   the memo's keptNames set, skip filterListByRBAC entirely.
//   On miss / stale: run the CohortNSACL fast-path; populate the memo.
//
// SAFETY (binding contracts)
//   - feedback_l1_per_user_keyed_never_cohort.md — the L1 RESOLVED
//     cache stays per-user-keyed. This memo is a per-cohort SHORTCUT
//     over the per-item RBAC filter; cohorts admit the SAME rule set
//     by construction (same Groups + Username admit the same RBAC
//     bindings), so the kept set is identical. We never store a
//     resolved-output body under a cohort key.
//   - feedback_no_special_cases.md — uniform over every GVR; never
//     references a specific resource by name. CohortNSACL is generic
//     over RBAC rules; no hardcoded admin / cluster-admin / system:masters
//     fast-paths.
//   - feedback_shared_vs_copy_is_a_concurrency_change.md — keptNames,
//     encodedJSON, encodedGzip are BUILT ONCE on memo populate then
//     READ-ONLY for every hit. The cache.CohortGateMemoStore guards
//     concurrent populate/read with a sync.Map; the memo struct itself
//     is never mutated post-store.
//   - feedback_no_naive_compression_middleware.md — gzip compresses
//     ONCE on memo populate (permitAll path) and stores the bytes in
//     the memo. Warm reads can be served from pre-compressed bytes —
//     zero compression CPU on hits. NOT a response-time middleware.
//
// FAIL-CLOSED
//   - Missing UserInfo / no identity => memo bypass + the underlying
//     filterListByRBAC returns served=false. Caller falls through to
//     the apiserver. No memo write on identity failure.
//   - Missing RBAC snapshot (pre-readiness) => CohortNSACL returns
//     (false, nil) → falls through to the canonical filterListByRBAC,
//     which itself fails closed on a missing snapshot.
//   - identity-OK memo writes use ONLY the items the ACL+filter
//     produced — never a partial / un-gated set.

package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"log/slog"
	"sync"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// cohortGateMemo is the per-(content-entry × cohort) memoized output of
// filterListByRBAC: the set of "namespace/name" keys this cohort can
// list, stamped with the rbacGen the memo was built against.
//
// Ship A.2 / 0.30.178 extends the memo with the CohortNSACL verdict
// (permitAll) + zero-bound encoded-bytes cache (encodedJSON,
// encodedGzip) populated only on the permitAll fast-path. Per-cohort
// variability is too high on the !permitAll branch to make encoded-
// bytes caching profitable; the keptNames + per-item map lookup serves
// those cohorts.
//
// The whole struct is BUILT ONCE on populate then NEVER mutated. Hit
// readers consume it lock-free; the cache.CohortGateMemoStore guards
// the (cohortKey -> *cohortGateMemo) lookup with sync.Map.
//
// PHASE B / 0.30.185: postJQEncoded is the EXCEPTION to the struct-
// immutability contract — it is a sync.Map populated LAZILY on the FIRST
// request per (stage-id, jqID), then read by every subsequent request.
// The struct itself is never reassigned (the sync.Map header is set
// once at populate); only the map contents grow over the memo's lifetime.
// Concurrent populate is dedupe'd by sync.Map.LoadOrStore — multiple
// goroutines computing the same (stage-id, jqID) write byte-identical
// payloads, so the LoadOrStore winner's bytes are correct for every
// reader.
type cohortGateMemo struct {
	rbacGen uint64

	// permitAll captures the CohortNSACL verdict for this (entry × cohort).
	// true: cluster-wide list grant — every item is kept; the memo also
	// populates encodedJSON + encodedGzip.
	// false: namespace-scoped grants only — keptNames is the filter.
	permitAll bool

	// keptNames is populated ONLY when !permitAll. "ns/name" keys this
	// cohort can list. A nil/empty map means "no items kept" — the
	// caller's range-over-nil walk yields an empty served envelope.
	keptNames map[string]struct{}

	// encodedJSON, encodedGzip are populated ONLY when permitAll. The
	// encoded LIST envelope (apiVersion + kind + items) and its gzip
	// transcoding, computed ONCE at populate time. Zero-bound (no size
	// threshold). Hit readers serve these directly without paying the
	// per-call listEnvelopeValue + DeepCopyJSON cost.
	encodedJSON []byte
	encodedGzip []byte

	// postJQEncoded is the Phase B / 0.30.185 lazy sub-cache for post-jq
	// stage output bytes. Keyed by postJQKey (stage-id || jqID), value
	// is *postJQEntry holding the JSON-encoded post-jq bytes. The map
	// header itself is built ONCE at memo populate (here); ENTRIES are
	// populated lazily by jsonHandlerCore's onPostJQ hook the FIRST time
	// a request with the matching filter runs against the cohort.
	//
	// Concurrent populate is dedupe'd by sync.Map.LoadOrStore — multiple
	// goroutines marshalling the same post-jq value write identical
	// bytes. The first writer wins; losers discard their bytes.
	//
	// Gated by the permitAll fast-path. On !permitAll cohorts the post-
	// jq output varies per cohort but the canonical filterListByRBAC
	// path already produces correct bytes; the cache is not engaged.
	// See apistage_postjq.go for the lookup/store helpers and the
	// (cohort, jqID, stage-id) key composition.
	postJQEncoded sync.Map // postJQKey -> *postJQEntry
}

// cohortKeyHashFromUserInfo is a thin shim over cache.CohortKeyHash so
// in-package callers keep their existing name. Ship GMC.1 / 0.30.175
// moved the canonical implementation to cache/ so the per-cohort gate
// memo (here) and the per-cohort RBAC generator (cache.CohortRBACGen)
// compute byte-identical cohort keys for the same identity.
func cohortKeyHashFromUserInfo(username string, groups []string) string {
	return cache.CohortKeyHash(username, groups)
}

// gateListItemsWithMemoEx is the Phase B / 0.30.185 super-set of
// gateListItemsWithMemo: same gating behaviour, but additionally
// returns the *cohortGateMemo + cohort key on a hit or just-populated
// miss so the caller can plumb the post-jq sub-cache (apistage_postjq.go).
// On nil-store / no-identity / fail-closed branches, memo + cohort are
// zero/nil.
//
// gateListItemsWithMemo is the original (memo-less) entry point retained
// for non-postJQ callers (the refresh path, tests).
func gateListItemsWithMemoEx(
	ctx context.Context,
	store *cache.CohortGateMemoStore,
	gvr schema.GroupVersionResource,
	parsed parsedListEnvelope,
) (any, bool, *cohortGateMemo, string) {
	if store == nil {
		v, ok := gateListItems(ctx, gvr, parsed)
		return v, ok, nil, ""
	}

	ui, err := xcontext.UserInfo(ctx)
	if err != nil {
		v, ok := gateListItems(ctx, gvr, parsed)
		return v, ok, nil, ""
	}
	cohort := cohortKeyHashFromUserInfo(ui.Username, ui.Groups)
	if cohort == "" {
		v, ok := gateListItems(ctx, gvr, parsed)
		return v, ok, nil, ""
	}

	currentGen := cache.CohortRBACGen(ui.Username, ui.Groups)

	if v, ok := store.Lookup(cohort); ok {
		if memo, isMemo := v.(*cohortGateMemo); isMemo && memo != nil && memo.rbacGen == currentGen {
			return cohortGateMemoServe(ctx, gvr, parsed, memo, cohort), true, memo, cohort
		}
	}

	memo, served := populateCohortGateMemo(ctx, gvr, parsed, ui.Username, ui.Groups, cohort, currentGen)
	if !served {
		return nil, false, nil, ""
	}
	store.Store(cohort, memo)

	log := xcontext.Logger(ctx)
	log.Debug("apistage.cohort_gate_memo.store",
		slog.String("subsystem", "cache"),
		slog.String("event", "memo_store"),
		slog.String("gvr", gvr.String()),
		slog.String("cohort", cohort),
		slog.Uint64("rbac_gen", currentGen),
		slog.Bool("permit_all", memo.permitAll),
		slog.Int("kept_names", len(memo.keptNames)),
		slog.Int("items_total", len(parsed.items)),
		slog.Int("encoded_json", len(memo.encodedJSON)),
		slog.Int("encoded_gzip", len(memo.encodedGzip)),
		slog.Int64("memo_size", store.Size()),
	)

	return cohortGateMemoServe(ctx, gvr, parsed, memo, cohort), true, memo, cohort
}

// gateListItemsWithMemo is the memo-aware companion to gateListItems.
// It runs the per-item filterListByRBAC ONLY on a memo miss or stale
// stamp; on a hit it walks parsed.items and re-builds the kept slice
// by keptNames lookup, skipping every EvaluateRBAC call.
//
// Returns (gatedEnvelope, served) — identical contract to gateListItems.
//
// `store` is the entry's *cache.CohortGateMemoStore (nil-safe — a nil
// store falls through to gateListItems with no memo). A nil store
// happens for non-entry callers (legacy refresh paths, tests); it must
// stay safe so the memo can be wired incrementally without breaking
// the wider gate surface.
//
// Retained for non-postJQ callers; Phase B / 0.30.185 added the Ex
// variant above. Both share the populate path; the Ex variant just
// surfaces the memo + cohort pair to its caller.
func gateListItemsWithMemo(
	ctx context.Context,
	store *cache.CohortGateMemoStore,
	gvr schema.GroupVersionResource,
	parsed parsedListEnvelope,
) (any, bool) {
	if store == nil {
		// No store — fall through to the original gate (per-item
		// EvaluateRBAC). Same wire shape; behaviorally pre-GMC.
		return gateListItems(ctx, gvr, parsed)
	}

	// Cohort key — extracts UserInfo from ctx. Missing identity =>
	// memo bypass + fail-closed on the underlying gate.
	ui, err := xcontext.UserInfo(ctx)
	if err != nil {
		// gateListItems will record the no-identity branch and return
		// served=false. We do NOT memoize a missing-identity outcome.
		return gateListItems(ctx, gvr, parsed)
	}
	cohort := cohortKeyHashFromUserInfo(ui.Username, ui.Groups)
	if cohort == "" {
		return gateListItems(ctx, gvr, parsed)
	}

	// Ship GMC.1 / 0.30.175 — per-cohort generation. Replaces the global
	// cache.RBACGen() stamp the 0.30.174 GMC used. Mutations to bindings
	// NOT in this cohort's matched-set leave currentGen untouched, so
	// admin's burst hit-rate stays ≥9/10 under multi-cohort RBAC churn.
	currentGen := cache.CohortRBACGen(ui.Username, ui.Groups)

	if v, ok := store.Lookup(cohort); ok {
		if memo, isMemo := v.(*cohortGateMemo); isMemo && memo != nil && memo.rbacGen == currentGen {
			// HIT — serve from memo. permitAll keeps every item;
			// !permitAll walks parsed.items and filters by keptNames.
			// No EvaluateRBAC fan-out on either path.
			return cohortGateMemoServe(ctx, gvr, parsed, memo, cohort), true
		}
		// Stamp mismatch (stale memo against current RBAC store) —
		// fall through to the populate path and overwrite.
	}

	// MISS or STALE — populate.
	memo, served := populateCohortGateMemo(ctx, gvr, parsed, ui.Username, ui.Groups, cohort, currentGen)
	if !served {
		// Fail-closed: caller falls through to the apiserver. No memo
		// write — the next request rebuilds against the live identity.
		return nil, false
	}
	store.Store(cohort, memo)

	log := xcontext.Logger(ctx)
	log.Debug("apistage.cohort_gate_memo.store",
		slog.String("subsystem", "cache"),
		slog.String("event", "memo_store"),
		slog.String("gvr", gvr.String()),
		slog.String("cohort", cohort),
		slog.Uint64("rbac_gen", currentGen),
		slog.Bool("permit_all", memo.permitAll),
		slog.Int("kept_names", len(memo.keptNames)),
		slog.Int("items_total", len(parsed.items)),
		slog.Int("encoded_json", len(memo.encodedJSON)),
		slog.Int("encoded_gzip", len(memo.encodedGzip)),
		slog.Int64("memo_size", store.Size()),
	)

	return cohortGateMemoServe(ctx, gvr, parsed, memo, cohort), true
}

// populateCohortGateMemo builds a fresh *cohortGateMemo by running the
// CohortNSACL fast-path against the live RBAC snapshot. On a successful
// build it returns (memo, true); on a fail-closed (no identity / no
// snapshot) it falls back to the canonical filterListByRBAC and returns
// (memo, true) or (nil, false).
//
// permitAll fast-path: every item kept; encodedJSON + encodedGzip
// populated for zero-CPU serve.
//
// !permitAll fast-path: keptNames built from item.GetNamespace()
// membership in permittedNS — single map lookup per item, no
// EvaluateRBAC fan-out.
//
// fail-through: when the snapshot is nil (pre-readiness) or the ACL
// returns a nonsensical empty for an authenticated cohort with no
// matching bindings, fall back to filterListByRBAC. The canonical
// filter's per-item evaluator handles every fail-closed branch
// (identity / evaluator error / deny). This guard protects against a
// hypothetical permit-loss regression where the ACL under-includes
// the cohort's actual grants — the canonical filter is the
// correctness barrier (HG-178.2 byte-identity gate).
func populateCohortGateMemo(
	ctx context.Context,
	gvr schema.GroupVersionResource,
	parsed parsedListEnvelope,
	username string,
	groups []string,
	cohort string,
	currentGen uint64,
) (*cohortGateMemo, bool) {
	snap := cache.LiveRBACSnapshot()
	if snap == nil {
		// Pre-readiness / cache=off — fall through to the canonical
		// filter, which itself fails closed on a missing snapshot.
		return populateMemoFromCanonicalFilter(ctx, gvr, parsed, currentGen)
	}

	permitAll, permittedNS := cache.CohortNSACL(snap, username, groups, gvr)

	if permitAll {
		// Cluster-wide list grant — keep every item, encode the envelope
		// and gzip it ONCE. Zero-bound: no size threshold.
		envelopeValue := listEnvelopeValue(parsed.apiVersion, parsed.kind, parsed.items)
		encoded, err := json.Marshal(envelopeValue)
		if err != nil {
			// json.Marshal on a map[string]any produced by
			// parseListEnvelope cannot fail under the json package's
			// documented contract (every reachable value is map / slice
			// / scalar / json.Number from the prior json.Unmarshal).
			// Defensive: fall back to canonical filter.
			xcontext.Logger(ctx).Warn("apistage.cohort_gate_memo.permitAll_encode_failed",
				slog.String("subsystem", "cache"),
				slog.String("gvr", gvr.String()),
				slog.String("cohort", cohort),
				slog.Any("err", err),
			)
			return populateMemoFromCanonicalFilter(ctx, gvr, parsed, currentGen)
		}
		gz, err := gzipBytes(encoded)
		if err != nil {
			// Same defensive posture — gzip on a byte slice cannot
			// fail in the standard library outside of OOM. Skip the
			// gzip cache; the serve path falls through to encodedJSON.
			xcontext.Logger(ctx).Warn("apistage.cohort_gate_memo.permitAll_gzip_failed",
				slog.String("subsystem", "cache"),
				slog.String("gvr", gvr.String()),
				slog.String("cohort", cohort),
				slog.Any("err", err),
			)
			gz = nil
		}

		memo := &cohortGateMemo{
			rbacGen:     currentGen,
			permitAll:   true,
			encodedJSON: encoded,
			encodedGzip: gz,
		}

		// Observability — bump aggregate counters.
		cache.RecordCohortMemoBytes(len(encoded) + len(gz))
		cache.RecordCohortMemoEncodedBytes(len(encoded) + len(gz))

		// Phase B / 0.30.185 — per-entry size slog at populate time.
		// HG-PB.14 requires per-entry distribution (p50/p95/p99/max)
		// for empirical cap derivation; the cohort_memo.populate line
		// here is the source for the post-deploy log-grep aggregator.
		xcontext.Logger(ctx).Info("cohort_memo.populate",
			slog.String("subsystem", "cache"),
			slog.String("event", "memo_populate"),
			slog.String("gvr", gvr.String()),
			slog.String("cohort", cohort),
			slog.Bool("permit_all", true),
			slog.Int("items_total", len(parsed.items)),
			slog.Int("encoded_json_bytes", len(encoded)),
			slog.Int("encoded_gzip_bytes", len(gz)),
			slog.Int("postjq_bytes", 0), // lazy — bumped at populate time below
			slog.Int("kept_names", 0),
		)

		return memo, true
	}

	// !permitAll — namespace-scoped grants. Filter parsed.items by
	// permittedNS membership; build keptNames. No per-item EvaluateRBAC.
	keptNames := make(map[string]struct{}, len(permittedNS))
	bytesEstimate := 0
	for _, it := range parsed.items {
		if it == nil {
			continue
		}
		ns := it.GetNamespace()
		if _, ok := permittedNS[ns]; !ok {
			continue
		}
		key := itemNSName(it)
		keptNames[key] = struct{}{}
		bytesEstimate += len(key) + 16 // map entry overhead estimate
	}

	memo := &cohortGateMemo{
		rbacGen:   currentGen,
		permitAll: false,
		keptNames: keptNames,
	}
	cache.RecordCohortMemoBytes(bytesEstimate)

	// Phase B / 0.30.185 — per-entry size slog at populate time.
	// HG-PB.14 instrumentation: same line shape as the permitAll path
	// so the post-deploy aggregator can compute distributions over the
	// full cohort population (permitAll + narrow combined).
	xcontext.Logger(ctx).Info("cohort_memo.populate",
		slog.String("subsystem", "cache"),
		slog.String("event", "memo_populate"),
		slog.String("gvr", gvr.String()),
		slog.String("cohort", cohort),
		slog.Bool("permit_all", false),
		slog.Int("items_total", len(parsed.items)),
		slog.Int("encoded_json_bytes", 0),
		slog.Int("encoded_gzip_bytes", 0),
		slog.Int("postjq_bytes", 0),
		slog.Int("kept_names", len(keptNames)),
	)

	return memo, true
}

// populateMemoFromCanonicalFilter is the fall-back populate path when
// the ACL fast-path cannot run (snapshot nil). Runs filterListByRBAC
// per-item against the live identity, stores the keptNames set in the
// memo. permitAll stays false. No encoded-bytes caching — the slow path
// rebuilds on every miss.
func populateMemoFromCanonicalFilter(
	ctx context.Context,
	gvr schema.GroupVersionResource,
	parsed parsedListEnvelope,
	currentGen uint64,
) (*cohortGateMemo, bool) {
	kept, identityOK := filterListByRBAC(ctx, gvr, parsed.items)
	if !identityOK {
		return nil, false
	}
	memo := &cohortGateMemo{
		rbacGen:   currentGen,
		permitAll: false,
		keptNames: make(map[string]struct{}, len(kept)),
	}
	bytesEstimate := 0
	for _, it := range kept {
		if it == nil {
			continue
		}
		key := itemNSName(it)
		memo.keptNames[key] = struct{}{}
		bytesEstimate += len(key) + 16
	}
	cache.RecordCohortMemoBytes(bytesEstimate)

	// Phase B / 0.30.185 — per-entry size slog at populate time (fallback
	// canonical-filter populate path). Cohort key is unknown at this
	// site (the caller picks it up from UserInfo); omit cohort here.
	xcontext.Logger(ctx).Info("cohort_memo.populate",
		slog.String("subsystem", "cache"),
		slog.String("event", "memo_populate_canonical"),
		slog.String("gvr", gvr.String()),
		slog.Bool("permit_all", false),
		slog.Int("items_total", len(parsed.items)),
		slog.Int("encoded_json_bytes", 0),
		slog.Int("encoded_gzip_bytes", 0),
		slog.Int("postjq_bytes", 0),
		slog.Int("kept_names", len(memo.keptNames)),
	)

	return memo, true
}

// cohortGateMemoServe assembles the served envelope from a memo (HIT
// or just-populated MISS):
//
//   - permitAll: return listEnvelopeValue(parsed.apiVersion, parsed.kind,
//     parsed.items) — every item served. (The encoded bytes in the memo
//     are cached for future plumb-through to the HTTP serve layer;
//     today's caller consumes the decoded value via jsonHandlerValue.)
//   - !permitAll: walk parsed.items, keep those whose ns/name is in
//     memo.keptNames.
//
// Returns the same map[string]any shape listEnvelopeValue produces on
// the canonical path, with the same deep-copy isolation invariant.
func cohortGateMemoServe(
	ctx context.Context,
	gvr schema.GroupVersionResource,
	parsed parsedListEnvelope,
	memo *cohortGateMemo,
	cohort string,
) map[string]any {
	log := xcontext.Logger(ctx)

	if memo.permitAll {
		log.Debug("apistage.cohort_gate_memo.hit_permit_all",
			slog.String("subsystem", "cache"),
			slog.String("event", "memo_hit"),
			slog.String("gvr", gvr.String()),
			slog.String("cohort", cohort),
			slog.Uint64("rbac_gen", memo.rbacGen),
			slog.Int("items_total", len(parsed.items)),
			slog.Int("encoded_json", len(memo.encodedJSON)),
			slog.Int("encoded_gzip", len(memo.encodedGzip)),
		)
		return listEnvelopeValue(parsed.apiVersion, parsed.kind, parsed.items)
	}

	// !permitAll — filter parsed.items by keptNames membership.
	kept := make([]*unstructured.Unstructured, 0, len(memo.keptNames))
	for _, it := range parsed.items {
		if it == nil {
			continue
		}
		if _, ok := memo.keptNames[itemNSName(it)]; ok {
			kept = append(kept, it)
		}
	}
	log.Debug("apistage.cohort_gate_memo.hit_narrow",
		slog.String("subsystem", "cache"),
		slog.String("event", "memo_hit"),
		slog.String("gvr", gvr.String()),
		slog.String("cohort", cohort),
		slog.Uint64("rbac_gen", memo.rbacGen),
		slog.Int("items_total", len(parsed.items)),
		slog.Int("kept", len(kept)),
	)
	return listEnvelopeValue(parsed.apiVersion, parsed.kind, kept)
}

// gzipBytes compresses src with the default gzip level. Returns the
// compressed bytes; on any error returns the error and nil bytes. The
// caller treats a non-nil error as "skip the gzip cache" — the serve
// path then falls through to encodedJSON.
//
// Per feedback_no_naive_compression_middleware: compression happens
// ONCE at memo populate time. Warm reads serve pre-compressed bytes
// from memory — zero CPU cost on hits. This is the value-layer cache
// pattern that feedback explicitly endorses.
func gzipBytes(src []byte) ([]byte, error) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(src); err != nil {
		_ = gw.Close()
		return nil, err
	}
	if err := gw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// itemNSName renders the "namespace/name" key used by keptNames. A
// cluster-scoped item (Namespace=="") yields "/name" — stable and
// non-overlapping with namespaced "ns/name".
func itemNSName(it *unstructured.Unstructured) string {
	return it.GetNamespace() + "/" + it.GetName()
}
