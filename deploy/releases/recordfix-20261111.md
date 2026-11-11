# 国标设备录像查询错误响应兼容修复

## 原因与处理

目标海康设备 `gb80cj6`（DS-2CD3T47SWDV3-LT，2022 协议）经 B 网关查询通道 `chyajzi` 的本地录像时，约 40ms 内已返回 `RecordInfo / Result=ERROR / Reason=Cannot get record file`。A 端此前拒绝 Result 字段并回 SIP 400，随后错误地继续等到 `wait RecordInfo response timeout`。现场抓包证明 B 端双向转发正常。

设备状态返回 `record=OFF`，用户确认尚未配置本地录像或存储卡。因此本次修复设备失败响应的识别和反馈，不把失败伪装成空录像成功，也不调整设备录像、存储卡或协议版本。

新增受限失败信封解析，检查认证身份、目标归属、SN、查询及注册代次，成功写出 SIP 确认后才结束对应查询；旧响应不得结束同键新查询。成功、空结果及分包路径继续使用原处理。

## 检查

四版本（2011、2014、2016、2022）失败响应、重复/混合/未知字段拒绝、父设备别名、兄弟通道隔离、SIP 确认失败及同键新查询隔离等相关回归通过。相关 RecordInfo、RecordQuery、MultiResponse 和注册代次回归通过；Linux/amd64 静态构建及最终差异检查通过。四版本为代码回归，现场设备为当前海康 2022 固件。

## 产物与发布范围

交付目录：`build/deploy-v1/recordfix-20261111-180821/`，服务器对应 `/www2/gowvp-v1/packages/recordfix-20261111-180821/`，留存固定源码、摘要、测试日志、故障抓包及发布核对报告。

镜像：`owl-v1-service:v2.0.9-recordfix-20261111-180821`。后端构建标识 `recordfix-ec4663548e72`，对应固定源码清单，不是 Git 提交号。二进制 SHA256：`34d8b9d2e0d333b72411a3c35d0be071c8eb791f982d1dc5e04b966f10159f8f`。

只更新 A 端应用后端，基于 `v2.0.9-alarmui-20261111-171514` 镜像替换二进制；保留前端、配置、数据库、事件数据及 PostgreSQL/ZLM 容器。B 端无需更新。

2026-11-11 18:13:55 发布完成，镜像 ID `sha256:5f56e92726ba0b7dd2cd02e404e05edd0b76e7f57a57101c4cc852bb118fe974`。应用 healthy、重启计数 0；5 台设备全部恢复在线、129 个通道，目标设备网关入口有效，网关 connected/ready。发布前后前端、配置、数据库结构、网关配置与证书摘要，以及 PostgreSQL/ZLM 容器身份核对一致。报警事件待入库及失败任务均为 0。

线上按当天至当前时间分别执行 `type=all`、`type=time` 查询，87ms、84ms 即返回 HTTP 400 / ErrDevice / `设备录像查询失败：Cannot get record file`，不再等待超时。设备没有本地录像，故此结果是正确反馈真实失败，并不代表已经查到录像；配置存储卡和本地录像后再进行实际录像查询验收。

## 回滚

服务器备份目录：`/www2/gowvp-v1/backups/before-recordfix-20261111-180821/`。发布脚本持有 `/run/lock/owl-v1-release.lock`，校验基线、包摘要及配置，失败自动恢复旧镜像。手动回退需先确认没有后续发布并持有相同锁，恢复该备份 `.env`，以 Compose 项目 `owl-v1` 仅重建 `gowvp`；不恢复数据库覆盖新业务数据。
