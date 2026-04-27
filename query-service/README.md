# aikey-data / query-service

Provides usage query APIs for personal and master dashboards, reading from the DWD layer populated by `collector-service`.

## API Endpoints

All endpoints require `Authorization: Bearer <SERVICE_TOKEN>` (except `/health`).

### Personal Page

| Endpoint | Description | Required Params |
|----------|-------------|-----------------|
| `GET /v1/usage/personal/timeline` | Daily total usage curve | `seat_id` |
| `GET /v1/usage/personal/by-protocol/timeline` | Daily usage by protocol | `seat_id` |
| `GET /v1/usage/personal/by-protocol/total` | Protocol distribution (pie chart) | `seat_id` |

### Master Page

| Endpoint | Description | Required Params |
|----------|-------------|-----------------|
| `GET /v1/usage/master/ranking` | Top users by usage | `org_id` |
| `GET /v1/usage/master/by-protocol/total` | Org protocol distribution | `org_id` |
| `GET /v1/usage/master/timeline` | Org daily total usage curve | `org_id` |

### Common Optional Params

| Param | Format | Default |
|-------|--------|---------|
| `start_date` | `YYYY-MM-DD` | 30 days ago |
| `end_date` | `YYYY-MM-DD` | today |
| `limit` | integer | 50 (ranking only) |

### Filters

- Personal endpoints: `user_usage_scope = 'normal'`
- Master endpoints: `billing_scope IN ('org_only', 'org_and_user')`

## Quick Start

```bash
cp .env.example .env
# Edit .env with your DATABASE_DSN
make build
./bin/query-service
```

## Environment Variables

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `DATABASE_DSN` | Yes | — | PostgreSQL connection string |
| `LISTEN_ADDR` | No | `0.0.0.0:27310` | HTTP listen address |
| `SERVICE_TOKEN` | No | — | Bearer token for API auth |
| `AIKEY_LOG_LEVEL` | No | `info` | Log level |

## Project Structure

```
query-service/
├── cmd/main.go
├── config/config.go
├── internal/
│   ├── api/          # HTTP handlers & router
│   ├── usage/        # Domain types, repository interface, PostgreSQL impl
│   └── shared/       # DB, response helpers, middleware
├── Makefile
└── .env.example
```

## Runtime

- Go 1.26+, PostgreSQL 14+ (shared with collector-service)
- Platforms: macOS, Linux, Windows
