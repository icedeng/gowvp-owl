# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Stack

Vue 3、TypeScript、Vite、Vue Router、Pinia、Axios、Tailwind CSS、Lucide Icons、ECharts，使用 pnpm 管理依赖。

## Users

- 视频监控值守人员：持续观察设备和通道状态，快速预览实时视频并处置告警。
- 设备运维人员：接入 GB28181、ONVIF、RTSP、RTMP 设备，诊断注册、心跳和流媒体问题。
- 系统管理员：维护 SIP、流媒体节点、录像策略、AI 分析与平台配置。

以上用户角色由仓库现有功能推断，尚未形成正式角色权限模型。

## Product Purpose

国标视频管理平台是基于 GoWVP Owl 能力构建的多协议视频设备统一监控与运维平台。它让用户在一个 Web 管理端完成设备接入、实时预览、云台控制、录像检索、AI 事件处置和系统配置。成功意味着用户能从异常发现快速进入设备、直播、录像或事件上下文，并完成闭环处理。

## Positioning

以 GB28181 为核心，同时统一管理 ONVIF、RTSP、RTMP 通道，并把实时播放、录像和 AI 检测事件串成同一条处置链路。

## Operating Context

- 持续值守与周期巡检并存，设备在线状态、最新心跳和告警优先级需要被快速扫读。
- 高频链路为：态势总览 → 定位设备/通道 → 实时预览或录像回看 → 事件处置 → 留痕。
- 运维链路为：设备接入 → 目录同步与能力探测 → 流媒体状态检查 → SIP/媒体服务配置。

## Capabilities and Constraints

- 支持 GB28181-2011/2016/2022、ONVIF、RTSP、RTMP。
- 支持设备与通道管理、实时播放、抓拍、云台控制、语音、设备升级、AI 区域与启停。
- 支持录像列表、时间轴、月度统计、播放、下载与删除。
- 支持 AI 事件图片、标签、置信度、时间范围和处理状态展示。
- 支持流媒体节点、推流/拉流和 SIP 参数配置。
- 本次交付为可运行的前端设计原型，使用演示数据呈现完整状态，不改动后端接口。

## Brand Commitments

- 管理端界面统一使用“国标视频管理平台”，不展示 Owl 英文字标。
- 保留仓库现有 `docs/logo.png` 与 `docs/logo2.png` 品牌资产。
- 界面语言为简体中文，协议名、设备 ID、流 ID 等技术标识保持原始英文或编码格式。

## Evidence on Hand

- 产品与功能说明：`README_CN.md`。
- 后端路由和数据模型：`internal/web/api`、`internal/core`。
- 既有界面截图：`docs/demo/home.webp`、`docs/phone/*.webp`。
- 品牌资产：`docs/logo.png`、`docs/logo2.png`。
- 页面中的业务数值、设备名称和事件记录均为演示数据，不代表真实部署指标。

## Product Principles

- 任务优先：信息架构围绕值守与处置链路，而不是后端模块名称。
- 状态清晰：在线、离线、告警、处理中和已完成使用一致语义。
- 上下文连续：设备、直播、录像和事件之间保留可追溯跳转。
- 协议兼容：统一体验不隐藏不同协议的真实能力边界。
- 可验证：原型提供可运行交互、响应式布局和明确的演示数据标识。

## Accessibility & Inclusion

界面需要支持键盘焦点、降低动态效果偏好、非颜色状态提示和桌面/移动端响应式布局。
