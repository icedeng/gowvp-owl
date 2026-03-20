# AI Secondary Development Notice

本项目基于上游开源项目 [gowvp/owl](https://github.com/gowvp/owl) 进行 AI 二次开发。

说明：

- 上游项目地址：<https://github.com/gowvp/owl>
- 当前项目在保留原有基础能力的前提下，结合 AI 辅助进行了定制开发与功能扩展
- 如需追溯基础实现、原始设计或上游更新，请以上游仓库为准

本轮通过 AI 增加或增强的功能：

- 增加 GB28181 抓拍回传地址独立配置 `Media.GBSnapshotBaseURL`，避免与普通媒体回调地址混用
- 优化 GB28181-2022 无推流抓拍流程，支持设备通过 `DeviceConfig + SnapShotConfig` 直接回传图片
- 将抓拍 `UploadURL` 从 query 参数改为路径参数，提升对部分设备厂商实现的兼容性
- 修复抓拍图片回传后的落盘键不一致问题，确保平台读取到的是最新抓拍封面
- 增强 `/gb28181/snapshot` 回调处理，兼容 `multipart/form-data` 上传并提取真实图片内容
- 增加 SIP 独立报文日志能力，支持按配置开启、按时间/文件大小滚动、独立文件存储
- 增加抓拍上传成功日志标识 `GBSNAPSHOT_UPLOAD_OK`，便于排查抓拍链路问题
- 优化抓拍接口等待逻辑，尽量在图片真正落盘后再返回封面访问地址
- 优化封面输出时的内容类型识别，避免固定按 `image/jpeg` 返回导致的兼容问题
- 设备接口新增路径参数兼容逻辑：优先按内部 `id` 查询，查不到再按设备编号 `device_id` 查询
- 设备兼容逻辑已接入设备详情、编辑、删除、目录查询、校时、订阅、OPTIONS 探测、GB 控制、GB 查询、A.4 快照等接口
- 新增 GB28181 通道兼容路由组：`/gb28181/devices/{device_id}/channels/{channel_id}/...`
- 新增通道兼容路由支持的能力包括：编辑、播放、录像目录查询、PTZ、PTZ 探测、升级、历史会话、语音、抓拍、区域、AI 开关、录像模式
- 通道兼容接口内部统一通过 `device_id + channel_id` 解析为内部通道 `id` 后再复用原有业务逻辑，避免直接依赖通道编号单列查询
- 旧通道接口 `/channels/{id}/...` 继续保留，语义仍然是传内部通道 `id`

后续开发约定：

- 设备类路径参数 `:id` 默认允许调用方传内部 `id` 或 GB 设备编号 `device_id`
- 通道类新兼容接口优先使用 `device_id + channel_id` 组合，不再依赖“20 位纯数字”之类的弱规则猜测标识类型
- 若后续新增设备维度接口，默认复用 `internal/core/ipc/device_lookup.go` 中的设备解析逻辑
- 若后续新增 GB28181 通道维度接口，优先同时补充到 `/gb28181/devices/{device_id}/channels/{channel_id}/...` 路由组
- 当前变更已完成代码级兼容，但 Swagger 生成产物未同步刷新；如需对外文档一致，需额外更新 `docs` 相关文件
