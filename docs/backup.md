# 备份与恢复运维手册（F27 收尾 A+B1）

> **定位**：F27 阶段三评审（[docs/ha-phase3-design.md](ha-phase3-design.md) §5）的推荐路径是
> A 立即做 + B1 下一步、B2 按需、C/D 不做。本手册即 A 与 B1 的落地：**A 是制度化的离线备份与演练
> （零代码、零新依赖），B1 是对象存储备份（只做备份级，不动存储格式、不引 SQLite、不动接管路径）**。
> 两者共同回答的只有一个问题：**卷坏了怎么办**。阶段一/二防的是"节点死"（秒级接管），
> 这里防的是"卷死"（灾备，恢复是分钟到小时级）。不要把两者混为一谈，也不要把异步备份包装成零丢失。

## 1. 备份什么

| 内容 | 位置 | 丢了会怎样 |
| --- | --- | --- |
| `state.json` | 数据目录（`AGENTGATE_DATA`，默认 `./data`） | 渠道、令牌、用户、策略等全部配置丢失 |
| `audit.jsonl` | 同上 | 热审计日志丢失（链断在恢复点之后） |
| `audit-archive-*.jsonl.gz` + `.manifest.json` | 同上（F4 归档段） | 历史审计段丢失（链断在归档点之前） |
| **主密钥** | `.env` 的 `AGENTGATE_MASTER_KEY` | **备份本身不可用**——渠道密钥与令牌都是主密钥加密的，丢主密钥等于丢全部凭据 |

备份 = 上面四样，缺一不可。主密钥不进任何备份文件（它加密了备份里的凭据），须另行妥善保管
（密码管理器 / 离线纸质 / KMS），恢复时配回同一个主密钥。

**备份内容红线**：备份只含既有的 state 与审计归档（审计不落敏感原文的口径不变）；
对象存储凭证只加密存储、不回显、不落日志。

## 2. A：离线备份（零代码，管理员本来就该做的事）

### 2.1 定期卷快照

- 云服务器：用云厂商的磁盘快照（按云厂商文档操作），建议每天一次、保留 7 天。
- 本地盘 / 虚拟机：用存储层或虚拟化层快照；没有快照能力时，用 §2.2 的离线备份代替。
- 快照是"整个卷"的恢复点；恢复后仍须按 §5 验链（快照本身不保证链不断）。

### 2.2 离线备份脚本

`deploy/backup-offline.sh` 把数据目录打包成一份带时间戳的离线备份：

```bash
AGENTGATE_DATA=/var/lib/agentgate deploy/backup-offline.sh /mnt/backup-disk
# 产出 /mnt/backup-disk/agentgate-offline-20261124-220000/
#   state.json  audit.jsonl  audit-archive-*.jsonl.gz + .manifest.json
#   SHA256SUMS（每份文件的 SHA-256 清单）  RESTORE.txt（恢复步骤速记）
```

- **频率**：每天一次（与 B1 的每小时快照互补：B1 是自动的、异地的；离线备份是不依赖任何服务的底线）。
- **保留**：建议 30 天，过期手工清理（脚本不自动删离线备份——删备份是不可逆动作，须人来做）。
- **存放**：另一台机器、移动硬盘或网盘——**不要放在同一块卷上**，否则卷坏时备份一起坏。
- **执行时机**：建议停服后执行（`systemctl stop agentgate`）；在线执行也可以，但 `audit.jsonl`
  是追加写，拷贝瞬间可能截到半行事件，恢复时以验链为准（§5.3）。

### 2.3 季度演练清单

每季度做一次（与 B1 的取回验链演练合并做一次即可，见 §5）。演练清单：

- [ ] 离线备份与对象存储备份均有最近 24 小时内的成功记录（§4 的状态页 / `agentgate backup status`）
- [ ] 按 §5.2 从对象存储取回一份数据目录，`agentgate backup verify` 验链通过
- [ ] 取回的数据目录能起服务：登录正常、`/api/audit/verify` 的 `chain_valid` 为 true
- [ ] 切换演练：按 [docs/ha-design.md](ha-design.md) §8.3 跑一遍主备切换（阶段一/二的接管路径仍有效）
- [ ] 演练结果记入 DEVLOG（日期、取回的快照名、事件总数、链校验结果、耗时）

**备份没验过恢复等于没备份**：演练的验收是"取回来、验链通过、服务起来"，不是"备份任务跑通了"。

## 3. B1：对象存储备份（本地 S3 兼容，只做备份级）

### 3.1 选型：只做本地

Nathan 2026-11-24 拍板：**对象存储只做本地**——端点必须在同一台机器或内网，
**不接任何云端桶**。推荐形态（二选一）：

| 形态 | 适用 | 说明 |
| --- | --- | --- |
| [Garage](https://garagehq.deuxfleurs.fr/)（推荐新部署） | 有一台常开的内网机器 | 轻量 S3 兼容对象存储，自托管 |
| `rclone serve s3` | 快速验证 / 已有 rclone 的环境 | 一条命令起 S3 端点，见 §3.2；本手册的实测即用它跑通 |
| 已有 MinIO | 已部署 MinIO 的环境 | 可继续用；注意 MinIO 社区版已不再分发（2026-11-24 实测 `dl.min.io` 返回 410 Gone），新部署不推荐 |

S3 兼容即可——客户端只用 PUT/GET/HEAD/LIST/DELETE 五个操作（纯标准库实现 SigV4 签名，
见 `internal/backup/s3.go`，无新依赖）。桶须开启版本控制（防误删）与异地备份（桶本身是新单点）。

### 3.2 配置

两种方式，管理台配置优先于环境变量（服务端每次备份周期重读配置，改完不用重启）：

**方式一：环境变量**（适合部署时一次配好）

```bash
AGENTGATE_BACKUP_S3_ENDPOINT=http://127.0.0.1:8333   # 本地/内网端点，不接云端桶
AGENTGATE_BACKUP_S3_BUCKET=agentgate-backup
AGENTGATE_BACKUP_S3_REGION=us-east-1                 # 可省，默认 us-east-1
AGENTGATE_BACKUP_S3_ACCESS_KEY=<access key>
AGENTGATE_BACKUP_S3_SECRET_KEY=<secret key>
# 可选：AGENTGATE_BACKUP_STATE_INTERVAL_HOURS（默认 1）
# 可选：AGENTGATE_BACKUP_STATE_RETENTION_DAYS（默认 7）
# 可选：AGENTGATE_BACKUP_AUDIT_RETENTION_DAYS（默认 90）
```

**方式二：管理台**（`POST /api/backup/config`，仅 admin）：端点、桶、凭证走接口存入
`state.json`，secret 经主密钥 AES-GCM 加密后持久化、永不回显；`GET /api/backup/status`
只显示脱敏后的 access key（`****` + 末 4 位）与最近一次备份结果。未配置时备份功能关闭，
服务行为与现版本完全一致（启动日志会说明）。

### 3.3 频率定稿（Nathan 2026-11-24 拍板基线）

| 内容 | 频率 | 保留 |
| --- | --- | --- |
| `state.json` 快照 | 每小时一份（即使内容未变也上传——每小时都有快照可取） | 7 天（总是最新的那份永不删） |
| 审计归档段（F4 产生后上传） | 每次备份周期上传尚未上传的段 | 90 天 |
| 取回验链 + 切换恢复演练 | 每季度一次（§2.3 清单） | — |

对象键：`state/state-YYYY-MM-DDTHH-MM-SSZ.json`、`audit/audit-archive-<first>-<last>.jsonl.gz`
（+ `.manifest.json`）。键按字典序即时间序，"最新"即最后一个键。

### 3.4 备份周期里发生什么

服务启动后，后台每 `StateInterval`（默认 1 小时）跑一个周期：

1. 快照内存中的 live state（与 `Save` 同一份 JSON 序列化，不读可能滞后的磁盘文件）上传；
2. 上传本地尚未上传的审计归档段（.gz + manifest 成对上传；缺 manifest 的段跳过并计数——
   没有 manifest 的段在恢复时无法验真，不上传）；
3. 剪枝：删掉超过保留期的远端 state 快照（总是最新的那份永不删）与审计对象；
4. 结果写进审计（`backup_uploaded` / `backup_failed`，只含错误类别，不含 secret、URL 与桶细节），
   并持久化到 `state.json` 的 `Backup.LastRun`，供状态页查看。

手动触发：管理台 `POST /api/backup/run`（仅 admin），或命令行 `agentgate backup upload`。

## 4. 取回与恢复（CLI）

CLI 不依赖服务端、不读主密钥——恢复必须在数据目录为空时也能工作：

```bash
agentgate backup status            # 当前配置（access key 脱敏显示，secret 只显示"已设置"）
agentgate backup list              # 远端对象清单
agentgate backup upload            # 立即跑一个备份周期（state + 未上传的段 + 剪枝）
agentgate backup restore <空目录>  # 取回最新 state + 全部审计段，写锚点，验整条链
agentgate backup verify <目录>     # 只验链：归档段 + 热日志整条链是否完整
```

恢复流程（卷坏后）：

```bash
systemctl stop agentgate            # 1. 停服务（恢复永不覆盖运行中的数据目录）
agentgate backup restore /var/lib/agentgate-restored
# 2. 取回最新 state.json + 全部审计段；热日志以锚点检查点起头，
#    续接在最后一段的 head 上（见 internal/audit.AnchorCheckpoint），
#    新事件不再从 genesis 重起、序号不与归档段冲突
agentgate backup verify /var/lib/agentgate-restored   # 3. 再验一次（restore 内已验过）
# 4. 把恢复目录换成数据目录（或改 AGENTGATE_DATA 指向它），配回同一主密钥，启动
systemctl start agentgate
curl -s localhost:8787/api/audit/verify   # 5. chain_valid 必须为 true，否则不要上线
```

**失败即拒绝**：取回的链验不过（段缺失、被改、顺序错）时 restore 返回失败、文件留在原地供检查，
**绝不在验不过的数据目录上起服务**。恢复出来的新节点仍需同一主密钥才能解开渠道密钥与令牌。

## 5. 演练记录

### 5.1 自动化测试（每次提交都跑）

- `internal/backup`：假 S3 端点（httptest）覆盖上传 / 取回 / 验链全流程
  （`TestServiceRunOnceAndRestore`）、剪枝、未配置、周期运行；SigV4 签名对 AWS 官方示例向量
  （`TestSignV4AWSExample`）；假 S3 认证失败。
- `internal/audit`：整条历史验链——无归档段、两段归档、段被篡改、热日志被篡改、缺 manifest、
  空目录、锚点检查点续接（`TestAnchorCheckpointContinuesChain`）。
- `internal/api`：备份状态未配置、配置 + 手动跑一轮（假 S3）、员工 403。

### 5.2 真机演练（2026-11-24，F27 收尾轮次）

对象存储：rclone v1.76.0 `serve s3`（本机 `127.0.0.1:8333`，桶 `agentgate-backup`）；
服务：演示模式（`AGENTGATE_DEMO=1`，数据目录 `/tmp/drill-data`）。

- 登录 `admin` → 归档审计（`before 2026-11-24T15:00:00Z`），产出 `audit-archive-1-2.jsonl.gz` +
  manifest（2 个事件归档，链有效）→ 停服务。
- `agentgate backup upload`：上传 state 快照 `state-2026-11-24T14-09-34Z.json` + 1 个审计段 +
  manifest，剪枝 0 个；`backup list` 见 3 个对象。**SigV4 客户端对真实 S3 服务端验证通过。**
- 模拟卷坏（数据目录移走）→ `backup restore` 进 live 数据目录被拒（exit 1，明确报错）；
  `backup restore /tmp/drill-restored` 成功：state 与原文件逐字节相同，1 段、2 个事件、链有效、热日志已锚定。
- `backup verify /tmp/drill-restored`：1 段、2 段事件、0 热事件、共 2 事件、链有效。
- 在恢复目录上起服务：登录正常、`/api/audit/verify` 的 `chain_valid` 为 true、`/readyz` 200；
  新登录事件 seq 3，其 `prev_hash` 为归档段 head 哈希（即锚点的 `archived_head_hash`），
  证明恢复后的新事件正确续接在归档历史之后、链不断叉。
- 演练记录见 `~/workspace/goals/agentgate-enterprise-agent-platform/hidden_files/f27-backup-drill/`。
  教训：`pkill -f` 会匹配自己所在的 shell 命令行——停进程一律先按端口取 pid 再 kill。

## 6. 诚实边界

- **RPO 是小时级，不是零**：state 快照每小时一份，卷坏丢的是上次快照以来的变更；
  审计归档段只在 F4 归档后才上传，热日志（未归档部分）只在离线备份里（§2.2）。
- **RTO 是小时级（手动恢复），不是秒级接管**：恢复要人动手（取回 → 验链 → 换目录 → 启动），
  秒级接管仍是阶段一/二的主备路径，B1 不替代它。
- **对象存储是新单点**：桶删、凭证丢、欠费停服 = 备份丢；缓解靠版本控制、凭证加密存储与异地桶，
  不是靠"选了对象存储就安全了"。离线备份（§2.2）是兜底的第二份。
- **B2 的启动条件**（评审 §5）：B1 已跑顺 + 对象存储选型已定 + 有真实客户提出卷坏恢复需求
  （RPO 小时级不够用），三者缺一则 B2 不启动。C（外部数据库）与 D（Raft 多活）不做，理由见评审 §3。
