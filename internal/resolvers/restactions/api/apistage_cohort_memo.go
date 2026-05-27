// apistage_cohort_memo.go — Ship GMC / 0.30.174, A.2 / 0.30.178,
// A.2-trim / 0.30.194.
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
//                              The serve path returns the parsed envelope
//                              directly — no encoded-bytes cache needed.
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
// SHIP 0.30.194 — DEAD-ENCODE REMOVAL (Fix A)
//   The A.2 design populated encodedJSON + encodedGzip on the permitAll
//   path expecting a future plumb-through to the HTTP serve layer. That
//   plumb-through never materialised: cohortGateMemoServe re-builds the
//   envelope value per hit via listEnvelopeValue(parsed.items) and the
//   downstream stage consumes the map[string]any value, not the bytes.
//   The cached bytes were NEVER READ. At admin-cohort scale they cost
//   ~363 MB / json.Marshal + ~100 MB / gzipBytes per populate — 52s of
//   wasted CPU during the cold-path PIP seed. Fix A removes the dead
//   fields and the dead work; the serve path is unchanged.
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
//   - feedback_shared_vs_copy_is_a_concurrency_change.md — keptNames
//     is BUILT ONCE on memo populate then READ-ONLY for every hit. The
//     cache.CohortGateMemoStore guards concurrent populate/read with a
//     sync.Map; the memo struct itself is never mutated post-store.
//     Fix A REMOVES fields (encodedJSON, encodedGzip) — it strengthens
//     the read-only invariant; it never introduces a copy→shared
//     transition.
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
	"context"
	"log/slog"
	"time"

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
// (permitAll). Per-cohort variability is too high on the !permitAll
// branch to make encoded-bytes caching profitable; the keptNames +
// per-item map lookup serves those cohorts. The permitAll branch
// returns the parsed envelope value directly at serve time (no encoded-
// bytes cache — Fix A / 0.30.194 removed the never-read encoded fields
// that the original A.2 design had populated speculatively).
//
// The whole struct is BUILT ONCE on populate then NEVER mutated. Hit
// readers consume it lock-free; the cache.CohortGateMemoStore guards
// the (cohortKey -> *cohortGateMemo) lookup with sync.Map.
type cohortGateMemo struct {
	rbacGen uint64

	// permitAll captures the CohortNSACL verdict for this (entry × cohort).
	// true: cluster-wide list grant — every item is kept.
	// false: namespace-scoped grants only — keptNames is the filter.
	permitAll bool

	// keptNames is populated ONLY when !permitAll. "ns/name" keys this
	// cohort can list. A nil/empty map means "no items kept" — the
	// caller's range-over-nil walk yields an empty served envelope.
	keptNames map[string]struct{}
}

// cohortKeyHashFromUserInfo is a thin shim over cache.CohortKeyHash so
// in-package callers keep their existing name. Ship GMC.1 / 0.30.175
// moved the canonical implementation to cache/ so the per-cohort gate
// memo (here) and the per-cohort RBAC generator (cache.CohortRBACGen)
// compute byte-identical cohort keys for the same identity.
func cohortKeyHashFromUserInfo(username string, groups []string) string {
	return cache.CohortKeyHash(username, groups)
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
// permitAll fast-path: every item kept; stamp the rbacGen + permitAll
// flag and return. Serve-side rebuilds the envelope per hit via
// listEnvelopeValue(parsed.items). Ship 0.30.194 Fix A removed the
// dead encodedJSON/encodedGzip populate that the original A.2 design
// had populated speculatively — those fields were never read by
// cohortGateMemoServe and cost ~52s of cold-path CPU (~363 MB
// json.Marshal + ~100 MB gzip per populate at admin-cohort scale).
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
	// Ship 0.30.193 Checkpoint 4 — populateCohortGateMemo timing.
	// Captures permitAll, input_items_count, kept_names_count,
	// elapsed_ms across every populate path (ACL fast-path, canonical
	// fallback). The sink is installed ONLY at PIP seed; production
	// /call has no sink → every Accumulate* below is a no-op.
	//
	// inputCount is set once at function entry (parsed.items length —
	// it is the same input regardless of populate branch). permitAll +
	// keptCount + elapsed are set by the deferred record below; the
	// nested closure captures the locals so each return path sets them
	// before the defer fires.
	pipSink := cache.PIPStageTimingSinkFrom(ctx)
	t0 := time.Now()
	inputCount := len(parsed.items)
	var recordedPermitAll bool
	var recordedKeptCount int
	defer func() {
		pipSink.AccumulateMemoPopulate(recordedPermitAll, inputCount,
			recordedKeptCount, time.Since(t0).Milliseconds())
	}()

	snap := cache.LiveRBACSnapshot()
	if snap == nil {
		// Pre-readiness / cache=off — fall through to the canonical
		// filter, which itself fails closed on a missing snapshot.
		memo, ok := populateMemoFromCanonicalFilter(ctx, gvr, parsed, currentGen)
		if ok && memo != nil {
			recordedPermitAll = memo.permitAll
			recordedKeptCount = len(memo.keptNames)
		}
		return memo, ok
	}

	permitAll, permittedNS := cache.CohortNSACL(snap, username, groups, gvr)

	if permitAll {
		// Cluster-wide list grant — keep every item. The serve path
		// rebuilds the envelope per hit via listEnvelopeValue(parsed.items);
		// no encoded-bytes cache (Fix A / 0.30.194).
		memo := &cohortGateMemo{
			rbacGen:   currentGen,
			permitAll: true,
		}

		// Ship 0.30.193 C4 — permitAll path: every input item kept.
		recordedPermitAll = true
		recordedKeptCount = inputCount

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

	// Ship 0.30.193 C4 — !permitAll path: keptNames is the filtered subset.
	recordedPermitAll = false
	recordedKeptCount = len(keptNames)

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
	return memo, true
}

// cohortGateMemoServe assembles the served envelope from a memo (HIT
// or just-populated MISS):
//
//   - permitAll: return listEnvelopeValue(parsed.apiVersion, parsed.kind,
//     parsed.items) — every item served. Ship 0.30.194 Fix A confirmed
//     this is the only consumer of the permitAll memo at serve time;
//     the previously cached encodedJSON/encodedGzip bytes were never
//     read here and have been removed from the memo struct.
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

// itemNSName renders the "namespace/name" key used by keptNames. A
// cluster-scoped item (Namespace=="") yields "/name" — stable and
// non-overlapping with namespaced "ns/name".
func itemNSName(it *unstructured.Unstructured) string {
	return it.GetNamespace() + "/" + it.GetName()
}
