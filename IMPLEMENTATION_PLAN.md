# aikey-data Implementation Plan

> Based on: DataService-ODS-DWD-数据上报方案.md + Design Review Decisions (D1–D9)

---

## Phase Overview

| Phase | Scope | Service | Status |
|-------|-------|---------|--------|
| 1 | Collector-Service skeleton + Ingest API + ODS/DWD tables | collector-service | Done |
| 2 | DWD Projector Worker | collector-service | Done |
| 3 | Local Proxy usage event generation + WAL + batch uploader | aikey-proxy | Done |
| 4 | Query Service (dashboards) | query-service | Done |
| 5 | Observability metrics | collector-service + aikey-proxy | Done |

---

## Phase 1: Collector-Service Skeleton + Ingest API (Done)

**Deliverables:**

- [x] Project structure: `aikey-data/collector-service/`
- [x] `cmd/main.go` — entry point, config, DB, migrations, HTTP server, graceful shutdown
- [x] `config/config.go` — env-based config (`DATABASE_DSN`, `LISTEN_ADDR`, `SERVICE_TOKEN`, etc.)
- [x] `migrations/001_usage_event_ods.sql` — DDL for 3 tables:
  - `usage_event_ods` (ODS raw events)
  - `usage_fact_dwd` (DWD enriched facts)
  - `usage_dwd_projector_tasks` (projector checkpoint)
- [x] `internal/ingest/domain.go` — `UsageEvent`, `BatchRequest`, `BatchResponse` types
- [x] `internal/ingest/repository.go` — `ODSRepository` interface
- [x] `internal/ingest/postgres.go` — PostgreSQL implementation, `ON CONFLICT (org_id, event_id) DO NOTHING` idempotency
- [x] `internal/ingest/service.go` — batch ingest logic + field validation
- [x] `internal/ingest/service_test.go` — 4 unit tests (happy path, duplicate, validation, mixed)
- [x] `internal/api/ingest.go` — `POST /v1/usage-events:batch` handler
- [x] `internal/api/router.go` — routing + `ServiceTokenAuth` middleware
- [x] `internal/shared/` — DB connection, migrations runner, JSON response helpers, auth middleware
- [x] `Makefile` — build, run, test, lint, tidy, clean
- [x] `README.md` + `README.zh.md`

**Design Decisions Applied:**

- D4: dual timestamps — `event_time` (client local) + `collector_time` (server)
- D7: idempotency key = `UNIQUE (org_id, event_id)`
- D8: DWD anomaly fields reserved; MVP only `valid` / `late_report_abnormal_charge` / `pending_review`

---

## Phase 2: DWD Projector Worker

**Goal:** Async background worker that scans `usage_event_ods` (pending/retry), enriches via `managed_key_control_events`, and writes to `usage_fact_dwd`.

**Deliverables:**

- [ ] `internal/projector/domain.go` — DWD fact types, quality/anomaly enums
- [ ] `internal/projector/repository.go` — DWD repository interface + control events reader interface
- [ ] `internal/projector/postgres.go` — DWD PostgreSQL implementation
- [ ] `internal/projector/worker.go` — background scan loop:
  - Read projector checkpoint from `usage_dwd_projector_tasks`
  - Batch-fetch ODS records where `dwd_status = pending` or `dwd_status = retry AND dwd_next_retry_at <= now()`
  - For each record: lookup `managed_key_control_events` by `virtual_key_id` + `event_time` window
  - Enrich and write to `usage_fact_dwd`
  - Update ODS `dwd_status` to `projected` / `retry` / `dead_letter`
  - Update projector checkpoint
- [ ] `internal/projector/enricher.go` — control-plane enrichment logic:
  - Validate ownership via control event snapshots
  - Classify anomaly: `valid`, `late_report_abnormal_charge`, `pending_review` (D8)
  - Set `billing_scope`, `user_usage_scope`, `completion_source`
- [ ] Retry strategy: 1min x3, 10min x7, 1hr thereafter, dead_letter after threshold
- [ ] Projector unit tests with mock repositories
- [ ] Wire into `cmd/main.go` as background goroutine

**Key Constraint (D5):** Only read `managed_key_control_events` table. No joins to other control-plane fact tables.

---

## Phase 3: Local Proxy Usage Event Generation + WAL

**Goal:** `aikey-proxy` generates usage events after each request, writes to JSONL WAL, and batch-uploads to collector-service.

**Deliverables:**

- [ ] `aikey-proxy/internal/events/usage_event.go` — build `UsageEvent` from request context
  - Populate all anchor fields: `event_id`, `org_id`, `account_id`, `seat_id`, `virtual_key_id`, `binding_id`, `credential_id`, `protocol_type`
  - `event_time` = client local time at request completion (D4)
  - Hash `virtual_key` and `real_key` for audit fields
- [ ] `aikey-proxy/internal/events/queue.go` — bounded in-memory queue
  - Configurable capacity (default 10000)
  - Non-blocking enqueue; drop on full (D9)
  - Track `usage_events_dropped_total` metric
- [ ] `aikey-proxy/internal/events/wal.go` — JSONL WAL writer
  - File pattern: `data/usage-wal/usage-YYYYMMDD-HH.jsonl`
  - Each line: `{"wal_seq":N, "written_at":"...", "schema_version":1, "event_json":{...}}`
  - Append-only, async, non-blocking (D2)
  - This phase: write-only, no read-back
- [ ] `aikey-proxy/internal/events/uploader.go` — batch uploader
  - Consume from in-memory queue
  - Batch size: up to 100 events
  - POST to `collector-service /v1/usage-events:batch`
  - Retry: exponential backoff (5s / 15s / 60s / 5min)
  - Auth: `SERVICE_TOKEN` bearer
- [ ] Config additions in `aikey-proxy.yaml`:
  - `events.collector_url`
  - `events.collector_token`
  - `events.queue_capacity`
  - `events.wal_dir`
  - `events.batch_size`
  - `events.upload_interval`
- [ ] Integration with request pipeline: post-response hook → build event → enqueue + WAL append

**Naming (D3):** Use `org_id` / `account_id` / `seat_id` (not `tenant_id` / `employee_id`).

---

## Phase 4: Query Service

**Goal:** Separate service providing dashboard query APIs for personal and master pages.

**Deliverables:**

- [ ] `aikey-data/query-service/` project skeleton (Go, same conventions)
- [ ] Query endpoints (all read from `usage_fact_dwd`):
  - `GET /v1/usage/personal/timeline` — personal total usage curve (`seat_id + usage_date`)
  - `GET /v1/usage/personal/by-protocol/timeline` — per-protocol usage curve
  - `GET /v1/usage/personal/by-protocol/total` — per-protocol pie chart
  - `GET /v1/usage/master/ranking` — per-user total ranking (`org_id + seat_id`)
  - `GET /v1/usage/master/by-protocol/total` — org-level per-protocol pie chart
  - `GET /v1/usage/master/timeline` — org total usage curve
- [ ] Default filters: `user_usage_scope = normal` for personal; `billing_scope IN ('org_only','org_and_user')` for master
- [ ] Pagination, date range filtering, response format
- [ ] README.md + README.zh.md

---

## Phase 5: Observability

**Goal:** Expose key metrics for operational visibility.

**Deliverables:**

- [ ] **Local Proxy metrics:**
  - `usage_events_generated_total`
  - `usage_events_enqueued_total`
  - `usage_events_dropped_total`
  - `usage_events_upload_success_total`
  - `usage_events_upload_failed_total`
  - `usage_wal_append_failed_total`
  - `usage_queue_depth`
- [ ] **Collector-Service metrics:**
  - `ingest_events_accepted_total`
  - `ingest_events_duplicated_total`
  - `ingest_events_rejected_total`
  - `projector_events_projected_total`
  - `projector_events_retry_total`
  - `projector_events_dead_letter_total`
  - `projector_scan_duration_seconds`
- [ ] Expose via `/metrics` endpoint or structured log counters
- [ ] Health endpoint enrichment with queue depth and projector lag
