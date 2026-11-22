# 终端持续盘点与断网策略缓存（F23）

把 F14 的一次性登录证明升级为持续证明：`ag-guard` 定期心跳上报
版本 / 配置哈希 / 二进制哈希 / 插件清单，平台维护全量终端
Agent 资产视图；同时 `ag-guard` 本地缓存服务端策略，网关不可
达时按缓存策略执行、高密级默认拦截，恢复后补报断网期间审计。

借鉴：Fleet / osquery 的终端可见性模式（Fleet 核心 MIT）——
主机按计划上报 vitals、服务端按最后上报时间推导新鲜度
（`inventory-heartbeat-age-seconds` 这类 osquery 策略查询即此
意）、Labels 按部门分组；IP-guard 的离线策略缓存（配合方式见
`docs/integrations/ipguard.md`）。

## 心跳与资产视图

- 端点：`POST /api/terminals/heartbeat`（API 令牌认证）。
  Agent 类型与等级取自令牌本身（心跳只能描述持令牌的 Agent），
  会话调用时退回请求体里的 `agent_type`。每（用户，Agent 类型）
  一条记录；心跳只含元数据，不含任何请求内容。
- 漂移：每次心跳用 F14 的同一套评估（`attest.Evaluate`）对照该
  Agent 类型的证明策略——最低版本、允许的配置/二进制哈希、
  插件白名单/黑名单、是否要求守卫。无策略的 Agent 类型永不漂移。
  漂移只记原因码（如 `version_below_minimum`），不记哈希值。
- 新鲜度（与 Fleet 的 heartbeat age 同款推导）：5 分钟内有心跳
  为 online，60 分钟内为 stale，超过 60 分钟为 offline。
- 审计：只记状态变迁，不记每次心跳——否则审计日志会被每分钟
  一次的心跳淹没。事件（Kind 均为 `terminal`）：
  `terminal_registered`（首次见到）、`terminal_drift`（漂移出现，
  触发 F5 告警）、`terminal_drift_cleared`（漂移消除）、
  `terminal_offline_audit`（断网补报，见下）。
- 管理端：`GET /api/terminals`（按部门 / Agent 类型 / 版本 /
  状态 / 是否漂移筛选，按最近心跳倒序）与
  `GET /api/terminals/{id}`（仅 admin/security）；管理台「终端清单」页。

## 断网策略缓存（端侧）

- 策略快照：`GET /api/sidecar/policy`（任意有效令牌可取；快照
  只含词表与 dry-run 标志，不含密钥）。`version` 是快照内容的
  SHA-256，sidecar 藉此识别过期缓存，不信任时间戳。
- 刷新：sidecar 在每次代理请求时借用该请求的令牌，最多每 5 分钟
  在后台刷新一次；成功则换检测器并把快照落盘
  （默认与 `guard.json` 同目录的 `policy-cache.json`，0600）。
  刷新失败时保持现有策略；缓存文件损坏或被篡改时忽略（版本校验
  不通过），继续用启动时的词表。
- dry-run 是端侧操作员的上线选择，刷新**不**改变它：快照里的
  `dry_run` 是网关自身的模式，只作展示，sidecar 始终用本地配置
  的 dry-run 标志。
- 网关不可达时（转发 `Do` 失败），sidecar 按缓存策略执行：
  - 请求密级 ≤ 快照的 `offline_max_level`（默认 internal）：记
    `offline_unreachable`，返回 502 `upstream_unavailable`（可重试
    的 outage）。
  - 请求密级高于该上限：记 `offline_blocked`，返回 503
    `sidecar_offline_blocked`（fail-closed：高密级调用在网关不可
    达时不转发、不排队等重放、不静默降级）。
  - dry-run 模式下永不拦截（只观测），断网时同样只记 unreachable。
- 补报：断网期间的决策缓存在 sidecar 内存中（只含计数与类别，
  不含原文），下一次心跳成功时随 `offline` 字段一并上报；平台
  写一条 `terminal_offline_audit` 审计事件（事件数 / 拦截数 /
  不可达数 / 时间窗口 / 按类别计数），并把摘要记在终端记录上，
  随后清空缓存。

## 端侧心跳

- `ag-guard` 默认每 60 秒发一次心跳（`-heartbeat-seconds` 可改，
  负数禁用）。心跳用最近一次代理请求里见到的令牌认证（只存内存，
  不写盘）；尚未见过任何令牌时等待，不伪造身份。
- 心跳内容：Agent 类型（配置的 `agent_type`，默认 `other`）、
  sidecar 版本、配置哈希（网关地址 + 生效词表 + dry-run +
  Agent 类型 + 插件的 SHA-256，策略刷新后即变化）、二进制哈希
  （sidecar 自身可执行文件的 SHA-256，对应 F20 的分发基准值）、
  插件清单（配置的 `plugins`）、`guard_applied: true`、操作系统、
  以及待补报的断网摘要。
- `ag-guard /status` 新增：策略版本/来源（local/gateway/cached）
  与刷新时间、心跳开关与最近心跳、待补报的断网决策数。

## 已知限制

- 证明仍是软件自报证据（与 F14 同款边界）：心跳内容由端侧上报，
  不防有意伪造；它防的是配置漂移与版本落后，不是恶意终端。
- 断网决策缓冲只在内存中：sidecar 在断网期间重启会丢失尚未补报
  的条目；策略缓存落盘，不受此限。
- 在线状态由心跳到达时间推导：时钟不同步或心跳间隔改大时，
  online/stale 的边界随之漂移；阈值（5 分钟 / 60 分钟）为常量，
  未做成可配项。
- 心跳不做令牌吊销检查之外的事：令牌过期或被吊销后心跳 401，
  终端随即在 60 分钟后显示 offline，不主动告警。
