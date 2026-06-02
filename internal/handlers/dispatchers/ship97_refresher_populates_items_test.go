// ship97_refresher_populates_items_test.go — Ship #97 (0.30.214)
// integration falsifier. Asserts the refresher's resolveAndPopulateL1 Put
// site at resolve_populate.go:255 populates ResolvedEntry.Items on an
// apistage-class LIST entry — the load-bearing fix that restores the R3
// fast-path predicate at apistage.go:487 (`len(entry.Items) > 0`).
//
// Pre-fix, the Put site wrote RawJSON only (Items: nil) and every
// subsequent content-Get-hit fell through to gateListEnvelope ->
// parseListEnvelope on the request goroutine (45% cum CPU at 0.30.212
// production scale, see ship-97-prefix-falsifier-2026-05-31).
//
// Falsifiers in this file:
//
//   F1 — apistage-class LIST refresh populates Items (+ ItemsAPIVersion +
//        ItemsKind). FAILS pre-fix (Items=nil).
//   F2 — apistage-class GET-by-name refresh leaves Items=nil. Verifies the
//        helper's Name!="" early-return; GET-by-name still flows via
//        gateGetEnvelope on read.
//   F3 — non-apistage classes (widgets, restactions) are unaffected — the
//        Ship #97 branch is class-gated; Items must stay nil.
//   F4 — concurrent (4 readers + 1 writer) over the apistage entry under
//        -race produces no detector hit. Discharges
//        feedback_shared_vs_copy_is_a_concurrency_change for the Items
//        slice now populated by a goroutine other than the request path.

package dispatchers

import (
	"context"
	"sync"
	"testing"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// ship97LISTEnvelope is a well-formed widgets LIST envelope. Same shape
// dispatchViaInformer returns for `kubectl get widgets -A` at production.
const ship97LISTEnvelope = `{"apiVersion":"widgets.krateo.io/v1","kind":"WidgetList","items":[` +
	`{"metadata":{"name":"w1","namespace":"team-a"}},` +
	`{"metadata":{"name":"w2","namespace":"team-b"}},` +
	`{"metadata":{"name":"w3","namespace":"team-c"}}` +
	`]}`

func ship97SetupRefresherEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	cache.ResetDepsForTest()
	cache.ResetResolvedCacheForTest()
	cache.ResetRefresherForTest()
	t.Cleanup(func() {
		cache.ResetRefresherForTest()
		cache.ResetResolvedCacheForTest()
		cache.ResetDepsForTest()
	})
}

// TestShip97_F1_ApistageRefreshPopulatesItems is THE central falsifier.
// It stubs the resolveOnceFn seam to return a well-formed LIST envelope,
// invokes the registered apistage RefreshFunc as the refresher worker
// would, then asserts the stored ResolvedEntry has Items populated.
//
// FAILS pre-fix: resolve_populate.go:255 Put writes RawJSON only —
// entry.Items stays nil — R3 fast-path predicate evaluates false on every
// subsequent Get-hit.
func TestShip97_F1_ApistageRefreshPopulatesItems(t *testing.T) {
	ship97SetupRefresherEnv(t)

	// Stub the resolve seam: hand back the well-formed LIST envelope the
	// refresher's RefreshContentEntry would have produced.
	freshBytes := []byte(ship97LISTEnvelope)
	restoreSeam := setResolveOnceForTest(func(_ context.Context, _ cache.ResolvedKeyInputs) ([]byte, error) {
		return freshBytes, nil
	})
	t.Cleanup(restoreSeam)

	c := cache.ResolvedCache()
	if c == nil {
		t.Fatalf("ResolvedCache nil — CACHE_ENABLED test setup wrong")
	}

	// An apistage-class content entry, LIST shape (Name=="").
	inputs := cache.ResolvedKeyInputs{
		CacheEntryClass: cache.CacheEntryClassApistage,
		Group:           "widgets.krateo.io",
		Version:         "v1",
		Resource:        "widgets",
		Namespace:       "",
		Name:            "", // LIST
	}
	key := cache.ComputeKey(inputs)

	// Seed a stale entry with Items=nil (what pre-fix refresh would also leave).
	c.Put(key, &cache.ResolvedEntry{RawJSON: []byte(`{"stale":true}`), Inputs: &inputs})

	// Drive the refresh as the worker would.
	RegisterRefreshHandlers(nil)
	fn := cache.RefreshFuncForTest(cache.CacheEntryClassApistage)
	if fn == nil {
		t.Fatalf("no RefreshFunc registered for apistage class")
	}
	if err := fn(context.Background(), key, inputs); err != nil {
		t.Fatalf("RefreshFunc returned error: %v", err)
	}

	// THE INVARIANT — post-refresh entry MUST carry pre-parsed Items.
	entry, ok := c.Get(key)
	if !ok {
		t.Fatalf("F1: entry gone after refresh")
	}
	if string(entry.RawJSON) != ship97LISTEnvelope {
		t.Fatalf("F1: RawJSON not refreshed; got %q", entry.RawJSON)
	}
	if len(entry.Items) != 3 {
		t.Fatalf("F1: refresher-Put entry has Items=%v (len=%d), want 3 items. "+
			"Pre-fix the refresher Put writes RawJSON only; this is the R3 "+
			"hot-path defect the fix closes (see "+
			"docs/ship-97-prefix-falsifier-2026-05-31.md).",
			entry.Items, len(entry.Items))
	}
	if entry.ItemsAPIVersion != "widgets.krateo.io/v1" {
		t.Fatalf("F1: ItemsAPIVersion=%q want widgets.krateo.io/v1", entry.ItemsAPIVersion)
	}
	if entry.ItemsKind != "WidgetList" {
		t.Fatalf("F1: ItemsKind=%q want WidgetList", entry.ItemsKind)
	}
	// And per-item metadata must be intact (the gate reads metadata.name +
	// metadata.namespace via filterListByRBAC on every Get-hit).
	for i, it := range entry.Items {
		md, _ := it.Object["metadata"].(map[string]any)
		if md == nil {
			t.Fatalf("F1: item[%d] missing metadata after refresh-Put", i)
		}
		if md["name"] == nil || md["namespace"] == nil {
			t.Fatalf("F1: item[%d] metadata missing name/namespace: %v", i, md)
		}
	}
}

// TestShip97_F2_ApistageGetByNameLeavesItemsNil — the helper returns
// ok=false for GET-by-name (Name != ""); the entry must keep Items=nil.
// GET-by-name responses are not LIST envelopes and the R3 fast-path
// predicate at apistage.go:487 is LIST-only by design.
func TestShip97_F2_ApistageGetByNameLeavesItemsNil(t *testing.T) {
	ship97SetupRefresherEnv(t)

	getByNameBytes := []byte(`{"apiVersion":"widgets.krateo.io/v1","kind":"Widget","metadata":{"name":"w1","namespace":"team-a"}}`)
	restoreSeam := setResolveOnceForTest(func(_ context.Context, _ cache.ResolvedKeyInputs) ([]byte, error) {
		return getByNameBytes, nil
	})
	t.Cleanup(restoreSeam)

	c := cache.ResolvedCache()
	inputs := cache.ResolvedKeyInputs{
		CacheEntryClass: cache.CacheEntryClassApistage,
		Group:           "widgets.krateo.io",
		Version:         "v1",
		Resource:        "widgets",
		Namespace:       "team-a",
		Name:            "w1", // GET-by-name
	}
	key := cache.ComputeKey(inputs)
	c.Put(key, &cache.ResolvedEntry{RawJSON: []byte(`{"stale":true}`), Inputs: &inputs})

	RegisterRefreshHandlers(nil)
	fn := cache.RefreshFuncForTest(cache.CacheEntryClassApistage)
	if fn == nil {
		t.Fatalf("no apistage RefreshFunc registered")
	}
	if err := fn(context.Background(), key, inputs); err != nil {
		t.Fatalf("RefreshFunc returned error: %v", err)
	}

	entry, ok := c.Get(key)
	if !ok {
		t.Fatalf("F2: entry gone after refresh")
	}
	if entry.Items != nil {
		t.Fatalf("F2: GET-by-name refresh-Put populated Items=%v (len=%d); want nil. "+
			"R3 fast-path is LIST-only; GET-by-name responses are a single object, "+
			"not a LIST envelope.",
			entry.Items, len(entry.Items))
	}
	if entry.ItemsAPIVersion != "" || entry.ItemsKind != "" {
		t.Fatalf("F2: ItemsAPIVersion=%q ItemsKind=%q; both must be empty for GET-by-name",
			entry.ItemsAPIVersion, entry.ItemsKind)
	}
}

// TestShip97_F3_NonApistageClassesUnaffected — the Ship #97 branch is
// gated on `inputs.CacheEntryClass == cache.CacheEntryClassApistage`. A
// widgets-class or restactions-class refresh-Put MUST leave Items=nil
// (those classes don't use the R3 fast path).
func TestShip97_F3_NonApistageClassesUnaffected(t *testing.T) {
	ship97SetupRefresherEnv(t)

	freshBytes := []byte(ship97LISTEnvelope) // shape doesn't matter — class is the gate
	restoreSeam := setResolveOnceForTest(func(_ context.Context, _ cache.ResolvedKeyInputs) ([]byte, error) {
		return freshBytes, nil
	})
	t.Cleanup(restoreSeam)

	c := cache.ResolvedCache()
	for _, class := range []string{"widgets", "restactions"} {
		inputs := cache.ResolvedKeyInputs{
			// Ship 0.30.240 — ResolvedKeyInputs identity-free.
			CacheEntryClass: class,
			Group:           "widgets.templates.krateo.io",
			Version:         "v1beta1",
			Resource:        "buttons",
			Namespace:       "demo",
			Name:            "save-btn",
		}
		key := cache.ComputeKey(inputs)
		c.Put(key, &cache.ResolvedEntry{RawJSON: []byte(`{"stale":true}`), Inputs: &inputs})

		RegisterRefreshHandlers(nil)
		fn := cache.RefreshFuncForTest(class)
		if fn == nil {
			t.Fatalf("F3[%s]: no RefreshFunc registered", class)
		}
		if err := fn(context.Background(), key, inputs); err != nil {
			t.Fatalf("F3[%s]: RefreshFunc returned error: %v", class, err)
		}

		entry, ok := c.Get(key)
		if !ok {
			t.Fatalf("F3[%s]: entry gone after refresh", class)
		}
		if entry.Items != nil {
			t.Fatalf("F3[%s]: non-apistage class refresh-Put populated Items=%v "+
				"(len=%d); want nil. The Ship #97 branch is apistage-only.",
				class, entry.Items, len(entry.Items))
		}
	}
}

// TestShip97_F4_ConcurrentReadersOverRefresherPutEntry — discharges
// feedback_shared_vs_copy_is_a_concurrency_change: the Items slice is
// now populated by a goroutine other than the request path. 4+
// concurrent readers over the refresher-Put entry must not trip the
// race detector.
//
// Reader pattern mirrors what apistageContentServe does on a content-hit
// at apistage.go:479-494: store.Get(key), read entry.RawJSON, read
// len(entry.Items), iterate entry.Items.
//
// Run with `go test -race -run TestShip97_F4_Concurrent ./internal/handlers/dispatchers/...`.
func TestShip97_F4_ConcurrentReadersOverRefresherPutEntry(t *testing.T) {
	ship97SetupRefresherEnv(t)

	freshBytes := []byte(ship97LISTEnvelope)
	restoreSeam := setResolveOnceForTest(func(_ context.Context, _ cache.ResolvedKeyInputs) ([]byte, error) {
		return freshBytes, nil
	})
	t.Cleanup(restoreSeam)

	c := cache.ResolvedCache()
	inputs := cache.ResolvedKeyInputs{
		CacheEntryClass: cache.CacheEntryClassApistage,
		Group:           "widgets.krateo.io",
		Version:         "v1",
		Resource:        "widgets",
		Namespace:       "",
		Name:            "",
	}
	key := cache.ComputeKey(inputs)
	c.Put(key, &cache.ResolvedEntry{RawJSON: []byte(`{"stale":true}`), Inputs: &inputs})

	RegisterRefreshHandlers(nil)
	fn := cache.RefreshFuncForTest(cache.CacheEntryClassApistage)
	if fn == nil {
		t.Fatalf("F4: no apistage RefreshFunc registered")
	}

	// Prime the entry with a refresh.
	if err := fn(context.Background(), key, inputs); err != nil {
		t.Fatalf("F4: initial refresh failed: %v", err)
	}

	// Stop signal for readers; the refresher exits naturally after N iters.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var readersWG sync.WaitGroup

	// 4 readers — emulate apistageContentServe's Get-hit pattern.
	for i := 0; i < 4; i++ {
		readersWG.Add(1)
		go func() {
			defer readersWG.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				entry, ok := c.Get(key)
				if !ok || entry == nil {
					continue
				}
				// Read the same fields the gate reads.
				_ = entry.RawJSON
				if len(entry.Items) == 0 {
					continue
				}
				for _, it := range entry.Items {
					if it == nil {
						continue
					}
					md, _ := it.Object["metadata"].(map[string]any)
					_ = md["name"]
					_ = md["namespace"]
				}
				_ = entry.ItemsAPIVersion
				_ = entry.ItemsKind
			}
		}()
	}

	// Refresher in foreground — re-Puts the entry N times then exits. This
	// is the goroutine the Ship #97 fix moves the parse to; the readers
	// above must observe no race on the Items slice population.
	for n := 0; n < 200; n++ {
		if err := fn(context.Background(), key, inputs); err != nil {
			cancel()
			readersWG.Wait()
			t.Fatalf("F4: refresh iter %d returned error: %v", n, err)
		}
	}

	// Signal readers to stop and wait for them.
	cancel()
	readersWG.Wait()
}
