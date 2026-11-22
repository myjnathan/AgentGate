# 本地隐私网关 sidecar（ag-guard）

本地隐私网关是 AgentGate 双层脱敏的端侧一层：员工本机上运行的
`ag-guard` 守护进程，以 `127.0.0.1` 本地代理形态存在于桌面 Agent
与企业网关之间。任何能改模型 Base URL 的桌面 Agent 都可接入
（这是「不绑 Agent」在端侧的落点）。

```text
桌面 Agent（任意） ──► 127.0.0.1:18765 (ag-guard) ──► 企业网关 ──► 模型
                          │ 本地检测 / 拦截 / 脱敏 / 还原
                          │ 映射只存本机内存，从不落地、不上报
```

借鉴来源：Calcium-Ion/AstrLink 的本地隐私网关做法（端侧检测、
提醒/拦截/可逆脱敏、响应还原、试运行模式）。只借方法，未复制
代码：检测复用本仓库已有的 `internal/guard`，代理与映射生命周期
为本轮自研（纯 Go 标准库）。

## 运行

```bash
# 构建（或 make build，产物在 dist/ag-guard）
go build -o ag-guard ./cmd/ag-guard

# 最简：指向企业网关，监听默认 127.0.0.1:18765
./ag-guard -gateway http://192.168.1.10:8787

# 再把 Agent 的模型地址指到 http://127.0.0.1:18765/v1，
# 或用 ag-setup 一步写好：
./ag-setup --server http://192.168.1.10:8787 --agent dsh --tier managed --sidecar
```

`ag-setup --sidecar` 会把模型地址写成 sidecar 地址、并在 Agent
证明里上报 `guard_applied=true`（与 F14 的证明策略联动：要求守卫
的 Agent 类型，未经 sidecar 的受管申请会被降级为自带级）。

## 配置

优先级：配置文件 < 环境变量 < 命令行参数。

| 配置项 | 配置文件字段 | 环境变量 | 命令行 | 默认 |
|---|---|---|---|---|
| 企业网关地址 | `gateway_url` | `AGENTGATE_GATEWAY` | `-gateway` | `http://127.0.0.1:8787` |
| 监听地址 | `listen` | `AGENTGATE_GUARD_LISTEN` | `-listen` | `127.0.0.1:18765` |
| 违禁词（本地拦截） | `banned_words` | `AGENTGATE_GUARD_BANNED_WORDS`（逗号分隔） | `-banned-words` | 空 |
| 企业自定义敏感词 | `custom_words` | `AGENTGATE_GUARD_CUSTOM_WORDS`（逗号分隔） | `-custom-words` | 空 |
| 试运行 | `dry_run` | `AGENTGATE_GUARD_DRY_RUN=1` | `-dry-run` | false |

配置文件默认读 `~/.agentgate/guard.json`（存在时），可用
`-config` 指定其他路径。示例：

```json
{
  "gateway_url": "http://192.168.1.10:8787",
  "banned_words": ["绝密项目代号"],
  "custom_words": ["XH-200", "星河 XH-200"],
  "dry_run": false
}
```

企业分发受管 Agent 包时，随包下发该配置文件（与出口守卫防火墙
模板同款分发路径，见 `endpoint/egress/`）。

## 策略

请求离开本机前，sidecar 对消息内容与 `tool_calls` 参数做同一
遍扫描（与网关管线相同的检测器）：

- **违禁词**：本地直接拦截（400 `sidecar_blocked`），请求从不
  离开本机；试运行模式下只记录、不拦截。
- **PII / 凭据 / 自定义敏感词**：替换为可逆占位符
  （`<PII_1>`、`<CREDENTIAL_1>`、`<CUSTOM_1>`），同一值在同一
  请求内得同一占位符；响应回程在本地还原。映射只存在于本次
  请求的内存中，请求结束即丢弃，不写盘、不记日志、不上报。
- **密级声明**：检测到的最高密级随 `X-AgentGate-Data-Level`
  发给网关（只升不降）；网关对已脱敏文本重新扫描并取两者较高者。
- **无发现**：原样转发，只附加 `X-AgentGate-Sidecar: ag-guard/0.1.0`
  标识，供网关审计。

网关收到带 sidecar 标识的调用后，在审计事件的 `detail.sidecar`
中记录，管理员可在管理台「设备接入」页或
`GET /api/sidecar/stats` 看到经 sidecar 的调用量。

## 出口守卫联动

sidecar 只拨一个地址——配置的企业网关，其他任何目的地都不会
转发（构造上就不可能）。配合 `endpoint/egress/` 的防火墙模板
（受管 Agent 进程只准连企业网关与 `127.0.0.1`），Agent 即使绕开
sidecar 直连公网也会被系统防火墙拒绝；sidecar 是「唯一合规出口」
的本地执行点。

## 本机状态

```bash
curl http://127.0.0.1:18765/healthz   # {"status":"ok",...}
curl http://127.0.0.1:18765/status    # 计数器与最近决策
```

`/status` 只含计数（请求/转发/本地拦截/脱敏请求/还原值数量、
按类别的发现数）与最近 50 条决策（时间、处置、类别、模型、密级），
**不含任何原文、占位符映射或令牌**。本地拦截只记录在这里，不
上报网关——平台侧只统计到达网关的 sidecar 调用（见上节），这是
有意的分工：端侧细节不出端。

## 上线路径

与产品「先观察 → 再提醒 → 后拦截」一致：

1. 先以 `-dry-run` 跑一周：只记录发现数（`/status` 的
   `findings_by_category`），评估误报；
2. 误报可控后切正式模式；违禁词表与自定义词表随发现逐步补齐；
3. 受管 Agent 的证明策略再把 `require_guard` 打开（见
   `docs/attestation.md`），未走 sidecar 的受管申请自动降级。

## 已知限制

- 本轮只做**规则检测**（正则 + Aho-Corasick 敏感词，与网关同款
  检测器）。本地小模型检测（AstrLink Guard 式）是后续轮次，
  蓝图 P2 试点版已列入。
- 只代理模型流量（`/v1/chat/completions`、`/v1/models`）。MCP
  工具调用仍直连网关 `/mcp`，由服务端工具级授权与工具参数 DLP
  （F6/F7）管控，不经过端侧脱敏。
- 映射只在内存中：sidecar 重启后进行中的请求会失败，须由 Agent
  重试；已完成的映射本就随请求丢弃，无影响。
- 流式响应还原依赖模型原样回传占位符；模型改写或拆分占位符时，
  还原可能不完整（与 AstrLink 的同款限制一致）。
- 违禁词/自定义词表来自本机配置：网关侧改了词表不会自动同步到
  端侧，需随受管包配置重新下发；网关二次检测是兜底。
