// proactive_ra_seed.go — the RBAC-reachable RESTAction enumeration
// source for the prewarm-engine boot seed (Option A).
//
// PROBLEM. The boot seed source (prewarm_engine_boot.go:258) drains the
// nav-walk content harvester (contentPrewarmHarvester). The harvester
// only records the spec.apiRef RESTActions of widgets the walk actually
// REACHES — the nav-tree roots and their resolvable children. The
// per-composition click-through detail widgets (and their
// `composition-resources`-style RESTActions) are NEVER walked at boot:
// the frontend reaches them only on a user navigation into a composition
// detail page. So those RESTActions are never seeded — every first
// composition-detail /call is a cold resolve.
//
// SOURCE (Option A — ALL RBAC-reachable RESTActions). The seed-targeting
// source is purely RBAC-derived, with NO resource/name/path literal
// (feedback_no_special_cases): the set of RESTActions some published RBAC
// binding grants `get` on, intersected with the RESTAction CRs that
// actually exist in the cluster (read from the BOOT-anchored RESTActions
// informer — restActionGVR is a MetaQuerySeeds boot anchor, phase1.go,
// so NO new informer is registered).
//
// WHY THE INTERSECTION IS CLEAN. RBAC on `restactions` is resource-level
// (group/resource), never per-object-name — rulesGrantGetList
// (bindings_by_gvr.go) skips resourceNames-scoped rules entirely. So the
// reachable set is binary: if ANY binding grants get on restactions, the
// whole existing RESTAction CR set is reachable by that binding's
// subjects; if NO binding does, the set is empty. The PER-RA → per-binding
// scoping for the actual seed is NOT done here — it is done downstream by
// the existing seed loop (seedScopeYielding → restActionTargetGVR →
// EnumeratePrewarmTargetsForGVR on each RA's TARGET GVR). This function
// only widens WHICH RESTAction refs the seed loop iterates; it does not
// change the per-binding cell-key scoping (which stays per the RA's own
// userAccessFilter target GVR).
//
// Option B (filter to a compositions GVR) was REJECTED at the gate as a
// special-case smell. This source is the full RBAC-reachable RESTAction
// set — no GVR/resource filter.

package cache

import (
	"k8s.io/apimachinery/pkg/api/meta"
)

// RestActionRef is a namespace/name pointer to a RESTAction CR. The
// caller (the dispatcher prewarm-engine seed) builds the
// templatesv1.ObjectReference from it (the GVR is fixed — restActionGVR —
// so only ns/name need cross the package boundary). Kept package-local in
// SHAPE (no apis/templates import in the cache package).
type RestActionRef struct {
	Namespace string
	Name      string
}

// RBACReachableRestActionRefs returns the namespace/name refs of every
// RESTAction CR that is RBAC-reachable for a `get` — i.e. the intersection
// of (a) "some published binding grants get on restactions" and (b) the
// RESTAction CRs resident in the boot-anchored RESTActions informer.
//
// EMPTY RETURN (nil) when:
//   - rw is nil / passthrough, OR
//   - the BindingsByGVR index reports NO binding granting get on
//     restactions (EnumeratePrewarmTargetsForGVR empty) — nothing is
//     RBAC-reachable, so the seed is a no-op and serving is transparent
//     (F-6), OR
//   - the RESTActions informer holds no items.
//
// This is SEED-TARGETING only (per the bindings_by_gvr.go AUTHZ-BOUNDARY
// note): over-inclusion = wasted seed (benign); the per-request authz
// boundary is unchanged (EvaluateRBAC at /call time). The refs are read
// via meta.Accessor (ns/name only) — the full RESTAction JSON is NOT
// decoded here.
func RBACReachableRestActionRefs(rw *ResourceWatcher) []RestActionRef {
	if rw == nil || rw.mode == modePassthrough {
		return nil
	}

	// (a) RBAC gate — is `get restactions` granted by ANY published
	// binding? Reuse the per-binding enumerator (the same one the seed
	// loop uses for per-RA target scoping). We only need the YES/NO here;
	// the per-binding scoping happens downstream per the RA's TARGET GVR.
	if len(EnumeratePrewarmTargetsForGVR(restActionGVR, "get")) == 0 {
		return nil
	}

	// (b) the RESTAction CRs resident in the boot-anchored informer.
	// indexerList reads the registered informer's indexer directly — no
	// new informer, no full-object decode (meta.Accessor reads the
	// embedded ObjectMeta of each store value: *bytesObject or
	// *unstructured.Unstructured).
	items := indexerList(rw, restActionGVR)
	if len(items) == 0 {
		return nil
	}

	out := make([]RestActionRef, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, it := range items {
		acc, err := meta.Accessor(it)
		if err != nil {
			continue
		}
		ns := acc.GetNamespace()
		name := acc.GetName()
		if name == "" {
			continue
		}
		key := ns + "/" + name
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, RestActionRef{Namespace: ns, Name: name})
	}
	return out
}
