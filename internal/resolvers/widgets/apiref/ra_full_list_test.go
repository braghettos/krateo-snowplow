// ra_full_list_test.go — Ship 4a (0.30.198) falsifiers for the apiRef-
// chokepoint page-independent serve path.
//
// These exercise raFullListServe with a STUBBED resolve closure (no live
// cluster) so the hit / first-sight-verify / fallback / repopulate paths +
// the cache-off byte-identity are provable in isolation. The pure slice/key/
// memo logic is covered in internal/cache/ra_full_list_slice_test.go.

package apiref

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/jwtutil"
	"github.com/krateoplatformops/plumbing/jqutil"
	"github.com/krateoplatformops/plumbing/ptr"
	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// raSliceJQ — the compositions-panels RESTAction's top-level slice filter.
const raSliceJQ = `
{
  compositionspanels: (
    (.compositionspanels // []) as $items
    | ($items | sort_by(.metadata.creationTimestamp // "") | reverse) as $sorted
    | (.slice.offset  // 0)                 as $offset
    | (.slice.perPage // ($sorted | length)) as $perPage
    | [ $sorted | length as $len | range($offset; $offset + $perPage) | select(. < $len) | $sorted[.] ]
  )
}
`

func panelDict(n int) map[string]any {
	items := make([]any, n)
	for i := 0; i < n; i++ {
		items[i] = map[string]any{"metadata": map[string]any{
			"name":              padName(i),
			"creationTimestamp": tsName(i),
		}}
	}
	return map[string]any{"compositionspanels": items}
}
func padName(i int) string { return "panel-" + itoa3(i) }
func tsName(i int) string  { return "2026-01-" + itoa2(i+1) + "T00:00:00Z" }
func itoa2(i int) string {
	if i < 10 {
		return "0" + string(rune('0'+i))
	}
	return string(rune('0'+i/10)) + string(rune('0'+i%10))
}
func itoa3(i int) string {
	return string(rune('0'+(i/100)%10)) + string(rune('0'+(i/10)%10)) + string(rune('0'+i%10))
}

// stubResolveRA returns a resolveRA closure backed by an in-memory panel set,
// running the REAL RA slice jq. It counts invocations so a test can assert
// the cheap hit path does NO resolve.
func stubResolveRA(t *testing.T, panels map[string]any, calls *atomic.Int64) func(context.Context, int, int) (map[string]any, error) {
	return func(_ context.Context, perPage, page int) (map[string]any, error) {
		calls.Add(1)
		dict := map[string]any{}
		for k, v := range panels {
			dict[k] = v
		}
		if perPage > 0 && page > 0 {
			dict["slice"] = map[string]any{
				"perPage": float64(perPage),
				"page":    float64(page),
				"offset":  float64((page - 1) * perPage),
			}
		}
		s, err := jqutil.Eval(t.Context(), jqutil.EvalOptions{Query: raSliceJQ, Data: dict})
		if err != nil {
			return nil, err
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(s), &out); err != nil {
			return nil, err
		}
		return out, nil
	}
}

func ctxWithUser(t *testing.T) context.Context {
	return xcontext.BuildContext(t.Context(),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: "admin", Groups: []string{"system:masters"}}))
}

func ra(jq string) *templatesv1.RESTAction {
	return &templatesv1.RESTAction{Spec: templatesv1.RESTActionSpec{Filter: ptr.To(jq)}}
}

func gvr() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "templates.krateo.io", Version: "v1", Resource: "restactions"}
}

// HG-4a.serve.1 — first-sight VERIFY → Put + serve Go-slice; second /call at a
// DIFFERENT page is a cheap HIT (no resolve); the served bytes match the RA
// jq slice exactly.
func TestRAServe_VerifyThenHit(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	cache.ResetResolvedCacheForTest()

	panels := panelDict(40)
	var calls atomic.Int64
	resolve := stubResolveRA(t, panels, &calls)
	ctx := ctxWithUser(t)

	// First /call page 1 perPage 10 — first sight: 2 resolves (unpaginated +
	// page-keyed reference for the byte-verify).
	got1, ok, err := raFullListServe(ctx, gvr(), "krateo-system", "compositions-panels",
		ra(raSliceJQ), 10, 1, nil, resolve)
	if err != nil || !ok {
		t.Fatalf("first serve failed: ok=%v err=%v", ok, err)
	}
	firstSightCalls := calls.Load()
	if firstSightCalls != 2 {
		t.Fatalf("first sight should resolve exactly twice (unpaginated + page-keyed verify), got %d", firstSightCalls)
	}

	// Verify served page 1 == RA jq page 1.
	ref1, _ := resolve(ctx, 10, 1)
	assertCanonEqual(t, got1, ref1, "page1")

	// Second /call page 3 perPage 10 — cheap HIT, NO new resolve.
	callsBefore := calls.Load()
	got3, ok, err := raFullListServe(ctx, gvr(), "krateo-system", "compositions-panels",
		ra(raSliceJQ), 10, 3, nil, resolve)
	if err != nil || !ok {
		t.Fatalf("second serve failed: ok=%v err=%v", ok, err)
	}
	if calls.Load() != callsBefore {
		t.Fatalf("page-3 hit should do ZERO resolve (Go-slice over cached full), but %d resolves ran", calls.Load()-callsBefore)
	}
	ref3, _ := resolve(ctx, 10, 3)
	assertCanonEqual(t, got3, ref3, "page3")

	// Serve-outcome metrics: one verified-slice + one hit.
	s := cache.RAFullListServeSnapshot()
	if s.VerifiedSlice < 1 || s.Hit < 1 {
		t.Fatalf("serve outcomes wrong: %+v", s)
	}
}

// HG-4a.serve.2 — a NON-sliceable RA (per-page aggregation) byte-verify FAILS
// → memo NOT-sliceable → every /call falls back to the page-keyed resolve
// (correct result, never a wrong one). The returned bytes equal the page-keyed
// resolve.
func TestRAServe_NonSliceableFallsBack(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	cache.ResetResolvedCacheForTest()

	// Aggregation RA: output depends on the slice (returns a count), so a
	// Go-slice over the unpaginated full can never reproduce a paginated
	// resolve → byte-verify fails.
	aggJQ := `{ count: ((.compositionspanels // []) | length), pp: (.slice.perPage // 0) }`
	panels := panelDict(30)
	var calls atomic.Int64
	resolve := func(_ context.Context, perPage, page int) (map[string]any, error) {
		calls.Add(1)
		dict := map[string]any{"compositionspanels": panels["compositionspanels"]}
		if perPage > 0 && page > 0 {
			dict["slice"] = map[string]any{"perPage": float64(perPage), "offset": float64((page - 1) * perPage)}
		}
		s, _ := jqutil.Eval(t.Context(), jqutil.EvalOptions{Query: aggJQ, Data: dict})
		var out map[string]any
		_ = json.Unmarshal([]byte(s), &out)
		return out, nil
	}
	ctx := ctxWithUser(t)

	got, ok, err := raFullListServe(ctx, gvr(), "krateo-system", "agg-ra",
		ra(aggJQ), 10, 1, nil, resolve)
	if err != nil || !ok {
		t.Fatalf("agg serve failed: ok=%v err=%v", ok, err)
	}
	// Served == the page-keyed resolve (the fall-back reference).
	ref, _ := resolve(ctx, 10, 1)
	assertCanonEqual(t, got, ref, "agg-page1")

	// A second /call: the NOT-sliceable verdict is memoised → fall back
	// (served=false so the caller resolves page-keyed; raFullListServe returns
	// ok=false).
	_, ok2, _ := raFullListServe(ctx, gvr(), "krateo-system", "agg-ra",
		ra(aggJQ), 10, 2, nil, resolve)
	if ok2 {
		t.Fatalf("a known NOT-sliceable shape must return served=false (caller falls back)")
	}
	s := cache.RAFullListServeSnapshot()
	if s.Fallback < 1 {
		t.Fatalf("expected at least one fallback serve outcome: %+v", s)
	}
}

// HG-4a.serve.3 — no identity on ctx → serve declines (fall back). NEVER serve
// a per-cohort cell without an identity.
func TestRAServe_NoIdentityDeclines(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	cache.ResetResolvedCacheForTest()
	var calls atomic.Int64
	resolve := stubResolveRA(t, panelDict(10), &calls)

	// ctx with NO UserInfo.
	_, ok, err := raFullListServe(t.Context(), gvr(), "krateo-system", "compositions-panels",
		ra(raSliceJQ), 10, 1, nil, resolve)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ok {
		t.Fatalf("serve without identity MUST decline (ok=false), never serve a cohort cell")
	}
	if calls.Load() != 0 {
		t.Fatalf("no-identity decline should not resolve, got %d resolves", calls.Load())
	}
}

// HG-4a.serve.4 — cache OFF → raFullListServe declines immediately (the
// Resolve caller then takes the byte-identical pre-4a page-keyed path). This
// is the flag-off byte-identity guard at the serve layer.
func TestRAServe_CacheOffDeclines(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "false")
	cache.ResetResolvedCacheForTest()
	var calls atomic.Int64
	resolve := stubResolveRA(t, panelDict(10), &calls)

	_, ok, err := raFullListServe(ctxWithUser(t), gvr(), "krateo-system", "compositions-panels",
		ra(raSliceJQ), 10, 1, nil, resolve)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ok {
		t.Fatalf("cache-off serve MUST decline (ok=false) so the pre-4a path runs byte-identically")
	}
	if calls.Load() != 0 {
		t.Fatalf("cache-off decline should not resolve via the 4a path, got %d", calls.Load())
	}
}

func assertCanonEqual(t *testing.T, a, b map[string]any, label string) {
	t.Helper()
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	if string(ab) != string(bb) {
		t.Fatalf("%s: served bytes != RA jq bytes\n  got: %s\n  ref: %s", label, ab, bb)
	}
}
