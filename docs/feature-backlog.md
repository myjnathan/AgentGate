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

### F5 Webhook 告警 `done`（2026-11-21）
- 来源：企业安全运营刚需（各家 SIEM/告警集成）；WorkBuddy 企业版的安全事件通知。
- 适配：拦截、密级越权、熔断、登录锁定等事件可推送到企业微信/钉钉/飞书机器人与通用 Webhook；告警内容脱敏（不带原文）。
- 验收：触发一次拦截后测试 Webhook 收到结构化告警；签名校验可选；失败重试 3 次后放弃并记录。
- 借鉴点（企业微信官方文档 developer.work.weixin.qq.com/document/path/91770；钉钉自定义机器人安全设置说明，2026-11-21 查阅）：
  企业微信群机器人支持 text/markdown 等 8 种消息类型，markdown 内容上限 4096 字节、仅支持标题/加粗/链接/行内代码/引用/三色字体子集，
  每机器人限 20 条/分钟；钉钉自定义机器人同样支持 markdown（title + text），同为 20 条/分钟、超限停 10 分钟（410100）。
  三家（企业微信/钉钉/飞书）加签是同一公式：待签串 `timestamp(毫秒) + "\n" + secret`，HMAC-SHA256（key 为 secret），
  Base64 后 URL 编码，拼进 Webhook URL 的 `timestamp` 与 `sign` 查询参数（secret 在算法里出现两次）。
  我们照此实现：generic 用 JSON 结构化告警 + `X-AgentGate-Signature: sha256=<hex HMAC>` 请求头；
  wecom/dingtalk/feishu 用 markdown/text 消息体 + 上述 query 参数加签；告警只取审计事件已有字段
  （处置类别如 banned_word、操作者、密级、渠道），审计 Detail 本就不含匹配原文，故告警天然脱敏；
  限流不做告警（量大、属软性控制），避免告警疲劳；投递失败最多重试 3 次后放弃并记审计事件。
- 实现：新包 `internal/alert`（`FromEvent` 分类 + 各平台载荷构建 + 签名 + 异步投递器，队列满丢弃计数，
  失败 1+3 次重试、错误只记 `http_500`/`network_error`/`timeout`/`config_error` 类别，绝不把可能含 bot key 的 URL 写进审计）；
  `audit.Log.SetNotifyHook`（锁外回调，`alert` 类事件不触发，避免自循环）；`store.Webhook`（签名密钥 AES-GCM 加密后持久化，
  与 `Channel.APIKeyEnc` 的 `json:"-"` 不同——Webhook 是运行时由管理员配置的，丢密钥等于丢配置）；
  `GET/POST /api/alerts/webhooks` + `DELETE /api/alerts/webhooks/{id}` + `POST /api/alerts/webhooks/{id}/test`
  （仅 admin/security，URL 列表展示时 query 值打码、密钥永不回显）；登录锁定在 `auth` 层以哨兵错误
  `ErrAccountLocked` 区分，API 记 `account_locked` 审计事件（可告警）而普通失败仍为 `login_failed`（不告警）；
  管理台新增告警 Webhook 卡片（列表/添加/测试/删除，中英双语）；冒烟新增 5 项断言。
- 测试：alert 包 13 项（事件分类、generic 载荷无敏感原文、三平台消息体形状、钉钉/企微/飞书签名 Python 独立向量、
  HMAC 请求头向量、签名 URL 保留原 query、markdown 4096 字节截断、首发成功、2 次失败后成功、4 次尝试后放弃、
  网络失败类别、异步投递跳过禁用项、钉钉签名投递）+ API 端点测试（鉴权 401/403、校验 400、密钥加密存储与不回显、
  URL 打码、测试端点成功/失败 502 + 4 次尝试、锁定审计为 account_locked）+ 网关集成测试（违禁词拦截→Webhook 收到结构化告警）；
  `gofmt`/`go vet`/`go test ./...` 全绿；`make smoke` 25 项断言 0 失败。

## P1 — 能力侧扩展（MCP 联邦与企业接入）

### F6 MCP 联邦代理 `done`（2026-11-21）
- 来源：IBM ContextForge：把多个上游 MCP Server 联邦到一个端点，工具名加命名空间前缀防冲突，统一鉴权与限流。
- 适配：上游工具继承平台的工具级授权与密级标注；上游凭据服务端加密托管（dsh-ee 模式）；工具出入参过 DLP。
- 验收：注册一个上游 MCP（如本地测试 server）后，其工具以 `upstream.tool` 形式出现在 tools/list 并可调用；未授权等级调用被拒。
- 借鉴点（ContextForge 官方文档 ibm.github.io/mcp-context-forge/architecture；PyPI mcp-contextforge-gateway，2026-11-21 查阅）：
  多个 MCP/REST 后端联邦到单一合规端点、注册中心合并各后端能力目录、虚拟服务器按需组合工具子集、
  定期健康检查与故障转移、工具带输入校验与重试/限流策略、插件式请求/响应钩子（含 PII 过滤）。
  我们照此实现，但收紧为企业治理语义：联邦只做一层代理（不做对等网关发现/mDNS、不做 Redis 同步，
  工具列表 60 秒缓存 + 失败回落旧缓存）；命名空间即工具前缀（`hr.leave_balance`），
  上游只配最高密级与最低等级两把闸（ContextForge 是通用注册中心，我们把授权收进平台分级体系）；
  凭据用主密钥 AES-GCM 加密托管、只在转发瞬间解密；出入参都过 DLP（ContextForge 的 PII 插件是可选项，
  我们是强制门禁）。
- 实现：新包 `internal/federation`（JSON-RPC 2.0 over HTTP 转发、60 秒工具列表缓存、失败回落旧缓存、
  上游错误不透传原文）；`store.UpstreamServer`（命名空间/URL/加密凭据/最高密级/最低等级）；
  `/mcp` 的 tools/list 合并本地与联邦工具（高于调用者等级的上游对该调用者隐藏，不可达上游静默缺席）、
  tools/call 对命名空间工具先做等级授权、再对参数做 DLP（违禁词或密级高于上游上限直接拦截、参数永不外发），
  结果复用本地工具的 DLP 扫描与自带级脱敏规则；`GET/POST /api/mcp/upstreams` +
  `DELETE /api/mcp/upstreams/{id}` + `POST /api/mcp/upstreams/{id}/test`（仅 admin/security，
  命名空间校验、重名 409、凭据永不回显、测试走实时拉取）；管理台新增联邦上游卡片（中英双语）。
- 测试：federation 包 4 项（列表缓存命中 1 次、刷新失败回落旧缓存、转发带凭据与参数、上游宕机报错）+
  MCP 集成 6 项（命名空间列表与等级隐藏、转发成功、受管级上游拒自带级、机密参数被拦、违禁词参数被拦、
  自带级结果脱敏/受管级原文、未知命名空间）+ API 端点测试（鉴权 401/403、命名空间/URL 校验 400、
  重名 409、凭据加密存储与不回显、死链测试 502、删除 404）+ 冒烟新增 12 项断言；
  `gofmt`/`go vet`/`go test ./...` 全绿；`make smoke` 37 项断言 0 失败。

### F7 REST → MCP 适配 `done`（2026-11-21）
- 来源：ContextForge 的 REST-to-MCP（按 OpenAPI/手工映射把存量 REST API 变成工具）。
- 适配：映射配置里声明工具密级与最低 Agent 等级；请求头可注入托管凭据。
- 验收：把一个示例 REST 接口注册为工具并成功调用；参数 schema 校验生效。
- 借鉴点（ContextForge 官方 README 与文档 ibm.github.io/mcp-context-forge；mcp-rest-forge 配置模型，2026-11-21 查阅）：
  ContextForge 的 REST 工具是逐端点 JSON 注册（name/url/request_type/input_schema），
  凭据经 header passthrough 注入；v1.0.5 起可从 OpenAPI 生成单个端点的 schema，但整份 spec
  导入已明确 not planned——大 API 仍需逐端点注册或自写转换脚本。mcp-rest-forge 的 YAML
  模型与之同源：method/path 模板（{{param}} 转义替换）/query_params/body/headers。
  我们照此实现，但收紧为企业治理语义：每个工具只配最高密级与最低等级两把闸（与 F6 同款），
  参数 schema 由管理员在注册时提供并即时校验（不做 OpenAPI 整份导入——与 ContextForge
  的取舍一致，留待后续轮次评估）；上游 5xx 只报状态码不回传 body（不透传上游内部细节），
  响应单体上限 1 MiB；凭据只在调用瞬间解密注入。
- 实现：新包 `internal/restadapter`（路径模板 {{param}} 转义替换、其余参数 GET/DELETE 走 query、
  POST/PUT/PATCH 走 JSON body、JSON Schema 子集校验 required/type/enum/未知参数拒绝、
  上游错误只报状态码、1 MiB 响应上限）；`store.RESTTool`（方法/基址/路径模板/静态头/
  加密托管凭据/输入 schema/最高密级/最低等级）；`/mcp` tools/list 合并 `rest.<name>` 工具
  （高于调用者等级的隐藏）、tools/call 走与联邦工具相同的门禁（等级授权 → 参数 DLP →
  密级上限）与结果 DLP + 自带级脱敏，参数校验失败报 -32602；`GET/POST /api/mcp/rest-tools` +
  `DELETE /api/mcp/rest-tools/{id}` + `POST /api/mcp/rest-tools/{id}/test`（仅 admin/security，
  名称 snake_case、方法白名单、路径以 / 开头、schema 注册时校验、重名 409、凭据永不回显，
  测试端点返回 ok + 200 字内 preview 且不写入审计）；管理台新增 REST 工具卡片（中英双语）；
  演示：mock 增加 `/mock/demo/hr/leave-balance`（星河精密虚拟 HR 接口），seed 注册
  `rest.leave_balance` 演示工具。
- 测试：restadapter 包 6 项（GET 路径模板转义 + query、POST JSON body + 凭据注入、
  schema 校验四类拒绝、5xx 不泄漏 body、schema 注册校验、名称/方法白名单）+
  MCP 集成 8 项（命名空间列表与等级隐藏、GET 转发、POST body 转发、受管级工具拒自带级、
  参数校验 -32602、机密/违禁词参数被拦、自带级结果脱敏/受管级原文、未知工具）+
  API 端点测试（鉴权 401/403、名称/方法/路径/schema 校验 400、重名 409、凭据加密存储与
  不回显、测试端点成功/400/404/502）+ 冒烟新增 16 项断言；
  `gofmt`/`go vet`/`go test ./...` 全绿；`make smoke` 53 项断言 0 失败。

### F8 Prompt 模板库 `done`（2026-11-21）
- 来源：ContextForge Prompt Registry（模板版本管理 + 回滚）、Portkey prompt management。
- 适配：模板按部门范围下发，作为一种新能力类型进能力中枢（与技能同流水线：提交→审核→发布）。
- 验收：管理员可建/改/回滚模板版本；Agent 经 MCP 读取被授权的模板。
- 借鉴点（ContextForge 官方文档 ibm.github.io/mcp-context-forge；Portkey 官方文档 portkey.ai/docs/product/prompt-engineering-studio，2026-11-21 查阅）：
  ContextForge 的 Prompt 注册中心：Jinja2 模板 + JSON-Schema 参数校验 + 多模态块，版本管理与回滚，工具/资源/提示词统一注册表。
  Portkey 的版本语义是关键借鉴：**更新（update）只产生新版本、不推向生产；发布（publish）才把某个版本标为默认生产版本**；版本一旦创建不可变，改动只能产生新版本；调用默认走已发布版本，也可按版本号或 `@latest` 显式引用；历史版本可随时恢复/发布；另有 Render 端点（只取填充变量后的模板、不执行）。
  我们照此实现，但做三处治理收紧（与 F6/F7 同源）：① 版本选择只服务已发布版本——Agent 永远拿不到未发布版本（Portkey 的 `@latest` 是最新版无论是否发布，我们的 `@latest` 即已发布版），未发布版本只能由管理员在管理台预览；② 参数校验默认严格——未知参数拒绝、必填缺失拒绝、非字符串值拒绝（与 F7 的 schema 即契约一致）；③ 渲染走与工具调用相同的门禁：参数先过 DLP（违禁词直接拦截、参数密级高于模板上限不发送），渲染结果再过 DLP + 自带级脱敏，模板本身按部门范围与最低等级下发。
- 实现：`store.PromptTemplate`（名称 snake_case 唯一、部门范围、最高密级、最低等级、状态 draft/in_review/published/rejected、当前版本/已发布版本）+ `PromptVersion`（不可变版本：内容 `{{name}}` 占位符 + 声明参数）+ `RenderPrompt`（严格替换：未知/必填缺失/非字符串参数报错，只报字段名不报值）+ `ValidateTemplateVersion`（占位符须已声明、内容 ≤32 KiB、参数 ≤20 个）；`/mcp` 新增 `prompts/list`（仅已发布 + 范围内 + 等级足够）与 `prompts/get`（默认/显式版本选择、数字版本须为已发布版、参数与渲染结果双重 DLP + 自带级脱敏、审计 `prompt_get`）；管理 API `GET/POST /api/prompts`、`GET /api/prompts/{id}`、`POST /api/prompts/{id}/versions`（只追加不改旧版、已发布版继续服务）、`POST /api/prompts/{id}/review`（批准即发布当前版，首次发布时定已发布版）、`POST /api/prompts/{id}/publish`（发布/回滚到指定版本）、`DELETE /api/prompts/{id}`；管理台新增 Prompt 模板卡片（中英双语，建/发布-回滚/删）；演示种子 `weekly_report`（星河精密周报助手，已发布 v1）。
- 测试：store 5 项（渲染替换与可选参数、参数校验三类拒绝、版本校验八类拒绝、已发布版解析、部门范围）+ API 端点 1 项（提交→加版→审核发布→回滚→删除全链路、重名 409、非法名 400、员工 403）+ MCP 集成 7 项（列表仅已发布/范围/等级过滤、渲染、版本钉选与回滚、参数校验、参数密级拦截、渲染结果自带级脱敏/受管级原文、等级拒绝与草稿不泄漏）+ 冒烟新增 16 项断言；`gofmt`/`go vet`/`go test ./...` 全绿；`make smoke` 76 项断言 0 失败。

### F9 OIDC / LDAP 登录 `done`（2026-11-21）
- 来源：dsh-ee、LiteLLM Enterprise（SSO/SCIM 为其付费墙功能，我们开源实现即差异化）。
- 适配：本地账号保留为兜底；OIDC 用户自动映射部门与角色。
- 验收：对接一个公开 OIDC 测试 IdP 完成登录闭环；LDAP 以文档 + 配置预留（无环境时标注未实测）。
- 借鉴点（OpenID Connect Core 1.0 / Discovery 1.0，openid.net/specs，2026-11-21 查阅；Duende IdentityServer 演示站 demo.duendesoftware.com，2026-11-21 实测）：
  OIDC 授权码流程：discovery 文档取端点 → authorize（response_type=code + PKCE S256 + state/nonce）→ 回调换码（client_secret 走服务端 Basic 认证）→ 按 JWKS 校验 ID token（iss/aud/exp/nonce）→ userinfo 补 profile claim。
  Duende 演示站的实测教训有两条直接进了实现：① 它的 profile claim（name/email）不在 ID token 里、只在 userinfo 里，所以换码后必须调 userinfo 合并，不能假设所有 IdP 都把姓名放进 ID token；② 演示 client（interactive.confidential，secret `secret`，用户 alice/alice）登记的 redirect URI 是示例地址，验证时用登记地址走完换码、再用本仓库代码验 token——服务自身的回调闭环由 Go 测试（假 IdP）覆盖。
  我们照此实现，但做三处治理收紧：① IdP 只认身份、不认权限——role claim 即便给了也不采信，JIT 新用户永远是 employee，提权必须本地管理员操作；② 账号不串户——OIDC 用户名撞上本地账号时自动加后缀，OIDC 账号无本地密码哈希、本地登录永远失败；③ 只认 RS256（`alg: none` 与 HS256 直接拒绝），JWKS 按 kid 取键、未知 kid 刷新一次再拒；且纯标准库实现，不拉 oauth2/oidc 依赖。
- 实现：新包 `internal/oidc`（discovery + 授权码 + PKCE + RS256 验签 + userinfo 合并，state/nonce 单次有效 10 分钟过期）；`store.User` 增加 `oidc_subject` 与 `auth_source`；`auth.LoginOIDC`（按 sub 找号、没有则 JIT 开户、每次登录刷新姓名/部门、角色本地管、禁用拒登）+ `auth.CreateSession`；`GET /auth/oidc/login`（302 跳 IdP）与 `GET /auth/oidc/callback`（换码验签后发会话、回调页写 localStorage 回管理台，失败只给通用文案）+ `GET /api/auth/providers`（登录页用）+ `GET /api/auth/oidc/status`（admin/security，配置视图不含 secret）；管理台登录页在 OIDC 启用时显示「使用企业 SSO 登录」按钮（中英双语）；环境变量 `AGENTGATE_OIDC_*`（见 docs/oidc-sso.md），缺一个即不开启、只记日志，本地登录始终可用。
  LDAP：本轮只做配置与设计——新包 `internal/ldap`（`Config` + `ConfigFromEnv` + `Validate`，`Implemented()` 为 false）与 docs/ldap.md（绑定→搜索→用户绑定流程、安全约定、实现轮次验收清单）；无 LDAP 测试目录，未实现客户端、未实测。
- 测试：oidc 包 5 项（Config 默认与 http issuer 拒绝、假 IdP 全流程换码 + userinfo 合并、state 单次有效、ID token 八类拒绝：nonce/aud/iss/过期/无 sub/篡改/alg none/未知 kid、发现文档 issuer 不一致拒绝）+ auth 包 4 项（JIT 开户为 employee 且无本地密码、重复登录同一账号并刷新资料、撞本地用户名自动加后缀、禁用与空 sub 拒绝、用户名清洗与回退）+ API 端点 4 项（假 IdP 下 HTTP 全程：login → IdP → callback → 会话页 → /api/me；坏 state 400；未配置时 503；status 需登录）+ ldap 包 2 项（配置默认与五类拒绝、ldap:// + StartTLS 合法）；另用真实 Duende 演示 IdP 跑通登录闭环并以本仓库 `VerifyIDToken` 对线上 JWKS 验签通过（临时测试，验证后删除、未进仓库）；`gofmt`/`go vet`/`go test ./...` 全绿；`make smoke` 81 项断言 0 失败（新增 5 项 F9 断言）。

### F10 流式脱敏与 tool_calls 防护（探针 3） `done`（2026-11-21）
- 来源：蓝图探针 3；AstrLink 的流式占位符处理思路。
- 适配：SSE 分片跨界占位符缓冲还原；请求中 tool_calls 参数同样过 DLP 与分级。
- 验收：构造占位符被分片切断的用例仍正确还原；tool 参数含机密词时按等级拦截/脱敏；测试全绿。
- 借鉴点（nr-llm ADR 88《Live streaming redaction》netresearch.github.io/t3x-nr-llm/adr/adr088-live-streaming-redaction.html；Sether《PII redaction for LLM applications》sether.ai，2026-11-21 查阅）：
  流式脱敏的难点是分片边界：秘密可能被切在两个 chunk 之间，逐 chunk 独立处理必然漏检。两家做法同源——**holdback**：nr-llm 每 chunk 对整段已收 buffer 重新脱敏、只放行超出最后 128 字节的安全前缀，尾部在流结束时冲掉；Sether 用 `safeDistanceBytes`（默认 256）做同样的安全距离；关键教训是必须对**原始 buffer**反复处理——已打上的标记（如 `sk-***`）会破坏模式自身的字符类，二次处理会漏掉跨界后半段。
  我们照此实现，但方向相反：我们要还原的是**占位符**（请求脱敏时已打上），不是现场脱敏秘密——因此每读一块就对整段待发 buffer 做 `Restore`、只放行超出最长占位符长度的安全前缀，流结束时冲掉尾部；多字节 UTF-8 在切分点退让到 rune 边界。差异：holdback 取映射中最长占位符的**转义后**长度（上游 JSON 编码把 `<PII_1>` 写成 `\u003cPII_1\u003e`，比原文长 10 字节），且无映射时走零缓冲直通、不增加延迟。
- 实现：`internal/guard` 新增 `Redactor`（同一请求内多段文本共享占位符编号——同一值在 content 与 tool_calls 参数里得同一占位符，避免逐段编号合并时冲突）与 `MaxPlaceholderLength`（最长占位符的转义后长度，供流式 holdback 用）；`internal/gateway` 的 `chatRequest` 改为命名类型并解析 `tool_calls`（`scanText` 把参数文本并入防护管线：分级/违禁词/脱敏与 content 同权），`redactBody` 用同一 `Redactor` 处理 content 与各 tool_calls 参数、重组后保留 id/name/tool_call_id；新文件 `internal/gateway/stream.go`（`streamCopy` 带 mapping：无映射直通、有映射缓冲还原 + 用量提取）；`internal/mockllm` 回显 tool_calls 参数（供端到端观测上游实际收到的内容）。另修一个顺带发现的旧 bug：流式用量的旧提取逻辑切片出非法 JSON（前导逗号 + 多余花括号），流式调用一直记 0 tokens、TPM 与统计都漏记，改为按花括号配对解析 usage 对象。
- 测试：包内 stream 测试 5 项（逐字节/2/3/7 字节分片下转义与原始两种占位符均还原、多字节文本无损、`[DONE]` 收尾、用量提取、流尾占位符冲刷、空映射直通、双占位符不同长度 holdback）+ 包内 redact 测试 3 项（tool_calls 脱敏与字段保留、content/参数共享占位符无冲突、无发现时原文原样直通）+ HTTP 端到端 5 项（流式还原、流式 tool_calls 参数还原、tool_calls 违禁词 400、自带级机密参数 403、受管级机密参数脱敏后 200 且原文还原）+ 冒烟新增 3 项断言；`gofmt`/`go vet`/`go test ./...` 全绿；`make smoke` 84 项断言 0 失败。
- 探针 3 结论：通过——占位符跨分片还原用例全过；同一值在同一请求内跨 content/tool_calls 得同一占位符（跨轮次一致性由每请求从 `_1` 重新编号保证，映射随请求走、不跨请求复用）。

### F17 渠道信任类型与分级策略梯度 `done`（2026-11-21）
- 来源：Nathan 2026-11-21 决策：蓝图「分级路由」默认机密走企业自有私有模型的假设不成立——正常公司没有自己的模型（无 GPU/无运维），很多也没有数据库。
- 适配：分级放行不绑「自有模型」，改绑「通道信任类型」梯度：公开/内部走签约云（零留存/不训练承诺）；机密默认走「网关脱敏后上云」（原文不出公司，云端只见代号、响应还原），厂商专有云/VPC 部署为可选项；绝密走本地小模型（单机可跑）或直接拦截并明确提示「无合规通道」。私有模型从必需项降为可选项之一。默认「保守模式」预设（机密以上默认拦、审计全开），上线路径分三步「先观察（DryRun 只审计不拦截）→ 再提醒 → 后拦截」。
- 验收：渠道配置信任类型（签约云 / 专有云-VPC / 本地小模型）；机密请求在无合规通道时被拦截并返回明确原因；蓝图与企业级防护设计文档同步修订。
- 借鉴点（Cloudflare AI Gateway 官方文档 developers.cloudflare.com/ai-gateway/get-started；层3实验室 2026 年七家网关对比，2026-11-21 查阅）：
  Cloudflare AI Gateway 只做托管式代理、没有自托管或 VPC 内部署选项——这是受数据驻留约束的团队最先撞上的墙；数据驻留只在网关层可配，最终推理位置由模型厂商决定；AWS Bedrock 与 Databricks 是唯二开箱同时具备数据驻留控制与 HIPAA 适用性的网关。
  我们照此思路但反过来做：不把部署形态锁死在某一家厂商——信任类型就是部署形态本身（签约云 / 专有云-VPC / 本地小模型），每个渠道自带信任类型，路由按密级选信任梯度；机密默认脱敏后上云是 Cloudflare 这类纯托管网关给不了的保证（它们不替你脱敏，只替你转发）。差异化收紧与 F1 同源：分级约束高于可用性——无合规通道时直接拦截并明说缺哪种通道，绝不静默降级。
- 实现：`store.TrustType`（signed_cloud / dedicated_cloud / local_model，JSON 字符串、空值归一为签约云以兼容旧快照）+ `Channel.TrustType` + `TrustTypeOrDefault()`；`internal/gateway/trust.go`（梯度判定：公开/内部任意、机密签约云须强制脱敏、绝密仅本地模型；`no_compliant_channel` 提示文案）；网关 `pickCandidates` 与 `channelsCoolingDown` 按信任过滤，机密→签约云时即使等级脱敏配置关闭也强制脱敏（审计 Detail 记 `desensitized=signed_cloud`），无合规通道时 403 `no_compliant_channel` 并写 `violation` 审计事件；`GET/POST /api/channels` 返回与接受 `trust_type`（非法值 400、缺省为签约云、密钥仍加密存储不回显）；管理台渠道表新增信任类型列（中英双语）；seed 增加脱敏云渠道（机密默认）与本地模型渠道（绝密），私有渠道标为专有云/VPC。
- 测试：store 3 项（解析/JSON 往返/空值默认）+ 网关信任测试 5 项（梯度路由与上游实收报文验证、等级脱敏关闭时仍强制脱敏、机密走专有云原文、无线上本地模型时绝密 403 `no_compliant_channel` 及审计原因、仅专有云时绝密仍为 `no_compliant_channel` 而非通用错误）+ API 渠道测试 1 项（种子信任类型/员工 403/非法值 400/本地渠道创建与密钥加密/缺省签约云/列表不泄漏密钥）+ 冒烟新增 7 项断言；`gofmt`/`go vet`/`go test ./...` 全绿；`make smoke` 60 项断言 0 失败。
- 已知注意点：故障转移链仍沿用 F1 规则（只在首选渠道同一 MaxDataLevel 内转移）——脱敏云故障时不会自动升级到专有云/本地模型，而是 502；跨信任梯度的合规故障转移留待后续轮次评估。另：本轮同时落地蓝图修订稿 8 处替换（见 DEVLOG）。

### F18 文件夹即知识库 `done`（2026-11-21）
- 来源：Nathan 2026-11-21 决策：目标客户是传统/保守公司，连知识库也没有——知识散落在共享文件夹的 Word/Excel/PDF 与老员工脑子里。设计原则「零预置假设」：不假设客户有自有模型、数据库、知识库中的任何一样。
- 适配：指定共享文件夹即自动索引（按文件夹映射密级、增量更新、关键词检索），不要求客户先整理录入；与现有 kb 检索同一流水线（关键词检索，不用向量库——与零依赖定位一致）。关键设计（Nathan 补充）：采用 PageIndex 式索引树 + Wiki 形态 + 渐进式披露——索引分三层（全局目录/Wiki 首页 → 每份文档的摘要页与章节页码索引树 → 原文按需下钻），Agent 先看目录和摘要、判断相关后才取具体章节原文，大幅减少上下文占用，引用可精确到文件+章节+页码；摘要生成本身也必须走分级路由/脱敏，不能成为泄漏通道；副产品是公司直接得到一个可浏览的 Wiki 知识库。与现有无向量库、关键词路线同源（PageIndex 本就是无向量 RAG 思路）。
- 验收：指定一个含 Word/PDF 的演示文件夹后可检索到内容；文件夹密级映射生效（机密文件夹内容自带级不可见）；增量更新只索引变化文件。
- 借鉴点（PageIndex 官方介绍 pageindex.ai/blog/pageindex-intro；PageIndex 架构深挖 nowucca.com/research/pageindex，2026-11-21 查阅）：
  PageIndex 以每文档一张「目录树」替代 chunk + 向量检索：索引时建章节树（节点含 node_id、标题、起止页码、每节点一段摘要），检索时 Agent 用三个工具走目录 → 读树（只要摘要不要正文）→ 按页取原文；Flash 模式不用 LLM 建结构（字体样式识别标题、优先用 PDF 内嵌书签、薄节点合并）。我们在零依赖与治理约束下照此实现，但做三处改动：① 摘要不用模型生成——改抽取式（章节首句），摘要生成因此不产生任何出站调用、不成为泄漏通道，代价是摘要质量低于模型生成；② 节点页码：PDF 用真实页（内容流与页数 1:1 时），.md/.txt/.docx 用 45 行伪页（无渲染引擎，引用页码是稳定的行窗口）；③ 检索仍保留关键词（与现有 kb 同一流水线），树只负责渐进式取用，不替代检索。
- 实现：新包 `internal/folderkb`（`Registry` 内存索引 + `Rescan` 增量扫描：大小+修改时间未变不读文件、内容哈希相同保留旧解析、抽取失败保留上次可用索引；`Catalog`/`Structure`/`Section` 三层取用 + 按章节粒度的 `Search`）；抽取器纯标准库：.md/.txt 文本解析（ATX 标题建树）、.docx（zip + XML，只取 `w:t`，Heading1/2/3 建树、显式分页符分断）、.pdf（best-effort：Flate 解压 + Tj/TJ 字面串抽取，内容流与页数 1:1 时映射真实页码）；`store.KBFolderSource`（名称/路径/密级/启用）持久化注册、索引不落盘；管理 API `GET/POST /api/kb/folders` + `DELETE /api/kb/folders/{id}` + `POST /api/kb/folders/{id}/rescan`（仅 admin/security，注册前先扫一遍、路径已注册 409）；MCP 新工具 `kb_catalog` / `kb_structure` / `kb_section`（与 `kb_get` 不同：按 `MaxLevelForTier` 门禁，自带级可读内部章节），`kb_search` 合并种子文档与文件夹命中（`source` 字段区分 `kb`/`folder`）；管理台新增文件夹知识库卡片（中英双语）；演示种子注册 `demo/folders/公司制度/`（internal：.md）与 `demo/folders/机密资料/`（confidential：2 页 .pdf，内容流未压缩以保持纯 UTF-8，见下），仅当文件夹存在于工作目录旁时注册；详见 docs/folder-kb.md。
- 测试：folderkb 包 7 项（Markdown 树/摘要/引用、.docx 抽取与分页符、.pdf 双页抽取含 Flate 压缩页与真实页码引用、密级四处隔离、增量扫描：新增/修改/删除/无变化/损坏文件、缺失文件夹报错、单页文档根节点即全文）+ API 端点 1 项（注册→列表→重扫→删除全链路、400/403/404/409）+ MCP 集成 3 项（目录→结构→章节三步取用、机密文件夹对自带级四处不可见、kb_search 合并命中且来源可区分）+ 冒烟新增 8 项断言；`gofmt`/`go vet`/`go test ./...` 全绿；`make smoke` 92 项断言 0 失败。
- 已知注意点：.md/.txt/.docx 的引用页码是 45 行伪页、不是渲染页码；.docx 只读正文段落（不读页眉页脚/批注/文本框）；PDF 抽取不支持扫描件、加密 PDF 与非标准编码，此类文件计入扫描 errors；Excel（.xlsx）暂不支持（与 .docx 同属 zip+XML，后续轮次可按同一模式接入）；索引在内存中，重启后启动时自动重扫。

## P2 — 进阶与差异化

### F11 精确匹配响应缓存 `done`（2026-11-21）
- 来源：Cloudflare AI Gateway / Portkey cache。
- 适配：仅 public/internal 且无 DLP 命中时可缓存；缓存键含渠道与密级；管理台可关。
- 验收：相同请求第二次命中缓存不计上游用量；含敏感内容永不缓存（测试证明）。
- 借鉴点（Cloudflare AI Gateway 官方文档 developers.cloudflare.com/ai-gateway/features/caching；Portkey 官方文档 portkey.ai/docs/product/ai-gateway/cache-simple-and-semantic，2026-11-21 查阅）：
  Cloudflare 的缓存键是提供商 + 端点 + 模型 + 认证头 + 整份请求体做 SHA-256，精确匹配才命中，TTL 最小 60 秒、最大一个月，默认关闭、逐请求可跳过（cf-aig-skip-cache）或改 TTL，响应头 cf-aig-cache-status 报告 HIT/MISS；流式响应不缓存（边生成边发送，无法缓存）。Portkey Simple 模式同为整份请求体精确匹配（messages/prompt、model、temperature、max_tokens 及全部参数），另有按 x-portkey-cache-namespace 分区的命名空间、max_age 与强制刷新；语义模式要向量库，只做精确匹配已覆盖确定性调用。我们照此实现，但做三处治理收紧：① 只有请求与响应双向都干净才缓存——请求须公开/内部且 DLP 零命中（有机密词或 PII 的请求根本不查缓存），响应入库前再扫一次（模型也可能对干净提示吐出 PII）；② 键里加渠道 ID 与数据密级，同一请求走不同渠道/密级不共享；③ 命中审计为独立的 `cache_hit` 事件，不进 F2 用量统计（统计只计量上游用量），也不触发 F5 告警。
- 实现：新包 `internal/cache`（进程内内存缓存：SHA-256 键、TTL 默认 1 小时、最多 1000 条 FIFO 淘汰、仅 200 响应且 ≤1 MiB 才存、命中/未命中计数、停用时不查不存、清空不重置计数；重启即清空，与 F1 的渠道健康状态同为运行时状态）；网关 `Policy.CacheTTL` + `AGENTGATE_CACHE_TTL_SECONDS`（默认 3600，最小 60）与 `AGENTGATE_CACHE_ENABLED`（0/false 启动即停用）；`ChatCompletions` 在管线之后、转发之前查缓存（仅非流式、非 DryRun、公开/内部、无 findings、缓存启用），命中直接返回存体并写 `cache_hit` 审计（0 tokens，不记 TPM/配额/统计），响应头 `X-AgentGate-Cache: HIT/MISS`（不合规请求不查缓存、故无此头）；管理 API `GET /api/cache`（状态与计数）+ `POST /api/cache`（开关）+ `POST /api/cache/clear`（清空，返回条数），仅 admin/security；管理台新增响应缓存卡片（状态/计数 + 启用-停用 + 清空，中英双语）；失败转移时按实际服务渠道的键入库（该渠道成为首选时可再次命中）。
- 测试：cache 包 7 项（键确定性/渠道与密级区分/参数区分、存取往返与返回体拷贝隔离、过期、停用不查不存、FIFO 淘汰与刷新不驱逐、超大与非 200 拒绝入库、统计与清空不重置计数）+ 网关缓存测试 5 项（命中返回与首份响应逐字节相同且审计恰为 1 次上游调用 + 1 次 cache_hit、变 temperature 不命中、PII 请求两次均走上游且无缓存头、机密请求不查缓存、流式无缓存头、干净请求但响应含 PII 时两次均走上游）+ API 端点 1 项（状态默认值/员工 403/匿名 401/开关往返/缺字段 400/清空）；另修顺带发现的测试精度问题：故障转移测试的相同请求会被缓存截住不再触及上游，`setupFailover` 改为停用缓存（该套测试测的是上游故障计数，与缓存无关）；`gofmt`/`go vet`/`go test ./...` 全绿；`make smoke` 105 项断言 0 失败（新增 13 项 F11 断言）。
- 已知注意点：同一密级的公开/内部请求若首选渠道相同会共享缓存条目——响应本身无差别（无敏感内容、无脱敏），共享安全；缓存键是整份请求体的精确匹配，键顺序或空白不同即不命中（与 Cloudflare/Portkey 的精确匹配语义一致）；故障转移链中首选渠道故障时按实际服务渠道入库，首选恢复前命中率略降（正确性不受影响）；语义缓存（近似匹配）需要向量库，与零依赖定位冲突，本轮不做。

### F12 渠道多 Key 与轮换 `done`（2026-11-21）
- 来源：New API 渠道多 key 轮询、LiteLLM key rotation。
- 适配：key 仍加密托管；轮换不中断在途请求；失效 key 自动摘除并告警（联动 F1/F5）。
- 验收：同一渠道配两把 key 时调用轮流走两把 key；一把 key 被上游 401 后自动摘除、同一请求用另一把 key 成功、后续请求不再用失效 key；管理台可增/停用/恢复/删除 key。
- 借鉴点（QuantumNous/new-api 源码 model/channel.go `GetNextEnabledKey`，AGPL-3.0，2026-11-21 查阅；LiteLLM 多部署路由，2026-11-21 查阅；通用密钥轮换 runbook）：
  new-api 的多密钥形态是单渠道多密钥 + 按索引的状态列表（`MultiKeyStatusList` / `MultiKeyDisabledReason` / `MultiKeyDisabledTime`，缺失即启用）+ 轮询游标 `MultiKeyPollingIndex`；选取时每渠道一把锁、从游标出发环形扫描第一把启用密钥、游标推进到下一位、全部禁用时显式报错（早期曾静默回落到第一把密钥导致继续打失效 key，后修复为报错）；轮换的通用 runbook 是先加新 key、验证可用、再删旧 key（重叠窗口，零停机）。
  LiteLLM 则是同一逻辑模型名下挂多个部署、各配自己的 key 与 rpm/tpm，按 `simple-shuffle` 或 `usage-based-routing-v2` 分摊，`allowed_fails`/`cooldown_time` 按部署隔离。
  我们照 new-api 的选取算法实现，但做四处改动：① 只做轮询、不做随机模式（轮询可预测、易审计）；② 密钥逐把 AES-GCM 加密存储、每把自带停用状态，不用换行拼接单字符串（避免新 key 含换行时切分错乱）；③ 游标与停用状态为运行态（与 F1 冷却一致，重启后重新向上游证明）；④ 401 只摘除该 key（不像 new-api 那样把整条渠道标不可用），429/5xx 仍走 F1 的渠道级故障转移（那是渠道/上游的问题，不是某把 key 的问题）。
- 实现：`store.ChannelKey`（加密值 `json:"-"` + 脱敏显示 + 停用状态/原因/时间）+ `Channel.Keys []ChannelKey`（`APIKeyEnc`/`KeyMasked` 单 key 字段随之退役，旧快照本就不持久化 key 故无需迁移）+ `store.MaxChannelKeys = 10`；`internal/gateway/keys.go`（`nextKey` 轮询：每渠道游标 + 环形扫描跳过停用 key + 无可用 key 显式失败；`disableKey` 写停用状态 + `channel_key_disabled` 审计事件；锁顺序固定为 keyMu → store 锁，race 检测通过）；网关转发循环改为渠道内 key 循环：401 时摘除该 key、同请求换下一 key 重试，全部 key 都 401 时原样返回上游 401；全部 key 停用时返回明确的 `channel_no_enabled_key`（502）；`alert.FromEvent` 新增 `channel_key_disabled`（warning，联动 F5）；管理 API `GET /api/channels` 返回每把 key 的脱敏值与状态、`POST /api/channels` 接受 `api_keys`（去重、上限 10、空主 key 时首个附加 key 晋升主 key）、`POST /api/channels/{id}/keys` 增加、`DELETE .../keys/{index}` 删除（禁删最后一把）、`POST .../keys/{index}/enable|disable` 启停（均仅 admin，密钥永不回显）；管理台渠道表逐 key 显示脱敏值与停用 pill + 启用/停用/删除按钮，新增「管理渠道密钥」卡片（中英双语）；演示种子 `ch_cloud` 带两把 key，轮换开箱可见。
- 测试：gateway 白盒 2 项（轮询顺序与环绕、停用后跳过、全停用无 key 可选、摘除审计字段齐全）+ 网关端到端 3 项（401 摘除 + 同请求重试 + 后续请求只走存活 key、两 key 轮询分摊 a/b/a、全 key 失效时返回上游 401、再请求报 `channel_no_enabled_key`）+ API 端点 1 项（创建 3 key、列表脱敏、加密存储、重复 400、增/停用/恢复/删除全链路、删最后一把 400、未知索引 404、员工 403、明文永不泄漏）+ alert 分类 1 项（`channel_key_disabled` 触发 warning 告警）+ 冒烟新增 9 项断言；`gofmt`/`go vet`/`go test ./...` 全绿，`go test -race`（gateway/api）全绿；`make smoke` 114 项断言 0 失败。
- 已知注意点：key 停用状态与轮询游标是运行态（与 F1 冷却一致），重启后失效 key 会重新启用、由网关再次向上游证明（401 后重新摘除）；删除 key 后其后 key 的索引前移（与 new-api 的位置索引语义一致）；429 不触发 key 内轮换——限流仍走 F1 的渠道级故障转移。

### F13 成本计价与部门预算 `done`（2026-11-21）
- 来源：LiteLLM budgets（硬顶 429）、Portkey per-key budget。
- 适配：渠道配置模型单价，统计折算金额（联动 F2）；部门月度预算超限拦截。
- 验收：渠道配置模型单价后，统计折算出金额且与审计对账；部门月度预算超限后下一调用被 429 拦截；花费达告警阈值时每月发一次提醒；管理台可配预算与单价。
- 借鉴点（LiteLLM 官方文档 docs.litellm.ai/docs/tutorials/claude_code_cut_costs，2026-11-21 查阅；Portkey 官方文档 portkey.ai/docs/virtual_key_old/product/ai-gateway/virtual-keys/budget-limits 与 docs.gw.prismaairs.com 限额用例指南，2026-11-21 查阅）：
  LiteLLM 的虚拟 key 预算是 `max_budget`（USD）+ `budget_duration`（"24h"/"30d" 等滚动窗口，可叠加多档），计数到期自动重置；超限后**下一调用直接拒绝**、不走到模型；另有 `model_max_budget` 按模型设限 + `budget_fallbacks` 链（超限静默降级到更便宜的模型）。
  Portkey 的预算上限是按虚拟 key（或按工作区，即部门）设的成本上限（USD，最低 $1）或 token 上限（最低 100），达限自动停用该 key 防止超支；可设告警阈值（如 80%）提前通知；可按周（周日 0 点 UTC）/按月（每月 1 日 0 点 UTC）自动重置；上限一旦设定不可改（改需复制 key 重建）。
  我们照此实现，但做三处改动：① 预算按**部门**设而非按 key——企业是按部门分钱的，同一部门的多个 key/令牌共享一个钱袋；② 不做 LiteLLM 的静默降级——超限直接 429 明确拒绝（降级会改变模型行为、且分级约束高于可用性，降级到的便宜模型未必合规）；③ 预算可随时 PATCH 改上限（Portkey 的不可改是为防抵赖，我们的改动本身记审计、可追溯）。
  计价形状沿用 LiteLLM 成本表的输入/输出分开计价（input/output 每百万 Token，USD）；其整张真实厂商价目表未复制——AgentGate 的渠道服务的是管理员配置的模型（含 mock 与本地模型），价目应由管理员按合同价配，抄一张真实厂商价目表是错误的复用。
- 实现：新包 `internal/billing`（`Cost` 输入/输出拆分计价、`MonthWindow` 自然月 UTC 窗口、`DepartmentSpend` 按审计日志汇总部门本月花费）；`store.ModelPrice`（输入/输出每百万 Token USD）+ `Channel.Pricing map[string]ModelPrice` + `store.DepartmentBudget`（部门/月度上限/告警阈值 0–1）；`audit.Event` 增加 `CostUSD`（调用时按服务渠道单价写入，哈希链覆盖；0 表示未定价）；网关 `parseUsage`/`streamCopy` 返回 prompt/completion/total 拆分（流式用量同样拆分），调用完成即计价入审计；预算预检在流水线之前（限流与令牌配额之后）：本月花费 ≥ 上限时 429 `department_budget_exceeded` + `violation` 审计事件；花费首次达到告警阈值时记一条 `budget_threshold` 审计事件（每月每部门一次，`alert.FromEvent` 映射为 warning）；`GET /api/stats` 各维度与 totals 增加 `cost_usd`，totals 增加 `unpriced_calls`（有 token 但成本为 0 的调用数，成本总数因此是下限）；管理 API `GET/POST /api/budgets` + `PATCH/DELETE /api/budgets/{id}`（仅 admin，部门唯一、重复 409、上限须 >0、阈值须在 (0,1] 内）；管理台用量统计页三张表加成本列、总览 KPI 下显示累计成本与未定价调用数、新增部门月度预算卡片（列表/新建/行内改上限/删除）、渠道表加单价列、添加渠道表单可填单价 JSON（中英双语）；演示种子：四条渠道配演示单价（mock-eco $0.50/$1.50、mock-pro $2.50/$10.00、专有云 $5.00/$15.00、本地模型 $0），研发部演示预算 $50/月、阈值 0.8。
- 测试：billing 包 3 项（计价拆分/未定价为 0、自然月窗口含闰年二月、部门花费聚合：跨部门/跨月/拦截与缓存命中不计/未知部门不计）+ 网关预算测试 4 项（调用成本入审计且数额与 mock usage 拆分一致、流式调用同样计价、超预算后下一调用 429 `department_budget_exceeded` 且审计为 violation、其他部门不受影响、阈值警告每月恰一次、未定价渠道成本为 0）+ API 端点 1 项（预算 CRUD 全链路、种子预算、400/403/404/409）+ stats 聚合 1 项（cost_usd 汇总、unpriced_calls、currency USD）+ alert 分类 1 项（`budget_threshold` 为 warning）；`gofmt`/`go vet`/`go test ./...` 全绿；`go test -race`（gateway/api）全绿；`make smoke` 126 项断言 0 失败（新增 12 项 F13 断言）。
- 已知注意点：同一模型、同一密级有多个渠道时按 ID 排序取首个——未配单价的渠道会使调用记为未定价（冒烟初版即因此失败：F12 轮建的 mock-eco 渠道随机 ID 排在 ch_cloud 之前、无单价，跨越预算的调用记了 $0；冒烟改用只由定价渠道服务的 mock-pro 钉住，产品行为本身不变）；成本是调用完成时才知道的精确数，一次调用本身可能把花费推过上限，拦的是下一次调用；审计归档（F4）把事件移出热日志后，归档段的花费不再计入当月预算（归档只对已过保留期的旧事件做，默认 180 天，不会动当月数据）；本地模型单价为 0，在统计中计入 `unpriced_calls`（0 价与未定价在事件中无法区分，是有意的保守口径）；单价是静态配置，上游调价后需管理员同步改。

### F14 Agent 证明（Attestation） `done`（2026-11-22）
- 来源：蓝图既有设计；WorkBuddy 企业版的终端管控（iOA 类）思想。
- 适配：受管 Agent 登录时上报版本/配置哈希/插件清单，平台校验后才发受管级令牌；不符自动降级自带级 + 告警。
- 借鉴点（IETF RATS RFC 9334，rfc9334；SPIFFE/SPIRE 官方文档 spiffe.io，2026-11-22 查阅）：
  RATS 的角色模型：Attester 上报 Evidence，Verifier 按 Appraisal Policy 对照 Reference Values 与 Endorsements 评估、产出 Attestation Results，Relying Party 据此决策；Verifier 与 Relying Party 分离；新鲜度靠 nonce（或可信时钟/epoch）。SPIFFE/SPIRE 的做法是先证明、后发身份：节点证明与工作负载证明都通过才发 SVID，身份即证明链的结论，SVID 短期有效。
  我们照此实现，但做三处改动：① 新鲜度不用单独 nonce——证明绑定到一次设备码（code + polling secret，单次使用、10 分钟过期），一份证明不能重放到下一次登录；② 失败不拒绝登录、而是降级为自带级（员工仍可用通用能力，敏感能力不落到不合规 Agent 手里），降级本身记审计并触发 F5 告警；③ 本轮只做软件自报证据的一致性校验，不做密码学绑定（硬件根信任留待后续，见 docs/attestation.md 已知限制）。
  复用检查（Nathan 复用优先指示）：无可直接复制的代码——RATS 是架构文档、无参考实现；SPIRE/Veraison 是整套证明系统（Apache-2.0），与本功能体量不匹配；评估逻辑（版本比较 + 哈希白名单 + 插件白名单）约 150 行纯标准库，自研并在包注释与 DEVLOG 记录来源。
- 实现：新包 `internal/attest`（`Report` 证据 + `Policy` 评估策略与参考值 + `Evaluate` 裁决 + `CompareVersion` semver 式比较，原因只记类别码、不记哈希值）；`store` 增加 `AttestationPolicies`（按 Agent 类型）与设备码上的证明记录、令牌上的 `Attested`/`AttestVerdict`；`auth.SubmitAttestation`（凭 code+secret 提交、绑定设备码，Agent 类型以设备码为准）+ `ApproveDeviceCode` 改为返回实发等级（通过才发受管级、失败或缺失且策略要求时降级自带级、无策略时按申请发并标记未验证）；`POST /api/setup/device/attest`（无会话，凭 code+secret）+ 管理 API `GET/POST /api/attestation/policies`、`DELETE /api/attestation/policies/{agent}`、`GET /api/attestation/reports`（读审计日志）；`alert.FromEvent` 新增 `attest_downgrade`（warning）；`ag-setup` 增加 `-version`/`-plugins`/`-guard` 并在登录时提交证明（配置哈希取待写网关配置骨架的 SHA-256、不含令牌，二进制哈希取自身可执行文件；提交失败只警告、不阻塞登录）；管理台新增「Agent 证明」页（策略增删 + 最近证明记录，中英双语）与令牌表证明列；演示种子 `dsh` 策略为最低版本 0.1.0 且要求提交证明；详见 docs/attestation.md。
- 测试：attest 包 3 项（版本比较 12 组、评估 10 类分支含多原因并报、报告与策略校验）+ auth 包 1 项（通过→受管已验证、失败→自带级、未提交→自带级、无策略→受管未验证、错 secret/批准后提交拒绝）+ API 端点 1 项（种子策略、证明通过/失败/未提交全链路、降级审计事件、策略 CRUD、400/403/404）+ alert 分类 1 项（`attest_downgrade` 为 warning，`attest_submit` 本身不告警）+ 冒烟新增 9 项断言；`gofmt`/`go vet`/`go test ./...` 全绿；`make smoke` 136 项断言 0 失败。
- 已知注意点：本轮证明为软件自报证据，不防有意伪造（见 docs/attestation.md）；证明只在登录时做一次，令牌 30 天有效期内不重评；网关/MCP 的旧测试里 dsh 受管令牌现改为先提交证明再批准（与真实 Agent 同款流程）。冒烟调试中发现并修复一个与 F14 无关的冒烟脚本旧缺陷：F5 段末尾 `httpd.shutdown()` 提前关闭了 Webhook 接收器，但 webhook 仍注册在服务端，F6 `tool_call_denied` 与 F14 `attest_downgrade` 继续向已死监听投递，请求躺在 socket 缓冲区无人读，Go 客户端等满 10 秒超时、重试积压致队列满后丢弃后续告警；已改为 main() 末尾再关闭接收器。另记后续改进：单个失效 webhook 会以 10 秒超时 + 退避重试阻塞分发队列（队头阻塞），可考虑单 webhook 超时隔离或并行投递。

### F15 本地隐私网关 sidecar `done`（2026-11-22）
- 来源：AstrLink 本地隐私网关（端侧检测与可逆脱敏、映射不出端）。
- 适配：ag-setup 配套的本地守护进程，受管级双层脱敏的端侧一层；与出口守卫联动。
- 借鉴点（AstrLink 官方介绍 toolhunter.cc/tools/astrlink；Calcium-Ion/AstrLink 隐私引擎 DeepWiki，2026-11-22 查阅）：
  AstrLink 是本地 AI 网关（Go core + Rust 隐私 worker sidecar）：端侧隐私策略按规则（凭据/邮箱/电话/卡号，卡号带 Luhn 校验）或本地 ONNX 模型检测，策略为提醒/拦截/占位符替换，响应回程本地还原；隐私 worker 以独立进程跑模型、Go core 通过 IPC 管理其生命周期与崩溃恢复；另有 dry-run 诊断模式。
  我们照此实现，但做四处改动：① 检测复用本仓已有的 `internal/guard`（规则 + Aho-Corasick，与网关同款检测器），不引入 ONNX 模型与 Rust worker——本轮只做规则检测（蓝图既定），本地小模型检测留待后续轮次；② 不拉独立模型进程——sidecar 本身就是 Go 单进程守护进程，映射只在本次请求的内存中；③ 占位符编号与网关 F10 同款（同一请求内同一值同一占位符，流式还原用同一 holdback 模式）；④ 出口收紧：sidecar 构造上只拨配置的企业网关一个地址（配合 endpoint/egress 防火墙模板，受管 Agent 进程只准连网关与 127.0.0.1），AstrLink 是通用本地网关、可配任意上游。
  复用检查（Nathan 复用优先指示）：未复制 AstrLink 代码——其隐私引擎为 Go core + Rust ONNX worker，与本仓纯 Go 标准库路线不匹配；检测与还原复用本仓已有 `internal/guard`（F10 已验证的 Redactor/Restore/MaxPlaceholderLength），流式还原复用 F10 的 holdback 模式（整段还原后只放行安全前缀）。
- 实现：新包 `internal/sidecar`（`Config` + `Validate`：网关地址须为 http(s)、不得嵌凭据、不得指回自身；`Server`：`127.0.0.1` 代理，`POST /v1/chat/completions` 先扫描后转发、`GET /v1/models` 直通、`GET /healthz` 与 `GET /status`；违禁词本地 400 `sidecar_blocked`、PII/凭据/自定义词脱敏后转发、响应还原（流式用 holdback）、`X-AgentGate-Sidecar: ag-guard/0.1.0` 与只升不降的 `X-AgentGate-Data-Level` 发往网关；计数器与最近 50 条决策只含类别、不含原文）；新二进制 `cmd/ag-guard`（配置文件 `~/.agentgate/guard.json` < 环境变量 < 命令行参数，优雅停机）；网关 `ChatCompletions` 把 sidecar 标识记进审计 `detail.sidecar`（model_call 与 cache_hit）；管理 API `GET /api/sidecar/stats`（admin/security，扫审计日志计数经 sidecar 的调用/缓存命中/最近一次/按 Agent 分组，本地拦截不上报、只在端侧可见）；`ag-setup` 增加 `--sidecar`/`--sidecar-url`（模型地址改写为 sidecar 地址、证明里 `guard_applied` 隐含为真、配置哈希覆盖实际写入的 model_base）；管理台「设备接入」页新增 sidecar 卡片（调用量 + 最近一次 + 部署说明，中英双语）；详见 docs/local-gateway-sidecar.md。
- 测试：sidecar 包 9 项（配置校验六类拒绝、干净请求直通、PII 上游只见占位符/下游还原/状态计数无原文泄漏、违禁词本地拦截且网关零调用、dry-run 只记录不拦截不脱敏、流式逐字节分片还原、tool_calls 参数共享占位符编号、models 直通、healthz）+ 流式还原白盒 2 项（逐字节读取下双占位符还原、空映射原样直通）+ 网关 3 项（sidecar 标识进 model_call 审计、cache_hit 同样记录、无标识不记）+ API 端点 1 项（计数口径：2 次调用 + 1 次缓存命中、直接调用与 violation 不计、员工 403）+ 冒烟新增 9 项断言；`gofmt`/`go vet`/`go test ./...` 全绿；`make smoke` 145 项断言 0 失败。
- 已知注意点：本轮只做规则检测，本地小模型检测留待后续轮次；只代理模型流量（`/v1/chat/completions`、`/v1/models`），MCP 仍直连网关、由服务端工具授权与 DLP 管控；映射只在内存中，sidecar 重启会中断进行中的请求；流式还原依赖模型原样回传占位符；网关侧词表改动不自动同步到端侧配置，网关二次检测是兜底。

### F16 OpenTelemetry 导出 `done`（2026-11-22）
- 来源：ContextForge 的 OTel 可观测（Phoenix/Jaeger 等）。
- 适配：网关与 MCP 调用产生 trace/metric，字段脱敏后导出。
- 借鉴点（ContextForge 官方文档 ibm.github.io/mcp-context-forge，2026-11-22 查阅；OTLP/HTTP JSON 协议说明，2026-11-22 查阅）：
  ContextForge 的 OTel 形态是厂商中立的 OTLP 导出：Phoenix/Jaeger/Zipkin/Tempo 等后端都接，
  网关操作与工具/提示词调用自动产生 trace，LLM 指标（token 用量、成本、模型性能）单独记，
  关闭时零开销、优雅降级；配置用标准变量（`OTEL_ENABLE_OBSERVABILITY` / `OTEL_SERVICE_NAME` /
  `OTEL_EXPORTER_OTLP_ENDPOINT` / `OTEL_EXPORTER_OTLP_PROTOCOL`）。OTLP/HTTP JSON 的线形是公开规范：
  POST `/v1/traces`（`/v1/metrics` 同理），`Content-Type: application/json`，
  `resourceSpans → scopeSpans → spans` 三层嵌套，traceId 32 位 hex、spanId 16 位 hex，
  时间戳为纳秒十进制字符串，kind/status 为整数枚举，属性是带类型包装的 AnyValue。
  我们照此实现，但做三处改动：① 不拉 OpenTelemetry Go SDK——纯标准库手写 OTLP/HTTP JSON 导出器，
  与 F9 不拉 oauth2/oidc 依赖的取舍一致，零依赖单二进制不变；② 属性只取 allowlist 元数据
  （模型/渠道/密级/处置/Token 数/工具名等），提示词、工具参数、模型响应、DLP 命中值永不进 span
  （ContextForge 的 LLM 追踪默认会记 prompt， 我们按分级红线收紧）；③ 导出失败只计数不重试、
  不阻塞请求（遥测不值得为一次失败拖慢网关），关闭时 tracer 为 no-op（零开销）。
- 实现：新包 `internal/otel`（`Config` 读标准环境变量 + `Validate` 说明关闭原因；
  `Tracer`：span 队列上限 1000（满了丢弃计数）、5 秒批量导出（满 100 提前导出）、
  `traceparent` 入站加入/出站传播、累计 Sum 计数器；`Stats()` 供管理 API）；
  网关 `ChatCompletions` 每调用一个 SERVER span（`gateway.chat_completions`，经 statusRecorder
  取 HTTP 状态、defer 收尾，`traceparent` 透传给模型上游）+ 计数器
  `agentgate.gateway.calls{decision}` / `agentgate.gateway.tokens{channel}`；
  MCP `tools/call` 与 `prompts/get` 各一个 span（响应缓冲后判 JSON-RPC error，参数与结果
  永不进属性）+ 计数器 `agentgate.mcp.tool_calls{tool,decision}` /
  `agentgate.mcp.prompt_gets{decision}`；管理 API `GET /api/otel`（状态与计数）+
  `POST /api/otel/test`（发合成 span 同步导出验证连通性，未启用 503、导出失败 502，
  仅 admin/security）；`cmd/agentgate` 从环境变量建 tracer 并接线三处
  （网关/MCP/管理 API），配置错误只记日志、不阻止启动；管理台用量统计页新增
  OpenTelemetry 卡片（状态 + 测试按钮，中英双语）；详见 docs/opentelemetry.md。
- 测试：otel 包 8 项（配置解析与协议拒绝、traceparent 解析 6 类拒绝、nil/禁用 tracer
  no-op、导出 payload 形状：resource/scope 三层、纳秒时间戳、AnyValue 类型、累计 Sum、
  失败计数不致命、队列满丢弃计数、根 span 新 trace）+ 网关集成 4 项（干净调用 span
  含模型/处置/Token 数且请求标志串不出现在导出体中、上游收到同 trace 的 traceparent、
  违禁词拦截为 ERROR span 且违禁词不泄漏、入站 traceparent 被加入、指标含 calls+tokens）
  + MCP 集成 3 项（kb_search span 含工具名且参数不泄漏、受管工具拒自带级为 ERROR span
  含 RPC 错误码、tools/list 不产生 span）+ API 端点 4 项（未启用状态/员工 403、测试端点
  收到合成 span、未启用 503、collector 宕机 502 且失败计数）；另修一个测试自死锁：
  otel 测试曾在断言区持 collector 锁的同时调 counts/Flush（锁不可重入），改为快照后释放。
  `gofmt`/`go vet`/`go test ./...` 全绿；`make smoke` 153 项断言 0 失败（新增 8 项 F16 断言）。
- 已知注意点：只支持 OTLP/HTTP JSON，不支持 gRPC（4317）与 http/protobuf（需协议转换时中间放
  OpenTelemetry Collector）；导出失败的 span 直接丢弃不重试；无采样（调用量不大时全量导出），
  无直方图与日志信号。

### F19 知识库语义索引层 `done`（2026-11-22）
- 来源：Nathan 2026-11-22 批准立项（AOCI-CODE 调研结论）：aoci-spec/aoci-code 是给编码 Agent 用的代码库认知索引（Go 本地 MCP，模型逐文件写 FRAS 语义条目）。
- 适配：把「每对象一行语义条目（职责/关联/契约/约束）+ 标签坐标 + 索引治理（校验/漂移/覆盖率）」方法用于优化 F18 文件夹知识库的弱摘要（现为章节首句抽取式）；语义条目须模型生成，必须走网关分级路由/脱敏，绝密只给标题。
- 借鉴策略（只借方法不抄代码）：AOCI-CODE 许可证为 FSL-1.1-MIT（Fair Source，非 OSI 开源、有竞品限制），与本项目 AGPL-3.0 不兼容，代码禁止复制；只借鉴其方法设计，自研实现，并在条目与 DEVLOG 写明来源与许可证不兼容原因。
- 验收：每章节一行 FRAS 条目（F 职责 / R 关联 / A 适用范围 / S 硬约束）+ 标签坐标（部门 / 类型 / 重要度），每条带原文回指（文件 + 章节 + 页码）；条目格式校验、漂移检测、覆盖率报告齐全；模型生成走网关分级路由 + 脱敏，绝密只登记标题；MCP `kb_entries` 按等级门禁取用。
- 约束补充（2026-11-22）：语义条目必须带原文回指（文件 + 章节 + 页码），不许只保留模型生成的文字（借鉴 Liner 的引用可回指原则）。
- 实现：新包 `internal/semidx`（`Entry` + `Validate`：必填字段、FRAS 各 ≤500 字、标签值合法、回指完整；`Index`：内存索引、按文档内容哈希增量再生、漂移检测、覆盖率报告；抽取式生成器：职责=标题+摘要、关联=结构邻居、适用范围=文件夹范围、硬约束=密级取用规则；模型生成器：提示词只含章节标题与摘要、不含正文，调 chat-completions 端点、带 `X-AgentGate-Data-Level` 请求头走网关分级路由，输出逐条校验、回指由树填入、失败整篇回落抽取式并记 `model_error`，绝密直接 title-only 且生成器拒发）；`folderkb.DocSnapshot`（克隆树 + 文件夹名/部门/密级/指纹）供派生索引取用；`store.KBFolderSource` 增加可选 `Department`（种子演示文件夹：公司制度→人力部、机密资料→采购部）；管理 API `GET /api/kb/semantic/coverage` + `GET /api/kb/semantic/entries` + `POST /api/kb/folders/{id}/semantic`（`{"model": true}` 可内联端点与令牌，令牌只用于当次请求、不存储不回显；未配模型时 503），重扫/新建/删除文件夹时自动刷新或清理条目；MCP 新工具 `kb_entries`（与 `kb_structure` 同款等级门禁，机密条目自带级不可见）；管理台文件夹知识库页新增语义索引卡片（覆盖率 + 过期数）与每文件夹「生成语义索引」按钮（中英双语）；详见 docs/semantic-index.md。
- 测试：semidx 包 8 项（校验 12 类拒绝 + title-only 豁免、抽取式条目字段与标签、绝密 title-only、生成器失败回落、模型部分覆盖 mixed、增量再生与漂移、覆盖率、等级门禁、按源删除）+ 模型生成器 5 项（提示词只含索引不含正文、解析：回指由树填入/未知节点/重复节点/非法标签/围栏 JSON、假服务端：请求头含密级与令牌、回显提示词不解析、绝密零请求、上游 502 不回显响应体）+ API 端点 2 项（覆盖率与条目全链路、绝密 title-only、漂移出现与重扫清除、403/404/400、模型生成：真 JSON 成 model、回显回落 extractive + model_error、未配模型 503、令牌不回显）+ MCP 集成 3 项（BYOA 读内部文档条目、机密条目自带级不可见/受管级可见、tools/list 含 kb_entries）+ 冒烟新增 9 项断言；`gofmt`/`go vet`/`go test ./...` 全绿；`make smoke` 162 项断言 0 失败。
- 已知注意点：抽取式条目的关联/适用范围是结构与范围信息、不是内容语义，内容级 FRAS 需走模型生成；模型生成同步逐篇调用，大批量建议分文件夹触发；语义索引只在内存中，重启后启动扫描时重建。

### F20 受管 Agent 安装包分发与离线导入 `done`（2026-11-22）
- 来源：Nathan 2026-11-22 批准立项（dsh-ee 调研结论）：借鉴 dsh-ee（hikariming/dsh-ee，PR #309）的离线安装包导入与分发——管理员在线上传或离线导入受管 Agent 安装包，平台托管、员工自助下载；SHA-256 校验和验证并明确区分已校验 / 未校验；导入中断可恢复、过期包清理；分发包的哈希自动登记为 F14 证明的基准值来源，形成『平台分发 → 平台证明』闭环。
- 适配：双包结构（Nathan 2026-11-22 要求，待办登记 4）：管理包给 IT（agentgate 服务端，现有 Docker / 离线 bundle / systemd 形态，走现有离线部署通道），用户包给员工（ag-guard + ag-setup 在管理台按策略生成、装一次零配置常驻、包哈希作 F14 证明基准值）；F20 的分发与离线导入同时覆盖两个包。
- 交付形态补充（Nathan 2026-11-22 要求，待办登记 4）：明确区分双包（对齐 IP-guard 分法）——管理包给 IT（agentgate 服务端，现有 Docker / 离线 bundle / systemd 形态），用户包给员工（ag-guard + ag-setup 在管理台按策略生成、装一次零配置常驻、包哈希作 F14 证明基准值）；F20 的分发与离线导入同时覆盖两个包。现状：三个二进制已分开，但打包未分（make 一锅烩、离线包只有服务端），F20 补齐。
- 【借鉴策略标注】dsh-ee 为 Apache-2.0，与 AGPL-3.0 兼容，可借鉴设计与代码（复制须保留原版权声明并在 DEVLOG 记录来源项目、文件与许可证）；其实现与 DSH 插件绑定较深，本项目按自身架构实现为主。
- 借鉴点（dsh-ee PR #309「feat: DSH 分发支持导入内网离线安装包」，2026-11-22 查阅 PR 页标题；api.github.com 返回 403 未取到正文，设计点沿用 2026-11-22 当轮的调研结论）：离线导入（内网文件经平台托管再分发）、SHA-256 校验和验证并明确区分已校验 / 未校验、导入中断可恢复、过期包清理。我们照此实现，但做三处改动：① 校验状态直接决定基准值资格——未校验包永远不可登记（dsh-ee 只区分展示，我们把「未校验」变成门禁）；② 用户包由平台按策略生成（网关地址 / 密级 / 出口白名单 baked 进 guard.json），不是只托管上传的文件；③ 断点续传的会话与分片落盘，服务端重启后仍可续传。
- 实现：新包 `internal/distrib`（`Package`/`Upload` 记录 + `Registry` 文件管理：单次上传、断点续传会话、分片 offset 校验、完成时验算 SHA-256 定 verified/unverified、过期筛选、用户包 zip 生成：ag-guard + ag-setup + guard.json + MANIFEST.json，确定性条目顺序）；`store.State` 增加 `Packages` 与 `PackageUploads`（会话持久化，重启可续传）；`attest.Policy` 增加 `AllowedBinaryHashes`（F14 基准值）+ `Evaluate` 校验（原因码 `binary_hash_missing`/`binary_hash_not_allowed`，空列表保持旧行为）；管理 API（`internal/api/packages.go`）：单次 multipart 上传、断点续传（建会话/分片/状态/完成/中止）、员工下载（任意登录用户，流式 + `X-Package-SHA256`/`X-Package-Status` 头）、删除、登记基准值（仅已校验包，幂等）、清理过期包、生成用户包（两组件 + 策略，可一步登记基准值）；`make bundle-user`（`deploy/make-user-bundle.py`，离线组装同布局 zip 并打印 SHA-256）；管理台新增「安装包分发」页（上传表单、生成用户包表单、包列表含校验与基准值状态、下载/登记/删除/清理，中英双语）；详见 docs/package-distribution.md。
- 测试：distrib 包 7 项（元数据校验 10 类拒绝、单次上传已校验/未校验/校验和不符、断点续传：新 Registry 读盘续传、重叠与跳号分片拒绝、超大分片拒绝、未完成拒绝完成、删除与中止、过期筛选、用户包 zip 内容与清单哈希、生成校验 8 类拒绝、StoreBytes 哈希复验）+ attest 1 项（二进制基准值：通过/大小写折叠/未知降级/缺失降级/无基准值保持旧行为、策略校验）+ API 端点 4 项（上传全链路：已校验/未校验/校验 400/员工下载与匿名 401/基准值登记与策略落盘/幂等/删除后 404/员工删除 403、断点续传全链路、用户包生成与 zip 内容/策略/基准值闭环/校验 400/员工 403、过期清理与过去过期时间拒绝）+ 冒烟新增 16 项断言；`gofmt`/`go vet`/`go test ./...` 全绿；`go test -race`（distrib/attest/api/store）全绿；`make smoke` 178 项断言 0 失败（基线 162 + 新增 16）。
- 已知注意点：校验只验 SHA-256（内容完整性），不验厂商签名；组件包本身不校验「确实是 ag-guard/ag-setup 二进制」（平台只保证 zip 里装的就是管理员指定的两个文件）；证明仍是软件自报证据，基准值保证「装的是平台发的包」，不防有意伪造（见 docs/attestation.md）。

### F21 组织架构同步与 IM 身份绑定 `done`（2026-11-22）
- 来源：Nathan 2026-11-22 批准立项（dsh-ee 调研结论）：借鉴 dsh-ee（2026-11-20）的钉钉组织同步与飞书 / 钉钉身份绑定——从钉钉 / 飞书 / 企业微信同步部门与员工目录，登录时把第三方身份绑定到平台账号，与 F9 OIDC 并行；同步只建 / 更新账号与部门映射，不自动授予受管级权限（权限仍走平台策略）。
- 适配：三个平台统一到一个 `DirectoryClient` 接口 + 一个 `Syncer`；企业微信是 dsh-ee 没有的平台，按官方通讯录 API 形状新写（`gettoken` / `department/list` / `user/list?fetch_child=1` / `user/get`）。
- 红线：第三方应用密钥只加密存储、不回显；同步与绑定全程审计。
- 【借鉴策略标注】同 F20：Apache-2.0，借设计为主，复制代码保留版权声明并记 DEVLOG。
- 借鉴点（dsh-ee 源码，hikariming/dsh-ee，Apache-2.0，2026-11-22 查阅：`server/internal/dingtalk/client.go` 与 `directory.go`、`server/internal/api/dingtalk_sync.go`、`server/internal/feishu/client.go`）：
  钉钉：`POST /v1.0/oauth2/accessToken` 取应用 token（缓存至过期）→ `POST /topapi/v2/department/listsub`（根部门 1）+ `POST /topapi/v2/user/list`（cursor/size ≤ 100）广度优先拉整份通讯录、`{errcode,errmsg,result}` 信封、token 放 query 且报错信息里打码；`POST /topapi/v2/user/get` 单查，60121 为离职、50002/50004/60011 为权限范围外。
  飞书：`POST /open-apis/auth/v3/tenant_access_token/internal` 取 tenant token → `GET /open-apis/contact/v3/departments/parentchildren` + `GET /open-apis/contact/v3/users/find_by_department`（page_token 分页）；单查错误码 41012 查不到、41050 权限范围外。
  同步应用层（`dingtalk_sync.go`）：整份目录才生效（拉取失败或目录为空整轮不生效）、外部主体 `dingtalk:<corpId>:<userId>`、邮箱自动关联已有账号（企业邮箱优先）、`AutoCreate` 门控建号、本地已禁用不自动恢复、离职禁用前逐个向平台确认（目录里没看到 ≠ 离职）、关联在另一来源账号上的身份只解除关联不禁用账号、每轮记一条汇总审计、最近一次同步结果持久化、每小时定时同步（带租约）。
  我们照此实现，但做四处改动：① 三个平台统一接口与同步器，企业微信新增（dsh-ee 没有）；② 审计汇总只含计数，用户名列表只在同步结果里、不进审计（dsh-ee 的审计含被禁用户名）；③ 本轮只做手动同步，不做每小时定时任务（留待后续轮次）；④ 绑定须以同步快照为准（快照里没有的第三方 ID 拒绝绑定），dsh-ee 的绑定走登录时的 OAuth 换码，我们的绑定走同步后的快照校验 + 管理员手动绑定。
- 复用检查（Nathan 复用优先指示）：未逐字复制 dsh-ee 代码——其客户端绑定自家 store/OIDC 类型与 TiDB，端点形状、信封处理、分页行走、离职确认与同步规则按上述借鉴点改写为本仓库的 store/secretbox 类型（约 1400 行，三客户端 + 同步器均为纯标准库）；来源项目、文件与许可证记于 `internal/orgsync` 包注释、backlog 本条与 DEVLOG。
- 实现：新包 `internal/orgsync`（`Config` + 三客户端 dingtalk/feishu/wecom：token 缓存、信封校验、分页行走、UserState 三态；`Syncer`：建号/邮箱自动关联/刷新/离职确认与禁用/解绑关联身份/快照）；`store.User` 增加 `Email` 与 `ExternalIdentities`（`{provider, external_id, subject}`），`store.State` 增加 `OrgProviders`（`SecretEnc json:"-"` + 打码显示）；`auth.RevokeUserSessions`（离职禁用时吊销会话）；管理 API（`internal/api/orgsync.go`）：来源 CRUD（仅 admin，密钥加密存储不回显）、`POST .../sync`（返回结果、写汇总审计）、`POST .../test`（只计数、不建号、不写审计）、绑定/解绑/本人绑定列表（绑定须快照校验、同一身份不许双绑）；管理台新增「组织同步」页（来源添加/列表/同步/测试/删除 + 绑定表单 + 我的身份，中英双语）；详见 docs/org-sync.md。
- 测试：orgsync 包 13 项（钉钉：目录分页 + 去重 + 排序 + token 缓存 1 次、业务错误不泄漏 token、三态确认；飞书：目录分页 + 企业邮箱优先、三态确认；企微：扁平部门 + fetch_child + token 缓存、三态确认；配置校验 7 组；同步器：建号与再同步对账、自动建号关时跳过、邮箱自动关联、已禁用不恢复、空目录中止、拉取失败零改动、离职禁用三态：确认离职禁用 + 吊销会话、关联身份只解绑、范围外保留、LastSync 落盘）+ API 端点 5 项（来源 CRUD 全链路、密钥永不回显、403/404/400；同步全链路：测试只计数不建号、同步建号 3 人 + 再同步对账 + LastSync + 汇总审计只含计数；停用来源同步 400；绑定全链路：快照校验、双绑 409、未知 ID 409、本人绑定列表与解绑；双来源同外部 ID 时绑定选空闲主体）+ 冒烟新增 12 项断言；`gofmt`/`go vet`/`go test ./...` 全绿；`go test -race`（orgsync/api/auth/store）全绿；`make smoke` 190 项断言 0 失败（基线 178 + 新增 12）。
- 本轮实测抓到的真问题：① 绑定初版按平台类型取第一个快照里有该外部 ID 的来源——两个同类型来源（两家企业）都有 u1 时，map 随机顺序会命中已被他人绑定的主体而误报 409。已改为收集全部候选主体（按来源 ID 排序）、跳过已被他人绑定的、选空闲的；全部被占时才 409。② 同一原因的另一面：同步的邮箱自动关联会在第二个来源同步时就把同一人绑过去（dsh-ee 的跨来源关联本就是这个语义），冒烟与回归测试因此改用无邮箱的 u3 覆盖「选空闲主体」分支。③ 解绑初版只认用户 ID、管理台传用户名时 404，已与绑定端统一为用户名或 ID 都接受。
- 已知注意点：本轮只做手动同步，无定时任务；飞书用 `open_id`（每应用唯一，换应用需重新同步）；企微用根部门 `fetch_child=1` 一次拉全量，超大通讯录后续可改分页；同步不授予任何权限，能力授权仍以令牌等级与策略为准。

### F22 专题知识包 `done`（2026-11-22）
- 来源：Nathan 2026-11-22 批准立项（Liner 调研结论）：借鉴 Liner（linersh，liner.sh）的调研包形态——管理员或部门专家围绕一个具体工作问题，把若干文件精选成一个『知识包』：来源清单（配方）+ 综合说明（用途 / 证据用法 / 边界）+ 密级与授权范围，审批发布后 Agent 经 MCP 按包取用（目录 → 综合 → 原文下钻），综合中每条结论必须带原文回指（文件 + 章节 + 页码）。维护走 Agent 起草 → 管理员审批 → 变更回执进审计；包可引用其他已发布包的综合稿，但密级只能向下引用（低密级包不得嵌入高密级内容）；包的密级取所含文件最高密级。
- 适配：F18 之上的策展层（图书馆 vs 案卷之别，价值在口径受控、经验沉淀、责任可追溯）。
- 验收：管理员可新建知识包（来源 + 综合 + 引用回指）并走审核发布；Agent 经 MCP `kb_pack_list` / `kb_pack_get` 按包取用（综合 → 来源原文下钻）；每版至少一条回指且回指由索引树填入（不存在的章节在创建时即被拒绝）；引用只能向下（内部包引用机密包被拒，机密包引用内部包通过）；被引用包撤回或改发更高密级后，读取时该引用按名扣留、不静默提供；每次创建 / 加版 / 审核 / 发布 / 删除都记审计。
- 【借鉴策略标注】Liner 为 MIT 许可证，与 AGPL-3.0 兼容；借其形态与策展流程（配方 / 编译分离、审批关口、变更回执），代码按本项目架构自研为主，复制须保留版权声明并记 DEVLOG。Liner 本身是个人单机工具，无多用户 / 分级 / 审计，治理层全部自研。
- 借鉴点（Liner 官方 npm 包页与 liner.sh 变更日志，2026-11-22 查阅）：
  tape.yaml 是来源配方，Mixtape 是编译产物（synthesis.md + sources/ + MIXTAPE.md），Liner Project 是围绕产物的文件夹；Agent 按 `liner research` 逐步建包，在综合、来源清单与说明三处停下等人工批准，完成后写 LINER.md / SKILL.md / liner.yaml 说明用途、证据用法与边界；`liner catalog` / `liner search` 跨库检索，导航器可从引用跳到编译后的本地来源。
  我们照此实现，但做四处改动：① 配方与综合同为版本的一部分（每版 = 配方 + 综合 + 回指），版本追加式不可变、发布才生效，与 F8 模板同款；② 密级是算出来的（来源与引用包的最高密级），不是填出来的；③ 回指由索引树填入（文件 + 章节 + 页码），不接受作者手写回指；④ 审批关口收进平台角色（起草任何员工可做，审核 / 发布 / 删除仅 admin/security），每次变更都记审计（变更回执）。
- 复用检查（Nathan 复用优先指示）：未复制 Liner 代码——其核心是 CLI + Go TUI，与本仓库的服务端架构不匹配；包模型、真相校验、API 与 MCP 约 1300 行纯标准库自研；分级与版本语义复用本仓已有逻辑（F8 的版本/发布流程、F18 的文件夹索引树、F19 的「回指由树填入」）。
- 实现：`store.KnowledgePackage` + `PackageVersion`（配方 `sources` + 引用 `references` + 综合 `content` + 回指 `citations` + 变更说明 `change_note` + 创建时算好的 `level`）+ `ValidatePackageVersion`（形状门：内容 ≤32 KiB、至少 1 来源与 1 回指、无重复来源/回指/引用）；新包 `internal/knowpack`（真相门：`ResolveVersion` 对照实时索引校验来源存在、引用已发布且向下、回指由树填入并算密级；`CheckReferences`/`CheckSources` 供管理台 `stale` 提示与 MCP 读取时扣留；`PublishedSynthesis` 供 MCP 内联被引用综合）；管理 API（`internal/api/knowpack.go`）：列表/创建/详情/加版/审核/发布/删除，发布前对照实时索引重验配方（失效 409）；MCP 新工具 `kb_pack_list` / `kb_pack_get`（仅已发布 + 本等级可见 + 部门范围内，引用综合内联、失效引用按名扣留）；管理台新增「专题知识包」页（新建/列表/审核/发布-回滚/删除/加版，含配方过期提示，中英双语）；演示种子 `onboarding_essentials`（新员工入职必读，internal，已发布 v1，来源为考勤与请假制度.md 全文，综合概括请假/加班/考勤三节、回指 n1/n2/n3）；详见 docs/knowledge-packages.md。
- 测试：knowpack 包 5 项（版本解析：回指由树填入/未知来源与章节/配方外回指/节点级来源内外、引用规则：向下通过/向上拒绝/机密引内部/自引用/未知/未发布、引用与来源的过期检查、已发布综合取用与等级扣留、版本形状校验 7 类拒绝）+ API 端点 1 项（创建→审核→发布→加版→回滚全链路、员工起草与加版权限、引用规则、发布时重验（撤回引用 409 + stale 提示 + 恢复后发布）、删除、400/403/404/409）+ MCP 集成 1 项（列表分级过滤、综合与回指、按名/id 取用、机密包对自带级隐藏、引用综合内联、撤回引用按名扣留、草稿与未知包不可区分）+ 冒烟新增 14 项断言；`gofmt`/`go vet`/`go test ./...` 全绿；`go test -race`（knowpack/api/mcp/store）全绿；`make smoke` 204 项断言 0 失败（基线 190 + 新增 14）。
- 已知注意点：引用有效性只在创建 / 发布 / 读取三处检查——被引用包改发更高密级后，引用包在下次读取时才扣留该引用，不自动下线引用包（管理台 `stale` 提示策展人修订）；来源文档被删除后，已发布综合仍可读（回指已在创建时填好），但 `kb_section` 下钻会 404；综合正文本轮只做人工策展 + 结构校验，不走模型生成（模型代写综合须走 F19 同款分级路由与脱敏，留待后续轮次）；「每条结论都有回指」是策展纪律（起草说明 + 管理员审核关口），机器只保证每条回指真实存在，不做 NLP 检查。

### F23 终端持续盘点与断网策略缓存 `done`（2026-11-22）
- 来源：Nathan 2026-11-22 批准立项（开源类 IP-guard 工具调研结论）：把 F14 的一次性证明升级为持续证明——ag-guard 定期心跳上报版本 / 配置哈希 / 插件清单 / 在线状态，平台维护全量终端 Agent 资产视图（管理台终端清单页：按部门 / 版本 / 在线状态筛选，配置漂移标红）；同时 ag-guard 本地缓存策略，网关不可达时按缓存策略执行、高密级默认拦截（fail-closed），恢复后补报断网期间审计。借鉴：osquery / Fleet 的终端可见性模式（Fleet 核心 MIT，可借鉴设计）；IP-guard 的离线策略缓存。
- 验收：ag-guard 心跳后终端出现在管理台清单中，版本低于策略最低版本时漂移标红并告警；网关断网时公开请求报 502 可重试、机密请求报 503 明确拒绝；恢复后断网期间的决策经心跳补报进审计（只含计数与类别）；策略刷新后端侧词表即更新，缓存文件被篡改时忽略。
- 借鉴点（Fleet 官方发布说明 fleetdm.com/releases/fleet-3-12-0，2026-11-22 查阅；osquery 心跳策略模式，见同站 DeepWiki 综述）：
  Fleet 的主机 vitals 是服务端按需可刷新的设备状态：每个主机定期上报 vitals，管理台按最后上报时间推导新鲜度，并可对单个主机点「Refetch」拿到权威的最新值；osquery 侧的落地形态是策略查询——如 `inventory-heartbeat-age-seconds.sql`（距上次心跳的秒数）与 `beacon-version.sql`（已装版本）这类查询即资产盘点的原子操作，配 Labels（按 IdP 部门分组）做筛选。
  我们照此实现，但做三处改动：① 不做通用设备盘点、只盘 Agent：一条记录即（用户，Agent 类型），vitals 只有证明相关的六项（版本 / 配置哈希 / 二进制哈希 / 插件 / 守卫 / 系统），不采 OS 补丁、磁盘等通用设备信息；② 新鲜度不用查询、直接由心跳到达时间推导（5 分钟 online / 60 分钟 offline），Refetch 即下一次心跳；③ 漂移不用策略查询判定、直接用 F14 已有的证明评估——心跳即一次持续证明。
  IP-guard 的离线策略缓存无公开 API 文档，模型沿用本仓 `docs/integrations/ipguard.md` 的配合结论（终端本地缓存策略、断网按缓存执行）。
- 复用检查（Nathan 复用优先指示）：未复制 Fleet/osquery 代码——Fleet 是 Go+MySQL+Redis 的整套设备管理平台（MIT），osquery 是 C++ 端点探针，两者都与本仓库的单二进制 + 令牌认证架构不匹配；心跳、漂移评估、策略快照与离线缓冲约 900 行纯标准库自研；漂移评估复用本仓已有 `attest.Evaluate`（F14），词表检测复用 `internal/guard`（F15）。
- 实现：新包 `internal/terminal`（`Heartbeat` + `Validate`：形状校验复用证明报告规则；`PolicySnapshot`：快照内容 SHA-256 作版本、词表排序后定版、篡改校验；`DriftReasons`：无策略不漂移、有策略走 `attest.Evaluate`、只返原因码；`Terminal` 记录 + `Status`：5 分钟 online / 60 分钟 offline）；`store.State` 增加 `Terminals`（按 ID，键为（用户，Agent 类型））；管理 API（`internal/api/terminals.go`）：`POST /api/terminals/heartbeat`（令牌认证，Agent 类型与等级取自令牌、会话调用退回请求体；只审计注册/漂移出现/漂移消除/断网补报四类变迁，不记每次心跳）、`GET /api/terminals`（部门/Agent/版本/状态/漂移筛选，按最近心跳倒序）+ `GET /api/terminals/{id}`、`GET /api/sidecar/policy`（任意有效令牌可取，快照只含词表与 dry-run 标志）；`alert.FromEvent` 新增 `terminal_drift`（warning）；sidecar（F15 之上）：`Config` 增加 `agent_type`/`plugins`/`heartbeat_interval_seconds`（0=默认 60 秒，负数禁用）/`policy_cache_path`（默认与 guard.json 同目录），`Server` 增加策略快照（来源 local/gateway/cached）+ 检测器热换（读写锁）+ 心跳循环（借用代理请求里见到的令牌，只存内存）+ 断网决策缓冲（只含计数与类别）；代理请求时最多每 5 分钟后台刷新一次策略（成功换检测器并落盘 0600，失败保持旧策略，篡改缓存忽略）；转发失败时按缓存策略执行：密级 ≤ 快照 offline_max_level（默认 internal）记 `offline_unreachable` 返回 502，之上记 `offline_blocked` 返回 503（fail-closed），dry-run 永不拦截；心跳成功时清空缓冲；`GET /status` 新增策略版本/来源/刷新时间、心跳状态与待补报数；`ag-guard` 增加 `-agent-type`/`-plugins`/`-heartbeat-seconds`/`-policy-cache`（与同名环境变量），`cmd/agentgate` 接线 `SetSidecarPolicy`（快照取网关自身词表，`AGENTGATE_OFFLINE_MAX_LEVEL` 可调上限，默认 internal）；管理台新增「终端清单」页（筛选 + 列表，漂移标红，中英双语）；详见 docs/terminal-inventory.md。
- 测试：terminal 包 4 项（快照版本：词表顺序无关/内容变化/篡改校验/上限回退、离线放行梯度、心跳校验 8 类拒绝、漂移评估：无策略/低于最低版本/通过/禁用插件、状态阈值）+ API 端点 1 项（策略快照 401/内容、令牌心跳注册/漂移出现与消除/无策略 Agent 永不漂移/断网补报审计内容、五类筛选、员工 403、会话心跳回退路径）+ alert 分类 1 项（`terminal_drift` 为 warning，`terminal_registered` 不告警）+ sidecar 5 项（策略刷新换检测器且配置哈希变化、缓存落盘与重启加载、篡改缓存忽略且启动词表仍生效、断网 fail-closed：公开 502/机密 503/计数与缓冲、心跳补报含 2/1/1 计数与类别且缓冲清空、心跳体含 Agent/版本/插件/二进制哈希/系统）+ 冒烟新增 14 项断言；`gofmt`/`go vet`/`go test ./...` 全绿；`go test -race`（terminal/api/sidecar）全绿；`make smoke` 218 项断言 0 失败（基线 204 + 新增 14）。
- 已知注意点：证明仍是软件自报证据（与 F14 同款边界），心跳防配置漂移与版本落后、不防有意伪造；断网决策缓冲只在内存中，sidecar 在断网期间重启会丢失尚未补报的条目（策略缓存落盘，不受此限）；在线状态由心跳到达时间推导，阈值（5 分钟/60 分钟）为常量未做成可配项；令牌过期或被吊销后心跳 401，终端 60 分钟后显示 offline，不主动告警。

### F24 高密级调用例外审批流 `done`（2026-11-22）
- 来源：Nathan 2026-11-22 批准立项（开源类 IP-guard 工具调研结论）：借鉴 IP-guard 外发解密审批工作流——被分级策略拦截的调用，员工可发起例外申请（理由 + 范围 + 时效），管理员在管理台审批；通过后发一次性放行凭证（绑定该员工 / 该次调用特征 / 有效期），放行与审批全程审计 + Webhook 通知。红线：绝密默认不可例外（须显式开启），放行不改变分级策略本身。
- 验收：员工申请 → 管理员批准发一次凭证 → 凭证放行一次被拦截的调用、第二次复用失败；违禁词拦截带凭证仍被拦；绝密申请默认 400；申请/批准/使用全程审计。
- 借鉴点（域智盾外发文件审批流程说明 cnblogs.com/cksd/p/23705120，2026-11-22 查阅；IP-guard 本身无公开 API 文档，流程形态以同类 DLP 产品说明为准）：
  外发审批的标准闭环是「员工先申请并说明用途 → 管理员按文件类型/用途/接收范围审核 → 通过后对本次外发解密或放行、驳回则不予放行 → 全程留痕」；关键约束有二：① 审批通过对应本次申请和本次外发需要，不等于此后可随意（后续再次外发仍须重新申请）；② 受控外发可设有效期与打开次数。
  我们照此实现，但做三处改动：① 放行凭证不是解密文件、而是一次性调用凭证（随机 token，服务端只存 SHA-256，批准响应里只返回一次），绑定申请人 + 密级 + 有效期 + 一次性，网关以 `X-AgentGate-Exception-Grant` 请求头核验；② 违禁词拦截永不例外（同类产品里解密申请也只针对加密文件、不针对违规内容）；③ 绝密默认不可例外，须运维显式开 `AGENTGATE_ALLOW_TOPSECRET_EXCEPTIONS=1`。
- 复用检查（Nathan 复用优先指示）：未复制代码——同类 DLP 产品的审批流无开源实现可直接复用（IP-guard/域智盾均为闭源商业产品），流程形态只借方法；约 600 行纯标准库自研（模型与校验、网关核验与消费、管理 API、管理台页）。
- 实现：新包 `internal/exception`（`ValidateRequest`：仅机密以上可申请、理由/范围必填、有效期 1–72 小时、绝密须 opt-in；`NewGrant`/`HashGrant`：随机凭证 + SHA-256；`FindValidGrant`：哈希 + 申请人 + 密级覆盖 + 未过期 + 未使用五项核验；`RefreshStatus`：过期 lazily 置 expired）；`store.Exception` + `State.Exceptions` 持久化；网关（`internal/gateway/exception.go`）：违禁词拦截之后才查凭证（凭证永不豁免违禁词），豁免两处分级拦截——自带级禁发机密以上、与无合规通道（无本地模型时绝密可走专有云/VPC 一次），合规路由优先、仅在策略否则拦截时才动用凭证；消费点在选路完成即将转发时、存储锁内二次校验（并发复用只放行一次，败者 403 `exception_grant_invalid`），无效凭证（未知/过期/已用/他人/密级不足）在需要豁免时才 403，不需要豁免的调用不受影响、凭证也不消耗；放行仍按信任梯度脱敏（签约云强制脱敏），`model_call` 审计带 `exception_id`；管理 API（`internal/api/exceptions.go`）：申请/列表（管理员看全部、员工只看自己的）/详情/批准（返回凭证一次）/驳回/撤销（已批准未使用），审计只含元数据不含理由与范围原文；`alert.FromEvent` 新增 `exception_requested`/`exception_approved`（warning）；管理台新增「例外审批」页（申请表单 + 列表 + 批准/驳回/撤销，批准后凭证只显示一次，中英双语）；`AGENTGATE_ALLOW_TOPSECRET_EXCEPTIONS=1` 开启绝密例外（默认关闭）；详见 docs/exception-approval.md。
- 测试：exception 包 4 项（校验 8 类拒绝、凭证哈希往返、核验 7 类拒绝、过期翻转）+ API 端点 1 项（401/5 类 400/申请→批准→凭证仅返回一次→重复审批 400→撤销→驳回→store 只存哈希）+ 网关集成 5 项（BYOA 豁免且签约云仍脱敏、复用 403、绝密无本地模型时豁免走专有云且原文直达、无效凭证三类 403、违禁词不豁免且凭证不消耗、合规调用不消耗凭证）+ alert 分类 1 项（申请/批准告警，驳回/使用不告警）+ 冒烟新增 9 项断言；`gofmt`/`go vet`/`go test ./...` 全绿；`make smoke` 227 项断言 0 失败（基线 218 + 新增 9）。
- 已知注意点：`scope` 是给审批人看的说明文字，网关不做机器匹配，强制绑定是申请人 + 密级 + 有效期 + 一次性；转发后上游故障（502）不退还凭证，须重新申请；凭证原文丢了只能重新申请（服务端不存原文）。

### F25 合规周期报表 `done`（2026-11-22）
- 来源：Nathan 2026-11-22 批准立项（开源类 IP-guard 工具调研结论）：面向等保 / 保密评审的周期报表——按周 / 月生成（调用量与 Token、拦截与脱敏统计、Top 风险事件、渠道健康、令牌与终端概况），管理台可预览、导出表格；与 F5 告警构成『审计有人看』闭环（对应弱点清单 #8）。报表只含统计与元数据，不含敏感原文。
- 借鉴点（Wazuh 合规报表形态，2026-11-22 查阅 cenovacyber.com 的 Wazuh 审计证据文章与 Wazuh 官方文档综述）：
  Wazuh 的做法是规则映射到框架（PCI DSS、HIPAA、GDPR、NIST 800-53、TSC 等），按框架出仪表盘与报表、供审计师取证；其文档明确说明映射只是证据导航、不是合规认证——平台能帮你收集整理技术证据，不能替你合规。另有社区做法是定时任务出 JSON/HTML/CSV 三种格式的报表。
  我们照此实现，但做三处改动：① 不做框架条款映射——我们没有等保条款对照表，报表只给事实（统计与元数据），不下「通过/不通过」的结论，框架映射留待后续轮次评估；② 报表是冻结快照（生成时聚合 + 生成时刻的平台状态），不是实时仪表盘——评审要的是某一周期盖了章的数字，不是随时会变的看板；③ 导出只有 CSV 一种扁平表（section/key/calls/tokens/blocked/cost_usd），评审进表格即用，JSON 预览走 API。
- 复用检查（Nathan 复用优先指示）：未复制 Wazuh 代码——Wazuh 是 GPLv2 的整套 SIEM/XDR（Python/Elasticsearch/Kibana），与本仓库的单二进制 + 审计日志架构不匹配；报表聚合约 500 行纯标准库自研（report 包）+ API 与管理台页；用量口径复用 F2 的事件分类（model_call + violation/model_call_blocked），与 /api/stats 同口径可对账。
- 实现：新包 `internal/report`（`Report` + `Generate`：窗口内审计事件聚合 + 生成时刻快照（渠道健康/令牌/终端概况/链完整性）；`PeriodWindow`：周报表为 ISO 周一至下周一、月报表为自然月（UTC）；`WriteCSV`：扁平表导出）；`store.State` 增加 `Reports`（最多保留 52 份，超了删最旧）；管理 API（`internal/api/reports.go`）：`POST /api/reports`（生成，period 必填 weekly|monthly、date 可选 YYYY-MM-DD，缺省今天）、`GET /api/reports`（列表摘要）、`GET /api/reports/{id}`（预览）、`GET /api/reports/{id}/export`（CSV 下载）、`DELETE /api/reports/{id}`（均仅 admin/security，生成/导出/删除记审计，只含报表 ID 与周期）；管理台新增「合规报表」页（生成表单 + 列表 + 预览 + 导出 CSV + 删除，中英双语）；详见 docs/compliance-reports.md。
- 测试：report 包 4 项（周期窗口：周/月/闰年二月/非法周期报错、生成全量：用量/脱敏/DryRun/拦截原因与发现类别/Top 风险/例外流/窗口外事件不计、空窗口零值且切片非 null、CSV 形状与无敏感值泄漏）+ API 端点 2 项（生成→列表→预览→导出 CSV→删除全链路、401/403、400 校验、未知 ID 404、保留 52 份上限）+ 冒烟新增 10 项断言；`gofmt`/`go vet`/`go test ./...` 全绿；`make smoke` 237 项断言 0 失败（基线 227 + 新增 10）。
- 已知注意点：报表数字只覆盖窗口内的热审计日志——F4 归档把事件移出热日志后，已归档的旧事件不再进入新生成的报表（归档默认只动 180 天前的事件，不影响当周/当月报表）；已生成的报表是冻结快照，事后归档不改写它；报表不含框架条款映射与「通过/不通过」结论；令牌与终端概况是生成时刻的点状态，不是周期内的变化历史。

### F28 语义复核层 `done`（2026-11-23）
- 来源：Nathan 2026-11-22 批准立项（竞品功能对照 2026-11-22）：语义检测是全行业标配而本项目缺失——奇安信为『规则引擎 + GIS 安全小模型』双层，深信服为『快分类 + 深度研判』双模型，360 有风险检测大模型，安恒能做 ToolCall 语义研判。
- 适配：在现有规则 DLP 之上增加可选的第二层语义研判：规则未命中但特征可疑（变相表达、拆分、编码、提示注入语义）的请求送语义复核，复核可拦截 / 告警 / 放行，全程审计。借鉴上述厂商公开的双层检测形态（借设计思想，实现自研）。
- 红线：① 复核通道本身走分级路由，待复核内容按密级选通道，绝密级不送任何模型复核、只走规则硬拦（fail-closed）；② 能脱敏先脱敏再送审；③ 复核层可独立开关，关闭时行为与现版本完全一致；④ 超时策略可配置且默认保守（超时按高密级 fail-closed、低密级告警放行）。
- 排序：排在 F26 之后、F27 之前。
- 路线补充（2026-11-22，Nathan 批准，待办登记 7，1Panel AI 网关调研结论）：双路线——Embedding 样本匹配为默认层（本地小向量模型 + 风险样本库 + 决策日志，不调大模型、不出站），模型复核为可选深层；样本匹配绕开「复核通道本身可能泄密」的红线（待复核内容不送任何模型）。深层首选候选为 Higress 的 Qwen3Guard 式自托管小模型（待办登记 8，2026-11-22 批准）。
- 借鉴点（2026-11-23 查阅一手来源）：① 1Panel AI 网关（飞致云论坛 bbs.fit2cloud.com/t/topic/19664 与官方博客 v1.6.0，2026-11-06）：内容合规是「敏感词 + 语义审核 + 内容审计」三层；向量语义审核把请求转语义向量、与预置样本库算相似度，样本未命中时规则兜底，判定写入决策日志；v1.6.0 接入 Qwen3Guard 安全围栏模型（Safe / Controversial / Unsafe，「有争议」与「不安全」视为风险命中），三层审核可独立启停、组合使用；「阻断」模式下审核服务不可用时不放行未审核内容。② Higress + Qwen3Guard（阿里云博客）：Qwen3Guard-Gen 以 Wasm 插件接入网关，默认调用 `Qwen/Qwen3Guard-Gen-4B`，暴露 OpenAI 兼容 `POST /v1/chat/completions`，三档风险级别映射为两档网关策略（只拦 Unsafe / 连 Controversial 也拦），模型自托管部署。③ API7 文档：Qwen3Guard 为 Apache-2.0，Gen 家族 0.6B/4B/8B，回答为 `Safety: Unsafe` / `Categories:` 纯文本。我们照此实现，但做三处改动：① 默认层用确定性 n-gram 向量（字符 trigram + 词 unigram，纯标准库）代替神经网络 embedding——单二进制零依赖定位装不下 embedding 模型，`Vectorize` 可替换，诚实边界写进 docs/semantic-review.md；② 深层只审输入不审输出；③ 超时与深层故障按密级分流（机密以上拦截、低密级告警放行），而非一律不放行。
- 复用检查（Nathan 复用优先指示）：未复制代码——1Panel 为闭源商业产品、Qwen3Guard 只借其 OpenAI 兼容的调用与回答格式（Apache-2.0 协议也无代码可抄），深层客户端与样本匹配器约 630 行纯标准库自研（`internal/semreview`）；复用本仓已有 `internal/guard` 的脱敏、`store.Channel` 的渠道与密钥轮询、F17 的信任梯度校验、F5 的告警、F14 的审计事件。
- 实现：新包 `internal/semreview`（`Matcher`：字符 trigram + 词 unigram 向量化，余弦相似度 + 包含分支（长请求里原样嵌入的越狱仍命中），默认阈值 0.35；`DeepClient`：OpenAI 兼容端点调用，解析 `Safety:` / `Categories:`，Unsafe 拦截、Controversial 按策略处置、Safe 放行；`ParseGuardAnswer`）；新存储 `internal/store/semreview.go`（`SemanticReviewSample` + `SemanticReviewConfig`，默认策略 enabled=true、阈值 0.35、超时 3000ms、争议=告警、深层关闭，未配置时返回内置默认值，种子样本 9 条：越狱 4 / 拆分外发 2 / 诱导 2 / 伪装 1，其中诱导类「委婉地问怎么绕过审批流程」为告警样本）；网关接入点在 `ChatCompletions` 脱敏之后、缓存之前（被拦的请求永不进缓存，例外凭证不豁免复核拦截、不消耗凭证，DryRun 只记事件不拦截）；复核文本是脱敏后的副本（自带级未脱敏与 DryRun 也先脱敏再送审）；深层调用前校验渠道存在 / 启用 / 未冷却 + `trustAllowed` + 密钥轮询（F12 同款），绝密跳过整个复核层；审计：拦截 `violation`/`model_call_blocked`（`semantic_review_blocked` + `semantic_source`/`semantic_sample`/`semantic_similarity`，不含请求原文）→ F5 关键告警，告警记 `semantic_review` 事件 → F5 警告告警，放行记 `model_call` 的 `detail.semantic_review`，DryRun 记 `semantic_review_dry_run`；运行时计数 8 项经 `GET /api/semantic-review` 返回；管理 API 四条（策略查 / 改、样本增 / 删，admin/security）；管理台「语义复核」页（策略表单 + 计数 + 样本库增删，中英双语）；详见 docs/semantic-review.md。
- 测试：semreview 包 12 项（匹配：阈值边界、包含分支、脱敏文本、长请求稀释、样本校验；深层：回答解析 6 组、渠道配置校验）+ 网关集成 9 项（样本拦截与审计不含原文、告警样本放行、放行记 `sample:allow`、关层行为一致、绝密跳过深层（假守卫 0 次调用）、深层 Unsafe 拦截 / Safe 放行、深层超时机密拦截 / 内部告警放行、tool_calls 参数同权、深层只收到脱敏文本含 `PII_` 占位符、无原始手机号）+ API 1 项（端到端策略 / 样本增删、员工 403、非法值 400、深层渠道不存在 400）；阈值 0.35 来自实测调参（应命中改写 0.38–0.69、正常提示最高 0.28、重度改写 0.21–0.33 留给深层）；`gofmt`/`go vet`/`go test ./...` 全绿；`go test -race`（semreview/gateway/api/store/alert）全绿；`make smoke` 266 项断言 0 失败（基线 258 + 新增 8）。
- 已知注意点：只审输入、不审模型输出（输出审核需在流式与非流式回程加缓冲，留待后续轮次）；样本匹配是 n-gram 近似、不是语义理解（重度改写与编码后的风险意图低于阈值，深层未启用时会放行，规则 DLP 仍在）；深层每次调用一次审核端点往返（默认超时 3s），超时按保守策略处置、不重试；样本库在网关进程内存中现算向量，库很大时（数千条）匹配成本上升，当前 9 条可忽略；冒烟调试：F2 的 stats 断言是精确计数（3 调用 / 2 拦截），F28 的 1 次拦截 + 3 次放行断言须放在该断言之后，另用新令牌以免干扰 F3 的 RPM 计数。

### F29 DLP 归一化管线 `done`（2026-11-23）
- 来源：Nathan 2026-11-22 批准立项（1Panel AI 网关调研结论，待办登记 7）：1Panel 的敏感词归一化管线（谐音 / 拆字 / 拼音变形识别）。
- 适配：DLP 检测前先做归一化——全角半角 / 大小写 / 分隔符 / 零宽字符归一化（标准库可及的 80%）；归一化只用于检测，不改写原文与脱敏占位符。红线：归一化只增命中、不减命中（原文直扫的结果必须保留），不得因归一化产生新的敏感原文落盘。谐音 / 拼音 / 拆字识别需拼音词典，工程量大，本条目不含、后续单独评估。
- 借鉴点（1Panel AI 网关，飞致云论坛 bbs.fit2cloud.com/t/topic/19664 与官方博客，2026-11-23 查阅）：
  1Panel 的内容合规是「敏感词匹配 + 语义审核 + 内容审计」三层，
  其中动态敏感词拦截「支持自定义行业词库和黑白名单，并且通过归一化处理识别谐音、拆字、拼音等变形内容」。
  我们照此实现，但做三处改动：① 只做标准库可及的归一化（全角 / 大小写 / 分隔符 / 零宽字符），谐音 / 拼音 / 拆字不做；② 归一化分两个视图——词表用「只留字母数字」视图（任何插进来的分隔符都是绕过手段），正则用「保留标点」视图（API Key 要 `-`、邮箱要 `@` 与 `.`）；③ 归一化发现一律映射回原文偏移再合并（原文直扫不动、重叠即丢弃），保证只增命中、不减命中，且脱敏占位符替换的是原文片段、响应还原回原文。
- 复用检查（Nathan 复用优先指示）：未复制代码——1Panel 为闭源商业产品，只借其归一化管线的形态；归一化与回指映射约 200 行纯标准库自研（`internal/normalize`），检测复用本仓已有 `internal/guard`（Aho-Corasick + 正则）。
- 实现：新包 `internal/normalize`（`Normalize`：全角折叠 + 统一小写 + 零宽/不可见字符去除，分 `ForKeywords`（只留字母数字）与 `ForPatterns`（保留标点、去空白）两视图；`Normalized` 带 `Orig`/`Ends` 回指表，`MapRuneRange`/`MapByteRange` 把归一化偏移映射回原文字节偏移，区间端点落在多字节 rune 内时向外取整）；`guard.Detector` 增加归一化词表自动机（构造时同规则折叠词表），`Scan` 在原文直扫后追加归一化两遍扫描并合并（重叠丢弃、按位置排序）；顺带修一个旧缺陷：内置 `password_assignment` 正则的 `\b` 在 Go 里只认 ASCII 词边界，行首或中文标点后的 `密码：…` 永远匹配不上，改为只对 ASCII 候选词要求词边界；详见 docs/dlp-normalization.md。
- 测试：normalize 包 4 项（两视图 20 余组变形、回指映射：含分隔符的原文跨度、多字节 rune 内取整、全角数字整段映射、逐 rune 回指往返）+ guard 4 项（变形词表 10 组：空格/零宽/全角/大小写拆分、变形正则 6 组：拆分手机号/全角手机号/全角 API Key/全角邮箱/全角密码赋值、只增不减：原文命中逐字节一致且无重复、变形手机号脱敏还原往返）+ 网关集成 4 项（空格与零宽拆分的违禁词 400、空格拆分的自定义词对 BYOA 403、全角手机号受管级脱敏还原且占位符不泄漏、归一化命中的审计只记 `banned:banned_word` 不含变形文本）+ 冒烟新增 5 项断言；`gofmt`/`go vet`/`go test ./...` 全绿；`go test -race`（guard/normalize/gateway）全绿；`make smoke` 258 项断言 0 失败（基线 253 + 新增 5）。
- 已知注意点：谐音 / 拼音 / 拆字识别不在本轮（需拼音词典，后续单独评估）；兼容字符（`①`、`²`）不折叠（需 NFKC，暂不引入 `x/text` 依赖）；归一化可能多命中（两个词被分隔符连起来恰好拼成违禁词时也会拦，是变形检测的固有代价）；`Scan` 最多跑三遍匹配，检测对象是聊天消息与工具参数，量级很小。冒烟调试中发现并处理：F2 的 stats 断言是精确计数（3 调用 / 2 拦截），F29 的 3 次拦截断言须放在该断言之后，另用新令牌以免干扰 F3 的 RPM 计数。
- 排序：排在 F26 之后、F28 之前。

### F30 复杂度智能路由 `todo`
- 来源：Nathan 2026-11-22 批准立项（1Panel AI 网关调研结论，待办登记 7）：1Panel 的智能路由按复杂度分发（model=auto，简单问题走便宜模型组）。
- 适配：与治理路由正交、可叠加——复杂度只决定在同一密级与信任梯度内选哪个模型组，不改变分级约束；优先级不高。
- 验收：待立项细化。
- 排序：排在 F28 之后、F27 之前。

### F31 MCP 列表/调用同规则过滤 `todo`
- 来源：Nathan 2026-11-22 批准立项（Higress / APISIX / Kong / Bifrost / Envoy 网关调研结论，待办登记 8）：借鉴 Envoy 的 MCP 工具过滤——tools/list 与 tools/call 用同一套规则过滤，看不见的工具即调不了。
- 适配：待立项细化。
- 排序：排在 F30 之后、F27 之前。

### F32 一致占位符脱敏 `todo`
- 来源：Nathan 2026-11-22 批准立项（同上调研结论，待办登记 8）：借鉴 Kong 的一致占位符——同一敏感值在一次会话内始终得同一占位符，利于模型理解；只借占位符一致性，进程内实现（Kong 的实现是独立 spaCy 容器，不借）。
- 适配：待立项细化。
- 排序：排在 F31 之后、F27 之前。

### F33 失败分类重试 `todo`
- 来源：Nathan 2026-11-22 批准立项（同上调研结论，待办登记 8）：借鉴 Bifrost 的失败分类重试——密钥级失败立即轮换、瞬时错误同密钥重试；另含分层预算。
- 适配：待立项细化。
- 排序：排在 F32 之后、F27 之前。

### F26 分级管理员与职责分离 `done`（2026-11-22）
- 来源：Nathan 2026-11-22 批准立项（开源类 IP-guard 工具调研结论）：借鉴 IP-guard 分级管理员体系——在现有 admin 之外拆出审计员（只读审计与报表、不可改策略）与运维员（渠道 / 令牌运维、不可看审计原文）等角色，OIDC 映射同步支持；关键操作（改分级策略、批例外、导出审计）要求对应角色并全程留痕。对应弱点清单 #3 的管理兜底。
- 适配：五档角色——平台管理员（全部权限、唯一可分配角色）、安全管理员（与平台管理员同等的策略权、不可分配角色）、审计员（只读审计与报表）、运维员（渠道与令牌运维、不可看审计）、员工；`security` 保留与 `admin` 同等的策略权是此前 `requireAdmin`（admin | security）行为的延续，本轮没有降级它。
- 借鉴点（等保三级「三权分立」，GB/T 22239-2019；2026-11-22 查阅 opforge.srebro.cn 等保三权分立配置指南与 openGauss 访问控制模型文档）：
  系统管理员（配置权）、安全管理员（授权权）、审计管理员（审计权）三权分离、互相制约；审计员只读、系统管理员不改安全策略、安全管理员不碰业务数据；三员不得由同一人兼任、最小权限。另有 IP-guard 分级管理员体系（无公开 API 文档，形态以同类 DLP 产品说明与本次调研为准）。
  我们照此实现，但做三处改动：① 五档而非三档——在等保三员之外单列运维员（渠道 / 令牌日常运维），因为网关的日常运维量最大、又不该碰审计；② 角色分配权只收不放——连安全管理员也不能授角色（等保只要求三员不兼任，我们进一步要求授角色权只属于平台管理员，防自我提权）；③ OIDC 映射是显式配置的组映射、不是 IdP 直授角色（F9 的「IdP 只认身份」红线不变）。
- 实现：新包 `internal/roles`（五档角色常量 + 12 项权限的矩阵 `Can` + 角色说明卡片，唯一事实来源）；`store.User` 增加 `RoleSource`（local/oidc，旧行按账号来源推断）；`internal/api` 增加通用守卫 `requirePerm`，路由按矩阵重接——审计读 / 导出 / 报表生成归审计员，渠道 / 令牌 / 缓存 / OTel 归运维员，归档审计与报表删除仍仅 admin/security，批例外仍仅 admin/security；新端点 `GET /api/users`（列表，不返密码哈希）、`GET /api/users/roles`（角色矩阵）、`PATCH /api/users/{id}`（仅 admin，护栏：不能改自己角色、不能禁用自己、不能动最后一位启用的平台管理员；改角色即转本地管理；禁用即吊销全部会话；每次变更记 `user_updated` 审计）；OIDC：`groups` claim 解析（数组或单字符串，claim 名可配）+ `AGENTGATE_OIDC_ROLE_MAP` 组 → 角色映射（仅 auditor/operator/employee，admin/security 写进映射会在启动校验时拒绝；映射只作用于 IdP 管理的账号、每次登录按当前组同步、本地改过角色的不再覆盖；`auth` 层对映射返回值再挡一次，防错配提权）；管理台新增「用户与角色」页（角色分工表 + 用户列表 + 改角色 / 禁用，安全管理员只见列表）与导航按角色过滤（服务端 403 仍是真凭据）；演示种子新增 `auditor` 与 `ops` 账号（密码均为 demo12345）；详见 docs/roles.md，OIDC 映射见 docs/oidc-sso.md。
- 测试：roles 包 4 项（矩阵全覆盖、未知角色与未知权限拒绝、ValidRole/IsAdminRole、MappableViaIdP、Describe）+ API 用户管理 1 项（列表 5 用户无哈希、角色矩阵、提权与恢复全链路及审计事件、未知角色 400/未知用户 404、自改角色与自禁用 400、审计员与运维员改角色 403、最后一位平台管理员降级与禁用 400、禁用吊销会话、再启用恢复）+ HTTP 权限矩阵 1 项（审计读 / 导出 / 归档、报表生成与删除、渠道与令牌、缓存与 OTel、批例外、证明策略共 40 余个断言）+ auth 映射 2 项（JIT 按组授角色、多组、退组回落、本地覆盖优先、旧行视作 IdP 管理、错配映射不提权、无映射不碰角色）+ oidc 解析 4 项（ParseRoleMap 8 类拒绝、groups claim 三形态、自定义 claim 名、mergeClaims 并集、ConfigFromEnv 映射与非法映射禁用 OIDC）；另更新 orgsync 旧测试的种子用户计数 3→5（种子新增两人，行为未变）；`gofmt`/`go vet`/`go test ./...` 全绿；`go test -race`（roles/auth/oidc/api/store）全绿；`make smoke` 253 项断言 0 失败（基线 237 + 新增 16，两次运行均稳定）。
- 已知注意点：角色是粗粒度的五档，无按部门或按资源的细粒度授权；OIDC 映射是登录时同步，组变化在用户下次登录时才生效；管理操作 HMAC 签名审计（Bifrost 形态，待办登记 8 备注）：哈希链已覆盖审计完整性，管理操作再加 HMAC 签名是补充、未在本轮实现，留待后续轮次评估。
- 备注（待办登记 8，2026-11-22 批准）：F28 深层首选候选补 Qwen3Guard 式自托管小模型，见 F28 条目。

### F27 双机热备与高可用 `todo`
- 来源：Nathan 2026-11-22 批准立项（开源类 IP-guard 工具调研结论）：网关无状态化 + 状态外置或主备切换，单点故障时受管调用不中断、审计不丢失；对应弱点清单 #3。工程量大，排在其余 P2 之后，启动前先做方案评审。
- 适配：待立项细化（本条目登记时仅定方向与借鉴策略）。

## 调研来源（持续更新）
- LiteLLM / Portkey / Kong / Cloudflare AI Gateway 对比（genaimaturity.net、devopsness.com，2026-11 查阅）
- IBM ContextForge（MCP 网关/注册中心，Apache-2.0，ibm.github.io/mcp-context-forge）
- New API 及其生态分叉（防护插件管线、合规归档）、AstrLink（端侧隐私网关）
- hikariming/dsh-ee（能力中枢闭环）、WorkBuddy 企业版安全白皮书（终端管控与审计）

备忘：IM（飞书 / 钉钉）机器人消息中继：Nathan 2026-11-22 决定暂不做——与受管级准入标准（能碰加密文件的 Agent 不开 IM 远程分发通道）冲突，不立项。

备忘：渠道（1Panel 应用商店上架与诚实版选型对比页）：Nathan 2026-11-22 批准记为发布动作（待办登记 7）——AgentGate 可打包上架 1Panel 应用商店（其用户即目标 IT 管理员），并做诚实版选型对比页；随 v0.1 发布动作推进，不单独占 Loop 条目。

队列顺序（2026-11-22，待办登记 7、8）：F26 → F29 → F28 → F30 → F31 → F32 → F33 → F27。
