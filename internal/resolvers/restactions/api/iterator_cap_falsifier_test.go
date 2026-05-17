// iterator_cap_falsifier_test.go — Ship 0.30.111 pre-flight falsifier F2.
//
// Team rule feedback_falsifier_first_before_ship: written BEFORE the
// production fix; the "capped" leg MUST fail against the unfixed
// createRequestOptions.
//
//   F2 — Phase-1 iterator cap. createRequestOptions resolving an
//        iterator-bearing RESTAction api stage:
//          - under a real-`/call`-style context → expands the iterator
//            FULLY (all N elements);
//          - under a cache.WithPhase1Resolution context → the iterator
//            fan-out is capped at phase1IteratorCap.
//
//        The "capped" half FAILS today: no cap exists, so the Phase-1
//        context expands to all N elements exactly like the real call.
//
// The UAF-stage leg exercises the same cap on an api stage that also
// declares a userAccessFilter — the cap is upstream of UAF dispatch and
// must fire regardless.

package api

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/krateoplatformops/plumbing/ptr"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
)

// iterCapTestN is the iterator element count — deliberately well above
// any plausible phase1IteratorCap (2-4) so the cap is unambiguous.
const iterCapTestN = 50

// iterCapDict builds a dict whose `namespaces` key holds iterCapTestN
// distinct namespace strings — the iterator source.
func iterCapDict() map[string]any {
	ns := make([]any, 0, iterCapTestN)
	for i := 0; i < iterCapTestN; i++ {
		ns = append(ns, "bench-ns-"+itoaIterCap(i))
	}
	return map[string]any{"namespaces": ns}
}

// iterCapStage builds an iterator-bearing api stage mirroring the
// compositions-panels stage-2 shape: a dependsOn iterator over
// `.namespaces`, one templated per-namespace path per element. uaf
// controls whether the stage also declares a userAccessFilter.
func iterCapStage(uaf bool) *templates.API {
	a := &templates.API{
		Name: "compositionspanels",
		Path: `${ "/apis/widgets.templates.krateo.io/v1beta1/namespaces/" + (.) + "/panels" }`,
		DependsOn: &templates.Dependency{
			Name:     "namespaces",
			Iterator: ptr.To(".namespaces"),
		},
	}
	if uaf {
		a.UserAccessFilter = &templates.UserAccessFilterSpec{
			Verb:          "list",
			Group:         "",
			Resource:      "namespaces",
			NamespaceFrom: ".",
		}
	}
	return a
}

func iterCapLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestFalsifierF2_IteratorCap_RealCallExpandsFully is the always-true
// control: a real-`/call`-style context (no Phase-1 marker) expands the
// iterator to ALL iterCapTestN elements. This leg passes today AND after
// the fix — the cap must never touch a real call.
func TestFalsifierF2_IteratorCap_RealCallExpandsFully(t *testing.T) {
	log := iterCapLogger()
	ctx := context.Background() // a real /call carries no Phase-1 marker

	all := createRequestOptions(ctx, log, iterCapStage(false), iterCapDict())
	if len(all) != iterCapTestN {
		t.Fatalf("F2-real: real-call iterator expanded to %d elements; want %d (FULL expansion)",
			len(all), iterCapTestN)
	}
}

// TestFalsifierF2_IteratorCap_Phase1Capped is the falsifier proper: a
// cache.WithPhase1Resolution context must cap the iterator fan-out at
// phase1IteratorCap.
//
// FAILS today: no cap exists, so the Phase-1 context expands to all
// iterCapTestN elements.
func TestFalsifierF2_IteratorCap_Phase1Capped(t *testing.T) {
	log := iterCapLogger()
	ctx := cache.WithPhase1Resolution(context.Background())

	all := createRequestOptions(ctx, log, iterCapStage(false), iterCapDict())
	if len(all) > phase1IteratorCap {
		t.Fatalf("F2-cap: under WithPhase1Resolution the iterator expanded to %d elements; "+
			"want <= phase1IteratorCap=%d — no Phase-1 iterator cap exists",
			len(all), phase1IteratorCap)
	}
	if len(all) == 0 {
		t.Fatalf("F2-cap: cap collapsed the iterator to 0 elements; warmup needs >=1 to discover the informer")
	}
}

// TestFalsifierF2_IteratorCap_Phase1CappedWithUAF asserts the cap also
// fires for an iterator stage that declares a userAccessFilter — the cap
// is upstream of UAF dispatch.
func TestFalsifierF2_IteratorCap_Phase1CappedWithUAF(t *testing.T) {
	log := iterCapLogger()
	ctx := cache.WithPhase1Resolution(context.Background())

	all := createRequestOptions(ctx, log, iterCapStage(true), iterCapDict())
	if len(all) > phase1IteratorCap {
		t.Fatalf("F2-cap-uaf: under WithPhase1Resolution a UAF iterator stage expanded to %d "+
			"elements; want <= phase1IteratorCap=%d", len(all), phase1IteratorCap)
	}
}

func itoaIterCap(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
