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
