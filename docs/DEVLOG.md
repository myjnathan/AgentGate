# 开发日志（DEVLOG）

Loop 与人工开发的统一记录，最新在最上面。

## 2026-11-21 · F18 文件夹即知识库
- 本轮按常规规程选条目：特别指令（F17 + 蓝图修订稿）已在 F17 轮执行完毕、执行后作废；P0 全部 done，P1 按清单顺序取 F18（唯一剩余 P1 条目）。远端 main 已含 F10（PR #6，HEAD 95729472），无在途分支需续传。
- 借鉴 PageIndex 官方介绍（pageindex.ai/blog/pageindex-intro）与架构深挖（nowucca.com/research/pageindex）：索引是一棵章节树（节点含 node_id、标题、起止页码、每节点一段摘要），检索是 Agent 用三个工具走目录 → 读树（只要摘要不要正文）→ 按页取原文；Flash 模式不用 LLM 建结构（字体样式识别标题、优先用 PDF 内嵌书签）。我们照此实现，但做三处改动：① 摘要不用模型生成，改抽取式（章节首句）——摘要生成因此不产生任何出站调用、不成为泄漏通道；② PDF 用真实页码，.md/.txt/.docx 用 45 行伪页；③ 检索仍保留关键词（与现有 kb 同一流水线），树只负责渐进式取用。
- 实现：新包 `internal/folderkb`（内存索引 `Registry` + 增量 `Rescan`：大小+修改时间未变不读文件、内容哈希相同保留旧解析、抽取失败保留上次可用索引；`Catalog`/`Structure`/`Section` 三层取用 + 按章节粒度的 `Search`）；抽取器纯标准库：.md/.txt（ATX 标题建树）、.docx（zip + XML，只取 `w:t`，Heading1/2/3 建树、显式分页符分断）、.pdf（best-effort：Flate 解压 + Tj/TJ 字面串抽取，内容流与页数 1:1 时映射真实页码）；`store.KBFolderSource` 持久化注册、索引不落盘；管理 API `GET/POST /api/kb/folders` + `DELETE /api/kb/folders/{id}` + `POST /api/kb/folders/{id}/rescan`（仅 admin/security）；MCP 新工具 `kb_catalog` / `kb_structure` / `kb_section`，`kb_search` 合并种子文档与文件夹命中；管理台新增文件夹知识库卡片（中英双语）；演示种子注册 `demo/folders/公司制度/`（internal：.md）与 `demo/folders/机密资料/`（confidential：2 页 .pdf，内容流未压缩以保持纯 UTF-8）。
- 关键决策：摘要抽取式而非模型生成——这是治理要求不是偷工减料：摘要是原文子串、不出本机、与原文同密级门禁，不可能把机密内容带到不该去的地方；首个一级标题即文档标题、不进章节树（.md 的 H1 与 .docx 的 Heading1 同理）；根节点（n0）章节含整篇文档正文、但检索时有子节点即不参与（否则每处命中重复两次）；无标题的多页文档按页建节（「第 N 页」），引用保持页码精确；机密文件夹对自带级四处不可见（目录/检索/章节树/章节原文），与 `kb_get` 的受管级限制同源但按 `MaxLevelForTier` 放行内部章节。
- 测试：folderkb 包 7 项 + API 端点 1 项（注册→列表→重扫→删除全链路、400/403/404/409）+ MCP 集成 3 项（目录→结构→章节三步取用、机密文件夹隔离、kb_search 合并命中）+ 冒烟新增 8 项断言；`gofmt`/`go vet`/`go test ./...` 全绿；`make smoke` 92 项断言 0 失败。
- 已知限制：伪页不是渲染页码；.docx 只读正文段落；PDF 抽取不支持扫描件、加密 PDF 与非标准编码（计入扫描 errors）；Excel 暂不支持；索引在内存中，重启后启动时自动重扫。详见 docs/folder-kb.md。
- 推送：按分支 + PR 机制走 `loop/f18` 分支，见本轮汇报。
- 推送结果：待填。
- 另记（推送通道限制）：GitHub 连接器的两种写通道都无法原样传输二进制文件——`push-files` 只接受 UTF-8 文本（快照校验直接拒掉含非 UTF-8 内容的整包），contents API（`create_or_update_file`）会把 `content` 当文本再做一次 Base64（实测：传 Base64 串存下来的是 Base64 文本本身，blob SHA 与 Base64 文本一致；传 latin-1 解码串会被 UTF-8 重编码，字节膨胀）。本轮因此：① 演示 PDF 改为未压缩内容流（仍是合法 2 页 PDF，纯 UTF-8，可走快照推送；Flate 解压路径由单元测试覆盖）；② 演示文件夹去掉 .docx（.docx 抽取仍由单元测试覆盖，测试内现生成夹具）；③ 两次二进制试错各在 `loop/f18` 分支留了一个提交（先是 Base64 文本版 PDF、再是 UTF-8 重编码版），最终以快照推送修正，分支历史有此污点、终态文件正确。后续轮次如需向仓库推二进制演示文件，须另寻通道（Nathan 网页端上传或本地 git 推送）。

## 2026-11-21 · F10 流式脱敏与 tool_calls 防护（探针 3）
- 本轮按常规规程选条目：特别指令（F17 + 蓝图修订稿）已在 F17 轮执行完毕、执行后作废；P0 全部 done，P1 按清单顺序取 F10。远端 main 已含 F9（PR #5，HEAD 0032b40a），无在途分支需续传。
- 借鉴 nr-llm ADR 88《Live streaming redaction》与 Sether《PII redaction for LLM applications》：流式处理的核心是 holdback——秘密/占位符可能被切在两个 chunk 之间，逐 chunk 独立处理必然漏；做法是对整段已收 buffer 反复处理、只放行超出安全距离（最长占位符的转义后长度）的前缀，尾部在流结束时冲掉；且必须对原始 buffer 反复处理（已打标记会破坏模式字符类）。我们方向相反（还原占位符而非现场脱敏），但同一 holdback 模式；无映射时零缓冲直通。
- 实现：`internal/guard` 新增 `Redactor`（同一请求内多段文本共享占位符编号）与 `MaxPlaceholderLength`（转义后最长占位符长度）；`internal/gateway` 的 `chatRequest` 改为命名类型并解析 `tool_calls`（`scanText` 把参数文本并入防护管线），`redactBody` 用同一 `Redactor` 处理 content 与各 tool_calls 参数；新文件 `internal/gateway/stream.go`（`streamCopy` 带 mapping：无映射直通、有映射缓冲还原 + 用量提取，UTF-8 切分点退让到 rune 边界）；`internal/mockllm` 回显 tool_calls 参数（供端到端观测上游实收内容）。另修一个顺带发现的旧 bug：流式用量的旧提取切片出非法 JSON，流式调用一直记 0 tokens、TPM 与统计都漏记，改为按花括号配对解析 usage 对象。
- 关键决策：同一值在 content 与 tool_calls 参数里必须得同一占位符——逐段编号再合并映射会冲突（参数里的邮箱在内容里是 `<PII_2>`、在参数里会变成 `<PII_1>`），故一个请求只用一个 `Redactor`；holdback 按转义后长度算（上游 JSON 编码把 `<PII_1>` 写成 `\u003cPII_1\u003e`，长 10 字节）；`tools` 定义（函数 schema）不在本轮管线内，只处理消息里的 `tool_calls` 参数。
- 测试：包内 stream 测试 5 项（1/2/3/7 字节分片下转义与原始两种占位符均还原、多字节文本无损、流尾占位符冲刷、用量提取、空映射直通、双占位符不同长度 holdback）+ 包内 redact 测试 3 项（tool_calls 脱敏与字段保留、共享占位符无冲突、无发现时原文直通）+ HTTP 端到端 5 项（流式还原、流式 tool_calls 参数还原、tool_calls 违禁词 400、自带级机密参数 403、受管级机密参数脱敏后 200 且原文还原）+ 冒烟新增 3 项断言；`gofmt`/`go vet`/`go test ./...` 全绿；`make smoke` 84 项断言 0 失败。
- 探针 3 结论：通过——占位符跨分片还原用例全过；同一值在同一请求内跨 content/tool_calls 得同一占位符。
- 推送：按分支 + PR 机制走 `loop/f10` 分支，见本轮汇报。
- 推送结果：整包快照推送一次成功（push-files，11 个文件一次提交 dda1cfe7，逐文件哈希核对 11/11 一致，未触发 contents API 回退；核对时发现 `get_file_contents`/`list_commits` 的分支参数名应为 `ref`/`sha` 而非 `branch`，错传会被静默忽略读回 main——已用正确参数重核）；PR #6 文件清单与本轮一致，已合并进远端 main，远端 HEAD 95729472（合并提交），读回验证 stream.go 已在 main 上。

## 2026-11-21 · F9 OIDC / LDAP 登录
- 本轮按常规规程选条目：特别指令（F17 + 蓝图修订稿）已在 F17 轮执行完毕、执行后作废；P0 全部 done，P1 按清单顺序取 F9。Nathan 于本轮前在主对话说「继续」，Loop 照每小时一轮推进。
- 借鉴 OpenID Connect Core 1.0 / Discovery 1.0（openid.net/specs）与 Duende IdentityServer 演示站（demo.duendesoftware.com）：授权码 + PKCE S256 + state/nonce、服务端换码、JWKS 验 RS256、userinfo 补 profile。Duende 实测的两条教训直接进了实现：① profile claim 不在 ID token 里、只在 userinfo 里，换码后必须调 userinfo 合并；② 演示 client 登记的 redirect URI 是示例地址，验证时用登记地址走完换码、再用本仓库代码验 token。
- 实现：新包 `internal/oidc`（纯标准库：discovery、授权码 + PKCE、RS256 验签、userinfo 合并；state/nonce 单次有效 10 分钟过期）；`store.User` 增加 `oidc_subject` 与 `auth_source`；`auth.LoginOIDC`（JIT 开户、每次登录刷新姓名/部门）+ `auth.CreateSession`；`GET /auth/oidc/login`、`GET /auth/oidc/callback`、`GET /api/auth/providers`、`GET /api/auth/oidc/status`（admin）；管理台登录页在 OIDC 启用时显示 SSO 按钮（中英双语）；环境变量 `AGENTGATE_OIDC_*`，缺一个即不开启，本地登录始终兜底。LDAP 本轮只做配置与设计：新包 `internal/ldap`（`Config` + `Validate`，`Implemented()` 为 false）与 docs/ldap.md（流程、安全约定、实现轮次验收清单），无测试目录、未实现客户端、未实测。
- 关键决策：IdP 只认身份、不认权限——role claim 即便给了也不采信，JIT 新用户永远是 employee，提权必须本地管理员操作；账号不串户（OIDC 用户名撞本地账号自动加后缀，OIDC 账号无本地密码哈希）；只认 RS256（`alg: none` 与 HS256 直接拒绝）；回调失败只给通用文案，code/state 不进审计；不引入 oauth2/oidc 依赖，登录路径纯标准库。
- 测试：oidc 包 5 项 + auth 包 4 项（JIT 开户/撞名加后缀/禁用拒绝/用户名清洗）+ API 端点 4 项（假 IdP 下 HTTP 全程 login→IdP→callback→会话页→/api/me）+ ldap 包 2 项；另用真实 Duende 演示 IdP（alice/alice）跑通登录闭环并以本仓库 `VerifyIDToken` 对线上 JWKS 验签通过（临时测试，验证后删除、未进仓库）；`gofmt`/`go vet`/`go test ./...` 全绿；`make smoke` 81 项断言 0 失败（新增 5 项 F9 断言）。
- 推送：按分支 + PR 机制走 `loop/f9` 分支，见本轮汇报。
- 推送结果：整包快照推送一次成功（push-files，18 个文件一次提交 06cf28d6，逐文件哈希核对 18/18 一致，未触发 contents API 回退）；PR #5 文件清单与本轮一致，已合并进远端 main，远端 HEAD 0032b40a（合并提交），读回验证 oidc.go 与 oidc-sso.md 已在 main 上。

## 2026-11-21 · F8 Prompt 模板库
- 本轮按常规规程选条目：上一轮特别指令（F17 + 蓝图修订稿）已在 F17 轮执行完毕、执行后作废，本轮起忽略；另核实主对话的许可证变更已于本轮前经 PR #3 合并进远端 main（3556114，5 个文件逐个哈希核对与本地 00c39b5 一致），无在途分支需续传。P0 全部 done，P1 按清单顺序取 F8。
- 借鉴 ContextForge Prompt 注册中心（Jinja2 模板 + JSON-Schema 参数校验 + 版本管理与回滚）与 Portkey Prompt 版本语义（更新只产生新版本、不推向生产；发布才标定默认生产版本；版本不可变；默认走已发布版、可按版本号或 `@latest` 引用；历史版本可恢复/发布）。我们照此实现，但做三处治理收紧：① 版本选择只服务已发布版本——`@latest` 即已发布版，未发布版本 Agent 永远拿不到；② 参数校验默认严格——未知参数、必填缺失、非字符串值一律拒绝（与 F7 的 schema 即契约一致）；③ 渲染走与工具调用相同的门禁：参数先过 DLP、渲染结果再过 DLP + 自带级脱敏，模板按部门范围与最低等级下发。
- 实现：`store.PromptTemplate` + `PromptVersion`（不可变版本、`{{name}}` 占位符 + 声明参数）+ `RenderPrompt`（严格替换，只报字段名不报值）+ `ValidateTemplateVersion`（占位符须已声明、内容 ≤32 KiB、参数 ≤20 个）；`/mcp` 新增 `prompts/list` 与 `prompts/get`（默认/显式版本选择、数字版本须为已发布版、参数与渲染结果双重 DLP、审计 `prompt_get`，草稿存在性不泄漏）；管理 API `GET/POST /api/prompts`、`GET /api/prompts/{id}`、`POST /api/prompts/{id}/versions`、`POST /api/prompts/{id}/review`、`POST /api/prompts/{id}/publish`（发布/回滚到指定版本）、`DELETE /api/prompts/{id}`；管理台新增 Prompt 模板卡片（中英双语）；演示种子 `weekly_report`（星河精密周报助手，已发布 v1）。
- 关键决策：新版本永不自动发布——加版后已发布版继续服务，直到管理员显式发布（Portkey 的更新≠发布语义）；审核批准仅在首次发布时定已发布版，已发布模板的后续版本一律走 publish 端点，避免审核动作悄悄改生产；拒绝已发布模板会撤下服务（prompts/get 要求 status 为 published），与技能的 rejected 语义一致。
- 测试：store 5 项 + API 端点 1 项（提交→加版→审核发布→回滚→删除全链路）+ MCP 集成 7 项 + 冒烟新增 16 项断言；`gofmt`/`go vet`/`go test ./...` 全绿；`make smoke` 76 项断言 0 失败。
- 推送：按分支 + PR 机制走 `loop/f8` 分支，见本轮汇报。
- 推送结果：整包快照推送一次成功（push-files，14 个文件一次提交 560d8777，逐文件哈希核对 14/14 一致，未触发 contents API 回退）；PR #4 文件清单与本轮一致，已合并进远端 main，远端 HEAD 47b2b190（合并提交），读回验证 prompts.go 与 mcp.go 已在 main 上。

## 2026-11-21 · 许可证变更：PolyForm Noncommercial → GNU AGPL-3.0
- Nathan 2026-11-21 15:43 决定：项目目标是普及而非盈利，许可证由 PolyForm Noncommercial 1.0.0（禁止商用）改为 GNU AGPL-3.0——任何人可免费使用（含商业使用）；修改后分发或以网络服务形式提供时，须以 AGPL-3.0 公开对应源代码，防止闭源转卖。此决定取代同日早先「禁止商用」的指示。
- 变更内容：LICENSE 替换为 AGPL-3.0 标准全文（取自 SPDX license-list-data 的 AGPL-3.0-only 文本，34,020 字符，结构校验通过）；README 许可证一节、docs/blueprint.md 许可证决策记录与仓库可见性条目、docs/development-loop.md 红线表述同步改为 AGPL 口径。
- 本条为主对话人工变更（非 Loop 轮次），走 loop/license-agpl 分支 + PR 合入 main；同时首次实测整包快照推送（push-files）机制。

## 2026-11-21 · F17 渠道信任类型与分级策略梯度
- 特别指令已执行（Nathan 2026-11-21 12:26 决定，本轮执行后作废）：先核实 F6+F7 已并入远端 main——
  PR #1 已合并（远端 main HEAD 342c56b，2026-11-21 15:13 前后经 list_commits / list_pull_requests 核实），
  无在途分支需续传；本轮条目锁定 F17，并一并落地蓝图修订稿 8 处替换。下一轮起忽略该特别指令，按常规规程选条目。
- 借鉴 Cloudflare AI Gateway 官方文档与 2026 年七家网关对比：Cloudflare 只做托管式代理、没有自托管或
  VPC 内部署选项（受数据驻留约束的团队最先撞上的墙）；数据驻留只在网关层可配，最终推理位置由模型厂商决定。
  我们反过来做：不把部署形态锁死在某一家厂商——信任类型就是部署形态本身（签约云 / 专有云-VPC / 本地小模型），
  每个渠道自带信任类型，路由按密级选信任梯度；机密默认脱敏后上云是纯托管网关给不了的保证（它们只转发、不替你脱敏）。
- 实现：`store.TrustType`（signed_cloud / dedicated_cloud / local_model，空值归一为签约云以兼容旧快照）+
  `Channel.TrustType` + `TrustTypeOrDefault()`；新文件 `internal/gateway/trust.go`（梯度判定与
  `no_compliant_channel` 提示文案）；网关 `pickCandidates` 与 `channelsCoolingDown` 按信任过滤，机密→签约云时
  即使等级脱敏配置关闭也强制脱敏（审计 Detail 记 `desensitized=signed_cloud`，decision 仍为既有约定的
  `reroute`），无合规通道时 403 `no_compliant_channel` 并写 `violation` 审计事件；
  `GET/POST /api/channels` 返回与接受 `trust_type`（非法值 400、缺省签约云）；管理台渠道表新增信任类型列（中英双语）；
  seed 增加脱敏云渠道（机密默认）与本地模型渠道（绝密），私有渠道标为专有云/VPC。
- 关键决策：脱敏是信任类型的结构属性、不是等级配置的附属——等级脱敏关掉也不影响机密上云必脱敏；
  绝密只认本地小模型，专有云/VPC 也不行（密级够不等于信任够），错误码与通用 `no_channel_for_data_level` 区分开，
  让运维一眼看到缺的是哪种通道；旧快照渠道无信任类型字段时按签约云处理（此前默认即公有云行为，不改变既有部署语义）。
  故障转移链仍沿用 F1 规则（只在首选同一 MaxDataLevel 内转移），脱敏云故障时不自动升级到专有云/本地模型——
  跨信任梯度的合规故障转移留待后续轮次评估。
- 蓝图修订稿 8 处替换已落地（修订稿存 goal hidden_files/blueprint-revision-draft-2026-11-21.md）：防护设计 §2 分级路由表
  （机密默认脱敏后上云、绝密本地小模型或明确拦截）与 §5 MVP 管线；蓝图架构图、§6 交叉规则、§9.1 MVP 清单、
  §4.5 知识库条目补 F18 说明、新增 §6.1「目标客户与落地方式」（零预置假设/默认保守模式/先观察→再提醒→后拦截/
  双轨可插拔）；README 架构图。另同步 README 网关特性行与演示段落（受管级机密默认脱敏后上云、绝密走本地模型），
  并把 Loop 规程 `docs/development-loop.md` 第 6 步更新为整包快照推送优先（push-files 一次推全部文件，
  403 才回退 contents API）与每小时一轮的节奏。
- 测试：store 3 项 + 网关信任测试 5 项（梯度路由与上游实收报文验证、等级脱敏关闭时仍强制脱敏、机密走专有云原文、
  无本地模型时绝密 403 `no_compliant_channel` 及审计原因、仅专有云时绝密仍为 `no_compliant_channel`）+
  API 渠道测试 1 项 + 冒烟新增 7 项断言；更新旧测试 `TestManagedConfidentialRoutedToPrivate`（机密默认改走脱敏云）
  与 `TestConfidentialNeverFallsBackToLowerLevel`（脱敏云故障时不降级到普通云、也不升级到专有云/本地模型）；
  `gofmt`/`go vet`/`go test ./...` 全绿；`make smoke` 60 项断言 0 失败。
- 推送：按分支 + PR 机制走 `loop/f17` 分支，见本轮汇报。
- 推送结果：整包快照推送一次成功（push-files，18 个文件一次提交 fc421227，逐文件哈希核对 18/18 一致，
  未触发 contents API 回退——此前 F4 轮次 403 的快照路径本次在非空仓库上首次走通）；PR #2 文件清单与本轮一致，
  已合并进远端 main，远端 HEAD 08676f3f（合并提交），读回验证 trust.go 与蓝图 §6.1 已在 main 上。
- 另记：Nathan 2026-11-21 15:14 定下防护范围边界——AgentGate 只防「电脑内」的数字数据流，员工拍照、默记、口述等
  人的行为不在防护范围内（是边界、不是弱点，明说归客户保密制度）；产品弱点要明说不遮掩，靠管理手段兜底的弱点须把
  管理措施讲清楚；「绕过场景与对策」先不放进防护设计文档，先在讨论中对齐弱点与策略再说。本轮按此指示未写入文档。

## 2026-11-21 · F7 REST → MCP 适配
- 借鉴 ContextForge 官方 README 与文档（ibm.github.io/mcp-context-forge）及 mcp-rest-forge 配置模型：
  REST 工具逐端点 JSON 注册（name/url/request_type/input_schema），凭据 header 注入；
  v1.0.5 起可从 OpenAPI 生成单个端点的 schema，但整份 spec 导入已明确 not planned；
  mcp-rest-forge 同源：method/path 模板（{{param}} 转义替换）/query_params/body/headers。
  我们只取逐端点注册语义，不做 OpenAPI 整份导入；差异化收紧：每个工具只配最高密级与
  最低等级两把闸（与 F6 同款），参数 schema 注册时即校验，上游 5xx 只报状态码不回传 body。
- 实现：新包 `internal/restadapter`（路径模板转义替换、其余参数 GET/DELETE 走 query、
  POST/PUT/PATCH 走 JSON body、JSON Schema 子集校验、上游错误只报状态码、1 MiB 响应上限）；
  `store.RESTTool`（方法/基址/路径模板/静态头/加密托管凭据/输入 schema/最高密级/最低等级）；
  `/mcp` tools/list 合并 `rest.<name>`（高于调用者等级的隐藏）、tools/call 走与联邦工具
  相同的门禁与结果 DLP + 自带级脱敏，参数校验失败报 -32602；`GET/POST /api/mcp/rest-tools` +
  `DELETE /api/mcp/rest-tools/{id}` + `POST /api/mcp/rest-tools/{id}/test`（仅 admin/security，
  测试端点返回 ok + 200 字内 preview，不写入审计）；管理台新增 REST 工具卡片（中英双语）；
  演示：mock 增加 `/mock/demo/hr/leave-balance`（星河精密虚拟 HR 接口），seed 注册
  `rest.leave_balance`。
- 关键决策：schema 校验默认严格——未知参数直接拒绝（schema 即契约，避免未声明参数被
  静默转发到上游）；凭据只在调用瞬间解密注入 Authorization 头；测试端点的 preview 只给
  管理员调用方、不进审计（审计只记工具名与成败）。
- 测试：restadapter 包 6 项 + MCP 集成 8 项 + API 端点 2 项（CRUD 与实时测试）+ 冒烟新增 16 项断言；
  `gofmt`/`go vet`/`go test ./...` 全绿；`make smoke` 53 项断言 0 失败。
- 推送：按分支 + PR 机制走 `loop/f7` 分支，见本轮汇报。
- 交接：Nathan 于 2026-11-21 12:26 拍板，**下一轮先做 F17（通道信任分级，跳过清单顺序在前的
  F7——本轮在拍板前 6 分钟已开工故不打断）**，且蓝图修订稿（8 处替换，存
  `~/workspace/goals/agentgate-enterprise-agent-platform/hidden_files/blueprint-revision-draft-2026-11-21.md`）
  一并落地到仓库 `docs/blueprint.md`、`docs/enterprise-protection-design.md`、`README.md`；
  该决定已写为 Loop 的「下一轮特别指令」，执行后作废。F17 顺延为紧接着的一轮。

## 2026-11-21 · F6 MCP 联邦代理
- 借鉴 IBM ContextForge 官方文档（ibm.github.io/mcp-context-forge/architecture）与 PyPI 说明：
  多后端联邦到单一合规端点、注册中心合并能力目录、虚拟服务器组合工具子集、定期健康检查与故障转移、
  工具输入校验与重试/限流、PII 过滤插件。我们只取一层代理语义：命名空间前缀、60 秒工具列表缓存、
  上游只配最高密级与最低等级两把闸；不出对等网关发现/mDNS 与 Redis 同步；DLP 由可选插件改为强制门禁。
- 实现：新包 `internal/federation`（JSON-RPC 2.0 over HTTP 转发、工具列表缓存 + 刷新失败回落旧缓存、
  上游错误不透传原文）；`store.UpstreamServer`（命名空间/URL/加密凭据/最高密级/最低等级）；
  `/mcp` tools/list 合并本地与联邦工具（高于调用者等级的上游对该调用者隐藏，不可达上游静默缺席）、
  tools/call 对命名空间工具先做等级授权、再对参数做 DLP（违禁词或密级高于上游上限直接拦截），
  结果复用本地工具的 DLP 扫描与自带级脱敏；`GET/POST /api/mcp/upstreams` +
  `DELETE /api/mcp/upstreams/{id}` + `POST /api/mcp/upstreams/{id}/test`（仅 admin/security，
  命名空间校验、重名 409、凭据永不回显）；管理台新增联邦上游卡片（中英双语）。
- 关键决策：tools/list 按调用者等级过滤（最小暴露存在性）；参数密级门禁在转发前做——
  机密参数永不发往仅 internal 的上游（分级约束高于可用性）；上游故障时列表静默缺席、调用报 -32002，
  不让单个上游拖垮整个 /mcp；凭据只在转发瞬间解密、永不进审计与 API 响应。
- 测试：federation 包 4 项 + MCP 集成 6 项 + API 端点 2 项（CRUD 与实时测试）+ 冒烟新增 12 项断言；
  `gofmt`/`go vet`/`go test ./...` 全绿；`make smoke` 37 项断言 0 失败。
- 推送：按 Nathan 指示的新机制（分支 + PR，main 只收完整成品）走 `loop/f6` 分支，见本轮汇报；
  另按任务书把该机制同步进了 `docs/development-loop.md` 第 6 步。
- 另记：Nathan 本轮新决策已记入 backlog 待办——F17 渠道信任类型与分级策略梯度（分级放行改绑通道信任类型，
  私有模型降为可选项；默认保守模式 + 先观察→再提醒→后拦截）、F18 文件夹即知识库（零预置假设，
  指定共享文件夹即自动索引）。

## 2026-11-21 · F5 Webhook 告警
- 本轮先续推 F4：重跑 `/tmp/push_f4b.py`，剩余 8 个文件经校验已与远端一致（主对话并行推完），
  26/26 文件全部同步，远端 HEAD d761e80、内容与本地 b8907a8 相同，CI 第 51 次运行全绿——
  自 F2 起困扰三轮的推送阻塞正式解除，远端首次完整绿灯。随后按 Loop 规程取下一条 P0 做 F5。
- 借鉴企业微信官方群机器人文档（developer.work.weixin.qq.com/document/path/91770）与钉钉自定义机器人
  安全设置说明：企微 markdown 上限 4096 字节、仅支持有限语法子集、每机器人 20 条/分钟；钉钉同样
  markdown（title + text）、20 条/分钟、超限停 10 分钟；三家加签同一公式
  `timestamp(毫秒) + "\n" + secret` 经 HMAC-SHA256、Base64、URL 编码后拼进 `timestamp` 与 `sign` 查询参数。
  签名公式与通用 HMAC 请求头均以 Python 独立算出测试向量、写入 Go 测试硬编码校验。
- 实现：新包 `internal/alert`（事件分类 + 四平台载荷 + 签名 + 异步投递器，队列满丢弃计数，
  失败 1 + 3 次重试后放弃，错误只记 `http_500`/`network_error`/`timeout`/`config_error` 类别，
  绝不把可能含 bot key 的 URL 写进审计或 API 响应）；`audit.Log.SetNotifyHook`（锁外回调，
  `alert` 类事件不触发，避免自循环）；`store.Webhook`（签名密钥 AES-GCM 加密后持久化，
  与 `Channel.APIKeyEnc` 的 `json:"-"` 不同——Webhook 是运行时由管理员配置的，丢密钥等于丢配置）；
  `GET/POST /api/alerts/webhooks` + `DELETE /api/alerts/webhooks/{id}` + `POST /api/alerts/webhooks/{id}/test`
  （仅 admin/security，列表 URL 的 query 值打码、密钥永不回显）；登录锁定在 `auth` 层以哨兵错误
  `ErrAccountLocked` 区分，API 记 `account_locked` 审计事件（可告警），普通失败仍为 `login_failed`（不告警）；
  管理台新增告警 Webhook 卡片（列表/添加/测试/删除，中英双语）。
- 关键决策：告警只取审计事件已有字段（处置类别如 banned_word、操作者、密级、渠道），审计 Detail
  本就不含匹配原文，故告警天然脱敏；限流（rate_limit）不做告警——量大且属软性控制，告警会淹没真实安全信号；
  投递是异步的（worker + 64 缓冲队列），慢/死的 Webhook 永不阻塞请求路径；已知注意点：
  `Channel.APIKeyEnc` 带 `json:"-"`、非演示模式重启后渠道密钥会丢失（demo 每次重建故未暴露），与本次
  Webhook 密钥持久化形成反差，留待后续轮次修（不属于 F5 范围）。
- 测试：alert 包 13 项 + API 端点 3 项（鉴权/校验/密钥加密存储与不回显/URL 打码/测试端点成功与 502 + 4 次尝试/
  锁定审计为 account_locked）+ 网关集成 1 项（违禁词拦截→Webhook 收到结构化告警、无敏感原文）；
  `gofmt`/`go vet`/`go test ./...` 全绿；`make smoke` 25 项断言 0 失败（新增 5 项 F5 断言）。
- 推送：已全部上远端（17/17 文件逐个哈希核对一致，远端 HEAD b9f045c；另补推 1 个注释修正
  4efaf18，远端 HEAD 现为 8f45854）。Nathan 在线批卡，推送阻塞解除后第二轮全绿。

## 2026-11-21 · F4 审计导出与合规归档
- 借鉴 LiteLLM 官方审计日志文档（docs.litellm.ai/docs/proxy/audit_logs）与 Cloudflare Logpull 文档
  （developers.cloudflare.com/logs/logpull）：LiteLLM 审计日志带保留策略、可异步批量导出到外部存储
  （S3，按日期分目录的 JSON 文件）；Cloudflare Logpull 以 NDJSON 为标准导出格式、按时间范围拉取、
  保留期到期后改用推送归档。我们照此实现，但导出不走外部存储：本机 NDJSON + 响应头校验，离线可验真。
- 实现：`internal/audit` 新增 Export 与 Archive；`GET /api/audit/export`（仅 admin/security，
  半开时间窗口 + kind/actor 过滤，NDJSON 附件，`X-Audit-Chain-Valid` 等 5 个响应头）+
  `POST /api/audit/archive`（仅 admin/security，链破损时 409 拒绝归档）；归档写
  `audit-archive-<seq>-<seq>.jsonl.gz` + `.manifest.json` 清单，热日志以
  `audit_archive_checkpoint` 检查点事件开头（记录被归档段末哈希）；`AGENTGATE_AUDIT_RETENTION_DAYS`
  默认 180 天（启动日志打印生效值）；管理台审计卡片新增导出/归档入口（中英双语）。
- 关键决策：检查点是**标记而非链节**——归档后新事件仍从被归档段的末哈希链接（`chainHeadHash`），
  而非从检查点自身哈希链接；否则热链在“归档后追加事件”时必断（实现中先踩到该 bug，靠
  TestArchiveTwiceKeepsChainValid 抓出并修复）。离线验真规则：归档段 + 热日志拼接时跳过检查点行。
  保留期是策略值而非定时器：服务不自动删日志，运维按月调一次归档接口（文档含 cron 示例）。
- 测试：audit 包 8 项新测试 + API 端点测试（401/403/400、导出头与行数一致、归档文件落盘、归档后链仍有效）；
  `gofmt`/`go vet`/`go test ./...` 全绿；`make smoke` 20 项断言 0 失败（新增 3 项 F4 断言）。
- 推送（本轮重试）：本轮先按任务书重试 `push-files` 全量推送 F2+F3，Nathan 在线批了卡，但调用在
  GitHub API 层失败：`POST /git/trees 403 Resource not accessible by integration`——证实 push-files
  对非空仓库不可用（与 Loop 规程 09850c1 一致）。改按规程用 contents API（`create_or_update_file`）
  逐文件同步。期间主对话也在并行推 F2/F3（远端出现约 20 个 `sync: F2 usage stats + F3 per-token
  rate limiting` 提交），本轮驱动改为按 blob SHA 比对、只推未同步文件，避免重复提交。
- 推送结果（部分完成，16/26 文件已上远端）：F2/F3 全部文件已由主对话推完并逐个验 blob SHA 一致；
  F4 已推 5 个文件（`cmd/agentgate/main.go`、docs/audit-retention.md、`internal/api/export.go`、
  `internal/api/export_test.go`、`internal/audit/archive.go`，远端提交 46fd8835…d9329e76、b4841338）。
  剩余 10 个文件未推（`internal/audit/archive_test.go`、`export.go`、`export_test.go`、`internal/api/api.go`、
  `internal/audit/audit.go`、README.md、docs/feature-backlog.md、e2e/smoke.py、`internal/webui/assets/index.html`、
  docs/DEVLOG.md）：Nathan 在 01:49–01:52（UTC）连批 5 张卡后停下，第 17 个文件的审批卡 900 秒无人决策而超时，
  驱动按设计停止。**注意：远端目前处于中间态、编译不过**——新 `archive.go` 用到的 `chainHeadHash`
  定义在未推的 `audit.go` 新版里；恢复办法是下一轮重跑驱动即可续推（`python3 /tmp/push_f4b.py`，
  进度在 /tmp/push_f4b_progress.json，已推文件会自动跳过），先推 `audit.go` 即可恢复编译。
  若 Nathan 不在，勿反复刷卡；等他在时一次批完剩余 10 张卡。
- 另：Nathan 已在网页端把 `.github/workflows/go.yml` 改为 `go-version-file: go.mod`（远端提交 cd4e24d），
  CI 第 20 次运行首次全绿——困扰一夜的 CI 红灯已解除。远端 HEAD 现为 cd4e24d（F1 + 该 CI 修复），
  本地不含该提交（仅改了 workflow 文件，内容 API 推送不碰它，无冲突）。

## 2026-11-21 · F3 令牌级限流（RPM/TPM）
- 借鉴 LiteLLM 官方动态限流文档（docs.litellm.ai/docs/proxy/dynamic_rate_limit）：
  每 key 可配 rpm_limit/tpm_limit，超限 429；tokens 在响应完成后才记入计数器，
  在途请求不被刚耗尽的预算拦截；60 秒滚动窗口。我们照此实现，差异是限流键为
  AgentGate 自有令牌、未配的令牌走等级默认（受管级 60 RPM / 100000 TPM，
  自带级 20 RPM / 20000 TPM）。
- 实现：新包 `internal/ratelimit`（60s 滑动窗口，RPM 计请求、TPM 计响应后上报的
  tokens，0 = 不限）；`store.APIToken` 增加 `rpm_limit`/`tpm_limit`；
  网关鉴权后立即检查，超限返回 429 `rate_limit_exceeded` + `Retry-After`，
  并写 Kind `rate_limit` 审计事件（不含请求原文；该 Kind 不计入 F2 用量统计，
  统计对账不受影响）；`GET /api/tokens` + `PATCH /api/tokens/{id}`（仅
  admin/security，不暴露令牌哈希）；管理台新增令牌限流卡片（中英双语）。
- 关键决策：限流检查放在鉴权后、防护管线前——被防护拦截的调用同样计 RPM
  （已占用网关处理）；限流状态为进程内运行时状态，重启即复位（与 F1 熔断一致，
  不引入 Redis）；限流不写新表，审计即事实来源。
- 测试：ratelimit 单元测试 6 项（RPM 边界、窗口滑动、TPM 响应后记账、0 不限、
  Retry-After 递减、200 并发恰好放行 100）+ 网关集成测试 4 项（RPM 429 +
  Retry-After + 审计事件、TPM 429、限流按令牌隔离、等级默认 BYOA 20 RPM）+
  API 端点测试（列表/修改/部分修改/负值 400/未知令牌 404/员工 403、不暴露哈希）；
  `gofmt`/`go vet`/`go test ./...` 全绿；`make smoke` 17 项断言 0 失败
  （含新增 2 项：RPM 配置生效、第 6 次调用 429）。
- 推送（阻塞，待 Nathan 在线时批卡或下一轮重试）：本地提交 1f5d185 已完成；
  按任务书用 `push-files` 全量快照推送两次（UTC 11-20 21:55 与 23:24），两次均因
  GitHub 写入审批卡无人决策而超时（`permissions.history` 查证：Decision 均为
  Timed out；`list_pending` 当前无待批卡）。远端 HEAD 经读回验证仍停在 F1
  （81ab47e），F2（5acccac）与 F3（1f5d185）均未上远端。注意：这不是 push-files
  403——审批超时才是卡点，白天 Nathan 在线批一次卡即可一次推完（F1 当时就是这样
  批卡通过的）。下一轮优先重试推送；若届时仍无人批卡，改做 F4。

## 2026-11-21 · F2 用量与成本统计
- 借鉴 LiteLLM 官方成本追踪文档（docs.litellm.ai/docs/proxy/cost_tracking）与
  Portkey Analytics 文档（portkey.ai/docs/product/observability/analytics）：
  按 key/user/team/model 的 spend 日志与时间范围查询、多维度聚合（调用数、成本、
  平均 tokens、成功率）与任意维度分组。我们照此实现，但统计口径改为**直接聚合
  哈希链审计日志**（不另建统计表，数字与审计天然对账），维度换成企业口径
  （部门/Agent 类型/数据密级/处置），被拦截的调用计入调用数与 blocked（tokens 为 0）。
- 实现：`audit.Log.All()` 快照；新增 `internal/api/stats.go`，`GET /api/stats`
  （仅 admin/security，`since`/`until` RFC3339 半开区间过滤，非法时间返回 400）；
  聚合维度 by_user/by_department/by_channel/by_model/by_agent_type/by_data_level/
  by_decision，桶内含 calls/tokens/blocked，按调用数降序、key 升序；部门由事件的
  ActorID 查用户表解析，未知记为 unknown；被拦截调用无渠道，归入 channel "none"。
  管理台新增用量统计卡片（总调用/tokens/拦截 + 按部门/模型/渠道表 + 处置汇总，中英双语）。
- 关键决策：金额暂不计入——渠道尚未配置单价，计价与部门预算留给 F13；
  统计不写新表、不引入外部存储，审计日志即唯一事实来源（对账零成本）。
- 测试：纯聚合测试（各维度、拦截、范围外事件过滤、半开时间窗口、空输入）+
  HTTP 端点测试（鉴权 401/403、非法时间 400、未来窗口 0 调用）；`gofmt`/`go vet`/
  `go test ./...` 全绿；`make smoke` 15 项断言 0 失败（含新增 2 项：统计与审计对账、员工访问 403）。
- 推送阻塞（待下一轮优先处理）：本地提交 692b8ce 已完成，但向 myjnathan/AgentGate
  main 的 contents API 推送两次均未执行——GitHub 写入每次需新的审批卡，而本轮是深夜
  无人值守运行，两次审批请求均超时未决策（`permissions.list_active` 显示当前无任何
  有效授权）；远端 HEAD 仍停在 F1（81ab47e），`internal/api/stats.go` 经读回验证确认
  未上远端。按 Loop 规程，推送失败留待下一轮优先重试（届时 Nathan 在线可批审批卡）。
  未尝试 push-files：Loop 规程已记明其对非空仓库建 tree 会 403，且同样要过同一审批关。

## 2026-11-21 · F1 渠道健康、重试与熔断
- 借鉴 LiteLLM 官方文档（docs.litellm.ai/docs/router/learn/reliability）：
  可重试错误按类型区分（5xx/429 可重试，其余 4xx 不重试）、`allowed_fails`=3 +
  `cooldown_time`=60s 默认值、冷却按渠道隔离、到期自动恢复、fallback 逐跳上报。
  差异化收紧：故障转移只在与首选渠道**同一 MaxDataLevel** 的渠道间发生，
  绝不借故障之名把数据送到更高密级渠道（也不降级到更低密级渠道）。
- 实现：`pickChannel` 改为 `pickCandidates`（剔除冷却中渠道、按密级升序），
  转发循环遇传输错误/5xx/429 时记失败并试下一个同级渠道；连续失败达阈值
  （默认 3 次）进入冷却（默认 60s），成功清零计数；全渠道冷却时返回 503
  `channels_in_cooldown`；熔断与每次 fallback 均写入审计（不含请求原文）。
  `GET /api/channels` 返回 health/consecutive_failures/cooldown_until，
  管理台渠道表新增健康列（中英双语）；smoke 增加渠道健康断言。
- 关键决策：熔断阈值与冷却时长做成 `gateway.Policy` 可配字段（默认 3 次/60s），
  不引入 Redis 等外部状态——健康状态为进程内运行时状态，重启即复位（与现有
  `Failures` 字段一致，`json:"-"` 不持久化）。
- 测试：新增 7 个测试（5xx 同级转移、冷却进入/到期恢复、全冷却 503、拒绝向高密级
  升级、机密数据不降级、4xx 不重试不计数、成功清零），`gofmt`/`go vet`/`go test ./...`
  全绿，`make smoke` 13 项断言 0 失败。
- 按 Nathan 指示，许可证由 Apache-2.0 变更为 **PolyForm Noncommercial License 1.0.0**：
  源码公开可审计；个人学习/研究与非商业组织免费；**商业使用（含企业内部经营性使用）须另行书面授权**（双许可证模式）。
- 同步更新 README License 节、蓝图 §11 与决策记录、开发 Loop 红线。

## 2026-11-21 · CI 与部署方案
- CI（.github/workflows/ci.yml）：gofmt/vet/build 门禁、race + 覆盖率测试、三平台构建矩阵、
  演示模式端到端冒烟（e2e/smoke.py，12 项断言）、govulncheck 安全扫描；README 加状态徽章。
- 部署参考 dsh-ee 方案落地：单镜像 Dockerfile（前端已嵌入二进制、非 root、/data 卷、HEALTHCHECK）、
  deploy/compose.yml、deploy/install.sh（在线/离线双模式、首次生成 .env、重跑即升级）、
  systemd 单元、Makefile（build/release/test/smoke/image/bundle）、deploy/README.md 部署维护手册。
  差异点：我们不需要 TiDB/OceanBase，单二进制 + 数据目录即可，备份 = 数据目录 + 主密钥。

## 2026-11-21 · Loop 基础设施
- 建立开发 Loop：`docs/development-loop.md` 规程 + 每 2 小时一轮的定时任务。
- 建立 `docs/feature-backlog.md`：调研 LiteLLM/Portkey/Kong/Cloudflare 网关对比、
  IBM ContextForge、New API 生态、dsh-ee、WorkBuddy 企业版，整理出 F1–F16 借鉴清单（P0–P2）。
- 仓库已推送 GitHub：`myjnathan/AgentGate`（main，远端提交 fd2e116）。

## 2026-11-21 · 测试补强（commit 6f989c2）
- 新增 audit/auth/mcp/网关流式共 13 个测试，全部通过，未发现 bug。
- 覆盖：哈希链防篡改、设备码令牌只交付一次、20 次失败锁定、kb_get 工具级分级拒绝、SSE 以 [DONE] 结束。

## 2026-11-21 · v0.1 骨架（commit 5fb8c51）
- Go 单二进制服务端跑通核心闭环：模型网关防护管线（敏感词→DLP→数据分级→分级路由/脱敏还原）、
  MCP 网关（kb 工具 + 工具级授权）、分级令牌 + 设备码接入（ag-setup）、哈希链审计、
  双语 Web 管理台、模拟公司「星河精密」PoC（含 mock 上游）。
- 全流程冒烟测试通过：自带级发机密词被 403 拦截、受管级自动路由私有渠道、PII 脱敏往返、审计链校验有效。
- 期间修复 1 个真实 bug：上游 JSON 对占位符的 HTML 转义（`<PII_1>` → `\u003cPII_1\u003e`）导致响应还原失败，
  已在 guard.Restore 中兼容转义形式并加测试。

## 2026-11-21 · IP-guard 配合调研
- 调研溢信 IP-guard：驱动层透明加密按「授权软件（涉密进程）」授权、官方支持自研软件登记、
  外发解密审批与终端管控齐全、无公开第三方 API → 配合方式定为「策略配置 + 进程准入」。
- 产出 docs/integrations/ipguard.md：三道闸分工、企业 IT 配置清单（含子进程树这一关键坑）、
  8 项验证矩阵、申请材料包、已知边界；探针 5 首个实测对象定为 IP-guard。
- 另修复 .gitignore 误伤：`agentgate`/`ag-setup` 模式曾把 cmd/ 下同名目录排除在版本控制外，已改根锚定并补提。

## 2026-11-21 · 远端同步方式与 CI 文件限制
- GitHub 连接器 push-files 仅能用于空仓库首推；增量同步改用 contents API 逐文件 create_or_update_file（已写入 Loop 规程）。
- `.github/workflows/ci.yml` 被 GitHub 拒绝（App 无 workflows 权限，403），需 Nathan 在网页端手动新建该文件或本地 git 推送；文件内容已随仓库本地提交保存，并单独发给他。
