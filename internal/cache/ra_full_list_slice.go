// ra_full_list_slice.go — Ship 4a (0.30.198): the page-independent
// RAFullList cache's pure-logic core.
//
// THE SHIP. The compositions-panels RESTAction's top-level filter
// sort_by(creationTimestamp)|reverse then slices `$sorted[offset:offset+
// perPage]` driven by the injected `.slice`. Today every page (page 1, 2,
// 3 …) re-runs the WHOLE resolve (62-namespace fan-out + 49K-element sort +
// deep-copy + gojq recompile) because the L1 cell folds page/perPage into
// its key. Ship 4a caches the RA's full result ONCE per (RA × cohort ×
// non-slice inputs) resolved UNPAGINATED (no `.slice` → the RA's own jq
// returns the full sorted set) and serves each page as a cheap Go-slice
// over that one cached full array.
//
// This file holds ONLY pure functions + the sliceability memo — no cache
// store, no HTTP, no resolver coupling — so the falsifiers (page-
// independent key, Go-slice == RA `.slice` jq, sliceability memo) exercise
// it in isolation. The dispatcher/resolver wiring lives elsewhere; removing
// the layer is deleting this file + the apiRef Get/Put + the resident
// region (project_caching_is_provisional — wholesale-deletable).

package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"sync/atomic"
	"time"
)

// sliceExtrasKeys is the set of Extras keys that carry PAGINATION/slice
// intent. They are STRIPPED from the RAFullList key (extrasMinusSlice) so a
// /call differing only in slice lands on the SAME page-independent cell.
// Mechanism-uniform (feedback_no_special_cases): a fixed, slice-semantic
// key set — NOT a resource/path/name literal.
var sliceExtrasKeys = map[string]struct{}{
	"slice":   {},
	"page":    {},
	"perPage": {},
	"offset":  {},
}

// extrasMinusSlice returns a COPY of extras with every slice-bearing key
// (slice/page/perPage/offset) removed. Used to build the page-independent
// RAFullList key: two /calls differing only in slice produce identical
// stripped extras → identical key. A nil/empty input returns nil (so the
// key's Extras fold is byte-identical to "no extras"). The input map is
// never mutated.
//
// Falsifier HG-4a.1 asserts the strip + the page-independence it yields.
func extrasMinusSlice(extras map[string]any) map[string]any {
	if len(extras) == 0 {
		return nil
	}
	out := make(map[string]any, len(extras))
	for k, v := range extras {
		if _, isSlice := sliceExtrasKeys[k]; isSlice {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// RAFullListKeyInputs builds the canonical page-INDEPENDENT ResolvedKeyInputs
// for a RAFullList cell. It takes the RESTACTION's OWN identity (gvr/ns/name
// — NOT the calling widget's), the cohort BindingSetHash, and the request
// Extras, and forces PerPage=0/Page=0 + extrasMinusSlice(extras). The result
// fed to ComputeKey folds ONLY the page-independent material (ComputeKey
// folds PerPage at resolved.go's PerPage write + Page; both 0 here) so every
// page of the same (RA × cohort × non-slice-extras) hashes to ONE key.
//
// bindingSetHash is the cohort hash (BindingSetHash(username, groups)) — the
// SAME per-cohort identity the restactions/widgets classes use; RAFullList is
// identity-BOUND (RA output is RBAC-narrowed), so ComputeKey folds it.
func RAFullListKeyInputs(group, version, resource, namespace, name string,
	bindingSetHash uint64, extras map[string]any) ResolvedKeyInputs {
	return ResolvedKeyInputs{
		CacheEntryClass: CacheEntryClassRAFullList,
		Group:           group,
		Version:         version,
		Resource:        resource,
		Namespace:       namespace,
		Name:            name,
		BindingSetHash:  bindingSetHash,
		// Page-INDEPENDENT: slice folded out of the key.
		PerPage: 0,
		Page:    0,
		Extras:  extrasMinusSlice(extras),
	}
}

// GoSliceFullList applies a per-/call page slice [offset:offset+perPage] over
// the single array-valued key of a RA full-result map, returning a NEW result
// map of the same shape with that key sliced. It is the Go equivalent of the
// RA's own output jq `$sorted[$offset : $offset+$perPage]`.
//
// SHAPE CONTRACT (the sliceability precondition — verified per (key ×
// sliceShape) by the byte-verify before this is trusted): the full map must
// have EXACTLY ONE key whose value is a []any (the sorted list); every other
// key is copied through unchanged. ok=false when the shape is not a clean
// single-array map (zero or multiple array-valued keys) — the caller then
// falls back to the page-keyed resolve (never a wrong result).
//
// offset/perPage are the per-/call slice (offset=(page-1)*perPage). The slice
// is clamped to the array bounds exactly as the RA jq's
// `range($offset;$offset+$perPage) | select(. < $len)` clamps. A perPage<=0
// (unpaginated) returns the full array unchanged (ok=true) — the identity
// case. The returned map shares the element VALUES with the input (a shallow
// slice — the elements are not deep-copied); callers that re-encode
// immediately (the serve path) never mutate them.
func GoSliceFullList(full map[string]any, offset, perPage int) (map[string]any, bool) {
	if full == nil {
		return nil, false
	}
	// Find the single array-valued key.
	arrKey := ""
	var arr []any
	arrCount := 0
	for k, v := range full {
		if a, isArr := v.([]any); isArr {
			arrCount++
			arrKey = k
			arr = a
		}
	}
	if arrCount != 1 {
		// Zero or multiple arrays — not a clean single-list shape. Fail-safe.
		return nil, false
	}

	out := make(map[string]any, len(full))
	for k, v := range full {
		if k != arrKey {
			out[k] = v
		}
	}

	if perPage <= 0 {
		// Unpaginated identity — full array through.
		out[arrKey] = arr
		return out, true
	}

	// Clamp [offset:offset+perPage] to bounds, mirroring the RA jq's
	// out-of-bounds guard.
	length := len(arr)
	start := offset
	if start < 0 {
		start = 0
	}
	if start > length {
		start = length
	}
	end := offset + perPage
	if end < start {
		end = start
	}
	if end > length {
		end = length
	}
	// Materialise a fresh slice (not a sub-slice alias) so the cached full
	// array's backing store is never referenced by the served page (defensive
	// against any downstream append mutating shared backing memory).
	page := make([]any, end-start)
	copy(page, arr[start:end])
	out[arrKey] = page
	return out, true
}

// FullListIsEmpty reports whether a freshly-resolved RA full-result map is
// "empty" — i.e. its SINGLE array-valued key has length 0. It is the
// mechanism-uniform emptiness probe used by the serve path to refuse to
// freeze a sliceability verdict on an empty full (the "cache empty at the
// prewarm tuple → permanent stale hit" foot-gun): an empty full under a
// continueOnError stage is INDISTINGUISHABLE from a not-yet-synced / degraded
// resolve, so it must never be recorded as an authoritative sliceable verdict.
//
// Keyed off the SAME single-array shape contract as GoSliceFullList (NO
// resource/name/GVR literal — feedback_no_special_cases): a clean single-list
// map whose list is empty → true. A nil map, a zero-array-key map, or a
// multi-array map → false (NOT "empty" in the sliceable sense — those shapes
// are handled by GoSliceFullList's ok=false fail-safe and never reach a
// "freeze the empty verdict" decision). A non-empty single list → false.
func FullListIsEmpty(full map[string]any) bool {
	if full == nil {
		return false
	}
	arrCount := 0
	emptyArr := false
	for _, v := range full {
		if a, isArr := v.([]any); isArr {
			arrCount++
			emptyArr = len(a) == 0
		}
	}
	return arrCount == 1 && emptyArr
}

// --- Sliceability memo --------------------------------------------------
//
// Per the design §3: the verdict "is this RAFullList cell safely sliceable
// in Go for THIS caller's slice shape?" is memoised per (RAFullList-key ×
// sliceShape). sliceShape fingerprints HOW the caller slices — its handler
// class + the caller CR identity + a hash of the RA's output slice jq — so
// widget A's verdict NEVER applies to widget B (different sliceShape). The
// verdict is computed on FIRST sight (the byte-verify) and reused after;
// re-evaluated per new shape + on pod restart (the memo is process-local).

// SliceabilityLabels carry the HUMAN-READABLE caller-CR identity for a memo
// entry. Captured at record time so the read-side snapshot (used by
// snowplow_ra_full_list_memo) can describe each entry by its callerNamespace
// / callerName / callerResource — without those, every entry is a sha256
// hash and the operator cannot pick out a specific widget's verdict.
//
// Mechanism-uniform (feedback_no_special_cases): the labels are GENERIC
// caller-identity fields populated for EVERY caller, not a per-widget literal.
// The same struct describes apiref's "compositions-page-datagrid" and any
// future caller class identically.
//
// Optional (zero-value safe): when the labels are unset (e.g. tests calling
// the legacy RecordSliceability) the snapshot emits empty strings — verdicts
// are still recorded, just less describable. Production callers MUST pass
// real labels.
type SliceabilityLabels struct {
	// CallerClass mirrors sliceShapeHash's callerClass argument (e.g.
	// "apiref" / "restactions") — distinguishes serve-path families.
	CallerClass string
	// CallerGroup/Version/Resource is the caller CR's GVR (e.g. the apiRef
	// widget's GVR).
	CallerGroup    string
	CallerVersion  string
	CallerResource string
	// CallerNamespace/Name is the caller CR's ns/name (e.g. the apiRef
	// widget's ns/name — what the operator greps for in the snapshot).
	CallerNamespace string
	CallerName      string
}

// sliceabilityMemoEntry is the in-memory value stored in the memo's sync.Map.
// Pre-0.30.210 this was a bare `bool`; we widened it so the read-side
// snapshot can describe each entry by raKey + sliceShape + labels +
// recordedAt. The hot lookup() path still does one sync.Map.Load + one
// pointer-deref of an immutable struct (no extra locks, no allocations on
// the read path).
type sliceabilityMemoEntry struct {
	verdict bool
	// RAKey/SliceShape are the SAME strings the caller passed to record() —
	// kept here only so the snapshot can emit them without a reverse lookup.
	// Reading them is read-only (immutable post-Store), no mutex needed.
	raKey      string
	sliceShape string
	labels     SliceabilityLabels
	// recordedAtUnix is the Unix-seconds timestamp captured at first record().
	// Re-record (refresh in place) does NOT update it — the snapshot field
	// answers "when was this verdict first frozen", which is what the
	// operator needs to distinguish "recorded long ago" from "just now".
	recordedAtUnix int64
	// lastUpdatedAtUnix is the Unix-seconds timestamp of the MOST RECENT
	// record() call for this (raKey, sliceShape) — updated on every record()
	// call (insert AND refresh-in-place). Discriminates Mode-3 stuck-since-boot
	// (verdict=false, lastUpdated ≈ pod-start) from Mode-3 refreshing-still-
	// failing (verdict=false, lastUpdated ≈ now) — the two need different
	// next ships. Architect REQ-2 (0.30.210).
	lastUpdatedAtUnix int64
}

// sliceabilityMemo is the process-wide bounded map of
// hash(RAFullListKey × sliceShape) -> *sliceabilityMemoEntry. sync.Map for
// lock-free Load on the hot serve path; the insertion-order cap is enforced
// only on the Store-miss path (rare — once per distinct (key, shape)).
type sliceabilityMemo struct {
	verdicts sync.Map // string(hash) -> *sliceabilityMemoEntry
	count    int64    // approximate entry count (atomic via mu)
	mu       sync.Mutex
	cap      int
}

var raSliceabilityMemo = &sliceabilityMemo{cap: defaultSliceabilityMemoCap}

// nowUnix is the clock used to stamp recordedAtUnix. Indirected via atomic
// so tests can replace it without a race; production reads time.Now().Unix().
var nowUnix atomic.Pointer[func() int64]

func init() {
	def := func() int64 { return time.Now().Unix() }
	nowUnix.Store(&def)
}

// currentNowUnix returns the active clock function.
func currentNowUnix() int64 {
	f := nowUnix.Load()
	if f == nil {
		return time.Now().Unix()
	}
	return (*f)()
}

// defaultSliceabilityMemoCap bounds the sliceability memo. It is small:
// the distinct (RA × sliceShape) population is O(num_slicing_RAs ×
// num_caller_widget_shapes), a few hundred at most. The cap is a runaway
// guard, not a tuning knob.
const defaultSliceabilityMemoCap = 4096

// sliceShapeHash fingerprints HOW a caller slices a RAFullList cell:
// (callerClass, caller gvr/ns/name, hash of the RA's output slice jq). Two
// callers with the same shape share a verdict; different shapes get
// INDEPENDENT verdicts. The RA slice-jq hash distinguishes RAs that emit a
// non-sliceable shape (e.g. a per-page aggregation) from clean sort-then-
// slice RAs even under the same caller identity.
func sliceShapeHash(callerClass, callerGroup, callerVersion, callerResource,
	callerNamespace, callerName, raSliceJQ string) string {
	h := sha256.New()
	h.Write([]byte("sliceShape\x00"))
	h.Write([]byte(callerClass))
	h.Write([]byte{0})
	h.Write([]byte(callerGroup))
	h.Write([]byte{0})
	h.Write([]byte(callerVersion))
	h.Write([]byte{0})
	h.Write([]byte(callerResource))
	h.Write([]byte{0})
	h.Write([]byte(callerNamespace))
	h.Write([]byte{0})
	h.Write([]byte(callerName))
	h.Write([]byte{0})
	h.Write([]byte(raSliceJQ))
	return hex.EncodeToString(h.Sum(nil))
}

// memoKey composes the per (RAFullList-key × sliceShape) memo key.
func memoKey(raFullListKey, sliceShape string) string {
	h := sha256.New()
	h.Write([]byte(raFullListKey))
	h.Write([]byte{0})
	h.Write([]byte(sliceShape))
	return hex.EncodeToString(h.Sum(nil))
}

// SliceabilityVerdict returns (sliceable, known) for the given
// (RAFullListKey × sliceShape). known=false means no verdict has been
// recorded yet — the caller must run the byte-verify and RecordSliceability.
func (m *sliceabilityMemo) lookup(raFullListKey, sliceShape string) (sliceable, known bool) {
	v, ok := m.verdicts.Load(memoKey(raFullListKey, sliceShape))
	if !ok {
		return false, false
	}
	e, ok := v.(*sliceabilityMemoEntry)
	if !ok || e == nil {
		return false, false
	}
	return e.verdict, true
}

// record stores the verdict for (RAFullListKey × sliceShape). Idempotent;
// the bounded cap is enforced on the miss path only — once full, new
// verdicts are dropped (the caller then re-verifies per /call, which is the
// fail-safe page-keyed path, never a wrong result).
//
// TIMESTAMP SEMANTICS (architect REQ-2, 0.30.210):
//   - recordedAtUnix:    set on INSERT only, preserved on refresh-in-place.
//     Answers "when was this verdict first frozen".
//   - lastUpdatedAtUnix: set on EVERY record() (insert AND refresh-in-place).
//     Answers "when was the verdict last (re-)written". Discriminates
//     Mode-3 stuck-since-boot from Mode-3 refreshing-still-failing.
//
// Labels and raKey/sliceShape are preserved across refresh-in-place (a
// refresh-without-labels never overwrites real labels with empty).
func (m *sliceabilityMemo) record(raFullListKey, sliceShape string, sliceable bool, labels SliceabilityLabels) {
	mk := memoKey(raFullListKey, sliceShape)
	now := currentNowUnix()
	if prev, exists := m.verdicts.Load(mk); exists {
		// Refresh in place: build a NEW entry (entries are immutable post-Store)
		// preserving recordedAt + labels, updating verdict + lastUpdatedAt.
		pe, _ := prev.(*sliceabilityMemoEntry)
		if pe != nil {
			m.verdicts.Store(mk, &sliceabilityMemoEntry{
				verdict:           sliceable,
				raKey:             pe.raKey,
				sliceShape:        pe.sliceShape,
				labels:            pe.labels,
				recordedAtUnix:    pe.recordedAtUnix,
				lastUpdatedAtUnix: now,
			})
		} else {
			// Defensive: a pre-0.30.210 raw bool snuck in somehow — replace it
			// with a fresh entry. No correctness impact.
			m.verdicts.Store(mk, &sliceabilityMemoEntry{
				verdict:           sliceable,
				raKey:             raFullListKey,
				sliceShape:        sliceShape,
				labels:            labels,
				recordedAtUnix:    now,
				lastUpdatedAtUnix: now,
			})
		}
		return
	}
	m.mu.Lock()
	if m.cap > 0 && m.count >= int64(m.cap) {
		m.mu.Unlock()
		return // cap reached — drop; caller re-verifies (fail-safe)
	}
	m.count++
	m.mu.Unlock()
	m.verdicts.Store(mk, &sliceabilityMemoEntry{
		verdict:           sliceable,
		raKey:             raFullListKey,
		sliceShape:        sliceShape,
		labels:            labels,
		recordedAtUnix:    now,
		lastUpdatedAtUnix: now,
	})
}

// SliceabilityLookup is the package-level lookup against the process memo.
func SliceabilityLookup(raFullListKey, sliceShape string) (sliceable, known bool) {
	return raSliceabilityMemo.lookup(raFullListKey, sliceShape)
}

// RecordSliceability records a byte-verify verdict in the process memo with
// EMPTY labels. Test/back-compat entry point — production callers SHOULD use
// RecordSliceabilityWithLabels so the read-side snapshot can describe the
// entry's caller-CR identity.
func RecordSliceability(raFullListKey, sliceShape string, sliceable bool) {
	raSliceabilityMemo.record(raFullListKey, sliceShape, sliceable, SliceabilityLabels{})
}

// RecordSliceabilityWithLabels records a byte-verify verdict + the caller-CR
// identity labels. The labels are READ-SIDE ONLY (the snowplow_ra_full_list_memo
// expvar snapshot) — they DO NOT participate in the verdict key (memoKey is
// built from raKey + sliceShape only, byte-identical to pre-0.30.210). So two
// callers that hash to the SAME sliceShape (an architectural impossibility
// given sliceShapeHash folds caller ns/name) would share one verdict and the
// labels of WHICHEVER recorded first; in practice sliceShape is unique per
// caller-CR so labels are 1:1 with entries.
func RecordSliceabilityWithLabels(raFullListKey, sliceShape string, sliceable bool, labels SliceabilityLabels) {
	raSliceabilityMemo.record(raFullListKey, sliceShape, sliceable, labels)
}

// SliceabilityMemoEntry is the public, snapshot-friendly view of one memo
// entry. Field names map directly to the snowplow_ra_full_list_memo JSON keys.
//
// RecordedAtUnixSeconds / LastUpdatedAtUnixSeconds are deliberately NOT
// omitempty — operators reading /debug/vars shouldn't have to guess "field
// not in this build" vs "value zero" (architect REQ-2 callout). Caller
// labels keep omitempty because zero-label entries (legacy
// RecordSliceability tests) shouldn't crowd the snapshot.
type SliceabilityMemoEntry struct {
	RAKey                    string `json:"raKey"`
	SliceShape               string `json:"sliceShape"`
	Verdict                  bool   `json:"verdict"`
	RecordedAtUnixSeconds    int64  `json:"recordedAtUnixSeconds"`
	LastUpdatedAtUnixSeconds int64  `json:"lastUpdatedAtUnixSeconds"`
	CallerClass              string `json:"callerClass,omitempty"`
	CallerGroup              string `json:"callerGroup,omitempty"`
	CallerVersion            string `json:"callerVersion,omitempty"`
	CallerResource           string `json:"callerResource,omitempty"`
	CallerNamespace          string `json:"callerNamespace,omitempty"`
	CallerName               string `json:"callerName,omitempty"`
}

// SliceabilityMemoSnapshot returns a flat list of every recorded entry in the
// process memo. The walk is sync.Map.Range — concurrent record/lookup callers
// race-free; the result is a point-in-time view (entries added during the walk
// may or may not appear). Order is unspecified.
//
// Cost: O(entries). The memo is capped at defaultSliceabilityMemoCap (4096)
// so worst case is a 4096-element slice — trivial at the typical
// expvar-scrape cadence (10-60s).
//
// READ-ONLY: this function never mutates the memo. It is intended for
// /debug/vars consumption only; nothing on the serve path calls it.
func SliceabilityMemoSnapshot() []SliceabilityMemoEntry {
	return raSliceabilityMemo.snapshot()
}

// snapshot walks the memo's sync.Map and returns a flat slice of public
// entries.
func (m *sliceabilityMemo) snapshot() []SliceabilityMemoEntry {
	// Pre-size from the approximate count under the cap mutex; sync.Map.Range
	// may add/remove during iteration but the cap is small so over/under by
	// a few is fine.
	m.mu.Lock()
	n := m.count
	m.mu.Unlock()
	if n < 0 {
		n = 0
	}
	out := make([]SliceabilityMemoEntry, 0, n)
	m.verdicts.Range(func(_, v any) bool {
		e, ok := v.(*sliceabilityMemoEntry)
		if !ok || e == nil {
			return true
		}
		out = append(out, SliceabilityMemoEntry{
			RAKey:                    e.raKey,
			SliceShape:               e.sliceShape,
			Verdict:                  e.verdict,
			RecordedAtUnixSeconds:    e.recordedAtUnix,
			LastUpdatedAtUnixSeconds: e.lastUpdatedAtUnix,
			CallerClass:              e.labels.CallerClass,
			CallerGroup:              e.labels.CallerGroup,
			CallerVersion:            e.labels.CallerVersion,
			CallerResource:           e.labels.CallerResource,
			CallerNamespace:          e.labels.CallerNamespace,
			CallerName:               e.labels.CallerName,
		})
		return true
	})
	return out
}

// SliceShapeHash is the exported sliceShape fingerprint (see sliceShapeHash).
func SliceShapeHash(callerClass, callerGroup, callerVersion, callerResource,
	callerNamespace, callerName, raSliceJQ string) string {
	return sliceShapeHash(callerClass, callerGroup, callerVersion, callerResource,
		callerNamespace, callerName, raSliceJQ)
}

// resetSliceabilityMemoForTest clears the process memo. TEST-ONLY.
func resetSliceabilityMemoForTest() {
	raSliceabilityMemo = &sliceabilityMemo{cap: defaultSliceabilityMemoCap}
}
