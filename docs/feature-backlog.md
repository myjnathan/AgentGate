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
