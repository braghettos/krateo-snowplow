// Package cache — Ship 0.30.242 H.c-layered Phase 2 step 2b.
//
// request_authz_memo.go — per-request memo of EvaluateRBAC verdicts.
//
// PURPOSE (design §5.1)
//
// A single /call processes multiple cache-class layers:
//   - 1× widgets   (one EvaluateRBAC for the widget's GET)
//   - 1× restactions (one EvaluateRBAC for the RA's GET)
//   - N× apistage  (one EvaluateRBAC per stage, N = UAF-declared fanout)
//   - 1× widgetContent (no evaluator call — identity-free key)
//   - 1× raFullList (reuses widgets's BindingUID — no extra evaluator call)
//
// For cyberjoker's compositions dashboard: 1+1+3 = 5 EvaluateRBAC calls
// per /call. Without memoisation, identical (verb, gvr, namespace)
// tuples within the request would re-walk the snapshot. The memo
// collapses such duplicates within ONE request.
//
// SNAPSHOT-COHERENT (design §5.3)
//
// The memo is bound to the snapshot generation observed at request
// entry (the snap.PublishSeq stamped by rebuildRBACSnapshot in
// rbac_snapshot.go — Ship 0.30.242 atomic order fix). On any mid-
// request snapshot republish (rbacSnap.Load().PublishSeq !=
// m.snapPublishSeq), the memo RESETS itself before recording the
// fresh verdict. Guarantees:
//   - Two layers within the SAME memo lifetime never see different
//     first-match bindings for an identical (verb, gvr, ns, name).
//   - A mid-request republish that DOES change the first-match
//     binding for a layer is observed correctly — the memo invalidates
//     itself; the next lookup re-derives against the new snapshot.
//
// PATH B SCAFFOLDING (Ship 0.30.242 H.c-layered 2b deviation)
//
// This commit ships the memo TYPE fully implemented; ZERO production
// callers in 2b. Cell-key derivation sites (helpers.go, ra_full_list.go,
// apistage.go) call rbac.EvaluateRBAC directly. The memo plumbing
// through ResolveOptions is a Phase 3 follow-up: thread
// *RequestAuthzMemo through every resolver's ResolveOptions, construct
// the memo at request entry, migrate BindingUID-derivation sites to
// EvaluateOrLookup. Diego ratified Path B 2026-06-03 (per-request memo
// is OPTIMIZATION not correctness; F3 falsifier gate validates).
//
// FALSIFIER GUARDRAIL (design §5.3 + Phase 3 F3)
//
// F3 (seed↔dispatcher convergence with mid-test mutation under -race)
// is the falsifier that detects whether per-request consistency is
// load-bearing for correctness. If F3 PASSES with direct rbac.EvaluateRBAC
// calls (no memo), Path B deferral is validated. If F3 FAILS, memo
// plumbing is pulled forward as Phase 2d before dual-gate review.

package cache

import (
	"context"
	"sync"
)

// authzMemoKey identifies a single EvaluateRBAC verdict for memoisation.
// We don't fold Username / Groups into the key — the memo is per-REQUEST
// and the identity doesn't change mid-request. Construction binds the
// memo to a SubjectIdentity once; EvaluateOrLookup uses that identity
// for every cache-miss.
type authzMemoKey struct {
	Verb, Group, Resource, Name, Namespace string
}

// authzMemoEntry is the cached verdict. Stored exactly as EvaluateRBAC
// returned it; the Err field carries an evaluator error (degrade-to-deny)
// so two layers asking the same question get identical results.
type authzMemoEntry struct {
	Allowed           bool
	MatchedBindingUID string
	Err               error
}

// EvaluateAuthzFunc is the function signature the memo invokes on miss.
// Matches rbac.EvaluateRBAC's post-Ship-0.30.242 surface
// (allowed, matchedBindingUID, err). Threaded as a hook so internal/cache
// cannot import internal/rbac (package boundary — design §4.3).
type EvaluateAuthzFunc func(ctx context.Context, opts EvaluateOptions) (bool, string, error)

// EvaluateOptions mirrors internal/rbac.EvaluateOptions to avoid the
// package-cycle. The rbac caller passes its own EvaluateOptions; the
// memo carries the cache-package mirror so internal callers don't need
// to import rbac. Fields are explicit (not aliased) to keep the cache-
// package surface self-contained.
type EvaluateOptions struct {
	Username  string
	Groups    []string
	Verb      string
	Group     string
	Resource  string
	Namespace string
	Name      string
}

// RequestAuthzMemo memoises EvaluateRBAC verdicts within ONE /call
// request. Bound at construction to the snapshot generation observed
// at request entry (m.snapPublishSeq). On mid-request snapshot
// republish the memo invalidates itself and re-derives.
//
// CONCURRENCY: the memo is NOT safe for concurrent EvaluateOrLookup
// from multiple goroutines. The /call request path is single-goroutine
// per request (the apistage fan-out is sequential within one Resolve).
// If a future caller needs concurrent access, wrap the mu field; the
// scaffold deliberately omits that complexity until a caller proves it
// needed.
type RequestAuthzMemo struct {
	// snapPublishSeq is the snapshot generation this memo is bound to.
	// Reset to the current snap's PublishSeq on every mismatch.
	snapPublishSeq uint64

	// identity is the request's subject. Bound once at construction by
	// NewRequestAuthzMemo. The Evaluate fn signature accepts opts that
	// MUST carry the same Username/Groups; identity drift is a caller bug.
	identity SubjectIdentity

	// evaluate is the wrapped EvaluateRBAC function (injected so
	// internal/cache doesn't import internal/rbac).
	evaluate EvaluateAuthzFunc

	mu      sync.Mutex
	entries map[authzMemoKey]authzMemoEntry

	// hits / misses are diagnostic counters surfaced via Stats. They
	// help measure the per-/call hit rate (design §5.1 noted the
	// expected hit rate is modest; the counters falsify that claim
	// empirically).
	hits   uint64
	misses uint64
	resets uint64
}

// NewRequestAuthzMemo creates a memo bound to the snapshot generation
// observed at construction. The memo carries the caller's identity +
// the injected EvaluateRBAC function.
//
// The snap-coherent PublishSeq stamping (rbac_snapshot.go writer fix)
// guarantees a reader who calls LiveRBACSnapshot() at this moment
// observes a snapshot whose PublishSeq is already coherent.
func NewRequestAuthzMemo(identity SubjectIdentity, evaluate EvaluateAuthzFunc) *RequestAuthzMemo {
	var seq uint64
	if snap := LiveRBACSnapshot(); snap != nil {
		seq = snap.PublishSeq
	}
	return &RequestAuthzMemo{
		snapPublishSeq: seq,
		identity:       identity,
		evaluate:       evaluate,
		entries:        make(map[authzMemoKey]authzMemoEntry, 8),
	}
}

// EvaluateOrLookup returns the cached verdict for opts, or evaluates via
// the wrapped EvaluateRBAC function on miss. The verdict is cached for
// the memo's lifetime (modulo snapshot republish — see below).
//
// The opts.Username + opts.Groups MUST match the identity passed to
// NewRequestAuthzMemo. The memo carries the identity for documentation
// + future hardening; it does NOT re-validate identity per call (the
// caller threading bug — passing a different identity to the same memo
// — would manifest as cross-identity verdict leakage WITHIN the
// request, but the verdict is still consistent for THIS memo's identity
// because it's the identity that was used at the cache-miss).
//
// CRITICAL — design §5.3: after the evaluate call we check whether the
// snapshot has shifted (cur.PublishSeq != m.snapPublishSeq). If it has,
// we RESET the cache (clear entries map, rebind to the new seq) before
// storing the just-computed verdict. This guarantees the stored
// verdict's snapshot is the memo's current snapshot — no stale verdict
// can survive across a snapshot generation.
func (m *RequestAuthzMemo) EvaluateOrLookup(
	ctx context.Context, opts EvaluateOptions,
) (allowed bool, matchedBindingUID string, err error) {
	if m == nil || m.evaluate == nil {
		// Defensive: a nil memo or nil evaluate function means callers
		// should treat as cache=off equivalent. Return zero values; the
		// caller's per-layer EvaluateRBAC fallback (Path B) handles the
		// actual authz.
		return false, "", nil
	}

	key := authzMemoKey{
		Verb:      opts.Verb,
		Group:     opts.Group,
		Resource:  opts.Resource,
		Name:      opts.Name,
		Namespace: opts.Namespace,
	}

	m.mu.Lock()
	if entry, ok := m.entries[key]; ok {
		m.hits++
		m.mu.Unlock()
		return entry.Allowed, entry.MatchedBindingUID, entry.Err
	}
	m.misses++
	m.mu.Unlock()

	// Evaluate (outside the lock — the evaluator may take milliseconds).
	allowed, matchedBindingUID, err = m.evaluate(ctx, opts)

	// Snapshot-coherence check: if the snapshot shifted between memo
	// construction and now, the existing entries are stale. Reset before
	// recording.
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur := LiveRBACSnapshot(); cur != nil && cur.PublishSeq != m.snapPublishSeq {
		m.snapPublishSeq = cur.PublishSeq
		m.entries = make(map[authzMemoKey]authzMemoEntry, 8)
		m.resets++
	}
	m.entries[key] = authzMemoEntry{
		Allowed:           allowed,
		MatchedBindingUID: matchedBindingUID,
		Err:               err,
	}
	return allowed, matchedBindingUID, err
}

// Stats returns the memo's diagnostic counters. Reads are NOT
// synchronised with concurrent EvaluateOrLookup callers — the values
// are point-in-time snapshots, used for debug logging at request exit.
//
// hits + misses == EvaluateOrLookup call count (sans the nil-memo
// short-circuit). resets is the count of mid-request snapshot
// republishes observed.
func (m *RequestAuthzMemo) Stats() (hits, misses, resets uint64) {
	if m == nil {
		return 0, 0, 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hits, m.misses, m.resets
}

// Identity returns the SubjectIdentity the memo was bound to. Read-only;
// intended for diagnostic logging.
func (m *RequestAuthzMemo) Identity() SubjectIdentity {
	if m == nil {
		return SubjectIdentity{}
	}
	return m.identity
}

// SnapPublishSeq returns the snapshot generation the memo is currently
// bound to. Updated on every mid-request snapshot republish (reset
// path). Intended for diagnostic logging.
func (m *RequestAuthzMemo) SnapPublishSeq() uint64 {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapPublishSeq
}
