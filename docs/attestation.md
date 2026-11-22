# Agent 证明（Attestation，F14）

受管 Agent 登录时先自证身份状态，平台校验通过后才发受管级令牌；
不通过不拒绝登录，而是降级为自带级令牌并告警——员工仍可用通用
能力，企业敏感能力不会落到不合规的 Agent 手里。

## 角色模型（IETF RATS RFC 9334）

| RATS 角色 | AgentGate 落点 |
|---|---|
| Attester（证明方） | Agent 本身（经 `ag-setup` 上报证据） |
| Evidence（证据） | 版本、配置哈希、二进制哈希、插件清单、守卫规则状态 |
| Verifier（校验方） | `internal/attest.Evaluate`：证据对照 Reference Values 与 Appraisal Policy 评估 |
| Reference Values（参考值） | 策略里的最低版本、允许的配置哈希、插件白/黑名单 |
| Appraisal Policy（评估策略） | 每 Agent 类型的 `AttestationPolicy`（管理员配置） |
| Attestation Result（证明结果） | `pass` / `downgraded` / `unverified` |
| Relying Party（依赖方） | 令牌签发（`ApproveDeviceCode`）：只按裁决发对应等级的令牌 |

第二借鉴点是 SPIFFE/SPIRE 的「先证明、后发身份」：没有匹配的
注册条目就不发身份凭证。我们照此：受管级令牌只在证明通过后
发放；身份（令牌等级）即证明链的结论。

新鲜度（RATS 的 nonce 角色）由设备码登录本身承担：证明绑定到
一次设备码（code + polling secret，单次使用、10 分钟过期），
一份证明不能重放到下一次登录。

## 流程

1. `ag-setup` 启动设备码（`POST /api/setup/device/start`）。
2. `ag-setup` 提交证明（`POST /api/setup/device/attest`，凭 code +
   secret）：版本（`-version`，默认 `0.1.0`）、配置哈希（待写入
   的网关配置的 SHA-256，不含令牌）、二进制哈希（自身可执行文件
   的 SHA-256）、插件清单（`-plugins`，逗号分隔）、守卫规则状态
   （`-guard`）、操作系统。返回裁决供员工端显示。
3. 管理员在管理台批准设备码（`POST /api/device/approve`）。批准
   时按**当时**的策略重新评估（提交时的裁决只是预览）：
   - `pass` → 按申请发受管级令牌，令牌标记 `attested=true`；
   - `downgraded` → 发自带级令牌，记 `attestation` /
     `attest_downgrade` 审计事件（F5 告警，warning）；
   - `unverified`（无适用策略，或策略不要求提交证明且未提交）→
     按申请发令牌，令牌标记 `attested=false`。
4. 令牌在 `GET /api/tokens` 中可见证明状态（`attested` /
   `attest_verdict`），管理台令牌表与「Agent 证明」页同步展示。

## 策略字段

`GET/POST /api/attestation/policies`（仅 admin/security；按
Agent 类型 upsert），`DELETE /api/attestation/policies/{agent}`：

| 字段 | 含义 |
|---|---|
| `agent_type` | Agent 类型（如 `dsh`），策略的键 |
| `min_version` | 最低版本（`1.2.0` 形；低于即降级；空为不限） |
| `allowed_config_hashes` | 允许的配置哈希白名单（空为不限；非空时未上报或不在列即降级） |
| `allowed_plugins` | 允许的插件白名单（空为不限） |
| `blocked_plugins` | 禁用的插件黑名单（命中即降级） |
| `require_guard` | 要求守卫规则已应用（未应用即降级） |
| `require_report` | 要求提交证明（未提交即降级；为 false 时未提交只记未验证） |

降级原因只记类别码（`version_below_minimum` /
`config_hash_not_allowed` / `config_hash_missing` /
`blocked_plugin` / `plugin_not_allowed` / `guard_not_applied` /
`report_missing`），不记哈希值本身；审计与告警只含类别码，
不含敏感原文。

演示种子：`dsh` 策略为最低版本 `0.1.0` 且要求提交证明；其余
Agent 类型无策略，走旧行为（按申请发令牌，标记未验证）。

## 已知限制（明说）

- **本轮是软件自报证据**：版本、哈希、插件清单都由 Agent 自己
  上报，平台只做一致性校验，不做密码学绑定。它能拦住配错版
  本、配错配置、装了禁用插件的 Agent，并留下可审计的降级记录；
  **不能防有意伪造**——一个恶意 Agent 可以谎报版本号与哈希。
  硬件根信任（TPM quote、SPIRE 式平台证明）是后续轮次的工作，
  不在本轮佯装已解决。
- 配置哈希只覆盖 `ag-setup` 写入的网关配置骨架（server /
  model_base / mcp_url / agent_type / tier），不含令牌；它证明
  「按企业模板配置」，不证明 Agent 二进制未被篡改（二进制哈希
  是自报值，同样不在本轮验证链内）。
- 证明只在登录（发令牌）时做一次；令牌 30 天有效期内 Agent
  状态变化（升级、装新插件）不会触发重评。持续校验（定期重
  证明、状态变化即降级）留待后续轮次。
