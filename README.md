# Dataverse Hub

HTTP server for [dataverse001](https://github.com/tijszwinkels/dataverse001) — a decentralized, self-describing graph data format.

The hub stores, indexes, and serves signed instructionGraph001 objects. It verifies ECDSA signatures on ingest, maintains an in-memory index for fast lookups, and serves objects as JSON (API) or HTML (browsers).

## Modes

**Root mode** — authoritative hub, serves directly from local store.

**Proxy mode** (default) — caches locally, forwards to an upstream hub. Falls back to local cache when upstream is unreachable. Pending writes are queued and synced when connectivity returns.

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
| `auth_widget_host` | *(empty)* | Hostname for auth widget |
| `auth_widget_allowed_origins` | `[]` | Origins that may embed the widget |

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
| `HUB_AUTH_WIDGET_HOST` | `auth_widget_host` |
| `HUB_AUTH_WIDGET_ALLOWED_ORIGINS` | `auth_widget_allowed_origins` (comma-separated) |

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

### Content negotiation

- `Accept: application/json` — always returns JSON
- `Accept: text/html` — PAGE objects served as HTML; other objects rendered via default viewer
- BLOB objects (`type: BLOB`) — served as raw content when Accept matches `content.mime_type`

## Tests

```bash
go test ./...
```
