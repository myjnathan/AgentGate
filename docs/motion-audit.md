# 动效审查清单（Motion Audit，F37）

管理台全部状态变化逐项审查：变化是否可理解、是否有来有去、交代得清不清楚。
动效只解释变化（来源 / 去向 / 关系），不做纯装饰。

## 动效语言（统一变量，全站复用，禁止各处自创曲线）

| 变量 | 值 | 用途 |
|---|---|---|
| `--motion-fast` | `120ms` | 按下反馈、悬停、颜色切换 |
| `--motion-base` | `180ms` | Toast、弹框、页面/步骤淡入 |
| `--motion-slow` | `280ms` | 登录卡片入场等单次较重入场 |
| `--motion-ease-out` | `cubic-bezier(.2,0,0,1)` | 出现类变化（由无到有） |
| `--motion-ease-standard` | `cubic-bezier(.4,0,.2,1)` | 状态切换类变化 |

规则：

- 只动 `transform` / `opacity`；颜色类过渡（`background-color` / `border-color` / `color`）只用于悬停与状态色切换，不触发布局。
- 不动 `width` / `height` / `top` / `left` 等触发布局的属性；不做纯装饰动画。
- 高频重复操作（按钮按下、Toast）用 `--motion-fast`，比低频变化更快。
- 动效进行中再次操作能自然接上：出现类用 `transition`（可打断、可反向），不用不可打断的 `animation` 做交互反馈；页面/步骤淡入用单次 `animation`（元素重建即重播，无交互需要打断它）。
- 尊重 `prefers-reduced-motion: reduce`：全部 `transition` / `animation` 时长压到 `0.01ms`、延迟清零，动效退化为瞬间切换，功能（Toast 自动消失、弹框开关）不受影响。
- 深 / 浅色：动效只改透明度与位移，不依赖颜色，两种主题均成立。

## 审查清单

状态：`done` 已落地 / `todo` 待做 / `不做` 明确不做（附理由）。

| 编号 | 状态变化 | 所在位置 | 判定与修法 | 状态 |
|---|---|---|---|---|
| M-001 | Toast 出现 / 消失 | 全站反馈 | 由 `display` 瞬切改为 `opacity` + `translateY(8px)` 淡入淡出，`--motion-base` + ease-out；自动消失 3.4 秒不变，点击关闭立即消失（可打断、可反向） | done（切片 1） |
| M-002 | 确认框 / 输入框出现 / 消失 | 删除确认、发布版本、REST 测试参数 | 遮罩 `opacity` 淡入淡出 + 卡片 `translateY(8px) scale(.98)` → 原位，`--motion-base` + ease-out；`display` 瞬切改为 `.open` 类切换，Esc / 遮罩 / 取消均可打断 | done（切片 1） |
| M-003 | 页面切换 | 24 页 `go()` / `showPage()` | 新页 `.page.active` 单次淡入（`opacity` + `translateY(4px)`，`--motion-base`），说明「从哪来、到哪去」；旧页直接隐藏，不做双向交叉淡入（会闪） | done（切片 1） |
| M-004 | 窄屏导航展开 / 收起 | `≤960px` 菜单按钮 | 展开时导航区单次淡入（复用 M-003 同一关键帧）；收起直接隐藏，不做高度动画（导航为多行 wrap 布局，高度动画会重排） | done（切片 1） |
| M-005 | 向导步骤切换 | 开通向导五步 `wizGo()` | 新步骤单次淡入（复用 M-003 同一关键帧）；步骤条 pill 切换只改颜色，无位移动效（步骤条本身即位置说明） | done（切片 1） |
| M-006 | 登录卡片入场 | 登录页加载 | 单次淡入上浮（`opacity` + `translateY(8px)`，`--motion-slow` + ease-out）；只在页面加载时跑一次 | done（切片 1） |
| M-007 | 按钮按下 / 悬停 | 全站 `.btn`、`.navItem`、`.quickCard` | 按下 `scale(.97)`（`--motion-fast`，transform 可打断、松开即回弹）；悬停只过渡 `background-color` / `border-color`；导航项与快捷卡片同款过渡，同一件事处处一样 | done（切片 1） |
| M-008 | KPI 数值跳动 | 仪表盘四组 KPI | 不做。数值本身即是信息，额外动效会分散注意；数字已用 `font-variant-numeric: tabular-nums`，刷新时不抖 | 不做 |
| M-009 | 统计条形填充宽度变化 | 仪表盘 / 用量统计 `bars()` | 不做。每次刷新重建 DOM（`innerHTML` 整体替换），同一元素不存在、transition 无从生效；改为 `scaleX` 也无收益。条形只在首次渲染时出现，无需生长动画 | 不做 |
| M-010 | 表格 / 列表内容刷新 | 各页 `innerHTML` 重渲染 | 不做。内容整体替换是数据刷新，不是位置变化；逐行淡入会拖慢扫读，且高频刷新（审计、终端）下会闪 | 不做 |
| M-011 | 行内错误文案出现 | 登录 `lerr`、各页 `.msg`、输入框 `inputErr` | 不做（本切片）。颜色已由 `.msg.err` 区分，文案即是说明；登录失败加抖动属装饰性强调，留待后续切片评估是否值得 | todo |
| M-012 | 开通自检卡片出现 | 仪表盘 `#onboardCard` | 与 M-003 同一淡入（元素 `display:none → block` 时重播）；卡片内阶段按钮切换只改文案与颜色 | done（切片 1） |
| M-013 | `<details>` 展开 / 收起 | 向导「高级」折叠区 | 不做。浏览器原生行为，无自定义动效可接；展开即见内容，无需解释 | 不做 |
| M-014 | 焦点框出现 | 全站 `:focus` 描边 | 不做。焦点是无障碍信号，必须瞬间出现、可感知；过渡焦点框会延迟键盘用户的定位 | 不做 |

## 切片记录

- 切片 1（2026-11-25）：立动效语言（5 个变量 + 1 个关键帧 + `prefers-reduced-motion`）并落地 M-001–M-007、M-012；M-008–M-010、M-013、M-014 明确不做并记理由。验证见 `~/workspace/goals/agentgate-enterprise-agent-platform/hidden_files/f37-motion/measure1/`（`verify-slice1.js` + `after/slice1.json`、溢出复测 `measure1-overflow/overflow.json`）。
- 下一切片候选：M-011（行内错误提示是否加轻量强调）、登录页深色模式（F36 遗留，属 F37 验收「深/浅色均成立」的一部分）、逐项复审已落地动效在窄屏与 EN 模式下的表现。
