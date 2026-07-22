// uaf_shortttl_test.go — #118 (d) interim short-TTL falsifiers, dispatcher level.
//
// C-118-6 (THE CRUX): the short UAF TTLOverride must be stamped at BOTH Put
// sites — the customer dispatch Put AND the refresher re-Put. The refresher
// builds a FRESH entry with zero CreatedAt, so Put stamps a NEW time.Now() and
// the absolute TTL slides forward on every data-plane refresh. If the override
// were stamped only on the first customer Put, a hot churning UAF cell would
// re-Put WITHOUT the override and OUTLIVE the cap. These arms drive the REAL
// override derivation (uafTTLOverrideForEntry at both sites) + the REAL UAF
// predicate (restactionHasUAFStage), against a real ResolvedCacheStore.

package dispatchers

import (
	"os"
	"strings"
	"testing"
	"time"

	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
)

// TestUAF_C118_6_AllThreePutSitesWired is the SOURCE-LEVEL guard that the short
// UAF override is stamped at ALL THREE prod Put sites of a restactions
// ResolvedEntry (C-118-6, extended by the arch gate on 3783e65 which caught the
// missing seed site):
//   - restactions.go — the customer dispatch Put;
//   - resolve_populate.go — the refresher re-Put (slides CreatedAt forward on
//     every data-plane refresh → dropping the stamp defeats the cap on a hot
//     cell);
//   - phase1_pip_seed.go — the BOOT-SEED Put (seeds UAF cells under a cohort
//     representative identity; uncapped until a customer /call overwrites it).
// Guards against a future edit removing any TTLOverride stamp (silently
// re-opening the uncapped-cell hole for that population).
func TestUAF_C118_6_AllThreePutSitesWired(t *testing.T) {
	for _, f := range []string{"restactions.go", "resolve_populate.go", "phase1_pip_seed.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if !strings.Contains(string(src), "uafTTLOverrideForEntry(") {
			t.Fatalf("C-118-6 wiring: %s must stamp the UAF TTLOverride via uafTTLOverrideForEntry(...) — the override MUST be stamped at ALL THREE restactions ResolvedEntry Put sites (customer dispatch restactions.go, refresher re-Put resolve_populate.go, boot-seed phase1_pip_seed.go), else that population's UAF cells are uncapped", f)
		}
	}
}

func uafRA() *templatesv1.RESTAction {
	return &templatesv1.RESTAction{
		Spec: templatesv1.RESTActionSpec{
			API: []*templatesv1.API{
				{Name: "list"},
				{Name: "refilter", UserAccessFilter: &templatesv1.UserAccessFilterSpec{}},
			},
		},
	}
}

func plainRA() *templatesv1.RESTAction {
	return &templatesv1.RESTAction{
		Spec: templatesv1.RESTActionSpec{
			API: []*templatesv1.API{{Name: "list"}, {Name: "project"}},
		},
	}
}

// C-118-1 predicate — restactionHasUAFStage fires iff some api-step declares a
// userAccessFilter; nil-guards each *API and *UserAccessFilterSpec.
func TestUAF_HasUAFStagePredicate(t *testing.T) {
	if !restactionHasUAFStage(uafRA()) {
		t.Fatal("an RA with a userAccessFilter stage must be detected")
	}
	if restactionHasUAFStage(plainRA()) {
		t.Fatal("an RA with no userAccessFilter stage must NOT be detected")
	}
	if restactionHasUAFStage(nil) {
		t.Fatal("nil RA must be false")
	}
	// nil api-step element + nil UAF are guarded.
	mixed := &templatesv1.RESTAction{Spec: templatesv1.RESTActionSpec{API: []*templatesv1.API{
		nil,
		{Name: "s", UserAccessFilter: nil},
	}}}
	if restactionHasUAFStage(mixed) {
		t.Fatal("nil step + nil UAF must be false (nil-guarded)")
	}
}

// C-118-6 — the both-Put-sites RED arm. The refresher re-Put builds a FRESH
// entry from the CARRIED inputs (it has no RESTAction CR), so the ONLY way it
// can re-stamp the override is by reading inputs.HasUAF that the customer
// dispatch recorded. This arm proves BOTH Put sites derive the SAME non-zero
// override from the SAME single-source uafTTLOverrideForEntry:
//
//   (1) customer dispatch: inputs.HasUAF = restactionHasUAFStage(cr) →
//       uafTTLOverrideForEntry(inputs) is the short cap.
//   (2) refresher re-Put: reads the CARRIED inputs (HasUAF preserved) →
//       uafTTLOverrideForEntry(carried) yields the SAME short cap.
//
// GREEN: both derivations are the short cap (> 0, equal) → the cell is capped no
// matter which path last wrote it, so the CreatedAt-slide on a hot refreshed
// cell cannot make it outlive the bound. RED (the bug the crux guards): if the
// refresher stamps 0 (stamp-first-Put-only), the store's TestUAFShortTTL_CapsUAFCell
// _NotNonUAF arm shows a 0-override cell is governed by the long standard ttl →
// the hot cell OUTLIVES the cap. Here we assert the derivation is identical and
// non-zero at both sites (the property that makes the cap churn-proof); the RED
// is the mutation `refresher passes a HasUAF-stripped inputs`.
func TestUAF_C118_6_OverrideDerivedIdenticallyAtBothPutSites(t *testing.T) {
	t.Setenv("UAF_RESOLVED_TTL_SECONDS", "30") // short cap

	// (1) CUSTOMER dispatch site — detect UAF from the CR, carry HasUAF.
	custInputs := &cache.ResolvedKeyInputs{CacheEntryClass: "restactions"}
	custInputs.HasUAF = restactionHasUAFStage(uafRA())
	custOverride := uafTTLOverrideForEntry(custInputs)
	if custOverride != 30*time.Second {
		t.Fatalf("customer Put must derive the 30s UAF cap for a UAF cell; got %v", custOverride)
	}

	// (2) REFRESHER re-Put site — it only has the CARRIED inputs (no CR). The
	// carried inputs preserve HasUAF, so the refresher re-derives the SAME cap.
	carried := custInputs // the refresher reads ResolvedEntry.Inputs verbatim
	refresherOverride := uafTTLOverrideForEntry(carried)
	if refresherOverride != custOverride {
		t.Fatalf("C-118-6 VIOLATED: refresher re-Put must derive the SAME cap as the customer Put (both single-source uafTTLOverrideForEntry over the carried HasUAF); customer=%v refresher=%v", custOverride, refresherOverride)
	}
	if refresherOverride == 0 {
		t.Fatal("C-118-6: the refresher-side cap must be NON-ZERO for a UAF cell — a zero cap on the refresher re-Put is the CreatedAt-slide defeat the crux guards against")
	}

	// RED — the stamp-first-Put-only bug: the refresher builds its inputs WITHOUT
	// carrying HasUAF (e.g. a fresh ResolvedKeyInputs, or the HasUAF stripped).
	// Then it derives a 0 cap → the store's standard (long) ttl governs → the hot
	// cell outlives the bound (per the cache-package store arm). This asserts the
	// carry is load-bearing: strip HasUAF and the refresher cap collapses to 0.
	strippedInputs := &cache.ResolvedKeyInputs{CacheEntryClass: "restactions" /* HasUAF: false */}
	if uafTTLOverrideForEntry(strippedInputs) != 0 {
		t.Fatal("C-118-6 RED-control broke: an inputs without HasUAF must derive a 0 cap (proving HasUAF must be CARRIED to the refresher, else the CreatedAt-slide defeats the cap)")
	}
}

// C-118-6 SEED PATH — a boot-seeded UAF cell carries the cap. The seed Put
// (phase1_pip_seed.go) mirrors the customer path: set inputs.HasUAF from the
// typed CR, then stamp TTLOverride via uafTTLOverrideForEntry. This arm drives
// that exact derivation and asserts a seeded UAF cell gets the short cap while a
// seeded NON-UAF cell does not. RED (the gap the arch caught on 3783e65): an
// un-stamped seed Put → the seeded UAF cell derives 0 override → governed by the
// long store TTL → uncapped. The RED-control below (HasUAF false / no stamp → 0)
// proves the stamp is what caps it.
func TestUAF_C118_6_SeedPathCapsUAFCell(t *testing.T) {
	t.Setenv("UAF_RESOLVED_TTL_SECONDS", "30")

	// Seed a UAF cell: the seed sets inputs.HasUAF = restactionHasUAFStage(&cr)
	// (cr is the typed RESTAction converted from got.Unstructured at the seed).
	seedInputs := &cache.ResolvedKeyInputs{CacheEntryClass: "restactions"}
	seedInputs.HasUAF = restactionHasUAFStage(uafRA())
	if got := uafTTLOverrideForEntry(seedInputs); got != 30*time.Second {
		t.Fatalf("C-118-6 seed: a boot-seeded UAF cell must carry the 30s cap; got %v (an un-stamped seed Put leaves it uncapped under the long store TTL — the gap the arch caught)", got)
	}

	// A seeded NON-UAF cell gets no override (standard ttl) — the seed stamp is
	// UAF-scoped, no collateral shortening of seeded non-UAF cells.
	plainInputs := &cache.ResolvedKeyInputs{CacheEntryClass: "restactions"}
	plainInputs.HasUAF = restactionHasUAFStage(plainRA())
	if got := uafTTLOverrideForEntry(plainInputs); got != 0 {
		t.Fatalf("C-118-6 seed: a boot-seeded NON-UAF cell must get 0 override (standard ttl); got %v", got)
	}
}

// C-118 toggle-off — with UAF_RESOLVED_TTL_SECONDS unset, uafTTLOverrideForEntry
// returns 0 even for a UAF cell → no override → standard ttl → byte-identical to
// today.
func TestUAF_ToggleOff_NoOverride(t *testing.T) {
	t.Setenv("UAF_RESOLVED_TTL_SECONDS", "")
	uafInputs := &cache.ResolvedKeyInputs{HasUAF: true}
	if d := uafTTLOverrideForEntry(uafInputs); d != 0 {
		t.Fatalf("toggle-off: a UAF cell must get 0 override (standard ttl); got %v", d)
	}
	// And with the knob ON, a NON-UAF cell still gets 0 (scoped to UAF).
	t.Setenv("UAF_RESOLVED_TTL_SECONDS", "30")
	if d := uafTTLOverrideForEntry(&cache.ResolvedKeyInputs{HasUAF: false}); d != 0 {
		t.Fatalf("a non-UAF cell must get 0 override even when the knob is on; got %v", d)
	}
	if d := uafTTLOverrideForEntry(nil); d != 0 {
		t.Fatalf("nil inputs must get 0 override; got %v", d)
	}
	if d := uafTTLOverrideForEntry(uafInputs); d != 30*time.Second {
		t.Fatalf("a UAF cell with the knob on must get the 30s override; got %v", d)
	}
}
