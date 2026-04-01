# aikey-data 分阶段实施计划

> 依据：DataService-ODS-DWD-数据上报方案.md + 设计评审决策（D1–D9）

---

## 阶段总览

| 阶段 | 范围 | 服务 | 状态 |
|------|------|------|------|
| 1 | Collector-Service 骨架 + Ingest API + ODS/DWD 建表 | collector-service | 已完成 |
| 2 | DWD Projector Worker | collector-service | 已完成 |
| 3 | Local Proxy 事件生成 + WAL + 批量上传 | aikey-proxy | 已完成 |
| 4 | Query Service（仪表盘查询） | query-service | 已完成 |
| 5 | 可观测性指标 | collector-service + aikey-proxy | 已完成 |

---

## 阶段 1：Collector-Service 骨架 + Ingest API（已完成）

**交付物：**

- [x] 项目结构：`aikey-data/collector-service/`
- [x] `cmd/main.go` — 入口：配置加载、DB 连接、迁移、HTTP 服务、优雅关停
- [x] `config/config.go` — 基于环境变量的配置
- [x] `migrations/001_usage_event_ods.sql` — 三表 DDL：
  - `usage_event_ods`（ODS 原始事件）
  - `usage_fact_dwd`（DWD 标准明细）
  - `usage_dwd_projector_tasks`（投影器检查点）
- [x] `internal/ingest/domain.go` — `UsageEvent`、`BatchRequest`、`BatchResponse` 类型定义
- [x] `internal/ingest/repository.go` — `ODSRepository` 接口
- [x] `internal/ingest/postgres.go` — PostgreSQL 实现，`ON CONFLICT (org_id, event_id) DO NOTHING` 幂等
- [x] `internal/ingest/service.go` — 批量 ingest 业务逻辑 + 字段校验
- [x] `internal/ingest/service_test.go` — 4 个单元测试
- [x] `internal/api/ingest.go` — `POST /v1/usage-events:batch` 处理器
- [x] `internal/api/router.go` — 路由 + `ServiceTokenAuth` 中间件
- [x] `internal/shared/` — DB 连接、迁移执行器、JSON 响应工具、认证中间件
- [x] `Makefile`、`README.md`、`README.zh.md`

**已落地的设计决策：**

- D4：双时间戳 — `event_time`（客户端本地时间）+ `collector_time`（服务端时间）
- D7：幂等键 = `UNIQUE (org_id, event_id)`
- D8：DWD 异常字段预留；MVP 只判定 `valid` / `late_report_abnormal_charge` / `pending_review`

---

## 阶段 2：DWD Projector Worker

**目标：** 后台异步 worker，扫描 `usage_event_ods` 中 pending/retry 记录，联查 `managed_key_control_events` 补全，写入 `usage_fact_dwd`。

**交付物：**

- [ ] `internal/projector/domain.go` — DWD 事实类型、质量/异常枚举
- [ ] `internal/projector/repository.go` — DWD 仓库接口 + 控制事件只读接口
- [ ] `internal/projector/postgres.go` — DWD PostgreSQL 实现
- [ ] `internal/projector/worker.go` — 后台扫描循环：
  - 读取 `usage_dwd_projector_tasks` 检查点
  - 批量拉取 ODS 中 `dwd_status = pending` 或 `dwd_status = retry AND dwd_next_retry_at <= now()` 的记录
  - 对每条记录：按 `virtual_key_id` + `event_time` 时间窗口查询 `managed_key_control_events`
  - 补全后写入 `usage_fact_dwd`
  - 更新 ODS `dwd_status`（projected / retry / dead_letter）
  - 更新投影器检查点
- [ ] `internal/projector/enricher.go` — 控制面补全逻辑：
  - 通过控制事件快照验证归属关系
  - 异常分类：`valid`、`late_report_abnormal_charge`、`pending_review`（D8）
  - 设置 `billing_scope`、`user_usage_scope`、`completion_source`
- [ ] 重试策略：1min x3、10min x7、之后 1hr、超阈值标记 dead_letter
- [ ] 单元测试（mock 仓库）
- [ ] 接入 `cmd/main.go` 作为后台 goroutine

**关键约束（D5）：** 仅读取 `managed_key_control_events` 表，不联查其他控制面事实表。

---

## 阶段 3：Local Proxy 事件生成 + WAL

**目标：** `aikey-proxy` 在每次请求完成后生成 usage event，写入 JSONL WAL，并批量上传到 collector-service。

**交付物：**

- [ ] `aikey-proxy/internal/events/usage_event.go` — 从请求上下文构建 `UsageEvent`
  - 填充所有锚点字段：`event_id`、`org_id`、`account_id`、`seat_id`、`virtual_key_id`、`binding_id`、`credential_id`、`protocol_type`
  - `event_time` = 请求完成时的客户端本地时间（D4）
  - 对 `virtual_key` 和 `real_key` 做不可逆摘要
- [ ] `aikey-proxy/internal/events/queue.go` — 有界内存队列
  - 可配置容量（默认 10000）
  - 非阻塞入队；满时丢弃（D9）
  - 记录 `usage_events_dropped_total` 指标
- [ ] `aikey-proxy/internal/events/wal.go` — JSONL WAL 写入器
  - 文件模式：`data/usage-wal/usage-YYYYMMDD-HH.jsonl`
  - 每行：`{"wal_seq":N, "written_at":"...", "schema_version":1, "event_json":{...}}`
  - 追加写入、异步、非阻塞（D2）
  - 本期只写不读
- [ ] `aikey-proxy/internal/events/uploader.go` — 批量上传器
  - 从内存队列消费
  - 批大小：最多 100 条
  - POST 到 collector-service `/v1/usage-events:batch`
  - 重试：指数退避（5s / 15s / 60s / 5min）
  - 认证：`SERVICE_TOKEN` Bearer
- [ ] `aikey-proxy.yaml` 新增配置项：
  - `events.collector_url`
  - `events.collector_token`
  - `events.queue_capacity`
  - `events.wal_dir`
  - `events.batch_size`
  - `events.upload_interval`
- [ ] 请求流水线集成：响应完成后 → 构建 event → 入队 + WAL 追加

**命名规范（D3）：** 统一使用 `org_id` / `account_id` / `seat_id`。

---

## 阶段 4：Query Service

**目标：** 独立查询服务，为个人页和 Master 页提供仪表盘查询 API。

**交付物：**

- [ ] `aikey-data/query-service/` 项目骨架
- [ ] 查询端点（全部读取 `usage_fact_dwd`）：
  - `GET /v1/usage/personal/timeline` — 个人总用量曲线（`seat_id + usage_date`）
  - `GET /v1/usage/personal/by-protocol/timeline` — 分协议用量曲线
  - `GET /v1/usage/personal/by-protocol/total` — 分协议总量饼图
  - `GET /v1/usage/master/ranking` — 分用户总量排行（`org_id + seat_id`）
  - `GET /v1/usage/master/by-protocol/total` — 组织级分协议饼图
  - `GET /v1/usage/master/timeline` — 所有成员总用量曲线
- [ ] 默认过滤：个人页 `user_usage_scope = normal`；Master 页 `billing_scope IN ('org_only','org_and_user')`
- [ ] 分页、日期范围过滤、响应格式
- [ ] README.md + README.zh.md

---

## 阶段 5：可观测性

**目标：** 暴露关键指标，支持运维监控。

**交付物：**

- [ ] **Local Proxy 指标：**
  - `usage_events_generated_total`
  - `usage_events_enqueued_total`
  - `usage_events_dropped_total`
  - `usage_events_upload_success_total`
  - `usage_events_upload_failed_total`
  - `usage_wal_append_failed_total`
  - `usage_queue_depth`
- [ ] **Collector-Service 指标：**
  - `ingest_events_accepted_total`
  - `ingest_events_duplicated_total`
  - `ingest_events_rejected_total`
  - `projector_events_projected_total`
  - `projector_events_retry_total`
  - `projector_events_dead_letter_total`
  - `projector_scan_duration_seconds`
- [ ] 通过 `/metrics` 端点或结构化日志计数器暴露
- [ ] 健康端点增强：队列深度、投影器延迟
