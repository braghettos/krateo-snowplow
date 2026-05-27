// phase1_pip_seed_test.go — Ship 0.30.187 D2 falsifier.
//
// D2 (defect): the Phase-1 prewarm PIP seed Put used the walker's
// RESOLUTION pagination (prewarmPageLimit()=5 default) as the dispatcher
// cache key tuple. The dispatcher at serve time computes the key from
// the request URL's `?page=N&perPage=M` query params via paginationInfo
// (helpers.go:50-76), which DEFAULTS to (-1, -1) when the URL carries
// no slice. Seed→serve cells mismatch on every no-slice widget, so the
// PIP-seeded entries never hit and the first nav looks cold.
//
// THE FIX (architect's TRACED design 2026-05-27): decouple the seed-key
// tuple from the resolution tuple. The seed-key tuple MUST match the
// dispatcher's paginationInfo for an equivalent request:
//   - widget reached via /call Path with NO page/perPage params:
//       seed-key tuple = (-1, -1)  (matches paginationInfo's default).
//   - widget reached via /call Path WITH declared page/perPage:
//       seed-key tuple = (declared page, declared perPage)
//       (matches paginationInfo when the frontend hits the same URL).
// The resolution tuple stays = prewarmPageLimit() for no-slice widgets
// (the 0.30.127 storm guard) and = declared (page,perPage) when present.
//
// THESE TESTS pin the seed-key derivation in isolation — they do not
// require a live apiserver, do not require an informer, and run in <1ms.
// A regression that reverts the decoupling (re-folds resolution into the
// seed key) fails TestPhase1PIPSeedKey_NoSliceUsesDispatcherDefaultTuple.

package dispatchers

import (
	"testing"
)

// TestPhase1PIPSeedKey_NoSliceUsesDispatcherDefaultTuple is the D2
// falsifier. It pins the contract that the walker's seed-key derivation
// for a widget reached via a /call Path with NO page/perPage params
// produces the SAME (perPage, page) tuple the dispatcher's
// paginationInfo defaults to for a request with no URL slice params:
// (-1, -1).
//
// The contract is enforced by the helper deriveSeedKeyTuple in
// phase1_pip_seed.go (introduced this ship).
func TestPhase1PIPSeedKey_NoSliceUsesDispatcherDefaultTuple(t *testing.T) {
	// No-slice widget: the /call Path declares no page/perPage. The
	// walker resolves under prewarmPageLimit() (the 0.30.127 storm
	// guard) but the seed-key tuple MUST be (-1, -1) so a serve-time
	// request with no URL slice params lands on the same cell.
	keyPerPage, keyPage := deriveSeedKeyTuple(noSlicePath)
	if keyPerPage != -1 || keyPage != -1 {
		t.Fatalf("D2: no-slice widget seed-key tuple = (perPage=%d, page=%d), "+
			"want (-1, -1) — the dispatcher's paginationInfo defaults to "+
			"(-1, -1) when the request URL carries no ?page/?perPage, so the "+
			"seed Put must use the same tuple or it hashes to a different "+
			"cell and the serve-time lookup misses (the 0.30.186 14/17 first-"+
			"nav-hit defect)",
			keyPerPage, keyPage)
	}
}

// TestPhase1PIPSeedKey_DeclaredSlicePreserved pins the symmetric
// contract: a /call Path that carries explicit page/perPage must yield a
// seed-key tuple equal to the declared slice. The frontend hits the same
// URL at serve time, so paginationInfo returns the same (page, perPage),
// so the seed-key tuple must equal the declared slice.
func TestPhase1PIPSeedKey_DeclaredSlicePreserved(t *testing.T) {
	// Dashboard table: declared slice page=1 perPage=5.
	keyPerPage, keyPage := deriveSeedKeyTuple(dashboardTablePath)
	if keyPerPage != 5 || keyPage != 1 {
		t.Fatalf("D2: declared-slice widget seed-key tuple = (perPage=%d, page=%d), "+
			"want (perPage=5, page=1) — paginationInfo at serve time returns "+
			"the URL's declared page/perPage so the seed Put must use the "+
			"same tuple",
			keyPerPage, keyPage)
	}
}

// TestPhase1PIPSeedKey_RootWidgetUsesDispatcherDefaultTuple pins the
// root-navigation case. A root has no /call Path (it is fetched directly
// via objects.Get from a listed ObjectReference), so the walker passes
// an empty path string — deriveSeedKeyTuple must yield the dispatcher's
// no-slice default tuple. The frontend's first hit on a root navigation
// widget URL carries no slice params, so paginationInfo returns
// (-1, -1); the seed-key tuple must match.
func TestPhase1PIPSeedKey_RootWidgetUsesDispatcherDefaultTuple(t *testing.T) {
	keyPerPage, keyPage := deriveSeedKeyTuple("")
	if keyPerPage != -1 || keyPage != -1 {
		t.Fatalf("D2: root widget (empty path) seed-key tuple = "+
			"(perPage=%d, page=%d), want (-1, -1) — a root widget has no "+
			"/call Path and the dispatcher's first request for it carries "+
			"no slice params, so paginationInfo returns (-1, -1)",
			keyPerPage, keyPage)
	}
}
