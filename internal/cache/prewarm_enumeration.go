// Package cache — Ship 0.30.242 H.c-layered Phase 2 step 2b.
//
// prewarm_enumeration.go — replaces binding_set_enumeration.go's
// EnumerateBindingSetClasses + bindings_by_gvr.go's
// EnumerateResourceCohorts.
//
// PURPOSE (design §7.1 + §7.2; #42 dedup design 2026-07-04)
//
// The prewarm engine seeds one L1 cell per layer per distinct
// REPRESENTATIVE IDENTITY. For each navigated GVR × verb, look up the
// bindings in BindingsByGVR (the existing reverse index —
// bindings_by_gvr.go), project each binding's Subjects to a
// representative SubjectIdentity, and emit ONE PrewarmTarget per DISTINCT
// (Username, sorted-Groups) tuple.
//
// WHY per-identity, not per-binding (#42): the enumerated BindingUID is
// carried on PrewarmTarget for diagnostics but is NEVER consumed by the
// seed dispatch — the cell key's BindingUID is RE-DERIVED at populate
// time from a FIRST-MATCH EvaluateRBAC over the representative identity
// (Path B: dispatchCacheLookupKey → EvaluateRBAC → matchedBindingUID →
// ComputeKey; Username/Groups themselves are never hashed). So two
// bindings with the same representative tuple produce BYTE-IDENTICAL seed
// dispatches populating the SAME cell. On a per-composition-RoleBinding
// topology one widget GVR carries hundreds of bindings that all project
// to Group/devs → one identity → 456 redundant full resolves for ONE
// cell (live: 81 obs-by-kind-list resolves × p50 4.45s = 396s → ONE hash,
// aborting the widgets loop before dashboard-flex). Deduping by
// representative identity is LOSSLESS (the seeded CELL SET is proven
// unchanged, design §1 correctness proof) and leak-free (distinct tuples
// still dispatch separately; L1 stays per-first-match-binding keyed —
// feedback_l1_per_user_keyed_never_cohort intact; this is DISPATCH dedup,
// not key cohorting). It is dedup of provably-identical work, NOT sampling
// (feedback_dynamic_cohort_prewarm_no_static_no_cold_fill — no caps, no
// static lists).
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
	"log/slog"
	"sort"
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// PrewarmTarget is one prewarm dispatch's input. Each target represents
// ONE DISTINCT REPRESENTATIVE IDENTITY (post-#42-dedup) that the prewarm
// engine should dispatch a seed resolve as.
//
// FIELDS:
//   - BindingUID: DIAGNOSTIC ONLY. The lexicographically-smallest binding
//     UID among the bindings that projected to this target's identity —
//     retained for log/telemetry (cohortLogLabel) and a stable tie-break.
//     It is NOT the cell-key identity dimension: the cell key's BindingUID
//     is RE-DERIVED at populate time from a first-match EvaluateRBAC over
//     Subject (Path B — dispatchCacheLookupKey; the enumerated BindingUID
//     never reaches ComputeKey). See the package header.
//   - Subject: the SubjectIdentity to dispatch the prewarm call as
//     (the dedup key domain: Username + sorted Groups). Derived from a
//     binding's Subjects via pickRepresentativeFromSubjects.
//   - GVR: the layer's resource (the cell-key gvr dimension).
//   - Verb: the layer's authz verb (typically "get" for widgets /
//     restactions; "list" for apistage; depends on layer).
//
// CONTRACT (#42): the enumerator returns ONE target per distinct
// representative identity. Two real-user requests whose first-match for
// (verb, gvr, ns, name) is the SAME binding still observe the SAME
// re-derived BindingUID and SHARE the prewarm-populated cell (per-binding
// sharing invariant, design §1.2). Deduping identical-tuple dispatches
// does not change which cells are seeded — only how many redundant
// resolves run to seed them (design §1 lossless/leak-free proof).
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

	// #42 DEDUP BY REPRESENTATIVE IDENTITY (design §2). Two bindings that
	// project to the same representative (Username, sorted-Groups) tuple
	// produce byte-identical seed dispatches populating the SAME cell (Path B
	// re-derives the cell-key BindingUID from the identity, not the enumerated
	// one — see the package header). We therefore emit ONE PrewarmTarget per
	// distinct tuple. Deterministic: keep the lexicographically-smallest
	// bindingID among a tuple's bindings as the DIAGNOSTIC BindingUID (the
	// field is diagnostic-only). This is dedup of provably-identical work —
	// NOT sampling: the seeded CELL SET is unchanged (no caps, no static
	// lists; feedback_dynamic_cohort_prewarm_no_static_no_cold_fill,
	// feedback_no_special_cases). Placing it in the enumerator covers the
	// engine's widgets AND restactions loops (and proactive_ra_seed's
	// len()==0 gate — dedup never turns a non-empty set empty) with ONE
	// insertion; the boot.go seam signature is unchanged.
	byIdentity := make(map[string]PrewarmTarget, len(ids))
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

		// Dedup key = Username + US + join(SORTED Groups, US). Today a
		// representative carries ≤1 group, but sorting future-proofs the key
		// against EvaluateRBAC's set-semantics over Groups (the SEP US=\x1f
		// can't collide with an RFC-1123 subject name or a group string). The
		// binding-namespace is deliberately NOT in the key: the seed dispatch
		// evaluates at the WIDGET's (gvr, ns, name), so two same-subject
		// RoleBindings in different namespaces are one identical dispatch
		// (design §1 namespace nuance).
		groups := append([]string(nil), rep.Groups...)
		sort.Strings(groups)
		key := rep.Username + "\x1f" + strings.Join(groups, "\x1f")

		if prev, seen := byIdentity[key]; seen {
			// Keep the lexicographically-smallest bindingID as the stable
			// diagnostic representative.
			if string(id) < prev.BindingUID {
				prev.BindingUID = string(id)
				byIdentity[key] = prev
			}
			continue
		}
		byIdentity[key] = PrewarmTarget{
			BindingUID: string(id),
			Subject:    rep,
			GVR:        gvr,
			Verb:       verb,
		}
	}
	rawBindings := len(ids)
	idx.mu.RUnlock()

	out := make([]PrewarmTarget, 0, len(byIdentity))
	for _, t := range byIdentity {
		out = append(out, t)
	}

	// AC-D4: one greppable line per call — {gvr, bindings(raw), identities(deduped)}
	// preserves the post-deploy "how much redundancy collapsed?" story now that
	// the engine's `targets` field reports the deduped count. Info-level (arch
	// C-1): the default slog handler is Info, so at Debug the OC-1 live evidence
	// would never emit on a standard deploy; one line per GVR per walk is cheap
	// and matches the sibling widget_targets/restaction_targets Info lines.
	slog.Info("prewarm.enumerate.dedup",
		slog.String("subsystem", "cache"),
		slog.String("gvr", gvr.String()),
		slog.String("verb", verb),
		slog.Int("bindings", rawBindings),
		slog.Int("identities", len(out)),
	)

	return out
}

// pickRepresentativeFromSubjectKeys is the index-domain adapter onto the
// SOT pickRepresentativeFromSubjects (match_subject.go). It accepts
// []subjectKey (the projection bindings_by_gvr.go stores on bindingEntry),
// re-types each into the rbacv1.Subject shape the SOT consumes, and
// delegates — so the User/Group/ServiceAccount → SubjectIdentity contract
// lives in exactly ONE place.
//
// The subjectKey.Kind values are the rbac/v1 kind strings recorded by
// subjectsFromRBAC (only User / Group / ServiceAccount are projected), so
// the round-trip is loss-free: the SOT's rbacv1.{User,Group,ServiceAccount}Kind
// switch matches each re-typed Subject byte-for-byte. The earlier copy
// duplicated that kind-switch in the index domain to dodge this small
// allocation; the DRY win outweighs allocating a short Subject slice on the
// (cold) prewarm-enumeration path.
func pickRepresentativeFromSubjectKeys(subjects []subjectKey) SubjectIdentity {
	if len(subjects) == 0 {
		return SubjectIdentity{}
	}
	retyped := make([]rbacv1.Subject, 0, len(subjects))
	for _, s := range subjects {
		retyped = append(retyped, rbacv1.Subject{
			Kind:      s.Kind,
			Name:      s.Name,
			Namespace: s.Namespace,
		})
	}
	return pickRepresentativeFromSubjects(retyped)
}
