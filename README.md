# Dataverse Hub

HTTP server for [dataverse001](https://dataverse001.net) — a decentralized, self-describing graph data format.

The hub stores, indexes, and serves signed [instructionGraph001](https://dataverse001.net) objects. It verifies ECDSA-P256 signatures on ingest, maintains an in-memory index for fast lookups, and serves objects as JSON (API) or HTML (browsers).

## Features

- **Signature verification** — every ingested object is cryptographically verified (ECDSA-P256)
- **Content negotiation** — JSON for APIs, rendered HTML for browsers, raw content for BLOBs
- **Authentication** — ECDSA challenge-response auth with bearer tokens and session cookies
- **Private objects** — pubkey-realm access control (owner's pubkey as realm name)
- **Virtual hosting** — per-PAGE origin isolation via wildcard subdomains and custom domains
- **Proxy mode** — cache-and-forward to an upstream hub with offline resilience
- **Rate limiting** — per-IP request throttling (configurable per-minute and per-day limits)
- **On-demand TLS** — smart `/ask` endpoint for Caddy's on-demand TLS with abuse prevention
- **Minimal dependencies** — only two: [chi](https://github.com/go-chi/chi) (router) and [toml](https://github.com/BurntSushi/toml) (config)

## Quick start

```bash
go build -o hub .
./hub
```

Defaults to proxy mode on `:5678`, upstream `https://dataverse001.net`, store in `./dataverse001/`.

### Docker

```bash
docker build -t hub .
docker run -p 5678:5678 -v ./dataverse001:/dataverse001 hub
```

## Modes

**Root mode** — authoritative hub, serves directly from local store.

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

## Tests

```bash
go test ./...
```

Race detector:

```bash
go test -race ./...
```
