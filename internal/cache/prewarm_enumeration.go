// Package cache — Ship 0.30.242 H.c-layered Phase 2 step 2b.
//
// prewarm_enumeration.go — replaces binding_set_enumeration.go's
// EnumerateBindingSetClasses + bindings_by_gvr.go's
// EnumerateResourceCohorts.
//
// PURPOSE (design §7.1 + §7.2)
//
// The prewarm engine seeds one L1 cell per layer per per-layer first-
// match binding. Under H.c-layered the cell key folds BindingUID (the
// matched binding's stable identity), so the prewarm enumeration
// returns PER-BINDING TARGETS rather than per-cohort tuples.
//
// For each navigated GVR × verb, look up the bindings in BindingsByGVR
// (the existing reverse index — bindings_by_gvr.go). For each binding,
// emit ONE PrewarmTarget. The representative SubjectIdentity for each
// target is drawn from the binding's Subjects (the prewarm dispatch
// runs under THIS identity; the cell it populates is shared with every
// real-user request whose first-match for the same (verb, gvr, ns) is
// the same binding).
//
// COMPARISON TO V3
//
//   V3: EnumerateBindingSetClasses() enumerated cohort universe via
//   powerset of groups × users + a sentinel for group-only cohorts.
//   The enumerator was global (not per-GVR); seed-targeting was
//   widened by EnumerateResourceCohorts per GVR but the COHORT KEY
//   was still BindingSetHash over the FULL matched binding-set.
//
//   H.c-layered: per-(GVR, verb) lookup against BindingsByGVR. Each
//   binding → one target. Cell sharing is per-binding (design §1.2).
//   No powerset; no sentinel; no group-vs-user dichotomy at the
//   enumerator level (the SubjectIdentity is whatever
//   pickRepresentativeFromSubjects picks from the binding's first
//   non-empty Subject).

package cache

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// PrewarmTarget is one prewarm dispatch's input. Each target represents
// a per-(binding, layer) cell that the prewarm engine should populate.
//
// FIELDS:
//   - BindingUID: the cell-key identity dimension (matched binding's
//     UID, as produced by cache.BindingUIDFromCRB / FromRB).
//   - Subject: the SubjectIdentity to dispatch the prewarm call as.
//     Derived from the binding's Subjects via pickRepresentativeFromSubjects.
//   - GVR: the layer's resource (the cell-key gvr dimension).
//   - Verb: the layer's authz verb (typically "get" for widgets /
//     restactions; "list" for apistage; depends on layer).
//
// CONTRACT: two real-user requests whose first-match for (verb, gvr,
// ns, name) is the SAME binding observe the SAME BindingUID and thus
// SHARE the prewarm-populated cell. This is the per-binding sharing
// invariant — design §1.2.
type PrewarmTarget struct {
	BindingUID string
	Subject    SubjectIdentity
	GVR        schema.GroupVersionResource
	Verb       string
}

// EnumeratePrewarmTargetsForGVR returns the per-binding prewarm targets
// that grant `verb` on `gvr` in the published RBAC snapshot. Reads the
// BindingsByGVR reverse index (immutable per snapshot post-build) and
// projects each matched binding's subjects into a representative
// SubjectIdentity.
//
// Design §7.2.
//
// EMPTY RETURN: nil-or-empty when (a) the index is not yet built
// (pre-readiness) or (b) no binding grants the (gvr, verb). The
// caller (the prewarm engine — phase1_pip_seed.go,
// prewarm_engine_boot.go) treats nil as "skip this GVR — no
// prewarmable identities" — design §7.2 explicitly rejects the v3
// fallback path that widened to a global cohort universe when the
// per-GVR index was empty (RBAC leak: would prewarm cells under
// identities that can't authorise the GVR).
//
// VERB SCOPING NOTE: the existing BindingsByGVR index buckets per
// {group, resource} for the verb pair {get, list}. It does NOT split
// by verb — a binding granting get+list lands once. The verb parameter
// here is for diagnostic / future per-verb refinement; the current
// implementation routes every supported verb through the same per-GVR
// bucket. If a future verb (e.g. "watch") needs separate enumeration,
// the index buckets shape evolves first; this enumerator follows.
func EnumeratePrewarmTargetsForGVR(gvr schema.GroupVersionResource, verb string) []PrewarmTarget {
	idx := bindingsByGVRSingleton()
	gr := grFromGVR(gvr)

	idx.mu.RLock()
	if !idx.built {
		idx.mu.RUnlock()
		return nil
	}

	// Collect the matching binding ids: per-GVR bucket ∪ wildcard.
	ids := make(map[bindingID]struct{}, 16)
	if set := idx.byGVR[gr]; set != nil {
		for id := range set {
			ids[id] = struct{}{}
		}
	}
	for id := range idx.wildcard {
		ids[id] = struct{}{}
	}

	out := make([]PrewarmTarget, 0, len(ids))
	for id := range ids {
		entry, ok := idx.entries[id]
		if !ok {
			continue
		}
		// Project the binding's subjects into a representative
		// SubjectIdentity. pickRepresentativeFromSubjects lives in
		// match_subject.go (the SOT for binding-identity primitives);
		// it picks the first non-empty User/Group/SA subject.
		//
		// The bindingEntry's subjects slice was projected at enrol
		// time (subjectsFromRBAC) to a []subjectKey; reconstitute a
		// minimal rbacv1.Subject slice so we can re-use the SOT helper
		// without re-walking the typed snapshot here.
		rep := pickRepresentativeFromSubjectKeys(entry.subjects)

		out = append(out, PrewarmTarget{
			BindingUID: string(id),
			Subject:    rep,
			GVR:        gvr,
			Verb:       verb,
		})
	}
	idx.mu.RUnlock()

	return out
}

// pickRepresentativeFromSubjectKeys is the index-domain mirror of
// pickRepresentativeFromSubjects. It accepts []subjectKey (the
// projection bindings_by_gvr.go stores on bindingEntry) and produces a
// SubjectIdentity. Same contract as the SOT helper:
//
//   - User-kind: SubjectIdentity{Username: name}
//   - Group-kind: SubjectIdentity{Groups: [name]}
//   - ServiceAccount-kind: SubjectIdentity{Username: "system:serviceaccount:<ns>:<name>"}
//   - empty: zero value
//
// Could call the rbacv1.Subject SOT helper if we re-typed the subjects
// here; staying in the index domain avoids the round-trip allocation.
func pickRepresentativeFromSubjectKeys(subjects []subjectKey) SubjectIdentity {
	for _, s := range subjects {
		switch s.Kind {
		case "User":
			if s.Name != "" {
				return SubjectIdentity{Username: s.Name}
			}
		case "Group":
			if s.Name != "" {
				return SubjectIdentity{Groups: []string{s.Name}}
			}
		case "ServiceAccount":
			if s.Name != "" && s.Namespace != "" {
				return SubjectIdentity{
					Username: "system:serviceaccount:" + s.Namespace + ":" + s.Name,
				}
			}
		}
	}
	return SubjectIdentity{}
}
