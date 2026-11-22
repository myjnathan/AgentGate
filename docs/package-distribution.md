# 安装包分发与离线导入（F20）

AgentGate 托管受管 Agent 的安装包：管理员上传或离线导入，员工自助
下载；分发包的 SHA-256 可登记为 F14 证明的基准值，形成「平台分发 →
平台证明」闭环。

借鉴 dsh-ee PR #309「DSH 分发支持导入内网离线安装包」
（hikariming/dsh-ee，Apache-2.0，与 AGPL-3.0 兼容）：离线导入、平台
托管、员工下载、SHA-256 校验并区分已校验 / 未校验、导入中断可恢复、
过期包清理。dsh-ee 的实现与 DSH 插件绑定较深，本项目按自身架构实现，
未复制其代码；借鉴点见 `docs/feature-backlog.md` F20 条目。

## 双包结构（Nathan 2026-11-22，对齐 IP-guard 的分法）

| 包 | 给谁 | 内容 | 分发通道 |
|---|---|---|---|
| 管理包（management） | IT | agentgate 服务端：Docker 镜像 / 离线 bundle / systemd（现有形态，`make bundle`） | 现有离线部署通道；也可在管理台登记为 management 包，便于统一查看 |
| 用户包（user） | 员工 | ag-guard + ag-setup + 生成的 guard.json + MANIFEST.json | 平台托管分发 / 离线导入；员工装一次即常驻、零配置 |

用户包的 guard.json 由平台按策略生成：网关地址、最高数据等级、出口
白名单、Agent 类型。ag-guard 只认其中的 `gateway_url`（其余字段由
Go 的 JSON 解析忽略），出口收紧靠 sidecar 只拨这一个网关地址（见
`docs/local-gateway-sidecar.md`）；`max_data_level` 与
`egress_allowlist` 是该安装的策略记录，供审计与管理台展示。

## 上传与校验

- 单次上传：`POST /api/packages`（multipart，`file` 字段 + 元数据）。
  期望校验和（`expected_sha256`，来自管理员或厂商发布清单）与入库文件
  实算 SHA-256 一致时状态为 `verified`，否则为 `unverified`。未校验的
  包照常托管下载，但**永远不能登记为证明基准值**——管理台明确标出。
- 断点续传：`POST /api/packages/uploads` 建会话 →
  `PUT /api/packages/uploads/{id}?offset=N` 分片上传（每片 ≤ 8 MiB，
  offset 必须等于已收字节数，否则 400）→
  `GET /api/packages/uploads/{id}/status` 查进度 →
  `POST /api/packages/uploads/{id}/complete` 完成并校验。
  会话与分片文件都落盘，服务端重启后仍可从已收字节数续传；
  `DELETE /api/packages/uploads/{id}` 中止并删除分片。
- 单个包上限 256 MiB。过期时间（`expires_at`，RFC3339）须在未来；
  `POST /api/packages/cleanup` 删除所有过期包（文件 + 记录）并返回清单。

## 员工下载

`GET /api/packages/{id}/download`（任意登录用户）：流式下载，响应头带
`X-Package-SHA256` 与 `X-Package-Status`，员工可自行核对校验和。
下载记审计事件（`package_downloaded`，只记包名 / 类型 / 状态，不记
校验和原文）。

## 生成用户包

`POST /api/packages/user-bundle`：以两个已托管的组件包（角色须为
`ag-guard` 与 `ag-setup` 各一）加策略（网关地址、最高数据等级、出口
白名单）生成 zip：`ag-guard`、`ag-setup`、`guard.json`、`MANIFEST.json`
（含各组件的 SHA-256 与 guard.json 的哈希）。生成的包状态恒为
`verified`（哈希在生成时计算，期望值即实际值），可随生成一步登记基准值
（`register_baseline: true`）。

离线做法相同：`make bundle-user`（环境变量见
`deploy/make-user-bundle.py` 顶部注释）从 `make build` 的产物组装同一
布局的 zip；上传到平台时平台重新验算哈希，登记后再由员工下载。

## 证明基准值闭环

`POST /api/packages/{id}/baseline`（仅已校验包，`{"agent_type": "..."}`）：
把包的 SHA-256 加入该 Agent 类型的 F14 证明策略
（`allowed_binary_hashes`）。此后登录时提交的证明若二进制哈希不在列表
中，裁决为 `downgraded`（原因码 `binary_hash_not_allowed`，无哈希上报
则为 `binary_hash_missing`）；未登记基准值的类型保持旧行为（任意二进制
通过）。登记本身记审计事件（只记包名 / 类型，不记哈希值）；重复登记
幂等，不重复写入。

注意：证明仍是软件自报证据（见 `docs/attestation.md` 已知限制）——基准
值保证「装的是平台发的包」，不防有意伪造的证明。

## 管理台

「安装包分发」页：上传表单（文件 + 元数据）、生成用户包表单（组件包
下拉 + 策略）、包列表（类型 / 版本 / 大小 / SHA-256 前 12 位 / 校验状态 /
基准值登记状态）、下载、登记基准值、删除、清理过期包。中英双语。

## 已知限制

- 校验只验 SHA-256（内容完整性），不验厂商签名；签名验证留待后续轮次。
- 组件包本身不校验「确实是 ag-guard/ag-setup 二进制」——平台只保证
  zip 里装的就是管理员指定的两个文件；组件来源可信由上传环节负责。
- 上传会话与包文件在数据目录（`<data>/packages`）内；备份数据目录时
  一并备份。
