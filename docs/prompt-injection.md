# 提示词注入防护（F39）

> 状态：切片 1、切片 2 均已落地（2026-11-27）。F39 完成；F41 落地后把动作兜底的工具密级代理升级为真实风险分级。

## 问题

OWASP LLM01（Prompt Injection）：攻击者用精心构造的输入改写模型行为。
分两种：

- **直接注入**——攻击者自己在对话框里输入越狱指令；
- **间接注入**——恶意指令藏在模型稍后读到的内容里：网页、文档、邮件、
  工具返回、知识库条目。EchoLeak（CVE-2025-32711）就是零点击间接注入
  的实例：一个精心构造的邮件让 Copilot 在无用户操作的情况下外发数据。

OWASP 的原话是：目前没有已知的万无一失的防注入方法。产品的口径因此
是「检测 + 来源分离 + 高危动作兜底」三层，不宣传单层拦截率。

## 分工

| 层 | 管什么 | 不管什么 |
|---|---|---|
| F39 注入防护 | 内容**冒充指令**（改写指令、越狱、窃取系统提示词、外发、隐藏、编码藏指令） | 内容本身安不安全（那是 F28） |
| F28 语义复核 | 内容**安不安全**（违规内容、变相敏感表达） | 内容冒充指令 |
| DLP / 数据分级 | 数据**能不能出境**（敏感词、密级、渠道信任） | 指令本身 |

三层口径不冲突：一次调用可以同时触发多层，判定日志用不同的 Kind
区分（`prompt_injection` / `semantic_review` / `violation`）。

## 切片 1：来源打标 + 规则路 + 决策日志

### 来源打标

网关按消息角色给每段内容打来源：

- `user`（role=user）：员工直输；
- `tool`（role=tool，以及 assistant 消息里的 tool_calls 参数）：工具返回
  或 Agent 取回的内容；
- `assistant`（role=assistant 正文）：模型回显——模型可能把上轮被骗
  着重复的注入文本再吐回来；
- `system`：只打标、**不参与规则扫描**。系统提示词本身就是运营方写
  的指令，扫描它会把每一条合法系统提示词都判成注入。

**非用户来源默认「只作数据、不作指令」**：同一句话出现在工具返回
里是可疑信号，出现在用户输入里可能是正常指令。因此一部分规则
（外发、隐藏、控制面标记、编码载荷）只对非用户来源生效。

看不见的边界（明说）：

- 用户自己粘贴进对话框的内容，网关无法区分「用户亲手打的」和
  「用户从别处复制来的」，按用户来源处理；
- Agent 读了工具返回后**不经过网关**就直接动手（例如直接调下一
  个工具），网关看不见——与 F38 的装前校验边界同类：网关只管过
  网关的东西。

### 规则路

`internal/inject`：七条确定性规则，按优先级排序，先命中者定判定。
匹配在 F29 归一化管线的词边界视图（`normalize.Tokens`：全角折叠、
大小写折叠、分隔符与零宽字符剥离、词边界保留）上做 token 序列匹配，
短语之间允许少量间隔 token（预算 8 个），因此「忽略你之前收到的
所有指令」「Ignore all previous instructions」与插符/全角/零宽变体
都命中同一条规则。

| 规则 | 类别 | 严重度 | 来源 |
|---|---|---|---|
| `override` 改写指令（忽略/无视/忘掉 + 之前/所有/以上 + 指令/提示词） | override | 高 | 全部 |
| `jailbreak_roleplay` 角色扮演越狱（假装你是管理员、开发者模式、DAN） | jailbreak | 高 | 全部 |
| `exfil_system_prompt` 窃取系统提示词（输出/告诉我 + 系统提示词/初始指令） | exfil | 高 | 全部 |
| `exfil_external` 对外发送（发到私人邮箱、send to personal email） | exfil | 高 | 仅非用户 |
| `concealment` 隐藏（不要告诉用户、secretly send） | concealment | 中 | 仅非用户 |
| `instruction_marker` 控制面标记（`[SYSTEM]`、`### instruction`） | marker | 中 | 仅非用户 |
| `encoded_payload` 编码载荷（base64/hex 解出指令才命中） | encoded | 高 | 仅非用户 |

编码规则只认「解出来是指令」的 blob：长 base64/hex 串先解码，解码
结果再过同一套规则；解不出来、解出来是 JSON（JWT 之类）或乱码的，
一律不命中。形状本身不是证据。

误报控制（单元测试逐项锁定）：「把周报发到我邮箱」（用户正常指令，
外发规则不对用户生效）、「请忽略之前的格式」（有改写动词、无指令
宾语）、「系统提示词需要评审」（有宾语、无窃取动词）、知识库里的
程序性文字（「员工报销按以下步骤操作」）均不命中。

### 处置

策略只有两个旋钮：启用开关 + 模式（仅记录 / 告警放行 / 拦截，
默认告警放行）。处置表：

| 模式 | 公开/内部 | 机密以上（高严重度） | 机密以上（中/低严重度） |
|---|---|---|---|
| 仅记录 | 只记日志 | 只记日志（永不拦截） | 只记日志 |
| 告警放行（默认） | 告警放行 | **拦截** | 告警放行 |
| 拦截 | 拦截 | 拦截 | 拦截 |

机密以上默认拦截是「默认」二字的意思：仅记录模式是运营方明确选
的上线观察态，永不拦截。规则层是纯本地模式匹配，不调任何模型，
因此与 F28 不同——**绝密内容也照常检查**（F28 的绝密不送审，是因
为它可能把内容送给审核模型；这里什么都不往外送，绝密会话恰恰最
需要检查）。

### 决策日志

- 告警/仅记录/观察期拟拦截：Kind `prompt_injection`，Action
  `prompt_injection_alert` / `_observe` / `_dry_run`，Detail 只有
  `source` / `rule` / `category` / `severity`——**永不含请求原文**。
  告警事件进 F5 Webhook（warning 级）。
- 拦截：Kind `violation`，Action `model_call_blocked`，Detail
  `reason=prompt_injection_blocked` + `injection_rule` /
  `injection_source` / `injection_severity`，HTTP 403
  `prompt_injection_blocked`。
- 每次调用的 `model_call` 事件带一行 `prompt_injection`
  （`rule:alert` / `rule:observe` / `rule:allow` / `dry_run:would_block`），
  与 F28 的 `semantic_review` 并列、可区分。切片 2 起两路结果用 `+`
  连接（如 `rule:allow+sample:alert`），样本命中时拦截审计还带
  `injection_sample` 与 `injection_similarity`。

例外（F24）永不豁免注入拦截：与违禁词、语义复核同级，是安全层，
不是分级豁免。拦截发生在缓存之前，被拦的调用不进缓存、不消耗
例外凭证。

### 管理面

- `GET /api/injection` + `POST /api/injection`（仅 admin/security，
  roles.Policy）：启用开关与模式；策略变更进审计。切片 2 起另有
  语义路开关与相似度阈值（`semantic_enabled` / `semantic_threshold`，
  默认开启 / 0.35），同样只接受 admin/security 的部分更新。
- 管理台「语义复核」页新增「注入防护策略」卡片：开关、模式、运行
  计数（已检查 / 已拦截 / 已告警 / 仅记录）+ 语义路开关与阈值 +
  语义计数（已审 / 已拦 / 已告警 / 深层调用数 / 当前可疑会话数），
  中英双语。
- 运行时计数（`InjectionStats`）只存进程内，不持久化，与 F28 同款。

## 切片 2（已落地，2026-11-27）：语义路接入 + 动作兜底联动

### 语义路

`internal/inject/semantic.go`，复用 F28 的基础设施而不另建一套：

- **样本层**：复用 F28 的 n-gram 向量化/匹配器（`semreview`
  的 Vectorize/Cosine/Matcher，F30 也复用同一向量化器），配 10 条
  注入专用种子样本（`inj_seed_*`，类别 indirect / delegation /
  authority / conceal / exfil），全部是规则路刻意抓不到的改写——
  无显式改写动词、无控制面标记、无规则列出的外发动词。
- **深层**：规则与样本都未命中、内容又可疑时，复用 F28 的深层
  审核端点（网关经 `g.semReviewDeep` 构造的同一 `DeepFunc`）。
- **只审非用户内容**：样本层与深层都只审 tool 结果、assistant
  回显、tool_calls 参数；用户直输的越狱仍由规则路与 F28 负责。
- **脱敏先行**：样本层读原始非用户文本（本地匹配，不出进程）；
  深层读脱敏后的非用户文本（从脱敏副本解析）；脱敏副本解析失败
  时跳过深层调用，绝不把原文送深层。
- **红线与 F28 一致**：绝密内容永不送审（样本匹配与深层调用都
  跳过）；语义路可独立开关（默认开启）；深层超时/故障保守处置
  （机密以上拦截、低密级告警放行）。
- **处置复用规则路同一张 `decideAction` 表**：样本 action block =
  高严重度、alert = 中严重度；深层 Unsafe = 高、Controversial = 中。
  语义路跑在规则层之后、F28 之前（规则未拦截时才跑）；脱敏副本
  与 F28 共用一份。
- 配置：`InjectionConfig` 新增 `SemanticEnabled *bool`（指针：
  nil 表示未配置、默认开启，避免切片 1 存量配置因缺字段而静默
  关闭语义路）与 `SemanticThreshold float64`（0 = 默认 0.35，与
  F28 共用）；阈值校验在 [0,1]。

### 可疑会话追踪

`inject.Tracker`：按令牌 ID 记录，`SuspicionTTL = 30 分钟`，
重复命中刷新窗口；干净调用不提前清除；只存进程内，重启清空。

- 只在规则/语义任一路对**非用户内容**命中 alert/block 时标记；
  用户亲手输入的命中永不标记（用户是本人指令自己的 Agent）；
- 仅记录模式与 DryRun 拟拦截永不标记。

### 动作兜底（MCP）

网关的追踪器经 `InjectionSuspicion()` 共享给 MCP（`main.go` 中
`mcpSrv.SetInjectionSuspicion(gw.InjectionSuspicion())`，nil = 兜底
关闭）。执行点在 `callTool` 开头，覆盖 rest / 联邦 / 内置全部工具：

- 会话可疑且工具的声明最高密级 ≥ 机密时拒绝，错误码 `-32004`
  （与其它数据策略拒绝一致）+ 审计 `violation/tool_call_denied/
  reason=injection_suspect_session`。
- `toolMaxLevel` 代理规则（F41 落地后以真实风险分级替代）：
  `rest.<名>` 取该 REST 工具的 `MaxDataLevel`；`<命名空间>.<工具>`
  取上游的 `MaxDataLevel`；内置知识工具取 `kb.MaxLevelForTier`
  （受管级 = 机密、自带级 = 内部）；whoami 与未知工具不视为高危
  （未知工具仍走原有 "unknown tool" 路径）。
- `tools/list` 不过滤可疑（可疑是瞬态，列表不得向 Agent 泄露）；
  `prompts/get` 不拦截（模板是管理员发布内容，不移动企业数据）。

### 诚实边界（新增）

- **长文档内嵌改写可能低于样本阈值**：余弦相似度按全文 trigram
  计算，长文档稀释后相似度可能低于 0.35 阈值（如内嵌改写共享
  trigram 覆盖约 70% 时相似度仅 0.04，低于 containment 分支 80%
  阈值）。这是样本层的职责边界——长文档内的改写由深层复核
  负责；样本层只保证短文本改写的检出。
- 兜底的「高危」现为工具密级代理，不是真实风险分级：一个机密级
  但只读的工具也会被拦，一个内部级但会外发数据的工具拦不住。
  F41 落地后以动作维度四分级替代。

## 借鉴

- OWASP LLM Top 10（2025）LLM01 Prompt Injection：直接/间接注入
  的区分、「把外部内容与指令分开」（segregate and identify
  external content）、「输入输出过滤只是七项措施之一，不是防线
  本身」——本层的来源打标即该措施的网关实现。
- 1Panel AI 网关 / 华为云 WAF 大模型检测：输入侧注入检测动作可设
  为仅记录/脱敏/拦截三档——本层的模式旋钮同源。
- F29 归一化管线：规则匹配前的预处理（全角/大小写统一、分隔符与
  零宽字符去除）直接复用，并为英文短语匹配新增词边界视图
  `normalize.Tokens`。
