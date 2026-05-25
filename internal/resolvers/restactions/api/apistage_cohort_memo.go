// apistage_cohort_memo.go — Ship GMC / 0.30.174: per-cohort gate memo.
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
// SOLUTION
//   Memoize the kept-name set per (content-entry × cohort). Cohort is
//   keyed by hash(sorted(Groups) || Username) — stable across requests
//   from the same identity. The memo stamps the RBAC publish generation
//   (cache.RBACGen()) at write time; a stale memo (gen mismatch) is
//   discarded and re-filtered.
//
//   On hit: walk entry.Items, keep items whose "namespace/name" is in
//   the memo's keptNames set, skip filterListByRBAC entirely.
//   On miss / stale: run filterListByRBAC, populate the memo from the
//   kept items' names, store under cohortKey with the current rbacGen.
//
// SAFETY (binding contracts)
//   - feedback_l1_per_user_keyed_never_cohort.md — the L1 RESOLVED
//     cache stays per-user-keyed. This memo is a per-cohort SHORTCUT
//     over the per-item RBAC filter; cohorts admit the SAME rule set
//     by construction (same Groups + Username admit the same RBAC
//     bindings), so the kept set is identical. We never store a
//     resolved-output body under a cohort key.
//   - feedback_no_special_cases.md — uniform over every GVR; never
//     references a specific resource by name.
//   - feedback_shared_vs_copy_is_a_concurrency_change.md — keptNames
//     is BUILT ONCE on memo populate then READ-ONLY for every hit.
//     The cache.CohortGateMemoStore guards concurrent populate/read
//     with a sync.Map; the keptNames map itself is never mutated
//     post-store.
//   - feedback_capacity_caps_empirical_per_entry_cost.md — LRU cap at
//     256 entries per ResolvedEntry (cache.CohortGateMemoMaxEntries).
//     Well above empirically observed cohort cardinality (admin +
//     cyberjoker + a handful of per-namespace cohorts).
//
// FAIL-CLOSED
//   - Missing UserInfo / no identity => memo bypass + the underlying
//     filterListByRBAC returns served=false. Caller falls through to
//     the apiserver. No memo write on identity failure.
//   - filterListByRBAC's (subset, false) "no identity" return is
//     surfaced unchanged: no memo write on identity failure.
//   - identity-OK memo writes use ONLY the items filterListByRBAC
//     returned — never a partial / un-gated set.

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"sort"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// cohortGateMemo is the per-(content-entry × cohort) memoized output of
// filterListByRBAC: the set of "namespace/name" keys this cohort can
// list, stamped with the rbacGen the memo was built against.
//
// The keptNames map is BUILT ONCE on populate then NEVER mutated. Hit
// readers consume it lock-free; the cache.CohortGateMemoStore guards
// the (cohortKey -> *cohortGateMemo) lookup with sync.Map.
type cohortGateMemo struct {
	keptNames map[string]struct{} // "ns/name" keys this cohort can list
	rbacGen   uint64
}

// cohortKeyHashFromUserInfo hashes (sorted(groups), username) into a
// stable cohort identifier. SHA-256 truncated to 16 hex chars (8 bytes).
//
// Empty username + empty groups yields a deterministic key — anonymous
// requests cohort together; callers that care about anonymity drop the
// memo upstream (gateListItemsWithMemo bypasses the memo on a missing
// identity).
func cohortKeyHashFromUserInfo(username string, groups []string) string {
	sortedGroups := append([]string(nil), groups...)
	sort.Strings(sortedGroups)

	h := sha256.New()
	// Version prefix so a future key-shape change rotates the cohort
	// space cleanly across rolling restarts.
	h.Write([]byte("cohort-v1"))
	h.Write([]byte{0})
	h.Write([]byte(username))
	h.Write([]byte{0})
	for _, g := range sortedGroups {
		h.Write([]byte(g))
		h.Write([]byte{0})
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:8])
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

	currentGen := cache.RBACGen()

	if v, ok := store.Lookup(cohort); ok {
		if memo, isMemo := v.(*cohortGateMemo); isMemo && memo != nil && memo.rbacGen == currentGen {
			// HIT — walk parsed.items, keep those whose ns/name
			// appears in keptNames. No EvaluateRBAC fan-out on this
			// path.
			return cohortGateMemoServe(ctx, gvr, parsed, memo, cohort), true
		}
		// Stamp mismatch (stale memo against current RBAC store) —
		// fall through to the canonical filter and overwrite.
	}

	// MISS or STALE — run the canonical filter, then memoize.
	kept, identityOK := filterListByRBAC(ctx, gvr, parsed.items)
	if !identityOK {
		// Fail-closed: caller falls through to the apiserver. No memo
		// write — the next request rebuilds against the live identity.
		return nil, false
	}

	memo := &cohortGateMemo{
		keptNames: make(map[string]struct{}, len(kept)),
		rbacGen:   currentGen,
	}
	for _, it := range kept {
		if it == nil {
			continue
		}
		memo.keptNames[itemNSName(it)] = struct{}{}
	}
	evicted := store.Store(cohort, memo)

	log := xcontext.Logger(ctx)
	log.Debug("apistage.cohort_gate_memo.store",
		slog.String("subsystem", "cache"),
		slog.String("event", "memo_store"),
		slog.String("gvr", gvr.String()),
		slog.String("cohort", cohort),
		slog.Uint64("rbac_gen", currentGen),
		slog.Int("kept", len(kept)),
		slog.Int("items_total", len(parsed.items)),
		slog.Int64("memo_size", store.Size()),
		slog.Bool("lru_evicted", evicted),
	)

	return listEnvelopeValue(parsed.apiVersion, parsed.kind, kept), true
}

// cohortGateMemoServe assembles the served envelope from a memo HIT:
// walk parsed.items, keep those whose ns/name appears in memo.keptNames.
// Returns the same map[string]any shape listEnvelopeValue produces on
// the canonical path, with the same deep-copy isolation invariant.
func cohortGateMemoServe(
	ctx context.Context,
	gvr schema.GroupVersionResource,
	parsed parsedListEnvelope,
	memo *cohortGateMemo,
	cohort string,
) map[string]any {
	kept := make([]*unstructured.Unstructured, 0, len(memo.keptNames))
	for _, it := range parsed.items {
		if it == nil {
			continue
		}
		if _, ok := memo.keptNames[itemNSName(it)]; ok {
			kept = append(kept, it)
		}
	}
	log := xcontext.Logger(ctx)
	log.Debug("apistage.cohort_gate_memo.hit",
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
