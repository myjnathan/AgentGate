# OpenTelemetry 导出（F16）

网关与 MCP 调用产生**脱敏的 trace 与指标**，以 OTLP/HTTP JSON 导出到
Jaeger、Tempo、Arize Phoenix 或任何 OTLP 兼容后端。默认关闭；导出失败
永远不影响请求处理（遥测是尽力而为的，见下）。

## 开启

```bash
# Jaeger（OTLP/HTTP 在 4318）：
docker run -p 16686:16686 -p 4318:4318 jaegertracing/all-in-one:latest
# Phoenix（LLM 可观测）：
docker run -p 6006:6006 -p 4318:4318 arizephoenix/phoenix:latest

OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 \
OTEL_SERVICE_NAME=agentgate \
./agentgate
```

环境变量（与 ContextForge 的 OTel 配置同名，方便迁移）：

| 变量 | 说明 | 默认 |
|---|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | OTLP/HTTP collector 地址，如 `http://localhost:4318`；**不设 = 不导出** | — |
| `OTEL_SERVICE_NAME` | `service.name` 资源属性 | `agentgate` |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | 只支持 `http/json`；设为 `grpc`/`http/protobuf` 时导出关闭并记一条日志（网关照常启动） | — |

实现是纯标准库的 OTLP/HTTP JSON 导出器（`internal/otel`），不拉
OpenTelemetry Go SDK：本项目坚持零依赖单二进制（与 F9 不拉 oauth2/oidc
依赖的取舍一致），而 OTLP/HTTP JSON 是已稳定的公开协议（POST
`/v1/traces` 与 `/v1/metrics`，`Content-Type: application/json`），
任何合规后端都能直接接收。导出器 5 秒批量导出一次（或满 100 个 span
时提前导出）；队列上限 1000，满了丢弃并计数。

## 产生什么

### Span（每调用一个）

| Span | 说明 |
|---|---|
| `gateway.chat_completions` | 每次 `/v1/chat/completions` 调用（含限流/预算/拦截等提前拒绝） |
| `mcp.tools.call` | 每次 MCP `tools/call` |
| `mcp.prompts.get` | 每次 MCP `prompts/get` |

`tools/list` 等只读元数据操作不产生 span。

属性（全部是元数据，见下一节红线）：

| 属性 | 说明 |
|---|---|
| `agentgate.actor_id` | 调用者用户 ID（不含用户名） |
| `agentgate.agent_type` / `agentgate.tier` / `agentgate.department` | 调用者身份 |
| `agentgate.model` | 请求的模型 |
| `agentgate.channel` | 实际服务的渠道 |
| `agentgate.data_level` | 判定出的数据密级（public/internal/confidential/secret） |
| `agentgate.decision` | 处置：allow / warn / redact / reroute / block / error |
| `agentgate.tokens_total` | 本次调用的 token 数 |
| `agentgate.cache` | `hit` / `miss`（仅缓存相关调用） |
| `agentgate.sidecar` | 经 ag-guard 到达时为 `ag-guard/<版本>` |
| `agentgate.tool` | MCP 工具名 |
| `agentgate.rpc_error_code` | MCP 调用失败时的 JSON-RPC 错误码 |

Span 状态：HTTP ≥ 400 或 JSON-RPC 含 error 时为 ERROR，否则 OK。
状态消息只放短类别码（如 `Forbidden`、`rpc_error`），不放任何内容。

### Metrics（累计 Sum，每次导出时上报）

| 指标 | 标签 | 说明 |
|---|---|---|
| `agentgate.gateway.calls` | `decision` | 网关调用数 |
| `agentgate.gateway.tokens` | `channel` | 网关 token 数 |
| `agentgate.mcp.tool_calls` | `tool`, `decision` | MCP 工具调用数 |
| `agentgate.mcp.prompt_gets` | `decision` | MCP 模板读取数 |

### 分布式追踪

- 入站 `traceparent`（W3C）有效时，本次调用加入该 trace（span 的
  traceId 与 parentSpanId 即入站值），Agent 发起的整条链路可在后端
  看到。
- 出站：网关向模型上游转发时带上本次 span 的 `traceparent`。

## 红线：什么绝不进 Span

- 提示词正文、tool_calls 参数、模型响应原文；
- DLP 命中的敏感值（只记录处置类别，不记录匹配到的词本身）；
- 密钥、令牌、用户密码。

实现上由调用点保证：`internal/gateway` 与 `internal/mcp` 只传上表
列出的字段，`internal/otel` 的 `Attr` 只接受 string/int64/float64/
bool。集成测试用标志字符串断言请求内容不会出现在导出的 payload
中（见 `internal/gateway/gateway_otel_test.go` 与
`internal/mcp/mcp_otel_test.go`）。

## 管理台与 API

- 管理台「用量统计」页新增 OpenTelemetry 卡片：是否启用、collector
  地址、已导出/丢弃/失败的 span 数、最近错误，及「测试」按钮。
- `GET /api/otel`（admin/security）：导出状态与计数器。
- `POST /api/otel/test`（admin/security）：发一个合成 span
  （`agentgate.test`，不含任何请求数据）并同步导出，立即验证
  collector 是否接受；未启用时 503，导出失败时 502。

## 已知限制

- 只支持 OTLP/HTTP JSON，不支持 gRPC（4317）与 `http/protobuf`：
  需要 gRPC 时可在中间放一个 OpenTelemetry Collector 做协议转换。
- 导出失败的 span 直接丢弃、不重试、不落盘：遥测不值得为一次
  失败阻塞网关或无限增长；失败次数与最近错误可见于 `GET /api/otel`。
- 指标只有上述四个累计计数器，没有直方图（延迟分布）与日志信号；
  需要时可再接 OTLP logs（`/v1/logs`）或在后端由 span 时长聚合。
- 不做采样：调用量不大时全量导出；高并发场景下队列满了会丢弃
  （计数可见），采样留待后续轮次评估。
