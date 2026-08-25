# GB/T 28181 1.0/1.1 开发验证报告

记录时间：2026-08-25（Asia/Shanghai）

## 1. 当前结论

- 1.0：代码与模拟器核心流程已完成，尚未完成真实设备矩阵验收；
- 1.1：Catalog 扩展、Basic/视频/音频/SVAC 设备配置、MediaStatus、多响应、目录订阅、接收者主动 INVITE 广播和附录 O 直接 TCP 文件下载已完成自动化/模拟器验证；
- 2.0/3.0：相关包回归通过，广播使用 `PCMA/8000`，对讲具备 RTP 双向媒体闭环；RTP over TCP 与 2022 能力门禁未被 1.0/1.1 打开或降级；
- 上下级平台级联：基于 UDP/TCP/TLS 的多上级注册、核心及版本化扩展查询、目录、订阅通知、直播、回放、下载和语音广播/对讲已完成自动化验证；TCP/TLS 持久连接覆盖 Digest 注册、心跳、重连事务换连接及 301/302 传输切换，出向 TLS 校验服务端证书并支持客户端证书，入向 TLS 可校验或强制要求客户端证书；2022 附录 H 指定路径已覆盖实时、回放和下载；仍需四版本真实上级平台互通；
- 全仓 test/vet/race 已通过；对外状态仍不得标记为“生产完整支持”，真实设备矩阵和目标环境灰度/回滚仍是阶段门。

## 2. 已执行验证

### 全仓测试

```bash
go test ./... -count=1
```

结果：通过。

覆盖内容包括：

- 四版本 REGISTER、SIP/XML/SDP 夹具；
- SIP/TCP 大小写不敏感 `Content-Length`、紧凑头 `l`、同连接连续报文分帧；对话路由集反转 `Record-Route` 及响应 CSeq 不变性；
- REGISTER 同时携带 Contact `expires` 与全局 `Expires` 时，以 Contact 绑定参数为准，包括 `expires=0` 注销语义；
- 版本解析、协商、持久化、手动覆盖和下行版本；
- 1.0 功能门禁、直播/回放/下载 SDP golden；
- 1.0 REGISTER、Keepalive、Catalog、RecordInfo、Alarm 模拟流程，以及平台主动点播使用的 UDP SDP golden；
- 已注册上级的入向 INVITE 进入级联 B2BUA；其他未知非广播入向 INVITE 明确返回 501；
- 设备级 `gb_disabled_capabilities` 会同步影响持久化能力快照、诊断输出和发送前能力门禁；
- 1.1 Keepalive、Catalog、DeviceConfig、MediaStatus/121、Broadcast Response 串联模拟流程；
- 1.1 Catalog 扩展与目录节点、未知 XML 保留；
- BasicParam、VideoParamConfig、AudioParamConfig、SVACEncodeConfig、SVACDecodeConfig 查询与写入报文、组合下发、SVAC XML 片段安全校验及原始响应保存；
- MediaStatus/121 幂等收敛；
- Catalog/RecordInfo 多响应乱序、重复、总数冲突、超时部分结果和同设备并发；
- `Event: Catalog;id=...` 初始订阅、续订、取消；
- Broadcast 通知与业务应答、接收者主动 INVITE、版本化 SDP、ZLM RTP 启停和 ACK/BYE 清理；
- 2016/2022 对讲在设备音频流建立后复用 RTP 连接反向发送 G.711 A-law 音频；
- 媒体注册、注销、RTP 超时状态竞争；
- 1.1/2.0/3.0 能力矩阵和诊断字段。
- 附录 O `m=video ... tcp` SDP 构建/解析，与 `TCP/RTP/AVP` 严格隔离；
- 本地 TCP 文件发送端、已知/未知大小、SHA-256、零字节、大小超限、提前 EOF、超量数据；
- 连接/首字节/空闲/总超时、取消、MediaStatus、单设备/全局并发和终态竞争；
- 下载进度查询、取消接口、白名单、注册地址校验和原子落盘。
- MediaStatus/121 与入向 BYE 必须同时匹配 Call-ID 和已鉴权设备编号，跨设备同 Call-ID 不得终止 RTP 或 Direct TCP 会话；
- 注册、目录、媒体会话、异常断流和直接 TCP 下载灰度计数器。
- LALMAX `code/msg` 与 `error_code/desp` 响应兼容；管理请求携带配置 token，主动健康检查必须真实访问配置接口；`stat/group` 映射媒体存在性、音视频轨道、码率及累计字节；直播、回放和语音接收结束后通过 `stop_rtp_pub` 幂等释放 RTP 接收会话；新版 ZLM 兼容接口覆盖关闭流与开始/停止录像，缺失录像状态列表时必须返回错误而非空成功；
- 媒体节点连接、动态 Hook 配置和状态持久化必须全部成功后才能显示在线；初次失败保持离线，探测恢复重新执行完整配置，数据库失败返回错误且不得触发后台 `panic`；应用清理必须等待节点检查器退出；
- Webhook 目标为空时 GB 报警事件分发器必须是真正的 nil 接口；并发分发/关闭不得向已关闭队列写入，关闭需等待已入队事件。事件清理、录像清理和录像同步 worker 必须响应应用取消并在清理函数返回前退出；Wire 注入代码必须可重复生成；
- 四版本级联 REGISTER、Catalog、UDP/TCP/TLS、下载倍速、订阅能力矩阵；
- 四版本协议时间固定按北京时间编码/解析；查询与控制 SN 为单调正整数，RecordInfo 无响应、空结果、部分分包、重叠区间及午夜分日能够区分；
- 上级级联信令可配置 UDP/TCP/TLS；TCP/TLS 同一连接完成 REGISTER Digest 与 Keepalive，断线后同一 Call-ID 事务切换新连接，301/302 可切换到 TCP/TLS 或 `sips:` 目标，且 SIPS 不允许降级到明文传输；
- 2022 上级 REGISTER 301/302 重定向，校验 Contact 的 ServerID、SIPS/transport，重定向后重新 Digest 注册，后续心跳、通知、媒体请求和入向身份校验绑定新地址；
- 共享通道 DeviceInfo、DeviceStatus 和 RecordInfo 查询，录像响应分包并映射为上级可见编码；NVR 代表已知子通道返回 DeviceInfo 时只更新子通道元数据；
- 共享通道 1.1 PresetQuery、2.0 HomePositionQuery/MobilePosition、3.0 PTZPosition/SDCardStatus 查询转发；上下级 SN 转换、DeviceID/ParentID 安全映射、未知编码拒绝、下级失败业务应答及多上级响应隔离；
- 3.0 附录 A.4 扩展对象按 Catalog ExtraInfo、Alarm/MobilePosition 嵌套对象和 DeviceStatus 响应的真实承载路径级联；共享对象递归编码映射，未知 20 位对象编码整条拒绝，且 1.0/1.1/2.0 不输出 3.0 ExtraInfo；
- 3.0 PTZ 精准位置变化事件订阅/通知及级联；1.0/1.1/2.0 版本门禁，通知通道编码映射和非共享通道隔离；上级 Catalog/Alarm/MobilePosition/PTZPosition 订阅自动建立下级订阅并覆盖续订、退订、引用计数、过期和上级移除清理；Catalog 会订阅承载共享通道的下级 NVR，并在目录快照变化后为新增或迁移通道补订阅；
- 3.0 CruiseTrackListQuery/CruiseTrackQuery 的请求、结构化响应、轨迹编号参数和安全级联；
- 3.0 附录 H `X-PreferredPath` 首跳消费/剩余路径转发和 `X-RoutePath` 响应前置；平台编码、重复路径、错误首跳、下级确认不匹配及路由环拒绝；不同指定路径的实时、回放和下载媒体源隔离及正确释放；
- 3.0 ConfigDownload 使用标准 `SnapShotConfig`，响应解析兼容厂商旧节点 `SnapShot`；
- A.4 ExtraInfo JSON 的整数、零值、数组和 20 位数值型编码保持原始精度；未知数值型对象编码同样拒绝转发；
- 共享通道 PTZ、录像和版本化通道控制转发；设备级高影响控制不因共享单个通道而被级联放大；
- 四版本历史级联 `INVITE→ACK→INFO→BYE` 完整对话、媒体启动/释放和 MANSRTSP/RTSP 转换；
- 无持久化通道记录的 `cascade-*` 指定路径/语音级联媒体流注册、注销及断流会话释放；
- 两个已注册上级之间的级联对话所有权隔离，非会话所属上级的 ACK/CANCEL/BYE 不得改变或终止会话；
- Catalog 查询和订阅均支持平台根或单个共享通道目标；1.0 变化通知使用 `Response` 根，1.1+ 使用 `Notify` 根；目录快照只产生 ADD/DEL/ON/OFF/UPDATE 增量，分包中 `SumNum` 与本包 `DeviceList Num` 一致；同一订阅并发触发不重复发送，任一分包失败时不提交快照并在下次重试；
- 1.1+ 域间目录初始订阅只通知离线/异常目录，刷新订阅保留 NOTIFY CSeq；续订与过期清理并发时由续订保留有效会话，过期会话释放下级引用；空消息体 terminated NOTIFY 返回 200 并清理/按引用重建下级订阅；未重复携带 X-GB-Ver 的上级 SUBSCRIBE 复用注册协商版本；Alarm/MobilePosition 和 1.0 目录订阅不误发初始 Catalog；Alarm 订阅按级别、方式、类型和时间过滤，兼容 2011 `StartTime/EndTime` 示例别名；
- 设备级关闭 `download_speed` 或 `h265` 后，运行时分别不再发送下载倍速或 H.265 SDP 声明，能力快照与实际发送门禁一致；
- 1.1/2.0/3.0 级联 Broadcast 通知、上游语音源 Digest INVITE、ZLM 音频接收、下游接收方主动 INVITE 和 RTP 转发；1.1 PS/90000 与 2.0/3.0 PCMA/8000 版本矩阵；多通道 Subject 定位、CANCEL、双侧 BYE、错误应答、来源隔离和资源回收；

### 静态检查

```bash
go vet ./...
```

结果：通过。

检查过程中修正了 `GB28181API` 含锁结构体被值接收者复制的问题。

### 竞态检查

```bash
go test -race -vet=off ./... -count=1
```

结果：通过。

竞态检查先后发现并修复：

- SIP parser 停止标记竞争，改为 `done channel + sync.Once`；
- 媒体节点 `LastUpdatedAt/IsOnline` 在心跳更新和定时检查间的竞争，改为包装对象内加锁访问。
- 历史播放控制 `CSeq` 并发自增竞争，改为统一原子分配，避免重复序号。
- 订阅刷新改为在原订阅对象上加锁更新，保留 NOTIFY CSeq，并避免刷新/事件通知并发读写状态。

### 补丁检查

```bash
git diff --check
```

结果：通过。

## 3. 全仓测试清理

为使发布候选验证可重复，已清理以下既有测试问题：

- 6 个生成式 Store `Get` 测试补充 `sqlmock` 返回行及结果断言；
- RPC 健康检查改用 gRPC `bufconn` 内存服务；
- ZLM/LALMAX HTTP 测试改用内存 `RoundTripper`，不再依赖本机 `8080` 服务；
- ZLM 截图测试不再向仓库写出 `snap.jpg`；
- OTA 最新版本测试不再访问 GitHub；
- 直接 TCP 下载测试继续使用 `127.0.0.1` 临时监听端口，在受限沙箱中需使用允许本地监听的测试环境。

结果：`go test ./... -count=1`、`go vet ./...`、`go test -race -vet=off ./... -count=1` 和 `git diff --check` 均通过。

## 4. 未完成和阻断项

### 2014 直接 TCP 下载真实设备验证

ADR：`docs/adr/gb28181-2014-direct-tcp-download.md`。AI-403 已按 Owl 内置 TCP 客户端方案实现并通过本地发送端模拟器验证；尚无真实 1.1 设备证明不同厂商对占位 SDP 端口、`filesize/fileszie` 和 MediaStatus 时序的兼容性。

### 语音真实设备验证

自动化已证明 SIP/SDP 状态机和 ZLMediaKit RTP API 调用，但尚无真实设备证明扬声器实际出声、设备主动 INVITE 的厂商差异、NAT 下 RTP 地址以及长时间广播/对讲稳定性。语音发送当前要求 ZLMediaKit 中存在已就绪的 G.711 A-law 音频源；对 LALMAX 官方公开源码提交 `0845640`（2026-05-28）的核验仍未发现 RTP 发送接口，因此 LALMAX 明确返回不支持。

### 四版本真实上级平台级联验证

自动化已经覆盖注册、目录、核心及版本化扩展查询、订阅、直播、回放、下载、语音广播/对讲、INFO 控制和资源释放；2022 附录 H 仍缺至少三层真实平台对 `X-PreferredPath`/`X-RoutePath` 的路径选择和媒体结果证明；尚无 1.0/1.1/2.0/3.0 真实上级平台分别执行完整链路的脱敏 SIP 报文和媒体结果，不能据此宣称生产互通完成。

语音广播/对讲 B2BUA 与 2022 A.4 嵌入对象的安全级联均已完成自动化闭环，但尚无真实上级语音源与真实下级扬声器证明跨域音频可听、长时间稳定及厂商 SDP 差异兼容，也尚无真实 3.0 平台验证各厂商 A.4 ExtraInfo 结构。自动化能力不能替代真实平台和设备验收。

### 安全认证与跨域用户身份真实验证

2016 `Capability/Asymmetric` 证书 REGISTER、CA/CRL、`Date + Note` 和 `Monitor-User-Identity` 已形成自动化闭环，但尚无真实证书体系、真实用户目录及至少两级 `211` 安全路由网关的互通证据。Required 防降级、吊销证书、nonce 重放、用户属性越权、超跳和路由环必须在真实链路分别验证；UDP/TCP 明文还必须提供 IPsec 或等价可信边界证据。2022 高安全级别按 GB 35114 单独验收，不能由上述能力组合替代。

### 真实设备联调

尚未提供以下真实设备与脱敏报文：

- 1.0 两个厂商；
- 1.1 两个厂商；
- 2.0 一个厂商；
- 3.0 一个厂商。

因此当前只能声明“自动化/模拟器支持”，不能声明检测认证或生产兼容。

### 灰度和回滚演练

已具备按设备设置/清除手动版本、查询诊断信息和记录最后拒绝命令的能力；尚未在部署环境执行白名单灰度、指标观察和回滚演练。

## 5. 发布前必须完成

1. 使用至少两个厂商的 1.1 设备验证直接 TCP 下载和语音广播，并归档抓包、RTP 统计与文件哈希；
2. 完成真实设备矩阵与抓包归档；
3. 在最终发布候选提交上重新执行全量 test/vet/race；
4. 执行按设备版本回退演练；
5. 记录注册失败率、目录超时率、播放成功率和异常断流指标。
