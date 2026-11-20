# 部署与维护手册

部署形态参考 dsh-ee 的成熟方案（单镜像、一条命令安装、离线包、Makefile 统一入口），
并利用 AgentGate 自身的简化：**不需要外部数据库**——dsh-ee 必须运维 TiDB/OceanBase，
AgentGate 当前为单二进制 + 数据目录（state.json + 哈希链审计日志），运维面小得多。
（多节点/ SQL 存储在路线图上，接口已预留。）

## 三种安装方式

### 1. Docker Compose（推荐）
```bash
curl -fsSL https://raw.githubusercontent.com/myjnathan/AgentGate/main/deploy/install.sh | sh
```
脚本在 `./agentgate/` 生成 `.env`（含主密钥）并拉起服务；**再次执行同一脚本即升级**
（拉新镜像、重建容器，数据卷不动）。手动方式：`cp deploy/compose.yml .`、写好 `.env` 后
`docker compose up -d`。

### 2. 二进制 + systemd
```bash
make release   # 或从 Release 下载
sudo cp dist/agentgate-linux-amd64 /usr/local/bin/agentgate
sudo useradd -r -d /var/lib/agentgate agentgate || true
sudo mkdir -p /var/lib/agentgate /etc/agentgate
# /etc/agentgate/agentgate.env 至少包含：
#   AGENTGATE_MASTER_KEY=<至少16位随机串>
#   AGENTGATE_ADDR=0.0.0.0:8787
#   AGENTGATE_DATA=/var/lib/agentgate
sudo cp deploy/agentgate.service /etc/systemd/system/
sudo systemctl enable --now agentgate
```

### 3. 离线安装包（内网/涉密环境）
`make bundle` 产出 `dist/bundle/`（镜像 tar + compose + install.sh + VERSION），
拷入内网后执行 `./install.sh` 即可，全程不联网。

## 配置（环境变量）
| 变量 | 说明 |
|---|---|
| `AGENTGATE_MASTER_KEY` | **必填**，≥16 字符。加密上游模型 Key 的主密钥，丢失 = 已存 Key 全部不可解密 |
| `AGENTGATE_ADDR` | 监听地址，默认 `127.0.0.1:8787`；容器/systemd 部署用 `0.0.0.0:8787`，前面建议加企业反向代理做 TLS |
| `AGENTGATE_DATA` | 数据目录（state.json、audit.jsonl），默认 `./data` |
| `AGENTGATE_DEMO` | `1` = 演示模式（种子公司 + mock 上游），**生产禁用** |
| `AGENTGATE_BANNED_WORDS` / `AGENTGATE_CUSTOM_WORDS` | 逗号分隔的拦截词/机密词覆盖配置 |

## 备份与恢复
- 备份两样东西：**数据目录**（或 docker 卷 `agentgate-data`）+ **`.env` 里的主密钥**，缺一不可。
- 恢复：还原数据目录、配回同一主密钥、启动即可。审计日志为哈希链，恢复后可在管理台
  「审计校验」验证完整性，防篡改可证。

## 升级与回滚
- 升级：重跑 install.sh，或 `docker compose pull && docker compose up -d`；二进制方式替换文件后 `systemctl restart`。
- 回滚：镜像打版本标签部署（不要长期用 latest）；回滚即切回上一个标签/二进制。数据格式变更会在
  发布说明中标注，回滚前先备份数据目录。

## 日常维护
- 健康检查：`GET /healthz`（容器已内置 HEALTHCHECK）；日志即进程 stdout，`docker compose logs -f` / `journalctl -u agentgate -f`。
- 审计巡检：管理台定期点「审计校验」；拦截/越权事件在审计页按 decision 过滤复盘。
- 密钥轮换：上游 Key 在管理台渠道里更新即生效（加密存储）；主密钥轮换工具在路线图（当前需停机迁移，见 backlog）。
- 磁盘：审计日志为追加写，按保留期归档（见 backlog F4 自动化归档；当前可手动按月切分 audit.jsonl 并保留校验）。

## 与 dsh-ee 部署方案的对照
| 项 | dsh-ee | AgentGate |
|---|---|---|
| 镜像 | 单镜像（前端嵌入 Go 二进制） | 同左（借鉴） |
| 安装 | install.sh 在线/离线双模式 + 首次生成 .env | 同左（借鉴） |
| 数据库 | 必需 TiDB / OceanBase / 外部 MySQL | 无外部 DB（当前文件存储） |
| 升级 | 重跑 install.sh | 同左（借鉴） |
| 构建入口 | Makefile（build/release/image/bundle） | 同左（借鉴） |
