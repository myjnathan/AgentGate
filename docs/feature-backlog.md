# 功能借鉴清单（Feature Backlog）

开发 Loop 的工作队列。每项注明**来源**（市面上谁做了、值得借鉴什么）、
**我们的适配**（结合 AgentGate 的分级信任/数据分级做出的差异）、
**验收标准**。Loop 每轮取优先级最高的一条做完（见 development-loop.md）。

状态图例：`todo` 未开始 / `doing` 进行中 / `done` 已完成（注明版本与日期）。

## P0 — 网关可靠性与可观测（先补齐"能用"）

### F1 渠道健康、重试与熔断 `done`（2026-11-21）
- 来源：LiteLLM / Portkey 的 fallback + cooldown：上游连续失败自动冷却、按错误类型重试到备用部署。
- 适配：重试/切换只能在**密级不高于原渠道**的渠道间发生（数据分级约束优先于可用性）；熔断事件进审计。
- 验收：mock 上游模拟 5xx 时自动切换到同级备用渠道成功；连续失败 N 次后渠道进入冷却，冷却期内不再被选中；单元测试覆盖。
- 借鉴点（LiteLLM 官方文档 docs.litellm.ai/docs/router/learn/reliability，2026-11-21 查阅）：
  按错误类型区分可重试（5xx/429/超时）与不可重试（401/404/内容策略等 4xx）；
  `allowed_fails`（默认 3 次连续失败）+ `cooldown_time`（默认 60s）两把旋钮；
  冷却以部署为隔离粒度、冷却期内从所有选路中剔除、到期自动恢复；
  每次 fallback 逐跳上报路由事件（from/to/原因）。我们照此实现，但把 fallback
  限定在与首选渠道**同一 MaxDataLevel** 的渠道之间（LiteLLM 是跨模型组 fallback，
  我们不允许借故障转移之名把数据送到更高密级渠道），且成功会清零连续失败计数。
- 实现：`internal/gateway`（pickCandidates + 同级故障转移循环 + 熔断），
  `GET /api/channels` 返回 health/consecutive_failures/cooldown_until，
  管理台渠道表新增健康列；7 个新测试覆盖 5xx 转移、冷却进入/到期恢复、
  全渠道冷却 503、拒绝向高密级渠道升级、机密数据不降级、4xx 不重试不计数、成功清零。

### F2 用量与成本统计 `done`（2026-11-21）
- 来源：Portkey observability（按 key/用户/模型看 latency、tokens、cost）；LiteLLM spend tracking。
- 适配：统计维度加**部门、Agent 类型、数据密级、处置（放行/脱敏/拦截）**——这是企业安全复盘要看的口径，通用网关没有。
- 验收：`GET /api/stats` 返回按用户/部门/渠道/模型聚合的调用数与 tokens；管理台新增统计视图；数字与审计日志可对账。
- 借鉴点（LiteLLM 官方文档 docs.litellm.ai/docs/proxy/cost_tracking；Portkey Analytics 文档 portkey.ai/docs/product/observability/analytics，2026-11-21 查阅）：
  LiteLLM 按 key/user/team/model 记录 spend 日志并支持按用户与时间范围查询；
  Portkey Analytics 按请求/用户/错误/成本多维度聚合，任意维度分组出调用数、成本、平均 tokens 与成功率，
  并支持时间范围过滤。我们照此实现，但统计口径改为**直接聚合哈希链审计日志**（不另建统计表，
  数字与审计天然对账），维度换成企业口径（部门/Agent 类型/数据密级/处置），被拦截的调用
  计入调用数与 blocked（tokens 为 0，相当于 Portkey 的错误率视角）；金额暂不计入——
  渠道尚未配置单价，计价与预算留给 F13。
- 实现：`audit.Log.All()` 快照；`internal/api/stats.go`（`GET /api/stats`，admin/security 可见，
  `since`/`until` RFC3339 半开区间过滤，非法时间返回 400）；聚合维度 by_user/by_department/
  by_channel/by_model/by_agent_type/by_data_level/by_decision，桶内含 calls/tokens/blocked，
  按调用数降序、key 升序；管理台新增用量统计卡片（总调用/tokens/拦截 + 按部门/模型/渠道表 + 处置汇总，
  中英双语）；冒烟新增 2 项断言（统计与审计对账、员工 403）。
- 测试：纯聚合测试（维度、拦截、范围外事件、半开时间窗口、空输入）+ HTTP 端点测试
  （鉴权 401/403、非法时间 400、未来窗口 0 调用），`gofmt`/`go vet`/`go test ./...` 全绿，
  `make smoke` 15 项断言 0 失败。

### F3 令牌级限流（RPM/TPM） `done`（2026-11-21）
- 来源：LiteLLM per-key rate limit、Kong AI Gateway 限流插件。
- 适配：限流按令牌配置，超限返回 429 并审计；受管级默认更宽松、自带级更严。
- 验收：配置 RPM=5 后第 6 次/分钟请求被拒；窗口滑动正确；测试覆盖。
- 借鉴点（LiteLLM 官方文档 docs.litellm.ai/docs/proxy/dynamic_rate_limit，2026-11-21 查阅）：
  每个虚拟 key 可配 `rpm_limit`/`tpm_limit`，在 `/key/generate` 时设定；超限返回 429
  throttling_error；tokens 在响应完成后才记入计数器——已在途的请求不会因刚耗尽的
  预算被拦，限流在已记录用量达到 TPM 后的下一请求才触发；窗口为 60 秒滚动。
  我们照此实现：60 秒滑动窗口、429 + `Retry-After`、tokens 响应后记账；差异是
  限流键为 AgentGate 自有令牌 ID、限流值可按令牌单独改（`PATCH /api/tokens/{id}`），
  未配的令牌走等级默认（受管级 60 RPM / 100000 TPM，自带级 20 RPM / 20000 TPM），
  且限流发生在鉴权后、防护管线前——被防护拦截的调用同样计 RPM（已占用网关处理）。
- 实现：新包 `internal/ratelimit`（60s 滑动窗口，RPM 计请求、TPM 计响应后上报的 tokens，
  0 = 不限该维）；`store.APIToken` 增加 `rpm_limit`/`tpm_limit`（0 = 走等级默认）；
  网关在鉴权后立即检查，超限 429 `rate_limit_exceeded` 并写 `rate_limit` 审计事件
  （不含请求原文，Kind 为 rate_limit 故不计入 F2 用量统计）；`GET /api/tokens` +
  `PATCH /api/tokens/{id}`（仅 admin/security，不暴露令牌哈希）；管理台新增令牌限流卡片
  （中英双语，可改 RPM/TPM）；冒烟新增 2 项断言（RPM 配置生效、第 6 次调用 429）。
- 测试：ratelimit 单元测试（RPM 边界、窗口滑动、TPM 响应后记账、0 不限、Retry-After 递减、
  200 并发恰好放行 100）+ 网关集成测试（RPM 429 + Retry-After + 审计事件、TPM 429、
  限流按令牌隔离、等级默认 BYOA 20 RPM）+ API 端点测试（列表/修改/部分修改/负值 400/
  未知令牌 404/员工 403、不暴露哈希）；`gofmt`/`go vet`/`go test ./...` 全绿，
  `make smoke` 17 项断言 0 失败。

### F4 审计导出与合规归档 `done`（2026-11-21）
- 来源：幻城网安 new-api 分叉的日志合规归档（热存 + NDJSON 压缩归档 + 保留期）；LiteLLM Enterprise 审计保留策略。
- 适配：导出带哈希链校验信息，归档文件可离线验真。
- 验收：管理台/API 可按时间段导出 NDJSON；导出包附 chain 校验结果；文档说明保留期配置。
- 借鉴点（LiteLLM 官方文档 docs.litellm.ai/docs/proxy/audit_logs；Cloudflare Logpull 文档 developers.cloudflare.com/logs/logpull，2026-11-21 查阅）：
  LiteLLM 审计日志带合规方设定的保留策略，可异步批量导出到外部存储（S3，按日期分目录的 JSON 文件），
  审计日志可用独立的合规存储桶；Cloudflare Logpull 以 NDJSON 为标准导出格式（每行一个有效 JSON 对象），
  按时间范围拉取、保留期到期后改用推送归档。我们照此实现，但导出不走外部存储：本机 NDJSON +
  响应头校验（`X-Audit-Chain-Valid` / `X-Audit-Event-Count` / `X-Audit-First-Seq` / `X-Audit-Last-Seq` /
  `X-Audit-Head-Hash`），离线可验真；归档为 gzip NDJSON + JSON 清单，保留期以环境变量配置、手动/定时触发。
- 实现：`internal/audit` 新增 Export（半开时间窗口 + kind/actor 过滤，整链验真进摘要）与 Archive
  （链破损时 409 拒绝；归档事件写 `audit-archive-<seq>-<seq>.jsonl.gz` + `.manifest.json` 清单；
  热日志以 `audit_archive_checkpoint` 检查点事件开头、检查点记录被归档段末哈希，热链仍可验真、
  归档+热日志跳过检查点行可离线验整链；`AGENTGATE_AUDIT_RETENTION_DAYS` 默认 180 天）；
  `GET /api/audit/export`（仅 admin/security，NDJSON 附件下载，导出本身记审计事件）+
  `POST /api/audit/archive`（仅 admin/security）；管理台审计卡片新增导出（日期范围）与归档按钮（中英双语）；
  详见 docs/audit-retention.md。
- 测试：audit 包 8 项新测试（导出全量/过滤/空窗/破链仍上报、归档移出+清单+检查点+离线整链验真、
  空归档 no-op、破链拒绝归档、两次归档后链仍有效）+ API 端点测试（鉴权 401/403、非法时间 400、
  导出头与行数一致、归档文件落盘、归档后链仍有效）+ 冒烟新增 3 项断言（导出 NDJSON 与头、
  归档 no-op、导出/归档后链仍有效）；`gofmt`/`go vet`/`go test ./...` 全绿，`make smoke` 20 项断言 0 失败。

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
