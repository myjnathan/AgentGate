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
| 防护管线 | 敏感词（Aho-Corasick）→ DLP 检测（凭据/PII/企业自定义词）→ 数据分级 → 拦截 / 脱敏（可逆占位符，响应还原）/ 分级路由；检测前先做归一化（全角/大小写/分隔符/零宽字符），`机 密`、`ＸＨ－２００`、`138-0000-0000` 这类变形内容仍能命中，只增命中、不减命中（见 [docs/dlp-normalization.md](docs/dlp-normalization.md)）；`tool_calls` 参数与正文同权过管线；流式响应同样还原占位符（分片切断的占位符经缓冲还原，不会半截泄漏）；规则 DLP 之上再加可选的语义复核层：本地样本匹配识别越狱 / 诱导 / 伪装 / 拆分外发等风险意图，可选深层调用自托管审核小模型（Qwen3Guard 式），绝密永不送审、复核先脱敏再送审，关闭后行为与此前完全一致（见 [docs/semantic-review.md](docs/semantic-review.md)） |
| Agent 分级信任 | 受管级 / 自带级分级令牌；自带级禁止发送机密数据；设备码登录（`ag-setup` 一条命令接入）；企业 OIDC 登录（IdP 只认身份不授权限，新用户一律 employee，本地账号保留兜底，见 [docs/oidc-sso.md](docs/oidc-sso.md)）；Agent 证明（受管 Agent 登录时上报版本/配置哈希/插件清单，平台校验后才发受管级令牌，不符降级为自带级并告警，见 [docs/attestation.md](docs/attestation.md)）；终端持续盘点（ag-guard 每 60 秒心跳上报版本/配置哈希/插件清单，管理台「终端清单」页按部门/版本/在线状态筛选、配置漂移标红并告警；网关断网时 ag-guard 按缓存策略执行、高密级默认拦截，恢复后补报断网审计，见 [docs/terminal-inventory.md](docs/terminal-inventory.md)）；高密级调用例外审批（员工申请、管理员审批后发一次性放行凭证，仅限一次调用，违禁词拦截永不例外，绝密默认不可例外，见 [docs/exception-approval.md](docs/exception-approval.md)） |
| 安装包分发 | 管理员上传或离线导入受管 Agent 安装包，平台托管、员工自助下载；SHA-256 校验并区分已校验 / 未校验，导入中断可断点续传，过期包一键清理；分发包的哈希可登记为 Agent 证明的基准值（未校验包不可登记）；双包结构：管理包（agentgate 服务端，`make bundle`）与用户包（ag-guard + ag-setup，按策略生成，`make bundle-user` 或管理台生成，见 [docs/package-distribution.md](docs/package-distribution.md)） |
| 组织同步 | 从钉钉 / 飞书 / 企业微信同步部门与员工目录（与 OIDC 并行）；同步只建 / 更新账号与部门映射，不授予受管级权限；邮箱自动关联已有账号，本地已禁用不自动恢复，目录里没看到 ≠ 离职（只有平台明确说找不到时才禁用并吊销会话）；第三方身份可手动绑定（须以同步快照为准），应用密钥只加密存储、不回显（见 [docs/org-sync.md](docs/org-sync.md)） |
| 分级管理员 | 平台管理员 / 安全管理员 / 审计员 / 运维员 / 员工五档职责分离（等保三权分立思路）：审计员只读审计与报表、运维员只做渠道与令牌运维、不可看审计；角色分配只由平台管理员操作，改角色、批例外、导审计均要求对应角色并全程留痕；OIDC 组映射可把 IdP 组映射为审计员 / 运维员（admin 与 security 永不经 IdP 授予，本地改过角色的账号不再被映射覆盖）；管理台「用户与角色」页可查角色分工与用户列表（见 [docs/roles.md](docs/roles.md)） |
| MCP 网关 `/mcp` | 远程 MCP，工具级授权（如 kb_get 仅受管级），工具返回过 DLP，每次调用审计；上游 MCP 服务器可联邦进来，其工具以 `命名空间.工具名` 出现在 tools/list 中；存量 REST 接口可按「方法 + 路径模板 + 参数 schema」注册为工具，以 `rest.工具名` 出现，调用前做参数 schema 校验、等级授权与 DLP（凭据服务端加密托管） |
| 知识库 | 关键词多轮检索 + 读原文 + 引用，不用向量库；按等级控制可见范围；共享文件夹即知识库（指定文件夹自动索引 .md/.txt/.docx/.pdf，按文件夹映射密级，增量更新；Agent 按「目录 → 语义条目 → 章节树 → 章节原文」渐进式取用，语义条目为每章节一行 FRAS（职责 / 关联 / 适用范围 / 硬约束）加标签坐标、每条带原文回指，见 [docs/semantic-index.md](docs/semantic-index.md)；引用精确到文件 + 章节 + 页码，见 [docs/folder-kb.md](docs/folder-kb.md)）；专题知识包是其上的策展层：围绕一个工作问题精选文件成包（来源清单 + 综合说明），审批发布后 Agent 经 MCP `kb_pack_list` / `kb_pack_get` 按包取用，综合中每条结论带原文回指，引用只能向下，见 [docs/knowledge-packages.md](docs/knowledge-packages.md) |
| 能力中枢 | 技能（SKILL.md）提交 → 审核 → 签名校验和 → 按部门范围下发；Prompt 模板同样走提交 → 审核 → 发布，版本不可变，Agent 经 `/mcp` `prompts/get` 只读已发布版（参数与渲染结果均过 DLP）；专题知识包同样走起草 → 审核 → 发布，版本不可变，Agent 经 `/mcp` `kb_pack_list` / `kb_pack_get` 只读已发布版（综合与引用综合均过 DLP） |
| 审计 | 哈希链防篡改审计日志，可验证完整性；原文与敏感值不落日志；`GET /api/audit/export` 按时间段导出 NDJSON（响应头附链校验结果），`POST /api/audit/archive` 把超保留期的事件归档为 gzip + 清单（默认保留 180 天，见 [docs/audit-retention.md](docs/audit-retention.md)） |
| 用量统计 | `GET /api/stats` 按用户/部门/渠道/模型/Agent 类型/数据密级/处置聚合调用数与 tokens（被拦截调用计入 blocked）；直接聚合审计日志，数字与审计天然对账；支持 `since`/`until` 时间范围；每次调用按渠道配置的模型单价（输入/输出每百万 Token，USD）折算成本记进审计，统计含 `cost_usd` 与未定价调用数（见 [docs/cost-budgets.md](docs/cost-budgets.md)） |
| 合规报表 | 按周 / 月生成周期报表：调用量与 Token、拦截与脱敏统计、Top 风险事件、渠道健康、令牌与终端概况；管理台可预览、导出 CSV 表格；与告警构成「审计有人看」闭环。报表只含统计与元数据，不含敏感原文；报表是审计证据，不是合规认证。最多保留 52 份（一年周报量），见 [docs/compliance-reports.md](docs/compliance-reports.md) |
| 部门预算 | 部门月度预算硬上限（自然月 UTC）：本月花费达到上限后网关以 `429 department_budget_exceeded` 拒绝后续调用；花费达告警阈值时每月发一次提醒（见 [docs/cost-budgets.md](docs/cost-budgets.md)） |
| 告警 Webhook | 拦截、密级越权、渠道熔断、账号锁定、Agent 证明降级触发结构化告警，可推送到企业微信/钉钉/飞书机器人与通用 Webhook；签名可选（通用：`X-AgentGate-Signature` HMAC 头；企微/钉钉/飞书：`timestamp` + `sign` 查询参数）；投递失败最多重试 3 次后放弃并记审计；管理台可配、可测 |
| 可观测性 | 网关与 MCP 调用产生脱敏的 trace 与指标（OTLP/HTTP JSON），默认关闭；设置 `OTEL_EXPORTER_OTLP_ENDPOINT`（如 `http://localhost:4318`）后导出到 Jaeger / Tempo / Phoenix 等后端；Span 只含模型、渠道、密级、处置、Token 数等元数据，绝不含提示词、工具参数或模型回复；管理台可查看导出状态并一键测试连通性（见 [docs/opentelemetry.md](docs/opentelemetry.md)） |
| 管理台 `/` | 内嵌 Web UI（中英双语），渠道/健康状态/用量统计/审计/合规报表/设备批准/终端清单/例外审批/令牌限流/告警 Webhook/MCP 联邦上游/REST → MCP 工具/文件夹知识库/专题知识包/安装包分发/组织同步配置/用户与角色/语义复核 |

## 快速开始（PoC 演示模式）

需要 Go 1.27+。

```bash
go build -o agentgate ./cmd/agentgate
AGENTGATE_MASTER_KEY=change-me-please-16+ \
AGENTGATE_DEMO=1 \
./agentgate
# 打开 http://127.0.0.1:8787
# 管理员 admin / agentgate-admin；审计员 auditor / demo12345；运维员 ops / demo12345；员工 lixiaofeng / demo12345（模拟公司「星河精密」）
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
