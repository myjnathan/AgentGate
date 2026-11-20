# 进程级出口守卫（Egress Guard）

受管 Agent 的预防型控制核心：**按进程**下发操作系统防火墙出站规则，
受管 Agent 进程只允许连接：

1. 企业 AgentGate 网关（内网地址）
2. 本机本地隐私网关（127.0.0.1）

其余出站一律拒绝。这样即使 Agent 读到了透明加密文件的明文，
也没有任何通道能把数据送出企业网络——预防在前，审计在后。

## Windows（模板：windows-firewall.ps1）

以管理员运行 `windows-firewall.ps1 -AgentPath "C:\Program Files\DSH\dsh.exe" -GatewayHost 10.0.0.10 -GatewayPort 8787`。
脚本创建两条规则：放行到网关、拒绝该程序其余出站。

## macOS

macOS 的 pf 无法按进程过滤。可选方案（强度递减，需如实标注）：
- 网络扩展（NEFilterDataProvider）——正式方案，二期；
- 以专用本地用户运行受管 Agent + pf 按用户过滤；
- 仅 sidecar 强制（Agent 配置只指向本机隐私网关）+ 路由黑洞厂商域名。

## Linux

nftables 按 cgroup/用户过滤，或 systemd 服务单元 `IPAddressAllow/IPAddressDeny` 包裹受管 Agent 进程。

## 校验

平台侧通过 Agent 证明（attestation）定期收集：进程哈希、守卫规则
指纹、Agent 版本与插件清单，与预期不一致时令牌降级为自带级并告警。
