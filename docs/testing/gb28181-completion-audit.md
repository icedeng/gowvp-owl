# GB/T 28181 1.0/1.1/2.0/3.0 实现完成度审计

审计时间：2026-08-25（Asia/Shanghai）

## 1. 审计结论

代码建设、自动化测试和本地协议模拟器已完成到开发计划的 AI-502；AI-403 的 2014 附录 O 裸 TCP 下载已经实现，不再是代码阻断项。2022 的设备升级、图像抓拍、目标跟踪、扩展配置写入、注册重定向、AAC/H.265 SDP 和主动视频上传通知已补齐代码闭环，其中升级与抓拍现在同时覆盖受理应答、最终通知和会话状态查询，不再把“命令已受理”误判为“业务已完成”。四版本除 REGISTER 外的 `Date + Note` 信令摘要已经形成请求/响应、时间窗、设备及上级独立 seed 的自动化闭环，但默认关闭，尚未经过真实设备和真实上级平台互通。SIP URI/地址解析已补齐短输入、纯空白、缺失闭合符、空主机及括号 IPv6 的边界处理；畸形报文返回解析错误，不再触发切片越界。SSRC 生成会校验 10 位数字域编码，并将配置错误显式返回给直播、回放、下载、广播、对讲和级联媒体调用链。SIP 配置加载和 API 更新现在统一校验平台 ID、有效域、监听端口、TLS 必填项、历史范围及信令摘要；配置先原子写盘再提交运行时快照，失败不再污染内存。监听地址、平台身份、TLS 和报文日志等不能安全热更新的字段会明确要求重启，不再返回“成功”但继续使用旧监听器；运行中的 SIP 请求通过受锁快照读取配置，避免更新时发生数据竞争。

目标尚不能标记为生产完成，原因是 AI-503 真实设备矩阵和 AI-602 部署灰度/回滚必须在外部设备及目标环境执行。上下级平台级联的 UDP/TCP 注册、查询、目录、事件订阅、直播、回放、下载和语音广播/对讲代码链路已经补齐；2022 附录 H 指定路径级联已完成自动化闭环；上级 Catalog/Alarm/MobilePosition/PTZPosition 订阅现在会自动建立、续订、复用和取消下级订阅，但仍需要真实上级平台及真实设备共同验证。

## 2. 任务逐项证据

| 任务 | 状态 | 当前证据 |
|---|---|---|
| AI-000 基线 | 完成 | `docs/testing/gb28181-baseline.md` |
| AI-001 四版本夹具 | 完成 | `pkg/gbs/testdata/gb28181/{1.0,1.1,2.0,3.0}`；`TestGBVersionSIPFixturesParse`、`TestGBVersionXMLAndSDPFixtures` |
| AI-101 版本/能力矩阵 | 完成 | `pkg/gbs/version.go`；`version_test.go`、`version_gate_test.go` |
| AI-102 X-GB-Ver 1.1 | 完成 | SIP context/header 测试及四版本 SIP 夹具 |
| AI-103 持久化/手动覆盖 | 完成 | `DeviceExt` 版本档案、设备编辑输入、在线会话刷新；`device_version_test.go` |
| AI-104 REGISTER 协商 | 完成 | 401/200/失败响应平台 3.0、声明/有效版本；Contact `expires` 按 SIP 注册绑定语义覆盖全局 `Expires` 头；同设备 REGISTER 的查询、鉴权、首次建档和状态提交按设备串行，不同设备仍可并行，多实例首次建档唯一键竞争回读胜出记录而不误报 500；`register_version_test.go`、`gb10_core_flow_test.go`、`ipc/protocol_test.go` |
| AI-105 下行版本/探测 | 完成 | 下行使用有效版本；1.0 不发 ConfigDownload；版本夹具与门禁测试 |
| AI-201 1.0 门禁 | 完成 | Config、广播/对讲、RTP over TCP、IFrame、DragZoom 等能力门禁测试 |
| AI-202 1.0 SDP | 完成 | 统一 SDP Builder；直播/回放/下载 golden；Subject、u/t/y/f 测试 |
| AI-203 1.0 核心流程 | 完成（设备接入模拟器） | REGISTER→Keepalive→Catalog、RecordInfo、Alarm 流程测试；Keepalive 在在线状态落定后先返回 `200 OK`，再转发 DeviceStatus NOTIFY 和执行首次 Catalog 补载，慢订阅方不再拖延心跳确认；Alarm、MobilePosition、DeviceInfo、ConfigDownload 及通用查询响应同样先确认入向 SIP 报文、后执行外部回调/订阅转发，避免慢消费方触发设备重传；平台主动点播的 1.0 UDP SDP 由 golden 测试覆盖；SIP/TCP 支持大小写不敏感 `Content-Length`、紧凑头 `l` 及同连接连续报文分帧，连接断开时解析器关闭输出通道并终止对应 handler goroutine，不再按设备重连次数泄漏；单头行、总头、正文分别限制为 8 KiB、64 KiB、1 MiB，超限长度、重复长度和截断正文在分配/分派前拒绝，UDP Content-Length 同样在正文分配前受限。缺失 From/Via 等路由必需头的入向请求在进入业务 handler 前返回 400。ACK/BYE 从响应建立的路由集按 RFC 3261 反转 `Record-Route` 且不修改原响应 CSeq；设备 2xx 缺失 Contact 时回退 To，缺失 To/From/Via/Call-ID/CSeq 时向直播、回放、下载、广播和对讲调用链返回错误，不再构造半有效请求或 panic。短 URI、纯空白地址、缺失 `>`、空主机等畸形输入返回错误，合法裸地址与括号 IPv6 保持可解析，并有模糊测试防止解析 panic。已注册上级的入向 INVITE 进入级联 B2BUA，其他未知入向 INVITE 明确返回 501；`server_tcp_test.go`、`panic_fix_test.go`、`parser_robustness_test.go`、`subscribe_11_test.go` |
| AI-301 Catalog 扩展 | 完成 | 目录树五类节点、2014 字段、厂商 XML 保留；`catalog_extension_test.go` |
| AI-302 配置读写 | 完成 | ConfigDownload 支持 BasicParam、VideoParamOpt/Config、AudioParamOpt/Config、SVACEncode/DecodeConfig 多响应聚合；DeviceConfig 写入支持 BasicParam、结构化 VideoParamConfig/AudioParamConfig 及经过 XML 完整性/指令校验的 SVACEncodeConfig/SVACDecodeConfig，可组合下发并保留 BasicParam 兼容 API；Web/Core/Adapter/Swagger 已贯通；`config_11_test.go`、`gb_config_test.go`、`ipc_gb_config_test.go` |
| AI-303 MediaStatus | 完成 | Call-ID 关联、121、未知/重复幂等；`media_status_test.go`；并纳入 1.1 串联模拟流程 |
| AI-304 多响应 | 完成 | 乱序、重复、总数冲突、超时部分结果、同设备并发；服务关闭会并发幂等地关闭 Catalog/RecordInfo 聚合器并立即唤醒等待者，关闭后拒绝创建新聚合项；XML 的 UTF-8→GBK/GB18030 兼容回退使用临时对象原子解码，首轮失败的部分目录/录像切片不会残留并在第二轮重复追加，完全失败也不污染调用方目标；`multi_response_test.go`、`sip/xml_decode_test.go` |
| AI-305 目录/事件订阅 | 完成（自动化） | Catalog 初始、续订、取消、Event id、部分目录目标及真实 ADD/DEL/ON/OFF/UPDATE 增量；2011 Catalog 通知使用 `Response` 根，2014+ 使用 `Notify` 根；空消息体 terminated NOTIFY 正确确认、清理并在仍被上级引用时重订；Alarm 查询过滤参数、MobilePosition Interval、PTZPosition 版本门禁；上级 Catalog/Alarm/MobilePosition/PTZPosition 订阅自动桥接下级并按引用计数复用及释放，目录快照变化后重新计算 Catalog 下级来源；`subscribe_11_test.go`、`cascade_subscribe_test.go`、`cascade_query_test.go` |
| AI-401 广播时序 | 完成（自动化） | 接收者主动 INVITE；1.1 `PS/90000` 与 2.0/3.0 `PCMA/8000` 分派；ZLM `startSendRtp/stopSendRtp`；ACK/BYE 清理；`broadcast_11_test.go`、`pkg/zlm/rtp_test.go` |
| AI-402 技术验证 | 完成 | `docs/adr/gb28181-2014-direct-tcp-download.md`，已采用 Owl 内置客户端 |
| AI-403 裸 TCP 下载 | 完成（模拟器） | 独立 `tcp` SDP、文件客户端、进度/取消 API、SHA-256、原子落盘、大小/并发/超时/地址策略；终态按 `RetainDays` 和 4096 条上限回收，独立生命周期清理器保证无新下载时仍清理状态及文件，活动任务及其 `.part` 文件不会被过期清理误删；`direct_tcp_*_test.go`、`server_lifecycle_test.go` |
| AI-404 媒体保活 | 完成 | ZLM/LALMAX 流事件收敛、MediaStatus/BYE 竞争；直播/历史 RTP 端口打开失败不再遗留占位流，停止未完成历史会话也会删除占位状态；GB28181 服务关闭会幂等释放普通直播、回放、下载、广播、对讲及级联媒体，不再只清理级联会话；普通 RTP 下载终态按 7 天及通道/独立会话容量上限回收，结构化查询快照按 7 天/4096 条回收。无持久化通道记录的 `cascade-*` 指定路径/语音级联流也能接收注册与注销事件并释放会话；LALMAX 新旧响应结构兼容，通过 `stat/group` 提供媒体源状态、轨道和下载进度，并通过 `stop_rtp_pub` 幂等释放 RTP 接收会话；`media_lifecycle_test.go`、`server_lifecycle_test.go`、`rtp_download_state_test.go`、`query_state_test.go`、`zlm_webhook_gb_test.go`、`driver_lalmax_test.go`、`pkg/lalmax/rtp_test.go`、`pkg/lalmax/stat_test.go` |
| AI-501 2.0/3.0 回归 | 完成（自动化） | RTP over TCP、双向对讲 RTP；2022 升级覆盖 `DeviceUpgrade` 受理、`DeviceUpgradeResult` 最终结果和状态查询，最终通知的 completed/failed 优先于迟到的控制 accepted/rejected，通知先到时不会被控制应答覆盖；抓拍覆盖 `SnapShotConfig`、受约束 HTTP 上传、`UploadSnapShotFinished` 最终通知和状态查询。抓拍在发送 DeviceConfig 前登记 pending 会话，设备先上传/完成、后回业务 Response 时不会被当作未知会话或被 accepted 状态回退；目标必须是在线设备自身或其已知子通道。公开上传入口先限制请求体 10 MiB、再限制解出的图片 5 MiB，超限返回 413，不再无界 `ReadAll`；空图片返回 400，原子落盘失败返回 500，只有保存成功后才确认并增加接收计数。升级/抓拍状态均按 7 天 TTL 和 1024 条上限回收，查询时也会惰性删除过期项；并发图片上传使用同一临界区原子累加，完成/失败终态不会被迟到上传回退。另含 TargetTrack、PTZ 精准控制、存储卡、VideoUploadNotify、A.4、H.265/AAC SDP 及八类 2022 DeviceConfig 写入；`version_regression_test.go`、`upgrade_test.go`、`img_test.go`、`ipc_snapshot_upload_test.go`、`voice_talk_test.go`、`server_lifecycle_test.go` |
| AI-502 诊断/API | 完成 | 有效版本、来源、过滤后的能力、设备级 `gb_disabled_capabilities`、最近拒绝；手动版本设置/清除；下载状态、取消和灰度指标 API；SIP 配置写盘成功后才提交运行时快照，监听器/身份/TLS/日志字段明确要求重启，运行时读取和更新由并发快照隔离；`config_sip_test.go`、`config_reload_test.go` |
| AI-503 真实设备矩阵 | 待外部执行 | `docs/testing/gb28181-device-matrix.md` 提供证据模板，`docs/testing/gb28181-interoperability-runbook.md` 提供逐步执行与回滚手册 |
| AI-601 发布候选验证 | 完成（自动化） | 全仓 test/vet/race 和补丁检查通过；外部真实设备与部署验收归入 AI-503/AI-602 |
| AI-602 灰度/回滚 | 待目标环境执行 | 已具备按设备手动版本覆盖、诊断、`GET /gb28181/metrics` 和关闭下载即取消活动任务；尚无目标环境指标及回滚演练证据 |

## 3. 上下级平台级联增量审计

| 能力 | 状态 | 当前证据 |
|---|---|---|
| 多上级注册 | 完成（自动化） | `SIPUpstream.transport` 支持 UDP/TCP 且空值默认 UDP；TCP 持久连接覆盖 REGISTER→401 Digest→认证 REGISTER→Keepalive、断线重连事务换连接和入向身份绑定；Digest/qop、续注册、注销、退避、热更新、实际 Expires；2022 301/302 支持 UDP/TCP 重定向并校验 Contact/ServerID/传输。SIP 事务表按 Server 实例隔离，事务键使用 `Call-ID + CSeq 序号 + 方法` 隔离同一对话并发请求和 REGISTER 重试，同键并发获取原子去重，停服并发幂等关闭事务并唤醒等待者；已建立的设备/上级 TCP/TLS 连接统一登记，停服会主动关闭以解除阻塞读取并终止解析 goroutine，避免多实例覆盖、响应串队列、旧事务误删新事务和活动连接泄漏；`cascade_test.go`、`server_tcp_test.go`、`sip/tx_lifecycle_test.go` |
| `Date + Note` 信令摘要 | 完成（自动化，默认关闭） | 除 REGISTER 请求/响应外，级联 MESSAGE/Keepalive 在写出前签名并校验响应；入向已注册上级按 `SignalDigestSeed → Password → 全局 Seed` 选择 seed，设备按设备独立密码优先；Required 模式丢弃无签名、超时或正文被篡改的响应且事务不阻塞；`cascade_test.go`、`signal_digest_test.go`、`sip/signal_digest_test.go` |
| 查询与目录 | 完成（自动化） | 平台与共享通道 DeviceInfo/DeviceStatus、共享通道 RecordInfo 下级查询及响应编码映射；可信 NVR 可代表其已知子通道返回 DeviceInfo，通道元数据单独落库且不会覆盖父设备，非法跨设备编码仍拒绝；按上级版本安全转发 1.1 PresetQuery、2.0 HomePositionQuery/MobilePosition、3.0 CruiseTrackListQuery/CruiseTrackQuery/PTZPosition/SDCardStatus；巡航轨迹编号、列表、预置位、停留时间和速度结构化解析；上下级 SN 独立、响应恢复上级 SN，DeviceID/ParentID 映射且未知下级编码不透传，多上级并发响应隔离；Catalog 支持平台根或单个共享通道目标、共享白名单、20 条查询分包和版本化 Info；录像列表 20 条分包且不泄露下级编码；`cascade_query_test.go`、`device_info_channel_test.go` |
| 设备控制 | 完成（共享通道安全子集） | PTZ、录像控制及版本匹配的 IFrame/DragZoom/HomePosition/精准 PTZ/TargetTrack 转发下级并映射业务响应；TargetTrack 仅允许控制当前共享通道，`DeviceID2` 会映射为真实下级编码，禁止借此越权控制未共享通道；校验 PTZ 校验和、上下级版本与设备能力关闭项；重启、布撤防、报警复位、格式化等设备级操作不允许经仅共享通道越权执行；`cascade_control_test.go` |
| 订阅通知 | 完成（自动化） | 1.1+ Catalog 初始通知仅发送离线/异常目录并携带 `Status=OK`，默认 Expires 600 秒；1.0、Alarm、MobilePosition、PTZPosition 不误发初始 Catalog；2011 Catalog 变化通知使用 `Response` 根，2014+ 使用 `Notify` 根；平台根和单个共享通道订阅分别维护可见目录快照，只发送 ADD/DEL/ON/OFF/UPDATE 增量，单包 `SumNum` 与 `DeviceList Num` 一致；同一订阅的增量计算、分包发送和快照提交串行执行，失败不提交快照且重试补发；刷新保留原对话与 NOTIFY CSeq，未重复携带 X-GB-Ver 时复用注册协商版本；空消息体 terminated NOTIFY 返回 200、清理出向对话，并在仍有级联引用时自动重订；上级 Catalog 订阅自动映射到承载共享通道的下级 NVR，目录快照变化后重新计算来源并为新增或迁移通道补订阅；Catalog/Alarm/MobilePosition/PTZPosition 下级订阅支持续订、退订、过期/上级移除清理和多上级引用计数，续订与过期清理串行收敛；上级对话和下级引用的建立/续订/退订均改为按订阅键串行，无关设备或事件可并行；操作锁按等待者/持有者引用计数自动回收，在途创建用占位项与关闭串行收敛，不遗留刚创建的下级对话；事件 NOTIFY 由独立串行锁保证 CSeq 和发送顺序，不在网络等待期间持有订阅状态锁，上级热更新/关闭可先移除订阅再通过 worker context 立即取消在途响应等待；Alarm 完整支持 Start/EndAlarmPriority、AlarmMethod、AlarmType、Start/EndAlarmTime 过滤，兼容 2011 示例的 StartTime/EndTime 别名；事件通知按平台级、指定共享通道和过滤条件二次筛选并映射编码；2.0 起支持 AlarmType，3.0 支持 PTZ 精准位置变化订阅/通知且 1.0/1.1/2.0 明确拒绝；非标准级联订阅明确拒绝；`cascade_subscribe_test.go`、`cascade_query_test.go`、`subscribe_11_test.go` |
| 实时点播 | 完成（自动化） | 1.0/1.1 UDP，2.0/3.0 UDP/TCP；ZLM 转发、ACK/BYE/CANCEL 和引用计数；3.0 指定路径使用独立媒体源与 RTP 流并按同一路径会话释放；对话内请求绑定发起该会话的已注册上级，其他上级不能确认或终止会话；INVITE 清理器只回收 10 分钟未建立的半开对话，已建立且媒体正常的长时会话不会因 SIP 静默被误切断；`cascade_media_test.go`、`server_lifecycle_test.go` |
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
go test ./...
go test -race -vet=off ./pkg/gbs/... -count=1
go test -race -vet=off ./internal/conf ./internal/web/api ./pkg/gbs/... ./internal/core/ipc/store/ipccache ./internal/app
go test ./pkg/gbs/sip -run '^$' -fuzz FuzzParseSIPAddressesDoNotPanic -fuzztime=5s
go test ./pkg/gbs/sip -run '^$' -fuzz FuzzParseSIPHeaderDoesNotPanic -fuzztime=8s
GOCACHE=/tmp/owl-gocache go vet ./...
git diff --check
```

本轮未执行管理端构建，不能用上一次构建结果替代当前证据。

## 6. 已知边界与剩余问题

| 项目 | 当前状态 | 处理结论 |
|---|---|---|
| SIP REGISTER 口令摘要认证 | 已加固并有自动化证据 | 验签在计算前保存客户端原始 `response`，避免计算函数覆盖字段后形成自比较；错误口令摘要已有拒绝回归。nonce 由服务端使用密码学安全随机源签发，绑定设备与源 IP、5 分钟失效且有界保存；首次成功后仅允许同 Call-ID/CSeq/摘要的 UDP 幂等重传，拒绝跨请求重放、任意 nonce、错误 realm/username/URI 和算法混淆；显式 SIP 域为空时，挑战、验签及缺失 Contact 的回放响应统一使用平台 ID 前 10 位派生域，不再出现配置展示与实际 realm/URI 不一致；服务端保持 MD5 设备兼容，级联客户端支持 MD5/SHA-1/SHA-256 并明确拒绝未知算法；`register_version_test.go`、`sip/auth_test.go`、`cascade_test.go` |
| MESSAGE/NOTIFY RFC Digest 兼容鉴权 | 已加固并有自动化证据，仍为兼容开关 | `RequireMessageAuth` 挑战使用与 REGISTER 一致的有效域；验签先保存客户端原始 `response`，明确绑定 realm、username、请求 URI、方法、设备及源 IP。nonce 由服务端签发、5 分钟失效且有界保存；`qop=auth` 按递增 `nc` 防重放并允许同 Call-ID/CSeq/摘要幂等重传，旧式无 qop 摘要仅允许原请求重传；过期或失败响应返回新挑战。该开关与 `Date + Note` 标准信令摘要相互独立；`access_control_test.go` |
| 畸形 SIP 地址与 SSRC 配置 | 已加固并有自动化证据 | `ParseSipURI`、`ParseAddressValue(s)` 对空串、短 scheme、纯空白、空用户/主机、缺失引号或 `>` 返回错误；裸 `addr-spec` 和括号 IPv6 正常解析；模糊测试覆盖任意字符串不 panic。SSRC 仅接受 0/1 流类型和 10 位数字域编码，错误沿媒体调用链返回，不再直接切片；`parser_robustness_test.go`、`stream_test.go` |
| 目录目标与出站请求边界 | 已加固并有自动化证据 | SIP URI 解析统一拒绝 CR/LF 等控制字符；完整 Catalog 快照会先验证全部通道目标可构造，再一次性替换，避免部分提交把既有通道误删，同时保留安全的厂商非标准编码兼容。持久化通道恢复会跳过无法构造 SIP URI 的目标；统一出站入口在 Header Builder 前校验 To URI、连接和目的地址，畸形厂商 XML 不再通过空 `to.URI` 触发 panic；`request_target_test.go`、`catalog_extension_test.go`、`parser_robustness_test.go` |
| 在线设备运行态并发 | 已加固并有 race 证据 | REGISTER、Keepalive、OPTIONS、DeviceStatus、配置响应和离线检测对在线状态、连接、地址、口令及心跳时间的数据库更新与运行态同步按设备串行提交；离线扫描在提交前复核 `RegisteredAt`、`KeepaliveAt` 和 `Expires`，旧快照不能覆盖刚完成的新注册、心跳或探测，服务重启恢复设备时不再遗漏 `Expires`。点播、回放、下载、控制、订阅、摘要和级联转发读取同一份运行态快照。出站请求一次获取 To/连接/目的地址三元组，避免重注册切换连接时拼出跨代请求；`device_runtime_test.go`、`server_lifecycle_test.go`、`ipccache/cache_test.go` 及 race 回归 |
| SIP 传输资源上限 | 已加固并有自动化证据 | TCP 使用有界 `ReadSlice`，单头行 8 KiB、总头 64 KiB、正文 1 MiB；超限 `Content-Length` 在 `make` 前拒绝，重复长度和正文提前断开不再把部分报文交给解析器。长连接空闲等待首字节不计时，首字节到达后整帧必须在 30 秒内完成，防止慢速逐字节占用；UDP 的解析头和 Packet 分配有同一正文上限，并要求声明长度与报文正文精确一致，拒绝重复长度、截断正文和尾随正文，避免报文走私或部分 XML 进入业务层；2014 附录 O 文件数据走独立裸 TCP 下载链路，不受 SIP XML 正文上限影响；`server_tcp_test.go` |
| SIP 连接错误边界 | 已加固并有自动化证据 | 出站请求在创建事务前统一拒绝缺失连接或目的地址；UDP 包读写不再依赖会 panic 的直接类型断言，监听型 UDP 的空 `RemoteAddr` 在关闭错误路径可安全记录；TCP 包装明确按构造类型走流式写入，不受测试连接或代理连接的 `Network()` 文本影响；`connection_test.go`、`request_validation_test.go` |
| SIP 监听器启动与后台任务 | 已加固并有自动化证据 | UDP/TCP/TLS 在构建应用时同步完成地址解析、证书加载和端口绑定，错误沿 `NewServer → wireApp → Run` 返回，不再在后台 goroutine `panic` 或无限等待 UDP 初始化；持久化设备/通道恢复失败同样返回构建错误，不再由缓存层 `panic`。已成功绑定的监听器在后续启动步骤失败时统一回收。设备离线检查随 GB28181 生命周期退出，不再使用永不取消的后台 context；保留的包级旧停止接口通过原子默认实例访问，不再由无保护全局指针产生竞态；`server_listener_test.go`、`server_lifecycle_test.go` |
| 下载/查询状态与媒体关闭 | 已加固并有 race 证据 | RTP 下载、Direct TCP 终态和设备结构化查询快照均有 TTL 与容量上限；小时清理器随 GB28181 生命周期启动和停止，无新业务时仍执行，活动下载及其临时文件受保护。结构化查询读取及 `DeviceQuery` 返回值对嵌套切片、指针和 A.4 字段映射执行深快照，调用方不能改写内部运行态。通用查询只解析/落地一次，DeviceStatus 外部更新不占用查询状态锁；Keepalive、Alarm、DeviceConfig、ConfigDownload、MobilePosition 和通用查询先更新内存快照及解除等待、返回 `200 OK`，再执行设备历史/A.4 等数据库写入。省略 `Status` 的 Keepalive 在内存和持久状态中均视为在线。MediaStatus `121` 与普通出站媒体的入向 BYE 先从流表删除匹配会话并写入幂等终态，再返回 `200 OK`；媒体服务器关端口、RTP 下载进度刷新和播放状态写库放在确认后，不会因外部清理慢而引发重传。关闭服务会停止普通和级联广播/对讲发送、关闭 RTP 接收端口、取消直接下载并清空流会话；PTZ、DeviceControl、DeviceQuery、DeviceConfig、Upgrade、Snapshot、Broadcast、级联控制/媒体/语音及 Catalog/RecordInfo 等等待会立即返回统一停服错误，不再等待 6～10 秒业务超时；在途请求、响应别名及订阅索引同时清空，停服后统一出站入口拒绝创建 SIP 请求。合法已建立级联会话不再被 10 分钟 SIP 静默清理器误终止；`direct_tcp_download_test.go`、`rtp_download_state_test.go`、`query_state_test.go`、`media_status_test.go`、`media_lifecycle_test.go`、`server_lifecycle_test.go`、`request_target_test.go`、`multi_response_test.go` |
| 调用方取消与 SIP 事务 | 已加固并有 race 证据 | 实时点播、历史回放/下载与控制、PTZ、DeviceControl、DeviceQuery、DeviceConfig、Catalog、RecordInfo、Subscribe、TimeSync、OPTIONS、Upgrade、Snapshot、Broadcast/Talk 及级联控制的公开调用链会在发送前拒绝已取消请求，并将调用方 `context` 传播到 SIP 最终应答和后续业务/媒体等待；级联实时点播将入向 INVITE 会话 context 传入下级点播，上级 CANCEL 可立即取消下级 INVITE 等待。取消或 deadline 会关闭对应 SIP 事务，不再继续占用事务表到固定 20 秒空闲超时。同一通道的直播、历史 RTP/直连下载、广播、对讲及停止操作通过设备内按通道 ID 管理的引用计数可取消锁串行化，Catalog 替换 Channel 对象也不会产生两把锁；不同通道可并行，等待者取消或最后持有者释放后锁项自动删除。保留原有无 context 的 Play、PTZ、Catalog、Snapshot 方法并委托到 Background context，避免破坏已有调用方；Catalog/RecordInfo 自身超时仍保留部分响应兼容语义，只有调用方取消才返回 context 错误；`sip_response_context_test.go`、`cancelable_mutex_test.go`、`cascade_media_test.go` 及 race 回归 |
| SIP 报文日志生命周期 | 已加固并有 race 证据 | 全局日志器只在监听器全部绑定成功后安装，启动失败关闭候选日志器；关闭与并发报文写入使用同一互斥锁，输出对象只关闭一次并置空，停服或启动回滚不再与在途日志写入竞争；`traffic_logger_test.go` |
| `Date + Note` 信令数字摘要 | 已形成自动化闭环，待真实互通 | 按 `From + To + Call-ID + Date + seed + 消息体` 计算；Date 使用北京时间并校验可配置时间窗；支持 MD5/SHA-1/SHA-256/SM3，SM3 使用成熟国密库并以 GB/T 32905 已知向量校验；nonce 默认 Base64 输出，可配置 hex 并可兼容接收标准示例中的十六进制形式；REGISTER 请求/响应免签。`Enabled` 启用出站签名，并对入向已携带 Note 的报文验签；`Required` 还会拒绝缺失或验签失败的入向报文；无效响应被丢弃，级联和 OPTIONS 使用 context-aware 事务等待，在 deadline 到达时退出且不遗留阻塞等待 goroutine。默认 MD5 且整体默认关闭以保持旧设备兼容。`RequireMessageAuth` 仍是另一套 RFC Digest 兼容开关，二者不能混同；`signal_digest.go`、`signal_digest_test.go`、`sip/signal_digest_test.go`、`cascade_test.go` |
| SIP 配置加载与热更新 | 已加固并有自动化证据 | 启动及 API 更新统一校验 20 位平台 ID、10 位有效域、端口、TLS 必填项、设备历史范围和信令摘要。候选配置先写临时文件并原子替换，成功后才提交内存及 SIP 运行时快照；失败保持原配置。Host、ID/Domain、Port、TLS 和 SIP 报文日志会影响监听器、身份或全局日志器，API 明确拒绝伪热更新并提示修改文件后重启；密码、访问控制、摘要、历史、2014 直连下载及上级级联可安全热更新。SIP 请求只读取受锁配置快照；`config_sip_test.go`、`config_reload_test.go`、`signal_digest_test.go` |
| 数字证书双向认证、CRL | 未实现完整协议闭环 | 高安全级别专项，不能宣称支持 |
| `Monitor-User-Identity` 跨域身份头 | 未实现完整生成、逐级追加和验证 | 与安全路由网关身份体系一起设计，不能仅做字符串透传后宣称完成 |
| NTP 服务端/客户端 | 项目未内置 NTP 服务 | 标准要求 IP 接入设备通过 REGISTER `200 OK` 的 `Date` 校时，代码已实现；NTP 是可配置部署能力，不把非标准主动 `DeviceControl(Time)` 当成 NTP 完成证据 |
| 升级/抓拍状态持久化 | 当前为 7 天 TTL、1024 条上限的有界内存状态 | 小时清理器和查询惰性清理会回收过期会话；进程重启后可接收合法最终通知并重建终态，但重启前的 accepted/uploading 中间态不会恢复。若业务要求跨重启展示完整任务链，应设计独立任务表，不把多会话状态塞入单个设备 Ext JSON 造成写放大和并发覆盖 |
| 真实设备、真实上级和三级路径 | 未执行 | 包括 `Date + Note` 的 Base64/hex、算法和 seed 协商互通；仍是生产完成阶段门，不得由模拟器结果替代 |

## 7. 生产完成所需证据

1. 按设备矩阵完成 1.0 两厂商、1.1 两厂商、2.0 一厂商、3.0 一厂商联调；
2. 每台设备归档厂商、型号、固件、网络模式、脱敏 SIP 报文、语音 RTP 收发证据、文件 SHA-256 和结果；
3. 在最终发布候选提交上重跑全仓 test/vet/race；
4. 白名单灰度观察注册失败率、目录超时率、播放成功率、下载失败率和异常断流；
5. 演练按设备清除/设置手动版本、关闭直接 TCP 开关和回退发布版本。
