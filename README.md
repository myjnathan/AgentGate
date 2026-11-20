# AgentGate

[![CI](https://github.com/myjnathan/AgentGate/actions/workflows/ci.yml/badge.svg)](https://github.com/myjnathan/AgentGate/actions/workflows/ci.yml)

**员工的 Agent 随便换，企业的能力、权限和账本不动。**
*Your employees' agents may change. Your company's capabilities, permissions and audit trail don't.*

AgentGate 是一个**不绑定任何单一桌面 Agent** 的企业级平台（源码公开、自托管）：

- 企业把系统、知识、模型与能力沉淀在平台侧；
- 员工用自己习惯的通用桌面 Agent（DSH、WorkBuddy、Claude Code、Codex 等），经 **OpenAI 兼容协议 + 远程 MCP** 接入；
- 涉及企业加密文件时，只有通过准入标准的**受管 Agent** 能在预防型防护下读取明文——进程白名单 + 出口守卫 + 数据分级路由，而不是事后审计了事。

> 状态：早期开发中（v0.1 骨架已跑通核心闭环）。设计文档见 [docs/blueprint.md](docs/blueprint.md) 与 [docs/enterprise-protection-design.md](docs/enterprise-protection-design.md)。

## 核心能力（已实现）

| 模块 | 说明 |
|---|---|
| 模型网关 `/v1` | OpenAI 兼容；多渠道、按**数据密级**选路（密级不够的渠道根本收不到数据）、月度配额 |
| 防护管线 | 敏感词（Aho-Corasick）→ DLP 检测（凭据/PII/企业自定义词）→ 数据分级 → 拦截 / 脱敏（可逆占位符，响应还原）/ 分级路由 |
| Agent 分级信任 | 受管级 / 自带级分级令牌；自带级禁止发送机密数据；设备码登录（`ag-setup` 一条命令接入） |
| MCP 网关 `/mcp` | 远程 MCP，工具级授权（如 kb_get 仅受管级），工具返回过 DLP，每次调用审计 |
| 知识库 | 关键词多轮检索 + 读原文 + 引用，不用向量库；按等级控制可见范围 |
| 能力中枢 | 技能（SKILL.md）提交 → 审核 → 签名校验和 → 按部门范围下发 |
| 审计 | 哈希链防篡改审计日志，可验证完整性；原文与敏感值不落日志 |
| 管理台 `/` | 内嵌 Web UI（中英双语），渠道/审计/设备批准 |

## 快速开始（PoC 演示模式）

需要 Go 1.27+。

```bash
go build -o agentgate ./cmd/agentgate
AGENTGATE_MASTER_KEY=change-me-please-16+ \
AGENTGATE_DEMO=1 \
./agentgate
# 打开 http://127.0.0.1:8787
# 管理员 admin / agentgate-admin；员工 lixiaofeng / demo12345（模拟公司「星河精密」）
```

演示模式内置一个 mock 模型上游，无需任何真实 API Key 即可体验完整闭环：
普通问答放行、自带级 Agent 发送机密词（XH-200）被拦截、受管级自动路由到私有渠道、PII 自动脱敏并在响应中还原、知识库带引用检索、审计链可验证。

员工接入（另一终端）：

```bash
go build -o ag-setup ./cmd/ag-setup
./ag-setup --server http://127.0.0.1:8787 --agent dsh --tier byoa
# 按提示在网页端批准设备码即可
```

## 架构一瞥

```text
桌面 Agent（任意） ──► [本地隐私网关(规划)] ──► 企业网关（防护管线/分级路由）
                                                    ├─► 签约云模型（低密级）
                                                    ├─► 私有化模型（高密级）
                                                    └─► MCP 工具 / 知识库 / 企业系统
受管 Agent ── 进程白名单（透明加密明文）+ 进程级出口守卫（只准连企业网关）
```

## 与同类项目的区别

- vs **dsh-ee**：它绑定 DSH；AgentGate 不绑任何 Agent，且把 DLP/分级路由/分级信任做成核心。
- vs **New API**：它只管模型流量；AgentGate 延伸到端侧、能力侧与加密文件通道，密钥加密存储不落明文。
- vs **WorkBuddy 企业版**：它绑自家 Agent 与生态、闭源靠厂商承诺；AgentGate 源码公开可审计、中立、自托管。SaaS 型 Agent 自带网盘/同步导致“只能审计、防不住”的结构性问题，见设计文档 §2.1 的准入标准。

## 开发

```bash
go vet ./...
go test ./...
```

仓库结构：`server` 逻辑在 `internal/`，端侧工具在 `cmd/ag-setup`，出口守卫模板在 `endpoint/egress/`，受管 Agent 说明在 `managed-agent/`，部署脚本在 `deploy/`。

## License

**PolyForm Noncommercial License 1.0.0（禁止商用）** — 见 [LICENSE](LICENSE)。

- 免费使用：个人学习、研究、实验，以及慈善、教育、公共研究、公共安全/健康、环保组织与政府机构的使用。
- **商业使用（包括企业内部经营性使用、集成进对外收费的产品或服务）必须另行取得书面商业授权**，请通过 GitHub 联系作者。这保证项目可持续：代码公开接受审计，但不能被白嫖商用。
