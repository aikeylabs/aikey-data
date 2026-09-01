# aikey-data / query-service

为个人页和 Master 页提供 usage 查询 API，读取 `collector-service` 生成的 DWD 层数据。

## API 端点

所有端点需要 `Authorization: Bearer <SERVICE_TOKEN>`（`/health` 除外）。

### 个人页

| 端点 | 说明 | 必需参数 |
|------|------|----------|
| `GET /v1/usage/personal/timeline` | 个人总用量曲线 | `seat_id` |
| `GET /v1/usage/personal/by-protocol/timeline` | 分协议用量曲线 | `seat_id` |
| `GET /v1/usage/personal/by-protocol/total` | 分协议总量饼图 | `seat_id` |

### Master 页

| 端点 | 说明 | 必需参数 |
|------|------|----------|
| `GET /v1/usage/master/ranking` | 分用户总量排行 | `org_id` |
| `GET /v1/usage/master/by-protocol/total` | 组织级分协议饼图 | `org_id` |
| `GET /v1/usage/master/timeline` | 所有成员总用量曲线 | `org_id` |

### 通用可选参数

| 参数 | 格式 | 默认值 |
|------|------|--------|
| `start_date` | `YYYY-MM-DD` | 30 天前 |
| `end_date` | `YYYY-MM-DD` | 今天 |
| `limit` | 整数，或 `all` | 50（仅排行）|

> `limit=all` 返回**全部**行，而不是前 N 名。它是给「合计必须对得上」的调用方用的 —— 控制台
> 「按部门」的成本表必须等于组织总额，而前 N 名的列表无论 N 取多大都做不到。图表继续传数字。

### 过滤条件

- 个人页：`user_usage_scope = 'normal'`
- Master 页：`billing_scope IN ('org_only', 'org_and_user')`

## 快速开始

```bash
cp .env.example .env
# 编辑 .env 填入 DATABASE_DSN
make build
./bin/query-service
```

## 环境变量

| 变量 | 必填 | 默认值 | 说明 |
|------|------|--------|------|
| `DATABASE_DSN` | 是 | — | PostgreSQL 连接字符串 |
| `LISTEN_ADDR` | 否 | `0.0.0.0:27310` | HTTP 监听地址 |
| `SERVICE_TOKEN` | 否 | — | API 认证 Bearer Token |
| `AIKEY_LOG_LEVEL` | 否 | `info` | 日志级别 |

## 项目结构

```
query-service/
├── cmd/main.go
├── config/config.go
├── internal/
│   ├── api/          # HTTP 处理器与路由
│   ├── usage/        # 领域类型、仓库接口、PostgreSQL 实现
│   └── shared/       # 数据库、响应工具、中间件
├── Makefile
└── .env.example
```

## 运行环境

- Go 1.26+、PostgreSQL 14+（与 collector-service 共用）
- 支持平台：macOS、Linux、Windows
