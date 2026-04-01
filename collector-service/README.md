# aikey-data / collector-service

Receives usage events from `aikey-proxy`, persists raw events to ODS, and asynchronously projects enriched facts to DWD.

## Architecture

```
Local Proxy ──POST /v1/usage-events:batch──▶ Ingest API ──▶ USAGE_EVENT_ODS
                                                                   │
                                                         DWD Projector (async)
                                                                   │
                                                                   ▼
                                              MANAGED_KEY_CONTROL_EVENTS (read-only)
                                                                   │
                                                                   ▼
                                                          USAGE_FACT_DWD
```

## Services

| Service | Port | Responsibility |
|---------|------|----------------|
| `collector-service` | 27300 | Ingest API, ODS persistence, DWD projection |
| `query-service` | 27301 | Query aggregation for dashboards (planned) |

## Quick Start

```bash
# Prerequisites: Go 1.26+, PostgreSQL (shared with aikey-control)
cp .env.example .env
# Edit .env with your DATABASE_DSN

make build
./bin/collector-service
```

## API

### `POST /v1/usage-events:batch`

Batch ingest usage events. Requires `Authorization: Bearer <SERVICE_TOKEN>`.

**Request:**
```json
{
  "source": "aikey-proxy",
  "source_version": "0.1.0",
  "proxy_instance_id": "proxy-01",
  "events": [{ "event_id": "...", "org_id": "...", ... }]
}
```

**Response:**
```json
{ "accepted": 97, "duplicated": 3, "rejected": 0 }
```

### `GET /health`

Health check (unauthenticated).

## Database

Shares PostgreSQL instance with `aikey-control`. Tables:

- `usage_event_ods` — raw events (ODS layer)
- `usage_fact_dwd` — enriched facts (DWD layer)
- `usage_dwd_projector_tasks` — projector checkpoint

Migrations are in `migrations/` and run automatically on startup.

## Environment Variables

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `DATABASE_DSN` | Yes | — | PostgreSQL connection string |
| `LISTEN_ADDR` | No | `0.0.0.0:27300` | HTTP listen address |
| `MIGRATIONS_DIR` | No | `./migrations` | SQL migrations directory |
| `SERVICE_TOKEN` | No | — | Bearer token for API auth |
| `LOG_LEVEL` | No | `info` | Log level (debug/info/warn/error) |

## Project Structure

```
collector-service/
├── cmd/main.go              # Entry point
├── config/config.go         # Env-based configuration
├── migrations/              # SQL migration files
├── internal/
│   ├── api/                 # HTTP handlers & router
│   ├── ingest/              # Domain types, service, ODS repository
│   ├── projector/           # DWD projection worker (planned)
│   └── shared/              # DB, response helpers, middleware
├── Makefile
└── .env.example
```

## Runtime

- Go 1.26+
- PostgreSQL 14+
- Platforms: macOS, Linux, Windows
