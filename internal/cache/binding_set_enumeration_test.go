// binding_set_enumeration_test.go — Ship A.3 / 0.30.179, refined
// 0.30.183.
//
// Validates:
//
//   TestBindingSetHash_StableUnderEquivalentInput      — same (u, gs) hashes
//                                                         identically across calls.
//   TestBindingSetHash_MatchesCohortRBACGenMechanism   — AC-178.2 byte-equality
//                                                         with the pointer-set
//                                                         hash via
//                                                         collectCohortBindingPtrs.
//   TestBindingSetHash_ShiftsOnBindingMutation         — HG-178.5 invariant:
//                                                         a binding ADD touching
//                                                         this cohort flips
//                                                         the hash.
//   TestEnumerateBindingSetClasses_EmptySnapshot       — nil snapshot returns nil.
//   TestEnumerateBindingSetClasses_BasicDedupe         — two users on the same
//                                                         binding collapse via
//                                                         BindingSetHash.
//   TestEnumerateBindingSetClasses_PrunesSystemAuth    — system:authenticated
//                                                         is not in the powerset
//                                                         domain.
//   TestPrunePredicate_ZetaCorpusReal                  — Ship 0.30.183 headline
//                                                         falsifier: every one
//                                                         of the 29 production
//                                                         GKE control-plane
//                                                         User-kind names is
//                                                         pruned by predicate
//                                                         (ζ); real users
//                                                         (admin/cyberjoker,
//                                                         alice@org/admin-role)
//                                                         survive; a real user
//                                                         bound ONLY to
//                                                         system:basic-user
//                                                         is pruned as lossless
//                                                         false-prune.
//   TestPrunePredicate_NotInvokedForGroupOrSA          — predicate (ζ) NEVER
//                                                         observes a Group- or
//                                                         SA-kind subject.
//   TestPrunePredicate_WildcardRoleKeepsUser           — wildcard PolicyRule
//                                                         (APIGroups=["*"],
//                                                         Resources=["*"])
//                                                         overlaps every
//                                                         handler GVR → KEEP.
//   TestPrunePredicate_EmptyHandlerSet_PrunesAll       — defensive fail-closed
//                                                         posture: empty handler
//                                                         set prunes every
//                                                         non-system: User.
//   TestPrunePredicate_RoleBindingRoleKind             — RB roleRefs of
//                                                         Kind=Role resolve
//                                                         against
//                                                         RolesByNSName, not
//                                                         ClusterRolesByName.

package cache

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// snowplowHandlerGVRSetForTest is the (ζ)-predicate input set used by
// the test fixtures. Mirrors the production handler-gvr-snapshot
// artifact (/tmp/snowplow-runs/0.30.183/before/handler-gvr-snapshot-
// 0.30.181.txt) — the *.krateo.io-domain GVRs the snowplow /call
// dispatcher routes.
var snowplowHandlerGVRSetForTest = []schema.GroupResource{
	{Group: "templates.krateo.io", Resource: "restactions"},
	{Group: "widgets.templates.krateo.io", Resource: "datagrids"},
	{Group: "widgets.templates.krateo.io", Resource: "panels"},
	{Group: "widgets.templates.krateo.io", Resource: "navmenus"},
	{Group: "widgets.templates.krateo.io", Resource: "navmenuitems"},
	{Group: "widgets.templates.krateo.io", Resource: "routes"},
	{Group: "widgets.templates.krateo.io", Resource: "routesloaders"},
	{Group: "composition.krateo.io", Resource: "githubscaffoldingwithcompositionpages"},
	{Group: "core.krateo.io", Resource: "compositiondefinitions"},
}

// mkCRBWithRole is a CRB factory that lets a caller pin the RoleRef.Name
// to a specific value. The shared mkCRB factory hard-codes the role to
// name+"-role"; predicate (ζ) tests need explicit control over roleRef
// names because the predicate resolves them against ClusterRolesByName.
func mkCRBWithRole(name, roleName string, sub rbacv1.Subject) *rbacv1.ClusterRoleBinding {
	c := mkCRB(name, sub)
	c.RoleRef.Name = roleName
	c.RoleRef.Kind = "ClusterRole"
	return c
}

// mkClusterRole builds a ClusterRole with explicit PolicyRules — used
// by the (ζ) predicate corpus tests to attach the role's rules to the
// snapshot so unionRulesForRefs can resolve them. The role name is the
// snapshot lookup key (ClusterRolesByName[name]).
func mkClusterRole(name string, rules []rbacv1.PolicyRule) *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Rules:      rules,
	}
}

// noKrateoRule is the canonical "rules that don't touch *.krateo.io"
// PolicyRule shape — exactly what the live cluster's 29 control-plane
// User-kind subjects' matched ClusterRoles carry per the wire-probe-
// zeta.txt artifact. The (ζ) predicate's intersection against
// snowplowHandlerGVRSetForTest is empty, so subjects bound only to
// such roles MUST prune.
var noKrateoRule = rbacv1.PolicyRule{
	APIGroups: []string{""},
	Resources: []string{"nodes"},
	Verbs:     []string{"get", "list", "watch"},
}

// wildcardRule is the cyberjoker-style universal grant. (ζ) MUST
// observe an overlap with every handler GVR and KEEP the subject.
var wildcardRule = rbacv1.PolicyRule{
	APIGroups: []string{"*"},
	Resources: []string{"*"},
	Verbs:     []string{"*"},
}

// krateoTemplatesRule is the "real-user reads RESTActions" PolicyRule
// shape — overlaps templates.krateo.io/restactions. (ζ) MUST KEEP a
// subject bound to a role carrying this rule.
var krateoTemplatesRule = rbacv1.PolicyRule{
	APIGroups: []string{"templates.krateo.io"},
	Resources: []string{"restactions"},
	Verbs:     []string{"get", "list", "watch"},
}

// systemBasicUserRule is the upstream `system:basic-user` ClusterRole's
// rule shape (grants self-info reads, no resource access against any
// custom resource). (ζ) prunes any subject bound EXCLUSIVELY to this
// rule shape — lossless false-prune (the cohort produces no L1-hit
// content even if seeded).
var systemBasicUserRule = rbacv1.PolicyRule{
	APIGroups: []string{"authorization.k8s.io"},
	Resources: []string{"selfsubjectaccessreviews", "selfsubjectrulesreviews"},
	Verbs:     []string{"create"},
}

// buildSnapshotWithRoles is `buildSnapshot` extended with explicit
// ClusterRoles so the predicate-(ζ) PolicyRule walk has rules to
// resolve. Each role is keyed by ClusterRolesByName[name] — the same
// shape rbac/evaluate.go:411 reads at request time.
func buildSnapshotWithRoles(t *testing.T,
	crbs []*rbacv1.ClusterRoleBinding,
	rbs map[string][]*rbacv1.RoleBinding,
	clusterRoles []*rbacv1.ClusterRole,
) *RBACSnapshot {
	t.Helper()
	snap := &RBACSnapshot{
		ClusterRoleBindings: crbs,
		RoleBindingsByNS:    rbs,
		ClusterRolesByName:  map[string]*rbacv1.ClusterRole{},
		RolesByNSName:       map[string]*rbacv1.Role{},
	}
	for _, cr := range clusterRoles {
		snap.ClusterRolesByName[cr.Name] = cr
	}
	rebuildSubjectIndexes(snap)
	PublishRBACSnapshotForTest(snap)
	return snap
}

// TestBindingSetHash_StableUnderEquivalentInput — calling BindingSetHash
// twice for the same (username, groups) against the same snapshot returns
// the same value. Trivial but load-bearing: ComputeKey folds the hash in
// little-endian uint64; instability would re-bake the L1 key per request.
func TestBindingSetHash_StableUnderEquivalentInput(t *testing.T) {
	resetGenAndSnapshot(t)
	buildSnapshot(t,
		[]*rbacv1.ClusterRoleBinding{
			mkCRB("admin-bind", userSub("admin")),
			mkCRB("devs-bind", groupSub("devs")),
		},
		nil,
	)

	h1 := BindingSetHash("admin", []string{"devs"})
	h2 := BindingSetHash("admin", []string{"devs"})
	if h1 != h2 {
		t.Fatalf("BindingSetHash not stable: %#x vs %#x", h1, h2)
	}
	if h1 == 0 {
		t.Fatalf("BindingSetHash returned 0 for a cohort with matched bindings")
	}
}

// TestBindingSetHash_MatchesCohortRBACGenMechanism — AC-178.2. The hash
// returned by BindingSetHash MUST equal the value
// fnv64aPointers(collectCohortBindingPtrs(snap, u, gs+implicit-auth)) —
// same helpers, same snapshot. By construction the L1 cell the seed
// populates is the SAME cell the request-time dispatchCacheLookupKey
// hashes for a cohort member.
//
// BindingSetHash injects "system:authenticated" for authenticated users
// (mirrors evaluate.go), so the reference must inject it too.
func TestBindingSetHash_MatchesCohortRBACGenMechanism(t *testing.T) {
	resetGenAndSnapshot(t)
	snap := buildSnapshot(t,
		[]*rbacv1.ClusterRoleBinding{
			mkCRB("admin-bind", userSub("admin")),
			mkCRB("devs-bind", groupSub("devs")),
		},
		nil,
	)

	want := fnv64aPointers(collectCohortBindingPtrs(snap, "admin",
		[]string{"devs", "system:authenticated"}))
	got := BindingSetHash("admin", []string{"devs"})
	if got != want {
		t.Fatalf("AC-178.2 byte-equality fail: BindingSetHash=%#x; want fnv64aPointers(collectCohortBindingPtrs(... +implicit-auth))=%#x",
			got, want)
	}
}

// TestBindingSetHash_ShiftsOnBindingMutation — HG-178.5. Adding a new
// CRB whose Subjects include the cohort's user MUST change the hash for
// that cohort. A cohort whose binding-set is unchanged keeps the same
// hash.
func TestBindingSetHash_ShiftsOnBindingMutation(t *testing.T) {
	resetGenAndSnapshot(t)
	// Initial: admin matches one CRB; alice matches none.
	buildSnapshot(t,
		[]*rbacv1.ClusterRoleBinding{mkCRB("admin-bind", userSub("admin"))},
		nil,
	)
	hAdminBefore := BindingSetHash("admin", nil)
	hAliceBefore := BindingSetHash("alice", nil)

	// Mutate: add a SECOND CRB matching admin. alice still matches none.
	buildSnapshot(t,
		[]*rbacv1.ClusterRoleBinding{
			mkCRB("admin-bind", userSub("admin")),
			mkCRB("admin-bind-v2", userSub("admin")),
		},
		nil,
	)
	hAdminAfter := BindingSetHash("admin", nil)
	hAliceAfter := BindingSetHash("alice", nil)

	if hAdminBefore == hAdminAfter {
		t.Fatalf("HG-178.5: admin's BindingSetHash did NOT shift on a matching binding add (%#x stable)",
			hAdminAfter)
	}
	if hAliceBefore != hAliceAfter {
		t.Fatalf("HG-178.5: alice's BindingSetHash shifted despite no matching binding change (%#x -> %#x)",
			hAliceBefore, hAliceAfter)
	}
}

// TestEnumerateBindingSetClasses_EmptySnapshot — nil snapshot returns
// nil. The PIP seed caller treats nil as "no cohorts to seed".
func TestEnumerateBindingSetClasses_EmptySnapshot(t *testing.T) {
	resetGenAndSnapshot(t)
	PublishRBACSnapshotForTest(nil)
	got := EnumerateBindingSetClasses()
	if got != nil {
		t.Fatalf("EnumerateBindingSetClasses on nil snapshot: got %d classes; want nil", len(got))
	}
}

// TestEnumerateBindingSetClasses_BasicDedupe — two users on the SAME
// matching binding collapse via BindingSetHash dedupe. The invariant
// is hash-level, INDEPENDENT of predicate (ζ): two users in the same
// CRB Subjects list share the same matched binding-pointer-set and
// therefore the same hash.
func TestEnumerateBindingSetClasses_BasicDedupe(t *testing.T) {
	resetGenAndSnapshot(t)
	// Single CRB binding two distinct users in the SAME Subjects list:
	// both users share the SAME matched binding-pointer-set, so they
	// collapse to ONE class.
	sharedCRB := mkCRB("shared", userSub("alice"))
	sharedCRB.Subjects = append(sharedCRB.Subjects, userSub("bob"))

	// A separate CRB for carol only.
	carolCRB := mkCRB("carol-bind", userSub("carol"))

	buildSnapshot(t,
		[]*rbacv1.ClusterRoleBinding{sharedCRB, carolCRB},
		nil,
	)

	// Hash equality is the dedupe contract. Compute alice/bob/carol hashes
	// directly and assert alice == bob (same class) and alice != carol.
	hAlice := BindingSetHash("alice", nil)
	hBob := BindingSetHash("bob", nil)
	hCarol := BindingSetHash("carol", nil)
	if hAlice != hBob {
		t.Fatalf("BindingSetHash dedupe: alice=%#x bob=%#x; want equal (same shared CRB)", hAlice, hBob)
	}
	if hAlice == hCarol {
		t.Fatalf("BindingSetHash dedupe: alice and carol collide (%#x); want distinct", hAlice)
	}
}

// TestEnumerateBindingSetClasses_PrunesSystemAuth — the implicit
// system:authenticated group is INJECTED into every authenticated tuple's
// effective groups, but NOT part of the powerset domain. To validate
// that the enumerator surfaces an admin cohort whose Groups include
// system:authenticated, admin is bound to a wildcardRule ClusterRole
// (cyberjoker semantics) so (ζ) KEEPs the cohort even with an empty
// handlerGVRSet (the wildcard overlaps every conceivable GVR — but
// only when handlerGVRSet is non-empty).
//
// Unit tests run without a wired ResourceWatcher (Global() returns nil),
// so handlerGVRSetSnapshot returns nil. We therefore drive the test
// via the empty-handler-set defensive branch: every User-kind subject
// prunes. The PrunesSystemAuth invariant is then tested via the GROUP
// cohort (group:authenticated bound subjects) — which the Group-kind
// pass surfaces unconditionally.
func TestEnumerateBindingSetClasses_PrunesSystemAuth(t *testing.T) {
	resetGenAndSnapshot(t)
	// Use a Group-kind cohort so predicate (ζ) does not apply. The
	// `devs` group binds admin via Subjects list — but we exercise the
	// Groups path: a CRB with a Group subject `devs` produces a
	// (Username="", Groups=["devs"]) cohort post-enumeration.
	buildSnapshot(t,
		[]*rbacv1.ClusterRoleBinding{
			mkCRB("auth-bind", groupSub("system:authenticated")),
			mkCRB("devs-bind", groupSub("devs")),
		},
		nil,
	)

	out := EnumerateBindingSetClasses()
	// system:authenticated MUST NOT appear as a Username OR as a
	// standalone Groups element in any cohort — it is implicit, not
	// a discretionary discriminant.
	for _, c := range out {
		if c.Username == "system:authenticated" {
			t.Errorf("system:authenticated appeared as Username in cohort %+v", c)
		}
		// "devs" group cohort: groups should be ["devs"] (no auth
		// injection on anonymous groups — BindingSetHash injects
		// system:authenticated only for non-empty Username).
		if c.Username == "" {
			for _, g := range c.Groups {
				if g == "system:authenticated" {
					t.Errorf("system:authenticated appeared in anonymous-Group cohort %+v Groups", c)
				}
			}
		}
	}
	// Devs cohort must be present (Group-kind survives every predicate).
	foundDevs := false
	for _, c := range out {
		if c.Username == "" {
			for _, g := range c.Groups {
				if g == "devs" {
					foundDevs = true
				}
			}
		}
	}
	if !foundDevs {
		t.Fatalf("devs Group cohort missing; got %+v", out)
	}
}

// TestPrunePredicate_ZetaCorpusReal — Ship 0.30.183 headline falsifier.
//
// Inputs: the 29 production GKE control-plane User-kind names + their
// matched ClusterRoles captured LIVE in /tmp/snowplow-runs/0.30.183/
// before/cohort-enumeration-0.30.181.txt and /tmp/snowplow-runs/
// 0.30.183/before/wire-probe-zeta.txt (the latter authoritatively
// confirms the ClusterRoles' PolicyRules touch zero krateo.io
// apiGroups).
//
// 24 of the 29 subjects carry the K8s reserved-name `system:` prefix
// — they take the fast path and prune without a PolicyRule walk. The 5
// non-`system:` stragglers (cluster-autoscaler, kube-apiserver,
// kubelet, kubelet-bootstrap, kubelet-nodepool-bootstrap) take the
// full PolicyRule walk and prune via empty-intersection against the
// snowplow handler GVR set.
//
// REAL USER edge cases that MUST survive (ζ):
//   - admin → cyberjoker (APIGroups=["*"]) → wildcard overlap → KEEP.
//   - alice@krateo.io → admin-role with APIGroups=["templates.krateo.io"]
//     → overlap on restactions → KEEP.
//
// LOSSLESS FALSE-PRUNE — bob bound EXCLUSIVELY to system:basic-user
// (APIGroups=["authorization.k8s.io"]) → empty intersection → PRUNE.
// This is acceptable because system:basic-user grants no krateo.io
// resource access; the cohort produces no L1-hit content.
//
// Per-name fixture comes from LIVE RBAC (feedback_empirical_apiserver_
// probe_for_predicate_design); the predicate is generic over (name,
// refs, snap, handlerGVRSet) (feedback_no_special_cases).
func TestPrunePredicate_ZetaCorpusReal(t *testing.T) {
	resetGenAndSnapshot(t)

	// productionUserCorpus is the live cohort enumeration captured by
	// the architect's pre-flight probe. Each entry is (subject.Name,
	// matched ClusterRoleBinding RoleRefs). All 29 MUST prune.
	productionUserCorpus := []struct {
		name  string
		roles []string
	}{
		// User-kind subjects whose name LACKS the system: prefix —
		// take the PolicyRule walk and prune via empty-intersection.
		{"cluster-autoscaler", []string{"read-updateinfo"}},
		{"kube-apiserver", []string{"kubelet-api-admin"}},
		{"kubelet", []string{
			"gce:beta:kubelet-certificate-bootstrap",
			"system:node-bootstrapper",
			"system:node-problem-detector",
		}},
		{"kubelet-bootstrap", []string{
			"gce:beta:kubelet-certificate-bootstrap",
			"system:node-bootstrapper",
		}},
		{"kubelet-nodepool-bootstrap", []string{
			"gce:beta:kubelet-certificate-bootstrap",
			"system:node-bootstrapper",
		}},
		// User-kind subjects whose name CARRIES the system: prefix —
		// fast path, no PolicyRule walk.
		{"system:cloud-controller-manager", []string{
			"system:cloud-controller-manager",
			"system::leader-locking-cloud-controller-manager",
			"system:gke-ccm-migration-leader-election",
		}},
		{"system:cluster-autoscaler", []string{
			"ca-cr-actor",
			"ca-pr-beta-actor",
			"cluster-autoscaler",
		}},
		{"system:clustermetrics", []string{
			"system:clustermetrics",
		}},
		{"system:controller:glbc", []string{"system:glbc-status"}},
		{"system:gcp-controller-manager", []string{"system:gcp-controller-manager"}},
		{"system:gke-common-webhooks", []string{"system:gke-common-webhooks"}},
		{"system:gke-volume-populator-controller", []string{
			"gke-volume-populator-role",
			"gke-volume-populator-leaderelection-role",
		}},
		{"system:konnectivity-server", []string{
			"system:auth-delegator",
			"leases-writer",
		}},
		{"system:kube-controller-manager", []string{
			"system:kube-controller-manager",
			"extension-apiserver-authentication-reader",
			"system::leader-locking-kube-controller-manager",
			"system:gke-kcm-ccm-leader-election",
		}},
		{"system:kube-proxy", []string{"system:node-proxier"}},
		{"system:kube-scheduler", []string{
			"system:kube-scheduler",
			"system:volume-scheduler",
			"extension-apiserver-authentication-reader",
			"system::leader-locking-kube-scheduler",
		}},
		{"system:kubestore-collector", []string{
			"system:kubestore-collector",
			"system:kubestore-collector-leader-election",
		}},
		{"system:l4-lb-controller", []string{"system:glbc-status"}},
		{"system:l7-lb-controller", []string{"system:glbc-status"}},
		{"system:l7-lb-controller-neg", []string{"system:glbc-status"}},
		{"system:maintenance-controller", []string{"system:maintenance-controller-cluster-role"}},
		{"system:managed-certificate-controller", []string{"system:managed-certificate-controller"}},
		{"system:master-prom-to-sd-monitor", []string{"system:master-monitoring-role"}},
		{"system:metrics-server-nanny", []string{
			"system:auth-delegator",
			"system:metrics-server-nanny",
			"extension-apiserver-authentication-reader",
			"system:metrics-server-nanny-leader-election",
		}},
		{"system:node-problem-detector", []string{"system:node-problem-detector"}},
		{"system:pdcsi-controller", []string{
			"pdcsi-attacher-role",
			"pdcsi-provisioner-role",
			"pdcsi-resizer-role",
			"pdcsi-snapshotter-role",
			"pdcsi-leaderelection",
		}},
		{"system:resource-tracker", []string{"system:resource-tracker"}},
		{"system:snapshot-controller", []string{
			"snapshot-controller-runner",
			"snapshot-controller-leaderelection",
		}},
		{"system:vpa-recommender", []string{
			"external-metrics-reader",
			"system:controller:horizontal-pod-autoscaler",
			"system:gke-controller",
			"system:gke-hpa-actor",
			"system:gke-hpa-service-reader",
			"system:gke-uas-collection-reader",
			"system:gke-uas-metrics-reader",
		}},
	}

	if len(productionUserCorpus) != 29 {
		t.Fatalf("test fixture corruption: expected 29 production users, got %d", len(productionUserCorpus))
	}

	// Build the snapshot: one CRB per (subject, role) pair so the
	// snapshot indexer routes each role's RoleRef.Name into
	// CRBsByUser[subject]. Mirrors how live RBAC binds these
	// identities — one CRB per role grant.
	//
	// EVERY production ClusterRole is attached with noKrateoRule — the
	// wire-probe-zeta.txt finding (all 5 straggler roles' rules
	// confirmed). The system:-prefixed roles take the fast path so
	// their rule content is irrelevant.
	var crbs []*rbacv1.ClusterRoleBinding
	roleNameSet := map[string]struct{}{}
	bindIdx := 0
	for _, e := range productionUserCorpus {
		for _, role := range e.roles {
			bindIdx++
			crb := mkCRBWithRole("bind-prod-"+itoaTest(bindIdx), role, userSub(e.name))
			crbs = append(crbs, crb)
			roleNameSet[role] = struct{}{}
		}
	}

	// Real-user bindings: admin → cyberjoker (wildcard, KEEP);
	// alice@krateo.io → [admin-role (krateo.io-touching), system:
	// basic-user (no-krateo)] — alice KEEPs via admin-role overlap;
	// bob → system:basic-user ONLY — bob PRUNES (lossless).
	bindIdx++
	crbs = append(crbs, mkCRBWithRole("bind-admin", "cyberjoker", userSub("admin")))
	bindIdx++
	crbs = append(crbs, mkCRBWithRole("bind-alice-admin", "admin-role", userSub("alice@krateo.io")))
	bindIdx++
	crbs = append(crbs, mkCRBWithRole("bind-alice-basic", "system:basic-user", userSub("alice@krateo.io")))
	bindIdx++
	crbs = append(crbs, mkCRBWithRole("bind-bob-basic", "system:basic-user", userSub("bob")))

	// 9 Group-kind subjects — NEVER pruned by (ζ).
	groupNames := []string{
		"admins",
		"authn",
		"devs",
		"system:masters",
		"system:nodes",
		"system:monitoring",
		"system:authenticated",
		"system:serviceaccounts",
		"system:unauthenticated",
	}
	for _, g := range groupNames {
		bindIdx++
		crbs = append(crbs, mkCRB("bind-group-"+itoaTest(bindIdx), groupSub(g)))
	}

	// Build the ClusterRole set. Every production role from the
	// corpus + cyberjoker + admin-role + system:basic-user.
	var clusterRoles []*rbacv1.ClusterRole
	for roleName := range roleNameSet {
		clusterRoles = append(clusterRoles,
			mkClusterRole(roleName, []rbacv1.PolicyRule{noKrateoRule}))
	}
	clusterRoles = append(clusterRoles,
		mkClusterRole("cyberjoker", []rbacv1.PolicyRule{wildcardRule}),
		mkClusterRole("admin-role", []rbacv1.PolicyRule{krateoTemplatesRule}),
		mkClusterRole("system:basic-user", []rbacv1.PolicyRule{systemBasicUserRule}),
	)

	snap := buildSnapshotWithRoles(t, crbs, nil, clusterRoles)

	// PURE PREDICATE assertions — exercise pruneUserKindSubjectZeta
	// directly over each fixture entry with the deterministic
	// handler GVR set. This is the falsifier the architect's brief
	// specifies: every production User-kind name MUST prune.
	for _, e := range productionUserCorpus {
		refs := collectMatchedRoleRefsForUser(snap, e.name)
		got := pruneUserKindSubjectZeta(e.name, refs, snap, snowplowHandlerGVRSetForTest)
		if !got {
			t.Errorf("pruneUserKindSubjectZeta(%q, ...) = false; expected true (production control-plane identity, empty intersection)",
				e.name)
		}
	}

	// REAL USER edge cases — must SURVIVE the predicate.
	if got := pruneUserKindSubjectZeta("admin",
		collectMatchedRoleRefsForUser(snap, "admin"),
		snap, snowplowHandlerGVRSetForTest); got {
		t.Errorf("pruneUserKindSubjectZeta(admin, [cyberjoker]) = true; expected false (wildcard rule overlaps every handler)")
	}
	if got := pruneUserKindSubjectZeta("alice@krateo.io",
		collectMatchedRoleRefsForUser(snap, "alice@krateo.io"),
		snap, snowplowHandlerGVRSetForTest); got {
		t.Errorf("pruneUserKindSubjectZeta(alice@krateo.io, [admin-role, system:basic-user]) = true; expected false (admin-role overlaps templates.krateo.io/restactions)")
	}

	// BOB — LOSSLESS FALSE-PRUNE EDGE CASE. A real user bound only to
	// system:basic-user prunes via empty intersection. Documented as
	// acceptable: system:basic-user grants no krateo.io resource access.
	if got := pruneUserKindSubjectZeta("bob",
		collectMatchedRoleRefsForUser(snap, "bob"),
		snap, snowplowHandlerGVRSetForTest); !got {
		t.Errorf("pruneUserKindSubjectZeta(bob, [system:basic-user]) = false; expected true (lossless false-prune)")
	}

	// END-TO-END enumeration — install a slog buffer handler to capture
	// binding_set.prune lines emitted by EnumerateBindingSetClasses.
	// In unit tests Global() returns nil → handlerGVRSetSnapshot
	// returns nil → predicate (ζ) fires the empty-handler-set defensive
	// branch for the 5 non-system: stragglers (and admin/alice/bob).
	// The 24 system:-prefixed names take the fast path independently
	// of the handler set. We assert:
	//   - >= 24 lines with reason=system_prefix (fast path)
	//   - every prune line has subject_kind="User"
	//   - no log line for Group-kind subjects (predicate not invoked)
	prevLogger := slog.Default()
	defer slog.SetDefault(prevLogger)
	var logBuf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))

	_ = EnumerateBindingSetClasses()

	logText := logBuf.String()
	pruneLines := 0
	systemPrefixLines := 0
	for _, line := range strings.Split(strings.TrimSpace(logText), "\n") {
		if line == "" {
			continue
		}
		if !strings.Contains(line, `"msg":"binding_set.prune"`) {
			continue
		}
		pruneLines++
		if !strings.Contains(line, `"subject_kind":"User"`) {
			t.Errorf("binding_set.prune line missing subject_kind=\"User\": %s", line)
		}
		if strings.Contains(line, `"reason":"system_prefix"`) {
			systemPrefixLines++
		}
		// Every prune line must include the `reason` field (HG-183.10).
		if !strings.Contains(line, `"reason":`) {
			t.Errorf("binding_set.prune line missing reason field: %s", line)
		}
	}
	if systemPrefixLines < 24 {
		t.Errorf("expected >=24 prune lines with reason=system_prefix; got %d", systemPrefixLines)
	}
	if pruneLines < 32 {
		t.Errorf("expected >=32 binding_set.prune lines (29 production User-kind + admin/alice/bob); got %d", pruneLines)
	}
}

// TestPrunePredicate_WildcardRoleKeepsUser — A User-kind subject bound
// to a role with APIGroups=["*"] Resources=["*"] (cyberjoker semantics)
// is KEPT by predicate (ζ) regardless of the (non-empty) handler GVR
// set, because `stringSliceMatchesRBAC([]string{"*"}, anyGroup)` is
// always true. This is the wildcard-overlap correctness gate from the
// architect's brief.
func TestPrunePredicate_WildcardRoleKeepsUser(t *testing.T) {
	resetGenAndSnapshot(t)
	snap := buildSnapshotWithRoles(t,
		[]*rbacv1.ClusterRoleBinding{
			mkCRBWithRole("bind-wildcard", "wildcard-role", userSub("wildcard-user")),
		},
		nil,
		[]*rbacv1.ClusterRole{
			mkClusterRole("wildcard-role", []rbacv1.PolicyRule{wildcardRule}),
		},
	)

	refs := collectMatchedRoleRefsForUser(snap, "wildcard-user")
	if got := pruneUserKindSubjectZeta("wildcard-user", refs, snap, snowplowHandlerGVRSetForTest); got {
		t.Errorf("pruneUserKindSubjectZeta(wildcard-user, [wildcard-role]) = true; expected false (wildcard rule MUST overlap every handler GVR)")
	}
}

// TestPrunePredicate_NotInvokedForGroupOrSA — Ship 0.30.183 invariant:
// predicate (ζ) NEVER observes a Group-kind or ServiceAccount-kind
// subject. Group-kind subjects always survive enumeration (their
// own subject-kind branch); SA-kind subjects route to
// CRBsByServiceAccount and never appear in candidateUsers.
//
// Verification: scan the binding_set.prune INFO log output for any
// line whose subject_kind != "User". A non-User prune line is a
// regression — predicate (ζ) leaking outside its User-kind scope.
func TestPrunePredicate_NotInvokedForGroupOrSA(t *testing.T) {
	resetGenAndSnapshot(t)

	// One CRB per Subject-Kind variant.
	crbs := []*rbacv1.ClusterRoleBinding{
		mkCRBWithRole("user-bind", "system:reserved-role", userSub("system:something")), // User pruned via fast path
		mkCRBWithRole("group-bind", "system:masters-role", groupSub("system:masters")),  // Group — predicate must NOT see
		mkCRBWithRole("sa-bind", "system:sa-role", saSub("kube-system", "snowplow")),    // SA — predicate must NOT see
	}
	buildSnapshotWithRoles(t, crbs, nil, []*rbacv1.ClusterRole{
		mkClusterRole("system:reserved-role", []rbacv1.PolicyRule{noKrateoRule}),
		mkClusterRole("system:masters-role", []rbacv1.PolicyRule{noKrateoRule}),
		mkClusterRole("system:sa-role", []rbacv1.PolicyRule{noKrateoRule}),
	})

	prevLogger := slog.Default()
	defer slog.SetDefault(prevLogger)
	var logBuf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))

	_ = EnumerateBindingSetClasses()

	logText := logBuf.String()
	for _, line := range strings.Split(strings.TrimSpace(logText), "\n") {
		if line == "" {
			continue
		}
		if !strings.Contains(line, `"msg":"binding_set.prune"`) {
			continue
		}
		if !strings.Contains(line, `"subject_kind":"User"`) {
			t.Errorf("binding_set.prune line emitted with non-User subject_kind (predicate leaked outside User-kind scope): %s", line)
		}
	}
}

// TestPrunePredicate_EmptyHandlerSet_PrunesAll — defensive branch
// verification. With an empty handler GVR set, every non-system: User
// subject (regardless of role rules) prunes via the `handler_set_empty`
// reason. This validates the conservative fail-closed posture: when
// the watcher is unwired (cache=off, pre-readiness), no seed work is
// queued.
func TestPrunePredicate_EmptyHandlerSet_PrunesAll(t *testing.T) {
	resetGenAndSnapshot(t)
	snap := buildSnapshotWithRoles(t,
		[]*rbacv1.ClusterRoleBinding{
			mkCRBWithRole("bind-real", "wildcard-role", userSub("real-user")),
		},
		nil,
		[]*rbacv1.ClusterRole{
			mkClusterRole("wildcard-role", []rbacv1.PolicyRule{wildcardRule}),
		},
	)

	refs := collectMatchedRoleRefsForUser(snap, "real-user")
	// With handlerGVRSet=nil, even wildcard rules prune (the defensive
	// `handler_set_empty` branch fires before the PolicyRule walk).
	if got := pruneUserKindSubjectZeta("real-user", refs, snap, nil); !got {
		t.Errorf("pruneUserKindSubjectZeta(real-user, ..., handlerGVRSet=nil) = false; expected true (defensive empty-handler-set prune)")
	}
}

// TestPrunePredicate_RoleBindingRoleKind — predicate (ζ) MUST resolve
// RB roleRefs of Kind=Role against snap.RolesByNSName, not against
// ClusterRolesByName. A subject bound exclusively via a namespaced
// Role with krateo.io rules MUST be KEPT.
func TestPrunePredicate_RoleBindingRoleKind(t *testing.T) {
	resetGenAndSnapshot(t)

	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "team-a-rb"},
		Subjects:   []rbacv1.Subject{userSub("team-a-dev")},
		RoleRef:    rbacv1.RoleRef{Kind: "Role", Name: "team-a-role", APIGroup: "rbac.authorization.k8s.io"},
	}
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "team-a-role"},
		Rules:      []rbacv1.PolicyRule{krateoTemplatesRule},
	}

	snap := &RBACSnapshot{
		ClusterRoleBindings: nil,
		RoleBindingsByNS:    map[string][]*rbacv1.RoleBinding{"team-a": {rb}},
		ClusterRolesByName:  map[string]*rbacv1.ClusterRole{},
		RolesByNSName:       map[string]*rbacv1.Role{"team-a/team-a-role": role},
	}
	rebuildSubjectIndexes(snap)
	PublishRBACSnapshotForTest(snap)
	t.Cleanup(func() { PublishRBACSnapshotForTest(nil) })

	refs := collectMatchedRoleRefsForUser(snap, "team-a-dev")
	if len(refs) != 1 {
		t.Fatalf("collectMatchedRoleRefsForUser(team-a-dev): got %d refs; want 1", len(refs))
	}
	if refs[0].Kind != "Role" || refs[0].Namespace != "team-a" || refs[0].Name != "team-a-role" {
		t.Fatalf("collectMatchedRoleRefsForUser(team-a-dev): got %+v; want {Role team-a team-a-role}", refs[0])
	}
	if got := pruneUserKindSubjectZeta("team-a-dev", refs, snap, snowplowHandlerGVRSetForTest); got {
		t.Errorf("pruneUserKindSubjectZeta(team-a-dev, namespaced Role with krateo.io rules) = true; expected false (RB Role-kind resolution)")
	}
}

// itoaTest is a tiny local helper that avoids importing strconv for
// trivial counter formatting in the corpus fixture builder.
func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
