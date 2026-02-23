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

All via environment variables:

| Variable | Default | Description |
|---|---|---|
| `DATAVERSE_MODE` | `proxy` | `root` or `proxy` |
| `DATAVERSE_UPSTREAM_URL` | `https://dataverse001.net` | Upstream hub (proxy mode) |
| `HUB_ADDR` | `:5678` | Listen address |
| `HUB_STORE_DIR` | `./dataverse001` | Object store directory |
| `HUB_RATE_LIMIT_PER_MIN` | `120` | Requests per minute per IP |
| `HUB_RATE_LIMIT_PER_DAY` | `20000` | Requests per day per IP |
| `HUB_DEFAULT_VIEWER_REF` | *(built-in)* | PAGE ref used as default HTML viewer |
| `HUB_BACKUP_ENABLED` | `true` | Keep old revisions in `bk/` |
| `HUB_AUTH_WIDGET_HOST` | *(empty)* | Hostname for auth widget (e.g. `auth.dataverse001.net`) |
| `HUB_AUTH_WIDGET_ALLOWED_ORIGINS` | *(empty)* | Comma-separated origins that may embed the widget |

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
