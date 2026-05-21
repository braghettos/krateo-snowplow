// widget_content_bench_test.go — Ship G (0.30.16x): empirical overhead
// measurement for gateWidgetEnvelope. Per the design's AC-G.9 sub-claim,
// the per-hit gate cost is sub-microsecond/item × ~200 items < 50ms p99.
// This benchmark measures the gate on a synthetic envelope shaped like a
// real navigation widget (Dashboard panel: ~20 resourcesRefs items).
//
// Run: go test -bench BenchmarkGateWidgetEnvelope -run NONE ./internal/handlers/dispatchers/...
//
// Captured in the diff-review package per workflow step 4.

package dispatchers

import (
	"context"
	"testing"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/jwtutil"
)

func BenchmarkGateWidgetEnvelope_20Items(b *testing.B) {
	items := make([]map[string]any, 20)
	for i := 0; i < 20; i++ {
		items[i] = map[string]any{
			"id":      "item-" + string(rune('a'+i%26)),
			"path":    "/call?resource=compositions&apiVersion=composition.krateo.io/v1&namespace=fireworks-app&name=comp-x",
			"verb":    "GET",
			"allowed": true,
		}
	}
	raw := fakeEnvelope(items)
	ctx := xcontext.BuildContext(context.Background(),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: "admin", Groups: []string{"system:authenticated"}}))

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, ok := gateWidgetEnvelope(ctx, raw)
		if !ok {
			b.Fatal("gate failed")
		}
	}
}

func BenchmarkGateWidgetEnvelope_200Items(b *testing.B) {
	items := make([]map[string]any, 200)
	for i := 0; i < 200; i++ {
		items[i] = map[string]any{
			"id":      "item-" + string(rune('a'+i%26)),
			"path":    "/call?resource=compositions&apiVersion=composition.krateo.io/v1&namespace=fireworks-app&name=comp-x",
			"verb":    "GET",
			"allowed": true,
		}
	}
	raw := fakeEnvelope(items)
	ctx := xcontext.BuildContext(context.Background(),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: "admin", Groups: []string{"system:authenticated"}}))

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, ok := gateWidgetEnvelope(ctx, raw)
		if !ok {
			b.Fatal("gate failed")
		}
	}
}
