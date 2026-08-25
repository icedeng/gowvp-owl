# GB/T 28181 1.0/1.1/2.0/3.0 实现完成度审计

审计时间：2026-08-25（Asia/Shanghai）

## 1. 审计结论

代码建设、自动化测试和本地协议模拟器已完成到开发计划的 AI-502；AI-403 的 2014 附录 O 裸 TCP 下载已经实现，不再是代码阻断项。2022 的设备升级、图像抓拍、目标跟踪、扩展配置写入、注册重定向、AAC/H.265 SDP 和主动视频上传通知已补齐代码闭环，其中升级与抓拍现在同时覆盖受理应答、最终通知和会话状态查询，不再把“命令已受理”误判为“业务已完成”。四版本除 REGISTER 外的 `Date + Note` 信令摘要已经形成请求/响应、时间窗、设备及上级独立 seed 的自动化闭环，但默认关闭，尚未经过真实设备和真实上级平台互通。

目标尚不能标记为生产完成，原因是 AI-503 真实设备矩阵和 AI-602 部署灰度/回滚必须在外部设备及目标环境执行。上下级平台级联的 UDP/TCP 注册、查询、目录、事件订阅、直播、回放、下载和语音广播/对讲代码链路已经补齐；2022 附录 H 指定路径级联已完成自动化闭环；上级 Catalog/Alarm/MobilePosition/PTZPosition 订阅现在会自动建立、续订、复用和取消下级订阅，但仍需要真实上级平台及真实设备共同验证。

## 2. 任务逐项证据

| 任务 | 状态 | 当前证据 |
|---|---|---|
| AI-000 基线 | 完成 | `docs/testing/gb28181-baseline.md` |
| AI-001 四版本夹具 | 完成 | `pkg/gbs/testdata/gb28181/{1.0,1.1,2.0,3.0}`；`TestGBVersionSIPFixturesParse`、`TestGBVersionXMLAndSDPFixtures` |
| AI-101 版本/能力矩阵 | 完成 | `pkg/gbs/version.go`；`version_test.go`、`version_gate_test.go` |
| AI-102 X-GB-Ver 1.1 | 完成 | SIP context/header 测试及四版本 SIP 夹具 |
| AI-103 持久化/手动覆盖 | 完成 | `DeviceExt` 版本档案、设备编辑输入、在线会话刷新；`device_version_test.go` |
| AI-104 REGISTER 协商 | 完成 | 401/200/失败响应平台 3.0、声明/有效版本；Contact `expires` 按 SIP 注册绑定语义覆盖全局 `Expires` 头；`register_version_test.go`、`gb10_core_flow_test.go` |
| AI-105 下行版本/探测 | 完成 | 下行使用有效版本；1.0 不发 ConfigDownload；版本夹具与门禁测试 |
| AI-201 1.0 门禁 | 完成 | Config、广播/对讲、RTP over TCP、IFrame、DragZoom 等能力门禁测试 |
| AI-202 1.0 SDP | 完成 | 统一 SDP Builder；直播/回放/下载 golden；Subject、u/t/y/f 测试 |
| AI-203 1.0 核心流程 | 完成（设备接入模拟器） | REGISTER→Keepalive→Catalog、RecordInfo、Alarm 流程测试；平台主动点播的 1.0 UDP SDP 由 golden 测试覆盖；SIP/TCP 支持大小写不敏感 `Content-Length`、紧凑头 `l` 及同连接连续报文分帧；ACK/BYE 从响应建立的路由集按 RFC 3261 反转 `Record-Route` 且不修改原响应 CSeq。已注册上级的入向 INVITE 进入级联 B2BUA，其他未知入向 INVITE 明确返回 501；`server_tcp_test.go`、`panic_fix_test.go` |
| AI-301 Catalog 扩展 | 完成 | 目录树五类节点、2014 字段、厂商 XML 保留；`catalog_extension_test.go` |
| AI-302 配置读写 | 完成 | ConfigDownload 支持 BasicParam、VideoParamOpt/Config、AudioParamOpt/Config、SVACEncode/DecodeConfig 多响应聚合；DeviceConfig 写入支持 BasicParam、结构化 VideoParamConfig/AudioParamConfig 及经过 XML 完整性/指令校验的 SVACEncodeConfig/SVACDecodeConfig，可组合下发并保留 BasicParam 兼容 API；Web/Core/Adapter/Swagger 已贯通；`config_11_test.go`、`gb_config_test.go`、`ipc_gb_config_test.go` |
| AI-303 MediaStatus | 完成 | Call-ID 关联、121、未知/重复幂等；`media_status_test.go`；并纳入 1.1 串联模拟流程 |
| AI-304 多响应 | 完成 | 乱序、重复、总数冲突、超时部分结果、同设备并发；`multi_response_test.go` |
| AI-305 目录/事件订阅 | 完成（自动化） | Catalog 初始、续订、取消、Event id、部分目录目标及真实 ADD/DEL/ON/OFF/UPDATE 增量；2011 Catalog 通知使用 `Response` 根，2014+ 使用 `Notify` 根；空消息体 terminated NOTIFY 正确确认、清理并在仍被上级引用时重订；Alarm 查询过滤参数、MobilePosition Interval、PTZPosition 版本门禁；上级 Catalog/Alarm/MobilePosition/PTZPosition 订阅自动桥接下级并按引用计数复用及释放，目录快照变化后重新计算 Catalog 下级来源；`subscribe_11_test.go`、`cascade_subscribe_test.go`、`cascade_query_test.go` |
| AI-401 广播时序 | 完成（自动化） | 接收者主动 INVITE；1.1 `PS/90000` 与 2.0/3.0 `PCMA/8000` 分派；ZLM `startSendRtp/stopSendRtp`；ACK/BYE 清理；`broadcast_11_test.go`、`pkg/zlm/rtp_test.go` |
| AI-402 技术验证 | 完成 | `docs/adr/gb28181-2014-direct-tcp-download.md`，已采用 Owl 内置客户端 |
| AI-403 裸 TCP 下载 | 完成（模拟器） | 独立 `tcp` SDP、文件客户端、进度/取消 API、SHA-256、原子落盘、大小/并发/超时/地址策略；`direct_tcp_*_test.go` |
| AI-404 媒体保活 | 完成 | ZLM/LALMAX 流事件收敛、MediaStatus/BYE 竞争；无持久化通道记录的 `cascade-*` 指定路径/语音级联流也能接收注册与注销事件并释放会话；LALMAX 新旧响应结构兼容，通过 `stat/group` 提供媒体源状态、轨道和下载进度，并通过 `stop_rtp_pub` 幂等释放 RTP 接收会话；`media_lifecycle_test.go`、`zlm_webhook_gb_test.go`、`driver_lalmax_test.go`、`pkg/lalmax/rtp_test.go`、`pkg/lalmax/stat_test.go` |
| AI-501 2.0/3.0 回归 | 完成（自动化） | RTP over TCP、双向对讲 RTP；2022 升级覆盖 `DeviceUpgrade` 受理、`DeviceUpgradeResult` 最终结果和状态查询；抓拍覆盖 `SnapShotConfig`、受约束 HTTP 上传、`UploadSnapShotFinished` 最终通知和状态查询；另含 TargetTrack、PTZ 精准控制、存储卡、VideoUploadNotify、A.4、H.265/AAC SDP 及八类 2022 DeviceConfig 写入；`version_regression_test.go`、`upgrade_test.go`、`img_test.go`、`voice_talk_test.go` |
| AI-502 诊断/API | 完成 | 有效版本、来源、过滤后的能力、设备级 `gb_disabled_capabilities`、最近拒绝；手动版本设置/清除；下载状态、取消和灰度指标 API |
| AI-503 真实设备矩阵 | 待外部执行 | `docs/testing/gb28181-device-matrix.md` 提供证据模板，`docs/testing/gb28181-interoperability-runbook.md` 提供逐步执行与回滚手册 |
| AI-601 发布候选验证 | 完成（自动化） | 全仓 test/vet/race 和补丁检查通过；外部真实设备与部署验收归入 AI-503/AI-602 |
| AI-602 灰度/回滚 | 待目标环境执行 | 已具备按设备手动版本覆盖、诊断、`GET /gb28181/metrics` 和关闭下载即取消活动任务；尚无目标环境指标及回滚演练证据 |

## 3. 上下级平台级联增量审计

| 能力 | 状态 | 当前证据 |
|---|---|---|
| 多上级注册 | 完成（自动化） | `SIPUpstream.transport` 支持 UDP/TCP 且空值默认 UDP；TCP 持久连接覆盖 REGISTER→401 Digest→认证 REGISTER→Keepalive、断线重连事务换连接和入向身份绑定；Digest/qop、续注册、注销、退避、热更新、实际 Expires；2022 301/302 支持 UDP/TCP 重定向并校验 Contact/ServerID/传输；`cascade_test.go`、`server_tcp_test.go` |
| `Date + Note` 信令摘要 | 完成（自动化，默认关闭） | 除 REGISTER 请求/响应外，级联 MESSAGE/Keepalive 在写出前签名并校验响应；入向已注册上级按 `SignalDigestSeed → Password → 全局 Seed` 选择 seed，设备按设备独立密码优先；Required 模式丢弃无签名、超时或正文被篡改的响应且事务不阻塞；`cascade_test.go`、`signal_digest_test.go`、`sip/signal_digest_test.go` |
| 查询与目录 | 完成（自动化） | 平台与共享通道 DeviceInfo/DeviceStatus、共享通道 RecordInfo 下级查询及响应编码映射；可信 NVR 可代表其已知子通道返回 DeviceInfo，通道元数据单独落库且不会覆盖父设备，非法跨设备编码仍拒绝；按上级版本安全转发 1.1 PresetQuery、2.0 HomePositionQuery/MobilePosition、3.0 CruiseTrackListQuery/CruiseTrackQuery/PTZPosition/SDCardStatus；巡航轨迹编号、列表、预置位、停留时间和速度结构化解析；上下级 SN 独立、响应恢复上级 SN，DeviceID/ParentID 映射且未知下级编码不透传，多上级并发响应隔离；Catalog 支持平台根或单个共享通道目标、共享白名单、20 条查询分包和版本化 Info；录像列表 20 条分包且不泄露下级编码；`cascade_query_test.go`、`device_info_channel_test.go` |
| 设备控制 | 完成（共享通道安全子集） | PTZ、录像控制及版本匹配的 IFrame/DragZoom/HomePosition/精准 PTZ/TargetTrack 转发下级并映射业务响应；TargetTrack 仅允许控制当前共享通道，`DeviceID2` 会映射为真实下级编码，禁止借此越权控制未共享通道；校验 PTZ 校验和、上下级版本与设备能力关闭项；重启、布撤防、报警复位、格式化等设备级操作不允许经仅共享通道越权执行；`cascade_control_test.go` |
| 订阅通知 | 完成（自动化） | 1.1+ Catalog 初始通知仅发送离线/异常目录并携带 `Status=OK`，默认 Expires 600 秒；1.0、Alarm、MobilePosition、PTZPosition 不误发初始 Catalog；2011 Catalog 变化通知使用 `Response` 根，2014+ 使用 `Notify` 根；平台根和单个共享通道订阅分别维护可见目录快照，只发送 ADD/DEL/ON/OFF/UPDATE 增量，单包 `SumNum` 与 `DeviceList Num` 一致；同一订阅的增量计算、分包发送和快照提交串行执行，失败不提交快照且重试补发；刷新保留原对话与 NOTIFY CSeq，未重复携带 X-GB-Ver 时复用注册协商版本；空消息体 terminated NOTIFY 返回 200、清理出向对话，并在仍有级联引用时自动重订；上级 Catalog 订阅自动映射到承载共享通道的下级 NVR，目录快照变化后重新计算来源并为新增或迁移通道补订阅；Catalog/Alarm/MobilePosition/PTZPosition 下级订阅支持续订、退订、过期/上级移除清理和多上级引用计数，续订与过期清理串行收敛；Alarm 完整支持 Start/EndAlarmPriority、AlarmMethod、AlarmType、Start/EndAlarmTime 过滤，兼容 2011 示例的 StartTime/EndTime 别名；事件通知按平台级、指定共享通道和过滤条件二次筛选并映射编码；2.0 起支持 AlarmType，3.0 支持 PTZ 精准位置变化订阅/通知且 1.0/1.1/2.0 明确拒绝；非标准级联订阅明确拒绝；`cascade_subscribe_test.go`、`cascade_query_test.go`、`subscribe_11_test.go` |
| 实时点播 | 完成（自动化） | 1.0/1.1 UDP，2.0/3.0 UDP/TCP；ZLM 转发、ACK/BYE/CANCEL 和引用计数；3.0 指定路径使用独立媒体源与 RTP 流并按同一路径会话释放；对话内请求绑定发起该会话的已注册上级，其他上级不能确认或终止会话；`cascade_media_test.go` |
| 历史回放/下载 | 完成（自动化） | 独立时间段及指定路径媒体源、Playback/Download SDP、下载倍速/文件大小；四版本 `INVITE→ACK→INFO→BYE` 串联回归，3.0 下载覆盖指定路径转发与响应确认；MANSRTSP/RTSP 版本转换；普通和级联控制 CSeq 原子递增；资源关闭；`cascade_media_test.go`、`history_control_test.go` |
| 语音广播/对讲级联 | 完成（自动化） | 按附录 Q/9.12 转发上级 Broadcast 通知，主动建立上游语音源 INVITE（支持 Digest）与 ZLM 接收流，再通知真实下级并处理接收方主动 INVITE；1.1 上游/下游使用 PS/90000，2.0/3.0 使用 PCMA/8000，跨版本由 ZLM 解封装/重封装；上下游 SN、SourceID/TargetID 隔离，多通道按 Subject 接收者定位；ACK/CANCEL/双侧 BYE、媒体注销和关闭均幂等释放。语音对讲按 2016 的“实时视音频点播上行 + 语音广播下行”组合实现；`cascade_voice_test.go`、`cascade_version_matrix_test.go` |
| 2022 扩展级联 | 完成（自动化） | 附录 H `X-PreferredPath` 按首跳消费并向下转发剩余路径，`X-RoutePath` 前置本平台编码返回；路径平台编码严格校验为 20 位数字及类型码 `200`，拒绝重复、错误首跳、响应不匹配和路由环；指定路径媒体源按路径隔离，未使用路径时兼容历史非 `200` 本地编码。CruiseTrackListQuery、CruiseTrackQuery、PTZPosition、SDCardStatus 按 3.0 门禁安全转发并重写 SN/DeviceID/ParentID；TargetTrack 只在上下游均为 3.0 且目标是当前共享通道时转发；3.0 PTZ 精准位置变化事件支持自动下级订阅和安全通知级联；ConfigDownload 使用标准 `SnapShotConfig`，同时接收旧厂商 `SnapShot` 别名；A.4 是扩展应用数据对象类型定义而非独立查询命令，已按真实承载路径支持 3.0 Catalog ExtraInfo、Alarm/MobilePosition 嵌套对象和 3.0 DeviceStatus 下级响应。共享对象编码递归映射、ParentID/下级父设备映射为本级平台编码，字符串及数值型 20 位对象编码均保留精度并执行映射，未知编码拒绝整条响应/通知；JSON 数字和数组不丢值；补正标准 `behavioralEventType` 并兼容旧厂商别名；`cascade_path.go`、`cascade_a4.go`、`cascade_path_test.go`、`cascade_query_test.go`、`cascade_subscribe_test.go`、`cascade_control_test.go` |
| 真实平台互通 | 待外部执行 | 需要 1.0/1.1/2.0/3.0 上级平台分别验证注册、目录、直播、回放、下载和订阅；3.0 还需使用至少三层平台验证附录 H 指定路径 |

四版本级联自动化矩阵由 `cascade_version_matrix_test.go` 固化，统一检查 REGISTER 版本、Catalog 字段、UDP/TCP 媒体、下载倍速、语音载荷、MANSRTSP/RTSP 控制和订阅能力。`cascade_media_test.go` 进一步使用四版本完整历史对话验证媒体启动、控制、终止和资源回收，并验证两个已注册上级之间的对话所有权隔离；`cascade_voice_test.go` 验证广播的上下游完整 B2BUA、Digest、编码映射、会话所有权及双侧清理。SIP 配置 Swagger 已包含上级平台完整字段；配置 API 的 Duration 同时接受历史纳秒整数和 `60s` 形式字符串。

## 4. 阶段门

| 阶段门 | 结果 |
|---|---|
| G0 版本映射/夹具 | 自动化证据通过 |
| G1 四版本 REGISTER/未知版本保守处理 | 自动化证据通过 |
| G2 1.0 模拟核心流程 | 模拟器通过；真实设备项待 AI-503 |
| G3 1.1 信令和数据 | 模拟器通过；新增 Keepalive→Catalog→DeviceConfig→MediaStatus 串联回归 |
| G4 1.1 广播和下载 | 广播 SIP/SDP/RTP 状态机与下载模拟器通过；真实设备音频和下载兼容待 AI-503 |
| G5 生产发布 | 未通过：设备矩阵、目标环境灰度和回滚未完成 |

## 5. 最近验证

通过：

```bash
GOCACHE=/tmp/owl-gocache go test ./pkg/gbs/... ./internal/conf ./internal/core/ipc ./internal/adapter/gbadapter ./internal/web/api -count=1
GOCACHE=/tmp/owl-gocache go test -race -vet=off ./pkg/gbs/... ./internal/adapter/gbadapter ./internal/core/ipc ./internal/web/api -count=1
GOCACHE=/tmp/owl-gocache go test ./... -count=1
GOCACHE=/tmp/owl-gocache go vet ./...
git diff --check
```

本轮未执行管理端构建，不能用上一次构建结果替代当前证据。

## 6. 已知边界与剩余问题

| 项目 | 当前状态 | 处理结论 |
|---|---|---|
| SIP REGISTER 口令摘要认证 | 已加固并有自动化证据 | nonce 由服务端使用密码学安全随机源签发，绑定设备与源 IP、5 分钟失效且有界保存；首次成功后仅允许同 Call-ID/CSeq/摘要的 UDP 幂等重传，拒绝跨请求重放、任意 nonce、错误 realm/username/URI 和算法混淆；服务端保持 MD5 设备兼容，级联客户端支持 MD5/SHA-1/SHA-256 并明确拒绝未知算法；`register_version_test.go`、`sip/auth_test.go`、`cascade_test.go` |
| `Date + Note` 信令数字摘要 | 已形成自动化闭环，待真实互通 | 按 `From + To + Call-ID + Date + seed + 消息体` 计算；Date 使用北京时间并校验可配置时间窗；支持 MD5/SHA-1/SHA-256，nonce 默认 Base64 输出，可配置 hex 并可兼容接收标准示例中的十六进制形式；REGISTER 请求/响应免签。`Enabled` 启用出站签名，并对入向已携带 Note 的报文验签；`Required` 还会拒绝缺失或验签失败的入向报文；默认关闭以保持旧设备兼容。`RequireMessageAuth` 仍是另一套 RFC Digest 兼容开关，二者不能混同。2022 建议的 SM3 尚未实现；`signal_digest.go`、`signal_digest_test.go`、`sip/signal_digest_test.go`、`cascade_test.go` |
| 数字证书双向认证、CRL | 未实现完整协议闭环 | 高安全级别专项，不能宣称支持 |
| `Monitor-User-Identity` 跨域身份头 | 未实现完整生成、逐级追加和验证 | 与安全路由网关身份体系一起设计，不能仅做字符串透传后宣称完成 |
| NTP 服务端/客户端 | 项目未内置 NTP 服务 | 标准要求 IP 接入设备通过 REGISTER `200 OK` 的 `Date` 校时，代码已实现；NTP 是可配置部署能力，不把非标准主动 `DeviceControl(Time)` 当成 NTP 完成证据 |
| 升级/抓拍状态持久化 | 当前为有界内存状态 | 进程重启后可接收合法最终通知并重建终态，但重启前的 accepted/uploading 中间态不会恢复 |
| 真实设备、真实上级和三级路径 | 未执行 | 包括 `Date + Note` 的 Base64/hex、算法和 seed 协商互通；仍是生产完成阶段门，不得由模拟器结果替代 |

## 7. 生产完成所需证据

1. 按设备矩阵完成 1.0 两厂商、1.1 两厂商、2.0 一厂商、3.0 一厂商联调；
2. 每台设备归档厂商、型号、固件、网络模式、脱敏 SIP 报文、语音 RTP 收发证据、文件 SHA-256 和结果；
3. 在最终发布候选提交上重跑全仓 test/vet/race；
4. 白名单灰度观察注册失败率、目录超时率、播放成功率、下载失败率和异常断流；
5. 演练按设备清除/设置手动版本、关闭直接 TCP 开关和回退发布版本。
