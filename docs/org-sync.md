# 组织架构同步与 IM 身份绑定（F21）

从钉钉 / 飞书 / 企业微信同步部门与员工目录，登录时把第三方身份绑定到
平台账号，与 F9 OIDC 并行。同步只建 / 更新账号与部门映射，不自动授予
受管级权限（权限仍走平台策略）。

## 平台与凭据

| 平台 | 应用类型 | 需要的凭据 | 备注 |
|---|---|---|---|
| 钉钉 | 企业内部应用 | CorpId + AppKey + AppSecret | 需开通通讯录「部门 / 成员读」权限，并把通讯录权限范围设为全部员工 |
| 飞书 | 企业自建应用 | AppID + App Secret | 需开通通讯录 `contact:contact:readonly` 等读权限 |
| 企业微信 | 自建应用（通讯录） | CorpID + CorpSecret | 需开通通讯录读权限 |

管理台「组织同步」页添加来源，或调 `POST /api/orgsync/providers`：

```json
{
  "provider": "dingtalk", "name": "星河钉钉",
  "corp_id": "dingXXXX", "app_key": "dingXXXX", "app_secret": "…",
  "auto_create": true, "disable_missing": false
}
```

- `app_secret` 只加密存储（主密钥 AES-GCM），列表与详情只返回打码值，永不回显。
- `base_url` 可空，默认官方地址；填了须为 http(s)（用于专属域名 / 测试假服务）。
- `auto_create` 默认 false：只映射已预先开通的账号；true 时目录成员无账号则新建员工账号。
- `disable_missing` 默认 false：见下。

## 同步规则（借鉴 dsh-ee 的钉钉 / 飞书组织同步）

1. **整份目录才生效**：拉取任何一页失败、或目录为空时整轮不生效，绝不拿半份目录更新或禁用账号。
2. **建号**：按外部主体 `dingtalk:<corpId>:<userId>` / `feishu:<appId>:<openId>` / `wecom:<corpId>:<userid>` 找账号；没有则按企业邮箱自动关联已有账号（邮箱必须精确匹配）；仍没有且 `auto_create` 开启时新建员工账号（角色永远是 `employee`，无本地密码，`auth_source` 为平台名）；未开启时记入 skipped。
3. **用户名**：邮箱 local-part 清洗为 `[a-z0-9._-]`，撞名时加主体哈希后缀；无邮箱时用第三方 ID。
4. **刷新**：每次同步刷新姓名 / 部门（部门名为 `A / B` 拼接）/ 企业邮箱（优先企业邮箱，飞书优先 `enterprise_email`）。
5. **本地已禁用不恢复**：同步跳过已禁用的账号，不自动恢复。
6. **离职判定**（仅 `disable_missing` 开启时）：目录里没看到不等于离职——逐个向平台确认，只有平台明确说找不到（钉钉 60121、飞书 41012、企微 60111）才处理；权限范围外（钉钉 50002/50004/60011、飞书 41050）或仍在职的记入 kept。平台自建的账号直接禁用并吊销全部会话；只是关联在别的来源账号上的身份只解除关联、账号归主身份的来源管。
7. **审计**：每轮写一条 `orgsync/org_sync` 汇总事件（只含计数：remote/created/reconciled/linked/disabled/kept/unlinked/conflicts/skipped），用户名列表只在同步结果（API 返回与来源记录的 `last_sync`）里，不进审计。绑定 / 解绑各记一条 `org_identity_bound` / `org_identity_unbound`。

## 身份绑定

- 同步时按邮箱自动关联（见上）；管理员也可手动绑定：`POST /api/orgsync/bindings`（`user_id` 可填用户名或 ID，`provider`，`external_id`）。被绑的第三方 ID 必须在某次已同步的目录快照里出现，否则 409；同一身份已绑到别的账号时 409。同一外部 ID 在多个来源出现时（如两家企业都有 u1），绑定按来源 ID 顺序选尚未被他人绑定的那个主体；全部被占时 409。
- 解绑：`DELETE /api/orgsync/bindings/{user_id}/{provider}`（`user_id` 同样可填用户名或 ID；只解除关联、不删账号）。
- 本人查看：`GET /api/orgsync/bindings`（当前会话用户）。

## API 一览

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| GET | /api/orgsync/providers | admin | 列表（密钥打码） |
| POST | /api/orgsync/providers | admin | 新建来源 |
| GET | /api/orgsync/providers/{id} | admin | 详情（含 last_sync） |
| DELETE | /api/orgsync/providers/{id} | admin | 删除来源（不删账号） |
| POST | /api/orgsync/providers/{id}/sync | admin | 手动完整同步一次 |
| POST | /api/orgsync/providers/{id}/test | admin | 连通性测试（只返回部门 / 成员数，不建号、不写审计） |
| POST | /api/orgsync/bindings | admin | 绑定身份 |
| DELETE | /api/orgsync/bindings/{user_id}/{provider} | admin | 解绑身份 |
| GET | /api/orgsync/bindings | 登录用户 | 本人已绑身份 |

## 已知限制

- 本轮只做手动同步（管理台按钮 / API），无定时自动同步；后续轮次可加每小时定时任务（dsh-ee 为每小时一次）。
- 飞书用 `open_id` 作外部 ID（每应用唯一）；换应用后 open_id 会变，需重新同步。
- 企业微信用根部门 `fetch_child=1` 一次拉全量成员；超大通讯录（上万成员）时单次响应较大，留待分页接入 `user/list_id` 后优化。
- 同步不处理「员工调岗后旧部门残留」之外的组织变更之外的任何权限：部门只影响展示与统计口径，能力授权仍以令牌等级与策略为准。
- IM（飞书 / 钉钉）机器人消息中继：Nathan 2026-11-22 决定暂不做——与受管级准入标准（能碰加密文件的 Agent 不开 IM 远程分发通道）冲突，不立项。
