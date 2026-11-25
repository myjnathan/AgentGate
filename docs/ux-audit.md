# 全站体验审查清单（F36）

以 Nathan 2026-11-24 定的八条标尺逐页审查管理台（`internal/webui`）。
状态：`待修` / `已修复` / `待验证` / `观察`。高/中严重度清零即达 F36 验收。

## 审查方法与基线

- 演示模式 + 无头浏览器（puppeteer-core + chrome-headless-shell）逐页访问：
  24 页（wizard / dashboard / stats / audit / reports / device / tokens / attest /
  terminals / exceptions / orgsync / users / channels / dlpwords / semreview /
  autoroute / kb / skills / prompts / knowpack / upstreams / resttools / alerts /
  packages）+ 登录页；1440px 全页截图，390px 窄屏复测 dashboard / wizard / channels。
- 基线（2026-11-25 修复前，上一轮截图）：控制台/页面/请求错误 0；1440px 下 24 页
  全部无横向溢出；390px 下 12 页横向溢出。
- 本轮新增实测脚本 `measure-overflow.js`：逐页量
  `documentElement.scrollWidth` vs `innerWidth`，并列出超宽元素（标签/类名/超宽量），
  390px 与 1440px 双宽复测，替代目测。
- 证据：`~/workspace/goals/agentgate-enterprise-agent-platform/hidden_files/f36-audit/`
  （`shots/` 截图、`audit-shots.js`、`measure-overflow.js`、`measure1/2/3/overflow.json`）。

## 问题清单

### UX-001（高）窄屏下 12 张表格顶出页面横向滚动 — 已修复

- 违反标尺：⑧ 界面自己说明自己（内容被截断、须整页横向拖动才能看全）、⑦ 常用的好做。
- 实测（修复前，390px，`scrollWidth`）：channels 816、users 740、prompts 658、
  resttools 638、packages 512、skills 473、orgsync 472、exceptions 465、
  terminals 448、tokens 429、stats 459（budgets 表）；1440px 下全部正常。
  超宽元素一律是裸 `<table>`（表头 `white-space:nowrap`，无滚动容器）。
- 根因：26 张静态表与报表预览动态表都没有横向滚动容器。
- 修法：新增 `.tableWrap{overflow-x:auto}`，包裹全部 26 张静态表与 `repTable()` 生成的
  报表预览表（同一件事处处一样，标尺④）；宽表在卡片内横向滚动，页面本身不再溢出。
- 验证（本轮）：修复后复测 390px 与 1440px 下 24 页 `scrollW` 全部等于视口宽；
  渠道表实测 `wrapScrollW 777 > wrapClientW 312`（卡片内可滚动）、页面 `scrollW 390`；
  修复前后窄屏截图对比已存 `shots/shot-narrow-channels-fixed.png`。
- 状态：**已修复**（2026-11-25，F36 第一切片）。

### UX-002（高）窄屏下添加渠道表单仍为两列，下拉选项被截断 — 已修复

- 违反标尺：⑧（选项看不全）、①（系统能判断的不留给用户——用户须猜 "Deep…" 是哪个厂商）。
- 实测（修复前，390px）：`.formGrid` 计算值 `78px 220px`（两列），厂商预设下拉
  宽仅 78px 显示为 "Deep…"，信任类型下拉显示为 "signe…"；页面本身不溢出，
  故上一轮目测未发现，系本轮 `getComputedStyle` 探查确认。
- 根因：`.formGrid .wide{grid-column:span 2}` 在 `≤960px` 媒体查询下未复位——
  `grid-template-columns:1fr` 只有一列显式轨道，`span 2` 的字段撑出一条内容宽的
  隐式第二列，其余字段随之两两配对；最小复现已验证（同款 CSS 在空白页复现）。
- 修法：媒体查询内补 `.formGrid .wide{grid-column:auto}`，窄屏下全部字段单列堆叠。
- 验证（本轮）：复测 390px 下 `gridTemplateColumns` 为单列 `312px`，厂商预设下拉
  宽 312px 显示完整 "DeepSeek"，信任类型显示完整 "signed_cloud 签约云"；
  1440px 下仍为三列，不受影响；截图见 `shots/shot-narrow-channels-fixed.png`。
- 状态：**已修复**（2026-11-25，F36 第一切片，与 UX-001 同一切片：同一页窄屏布局）。

### UX-003（中）窄屏下侧边导航 24 项全量平铺在内容上方 — 已修复

- 违反标尺：② 此刻没用的就不出现、⑦ 常用的好做（手机上先翻一屏半导航才看到内容）。
- 证据：390px 截图（`shot-narrow-dashboard.png` / `shot-narrow-channels.png`）——
  `≤960px` 时 `aside` 变静态块、导航项换行平铺，约占 1400px 高后才是页面内容。
- 修法：窄屏折叠导航——品牌栏常驻，右端加「☰ 菜单」按钮（中英双语，`aria-expanded`
  同步状态）；`≤960px` 时 `nav` 与 `.sideFoot` 默认隐藏，点按钮展开（`aside.navOpen`），
  点任一导航项进页面后自动收起；桌面端按钮隐藏、导航常驻，行为不变。
  入口始终一 tap 可达，不违反 F36 红线「不得把入口藏到找不到」。
- 验证（本轮，`measure-nav.js`，390px）：默认态 `aside` 高 65px（修复前约 1400px）、
  `nav` 不显示、0 个导航项可见、页面 `scrollW 390`；点菜单后导航展开（8 个默认页
  可见，`aria-expanded=true`）；点「渠道」后进渠道页且导航自动收起；截图见
  `measure-nav/nav-narrow-{collapsed,open,after-nav}.png`。
  1440px 下按钮隐藏、导航常驻（24 项可见），桌面布局不受影响。
- 状态：**已修复**（2026-11-25，F36 第二切片）。

### UX-004（低）两处筛选行用内联多列模板，不跟随媒体查询堆叠 — 已修复

- 位置（本轮核对修正：清单初稿写的「审计页」有误，审计页筛选行是可换行的 flex `.row`，
  无内联网格）：设备接入页批准行（`index.html` 约 338 行，内联
  `grid-template-columns:1fr 1fr auto`）、组织同步页绑定行（约 456 行，内联
  `grid-template-columns:1fr 1fr 1fr auto`）。内联样式优先级高于样式表，`≤960px`
  的 `.formGrid{grid-template-columns:1fr}` 对它们无效。
- 实测：390px 下两页均不溢出；但三/四列挤在 312px 内，输入框偏窄、按钮被压扁，
  与 UX-002 同类问题（标尺⑧、④）。
- 修法：删掉内联样式，改用共享修饰类 `.formGrid.cols2a`（`1fr 1fr auto`）与
  `.formGrid.cols3a`（`1fr 1fr 1fr auto`），媒体查询内统一收为单列（同一件事处处一样，标尺④）；
  桌面端列宽不变。
- 验证（本轮）：390px 下两行均为单列 `312px`，子控件全宽 312px、无溢出；
  1440px 下设备行仍为 `504px 504px 58px`、组织同步行仍为 `331px 331px 331px 58px`；
  截图见 `measure-nav/nav-narrow-device.png`。
- 状态：**已修复**（2026-11-25，F36 第二切片）。

### UX-005（中）EN 模式下多处静态文案仍是中文 — 下拉选项、表单标签与占位符未进 I18N — 已修复

- 违反标尺：⑧ 界面自己说明自己（切到 EN 后仍有中文选项/标签/占位符）、④ 同一件事处处一样（同一控件在两种语言下长得不一样）。
- 审查方法：静态枚举 + 运行时双语实测。新脚本 `measure-i18n.js`（登录后逐页切 `ag_lang=en/zh`，取每页可见文本 + 全部 `placeholder` + 全部 `<option>` 文本，统计 CJK 字符数；另有 `measure-copy.py` 做词典键/引用静态核对）。
- 修复前实测（EN 模式，24 页，`measure5/i18n.json`）：dashboard 116 字、semreview 235 字、autoroute 226 字、channels 10 字、terminals 10 字、exceptions 9 字、orgsync 8 字、packages 13 字、wizard 5 字、stats 3 字、users 25 字、kb 22 字、dlpwords 2 字、upstreams/resttools/alerts 各 2 字；其余页 0。静态核对同时确认：词典 zh/en 键 400/400 对齐（0 缺失，`pkgSha` 中英同值是标准术语），324 处 `data-i18n` 与 70 处 `T()` 引用无一缺键。
- 根因分四类（`applyLang()` 只处理 `[data-i18n]` 与 `[data-i18n-ph]`，以下均不在其列）：
  ① 约 30 个 `<option>` 硬编码中文：向导与渠道页信任类型（`signed_cloud（签约云）`等）、终端筛选（全部/仅漂移/仅正常）、例外密级、组织同步平台（钉钉/飞书/企业微信）、语义复核开关/处置/分类、智能路由开关/分层；
  ② 6 个 `<label class="f">` 无 `data-i18n`：Token（可选）、Credential（可选）、Signing secret（可选）、SHA-256（期望校验和，可空）、ag-guard/ag-setup 组件包；
  ③ 11 个 `placeholder` 中文无 `data-i18n-ph`：预算部门「研发部」、例外理由/范围、组织同步名称、语义/路由样本示例、文件夹名/部门、知识包与模板的范围示例、变更说明「初版」；反向另有 3 处英文占位符在中文模式下不翻译（device name、两处 description）；
  ④ 后端下发的开通自检清单：`GET /api/onboarding/checklist` 的 `label`/`detail` 是 Go 代码里硬编码的中文（`internal/api/onboarding.go`），前端 `checklistRows()` 原样渲染，EN 模式下仪表盘与向导第五步整段中文。
- 修法：新增 43 个词典键（中英双语，见下）并给全部 30 个选项、6 个标签、14 个占位符挂上 `data-i18n`/`data-i18n-ph`；渠道页信任类型选项中文写法统一为向导页的括号式（`signed_cloud（签约云）`）；语义复核深层开关的「关闭」统一为「停用」（与同页启用开关一致）；后端 checklist 条目加 `label_en`/`detail_en` 字段（`checklistItem`，`omitempty`，只加字段不改结构），前端按语言选用；用户页角色下拉改用 `T("role"+首字母大写)`（新增 `roleAdmin/roleSecurity/roleAuditor/roleOperator/roleEmployee`）。`T()` 查不到键时回落为键名本身——修复中抓到一处现行：首版把键拼成 `role_Admin`（下划线），实测下拉显示 `role_Admin` 原文，已改为 `roleAdmin` 并复测通过。
- 验证（本轮，`measure5-after2/i18n.json`）：EN 模式 17 页 0 字，剩余 480 字全部是数据而非界面文案——向导 5 字（管理员显示名「系统管理员」）、stats 3 字（预算部门名「研发部」）、users 25 字（用户显示名/部门）、dlpwords 2 字（词表内容「星河 XH-200」）、semreview 205 字与 autoroute 218 字（风险/复杂度样本库正文）、kb 22 字（文件夹名与路径）；角色下拉中英双语均显示翻译名；`verify-slice3.js` 直查 checklist 接口：8 个条目 `label_en`/`detail_en` 齐全。中文模式复测无变化（各页 CJK 数与修复前一致，1440px 下无新增溢出——见下）。
- 状态：**已修复**（2026-11-25，F36 第三切片）。

### UX-006（低）新建类按钮动词「新建」与「添加」混用 — 已修复

- 违反标尺：④ 同一件事处处一样。
- 实测：词典动词清点——9 处「添加…」（添加渠道/文件夹/上游/Webhook/密钥/模板版本等）对 2 处「新建…」（`addKnowpack` 新建知识包、`addPrompt` 新建模板），同为「创建一条新记录」的操作。
- 修法：`addKnowpack` →「添加知识包」/“Add knowledge package”，`addPrompt` →「添加模板」/“Add template”。
- 验证：词典复核 + 冒烟全绿（见下）。
- 状态：**已修复**（2026-11-25，F36 第三切片）。

### UX-007（低）词典死键 10 个 — 已修复

- 违反标尺：② 此刻没用的就不出现（死代码/死文案是同类问题）。
- 实测（静态引用核对，动态 `T(g.group)` / `T("dlpSrc_"+…)` / `T("wizPhase"+…)` 均已排除）：`wizChecklist`（与同名 div id 混淆，键本身无人引用）、`obAdvance`/`obAllGreen`/`obComplete`（仪表盘开通卡片未用）、`kpSources`/`kpRefs`（只有同名 input id）、`pkgManagement`/`pkgUser`/`pkgGenerated`/`pkgOfflineImport`（安装包表直接渲染原始 `kind`/`source` 码）。另注：`orgEnabled` 原先也是死键，本轮被复用为「启用」选项键，转为在用；`auditor`/`employee`/`operator` 三个 en 独有键是误报——是 `loginHint` 文案里的冒号，不是词典键。
- 修法：删除上述 10 个死键（中英双语各 10 处）。
- 验证：删后复核 `data-i18n`/`T()` 引用与词典键双向无缺失、无重复。
- 状态：**已修复**（2026-11-25，F36 第三切片）。

### UX-008（低）报表预览中空的小节直接消失 — 已修复

- 违反标尺：⑧ 界面自己说明自己（用户分不清「本期无风险事件」还是「预览没加载出来」）。
- 实测：`repTable()` 在 `rows` 为空时 `return ""`——整个小节（含标题）不渲染；影响 Top 风险事件、渠道健康等 7 个预览小节（`bRows`/`cRows` 的固定键值表永不为空，不受影响）。
- 修法：空行时仍渲染标题与表头，正文为一行 `class="empty"` 的 `T("empty")`（「暂无数据」/“No data yet”），与其他 24 张表的空状态同一写法。
- 验证：`verify-slice3.js` 直调 `repTable`：空入参返回含标题、表头与 `No data yet` 空行的完整表格，非空入参行为不变。
- 状态：**已修复**（2026-11-25，F36 第三切片）。

### UX-009（中）删除操作一律无确认，点一下就删且不可撤销 — 已修复

- 违反标尺：⑦ 会丢东西的难误碰、⑤ 再按能收回（删了就没了，连确认都没有）。
- 实测（修复前，`verify-clicks.js` 逐一点击，`measure6/before/clicks.json`）：全站 15 个删除处理函数——预算、报表、证明策略、渠道密钥、语义样本、智能路由样本、知识库文件夹、提示词模板、知识包、上游、REST 工具、Webhook、安装包、组织同步来源、DLP 词——无一处 `confirm()`、无一处自定义确认框；实测在 DLP 页点一次 ✕，词立即消失（3 → 2），且无任何 toast/提示（见 UX-011）。
- 修法：新增共享确认框 `confirmDelete()`（同一件事处处一样，标尺④）——居中遮罩卡片（`role="alertdialog"`，标题「确认删除」/正文「删除后无法恢复，确定继续吗？」，中英双语新键 4 个），取消 / 遮罩点击 / Esc 均取消，焦点默认落在「取消」上（回车也不会误删）；15 个删除处理函数首行统一加 `if(!(await confirmDelete())) return;`，不改任何删除逻辑本身。
- 验证（本轮，`verify-clicks-after.js`，24 项断言全过、`measure6/after/clicks-after.json`）：点 ✕ 弹出确认框（标题/正文/焦点均正确），Esc、点「取消」、点遮罩三种取消方式下词都还在；点「确认删除」后词消失且 toast「✓ 已删除」；EN 模式下对话框为 "Confirm deletion / This cannot be undone. Continue? / Cancel / Delete"，取消后词仍在；向导走完仍正常（见下）。
- 状态：**已修复**（2026-11-25，F36 第四切片）。

### UX-010（低）Toast 点了关不掉，只能等 3.4 秒自动消失 — 已修复

- 违反标尺：⑤ 按下去有回应、再按能收回。
- 实测（修复前）：`toast()` 只设 3400ms 定时隐藏，`#toast` 无点击处理、`cursor:auto`、无 `role`；实测点击后仍显示，3.6 秒后才消失。
- 修法：`#toast` 加 `cursor:pointer`、`role="status"` 与 `onclick="dismissToast()"`（点击立即隐藏并清掉定时器，不会再弹回来）；每次 `toast()` 顺带写 `title`（新键 `toastDismiss`：「点击关闭」/“Click to dismiss”），界面自己说明自己（标尺⑧）。
- 验证（本轮）：点击后立即隐藏、等足 3.6 秒仍隐藏；0 控制台错误。
- 状态：**已修复**（2026-11-25，F36 第四切片）。

### UX-011（低）DLP 删词成功后没有任何回应 — 已修复

- 违反标尺：⑤ 按下去有回应（其余 14 个删除都有 `toast(T("deleted"))`，只有 `removeDLPWord` 成功时静默重载列表）。
- 实测（修复前）：点 ✕ 删词后 `toastVisible:false`、`toastText:""`，用户分不清是删了还是没点上。
- 修法：成功分支补 `toast(T("deleted"))`，与其他删除同一写法（标尺④）；本项修复与 UX-009 同一切片，确认框取消时不发 toast（还没删）。
- 验证（本轮）：确认删除后 toast「✓ 已删除」可见。
- 状态：**已修复**（2026-11-25，F36 第四切片）。

### UX-012（低）向导步骤条不可点，只能按「下一步」顺序走 — 已修复

- 违反标尺：⑦ 常用的好做（步骤条看起来可点、实际点了没反应）。
- 实测（修复前）：5 个步骤 pill 均为无 `onclick` 的 `span`，`cursor:auto`，点击后仍停留在第 1 步。
- 修法：`wizGo()` 渲染步骤条时给 pill 加 `cursor:pointer` 与 `onclick="wizGo(i)"`。步骤之间本无强制顺序（模板/渠道/阶段均可跳过），跳步不破坏任何校验；跳到第 5 步时照常渲染自检清单。
- 验证（本轮）：点第 5 个 pill 直接跳到第 5 步、自检清单 9 行正常渲染；随后走完向导仍正常（「开通完成」+ 回到仪表盘）。
- 状态：**已修复**（2026-11-25，F36 第四切片）。

### UX-013（低）渠道表单校验报错曾为服务端英文原文 — 已修复（本项两处）

- 违反标尺：⑧ 界面自己说明自己（中文界面里出现 "name and base_url required" / "pricing JSON invalid"，用户不知道错在哪）。
- 实测（修复前）：空表单提交 → `chMsg` 红色显示 "name and base_url required"；pricing 填错 → "pricing JSON invalid"（JS 里硬编码的英文串）。
- 修法：`addChannel()` 先做前端必填校验（名称/地址为空 → 新键 `chNeedNameUrl`「请填写渠道名称与地址」），pricing 解析失败改走新键 `chBadPricing`（「pricing JSON 格式不正确」），均中英双语；其余服务端错误仍直出，见 UX-014。
- 验证（本轮）：空提交与错 pricing 均显示中文红字 `msg err`；冒烟全绿。
- 状态：**已修复**（2026-11-25，F36 第四切片）。

### UX-014（低）其余服务端错误原文直出 — 已修复

- 违反标尺：⑧（中文界面下各类操作失败 toast 仍为服务端英文，如登录 "invalid credentials"、各页 `catch(e){ toast(e.message, false) }`）。
- 实测（第四切片点击验证附带）：登录错密码 → `#lerr` 红色显示 "invalid credentials"（样式与位置正确，只有文案是英文）；其余失败 toast 同款。
- 修法（本轮落地，取清单建议的第二种做法——前端映射表，不动后端错误口径）：新增 `errText(e)`（`api()` 之后）+ `ERR_EXACT` 精确映射表（74 条服务端固定错误串 → 词典键，含登录/设置码/渠道/用户/令牌/DLP/预算/版本/保存失败等高频可操作项，复用已有键 `chNeedNameUrl`）；前缀模式处理带引用标识的动态错误（`invalid trust type …`、`duplicate source …`、`at least one citation …`、`invalid arguments: missing required argument …`、JSON 解码错误）；后缀模式兜底长尾（`…not found` / `…not configured|not available` / `…already exists` / `…failed` / `…required|must be`）；仍未命中时中文界面显示通用文案 `errGeneric`、英文界面原样透传服务端英文。`SyntaxError` → `errBadJson`（覆盖四处 JSON.parse 用户输入）、`TypeError: Failed to fetch` → `errNetwork`、`HTTP NNN` → 通用文案。104 处 `e.message` 直接显示点（61 toast + 38 setMsg + 5 textContent）全部改为 `errText(e)`；另 4 处 200 响应里的 `d.error`/`r.error` 显示（上游测试、REST 测试、Webhook 测试、组织同步）同样走 `errText`。新增词典键 80 个（中英双语，zh 441→521、en 444→524）。后端错误码方案留作后续演进选项（映射表是单一扩展点，新错误串只加一行即可）。
- 验证（本轮，`verify-slice5.js` 20 项断言全过，证据在 `measure7/after/slice5.json`）：错密码登录中英双语均为本地化文案；渠道 http 地址、DLP 超长词、模板参数坏 JSON、发布不存在的版本、发布不存在的知识包、REST 测试缺必填参数，中文界面均显示中文、EN 界面均显示英文；全程 0 原生弹窗、0 控制台/页面/请求错误（故意触发的 4xx fetch 日志已按切片 4 的口径排除）。验证中另抓到一处映射缺口并当场补齐：REST 测试缺必填参数时服务端返回 `invalid arguments: missing required argument "employee_id"`，前缀匹配不到（以 `invalid arguments:` 开头），已把该前缀与子串匹配补进 `errText`，复测 toast 为「参数不正确，请检查输入后重试」。
- 状态：**已修复**（2026-11-25，F36 第五切片）。

### UX-015（低）三个功能用浏览器原生 `prompt()` 弹窗 — 已修复

- 违反标尺：⑧ 界面自己说明自己（`publishPrompt` / `publishKnowpack` 的 `prompt("publish version", …)` 与 `testRESTTool` 的 `prompt("arguments JSON", "{}")` 是无上下文的裸浏览器框）、⑤（`parseInt` 得 NaN、非法 JSON 直接发请求，错了才 toast 报错）。注：清单初稿记为「两处」，实为三处（两个发布入口各一处 + REST 测试一处）。
- 实测：静态核对（`prompt(` 仅此三处）；第四切片未实点（知识包发布需待审包、REST 测试需已建工具，演示种子状态不够，留待修那轮实测）。
- 修法：新增共享输入框 `promptInput()`（与确认框同一套遮罩卡片：`#inputOverlay`/`#inputCard` 复用确认框样式，`role="dialog"`；标题/说明/初始值/单行或多行/占位符/校验函数均由调用方传入；Esc / 点遮罩 / 取消均返回 null；单行输入框内回车提交，焦点在取消按钮上时回车仍走取消——不误提交）。提交前校验：版本号须为正整数（`^\d+$` 且 >0）、REST 参数须为合法 JSON；校验失败在框内红字提示、不关框、不发请求。三个调用点全部改用 `promptInput`，`publishPrompt`/`publishKnowpack` 共用「发布版本」标题与说明，`testRESTTool` 用多行 textarea。新增词典键 5 个（`inputConfirm`、`inputVersionTitle/Body`、`inputArgsTitle/Body`，含在 UX-014 的 80 个新键内）。
- 验证（本轮，`verify-slice5.js`）：发布框打开时标题「发布版本」、预填当前版本 1；填 `abc` 点确定 → 框内红字「版本号须为正整数」且框不关闭；Esc 取消不发布；填 99 提交 → toast「该版本不存在」；填 1 回车提交 → toast「✓ published v1」；知识包发布框同样走通（不存在的包 → toast「未找到该记录，请刷新后重试」）；REST 测试框为多行 textarea，填 `{bad` → 框内红字「输入的 JSON 格式不正确」，填 `{}` → toast「参数不正确，请检查输入后重试」（缺必填参数 employee_id）；全程 0 个原生弹窗（脚本监听 `dialog` 事件）。验证脚本修掉一个脚本自身缺陷：`page.evaluate(() => publishPrompt(...))` 会等输入框的 Promise 导致协议超时，改为 fire-and-forget 形式。
- 状态：**已修复**（2026-11-25，F36 第五切片）。

### UX-016（低）DLP 空输入曾静默无反应 — 已修复

- 违反标尺：⑤ 按下去有回应（`addDLPWord` 空输入时 `if(!word) return`，点了「添加」什么都不发生）。
- 修法：空输入改为 `setMsg(…, T("needWord"), false)`（新键「请填写敏感词」/“Enter a word”，中英双语）。
- 验证（本轮）：空点「添加」显示红色「请填写敏感词」。
- 状态：**已修复**（2026-11-25，F36 第四切片）。

## 已核对、无问题的项（不虚构为问题）

- 表格空状态：24 张静态表 + 统计页 `fill()` 均有 `|| <tr…class="empty">` 兜底（此前启发式脚本误报已逐行核对纠正）；唯一例外是报表预览 `repTable`，即 UX-008。
- 状态 pill 显示原始枚举码（`managed`/`confidential`/`healthy`/`online` 等）：中英双语一致、处处一样，属有意设计，不作问题。
- 批准类动词「通过」（知识包审核）与「批准」（设备码/例外审批）并存：中文产品文案里「审核通过」与「审批批准」是不同领域的惯用动词，有意区分，不统一。
- EN 模式剩余中文均为数据（种子用户姓名/部门、词表内容、样本库正文、文件夹名），不是界面文案，见 UX-005 验证段。
- Webhook 表里的 `last_error` 与备份状态行的 `ls.error` 是运维诊断日志摘录（数据），仍原样显示英文原文，不属 UX-014 的 toast/setMsg 错误显示范围。

## 未覆盖（后续切片，不虚构结论）

- ~~逐页文案与信息架构深审：中英双语文案一致性、空状态文案、按钮动词一致性、字段说明是否自解释（标尺⑧）。~~ → 已完成并扩清单为 UX-005–UX-008。
- ~~交互流程点击验证：向导五步走完、表单校验报错样式、删除/危险操作确认、Toast 提示是否可收回（标尺⑤）、操作后有无回应（标尺⑤）。~~ → 已完成并扩清单为 UX-009–UX-016，第五切片修完 UX-014、UX-015 后全部 16 项已修复。
- ~~登录页窄屏~~ → 已覆盖（本轮实测：390px 下 `scrollW` 390、卡片宽 342px、右缘 366px、0 错误，截图 `measure7/after/shot-login-narrow.png`；验证脚本修掉一个取数缺陷：主页面会话会泄漏进同浏览器新标签导致登录卡被隐藏，改用独立隐身上下文取数）。
- 深色模式（管理台暂无深色主题，F37 时一并确认）。
- demo-site 宣传页只处理与标尺明显冲突的问题，不作验收主体（F36 范围）。

## 验收进度

- 高严重度：2/2 已修复（UX-001、UX-002）。
- 中严重度：3/3 已修复（UX-003、UX-005、UX-009）；低严重度：11/11 已修复（UX-004、UX-006、UX-007、UX-008、UX-010、UX-011、UX-012、UX-013、UX-014、UX-015、UX-016）。
- 清单全部清零；登录页窄屏本轮实测通过（390px 无溢出，卡片 342px）；深色模式管理台暂无深色主题，按原计划 F37 时一并确认。F36 验收达成，标记 `done`。
