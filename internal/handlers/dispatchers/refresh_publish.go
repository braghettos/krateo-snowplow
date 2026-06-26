// refresh_publish.go — #62: publish a live-refresh signal on a genuine
// COLD-dispatch Put when (and only when) a /refreshes connection is armed for
// that key.
//
// Why (the #61 TTL-evict residual, RCA §C-2 / backlog #62): #61 made a
// dep-change dirty-mark the armed top-level key so the REFRESHER re-resolves
// it and PublishRefreshes. But if the armed key was TTL-EVICTED before the
// resource churned, its dep edges were removed (RemoveL1Key) → the dep-change
// matches nothing → the refresher never re-resolves it → no PublishRefresh →
// that cycle's delivery is missed. The frontend re-arms on the next /call,
// which COLD-fills the key (a genuine Put here, dep edges re-recorded) — but
// that cold Put historically did NOT publish (PublishRefresh fires only on the
// refresher path, resolve_populate.go). So a viewer whose key cold-filled
// after an eviction sees one stale frame until the NEXT churn. This closes
// that gap: announce the cold-fill to any already-armed subscriber.
//
// SCOPE IS THE SAFETY BOUNDARY (C62-4 placement + class scope): call this
// ONLY from the genuine-Put else-if of restactions.go + widgets.go — AFTER the
// Put + dep-Record, NEVER on the stage-error / external-skip / declined
// branches (no Put there → publishing would signal a non-event). Do NOT call
// it for widgetContent (the one class ComputeKey does NOT fold BindingUID into
// — resolved.go:669 — so a publish on its shared key could cross-deliver to a
// differently-authorized viewer) or apistage (never armed — dead surface).
//
// Cost-proportional + leak-safe by construction: HasRefreshSubscriber is an
// O(1) RLock map read; when no connection is armed for the key it returns
// false and we never publish (counters stay flat). When armed, PublishRefresh
// fans out ONLY to subscribers whose armed set contains this exact key — and
// the key is the per-identity DeriveSubscriptionKey (BindingUID folded in for
// identity-bearing classes), so the cold-filler's key matches only a
// subscriber armed under the SAME identity. Both HasRefreshSubscriber and
// PublishRefresh take the hub RLock, so this is safe under concurrent
// Subscribe/unsubscribe (no deliver-after-unsub: PublishRefresh re-checks the
// armed set under the lock).
package dispatchers

import "github.com/krateoplatformops/snowplow/internal/cache"

// publishIfSubscribed announces a genuine cold-dispatch Put of l1Key to any
// already-armed /refreshes subscriber, and is a no-op when none is armed
// (cost-proportional) or when the SSE layer / cache is off (both nil-safe via
// the broadcaster). l1Key MUST be the just-Put genuine cache key (the
// per-identity DeriveSubscriptionKey-equivalent the serve path stamps).
func publishIfSubscribed(l1Key string) {
	if l1Key == "" {
		return
	}
	if cache.HasRefreshSubscriber(l1Key) {
		cache.PublishRefresh(l1Key)
	}
}
