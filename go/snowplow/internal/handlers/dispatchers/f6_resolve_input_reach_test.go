// f6_resolve_input_reach_test.go — L12 (coverage-audit): the F6 request-extras
// allowlist is KEY-ONLY. An UNDECLARED request extra (not in spec.keyExtras) is
// DROPPED from the cache KEY (filterDeclaredKeyExtras / effectiveKeyExtras) but
// STILL REACHES the resolve INPUT — widgets.go hands widgets.Resolve the RAW,
// unfiltered extras, so a widget's widgetDataTemplate jq that reads an undeclared
// extra still renders correctly (the design's deliberate "resolve sees
// everything, key sees only declared" split, helpers.go filterDeclaredKeyExtras
// doc §KEY-ONLY).
//
// This is the resolve-side complement of the farch_f6_keyextras_test.go arms
// (which pin the KEY side — undeclared extras drop from the key). Here we DRIVE
// the REAL widgets.Resolve over a CR whose jq echoes extras.debugFlag while
// declaring NO keyExtras, and assert:
//
//	(1) RESOLVE INPUT reach: the rendered body echoes debugFlag=on (the raw extra
//	    reached widgets.Resolve's jq dict), AND
//	(2) KEY exclusion: effectiveKeyExtras / filterDeclaredKeyExtras DROP debugFlag
//	    from the key material (it does not partition the cell).
//
// RED arm (resolve path filters extras by the keyExtras allowlist): if widgets.go
// mistakenly passed the FILTERED (declared-only) extras to widgets.Resolve — i.e.
// applied the key-side allowlist to the resolve input — debugFlag would be absent
// from the dict and the rendered body would NOT echo it. TestL12_RED_FilteredExtrasDropEcho
// drives resolve with the key-side-FILTERED extras and asserts the echo VANISHES,
// proving arm (1) discriminates the "resolve filters by keyExtras" regression.

package dispatchers

import (
	"testing"
)

// widgetCRDebugEcho builds a widget CR that declares NO spec.keyExtras and whose
// widgetDataTemplate echoes the request extra `.debugFlag` into
// status.widgetData.echoedDebug via jq. No apiRef (hermetic — no apiserver).
func widgetCRDebugEcho() map[string]any {
	return map[string]any{"spec": map[string]any{
		// NO keyExtras → debugFlag is UNDECLARED (must drop from the key).
		"widgetData": map[string]any{},
		"widgetDataTemplate": []any{
			map[string]any{"forPath": ".echoedDebug", "expression": "${ .debugFlag }"},
		},
	}}
}

// TestL12_UndeclaredExtra_ReachesResolveInput_NotKey — the make-or-break L12
// arm. debugFlag is undeclared: it is DROPPED from the key but REACHES the
// resolve input (echoed in the rendered body).
func TestL12_UndeclaredExtra_ReachesResolveInput_NotKey(t *testing.T) {
	cr := widgetCRDebugEcho()
	ctx := ctxAsIdentity("alice", "devs") // farch_identity_contract_test.go helper
	requestExtras := map[string]any{"debugFlag": "on"}

	// (1) RESOLVE INPUT reach — drive the REAL widgets.Resolve with the RAW
	// request extras (exactly what widgets.go passes to the resolver: unfiltered).
	wd := resolveRenderedWidgetData(t, ctx, cr, requestExtras)
	if wd["echoedDebug"] != "on" {
		t.Fatalf("L12 (resolve-input reach): the undeclared extra debugFlag must REACH the resolve input and echo into the body; status.widgetData=%#v — widgets.go must pass the RAW extras to widgets.Resolve", wd)
	}

	// (2) KEY exclusion — the SAME undeclared extra must be DROPPED from the key
	// material. filterDeclaredKeyExtras (no declaration → drop everything) and the
	// full effectiveKeyExtras must both exclude debugFlag.
	filtered := filterDeclaredKeyExtras(cr, requestExtras)
	if _, present := filtered["debugFlag"]; present {
		t.Fatalf("L12 (key exclusion): filterDeclaredKeyExtras must DROP an undeclared extra from the key; got %#v", filtered)
	}
	keyExtras := effectiveKeyExtras(ctx, cr, requestExtras)
	if _, present := keyExtras["debugFlag"]; present {
		t.Fatalf("L12 (key exclusion): effectiveKeyExtras must NOT fold an undeclared extra into the key; got %#v", keyExtras)
	}

	// The two halves together are the KEY-ONLY property: the extra shaped the
	// BODY (resolve input) but not the KEY. That is exactly why widgets.go pairs
	// the fold-nothing key with the F6 self-quarantine Put-gate — the body is
	// correct-but-unshareable, so it must not be Put into the shared cohort cell.
}

// TestL12_DeclaredExtra_FoldsIntoKey_Control — control: once the widget DECLARES
// keyExtras:[debugFlag], the SAME extra BOTH reaches the resolve input AND folds
// into the key (it now legitimately partitions the cell). Proves the drop in the
// main arm is caused by the MISSING declaration, not by the extra being special.
func TestL12_DeclaredExtra_FoldsIntoKey_Control(t *testing.T) {
	cr := widgetCRDebugEcho()
	// Declare debugFlag now.
	spec := cr["spec"].(map[string]any)
	spec["keyExtras"] = []any{"debugFlag"}

	ctx := ctxAsIdentity("alice", "devs")
	requestExtras := map[string]any{"debugFlag": "on"}

	// Still reaches the resolve input (unchanged — resolve always sees raw extras).
	wd := resolveRenderedWidgetData(t, ctx, cr, requestExtras)
	if wd["echoedDebug"] != "on" {
		t.Fatalf("control: a declared extra must still reach the resolve input; wd=%#v", wd)
	}
	// Now it DOES fold into the key.
	keyExtras := effectiveKeyExtras(ctx, cr, requestExtras)
	if keyExtras["debugFlag"] != "on" {
		t.Fatalf("control: a DECLARED extra must fold into the key material; got %#v", keyExtras)
	}
}

// TestL12_RED_FilteredExtrasDropEcho proves arm (1) is DISCRIMINATING. If the
// resolve path had (wrongly) filtered the extras by the key-side allowlist
// before handing them to widgets.Resolve, debugFlag would be gone from the dict
// and the body would NOT echo it. Driving resolve with the key-side-FILTERED
// extras (the regression's input) makes the echo VANISH — the exact RED the
// main arm's GREEN echo excludes.
func TestL12_RED_FilteredExtrasDropEcho(t *testing.T) {
	cr := widgetCRDebugEcho() // no keyExtras
	ctx := ctxAsIdentity("alice", "devs")
	requestExtras := map[string]any{"debugFlag": "on"}

	// The key-side allowlist output an undeclared-widget would produce = {}.
	filtered := filterDeclaredKeyExtras(cr, requestExtras)
	if len(filtered) != 0 {
		t.Fatalf("setup: an undeclared widget's key-side filter must be empty; got %#v", filtered)
	}

	// Drive resolve with the FILTERED extras (the regression: resolve fed the
	// key-side allowlist). The echo must VANISH — this is the wrong impl.
	wd := resolveRenderedWidgetData(t, ctx, cr, filtered)
	if wd["echoedDebug"] == "on" {
		t.Fatalf("RED arm inconsistent: feeding the FILTERED extras must DROP the echo (that IS the regression); wd=%#v", wd)
	}
	// And the GREEN main arm proves the prod path (raw extras) keeps the echo —
	// so "resolve filters by keyExtras" is a real, detectable defect.
}
