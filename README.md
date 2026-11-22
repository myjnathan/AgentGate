# AgentGate

[![CI](https://github.com/myjnathan/AgentGate/actions/workflows/ci.yml/badge.svg)](https://github.com/myjnathan/AgentGate/actions/workflows/ci.yml)

**员工的 Agent 随便换，企业的能力、权限和账本不动。**
*Your employees' agents may change. Your company's capabilities, permissions and audit trail don't.*

AgentGate 是一个**不绑定任何单一桌面 Agent** 的企业级平台（源码公开、自托管）：

- 企业把系统、知识、模型与能力沉淀在平台侧；
- 员工用自己习惯的通用桌面 Agent（DSH、WorkBuddy、Claude Code、Codex 等），经 **OpenAI 兼容协议 + 远程 MCP** 接入；
- 涉及企业加密文件时，只有通过准入标准的**受管 Agent** 能在预防型防护下读取明文——进程白名单 + 出口守卫 + 数据分级路由，而不是事后审计了事。

> 状态：早期开发中（v0.1 骨架已跑通核心闭环）。设计文档见 [docs/blueprint.md](docs/blueprint.md) 与 [docs/enterprise-protection-design.md](docs/enterprise-protection-design.md)。

## 核心能力（已实现）

| 模块 | 说明 |
|---|---|
| 模型网关 `/v1` | OpenAI 兼容；多渠道、按**数据密级**选路（密级不够的渠道根本收不到数据）；渠道带**信任类型**（签约云 / 专有云-VPC / 本地小模型）：机密默认脱敏后上云、绝密仅走本地模型、无合规通道时明确拦截；月度配额；上游 5xx/429 自动同级故障转移，连续失败 3 次熔断冷却 60s（分级约束高于可用性，绝不向更高密级渠道升级）；令牌级 RPM/TPM 限流，超限 429 + `Retry-After`；精确匹配响应缓存（仅公开/内部、无敏感内容的非流式回答，命中不计上游用量，`X-AgentGate-Cache: HIT/MISS`，管理台可关）；渠道可配多把上游密钥（上限 10 把），按轮询分摊调用，上游 401 时自动摘除该 key 并告警，同一请求用下一 key 继续（管理台可增/停用/恢复/删除） |
| 防护管线 | 敏感词（Aho-Corasick）→ DLP 检测（凭据/PII/企业自定义词）→ 数据分级 → 拦截 / 脱敏（可逆占位符，响应还原）/ 分级路由；`tool_calls` 参数与正文同权过管线；流式响应同样还原占位符（分片切断的占位符经缓冲还原，不会半截泄漏） |
| Agent 分级信任 | 受管级 / 自带级分级令牌；自带级禁止发送机密数据；设备码登录（`ag-setup` 一条命令接入）；企业 OIDC 登录（IdP 只认身份不授权限，新用户一律 employee，本地账号保留兜底，见 [docs/oidc-sso.md](docs/oidc-sso.md)）；Agent 证明（受管 Agent 登录时上报版本/配置哈希/插件清单，平台校验后才发受管级令牌，不符降级为自带级并告警，见 [docs/attestation.md](docs/attestation.md)） |
| MCP 网关 `/mcp` | 远程 MCP，工具级授权（如 kb_get 仅受管级），工具返回过 DLP，每次调用审计；上游 MCP 服务器可联邦进来，其工具以 `命名空间.工具名` 出现在 tools/list 中；存量 REST 接口可按「方法 + 路径模板 + 参数 schema」注册为工具，以 `rest.工具名` 出现，调用前做参数 schema 校验、等级授权与 DLP（凭据服务端加密托管） |
| 知识库 | 关键词多轮检索 + 读原文 + 引用，不用向量库；按等级控制可见范围；共享文件夹即知识库（指定文件夹自动索引 .md/.txt/.docx/.pdf，按文件夹映射密级，增量更新；Agent 按「目录 → 章节树 → 章节原文」渐进式取用，引用精确到文件 + 章节 + 页码，见 [docs/folder-kb.md](docs/folder-kb.md)） |
| 能力中枢 | 技能（SKILL.md）提交 → 审核 → 签名校验和 → 按部门范围下发；Prompt 模板同样走提交 → 审核 → 发布，版本不可变，Agent 经 `/mcp` `prompts/get` 只读已发布版（参数与渲染结果均过 DLP） |
| 审计 | 哈希链防篡改审计日志，可验证完整性；原文与敏感值不落日志；`GET /api/audit/export` 按时间段导出 NDJSON（响应头附链校验结果），`POST /api/audit/archive` 把超保留期的事件归档为 gzip + 清单（默认保留 180 天，见 [docs/audit-retention.md](docs/audit-retention.md)） |
| 用量统计 | `GET /api/stats` 按用户/部门/渠道/模型/Agent 类型/数据密级/处置聚合调用数与 tokens（被拦截调用计入 blocked）；直接聚合审计日志，数字与审计天然对账；支持 `since`/`until` 时间范围；每次调用按渠道配置的模型单价（输入/输出每百万 Token，USD）折算成本记进审计，统计含 `cost_usd` 与未定价调用数（见 [docs/cost-budgets.md](docs/cost-budgets.md)） |
| 部门预算 | 部门月度预算硬上限（自然月 UTC）：本月花费达到上限后网关以 `429 department_budget_exceeded` 拒绝后续调用；花费达告警阈值时每月发一次提醒（见 [docs/cost-budgets.md](docs/cost-budgets.md)） |
| 告警 Webhook | 拦截、密级越权、渠道熔断、账号锁定、Agent 证明降级触发结构化告警，可推送到企业微信/钉钉/飞书机器人与通用 Webhook；签名可选（通用：`X-AgentGate-Signature` HMAC 头；企微/钉钉/飞书：`timestamp` + `sign` 查询参数）；投递失败最多重试 3 次后放弃并记审计；管理台可配、可测 |
| 可观测性 | 网关与 MCP 调用产生脱敏的 trace 与指标（OTLP/HTTP JSON），默认关闭；设置 `OTEL_EXPORTER_OTLP_ENDPOINT`（如 `http://localhost:4318`）后导出到 Jaeger / Tempo / Phoenix 等后端；Span 只含模型、渠道、密级、处置、Token 数等元数据，绝不含提示词、工具参数或模型回复；管理台可查看导出状态并一键测试连通性（见 [docs/opentelemetry.md](docs/opentelemetry.md)） |
| 管理台 `/` | 内嵌 Web UI（中英双语），渠道/健康状态/用量统计/审计/设备批准/令牌限流/告警 Webhook/MCP 联邦上游/REST → MCP 工具/文件夹知识库配置 |

## 快速开始（PoC 演示模式）

需要 Go 1.27+。

```bash
go build -o agentgate ./cmd/agentgate
AGENTGATE_MASTER_KEY=change-me-please-16+ \
AGENTGATE_DEMO=1 \
./agentgate
# 打开 http://127.0.0.1:8787
# 管理员 admin / agentgate-admin；员工 lixiaofeng / demo12345（模拟公司「星河精密」）
```

演示模式内置一个 mock 模型上游，无需任何真实 API Key 即可体验完整闭环：
普通问答放行、自带级 Agent 发送机密词（XH-200）被拦截、受管级机密请求默认脱敏后上云（原文不出公司、响应还原）、绝密请求走本地模型或被明确拦截、PII 自动脱敏并在响应中还原、知识库带引用检索、审计链可验证。

员工接入（另一终端）：

```bash
go build -o ag-setup ./cmd/ag-setup
./ag-setup --server http://127.0.0.1:8787 --agent dsh --tier byoa
# 按提示在网页端批准设备码即可
```

## 架构一瞥

```text
桌面 Agent（任意） ──► 本地隐私网关 ag-guard（端侧检测/脱敏/还原，见 [docs/local-gateway-sidecar.md](docs/local-gateway-sidecar.md)）──► 企业网关（防护管线/分级路由）
                                                    ├─► 签约云模型（低密级）
                                                    ├─► 脱敏后上云（机密默认）/ 专有云-VPC（可选）/ 本地小模型（绝密）
                                                    └─► MCP 工具 / 知识库 / 企业系统
受管 Agent ── 进程白名单（透明加密明文）+ 进程级出口守卫（只准连企业网关）
```

## 与同类项目的区别

- vs **dsh-ee**：它绑定 DSH；AgentGate 不绑任何 Agent，且把 DLP/分级路由/分级信任做成核心。
- vs **New API**：它只管模型流量；AgentGate 延伸到端侧、能力侧与加密文件通道，密钥加密存储不落明文。
- vs **WorkBuddy 企业版**：它绑自家 Agent 与生态、闭源靠厂商承诺；AgentGate 源码公开可审计、中立、自托管。SaaS 型 Agent 自带网盘/同步导致“只能审计、防不住”的结构性问题，见设计文档 §2.1 的准入标准。

## 开发

```bash
go vet ./...
go test ./...
```

仓库结构：`server` 逻辑在 `internal/`，端侧工具在 `cmd/ag-setup`（接入）与 `cmd/ag-guard`（本地隐私网关 sidecar），出口守卫模板在 `endpoint/egress/`，受管 Agent 说明在 `managed-agent/`，部署脚本在 `deploy/`。

## License

**GNU AGPL-3.0** — 见 [LICENSE](LICENSE)。

- 任何人可免费使用，包括商业使用；修改后分发、或以网络服务形式向他人提供时，须以 AGPL-3.0 公开对应源代码（防止闭源转卖）。
- 本项目的目标是普及：欢迎企业、组织与个人直接部署使用，无需另行授权；若你修改了代码并对外分发或提供网络服务，按 AGPL-3.0 公开你的修改即可。
