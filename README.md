# aikey-data

Usage data pipeline for AiKey — receives usage events from `aikey-proxy`, stores raw facts (ODS), enriches with control-plane context (DWD), and serves dashboard queries.

## Responsibilities

- Receive batch usage events from Local Proxy (idempotent ingest)
- Persist raw events to ODS layer for audit and replay
- Async DWD projection: enrich events via `managed_key_control_events` history
- Anomaly classification (valid / late report / pending review)
- Personal and Master dashboard query aggregation
- Observability metrics for ingest, projection, and upload pipeline

## Architecture

```
Local Proxy (aikey-proxy)
  │
  │  POST /v1/usage-events:batch
  │  (Bearer SERVICE_TOKEN)
  ▼
┌───────────────────────────────────────────────────────┐
│              collector-service (:27300)                │
│                                                       │
│  ┌──────────────┐   ┌─────────────────────────────┐   │
│  │ Ingest API   │──▶│ USAGE_EVENT_ODS             │   │
│  │ (validate,   │   │ (raw events, idempotent)    │   │
│  │  idempotent) │   └──────────┬──────────────────┘   │
│  └──────────────┘              │                      │
│                                ▼                      │
│  ┌──────────────────────────────────────────────┐     │
│  │ DWD Projector (async, every 5s)              │     │
│  │  ┌──────────────────────────────────────┐    │     │
│  │  │ managed_key_control_events (read)    │    │     │
│  │  └──────────────────────────────────────┘    │     │
│  │  enrich → validate ownership → classify     │     │
│  │  anomaly → write USAGE_FACT_DWD             │     │
│  └──────────────────────────────────────────────┘     │
│                                                       │
│  GET /health    GET /metrics                          │
└───────────────────────────────────────────────────────┘

┌───────────────────────────────────────────────────────┐
│              query-service (:27310)                    │
│                                                       │
│  ┌──────────────────────────────────────────────┐     │
│  │ Personal Page APIs                           │     │
│  │  /v1/usage/personal/timeline                 │     │
│  │  /v1/usage/personal/by-protocol/timeline     │     │
│  │  /v1/usage/personal/by-protocol/total        │     │
│  ├──────────────────────────────────────────────┤     │
│  │ Master Page APIs                             │     │
│  │  /v1/usage/master/ranking                    │     │
│  │  /v1/usage/master/by-protocol/total          │     │
│  │  /v1/usage/master/timeline                   │     │
│  └──────────────────────────────────────────────┘     │
│                        │                              │
│                        ▼                              │
│               USAGE_FACT_DWD (read)                   │
│                                                       │
│  GET /health                                          │
└───────────────────────────────────────────────────────┘
```

## Data Flow

```
Request completed at Local Proxy
    │
    ├─▶ JSONL WAL (write-only, phase 2 read-back)
    │
    └─▶ Memory Queue (10000 capacity, drop if full)
            │
            ▼
     Batch Uploader (100 events, 5s interval, exponential retry)
            │
            ▼
     collector-service: Ingest API
            │
            ▼
     USAGE_EVENT_ODS (raw, org_id + event_id unique)
            │
            ▼ (async projector, 5s scan)
     managed_key_control_events (read-only, D5)
            │
            ▼
     USAGE_FACT_DWD (enriched, anomaly classified)
            │
            ▼
     query-service: Dashboard APIs
```

## Tech Stack

| Component | Choice | Rationale |
|-----------|--------|-----------|
| Language | Go 1.26 | Consistent with aikey-proxy |
| HTTP | `net/http` (Go 1.22+ ServeMux) | Lightweight, no framework |
| Database | PostgreSQL (shared with aikey-control) | Transactional, JSONB, mature |
| DB driver | `github.com/lib/pq` | Standard `database/sql`, hand-written SQL |
| Migration | Raw SQL files, auto-run on startup | Simple, auditable |

## Runtime Environment

| Item | Requirement |
|------|-------------|
| Go | >= 1.26.1 (build only) |
| PostgreSQL | >= 14 (shared instance with aikey-control) |
| OS | macOS, Linux, Windows |
| Memory | ~30 MB RSS per service |
| Network | collector-service: accessible from Local Proxy; query-service: accessible from Web Console |

## Quick Start

### Prerequisites

- Go 1.26+ installed
- PostgreSQL running (reuse aikey-control's instance, or start a new one)
- `aikey-control` migrations applied (for `managed_key_control_events` table)

### 1. Start PostgreSQL (if not already running)

```bash
# Option A: reuse aikey-control's docker-compose
cd ../aikey-control/service && docker compose up -d postgres

# Option B: standalone
docker run -d --name aikey-pg \
  -e POSTGRES_USER=aikey \
  -e POSTGRES_PASSWORD=aikey_dev_password \
  -e POSTGRES_DB=aikey_control \
  -p 5432:5432 \
  postgres:16-alpine
```

### 2. Build and start collector-service

```bash
cd collector-service
cp .env.example .env
# Edit .env — set DATABASE_DSN to match your PostgreSQL

make build
./bin/collector-service
# Output: collector-service started addr=0.0.0.0:27300
```

### 3. Build and start query-service

```bash
cd query-service
cp .env.example .env
# Edit .env — set DATABASE_DSN (same as collector-service)

make build
./bin/query-service
# Output: query-service started addr=0.0.0.0:27310
```

### 4. Verify

```bash
curl http://localhost:27300/health
# {"status":"ok"}

curl http://localhost:27310/health
# {"status":"ok"}
```

## Usage Examples

### Ingest usage events

```bash
curl -X POST http://localhost:27300/v1/usage-events:batch \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer changeme" \
  -d '{
    "source": "aikey-proxy",
    "source_version": "0.1.0",
    "proxy_instance_id": "proxy-dev-01",
    "events": [
      {
        "event_id": "evt_test_001",
        "org_id": "org_demo",
        "account_id": "acc_demo",
        "seat_id": "seat_demo",
        "event_time": "2026-04-01T10:00:00Z",
        "occurred_at": "2026-04-01T10:00:00Z",
        "virtual_key_id": "vk_test",
        "provider_code": "anthropic",
        "protocol_type": "anthropic",
        "model": "claude-sonnet-4-5-20250929",
        "request_count": 1,
        "input_tokens": 1200,
        "output_tokens": 350,
        "total_tokens": 1550,
        "request_status": "success",
        "http_status_code": 200
      }
    ]
  }'
# {"accepted":1,"duplicated":0,"rejected":0}
```

### Check metrics

```bash
curl http://localhost:27300/metrics
# {"ingest":{"ingest_events_accepted_total":1,...},"projector":{...}}
```

### Query personal usage (after DWD projection)

```bash
curl "http://localhost:27310/v1/usage/personal/timeline?seat_id=seat_demo&start_date=2026-03-01&end_date=2026-04-30" \
  -H "Authorization: Bearer changeme"
# [{"date":"2026-04-01","total_tokens":1550,"request_count":1}]
```

### Query master ranking

```bash
curl "http://localhost:27310/v1/usage/master/ranking?org_id=org_demo&start_date=2026-03-01&end_date=2026-04-30" \
  -H "Authorization: Bearer changeme"
# [{"account_id":"acc_demo","seat_id":"seat_demo","total_tokens":1550,"request_count":1}]
```

## Manual Acceptance Testing

Below is a step-by-step guide to verify the full data pipeline end-to-end.

### Step 0: Environment setup

```bash
# Ensure PostgreSQL is running and aikey-control migrations are applied
# (managed_key_control_events table must exist)

export DATABASE_DSN="postgres://aikey:aikey_dev_password@localhost:5432/aikey_control?sslmode=disable"
export SERVICE_TOKEN="test-token-123"
```

### Step 1: Run unit tests

```bash
# collector-service (10 tests: 4 ingest + 6 projector)
cd collector-service && go test -race -v ./internal/...

# query-service (6 tests)
cd ../query-service && go test -race -v ./internal/...
```

Expected: all tests PASS.

### Step 2: Start services

```bash
# Terminal 1 — collector-service
cd collector-service
DATABASE_DSN=$DATABASE_DSN SERVICE_TOKEN=$SERVICE_TOKEN LOG_LEVEL=debug ./bin/collector-service

# Terminal 2 — query-service
cd query-service
DATABASE_DSN=$DATABASE_DSN SERVICE_TOKEN=$SERVICE_TOKEN LOG_LEVEL=debug ./bin/query-service
```

Verify health:

```bash
curl -s http://localhost:27300/health | grep ok
curl -s http://localhost:27310/health | grep ok
```

### Step 3: Verify table creation

```bash
psql "$DATABASE_DSN" -c "\dt usage_*"
```

Expected tables: `usage_event_ods`, `usage_fact_dwd`, `usage_dwd_projector_tasks`.

### Step 4: Ingest a test event

```bash
curl -s -X POST http://localhost:27300/v1/usage-events:batch \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $SERVICE_TOKEN" \
  -d '{
    "source": "manual-test",
    "events": [{
      "event_id": "test_accept_001",
      "org_id": "org_test",
      "account_id": "acc_test",
      "seat_id": "seat_test",
      "event_time": "2026-04-01T12:00:00Z",
      "occurred_at": "2026-04-01T12:00:00Z",
      "virtual_key_id": "vk_test_001",
      "provider_code": "openai",
      "protocol_type": "openai",
      "model": "gpt-4o-mini",
      "request_count": 1,
      "input_tokens": 500,
      "output_tokens": 200,
      "total_tokens": 700,
      "request_status": "success",
      "http_status_code": 200
    }]
  }'
```

Expected: `{"accepted":1,"duplicated":0,"rejected":0}`

### Step 5: Verify idempotency (send same event again)

```bash
# Re-send the exact same request from Step 4
# (same event_id + org_id)
```

Expected: `{"accepted":0,"duplicated":1,"rejected":0}`

### Step 6: Verify ODS persistence

```bash
psql "$DATABASE_DSN" -c "SELECT event_id, org_id, model, total_tokens, dwd_status FROM usage_event_ods WHERE event_id='test_accept_001';"
```

Expected: 1 row, `dwd_status` should transition from `pending` to `projected` within ~5s.

### Step 7: Verify DWD projection

Wait 5–10 seconds for the projector to run, then:

```bash
psql "$DATABASE_DSN" -c "SELECT event_id, org_id, seat_id, model, total_tokens, quality_status, billing_scope, user_usage_scope FROM usage_fact_dwd WHERE event_id='test_accept_001';"
```

Expected: 1 row. If no `managed_key_control_events` match exists for `vk_test_001`, expect `quality_status='partial'`, `anomaly_type='pending_review'`.

### Step 8: Verify metrics

```bash
curl -s http://localhost:27300/metrics | python3 -m json.tool
```

Expected:
```json
{
  "ingest": {
    "ingest_events_accepted_total": 1,
    "ingest_events_duplicated_total": 1,
    "ingest_events_rejected_total": 0
  },
  "projector": {
    "projector_events_projected_total": 1,
    ...
  }
}
```

### Step 9: Verify query-service

```bash
# Personal timeline
curl -s "http://localhost:27310/v1/usage/personal/timeline?seat_id=seat_test&start_date=2026-04-01&end_date=2026-04-01" \
  -H "Authorization: Bearer $SERVICE_TOKEN" | python3 -m json.tool
```

Expected (if DWD projected with `user_usage_scope='normal'`): 1 data point with `total_tokens=700`.

If DWD projected with `user_usage_scope='excluded'` (no control event match): empty array `[]` — this is correct behavior.

```bash
# Master timeline (uses billing_scope filter)
curl -s "http://localhost:27310/v1/usage/master/timeline?org_id=org_test&start_date=2026-04-01&end_date=2026-04-01" \
  -H "Authorization: Bearer $SERVICE_TOKEN" | python3 -m json.tool
```

### Step 10: Verify validation rejections

```bash
curl -s -X POST http://localhost:27300/v1/usage-events:batch \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $SERVICE_TOKEN" \
  -d '{
    "events": [{
      "event_id": "",
      "org_id": "",
      "event_time": "2026-04-01T12:00:00Z",
      "occurred_at": "2026-04-01T12:00:00Z",
      "request_status": "success"
    }]
  }'
```

Expected: `{"accepted":0,"duplicated":0,"rejected":1}` (missing `event_id` and `org_id`).

### Step 11: Verify auth rejection

```bash
curl -s -X POST http://localhost:27300/v1/usage-events:batch \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer wrong-token" \
  -d '{"events":[]}'
```

Expected: HTTP 401 `{"code":"UNAUTHORIZED","message":"invalid or missing service token"}`

### Acceptance Criteria Checklist

- [ ] Both services start and pass health check
- [ ] ODS/DWD/projector_tasks tables auto-created on startup
- [ ] Batch ingest accepts valid events
- [ ] Duplicate events return `duplicated` (idempotent)
- [ ] Invalid events return `rejected` without blocking the batch
- [ ] Auth failure returns 401
- [ ] ODS records transition from `pending` to `projected`
- [ ] DWD facts created with quality/anomaly classification
- [ ] Metrics endpoint returns accurate counters
- [ ] Query-service returns aggregated data from DWD

## Error Codes

### collector-service

| Code | HTTP | Description |
|------|------|-------------|
| `INVALID_JSON` | 400 | Cannot parse request body |
| `EMPTY_BATCH` | 400 | Events array is empty |
| `BATCH_TOO_LARGE` | 400 | Exceeds max batch size (500) |
| `UNAUTHORIZED` | 401 | Invalid or missing service token |

### query-service

| Code | HTTP | Description |
|------|------|-------------|
| `INVALID_PARAMS` | 400 | Missing required query parameter |
| `QUERY_FAILED` | 500 | Internal query error |
| `UNAUTHORIZED` | 401 | Invalid or missing service token |

## Project Structure

```
aikey-data/
├── IMPLEMENTATION_PLAN.md         Implementation phases and status
├── collector-service/
│   ├── cmd/main.go                Entry point, DI wiring, graceful shutdown
│   ├── config/config.go           Env-based configuration
│   ├── migrations/
│   │   └── 001_usage_event_ods.sql  ODS + DWD + projector_tasks DDL
│   ├── internal/
│   │   ├── api/                   HTTP handlers (ingest, metrics) + router
│   │   ├── ingest/                UsageEvent types, validation, ODS repository
│   │   ├── projector/             DWD enricher, worker, retry, checkpoint
│   │   └── shared/               DB, response helpers, auth middleware
│   ├── Makefile
│   └── .env.example
└── query-service/
    ├── cmd/main.go                Entry point, DI wiring, graceful shutdown
    ├── config/config.go           Env-based configuration
    ├── internal/
    │   ├── api/                   6 query handlers + router
    │   ├── usage/                 Domain types, Repository interface, PostgreSQL
    │   └── shared/               DB, response helpers, auth middleware
    ├── Makefile
    └── .env.example
```

## License

See [LICENSE](LICENSE) for details.
