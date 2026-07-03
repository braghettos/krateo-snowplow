// phase1_content_prewarm_falsifier_test.go — Ship 0.30.125 (F2)
// falsifiers for the SA-driven content-population pass.
//
// SIX FALSIFIERS (architect's set):
//   FAL-1 — HEADLINE, on-cluster (the tester runs it on the live 50K
//           bench): an SA prewarm of compositions-list, then a /call by a
//           never-seen user → content_hit + correct RBAC-narrowed view +
//           zero resolve. Hermetically here: the harvester + Step-7.5
//           wiring + the apistage-prewarm marker plumbing are unit-proven
//           so the on-cluster FAL-1 has a sound mechanism to measure.
//   FAL-2 — no leak (on-cluster: admin vs cyberjoker divergent).
//   FAL-3 — no double-populate (the pass populates content entries; a
//           second pass / a real /call does not re-Put — covered by the
//           apistage content-key identity, here asserted via the
//           harvester dedup + the marker contract).
//   FAL-4 — 50K MaxRSS (the OOM / flag-default gate) — on-cluster; this
//           file wires the serial-parallelism + circuit-breaker hooks
//           FAL-4 measures.
//   FAL-5 — flag-off neutrality: PREWARM_CONTENT_ENABLED unset ⇒ no
//           harvester, no Step 7.5, byte-identical to 0.30.124.
//   FAL-6 — prewarm-vs-refresher race (on-cluster).
//
// The hermetic tests below prove the F2 MECHANISM the on-cluster
// falsifiers rest on: the harvester rides the walk + dedups; the
// content-prewarm SA context sets exactly the right markers (apistage-
// prewarm ON, phase1-resolution OFF so the iterator uncaps, iter-serial
// ON); iterParallelism honours the serial marker; the circuit-breaker
// threshold is read; flag-off is a clean no-op.

package dispatchers

import (
	"context"
	"testing"

	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// --- harvester: rides the walk, dedups by (ns,name) ----------------------

// f2WidgetWithApiRef builds an unstructured widget CR carrying a
// spec.apiRef pointing at a RESTAction.
func f2WidgetWithApiRef(ns, name, apiRefName, apiRefNS string) *unstructured.Unstructured {
	apiRef := map[string]any{"name": apiRefName}
	if apiRefNS != "" {
		apiRef["namespace"] = apiRefNS
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "widgets.templates.krateo.io/v1beta1",
		"kind":       "Panel",
		"metadata":   map[string]any{"namespace": ns, "name": name},
		"spec":       map[string]any{"apiRef": apiRef},
	}}
}

// TestFAL_Harvester_DedupsApiRefs proves the content-prewarm harvester
// records each widget's spec.apiRef and deduplicates by (namespace,name)
// — so a RESTAction reached from multiple widgets is resolved once.
func TestFAL_Harvester_DedupsApiRefs(t *testing.T) {
	h := newContentPrewarmHarvester()

	// Three widgets: two share the SAME apiRef (data-grid + summary on the
	// same page), one distinct.
	h.harvestApiRef(f2WidgetWithApiRef("krateo-system", "panel-a", "compositions-list", "krateo-system"))
	h.harvestApiRef(f2WidgetWithApiRef("krateo-system", "panel-b", "compositions-list", "krateo-system"))
	h.harvestApiRef(f2WidgetWithApiRef("krateo-system", "panel-c", "events-list", "krateo-system"))

	refs := h.snapshot()
	if len(refs) != 2 {
		t.Fatalf("FAL: harvester recorded %d data-source RESTActions, want 2 "+
			"(compositions-list shared by two widgets must dedup to one)", len(refs))
	}
	// Each ref must target the RESTAction GVR.
	for _, ref := range refs {
		if ref.Resource != "restactions" || ref.APIVersion != "templates.krateo.io/v1" {
			t.Fatalf("FAL: harvested ref GVR = %s/%s; want templates.krateo.io/v1 restactions",
				ref.APIVersion, ref.Resource)
		}
	}
}

// TestFAL_Harvester_ApiRefNamespaceFallback proves an apiRef with no
// explicit namespace falls back to the widget's namespace (Krateo
// convention — readApiRef).
func TestFAL_Harvester_ApiRefNamespaceFallback(t *testing.T) {
	h := newContentPrewarmHarvester()
	// apiRef carries NO namespace — must inherit the widget's ns.
	h.harvestApiRef(f2WidgetWithApiRef("team-x", "panel", "x-list", ""))
	refs := h.snapshot()
	if len(refs) != 1 {
		t.Fatalf("FAL: want 1 harvested ref, got %d", len(refs))
	}
	if refs[0].Namespace != "team-x" {
		t.Fatalf("FAL: apiRef with no namespace must inherit the widget ns "+
			"(team-x); got %q", refs[0].Namespace)
	}
}

// TestFAL_Harvester_NilSafe proves a nil harvester (the flag-off Phase-1
// path) and a widget with no apiRef are clean no-ops — never a panic.
func TestFAL_Harvester_NilSafe(t *testing.T) {
	var nilH *contentPrewarmHarvester
	// nil harvester — must not panic.
	nilH.harvestApiRef(f2WidgetWithApiRef("ns", "w", "ra", "ns"))
	if got := nilH.snapshot(); got != nil {
		t.Fatalf("FAL: nil harvester snapshot must be nil, got %v", got)
	}
	// widget with no apiRef — no-op.
	h := newContentPrewarmHarvester()
	noApiRef := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "widgets.templates.krateo.io/v1beta1",
		"kind":       "Panel",
		"metadata":   map[string]any{"namespace": "ns", "name": "w"},
		"spec":       map[string]any{},
	}}
	h.harvestApiRef(noApiRef)
	if got := h.snapshot(); len(got) != 0 {
		t.Fatalf("FAL: a widget with no apiRef must harvest nothing, got %d", len(got))
	}
}

// --- the content-prewarm SA context: the THREE markers ------------------

// TestFAL_ContentPrewarmSAContext_Markers is the load-bearing mechanism
// proof. withContentPrewarmSAContext MUST set exactly:
//   - cache.ApistagePrewarm ON  — so apistageContentServe populates the
//     identity-free content L1 and skips the per-user gate;
//   - cache.PrewarmIterSerial ON — so iterParallelism returns 1 (OOM
//     mitigation 2 — the uncapped fan-out runs serially).
//
// (Ship 0.30.127 removed the cache.WithPhase1Resolution marker — the
// iterator cap it gated is deleted, so a "Phase1Resolution OFF"
// assertion is no longer meaningful and was dropped.)
func TestFAL_ContentPrewarmSAContext_Markers(t *testing.T) {
	saEP := endpoints.Endpoint{ServerURL: "https://kubernetes.default.svc"}
	ctx := withContentPrewarmSAContext(context.Background(), saEP, nil)

	if !cache.ApistagePrewarmFromContext(ctx) {
		t.Fatalf("FAL: content-prewarm ctx must set ApistagePrewarm — without it "+
			"apistageContentServe would not populate the content L1")
	}
	if !cache.PrewarmIterSerialFromContext(ctx) {
		t.Fatalf("FAL: content-prewarm ctx must set PrewarmIterSerial — without it "+
			"the uncapped iterator fan-out runs parallel and blows peak RSS "+
			"(OOM mitigation 2)")
	}
}

// TestFAL_ContrastPhase1SAContext confirms the DELIBERATE difference vs
// the discovery walk's context: withPhase1SAContext does NOT set the
// content-prewarm markers. Fork B (0.30.127): the discovery walk runs at
// the resolver's default bounded parallelism — it must NOT carry
// PrewarmIterSerial, which is the content pass's serial marker.
//
// (Ship 0.30.127 removed cache.WithPhase1Resolution — the iterator cap
// it gated is gone — so the former "discovery context sets
// Phase1Resolution" assertion was dropped.)
func TestFAL_ContrastPhase1SAContext(t *testing.T) {
	saEP := endpoints.Endpoint{ServerURL: "https://kubernetes.default.svc"}
	ctx := withPhase1SAContext(context.Background(), saEP, nil)

	if cache.ApistagePrewarmFromContext(ctx) {
		t.Fatalf("FAL: the discovery-walk context must NOT set ApistagePrewarm")
	}
	if cache.PrewarmIterSerialFromContext(ctx) {
		t.Fatalf("FAL: the discovery-walk context must NOT set PrewarmIterSerial "+
			"(Fork B — discovery runs default-bounded-parallel, not serial)")
	}
}

// --- FAL-5 — implicit-on-cache (REWORKED for the 2026-07-03 family fold) -----

// TestFAL5_ContentImplicitOnCache — POST-FOLD (docs/prewarm-engine-implicit-on-
// cache-2026-07-03.md): PREWARM_CONTENT_ENABLED is RETIRED; PrewarmContentEnabled()
// is now implicit-on-cache. Pre-fold this asserted the opt-in "only \"true\"
// enables" semantics (now void). New contract: content ON iff CACHE_ENABLED,
// OFF iff cache off, and the retired env has NO effect.
func TestFAL5_ContentImplicitOnCache(t *testing.T) {
	// Cache off → content off, regardless of any (retired) content env value.
	t.Setenv("CACHE_ENABLED", "false")
	for _, v := range []string{"", "false", "true", "garbage"} {
		t.Setenv("PREWARM_CONTENT_ENABLED", v)
		if PrewarmContentEnabled() {
			t.Fatalf("FAL-5: content must be OFF when CACHE_ENABLED=false (retired env=%q ignored)", v)
		}
	}
	// Cache on → content ON (implicit), regardless of the retired env value.
	t.Setenv("CACHE_ENABLED", "true")
	for _, v := range []string{"", "false", "true", "garbage"} {
		t.Setenv("PREWARM_CONTENT_ENABLED", v)
		if !PrewarmContentEnabled() {
			t.Fatalf("FAL-5: content must be ON when CACHE_ENABLED=true (implicit-on-cache; retired env=%q ignored)", v)
		}
	}
}

// TestFAL5_EmptyHarvestIsCleanNoOp proves runContentPrewarmPass over an
// EMPTY harvester (no apiRefs harvested — e.g. a navigation with no
// data-source widgets) is a clean no-op, never a panic / error.
func TestFAL5_EmptyHarvestIsCleanNoOp(t *testing.T) {
	h := newContentPrewarmHarvester()
	// No harvestApiRef calls — the set is empty.
	saEP := endpoints.Endpoint{ServerURL: "https://kubernetes.default.svc"}
	// Must return cleanly (logs "content.prewarm.skipped"); no panic.
	runContentPrewarmPass(context.Background(), h, saEP, nil, "krateo-system")
}

// --- circuit-breaker threshold ------------------------------------------

// TestFAL_CircuitBreakerThreshold proves PREWARM_CONTENT_MAX_BYTES is read
// with the ~32 MiB default and an env override.
func TestFAL_CircuitBreakerThreshold(t *testing.T) {
	t.Setenv("PREWARM_CONTENT_MAX_BYTES", "")
	if got := prewarmContentMaxBytes(); got != defaultPrewarmContentMaxBytes {
		t.Fatalf("FAL: default PREWARM_CONTENT_MAX_BYTES = %d, want %d",
			got, defaultPrewarmContentMaxBytes)
	}
	t.Setenv("PREWARM_CONTENT_MAX_BYTES", "1048576")
	if got := prewarmContentMaxBytes(); got != 1048576 {
		t.Fatalf("FAL: PREWARM_CONTENT_MAX_BYTES override = %d, want 1048576", got)
	}
}
