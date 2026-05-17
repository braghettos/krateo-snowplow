// resolve_populate_test.go — Ship C (0.30.112) AC tests for the shared
// resolveAndPopulateL1 path.
//
//   AC-C7 — the re-resolve runs under the entry's OWN Inputs identity
//        (Username+Groups, no SA/shared identity) AND the context
//        carries WithL1KeyContext(key) so dep edges are re-recorded.
//   AC-C5 (dispatcher leg) — an unknown handler kind / a resolve seam
//        that declines is a clean skip — no Put, no panic.
//   resurrect-guard — a refresh whose entry was evicted mid-resolve
//        must not re-create the entry.

package dispatchers

import (
	"context"
	"errors"
	"testing"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/snowplow/internal/cache"
)

// --- AC-C7 — re-resolve identity + WithL1KeyContext -------------------------

func TestACC7_ReResolveUsesEntryIdentityAndL1KeyContext(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	cache.ResetResolvedCacheForTest()
	t.Cleanup(cache.ResetResolvedCacheForTest)

	c := cache.ResolvedCache()
	inputs := cache.ResolvedKeyInputs{
		HandlerKind: "restactions",
		Group:       "templates.krateo.io",
		Version:     "v1",
		Resource:    "restactions",
		Namespace:   "team-a",
		Name:        "list-users",
		Username:    "cyberjoker",
		Groups:      []string{"devs", "qa"},
	}
	key := cache.ComputeKey(inputs)
	c.Put(key, &cache.ResolvedEntry{RawJSON: []byte(`{"stale":1}`), Inputs: &inputs})

	// Capture the context the resolve seam is handed.
	var gotUser string
	var gotGroups []string
	var gotL1Key string
	restore := setResolveOnceForTest(func(ctx context.Context, _ cache.ResolvedKeyInputs) ([]byte, error) {
		if ui, err := xcontext.UserInfo(ctx); err == nil {
			gotUser = ui.Username
			gotGroups = ui.Groups
		}
		gotL1Key = cache.L1KeyFromContext(ctx)
		return []byte(`{"fresh":1}`), nil
	})
	t.Cleanup(restore)

	if err := resolveAndPopulateL1(context.Background(), inputs); err != nil {
		t.Fatalf("resolveAndPopulateL1 error: %v", err)
	}

	// Identity: the entry's OWN Username+Groups — not a SA, not empty.
	if gotUser != "cyberjoker" {
		t.Fatalf("AC-C7: re-resolve ran as %q; want the entry's own identity %q",
			gotUser, "cyberjoker")
	}
	if len(gotGroups) != 2 || gotGroups[0] != "devs" || gotGroups[1] != "qa" {
		t.Fatalf("AC-C7: re-resolve groups=%v; want the entry's own [devs qa]", gotGroups)
	}
	// WithL1KeyContext: the ctx carries the L1 key so dep edges re-record.
	if gotL1Key != key {
		t.Fatalf("AC-C7: re-resolve ctx L1 key=%q; want %q (WithL1KeyContext "+
			"must be threaded so the resolver re-records dep edges)", gotL1Key, key)
	}
	// And the entry was refreshed.
	if e, ok := c.Get(key); !ok || string(e.RawJSON) != `{"fresh":1}` {
		t.Fatalf("AC-C7: entry not refreshed; got ok=%v content=%q", ok, e.RawJSON)
	}
}

// --- AC-C5 dispatcher leg — declined resolve is a clean skip ----------------

func TestACC5_ResolveSeamDeclineIsCleanSkip(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	cache.ResetResolvedCacheForTest()
	t.Cleanup(cache.ResetResolvedCacheForTest)

	c := cache.ResolvedCache()
	inputs := cache.ResolvedKeyInputs{HandlerKind: "widgets", Name: "skip"}
	key := cache.ComputeKey(inputs)
	c.Put(key, &cache.ResolvedEntry{RawJSON: []byte(`{"orig":1}`), Inputs: &inputs})
	before := c.Stats().StoreTotal

	// Seam returns (nil, nil) — "declined to resolve" (e.g. unknown
	// handler kind). resolveAndPopulateL1 must skip-to-TTL: no Put, no
	// error, no panic.
	restore := setResolveOnceForTest(func(context.Context, cache.ResolvedKeyInputs) ([]byte, error) {
		return nil, nil
	})
	t.Cleanup(restore)

	if err := resolveAndPopulateL1(context.Background(), inputs); err != nil {
		t.Fatalf("declined resolve must be a nil-error skip; got %v", err)
	}
	if got := c.Stats().StoreTotal; got != before {
		t.Fatalf("declined resolve must not Put; StoreTotal %d -> %d", before, got)
	}
}

// --- resurrect-guard — evicted entry not re-created -------------------------

func TestResolvePopulate_EvictedDuringResolveNotResurrected(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	cache.ResetResolvedCacheForTest()
	t.Cleanup(cache.ResetResolvedCacheForTest)

	c := cache.ResolvedCache()
	inputs := cache.ResolvedKeyInputs{HandlerKind: "widgets", Name: "racey"}
	key := cache.ComputeKey(inputs)
	c.Put(key, &cache.ResolvedEntry{RawJSON: []byte(`{}`), Inputs: &inputs})

	// The seam evicts the entry mid-resolve (emulates a DELETE landing
	// while the refresh is in flight), then returns fresh bytes.
	restore := setResolveOnceForTest(func(context.Context, cache.ResolvedKeyInputs) ([]byte, error) {
		c.DeleteForTest(key)
		return []byte(`{"fresh":1}`), nil
	})
	t.Cleanup(restore)

	if err := resolveAndPopulateL1(context.Background(), inputs); err != nil {
		t.Fatalf("resolveAndPopulateL1 error: %v", err)
	}
	// The post-resolve liveness re-check must have dropped the fresh
	// bytes — the evicted entry stays gone.
	if _, ok := c.Get(key); ok {
		t.Fatalf("resurrect-guard failed: a refresh re-created an evicted entry")
	}
}

// errSeam keeps the imports honest for a propagated-error sanity check.
func TestResolvePopulate_SeamErrorPropagates(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	cache.ResetResolvedCacheForTest()
	t.Cleanup(cache.ResetResolvedCacheForTest)

	c := cache.ResolvedCache()
	inputs := cache.ResolvedKeyInputs{HandlerKind: "widgets", Name: "err"}
	key := cache.ComputeKey(inputs)
	c.Put(key, &cache.ResolvedEntry{RawJSON: []byte(`{}`), Inputs: &inputs})

	restore := setResolveOnceForTest(func(context.Context, cache.ResolvedKeyInputs) ([]byte, error) {
		return nil, errors.New("resolver blew up")
	})
	t.Cleanup(restore)

	err := resolveAndPopulateL1(context.Background(), inputs)
	if err == nil {
		t.Fatalf("a resolve-seam error must propagate so the refresher can retry")
	}
}
