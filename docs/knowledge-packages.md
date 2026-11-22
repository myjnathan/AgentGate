# 专题知识包（F22）

专题知识包是文件夹知识库（F18）与语义索引（F19）之上的**策展层**：
图书馆与案卷之别——文件夹是案卷（有什么就索引什么），知识包是
图书馆员围绕一个具体工作问题精选、写好综合说明、经审批发布的
口径受控的知识单元。价值在三处：**口径受控**（同一问题只有一份
官方综合）、**经验沉淀**（专家的取用说明跟着包走）、**责任可追溯**
（谁起草、谁审批、何时变更，都有审计回执）。

## 形态（借鉴 Liner）

借鉴 Liner（linersh，liner.sh，MIT 许可证，与 AGPL-3.0 兼容）的
调研包形态与策展流程：

| Liner | AgentGate 知识包 |
|---|---|
| `tape.yaml` 来源配方 | 每版的 `sources`（来源清单）+ `references`（引用包） |
| Mixtape：`synthesis.md` + `sources/` + `MIXTAPE.md` | 每版的 `content`（综合正文）+ `citations`（原文回指） |
| Agent 按步骤调研，三处审批关口（综合 / 来源清单 / 说明） | 起草（任何员工）→ 管理员审核 → 发布；新版本只追加不改旧版 |
| 变更回执 | 每次创建 / 加版 / 审核 / 发布 / 删除都记审计事件 |
| `liner catalog` / `liner search` 跨库检索 | MCP `kb_pack_list` / `kb_pack_get` |

Liner 本身是个人单机工具，无多用户 / 分级 / 审计，治理层全部
自研；未复制 Liner 代码（其核心是 CLI + Go TUI，与本项目架构
不匹配），只借形态与流程。

## 数据模型

`store.KnowledgePackage`（持久化于 `state.json`）：

- `name`：snake_case 唯一；`description`；`scope`：部门范围（`*` 为全部），与 Prompt 模板同款语义。
- `status`：`draft | in_review | published | rejected`；`versions` 追加式不可变版本；`published_version` 为生产版本（0 = 未发布）。
- 每版 `PackageVersion`：`content`（综合正文：用途 / 证据用法 / 边界 + 结论）、`citations`（回指）、`sources`（配方）、`references`（引用的其他包 ID）、`change_note`（变更说明）、`level`（创建时算好并存下）。

## 治理规则

1. **包的密级 = 来源与引用包的最高密级。** 创建版本时由
   `internal/knowpack.ResolveVersion` 对照实时文件夹索引算出，
   存于版本上；取用时只读存值，不再查索引。
2. **引用只能向下。** 被引用的包须已发布，且其密级 ≤ 本包来源的
   最高密级——低密级包不得嵌入高密级内容。想引用更高密级的包，
   先给本包加一个同等密级的来源（包的密级随之升高）再引用。
   引用在三处受检：创建版本时、发布时（重新对照实时索引与
   引用包现状校验，失效则 409 拒绝发布）、MCP 读取时（被引用包
   被撤回或改发更高密级后，该引用在响应里按名扣留
   `withheld_references`，不静默提供）。
3. **每条结论必须带原文回指。** 结构保证分两层：
   - 每版至少一条 `citation`（形状校验，`store.ValidatePackageVersion`）；
   - 每条回指在创建时由 `ResolveVersion` 对照索引树填入
     `file / chapter / pages`，不接受作者手写的回指——不存在的
     章节在创建时就被拒绝（与 F19「回指由树填入、模型编造的来源
     会被丢弃」同款保证）。
   - 机器能保证的是「每条回指真实存在」；「每条结论都有回指」
     本身是策展纪律，由起草人的 `change_note` 与管理员审核关口
     承担，不做 NLP 检查。
4. **Agent 永远只读已发布版。** `kb_pack_list` / `kb_pack_get`
   只服务 `status == published` 的包；驳回已发布包即撤回（
   `Published()` 要求 status 为 published）；等级门禁与
   `kb_entries` 同款（自带级最高 internal，绝密包 Agent 永远
   看不到）；部门范围与 Prompt 模板同款。
5. **变更回执。** 创建 / 加版 / 审核 / 发布 / 删除均记审计事件
   （`package_created` / `package_version_added` /
   `package_reviewed` / `package_published` / `package_deleted`），
   Detail 只含包名、版本与密级——**不含综合正文**，综合本身可能
   概括机密文档内容，不应再进审计日志。
6. **配方过期提示。** 管理台列表对当前版算 `stale`：来源文档已
   从索引消失，或引用将被扣留。过期只提示、不自动改包——修订
   配方是策展动作，走新版本。

## 接口

管理 API（`/api/kb/packages`，创建与加版任何登录用户可做，
审核 / 发布 / 删除仅 admin/security）：

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/kb/packages` | 列表（可见性过滤同 Prompt 模板） |
| POST | `/api/kb/packages` | 创建 v1（`in_review`），返回 `id / status / level` |
| GET | `/api/kb/packages/{id}` | 详情（含 `stale`） |
| POST | `/api/kb/packages/{id}/versions` | 追加版本（作者或管理员） |
| POST | `/api/kb/packages/{id}/review` | `{"approve": bool}`；首次批准即发布当前版 |
| POST | `/api/kb/packages/{id}/publish` | `{"version": n}`；发布 / 回滚，发布前重验配方 |
| DELETE | `/api/kb/packages/{id}` | 删除 |

MCP 工具（`/mcp`）：

- `kb_pack_list`：已发布且本等级可见的包（id / 名称 / 描述 /
  密级 / 范围 / 已发布版本 / 来源数 / 引用数，按名称排序）。
- `kb_pack_get`（`name`，包名或 id）：已发布版的综合正文、
  引用回指、来源清单（文件 / 标题 / 密级由索引补齐）、被引用
  包的已发布综合（内联）与被扣留的引用名。结果过 DLP 与自带级
  脱敏（与其他工具同一出口）。

## 演示种子

演示模式（`AGENTGATE_DEMO=1`）在 `demo/folders/公司制度/` 存在
时注册 `onboarding_essentials`（新员工入职必读，internal，
已发布 v1）：来源为 `考勤与请假制度.md` 全文，综合概括请假 /
加班 / 考勤打卡三节，回指为 n1 / n2 / n3（页码均为 1，该文件
共 13 行）。种子包的回指按已知的演示文件树预填；若演示文件
改动，管理台会把包标为 `stale`。

## 已知限制

- 引用有效性只在创建 / 发布 / 读取三处检查：被引用包改发更高
  密级后，引用包在下次读取时才扣留该引用，不自动下线引用包
  （管理台 `stale` 提示策展人修订）。
- 来源文档被删除后，包的已发布综合仍可读（回指已在创建时填好），
  但 `kb_section` 按原文下钻会 404——策展人应修订配方。
- 综合正文不经过模型生成（本轮只做人工策展 + 结构校验）；模型
  代写综合是后续轮次的可能方向，届时须走 F19 同款分级路由与
  脱敏（综合不得成为泄漏通道）。
- 引用回指的页码沿用 F18 的 45 行伪页（.md/.txt/.docx）与 PDF
  真实页码，与文件夹索引一致。
