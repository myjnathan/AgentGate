# 开发日志（DEVLOG）

Loop 与人工开发的统一记录，最新在最上面。

## 2026-11-25 · 备份自动建桶（EnsureBucket，待办登记 13）

- 本轮按待办登记 13 执行（MEMORY.md 2026-11-25 新增：F27 收尾遗留——备份服务不会自动创建对象存储桶，上一轮演练失败的根因、当轮以预建桶绕过；下一轮先补自动建桶并用全新未建的桶重演一遍，再做 F36 第一切片）。本轮只做 EnsureBucket + 全新桶重演；F36 第一切片顺延下一轮——本轮已把 F36 审查的地基打好：演示模式 + 无头浏览器逐页截图 24 个管理台页面（桌面 1440px 全页 + 登录页 + 390px 窄屏三页），0 控制台/页面/请求错误，证据在 `~/workspace/goals/agentgate-enterprise-agent-platform/hidden_files/f36-audit/`（截图 + `audit-shots.js`），下一轮据此写 `docs/ux-audit.md`。另记：窄屏下渠道页存在横向溢出（scrollWidth 816 > 390，表格无滚动容器），已列入 F36 清单候选。
- 实现：`internal/backup/s3.go` 新增 `HeadBucket`（桶级 HEAD，404 = 不存在）+ `CreateBucket`（桶级空体 PUT；409 视为已存在、返回 created=false，解决并发建桶竞态）+ `EnsureBucket`（HEAD 不在则 PUT 创建，返回是否由本次创建）；`Service.RunOnce` 在上传前先 `EnsureBucket`（创建时记一行 operator 日志，不含桶名；失败则整周期按既有 `fail()` 路径记 `backup_failed`）；只读路径（取回/列表/验链）刻意不调用——桶不存在时直接报错，不悄悄建出空桶。`docs/backup.md` §3.1/§3.2/§3.4 同步：桶无需预建、只读操作不建桶。
- 关键决策：① HeadBucket 失败即整周期失败、不做 405/501 回退——rclone/Garage/MinIO/AWS 均支持 HeadBucket，对不支持的端点静默放行只会把错误推迟到 PUT、更难定位；② CreateBucket 用空体 PUT、不用 LocationConstraint——本地形态（rclone/Garage/MinIO）都接受，云端桶不在定位内；③ 发现并钉住假 S3 的测试缺口：旧 fake 对任何桶都来者不拒，是上一轮"测试全绿、真机失败"的根因——本轮起 fake 严格模拟桶存在性（桶不存在时对象操作一律 NoSuchBucket）。
- 测试：backup 包新增 4 项（`TestEnsureBucketCreatesMissingBucket` / `TestEnsureBucketRaceConflict` / `TestPutObjectMissingBucketFails` / `TestServiceRunOnceCreatesBucket`）；旧 `TestClientAgainstFakeS3` 改为先 EnsureBucket；api 包 `fakeS3Server` 补 HEAD（按是否 PUT 过返回 200/404，建模"桶不存在直到被创建"）——此前 HEAD 405 导致 `TestBackupConfigAndRun` 502，全量首跑另有 1 项 alert 包偶发失败（`TestNotifyDeliversAsyncAndSkipsDisabled`，与本轮无关、单跑 5 次全绿）。`gofmt`/`go vet`/`go test ./...` 全绿；`make smoke` 0 失败。
- 本轮实测（全新桶真机演练，脚本 `~/workspace/goals/agentgate-enterprise-agent-platform/hidden_files/f27-ensurebucket-drill/drill.py`，记录见同目录 `README.md` 与 `drill-log.md`）：rclone serve s3 全新 s3root、桶 `agentgate-ensurebucket` 从未预建——启动时的首个备份周期自动建桶并上传 state（日志有 `created object storage bucket`）；CLI 上传在桶已存在时幂等；归档 6 事件 → 上传 state + 1 段 + manifest → 卷坏模拟 → 取回验链通过 → 恢复目录上起服务、登录与 `/api/audit/verify` 链有效。20 项检查 19 通过。
- 演练抓到的真问题（不在本轮范围内，下一轮优先）：恢复后服务器上模型调用 502 `channel_no_enabled_key`——渠道 API 密钥从不落盘（`store.Channel.Keys` 与 `ChannelKey.Enc` 均为 `json:"-"`，代码里 Webhook 注释明说 "Channel.Keys, which predates persistence"）。实测确认：经 API 给渠道加一把密钥并保存后，磁盘 state.json 里仍无该密钥；重启/恢复后全部渠道 0 把密钥、全部模型调用 502，直到管理员逐个重录。上一轮演练第 20 项 FAIL（401）是同一根因的不同表现。下一轮：持久化加密后的渠道密钥（复用 Webhook/上游凭据已有的 AES-GCM 加密存储形态，API 仍只回显脱敏值），再重跑本演练使第 19 项变绿。

## 2026-11-24 · F27 收尾实现（A+B1，待办登记 11）+ F36/F37 登记（待办登记 12）

- 本轮按待办登记 11 执行（Nathan 2026-11-24 20:58 拍板：接受评审推荐路径 A+B1，B2 按需不立项、C/D 不做；对象存储只做本地；备份频率与演练节奏按行业常规定稿；A+B1 合并后按评审 §6 把 F27 标记 done，队列清空转待机）+ 待办登记 12 的登记动作（Nathan 2026-11-24 指示：体验总原则写入 backlog 顶部与 DEVLOG，F36/F37 写入 backlog、队列排在 F27 收尾之后）。体内的 12:26 特别指令与待办登记 1–10 均已在先前轮次执行完毕、作废；本地另有一条 docs 提交 26f2fad（F27 阶段三评审推送结果记录）随本轮整包快照一并带上。队列现为 F36 → F37（F27 已 done）。
- 续接说明：本轮开始时工作区已有未提交的 B1 实现（上一轮中断遗留：`internal/backup`、`internal/api/backup.go`、CLI、`audit.VerifyFullHistory`/`AnchorCheckpoint`、`store.BackupConfig`、semreview 顺带修，文件时间 2026-11-24 13:37–13:48 UTC）。本轮先核查而非重做：逐文件读过实现、`go vet` 与 `go test` 全绿、覆盖了上传/取回/验链全流程，确认可用后补齐缺失的 A 部分（运维文档 + 离线脚本）、真机演练、backlog/DEVLOG，再统一提交推送。
- 借鉴点（2026-11-24 查阅/实测一手来源）：① MinIO 社区版分发状态：`dl.min.io/server/minio/release/linux-amd64/minio` 实测返回 410 Gone（"archived and no longer maintained"），新部署不推荐 MinIO，文档改推 Garage 或 `rclone serve s3`；② rclone v1.76.0 `serve s3`（`--auth-key` 配对认证、SigV4）：真机演练的 S3 服务端，见下；③ AWS SigV4 官方示例向量（测试内嵌）——签名实现对向量通过。无其他外部做法可抄。
- 复用检查（Nathan 复用优先指示）：未复制代码——S3 客户端是纯标准库手写 SigV4（PUT/GET/HEAD/LIST/DELETE 五个操作，引 minio-go 会为一把调用破坏单二进制零依赖定位）；其余约 1200 行实现 + 1100 行测试均为自研，无新依赖。Litestream（Apache-2.0）未采用：B1 是备份级、不做复制级，B2 时再评估。
- 实现：A——`docs/backup.md` 运维手册（定期卷快照、离线备份、季度演练清单、频率定稿、恢复流程、诚实边界）+ `deploy/backup-offline.sh`（带时间戳目录与 SHA-256 清单的离线备份，实测可用）+ `deploy/README.md` 备份节同步；B1——新包 `internal/backup`（`config.go`：环境变量配置与校验、端点须为裸 http(s) origin 不得带路径/query/凭证；`s3.go`：SigV4 签名、路径式寻址、512 MiB 单对象上限；`backup.go`：`Service` 周期上传 state 快照 + 未上传的审计归档段 + 剪枝、串行化防重跑、取回时写锚点检查点并验整条链、拒绝覆盖 live 数据目录）+ `agentgate backup` CLI（upload/restore/list/verify/status，不读主密钥、不写审计）+ 管理台 API `GET /api/backup/status`、`POST /api/backup/config`、`POST /api/backup/run`（仅 admin；secret 经主密钥 AES-GCM 加密存储、永不回显，状态只显脱敏 access key）+ `store.BackupConfig`/`BackupRunSummary`（最近一次备份结果持久化供状态页查看）+ `audit.VerifyFullHistory`（归档段按序 + 热日志、检查点行跳过、manifest 与段字节逐项核对）+ `audit.AnchorCheckpoint`（恢复后热日志以锚点起头，`audit.go` 开日志时遇锚点检查点即以后者 `archived_head_hash` 为下一事件的 prev_hash——恢复后的新事件续接在归档段 head 之后，不从 genesis 重起）；另顺带修 semreview 深层超时判定：deep 层自报 `DeadlineExceeded` 时也按超时处置（此前只看调用方 ctx，1ms 测试上下文的时序抖动根因，F34 轮次记过一次偶发失败）。
- 关键决策：① 频率定稿（登记 11 基线）：state 快照每小时、保留 7 天（总是最新的那份永不删）；审计归档段随 F4 归档上传、保留 90 天；每季度取回验链 + 切换恢复演练；定稿值已写进 `docs/backup.md` §3.3 与代码默认值。② 对象存储只做本地：端点须在同机或内网，不接任何云端桶；文档只写本地形态（Garage / rclone serve s3 / 已有 MinIO）。③ 恢复锚点不从 genesis 重起：否则恢复后的新事件与归档段序号/链都断叉——锚点检查点 + `audit.go` 的 prev_hash 回落是本轮最关键的一处接线，已由真机演练验证（新事件 seq 3 的 prev_hash 即归档段 head 哈希）。④ 未配置时功能关闭、行为与现版本一致（启动日志说明）；配置错误只记日志、不阻塞启动。
- 测试：backup 包 10 项（假 S3 端点覆盖上传/取回/验链全流程、剪枝、未配置、周期运行；SigV4 对 AWS 官方示例向量；配置校验与环境变量解析；假 S3 认证失败）+ audit 7 项（整条历史验链：无归档段、两段归档、段被篡改、热日志被篡改、缺 manifest、空目录、锚点续接）+ api 2 项（状态未配置、配置 + 手动跑一轮、员工 403）+ 冒烟新增 3 项断言（未配置时状态/手动跑 503、员工 403）；`gofmt`/`go vet`/`go test ./...` 全绿（34 包）；`make smoke` 314 项断言 0 失败（基线 311 + 新增 3，连跑 3 次均 0 失败）。另记：全量首跑时 `TestPolicyCacheSurvivesRestart`（sidecar）失败一次，单跑与重跑全绿——与本轮改动无关（未碰 sidecar），记为已知偶发抖动、不修。
- 本轮实测（真机演练，rclone v1.76.0 `serve s3` 本机桶 + 演示模式服务）：归档审计产出 `audit-archive-1-2.jsonl.gz`（2 事件）→ `agentgate backup upload` 上传 state 快照 + 1 段 + manifest → 模拟卷坏（数据目录移走）→ restore 进 live 数据目录被拒（exit 1）→ 恢复到空目录成功（state 与原文件逐字节相同、链有效、热日志已锚定）→ `backup verify` 全链通过（1 段、2 事件）→ 在恢复目录上起服务：登录正常、`/api/audit/verify` chain_valid=true、`/readyz` 200，新登录事件 seq 3 续接在归档段 head 哈希之后。演练记录见 `~/workspace/goals/agentgate-enterprise-agent-platform/hidden_files/f27-backup-drill/`；教训：`pkill -f` 会匹配自己所在的 shell 命令行——停进程一律先按端口取 pid 再 kill（本次又中招一次）。
- 登记 12 执行：体验总原则（八条标尺）已写入 backlog 顶部与本条；F36（全站体验审查与修复）与 F37（全站动效系统）已写入 backlog（P1，队列 F36 → F37，均允许跨多轮切片推进；F37 在 F36 完成后再开工；范围以管理台为主，demo-site 只处理与标尺明显冲突的问题）；README 文档索引同步（新增备份手册链接）。下一轮：F36 首个切片（先产出 `docs/ux-audit.md` 审查清单，再按清单修第一批问题；每轮只完成一个可独立验证的切片）。
- 推送结果：本地提交 e5c1287（含随带的 26f2fad，即 F27 阶段三评审推送结果记录）+ e4c5b2a（bash 调用离线脚本的小修）；复用远端分支 loop/f27（头为阶段三评审快照提交 64859df9，已合入 main；推送前核对同步基点：10 个改动文件里 9 个远端 blob 与本地父提交 26f2fad 一致，DEVLOG.md 一处差异已核对——本地父提交多一段阶段三评审推送结果记录，正是随本轮带上的内容；13 个新文件远端确认不存在）；整包快照 23 文件一次推送（提交 d40716cc），推送后 23 个文件远端 blob 与快照逐一核对一致；另记：快照推送不保留执行位——`deploy/backup-offline.sh` 在远端树里为 100644（与仓库里已有的 `deploy/install.sh` 同样为 100644 一致），故补推第二批快照（3 文件，提交 e6c39d66，文档与脚本头改为 `bash deploy/backup-offline.sh` 调用），推送后 3 文件同样核对一致；PR #36 文件清单与本轮一致（23/23）；未触发 contents API 回退；已合并进远端 main，远端 HEAD a309cbdd（合并提交），读回 main 上 internal/backup/s3.go、docs/backup.md、docs/feature-backlog.md 三文件 blob 与快照一致。另记：① 第二批推送两次因服务重启中断（调用未完成），每次中断后先用只读通道核对分支文件 blob、确认未落盘再重试，第三次获批成功——写通道中断后绝不盲重推；② 同步记录已追加到 ~/workspace/agentgate-f27-snap.muse-github-snapshot-sync.json（第四、五条，提交 d40716cc + 23 路径、e6c39d66 + 3 路径）。待删分支仍为 loop/f27（共 25 个，需 Nathan 网页端手动删除；本轮复用同一分支，未新增）。下一轮：F36 首个切片。

## 2026-11-24 · F27 双机热备（阶段三方案评审，待办登记 10）

- 本轮按待办登记 10 执行（Nathan 2026-11-24 17:45 拍板）：F27 阶段三先做方案评审再决定是否开工——本轮只产出评审文档，不写实现代码；评审结论出来前 Loop 不得自行开工阶段三。体内的 12:26 特别指令与待办登记 1–9 均已在先前轮次执行完毕、作废。本地另有一条 docs 提交 811912a（F27 阶段二推送结果记录）随本轮整包快照一并带上。队列现为 F27（阶段三评审已完成、是否开工待 Nathan 定夺；定夺前 Loop 只做巡检与小修）。
- 借鉴点（2026-11-24 查阅一手来源）：① Litestream 官方《How it works》与恢复指南：WAL 页异步流式复制、LTX 文件、定期快照；默认约 1 秒同步间隔、硬崩溃最多丢约 1 秒写入；恢复 = 最新快照 + 重放其后 LTX，支持 `-timestamp` 时间点恢复（最细恢复点只在 L0 保留期内，默认压实后 5 分钟；快照默认保留 24 小时）；"single writer per replica path"；副本不是可查询的热库，须 restore 成文件才能用；自我定位是灾备而非高可用。② SQLite 官方《Write-Ahead Logging》（sqlite.org/wal.html）：WAL 提交追加进 WAL 文件、检查点搬回数据库文件、默认 1000 页自动检查点；"All processes using a database must be on the same host computer; WAL does not work over a network filesystem"——WAL 须用本地盘，是"双节点各用本地盘 + 复制"这一形态的决定性依据。
- 复用检查（Nathan 复用优先指示）：本轮只产出评审文档，未复制、未引入任何代码；Litestream 为 Apache-2.0（与 AGPL-3.0 兼容），若将来 B2 采用，须在 DEVLOG 记录来源与许可证。
- 实现：无实现代码——评审文档 `docs/ha-phase3-design.md`（候选形态对比：A 维持现状 / B 状态复制 / C 外部存储 / D Raft 对照；推荐：A 立即做 + B 分两步走，B1 备份级先行、B2 复制级按需，不做 C/D；F27 收尾定义：Nathan 拍板后续路径或决定不做即关闭，拍板前保持 doing、Loop 不自行开工；需 Nathan 定夺的五项事项：推荐是否接受、对象存储选型、RPO/RTO 目标、B2 是否立项、F27 收尾）；`docs/ha-design.md` 首部状态与 §7 同步指向评审文档；backlog F27 条目补阶段三方案评审段落、队列行更新；README 文档索引同步。
- 关键决策：① 本轮进行中 Nathan 的待办登记 10 到达时，我已按原计划写好了一小段阶段三 3a 实现代码（状态快照版本号 + HA 状态端点，未提交）；按新指令当即全部回滚（git checkout + 删除新文件），工作区恢复干净后只写评审文档——评审结论出来前不开工。② 推荐取"A + B1 先行、B2 按需"而非直接上 B2：阶段三的真实收益是"卷坏可恢复"（灾备）不是"接管更快"（阶段一/二已秒级）；B1 以一半工作量拿 B 的大部分收益，且不动存储格式、不引 SQLite；B2 的三个启动条件（B1 跑顺、选型已定、真实客户需求）缺一不可。③ 复制就绪的钩子（快照版本号、HA 状态端点）记为 deferred，留待 B2 启动时实现，不提前写代码占位。
- 测试：本轮无代码改动——基线 `gofmt`/`go vet`/`go test ./...` 全绿（回滚后复核，33 包；冒烟未重跑，端到端流程无改动）。
- 推送结果：本地提交 cf76094（含随带的 811912a，即 F27 阶段二推送结果记录）；复用远端分支 loop/f27（头为阶段二快照提交 2da20be4，已合入 main；推送前核对同步基点：3 个改动文件远端 blob 与本地父提交 811912a 一致，DEVLOG.md 一处差异已核对——本地父提交多一段阶段二推送结果记录，正是随本轮带上的内容；新文件 1 个远端确认不存在）；整包快照 5 文件一次推送（提交 64859df9），推送后 5 个文件远端 blob 与快照逐一核对一致；PR #35 文件清单与本轮一致（5/5，新增 153 行/删除 10 行，含随带的 DEVLOG 段落一行）；未触发 contents API 回退；已合并进远端 main，远端 HEAD f06f52d5（合并提交），读回 main 上 docs/ha-phase3-design.md 的 blob 与快照一致。另记：① pull_request_read 首次调用因服务重启中断，重试后获批成功——只读类调用失败时先查 pending 再重试；② 合并调用等待审批约 85 分钟后获批成功；③ 同步记录已追加到 ~/workspace/agentgate-f27-snap.muse-github-snapshot-sync.json（第三条，提交 64859df9 + 5 路径）。待删分支仍为 loop/f27（共 25 个，需 Nathan 网页端手动删除；本轮复用同一分支，未新增）。下一轮：携带本轮推送结果记录；F27 阶段三是否开工待 Nathan 定夺（对象存储选型、RPO/RTO 目标、B2 是否立项等五项事项），定夺前 Loop 只做巡检与小修。

## 2026-11-24 · F27 双机热备（阶段二：fsync 选项 + 备节点预热 + 租约式接管评估）

- 本轮按常规规程选条目：P0、P1 全部 done，P2 只剩 F27；按队列顺序继续 F27 阶段二（阶段一已于上一轮合并，PR #33，远端 HEAD e12f8bf8）。体内的 12:26 特别指令与待办登记 1–9 均已在先前轮次执行完毕、作废；本地另有一条 docs 提交 7e89aca（F27 阶段一推送结果记录）随本轮整包快照一并带上。队列现为 F27（阶段三为远期状态外置）。
- 借鉴点：本轮未查新的一手来源——fsync（写文件 + 原子改名 + fsync 目录三步）与轮询预热均为 POSIX 常规做法，租约评估的论证（时钟漂移 → 误判夺锁、夺锁后需 fencing）沿用阶段一已查的 Litestream「single writer per replica path」与 K8s 探针分工的同一批材料；无新外部做法可抄。
- 复用检查（Nathan 复用优先指示）：未复制代码——约 200 行纯标准库自研（`audit`/`store` 的 `SetSync`、`ha.AcquireStandby`、`main.go` 接线），无新依赖。
- 实现：① 写路径 fsync 选项——`audit.Log.SetSync(true)`（追加写后 `fsync`）与 `store.Store.SetSync(true)`（保存时先 `fsync` 临时文件、原子改名后 `fsync` 所在目录，临时文件写失败时清理），`main.go` 以 `AGENTGATE_FSYNC=1` 同时开启两者，硬掉电 RPO 收窄到「最后一次已完成的调用/保存」，默认关闭；② 备节点预热——新增 `ha.AcquireStandby`（非阻塞试锁 + 每 5 秒轮询，轮询间隙调 warmup 回调；`main.go` 的 standby 路径用它替代阻塞 flock，warmup 只读重读并解析 `state.json`、记一行 `ha: standby warmup` 日志，失败只记日志永不阻塞接管；接管日志带预热次数）；③ 租约式接管评估完成并写入 `docs/ha-design.md` §9，结论不采用（误判即双写、夺锁后缺 fencing、缺口已有 `/readyz` 排空缓解、租约不解决共享卷单点），阶段三（双节点各用本地盘 + 状态复制）时重评；`docs/ha-design.md` 同步更新 §1 RPO、§6 诚实边界与 §7 阶段二状态；`Makefile` 的 smoke 目标现透传 `AGENTGATE_FSYNC` 环境变量。
- 关键决策：① 单开关 `AGENTGATE_FSYNC` 同时覆盖两条写路径——设计文档初稿写的是 `AGENTGATE_AUDIT_FSYNC`（只覆盖审计）；两份文件共享同一个 RPO 窗口，只开审计 fsync 而状态仍丢最后一次变更，是「半持久」陷阱，不如一个开关管到底，改名原因已写进设计文档 §7。② 备节点用轮询等锁（5 秒）而不用阻塞 flock——每轮询间隙跑 warmup；代价是接管最多慢 5 秒，仍在 RTO ≤ 1 分钟预算内；`Acquire` 的阻塞 standby 分支保留（既有测试与 API 不变），生产 standby 路径走 `AcquireStandby`。③ warmup 只读 `state.json`、失败只记日志永不阻塞接管；`state.json` 原子改名写入，预热读到的永远是整份快照。④ 租约式接管本轮不引入租约文件与心跳写入，避免给将来「租约已部署」的误解留接口；`agentgate.lock` 仍是唯一的互斥机制。
- 测试：audit 1 项（fsync 开启时追加落盘且链可验、关闭后照常追加、失败路径仍进 `LastError`）+ store 1 项（fsync 开启时保存可重载、关闭后照常保存、无残留 `.tmp`）+ ha 3 项（等锁期间 warmup 至少跑 1 次且接管 `waited=true`、空闲锁不跑 warmup、warmup 为 nil 时照常等锁接管）；`gofmt`/`go vet`/`go test ./...` 全绿（33 包）；`go test -race`（audit/store/ha）全绿；`make smoke` 在 `AGENTGATE_FSYNC=1` 下 311 项断言 0 失败（断言数不变，fsync 路径已覆盖）。
- 本轮实测（真机双进程演练，`AGENTGATE_FSYNC=1`，临时数据目录）：standby 启动后每 5 秒一行 warmup 日志（state.json 字节数/用户数/渠道数，共 7 次）；主节点 `kill -9` 后备节点约 2 秒内接管（日志 `ha: took over as active`，含 7 次预热读数）、`/readyz` 200、登录与调用正常；接管后 `/api/audit/verify` 为 `{"chain_valid":true}`、导出头 `X-Audit-Chain-Valid: true`，审计链跨接管不断叉。演练教训：① 登录端点是 `/api/login`（不是 `/api/auth/login`），上轮已记过、这次又写错——演练前先对 `e2e/smoke.py` 核对端点；② `audit.jsonl` 在首个事件追加前不存在（`Open` 不建文件），空链接管后首个事件照常续接，属正常行为；③ `pkill -f` 会匹配自己所在的 shell 命令行——以后按端口/pid 精确停进程。演练日志与数据在 `~/workspace/goals/agentgate-enterprise-agent-platform/hidden_files/f27-phase2-drill/`。
- 推送结果：本地提交 a24c0bd（含随带的 7e89aca，即 F27 阶段一推送结果记录）；复用远端分支 loop/f27（头为阶段一快照提交 204370ce，已合入 main；推送前核对同步基点：10 个改动文件远端 blob 与本地父提交 7e89aca 一致，DEVLOG.md 一处差异已核对——本地父提交多一段阶段一推送结果记录，正是随本轮带上的内容；新文件 2 个远端确认不存在）；整包快照 13 文件一次推送（提交 2da20be4），推送后 13 个文件远端 blob 与快照逐一核对一致；PR #34 文件清单与本轮一致（13/13，新增 483 行/删除 30 行，含随带的 DEVLOG 段落一行）；未触发 contents API 回退；已合并进远端 main，远端 HEAD 28fa2770（合并提交），读回 main 上 internal/ha/lock_unix.go、docs/ha-design.md 两文件 blob 与快照一致。另记：① list_branches 前两次调用因服务重启中断、无待批卡，第三次重试获批成功——只读类调用失败时先查 pending 再重试；② 同步记录已追加到 ~/workspace/agentgate-f27-snap.muse-github-snapshot-sync.json（第二条，提交 2da20be4 + 13 路径）。待删分支仍为 loop/f27（共 25 个，需 Nathan 网页端手动删除；本轮复用同一分支，未新增）。下一轮：F27 只剩阶段三（远期状态外置：SQLite WAL + Litestream 式异步复制，工程量大、启动前需方案评审与 Nathan 定夺是否启动）。

## 2026-11-24 · F27 双机热备（阶段一：方案评审 + 主备 + 就绪探针）

- 本轮按常规规程选条目：P0、P1 全部 done，P2 只剩 F27；按 F27 条目「启动前先做方案评审」的要求，本轮交付方案评审（`docs/ha-design.md`）+ 阶段一最小完整切片（主备单写锁 + `/readyz` 就绪探针 + 写健康可见性）。F27 整体仍为 `doing`（阶段二：fsync 选项 + 备节点预热 + 租约式接管评估；阶段三：状态外置），下一轮继续 F27 阶段二。体内的 12:26 特别指令与待办登记 1–9 均已在先前轮次执行完毕、作废。
- 借鉴点（2026-11-24 查阅一手来源）：① Kubernetes 官方文档《Liveness, Readiness, and Startup Probes》（kubernetes.io/docs/concepts/workloads/pods/probes）：存活探针只判进程是否陷入不可恢复状态、失败则重启，**不得检查外部依赖**；就绪探针判「现在能否接流量」，失败只把 Pod 从 Service 端点摘掉、**不重启**，恢复后重新加回——我们照此分工，`/healthz` 保持原样作 liveness，新增 `/readyz` 作 readiness。② Litestream 官方《How it works》（litestream.io/how-it-works）：WAL 页异步流式复制、TXID 组织 LTX 文件、定期快照；明确说明异步复制的 RPO 边界（盘坏丢最后约 1 秒写入）与「single writer per replica path」——阶段三的状态外置走同一形态，RPO 的诚实表述也照此（异步即有窗口，不宣称零丢失）。
- 复用检查（Nathan 复用优先指示）：未复制代码——K8s 探针是编排语义、Litestream 是 SQLite 复制工具，两者只借设计思想；实现约 300 行纯标准库自研（`internal/ha` + 写健康可见性 + 接线），无新依赖。
- 实现：新包 `internal/ha`（`Mode` active/standby + `ParseMode`；`Acquire`：数据目录下 `agentgate.lock` 上 `flock(LOCK_EX)`，active 非阻塞、被占即 `ErrLocked` 快速失败，standby 先非阻塞试一次、被占则阻塞等锁，`waited` 标志供接管日志用；Windows 构建上锁为 no-op，HA 只支持 Linux）+ `ReadyHandler`（`GET /readyz`：就绪 200 `{"ready":true}`，未就绪 503 `{"ready":false,"problems":[...]}`，problems 只含类别码 `state_save_failed` / `audit_append_failed` / `audit_chain_invalid`，不含路径与原文）；`audit.Log.LastError()` 与 `store.Store.LastSaveError()`（此前 `audit.Append` 与各处 `store.Save()` 的错误都被静默吞掉，写失败时进程表面一切正常，本轮起记录最近一次写错误、成功清零，供 `/readyz` 读取）；`cmd/agentgate/main.go` 接线（`AGENTGATE_HA_MODE`，锁在 `store.New` 之前获取；启动时验链，standby 接管遇断链拒绝启动、active 只告警不退出，`/readyz` 同样报 `audit_chain_invalid`）；新增 `docs/ha-design.md`（方案评审全文：状态盘点、故障模型、四方案比较、接管流程、诚实边界、systemd/K8s 部署示例、切换演练）；README 文档索引同步；冒烟新增 1 项断言（`/readyz` 200）。
- 关键决策：① 锁的语义取 flock 而非 PID 文件——进程死亡（含 SIGKILL）时内核自动释放，无残留锁问题；代价是锁在 inode 上，运维勿手动删锁文件（删后重建会拿到另一个锁，双写防护失效），已写进文档与包注释。② 接管验链失败时 standby 拒绝启动（fail-closed，宁可无服务、不可把断链再延长），active 只告警（保持单节点既有行为，老部署不因升级而无法启动），`/readyz` 同时报未就绪、由编排器排空流量。③ 本轮不做 fsync——逐事件 fsync 会给每次调用加一次磁盘同步，是性能取舍；阶段二再做成可选项（`AGENTGATE_AUDIT_FSYNC`），RPO 的诚实边界已写进设计文档（硬掉电可能丢最后一次写入窗口，进程崩溃不丢）。④ 锁只防双写、不防「主活着但不健康」——主节点磁盘满/链断时锁仍在主手里、备不接管，只靠 `/readyz` 排空流量 + 运维介入；租约/心跳式接管需先解决误判夺锁的防护，留待阶段二评估。⑤ Windows 上锁为 no-op 而非报错——Windows 构建只跑单节点，报错会无故破坏现有 Windows 单节点部署；HA 部署只支持 Linux，已写进文档。
- 测试：ha 包 5 项（`ParseMode` 四类拒绝、问题类别三项组合、`ReadyHandler` 健康 200 / 写失败 503 且不泄漏原文 / 断链 503、锁竞争：第二 active 得 `ErrLocked`、释放后可重新获取、standby 阻塞等锁后接管且 `waited=true`、空闲锁立即获取）+ audit 1 项（目录作日志路径时追加失败可观测、健康时为 nil）+ store 1 项（删目录后保存失败可观测、重建后恢复且错误清零）；`gofmt`/`go vet`/`go test ./...` 全绿（33 包）；`go test -race`（ha/audit/store）全绿；三平台构建通过（linux/windows/darwin，Windows 构建验证了 build-tagged 锁文件）；`make smoke` 311 项断言 0 失败（基线 310 + 新增 1：`F27 readyz`）。
- 本轮实测（真机双进程演练，临时数据目录）：第二 active 节点快速失败并提示改用 standby；standby 进程存活但不监听；主节点 `kill -9` 后备节点约 4 秒内接管（日志 `ha: took over as active`）、`/readyz` 200、登录与调用正常；接管前后 `/api/audit/verify` 均为 `{"chain_valid":true}`、导出头 `X-Audit-Chain-Valid: true`，审计链跨接管不断叉。演练初版查链时用了错误端点（`/api/auth/login`）与错误用户名（`agentgate-admin` 是密码、用户名是 `admin`），登录拿不到 session、链验证报 unauthorized——已纠正并复验通过，记为教训：演练脚本里的端点与凭证先对着 `e2e/smoke.py` 与 `internal/seed/seed.go` 核对，不要凭印象写。
- 推送结果：本地提交 9e4a4e8（含随带的 bdd5ecf，即 F35 推送结果记录）；远端建分支 loop/f27（基点远端 main e197d454；推送前核对同步基点：7 个改动文件远端 blob 与本地父提交 bdd5ecf 一致，DEVLOG.md 一处差异已核对——本地父提交多一段 F35 推送结果记录，正是随本轮带上的内容；新文件 9 个远端确认不存在）；整包快照 16 文件一次推送（提交 204370ce），推送后 16 个文件远端 blob 与快照逐一核对一致；PR #33 文件清单与本轮一致（16/16）；未触发 contents API 回退；已合并进远端 main，远端 HEAD e12f8bf8（合并提交），读回 main 上 internal/ha/ha.go、docs/ha-design.md、docs/feature-backlog.md 三文件 blob 与快照一致。另记：① 首次推送时 paths 清单放在 /tmp 下、两次调用之间被系统清理导致 push-files 报错找不到文件，已改用 ~/workspace 下的持久快照目录与清单重推成功——记为教训：快照目录与清单一律放持久目录，不用 /tmp；② list_branches 调用一次因服务重启中断，重试后获批成功。待删分支新增 loop/f27（共 25 个，需 Nathan 网页端手动删除）。下一轮继续 F27 阶段二（fsync 选项 + 备节点预热 + 租约式接管评估）。

## 2026-11-24 · F35 交付体验改造（P1）

- 本轮按常规规程选条目：P0、P1 全部 done，P2 按队列顺序取 F35（首个剩余条目；F27 双机热备排最后，启动前先做方案评审）。体内的 12:26 特别指令与待办登记 1–9 均已在先前轮次执行完毕、作废。本地另有一条 docs 提交 29521fe（F34 推送结果记录）随本轮整包快照一并带上。队列现为 F27。
- 借鉴点：1Panel 预填表单 + 高级折叠（交付体验调研报告 §2.1）与 Purview 模板 + 向导 + 模拟模式（报告 §2.6）——三件都是「把到达功能的步数减到最少，不动治理语义」的同一结论；本轮未另查新的一手来源，复用 F34 轮次已查的同一批材料。
- 复用检查（Nathan 复用优先指示）：未复制代码——厂商预设值是各厂商公开文档里的端点事实（Base URL 与模型名），不是代码；`internal/channelpreset`（约 100 行数据 + 校验）与 DLP 词表 API（约 200 行）均为纯标准库自研，无新依赖。
- 实现：⑥ 导航渐进披露——控制台默认 8 页（开通向导 / 仪表盘 / 模型渠道 / 令牌 / 审计 / 用量统计 / 文件夹知识库 / 设备接入），其余 16 页收进导航底部「高级设置」折叠区（默认收起、`localStorage` 记展开状态、正在看高级页时自动展开；角色过滤先行，高级区无可见页时整个区不显示）；⑦ 渠道厂商预设库——新包 `internal/channelpreset`（9 厂商：DeepSeek / 通义千问 / 智谱 / OpenAI / Anthropic / Moonshot / 火山方舟 / Ollama 本地 / 自定义），`GET /api/channels/presets`（渠道管理权限），渠道页与向导第 3 步共用同一服务端列表（取代向导硬编码 JS 的 4 厂商；端点不可达时回退 4 厂商内置表），选厂商自动填 Base URL / 模型 / 信任类型 / 最高等级、只填 API Key；⑧ DLP 词表管理台页——`store.DLPWordLists`（托管词表 + 手动来源记录）+ `GET/POST /api/dlp/words`（`roles.Policy`）+ 管理台「DLP 词表」页（违禁词 / 企业敏感词两张表、来源标注 env/template/manual）；启动时托管表覆盖环境变量词表，模板在词表已托管后应用时模板词并进托管表；详见 docs/delivery-p1.md，README 文档索引同步。
- 关键决策：① 预设最高等级取保守默认——签约云厂商默认 internal（机密走脱敏后上云是 F17 的能力，不替管理员默认放宽），本地模型是唯一默认可到 secret 的类型；火山方舟模型填推理接入点 ID、Ollama 为本机回环地址，两点都在预设备注里写明。② 词表托管模型取「整表覆盖」而非「增量集合」：首次在本页改某张表时先以网关生效表初始化该表（环境变量词与模板词都保留），此后该表以本页为准、重启后不变——语义简单、可审计，代价是环境变量此后不再影响已托管的表（已写进文档）。③ 审计不记词本身：词可能是项目代号（敏感原文），`dlp_word_changed` 只记表名、动作与改后数量；冒烟与 API 测试都钉住了这一条。④ MCP 检测器仍只在启动时建成（F34 已知限制未变），词表改动需重启才对 MCP 生效，已写进文档与管理台提示。
- 测试：channelpreset 包 3 项（预设有效且唯一、信任与等级语义、非法预设拒绝）+ API 5 项（词表来源、增删往返与持久化与审计不含词本身、校验 400、模板并进托管表、预设端点与权限）+ `gofmt`/`go vet`/`go test ./...` 全绿；`go test -race`（api/store/channelpreset）全绿；`make smoke` 310 项断言 0 失败（基线 300 + 新增 10：预设列表与 Ollama 本地绝密、员工 403、词表来源、新增词即时拦截 BYOA、删除后恢复、控制台含 DLP 页与高级导航）。
- 本轮实测抓到的真问题：① 写冒烟断言时直接照抄了工具输出里被打码的 `token=<redacted>`（真实变量是 `alogin["session"]`），导致 smoke.py 语法错误；已逐行改回真实变量并用 `python3 -c compile` 校验——记为教训：从打码的输出里抄标识符前，先用字符拆分等方式确认真实值。② 写 API 测试时 `loginAs` 与 users_test.go 既有同名函数重名（F34 已记过同一教训），已删掉重复定义、复用既有函数。
- 推送结果：本地提交 39a1eb5（含随带的 29521fe，即 F34 推送结果记录）；远端建分支 loop/f35（基点远端 main 3dc8d215；推送前核对同步基点：8 个改动文件远端 blob 与本地父提交 29521fe 一致，DEVLOG.md 一处差异已核对——本地父提交多一段 F34 推送结果记录，正是随本轮带上的内容；新文件 6 个远端确认不存在）；整包快照 15 文件一次推送（提交 2dd0275d），推送后 15 个文件远端 blob 与快照逐一核对一致；PR #32 文件清单与本轮一致（15/15，可合并状态 clean）；未触发 contents API 回退；已合并进远端 main，远端 HEAD e197d454（合并提交），读回 main 上 internal/channelpreset/channelpreset.go、docs/feature-backlog.md 两文件 blob 与快照一致。另记：① 开 PR 调用一次因服务重启中断、经查无未关闭 PR 后重试，第二次获批成功；② 快照同步记录文件 CLI 未自动生成，已手动补记 `~/workspace/agentgate-f35-snap.muse-github-snapshot-sync.json`（提交 SHA + 15 路径）。待删分支新增 loop/f35（共 24 个，需 Nathan 网页端手动删除）。下一轮为 F27（双机热备，启动前先做方案评审）。

## 2026-11-24 · F34 交付体验改造（P0）+ README 重写

- 本轮按待办登记 9 执行：F34（P0）+ README 重写，随同一分支、同一 PR 提交。体内的 12:26 特别指令与待办登记 1–8 均已在先前轮次执行完毕、作废；本地另有一条 docs 提交 93fa58c（F33 推送结果记录）随本轮整包快照一并带上。队列现为 F35 → F27（F34 本轮做完，P1 拆为 F35 下一轮做；F27 双机热备排最后，启动前先做方案评审）。
- 借鉴点（2026-11-24 查阅一手来源）：dsh-ee 官方仓库 README（github.com/hikariming/dsh-ee，「One-command install」与「Docker Compose」节）：一键安装脚本自动生成 `.env`（主密钥 + 初始管理员密码）并在终端打印；Docker 直跑路径下未设主密钥时自动生成到数据目录并把**一次性设置码打印到日志**，用户打开网页输入设置码创建第一个管理员。另有 Purview 模板 + 向导 + 模拟模式（Microsoft Learn 一手，见交付体验调研报告 §2.6）与 1Panel 预填表单 + 高级折叠（见调研报告 §2.1）为同一结论的旁证。
  我们照此实现，但做四处改动：① 二选一中选了设置码（dsh-ee 的 Docker 路径），不选 install.sh 自动生成密码——设置码只在日志出现一次、用过即废、5 次输错作废，比打印在终端的长期密码更适合「装完即交接」的场景；主密钥仍要求启动前设置（未改）；② 提醒阶段只放宽自带级分级规则这一条——违禁词、语义复核、「无合规通道」仍拦截，放行的数据仍只走信任合规渠道且照常强制脱敏（Purview 的提醒是「显示策略提示、用户可继续」，我们的提醒是「网关放行并审计」，无弹窗交互面，故以审计事件为提醒载体）；③ 模板词表只作用于模型网关检测器，MCP 侧仍用启动时词表（MCP 的 detector 在 main.go 启动时一次建成、无运行时 setter，本轮不动它）；④ 报表节奏只是声明——F25 没有定时生成机制，预设把节奏记为「每周」，报表仍需手动生成。
- 复用检查（Nathan 复用优先指示）：未复制代码——dsh-ee 的设置码实现在其 web 服务层、与本项目单二进制网关形态不同，只借「首启生成一次性设置码 + 网页输码建首个管理员」的流程；新包 `internal/onboard`（模板、阶段、设置码）与 API/网关接线约 700 行纯标准库自研，无新依赖。
- 实现：新包 `internal/onboard`（`Phase` 三值 + `ParsePhase` + `DefaultPhase=observe`；`Conservative` 预设包——观察阶段、双等级脱敏、周报节奏、绝密例外关；两个内置模板 `general-conservative` / `medical-device`，后者含 6 个预置词；`NewSetupCode` 12 位无歧义字符设置码 + `SetupCodeHash` 只存 SHA-256）；`store.OnboardingState` 持久化（阶段、模板、预设应用标记、周报节奏、已应用词表、主密钥备份确认、向导完成时间）；`gateway.Policy.LaunchPhase` + 运行时 `SetLaunchPhase` / `SetRedactForTier` / `SetWords` / `Words`（互斥锁保护，`detector()` 每请求重建故词表即时生效；`Policy.DryRun` 保留为旧调用方的别名）；网关三处语义：观察 = DryRun（原有行为）、提醒 = 仅豁免 BYOA 分级拦截（放行后处置记 `warn` + `warn_reason=byoa_tier_would_block`，其余照常拦）、拦截 = 原有行为；`main.go` 启动时定阶段（新装观察、已有安装拦截、演示拦截）并重应用已持久化的模板词、非演示无用户时签发设置码并打印日志、sidecar 策略快照改读网关实时词表与阶段；API 新端点（`GET /api/setup/status`、`POST /api/setup`、`GET /api/onboarding/templates`、`POST /api/onboarding/apply-template`、`GET/POST /api/onboarding/launch-phase`、`GET /api/onboarding/checklist`、`POST /api/onboarding/ack`、`POST /api/onboarding/complete`），权限分别为 Policy（模板/阶段）与 AuditRead（清单）、ack/complete 仅 admin；管理台新增首次设置页、五步向导页（模板单选、厂商预设只填 Key、高级折叠、阶段三选一、自检清单 + 主密钥确认）与仪表盘「开通进度」卡（清单 + 阶段切换 + 本应拦截/已提醒计数），导航首位新增「开通向导」（admin/security 可见），中英双语；新增 docs/onboarding.md；README 按登记 9 固定结构重写（定位 → 适合谁/不适合谁 → 三道门 → 快速开始 → 双包交付 → 边界与弱点 → 文档索引 → 许可证），14069 字节压到 4075 字节（29%），两个死链已修正（弱点指向企业防护设计 §2、部署指向安装包分发页）。
- 关键决策：① 提醒阶段只豁免 BYOA 分级拦截——放宽面收敛到一条规则，违禁词、语义复核、无合规通道均不放宽；放行的数据仍只走信任合规渠道且照常脱敏；边界已写进 docs/onboarding.md（提醒是上线坡道、不是稳态），若 Nathan 要收紧，改动只在网关 BYOA 分支一处。② 设置码 5 次输错即作废（需重启签发新码），不设冷却——首启场景下重启成本低，且避免「冷却期可被无限拖延」的绕法。③ 模板词表不作用于 MCP 检测器（启动时建成、无运行时 setter），sidecar 快照则跟随网关实时词表与阶段；报表节奏只是声明，定时生成未实现（F35 不含，待后续轮次）。④ 演示模式固定拦截、且向导第 3 步的厂商预设只做表单预填、不连真实厂商（演示仍用 mock 渠道）。
- 测试：onboard 包 4 项（阶段校验、两模板内容、保守预设、设置码形状与哈希）+ 网关 5 项（默认拦截 / DryRun 别名映射 / 无效阶段回落；观察不拦禁词且审计带 `dry_run:` 前缀；提醒放行 BYOA 机密且处置为 `warn`、禁词仍拦、切回拦截后恢复 403；运行时双等级脱敏往返还原；运行时加词后新词定密级）+ API 5 项（设置码建首管全链路与二次 409、5 次输错后 410、未签发 503、模板应用与阶段往返及员工 403、自检清单逐项实测与 ack/complete）+ `gofmt`/`go vet`/`go test ./...` 全绿；`make smoke` 300 项断言 0 失败（基线 282 + 新增 18：设置状态、两模板、阶段默认拦截、清单实测、观察不拦禁词且本应拦截计数 +1、提醒放行 BYOA 机密且已提醒计数 +1、提醒仍拦禁词、医疗模板应用、切回拦截后新词定密级、主密钥确认、向导完成、控制台含向导页）。
- 本轮实测抓到的真问题：① 全量 `go test ./...` 首跑时 semreview 包 `TestReviewerDeepTimeoutConservative` 失败一次（1ms 超时上下文的时序抖动），单跑 5 次与全量重跑全绿——与本轮改动无关（未碰 semreview），记为已知偶发、不修。② 写 API 测试时 `callJSON` 与既有测试文件重名、`boolStr` 与 semreview.go 重名——已分别改名/复用，记为教训：新文件先 grep 包内同名符号。③ WebUI i18n 追加时丢了闭合 `}`，双逗号导致 `node --check` 报错——已修并用 node 校验通过，记为教训：单文件控制台改完必跑 `node --check`（从 HTML 抽出 `<script>` 校验）。④ 冒烟里 `custom_words_added` 的语义：API 计数的是「新登记进持久化记录的模板词」（医疗模板首应用为 6），其中 2 个（XH-200、星河 XH-200）演示种子里已有、网关实际新加 4 个；冒烟断言 == 6，文档已注明口径。
- 推送结果：本地提交 8dd0a24（含随带的 93fa58c，即 F33 推送结果记录）；远端建分支 loop/f34（基点远端 main 5d7b01ab；推送前核对同步基点：9 个改动文件远端 blob 与本地父提交 afbdc8f 一致——本地父提交与 afbdc8f 间仅 DEVLOG.md 一处差异，正是随本轮带上的 F33 推送结果记录；新文件 6 个远端确认不存在）；整包快照 15 文件一次推送（提交 dd399c1e），推送后 15 个文件远端 blob 与快照逐一核对一致；PR #31 文件清单与本轮一致（15/15）；未触发 contents API 回退；已合并进远端 main，远端 HEAD 3dc8d215（合并提交），读回 main 上 internal/onboard/onboard.go 的 blob 与快照一致。另记：① `list_branches` / `get_commit` / `pull_request_read` 在本环境不在只读 reviewed 集合内，须走 call-tool 审批通道（pull_request_read 另需 `method:"get"` 参数）；② 快照同步记录文件 CLI 未自动生成，已手动补记 `~/workspace/agentgate-f34-snap.muse-github-snapshot-sync.json`（提交 SHA + 15 路径）。待删分支新增 loop/f34（共 23 个，需 Nathan 网页端手动删除）。下一轮为 F35（导航渐进披露 + 渠道厂商预设库 + DLP 词表管理台页）。

## 2026-11-23 · F33 失败分类重试与尝试轨迹

- 本轮按常规规程选条目：P0、P1 全部 done，P2 按清单顺序取 F33（首个剩余 P2 条目）。体内的 12:26 特别指令与待办登记 1–8 均已在先前轮次执行完毕、作废。本地另有一条 docs 提交 61f7bc2（F32 推送结果记录）随本轮整包快照一并带上。队列现为 F34 → F35 → F27（Nathan 2026-11-23 拍板：F34 交付体验改造插队至 F33 之前，本轮只做 P0，P1 拆为 F35；另指示重写 README 随 F34 同轮同 PR 落地）。
- 借鉴点（2026-11-23 查阅一手来源）：Bifrost 官方文档《Retries & Fallbacks》（docs.getbifrost.ai/features/retries-and-fallbacks）：失败分三类——per-key failure（429 限流、凭据被拒、配额耗尽、密钥无模型权限、模型退役）、transient server failure（5xx/DNS/连接拒绝）、request failure（无密钥能服务）；per-key 失败轮换密钥，其中永久性 per-key 失败无退避立即轮换、且不计入 `max_retries` 预算，429 轮换时仍退避；transient 失败同一密钥指数退避 + 抖动重试；每个 fallback provider 获得一份全新完整重试预算；模型不可达类失败以原始状态码返回，全密钥因账号级原因被拒时返回 502 `upstream_credentials_exhausted`。
- 关键发现（立项细化）：登记 8 的 F33 条目原文只写了方向（"借鉴 Bifrost 的失败分类重试——密钥级失败立即轮换、瞬时错误同密钥重试；另含分层预算"），本轮细化时发现现有实现与目标有三处真实缺口：① 429/5xx 当时直接转移渠道、同一密钥从不重试（无退避、无预算）；② 403 当时原样返回调用方，从不轮换；③ 只有转移时才写一条 `channel_fallback` 事件，没有逐次尝试的轨迹（无密钥指纹、无耗时、无失败类别）。另注意登记 8 括号里的"只许同级或更低信任级别回退"——实测现有链已是首选渠道同一 MaxDataLevel 且信任合规，本轮不改链的构成，只在链内加分类重试。
- 复用检查（Nathan 复用优先指示）：未复制代码——Bifrost 为 Apache-2.0（与 AGPL-3.0 兼容），但其重试实现在 Bifrost 的 provider/transport 层、与本项目网关转发循环形态不同，只借分类与预算设计；实现为纯标准库自研（新文件 retry.go 约 130 行 + 转发循环改写），无新依赖。
- 实现：新文件 `internal/gateway/retry.go`（`failureClass` 四类 + `classifyStatus` + `maxTransientRetries`（默认 2，即每密钥 3 次尝试）+ `retryBackoff`（基数 100ms、翻倍、上限 2s、+50% 以内抖动）+ `keyMasked` + `auditAttempt`）；`gateway.go` 转发循环改写为"每渠道 × 每密钥 × 瞬时重试"三层：401 摘除并轮换（F12 不变）、403 轮换但不摘除（可能只是无模型权限）、429/5xx/网络错误同一密钥退避重试、其余 4xx 原样返回；`retryableStatus` 退役；熔断计数改为按失败调用（一次调用重试 3 次仍失败只记 1 次连续失败，成功照常清零）；重试过的调用在 `model_call` 事件 Detail 带 `attempts`；新增 docs/failure-retries.md，README 网关行同步；mock 增加 `[mock-fail-500-twice]` 测试钩子（前两次 500、之后 200）。
- 关键决策：① 429 按已批准的 F33 规格在**同一密钥**上退避重试（不轮换）——Bifrost 轮换是因为厂商常跨密钥共享账号级配额；本项目渠道按部署配置、密钥池本就是同一账号的，轮换烧掉的是同一配额；② 全密钥鉴权失败时返回最后一把密钥收到的上游鉴权响应本身（F12 惯例），不用 Bifrost 的 502 `upstream_credentials_exhausted`——调用方看到真实 401/403（如"该密钥无此模型权限"）；③ 403 不摘除密钥——401 证明密钥已死，403 只证明此次调用未被授权（无模型权限/地域/内容策略均可能），摘除会误伤可用密钥；④ 尝试轨迹每条尝试都写事件（含首次即成功的尝试）——规格原文"每次尝试的元数据轨迹写入审计"，事件为 Kind `channel`，不进用量统计、不触发告警；⑤ 退避不读上游 `Retry-After` 头（只指数退避），已记为已知注意点。
- 测试：内部单测 3 项（分类表 12 类状态、预算默认/配置、退避区间与 2s 上限、零配置回落 100ms）+ 网关集成 6 项（500 两次后同一密钥成功且不碰备用、429 同密钥重试、403 轮换不摘除且无 `channel_key_disabled` 事件、全密钥 403 时原样返回 403 且密钥保持启用、传输错误重试 3 次后转移备用、自定义预算 1 次重试时主渠道恰好命中 2 次）+ 既有 failover 测试 3 处期望随行为变更同步更新（转移前主渠道命中 1→3 次、冷却进入 3→9 次、冷却过期后命中 4→12 次、成功清零脚本按重试语义重写；测试本身未删）+ 基线修复测试 1 项（`TestTokensListOrderIsCreationOrder`，见下）；`gofmt`/`go vet`/`go test ./...` 全绿；`go test -race`（gateway/mockllm）全绿；`make smoke` 282 项断言 0 失败（基线 279 + 新增 3），修复后连跑 3 次全部 282/282。
- 本轮实测抓到的真问题：① 首跑 failover 测试 1 项失败——`TestChannelCooldownAfterConsecutiveFailures` 冷却过期后期望主渠道命中 10 次、实测 12 次：期望算错了（冷却过期后那次请求也会对主渠道做 3 次同密钥重试，9+3=12），不是实现问题，已修正期望；② 写测试时 `TestTransient500RetriesSameKeyThenSuccess` 里尝试序号断言曾写成 `string(rune('1'+i))`，可读性差，已改为 `strconv.Itoa(i+1)`。③ 冒烟 5 次跑了 3 次失败，全是同一项 `rpm limit enforced at 6th call`（F3 限流断言）——与本轮改动无关（F3 段在 F33 块之前、限流器只计网关入口请求、重试不产生新入口请求），但 F31 轮记的"60 秒窗口时序抖动"诊断是错的：真因是 `GET /api/tokens` 直接遍历 Go map、无排序，冒烟用 `next()` 挑第一个 `claude-code` token，而 F3 时已有 3 个同类 token（tok/tok29/tok30），挑中正确 token 的概率只有 1/3，挑错则 RPM=5 落到别的 token 上、冒烟 token 保持默认 20 RPM、三次调用全放行。已修基线：`listTokens` 按创建时间排序（ID 平局决胜），管理台 token 列表同样受益；新增 `TestTokensListOrderIsCreationOrder` 钉住；修复后冒烟连跑 3 次全部 0 失败。另记：冒烟第 2 跑的 1 失败未留存具体项，按第 4/5 跑的同一项判定为同一根因。
- 推送结果：本地提交 afbdc8f（含随带的 61f7bc2，即 F32 推送结果记录）；远端建分支 loop/f33（基点远端 main 5f97e4c2；推送前核对同步基点：9 个改动文件远端 blob 与本地父提交 b49056a 一致，DEVLOG.md 一处差异已核对——本地父提交多一段 F32 推送结果记录，正是随本轮带上的内容；新文件 4 个远端确认不存在）；整包快照 13 文件一次推送（提交 8acddd34），推送后 13 个文件远端 blob 与快照逐一核对一致；PR #30 文件清单与本轮一致（13/13）；未触发 contents API 回退；已合并进远端 main，远端 HEAD 5d7b01ab（合并提交），读回 main 上 internal/gateway/retry.go、docs/feature-backlog.md 两文件 blob 与快照一致。另记三点：① 推送调用两次因服务重启中断、经查分支仍在基点后重试，第三次转后台等审批约 36 分钟后获批成功；② 合并调用一次因运行重启中断（等待约 32 分钟）、经查 PR 仍 open、main 未动后重试，第二次转后台约 8 分钟后获批成功；③ 快照同步记录文件 CLI 未自动生成，已手动补记 `~/workspace/agentgate-f33-snap.muse-github-snapshot-sync.json`（提交 SHA + 13 路径）。待删分支新增 loop/f33（共 22 个，需 Nathan 网页端手动删除）。下一轮为 F34（交付体验改造 P0 + README 重写）。

## 2026-11-23 · F32 一致占位符脱敏

- 本轮按常规规程选条目：P0、P1 全部 done，P2 按清单顺序取 F32（首个剩余 P2 条目）。体内的 12:26 特别指令与待办登记 1–8 均已在先前轮次执行完毕、作废。本地另有一条 docs 提交（F31 推送结果记录）随本轮整包快照一并带上。
- 借鉴点（2026-11-23 查阅一手来源）：Kong AI PII Sanitizer 官方插件文档（developer.konghq.com/plugins/ai-sanitizer）：placeholder 模式用 `PLACEHOLDER{i}`，**identical original values receive the same placeholder**；`recover_redacted` 在响应中把原文恢复回来（仅对输入脱敏生效）；凭据按原长换成 `#`、自定义模式用 `CUSTOM{i}`；检测走独立的 AI PII Anonymizer Service（Docker + 每语言一个 spaCy 模型镜像，需 600MB 以上内存）。另有 2026-11-22 的五家网关调研报告（research_notes/ai-gateways-higress-apisix-kong-bifrost-20261122-1452/report.md）中 Kong 一节为同一结论的旁证。
- 关键发现（立项细化）：登记 8 称「现实现按类别计数编号，同一值重复出现会得到不同占位符」，实测不成立——F10 的 Redactor 已让完全相同的字符串在一次请求内共用占位符（网关 `TestRedactBodyToolCalls` 即证）。本轮的真实缺口是**同一逻辑值的不同写法**：`138-0000-0000` 与 `13800000000`、`Li@Xinghe.example` 与 `li@xinghe.example`、`XH 200` 与 `XH-200` 各得不同占位符，模型无法指代。F32 的实现即补这一层：同一性从字面扩展到逻辑值。
- 复用检查（Nathan 复用优先指示）：未复制代码——Kong 该功能为企业版闭源插件、其 spaCy NLP 容器形态与单二进制定位冲突，只借「同一值同一占位符」的设计；实现为 `guard.Redactor` 约 30 行改动 + 复用 F29 的 `normalize.Keywords` 作典范键，无新依赖。
- 实现：`guard.Redactor` 的复用键由原文精确值改为典范键（`canonicalKey`：PII/自定义词用 F29 关键词归一化——分隔符去掉、全角折半角、统一小写，凭据用原文精确值因密钥区分大小写，类别前缀防跨类碰撞，归一化为空时回落原文）；占位符与恢复原文在首次出现时一次定好（恢复填回首次出现的写法）；`MaxPlaceholderLength` 与 `Restore` 未动，流式回填缓冲行为不变；网关 `redactBody`、MCP 工具结果/提示词渲染、sidecar 请求代理三处共用同一套 Redactor，行为同步生效；新增 docs/consistent-placeholders.md，README 防护管线行同步。
- 关键决策：① 恢复取首次出现的写法——一个占位符只能对应一种写法，值相同、格式统一为首次出现的形式，这是为指代一致性付出的可见代价，已写入文档；② 凭据不参与归一化认同（大小写不同的两把 key 是不同的密钥）；③ 映射仍只存单次请求内存、不落盘、不跨请求复用（红线不变），多轮对话里同一值每轮重新编号。
- 测试：guard 包 2 项（`TestRedactorCanonicalIdentity`：电话/邮箱/银行卡/自定义词/身份证校验位大小写五组同值共用占位符、凭据大小写敏感不合并、编号按逻辑值计数；`TestRedactNormalizedRoundTrip` 期望随行为变更同步更新——同一电话两种写法现为 1 个映射条目、恢复为首次出现的写法，测试本身未删）+ 网关 1 项（content 用带横线手机号、tool_calls 参数用不带横线同一号：1 个映射条目、两处同为 `<PII_1>`、恢复为首次写法）+ MCP 1 项（联邦工具结果含 `XH 200` 与 `XH-200` 两种写法：BYOA 得 `<CUSTOM_1>` 两次、受管级得原文；假上游新增 `project_brief` 工具）+ sidecar 1 项（上游只见 `<PII_1>` 两次、下游恢复为首次写法两次）；`gofmt`/`go vet`/`go test ./...` 全绿；`go test -race`（guard/gateway/mcp/sidecar）全绿；`make smoke` 279 项断言 0 失败（基线 278 + 新增 1）。
- 本轮实测抓到的真问题：① 写测试时发现 F29 关键词视图会把纯标点分隔的两个数字连成一串——`138-0000-0000，13800000000` 中间只有逗号时，前一个变形值检测不到（占位符一致性只作用于已被检测到的值）；已记为 F32 已知注意点，检测本身的边界不在本轮范围内，不修。② 实现初版在 `Redact` 的替换循环里写 `used` 映射：多段文本（content 先、tool_calls 参数后）时后一段的写法会覆盖前一段，恢复取不到首次出现的写法；已改为占位符 minted 时一次定好恢复原文（网关测试当场捕获）。③ race 全量跑时 sidecar `TestPolicyCacheSurvivesRestart` 失败一次（状态已切 gateway 但缓存文件尚未落盘），与本轮改动无关（未碰 sidecar/terminal 的刷新路径）；随后单测 5 次、race 单测 3 次、race 全包 1 次全部通过，记为已知偶发时序抖动、不修。
- 推送结果：本地提交 b49056a（含随带的 d61343e，即 F31 推送结果记录）；远端建分支 loop/f32（基点远端 main 129b44a8；推送前核对同步基点：9 个改动文件远端 blob 与本地父提交 d61343e 一致，DEVLOG.md 一处差异已核对——本地父提交多一段 F31 推送结果记录，正是随本轮带上的内容；新文件 docs/consistent-placeholders.md 远端确认不存在）；整包快照 10 文件一次推送（提交 0d203037），推送后 10 个文件远端 blob 与快照逐一核对一致；PR #29 文件清单与本轮一致（10/10）；未触发 contents API 回退；已合并进远端 main，远端 HEAD 5f97e4c2（合并提交），读回 main 上 internal/guard/dlp.go、docs/feature-backlog.md 两文件 blob 与快照一致。另记两点：① 开 PR 的首次调用因服务重启中断、经查未建出 PR 后重试成功；② 快照同步记录文件 CLI 未自动生成，已手动补记 `~/workspace/agentgate-f32-snap.muse-github-snapshot-sync.json`（提交 SHA + 10 路径）。待删分支新增 loop/f32（共 21 个，需 Nathan 网页端手动删除）。下一轮为 P2 的 F33（失败分类重试）。

## 2026-11-23 · F31 MCP 列表/调用同规则过滤

- 本轮按常规规程选条目：P0、P1 全部 done，P2 按清单顺序取 F31（首个剩余 P2 条目）。体内的 12:26 特别指令与待办登记 1–8 均已在先前轮次执行完毕、作废。本地另有一条 docs 提交 fe9cd84（F30 推送结果记录）随本轮整包快照一并带上。
- 借鉴点（2026-11-23 查阅一手来源）：① Envoy AI Gateway 1.0 / Agent Router MCP Gateway 官方文档（envoy-ai-gateway.netlify.app/docs/1.2/capabilities/mcp）：多 MCP 服务器聚合到统一端点、按网关策略聚合与过滤可用工具（MCPRoute include/exclude 规则 + CEL 细粒度授权），工具名按后端加前缀路由。② Tetrate 1.0 发布说明（tetrate.io/blog/envoy-ai-gateway-v1-0-release）：「tools/list 只返回调用者实际有权用的工具」，1.0 起 MCP 网关含服务器复用、工具路由与过滤、细粒度授权。③ Nirmata《Govern MCP and LLM Traffic with Kyverno Authz》（nirmata.com，2026-11-18）：「列表过滤只是给 Agent 的便利、不是安全边界，真正拦住隐藏工具的是每次 tools/call 的检查」——与本条目红线「过滤只是纵深防御，tools/call 侧既有校验不得放松」同款。
- 复用检查（Nathan 复用优先指示）：未复制代码——Agent Router（Envoy AI Gateway）为 Apache-2.0，但其 MCP 过滤实现在 Envoy 控制面 / ext_proc 层、与本项目单二进制网关形态不同，只借「列表与调用同一套规则、列表过滤是纵深防御」的思想；实现为纯标准库自研约 60 行，复用本仓已有 `ToolMinTier`、联邦/REST 的 `MinTier` 与 `prompts/get` 的 unknown 形态。
- 实现：`internal/mcp/mcp.go` 新增三个共享静态谓词——`builtinToolVisible`（内置工具最低等级）、`upstreamVisible`（F6 联邦上游：启用 + 最低等级）、`restToolVisible`（F7 REST 工具：启用 + 最低等级），`listTools` 与三处调用入口共用同一谓词；此前唯一的列表/调用不一致是内置 `kb_get`（受管级工具，BYOA 的列表里可见、调用时才拒），本轮列表侧同样过滤；三处调用侧的等级拒绝由 -32003「tool not permitted for this agent tier」改为 -32602「unknown tool」，服务端 `tool_call_denied` 审计事件（reason=tier_insufficient）不变；`prompts/get` 的等级拒绝同步改为 -32602「unknown prompt」，与 prompts/list 的隐藏行为对齐；F22 知识包的列表/读取谓词未动（已共用，本轮只以测试钉住）。
- 关键决策：① 被隐藏的工具在调用时报「不存在」而非「无权」——错误码本身会泄漏工具存在性，-32602 与 prompts/get 对范围外提示词的「unknown prompt」同款；② 静态谓词共用、参数门禁（DLP 扫描、参数密级上限）留调用侧——列表时没有参数、无法判定，Envoy 也是请求授权与响应过滤分层；③ -32003 全仓退役——此前唯一引用就是这四处调用点与测试，改后全仓无 -32003。
- 测试：新增 `TestToolsListCallSameRule`（BYOA 列表无 kb_get、受管级列表有；三类隐藏工具调用均为 -32602 unknown tool；不变量——列表中任一工具以空参数调用永不报 unknown tool）与 `TestKnowledgePackageListGetAgreement`（列表中每个包都可读、列表外的一律不可读）；4 处既有期望随行为变更同步更新（kb_get / 联邦 / REST / 提示词的等级拒绝均改为 -32602，测试本身未删）；`gofmt`/`go vet`/`go test ./...` 全绿；`go test -race`（mcp）全绿；`make smoke` 278 项断言 0 失败（基线 276 + 新增 2）。
- 本轮实测抓到的真问题：① 冒烟首跑 1 项失败——`rpm limit enforced at 6th call`（F3 限流断言），与本轮改动无关（未碰 gateway/ratelimit）；随后重跑 4 次全部 0 失败，应为并行负载下的 60 秒滑动窗口时序抖动，记为已知偶发、不修。② 写测试时一次编辑误删了 `TestRESTToolCallArgsValidation` 的前几行，立即用 `git diff` 发现并恢复——教训：改测试文件后先 `git diff` 核对再跑测试。
- 推送结果：本地提交 45724f4（含随带的 fe9cd84，即 F30 推送结果记录）；远端建分支 loop/f31（基点远端 main 549bd216；推送前核对同步基点：7 个改动文件远端 blob 与本地父提交 a152949 一致）；整包快照 7 文件一次推送（提交 d4044316），PR #28 文件清单与本轮一致（7/7）；未触发 contents API 回退；已合并进远端 main，远端 HEAD 129b44a8（合并提交），读回 main 上 internal/mcp/mcp.go、docs/feature-backlog.md 两文件 blob 与快照一致。另记两点：① 推送后 get_file_contents / list_commits 读回 loop/f31 仍显示旧内容——疑似连接器读侧忽略 branch 参数或缓存滞后，PR 文件清单与合并后 main 读回证明分支实际已更新，以后轮次以 PR 清单 + main 读回为准；② 快照同步记录文件 CLI 未自动生成，已手动补记 `~/workspace/agentgate-f31-snap.muse-github-snapshot-sync.json`（提交 SHA + 7 路径）。待删分支新增 loop/f31（共 20 个，需 Nathan 网页端手动删除）。下一轮为 P2 的 F32（一致占位符脱敏）。

## 2026-11-23 · F30 复杂度智能路由

- 本轮按常规规程选条目：P0、P1 全部 done，P2 按清单顺序取 F30（首个剩余 P2 条目）。体内的 12:26 特别指令与待办登记 1–8 均已在先前轮次执行完毕、作废。本地另有一条 docs 提交 c3fc529（F28 推送结果记录）随本轮整包快照一并带上。
- 借鉴点（2026-11-23 查阅一手来源）：① 1Panel AI 网关官方文档（1panel.pro/docs/v2/user_manual/ai/gateway）：智能路由只在客户端请求 `model=auto` 时触发，其他模型名走正常转发；先配简单与复杂两个模型组（组内模型顺序即选路优先级，组内不做负载均衡），再在「设置 → Embedding」配向量服务、路由阈值与 TopK，在「智能路由 → 样本」维护简单 / 复杂请求样本并生成向量；请求转语义向量、与预置样本算相似度，样本无法明确命中时再通过规则兜底判断，最终分发到对应模型组；决策日志记录每次判定的简单 / 复杂占比。② Performance Hub Model Router 文档（docs.performancehub.co，旁证）：`auto` 同样是无定价的虚拟模型 ID，按提示复杂度在 Fast / Medium / Complex / Reasoning 四档中选模型、按解析出的模型计费，选路逻辑集中在网关侧。
- 复用检查（Nathan 复用优先指示）：未复制代码——1Panel 为闭源商业产品、Performance Hub 为商业服务，只借其智能路由形态；分类器约 400 行纯标准库自研（`internal/autorouter`），向量化复用 F28 的 `semreview.Vectorize`/`Cosine`（F28 DEVLOG 预留的复用点）再加一层字符 bigram；选路复用本仓已有 `pickCandidates`（F1/F17 的密级 + 信任 + 冷却过滤）、F28 的审计决策日志形态、F5 的告警口径（本轮无新告警事件）。
- 实现：新包 `internal/autorouter`（`Classifier`：样本匹配——F28 同款余弦相似度 + ≥3 共享 n-gram 门 + 包含分支（≥10 共享且覆盖较小一方 80%）、平局倾向复杂；规则兜底——≥2000 字 / ≥6 轮 / 带 tool_calls → 复杂，否则简单；`ValidateConfig`/`DefaultConfig`：默认启用、阈值 0.35、简单组 `mock-eco`、复杂组 `mock-pro`、12 条种子样本）；新存储 `internal/store/autoroute.go`（`AutoRouteSample` + `AutoRouteConfig`，`State.AutoRoute`，未配置时走内置默认值）；网关 `internal/gateway/autoroute.go`（`resolveAutoModel`：分类 → 组内按序找第一个有合规候选渠道的模型 → 该层无合规渠道回落另一层（仍在合规集合内）→ 两层都无时按组模型并集报显式错误；`rewriteModel` 改写请求体 model 字段；`AutoRouteStats` 运行时计数 6 项）；`ChatCompletions` 在分级与自带级拦截之后、渠道解析之前接入（解析后 `req.Model`/请求体/Trace 均为具体模型，`detail` 带 `auto_route` / `auto_route_confidence` / `auto_route_requested` / `auto_route_sample` / `auto_route_fallback`，例外凭证豁免逻辑与直接请求同款）；`GET /v1/models` 启用时列出虚拟模型 `auto`、停用时不列出；管理 API 四条（策略查 / 改、样本增 / 删，admin/security，`unresolved_models` 列出组里暂无渠道提供的模型）；管理台新增「智能路由」页（策略表单 + 计数 + 样本库增删，中英双语）；详见 docs/auto-routing.md。
- 关键决策：① 只做简单 / 复杂两档（1Panel 同款两档），不做四档——演示模型只有 eco/pro 两档，四档无模型可配；② 分类器在 F28 向量化器上加字符 bigram 层——短中文问句 trigram 太少（「今天星期几？」只有 3 个），插入一个字毁掉两个，bigram 对单字插入更稳健；F28 的向量化器本身未动，增强只在 autorouter 包内；③ 阈值保持 0.35（与 F28 同值）：实测分离为改写 0.39–0.56、正常提示最高 0.32（「本周的供应商报价单汇总」0.32），重度缩写「星河精密几点上班」0.34 落规则兜底、层级仍正确；④ 层级回落是可用性选择、不是降级：回落仍在合规集合内（每跳都复用 `pickCandidates`），且审计同时记录分类层级与实际服务层级；⑤ 组模型无渠道提供时不报错（可能是前向引用），只在 GET 响应与管理台提示——硬校验会挡住「先配组、后接渠道」的正常顺序；⑥ 停用时 `model=auto` 报 `model_not_available` 而不是静默落默认模型——`auto` 不是真实模型，静默回落会掩盖配置错误。
- 测试：autorouter 包 10 项 + 网关集成 10 项 + API 1 项；`gofmt`/`go vet`/`go test ./...` 全绿；`go test -race`（autorouter/gateway/api/store）全绿；`make smoke` 276 项断言 0 失败（基线 266 + 新增 10，三次运行均稳定）。
- 本轮实测抓到的真问题：① 阈值调参初版照抄 F28 的纯 trigram 向量，短问句改写（「今天是星期几？」相对种子样本仅 1 个共享 trigram）与长文本规则（测试用 100 次重复只有 1500 字、未达 2000 字阈值）双双落空——已加 bigram 层并把长文本测试改为 150 次重复（2250 字），两处均有回归用例；「星河精密几点上班」相对种子样本 0.34 仍低于阈值，已作为「缩写落规则兜底、层级仍正确」的用例写进测试与文档，不再调低阈值。② 层级回落集成测试初版未给自定义配置配样本库（`Samples` 为 nil 即空库），分类走了规则而非样本——已补 `autorouter.DefaultSamples()`，并确认空库即规则兜底是设计行为（配置语义：删空样本 ≠ 恢复种子）。③ 冒烟里 `token=<redacted>` 是工具输出的脱敏显示，真实变量是 `sess` 与 `tok30`（F29/F28 轮同款坑），已逐行核对真名并加 `python3 -c compile` 自检。④ 文档初版写了未经实测的相似度数字（「本周的供应商报价单汇总」0.26、合同改写 0.55），已用临时 probe 测试实测修正为 0.32 与 0.56，probe 用后删除。
- 推送结果：本地提交 a152949（含随带的 c3fc529，即 F28 推送结果记录）；远端建分支 loop/f30（基点远端 main 2cca9b68；推送前核对同步基点：README.md 与 internal/gateway/gateway.go 远端 blob 与本地父提交一致、8 个新文件在远端不存在（抽查 autorouter.go 确认）、DEVLOG.md 仅差本地独有的 F28 推送记录行；create_branch 首次调用被连接器服务重启中断、核查确认分支已建出后继续）；整包快照 17 文件一次推送（提交 8dbd5a5a），逐文件哈希核对 17/17 一致，未触发 contents API 回退；PR #27 文件清单与本轮一致（分支自 main HEAD 建出 + 仅一次快照提交 + 17/17 blob 核对证明，list_pull_files 在本环境报 unknown method，见 F20 轮记录），已合并进远端 main，远端 HEAD 549bd216（合并提交），读回 main 上 internal/autorouter/autorouter.go、docs/auto-routing.md 两文件 blob 与快照一致。待删分支新增 loop/f30（共 19 个，需 Nathan 网页端手动删除）。下一轮为 P2 的 F31（MCP 列表/调用同规则过滤）。

## 2026-11-23 · F28 语义复核层

- 本轮按常规规程选条目：P0、P1 全部 done，P2 按清单顺序取 F28（首个剩余 P2 条目）。体内的 12:26 特别指令与待办登记 1–8 均已在先前轮次执行完毕、作废。本地另有一条 docs 提交 583a41e（F29 推送结果记录）随本轮整包快照一并带上。
- 借鉴点（2026-11-23 查阅一手来源）：① 1Panel AI 网关（飞致云论坛 bbs.fit2cloud.com/t/topic/19664 与官方博客 v1.6.0，2026-11-06）：内容合规是「敏感词 + 语义审核 + 内容审计」三层；向量语义审核把请求转语义向量、与预置样本库算相似度，样本未命中时规则兜底，判定写入决策日志；v1.6.0 接入 Qwen3Guard 安全围栏模型（Safe / Controversial / Unsafe，「有争议」与「不安全」视为风险命中），三层审核可独立启停、组合使用；「阻断」模式下审核服务不可用时不放行未审核内容。② Higress + Qwen3Guard（阿里云博客）：Qwen3Guard-Gen 以 Wasm 插件接入网关，默认调用 `Qwen/Qwen3Guard-Gen-4B`，暴露 OpenAI 兼容 `POST /v1/chat/completions`，三档风险级别映射为两档网关策略（只拦 Unsafe / 连 Controversial 也拦），模型自托管部署。③ API7 文档：Qwen3Guard 为 Apache-2.0，Gen 家族 0.6B/4B/8B，回答为 `Safety: Unsafe` / `Categories:` 纯文本。
- 复用检查（Nathan 复用优先指示）：未复制代码——1Panel 为闭源商业产品、Qwen3Guard 只借其 OpenAI 兼容的调用与回答格式，深层客户端与样本匹配器约 630 行纯标准库自研（`internal/semreview`）；复用本仓已有 `internal/guard` 的脱敏、`store.Channel` 的渠道与密钥轮询、F17 的信任梯度校验、F5 的告警、F14 的审计事件。
- 实现：新包 `internal/semreview`（`Matcher`：字符 trigram + 词 unigram 向量化，余弦相似度 + 包含分支（长请求里原样嵌入的越狱仍命中），默认阈值 0.35；`DeepClient`：OpenAI 兼容端点调用，解析 `Safety:` / `Categories:`，Unsafe 拦截、Controversial 按策略处置、Safe 放行）；新存储 `internal/store/semreview.go`（`SemanticReviewSample` + `SemanticReviewConfig`，默认策略 enabled=true、阈值 0.35、超时 3000ms、争议=告警、深层关闭，未配置时返回内置默认值，种子样本 9 条：越狱 4 / 拆分外发 2 / 诱导 2 / 伪装 1，其中诱导类「委婉地问怎么绕过审批流程」为告警样本）；网关接入点在 `ChatCompletions` 脱敏之后、缓存之前（被拦的请求永不进缓存，例外凭证不豁免复核拦截、不消耗凭证，DryRun 只记事件不拦截）；复核文本是脱敏后的副本（自带级未脱敏与 DryRun 也先脱敏再送审）；深层调用前校验渠道存在 / 启用 / 未冷却 + `trustAllowed` + 密钥轮询，绝密跳过整个复核层；审计：拦截 `violation`/`model_call_blocked`（`semantic_review_blocked` + `semantic_source`/`semantic_sample`/`semantic_similarity`，不含请求原文）→ F5 关键告警，告警记 `semantic_review` 事件 → F5 警告告警，放行记 `model_call` 的 `detail.semantic_review`，DryRun 记 `semantic_review_dry_run`；运行时计数 8 项经 `GET /api/semantic-review` 返回；管理 API 四条（策略查 / 改、样本增 / 删，admin/security）；管理台新增「语义复核」页（策略表单 + 计数 + 样本库增删，中英双语）；详见 docs/semantic-review.md。
- 关键决策：① 默认层用确定性 n-gram 向量而非神经网络 embedding——单二进制零依赖定位装不下 embedding 模型，`Vectorize` 是可替换的函数，F30 复用同一向量化器，诚实边界写进 docs/semantic-review.md；② 阈值 0.35 来自实测调参（应命中改写 0.38–0.69、正常提示最高 0.28，重度改写 0.21–0.33 留给深层）；③ 深层只审输入不审输出（输出审核需在流式与非流式回程加缓冲，留待后续轮次）；④ 超时与深层故障按密级分流（机密以上拦截、低密级告警放行）而非一律不放行——低密级一律拦截会把可用性交给审核端点的可用性；⑤ 复核在缓存之前——被拦的请求永不进缓存；⑥ 例外凭证不豁免复核拦截——复核是安全层不是分级豁免，与违禁词同款。
- 测试：semreview 包 12 项 + 网关集成 9 项 + API 1 项；`gofmt`/`go vet`/`go test ./...` 全绿；`go test -race`（semreview/gateway/api/store/alert）全绿；`make smoke` 266 项断言 0 失败（基线 258 + 新增 8）。
- 本轮实测抓到的真问题：① 全量 `go test ./...` 首跑时 sidecar 包 `TestPolicyCacheSurvivesRestart` 失败（policy-cache.json 未写出），单跑 3 次 + 整包重跑全绿——与 F23 轮 DEVLOG 记的同款已知偶发抖动（1 秒心跳 ticker 测试在并行负载下的时序问题），非本轮改动所致（本轮未碰 sidecar 代码）；重跑全绿后继续。② 阈值调参初版用「重叠率」作包含分支，正常提示「本周的供应商报价单汇总」误报 0.500——已改为「≥10 个共享 trigram 且覆盖较小一方 80%」的双条件包含分支，误报消失。③ `make smoke` 首次在 Makefile 内报 `go: not found`——Makefile 用 `GO ?= go`，本机 Go 不在系统 PATH（在 `~/workspace/tools/gosdk/bin`），须 `GO=$HOME/workspace/tools/gosdk/bin/go make smoke`；已在 DEVLOG 记下，下一轮起直接用该形式。④ 冒烟 F28 段的 1 次拦截 + 3 次放行断言须放在 F2 的 stats 精确计数断言之后（与 F29 轮同款坑），并复用 F29 的新令牌 `tok29` 以免干扰 F3 的 RPM 计数；真实变量已逐行核对（`sess` / `tok29` / `alogin["session"]`）。
- 推送结果：本地提交 012278e（含随带的 583a41e，即 F29 推送结果记录）；远端建分支 loop/f28（基点远端 main f7fc1e25；推送前核对同步基点：9 个改动文件远端 blob 与本地父提交一致、DEVLOG.md 仅差本地独有的 F29 推送记录行、10 个新文件在远端不存在，抽查 2 个确认）；整包快照 20 文件一次推送（提交 26b9fc0a），逐文件哈希核对 20/20 一致，未触发 contents API 回退；另记：推送后发现快照同步记录文件未生成（CLI 未写出），已按规程从已确认的推送结果手动补记 `~/workspace/agentgate-f28-snap.muse-github-snapshot-sync.json`（提交 SHA + 20 路径）；PR #26 文件清单与本轮一致，已合并进远端 main，远端 HEAD 2cca9b68（合并提交），读回 main 上 internal/semreview/semreview.go、docs/feature-backlog.md、internal/api/semreview.go 三文件 blob 与快照一致。待删分支新增 loop/f28（共 18 个，需 Nathan 网页端手动删除）。下一轮为 P2 的 F30（复杂度智能路由）。

## 2026-11-23 · F29 DLP 归一化管线

- 本轮按常规规程选条目：P0、P1 全部 done，P2 按清单顺序取 F29（首个剩余 P2 条目）。体内的 12:26 特别指令与待办登记 1–8 均已在先前轮次执行完毕、作废。本地另有一条 docs 提交 d319bb0（F26 推送结果记录）随本轮整包快照一并带上。
- 借鉴 1Panel AI 网关的内容合规做法（飞致云论坛 bbs.fit2cloud.com/t/topic/19664 与官方博客，2026-11-23 查阅）：「动态敏感词拦截：支持自定义行业词库和黑白名单，并且通过归一化处理识别谐音、拆字、拼音等变形内容」；其向量语义审核与模型审核（Qwen3Guard）是 F28 的范围，本轮只取归一化管线。
- 复用检查（Nathan 复用优先指示）：未复制代码——1Panel 为闭源商业产品，只借其归一化管线的形态；归一化与回指映射约 200 行纯标准库自研（`internal/normalize`），检测复用本仓已有 `internal/guard`（Aho-Corasick + 正则）。
- 实现：新包 `internal/normalize`（`Normalize`：全角折叠 + 统一小写 + 零宽/不可见字符去除，分 `ForKeywords`（只留字母数字）与 `ForPatterns`（保留标点、去空白）两视图；`Normalized` 带 `Orig`/`Ends` 回指表，`MapRuneRange`/`MapByteRange` 把归一化偏移映射回原文字节偏移，区间端点落在多字节 rune 内时向外取整）；`guard.Detector` 增加归一化词表自动机（构造时同规则折叠词表），`Scan` 在原文直扫后追加归一化两遍扫描并合并（重叠丢弃、按位置排序）；顺带修一个旧缺陷：内置 `password_assignment` 正则的 `\b` 在 Go 里只认 ASCII 词边界，行首或中文标点后的 `密码：…` 永远匹配不上，改为只对 ASCII 候选词要求词边界；详见 docs/dlp-normalization.md。
- 关键决策：① 两个视图而非一个——词表要「只留字母数字」（任何插进来的分隔符都是绕过手段），正则要「保留标点」（API Key 要 `-`、邮箱要 `@` 与 `.`），一个视图无法同时满足；② 原文直扫一字不动、归一化只做并集合并，重叠即丢弃——这是「只增命中、不减命中」红线的机械保证，也是 `Redactor` 按偏移替换不损坏文本的前提；③ 归一化不改写任何落盘物：转发正文、审计记录、告警内容均保持原样，`Finding.Value` 恒为原文片段，脱敏替换整个原文片段（含插进来的分隔符），响应还原回原文。
- 测试：normalize 包 4 项 + guard 4 项 + 网关集成 4 项 + 冒烟新增 5 项断言；`gofmt`/`go vet`/`go test ./...` 全绿；`go test -race`（guard/normalize/gateway）全绿；`make smoke` 258 项断言 0 失败（基线 253 + 新增 5）。
- 本轮实测抓到的真问题：① 归一化初版的 `Ends` 表用了折叠后 rune 的字节长度（全角 3 字节折叠后是 1 字节），回指偏移错位——已改为按源文字节迭代取原始 rune 长度，`MapByteRange` 的多字节用例即为该 bug 的回归测试。② `password_assignment` 的 `\b密码` 缺陷是写归一化测试时发现的：全角密码赋值在模式视图里明明已折叠仍无命中，根因是 Go 的 `\b` 不认汉字为词字符——已修正则并补回归用例。③ 冒烟初版把 F29 断言放在 F2 的 stats 断言之前，`stats reconcile with audit` 是精确计数（3 调用 / 2 拦截），3 次新增拦截调用把它冲掉——已把 F29 块移到 stats 断言之后，并用新令牌做拦截断言以免干扰 F3 的 RPM 计数（与 F24/F25/F26 轮 DEVLOG 记的同款坑：冒烟里 `token=<redacted>` 是工具输出的脱敏显示，真实变量是 `sess` 与 `tok`/`tok2`，已逐行核对真名）。
- 推送结果：本地提交 4d1f678（含随带的 d319bb0，即 F26 推送结果记录）；远端建分支 loop/f29（基点远端 main 2203ec5b；推送前核对同步基点：5 个改动文件远端 blob 与本地父提交一致、DEVLOG.md 仅差本地独有的 F26 推送记录行、4 个新文件在远端不存在）；整包快照 10 文件一次推送（提交 ea9fcc84），逐文件哈希核对 10/10 一致，未触发 contents API 回退；PR #25 文件清单与本轮一致，已合并进远端 main，远端 HEAD f7fc1e25（合并提交），读回 main 上 internal/normalize/normalize.go、docs/feature-backlog.md 两文件 blob 与快照一致。待删分支新增 loop/f29（共 17 个，需 Nathan 网页端手动删除）。下一轮为 P2 的 F28（语义复核层）。

## 2026-11-22 · F26 分级管理员与职责分离

- 本轮先执行待办登记 7 与 8（Nathan 2026-11-22 批准，均在本轮进行中批准、执行后作废）：登记 7——F28 补 Embedding 样本匹配为默认层、模型复核为可选深层的双路线，新增 F29 DLP 归一化管线（排 F26 后、F28 前）与 F30 复杂度智能路由（排 F28 后、F27 前），1Panel 应用商店上架与诚实版选型对比页记为 backlog 渠道备忘（发布动作）；登记 8——新增 F31 MCP 列表/调用同规则过滤、F32 一致占位符脱敏、F33 失败分类重试（均排 F30 后、F27 前），F28 深层首选候选补 Qwen3Guard 式自托管小模型，F26 补管理操作 HMAC 签名备注。队列现为 F26 → F29 → F28 → F30 → F31 → F32 → F33 → F27。体内的 12:26 特别指令与待办登记 1–6 均已在先前轮次执行完毕、作废。
- 本轮按常规规程选条目：P0、P1 全部 done，P2 按清单顺序取 F26（首个剩余 P2 条目）。远端 main 在 F25 之后又前进了一步——Nathan（或另一路）合并了 PR #23（demo site + 宣传页 v2，main HEAD 98b0b32e）；本地另有一条 docs 提交 619f741（F25 推送结果记录）随本轮整包快照一并带上。
- 借鉴等保三级「三权分立」（GB/T 22239-2019；2026-11-22 查阅 opforge.srebro.cn 等保三权分立配置指南与 openGauss 访问控制模型文档）：系统管理员（配置权）、安全管理员（授权权）、审计管理员（审计权）三权分离、互相制约；审计员只读、系统管理员不改安全策略、安全管理员不碰业务数据；三员不得由同一人兼任、最小权限。IP-guard 分级管理员体系无公开 API 文档，形态以同类 DLP 产品说明与本次调研为准。
- 复用检查（Nathan 复用优先指示）：无可直接复制的代码——三权分立是制度模型、无参考实现；权限矩阵、用户管理、OIDC 组映射约 900 行纯标准库自研；角色判定复用本仓已有逻辑（`requireAdmin` 的 admin | security 语义原样保留，`auth.RevokeUserSessions` 复用于禁用吊销）。
- 实现：新包 `internal/roles`（五档角色常量 + 12 项权限矩阵 `Can` + 角色说明卡片）；`store.User` 增加 `RoleSource`（local/oidc）；`internal/api` 增加通用守卫 `requirePerm`，路由按矩阵重接（审计读/导出/报表生成归审计员，渠道/令牌/缓存/OTel 归运维员，归档审计与报表删除仍仅 admin/security，批例外仍仅 admin/security）；新端点 `GET /api/users`（列表，不返密码哈希）、`GET /api/users/roles`、`PATCH /api/users/{id}`（仅 admin：不能改自己角色、不能禁用自己、不能动最后一位启用的平台管理员；改角色即转本地管理；禁用即吊销全部会话；每次变更记 `user_updated` 审计）；OIDC：`groups` claim 解析 + `AGENTGATE_OIDC_ROLE_MAP` 组 → 角色映射（仅 auditor/operator/employee，admin/security 写进映射会在启动校验时拒绝；只作用于 IdP 管理的账号、每次登录按当前组同步、本地改过角色的不再覆盖；`auth` 层对映射返回值再挡一次）；管理台新增「用户与角色」页（角色分工表 + 用户列表 + 改角色/禁用，安全管理员只见列表）与导航按角色过滤；演示种子新增 `auditor` 与 `ops` 账号；详见 docs/roles.md，OIDC 映射见 docs/oidc-sso.md。
- 关键决策：① 五档而非等保三档——单列运维员（渠道/令牌日常运维），因为网关日常运维量最大、又不该碰审计；② 授角色权只收不放——连安全管理员也不能授角色（等保只要求三员不兼任，我们进一步要求授角色权只属于平台管理员，防自我提权）；③ `security` 保留与 `admin` 同等的策略权是此前 `requireAdmin` 行为的延续，本轮没有降级它；④ OIDC 映射不推翻 F9 红线——映射是管理员显式配置的组映射、不是 IdP 直授角色，未配置映射时 OIDC 登录永远不碰角色；⑤ 审计员生成报表算读侧工作（产出冻结的审计证据、不改策略），删除报表仍是管理动作；⑥ 导航过滤只是 UX，真正的凭据是 API 层的 403。
- 测试：roles 包 4 项 + API 用户管理 1 项 + HTTP 权限矩阵 1 项 + auth 映射 2 项 + oidc 解析 4 项；另更新 orgsync 旧测试的种子用户计数 3→5；`gofmt`/`go vet`/`go test ./...` 全绿；`go test -race`（roles/auth/oidc/api/store）全绿；`make smoke` 253 项断言 0 失败（基线 237 + 新增 16，两次运行均稳定）。
- 本轮实测抓到的真问题：① 冒烟初版照抄工具输出的脱敏显示、把 `token=<redacted>` 直接写进 F26 段，`python3 -c compile` 自检即报 SyntaxError——与 F24/F25 轮 DEVLOG 记的同款坑（真实变量是 `sess` 与 `admin_tok`，本轮新增 `aud_tok` 与 `ops_tok`）；已逐行改回真名并加编译自检。② 种子新增两名演示用户后，orgsync 旧测试的种子计数断言失败（3→5）——行为未变，只改计数。③ 角色重接路由后，`requireAdmin` 原样保留 admin | security 语义，`security` 的既有测试全部通过，未降级。
- 推送结果：本地提交 4c2c4f0（含随带的 619f741）；远端建分支 loop/f26（基点远端 main 98b0b32e，PR #23 只改 demo-site 两文件、与本轮 23 文件零重叠；推送前核对同步基点：14 个改动文件远端 blob 与本地父提交一致、DEVLOG.md 仅差本地独有的 F25 推送记录行、8 个新文件在远端不存在）；整包快照 23 文件一次推送（提交 1678f146），逐文件哈希核对 23/23 一致，未触发 contents API 回退；PR #24 文件清单与本轮一致，已合并进远端 main，远端 HEAD 2203ec5b（合并提交），读回 main 上 internal/roles/roles.go、docs/feature-backlog.md、internal/api/users.go 三文件 blob 与快照一致。另记：本轮 create_branch 与首次 push-files 调用均被 GitHub 连接器服务重启中断——前者中断后分支实际已建出（核查 loop/f26 HEAD 为 98b0b32e 后继续），后者中断后分支头未动（仍为 98b0b32e），重试成功；`list_commits` 读 loop/f26 仍返回旧基点（与 F24/F25 轮同款读工具滞后），以推送返回的提交 SHA 与逐文件 blob 核对为准。待删分支新增 loop/f26（共 16 个，需 Nathan 网页端手动删除）。下一轮为 P2 的 F29（DLP 归一化管线）。

## 2026-11-22 · F25 合规周期报表

- 本轮先执行待办登记 6（Nathan 2026-11-22 批准，仅执行一次）：把 F28 语义复核层写入 docs/feature-backlog.md（排 F26 之后、F27 之前），随本轮同一分支、同一 PR 提交；登记完成后本轮按常规规程选条目。体内的 12:26 特别指令与待办登记 1–5 均已在先前轮次执行完毕、作废。
- 本轮按常规规程选条目：P0、P1 全部 done，P2 按清单顺序取 F25（首个剩余 P2 条目）。远端 main 已含 F24（PR #20，HEAD c7390375），无在途分支需续传；本地另有一条 docs 提交 2e2fa1c（F24 推送结果记录）随本轮整包快照一并带上。
- 借鉴 Wazuh 合规报表形态（cenovacyber.com 的 Wazuh 审计证据文章与 Wazuh 官方文档综述，2026-11-22 查阅）：规则映射到框架（PCI DSS / HIPAA / GDPR / NIST 800-53 / TSC），按框架出仪表盘与报表供审计师取证；其文档明确说明映射只是证据导航、不是合规认证。社区另有定时任务出 JSON/HTML/CSV 三种格式报表的做法。
- 复用检查（Nathan 复用优先指示）：未复制 Wazuh 代码——Wazuh 是 GPLv2 的整套 SIEM/XDR（Python/Elasticsearch/Kibana），与本仓库的单二进制 + 审计日志架构不匹配；报表聚合与导出约 500 行纯标准库自研；用量口径复用 F2 的事件分类，与 /api/stats 同口径可对账。
- 实现：新包 `internal/report`（`Report` + `Generate`：窗口内审计事件聚合 + 生成时刻快照（渠道健康/令牌/终端概况/链完整性）；`PeriodWindow`：周报表 ISO 周一至下周一、月报表自然月（UTC）；`WriteCSV` 扁平表导出）；`store.State` 增加 `Reports`（最多保留 52 份，超了删最旧）；管理 API（`internal/api/reports.go`）：`POST /api/reports`（生成，period 必填 weekly|monthly、date 可选 YYYY-MM-DD 缺省今天 UTC）、`GET /api/reports`（列表摘要）、`GET /api/reports/{id}`（预览）、`GET /api/reports/{id}/export`（CSV 下载）、`DELETE /api/reports/{id}`（均仅 admin/security，生成/导出/删除记审计，只含报表 ID 与周期）；管理台新增「合规报表」页（生成表单 + 列表 + 预览 + 导出 CSV + 删除，中英双语）；详见 docs/compliance-reports.md。
- 关键决策：① 报表是冻结快照不是实时仪表盘——评审要的是某一周期盖了章的数字，生成后数字不再变化，事后归档也不改写已生成的报表；② 不做框架条款映射与「通过/不通过」结论——我们没有等保条款对照表，报表只给事实，是否合规由审计师判定，映射留待后续轮次评估；③ Top 风险事件按 (kind, action) 分组、原因是组内最常见的那一个——拦截按原因拆行会把主形态打散，而原因明细另有拦截原因分组；④ 令牌与终端概况取生成时刻的点状态，不是周期内的变化历史（与渠道健康同款快照语义）；⑤ 同一周期可重复生成、每次生成都是独立的一份，不覆盖旧报表，保留上限 52 份（一年周报量）。
- 测试：report 包 4 项 + API 端点 2 项（生成→列表→预览→导出 CSV→删除全链路、401/403、400 校验、未知 ID 404、保留 52 份上限）+ 冒烟新增 10 项断言；`gofmt`/`go vet`/`go test ./...` 全绿；`go test -race`（report/api/store）全绿；`make smoke` 237 项断言 0 失败（基线 227 + 新增 10）。
- 本轮实测抓到的真问题：冒烟初版照抄工具输出的脱敏显示、把 `token=<redacted>` 直接写进 F25 段，`make smoke` 报 SyntaxError——与 F24 轮 DEVLOG 记的同款坑（真实变量是 `sess` 与 `admin_tok`）；已逐行改回真名并加 `python3 -c compile` 自检。另记：冒烟里报表日期缺省走服务端今天（UTC），不传本地日期——本地日期（CST）与服务端 UTC 日期跨周时会让窗口错开整整一周，API 测试已覆盖 date 参数本身。
- 推送结果：本地提交 75aa7d4；本轮发现远端 main 在 F24 之后又前进了一步——Nathan（或另一路）合并了 PR #21 demo site assets（feat(demo)，2026-11-22 13:41 UTC），远端 main HEAD 变为 ac230003；该 PR 只新增 demo-site/ 下两个文件，与本轮 12 文件零重叠，同步基点核对 6 文件与本地父提交一致、DEVLOG 仅差本地独有的 F24 推送记录行、5 新文件在远端不存在，分支 loop/f25 按规程以远端 main 最新提交 ac230003 为基点新建。整包快照 12 文件一次推送（提交 95df1d66），推送后抽查 report.go / reports.go / compliance-reports.md 三文件 blob 与快照一致；推送提交文件清单与本轮清单一致（12 文件）。PR #22 合并进 main，远端 main 提交 9325cf46；读回 main 上 internal/report/report.go（blob SHA 与快照一致）验证无误。快照目录 ~/workspace/agentgate-f25-snap，同步记录存于其旁。另记：`list_commits` 读 loop/f25 分支头仍返回旧基点 ac230003（与 F24 轮同款读工具滞后），以推送返回的提交 SHA 与逐文件 blob 核对为准。待删分支新增 loop/f25（共 15 个，需 Nathan 网页端手动删除）。下一轮为 P2 的 F26（分级管理员与职责分离）。

## 2026-11-22 · F24 高密级调用例外审批流

- 本轮按常规规程选条目：P0、P1 全部 done，P2 按清单顺序取 F24（首个剩余 P2 条目）。体内的 12:26 特别指令与待办登记 1–5 均已在先前轮次执行完毕、作废。
- 借鉴域智盾外发文件审批流程说明（cnblogs.com/cksd/p/23705120，2026-11-22 查阅）：外发审批的标准闭环是「员工先申请并说明用途 → 管理员按文件类型/用途/接收范围审核 → 通过后对本次外发解密或放行、驳回则不予放行 → 全程留痕」；关键约束是审批通过只对应本次申请和本次外发（后续再次外发仍须重新申请），受控外发可设有效期与打开次数。IP-guard 本身无公开 API 文档，流程形态以同类 DLP 产品说明为准。
- 复用检查（Nathan 复用优先指示）：未复制代码——同类 DLP 产品的审批流无开源实现可直接复用（IP-guard/域智盾均为闭源商业产品），流程形态只借方法；约 600 行纯标准库自研（模型与校验、网关核验与消费、管理 API、管理台页）。
- 实现：新包 `internal/exception`（`ValidateRequest`：仅机密以上可申请、理由/范围必填、有效期 1–72 小时、绝密须 opt-in；`NewGrant`/`HashGrant`：随机凭证 + SHA-256；`FindValidGrant`：哈希 + 申请人 + 密级覆盖 + 未过期 + 未使用五项核验；`RefreshStatus`：过期 lazily 置 expired）；`store.Exception` + `State.Exceptions` 持久化；网关（`internal/gateway/exception.go` + `gateway.go` 三处拦截点）：违禁词拦截之后才查凭证（凭证永不豁免违禁词），豁免两处分级拦截——自带级禁发机密以上、与无合规通道（无本地模型时绝密可走专有云/VPC 一次），合规路由优先、仅在策略否则拦截时才动用凭证；消费点在选路完成即将转发时、存储锁内二次校验（并发复用只放行一次，败者 403 `exception_grant_invalid`），无效凭证在需要豁免时才 403，不需要豁免的调用不受影响、凭证也不消耗；放行仍按信任梯度脱敏（签约云强制脱敏），`model_call` 审计带 `exception_id`；管理 API（`internal/api/exceptions.go`）：申请/列表（管理员看全部、员工只看自己的）/详情/批准（返回凭证一次）/驳回/撤销（已批准未使用），审计只含元数据不含理由与范围原文；`alert.FromEvent` 新增 `exception_requested`/`exception_approved`（warning）；管理台新增「例外审批」页（申请表单 + 列表 + 批准/驳回/撤销，批准后凭证只显示一次，中英双语）；`AGENTGATE_ALLOW_TOPSECRET_EXCEPTIONS=1` 开启绝密例外（默认关闭）；详见 docs/exception-approval.md。
- 关键决策：① 凭证原文服务端永不存储、只存 SHA-256，批准响应里只返回一次——丢了只能重新申请，这是故意设计（凭证泄露后无法从服务端找回原文、只能作废重发）；② 消费点选在「确认转发」而非「查到凭证」或「收到上游响应」：既保证一次性不被并发复用，又保证不需要豁免的调用不烧凭证；转发后上游故障不退还（须重新申请），已写入文档；③ `scope` 是给审批人看的说明文字，网关不做机器匹配——强制绑定是申请人 + 密级 + 有效期 + 一次性，文档中明说；④ 审计 Detail 不含理由与范围原文（理由本身可能描述敏感工作）；⑤ 绝密例外默认关闭，须运维显式 opt-in，且已批的绝密凭证可撤销。
- 测试：exception 包 4 项 + API 端点 1 项 + 网关集成 5 项 + alert 分类 1 项 + 冒烟新增 9 项断言；`gofmt`/`go vet`/`go test ./...` 全绿；`go test -race`（exception/api/gateway/alert/store）全绿；`make smoke` 227 项断言 0 失败（基线 218 + 新增 9）。
- 推送结果：本地提交 c96033a；远端建分支 loop/f24（基点远端 main 5fe7d467）；整包快照 19 文件一次推送（提交 49328981），PR #20 文件清单与本轮清单一致（19 文件，新增文件行数与本地逐一吻合），合并进 main，远端 main 提交 c7390375（2026-11-22 20:50 前后核实）；读回 main 上 internal/gateway/exception.go（blob SHA 与快照一致）验证无误。快照目录 ~/workspace/agentgate-f24-snap，同步记录存于其旁。
- 本轮实测抓到的真问题：① 冒烟初版把全程共用的 BYOA 令牌 `tok` 直接用于 F24 凭证调用，整轮累计已超自带级 20 RPM，被限流 429 而非放行——凭证逻辑本身无错；已改为 F24 段内新 mint 一枚 BYOA 令牌（沿用 F23 的做法），并在冒烟注释中写明原因。② 推送后用 `get_file_contents` / `list_commits` 按分支 loop/f24 逐文件核对时，读回的仍是旧内容（11 个改动文件 SHA 全是推送前版本、8 个新文件查无），与 push-files 返回的成功提交矛盾；改以 PR #20 的文件清单核对（19 文件、新增文件行数与本地逐一吻合），确认推送已落地，随后合并成功、读回 main 验证一致。结论：这两个读工具对刚推送的分支不可靠（疑似忽略 branch 参数回落到 main），以后各轮以 PR 文件清单为准做推送核对，`get_file_contents` 只用于读 main。③ 另记：冒烟脚本里 `token=<redacted>` 是工具输出脱敏后的显示，真实变量是 `sess`（员工会话）与 `admin_tok`（管理员会话），写冒烟代码时须用真名——初版照抄脱敏显示导致 SyntaxError。
- 待办登记 6（MEMORY 2026-11-22 新增，Nathan 批准，本轮体未携带、下轮执行）：F28 语义复核层立项，排 F26 之后、F27 之前——规则 DLP 之上加可选语义复核层，复核通道本身走分级路由、绝密不送模型、能脱敏先脱敏、默认保守超时策略。下一轮仍为 P2 的 F25（合规周期报表）。

## 2026-11-22 · F23 终端持续盘点与断网策略缓存

- 本轮按常规规程选条目：体内的 12:26 特别指令（F17 + 蓝图修订稿）早已在 F17 轮执行完毕、执行后作废；待办登记 2–5 均已在 F16/F19 轮执行完毕、作废；P0、P1 全部 done，P2 按清单顺序取 F23（首个剩余 P2 条目）。远端 main 已含 F22（PR #18，HEAD 23db1b80），无在途分支需续传。
- 借鉴 Fleet / osquery 的终端可见性模式（Fleet 官方发布说明 fleetdm.com/releases/fleet-3-12-0，2026-11-22 查阅）：主机定期上报 vitals、服务端按最后上报时间推导新鲜度、可对单个主机点「Refetch」拿权威最新值；osquery 侧的落地形态是策略查询（`inventory-heartbeat-age-seconds.sql` 距上次心跳秒数、`beacon-version.sql` 已装版本）配 Labels（按 IdP 部门分组）做筛选。IP-guard 的离线策略缓存无公开 API 文档，模型沿用本仓 docs/integrations/ipguard.md 的配合结论。
- 复用检查（Nathan 复用优先指示）：未复制 Fleet/osquery 代码——Fleet 是 Go+MySQL+Redis 的整套设备管理平台（MIT），osquery 是 C++ 端点探针，均与本仓库单二进制 + 令牌认证架构不匹配；心跳、漂移评估、策略快照与离线缓冲约 900 行纯标准库自研；漂移评估复用本仓已有 `attest.Evaluate`（F14），词表检测复用 `internal/guard`（F15）。
- 实现：新包 `internal/terminal`（`Heartbeat` + 形状校验复用证明报告规则；`PolicySnapshot`：快照内容 SHA-256 作版本、词表排序后定版、篡改校验；`DriftReasons`：无策略不漂移、有策略走 `attest.Evaluate`、只返原因码；`Terminal` 记录 + `Status`：5 分钟 online / 60 分钟 offline）；`store.State` 增加 `Terminals`；管理 API（`internal/api/terminals.go`）：`POST /api/terminals/heartbeat`（令牌认证，Agent 类型与等级取自令牌、会话调用退回请求体；只审计注册/漂移出现/漂移消除/断网补报四类变迁，不记每次心跳）、`GET /api/terminals`（部门/Agent/版本/状态/漂移筛选）+ `GET /api/terminals/{id}`、`GET /api/sidecar/policy`（任意有效令牌可取）；`alert.FromEvent` 新增 `terminal_drift`（warning）；sidecar：`Config` 增加 `agent_type`/`plugins`/`heartbeat_interval_seconds`/`policy_cache_path`，`Server` 增加策略快照（local/gateway/cached）+ 检测器热换 + 心跳循环（借用代理请求里见到的令牌，只存内存）+ 断网决策缓冲；代理请求时最多每 5 分钟后台刷新一次策略（成功换检测器并落盘 0600，失败保持旧策略，篡改缓存忽略）；转发失败时按缓存策略执行：密级 ≤ offline_max_level（默认 internal）记 `offline_unreachable` 返回 502，之上记 `offline_blocked` 返回 503（fail-closed），dry-run 永不拦截；心跳成功时清空缓冲；`GET /status` 新增策略版本/来源/刷新时间、心跳状态与待补报数；`ag-guard` 增加 `-agent-type`/`-plugins`/`-heartbeat-seconds`/`-policy-cache`，`cmd/agentgate` 接线 `SetSidecarPolicy`（`AGENTGATE_OFFLINE_MAX_LEVEL` 可调上限）；管理台新增「终端清单」页（筛选 + 列表，漂移标红，中英双语）；详见 docs/terminal-inventory.md。
- 关键决策：① 心跳不进审计日志——每分钟一次的心跳会淹没审计，只审计注册与漂移/补报变迁；② 漂移评估复用 F14 的 `attest.Evaluate` 而不是另写一套判定，心跳即持续证明；③ 策略刷新不改变 sidecar 本地的 dry-run 标志（dry-run 是端侧操作员的上线选择，快照里的 dry-run 只作展示），避免一次刷新悄悄把观测模式翻成强制模式；④ 断网决策缓冲只在内存中（sidecar 在断网期间重启会丢失未补报条目，策略缓存落盘不受此限），是实现复杂度与可信度之间的取舍，已写入文档已知限制；⑤ 心跳认证借用代理请求里见到的令牌、不另存凭证，sidecar 自身不持有任何长期密钥。
- 测试：terminal 包 4 项 + API 端点 1 项 + alert 分类 1 项 + sidecar 5 项 + 冒烟新增 14 项断言；`gofmt`/`go vet`/`go test ./...` 全绿；`go test -race`（terminal/api/sidecar）全绿；`make smoke` 218 项断言 0 失败（基线 204 + 新增 14）。
- 本轮实测抓到的真问题：① 冒烟初版把员工 API 令牌（`tok`）当作会话去调 `GET /api/terminals`，期望 403 却拿到 401——requireAdmin 是会话守卫，API 令牌本来就进不去；已改用员工会话 `sess` 断言 403（与 API 测试的 empSess 口径一致）。② 心跳返回的 `drift_reasons` 在无漂移时是 JSON null 而非 `[]`，冒烟 `== []` 失败；已在服务端把 nil 强制为空数组（列表端点此前已做同样处理）。③ sidecar 策略源断言初版只认 `gateway`，但缓存文件（上一轮冒烟写下）未满 5 分钟刷新间隔时来源是 `cached`——行为本身正确（5 分钟内不重复刷新）；已改为断言策略版本与服务端快照一致且来源为 gateway/cached 之一。另记：冒烟脚本的中文部门名进 URL 查询串须 `urllib.parse.quote` 编码（F19 轮 doc_id 同类坑）。④ 提交前全量 `go test ./...` 时 sidecar 包偶发一次失败，单跑与重跑全绿（未抓到具体用例，疑似 1 秒心跳 ticker 测试在并行负载下的时序抖动）；已在两次全量重跑中确认通过，未改代码。
- 推送结果：本地提交 f4e6d68；远端建分支 loop/f23（基点远端 main 23db1b80）；整包快照 20 文件一次推送（提交 6b67b86b），逐文件哈希核对 20/20 一致；PR #19 文件清单与本轮清单一致，合并进 main，远端 main 提交 5fe7d467（2026-11-22 20:13 前后核实）；读回 main 上 internal/terminal/terminal.go 与 docs/DEVLOG.md 验证无误。快照目录 ~/workspace/agentgate-f23-snap，同步记录存于其旁。

## 2026-11-22 · F22 专题知识包

- 本轮按常规规程选条目：体内的 12:26 特别指令（F17 + 蓝图修订稿）早已在 F17 轮执行完毕、执行后作废；待办登记 2–5 均已在 F16/F19 轮执行完毕、作废；P0、P1 全部 done，P2 按清单顺序取 F22（首个剩余 P2 条目）。远端 main 已含 F21（PR #17，HEAD b0b85e2f），无在途分支需续传。
- 借鉴 Liner（linersh，liner.sh，MIT，与 AGPL-3.0 兼容）的调研包形态与策展流程（npm 包页与 liner.sh 变更日志，2026-11-22 查阅）：tape.yaml 是来源配方，Mixtape 是编译产物（synthesis.md + sources/ + MIXTAPE.md），Agent 按 `liner research` 逐步建包、在综合 / 来源清单 / 说明三处停下等人工批准，`liner catalog` / `liner search` 跨库检索，导航器可从引用跳到编译后的本地来源。
- 复用检查（Nathan 复用优先指示）：未复制 Liner 代码——其核心是 CLI + Go TUI，与本仓库的服务端架构不匹配；包模型、真相校验、API 与 MCP 约 1300 行纯标准库自研；分级与版本语义复用本仓已有逻辑（F8 的版本 / 发布流程、F18 的文件夹索引树、F19 的「回指由树填入」）。
- 实现：`store.KnowledgePackage` + `PackageVersion`（配方 sources + 引用 references + 综合 content + 回指 citations + 变更说明 change_note + 创建时算好的 level）+ `ValidatePackageVersion`（形状门）；新包 `internal/knowpack`（真相门：`ResolveVersion` 对照实时索引校验来源存在、引用已发布且向下、回指由树填入并算密级；`CheckReferences`/`CheckSources` 供管理台 `stale` 提示与 MCP 读取时扣留；`PublishedSynthesis` 供 MCP 内联被引用综合）；管理 API（`internal/api/knowpack.go`）：列表/创建/详情/加版/审核/发布/删除，发布前对照实时索引重验配方（失效 409）；MCP 新工具 `kb_pack_list` / `kb_pack_get`（仅已发布 + 本等级可见 + 部门范围内，引用综合内联、失效引用按名扣留）；管理台新增「专题知识包」页（新建/列表/审核/发布-回滚/删除/加版，含配方过期提示，中英双语）；演示种子 `onboarding_essentials`（新员工入职必读，internal，已发布 v1，来源为考勤与请假制度.md 全文，综合概括请假/加班/考勤三节、回指 n1/n2/n3）；详见 docs/knowledge-packages.md。
- 关键决策：① 引用只能向下——被引用包的密级须 ≤ 本包来源的最高密级（不是取 max 吸收：低密级包不得嵌入高密级内容是禁令，不是升级理由；想引用更高密级的包，先给本包加一个同等密级的来源，包的密级随之升高再引用）；② 回指由树填入、不接受手写回指，不存在的章节在创建时即被拒绝；③ 起草开放给任何登录用户（Agent 起草 → 管理员审批），审核 / 发布 / 删除仅 admin/security；④ 驳回已发布包即撤回——`Published()` 要求 status 为 published，只看 PublishedVersion 会把已撤回的包继续当作已发布；⑤ 审计只记包名/版本/密级，不记综合正文（综合本身可能概括机密文档内容）；⑥ 首次发布经审核与经发布端点同款重验配方，过期配方不能悄悄成为生产版本。
- 测试：knowpack 包 5 项 + API 端点 1 项 + MCP 集成 1 项 + 冒烟新增 14 项断言；`gofmt`/`go vet`/`go test ./...` 全绿；`go test -race`（knowpack/api/mcp/store）全绿；`make smoke` 204 项断言 0 失败（基线 190 + 新增 14）。
- 本轮实测抓到的真问题：① 审核-批准初版未重验配方：驳回已发布包只改 status、不清 PublishedVersion，`Published()` 初版只看版本号、被撤回的包仍算已发布，导致「撤回引用后发布」测试返回 200 而非 409。已修：`Published()` 要求 status == published；审核首次发布与发布端点同款重验配方（新增 API 回归用例）。② 冒烟断言的子串与种子综合文本不一致（断言写「请假须提前提交申请」，种子实际写「请假须提前在系统提交申请」），已按种子文本修正断言。③ API 测试初版把员工起草当 403——设计本就是任何员工可起草（Agent 起草 → 管理员审批），已按设计修正断言（员工可创建自己的草稿与加版，不可动他人包、不可审核/发布/删除）。
- 推送结果：整包快照推送一次成功（push-files，16 个文件一次提交 0a2ebeda 到 loop/f22，16/16 blob 哈希核对一致；推送前核对同步基点：8 个改动文件远端 blob 与本地父提交一致，DEVLOG.md 仅差本地独有的 F21 推送记录行，7 个新文件在远端 main 不存在）；PR #18 合并进远端 main（合并提交 23db1b80）；远端 main 上 `internal/knowpack/knowpack.go` 哈希与本地快照一致。另记：首次 `create_branch` 调用被连接器服务重启中断、核查确认分支未建出后重试成功；`list_branches` 不在连接器只读集内，须走 `call-tool`。待删分支新增 loop/f22（共 12 个，需 Nathan 网页端手动删除）。

## 2026-11-22 · F21 组织架构同步与 IM 身份绑定

- 本轮按常规规程选条目：体内的 12:26 特别指令（F17 + 蓝图修订稿）早已在 F17 轮执行完毕、执行后作废；待办登记 2–5 均已在 F16/F19 轮执行完毕、作废；P0、P1 全部 done，P2 按清单顺序取 F21（首个剩余 P2 条目）。远端 main 已含 F20（PR #16，HEAD 036b3055），无在途分支需续传。
- 借鉴 dsh-ee（hikariming/dsh-ee，Apache-2.0，与 AGPL-3.0 兼容）的钉钉 / 飞书组织同步：钉钉 `server/internal/dingtalk/client.go` 与 `directory.go`（应用 token 缓存、`{errcode,errmsg,result}` 信封、根部门广度优先 + cursor 分页、整份目录才生效）与 `server/internal/api/dingtalk_sync.go`（外部主体 `dingtalk:<corpId>:<userId>`、邮箱自动关联、AutoCreate 门控、本地已禁用不恢复、离职禁用前逐个确认、关联身份只解绑、汇总审计、最近同步结果持久化），飞书 `server/internal/feishu/client.go`（tenant token、contact v3 parentchildren / find_by_department、单查错误码 41012/41050）；企业微信是 dsh-ee 没有的平台，按官方通讯录 API 形状（`gettoken` / `department/list` / `user/list?fetch_child=1` / `user/get`，错误码 60111）新写。本轮通过 GitHub 连接器直接读到 dsh-ee 源码（search_code + get_file_contents），不再是只读 PR 标题。
- 复用检查（Nathan 复用优先指示）：未逐字复制 dsh-ee 代码——其客户端绑定自家 store/OIDC 类型与 TiDB，端点形状、信封处理、分页行走、离职确认与同步规则按上述借鉴点改写为本仓库的 store/secretbox 类型；三客户端 + 同步器约 1400 行纯标准库自研；来源项目、文件与许可证记于 `internal/orgsync` 包注释、backlog F21 条目与此条。
- 实现：新包 `internal/orgsync`（`Config` + 三客户端 dingtalk/feishu/wecom：token 缓存、信封校验、分页行走、UserState 三态确认；`Syncer`：建号 / 邮箱自动关联 / 刷新 / 离职确认与禁用 / 解绑关联身份 / 同步快照供绑定校验）；`store.User` 增加 `Email` 与 `ExternalIdentities`（`{provider, external_id, subject}`），`store.State` 增加 `OrgProviders`（`SecretEnc json:"-"` + 打码显示，`LastSync` 记最近一次结果）；`auth.RevokeUserSessions`（离职禁用时吊销全部会话）；管理 API（`internal/api/orgsync.go`）：来源 CRUD（仅 admin，密钥加密存储不回显）、`POST .../sync`（返回结果、写汇总审计）、`POST .../test`（只计数、不建号、不写审计）、绑定 / 解绑 / 本人绑定列表（绑定须快照校验、同一身份不许双绑，`user_id` 可填用户名或 ID）；管理台新增「组织同步」页（来源添加 / 列表 / 同步 / 测试 / 删除 + 绑定表单 + 我的身份，中英双语）；详见 docs/org-sync.md。
- 关键决策：① 审计汇总只含计数，用户名列表只在同步结果（API 返回与来源记录的 `last_sync`）里、不进审计（dsh-ee 的审计含被禁用户名）；② 本轮只做手动同步（管理台按钮 / API），不做每小时定时任务（dsh-ee 为每小时一次），留待后续轮次；③ 绑定须以同步快照为准——快照里没有的第三方 ID 拒绝绑定，同一外部 ID 在多个来源出现时按来源 ID 顺序选尚未被他人绑定的主体；④ 邮箱自动关联不分 AutoCreate 开关——账号已存在时关联不是建号，dsh-ee 的跨来源关联也是这个语义；⑤ 飞书用 `open_id` 作外部 ID（每应用唯一，换应用需重新同步），企业微信用根部门 `fetch_child=1` 一次拉全量。
- 测试：orgsync 包 13 项 + API 端点 5 项 + 冒烟新增 12 项断言；`gofmt`/`go vet`/`go test ./...` 全绿；`go test -race`（orgsync/api/auth/store）全绿；`make smoke` 190 项断言 0 失败（基线 178 + 新增 12）。
- 本轮实测抓到的真问题：① 绑定初版按平台类型取第一个快照里有该外部 ID 的来源——两个同类型来源（两家企业）都有 u1 时，map 随机顺序会命中已被他人绑定的主体而误报 409。已改为收集全部候选主体（按来源 ID 排序）、跳过已被他人绑定的、选空闲的；全部被占时才 409；回归测试覆盖。② 同一原因的另一面：同步的邮箱自动关联会在第二个来源同步时就把同一人绑过去（dsh-ee 的跨来源关联本就是这个语义），冒烟与回归测试因此改用无邮箱的 u3 覆盖「选空闲主体」分支。③ 解绑初版只认用户 ID、管理台传用户名时 404，已与绑定端统一为用户名或 ID 都接受。
- 推送结果：整包快照推送一次成功（push-files，18 个文件一次提交 22f73485 到 loop/f21，18/18 blob 哈希核对一致），PR #17 合并进远端 main（合并提交 b0b85e2f）；远端 main 上 `internal/orgsync/orgsync.go` 与 `docs/org-sync.md` 哈希与本地快照一致。

## 2026-11-22 · F20 受管 Agent 安装包分发与离线导入

- 本轮按常规规程选条目：体内的 12:26 特别指令（F17 + 蓝图修订稿）早已在 F17 轮执行完毕、执行后作废；待办登记 2–5 均已在 F16/F19 轮执行完毕、作废；P0、P1 全部 done，P2 按清单顺序取 F20（首个剩余 P2 条目）。远端 main 已含 F19（PR #15，HEAD 97de9e2a），无在途分支需续传。
- 借鉴 dsh-ee PR #309「feat: DSH 分发支持导入内网离线安装包」（hikariming/dsh-ee，Apache-2.0，与 AGPL-3.0 兼容）：离线导入、平台托管、员工下载、SHA-256 校验并区分已校验 / 未校验、导入中断可恢复、过期包清理。本轮查阅：PR 页标题已取到，api.github.com 取 PR 正文返回 403，设计点沿用 2026-11-22 当轮的调研结论（已记入 backlog F20 条目）。
- 复用检查（Nathan 复用优先指示）：未复制 dsh-ee 代码——其实现与 DSH 插件绑定较深，与本项目架构不匹配；分发与断点续传约 600 行纯标准库自研（archive/zip、crypto/sha256 均为标准库）；校验与分级复用本仓已有逻辑（F14 证明评估、F5 审计事件口径、sidecar 的 guard.json 字段）。
- 实现：新包 `internal/distrib`（包与上传会话记录 + Registry 文件管理：单次上传、断点续传、分片 offset 校验、完成时验算 SHA-256、过期筛选、用户包 zip 生成）；`store.State` 增加 `Packages`/`PackageUploads`；`attest.Policy` 增加 `AllowedBinaryHashes` + `Evaluate` 校验（原因码 `binary_hash_missing`/`binary_hash_not_allowed`，空列表保持旧行为）；管理 API `POST /api/packages`（multipart 单次上传）、断点续传五端点（建会话/分片/状态/完成/中止）、`GET /api/packages/{id}/download`（任意登录用户）、`DELETE /api/packages/{id}`、`POST /api/packages/{id}/baseline`（仅已校验包，幂等）、`POST /api/packages/cleanup`、`POST /api/packages/user-bundle`（两组件 + 策略，可一步登记基准值）；`make bundle-user`（`deploy/make-user-bundle.py`，离线组装同布局 zip）；管理台新增「安装包分发」页（上传/生成用户包/列表/下载/登记/删除/清理，中英双语）；详见 docs/package-distribution.md。
- 关键决策：① 未校验包永远不可登记基准值——「未校验」是门禁而不只是展示，避免管理员误把未验文件当作信任锚；② 断点续传的会话与分片都落盘（会话进 state.json，分片进 `<data>/packages/.uploads`），服务端重启后仍可从已收字节数续传；③ 审计事件不记校验和原文（只记包名/类型/状态），与 F14「原因码只记类别、不记哈希值」同款口径，校验和只出现在 API 返回与下载响应头里（那是员工核对下载用的公开值）；④ 生成用户包的 guard.json 是 sidecar 配置的超集（网关地址 + 策略字段），ag-guard 只读 `gateway_url`，其余字段由 Go JSON 解析忽略；⑤ 双包结构落地：管理包仍走现有 `make bundle` 离线通道（本轮只把它登记进同一注册表、统一查看），用户包新增 `make bundle-user` 与管理台生成两条路。
- 测试：distrib 包 7 项 + attest 1 项 + API 端点 4 项 + 冒烟新增 16 项断言；`gofmt`/`go vet`/`go test ./...` 全绿；`go test -race`（distrib/attest/api/store）全绿；`make smoke` 178 项断言 0 失败（基线 162 + 新增 16）。
- 本轮实测抓到的真问题：① Go ServeMux 拒绝 `GET /api/packages/{id}/download` 与 `GET /api/packages/uploads/{id}` 并存（`/api/packages/uploads/download` 同时匹配两者）——上传状态查询改为 `GET /api/packages/uploads/{id}/status` 子路径；② 冒烟下载断言取 `X-Package-SHA256` 响应头失败——Go 发出的头是规范大小写 `X-Package-Sha256`，Python 字典查找区分大小写，已改为小写化后再取；③ 同段冒烟的匿名下载调用把三元组 `get_bytes` 的返回值只解了两个，已补齐。
- 推送结果：整包快照推送一次成功（push-files，18 个文件一次提交 22572bc8，未触发 contents API 回退；推送前核对同步基点：11 个改动文件远端 blob 与本地父提交一致，DEVLOG.md 仅差本地独有的 F19 推送记录行，6 个新文件在远端 main 不存在；推送后逐文件哈希核对 18/18 一致；核对时发现 get_file_contents 的 `branch` 参数会被忽略、须用 `ref` 指定分支——前两次核对因此全数不符，改用 `ref` 后通过；另记：`pull_request_read` 的 `list_pull_files`/`get_pull` 方法在本环境均报 unknown method，PR 文件清单改由「分支自 main HEAD 建出 + 仅一次快照提交 + 18/18 blob 核对」证明）；PR #16 文件清单与本轮一致，已合并进远端 main，远端 HEAD 036b3055（合并提交），读回验证 distrib.go / package-distribution.md / packages.go 已在 main 上（blob 与本地快照一致）。待删分支新增 loop/f20（共 10 个，需 Nathan 网页端手动删除）。

## 2026-11-22 · F19 知识库语义索引层

- 本轮按常规规程选条目：体内的 12:26 特别指令（F17 + 蓝图修订稿）早已在 F17 轮执行完毕、执行后作废；F19 登记随 F14 轮进 backlog；P0、P1 全部 done，P2 按清单顺序取 F19（首个剩余 P2 条目）。远端 main 已含 F16（PR #14，HEAD 9b272691），无在途分支需续传。
- 另记登记（Nathan 2026-11-22 批准，仅执行一次、随本轮提交进 backlog）：F23 终端持续盘点与断网策略缓存、F24 高密级调用例外审批流、F25 合规周期报表、F26 分级管理员与职责分离、F27 双机热备与高可用（P2，F27 远期、排最后）。五条目状态均为 todo，验收待立项细化。至此待办登记 2–5 全部执行完毕、作废。
- 借鉴 AOCI-CODE（aoci-spec/aoci-code）的方法：每对象一行 FRAS 语义条目（职责 / 关联 / 契约 / 约束）+ 标签坐标（部门 / 类型 / 重要度）+ 索引治理（校验 / 漂移 / 覆盖率）。只借方法与思想，不复制代码——AOCI-CODE 许可证为 FSL-1.1-MIT（Fair Source，非 OSI 开源、有竞品限制），与本项目 AGPL-3.0 不兼容；实现全部自研，来源与许可证不兼容原因记于包注释、backlog 条目与此条。另按 2026-11-22 约束补充：语义条目必须带原文回指（文件 + 章节 + 页码），借鉴 Liner 的引用可回指原则。
- 复用检查（Nathan 复用优先指示）：无可直接复制的代码——AOCI-CODE 是 Go 本地 MCP 但许可证不兼容；FRAS 生成与索引治理约 600 行纯标准库自研；检测与分级复用本仓已有逻辑（条目只取文件夹索引已有的章节树与摘要，不新增检测；模型生成复用网关自身的 chat-completions 端点做分级路由与脱敏）。
- 实现：新包 `internal/semidx`（`Entry` + `Validate`：必填字段、FRAS 各 ≤500 字、标签值合法、回指完整且页码格式 `N`/`N-M`；`Index`：内存索引、按文档内容哈希增量再生、漂移检测、覆盖率报告；抽取式生成器：职责=标题+摘要、关联=结构邻居（上级/同级/包含章节）、适用范围=文件夹范围、硬约束=密级取用规则；模型生成器：提示词只含章节标题、页码与抽取式摘要、不含正文，调 chat-completions 端点、带 `X-AgentGate-Data-Level` 请求头走网关分级路由，输出逐条校验、回指由树填入、失败整篇回落抽取式并记 `model_error`，模型只覆盖部分章节时其余抽取式补齐记 `mixed`，绝密直接 title-only 且生成器拒发）；`folderkb.DocSnapshot`（克隆树 + 文件夹名/部门/密级/指纹）供派生索引取用；`store.KBFolderSource` 增加可选 `Department`（种子演示文件夹：公司制度→人力部、机密资料→采购部）；管理 API `GET /api/kb/semantic/coverage` + `GET /api/kb/semantic/entries` + `POST /api/kb/folders/{id}/semantic`（`{"model": true}` 可内联端点与令牌，令牌只用于当次请求、不存储不回显；未配模型时 503），重扫/新建/删除文件夹时自动刷新或清理条目；MCP 新工具 `kb_entries`（与 `kb_structure` 同款等级门禁，机密条目自带级不可见）；管理台文件夹知识库页新增语义索引卡片（覆盖率 + 过期数）与每文件夹「生成语义索引」按钮（中英双语）；详见 docs/semantic-index.md。
- 关键决策：条目按章节树节点生成（含根节点），不是按文档一行——目录层要的是「哪一节讲什么」，按文档一行无法定位章节；抽取式是默认生成方式（与 F18 摘要同一条安全线：不经过模型、不产生泄漏通道），模型生成是可选动作、只在管理员显式触发时发生；回指由树填入而非模型返回——模型即使编造来源也会被丢弃，这是「不许只保留模型生成的文字」的结构保证；漂移定义为「文件夹索引已更新、语义索引未跟进」（哈希比对），重扫经 API 时自动刷新、直接调 Registry.Rescan 的路径会在覆盖率里报 stale；部门标签优先取文件夹配置的部门、未配时取文件夹名（集合名是最可用的归属标签）。
- 测试：semidx 包 8 项（校验 12 类拒绝 + title-only 豁免、抽取式条目字段与标签、绝密 title-only、生成器失败回落、模型部分覆盖 mixed、增量再生与漂移、覆盖率、等级门禁、按源删除）+ 模型生成器 5 项（提示词只含索引不含正文、解析：回指由树填入/未知节点/重复节点/非法标签/围栏 JSON、假服务端：请求头含密级与令牌、回显提示词不解析、绝密零请求、上游 502 不回显响应体）+ API 端点 2 项（覆盖率与条目全链路、绝密 title-only、漂移出现与重扫清除、403/404/400、模型生成：真 JSON 成 model、回显回落 extractive + model_error、未配模型 503、令牌不回显）+ MCP 集成 3 项（BYOA 读内部文档条目、机密条目自带级不可见/受管级可见、tools/list 含 kb_entries）+ 冒烟新增 9 项断言；`gofmt`/`go vet`/`go test ./...` 全绿；`make smoke` 162 项断言 0 失败（基线 153 + 新增 9）。
- 本轮实测抓到的真问题：冒烟初版在 URL 查询串里直放含中文的 doc_id，urllib 按 ASCII 编码 URL 抛 UnicodeEncodeError——已改用 `urllib.parse.quote` 编码 doc_id（与 F12 轮的 edit 入参脱敏坑同类：工具管道对非 ASCII 内容的隐式处理，写脚本时留意）。
- 推送结果：整包快照推送一次成功（push-files，21 个文件一次提交 265b1744，未触发 contents API 回退；推送前核对同步基点：README / feature-backlog / mcp.go / smoke.py 四文件远端 blob 与本地父提交一致，DEVLOG.md 仅差本地独有的 F16 推送记录行、8 个新文件在 F16 提交树中不存在——以上由本地证明链 + 抽查核对确认，另一次逐文件核对调用（main.go）因连接器 30 分钟无响应而超时，未重复发起；推送后提交文件清单 21 个与本轮一致，抽查 semidx.go / semantic.go / mcp.go 三文件 blob 一致）；PR #15 文件清单与本轮一致，已合并进远端 main，远端 HEAD 97de9e2a（合并提交），读回验证 semidx.go 已在 main 上（blob 与本地快照一致）。待删分支新增 loop/f19；另核实：远端现存 loop 分支共 8 个（loop/f11–f16、loop/f18、loop/ui-overhaul），更早的 7 个（loop/f6、loop/f7、loop/f17、loop/license-agpl、loop/f8、loop/f9、loop/f10）已不在远端——待删清单实为 9 个（含 loop/f19），需 Nathan 网页端手动删除。

## 2026-11-22 · F16 OpenTelemetry 导出

- 本轮按常规规程选条目：体内的 12:26 特别指令（F17 + 蓝图修订稿）早已在 F17 轮执行完毕、执行后作废；F19 登记随 F14 轮进 backlog；P0、P1 全部 done，P2 按清单顺序取 F16（首个剩余 P2 条目）。远端 main 已含 F15（PR #13，HEAD 291d38b5），无在途分支需续传。
- 借鉴 ContextForge 官方文档（ibm.github.io/mcp-context-forge，2026-11-22 查阅）：厂商中立的 OTLP 导出（Phoenix/Jaeger/Zipkin/Tempo 等后端都接）、网关操作与工具/提示词调用自动产生 trace、LLM 指标（token 用量、成本、模型性能）单独记、关闭时零开销、标准环境变量配置；OTLP/HTTP JSON 线形据协议说明（2026-11-22 查阅）：POST `/v1/traces` 与 `/v1/metrics`、`Content-Type: application/json`、`resourceSpans → scopeSpans → spans` 三层嵌套、traceId 32 位 hex、时间戳纳秒十进制字符串、kind/status 为整数枚举、属性为 AnyValue。我们照此实现，但做三处改动：① 不拉 OpenTelemetry Go SDK——纯标准库手写导出器，零依赖单二进制不变（与 F9 不拉 oauth2/oidc 依赖的取舍一致）；② 属性只取 allowlist 元数据，提示词、工具参数、模型响应、DLP 命中值永不进 span（ContextForge 的 LLM 追踪默认会记 prompt，我们按分级红线收紧）；③ 导出失败只计数不重试、不阻塞请求，关闭时 tracer 为 no-op（零开销）。
- 复用检查（Nathan 复用优先指示）：未复制 ContextForge 代码——它是 Python 项目，无可直接复用的 Go 实现；OTLP/HTTP JSON 是公开协议，导出器约 300 行纯标准库自研并在包注释与此条记录来源；检测/分级复用本仓已有逻辑（span 属性只取网关与 MCP 已有判定结果，不新增检测）。
- 实现：新包 `internal/otel`（`Config` 读标准环境变量 + `Validate` 说明关闭原因；`Tracer`：span 队列上限 1000（满了丢弃计数）、5 秒批量导出（满 100 提前导出）、`traceparent` 入站加入/出站传播、累计 Sum 计数器、`Stats()` 供管理 API；nil tracer 全方法 no-op，调用点无需判空）；网关 `ChatCompletions` 每调用一个 SERVER span（`gateway.chat_completions`，经 statusRecorder 取 HTTP 状态、defer 收尾，`traceparent` 透传给模型上游）+ 计数器 `agentgate.gateway.calls{decision}` / `agentgate.gateway.tokens{channel}`；MCP `tools/call` 与 `prompts/get` 各一个 span（响应缓冲后判 JSON-RPC error，参数与结果永不进属性）+ 计数器 `agentgate.mcp.tool_calls{tool,decision}` / `agentgate.mcp.prompt_gets{decision}`；管理 API `GET /api/otel`（状态与计数）+ `POST /api/otel/test`（发合成 span 同步导出验证连通性，未启用 503、导出失败 502，仅 admin/security）；`cmd/agentgate` 从环境变量建 tracer 并接线三处（网关/MCP/管理 API），配置错误只记日志、不阻止启动；管理台用量统计页新增 OpenTelemetry 卡片（状态 + 测试按钮，中英双语）；详见 docs/opentelemetry.md。
- 关键决策：span 属性白名单在调用点硬编码（只传上表字段），不用通用「把审计事件转 span」的捷径——审计 Detail 以后加字段时不会悄悄把敏感值带进遥测；导出失败的 span 直接丢弃不重试（遥测不值得为一次失败阻塞网关或无限增长）；指标只做四个累计计数器，不做直方图与日志信号（后续轮次评估）；不做采样（调用量不大时全量导出，队列满了丢弃并计数）；只支持 OTLP/HTTP JSON，不支持 gRPC（需协议转换时中间放 OpenTelemetry Collector）。
- 测试：otel 包 8 项（配置解析与协议拒绝、traceparent 解析 6 类拒绝、nil/禁用 tracer no-op、导出 payload 形状：resource/scope 三层、纳秒时间戳、AnyValue 类型、累计 Sum、失败计数不致命、队列满丢弃计数、根 span 新 trace）+ 网关集成 4 项（干净调用 span 含模型/处置/Token 数且请求标志串不出现在导出体中、上游收到同 trace 的 traceparent、违禁词拦截为 ERROR span 且违禁词不泄漏、入站 traceparent 被加入、指标含 calls+tokens）+ MCP 集成 3 项（kb_search span 含工具名且参数不泄漏、受管工具拒自带级为 ERROR span 含 RPC 错误码、tools/list 不产生 span）+ API 端点 4 项（未启用状态/员工 403、测试端点收到合成 span、未启用 503、collector 宕机 502 且失败计数）；`gofmt`/`go vet`/`go test ./...` 全绿；`go test -race`（gateway/api/mcp/otel）全绿；冒烟新增 8 项断言，`make smoke` 153 项断言 0 失败。
- 本轮实测抓到的真问题：otel 单元测试首版在断言区持 collector 的互斥锁（`defer Unlock()` 锁住整个断言块），随后又调 `counts()`（同一把锁，不可重入）与 `Flush()`（触发 handler 也要这把锁）——自死锁，60 秒超时。已改为快照 payload 后立即释放锁（另附 goroutine 转储定位过程，见 backlog F16 条目）。
- 推送结果：整包快照推送一次成功（push-files，18 个文件一次提交 2bee9fde，逐文件哈希核对 18/18 一致，未触发 contents API 回退；推送前核对同步基点：10 个旧文件与远端 main 一致、docs/DEVLOG.md 有一处预期差异（本地独有的 F15 推送记录段，随快照一并带上）、7 个新文件不存在；分支 loop/f16 从远端 main 291d38b5 新建）；PR #14 文件清单与本轮一致，已合并进远端 main，远端 HEAD 9b272691（合并提交），读回验证 otel.go 与 opentelemetry.md 已在 main 上（blob 与本地快照一致）。待删分支新增 loop/f16（共 15 个，需 Nathan 网页端手动删除）。
- 另记登记（Nathan 2026-11-22 批准，均仅执行一次、随本轮提交进 backlog）：F20 受管 Agent 安装包分发与离线导入（借鉴 dsh-ee PR #309，Apache-2.0；分发包哈希登记为 F14 证明的基准值来源，形成闭环；另按 Nathan 同日要求补双包交付：管理包给 IT、用户包给员工，F20 的分发与离线导入同时覆盖两个包）；F21 组织架构同步与 IM 身份绑定（借鉴 dsh-ee 钉钉组织同步与飞书/钉钉身份绑定，Apache-2.0；红线：第三方应用密钥只加密存储、不回显，全程审计）；F22 专题知识包（借鉴 Liner linersh，MIT；F18 之上的策展层：配方 + 综合说明 + 审批发布，综合每条结论带原文回指）；F19 补约束一句（语义条目必须带原文回指）；backlog 末尾加备忘：IM 机器人消息中继 Nathan 决定暂不做（与受管级准入标准冲突，不立项）。三条目状态均为 todo，验收待立项细化。

## 2026-11-22 · F15 本地隐私网关 sidecar
- 本轮按常规规程选条目：体内的两段特别指令均已作废——12:26 特别指令（F17 + 蓝图修订稿）在 F17 轮执行完毕、F19 登记随 F14 轮进 backlog；P0、P1 全部 done，P2 按清单顺序取 F15（首个剩余 P2 条目）。远端 main 已含 F14（PR #12，HEAD 2e631884），无在途分支需续传。
- 借鉴 AstrLink 本地隐私网关（toolhunter.cc 介绍与 DeepWiki 隐私引擎页，2026-11-22 查阅）：端侧规则/本地模型检测、提醒/拦截/占位符替换、响应回程本地还原、隐私 worker 独立进程 + dry-run 诊断。我们照此实现，但做四处改动：① 检测复用本仓 `internal/guard`，不引 ONNX 模型与 Rust worker——本轮只做规则检测（蓝图既定）；② sidecar 本身就是 Go 单进程守护进程，映射只在本次请求内存中；③ 占位符编号与 F10 同款、流式还原用同一 holdback 模式；④ 出口收紧：sidecar 构造上只拨配置的企业网关一个地址（配合 endpoint/egress 防火墙模板），AstrLink 是通用本地网关、可配任意上游。
- 复用检查（Nathan 复用优先指示）：未复制 AstrLink 代码——其隐私引擎为 Go core + Rust ONNX worker，与本仓纯 Go 标准库路线不匹配；检测与还原复用本仓已有 `internal/guard`（F10 已验证的 Redactor/Restore/MaxPlaceholderLength），流式还原复用 F10 的 holdback 模式。
- 实现：新包 `internal/sidecar`（`Config` + `Validate`：网关地址须 http(s)、不得嵌凭据、不得指回自身；`Server`：`127.0.0.1` 代理，`POST /v1/chat/completions` 先扫描后转发、`GET /v1/models` 直通、`GET /healthz` 与 `GET /status`；违禁词本地 400 `sidecar_blocked`、PII/凭据/自定义词脱敏后转发、响应还原、发往网关时带 `X-AgentGate-Sidecar` 与只升不降的 `X-AgentGate-Data-Level`；计数器与最近 50 条决策只含类别、不含原文）；新二进制 `cmd/ag-guard`（配置优先级：`~/.agentgate/guard.json` < 环境变量 < 命令行，优雅停机）；网关 `ChatCompletions` 把 sidecar 标识记进审计 `detail.sidecar`（model_call 与 cache_hit）；管理 API `GET /api/sidecar/stats`（admin/security，扫审计日志计数经 sidecar 的调用/缓存命中/最近一次/按 Agent 分组）；`ag-setup` 增加 `--sidecar`/`--sidecar-url`（模型地址改写为 sidecar 地址、证明里 `guard_applied` 隐含为真、配置哈希覆盖实际写入的 model_base）；管理台「设备接入」页新增 sidecar 卡片（调用量 + 最近一次 + 部署说明，中英双语）；`make build`/`release` 产出 ag-guard，`make smoke` 起 sidecar 随测；详见 docs/local-gateway-sidecar.md。
- 关键决策：映射只在本次请求内存中、请求结束即丢弃（不写盘、不记日志、不上报），是端侧脱敏可信的前提；本地拦截只记在端侧 `/status`、不上报网关——平台侧只统计到达网关的 sidecar 调用（有意的分工，不是漏报）；sidecar 只代理模型流量，MCP 仍直连网关、由服务端工具授权与 DLP 管控；dry-run 先只记录不拦截不脱敏，与产品「先观察 → 再提醒 → 后拦截」一致；网关侧词表改动不自动同步端侧配置，网关二次检测是兜底。
- 测试：sidecar 包 9 项（配置校验六类拒绝、干净请求直通、PII 上游只见占位符/下游还原/状态计数无原文泄漏、违禁词本地拦截且网关零调用、dry-run 只记录、流式逐字节分片还原、tool_calls 参数共享占位符编号、models 直通、healthz）+ 流式还原白盒 2 项（逐字节读取下双占位符还原、空映射原样直通）+ 网关 3 项（sidecar 标识进 model_call 审计、cache_hit 同样记录、无标识不记）+ API 端点 1 项（计数口径、员工 403）+ 冒烟新增 9 项断言；`gofmt`/`go vet`/`go test ./...` 全绿；`make smoke` 145 项断言 0 失败。
- 本轮实测抓到的真问题：首版流式还原只对「放行前缀」做 Restore、把占位符切在放行边界上时会漏还原（占位符前半段已发出、后半段留在缓冲，永不再合）。已按 F10 的 streamCopy 改法修正：每次对整段待发缓冲做 Restore、只放行超出 holdback 的安全前缀；冒烟的流式断言与白盒逐字节测试均覆盖此路径。另：Go 的 `json.Marshal` 默认把 `<PII_1>` 转义成 `\u003cPII_1\u003e`，上游报文断言须先解码再计数（与 F10 占位符转义同款）。
- 推送结果：整包快照推送一次成功（push-files，17 个文件一次提交 7b2e1121，逐文件哈希核对 17/17 一致，未触发 contents API 回退；推送前核对同步基点：8 个旧文件与远端 main 一致、docs/DEVLOG.md 有一处预期差异（本地独有的 F14 推送记录段，随快照一并带上）、8 个新文件不存在；建分支曾被连接器服务重启中断两次，核实分支未建后重试成功；另记：/tmp 下的快照 worktree 在推送前消失（原因不明），改用 ~/workspace/agentgate-f15-snap 重建快照后推送成功，清单与同步记录一并存于该目录旁）；PR #13 文件清单与本轮一致，已合并进远端 main，远端 HEAD 291d38b5（合并提交），读回验证 sidecar.go 与 local-gateway-sidecar.md 已在 main 上（blob 与本地快照一致）。待删分支新增 loop/f15（共 14 个，需 Nathan 网页端手动删除）。

## 2026-11-22 · F14 Agent 证明（Attestation）
- 本轮按常规规程选条目：12:26 特别指令（F17 + 蓝图修订稿）早已在 F17 轮执行完毕、执行后作废；P0、P1 全部 done，P2 按清单顺序取 F14（首个剩余 P2 条目）。远端 main 已含 F13（PR #11，HEAD ebd42d4a），无在途分支需续传。
- 借鉴 IETF RATS RFC 9334（rfc9334）与 SPIFFE/SPIRE 官方文档（spiffe.io，2026-11-22 查阅）：RATS 的角色模型——Attester 上报 Evidence，Verifier 按 Appraisal Policy 对照 Reference Values 与 Endorsements 评估、产出 Attestation Results，Relying Party 据此决策；Verifier 与 Relying Party 分离；新鲜度靠 nonce（或可信时钟/epoch）。SPIFFE/SPIRE 的做法是先证明、后发身份：节点证明与工作负载证明都通过才发 SVID，身份即证明链的结论，SVID 短期有效。我们照此实现，但做三处改动：① 新鲜度不用单独 nonce——证明绑定到一次设备码（code + polling secret，单次使用、10 分钟过期），一份证明不能重放到下一次登录；② 失败不拒绝登录、而是降级为自带级（员工仍可用通用能力，敏感能力不落到不合规 Agent 手里），降级本身记审计并触发 F5 告警；③ 本轮只做软件自报证据的一致性校验，不做密码学绑定（硬件根信任留待后续，见 docs/attestation.md 已知限制）。
- 复用检查（Nathan 复用优先指示）：无可直接复制的代码——RATS 是架构文档、无参考实现；SPIRE/Veraison 是整套证明系统（Apache-2.0），与本功能体量不匹配；评估逻辑（版本比较 + 哈希白名单 + 插件白名单）约 150 行纯标准库，自研并在包注释与此条记录来源。
- 实现：新包 `internal/attest`（`Report` 证据 + `Policy` 评估策略与参考值 + `Evaluate` 裁决 + `CompareVersion` semver 式比较，原因只记类别码、不记哈希值）；`store` 增加 `AttestationPolicies`（按 Agent 类型）与设备码上的证明记录、令牌上的 `Attested`/`AttestVerdict`；`auth.SubmitAttestation`（凭 code+secret 提交、绑定设备码，Agent 类型以设备码为准）+ `ApproveDeviceCode` 改为返回实发等级（通过才发受管级、失败或缺失且策略要求时降级自带级、无策略时按申请发并标记未验证）；`POST /api/setup/device/attest`（无会话，凭 code+secret）+ 管理 API `GET/POST /api/attestation/policies`、`DELETE /api/attestation/policies/{agent}`、`GET /api/attestation/reports`（读审计日志）；`alert.FromEvent` 新增 `attest_downgrade`（warning）；`ag-setup` 增加 `-version`/`-plugins`/`-guard` 并在登录时提交证明（配置哈希取待写网关配置骨架的 SHA-256、不含令牌，二进制哈希取自身可执行文件；提交失败只警告、不阻塞登录）；管理台新增「Agent 证明」页（策略增删 + 最近证明记录，中英双语）与令牌表证明列；演示种子 `dsh` 策略为最低版本 0.1.0 且要求提交证明；详见 docs/attestation.md。
- 关键决策：证明失败不拒登录、降级自带级 + 告警（分级约束高于可用性，但不断员工的基本使用）；证明只在登录时做一次、30 天令牌期内不重评（与令牌生命周期对齐，重评留待后续）；配置哈希只覆盖网关配置骨架、不含令牌与密钥（审计不落敏感原文）；原因码只记类别、不记哈希值（审计日志不暴露可比对的哈希）；`ag-setup` 提交证明失败只警告、不阻塞登录（老版本客户端仍能登录，平台侧降级兜底）。
- 测试：attest 包 3 项（版本比较 12 组、评估 10 类分支含多原因并报、报告与策略校验）+ auth 包 1 项（通过→受管已验证、失败→自带级、未提交→自带级、无策略→受管未验证、错 secret/批准后提交拒绝）+ API 端点 1 项（种子策略、证明通过/失败/未提交全链路、降级审计事件、策略 CRUD、400/403/404）+ alert 分类 1 项（`attest_downgrade` 为 warning，`attest_submit` 本身不告警）+ 冒烟新增 9 项断言；`gofmt`/`go vet`/`go test ./...` 全绿；`go test -race`（attest/alert/auth/api）全绿；`make smoke` 136 项断言 0 失败。
- 冒烟调试实录（与 F14 功能无关的旧缺陷）：本轮冒烟一度 135 通过、1 失败（`downgrade alert delivered to webhook`）。手动复现（新 webhook + 立即降级、预算事件 + 降级、工具拒绝）均能投递；审计日志显示降级事件已记、但 `alert_delivered` 自 seq 14 后再无记录；分发器临时 DEBUG 显示 `tool_call_denied` 的 Send 在 POST 到接收器后挂满 10 秒客户端超时、重试积压致队列满。goroutine 转储证实请求已写出、读循环在等响应；`ss` 显示接收端 socket 收 buffer 躺着 539 字节未读请求、且 LISTEN socket 已消失、smoke.py 只剩主线程——根因是冒烟脚本 F5 段末尾 `httpd.shutdown()` 提前关闭了 Webhook 接收器，而 webhook 仍注册在服务端，F6 `tool_call_denied` 与 F14 `attest_downgrade` 继续向已死监听投递。已改为 main() 末尾再关闭接收器，冒烟随即 136 项全绿。另记后续改进：单个失效 webhook 会以 10 秒超时 + 退避重试阻塞分发队列（队头阻塞），可考虑单 webhook 超时隔离或并行投递。
- 另记 F19 登记（Nathan 2026-11-22 01:14 批准立项，P2，状态 todo）：知识库语义索引层，借鉴 AOCI-CODE（aoci-spec/aoci-code）的方法——每对象一行语义条目（职责/关联/契约/约束）+ 标签坐标 + 索引治理（校验/漂移/覆盖率），用于优化 F18 文件夹知识库的弱摘要（现为章节首句抽取式）；语义条目须模型生成，必须走网关分级路由/脱敏，绝密只给标题；只借方法不抄代码——AOCI-CODE 许可证 FSL-1.1-MIT（Fair Source，非 OSI 开源、有竞品限制）与 AGPL-3.0 不兼容，代码禁止复制。条目已写入 backlog，随本轮一并提交。
- 推送结果：上一轮推送被 GitHub 连接器服务反复重启中断（list_commits/list_branches/get_file_contents 多次失败，06:24 审批卡过期）；Nathan 10:25 要求重新提交审核后由主对话手动补推成功：建 loop/f14 分支、整包快照推送 22 个文件一次提交 511a90f0、逐文件哈希核对 22/22 一致；PR #12 文件清单与本轮一致，已合并进远端 main，远端 HEAD 2e631884（合并提交）。本轮复核：远端 main HEAD 确为 2e631884（父提交即 511a90f0），读回 main 上 docs/DEVLOG.md、docs/feature-backlog.md、README.md、internal/attest/attest.go 四个关键文件，blob SHA 与本地快照全部一致；本轮基线 go vet/go test 全绿、make smoke 0 失败。待删分支新增 loop/f14（共 13 个，需 Nathan 网页端手动删除）。

## 2026-11-21 · F13 成本计价与部门预算
- 本轮按常规规程选条目：12:26 特别指令（F17 + 蓝图修订稿）早已在 F17 轮执行完毕、执行后作废；P0、P1 全部 done，P2 按清单顺序取 F13（首个剩余 P2 条目）。远端 main 已含 F12（PR #10，HEAD 6c9faa4c），无在途分支需续传。
- 借鉴 LiteLLM 官方文档（docs.litellm.ai/docs/tutorials/claude_code_cut_costs）：虚拟 key 预算 `max_budget`（USD）+ `budget_duration`（"24h"/"30d" 滚动窗口、可叠加），超限后下一调用直接拒绝，另有 `model_max_budget` + `budget_fallbacks` 静默降级链；Portkey 官方文档（预算上限与限额用例指南）：按虚拟 key/工作区设成本或 token 上限、告警阈值、按周（周日 0 点 UTC）/按月（每月 1 日 0 点 UTC）自动重置、上限设定后不可改。我们照此实现，但做三处改动：① 预算按**部门**设而非按 key（企业按部门分钱）；② 不做静默降级——超限直接 429 明确拒绝（降级会改变模型行为、且分级约束高于可用性）；③ 预算可随时 PATCH 改上限（改动本身记审计、可追溯）。计价形状沿用 LiteLLM 成本表的输入/输出分开计价（input/output 每百万 Token，USD）；其整张真实厂商价目表未复制——AgentGate 的渠道服务的是管理员配置的模型（含 mock 与本地模型），价目应由管理员按合同价配，抄表是错误的复用。
- 实现：新包 `internal/billing`（`Cost` 输入/输出拆分计价、`MonthWindow` 自然月 UTC 窗口、`DepartmentSpend` 按审计日志汇总部门本月花费）；`store.ModelPrice` + `Channel.Pricing` + `store.DepartmentBudget`；`audit.Event` 增加 `CostUSD`（调用时按服务渠道单价写入、哈希链覆盖，0 表示未定价）；网关 `parseUsage`/`streamCopy` 返回 prompt/completion/total 拆分（流式用量同样拆分），调用完成即计价入审计；预算预检在流水线之前（限流与令牌配额之后）：本月花费 ≥ 上限时 429 `department_budget_exceeded` + `violation` 审计事件；花费首次达到告警阈值时记一条 `budget_threshold` 审计事件（每月每部门一次，`alert.FromEvent` 映射为 warning）；`GET /api/stats` 各维度与 totals 增加 `cost_usd`，totals 增加 `unpriced_calls`；管理 API `GET/POST /api/budgets` + `PATCH/DELETE /api/budgets/{id}`（仅 admin，部门唯一、重复 409）；管理台用量统计页三张表加成本列、总览 KPI 下显示累计成本与未定价调用数、新增部门月度预算卡片、渠道表加单价列、添加渠道表单可填单价 JSON（中英双语）；演示种子：四条渠道配演示单价，研发部演示预算 $50/月、阈值 0.8。
- 关键决策：成本在调用时写入审计事件、事后单价再改不改写历史（F2 的对账原则：统计与审计永远一致）；未定价调用成本记 0 并单独计数——成本总数是下限、不是编出来的数，本地模型 $0 同样计入未定价（0 价与未定价在事件中无法区分，是有意的保守口径）；预算预检在缓存之前——被预算拦下的调用即使命中缓存也不放行（与 F11 的“命中仍计 RPM”同款门口语义）；阈值警告每月每部门只发一次（网关内运行态 map 记月，重启后当月若已跨阈值、下一调用会重发一次，可接受）。
- 测试：billing 包 3 项 + 网关预算测试 4 项（调用成本入审计且数额与 mock usage 拆分一致、流式调用同样计价、超预算后下一调用 429 且审计为 violation、其他部门不受影响、阈值警告每月恰一次、未定价渠道成本为 0）+ API 端点 1 项（预算 CRUD 全链路、种子预算、400/403/404/409）+ stats 聚合 1 项 + alert 分类 1 项；`gofmt`/`go vet`/`go test ./...` 全绿；`go test -race`（gateway/api）全绿；`make smoke` 126 项断言 0 失败（新增 12 项 F13 断言）。
- 冒烟实测抓到的真问题：初版冒烟用 mock-eco 做跨越预算的调用，第一次跑失败——F12 轮建的 mock-eco 渠道随机 ID 排在 ch_cloud 之前、且无单价，调用走了它、记了 $0 成本， spend 没动、第二次调用放行。产品行为本身符合既有选路规则（同级渠道按 ID 排序取首个），但它暴露了“未配单价的渠道会悄悄让调用记为未定价”这一运维坑：冒烟改用只由定价渠道服务的 mock-pro 钉住（ch_cloud 按密级最低必为首选）；backlog 已知注意点已记下此坑。另：网关预算测试初版用三次相同内容调用测阈值，F11 缓存把后两次截成 HIT、花费用没涨，改为每次不同内容——同一教训。
- 推送结果：整包快照推送一次成功（push-files，22 个文件一次提交 7a1f554a，逐文件哈希核对 22/22 一致，未触发 contents API 回退；推送前核对同步基点：15 个旧文件与远端 main 一致、docs/DEVLOG.md 有一处预期差异（本地独有的 F12 推送记录段 + F13 条目，随快照一并带上）、6 个新文件不存在；首次 push-files 调用因服务重启未完成，核实分支头未动后重试成功）；PR #11 文件清单与本轮一致，已合并进远端 main，远端 HEAD ebd42d4a（合并提交），读回验证 billing.go 与 gateway.go 已在 main 上（blob 与本地快照一致）。

## 2026-11-21 · F12 渠道多 Key 与轮换
- 本轮按常规规程选条目：12:26 特别指令（F17 + 蓝图修订稿）早已在 F17 轮执行完毕、执行后作废；P0、P1 全部 done，P2 按清单顺序取 F12（首个剩余 P2 条目）。远端 main 已含管理台重写（PR #9，HEAD 8f9dbdb8），无在途分支需续传。另：本轮前 Nathan 在主对话新增「复用优先」指示（2026-11-21 22:38）：能直接复制开源代码就复制、不要都自己写，项目稳定最重要；仅限 AGPL-3.0 兼容许可证、保留原版权声明、DEVLOG 记录来源——已同步进 development-loop.md（替换了旧的「只记设计、不复制」表述）。
- 借鉴 QuantumNous/new-api 源码（AGPL-3.0，与本项目许可证兼容）`model/channel.go` 的 `GetNextEnabledKey`：单渠道多密钥 + 按索引的状态列表（缺失即启用）+ 轮询游标，每渠道一把锁、环形扫描第一把启用密钥、游标推进、全部禁用时显式报错（不静默回落失效 key）；LiteLLM 多部署路由（同一模型名多部署、各配 key 与 rpm/tpm）与通用轮换 runbook（先加新 key、验证、再删旧 key）为辅。本轮按新指示直接复用了 new-api 的选取算法形态（逐字复制不适用：其函数绑定自家 DB 模型与错误类型，改写为本仓库的 store 类型），来源与许可证记于 `internal/gateway/keys.go` 文件头注释。
- 实现：`store.ChannelKey` + `Channel.Keys []ChannelKey`（`APIKeyEnc`/`KeyMasked` 单 key 字段退役；旧快照本就不持久化 key，故无需迁移）；`internal/gateway/keys.go`（`nextKey` 轮询 + `disableKey` 摘除审计，锁顺序 keyMu → store 锁）；网关转发循环改为渠道内 key 循环：401 摘除该 key、同请求换下一 key 重试，全部 key 都 401 时原样返回上游 401，全部停用时报明确的 `channel_no_enabled_key`；`alert.FromEvent` 新增 `channel_key_disabled`（warning，联动 F5）；管理 API（创建接受 `api_keys`、增/删/启停四个端点，仅 admin，密钥永不回显）；管理台渠道表逐 key 显示 + 「管理渠道密钥」卡片（中英双语）；演示种子 `ch_cloud` 带两把 key。
- 关键决策：只做轮询、不做随机模式（可预测、易审计）；密钥逐把 AES-GCM 加密（不用 new-api 的换行拼接单字符串，避免新 key 含换行时切分错乱）；游标与停用状态为运行态（与 F1 冷却一致，重启后失效 key 重新启用、由网关再次向上游证明）；401 只摘除该 key（不像 new-api 那样把整条渠道标不可用），429/5xx 仍走 F1 的渠道级故障转移；删除 key 后其后 key 索引前移（位置索引语义，与 new-api 一致）；上限 10 把 key/渠道。
- 测试：gateway 白盒 2 项 + 网关端到端 3 项（401 摘除 + 同请求重试、两 key 轮询 a/b/a、全 key 失效 401 与 `channel_no_enabled_key`）+ API 端点 1 项 + alert 分类 1 项 + 冒烟新增 9 项断言；`gofmt`/`go vet`/`go test ./...` 全绿；`go test -race`（gateway/api）全绿；`make smoke` 114 项断言 0 失败（基线 105 + 新增 9）。
- 已知注意点：key 停用状态与轮询游标是运行态，重启后失效 key 会重新启用、由网关再次向上游证明；429 不触发 key 内轮换（限流仍走 F1 的渠道级故障转移）。
- 推送结果：整包快照推送一次成功（push-files，17 个文件一次提交 69984635，逐文件哈希核对 17/17 一致，未触发 contents API 回退；推送前核对同步基点：13 个旧文件与远端 main 一致、docs/DEVLOG.md 有一处预期差异（本地独有的 PR #9 记录段，随快照一并带上）、3 个新文件不存在）；PR #10 文件清单与本轮一致，已合并进远端 main，远端 HEAD 6c9faa4c（合并提交），读回验证 keys.go 已在 main 上（blob 与本地快照一致）。
- 另记（工具管道脱敏坑）：本轮写冒烟脚本时，`muse.edit` 的入参里 `token=alogin["session"]` 会被管道当作会话凭证替换为字面量 `<redacted>` 写进文件，导致 smoke.py 语法错误；改用已有的 `admin_tok` 变量（`alogin.get("session")`）绕过。后续轮次写脚本时避免在 edit 入参里直写 `...["session"]` 取值表达式，优先复用已有变量。

## 2026-11-21 · 管理台重写（WebUI overhaul，对标 dsh-ee）
- 本轮选题：不在 backlog 清单内，是 Nathan 2026-11-21 亲自运行管理台后的直接指示——旧控制台与 dsh-ee 差距很大，「完全没有任何使用逻辑和可使用性」，颜色也差得远，要求重做。重写代码此前已在独立 worktree（`/tmp/ag-ui`，分支 `ui/overhaul`）完成并本地提交，本轮的阻塞项逐一解除：F18 与 F11 均已合并进远端 main、浏览器截图验证已回传（`~/workspace/ui-shots/` 7 张），故本轮收尾该在途工作并推送；下一轮恢复常规规程，取 P2 首项 F12（渠道多 Key 与轮换）。
- 重写内容（单文件 `internal/webui/assets/index.html` 全量重写，+687/−327）：浅色主题（对标 dsh-ee 的 #f6f9fc 背景、白面、靛蓝主色）；左侧分组侧边栏（运营 / 接入与身份 / 模型网关 / 能力中心 / 系统）；总览仪表盘（KPI 卡：调用 / Token / 拦截 / 审计链状态 + 按部门与处置分布条 + 最近审计 + 快捷入口）；每模块独立视图、带标签的内嵌表单卡、Toast 反馈、空状态；密级/等级/状态/健康按 pill 分色；中英 i18n 保留。API 与后端零改动，全部卡片沿用既有端点；F11 响应缓存卡片已移植进用量统计视图（`dc79637` 移植前为 `ca61856`，rebase 后重排）。
- 截图验证（本轮人工逐张核查 7 张，全部通过）：登录页（浅色卡片、演示账号提示）；总览（KPI 2 调用 / 52 Token / 1 拦截 / 审计链完整 ✓，部门与处置分布条、最近审计表、快捷入口）；模型渠道（添加表单含信任类型下拉，渠道表信任类型与健康 pill 俱全，脱敏云/专有云/本地模型四条种子渠道）；文件夹知识库（添加表单 + 已索引两文件夹，密级 pill 正确）；用量统计（按部门/模型/渠道三表 + 响应缓存卡片，实测显示 enabled true · hits 1 · misses 1 · entries 1 · ttl 3600s）；审计日志（链完整、导出 NDJSON / 归档 180 天前入口、事件表）；技能（提交表单 + 已发布技能列表）。无布局错乱、无横向溢出、无空白视图。
- 关键决策：重写只动 `index.html` 一个文件（后端与 API 零改动，风险面最小）；rebase 到含 F11 的 main（3e67bca）时 `index.html` 冲突，取重写版解决——F11 缓存卡片已在重写分支内移植（截图实测有数据），不丢功能；分支沿用 `ui/overhaul`，远端推送分支按 Loop 惯例取 `loop/ui-overhaul`。
- 测试：`gofmt`（无 Go 改动）/`go vet`/`go test ./...` 全绿；`make smoke` 105 项断言 0 失败（与 F11 基线一致，无新增断言——纯前端重写）。
- 已知小瑕疵（不阻塞推送，留待后续轮次）：技能列表中种子技能 `reimbursement-helper` 的「更新」列显示 `0001-01-01`（种子数据未填 UpdatedAt，零值直接渲染）；修法是种子补 UpdatedAt 或渲染时零值显示「—」，属后端/种子小修，不在本轮纯前端重写范围内。
- 推送结果：整包快照推送一次成功（push-files，2 个文件一次提交 ff55a137，逐文件哈希核对 2/2 一致，未触发 contents API 回退；推送前核对同步基点：两文件与远端 main 一致、无分歧）；PR #9 文件清单与本轮一致，已合并进远端 main，远端 HEAD 8f9dbdb8（合并提交），读回验证 index.html 已在 main 上（blob 与本地快照一致）。

## 2026-11-21 · F11 精确匹配响应缓存
- 本轮按常规规程选条目：特别指令（F17 + 蓝图修订稿）已在 F17 轮执行完毕、执行后作废；P0、P1 全部 done，P2 按清单顺序取 F11（首个剩余 P2 条目）。远端 main 已含 F18（PR #7，HEAD 73137e7f），无在途分支需续传。
- 借鉴 Cloudflare AI Gateway 官方缓存文档与 Portkey Simple/Semantic 缓存文档：键是提供商 + 端点 + 模型 + 认证头 + 整份请求体做 SHA-256（Portkey 是整份请求体精确匹配），TTL 最小 60 秒，响应头报告 HIT/MISS，流式不缓存。我们照此实现，但做三处治理收紧：① 请求与响应双向干净才缓存——请求须公开/内部且 DLP 零命中，响应入库前再扫一次；② 键里加渠道 ID 与数据密级；③ 命中审计为独立的 `cache_hit` 事件，不进 F2 用量统计（只计量上游用量），不触发告警。
- 实现：新包 `internal/cache`（进程内内存缓存：SHA-256 键、TTL 默认 1 小时、最多 1000 条 FIFO 淘汰、仅 200 响应且 ≤1 MiB 才存、命中/未命中计数、停用时不查不存、清空不重置计数；重启即清空）；网关 `Policy.CacheTTL` + 环境变量 `AGENTGATE_CACHE_TTL_SECONDS`（默认 3600，最小 60）与 `AGENTGATE_CACHE_ENABLED`（0/false 启动即停用）；`ChatCompletions` 在管线后、转发前查缓存，命中直接返回存体（0 tokens，不记 TPM/配额/统计），响应头 `X-AgentGate-Cache: HIT/MISS`（不合规请求不查缓存、故无此头）；管理 API `GET /api/cache` + `POST /api/cache`（开关）+ `POST /api/cache/clear`（仅 admin/security）；管理台新增响应缓存卡片（中英双语）；失败转移时按实际服务渠道的键入库。
- 关键决策：缓存键必须显含数据密级（backlog 验收原文），即使同密级的公开/内部共享同一首选渠道——响应本身无差别故共享安全，但键上显式区分可防未来按密级分流的响应差异串户；命中仍计 RPM（请求确实发生了），但不计 TPM/配额/统计——「不计上游用量」指的是不消耗上游资源，网关门口的限流照常；停用保留已存条目、重启用可继续命中（停用≠清空，清空是独立动作）；语义缓存不做（要向量库，与零依赖定位冲突）。
- 测试：cache 包 7 项 + 网关缓存测试 5 项（命中逐字节相同且审计恰为 1 次上游调用 + 1 次 cache_hit、变 temperature 不命中、PII 请求两次均走上游、机密请求不查缓存、流式无缓存头、响应含 PII 时不入库）+ API 端点 1 项（状态/开关/清空/403/401/400）；另修顺带发现的测试精度问题：故障转移测试的相同请求会被缓存截住，`setupFailover` 改为停用缓存（该套测试测上游故障计数，与缓存无关）；`gofmt`/`go vet`/`go test ./...` 全绿；`make smoke` 105 项断言 0 失败（新增 13 项 F11 断言）。
- 已知限制：键是整份请求体的精确匹配，键顺序或空白不同即不命中；故障转移后按实际服务渠道入库，首选恢复前命中率略降；缓存只在内存中，重启即清空（与 F1 渠道健康状态同为运行时状态）。
- 推送结果：整包快照推送一次成功（push-files，15 个文件一次提交 5f195338，逐文件哈希核对 15/15 一致，未触发 contents API 回退；推送前核对同步基点：10 个旧文件与远端 main 一致、5 个新文件不存在）；PR #8 文件清单与本轮一致，已合并进远端 main，远端 HEAD 9c5322a0（合并提交），读回验证 cache.go 已在 main 上（blob 与本地一致）。

## 2026-11-21 · F18 文件夹即知识库
- 本轮按常规规程选条目：特别指令（F17 + 蓝图修订稿）已在 F17 轮执行完毕、执行后作废；P0 全部 done，P1 按清单顺序取 F18（唯一剩余 P1 条目）。远端 main 已含 F10（PR #6，HEAD 95729472），无在途分支需续传。
- 借鉴 PageIndex 官方介绍（pageindex.ai/blog/pageindex-intro）与架构深挖（nowucca.com/research/pageindex）：索引是一棵章节树（节点含 node_id、标题、起止页码、每节点一段摘要），检索是 Agent 用三个工具走目录 → 读树（只要摘要不要正文）→ 按页取原文；Flash 模式不用 LLM 建结构（字体样式识别标题、优先用 PDF 内嵌书签）。我们照此实现，但做三处改动：① 摘要不用模型生成，改抽取式（章节首句）——摘要生成因此不产生任何出站调用、不成为泄漏通道；② PDF 用真实页码，.md/.txt/.docx 用 45 行伪页；③ 检索仍保留关键词（与现有 kb 同一流水线），树只负责渐进式取用。
- 实现：新包 `internal/folderkb`（内存索引 `Registry` + 增量 `Rescan`：大小+修改时间未变不读文件、内容哈希相同保留旧解析、抽取失败保留上次可用索引；`Catalog`/`Structure`/`Section` 三层取用 + 按章节粒度的 `Search`）；抽取器纯标准库：.md/.txt（ATX 标题建树）、.docx（zip + XML，只取 `w:t`，Heading1/2/3 建树、显式分页符分断）、.pdf（best-effort：Flate 解压 + Tj/TJ 字面串抽取，内容流与页数 1:1 时映射真实页码）；`store.KBFolderSource` 持久化注册、索引不落盘；管理 API `GET/POST /api/kb/folders` + `DELETE /api/kb/folders/{id}` + `POST /api/kb/folders/{id}/rescan`（仅 admin/security）；MCP 新工具 `kb_catalog` / `kb_structure` / `kb_section`，`kb_search` 合并种子文档与文件夹命中；管理台新增文件夹知识库卡片（中英双语）；演示种子注册 `demo/folders/公司制度/`（internal：.md）与 `demo/folders/机密资料/`（confidential：2 页 .pdf，内容流未压缩以保持纯 UTF-8）。
- 关键决策：摘要抽取式而非模型生成——这是治理要求不是偷工减料：摘要是原文子串、不出本机、与原文同密级门禁，不可能把机密内容带到不该去的地方；首个一级标题即文档标题、不进章节树（.md 的 H1 与 .docx 的 Heading1 同理）；根节点（n0）章节含整篇文档正文、但检索时有子节点即不参与（否则每处命中重复两次）；无标题的多页文档按页建节（「第 N 页」），引用保持页码精确；机密文件夹对自带级四处不可见（目录/检索/章节树/章节原文），与 `kb_get` 的受管级限制同源但按 `MaxLevelForTier` 放行内部章节。
- 测试：folderkb 包 7 项 + API 端点 1 项（注册→列表→重扫→删除全链路、400/403/404/409）+ MCP 集成 3 项（目录→结构→章节三步取用、机密文件夹隔离、kb_search 合并命中）+ 冒烟新增 8 项断言；`gofmt`/`go vet`/`go test ./...` 全绿；`make smoke` 92 项断言 0 失败。
- 已知限制：伪页不是渲染页码；.docx 只读正文段落；PDF 抽取不支持扫描件、加密 PDF 与非标准编码（计入扫描 errors）；Excel 暂不支持；索引在内存中，重启后启动时自动重扫。详见 docs/folder-kb.md。
- 推送：按分支 + PR 机制走 `loop/f18` 分支，见本轮汇报。
- 推送结果：整包快照推送两次成功（push-files：第一次 19 个文件一次提交 6b8803fe，第二次 5 个文件一次提交 9ee19e1f，逐文件哈希核对 20/20 一致）；PR #7 文件清单与本轮一致，已合并进远端 main，远端 HEAD 73137e7f（合并提交），读回验证 folderkb.go 与演示 PDF 已在 main 上（PDF 内容为未压缩版原字节）。另：分支历史含两次二进制试错提交（见上条另记），终态文件正确。
- 另记（推送通道限制）：GitHub 连接器的两种写通道都无法原样传输二进制文件——`push-files` 只接受 UTF-8 文本（快照校验直接拒掉含非 UTF-8 内容的整包），contents API（`create_or_update_file`）会把 `content` 当文本再做一次 Base64（实测：传 Base64 串存下来的是 Base64 文本本身，blob SHA 与 Base64 文本一致；传 latin-1 解码串会被 UTF-8 重编码，字节膨胀）。本轮因此：① 演示 PDF 改为未压缩内容流（仍是合法 2 页 PDF，纯 UTF-8，可走快照推送；Flate 解压路径由单元测试覆盖）；② 演示文件夹去掉 .docx（.docx 抽取仍由单元测试覆盖，测试内现生成夹具）；③ 两次二进制试错各在 `loop/f18` 分支留了一个提交（先是 Base64 文本版 PDF、再是 UTF-8 重编码版），最终以快照推送修正，分支历史有此污点、终态文件正确。后续轮次如需向仓库推二进制演示文件，须另寻通道（Nathan 网页端上传或本地 git 推送）。

## 2026-11-21 · F10 流式脱敏与 tool_calls 防护（探针 3）
- 本轮按常规规程选条目：特别指令（F17 + 蓝图修订稿）已在 F17 轮执行完毕、执行后作废；P0 全部 done，P1 按清单顺序取 F10。远端 main 已含 F9（PR #5，HEAD 0032b40a），无在途分支需续传。
- 借鉴 nr-llm ADR 88《Live streaming redaction》与 Sether《PII redaction for LLM applications》：流式处理的核心是 holdback——秘密/占位符可能被切在两个 chunk 之间，逐 chunk 独立处理必然漏；做法是对整段已收 buffer 反复处理、只放行超出安全距离（最长占位符的转义后长度）的前缀，尾部在流结束时冲掉；且必须对原始 buffer 反复处理（已打标记会破坏模式字符类）。我们方向相反（还原占位符而非现场脱敏），但同一 holdback 模式；无映射时零缓冲直通。
- 实现：`internal/guard` 新增 `Redactor`（同一请求内多段文本共享占位符编号）与 `MaxPlaceholderLength`（转义后最长占位符长度）；`internal/gateway` 的 `chatRequest` 改为命名类型并解析 `tool_calls`（`scanText` 把参数文本并入防护管线），`redactBody` 用同一 `Redactor` 处理 content 与各 tool_calls 参数；新文件 `internal/gateway/stream.go`（`streamCopy` 带 mapping：无映射直通、有映射缓冲还原 + 用量提取，UTF-8 切分点退让到 rune 边界）；`internal/mockllm` 回显 tool_calls 参数（供端到端观测上游实收内容）。另修一个顺带发现的旧 bug：流式用量的旧提取切片出非法 JSON，流式调用一直记 0 tokens、TPM 与统计都漏记，改为按花括号配对解析 usage 对象。
- 关键决策：同一值在 content 与 tool_calls 参数里必须得同一占位符——逐段编号再合并映射会冲突（参数里的邮箱在内容里是 `<PII_2>`、在参数里会变成 `<PII_1>`），故一个请求只用一个 `Redactor`；holdback 按转义后长度算（上游 JSON 编码把 `<PII_1>` 写成 `\u003cPII_1\u003e`，长 10 字节）；`tools` 定义（函数 schema）不在本轮管线内，只处理消息里的 `tool_calls` 参数。
- 测试：包内 stream 测试 5 项（1/2/3/7 字节分片下转义与原始两种占位符均还原、多字节文本无损、流尾占位符冲刷、用量提取、空映射直通、双占位符不同长度 holdback）+ 包内 redact 测试 3 项（tool_calls 脱敏与字段保留、共享占位符无冲突、无发现时原文直通）+ HTTP 端到端 5 项（流式还原、流式 tool_calls 参数还原、tool_calls 违禁词 400、自带级机密参数 403、受管级机密参数脱敏后 200 且原文还原）+ 冒烟新增 3 项断言；`gofmt`/`go vet`/`go test ./...` 全绿；`make smoke` 84 项断言 0 失败。
- 探针 3 结论：通过——占位符跨分片还原用例全过；同一值在同一请求内跨 content/tool_calls 得同一占位符。
- 推送：按分支 + PR 机制走 `loop/f10` 分支，见本轮汇报。
- 推送结果：整包快照推送一次成功（push-files，11 个文件一次提交 dda1cfe7，逐文件哈希核对 11/11 一致，未触发 contents API 回退；核对时发现 `get_file_contents`/`list_commits` 的分支参数名应为 `ref`/`sha` 而非 `branch`，错传会被静默忽略读回 main——已用正确参数重核）；PR #6 文件清单与本轮一致，已合并进远端 main，远端 HEAD 95729472（合并提交），读回验证 stream.go 已在 main 上。

## 2026-11-21 · F9 OIDC / LDAP 登录
- 本轮按常规规程选条目：特别指令（F17 + 蓝图修订稿）已在 F17 轮执行完毕、执行后作废；P0 全部 done，P1 按清单顺序取 F9。Nathan 于本轮前在主对话说「继续」，Loop 照每小时一轮推进。
- 借鉴 OpenID Connect Core 1.0 / Discovery 1.0（openid.net/specs）与 Duende IdentityServer 演示站（demo.duendesoftware.com）：授权码 + PKCE S256 + state/nonce、服务端换码、JWKS 验 RS256、userinfo 补 profile。Duende 实测的两条教训直接进了实现：① profile claim 不在 ID token 里、只在 userinfo 里，换码后必须调 userinfo 合并；② 演示 client 登记的 redirect URI 是示例地址，验证时用登记地址走完换码、再用本仓库代码验 token。
- 实现：新包 `internal/oidc`（纯标准库：discovery、授权码 + PKCE、RS256 验签、userinfo 合并；state/nonce 单次有效 10 分钟过期）；`store.User` 增加 `oidc_subject` 与 `auth_source`；`auth.LoginOIDC`（JIT 开户、每次登录刷新姓名/部门）+ `auth.CreateSession`；`GET /auth/oidc/login`、`GET /auth/oidc/callback`、`GET /api/auth/providers`、`GET /api/auth/oidc/status`（admin）；管理台登录页在 OIDC 启用时显示 SSO 按钮（中英双语）；环境变量 `AGENTGATE_OIDC_*`，缺一个即不开启，本地登录始终兜底。LDAP 本轮只做配置与设计：新包 `internal/ldap`（`Config` + `Validate`，`Implemented()` 为 false）与 docs/ldap.md（流程、安全约定、实现轮次验收清单），无测试目录、未实现客户端、未实测。
- 关键决策：IdP 只认身份、不认权限——role claim 即便给了也不采信，JIT 新用户永远是 employee，提权必须本地管理员操作；账号不串户（OIDC 用户名撞本地账号自动加后缀，OIDC 账号无本地密码哈希）；只认 RS256（`alg: none` 与 HS256 直接拒绝）；回调失败只给通用文案，code/state 不进审计；不引入 oauth2/oidc 依赖，登录路径纯标准库。
- 测试：oidc 包 5 项 + auth 包 4 项（JIT 开户/撞名加后缀/禁用拒绝/用户名清洗）+ API 端点 4 项（假 IdP 下 HTTP 全程 login→IdP→callback→会话页→/api/me）+ ldap 包 2 项；另用真实 Duende 演示 IdP（alice/alice）跑通登录闭环并以本仓库 `VerifyIDToken` 对线上 JWKS 验签通过（临时测试，验证后删除、未进仓库）；`gofmt`/`go vet`/`go test ./...` 全绿；`make smoke` 81 项断言 0 失败（新增 5 项 F9 断言）。
- 推送：按分支 + PR 机制走 `loop/f9` 分支，见本轮汇报。
- 推送结果：整包快照推送一次成功（push-files，18 个文件一次提交 06cf28d6，逐文件哈希核对 18/18 一致，未触发 contents API 回退）；PR #5 文件清单与本轮一致，已合并进远端 main，远端 HEAD 0032b40a（合并提交），读回验证 oidc.go 与 oidc-sso.md 已在 main 上。

## 2026-11-21 · F8 Prompt 模板库
- 本轮按常规规程选条目：上一轮特别指令（F17 + 蓝图修订稿）已在 F17 轮执行完毕、执行后作废，本轮起忽略；另核实主对话的许可证变更已于本轮前经 PR #3 合并进远端 main（3556114，5 个文件逐个哈希核对与本地 00c39b5 一致），无在途分支需续传。P0 全部 done，P1 按清单顺序取 F8。
- 借鉴 ContextForge Prompt 注册中心（Jinja2 模板 + JSON-Schema 参数校验 + 版本管理与回滚）与 Portkey Prompt 版本语义（更新只产生新版本、不推向生产；发布才标定默认生产版本；版本不可变；默认走已发布版、可按版本号或 `@latest` 引用；历史版本可恢复/发布）。我们照此实现，但做三处治理收紧：① 版本选择只服务已发布版本——`@latest` 即已发布版，未发布版本 Agent 永远拿不到；② 参数校验默认严格——未知参数、必填缺失、非字符串值一律拒绝（与 F7 的 schema 即契约一致）；③ 渲染走与工具调用相同的门禁：参数先过 DLP、渲染结果再过 DLP + 自带级脱敏，模板按部门范围与最低等级下发。
- 实现：`store.PromptTemplate` + `PromptVersion`（不可变版本、`{{name}}` 占位符 + 声明参数）+ `RenderPrompt`（严格替换，只报字段名不报值）+ `ValidateTemplateVersion`（占位符须已声明、内容 ≤32 KiB、参数 ≤20 个）；`/mcp` 新增 `prompts/list` 与 `prompts/get`（默认/显式版本选择、数字版本须为已发布版、参数与渲染结果双重 DLP、审计 `prompt_get`，草稿存在性不泄漏）；管理 API `GET/POST /api/prompts`、`GET /api/prompts/{id}`、`POST /api/prompts/{id}/versions`、`POST /api/prompts/{id}/review`、`POST /api/prompts/{id}/publish`（发布/回滚到指定版本）、`DELETE /api/prompts/{id}`；管理台新增 Prompt 模板卡片（中英双语）；演示种子 `weekly_report`（星河精密周报助手，已发布 v1）。
- 关键决策：新版本永不自动发布——加版后已发布版继续服务，直到管理员显式发布（Portkey 的更新≠发布语义）；审核批准仅在首次发布时定已发布版，已发布模板的后续版本一律走 publish 端点，避免审核动作悄悄改生产；拒绝已发布模板会撤下服务（prompts/get 要求 status 为 published），与技能的 rejected 语义一致。
- 测试：store 5 项 + API 端点 1 项（提交→加版→审核发布→回滚→删除全链路）+ MCP 集成 7 项 + 冒烟新增 16 项断言；`gofmt`/`go vet`/`go test ./...` 全绿；`make smoke` 76 项断言 0 失败。
- 推送：按分支 + PR 机制走 `loop/f8` 分支，见本轮汇报。
- 推送结果：整包快照推送一次成功（push-files，14 个文件一次提交 560d8777，逐文件哈希核对 14/14 一致，未触发 contents API 回退）；PR #4 文件清单与本轮一致，已合并进远端 main，远端 HEAD 47b2b190（合并提交），读回验证 prompts.go 与 mcp.go 已在 main 上。

## 2026-11-21 · 许可证变更：PolyForm Noncommercial → GNU AGPL-3.0
- Nathan 2026-11-21 15:43 决定：项目目标是普及而非盈利，许可证由 PolyForm Noncommercial 1.0.0（禁止商用）改为 GNU AGPL-3.0——任何人可免费使用（含商业使用）；修改后分发或以网络服务形式提供时，须以 AGPL-3.0 公开对应源代码，防止闭源转卖。此决定取代同日早先「禁止商用」的指示。
- 变更内容：LICENSE 替换为 AGPL-3.0 标准全文（取自 SPDX license-list-data 的 AGPL-3.0-only 文本，34,020 字符，结构校验通过）；README 许可证一节、docs/blueprint.md 许可证决策记录与仓库可见性条目、docs/development-loop.md 红线表述同步改为 AGPL 口径。
- 本条为主对话人工变更（非 Loop 轮次），走 loop/license-agpl 分支 + PR 合入 main；同时首次实测整包快照推送（push-files）机制。

## 2026-11-21 · F17 渠道信任类型与分级策略梯度
- 特别指令已执行（Nathan 2026-11-21 12:26 决定，本轮执行后作废）：先核实 F6+F7 已并入远端 main——
  PR #1 已合并（远端 main HEAD 342c56b，2026-11-21 15:13 前后经 list_commits / list_pull_requests 核实），
  无在途分支需续传；本轮条目锁定 F17，并一并落地蓝图修订稿 8 处替换。下一轮起忽略该特别指令，按常规规程选条目。
- 借鉴 Cloudflare AI Gateway 官方文档与 2026 年七家网关对比：Cloudflare 只做托管式代理、没有自托管或
  VPC 内部署选项（受数据驻留约束的团队最先撞上的墙）；数据驻留只在网关层可配，最终推理位置由模型厂商决定。
  我们反过来做：不把部署形态锁死在某一家厂商——信任类型就是部署形态本身（签约云 / 专有云-VPC / 本地小模型），
  每个渠道自带信任类型，路由按密级选信任梯度；机密默认脱敏后上云是纯托管网关给不了的保证（它们只转发、不替你脱敏）。
- 实现：`store.TrustType`（signed_cloud / dedicated_cloud / local_model，空值归一为签约云以兼容旧快照）+
  `Channel.TrustType` + `TrustTypeOrDefault()`；新文件 `internal/gateway/trust.go`（梯度判定与
  `no_compliant_channel` 提示文案）；网关 `pickCandidates` 与 `channelsCoolingDown` 按信任过滤，机密→签约云时
  即使等级脱敏配置关闭也强制脱敏（审计 Detail 记 `desensitized=signed_cloud`，decision 仍为既有约定的
  `reroute`），无合规通道时 403 `no_compliant_channel` 并写 `violation` 审计事件；
  `GET/POST /api/channels` 返回与接受 `trust_type`（非法值 400、缺省签约云）；管理台渠道表新增信任类型列（中英双语）；
  seed 增加脱敏云渠道（机密默认）与本地模型渠道（绝密），私有渠道标为专有云/VPC。
- 关键决策：脱敏是信任类型的结构属性、不是等级配置的附属——等级脱敏关掉也不影响机密上云必脱敏；
  绝密只认本地小模型，专有云/VPC 也不行（密级够不等于信任够），错误码与通用 `no_channel_for_data_level` 区分开，
  让运维一眼看到缺的是哪种通道；旧快照渠道无信任类型字段时按签约云处理（此前默认即公有云行为，不改变既有部署语义）。
  故障转移链仍沿用 F1 规则（只在首选同一 MaxDataLevel 内转移），脱敏云故障时不自动升级到专有云/本地模型——
  跨信任梯度的合规故障转移留待后续轮次评估。
- 蓝图修订稿 8 处替换已落地（修订稿存 goal hidden_files/blueprint-revision-draft-2026-11-21.md）：防护设计 §2 分级路由表
  （机密默认脱敏后上云、绝密本地小模型或明确拦截）与 §5 MVP 管线；蓝图架构图、§6 交叉规则、§9.1 MVP 清单、
  §4.5 知识库条目补 F18 说明、新增 §6.1「目标客户与落地方式」（零预置假设/默认保守模式/先观察→再提醒→后拦截/
  双轨可插拔）；README 架构图。另同步 README 网关特性行与演示段落（受管级机密默认脱敏后上云、绝密走本地模型），
  并把 Loop 规程 `docs/development-loop.md` 第 6 步更新为整包快照推送优先（push-files 一次推全部文件，
  403 才回退 contents API）与每小时一轮的节奏。
- 测试：store 3 项 + 网关信任测试 5 项（梯度路由与上游实收报文验证、等级脱敏关闭时仍强制脱敏、机密走专有云原文、
  无本地模型时绝密 403 `no_compliant_channel` 及审计原因、仅专有云时绝密仍为 `no_compliant_channel`）+
  API 渠道测试 1 项 + 冒烟新增 7 项断言；更新旧测试 `TestManagedConfidentialRoutedToPrivate`（机密默认改走脱敏云）
  与 `TestConfidentialNeverFallsBackToLowerLevel`（脱敏云故障时不降级到普通云、也不升级到专有云/本地模型）；
  `gofmt`/`go vet`/`go test ./...` 全绿；`make smoke` 60 项断言 0 失败。
- 推送：按分支 + PR 机制走 `loop/f17` 分支，见本轮汇报。
- 推送结果：整包快照推送一次成功（push-files，18 个文件一次提交 fc421227，逐文件哈希核对 18/18 一致，
  未触发 contents API 回退——此前 F4 轮次 403 的快照路径本次在非空仓库上首次走通）；PR #2 文件清单与本轮一致，
  已合并进远端 main，远端 HEAD 08676f3f（合并提交），读回验证 trust.go 与蓝图 §6.1 已在 main 上。
- 另记：Nathan 2026-11-21 15:14 定下防护范围边界——AgentGate 只防「电脑内」的数字数据流，员工拍照、默记、口述等
  人的行为不在防护范围内（是边界、不是弱点，明说归客户保密制度）；产品弱点要明说不遮掩，靠管理手段兜底的弱点须把
  管理措施讲清楚；「绕过场景与对策」先不放进防护设计文档，先在讨论中对齐弱点与策略再说。本轮按此指示未写入文档。

## 2026-11-21 · F7 REST → MCP 适配
- 借鉴 ContextForge 官方 README 与文档（ibm.github.io/mcp-context-forge）及 mcp-rest-forge 配置模型：
  REST 工具逐端点 JSON 注册（name/url/request_type/input_schema），凭据 header 注入；
  v1.0.5 起可从 OpenAPI 生成单个端点的 schema，但整份 spec 导入已明确 not planned；
  mcp-rest-forge 同源：method/path 模板（{{param}} 转义替换）/query_params/body/headers。
  我们只取逐端点注册语义，不做 OpenAPI 整份导入；差异化收紧：每个工具只配最高密级与
  最低等级两把闸（与 F6 同款），参数 schema 注册时即校验，上游 5xx 只报状态码不回传 body。
- 实现：新包 `internal/restadapter`（路径模板转义替换、其余参数 GET/DELETE 走 query、
  POST/PUT/PATCH 走 JSON body、JSON Schema 子集校验、上游错误只报状态码、1 MiB 响应上限）；
  `store.RESTTool`（方法/基址/路径模板/静态头/加密托管凭据/输入 schema/最高密级/最低等级）；
  `/mcp` tools/list 合并 `rest.<name>`（高于调用者等级的隐藏）、tools/call 走与联邦工具
  相同的门禁与结果 DLP + 自带级脱敏，参数校验失败报 -32602；`GET/POST /api/mcp/rest-tools` +
  `DELETE /api/mcp/rest-tools/{id}` + `POST /api/mcp/rest-tools/{id}/test`（仅 admin/security，
  测试端点返回 ok + 200 字内 preview，不写入审计）；管理台新增 REST 工具卡片（中英双语）；
  演示：mock 增加 `/mock/demo/hr/leave-balance`（星河精密虚拟 HR 接口），seed 注册
  `rest.leave_balance`。
- 关键决策：schema 校验默认严格——未知参数直接拒绝（schema 即契约，避免未声明参数被
  静默转发到上游）；凭据只在调用瞬间解密注入 Authorization 头；测试端点的 preview 只给
  管理员调用方、不进审计（审计只记工具名与成败）。
- 测试：restadapter 包 6 项 + MCP 集成 8 项 + API 端点 2 项（CRUD 与实时测试）+ 冒烟新增 16 项断言；
  `gofmt`/`go vet`/`go test ./...` 全绿；`make smoke` 53 项断言 0 失败。
- 推送：按分支 + PR 机制走 `loop/f7` 分支，见本轮汇报。
- 交接：Nathan 于 2026-11-21 12:26 拍板，**下一轮先做 F17（通道信任分级，跳过清单顺序在前的
  F7——本轮在拍板前 6 分钟已开工故不打断）**，且蓝图修订稿（8 处替换，存
  `~/workspace/goals/agentgate-enterprise-agent-platform/hidden_files/blueprint-revision-draft-2026-11-21.md`）
  一并落地到仓库 `docs/blueprint.md`、`docs/enterprise-protection-design.md`、`README.md`；
  该决定已写为 Loop 的「下一轮特别指令」，执行后作废。F17 顺延为紧接着的一轮。

## 2026-11-21 · F6 MCP 联邦代理
- 借鉴 IBM ContextForge 官方文档（ibm.github.io/mcp-context-forge/architecture）与 PyPI 说明：
  多后端联邦到单一合规端点、注册中心合并能力目录、虚拟服务器组合工具子集、定期健康检查与故障转移、
  工具输入校验与重试/限流、PII 过滤插件。我们只取一层代理语义：命名空间前缀、60 秒工具列表缓存、
  上游只配最高密级与最低等级两把闸；不出对等网关发现/mDNS 与 Redis 同步；DLP 由可选插件改为强制门禁。
- 实现：新包 `internal/federation`（JSON-RPC 2.0 over HTTP 转发、工具列表缓存 + 刷新失败回落旧缓存、
  上游错误不透传原文）；`store.UpstreamServer`（命名空间/URL/加密凭据/最高密级/最低等级）；
  `/mcp` tools/list 合并本地与联邦工具（高于调用者等级的上游对该调用者隐藏，不可达上游静默缺席）、
  tools/call 对命名空间工具先做等级授权、再对参数做 DLP（违禁词或密级高于上游上限直接拦截），
  结果复用本地工具的 DLP 扫描与自带级脱敏；`GET/POST /api/mcp/upstreams` +
  `DELETE /api/mcp/upstreams/{id}` + `POST /api/mcp/upstreams/{id}/test`（仅 admin/security，
  命名空间校验、重名 409、凭据永不回显）；管理台新增联邦上游卡片（中英双语）。
- 关键决策：tools/list 按调用者等级过滤（最小暴露存在性）；参数密级门禁在转发前做——
  机密参数永不发往仅 internal 的上游（分级约束高于可用性）；上游故障时列表静默缺席、调用报 -32002，
  不让单个上游拖垮整个 /mcp；凭据只在转发瞬间解密、永不进审计与 API 响应。
- 测试：federation 包 4 项 + MCP 集成 6 项 + API 端点 2 项（CRUD 与实时测试）+ 冒烟新增 12 项断言；
  `gofmt`/`go vet`/`go test ./...` 全绿；`make smoke` 37 项断言 0 失败。
- 推送：按 Nathan 指示的新机制（分支 + PR，main 只收完整成品）走 `loop/f6` 分支，见本轮汇报；
  另按任务书把该机制同步进了 `docs/development-loop.md` 第 6 步。
- 另记：Nathan 本轮新决策已记入 backlog 待办——F17 渠道信任类型与分级策略梯度（分级放行改绑通道信任类型，
  私有模型降为可选项；默认保守模式 + 先观察→再提醒→后拦截）、F18 文件夹即知识库（零预置假设，
  指定共享文件夹即自动索引）。

## 2026-11-21 · F5 Webhook 告警
- 本轮先续推 F4：重跑 `/tmp/push_f4b.py`，剩余 8 个文件经校验已与远端一致（主对话并行推完），
  26/26 文件全部同步，远端 HEAD d761e80、内容与本地 b8907a8 相同，CI 第 51 次运行全绿——
  自 F2 起困扰三轮的推送阻塞正式解除，远端首次完整绿灯。随后按 Loop 规程取下一条 P0 做 F5。
- 借鉴企业微信官方群机器人文档（developer.work.weixin.qq.com/document/path/91770）与钉钉自定义机器人
  安全设置说明：企微 markdown 上限 4096 字节、仅支持有限语法子集、每机器人 20 条/分钟；钉钉同样
  markdown（title + text）、20 条/分钟、超限停 10 分钟；三家加签同一公式
  `timestamp(毫秒) + "\n" + secret` 经 HMAC-SHA256、Base64、URL 编码后拼进 `timestamp` 与 `sign` 查询参数。
  签名公式与通用 HMAC 请求头均以 Python 独立算出测试向量、写入 Go 测试硬编码校验。
- 实现：新包 `internal/alert`（事件分类 + 四平台载荷 + 签名 + 异步投递器，队列满丢弃计数，
  失败 1 + 3 次重试后放弃，错误只记 `http_500`/`network_error`/`timeout`/`config_error` 类别，
  绝不把可能含 bot key 的 URL 写进审计或 API 响应）；`audit.Log.SetNotifyHook`（锁外回调，
  `alert` 类事件不触发，避免自循环）；`store.Webhook`（签名密钥 AES-GCM 加密后持久化，
  与 `Channel.APIKeyEnc` 的 `json:"-"` 不同——Webhook 是运行时由管理员配置的，丢密钥等于丢配置）；
  `GET/POST /api/alerts/webhooks` + `DELETE /api/alerts/webhooks/{id}` + `POST /api/alerts/webhooks/{id}/test`
  （仅 admin/security，列表 URL 的 query 值打码、密钥永不回显）；登录锁定在 `auth` 层以哨兵错误
  `ErrAccountLocked` 区分，API 记 `account_locked` 审计事件（可告警），普通失败仍为 `login_failed`（不告警）；
  管理台新增告警 Webhook 卡片（列表/添加/测试/删除，中英双语）。
- 关键决策：告警只取审计事件已有字段（处置类别如 banned_word、操作者、密级、渠道），审计 Detail
  本就不含匹配原文，故告警天然脱敏；限流（rate_limit）不做告警——量大且属软性控制，告警会淹没真实安全信号；
  投递是异步的（worker + 64 缓冲队列），慢/死的 Webhook 永不阻塞请求路径；已知注意点：
  `Channel.APIKeyEnc` 带 `json:"-"`、非演示模式重启后渠道密钥会丢失（demo 每次重建故未暴露），与本次
  Webhook 密钥持久化形成反差，留待后续轮次修（不属于 F5 范围）。
- 测试：alert 包 13 项 + API 端点 3 项（鉴权/校验/密钥加密存储与不回显/URL 打码/测试端点成功与 502 + 4 次尝试/
  锁定审计为 account_locked）+ 网关集成 1 项（违禁词拦截→Webhook 收到结构化告警、无敏感原文）；
  `gofmt`/`go vet`/`go test ./...` 全绿；`make smoke` 25 项断言 0 失败（新增 5 项 F5 断言）。
- 推送：已全部上远端（17/17 文件逐个哈希核对一致，远端 HEAD b9f045c；另补推 1 个注释修正
  4efaf18，远端 HEAD 现为 8f45854）。Nathan 在线批卡，推送阻塞解除后第二轮全绿。

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
