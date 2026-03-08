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
| `addr` | `":5678"` | Listen address |
| `store_dir` | `"./dataverse001"` | Object store directory |
| `rate_limit_per_min` | `120` | Requests per minute per IP |
| `rate_limit_per_day` | `20000` | Requests per day per IP |
| `default_viewer_ref` | *(built-in)* | PAGE ref used as default HTML viewer |
| `backup_enabled` | `true` | Keep old revisions in `bk/` |
| `auth_token_expiry` | `"168h"` | Bearer token / session cookie lifetime |
| `base_domain` | *(empty)* | Base domain for virtual hosting (e.g. `"dataverse001.net"`) |
| `txt_cache_ttl` | `"5m"` | DNS TXT record cache TTL for custom domain resolution |

### Environment variables

Env vars override any value from the config file:

| Variable | Config key |
|---|---|
| `DATAVERSE_MODE` | `mode` |
| `DATAVERSE_UPSTREAM_URL` | `upstream_url` |
| `HUB_ADDR` | `addr` |
| `HUB_STORE_DIR` | `store_dir` |
| `HUB_RATE_LIMIT_PER_MIN` | `rate_limit_per_min` |
| `HUB_RATE_LIMIT_PER_DAY` | `rate_limit_per_day` |
| `HUB_DEFAULT_VIEWER_REF` | `default_viewer_ref` |
| `HUB_BACKUP_ENABLED` | `backup_enabled` |
| `HUB_AUTH_TOKEN_EXPIRY` | `auth_token_expiry` |
| `HUB_BASE_DOMAIN` | `base_domain` |
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

### Content negotiation

- `Accept: application/json` — always returns JSON
- `Accept: text/html` — PAGE objects served as HTML; other objects rendered via default viewer
- BLOB objects (`type: BLOB`) — served as raw content when Accept matches `content.mime_type`. Supports both binary (base64-encoded `content.data`) and text (`content.text`) BLOBs.

### On-demand TLS

```
GET /ask?domain={hostname}    # returns 200/403 for Caddy on-demand TLS decisions
```

Approves certificates for hash subdomains (`{hash}.{base_domain}`) and custom domains with a valid `_dv.{domain}` TXT record pointing to a PAGE ref.

## Virtual hosting

When `base_domain` is set, the hub resolves PAGE objects from the `Host` header for origin isolation:

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

**Without virtual hosting** (single-origin deployment), all PAGEs share one origin. This is fine for trusted content but unsuitable for hosting untrusted third-party pages. Enable `base_domain` in production if you serve user-submitted PAGEs.

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
