# Frontend guide — integrating snowplow's live-refresh SSE (per widget)

**Audience:** Krateo portal frontend. **Snowplow feature:** live-refresh-coherence (Ship 1).
**Status:** implemented + released on the 1.5.x line; default-ON when the cache is on.

---

## 1. What it gives you

When a cluster object behind a widget changes, snowplow's cache re-resolves the
affected entry in the background and then **pushes a one-line signal** to the
browser saying *"this widget's data is fresh — refetch it."* You get live UI
updates **without polling**, and the refetch is a warm cache **hit** (no apiserver
load).

It is **signal-only**: the SSE event carries *just a cache key*, never data. You
always get the actual content from a normal `GET /call` (which re-applies the
user's RBAC at serve time). So the stream can be shared/coalesced without ever
leaking another user's row-level visibility.

```
object changes ─▶ snowplow informer ─▶ dirty-mark ─▶ background re-resolve
                                                          │
   browser ◀── event: refresh\ndata: <l1Key> ◀───────────┘   (GET /refreshes)
      │
      └─▶ GET /call (same widget)  ──▶ warm L1 HIT, fresh content ──▶ update UI
```

---

## 2. The contract (what to build against)

### 2.1 Endpoint
```
GET /refreshes?sub=<base64-url(JSON coordinate array)>
Content-Type: text/event-stream
```
One **multiplexed** stream **per browser tab** — *not* one per widget. You arm all
the widgets the tab has mounted on a single connection.

### 2.2 Auth — cookie (EventSource can't set headers)
A browser `EventSource` cannot set the `Authorization` header, so snowplow reads
the JWT from a **session cookie**:
```js
new EventSource(url, { withCredentials: true })   // sends the session cookie
```
- Cookie name is deploy-configurable (`REFRESH_SESSION_COOKIE`, default
  **`krateo-session`**). Confirm the portal's actual session-cookie name with ops.
- The token is **never** put in the URL (it would leak in logs/referrer).
- Non-browser clients (tests, a polyfill) may instead send `Authorization: Bearer <jwt>`.
- Cross-origin? The snowplow CORS config must allow credentials.

### 2.3 Arming — send widget **coordinates**, not keys
`?sub=` is base64 of a JSON **array**, one object per widget you want notified
about. **You send coordinates; snowplow derives the cache key under the
authenticated identity** (you can't subscribe to someone else's key — it's
forgery-proof by construction):

```jsonc
[
  {
    "class": "widgets",          // the X-Snowplow-Refresh-Class from /call — see §2.5
    "group": "widgets.templates.krateo.io",
    "version": "v1beta1",
    "resource": "barcharts",     // the widget's GVR…
    "namespace": "demo",
    "name": "cpu-by-node",       // …and name — exactly as in your /call
    "perPage": 0,
    "page": 0,
    "extras": { "compositionId": "fsa-y8" }   // same extras you passed to /call
  }
]
```
**The coordinates must match the widget's `/call` exactly** (same GVR, ns, name,
page, extras) — that's what makes the derived key equal the key the event will
carry. Limits: **≤ 512 widgets** per connection, **≤ 16 KB** decoded `sub`.

### 2.4 Matching events back to widgets — the `X-Snowplow-Refresh-Key` + `X-Snowplow-Refresh-Class` headers
Every `GET /call` response that was cache-keyed carries TWO headers:
```
X-Snowplow-Refresh-Key:   <l1Key>
X-Snowplow-Refresh-Class: <class>   # widgets | widgetContent | restactions
```
- `X-Snowplow-Refresh-Key` is the exact key this widget's refresh events will
  arrive under. **Store `l1Key → widget` when you render**, then match incoming
  events by it.
- `X-Snowplow-Refresh-Class` is the class snowplow actually keyed this response
  under. **Arm the subscription with this class verbatim** — no guessing. (You
  arm by *coordinates + class*; you match by *key*. Both resolve to the same
  `l1Key`.)

Both headers are additive and only present when the response was cache-keyed
(absent on a cache-off / RBAC-skipped / identity-less response — in which case
there is nothing to arm). They are in the CORS `ExposedHeaders` list so a
cross-origin fetch can read them.

### 2.5 `class` — read it from the response, don't guess
**Send back the value of `X-Snowplow-Refresh-Class` from the `/call` response.**
That is the class snowplow keyed the response under, so it is always the
armable one:

| You rendered | `X-Snowplow-Refresh-Class` you'll get back |
|---|---|
| a RESTAction (`/call?resource=<ra>`) | `restactions` |
| an RBAC-*sensitive* widget (`/call?resource=<widget>`) | `widgets` |
| an RBAC-*insensitive* widget (shared shell) | `widgetContent` |

This resolves the earlier widgets-vs-widgetContent ambiguity: RBAC-insensitive
widgets are served from a shared shell keyed under `widgetContent`, and the
header now tells you that directly — no need to arm both classes. Other internal
classes (`apistage`, `raFullList`) are never stamped on a `/call` response, so
the frontend never arms them.

### 2.6 The event frames
```
event: refresh
data: <l1Key>

: keepalive
```
- **`event: refresh`** is a **named** event → listen with
  `es.addEventListener('refresh', …)`, **not** `es.onmessage`.
- `: keepalive` is an SSE **comment** (every 20 s) — `EventSource` ignores it; it
  just keeps the connection alive. You don't handle it.

---

## 3. Per-widget integration flow

1. **On render**, call `GET /call` for the widget as you do today. Read the
   **`X-Snowplow-Refresh-Key`** response header → record `key → widgetId`, and
   the **`X-Snowplow-Refresh-Class`** header → use as this widget's `class`.
2. **Collect coordinates** for every mounted widget (the same params you passed to
   `/call`), using the `class` from each widget's `X-Snowplow-Refresh-Class`.
3. **Open one `EventSource`** for the tab:
   `GET /refreshes?sub=<base64url(coordsArray)>` with `withCredentials: true`.
4. **On `refresh`** (`addEventListener('refresh', …)`): `e.data` is an `l1Key` →
   look up the widget(s) with that key → **refetch their `/call`** (warm hit, fresh
   data) → update the UI.
5. **Throttle** the refetch **per widget (~5 s)**. Snowplow already coalesces
   duplicate signals server-side (250 ms window) and may drop bursts by design, so
   treat a `refresh` as *"data changed, refetch when convenient"*, not *"refetch
   instantly every time."*
6. **On mount/unmount changes**, rebuild `sub` and re-open the connection (close
   the old one). It's cheap; keep it to one connection per tab.

---

## 4. Reference implementation (TypeScript)

```ts
type Coords = {
  class: 'widgets' | 'widgetContent' | 'restactions';
  group: string; version: string; resource: string;
  namespace: string; name: string;
  perPage?: number; page?: number;
  extras?: Record<string, unknown>;
};

const b64url = (s: string) =>
  btoa(s).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');

class RefreshManager {
  private es?: EventSource;
  private armed = new Map<string, Coords>();        // widgetId -> coords
  private keyToWidgets = new Map<string, Set<string>>(); // l1Key  -> widgetIds
  private lastRefetch = new Map<string, number>();  // widgetId -> ts (throttle)

  /** Call right after a widget's /call resolves. */
  onWidgetRendered(widgetId: string, coords: Coords, refreshKeyHeader: string | null) {
    this.armed.set(widgetId, coords);
    if (refreshKeyHeader) {
      let set = this.keyToWidgets.get(refreshKeyHeader);
      if (!set) this.keyToWidgets.set(refreshKeyHeader, (set = new Set()));
      set.add(widgetId);
    }
    this.reconnect(); // re-arm the (single) stream with the new widget set
  }

  onWidgetUnmounted(widgetId: string) {
    this.armed.delete(widgetId);
    for (const set of this.keyToWidgets.values()) set.delete(widgetId);
    this.reconnect();
  }

  private reconnect() {
    this.es?.close();
    const coords = [...this.armed.values()];
    if (coords.length === 0) return;
    const sub = b64url(JSON.stringify(coords));
    this.es = new EventSource(`/refreshes?sub=${sub}`, { withCredentials: true });
    this.es.addEventListener('refresh', (e) => this.onRefresh((e as MessageEvent).data));
    // EventSource auto-reconnects on drop and re-sends this same URL; no extra code.
  }

  private onRefresh(l1Key: string) {
    const widgets = this.keyToWidgets.get(l1Key);
    if (!widgets) return;
    const now = Date.now();
    for (const id of widgets) {
      if (now - (this.lastRefetch.get(id) ?? 0) < 5000) continue; // 5s/widget throttle
      this.lastRefetch.set(id, now);
      this.refetchWidget(id); // your existing /call refetch -> update UI
    }
  }

  // refetchWidget: re-issue the widget's GET /call; it returns a warm L1 hit
  // with the fresh content, and a fresh X-Snowplow-Refresh-Key (re-record it).
}
```

---

## 5. Graceful degradation & edge cases

- **Feature off / cache off** → `/refreshes` returns a clean **idle stream**
  (heartbeats only, *zero* refresh events). Your code simply never fires a
  `refresh` → fall back to your own polling/throttle. No errors, no hangs.
- **`400`** → malformed/oversized `sub` (bad base64, > 512 entries, > 16 KB).
- **Coordinates the user can't access** are **silently skipped** (fail-closed) —
  you won't get events for them; that's correct, not an error.
- **Always refetch via `/call`** on a signal — never treat the event `data` as
  content. RBAC is re-applied at `/call` serve time.
- **One connection per tab.** Arming 512 widgets on one stream is fine; opening
  512 EventSources is not.
- **Reconnect** is automatic (native `EventSource`); on reconnect it re-sends the
  same `?sub=`, so no extra handling — just rebuild `sub` when the widget set
  changes.
- **Fires on CONTENT CHANGE, not on every reconcile.** A `refresh` event is
  emitted only when the widget's *resolved rendered output* actually changes —
  not on every Kubernetes reconcile of the underlying resource. A resource that
  reconciles repeatedly but produces **stable output** (e.g. a stuck or erroring
  composition whose status keeps re-writing to the same values) will **never**
  publish a refresh. This is by design (it avoids spurious refetches), but it
  confounds a naive test: *"I watched a failing composition and got nothing"* is
  expected, not a bug. **When testing, pick a target whose rendered content
  actually changes** (e.g. scale a deployment, edit a value the widget displays)
  — and use `/debug/refreshes` (`published` climbing) to confirm the server is
  emitting at all.

---

## 6. Config knobs (ops / snowplow side, for reference)

| Env | Default | Meaning |
|---|---|---|
| `REFRESH_SSE_ENABLED` | on (when cache on) | master toggle for the layer |
| `REFRESH_SESSION_COOKIE` | `krateo-session` | cookie name the JWT is read from |
| `REFRESH_COALESCE_WINDOW_MS` | `250` | server-side per-key dedup window |

Live-refresh requires `CACHE_ENABLED=true` (it rides the cache's refresher).

---

## 7. Quick smoke test (non-browser)

```sh
SUB=$(printf '[{"class":"restactions","group":"templates.krateo.io","version":"v1",
  "resource":"restactions","namespace":"demo","name":"blueprints-list","extras":{}}]' \
  | base64 | tr '+/' '-_' | tr -d '=')

curl -N -H "Authorization: Bearer $JWT" \
  "https://<snowplow>/refreshes?sub=$SUB"
# → ': keepalive' every 20s; 'event: refresh / data: <l1Key>' when that RA's data changes.
```

---

## 8. Resolved items

- ✅ **`widgets` vs `widgetContent` class** ambiguity (§2.5) — **RESOLVED**.
  snowplow now stamps `X-Snowplow-Refresh-Class` on every cache-keyed `/call`
  response with the exact class it keyed under (`widgets` for RBAC-sensitive
  widgets, `widgetContent` for the shared shell, `restactions` for RESTActions).
  **Arm the subscription with that header value verbatim** — no arm-both, no
  guessing. (snowplow 1.5.5+ / the `X-Snowplow-Refresh-Class` header is in the
  CORS `ExposedHeaders` list.)
