# 开发日志（DEVLOG）

Loop 与人工开发的统一记录，最新在最上面。

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
