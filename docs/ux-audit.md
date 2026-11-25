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

### UX-003（中）窄屏下侧边导航 24 项全量平铺在内容上方 — 待修

- 违反标尺：② 此刻没用的就不出现、⑦ 常用的好做（手机上先翻一屏半导航才看到内容）。
- 证据：390px 截图（`shot-narrow-dashboard.png` / `shot-narrow-channels.png`）——
  `≤960px` 时 `aside` 变静态块、导航项换行平铺，约占 1400px 高后才是页面内容。
- 修法候选：窄屏折叠导航（汉堡按钮展开/收起，默认只露当前分组）；不得把入口藏到
  找不到（F36 红线：渐进披露既有结构可复用）。
- 状态：**待修**（下一轮切片首选）。

### UX-004（低）两处筛选行用内联多列模板，不跟随媒体查询堆叠 — 待验证

- 位置：审计页筛选行（`index.html` 约 338 行，
  内联 `grid-template-columns:1fr 1fr auto`）、组织同步页筛选行（约 456 行，
  内联 `grid-template-columns:1fr 1fr 1fr auto`）。
- 实测：390px 下两页均不溢出；但内联模板无视媒体查询，窄屏下控件偏挤，
  需截图确认拥挤程度后再定是否改（改法候选：改用 `.formGrid` 或在媒体查询内覆盖）。
- 状态：**待验证**。

## 未覆盖（后续切片，不虚构结论）

- 逐页文案与信息架构深审：中英双语文案一致性、空状态文案、按钮动词一致性、
  字段说明是否自解释（标尺⑧）。
- 交互流程点击验证：向导五步走完、表单校验报错样式、删除/危险操作确认、
  Toast 提示是否可收回（标尺⑤）、操作后有无回应（标尺⑤）。
- 登录页窄屏、深色模式（管理台暂无深色主题，F37 时一并确认）。
- demo-site 宣传页只处理与标尺明显冲突的问题，不作验收主体（F36 范围）。

## 验收进度

- 高严重度：2/2 已修复（UX-001、UX-002）。
- 中严重度：1 待修（UX-003）；低严重度：1 待验证（UX-004）。
- F36 仍为 `doing`，未达「高/中清零」验收标准。
