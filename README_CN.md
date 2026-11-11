[English](./README.md) | **简体中文**

# GoWVP Owl · 视频接入与开放平台

基于 [gowvp/owl](https://github.com/gowvp/owl) 持续开发的视频监控平台，面向国标设备接入、视频值守、录像与抓拍管理，以及第三方业务系统集成。

本分支采用人机协作方式，借助 AI 持续开展代码分析、功能开发、缺陷修复、测试与文档维护。需求与关键决策由维护者主导，发布前仍需代码检查及实际设备、业务场景验收。

本仓库已维护独立的 Vue 3 管理端、国标协议适配、媒体节点管理和三方应用开放平台。功能、配置与交付以本仓库代码和文档为准；上游演示站、前端压缩包及公共镜像不代表本项目。版本号见 [VERSION](./VERSION)，变更与部署证据见[发布记录](./deploy/releases/)。

## 当前能力

| 模块 | 已实现内容 |
| --- | --- |
| 视频值守 | 运行总览、资源树与多画面实时预览、通道详情、主子码流选择、播放地址复制与播放器设置 |
| 设备接入 | GB/T 28181、ONVIF 设备接入与虚拟设备服务、RTSP 拉流、RTMP 推流；设备与通道管理、心跳及注册历史 |
| 国标业务 | 注册鉴权与版本协商、目录和信息查询、实时点播、设备录像检索/回放/下载、云台控制、语音广播与对讲、上下级平台级联及协议诊断 |
| 平台录像 | 按通道手动启停录像、日期检索、日历与连续回放、文件下载、保留期限与磁盘占用清理 |
| 抓拍 | 设备上传或 FFmpeg 取帧、后台任务、定时计划、图片预览/下载/重命名/删除，以及全平台抓拍记录与参数设置 |
| 媒体节点 | ZLMediaKit / Lalmax 节点新增、编辑、启停、删除、运行状态查询及通道绑定；回调按节点隔离 |
| 第三方集成 | 应用管理、独立 AK/SK、接口 Scope、设备授权、来源与配额、实时播放、两类录像、抓拍、云台及事件订阅 |
| 运维与安全 | 账号安全、HTTP/HTTPS 加密登录兼容、登录防护、系统监控、媒体播放凭据校验，以及配套宿主服务的签名升级与回滚 |
| AI 与事件 | 可选 AI 服务对接、分析任务、事件查询、图片与 Webhook 推送；需另外部署分析服务 |

国标协议档案覆盖 `1.0=2011`、`1.1=2011 修改补充文件（2014）`、`2.0=2016`、`3.0=2022`。不同版本按能力启用，SIP-TLS、证书认证、附录 G / O 等可选能力需要额外配置。代码实现和自动化检查不等于所有厂商、标准条款或网络环境都已完成真机验收，详见[完成审计](./docs/testing/gb28181-completion-audit.md)和[互通手册](./docs/testing/gb28181-interoperability-runbook.md)。

媒体输出包括 HTTP-FLV、WS-FLV、HLS、WebRTC、RTSP 和 RTMP，实际可用协议、音视频解码及浏览器兼容性取决于媒体驱动、源编码和播放器。多节点管理不包含自动负载均衡或故障切换。当前管理端以中文为主。

## 运行结构

```text
摄像机 / NVR / 下级平台 ── SIP、RTP/RTCP ──┐
RTSP / RTMP / ONVIF 设备 ────────────────────┤
                                           ▼
浏览器 ── 管理页面 / API ── GoWVP Owl ── ZLMediaKit / Lalmax
第三方业务后端 ── 签名 API ──┤                  │
第三方播放器 ── 带票据播放地址 ┘                  │
                           │                  │
                       PostgreSQL       共享录像目录
```

- 后端使用 Go、Gin、GORM，管理端使用 Vue 3、TypeScript、Vite、Pinia 和 ECharts。
- 前端源码位于 `admin-ui/`，发布产物进入应用镜像 `/app/www`，由 Owl 提供 `/web/` 页面及同源 API。
- 推荐的独立编排为 Owl + ZLMediaKit + PostgreSQL。ZLM 使用宿主机网络，Owl 通过桥接网络访问它，录像目录在两个容器中使用一致的绝对路径。
- 数据层也支持 SQLite、MySQL；现有数据不会因修改数据库连接串自动迁移。迁移工具和数据保留说明见 [PostgreSQL 文档](./deploy/postgresql.md)。

## 新环境部署

以下命令用于**新的 Linux Docker 环境**。已有部署应先阅读[升级与数据说明](./deploy/v1/README.md)，完成备份、版本差异和数据库迁移核对。专用服务器使用自己的运行编排，不直接套用首次部署命令。

构建机需要 Git、Go（版本要求见 `go.mod`，当前为 1.26）、Node.js（建议 22 LTS）、pnpm（按 `admin-ui/package.json` 当前锁定为 11.11.0）、OpenSSL、Docker Engine 和 Docker Compose。支持生成 Linux amd64 / arm64 产物，跨架构镜像构建需要对应平台支持；首次 ZLM 构建还需要下载依赖并编译。

```bash
git clone https://github.com/icedeng/gowvp-owl.git
cd gowvp-owl

# 先切换到要部署的发布分支或标签，并核对 VERSION 与发布记录。
# 替换成摄像机、上级平台和播放端可访问的宿主机地址。
./deploy/v1/deploy.sh init 192.168.1.100

# 按下文核对 .env 和 config.toml 后再执行。
./deploy/v1/deploy.sh release
./deploy/v1/deploy.sh status
```

`init` 生成 `deploy/v1/.env` 和独立随机密码/密钥，已有环境文件时拒绝覆盖。`release` 编译前后端、构建本地镜像并启动服务，不推送镜像仓库。仅构建使用 `build`，仅启动已有镜像使用 `up`；完整命令见[部署说明](./deploy/v1/README.md)。

发布前至少核对：

| 配置位置 | 必查内容 |
| --- | --- |
| `deploy/v1/data/configs/config.toml` 的 `[Sip]` | `Host`、20 位平台 `ID`、10 位 `Domain`，以及设备注册策略 |
| `deploy/v1/.env` | `OWL_MEDIA_SDP_IP`、管理端口、管理员凭据、数据库连接及独立密钥 |
| SIP 注册密码 | `OWL_SIP_PASSWORD` 初始化为空；需要 Digest 密码验证时，设置后同步配置设备 |
| 媒体节点 | 平台访问节点的 API 地址、节点回调平台的地址、设备可达的收流地址及 API 共享密钥 |
| 存储与网络 | 数据盘容量、目录挂载、端口占用和设备/播放器到媒体地址的路由 |

启动后访问 `http://<宿主机地址>:15123/web/`，健康信息为 `/health`。端口按实际环境配置。没有固定通用登录密码，首次使用 `.env` 中的管理员凭据；网页改密后，`admin-credentials.toml` 优先于初始环境变量，须随配置备份。

### 常用端口

下表对应 `deploy/v1` 编排；变更端口时要同步设备、宣告地址和防火墙配置。

| 端口 | 用途 |
| --- | --- |
| `15123/tcp` | 管理页面、API、WebSocket、媒体 HTTP 代理及 ONVIF SOAP |
| `15060/tcp+udp` | 国标 SIP 信令 |
| `15061/tcp` | SIP-TLS，需配置证书并启用 |
| `3702/udp` | ONVIF WS-Discovery，容器组播发现受网络环境限制 |
| `8080/tcp` | ZLM HTTP/API |
| `554/tcp`、`1935/tcp` | RTSP、RTMP |
| `30000/tcp+udp` | WebRTC |
| `30001–50000/tcp+udp` | 国标 RTP/RTCP 动态端口池 |
| `50200–50300/tcp` | 按需启用的附录 O 裸 TCP 下载/级联中继 |

PostgreSQL 在 Compose 内部使用 `5432`，不映射宿主机端口。通用编排使用本地构建镜像；上游 `gospace/gowvp:latest` 及其他上游镜像不包含本仓库的定制功能。专用环境与通用模板的差异见[服务器部署方案](./deploy/server-192.168.6.20.md)。

## 第三方视频与业务接入

本平台的视频可以提供给第三方应用播放，不要求播放器持有管理员登录态。

| 接入方式 | 使用场景与权限边界 |
| --- | --- |
| 播放地址分享 `/proxy/sms/…` | 链接携带绑定媒体节点与流的播放 token，第三方播放器可以直接使用；HLS 清单及子资源保持凭据传递，禁止跨流、跨节点和管理接口透传。持有链接即持有对应播放权限，不能用它代替应用级资源授权 |
| 应用开放平台 `/openapi/v1/` + `/openmedia/v1/` | 第三方业务后端用 AK/SK 签名调用，按 Scope、设备、来源和配额授权，再为播放器申请短期媒体票据；适用于需要独立应用权限、撤销、审计和回调的集成 |

开放平台提供设备/通道编码查询、实时播放会话、设备录像目录与回放、平台落盘录像、抓拍任务、云台控制、事件与订阅。AK/SK 保存在第三方业务后端，播放器只拿媒体地址。

开放平台默认关闭，需要先在现有数据库上执行专用迁移，再配置独立密钥环、Audience、Public URL 并启用。当前只支持单实例运行；不能直接通过增加应用副本共享 worker。需要应用级访问隔离时，还须按接入文档控制旧 API、媒体直连端口和内部 Webhook 的网络入口。

- [部署、应用授权与业务说明](./docs/open-platform/README.md)
- [签名协议](./docs/open-platform/signing-v1.md) · [OpenAPI 契约](./docs/open-platform/openapi.yaml)
- 本机公开开发文档：`/web/#/open-platform/docs`，无需管理员登录；业务接口仍分别鉴权。
- 本机管理 API 文档：`/swagger/index.html`；以当前代码与生成文档为准。

普通管理接口可通过 `[Server.HTTP].AuthURL` 对接外部认证；该机制与开放平台的 AK/SK 是不同入口，开放平台管理和版本升级等接口仍要求管理员 JWT。

## 录像、抓拍与运维约定

- **录像**：通用部署默认允许手动录像，但不自动为全部通道录像。手动开启后持续录制，关闭页面不会停止；没有录像等保活业务时，无人观看按配置延时断流，默认 180 秒。
- **清理**：录像按保留天数和所在文件系统的磁盘使用率清理。共享磁盘上其他业务的占用也可能触发清理，不能把它当作项目目录硬配额；失败批次无进展时退出并留待后续重试。
- **抓拍**：成功图片和定时计划持久保存；抓拍历史不随录像过期策略自动清理，须管理容量。数据库与 `cover/captures` 图片目录一起备份，详见[抓拍说明](./docs/channel-captures.md)。
- **AI**：`deploy/v1` 未编排 AI 服务，模板显式禁用。需要分析时另行部署服务并配置认证，不能仅打开开关就视为可用。
- **升级**：版本号由 `VERSION` 管理。网页签名升级依赖宿主机升级代理、签名密钥和配套编排，只适用于兼容且无需数据库迁移的发布，详见[签名升级说明](./deploy/192.168.6.20/UPGRADE.md)。
- **HTTP**：管理端保留 HTTP 下的加密登录及复制兼容。RSA 登录不替代 HTTPS 对会话和业务数据的保护；开放平台默认要求 HTTPS，HTTP 需显式启用。
- **备份**：保留数据库一致性备份、主配置、管理员凭据、开放平台密钥环及所需媒体文件；仅保存镜像不能恢复业务数据。真实 `.env`、密钥和数据库不进入源码仓库。

## 开发与目录

前端不再依赖单独下载上游 `www.zip`。本地开发可连接已有后端：

```bash
pnpm --dir admin-ui install --frozen-lockfile

# 替换为自己的后端；开发代理将 /api 前缀移除后转发。
VITE_DEV_API_TARGET=http://127.0.0.1:15123 pnpm --dir admin-ui dev
```

浏览器访问 Vite 输出的本地地址。后端独立开发可执行 `go run ./main.go -conf ./configs`，启动前按 [.env 示例](./.env.example) 将凭据和密钥注入**进程环境**，并配置可用的媒体服务和数据库；`go run` 不自动读取 `.env`。生产前端构建使用 `VITE_API_BASE_URL=/ pnpm --dir admin-ui build`，避免把开发用 `/api` 前缀打入生产包。

```text
admin-ui/              Vue 管理端、播放器集成与公开接入文档页面
internal/              应用装配、配置、设备/媒体/录像/抓拍/开放平台业务及 API
pkg/gbs/               国标 SIP、SDP、XML、事务与级联实现
pkg/zlm/、pkg/lalmax/   媒体服务适配
plugin/                监控与公共管理接口
tools/                 数据迁移、文档生成、签名发布包和升级代理
deploy/                通用编排、专用服务器配置与历史发布记录
docs/                  功能说明、接口契约、协议审计与联调文档
```

修改业务时按影响范围运行相关检查。前端 `build` 包含类型检查与开放文档一致性检查；开放平台契约变更后使用 `go run ./tools/openplatform-docs` 同步生成资源。真实设备接入、解码播放和公网路由仍需按现场条件验收。

## 文档导航

| 主题 | 文档 |
| --- | --- |
| 构建与首次部署 | [通用部署](./deploy/v1/README.md) |
| 现有服务器运维 | [专用运行说明](./deploy/192.168.6.20/README.md) · [部署方案](./deploy/server-192.168.6.20.md) |
| 数据库与迁移 | [PostgreSQL 与设备历史](./deploy/postgresql.md) |
| 媒体节点 | [管理与操作边界](./docs/media-node-management.md) · [接口](./docs/api/media-nodes.md) |
| 抓拍与计划 | [通道抓拍](./docs/channel-captures.md) |
| 三方接入 | [开放平台](./docs/open-platform/README.md) · [云台接口](./docs/api/ptz.md) |
| 国标兼容与验收 | [完成审计](./docs/testing/gb28181-completion-audit.md) · [设备矩阵](./docs/testing/gb28181-device-matrix.md) · [发布检查](./docs/testing/gb28181-release-validation.md) |
| 协议扩展 | [附录 G 实现](./docs/GB28181-annex-g-implementation.md) · [附录 O 设计](./docs/adr/gb28181-2014-direct-tcp-download.md) |
| 上游更新取舍 | [上游吸纳评估与实施记录](./docs/upstream-adoption-review-2026-11-10.md) |
| 公网接入规划 | [WSS 网关方案](./docs/国标公网接入网关-WSS方案.md)，方案与在研内容，以后续发布记录确认交付范围 |

## 上游来源与许可证

本项目基于 [gowvp/owl](https://github.com/gowvp/owl) 开发，保留原项目来源与版权声明。感谢 [ixugo](https://github.com/ixugo)、[panjjo/gosip](https://github.com/panjjo/gosip)、[goddd](https://github.com/ixugo/goddd)、[ZLMediaKit](https://github.com/ZLMediaKit/ZLMediaKit)、[Lalmax](https://github.com/q191201771/lalmax) 及相关开源贡献者。

上游修复按当前实现逐项评估和适配，相关决策记录在仓库文档中。本仓库采用 [GNU GPL v3.0](./LICENSE)；修改和分发须遵守许可证并保留版权声明。播放器、媒体服务及其他第三方组件还须遵守各自许可证与授权条款。
