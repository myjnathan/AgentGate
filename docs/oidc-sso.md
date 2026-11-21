# OIDC / SSO 登录（F9）

AgentGate 支持用企业已有的 OpenID Connect 身份提供方（IdP，如
Keycloak、Entra ID、Okta、自建 IdentityServer）登录管理台。本地账号
始终保留为兜底——IdP 挂了也能用本地管理员账号进系统。

## 开启

设置三个环境变量后重启服务（缺一个即不开启，不报错、只记日志）：

```bash
AGENTGATE_OIDC_ISSUER=https://login.example.corp        # IdP 的 issuer（发现文档所在地址）
AGENTGATE_OIDC_CLIENT_ID=agentgate                          # 在 IdP 注册的 client_id
AGENTGATE_OIDC_CLIENT_SECRET=...                            # client_secret（只走服务端换码，永不回显）
# 可选：
AGENTGATE_OIDC_REDIRECT_URL=https://gate.example.corp/auth/oidc/callback
AGENTGATE_OIDC_SCOPES="openid profile email"               # 默认同此值，必须含 openid
AGENTGATE_OIDC_DEPARTMENT_CLAIM=department                 # 部门取自哪个 claim，默认 department
```

在 IdP 侧把 `AGENTGATE_OIDC_REDIRECT_URL`（默认
`http://127.0.0.1:8787/auth/oidc/callback`）登记为该 client 的
redirect URI，并允许 `openid profile email` scope。

启动日志会打印 `OIDC enabled: issuer ...`；发现文档拉取失败时
打印 `OIDC disabled: ...` 并继续以本地登录运行。

## 登录流程

1. 管理台登录页出现「使用企业 SSO 登录」按钮（`GET /api/auth/providers`
   返回 `oidc_enabled` 时才显示）。
2. 点按钮跳到 IdP（`GET /auth/oidc/login`）：授权码流程 + PKCE（S256），
   state 与 nonce 服务端保存、一次有效、10 分钟过期。
3. IdP 回调 `GET /auth/oidc/callback`：服务端用 client_secret 换码、
   按 JWKS 校验 ID token 的 RS256 签名并核对 `iss` / `aud` / `exp` /
   `nonce`，再调 userinfo 补齐姓名与部门（有些 IdP 不把 profile
   claim 放进 ID token）。
4. 首次登录即时开户（JIT）：按 IdP `sub` 建账号，用户名取
   `preferred_username`，没有则取邮箱前缀，再没有则取 `oidc-<hash>`；
   部门取配置的 claim；**角色一律为 employee**——IdP 只证明身份，
   不授权限，提权必须由本地管理员操作。
5. 回调页把 12 小时会话 token 写进浏览器 localStorage 后回到管理台，
   与本地登录完全一致。

## 安全约定（保守默认）

- **IdP 只认身份、不认权限**：role claim 即便 IdP 给了也不采信；
  新用户永远是 employee，管理员在本地提权。
- **账号不串户**：OIDC 用户名撞上已有的本地账号时自动加后缀，
  绝不复用本地账号；OIDC 账号没有本地密码哈希，本地登录永远失败。
- **禁用即拒登**：被本地禁用的账号 OIDC 也登不进来。
- **失败不泄漏**：回调失败只显示通用文案，不回显 IdP 原文、code 与 state
  不进审计；成功记 `oidc_login`、失败记 `oidc_login_failed` 审计事件。
- **只认 RS256**：`alg: none` 与 HS256 的 ID token 直接拒绝；JWKS
  按 kid 取键、未知 kid 刷新一次再拒；时钟偏差容忍 5 分钟。
- **不引入新依赖**：OIDC 客户端是纯标准库实现（`internal/oidc`），
  不拉 oauth2 / oidc 库——登录是高敏路径，依赖越少越好审。

## 管理接口

- `GET /api/auth/providers`（无需登录）：登录页用，返回 `local_enabled` /
  `oidc_enabled` / `oidc_issuer`（不含 secret）。
- `GET /api/auth/oidc/status`（admin/security）：返回 issuer、client_id、
  redirect_url、scopes、department_claim 与发现的各端点地址（不含 secret）。

## 用公开测试 IdP 验证（Duende demo）

本轮已用 Duende 的公开演示 IdP 全程跑通（2026-11-21 实测）：

- IdP：`https://demo.duendesoftware.com`，client `interactive.confidential` /
  secret `secret`，测试用户 `alice/alice`（另有 `bob/bob`）。
- 步骤：authorize（code + PKCE）→ IdP 登录页提交 alice/alice → 回调拿到
  code → token 端点换码 → 得到 RS256 的 ID token（`iss` /
  `aud=interactive.confidential` / `nonce` 均与请求一致，`sub=1`），
  用本仓库 `internal/oidc` 的 `VerifyIDToken` 对线上 JWKS 校验通过。
- 注意：该演示 client 登记的 redirect URI 是它自己的示例地址
  （如 `https://localhost:5001/signin-oidc`），不是本服务的回调地址；
  拿它做端到端验证时，用登记的地址走完换码、再用本仓库代码验 token
  即可——服务自身的回调闭环由 Go 测试（假 IdP）覆盖。
- 演示 IdP 的 profile claim 不在 ID token 里、只在 userinfo 里，这正是
  实现里「换码后调 userinfo 合并」的原因；不要假设所有 IdP 都把
  name/email 放进 ID token。
- 演示 IdP 仅供验证流程，生产请接企业自己的 IdP。

## LDAP

LDAP 目录登录本轮只做了配置与设计（见 [docs/ldap.md](docs/ldap.md)），
未实现 bind/search 客户端、未做实测——本环境没有 LDAP 测试目录。
LDAP 启用前必须先有测试目录并跑通绑定与部门映射，见该文档的验收清单。
