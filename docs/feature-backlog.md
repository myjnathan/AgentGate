# 功能借鉴清单（Feature Backlog）

开发 Loop 的工作队列。每项注明**来源**（市面上谁做了、值得借鉴什么）、
**我们的适配**（结合 AgentGate 的分级信任/数据分级做出的差异）、
**验收标准**。Loop 每轮取优先级最高的一条做完（见 development-loop.md）。

状态图例：`todo` 未开始 / `doing` 进行中 / `done` 已完成（注明版本与日期）。

## P0 — 网关可靠性与可观测（先补齐"能用"）

### F1 渠道健康、重试与熔断 `todo`
- 来源：LiteLLM / Portkey 的 fallback + cooldown：上游连续失败自动冷却、按错误类型重试到备用部署。
- 适配：重试/切换只能在**密级不高于原渠道**的渠道间发生（数据分级约束优先于可用性）；熔断事件进审计。
- 验收：mock 上游模拟 5xx 时自动切换到同级备用渠道成功；连续失败 N 次后渠道进入冷却，冷却期内不再被选中；单元测试覆盖。

### F2 用量与成本统计 `todo`
- 来源：Portkey observability（按 key/用户/模型看 latency、tokens、cost）；LiteLLM spend tracking。
- 适配：统计维度加**部门、Agent 类型、数据密级、处置（放行/脱敏/拦截）**——这是企业安全复盘要看的口径，通用网关没有。
- 验收：`GET /api/stats` 返回按用户/部门/渠道/模型聚合的调用数与 tokens；管理台新增统计视图；数字与审计日志可对账。

### F3 令牌级限流（RPM/TPM） `todo`
- 来源：LiteLLM per-key rate limit、Kong AI Gateway 限流插件。
- 适配：限流按令牌配置，超限返回 429 并审计；受管级默认更宽松、自带级更严。
- 验收：配置 RPM=5 后第 6 次/分钟请求被拒；窗口滑动正确；测试覆盖。

### F4 审计导出与合规归档 `todo`
- 来源：幻城网安 new-api 分叉的日志合规归档（热存 + NDJSON 压缩归档 + 保留期）；LiteLLM Enterprise 审计保留策略。
- 适配：导出带哈希链校验信息，归档文件可离线验真。
- 验收：管理台/API 可按时间段导出 NDJSON；导出包附 chain 校验结果；文档说明保留期配置。

### F5 Webhook 告警 `todo`
- 来源：企业安全运营刚需（各家 SIEM/告警集成）；WorkBuddy 企业版的安全事件通知。
- 适配：拦截、密级越权、熔断、登录锁定等事件可推送到企业微信/钉钉/飞书机器人与通用 Webhook；告警内容脱敏（不带原文）。
- 验收：触发一次拦截后测试 Webhook 收到结构化告警；签名校验可选；失败重试 3 次后放弃并记录。

## P1 — 能力侧扩展（MCP 联邦与企业接入）

### F6 MCP 联邦代理 `todo`
- 来源：IBM ContextForge：把多个上游 MCP Server 联邦到一个端点，工具名加命名空间前缀防冲突，统一鉴权与限流。
- 适配：上游工具继承平台的工具级授权与密级标注；上游凭据服务端加密托管（dsh-ee 模式）；工具出入参过 DLP。
- 验收：注册一个上游 MCP（如本地测试 server）后，其工具以 `upstream.tool` 形式出现在 tools/list 并可调用；未授权等级调用被拒。

### F7 REST → MCP 适配 `todo`
- 来源：ContextForge 的 REST-to-MCP（按 OpenAPI/手工映射把存量 REST API 变成工具）。
- 适配：映射配置里声明工具密级与最低 Agent 等级；请求头可注入托管凭据。
- 验收：把一个示例 REST 接口注册为工具并成功调用；参数 schema 校验生效。

### F8 Prompt 模板库 `todo`
- 来源：ContextForge Prompt Registry（模板版本管理 + 回滚）、Portkey prompt management。
- 适配：模板按部门范围下发，作为一种新能力类型进能力中枢（与技能同流水线：提交→审核→发布）。
- 验收：管理员可建/改/回滚模板版本；Agent 经 MCP 读取被授权的模板。

### F9 OIDC / LDAP 登录 `todo`
- 来源：dsh-ee、LiteLLM Enterprise（SSO/SCIM 为其付费墙功能，我们开源实现即差异化）。
- 适配：本地账号保留为兜底；OIDC 用户自动映射部门与角色。
- 验收：对接一个公开 OIDC 测试 IdP 完成登录闭环；LDAP 以文档 + 配置预留（无环境时标注未实测）。

### F10 流式脱敏与 tool_calls 防护（探针 3） `todo`
- 来源：蓝图探针 3；AstrLink 的流式占位符处理思路。
- 适配：SSE 分片跨界占位符缓冲还原；请求中 tool_calls 参数同样过 DLP 与分级。
- 验收：构造占位符被分片切断的用例仍正确还原；tool 参数含机密词时按等级拦截/脱敏；测试全绿。

## P2 — 进阶与差异化

### F11 精确匹配响应缓存 `todo`
- 来源：Cloudflare AI Gateway / Portkey cache。
- 适配：仅 public/internal 且无 DLP 命中时可缓存；缓存键含渠道与密级；管理台可关。
- 验收：相同请求第二次命中缓存不计上游用量；含敏感内容永不缓存（测试证明）。

### F12 渠道多 Key 与轮换 `todo`
- 来源：New API 渠道多 key 轮询、LiteLLM key rotation。
- 适配：key 仍加密托管；轮换不中断在途请求；失效 key 自动摘除并告警（联动 F1/F5）。

### F13 成本计价与部门预算 `todo`
- 来源：LiteLLM budgets（硬顶 429）、Portkey per-key budget。
- 适配：渠道配置模型单价，统计折算金额（联动 F2）；部门月度预算超限拦截。

### F14 Agent 证明（Attestation） `todo`
- 来源：蓝图既有设计；WorkBuddy 企业版的终端管控（iOA 类）思想。
- 适配：受管 Agent 登录时上报版本/配置哈希/插件清单，平台校验后才发受管级令牌；不符自动降级自带级 + 告警。

### F15 本地隐私网关 sidecar `todo`
- 来源：AstrLink 本地隐私网关（端侧检测与可逆脱敏、映射不出端）。
- 适配：ag-setup 配套的本地守护进程，受管级双层脱敏的端侧一层；与出口守卫联动。

### F16 OpenTelemetry 导出 `todo`
- 来源：ContextForge 的 OTel 可观测（Phoenix/Jaeger 等）。
- 适配：网关与 MCP 调用产生 trace/metric，字段脱敏后导出。

## 调研来源（持续更新）
- LiteLLM / Portkey / Kong / Cloudflare AI Gateway 对比（genaimaturity.net、devopsness.com，2026-11 查阅）
- IBM ContextForge（MCP 网关/注册中心，Apache-2.0，ibm.github.io/mcp-context-forge）
- New API 及其生态分叉（防护插件管线、合规归档）、AstrLink（端侧隐私网关）
- hikariming/dsh-ee（能力中枢闭环）、WorkBuddy 企业版安全白皮书（终端管控与审计）
