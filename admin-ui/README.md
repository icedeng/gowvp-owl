# GoWVP Owl Admin UI

面向视频监控值守、设备运维和系统管理的 Vue 3 管理端。页面数据与操作按 Owl 当前 HTTP API 接入，不包含固定账号、固定令牌或业务 Mock 数据依赖。

## 页面链路

```text
登录
  └─ 运行总览
      ├─ 异常设备 → 设备详情 → 通道详情 → 实时监控 / 录像中心
      ├─ AI / 国标报警 → 事件中心 → 实时核验 / 关联录像
      ├─ RTMP 推流 / RTSP 拉流 → 实时监控
      └─ 媒体节点 → 系统状态 → SIP / 协议诊断 / 版本升级
```

路由：

- `#/login`：登录入口与产品定位。
- `#/overview`：运行态势、协议矩阵、关注对象和快捷入口。
- `#/live`：1/4/9/16 分屏、PTZ、抓拍、对讲、AI 与录像控制。
- `#/events`：AI 检测和国标报警统一查询、实时与录像跳转。
- `#/recordings`：月历、时间轴、上下文筛选、播放与下载。
- `#/devices`、`#/devices/:id`：国标设备列表、设备档案和 GB/T 28181 高级能力。
- `#/transport-devices`：部标设备能力入口；当前后端开放对应设备类型后自动展示。
- `#/channels/:id`：通道播放、PTZ、AI、录像和设备端录像；兼容路由 `#/channels` 会跳转实时监控。
- `#/push-streams`、`#/pull-streams`：RTMP 推流与 RTSP 拉流配置。
- `#/media-servers`：默认媒体节点、带宽、存储和会话摘要。
- `#/system-status`：服务构建、资源、API 与依赖服务状态。
- `#/sip-settings`、`#/diagnostics`、`#/upgrade`：SIP、协议诊断和版本升级。
- `#/account`：单管理员账号安全。

## 技术栈

Vue 3、TypeScript、Vite、Vue Router、Pinia、Axios、Tailwind CSS、Lucide Icons、ECharts、pnpm。

## 运行

```bash
pnpm install
cp .env.example .env.local
pnpm dev
```

开发环境在 `.env.local` 中设置后端地址：

```env
VITE_API_BASE_URL=http://127.0.0.1:15123
```

不设置时使用同源 `/`，适合将构建产物部署到 Owl 的 `/web` 静态目录。环境文件中只应保存 API 地址，不要保存账号或密码。

生产构建：

```bash
pnpm build
pnpm preview
```

默认使用哈希路由和相对资源路径，`dist` 可直接部署到静态目录。

## 数据与接口

- 登录先读取 `/login/key`，在浏览器使用 RSA-OAEP/SHA-256 加密凭据，再以 JWT 调用受保护接口。
- Axios 在请求中统一附加 `Authorization: Bearer <token>`；401 会清理会话并返回登录页，重新登录后回到原页面。
- 总览、设备、通道、事件、录像、RTMP/RTSP、媒体节点、系统指标、SIP、诊断、升级和账号页面均使用真实接口。
- 数据页包含加载、空数据、错误和重试状态；写操作只在用户明确点击后执行。
- 密码、媒体 Secret、RTMP Session 和 RTSP 源凭据不会显示在列表、写入源码或持久化到浏览器配置。
- 事件/录像删除、AI 录像模式和多节点增删等后端边界按当前后端路由与权限能力保持禁用或显式降级。

## 已知接口差异

- 某些旧版本后端没有 `/gb28181/metrics`，协议诊断页会保留设备能力档案并将累计指标降级为空值。
- RTSP 高级参数已进入数据模型，但运行适配器尚未完整消费，页面不会把“可保存”描述为“已生效”。
- 实时播放页通过统一组件消费后端返回的多协议地址；未配置 EasyPlayer 时保留播放地址和快照，并在浏览器原生支持时使用 HLS 降级播放。

## 播放器配置

实时监控和通道详情统一使用 `src/components/StreamPlayer.vue`。业务页面只传入后端的 `PlayResult`，组件内部负责规范化新旧播放地址字段、按协议优先级选择地址，并调用 `src/player/index.ts` 中注册的适配器。

EasyPlayer 的常用配置可在管理端“平台运维 → 播放器设置”页面调整，包括协议优先级、缓冲、超时重连、MSE/WebCodecs/WASM 解码、H.265、音频、渲染、控制栏按钮和高级 JSON 透传。配置保存在当前浏览器本地；保存后播放器会自动重载并应用新参数。`isRtcZLM`、`isMute` 等运行时字段仍由组件根据协议和页面状态自动覆盖。

当前适配器包括：

- EasyPlayer：支持 HTTP-FLV、WS-FLV、HLS 和 ZLMediaKit WebRTC；从 `public/easyplayer` 按需加载。
- 浏览器原生 HLS：在 Safari 等原生支持 HLS 的环境作为无额外依赖的降级方案。

EasyPlayer 运行文件来自 [EasyDarwin/EasyPlayer.js](https://github.com/EasyDarwin/EasyPlayer.js) `main` 分支提交 `75c66d620036b4b5edc0a68e855c56ba7370c898`，存放在 `public/easyplayer`，默认地址为 `./easyplayer/EasyPlayer-pro.js`。如需使用 CDN 或其他部署位置，可在构建环境覆盖：

```bash
VITE_EASYPLAYER_SCRIPT_URL=/easyplayer/EasyPlayer-pro.js
```

`EasyPlayer-decode.js`、`EasyPlayer-lib.js`、`EasyPlayer-pro.wasm`、`EasyPlayer-snap.wasm` 需与 `EasyPlayer-pro.js` 保持在同一目录。后续替换播放器只需实现并注册 `PlayerAdapter`，无需修改业务页面。

上游仓库当前没有 `LICENSE` 文件；公开源码的版权与再分发条件仍应由使用方结合实际发布场景确认。
