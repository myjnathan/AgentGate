# 交付体验改造 P1（F35）

F34（P0）让新装的管理员 30 分钟内跑起来；本轮（P1）收尾三件让日常
管理变轻的事：导航渐进披露、渠道厂商预设库、DLP 词表管理台页。
三件都不改任何分级 / 脱敏语义——只改到达功能所需的步数。

## 导航渐进披露

管理台原有 23 页全量平铺（见调研报告 §2.1 的批评）。现在默认只
显示 8 页，其余收进导航底部的「高级设置」折叠区（默认收起，点开
后状态记在浏览器里，下次打开仍是展开的）：

默认 8 页：开通向导、仪表盘、模型渠道、令牌、审计、用量统计、
文件夹知识库、设备接入。其余 16 页（报表、证明、终端、例外、
组织同步、用户与角色、语义复核、智能路由、技能、Prompt 模板、
知识包、联邦上游、REST 工具、告警、安装包、DLP 词表）都在高级设置里。

- 角色过滤先行：某页对当前角色不可见时，它既不在默认区、也不在
  高级区；若高级区一页都不可见，整个区不显示。
- 正在看高级页时，区自动展开；从高级页切走再回来，仍按浏览器记
  住的状态来。
- 服务端权限不变：导航只是不展示，接口仍按角色守卫（F26）。

## 渠道厂商预设库

`GET /api/channels/presets` 返回内置厂商预设（与向导第 3 步共用
同一份服务端列表，取代向导原来硬编码在 JS 里的 4 厂商）：

| 厂商 | Base URL | 模型 | 信任类型 | 最高等级 |
|---|---|---|---|---|
| DeepSeek | api.deepseek.com/v1 | deepseek-chat, deepseek-reasoner | 签约云 | internal |
| 通义千问 | dashscope.aliyuncs.com/compatible-mode/v1 | qwen-plus, qwen-turbo | 签约云 | internal |
| 智谱 GLM | open.bigmodel.cn/api/paas/v4 | glm-4-plus, glm-4-flash | 签约云 | internal |
| OpenAI | api.openai.com/v1 | gpt-4o, gpt-4o-mini | 签约云 | internal |
| Anthropic Claude | api.anthropic.com/v1 | claude-sonnet-4-5, claude-haiku-4-5 | 签约云 | internal |
| Moonshot Kimi | api.moonshot.cn/v1 | kimi-k2, moonshot-v1-8k | 签约云 | internal |
| 火山方舟 | ark.cn-beijing.volces.com/api/v3 | doubao-seed-1-6 | 签约云 | internal |
| Ollama 本地模型 | localhost:11434/v1 | llama3.1, qwen2.5 | 本地模型 | secret |
| 自定义 | — | — | 签约云 | internal |

选厂商后自动填 Base URL、模型、信任类型与最高等级，管理员只填
API Key；每个字段仍可改。预设是保守默认值：签约云厂商默认最高
internal（机密要走脱敏后上云，见 F17，不替你默认放宽）；本地
模型是唯一默认可到 secret 的类型。火山方舟的模型要填推理接入点
ID（不是模型原名），Ollama 的 Base URL 是本机回环地址——两者
都在预设备注里写明，建渠道时显示在表单下方。

实现：新包 `internal/channelpreset`（纯数据 + 校验，无新依赖），
`GET /api/channels/presets` 权限与渠道管理相同（admin/security/
operator 可读，员工 403）。预设值是各厂商公开文档里的端点事实，
不是复制的代码；新厂商只需在该包加一行。

## DLP 词表管理台页

新页「DLP 词表」（高级设置 → 模型网关组），替代「改环境变量加
重启」：两张表——违禁词（命中即拦截）与企业敏感词（命中即按机
密级处理）——增删立即生效，下次调用就按新词表执行。

- `GET /api/dlp/words` 返回两张生效表，每个词带来源：`env`（启动
  环境变量 / 演示种子）、`template`（行业模板带入）、`manual`（本
  页添加）。
- `POST /api/dlp/words`（`{list, action, word}`）增删一个词：词
  1–128 字、无控制字符；重复添加是幂等的成功；删不存在的词 400。
- 持久化：词表存在 `store.State.DLP` 里。第一次在本页改某张表时，
  先以网关当前生效表初始化该表（环境变量词与模板词都保留），再
  改动；此后该表以本页为准、重启后不变，环境变量只剩全新安装的
  默认值。模板在词表已托管后应用时，模板词也会并进托管表（否则
  下次重启会丢）。
- 审计：每次改动记 `dlp_word_changed`，只记表名、动作与改后数量，
  **不记词本身**——词可能是项目代号，属于敏感原文。
- 生效范围：网关检测器每请求重建，改动对模型网关调用即时生效，
  sidecar 下次拉策略快照时同步拿到；MCP 工具侧的检测器仍在启动
  时建成一次（F34 已知限制），词表改动需重启才对 MCP 生效。
- 权限：读写均为 admin/security（`roles.Policy`，与模板/上线阶段
  同级），员工与审计员不可见。

## 已知注意点

- 导航默认 8 页是固定集合，不是按人定制：不记住「某人常去哪页」
  的偏好（那需要服务端存每用户的导航偏好，本轮不做）。
- 预设模型名会随厂商发布而变旧：预设只是起点，填错模型时上游
  会在调用时报 401/404（网关不预校验模型名，与既有建渠道行为一致）。
