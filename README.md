# Dataverse Hub

HTTP server for [dataverse001](https://dataverse001.net) — a decentralized, self-describing graph data format.

## What is dataverse001?

dataverse001 is a signed, self-describing graph. Every object is a JSON fragment carrying its own schema, cryptographic signature, and typed relations to other objects. Objects can live anywhere — files, APIs, QR codes, embedded in images — and any agent that encounters one can verify and understand it without external documentation.

The hub is one way to store and serve these objects. It's not the only way — the format is transport-agnostic — but it's the easiest way to get started.

See the [dataverse001 root node](https://dataverse001.net/AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.b3f5a7c9-2d4e-4f60-9b8a-0c1d2e3f4a5b?ref=AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.00000000-0000-0000-0000-000000000000) for the full data format specification.

## Run locally

The quickest way to explore the dataverse is to run a local hub in proxy mode. It caches objects from the public hub and lets you browse them at `http://localhost:5678`.

**Prerequisites:** Go 1.22+

```bash
git clone https://github.com/tijszwinkels/dataverse-hub.git
cd dataverse-hub
go build -o hub .
./hub
```

That's it. The hub starts on `http://localhost:5678`, proxying to `https://dataverse001.net`. Try opening [the root node](http://localhost:5678/AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.00000000-0000-0000-0000-000000000000) in your browser.

Objects you access are cached locally in `./dataverse001/`. If the upstream goes down, your local hub keeps serving everything it has seen.

### Docker

```bash
docker build -t hub .
docker run -p 5678:5678 -v ./dataverse001:/dataverse001 hub
```

## Modes

**Root mode** — authoritative hub, serves directly from local store. Use this when running your own independent hub.

**Proxy mode** (default) — caches locally, forwards to an upstream hub. Falls back to local cache when upstream is unreachable. Pending writes are queued and synced when connectivity returns.

## Configuration

Configure via TOML file, environment variables, or both. Precedence: **defaults < config file < env vars**.

```bash
cp hub.example.toml hub.toml
./hub -config hub.toml
```

See [`hub.example.toml`](hub.example.toml) for all options with comments.

### Config file (TOML)

| Key | Default | Description |
|---|---|---|
| `mode` | `"proxy"` | `"root"` or `"proxy"` |
| `upstream_url` | `"https://dataverse001.net"` | Upstream hub (proxy mode) |
| `upstream_push` | `"public"` | What to forward upstream: `"public"` (only dataverse001) or `"all"` (all realms) |
| `addr` | `":5678"` | Listen address |
| `store_dir` | `"./dataverse001"` | Object store directory |
| `rate_limit_per_min` | `120` | Requests per minute per IP |
| `rate_limit_per_day` | `20000` | Requests per day per IP |
| `default_viewer_ref` | *(built-in)* | PAGE ref used as default HTML viewer |
| `backup_enabled` | `true` | Keep old revisions in `bk/` |
| `auth_token_expiry` | `"168h"` | Bearer token / session cookie lifetime |
| `base_domain` | `"localhost"` | Base domain for virtual hosting; required for `vhost_mode = "redirect"` or `"isolate"` |
| `vhost_mode` | `"isolate"` | Host routing mode: `"off"`, `"redirect"`, or `"isolate"` |
| `txt_cache_ttl` | `"5m"` | DNS TXT record cache TTL for custom domain resolution |
| `[realms."name"]` | *(none)* | Shared realm config — see [Shared realms](#shared-realms) below |

### Environment variables

Env vars override any value from the config file:

| Variable | Config key |
|---|---|
| `DATAVERSE_MODE` | `mode` |
| `DATAVERSE_UPSTREAM_URL` | `upstream_url` |
| `DATAVERSE_UPSTREAM_PUSH` | `upstream_push` |
| `HUB_ADDR` | `addr` |
| `HUB_STORE_DIR` | `store_dir` |
| `HUB_RATE_LIMIT_PER_MIN` | `rate_limit_per_min` |
| `HUB_RATE_LIMIT_PER_DAY` | `rate_limit_per_day` |
| `HUB_DEFAULT_VIEWER_REF` | `default_viewer_ref` |
| `HUB_BACKUP_ENABLED` | `backup_enabled` |
| `HUB_AUTH_TOKEN_EXPIRY` | `auth_token_expiry` |
| `HUB_BASE_DOMAIN` | `base_domain` |
| `HUB_VHOST_MODE` | `vhost_mode` |
| `HUB_TXT_CACHE_TTL` | `txt_cache_ttl` |

## API

Objects are identified by composite key: `{pubkey}.{id}`.

### Read

```
GET /{ref}              # single object (JSON or HTML based on Accept header)
GET /{ref}/json         # signed JSON envelope, always (ignores Accept)
GET /{ref}/raw          # the object's native content (ignores Accept)
GET /{ref}/page         # HTML view: inline PAGE, page-relation viewer, or default viewer
GET /{ref}/inbound      # objects pointing at this ref
GET /json               # the /json representation of whatever GET / resolves to
GET /raw                # the /raw representation of whatever GET / resolves to
GET /page               # the /page representation of whatever GET / resolves to
GET /search             # list/filter objects
```

`GET /{ref}` negotiates the representation from the `Accept` header (see
[Content negotiation](#content-negotiation)). The `/json`, `/raw`, and `/page`
suffixes pin one representation regardless of `Accept`:

- **`/raw`** — the object's content in its *native* media type: a BLOB's bytes
  with its `content.mime_type`, or a PAGE's OWN `content.html` as `text/html`.
  It never renders a page-relation or default viewer (that stays `/page`'s job),
  so a PAGE with a `page` relation still serves its own html here. `409 NO_RAW`
  when the object has no native representation (e.g. a non-BLOB non-PAGE, or a
  PAGE with empty html). Because a PAGE's `/raw` is author-controlled HTML, it
  gets the same per-app origin-isolation redirect as `/page` (keyed on the object
  being a PAGE). HTML-mime BLOBs are intentionally still served on the shared
  origin (issue #14).
- **`/page`** — `409 NO_PAGE` when there is no HTML view.

Each suffix shares `GET /{ref}`'s realm auth (an unauthorized private object
returns `404`, not `403`, to avoid leaking existence) and, where the bytes
coincide, its `ETag`/`If-None-Match` semantics so a client can revalidate either
URL interchangeably.

**Root aliases.** `GET /json`, `GET /raw`, and `GET /page` (no ref) each serve
*the representation of whatever `GET /` resolves to on this host* — the
genesis/root object on the base domain, or the resolved PAGE on a vhost page
host. Unlike `GET /` (which `302`s to the root object), the aliases serve that
target **directly** with `200`, so a redirect-less client can bootstrap with a
single request (e.g. `curl https://dataverse001.net/json`). They then run the
exact same pipeline as `/{ref}/<repr>`, so `/raw` on the base domain is `409
NO_RAW` (the root object is neither BLOB nor PAGE) and `/page` composes the
default viewer. In `redirect` vhost mode a page host `302`s to the base domain
with the representation suffix preserved (`…/{ref}/json`); an unknown host is
`404`.

Query parameters for list endpoints:
- `by={pubkey}` — filter by author
- `type={TYPE}` — filter by object type
- `relation={name}` — filter inbound by relation type
- `include=inbound_counts` — include per-item `_inbound_counts`
- `limit=N` — page size (default 50)
- `cursor=...` — pagination cursor

### Write

```
PUT /{ref}              # upsert a signed object (signature verified server-side)
```

Every object carries a monotonic `revision`. A successful `PUT` returns the new
revision as a strong `ETag` (e.g. `ETag: "3"`) — the same tag `GET /{ref}`
serves for the raw object.

**Default (no `If-Match`)** — last-writer-wins by revision: a `PUT` whose object
`revision` is greater than the stored one is applied; a `PUT` whose `revision` is
**less than or equal to** the stored one is rejected `409 Conflict`
(`REVISION_CONFLICT`). A brand-new ref is created (`201`).

**Conditional (`If-Match`)** — protocol-level optimistic locking (RFC 9110):

```
PUT /{ref}
If-Match: "3"           # apply only if the stored object is currently revision 3
```

- `If-Match: "<rev>"` — proceeds only if the stored revision equals `<rev>`;
  otherwise `412 Precondition Failed` (`PRECONDITION_FAILED`) and the stored
  object is left untouched. Comma-separated lists match if any tag matches;
  weak tags (`W/"…"`) never match.
- `If-Match: *` — proceeds only if the object already exists; a not-yet-existing
  ref → `412`. A concrete `If-Match` tag on a missing ref is likewise `412`.

Read-check-write for a given ref is serialized server-side, so concurrent
conditional writes from the same base revision cannot both commit — exactly one
wins, the rest get `412`. In proxy mode the upstream hub is the authority for
global objects (the proxy forwards `If-Match` and relays the `412`); private
objects are enforced against the proxy's local store.

### Authentication

ECDSA challenge-response auth. Proves ownership of a P-256 keypair without revealing the private key.

```
GET  /auth/challenge    # get a single-use challenge (expires in 5 min)
POST /auth/token        # exchange signed challenge for bearer token + session cookie
POST /auth/logout       # invalidate current session
```

**Flow:**

```bash
# 1. Get challenge
CHALLENGE=$(curl -s https://hub.example.com/auth/challenge | jq -r .challenge)

# 2. Sign it
SIGNATURE=$(echo -n "$CHALLENGE" | openssl dgst -sha256 -sign private.pem | base64)

# 3. Exchange for token
curl -s -X POST https://hub.example.com/auth/token \
  -H 'Content-Type: application/json' \
  -d "{\"pubkey\":\"$PUBKEY\",\"challenge\":\"$CHALLENGE\",\"signature\":\"$SIGNATURE\"}"
```

The token response also sets a `dv_session` cookie (`HttpOnly; Secure; SameSite=Lax`). Use the bearer token for CLI/agents, the cookie for browsers.

### Private objects

Objects with the owner's pubkey as a realm in `item.in` are private — only accessible to authenticated users whose pubkey matches. Unauthenticated requests to private objects receive `404` (not `403`) to avoid leaking existence.

```json
{ "item": { "in": ["AxyU5_..."], "type": "DRAFT", ... } }
```

### Shared realms

Shared realms let a group of pubkeys share private access to objects. They sit between public (`dataverse001`) and identity-realms:

- **Public (`dataverse001`)** — anyone can read, propagated globally
- **Server-public** — anyone can read, stays on this hub
- **Shared realm** — only authenticated members can read
- **Identity-realm** — only the owner can read

Configure shared realms in `hub.toml`:

```toml
[realms."AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.MyTeam"]
members = [
  "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ",
  "BzxY7_other_pubkey_here",
]
```

Realm names follow the convention `{owner-pubkey}.{Name}` to show who created the realm, but any string works.

**Behavior:**

- Objects with a shared realm in `item.in` are only readable by authenticated members.
- Unauthenticated requests return `404` (same as identity-realms).
- Any signed object can be PUT into a shared realm — the `members_only` search filter (default: `true`) controls whether non-member contributions appear in list results.
- Objects can belong to both a shared realm and `dataverse001` — the public realm takes precedence for read access.

**Hot reload:** Edit `hub.toml` and send `SIGHUP` to the hub process — realm config reloads without restart. On parse error, the previous config is kept.

**API:**

```
GET /auth/realms    # list shared realms the authenticated user belongs to (401 if unauthenticated)
```

Query parameter for search/inbound endpoints:
- `members_only=false` — include objects signed by non-members (default: `true`)

### Server-public realm

The well-known realm `"server-public"` is readable by anyone without authentication (like `dataverse001`) but stays on the hub — it is not propagated upstream by default.

```json
{ "item": { "in": ["server-public"], "type": "POST", ... } }
```

Use this for data that should be publicly accessible on your hub but not spread globally (e.g. corporate/business applications).

### Realm access and propagation summary

| Realm | Auth to read | `upstream_push=public` | `upstream_push=all` |
|-------|-------------|----------------------|-------------------|
| `dataverse001` | No | Pushed upstream | Pushed upstream |
| `server-public` | No | **Stays local** | Pushed upstream |
| Shared realm | Members only | Stays local | Pushed upstream |
| Identity realm (pubkey) | Owner only | Stays local | Pushed upstream |
| `local` (ig CLI only) | N/A | Never pushed | Never pushed |

### Content negotiation

- `Accept: application/json` — always returns JSON
- `Accept: text/html` — PAGE objects served as HTML; other objects rendered via default viewer
- BLOB objects (`type: BLOB`) — served as raw content when Accept matches `content.mime_type`. Supports both binary (base64-encoded `content.data`) and text (`content.text`) BLOBs.

### Error responses

Non-2xx responses on the data API (`GET`/`PUT /{ref}`, `/search`, `/{ref}/inbound`, `/auth/*`, and unmatched routes) are content-negotiated (see below): JSON and wildcard clients receive `application/problem+json` ([RFC 9457](https://www.rfc-editor.org/rfc/rfc9457)), written for a programmatic consumer:

```json
{
  "title": "Revision conflict",
  "status": 409,
  "detail": "existing revision 5 >= incoming 3",
  "next_action": "Fetch the current object (GET /<ref>), set the item's `revision` field above the stored revision, re-sign, and PUT again.",
  "code": "REVISION_CONFLICT"
}
```

- `title` — short, stable summary of the problem class.
- `detail` — the specific cause of this occurrence.
- `next_action` — one concrete recovery step (the error message is the product).
- `code` — machine-stable identifier, preserved from the legacy `{error, code}` body for backward compatibility.

Content is negotiated on `Accept`: JSON and wildcard clients (curl's `*/*`, agents, browsers) receive problem+json; a client that accepts only `text/html` keeps the legacy `{error, code}` body. Status codes are unchanged. Both Hub and Proxy serving modes behave identically.

### On-demand TLS

```
GET /ask?domain={hostname}    # returns 200/403 for Caddy on-demand TLS decisions
```

Approves certificates for hash subdomains (`{hash}.{base_domain}`) and custom domains with a valid `_dv.{domain}` TXT record pointing to a PAGE ref. This works in both `redirect` and `isolate` mode.

## Virtual hosting

Virtual hosting uses `base_domain` plus `vhost_mode`:

- `vhost_mode = "off"` — disable host-based routing and on-demand TLS approval
- `vhost_mode = "redirect"` — resolve `_dv.{host}` and known PAGE hosts, but redirect browser HTML requests to `https://{base_domain}/{ref}` on the shared origin
- `vhost_mode = "isolate"` — resolve `_dv.{host}` and canonicalize PAGE HTML onto per-page origins

If `base_domain = ""`, virtual hosting is effectively disabled regardless of `vhost_mode`.

The hub resolves PAGE objects from the `Host` header:

- **Hash subdomains** — `{sha256prefix}.{base_domain}` maps to a PAGE ref deterministically
- **Named subdomains** — `social.{base_domain}` resolved via `_dv.social.{base_domain}` TXT record
- **Custom domains** — `example.com` resolved via `_dv.example.com` TXT record

TXT record format: bare ref (`{pubkey}.{id}`) or `dv1-page={pubkey}.{id}`.

Use `cmd/pagehash` to compute the hash subdomain for a PAGE:

```bash
go run ./cmd/pagehash AxyU5_...ea96b9f6-...
```

## Security model

The hub can serve user-submitted HTML (PAGE objects) that runs user-submitted JavaScript. This is powerful — anyone can publish a webapp to the dataverse — but it requires careful isolation.

### Origin isolation via virtual hosting

Virtual hosting gives each PAGE its own origin (subdomain or custom domain). The browser's same-origin policy then prevents pages from accessing each other's cookies, localStorage, or making authenticated requests on each other's behalf.

- **Hash subdomains** (`{hash}.dataverse001.net`) — every PAGE gets a unique, deterministic subdomain automatically.
- **Custom domains** — PAGE authors can point their own domain at the hub for friendlier URLs, with the same isolation.

**With `vhost_mode = "redirect"`**, pretty domains still work as entrypoints, but browsers are redirected back to the shared base-domain path. That keeps friendly URLs functional without per-page origin isolation.

**Without virtual hosting** (`base_domain = ""` or `vhost_mode = "off"`), all PAGEs share one origin. This is fine for trusted content but unsuitable for hosting untrusted third-party pages.

### Identity per site

Each PAGE origin has its own isolated authentication session. When you create an account (keypair) on a PAGE, that identity only exists on that origin — other pages cannot access it.

**For untrusted pages, create a separate identity.** A malicious page has full control over the JavaScript running in its origin, which means it can act as you within that origin. By using a throwaway identity on untrusted pages, you limit the blast radius: the worst a malicious page can do is act as a throwaway account that owns nothing of value.

Use your main identity only on pages you trust.

## Tests

```bash
go test ./...
```

Race detector:

```bash
go test -race ./...
```
