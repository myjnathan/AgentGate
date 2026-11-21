# LDAP 目录登录（设计稿，F9）

> 状态：**设计 + 配置预留，未实现、未实测**。本轮没有 LDAP 测试目录
> （无 AD / OpenLDAP 环境），所以只落地了配置结构
> （`internal/ldap` 的 `Config` + `ConfigFromEnv` + `Validate`）与本
> 文档；bind/search 客户端尚未写，`ldap.Implemented()` 返回 false。
> 启用 LDAP 之前必须先有测试目录并跑通下面的验收清单。

## 目标

用企业已有的目录（Active Directory / OpenLDAP）登录管理台，与 OIDC
并列为第二种企业登录方式。本地账号始终保留为兜底。

## 配置

```bash
AGENTGATE_LDAP_URL=ldaps://ad.example.corp:636
AGENTGATE_LDAP_BIND_DN=cn=agentgate,ou=services,dc=example,dc=corp
AGENTGATE_LDAP_BIND_PASSWORD=...
AGENTGATE_LDAP_BASE_DN=ou=users,dc=example,dc=corp
# 可选：
AGENTGATE_LDAP_USER_FILTER=(sAMAccountName=%s)   # AD；OpenLDAP 常用 (uid=%s)，默认 (uid=%s)
AGENTGATE_LDAP_DEPARTMENT_ATTR=department         # 映射到 AgentGate 部门的属性，默认 department
AGENTGATE_LDAP_STARTTLS=1                         # 仅 ldap:// 时可用，ldaps:// 禁止
```

`Config.Validate()` 的校验规则：URL 必须是 `ldap://` 或 `ldaps://`
且带 host；`ldaps://` 不得配 StartTLS；bind DN / 密码 / base DN 必填；
user filter 必须含 `%s` 且为括号括起的过滤器。

## 登录流程（待实现）

1. 用户在管理台输入用户名与密码（与本地登录同一表单，失败文案一致，
   不透露账号是否存在）。
2. 服务端用 BindDN + BindPassword 做一次服务账号绑定（连接可复用，
   但**绝不把用户密码记日志、记审计**）。
3. 按 `UserFilter` 在 `BaseDN` 下搜用户条目（只取一条；多条即拒绝，
   防止歧义）；取条目的 DN 与部门属性。
4. 用该用户 DN + 用户输入的密码再做一次绑定——成功即身份成立
   （密码只在内存里走这一次，不存储、不做哈希比对）。
5. 按 DN 找本地账号（新增 `ldap_dn` 字段，类比 OIDC 的 `oidc_subject`），
   没有则 JIT 开户：用户名取登录名、部门取目录属性、角色一律 employee
   （与 OIDC 相同的保守默认：目录只证明身份，不授权限）。
6. 发 12 小时会话；成功记 `ldap_login`、失败记 `ldap_login_failed`
   审计事件（不含密码、不含 DN 细节以外的信息）。

## 安全约定

- 只走 `ldaps://` 或 `ldap://` + StartTLS；明文 LDAP 不接受
  （内网也不例外——密码在里面跑）。
- 服务账号密码只放环境变量，不进仓库、不进 API 响应。
- 用户密码永不落盘：不存哈希（目录就是事实来源），失败重试走与
  本地登录相同的锁定策略（20 次失败锁 5 分钟，按账号计）。
- 部门属性只取白名单字段；role/group claim 不采信（与 OIDC 同款收紧）。
- 目录不可达时登录失败、服务本身不受影响（本地登录仍可用）。

## 验收清单（实现轮次做）

- [ ] 起一个 OpenLDAP 测试容器（或对接客户测试 AD），配好测试用户与
      部门属性，跑通「服务绑定 → 搜索 → 用户绑定」全程。
- [ ] 用户名不存在、密码错误、多条目、目录超时四种失败均返回通用文案，
      且不把目录原文带回前端。
- [ ] JIT 开户：首次登录建 employee 账号，部门正确映射；二次登录不重复建号。
- [ ] 禁用的 LDAP 账号拒绝登录；本地同名账号不被 LDAP 身份接管。
- [ ] `ldap://` 明文配置被启动校验拒绝；`ldaps://` + StartTLS 被拒绝。
- [ ] 冒烟测试加 LDAP 禁用路径断言（无目录时登录端点 503，与 OIDC 同款）。
