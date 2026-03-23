# AI Secondary Development Notice

本项目基于上游开源项目 [gowvp/owl](https://github.com/gowvp/owl) 进行 AI 二次开发与协议能力扩展。

## 基本说明

- 上游项目地址：<https://github.com/gowvp/owl>
- 当前项目在保留原有基础能力的前提下，结合 AI 辅助进行了面向 GB28181、ONVIF、抓拍、事件、部署交付等方向的定制开发
- 如需追溯基础实现、原始设计或上游更新，请以上游仓库为准
- 本说明主要记录当前分支相对上游/`main` 的重要二开内容，便于后续接手、联调、回归与继续开发

## 本轮新增或增强的功能

### 1. GB28181 设备与通道能力补全

- 新增 GB28181 设备控制接口，支持附录 A.2.3 相关能力调用
- 新增 GB28181 设备查询接口，支持附录 A.2.4 查询能力
- 新增附录 A.4 快照结果查询接口
- 新增通道录像目录查询能力
- 新增通道 PTZ 控制能力
- 新增通道 PTZ 探测能力
- 新增设备升级能力，面向 GB28181-2022 扩展场景
- 新增历史回放/下载开始、停止、控制能力
- 新增语音对讲/语音广播开始、停止能力
- 新增设备校时能力
- 新增设备订阅能力
- 新增 OPTIONS 探测能力
- 新增设备级 PTZ 能力探测入口
- 补全 GB 适配层实现，打通 API -> IPC Core -> GB Adapter -> SIP/GBS 调用链

### 2. ONVIF 能力增强

- 新增 ONVIF PTZ 控制实现
- 对统一 PTZ 接口做协议侧兼容，允许 GB28181 与 ONVIF 共用控制入口

### 3. 抓拍链路增强

- 增加 GB28181 抓拍回传地址独立配置 `Media.GBSnapshotBaseURL`，避免与普通媒体回调地址混用
- 优化 GB28181-2022 无推流抓拍流程，支持设备通过 `DeviceConfig + SnapShotConfig` 直接回传图片
- 将抓拍 `UploadURL` 从 query 参数改为路径参数，提升对部分设备厂商实现的兼容性
- 修复抓拍图片回传后的落盘键不一致问题，确保平台读取到的是最新抓拍封面
- 增强 `/gb28181/snapshot` 回调处理，兼容设备直接上传原始图片流与 `multipart/form-data` 两种格式
- 增加抓拍上传成功日志标识 `GBSNAPSHOT_UPLOAD_OK`，便于排查抓拍链路问题
- 优化抓拍接口等待逻辑，尽量在图片真正落盘后再返回封面访问地址
- 优化封面输出时的内容类型识别，避免固定按 `image/jpeg` 返回导致的兼容问题

### 4. 设备编号与通道编号兼容调用

- 设备接口新增路径参数兼容逻辑：优先按内部 `id` 查询，查不到再按设备编号 `device_id` 查询
- 设备兼容逻辑已接入设备详情、编辑、删除、目录查询、校时、订阅、OPTIONS 探测、GB 控制、GB 查询、A.4 快照等接口
- 新增通道兼容路由组：`/gb28181/devices/{device_id}/channels/{channel_id}/...`
- 新增兼容路由组支持的能力包括：编辑、播放、录像目录查询、PTZ、PTZ 探测、升级、历史会话、语音、抓拍、区域、AI 开关、录像模式
- 新增 `/channels/gb28181/{device_id}/{channel_id}/...` 风格的兼容接口，便于使用国标编码直接调用
- 通道兼容接口内部统一通过 `device_id + channel_id` 解析为内部通道 `id` 后再复用原有业务逻辑，避免直接依赖通道编号单列查询
- 旧通道接口 `/channels/{id}/...` 继续保留，语义仍然是传内部通道 `id`

### 5. 录像与录制相关能力增强

- 新增通道未设置录像模式时的默认配置项 `Server.Recording.DefaultMode`
- 默认录像模式支持 `always`、`ai`、`none`
- 启动时增加默认录像模式校验与兜底，非法值自动回退为 `always`
- 录制相关接口与 ZLM 回调联动，继续沿用统一录制入库逻辑
- 保留录制列表、时间轴、月度统计、下载与 HLS 播放能力，并补充了接口文档说明

### 6. SIP 安全、传输与日志能力增强

- 新增 SIP-TLS 配置项：`Sip.EnableTLS`、`Sip.TLSPort`、`Sip.TLSCert`、`Sip.TLSKey`
- 新增源地址严格校验配置 `Sip.StrictSourceCheck`
- 新增 `MESSAGE/NOTIFY` Digest 鉴权开关 `Sip.RequireMessageAuth`
- 新增 PTZ 弱确认模式 `Sip.PTZWeakConfirm`
- 增加 SIP 独立报文日志能力，支持按配置开启、按时间/文件大小滚动、独立目录存储
- 增加相关配置默认值与配置文件示例

### 7. 报警与事件中心打通

- 增加 GB 报警桥接逻辑，将 SIP 层报警上报转换为平台标准事件
- 报警事件自动写入 `events` 表，便于统一查询、展示、审计与后续联动
- 事件标签支持按报警方式细分，如 `gb_alarm_x`
- 原始报警数据序列化保存在事件字段中，便于问题追踪与二次分析

### 8. Swagger / OpenAPI 文档增强

- 接入 Swagger UI 路由 `/swagger/*any`
- 新增 `docs/swagger.json`、`docs/swagger.yaml`、`docs/docs.go`
- 为系统、设备、通道、录像、AI webhook、ZLM webhook、事件、配置等接口补充 Swagger 注释
- 为部分请求/响应结构补充示例字段与说明，降低联调成本

### 9. 部署与构建能力增强

- Makefile 新增构建 ARM64 镜像的目标
- Makefile 新增融合 ZLMediaKit 镜像并导出 `tar` 的目标，支持 `amd64/arm64`
- `Dockerfile_zlm` 重构为多阶段构建，兼容最新 ZLMediaKit 运行环境
- 修复 ZLMediaKit 更新后融合镜像构建成功但运行失败的问题
- 补充 Python 与相关运行时依赖，适配当前镜像中的 ZLM 运行需求

## 本轮修复与兼容改进

- 修复未鉴权数据被写入数据库的问题
- 修复 PTZ 控制场景下，设备未返回成功应答导致平台误判失败的问题
- 增加 PTZ 弱确认配置，允许针对弱实现设备按“命令已成功发送”处理
- 增加基于设备编号 `device_id` 的兼容查询逻辑，降低外部系统对内部主键的依赖
- 优化部分 GB28181 补充协议和未完成能力的实现
- 增加 `panic` 相关防护测试与 SIP 报文日志辅助排查能力

## 对外接口变化说明

- 新增 Swagger 文档访问入口：`/swagger/index.html`
- 新增设备级 GB 控制接口：`/devices/{id}/gb/control`
- 新增设备级 GB 查询接口：`/devices/{id}/gb/query`
- 新增设备级 A.4 结果接口：`/devices/{id}/gb/a4_snapshot`
- 新增通道录像目录查询接口：`/channels/{id}/records/query`
- 新增通道 PTZ 接口：`/channels/{id}/ptz`
- 新增通道 PTZ 探测接口：`/channels/{id}/ptz_probe`
- 新增通道升级接口：`/channels/{id}/upgrade`
- 新增历史会话接口：`/channels/{id}/history/start`、`/stop`、`/control`
- 新增语音会话接口：`/channels/{id}/voice/start`、`/stop`
- 新增设备校时接口：`/devices/{id}/time_sync`
- 新增设备订阅接口：`/devices/{id}/subscribe`
- 新增设备 OPTIONS 探测接口：`/devices/{id}/options_probe`
- 新增基于 `device_id + channel_id` 的兼容调用路由

## 后续开发约定

- 设备类路径参数 `:id` 默认允许调用方传内部 `id` 或 GB 设备编号 `device_id`
- 通道类兼容接口优先使用 `device_id + channel_id` 组合，不再依赖“20 位纯数字”之类的弱规则猜测标识类型
- 若后续新增设备维度接口，默认复用 `internal/core/ipc/device_lookup.go` 中的设备解析逻辑
- 若后续新增 GB28181 通道维度接口，优先同步补充到兼容路由组，保持能力对齐
- 若后续继续扩展 PTZ、历史回放、语音、订阅等能力，优先走统一 `IPC Core + Adapter` 端口模型，避免直接在 Web API 层堆协议分支
- 若后续新增抓拍相关能力，需同时考虑设备直传、multipart 上传、旧图等待、内容类型识别等兼容场景
- 若后续新增对外 HTTP 接口，默认同步补充 Swagger 注释并刷新 `docs` 产物

## 风险与限制

- 部分 GB28181 补充协议与设备厂商实现差异较大，当前代码已增强兼容，但仍需以真实设备联调结果为准
- 历史回放、升级、语音、订阅等能力依赖设备侧支持情况，不保证所有设备可用
- PTZ 弱确认模式适合兼容部分不规范设备，但会降低“设备已明确成功应答”的判断严格性
- 抓拍链路依赖设备回传与网络可达性，如设备无法主动回传图片，仍可能抓拍失败
- Swagger 文档已纳入当前分支，但后续若继续改接口且未刷新 `docs` 产物，文档与实现可能再次出现偏差
