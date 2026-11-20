# 开发日志（DEVLOG）

Loop 与人工开发的统一记录，最新在最上面。

## 2026-11-21 · 许可证变更为禁止商用
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
