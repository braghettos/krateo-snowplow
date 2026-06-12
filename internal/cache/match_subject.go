// Package cache — Ship 0.30.242 H.c-layered Phase 2 step 2a.
//
// This file is the SINGLE SOURCE OF TRUTH for binding-identity derivation.
//
// PACKAGE BOUNDARY:
//   internal/rbac/evaluate.go imports internal/cache (TRACED at evaluate.go:23).
//   internal/cache MUST NOT import internal/rbac (cycle).
//   Therefore the BindingUID constructors live HERE (the cache package),
//   and the rbac package calls back via the cache.BindingUIDFromCRB /
//   cache.BindingUIDFromRB exports.
//
// LINT GATE (Phase 3 F4 falsifier):
//   This file is the lint's SOLE WHITELISTED location for deriving a
//   BindingUID from snapshot indexes. Every "snap.CRBsBy*" or
//   "snap.RBsBy*" identity-projection iteration that produces a
//   BindingUID OUTSIDE this file fails the lint gate. The existing
//   snapshot-writer (rbac_snapshot.go), the reverse index builder
//   (bindings_by_gvr.go), and the rbac package's selectCRBCandidates /
//   selectRBCandidates are file-allowlisted (their iterations are NOT
//   BindingUID derivation — writer / index / candidate-fan-out).
//
// HISTORY:
//   Pre-ship the cohort identity lived as a uint64 hash
//   (`BindingSetHash` — rbac_cohort_gen.go:231) over the full matched
//   binding-set. Under H.c-layered, the L1 cell key carries a per-layer
//   BindingUID string — the metadata.uid of the FIRST-MATCH binding that
//   authorised the layer's access. The empty-UID fallback content tuple
//   matches v3's `crbIdentity` / `rbIdentity` (preserved at byte-equivalent
//   shape so synthetic test fixtures keep their pre-ship identity).
package cache

import (
	rbacv1 "k8s.io/api/rbac/v1"
)

// systemAuthenticatedGroup is the canonical implicit-group name the
// Kubernetes apiserver injects into every authenticated request. Mirrored
// from upstream rbac evaluation; used by the prewarm enumerator to skip
// the universal-authenticated Group-only cohort (every authenticated user
// carries it, so it is not a meaningful narrowing dimension).
//
// Pre-Ship-S.2 (Phase 2 step 2a) this constant lived in
// binding_set_enumeration.go; that file was deleted in Phase 2 step 1
// (commit 1d93d02). The constant moves here because the H.c-layered
// design positions match_subject.go as the canonical home for RBAC
// subject-matching primitives (design §4.3).
const systemAuthenticatedGroup = "system:authenticated"

// SubjectIdentity is the minimal subject shape consumed by the RBAC
// evaluator. Equivalent in surface to k8s authn/v1.UserInfo's required
// fields (Username + Groups). The cache-package mirror exists because
// internal/cache cannot import the rbac package (package-boundary cycle —
// see header).
//
// Used by:
//   - the prewarm enumerator (prewarm_enumeration.go — Phase 2b) as the
//     representative tuple for each prewarm target.
//   - pickRepresentativeFromSubjects — derives a SubjectIdentity from a
//     binding's Subjects slice.
//
// Username == "" denotes anonymous / no permit / no snapshot. Groups may
// be nil (User-kind subjects), single-element (Group-kind), or unset
// (ServiceAccount-kind — Username carries the canonical
// system:serviceaccount:<ns>:<name> form).
type SubjectIdentity struct {
	Username string
	Groups   []string
}

// BindingUIDFromCRB returns the stable per-binding identity for a CRB.
// The "C:" prefix namespaces CRB UIDs away from RB UIDs (the latter
// also prefix with "R:<ns>/" — see BindingUIDFromRB).
//
// Empty-UID fallback: synthetic / test-fixture bindings (and theoretically
// any apiserver pre-stamp gap) may have an empty UID. The fallback hashes
// the binding's content tuple (Name + RoleRef apiGroup/Kind/Name). This is
// the SAME mechanism the deleted crbIdentity (was at rbac_cohort_gen.go
// pre-Ship-S.2 line ~420) used — preserved across migration so synthetic
// fixtures retain their pre-ship identity.
//
// SAFETY: returns "" iff p == nil. Production binding pointers come from
// the snapshot's typed indexes — never nil; the nil-guard is defensive.
func BindingUIDFromCRB(p *rbacv1.ClusterRoleBinding) string {
	if p == nil {
		return ""
	}
	if uid := string(p.UID); uid != "" {
		return "C:" + uid
	}
	// US-001 separator (\x1f) keeps Name and RoleRef substrings from
	// colliding in the fallback tuple.
	return "C:fallback/" + p.Name +
		"\x1f" + p.RoleRef.APIGroup + "/" + p.RoleRef.Kind + "/" + p.RoleRef.Name
}

// BindingUIDFromRB returns the stable per-binding identity for an RB.
// "R:<ns>/" prefix keeps RB UIDs distinct from CRB UIDs AND distinct
// across namespaces (two RBs in different namespaces with the same UID
// produce different BindingUIDs — defensive; apiserver never reuses UIDs
// across namespaces but the prefix carries scope information into the
// key shape directly).
//
// Empty-UID fallback: same content-tuple shape as BindingUIDFromCRB,
// preserved from the deleted rbIdentity.
//
// SAFETY: returns "" iff p == nil.
func BindingUIDFromRB(p *rbacv1.RoleBinding) string {
	if p == nil {
		return ""
	}
	if uid := string(p.UID); uid != "" {
		return "R:" + p.Namespace + "/" + uid
	}
	return "R:" + p.Namespace + "/fallback/" + p.Name +
		"\x1f" + p.RoleRef.APIGroup + "/" + p.RoleRef.Kind + "/" + p.RoleRef.Name
}

// pickRepresentativeFromSubjects derives a representative SubjectIdentity
// from a binding's Subjects slice. Used by the prewarm enumerator (Phase
// 2b — prewarm_enumeration.go) to pick a single concrete identity to
// drive per-binding prewarm dispatches.
//
// CONTRACT (per design §7.2 + 4-ambiguity resolution):
//   - User-kind subject:           SubjectIdentity{Username: <user>, Groups: nil}
//   - Group-kind subject:          SubjectIdentity{Username: "",     Groups: []string{<group>}}
//   - ServiceAccount-kind subject: SubjectIdentity{Username: "system:serviceaccount:<ns>:<name>", Groups: nil}
//   - empty subjects:              SubjectIdentity{Username: "", Groups: nil} (the zero value)
//
// First-wins among the subject kinds in subjects[]; the cache cell is
// keyed by the BINDING's UID, not by subject identity, so the
// representative is only used at evaluate-time for the prewarm dispatch
// (any cohort member granted by the same binding produces the same cell
// contents — that's the whole sharing rationale, design §1.2).
//
// SAFETY: returns the zero value for nil/empty subjects.
func pickRepresentativeFromSubjects(subjects []rbacv1.Subject) SubjectIdentity {
	for _, s := range subjects {
		switch s.Kind {
		case rbacv1.UserKind:
			if s.Name != "" {
				return SubjectIdentity{Username: s.Name}
			}
		case rbacv1.GroupKind:
			if s.Name != "" {
				return SubjectIdentity{Groups: []string{s.Name}}
			}
		case rbacv1.ServiceAccountKind:
			if s.Name != "" && s.Namespace != "" {
				return SubjectIdentity{
					Username: "system:serviceaccount:" + s.Namespace + ":" + s.Name,
				}
			}
		}
	}
	return SubjectIdentity{}
}
