# Hub Events Stream — Design

**Status:** DESIGN — awaiting operator approval. No implementation until Tijs signs off.
**Date:** 2026-07-07
**Author:** Claude (Fable), design session dispatched via AgentFlow EXECUTABLE `Ao2FFASs4xqXKvF96TdAxU7rFWr6yKHodOWoSrTUTIno.7c7cde0b-c755-45dc-a103-61090e3fedd0`
**TaskFlow task:** `ApWJVWXvVKIIMnH6CP6u8HUyU2gLvyYGnwRlgrWAUwcP.8e6b5675-aa85-4134-9c44-76c88a47ee83`

## TL;DR

Add a `GET /events` change feed to the hub: an append-only, per-hub **event journal** (durable, monotonic sequence) exposed over **SSE** for live push and plain **JSON with `?since=<cursor>`** for catch-up/long-poll. Events are *skinny* (ref + revision + metadata, no body) and **realm-filtered per subscriber** with the exact same `realm.CanRead` gate as `GET /{ref}`. In proxy mode the hub runs one upstream subscriber (same lifecycle pattern as `SyncPending`), applies upstream events through the existing `CacheLocally` path, and re-emits into its **own** journal with its **own** cursor space — so cursors are strictly per-hop and an upstream restart/journal-loss never invalidates downstream subscribers. Delivery is **at-least-once, idempotent by (ref, revision)**; consumers treat events as cache-invalidation hints, which is exactly what the existing revision/ETag machinery already handles.

Three options considered; **Option A (journal + SSE + `?since=` replay)** is recommended. Option B (long-poll-only changes feed) is Option A minus the streaming transport — it shares ~90% of the work and remains as the built-in fallback. Option C (WebSocket bidirectional sync) is rejected for v1.

---

## 1. Problem

Every consumer that wants to know "what changed?" polls today:

- `instructiongraph-js` sync store (`src/store/sync.js`) issues conditional GETs per ref and merges `search`/`inbound` results from both stores on every call — freshness is only as good as the polling cadence of whatever sits above it.
- Standing daemons (AgentFlow executors waiting for `EXECUTION_REQUEST`s, transcript recorders, TaskFlow-style UIs) loop over `/search?type=...` on timers.
- A proxy hub only discovers upstream changes when a client happens to GET a ref (`ensureFresh`) or when a list response floats by (`cacheUpstreamListRefs`, which then crawls refs at 200 ms intervals — `serving/proxy.go:1108`).

Polling burns rate-limit budget (all proxy traffic shares one IP against upstream's per-IP limiter — `upstream/client.go:167-172`), adds latency equal to the poll interval, and doesn't scale with the number of watchers.

The hard requirement is **federation**: hubs run in proxy mode chained to an upstream hub (`mode = "proxy"`, the default — `config.go:66`). Events must stream *across* hubs: a proxy subscribes upstream and re-emits downstream, and cursor/resume semantics must survive restarts on either side of every hop.

## 2. Current architecture (code study, 2026-07-07)

Facts that constrain the design, with sources:

| Fact | Where | Consequence for events |
|---|---|---|
| Storage is one mutable JSON file per ref; no append-only log of any kind | `storage/store.go` | There is **no existing substrate for replay** — we must add one (journal) or accept scan-based diffing |
| In-memory index is rebuilt by scanning all files at startup | `storage/index.go:42`, `main.go:46` | Restart loses all in-memory state; anything needed for resume must be on disk |
| Object timestamps (`created_at`/`updated_at`) are **author-supplied** and signed; store mtime is set to them (`os.Chtimes`) | `object/types.go:83-89`, `storage/store.go:93` | **Timestamps cannot be cursors.** Clock skew, identical timestamps, and legitimately backdated objects would silently drop events from a `?since=<timestamp>` scan |
| Writes are only `PUT /{ref}` with strictly increasing `revision`; conflicts → 409 | `serving/handlers.go:309-315` | (ref, revision) is a natural idempotency key; there is **no delete** today, so tombstones are schema-reserved, not implemented |
| The `/search` cursor is `{t: updatedAt, r: ref}` over a (UpdatedAt DESC) sort | `object/types.go:117`, `serving/paginate.go` | Existing cursors paginate a *snapshot ordering*, they don't enumerate *changes*; unrelated to event cursors |
| Realm read gate is `realm.CanRead(realms, authPubkey, shared)`: public (`dataverse001`, `server-public`), identity realm (realm == subscriber pubkey), shared realms from config (SIGHUP-reloadable) | `realm/realm.go:57`, `main.go:124-145` | The event stream must apply the **same** gate per subscriber per event, against the *live* shared-realm config |
| Private objects return 404, not 403, to avoid leaking existence | `serving/handlers.go:123` | Even skinny events (ref + type) leak existence → filtering is mandatory, not an optimization |
| Auth tokens are random bearer tokens held **in memory**; hub restart invalidates all of them | `auth/auth.go:21-27` | Long-lived subscriptions must expect mid-stream auth death and reconnect+re-auth cleanly |
| Proxy → upstream requests are **unauthenticated** (no hub identity, no token) | `serving/proxy.go` (all upstream calls), `upstream/client.go` | A proxy's upstream subscription sees **public events only** — which exactly matches what a proxy can cache from upstream today. Private objects flow *up* (push) but never *down*. Documented asymmetry, future work |
| Proxy PUT path: global objects forwarded synchronously; on upstream-down stored locally + `sync_pending/` + background drain | `serving/proxy.go:246-370`, `upstream/sync.go` | Local writes already surface upstream-ward; events don't change the write path. `SyncPending` is the template for the upstream-subscriber lifecycle (Start/Stop, health-check gating) |
| `CacheLocally` refuses revision downgrades and no-ops on equal revisions | `serving/proxy.go:638-674` | Applying upstream events through this path makes event application idempotent for free |
| Upstream client: availability flag, fast-fail, 30 s health checker; 429/5xx = "down" | `upstream/client.go` | Subscriber reconnect can key off the same availability signal |
| `http.Server` has `WriteTimeout: 30 * time.Second` | `main.go:119` | An SSE handler **must** clear the per-connection write deadline (`http.NewResponseController(w).SetWriteDeadline(time.Time{})`, Go ≥1.20) or every stream dies at 30 s |
| Rate limiter is per-IP per-minute/day | `auth/ratelimit.go` | One SSE connection = one counted request; streaming *relieves* rate-limit pressure rather than adding to it |
| Client-side: `hub.js` is plain fetch; `sync.js` composes local+remote stores; shared-realm memberships cached to `configDir/config/shared-realms.json` | `instructiongraph-js/src/store/*.js` | Cursor persistence has an obvious home (`config/events-cursor.json`); note the native browser `EventSource` API cannot set an `Authorization` header — Node consumers should use fetch-streaming, browsers ride the `dv_session` cookie |

### Topology

Federation is a **tree**: each proxy has exactly one upstream (`upstream_url`), chains are legal (proxy → proxy → root), there are no cycles. This kills the need for federation-wide event IDs, vector clocks, or origin-loop suppression. Every design below leans on that.

## 3. Core design decisions (independent of option)

These hold for any of the three options:

1. **Per-hub cursors, never federation-wide.** A globally ordered cursor across hubs is impossible without global coordination (hubs apply changes in different orders; a proxy interleaves upstream events with local private writes that upstream never sees). Instead: every hub owns a private, opaque cursor space; a subscriber's cursor is only meaningful to the hub that issued it. A proxy is *itself a subscriber* to upstream and persists *its* upstream cursor as local state. Resume works hop-by-hop, and a reset at one hop is absorbed by that hop's cache-revalidation instead of cascading downstream.

2. **Skinny events, at-least-once, idempotent by (ref, revision).** Events carry metadata only; the body is fetched via the existing `GET /{ref}` (conditional, cached, realm-checked). This keeps the journal small, avoids double-serving 10 MB BLOBs, and makes duplicate delivery harmless — a consumer that re-fetches an unchanged ref gets a 304. Exactly-once is not worth its cost here; every consumer already handles revision comparison (`sync.js` get(), `CacheLocally`).

3. **Author timestamps are display data; the server assigns event order.** Each event gets a server-side receipt sequence number at journal-append time. `received_at` (server wall clock) is included for humans, never for resume.

4. **Realm filtering is evaluated at delivery time, per subscriber, with `realm.CanRead`.** Same function, same semantics as GET — a subscriber receives an event iff it could GET the ref at that moment. Live `SharedRealms` config is consulted, so SIGHUP membership changes apply to in-flight streams immediately. Unauthenticated subscribers get public events only.

5. **Tombstones are reserved, not designed here.** There is no delete in the hub. The event schema reserves `"op": "delete"`; when deletion happens it will need signed tombstone objects (everything in the dataverse is signed), which is its own design. Nothing in this spec blocks it.

## 4. Options

### Option A — Durable journal + SSE + `?since=` replay (recommended)

Add an append-only journal to every hub (root and proxy). Expose it two ways from one endpoint:

- `GET /events` with `Accept: text/event-stream` → SSE: replay from `?since=` (or `Last-Event-ID`), then live-tail. Native browser support, auto-reconnect with `Last-Event-ID` built into the protocol, flows through Caddy untouched, zero new Go dependencies (an SSE writer is ~40 lines around `http.Flusher`).
- `GET /events?since=<cursor>&limit=N` (JSON) → one page of events + `next_cursor` + `live` flag. Optional `&wait=30s` turns it into long-poll. This is the catch-up path, the compat fallback, and the test surface.

Proxy mode runs one upstream SSE subscription and re-journals downstream (§5.5).

*Pros:* real push latency (~0 added); one journal serves replay, SSE, and long-poll; resume is first-class at every hop; browser + Node + Go consumers all trivial; additive to the API.
*Cons:* new on-disk artifact (journal + retention); SSE needs the write-deadline fix and heartbeats; connection-count becomes a resource to cap.

### Option B — Long-poll changes feed only (CouchDB `_changes` style)

The same journal and the same `GET /events?since=&wait=` endpoint — but no SSE; consumers hold a long-poll open and immediately re-issue.

*Pros:* ~90% shared with Option A; dumbest possible client; no streaming-connection lifecycle at all.
*Cons:* per-event HTTP round-trip churn (reconnect after every batch); latency = RTT + re-request gap; N watchers × long-poll churn is strictly worse for the rate limiter and logs than N idle SSE connections; the proxy's upstream subscription becomes a poll loop again — the thing we set out to remove.

### Option C — WebSocket bidirectional sync channel

One WS connection per peer carrying subscriptions *and* pushes (subsume `PUT`-forwarding and `sync_pending` into a symmetric sync protocol).

*Pros:* most powerful end-state — one connection does subscribe + push + ack; could eventually replace the bespoke proxy write-path.
*Cons:* new dependency (`nhooyr.io/websocket` or similar); a second write path that must be kept consistent with `PUT` semantics (signature verify, revision conflicts, realm validation) or carefully share code; harder to reason about through proxies/CDNs; browsers can't set WS auth headers either; blast radius is the whole proxy, not an additive endpoint. All of the *value* (push events) is available from Option A at a fraction of the risk; none of the extra value (bidirectional push) is needed — the existing PUT-forward + `sync_pending` path already works and stays.

### Trade-off matrix

| Dimension | A: journal+SSE | B: long-poll only | C: WebSocket sync |
|---|---|---|---|
| Event schema | skinny, journal-backed | same | same, plus protocol frames |
| Push latency | ~0 | poll gap (bounded by `wait`) | ~0 |
| Cursor/resume | native (`Last-Event-ID` + `?since=`) | native (`?since=`) | must build into protocol |
| Replay/durability | journal | journal | journal still needed |
| Realm filtering | per-event at delivery | per-page at query | per-event at delivery |
| Proxy federation | 1 idle conn/hop | poll churn/hop | 1 conn/hop |
| Backpressure | drop conn → resume via replay | inherent (client-paced) | WS flow control + resume |
| Migration risk | additive endpoint | additive endpoint | new write path — high |
| New deps | none | none | websocket lib |
| Implementation size | M | S–M | L |

**Recommendation: Option A.** Build the journal + JSON replay first (that's Option B, ~a week of the work), layer SSE on top (small), then the proxy upstream subscriber. If SSE ever misbehaves in some environment, `?since=&wait=` long-poll is already there as the degraded mode.

---

## 5. Recommended design in detail (Option A)

### 5.1 Event schema

One JSON object per event; over SSE the same JSON is the `data:` field and the cursor is the SSE `id:` field.

```json
{
  "cursor": "8f3ka1:000000004212",
  "op": "put",
  "ref": "ApWJVWXvVKII….8e6b5675-…",
  "revision": 3,
  "type": "TASK",
  "pubkey": "ApWJVWXvVKII…",
  "realms": ["ApWJVWXvVKII…"],
  "received_at": "2026-07-07T21:40:11Z"
}
```

- `cursor` — opaque to clients; internally `<epoch>:<seq>` (§5.2). Sent as SSE `id:` so `Last-Event-ID` resume is automatic.
- `op` — `"put"` today. `"delete"` reserved for future tombstones. Unknown ops must be ignored by consumers (forward compat).
- `ref`, `revision`, `type`, `pubkey` — copied from the stored item; enough for a consumer to decide whether to fetch (compare against local revision) without a round trip.
- `realms` — the resolved `in` field. A subscriber only ever receives events whose realms it can read, so this leaks nothing beyond what `GET /{ref}` would; it lets `sync.js`-style clients run their existing `isVisible` logic and lets a multi-realm daemon demux without fetching.
- `received_at` — server receipt time, display/debug only. Resume uses `cursor`, never time.
- No object body. (`?include=body` considered and rejected for v1: doubles journal complexity, re-raises the BLOB-size problem `stripBlobData` exists to solve, and saves only one conditional GET that is usually a 304.)

Control events (SSE `event:` types, also expressible in the JSON page envelope):

- `event: reset` — "your cursor is unknown/expired; do a full revalidation, then resume from the `cursor` in this event." (§5.4)
- `event: ping` — comment/heartbeat every ~25 s (also defeats intermediary idle timeouts).
- `event: auth` — `{"status":"expired"}` then close, when the subscriber's token dies mid-stream (§5.6).

### 5.2 Journal, cursors, durability

New per-hub artifact: `<store_dir>/events/` —

```
events/
  journal-000001.jsonl     # append-only segments, one JSON event per line
  journal-000002.jsonl
  state.json               # {"epoch":"8f3ka1","seq":4212}  (fsynced)
```

- **Sequence**: `seq` is a uint64, incremented under the same critical section that makes the write visible (after `store.Write` + `index.Update` succeed, in both `handlePutObject`s, `storePrivateLocally`, `storeLocallyWithPending`, and `CacheLocally`). Journal append is fsync-per-event to start with — PUT rates are tiny (single-digit/sec at worst today); revisit batching only if it ever shows up in a profile.
- **Epoch**: a short random string minted whenever the hub cannot prove cursor continuity — first boot with events enabled, journal corruption, or manual deletion of `events/`. A cursor whose epoch ≠ current epoch is *unknown* → `reset`. This is what makes "resume after the other side restarted" well-defined: a clean restart keeps the epoch (state.json + segments are durable), a lossy restart changes it and every subscriber finds out explicitly instead of silently missing events.
- **Retention / replay window**: segments rotate at N events (e.g. 10k) and are pruned by age (`events_retention`, default **7 days**, config). `?since=` older than the oldest retained event → `reset`. The journal is *not* the store — losing it entirely is safe (new epoch, subscribers revalidate); it is never consulted for serving objects.
- **Startup**: read `state.json`, seek the tail segment, resume seq. A small in-memory ring (last ~4k events) fronts the segments so live subscribers and short replays never touch disk.
- **Restart-crash consistency**: the journal append happens after the store write; a crash between the two can lose the *event* but never the *object*. That is consistent with at-least-once-with-revalidation semantics — and the failure window is the same one the index already has (it, too, is only rebuilt from files). Consumers that must never miss anything do a revalidation sweep on `reset`/first-connect anyway.

What the cursor is **not**: it is not ordered across hubs, not comparable between hubs, and does not survive being presented to a different hub. `sync.js` persists one cursor per hub URL.

### 5.3 Endpoint & transport

```
GET /events                          → SSE (Accept: text/event-stream)
GET /events?since=<cursor>&limit=200 → JSON page {events, next_cursor, live}
GET /events?since=<cursor>&wait=30s  → long-poll variant of the same
```

Optional server-side filters, AND-ed with the auth gate: `?type=TASK`, `?by=<pubkey>`, `?realm=<realm>`. They cut noise for narrow daemons (an AgentFlow executor only cares about `type=EXECUTION_REQUEST`); the auth gate applies regardless.

`since` absent on SSE → start at head (live-only, no replay); `since=<cursor>` → replay-then-live. The JSON variant with no `since` returns the current head cursor and no events, which is how a fresh consumer bootstraps ("full sync via /search, then subscribe from head" — the head cursor is fetched *before* the sync so the gap is covered).

SSE handler specifics for *this* codebase:

- Clear the write deadline per connection: `http.NewResponseController(w).SetWriteDeadline(time.Time{})` — otherwise `main.go`'s `WriteTimeout: 30s` kills every stream.
- Route registered in both `Hub.Router()` and `Proxy.Router()`; handler is shared code operating on `(journal, index, authPubkey)`.
- Heartbeat comment every 25 s; `retry: 3000` hint on connect.
- Cap: `events_max_subscribers` (default 256) per hub; over cap → 503 with `Retry-After` (poller fallback still works).

### 5.4 Realm/auth filtering per subscriber

At delivery (SSE fan-out and JSON page assembly alike):

```go
if !realm.CanRead(ev.Realms, sub.authPubkey, h.shared) { skip }
```

- Same gate as `GET /{ref}` (`serving/handlers.go:126`), so the invariant *"a subscriber receives an event iff it could GET the object"* holds by construction. 404-style non-leakage carries over: filtered events are silently absent, never "redacted".
- The **journal stores everything unfiltered** (it lives inside `store_dir` next to the objects it describes — same trust domain); filtering is purely a delivery concern. This means one journal serves all subscribers, and replay for an authenticated user includes their private-realm events.
- Membership changes: `CanRead` consults the live `SharedRealms` (SIGHUP reload, `main.go:124`), so a revoked member stops receiving new events immediately. Events already delivered are gone — same as objects already GET-ed.
- A subtle case: an object whose *new revision changes its realms*. The event carries the new revision's realms; a subscriber who can read the new realms gets the event; one who could only read the old ones stops hearing about it. Both end states are what a fresh GET would produce.

### 5.5 Proxy federation: upstream subscription lifecycle

New component `upstream/subscriber.go`, lifecycle-managed exactly like `SyncPending` (Start/Stop from `main.go`, only in proxy mode, `events_upstream = true` by default when the feature ships):

```
loop:
  wait until upstream.Available()                     # reuse client.go availability
  cursor ← read store_dir/events/upstream-cursor.json # may be empty
  connect GET {upstream}/events  (SSE, since=cursor)  # unauthenticated → public events only
  on event(put ref rev):
      if index has ref and local rev >= event rev → skip        # dedup/echo suppression
      if index has ref  → ensureFresh(ref)                      # conditional GET; CacheLocally journals + re-emits
      else if mirror_mode → ensureFresh(ref)                    # config: events_prefetch = false (default) | true
      else → journal pass-through event                          # downstream hears about it; body fetched on demand
      persist upstream-cursor (batched: every event is fine at current rates, every N/1s under load)
  on reset | 409 cursor-unknown:
      revalidate: conditional-GET sweep over locally cached global refs   # bounded by cache size;
                                                                          # changed objects re-journal via CacheLocally
      reconnect from fresh head cursor
  on disconnect/error:
      exponential backoff 1s → 60s with jitter; health-checker signal short-circuits the wait
```

Why this composes:

- **Re-sequencing per hop.** The proxy never forwards upstream cursors; every event it emits — applied *or* passed through — gets a fresh local seq in the local journal. Downstream subscribers only ever hold *this* hub's cursors. An upstream epoch change costs the proxy one revalidation sweep; downstream cursors stay valid throughout and downstream simply receives whatever the sweep turns up as ordinary events. Resets do not cascade.
- **Echo suppression falls out of revisions.** A local client PUTs rev 5 → proxy forwards upstream + journals locally. Upstream later emits (ref, rev 5) back down → local index already has rev 5 → skip. No origin tags, no loop detection — the tree topology plus monotonic revisions cover it. (Duplicate *pass-through* events for uncached refs may occasionally slip through; at-least-once semantics make that a non-event.)
- **Local writes flow upstream-ward exactly as today.** The PUT-forward and `sync_pending` drain paths are untouched. The only addition: successful local stores (all four write sites) append to the local journal, so a proxy's downstream subscribers hear about local private writes and locally-cached upstream objects through one feed. When the drain pushes a backlog after an outage, upstream journals those PUTs as ordinary events for *its* subscribers — outage recovery propagates for free.
- **Privacy asymmetry is explicit.** Proxy→upstream is unauthenticated, so the subscription yields public (`dataverse001`) events only — precisely the set the proxy is allowed to cache today (`realm.IsGlobalObject`). Private objects flow up (with `upstream_push = "all"`) but their *events* never flow down. Fixing that needs hub-to-hub identity auth — out of scope, flagged as future work (§7).
- **`events_prefetch`** (default `false`): off = cache-on-demand is preserved, events for never-seen refs are passed through skinny and the body is fetched only when someone GETs it; on = the proxy eagerly `ensureFresh`es every event ref, converging toward a mirror. Both are correct; the default keeps today's storage behavior.

### 5.6 Backpressure & slow consumers

- Per-subscriber buffered channel (256 events). Fan-out never blocks the write path: if a subscriber's buffer is full, the hub **closes that connection** (SSE `retry:` already set). The client reconnects with `Last-Event-ID` and replays from the durable journal — slowness converts into replay, not into memory growth or lost events. This is the standard SSE + journal resolution and it's only safe *because* the journal exists (another reason Option A wants durability).
- Token death mid-stream (expiry, or hub restart wiping the in-memory token map — `auth/auth.go`): the stream keeps the pubkey captured at connect; a periodic (60 s) revalidation of the token sends `event: auth {"status":"expired"}` and closes. Client re-auths (challenge-response) and resumes with its cursor — cursor validity is independent of auth.
- Connection caps as in §5.3; the JSON `?since=` endpoint is always available as pressure relief.

### 5.7 Client consumption (`instructiongraph-js`)

- `hub.js` grows `subscribe({cursor, filters, onEvent, onReset})` implemented with **fetch streaming** (Node ≥18 and browsers both parse `text/event-stream` off a `ReadableStream` in ~30 lines; native `EventSource` is not used because it cannot send `Authorization: Bearer` — browsers that rely on the `dv_session` cookie may use it, but one code path is simpler).
- `sync.js` gains an optional live mode: on event → if `revision` > local revision → `get(ref)` through the existing sync path (which caches, realm-filters, and 304s). On `reset` → targeted revalidation (conditional GET over locally-known refs, the client-side mirror of §5.5) → resubscribe from the reset cursor.
- Cursor persisted per hub URL at `configDir/config/events-cursor.json` (same pattern as `shared-realms.json`, `sync.js:34-45`).
- Daemons (AgentFlow executor, recorders, watchers) replace their `/search` timers with `subscribe({filters: {type: …}})` + their existing fetch logic. Their polling code should remain as the fallback when `/events` 404s (old hub).

### 5.8 Migration & compatibility

- Purely **additive**: no existing endpoint, header, or store file changes. Polling keeps working forever; the feed is an accelerator, not a replacement.
- Feature detection = `GET /events` → 404 on old hubs → client stays in polling mode. No version negotiation.
- A new hub behind an old proxy: proxy doesn't subscribe (old binary), everything behaves exactly as today. An old hub above a new proxy: subscriber's connect gets 404, logs once, retries hourly; local journal still runs, downstream subscribers still get local + on-demand-cached events.
- Config additions (all defaulted so an unchanged `hub.toml` is valid):

```toml
events_enabled        = true      # kill switch
events_retention      = "168h"    # journal replay window
events_max_subscribers = 256
events_upstream       = true      # proxy mode: subscribe to upstream
events_prefetch       = false     # proxy mode: eager-fetch event refs
```

### 5.9 Implementation map (for the follow-up EXECUTABLE)

| Piece | Where | Size |
|---|---|---|
| Journal (segments, epoch, ring, retention) | new `events/journal.go` | M |
| Emit hooks after the 4 write sites + `CacheLocally` | `serving/handlers.go`, `serving/proxy.go` | S |
| `/events` handler (SSE + JSON + filters + CanRead gate) | new `serving/events.go`, routes in `hub.go`/`proxy.go` | M |
| Upstream subscriber (SSE client, cursor file, backoff, revalidation sweep) | new `upstream/subscriber.go`, wiring in `main.go` | M |
| Config + docs | `config.go`, `README.md` | S |
| Tests: journal resume, epoch reset, realm filtering (incl. 404-non-leak), proxy chain end-to-end (root→proxy→client), slow-consumer drop+resume, restart matrix (proxy restart / upstream restart / journal loss on each side) | `events_test.go`, extend `proxy_test.go` | L |

The restart matrix is the acceptance heart: **(a)** proxy restarts → resumes from `upstream-cursor.json`, no downstream reset; **(b)** upstream restarts cleanly → epoch persists, proxy resumes mid-stream; **(c)** upstream loses `events/` → epoch change → proxy revalidation sweep → downstream cursors *still valid*; **(d)** both restart → (a)+(b) compose.

## 6. Rejected alternatives (details)

- **Timestamp-based `?since=`** over the existing index: no journal needed, but author-controlled timestamps make it silently lossy (§2, row 3). Rejected on correctness.
- **Fat events** (inline body): kills the 10 MB BLOB problem twice (journal + fan-out), forces delivery-time re-canonicalization, and saves only a usually-304 conditional GET. Rejected.
- **Per-subscriber server-side resume state** (server remembers each consumer's position): stateful, unbounded, and dies on restart — the cursor-in-client model needs none of it.
- **WebSocket (Option C)** and **long-poll-only (Option B)**: see matrix; B survives as the built-in degraded mode of A.

## 7. Future work (explicitly out of scope)

- **Deletion/tombstones**: signed tombstone objects + `op: "delete"` + journal/GC semantics.
- **Hub-to-hub identity auth**: give proxies an identity keypair and a challenge-response client, so upstream subscriptions can carry shared-realm/private events downstream (today: public only). The event schema and per-hop cursors already accommodate it.
- **Filtered replay optimizations** (per-realm journal indices) if hubs grow beyond ~10⁶ events/window.
- **`?include=body`** if a consumer class emerges where the extra GET measurably hurts.

## 8. Open questions for the operator

1. Default `events_prefetch` — keep proxies cache-on-demand (`false`, proposed) or converge proxies toward mirrors (`true`)?
2. Is 7 days the right replay window for the deployment sizes you expect (it bounds `events/` disk usage to roughly `events/window × ~300 bytes`)?
3. Should the executor/recorder fallback-to-polling live in `instructiongraph-js` (proposed) or be each daemon's problem?
