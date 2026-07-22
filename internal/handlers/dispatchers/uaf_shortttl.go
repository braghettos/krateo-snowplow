// uaf_shortttl.go — #118 (d) interim short-TTL for userAccessFilter-bearing
// resolved cells.
//
// THE DEFECT (docs/118-uaf-rbac-stale-read-design-2026-07-22.md): the resolved-
// cache key folds a single dispatch-authorizing BindingUID, but a
// userAccessFilter stage re-evaluates RBAC PER OBJECT, PER that object's own
// namespace — a dependency the key never sees. An out-of-band RBAC grant/revoke
// bumps RBACGen + rebuilds the snapshot but evicts ZERO resolved cells, so a
// user's own now-stale UAF view (access granted-in-N not yet visible, or
// revoked-in-N still visible) is served until the cell leaves cache. On a hot,
// data-plane-refreshed cell the CreatedAt slides forward every refresh → the
// standard TTL never elapses → effectively-indefinite staleness.
//
// THIS IS THE INTERIM (d), NOT THE FIX. It does NOT fix the key — a within-TTL
// RBAC change is still served stale. It CAPS the exposure window at a short
// TTLOverride on UAF-bearing cells. The durable fix is #118 (c): a per-user RBAC
// sub-generation folded into the cache key.
//
// C-118-6 (THE CRUX): the override must be stamped at BOTH Put sites — the
// customer dispatch Put (restactions.go) AND the refresher re-Put
// (resolve_populate.go, which builds a fresh entry with zero CreatedAt and thus
// slides the absolute TTL forward on every data-plane refresh). Stamping only
// the first Put lets a hot churning UAF cell re-Put without the override and
// OUTLIVE the cap. The customer path detects UAF from the resolved RESTAction CR
// and records it on cacheInputs.HasUAF; the refresher reads the carried
// inputs.HasUAF (it has no CR). Both sites call uafTTLOverrideForEntry, so the
// override derivation is single-source and cannot drift between them.
//
// TOGGLE (project_caching_is_provisional): UAF_RESOLVED_TTL_SECONDS default 0 =
// DISABLED → uafTTLOverrideForEntry returns 0 → TTLOverride stays 0 → every UAF
// cell uses the standard TTL, byte-identical to today. Cleanly removable.
//
// R-d-4 SITE MAP — the complete `ResolvedEntry{` Put enumeration and WHY each
// site is or is not in (d)'s scope (reasoned, not missed). (d) caps the
// identity-bound restactions cell that carries the per-user REFILTER OUTPUT.
//
// IN SCOPE — all three restactions ResolvedEntry Put sites stamp the UAF cap:
//   - restactions.go       — customer dispatch Put.
//   - resolve_populate.go  — refresher re-Put (CreatedAt-slides on every
//                            data-plane refresh; the C-118-6 crux site).
//   - phase1_pip_seed.go seedOneRestaction — boot-seed Put (seeds UAF cells
//                            under a cohort representative identity; added after
//                            the arch gate on 3783e65 caught the "counted 2,
//                            there were 3" miss).
//
// OUT OF SCOPE BY DESIGN (the PM's open apistage question, resolved by tracing):
//   - apistage.go:607 / cluster_list.go:417 — CacheEntryClassApistage, keyed by
//     contentKeyInputs(gvr, ns, name): IDENTITY-FREE shared substrate, the raw
//     pre-refilter apiserver envelope. Its staleness is DATA-plane (an informer
//     dirty-mark on the watched GVR invalidates it), NOT RBAC-refilter-output
//     staleness. It carries NO BindingUID and NO per-user refilter output, so an
//     out-of-band RBAC change does not make IT stale — the identity-bound
//     restactions cell (which holds the refilter output and IS capped above) is
//     the one that goes stale. Both already self-stamp the CATALOG_UNSERVABLE
//     data-plane override for their own degradation. Capping the substrate would
//     be wrong-cell and would churn a shared hot cell for zero RBAC-freshness
//     gain. (Traced + agreed with arch/PM; no disagreement to flag.)
//   - partial_result_ttl.go:85 — self-stamps its OWN bounded TTLOverride for a
//     partial-with-errors body; independent bounded-staleness mechanism. Untouched.
//   - phase1_pip_seed.go seedOneWidget (~:1046) + widgets.go / widget_content.go
//     / ra_full_list_store.go — widgets / widgetContent / RAFullList classes.
//     UAF is a restactions-STAGE contract (API.UserAccessFilter). A widget whose
//     apiRef resolves a UAF RA warms the RESTACTIONS cell via the apiref resolve
//     path — which IS the seedOneRestaction / dispatch path now capped above — so
//     the widget path is NOT a hole: the UAF refilter output only ever lands in a
//     restactions-class cell, never a widget-class one. widgetContent is
//     additionally identity-free (shared envelope, per-user serve-time filter).

package dispatchers

import (
	"time"

	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
)

// restactionHasUAFStage reports whether any api-step of cr declares a
// userAccessFilter (the per-object refilter contract the resolved key is blind
// to). Each api-step is a *API and UserAccessFilter a *UserAccessFilterSpec —
// both nil-guarded. This is a general per-entry predicate ("the entry's RA has a
// UAF stage"), NOT a per-resource special-case (feedback_no_special_cases): it
// keys on the presence of the UAF contract itself, uniform across every RA.
func restactionHasUAFStage(cr *templatesv1.RESTAction) bool {
	if cr == nil {
		return false
	}
	for _, step := range cr.Spec.API {
		if step != nil && step.UserAccessFilter != nil {
			return true
		}
	}
	return false
}

// uafTTLOverrideForEntry returns the short UAF TTLOverride to stamp on a
// resolved entry, or 0 (no override → standard TTL) when either the knob is
// disabled (UAF_RESOLVED_TTL_SECONDS unset/0) OR the entry is not UAF-bearing
// (inputs.HasUAF false / inputs nil). Called at BOTH Put sites (customer +
// refresher) so the cap is derived identically regardless of which path writes
// the cell (C-118-6). effectiveTTLLocked already honours the SHORTER of the
// override and the store TTL, so this only ever TIGHTENS the bound.
func uafTTLOverrideForEntry(inputs *cache.ResolvedKeyInputs) time.Duration {
	if inputs == nil || !inputs.HasUAF {
		return 0
	}
	return cache.UAFResolvedTTL()
}
