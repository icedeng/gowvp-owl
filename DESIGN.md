---
name: "国标视频管理平台 Admin UI"
description: "浅色信号矩阵控制台，让设备、通道与媒体链路在固定槽位中快速可核验"
colors:
  ink: "#172033"
  ink-strong: "#0d1526"
  muted: "#647188"
  muted-2: "#647188"
  bg: "#f4f6f9"
  panel: "#ffffff"
  panel-2: "#f8fafc"
  panel-3: "#eef2f7"
  line: "#dfe5ed"
  line-strong: "#c8d1de"
  blue: "#1768d4"
  blue-hover: "#1057b6"
  blue-soft: "#eaf2ff"
  green: "#17875d"
  green-soft: "#e7f6ef"
  amber: "#b66a08"
  amber-soft: "#fff4df"
  red: "#c43c43"
  red-soft: "#fcebed"
  violet: "#6a56bd"
typography:
  display:
    fontFamily: "Noto Sans SC, sans-serif"
    fontSize: "24px"
    fontWeight: 700
    lineHeight: 1.25
    letterSpacing: "-0.02em"
  headline:
    fontFamily: "Noto Sans SC, sans-serif"
    fontSize: "17px"
    fontWeight: 700
    lineHeight: 1.35
  title:
    fontFamily: "Noto Sans SC, sans-serif"
    fontSize: "13px"
    fontWeight: 700
    lineHeight: 1.45
  body:
    fontFamily: "Noto Sans SC, sans-serif"
    fontSize: "14px"
    fontWeight: 400
    lineHeight: 1.55
  label:
    fontFamily: "Noto Sans SC, sans-serif"
    fontSize: "10px"
    fontWeight: 700
    lineHeight: 1.55
  numeric:
    fontFamily: "Barlow Condensed, Noto Sans SC, sans-serif"
    fontSize: "26px"
    fontWeight: 700
    lineHeight: 1
  mono:
    fontFamily: "SFMono-Regular, Consolas, monospace"
    fontSize: "10px"
    fontWeight: 400
    lineHeight: 1.55
rounded:
  sm: "8px"
  control: "8px"
  surface: "12px"
  avatar: "7px"
  status: "999px"
spacing:
  xs: "4px"
  sm: "8px"
  md: "12px"
  lg: "14px"
  xl: "16px"
  2xl: "18px"
  3xl: "26px"
  section: "32px"
components:
  button-primary:
    backgroundColor: "{colors.blue}"
    textColor: "#ffffff"
    typography: "{typography.label}"
    rounded: "{rounded.control}"
    padding: "0 13px"
    height: "36px"
  button-secondary:
    backgroundColor: "{colors.panel}"
    textColor: "{colors.ink}"
    typography: "{typography.label}"
    rounded: "{rounded.control}"
    padding: "0 13px"
    height: "36px"
  input:
    backgroundColor: "{colors.panel}"
    textColor: "{colors.ink}"
    typography: "{typography.body}"
    rounded: "{rounded.control}"
    padding: "0 11px"
    height: "36px"
  nav-item-active:
    backgroundColor: "{colors.blue-soft}"
    textColor: "{colors.blue}"
    typography: "{typography.label}"
    rounded: "{rounded.control}"
    padding: "0 10px"
    height: "37px"
  status-online:
    backgroundColor: "{colors.green-soft}"
    textColor: "{colors.green}"
    typography: "{typography.label}"
    rounded: "{rounded.status}"
    padding: "3px 8px"
  card:
    backgroundColor: "{colors.panel}"
    textColor: "{colors.ink}"
    rounded: "{rounded.surface}"
    padding: "17px"
  protocol-tag:
    backgroundColor: "{colors.panel-3}"
    textColor: "#3c526e"
    typography: "{typography.mono}"
    rounded: "5px"
    padding: "3px 7px"
  signal-strip:
    backgroundColor: "#172944"
    textColor: "#ffffff"
    rounded: "{rounded.surface}"
    padding: "15px 17px"
  matrix-slot:
    backgroundColor: "{colors.panel-2}"
    textColor: "{colors.ink}"
    rounded: "{rounded.sm}"
    padding: "12px"
---

# Design System: 国标视频管理平台 Admin UI

## Overview

**Creative North Star: "白天值守的信号矩阵"**

国标视频管理平台是一张白天值守的信号矩阵：冷白工作面承载持续操作，雾灰分区拉开层级，工程蓝把“可以继续”变成清楚、可执行的动作。布局和内容都服务于快速扫描，拒绝深色大屏与指标卡片堆叠；深色只被限制在实时视频画面、运行态势条和登录页信号场中，作为媒体与链路信息的对比框，而不是全局主题。本系统采用 Operate 模式，方向种子为 `20e0e63b`。

广播设备控制台的语法来自状态灯、协议铭牌、矩阵槽位和细密表格。值守人员先辨认平台风险，再沿设备 → 通道 → 直播 → 事件 → 录像链路核验，最后进入协议和系统运维定位故障。数字使用窄体节奏，设备 ID、流 ID、地址和时间使用等宽字，中文任务说明保持稳定的无衬线阅读感。

**Key Characteristics:**

- 冷白工作面（`#f4f6f9`）与白色面板（`#ffffff`）组成低反射、长时间可读的 admin 控制台。
- 工程蓝负责主动作、导航选中和信息态；绿、琥珀、红分别表达健康、注意/异常、危险/离线。
- 运行态势条先于协议矩阵、待关注对象和趋势内容出现，主动作直达实时监控。
- 固定分组导航与粘性顶栏保持位置感；窄屏导航收为遮罩抽屉，数据区域按断点重排。
- 细边、轻阴影、8–12px 圆角和高密度短标签形成设备控制台的工程质感。

## Colors

色彩以冷白和雾灰构成中性底，蓝色是唯一的操作声部，状态色只在对应语义出现；所有值以 frontmatter 及 `admin-ui/src/styles/design.css` 为准。

### Primary

- **工程蓝** (`colors.blue`)：主按钮、当前导航、链接、聚焦和信息态；悬停使用更深的 `colors.blue-hover`，柔和底使用 `colors.blue-soft`。

### Secondary

- **运行绿** (`colors.green`)：在线、完成、可用和健康信号；`colors.green-soft` 是其低对比背景。
- **注意琥珀** (`colors.amber`)：待处理、心跳异常、资源预警和 AI 事件；`colors.amber-soft` 是其低对比背景。

### Tertiary

- **危险红** (`colors.red`)：离线、危险、删除倾向和录制指示；`colors.red-soft` 用于状态底。
- **辅助紫** (`colors.violet`)：保留给需要区分的辅助分类，不抢占蓝色主动作或状态灯语义。

### Neutral

- **工作面雾灰** (`colors.bg`)：页面底色；**面板白** (`colors.panel`)：卡片、表单和浮层。
- **近白分区** (`colors.panel-2`) 与 **雾灰分区** (`colors.panel-3`)：矩阵槽位、输入辅助底和分段控件。
- **正文墨** (`colors.ink`) 与 **强调墨** (`colors.ink-strong`)：正文和标题；`colors.muted`、`colors.muted-2` 统一使用可达 4.5:1 的 `#647188`，说明、元数据与分组标签通过字重和间距区分层级。
- **结构线** (`colors.line`) 与 **强调结构线** (`colors.line-strong`)：卡片、表格、输入和顶栏边界。

**The State Color Rule.** 蓝色不表示异常；绿色只表示健康/完成，琥珀只表示注意/等待/异常，红色只表示危险/离线。每个状态同时提供文字或图形提示。

**The Blue Action Rule.** 每个局部动作组只保留一个工程蓝主动作，其余使用白底、透明或危险变体，避免页面变成蓝色噪声。

## Typography

**Display / Body Font:** Noto Sans SC（`sans-serif` 回退）  
**Numeric / Brand Font:** Barlow Condensed（`Noto Sans SC, sans-serif` 回退）  
**Label/Mono Font:** SFMono-Regular（Consolas、`monospace` 回退）

**Character:** Noto Sans SC 让中文任务信息、产品名称和表单保持清晰；Barlow Condensed 用于计数、带宽、时间和优先级，形成紧凑的监控节拍；等宽字体只承担可核对的技术标识。

### Hierarchy

- **Display** (`typography.display`, 24px, 700, 1.25)：页面标题和首要工作结论；窄屏为 21px。
- **Headline** (`typography.headline`, 17px, 700, 1.35)：运行态势条的核心结论、视频工作区标题。
- **Title** (`typography.title`, 13px, 700, 1.45)：卡片标题、导航当前项和模块标题。
- **Body** (`typography.body`, 14px, 400, 1.55)：页面描述、表单正文与一般操作文本。
- **Label** (`typography.label`, 10px, 700, 1.55)：分组名、字段名、状态标签和短按钮；操作性辅助信息不低于 10px。
- **Numeric** (`typography.numeric`, 26px, 700, 1)：态势条、资源摘要、日期和优先级数字。
- **Mono** (`typography.mono`, 10px, 400, 1.55)：设备编码、流 ID、地址、时间和协议相关标识。

**The Role Separation Rule.** 不用窄体或等宽字体承载长段中文；它们只服务于数字节奏和机器可核对的短标识。

## Layout

应用是“固定导航 + 流式工作区”：桌面侧栏宽 `224px`，左侧固定并带分组导航；顶栏高 `64px`、粘性定位，页面内容横向内边距为 `clamp(18px, 2.2vw, 34px)`，顶部起始留白 `26px`。主网格间隙常用 `14px`，卡片内边距常用 `17px`；内容区不设强制最大宽度，随可用工作区展开。

桌面首屏顺序为：页面标题与主动作 → 全宽运行态势条 → 四格资源摘要 → 协议信号矩阵与媒体/存储健康 → 待关注对象与最新事件 → 事件趋势和快捷入口。协议矩阵以四槽位为核心；实时监控采用 `250px / minmax(0, 1fr) / 280px` 的资源树、视频墙、控制栈三列结构。

响应式断点来自现有 CSS：`1180px` 以下收窄侧栏到 `204px`，隐藏演示标记并让控制栈横跨；`900px` 以下侧栏变成从左侧滑入的遮罩抽屉，工作区占满宽度，搜索收为图标，栅格大多降为单列或双列；`640px` 以下顶栏高 `56px`、页面内边距 `12px`，运行态势保持 `2 × 2` 固定槽位，协议矩阵提前到资源指标之前，视频墙转为单画面，工具栏字段全宽。最小视口保持 `320px`。

**The Scan Order Rule.** 运行态势、协议矩阵和待关注列表是首屏扫描线；新增模块应排在这些信号之后，并保留“对象 → 状态 → 下一步”的阅读顺序。

## Elevation & Depth

这是浅色、低反射的分层系统：白色面板靠 `1px` 冷灰边与轻微环境阴影从雾灰工作面浮起，普通卡片使用 `0 5px 18px rgba(34, 50, 78, .04)`，Toast 使用 `var(--shadow)`（`0 10px 28px rgba(25, 42, 70, .08)`）。实时视频工作区及态势条保留深海军蓝内嵌对比，深色仅用于让视频内容和白色数字更易辨识。

**The Tonal Layer Rule.** 先用 `panel` / `panel-2` / `panel-3` 和结构线表达层级，再使用轻阴影；不要用大面积黑底或高扩散阴影制造“大屏”效果。

## Shapes

形状是工程化的柔和矩形：主卡片和态势条 `12px`，按钮、输入、导航项和矩阵槽位 `8px`，头像/设备图标常用 `7px`，状态胶囊为 `999px`。协议铭牌额外使用 `5px` 小圆角，优先级块 `5px`；圆形只用于状态灯、节点轨道、PTZ 和步骤编号。边框保持 `1px`，不使用票据缺口、纸张纹理或虚线作为全局装饰。

## Components

### Buttons

- **Shape:** `8px` 圆角；默认高度 `36px`，紧凑按钮 `30px`。
- **Primary:** 工程蓝底（`colors.blue`）、白字、水平内边距 `13px`；一个动作组中只用于最主要动作，如“进入实时监控”。
- **Secondary / Ghost:** 白底 + `colors.line-strong` 边线；Ghost 透明无边，适合刷新、筛选和次级入口。
- **Danger:** `colors.red` 字色配 `colors.red-soft` 底和浅红边，用于删除或危险确认。
- **Hover / Focus / Disabled:** 悬停转向 `colors.blue-hover` 或浅蓝底，过渡约 `0.16s ease`；全局键盘焦点为 `3px` 半透明蓝环、`2px` 偏移；禁用态降低对比并取消交互。

### Chips

- **Status:** `3px 8px` 内边距、`999px` 胶囊、6px 状态灯和文字并列；在线/完成、注意/待处理、危险/离线、信息分别复用绿/琥珀/红/蓝语义。
- **Protocol:** `3px 7px` 内边距、`5px` 圆角、Barlow Condensed `9px`、细边和淡灰/浅蓝底，保留 GB28181、ONVIF、RTSP、RTMP 的技术铭牌感。
- **Segmented:** 外层浅雾灰底、`8px` 圆角和 `3px` 内衬；选中项白底并用轻阴影确认位置。

### Cards / Containers

- **Corner Style:** 主卡片 `12px`；视频工作区、表格卡和详情容器沿用同一圆角。
- **Background / Border:** `colors.panel` 白底、`colors.line` 细边；矩阵槽位使用 `colors.panel-2`，能力项和辅助区使用 `colors.panel-2` / `panel-3`。
- **Shadow Strategy:** 卡片为低强度环境阴影；深色只限视频内容和态势条，不扩散到普通面板。
- **Internal Padding:** 常规 `17px`，工具栏/表头 `15–16px`，紧凑行 `8–13px`。

### Inputs / Fields

- **Style:** 白底、`1px colors.line-strong` 边线、`8px` 圆角；标准输入/选择器高度 `36px`，搜索框左侧为 `34px` 图标槽位。
- **Focus:** 边线转为浅蓝并增加 `0 0 0 3px rgba(23, 104, 212, .1)` 焦点环。
- **Error / Disabled:** 禁用字段降低对比；错误延续红色状态语义并同时显示文字，不能只改边框颜色。

### Modal Dialog

模态框使用白色面板、`13px` 圆角和高层阴影，标题必须成为可访问名称并声明 `role="dialog"` 与 `aria-modal="true"`。打开时保存触发元素并聚焦首个控件，Tab / Shift+Tab 在对话框内循环，Escape、关闭按钮或点击遮罩均可关闭，关闭后焦点返回原触发元素。

### Navigation

侧栏白底固定，分组标签用 `10–11px` 弱化灰；导航项最小高 `37px`、图标 `16px`。悬停浅蓝灰底；当前项使用工程蓝文字、`colors.blue-soft` 底、700 字重，并在左缘显示 `3px × 20px` 蓝色短线。`900px` 以下导航成为遮罩抽屉，菜单按钮、遮罩和 Escape 均可关闭。

### Signal Strip

全宽态势条是首屏的运行结论：深海军蓝 `#172944` 只承载“核心链路运行正常”和四个数字槽位，左侧引导单元使用更深的 `#11233e`，槽位之间用半透明白线分隔；绿点表示健康，琥珀点表示最新事件等注意信号。

### Protocol Matrix & Video Wall

协议矩阵是四格 `matrix-slot` 槽位，每格显示协议铭牌、在线灯、在线/总数和短注释，可整格进入通道、推流或拉流上下文。实时监控视频墙使用 1/4/9/16 分屏，深海军蓝工具栏和视频背景只服务于画面辨识；资源树和控制栈仍保持浅色卡片，PTZ、抓拍、对讲、AI 和持续录像动作按协议能力启用或禁用。

## Do's and Don'ts

### Do:

- **Do** 让冷白工作面、雾灰分区和工程蓝操作色成为所有新页面的默认语法。
- **Do** 首屏先给运行态势条，再给协议信号矩阵、待关注对象和资源趋势；主动作直达实时监控。
- **Do** 用状态灯、文字、图标或形状共同表达在线、异常、危险和信息，不把颜色当作唯一线索。
- **Do** 保留协议名、设备 ID、流 ID、时间和地址的技术标识感，并用窄体数字/等宽字体提高核验速度。
- **Do** 沿用 `14px` 网格间隙、`12px` 主卡片圆角、`36px` 控件高度和 `1px` 冷灰结构线，保持密度一致。
- **Do** 在 `900px` 和 `640px` 断点重排内容，保留焦点可见、抽屉遮罩关闭和 `prefers-reduced-motion` 支持。

### Don't:

- **Don't** 把整个 admin UI 做成深色大屏、黑色石墨主题或指标卡片堆叠；深色只可作为实时视频/态势条的局部对比。
- **Don't** 引入纸张、登机牌、票据缺口、纤维纹理或虚线作为当前视觉世界的签名。
- **Don't** 让工程蓝承担警告/危险语义，或在同一动作组放置多个同权重蓝色主按钮。
- **Don't** 用窄体或等宽字体排版长段中文，也不要把协议铭牌做成装饰性徽章墙。
- **Don't** 在窄屏简单压缩桌面表格；应按断点转单列、折行、横向滚动或收起次级信息。
- **Don't** 用厚重渐变、玻璃拟态和高扩散阴影替代清晰的槽位、边线与状态文本。
