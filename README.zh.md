# aikey-data

AiKey 用量数据管道 — 接收 `aikey-proxy` 上报的 usage 事件，存储原始明细（ODS），结合控制面上下文补全（DWD），并提供仪表盘查询。

## 职责

- 接收 Local Proxy 批量上报的 usage 事件（幂等入库）
- 持久化原始事件到 ODS 层，用于审计和回放
- 异步 DWD 投影：通过 `managed_key_control_events` 历史记录补全事件
- 异常分类（valid / late_report_abnormal_charge / pending_review）
- 个人页和 Master 页仪表盘查询聚合
- 可观测性指标：ingest、投影、上传链路

## 架构

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
│  │ (校验、幂等) │   │ (原始事件)                   │   │
│  └──────────────┘   └──────────┬──────────────────┘   │
│                                ▼                      │
│  ┌──────────────────────────────────────────────┐     │
│  │ DWD Projector（异步，每 5 秒）               │     │
│  │  读取 managed_key_control_events             │     │
│  │  补全 → 校验归属 → 异常分类                  │     │
│  │  写入 USAGE_FACT_DWD                         │     │
│  └──────────────────────────────────────────────┘     │
│                                                       │
│  GET /health    GET /metrics                          │
└───────────────────────────────────────────────────────┘

┌───────────────────────────────────────────────────────┐
│              query-service (:27301)                    │
│                                                       │
│  个人页 API                      Master 页 API        │
│  /v1/usage/personal/timeline     /v1/usage/master/... │
│  /v1/usage/personal/by-protocol  /v1/usage/master/... │
│                                                       │
│  全部读取 USAGE_FACT_DWD                               │
│                                                       │
│  GET /health                                          │
└───────────────────────────────────────────────────────┘
```

## 数据流

```
请求在 Local Proxy 完成
    │
    ├─▶ JSONL WAL（只写，二期回读补传）
    │
    └─▶ 内存队列（10000 容量，满时丢弃）
            │
            ▼
     批量上传器（100 条/批，5s 间隔，指数退避重试）
            │
            ▼
     collector-service: Ingest API
            │
            ▼
     USAGE_EVENT_ODS（原始，org_id + event_id 唯一）
            │
            ▼（异步投影器，5s 扫描）
     managed_key_control_events（只读，D5）
            │
            ▼
     USAGE_FACT_DWD（补全后，异常分类）
            │
            ▼
     query-service: 仪表盘 API
```

## 技术栈

| 组件 | 选型 | 原因 |
|------|------|------|
| 语言 | Go 1.26 | 与 aikey-proxy 一致 |
| HTTP | `net/http`（Go 1.22+ ServeMux）| 轻量，无框架 |
| 数据库 | PostgreSQL（与 aikey-control 共用） | 事务、JSONB、成熟 |
| 数据库驱动 | `github.com/lib/pq` | 标准 `database/sql`，手写 SQL |
| 迁移 | 原始 SQL 文件，启动时自动执行 | 简单、可审计 |

## 运行环境

| 项目 | 要求 |
|------|------|
| Go | >= 1.26.1（仅构建时需要）|
| PostgreSQL | >= 14（与 aikey-control 共用实例）|
| 操作系统 | macOS、Linux、Windows |
| 内存 | 每个服务约 30 MB RSS |
| 网络 | collector-service 需要 Local Proxy 可达；query-service 需要 Web Console 可达 |

## 快速开始

### 前提

- Go 1.26+ 已安装
- PostgreSQL 运行中（复用 aikey-control 的实例，或单独启动）
- `aikey-control` 迁移已执行（`managed_key_control_events` 表必须存在）

### 1. 启动 PostgreSQL（如未运行）

```bash
# 方式 A: 复用 aikey-control 的 docker-compose
cd ../aikey-control/service && docker compose up -d postgres

# 方式 B: 独立启动
docker run -d --name aikey-pg \
  -e POSTGRES_USER=aikey \
  -e POSTGRES_PASSWORD=aikey_dev_password \
  -e POSTGRES_DB=aikey_control \
  -p 5432:5432 \
  postgres:16-alpine
```

### 2. 构建并启动 collector-service

```bash
cd collector-service
cp .env.example .env
# 编辑 .env — 设置 DATABASE_DSN 为你的 PostgreSQL 连接

make build
./bin/collector-service
# 输出: collector-service started addr=0.0.0.0:27300
```

### 3. 构建并启动 query-service

```bash
cd query-service
cp .env.example .env
# 编辑 .env — 设置 DATABASE_DSN（与 collector-service 相同）

make build
./bin/query-service
# 输出: query-service started addr=0.0.0.0:27301
```

### 4. 验证

```bash
curl http://localhost:27300/health
# {"status":"ok"}

curl http://localhost:27301/health
# {"status":"ok"}
```

## 使用示例

### 上报 usage 事件

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

### 查看指标

```bash
curl http://localhost:27300/metrics
# {"ingest":{"ingest_events_accepted_total":1,...},"projector":{...}}
```

### 查询个人用量（DWD 投影完成后）

```bash
curl "http://localhost:27301/v1/usage/personal/timeline?seat_id=seat_demo&start_date=2026-03-01&end_date=2026-04-30" \
  -H "Authorization: Bearer changeme"
# [{"date":"2026-04-01","total_tokens":1550,"request_count":1}]
```

## 手工测试验收步骤

以下是完整的端到端验收流程，可按步骤逐一执行。

### 步骤 0: 环境准备

```bash
# 确保 PostgreSQL 运行，且 aikey-control 迁移已执行
# （managed_key_control_events 表必须存在）

export DATABASE_DSN="postgres://aikey:aikey_dev_password@localhost:5432/aikey_control?sslmode=disable"
export SERVICE_TOKEN="test-token-123"
```

### 步骤 1: 运行单元测试

```bash
# collector-service（10 个测试：4 ingest + 6 projector）
cd collector-service && go test -race -v ./internal/...

# query-service（6 个测试）
cd ../query-service && go test -race -v ./internal/...
```

预期：全部 PASS。

### 步骤 2: 启动服务

```bash
# 终端 1 — collector-service
cd collector-service
DATABASE_DSN=$DATABASE_DSN SERVICE_TOKEN=$SERVICE_TOKEN LOG_LEVEL=debug ./bin/collector-service

# 终端 2 — query-service
cd query-service
DATABASE_DSN=$DATABASE_DSN SERVICE_TOKEN=$SERVICE_TOKEN LOG_LEVEL=debug ./bin/query-service
```

验证健康检查：

```bash
curl -s http://localhost:27300/health | grep ok
curl -s http://localhost:27301/health | grep ok
```

### 步骤 3: 检查表是否自动创建

```bash
psql "$DATABASE_DSN" -c "\dt usage_*"
```

预期表：`usage_event_ods`、`usage_fact_dwd`、`usage_dwd_projector_tasks`。

### 步骤 4: 写入测试事件

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

预期：`{"accepted":1,"duplicated":0,"rejected":0}`

### 步骤 5: 验证幂等（重发同一事件）

```bash
# 重新发送步骤 4 完全相同的请求
```

预期：`{"accepted":0,"duplicated":1,"rejected":0}`

### 步骤 6: 检查 ODS 入库

```bash
psql "$DATABASE_DSN" -c "SELECT event_id, org_id, model, total_tokens, dwd_status FROM usage_event_ods WHERE event_id='test_accept_001';"
```

预期：1 行记录，`dwd_status` 应在约 5 秒内从 `pending` 变为 `projected`。

### 步骤 7: 检查 DWD 投影

等待 5–10 秒让投影器运行，然后：

```bash
psql "$DATABASE_DSN" -c "SELECT event_id, org_id, seat_id, model, total_tokens, quality_status, billing_scope, user_usage_scope FROM usage_fact_dwd WHERE event_id='test_accept_001';"
```

预期：1 行记录。如果 `managed_key_control_events` 中没有 `vk_test_001` 的匹配记录，则 `quality_status='partial'`，`anomaly_type='pending_review'`。

### 步骤 8: 检查指标

```bash
curl -s http://localhost:27300/metrics | python3 -m json.tool
```

预期：
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

### 步骤 9: 验证查询服务

```bash
# 个人用量曲线
curl -s "http://localhost:27301/v1/usage/personal/timeline?seat_id=seat_test&start_date=2026-04-01&end_date=2026-04-01" \
  -H "Authorization: Bearer $SERVICE_TOKEN" | python3 -m json.tool
```

预期（如果 DWD 投影时 `user_usage_scope='normal'`）：1 个数据点，`total_tokens=700`。

如果 DWD 投影时 `user_usage_scope='excluded'`（无控制事件匹配）：空数组 `[]` — 这是正确行为。

```bash
# Master 总用量曲线（使用 billing_scope 过滤）
curl -s "http://localhost:27301/v1/usage/master/timeline?org_id=org_test&start_date=2026-04-01&end_date=2026-04-01" \
  -H "Authorization: Bearer $SERVICE_TOKEN" | python3 -m json.tool
```

### 步骤 10: 验证校验拒绝

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

预期：`{"accepted":0,"duplicated":0,"rejected":1}`（缺少 `event_id` 和 `org_id`）。

### 步骤 11: 验证认证拒绝

```bash
curl -s -X POST http://localhost:27300/v1/usage-events:batch \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer wrong-token" \
  -d '{"events":[]}'
```

预期：HTTP 401 `{"code":"UNAUTHORIZED","message":"invalid or missing service token"}`

### 验收检查清单

- [ ] 两个服务启动并通过健康检查
- [ ] ODS/DWD/projector_tasks 表启动时自动创建
- [ ] 批量 ingest 接受合法事件
- [ ] 重复事件返回 `duplicated`（幂等）
- [ ] 非法事件返回 `rejected` 且不阻断同批其他事件
- [ ] 认证失败返回 401
- [ ] ODS 记录从 `pending` 转为 `projected`
- [ ] DWD 事实已创建，含 quality/anomaly 分类
- [ ] metrics 端点返回准确计数
- [ ] query-service 返回 DWD 聚合数据

## 错误码

### collector-service

| 错误码 | HTTP | 说明 |
|--------|------|------|
| `INVALID_JSON` | 400 | 请求体解析失败 |
| `EMPTY_BATCH` | 400 | events 数组为空 |
| `BATCH_TOO_LARGE` | 400 | 超过最大批次大小（500）|
| `UNAUTHORIZED` | 401 | 认证 token 无效或缺失 |

### query-service

| 错误码 | HTTP | 说明 |
|--------|------|------|
| `INVALID_PARAMS` | 400 | 缺少必需的查询参数 |
| `QUERY_FAILED` | 500 | 内部查询错误 |
| `UNAUTHORIZED` | 401 | 认证 token 无效或缺失 |

## 项目结构

```
aikey-data/
├── IMPLEMENTATION_PLAN.md         实施阶段和状态
├── collector-service/
│   ├── cmd/main.go                入口、依赖注入、优雅关停
│   ├── config/config.go           基于环境变量的配置
│   ├── migrations/
│   │   └── 001_usage_event_ods.sql  ODS + DWD + projector_tasks DDL
│   ├── internal/
│   │   ├── api/                   HTTP 处理器（ingest、metrics）+ 路由
│   │   ├── ingest/                UsageEvent 类型、校验、ODS 仓库
│   │   ├── projector/             DWD 补全器、worker、重试、检查点
│   │   └── shared/               数据库、响应工具、认证中间件
│   ├── Makefile
│   └── .env.example
└── query-service/
    ├── cmd/main.go                入口、依赖注入、优雅关停
    ├── config/config.go           基于环境变量的配置
    ├── internal/
    │   ├── api/                   6 个查询处理器 + 路由
    │   ├── usage/                 领域类型、Repository 接口、PostgreSQL
    │   └── shared/               数据库、响应工具、认证中间件
    ├── Makefile
    └── .env.example
```

## 许可证

详见 [LICENSE](LICENSE)。
