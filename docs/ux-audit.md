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

## 已核对、无问题的项（不虚构为问题）

- 表格空状态：24 张静态表 + 统计页 `fill()` 均有 `|| <tr…class="empty">` 兜底（此前启发式脚本误报已逐行核对纠正）；唯一例外是报表预览 `repTable`，即 UX-008。
- 状态 pill 显示原始枚举码（`managed`/`confidential`/`healthy`/`online` 等）：中英双语一致、处处一样，属有意设计，不作问题。
- 批准类动词「通过」（知识包审核）与「批准」（设备码/例外审批）并存：中文产品文案里「审核通过」与「审批批准」是不同领域的惯用动词，有意区分，不统一。
- EN 模式剩余中文均为数据（种子用户姓名/部门、词表内容、样本库正文、文件夹名），不是界面文案，见 UX-005 验证段。

## 未覆盖（后续切片，不虚构结论）

- ~~逐页文案与信息架构深审：中英双语文案一致性、空状态文案、按钮动词一致性、字段说明是否自解释（标尺⑧）。~~ → 本轮已完成并扩清单为 UX-005–UX-008。
- 交互流程点击验证：向导五步走完、表单校验报错样式、删除/危险操作确认、Toast 提示是否可收回（标尺⑤）、操作后有无回应（标尺⑤）。
- 登录页窄屏、深色模式（管理台暂无深色主题，F37 时一并确认）。
- demo-site 宣传页只处理与标尺明显冲突的问题，不作验收主体（F36 范围）。

## 验收进度

- 高严重度：2/2 已修复（UX-001、UX-002）。
- 中严重度：2/2 已修复（UX-003、UX-005）；低严重度：4/4 已修复（UX-004、UX-006、UX-007、UX-008）。
- 当前清单（UX-001–UX-008）已全部清零，但 F36 仍为 `doing`：「未覆盖」里的交互流程点击验证尚未展开——下一切片先扩清单（交互流程），再按新清单修复，全部高/中清零才标记 done。
