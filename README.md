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
| `[realms."name"]` | *(none)* | Shared realm TOML override — see [Shared realms](#shared-realms) below |

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
GET /{ref}/inbound      # objects pointing at this ref
GET /search             # list/filter objects
```

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

Membership is declared **in the graph** by signed `SHARED_REALM` objects. Any hub that encounters the realm definition can resolve access without operator config. A hub operator can additionally grant members via `hub.toml` as an **additive local override**.

#### Defining a shared realm (graph-native)

A `SHARED_REALM` object names a realm and lists its members. The realm's canonical object lives at a **deterministic, computable address** — its `id` is the UUIDv5 hash of the realm name (namespace `00000000-0000-0000-0000-000000000000`) — so any hub can find the access list without a lookup table.

```json
{
  "is": "instructionGraph001",
  "signature": "<base64 ECDSA over item>",
  "item": {
    "in": ["dataverse001", "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ"],
    "ref": "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.6bb7d6cc-1556-5a76-a910-edc802d4a2b7",
    "id": "6bb7d6cc-1556-5a76-a910-edc802d4a2b7",
    "pubkey": "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ",
    "created_at": "2026-06-30T12:00:00Z",
    "revision": 1,
    "type": "SHARED_REALM",
    "name": "Acme Team",
    "instruction": "A shared realm. Listed members have read access to objects carrying this realm name in item.in.",
    "content": { "realm": "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.AcmeTeam" },
    "relations": {
      "member": [
        { "ref": "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.346bef5e-94ff-4f7a-bcf6-d78ae1e1541c" },
        { "ref": "BzxY7_other_pubkey_here.00000000-0000-0000-0000-000000000001" }
      ]
    }
  }
}
```

**Type contract (enforced by the hub on PUT):**

- `item.type` must be `"SHARED_REALM"`.
- `content.realm` names the realm. It **must be owner-prefixed**: the portion before the first `.` must be the signer's compressed pubkey (e.g. `{owner-pubkey}.{Name}`). The owner of a pubkey namespace owns the realms under it.
- The signer (`item.pubkey`) must equal the realm's owner prefix.
- `item.id` must equal `uuid_v5("00000000-0000-0000-0000-000000000000", content.realm)` — the deterministic hash. Compute it in any language with a standard UUIDv5: Python `uuid.uuid5(uuid.UUID("00000000-0000-0000-0000-000000000000"), realm)`, JS `uuid.v5(realm, NIL_UUID)`, etc.
- `item.in` **must include `"dataverse001"`** so the realm definition propagates globally and is discoverable by every hub.
- `relations.member` lists member refs; the **pubkey portion** (before the first `.`) of each ref is granted access. Invalid refs are skipped; duplicates are deduped. The owner is not implicitly a member — list them explicitly if they should have access.

**Updating / revoking:** edit `relations.member` and PUT a higher `revision` to the same canonical ref. Removing a pubkey revokes their access. Higher revision wins on sync.

**Resolving access:** given a realm `R`, a hub computes `owner = R.split(".")[0]`, `id = uuid_v5(NS, R)`, fetches `{owner}.{id}`, verifies the signature, and reads the members. No scanning or election — the address is unique by construction (a second object from the same owner collides on the composite key and is treated as an update; a different signer is rejected by the prefix rule).

#### TOML override (additive, local)

A hub operator can grant additional members via `hub.toml`. This is **additive**: TOML can add members the graph doesn't list, but it **cannot revoke** a graph-granted member (revocation is done in the graph via a higher revision). Use it for emergency/local grants.

```toml
[realms."AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.MyTeam"]
members = [
  "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ",
  "BzxY7_other_pubkey_here",
]
```

**Hot reload:** Edit `hub.toml` and send `SIGHUP` to the hub process — the TOML override reloads without restart (graph membership is always live, ingested from `SHARED_REALM` objects as they're PUT). On parse error, the previous config is kept.

**Behavior:**

- Objects with a shared realm in `item.in` are only readable by authenticated members (from the graph or TOML).
- Unauthenticated requests return `404` (same as identity-realms).
- Any signed object can be PUT into a shared realm — the `members_only` search filter (default: `true`) controls whether non-member contributions appear in list results.
- Objects can belong to both a shared realm and `dataverse001` — the public realm takes precedence for read access.

**API:**

```
GET /auth/realms    # list shared realms the authenticated user belongs to (graph + TOML) (401 if unauthenticated)
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
