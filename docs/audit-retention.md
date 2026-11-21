# 审计导出与合规归档（F4）

审计日志是哈希链 NDJSON 文件（数据目录下 `audit.jsonl`），事件本身
不含请求原文与敏感值，只有种类、动作、元数据与处置结果。本页说明
如何把它导出给审计方、如何归档过期事件、以及保留期如何配置。

## 导出：`GET /api/audit/export`

- 权限：仅 admin / security（员工与匿名分别返回 403 / 401）。
- 查询参数：`since` / `until`（RFC3339，半开区间 `[since, until)`，
  与 `/api/stats` 一致）、`kind`、`actor`。都不传即导出全部。
- 响应：`Content-Type: application/x-ndjson`，每行一个事件 JSON，
  按时间从旧到新；`Content-Disposition: attachment`，文件名形如
  `agentgate-audit-20261121-093000.ndjson`。
- 校验信息在响应头里，不在正文里（正文保持纯 NDJSON，可直接喂
  给 `jq` / SIEM / BigQuery）：
  - `X-Audit-Chain-Valid`：导出时刻整条链是否验真（`true` / `false`）
  - `X-Audit-Event-Count`：导出事件数
  - `X-Audit-First-Seq` / `X-Audit-Last-Seq`：导出事件的序号范围
  - `X-Audit-Head-Hash`：最后一个导出事件的哈希
- 每次导出本身也会记一条审计事件（`admin` / `audit_exported`，
  只有范围与数量，无内容），且写在响应体之后，不会混进本次导出。

示例：

```bash
curl -H "Authorization: Bearer $ADMIN_SESSION" \
  "http://127.0.0.1:8787/api/audit/export?since=2026-11-01T00:00:00Z&until=2026-12-01T00:00:00Z" \
  -o agentgate-audit-2026-11.ndjson -D headers.txt
# 校验：行数 == X-Audit-Event-Count，jq -s 拼成数组后逐行验链
```

## 归档：`POST /api/audit/archive`

```json
{"before": "2026-05-25T00:00:00Z"}
```

把 `before` 之前的所有事件移出热日志，写成数据目录下的两个文件：

- `audit-archive-<firstSeq>-<lastSeq>.jsonl.gz`：被归档事件原文的
  gzip 压缩 NDJSON（与热日志完全相同的行格式）
- `audit-archive-<firstSeq>-<lastSeq>.manifest.json`：归档清单
  （起止序号/时间、事件数、末事件哈希 `head_hash`、归档时刻的
  `chain_valid`、保留期天数）

热日志随后以一个检查点事件（`admin` / `audit_archive_checkpoint`）
开头，继续记录新事件。检查点记录了被归档段的末事件哈希，因此：

- 热日志的 `GET /api/audit/verify` 在归档后仍然返回 `chain_valid: true`；
- 归档过的旧事件不需要重算哈希——离线验真时把各归档文件解压后
  的事件与热日志事件拼接（跳过检查点行），整条链一次验完；
- 归档前会先验链：**链已破损时拒绝归档**（HTTP 409），防止把篡改
  痕迹悄悄剪掉。

归档本身也记一条审计事件（`admin` / `audit_archived`，只有文件
名与数量）。`before` 之前没有任何事件时返回 `archived: 0`，不写
文件、不动热日志。

## 保留期配置

- 环境变量 `AGENTGATE_AUDIT_RETENTION_DAYS`，默认 **180 天**。
  非法值回落到 180 并在启动日志里提示；启动日志会打印生效值
  （`audit retention: 180 days`）。
- 这是一个**策略值**，不是定时器：服务本身不自动删日志。合规
  做法是定期（例如每月一次，用 cron 或运维脚本）调一次归档接口，
  `before` 取“现在 − 保留期天数”：

```bash
BEFORE=$(date -u -d "-180 days" +%Y-%m-%dT%H:%M:%SZ)
curl -X POST -H "Authorization: Bearer $ADMIN_SESSION" \
  -d "{\"before\": \"$BEFORE\"}" http://127.0.0.1:8787/api/audit/archive
```

- 归档文件建议随数据目录一起备份（备份 = 数据目录 + 主密钥，
  见 `deploy/README.md`）；归档文件本身不加密——事件不含敏感
  原文，但仍应按企业备份策略保管。

## 借鉴来源

- LiteLLM 官方审计日志文档（docs.litellm.ai/docs/proxy/audit_logs）：
  审计日志带合规方设定的保留策略，可异步批量导出到外部存储
  （S3，按日期分目录的 JSON 文件），且审计日志可用独立的合规
  存储桶。我们照此实现，但导出不走外部存储：本机 NDJSON + 响应
  头校验，离线可验真；保留策略以环境变量配置、手动/定时触发归档。
- Cloudflare Logpull 文档（developers.cloudflare.com/logs/logpull）：
  NDJSON 是日志导出的标准格式（每行一个有效 JSON 对象），按时间
  范围拉取、保留期到期后需用推送归档。我们照此实现：导出格式为
  NDJSON、时间范围半开区间、超保留期的事件进 gzip 归档而非删除。
