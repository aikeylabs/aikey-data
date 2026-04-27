# aikey-data / collector-service

接收 `aikey-proxy` 上报的 usage 事件，持久化原始明细到 ODS，并异步投影到 DWD 标准明细层。

## 架构

```
Local Proxy ──POST /v1/usage-events:batch──▶ Ingest API ──▶ USAGE_EVENT_ODS
                                                                   │
                                                         DWD Projector (异步)
                                                                   │
                                                                   ▼
                                              MANAGED_KEY_CONTROL_EVENTS (只读)
                                                                   │
                                                                   ▼
                                                          USAGE_FACT_DWD
```

## 服务拆分

| 服务 | 端口 | 职责 |
|------|------|------|
| `collector-service` | 27300 | Ingest API、ODS 持久化、DWD 投影 |
| `query-service` | 27310 | 面向仪表盘的查询聚合（计划中）|

## 快速开始

```bash
# 前提：Go 1.26+、PostgreSQL（与 aikey-control 共用实例）
cp .env.example .env
# 编辑 .env，填入 DATABASE_DSN

make build
./bin/collector-service
```

## 时间戳存储规范（v1.0.3-alpha+）

所有 usage 管道时间字段（`event_time` / `occurred_at` / `started_at` / `finished_at` / `projected_at` / `ingest_received_at` / `collector_time` / `dwd_next_retry_at`）在 SQLite（personal / trial）下以 **int64 Unix 毫秒时间戳（UTC）** 存储；在 PostgreSQL（生产）下仍为 **TIMESTAMPTZ**。

Go 代码统一使用 `aikeytime.Millis` 作为结构体字段类型，`shared.DB.BindMillis(m)` 按方言下发正确的驱动参数。Proxy → Collector 线上 JSON wire 为 int64 毫秒：

```json
{ "event_time": 1777041000000, "occurred_at": 1777041000000 }
```

详见设计文档 `roadmap20260320/技术实现/update/20260424-时间戳统一为int64毫秒-data-service.md` 与修复记录 `workflow/CI/bugfix/20260424-today-use-card-empty.md`。

## API

### `POST /v1/usage-events:batch`

批量接收 usage 事件。需要 `Authorization: Bearer <SERVICE_TOKEN>`。

**请求体：**
```json
{
  "source": "aikey-proxy",
  "source_version": "0.1.0",
  "proxy_instance_id": "proxy-01",
  "events": [{ "event_id": "...", "org_id": "...", ... }]
}
```

**响应体：**
```json
{ "accepted": 97, "duplicated": 3, "rejected": 0 }
```

### `GET /health`

健康检查（无需认证）。

## 数据库

与 `aikey-control` 共用 PostgreSQL 实例。表：

- `usage_event_ods` — 原始事件明细（ODS 层）
- `usage_fact_dwd` — 补全后的标准明细（DWD 层）
- `usage_dwd_projector_tasks` — 投影器检查点

迁移脚本在 `migrations/` 目录下，服务启动时自动执行。

## 环境变量

| 变量 | 必填 | 默认值 | 说明 |
|------|------|--------|------|
| `DATABASE_DSN` | 是 | — | PostgreSQL 连接字符串 |
| `LISTEN_ADDR` | 否 | `0.0.0.0:27300` | HTTP 监听地址 |
| `MIGRATIONS_DIR` | 否 | `./migrations` | SQL 迁移目录 |
| `SERVICE_TOKEN` | 否 | — | API 认证 Bearer Token |
| `AIKEY_LOG_LEVEL` | 否 | `info` | 日志级别（debug/info/warn/error）|

## 项目结构

```
collector-service/
├── cmd/main.go              # 入口
├── config/config.go         # 基于环境变量的配置
├── migrations/              # SQL 迁移文件
├── internal/
│   ├── api/                 # HTTP 处理器与路由
│   ├── ingest/              # 领域类型、服务、ODS 仓库
│   ├── projector/           # DWD 投影 worker（计划中）
│   └── shared/              # 数据库、响应工具、中间件
├── Makefile
└── .env.example
```

## 运行环境

- Go 1.26+
- PostgreSQL 14+
- 支持平台：macOS、Linux、Windows
