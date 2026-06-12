// Package cache — RBAC subsystem map (Task #333, Option C′).
//
// rbac_doc.go is a DOC-ONLY file: a navigability fence around the RBAC
// cluster that lives inside the single `cache` package. It contains no
// code — only this map. It exists because the audit
// (docs/audit-clean-code-2026-06-09.md Finding 2) asked for a "where is
// the RBAC seam" map; the directory carve that would have answered it
// physically was evaluated and REJECTED (see CARVE VERDICT below), so the
// map lives here instead. New contributors start here.
//
// ── THE 7-FILE RBAC CLUSTER ─────────────────────────────────────────────
//
// These files form the RBAC cohesion cluster (they share the `rbac_` /
// `bindings_by_gvr` / `match_subject` / `request_authz_memo` prefixes). One
// line each; read the file header for the full contract.
//
//   - rbac_snapshot.go        — the immutable published RBAC snapshot
//                               (struct + typed conversions + subject
//                               index), its rebuild glue, and the two
//                               ResourceWatcher methods that fuse it to the
//                               informer core (Snapshot, rbacSnapshotEventHandlers).
//   - bindings_by_gvr.go      — the incremental BindingsByGVR reverse index:
//                               per-{group,resource} binding buckets, the
//                               build-once path, and the private index types.
//   - bindings_by_gvr_delta.go— the per-event delta hooks (onBinding{Add,
//                               Update,Delete}, onRole*) that keep that index
//                               current on RBAC informer events.
//   - bindings_by_gvr_metrics.go— the index's expvar surfaces (the delta
//                               drift canary + the RAFullList serve counters),
//                               registered under a sync.Once gated by Disabled().
//   - match_subject.go        — the SINGLE SOURCE OF TRUTH for binding-identity
//                               derivation: SubjectIdentity, BindingUIDFrom{CRB,
//                               RB}, pickRepresentativeFromSubjects.
//   - request_authz_memo.go   — the per-/call RequestAuthzMemo (memoises
//                               EvaluateRBAC verdicts within one request,
//                               snapshot-coherent) + the EvaluateOptions mirror.
//   - rbac_snapshot_expvar.go — the read-only expvar exposure of the snapshot
//                               publish-sequence counter (snowplow_rbac_publish_seq).
//
// NOT in the cluster: secrets_snapshot.go shares the "snapshot" NAMING but is
// the Secrets cache (a different cohesion cluster); it CONSUMES rbac
// (scheduleRBACRebuild) but is not part of it.
//
// Two functions the audit flagged as mis-filed have been relocated (Task
// #333 pieces 2+3, both intra-package, behaviour-identical):
//   - pickRepresentativeFromSubjectKeys (prewarm_enumeration.go) now ADAPTS
//     onto the match_subject.go SOT instead of re-implementing its kind-switch.
//   - dispatchAPIStageKey moved OUT of request_authz_memo.go into resolved.go,
//     next to ComputeKey / ResolvedKeyInputs — it is an L1-dispatch-key concern,
//     not a memo concern (the memo is only one of its inputs).
//
// ── PACKAGE-LEVEL SINGLETONS (process-wide state) ───────────────────────
//
// The cluster owns the following process-wide state. There is one of each
// per process; tests reset them via the *ForTest helpers.
//
//   - rbacSnap atomic.Pointer[RBACSnapshot]   (rbac_snapshot.go:173)
//       The sole publish container: single-writer / many-reader. Load()
//       returns nil pre-readiness; readers degrade to deny (AC-B.8).
//   - rbacRebuildLock / rbacRebuildDirty atomic.Bool (rbac_snapshot.go:187-188)
//       Single-writer tryLock + dirty-flag; collapses event bursts into one
//       follow-up rebuild (handlers must not block).
//   - rbacSnapshotPublishSeq atomic.Uint64    (rbac_snapshot.go:238)
//       Publish generation, bumped once per successful publish; read via
//       RBACGen() and the snowplow_rbac_publish_seq expvar.
//   - rbacSnapshotMissCount atomic.Uint64     (rbac_snapshot.go:197)
//       roleRefPermits snapshot-miss counter (AC-B.10 ratio probe).
//   - rbacSnapshotHandlerWired atomic.Bool    (rbac_snapshot.go:917)
//       Set once a typed-RBAC informer is wired with the snapshot handler;
//       AssertRBACSnapshotWired panics at boot if it stays false.
//   - rbacSnapshotAssertionDisabled atomic.Bool (rbac_snapshot.go:930)
//       Test-only escape hatch making AssertRBACSnapshotWired a no-op.
//   - bindingsIndexInstance / bindingsIndexOnce (bindings_by_gvr.go:159-160)
//       The BindingsByGVR index singleton + its sync.Once; the accessor is
//       bindingsByGVRSingleton() (bindings_by_gvr.go:163). The delta hooks,
//       the build path, and prewarm_enumeration.go all reach this ONE index.
//
// Three init() functions register expvar/handler surfaces during package
// init: bindings_by_gvr_metrics.go:33, rbac_snapshot_expvar.go (via the
// RegisterRBACSnapshotExpvar sync.Once, called from main.go), and
// rbac_snapshot.go:898 (the rbac.snapshot_writer HandlerExtension registration).
//
// ── LOCK-ORDERING CONTRACT (single source of truth) ─────────────────────
//
// The RBAC cluster touches THREE independent synchronisation primitives.
// Each rule below is LIFTED from an existing scattered comment — the cited
// file:line is the authority; this section only collects them in one place.
// Do NOT add ordering rules here without first adding them at a real lock
// site and citing it.
//
//  1. rw.mu (sync.RWMutex on ResourceWatcher, watcher.go:117) is the single
//     lock fanning the informer-core maps — rw.informers AND the four-conjunct
//     servability maps share it ("Both are guarded by rw.mu (same lock as
//     rw.informers) — no separate mutex", watcher.go:126-127; also :165, :215).
//     The RBAC rebuild reads rw.syncCh / rw.informers under rw.mu
//     (waitAndPublishInitialRBACSnapshot takes rw.mu.RLock at
//     rbac_snapshot.go:1026).
//
//  2. NEVER hold rw.mu while waiting on goroutineWG. The watcher's owned
//     goroutines take rw.mu.RLock on their exit paths, so Stop() releases
//     rw.mu BEFORE goroutineWG.Wait() — waiting under the write lock would
//     deadlock ("Never call goroutineWG.Wait() while holding rw.mu … the
//     goroutines take rw.mu.RLock on their exit paths", watcher.go:259-262;
//     the implementation site is watcher.go:2116-2123, which unlocks rw.mu
//     at :2116 before the Wait at :2140).
//
//  3. The BindingsByGVR index has its OWN RWMutex (the `mu` field on
//     bindingsByGVRIndex, bindings_by_gvr.go:142 — the header comment at
//     bindings_by_gvr.go:50 still calls it by the legacy name `bindingsIndexMu`).
//     It is INDEPENDENT of rw.mu: deltas take its write lock, enumeration its
//     read lock ("we take the write lock for deltas and the read lock for
//     enumeration", bindings_by_gvr.go:50-57). The delta hooks run on the RBAC
//     informer processor goroutine and hold idx.mu only for a few map ops
//     (bindings_by_gvr_delta.go:27-32). No path nests rw.mu and idx.mu: the
//     rebuild reads the snapshot, not rw, while under idx.mu.
//
//  4. The published *RBACSnapshot is IMMUTABLE post-publish and read LOCK-FREE
//     via the rbacSnap atomic.Pointer ("Snapshot is IMMUTABLE post-publish …
//     atomic.Pointer provides the single memory-ordered Store/Load",
//     rbac_snapshot.go:34-41; Snapshot() is "Lock-free: a single atomic load",
//     rbac_snapshot.go:261-264). Therefore the index's role-rule lookups
//     (rulesForRoleRef) read the snapshot lock-free even while holding idx.mu
//     (bindings_by_gvr.go:57-58, bindings_by_gvr_delta.go:31-32) — this is why
//     rule 3's "no nesting" holds: the snapshot read is not a lock acquisition.
//
//  5. The rebuild itself is single-writer via the rbacRebuildLock atomic.Bool
//     tryLock; at most ONE in-flight rebuild goroutine exists, a queued rebuild
//     rides the next loop iteration of that same goroutine ("max concurrent
//     rebuild goroutines is exactly 1 regardless of event burst rate",
//     rbac_snapshot.go:181-185).
//
// ── CARVE VERDICT: directory split evaluated and REJECTED ────────────────
//
// Splitting this cluster into a `cache/rbac` sub-package was designed and
// rejected — see docs/task-332-cache-rbac-carve-design-2026-06-12.md (the
// NO-GO; this file is its accepted Option C′ replacement). Two disqualifiers:
//
//   - HARD Go blocker: rbac_snapshot.go defines TWO methods on
//     *ResourceWatcher — Snapshot() (rbac_snapshot.go:262) and
//     rbacSnapshotEventHandlers() (rbac_snapshot.go:856). ResourceWatcher is
//     the cache package's central type (watcher.go) and cannot move, and Go
//     forbids defining its methods in another package — so the snapshot file
//     could not move whole; it would have to be split.
//   - The cut-set is dense + bidirectional: a carve would force exporting
//     ~10 currently-private symbols (incl. the index value types bindingEntry
//     / bindingID / subjectKey and the bindingsByGVRSingleton accessor) to
//     satisfy reverse-consumers like prewarm_enumeration.go, roughly DOUBLING
//     the RBAC public surface — for zero correctness/feature/perf upside, and
//     against project_caching_is_provisional (the cache must stay cleanly
//     removable as ONE unit). The design doc has the full cut-set + risk table.
package cache
