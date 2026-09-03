# GB/T 28181 1.0/1.1/2.0/3.0 开发验证报告

记录时间：2026-09-02（Asia/Shanghai）

## 1. 当前结论

- 1.0：代码与模拟器核心流程已完成，尚未完成真实设备矩阵验收；
- 1.1：Catalog 扩展、Basic/视频/音频/SVAC 设备配置、MediaStatus、多响应、目录订阅、接收者主动 INVITE 广播和附录 O 直接 TCP 文件下载已完成自动化/模拟器验证；
- 2.0/3.0：相关包回归通过，广播使用 `PCMA/8000`，对讲具备 RTP 双向媒体闭环；RTP over TCP 与 2022 能力门禁未被 1.0/1.1 打开或降级；2022 `VideoUploadNotify` 在任务存储可用时会先持久化所有已配置 3.0 上级目标，即使上级暂未 REGISTER 也会在恢复注册后补发；
- 2014 附录 O 下载管理器的公开等待接口兼容 `nil context`，活动任务仍可通过完成或显式取消结束，不因调用方未提供上下文而触发运行时崩溃；
- 四版本 Catalog 查询均支持可选 `StartTime/EndTime`；统一查询 API 会按北京时间下发已设置边界，级联在 SIP `200` 前校验时间格式和顺序，并按通道增加时间闭区间过滤。2022 先过滤显式共享通道，再补齐匹配项所需目录祖先，避免孤立设备；
- 上下级平台级联：基于 UDP/TCP/TLS 的多上级注册、核心及版本化扩展查询、目录、订阅通知、直播、回放、下载和语音广播/对讲已完成自动化验证；TCP/TLS 持久连接覆盖 Digest 注册、心跳、重连事务换连接及 301/302 传输切换，出向 TLS 校验服务端证书并支持客户端证书，入向 TLS 可校验或强制要求客户端证书；2022 附录 H 指定路径已覆盖实时、回放和下载；停止开始后迟到的 REGISTER 成功只保留远端绑定证据用于退注册，不会复活本地注册状态，Keepalive 成功也不会延长已到期绑定；级联语音源在等待媒体期间收到上级 BYE、媒体注销或 worker 终止时会立即中断，源终止与下游广播索引绑定通过同一状态锁原子互斥，批量关闭多个普通/广播源保持幂等，不能在终止后重新挂接下游会话；仍需四版本真实上级平台互通；
- SIP 业务响应事务：服务端只缓存成功写出的响应，写失败允许同一事务重传重新进入 handler；附录 G、Keepalive、MediaStatus、升级/抓拍、入向 SUBSCRIBE 创建/刷新/退订、上级查询/控制/广播、CANCEL、BYE、历史 TEARDOWN 及级联最终通知均已覆盖 ACK 阻塞、写失败、持久化故障或幂等重传；
- 公共入口复核：设备 API、前端类型和 Swagger 均不暴露 REGISTER 的 Call-ID、CSeq、请求指纹、原响应 Date 和确认状态，数据库序列化仍完整保留；SIP 的 REGISTER/MESSAGE/NOTIFY/SUBSCRIBE/INVITE/CANCEL/BYE/ACK/INFO/OPTIONS 路由已逐项复核，其中入向 OPTIONS 只返回 `200 + Allow`，不读写设备、上级或附录 G 业务状态；合法 `MESSAGE/NOTIFY` 中未注册的 MANSCDP `CmdType` 属于业务命令错误，四版本均返回 `400 Bad Request` 且不得携带 `Allow`；真正未知的 SIP 方法才返回 `405 Method Not Allowed`，其 `Allow` 与 OPTIONS 200 必须准确包含上述十种生产方法及 `REGISTER`，不得漏报、重复或输出空方法；
- 当前工作树的协议整包、全仓 test/vet、协议 race 及管理端 test/build 已通过；对外状态仍不得标记为“生产完整支持”，真实设备矩阵和目标环境灰度/回滚仍是阶段门。

## 2. 已执行验证

### 全仓测试

新增 INVITE 事务验证：全部 `200–299` 成功响应先 ACK；同一 To-tag 对话重传 2xx 原样重发缓存 ACK，不重复进入业务层且不跨 To-tag 复用；`3xx–6xx` 自动发送事务内 ACK；调用方取消覆盖 `INVITE → CANCEL → 200(CANCEL) → 487(INVITE) → ACK`。

```bash
go test ./... -count=1
go vet ./...
go test -race -vet=off ./... -count=1
go test ./pkg/gbs ./pkg/gbs/sip -shuffle=on -count=3
env GOCACHE=/private/tmp/owl-go-cache-fuzz go test ./pkg/gbs/sip -run=^$ -fuzz=FuzzParseSIPAddressesDoNotPanic -fuzztime=10s
env GOCACHE=/private/tmp/owl-go-cache-fuzz go test ./pkg/gbs/sip -run=^$ -fuzz=FuzzParseSIPHeaderDoesNotPanic -fuzztime=10s

cd admin-ui
pnpm test
pnpm build
```

结果：历史轮次的全仓 test/vet/race、全仓随机顺序回归及两项 SIP 解析模糊测试均通过；模糊测试分别完成 46,772 和 371,012 次输入，未发现崩溃。2026-09-02 当前工作树另行重跑全仓串行 test、全仓 race、全仓 vet、GB 核心包随机顺序回归 5 轮、三项协议解析模糊测试、管理端 46/46 契约测试和生产构建，均通过；GB 核心包语句覆盖率为 79.0%，TLS 重连事务波动用例连续执行 100 次通过。

2026-09-02 当前工作树重新执行并通过：

```bash
go test ./... -count=1 -p 1
go test -race -vet=off ./... -count=1 -p 1
go vet ./...
go test ./pkg/gbs -shuffle=on -count=5
go test ./pkg/gbs/sip -run '^TestServerTransactionReplaysResponseOnReconnectedTLSPeer$' -count=100
go test ./pkg/gbs/sip -run '^$' -fuzz '^FuzzParseSIPAddressesDoNotPanic$' -fuzztime=10s
go test ./pkg/gbs/sip -run '^$' -fuzz '^FuzzParseSIPHeaderDoesNotPanic$' -fuzztime=10s
go test ./pkg/gbs/annexg -run '^$' -fuzz '^FuzzDecode$' -fuzztime=10s
go test ./pkg/gbs -coverprofile=/private/tmp/owl-gbs.cover -count=1

cd admin-ui
pnpm test
pnpm build

cd ..
git diff --check
```

涉及本机临时监听的命令在允许绑定本机端口的环境执行；受限环境中的 `bind: operation not permitted` 属于执行环境限制，不归类为代码回归。

覆盖内容包括：

- 四版本 REGISTER、SIP/XML/SDP 夹具；每个 `1.0/1.1/2.0/3.0` 目录都固定包含 REGISTER 初始/认证/401/200、Keepalive、Catalog、RecordInfo、Alarm、INVITE SDP 和 MediaStatus/121，`TestGBProtocolFixtureManifest` 防止夹具回退缺失；
- SIP/TCP 大小写不敏感 `Content-Length`、紧凑头 `l`、同连接连续报文分帧；非空 MESSAGE/NOTIFY/SUBSCRIBE 必须在进入业务处理器前校验唯一、合法且大小写不敏感的 `Application/MANSCDP+xml` Content-Type，并兼容 `charset` 等合法参数，缺失、重复、畸形和错误媒体类型均不得产生业务副作用，空正文 terminated NOTIFY 不误拒绝；MESSAGE/NOTIFY 的方法名先按路由兼容规则规范化，再解析 `CmdType`，兼容标准示例中的命令前后空白并拒绝空命令，未知 `CmdType` 在 1.0/1.1/2.0/3.0 均返回 400 且不宣告 SIP 方法能力；入向请求必须为 `SIP/2.0`，校验 From/To/Call-ID/CSeq 单值有效性、`SIP/2.0` 顶层 Via，以及 Request-Line/CSeq 方法一致性；不支持版本返回 505，其他畸形请求使用独立临时事务，不能替换相同 Call-ID/CSeq 的合法事务连接；对话路由集反转 `Record-Route` 及响应 CSeq 不变性；
- 入向响应必须为 `SIP/2.0` 和 100～699 状态码，校验 From/To/Call-ID/CSeq 单值有效性及 `SIP/2.0` 顶层 Via；INVITE 最终响应必须有非空 To-tag，从响应构造 ACK/INFO/BYE/SUBSCRIBE 等对话请求还必须同时具有 From-tag 和 To-tag，缺失 Contact 仍可兼容回退 To URI；响应必须匹配出向事务记录的原请求 From-tag、目标端点及 Via 协议/传输/Sent-by/branch，Via 参数名大小写不敏感，重复 branch、重复核心头、错误或缺失 From-tag、错误 branch 或错误来源不得唤醒响应等待；全部 1xx 临时响应不得提前结束最终响应等待；
- 2xx ACK 必须复用 INVITE CSeq、生成新 branch，并按响应 `Record-Route` 构造宽松/严格路由；新 Via 不得回送 `received` 或有值 `rport`。3xx～6xx INVITE 最终响应必须自动发送复用原 Request-URI、Route、Via branch 和数字 CSeq 的事务内 ACK；等待 INVITE 取消/超时必须发送同样绑定原事务的 CANCEL。连续及并发 INFO/BYE 的本地 SIP CSeq 必须唯一递增，TCP 重连不得覆盖既有对话路由目标；
- 平台主动直播、回放、RTP 下载、Talk、级联语音源及 2014 裸 TCP 下载收到 2xx 后必须先 ACK，再校验唯一 `Application/SDP`、非空正文、SDP 语法、恰好一个目标类型媒体段、传输协议、方向、地址、端口和唯一且与 offer 一致的 SSRC；方向、TCP `setup/connection` 按 RFC 4566/4145 使用媒体级覆盖会话级，同一层重复方向、给无值方向属性附加值或重复单值属性必须拒绝；`rtpmap` 必须按完整负载编号关联，相同重复可兼容、冲突重复必须拒绝，不能使用库的前缀/首项结果；标准广播报文中 2014 为 `96/PS/90000`、2016/2022 为 `8/PCMA/8000`，显式且无冲突的厂商动态 PT 保持兼容；2014 裸 TCP 应答必须报价 96 且不得把它映射为非 `PS/90000`；作为媒体发送方的响应不得声明 `recvonly` 或 `inactive`，2014 裸 TCP 继续兼容 `sendonly`、`sendrecv` 和未声明方向的旧设备；重复 `y` 或下载 `filesize/fileszie` 单值字段必须拒绝，不能由 SDP 解码器静默选取其中一个；直播、普通历史回放/下载及兼容 Talk 打开 ZLM RTP 接收端口时必须使用与 INVITE `y=` 相同的非零 SSRC，HTTP API 序列化也须包含该值；UDP 应答不得混入 TCP 专用属性；UDP、被动 TCP、主动 TCP 与裸 TCP 必须走各自协议，非法应答或主动连接失败后发送 BYE 并释放 RTP，不得留下已确认对话；2022 主动 TCP 收流及向 `setup:passive` 上级发送级联 TCP 媒体时，首次客户端连接失败后均至少重连 3 次、间隔不少于 1 秒，空连接响应或非法发送端口不能被当作成功而跳过重连；2016 仍只连接一次，调用取消应中止重试等待；缺失 Contact 的应答按 To 建立 ACK/BYE 远端目标；
- SIP UDP/TCP/TLS 监听器、活动连接、parser/handler、请求处理器和事务 watcher 在关闭时全部等待退出；关闭后拒绝新连接、新 handler 和出向请求。GB 全局生命周期门禁还会拒绝停服起点后的 REGISTER/MESSAGE/NOTIFY/SUBSCRIBE/INVITE 等新业务，并等待此前已接纳请求；两阶段关闭先取消业务，再关闭 SIP 解除事务等待，最后清理 GB 会话，避免清理后状态复写和关闭互锁。GB 离线检查、订阅、INVITE 对话与运行状态 cleaner 同样随服务生命周期停止；
- REGISTER 同时携带 Contact `expires` 与全局 `Expires` 时，以 Contact 绑定参数为准，包括 `expires=0` 注销语义；2022 注册重定向的 `302 Moved Temporarily` 必须同时携带可信配置生成的 `Contact` 和请求有效期 `Expires`，且 `Expires=0` 注销不得进入重定向；注册状态可在成功响应前可靠提交，但应用层幂等结果、注册成功指标、设备历史及 DeviceInfo/Catalog/ConfigDownload 后续查询只能在 SIP `200` 实际写出成功后提交或触发；写失败不得缓存为完成请求，只能以 Pending 允许完全相同 REGISTER 重传重新完成确认；注册、刷新和注销状态转换必须使同设备旧成功缓存失效；同一 Call-ID 的持久化 CSeq 必须严格递增，更小 CSeq 或改报文复用相同 CSeq 返回 `500 + Retry-After: 0`，不得覆盖较新的绑定；最后一次请求指纹、原 `Date` 和响应确认状态必须与顺序状态一并持久化，使进程重启或另一实例收到完全相同的注册/注销重传时复用原 `200/Date` 且不刷新绑定；写出 200 后只有首个按指纹原子认领确认的实例执行历史和注册后查询，写失败保持未确认供精确重传继续，已确认实例只重放响应；这些内部事务字段不得出现在设备 API JSON、前端类型或 Swagger 中，但数据库 Value/Scan 往返必须完整保留；兼容 `MemoryStorer.Change` 必须同步持久态与运行态在线、地址、口令、注册/心跳时间、有效期和绑定关闭状态；
- 2011 短绑定的 REGISTER 成功缓存不得超过实际有效期；设备口令修改使旧绑定失效后，不得继续命中旧 REGISTER 成功缓存；
- 版本解析、协商、持久化、手动覆盖和下行版本；历史年份/补充文件别名 `2011/2014/2011-supplement-2014/2016/2022` 在标准名称、年份、能力、排序和运行态存储上分别严格等价于 `1.0/1.1/1.1/2.0/3.0`，不得出现 `Valid()` 成功但能力为空或最低版本判断失败；REGISTER 的 200/401/其他失败响应均携带平台 `X-GB-Ver`，级联认证重试从 401 起采用已获知的较低版本，同一注册握手只降不升；
- 1.0 功能门禁、直播/回放/下载 SDP golden；四版本普通 RTP 回放/下载 INVITE 在按起止时间检索且未显式指定录像类型时必须携带 `u=<通道编码>:3`，不得误用表示全部类型的 `:0`；显式 `record_type` 和入向历史级联应完整支持 `0=all、1=manual、2=alarm、3=time`，级联将暴露编码改写为真实下级通道编码并保留类型，非法类型或不匹配的媒体源编码必须在启动媒体前拒绝；`downloadspeed` 一旦出现必须是 1.1+ Download 的单一正整数，零值不能绕过模式或版本门禁；入向与出向直播/历史 `y` 字段首位必须分别为 `0/1`，出站级联广播的 `Play` offer 同样必须使用首位 `0` 的实时 SSRC，缺失时由本域按会话类型生成，重复 `u/y/f` 等会话字段不得产生歧义；入向与出向级联的 `f` 字段一旦出现都必须同时保留 `v/...a/...` 完整骨架并按目标版本校验各数值，2022 应接受附录 G 规定的正整数 `W×H` 自定义分辨率，旧版不得接收或透传该格式及 2022 独有的 H.265、SVAC 音频或 AAC 声明；TCP 级联协商只接受单一 `setup:passive`，可省略 `connection`，出现时只接受单一 `connection:new`，UDP 不得夹带这两类 TCP 属性；
- 入向级联 `downloadspeed` 按媒体级覆盖会话级读取；只有会话级声明时也必须生效，同一层重复值必须拒绝，不能由 SDP 库静默忽略或选取首项；
- 出站直播、回放、下载和广播/对讲 INVITE 的 Subject 接收方媒体流序列号不得固定为 `0`，应复用当前会话发送方序列号；实时与历史序列以及级联广播均有自动化覆盖；
- 入站级联视频和广播 INVITE 从 2014 起缺失 Subject 必须在启动媒体前返回 `400`，2011 保留存量厂商兼容；携带时必须是唯一头域和四段式格式，双方 20 位编码、发送/接收序列、当前会话身份以及级联视频实时 `0`/历史 `1` 前缀均应通过校验，非法请求不得创建对话或启动 RTP；级联视频和广播分别只允许一个目标 `m=video/m=audio` 媒体段，接收方不得通过会话级或媒体级方向声明 `sendonly/inactive`，媒体级合法方向可覆盖会话级；广播音频不误用视频首位规则；
- 相同 Subject 发送方标识的新级联视频 INVITE 必须替换旧发送链路：旧 INVITE 尚无最终响应时停止本地处理并返回 `487`；旧 2xx 尚未收到 ACK 时立即释放本地 RTP/共享源但保留旧 Call-ID，合法 ACK 后才摘除并发送 BYE；已建立旧会话先停止本地媒体并按对象身份摘表，再异步发送可能阻塞的 BYE，半开上级连接不得延迟新 INVITE 建链。连续替换不得重复处理仅等待 ACK 的旧代次，迟到 ACK/BYE 不得误删新会话；
- 1.0 REGISTER、Keepalive、Catalog、RecordInfo、Alarm 模拟流程，以及平台主动点播使用的 UDP SDP golden；`TestGBProtocolVersionFixtureMatrix` 进一步让四版本真实夹具逐一经过生产 `sipMessageRecordInfo`、`sipMessageAlarm` 和 `sipMessageMediaStatus`，验证 2011/2014 不接收新字段、2016 `FileSize` 与结构化报警类型、2022 `FileSize/RecordLocation/StreamNumber/ExtraInfo`、报警 `EventType` 和媒体终态真实进入模型、附录 A.4/回调/会话收敛路径；
- RecordInfo 四版本响应字段和值域：仅 2011 文件项兼容 `all`；2014 补充文件明确要求返回文件项携带具体录像类型，2014/2016/2022 文件项仅允许 `time/alarm/manual`。查询请求中的 `Type=all` 与响应文件项约束分开处理，非法版本字段不得进入多响应聚合；
- DeviceStatus `Alarmstatus` 按版本校验计数属性大小写和已出现条目的字段值：2016 使用 `num`，2011/2014/2022 使用 `Num`；2014 使用修订后的 `StatusDutyStatus`，并兼容存量设备常见的 `DutyStatus`，2011 同时覆盖 A.2.6 Schema 的 `Status` 与正文/J.11 示例的 `DutyStatus`，拒绝重复字段及已删除的 `Status`；同时保留 XSD 稀疏 Item 兼容。标准 `Online=OFFLINE` 必须归一为内部离线状态，并继续兼容历史厂商误拼 `OFFILE`；尾部基本多值扩展按 2011/2014/2016 纯文本 `Info`、2022 纯文本 `ExtraInfo` 的精确矩阵解析并限制单项 1024 个 Unicode 字符，跨版本字段拒绝；2022 结构化 `<Info>` 仅作为已识别附录 A.4 对象的承载容器接受并继续进入 A.4 流程；旧版下级向 2022 上级级联时将纯文本 `Info` 转换为 `ExtraInfo`；
- 四版本 RecordInfo 查询的 `FilePath`、`Address`、`Secrecy`、`Type=time/alarm/manual/all`、`RecorderID` 能从通道录像 API 和统一 GB A.2.4 查询 API 贯通到设备 SIP XML 并通过级联保留，空 `Type` 继续默认 `time`；`FilePath`、`Address`、`RecorderID` 按四版 Schema 的普通 `string` 处理，不限制 `RecorderID` 为 20 位设备编码，XML 特殊字符安全转义且首尾空白不被裁剪，中心录像按原始 `FilePath/RecorderID` 精确过滤；统一 GB A.2.4 查询省略 `target_id` 或显式使用注册设备编码时，必须按设备/系统级目标发送，SIP Request-URI、`To` URI 和 XML `DeviceID` 均为设备编码，不能以“必须指定通道”提前拒绝；通道目标保持原路由。两套公开入口均允许 2011/2014 省略 `StartTime/EndTime` 并验证请求实际下沉到协议层，2016/2022 仍拒绝缺失或倒置时间；2022 新增的非负 `StreamNumber`（0 主码流，1/2/... 子码流）、`AlarmMethod` 和按方式限定的 `AlarmType` 同样能下发并通过级联保留；`AlarmMethod` 组合在入站兼容紧凑形式与 `/` 分隔形式，内部规范化后按目标版本编码（2011/2014/2016 使用 `25`，2022 使用 `2/5`），2011/2014/2016 携带 RecordInfo 报警过滤扩展及非法值会在发送下级查询前拒绝；
- RecordInfo 响应的顶层/条目 `Name` 和条目 `RecorderID` 同样按四版 Schema 的普通 `string` 处理，设备响应解析、多响应聚合和级联输出不裁剪首尾空白；DeviceStatus 的 `Reason`、旧版纯文本 `Info`、2022 `ExtraInfo`，2014/2016/2022 PresetQuery 的 `PresetID/PresetName`、2022 巡航 `Name`、存储卡 `HddName`，以及四版本报警 `AlarmDescription` 也保持原始首尾空白，枚举、设备编码、报警方式/类型、日期时间和坐标继续规范化。必填的 `PresetID/PresetName/HddName` 区分元素缺失与空值：完全缺失拒绝，已出现的空值接受，不附加 Schema 未定义的 `minLength`。级联 Catalog/DeviceInfo 的普通名称、厂商和型号字符串也原样输出，仅真正的空串触发兼容回退。2022 Catalog 条目的条件必选普通字符串 `Name/Manufacturer/Model/Address` 及一类点位 `Info.ContactInfo` 使用出现标记，完全缺失拒绝，空节点与空白值接受并保留；其 `Info.PointCommonName/ManagementUnit/ContactInfo` 同样保真，编码/枚举/列表字段继续规范化。仅精确匹配的已知下级设备/通道 `RecorderID` 映射为上级暴露编码，未知且精确合法的 20 位设备编码继续裁剪以防泄露，其他合法不透明字符串保持原值；2022 `RecordLocation` 仍按 `deviceIDType` 严格映射，二者不得共用丢弃规则；
- 已注册上级的入向 INVITE 进入级联 B2BUA；其他未知非广播入向 INVITE 明确返回 501；
- 设备级 `gb_disabled_capabilities` 会同步影响持久化能力快照、诊断输出和发送前能力门禁；
- 应用启动恢复 GB28181 运行态时只处理显式 `GB28181` 或历史空类型设备；RTSP/RTMP/ONVIF 等其他协议不得进入 GB 内存索引，也不得被 GB 的 TCP/TLS 重启清理错误置离线或连带关闭通道；
- 1.1 Keepalive、Catalog、DeviceConfig、MediaStatus/121、Broadcast Response 串联模拟流程；
- 1.1 Catalog 扩展与目录节点、未知 XML 保留；
- ConfigDownload 精确版本矩阵（2014 七类、2016 四类、2022 十二类）和 DeviceConfig 精确配置段矩阵（2014 五类、2016 三类、2022 十一类）；BasicParam 按版本要求/裁剪字段，VideoParamConfig、AudioParamConfig、SVACEncodeConfig、SVACDecodeConfig 支持范围不跨版本泄漏。2014 视频/音频范围和当前配置应答校验字段白名单、必填项、整数列表、重复单例及可选 `Num` 计数；2016/2022 视频范围只接受 `DownloadSpeed/Resolution`。顶层标准包络按 Schema 顺序校验，拒绝未知字段和重复配置段；根和标准节点拒绝属性/命名空间，简单包络字段拒绝嵌套，普通配置段仅在 Schema 规定处接受 `Num`。失败应答同样执行版本化配置段门禁，标准 `SnapShot` 与厂商兼容别名 `SnapShotConfig` 互斥。SVAC 写入按 2014/2016/2022 独立结构校验 ROI/SVC/监控/加密/音频字段、数量、重复编号、坐标、数值范围和 2022 SSVC 比例，直连与级联使用同一门禁；SVAC 查询应答按各版本独立应答结构校验 ROI、编码/解码能力、监控开关和值域，拒绝写入字段混用、缺失能力字段及跨版本字段，非法应答不保存状态或解除等待；同时覆盖组合下发、XML 指令拒绝及原始响应保存；
- DeviceConfig 尾部扩展按原文精确为“2014/2016 无扩展、2022 多项 `ExtraInfo`”，2011 本身不支持 DeviceConfig。2022 `ExtraInfo` 已贯通 Web/Core/Adapter、直连和 3.0→3.0 级联，单项最多 1024 个 Unicode 字符且不裁剪首尾空白；旧版本、降级转发及 `Info/ExtralInfo` 均拒绝。级联请求在 SIP `200` 前严格校验版本化配置段顺序、单例、属性/命名空间、嵌套和未知字段；
- BasicParam 名称与 2022 升级字符串保真：2014/2016/2022 `BasicParam.Name` 不裁剪；2022 `DeviceUpgrade.Firmware/FileURL/Manufacturer` 在直连和级联中原样下发，级联幂等指纹按原值区分载荷；结果通知必须出现 `Firmware` 元素但允许普通 `string` 的空值，并在状态中保留原始首尾空白；`UpgradeFailedReason` 仍只接受 `01/02/03/99`；
- 2022 抓拍上传路径保真：`SnapShotConfig.UploadURL` 按普通 `string` 在直连和级联中原样下发，仅以裁剪值判断必填非空；`SessionID` 仍按结构化业务键规范化；
- MediaStatus/121 按 2011 第 9 章流程和附录 J 对四版本开放；合法通知幂等收敛；
- Catalog/RecordInfo 多响应乱序、重复、总数冲突、超时部分结果和同设备并发；2014 补充文件附录 M 明确 10000 项是“每条响应消息携带记录上限”，因此 2014/2016/2022 仅对本包条目数执行该协议门禁，合法 `SumNum > 10000` 可由多个同 SN 分包完成，不能把总数误判为单包越界。公共聚合器另以累计唯一条目 100000 项作为实现资源安全上限，该上限不冒充协议限制；累计越界会原子拒绝当前包、立即唤醒等待者并返回明确错误，已完成查询不再接收迟到分包。RecordInfo 去重后原始 XML、`ExtraInfo` 和附录 A.4 对象每查询共享 8 MiB/4096 对象双重硬上限，越界包的条目与元数据必须全部不提交、立即结束查询，重复元数据不重复计额，结果消费、取消、设备清理和停服后预算状态不得残留；
- 2014 附录 M、2016 附录 N、2022 附录 M 的级联 Catalog、RecordInfo 和 Catalog NOTIFY 分包必须保持同一 SN，并在上一包收到 SIP 最终响应后再发送下一包；1xx 临时响应不得提前放行后续分包。上级配置为 UDP 时，最终序列化 SIP 请求超过 1300 字节必须自动切换到同 IP/端口 TCP，并一致改写下一跳 URI、Via 和 Contact；无 Route 时改写 Request-URI，loose Route 时只改写顶层 Route URI 并保持远端 target 不变，strict Route 时只改写当前 Request-URI、不得改写后续 loose Route。长度判断必须包含 `rport` 及启用后的 `Date + Note`。四版本直连设备的大 MESSAGE 和事件订阅 NOTIFY 应使用事务独占 TCP、拨实际下一跳，并在最终响应、失败、超时或关闭时释放；最终响应早于初始写调用返回时，必须等写完成后再关闭连接，不能把已送达请求误报为失败；2011/2014/2016 附录 G 的 UDP 大 MESSAGE 应切到可复用 TCP，且不得破坏 Digest `401/407` 重试。小 UDP 请求、注册连接、INVITE/SUBSCRIBE/其他对话请求及显式 TCP/TLS 配置不得回归。自动化至少覆盖 `TestCascadeRecordInfoChunksWaitForPreviousSIPFinalResponse`、`TestTransactionGetResponseContextSkipsProvisionalResponse`、`TestCascadeOversizedUDPRequestUsesTCP`、`TestCascadeSignedOversizedUDPRequestUsesTCP`、`TestDeviceOversizedUDPMessageUsesRequestScopedTCP`、`TestDeviceOversizedUDPNotifyUsesRequestScopedTCP`、`TestDeviceOversizedUDPRoutedNotifyUsesRequestScopedTCP`、`TestDeviceOversizedUDPStrictRoutedNotifyUsesRequestScopedTCP`、`TestOwnedConnectionFinalResponseWaitsForRequestWriteCompletion`、`TestAnnexGOversizedUDPRequestUsesTCP`、`TestSendAnnexGSIPRequestRetriesRemoteDigestChallenge` 和 `TestSendAnnexGSIPRequestDigestRetriesOnlyOnce`；发布前仍须用真实设备、上级平台和附录 G 外部系统执行 MTU 与分包时序抓包；
- RecordInfo 响应包络、列表计数、顶层和条目必填 `Name` 元素（普通 `string`，允许 `<Name/>` 空值）、显式 `Secrecy=0/1`、可选 dateTime 起止时间、版本化 Type/FileSize/RecordLocation/StreamNumber、目标所有权和在途通道绑定；2011 空结果必须带 `RecordList Num=0`，2014/2016/2022 按附录多响应规则标准输出省略零结果列表，同时兼容接收显式空列表；2022 多值 `ExtraInfo` 必须为最长 1024 个 Unicode 字符的简单文本并随分包聚合，旧版本携带 `ExtraInfo/ExtralInfo`（包括空节点）必须在聚合前拒绝；级联响应按上级版本裁剪新字段并映射 RecorderID/RecordLocation，2022 `ExtraInfo` 只在首包输出并安全映射对象编码，未知编码转换为不含非法 `Result` 或记录列表的标准零结果，非法分包不得进入聚合器，NVR 顶层父设备编码别名仍可映射到原查询通道；
- Catalog 缺失/零值 SumNum、DeviceList Num、本包与通知总数、条目编码、已鉴权来源及父设备聚合目标；目录项的状态、0/1 属性、安全/注册方式、错误码、端口、有限坐标、证书时间和版本化摄像机属性枚举在聚合前校验，其中 `RegisterWay=4` 仅允许 2022，1.0/1.1/2.0 缺失零值继续兼容；3.0 按 A.2.1.9 与规范性附录 J 区分行政区划、系统、业务分组、虚拟组织和设备，校验各类固定必填字段及父节点编码，设备显式携带 `Parental/RegisterWay/Secrecy`，可选 `Info` 一旦出现必须携带 `GrassrootsCode`，131/132 摄像机的 `Info` 还必须携带 `PointType`，一/二类点位要求显式经纬度（零坐标也算已出现），一类点位另需 `InstallTime/ContactInfo/RecordSaveDays`，向 2022 上级级联时显式保留空 `ContactInfo` 与零值 `RecordSaveDays`；附录 J 允许摄像机整个 `Info` 缺省，122 多目设备不误套摄像机门禁；2011 不接受 2014 新增的 `Info/BusinessGroupID`；2014 未设置的 Info 字段不得输出零值，2022 多值 `PTZType=1..7`、`SupplyLightType=1/2/3/4/9`、7 位 `CapturePositionType`、6 位 `GrassrootsCode`、新增 Info 字段及外层 `SecurityLevelCode/BusinessGroupID` 需正确持久化和级联；`CapturePositionType` 前 5 位必须属于附录 O 表 O.1，后 2 位保留行业小类；2022 入站不得接受已删除的 `Owner/SafetyWay/CertNum/Certifiable/ErrCode/EndTime/PositionType/UseType/Info.BusinessGroupID`，旧上级不得收到 2022 字段；通用查询相同 SN 的兄弟通道响应不得抢占等待；
- Catalog 顶层包络、`DeviceList` 和结构化 `Info` 必须按版本严格校验字段顺序、单例、属性/命名空间与简单字段嵌套；2011 目录通知使用 `Response` 根，2014+ 使用 `Notify` 根并允许 `DeviceID` 后的可选 `Status`。目录项外层字段为兼容既有 1.1 设备允许调整标准字段顺序，但必须保持单例，拒绝未知标准字段和跨版本字段；仅目录项和结构化 `Info` 尾部未知厂商 XML 扩展允许透传。2016 `Info` 必须接受标准 `DownloadSpeed/SVCSpaceSupportMode/SVCTimeSupportMode`，2011/2014/2016 顶层尾部仅允许最长 1024 个 Unicode 字符的多项简单文本 `Info`，2022 改为 `ExtraInfo`；非法查询响应或通知不得进入聚合、状态/等待、事件发布或刷新任务；
- Catalog 类型值必须区分元素缺失与显式非法零值/空值：旧版兼容只适用于真正缺失的可选字段，四版 `RegisterWay=0`、旧三版 `ErrCode=0`/空 `EndTime`、2014/2016 零值摄像机枚举、2022 零值 `RoomType/SupplyLightType/DirectionType/MobileDeviceType/PointType`、零度视场角及空 `InstallTime`/设备编码型 `BusinessGroupID` 必须拒绝；SVC 能力值 `0`、零坐标和 `RecordSaveDays=0` 等标准合法零值不得误拒绝；
- 2022 Catalog 的 `MaxViewDistance` 和 `RecordSaveDays` 按 A.2.1.9 分别只执行 `double` 和 `integer` 类型约束，不附加标准未定义的非负范围；`MaxViewDistance` 仍必须是有限数，一类点位仍必须出现 `RecordSaveDays` 元素；
- Alarm 四版本必须在附录 A.4、状态、业务回调和订阅转发前严格校验 `Notify` 根及 `CmdType → SN → DeviceID → AlarmPriority → AlarmMethod → AlarmTime → AlarmDescription? → Longitude? → Latitude?` 顺序；重复/未知/跨版本字段、属性/命名空间、简单字段嵌套、非标准顶层 `AlarmType` 或 `Info.AlarmMethod` 均拒绝。2011/2014 只接受多项简单文本 `Info`，2016 接受一个结构化报警类型 `Info` 后接多项简单文本 `Info`，2022 接受结构化 `Info` 和多项简单文本 `ExtraInfo`；重复结构化类型、文本与子节点混用、重复 `EventType` 及跨版本扩展不得产生任何业务副作用；
- Alarm 9.4 自动化代码门槛：步骤 1～4 覆盖源设备上报、SIP 确认和源侧业务应答；步骤 5～8 同时覆盖默认关闭的上级平台 `AlarmDispatchEnabled` 和本域 `AlarmReceivers`。上级目标只允许已注册且显式共享报警通道的平台；本域目标只允许当前已注册在线且 `SourceIDs` 精确授权报警 XML `DeviceID` 的接警客户端。两类目标均按自身协商版本转换 XML 并使用独立 SN，上级另执行编码映射；严格关联目标 `MESSAGE/Response/Alarm` 后先返回 SIP `200`。必须覆盖四版本、默认关闭、未授权/离线隔离、错误目标/序号/Result、单目标失败、业务超时、上级热更新、本域终端注销/删除和停服取消；多个本域终端和多个上级平台必须在任何目标业务应答前独立启动，不得因等待首个目标而占用源设备注册锁并串行化。应用层不因超时盲目重复报警。真实 2011/2014/2016/2022 上级平台、设备和本域接警客户端互通及部署审计告警仍是发布前人工门槛；
- MobilePosition 的 2011/2014 版本门禁、2016 无 `DeviceID` 标准单点及厂商附带目标编码兼容、2022 固定批量 `DeviceList` 解码；2022 单点结构、任一版本的 `Info/ExtraInfo/ExtralInfo` 或 A.4 对象，以及非法时间、坐标、方向、计数或目标均不得写状态和转发；`DeviceList Num` 可省略，出现时必须匹配 Item 数；
- 2022 升级、抓拍完成和实时视音频回传通知必须使用 `MESSAGE + Notify`，固定 Notify 根，并严格校验字段顺序、单例出现次数、20 位目标及抓拍列表/文件标识完整性；重复/未知字段、属性、命名空间、非法嵌套或额外 XML 指令必须在任何状态或级联副作用前拒绝，标准 XML 声明保持兼容；同一命令误用 SIP `NOTIFY` 必须在生产路由返回 `400 Bad Request` 且不得携带 `Allow`，直接调用处理器也不得产生状态副作用；
- terminated NOTIFY 的设备、Call-ID 和双向 tag 对话绑定，以及伪造终止不得删除或重建下级订阅；
- active/pending/terminated 标准事件 NOTIFY 的 Event、Subscription-State、设备/目标、Call-ID、双向 tag、递增 CSeq、有效期和真实出向订阅绑定；主动 SUBSCRIBE 的全部 `2xx` 最终响应均视为成功，至少覆盖 `202/204`，显式 Expires 接受 0 默认值及 `uint32` delta-seconds 最大值，超过该范围时必须在组包及计时前拒绝；刷新收到 `481` 时四版本均删除旧对话，收到 `489` 时仅 2022 按 RFC 6665 删除而旧三版按 RFC 3265 保留，500 等未列为终止的错误保留最近一次成功期限；active/pending 的合法 `expires` 按版本引用的 RFC 3265/6665 作为事件源权威剩余期限，只能缩短、不能延长本地订阅，早到 NOTIFY 报告的更短期限不得被后续 SUBSCRIBE 最终响应覆盖；terminated 的 `timeout/deactivated` 可立即自动重订，`probation/giveup` 与扩展原因尊重 `retry-after`，`rejected/noresource` 不得自动重订，2022 的 `invariant` 同样不得重订而旧三版按未知扩展兼容，等待期内任何上级刷新不得绕过门禁；Event 事件类型及 `id` 必须逐字节匹配，其他通用参数忽略；重复、负数、引号、溢出或其他畸形已知参数返回 400 且不得删除合法对话；缺失、空值或重复 Event 返回 400，不支持事件包或仅大小写不同的事件类型返回 489，已支持事件与订阅对话不匹配或无订阅时返回 481，且不得产生状态、回调、级联或对话副作用；首个 NOTIFY 早于 SUBSCRIBE 最终响应的合法时序可完成一次远端 tag 绑定；
- 管理端订阅状态接口应合并当前已成功建立的出向对话与持久化恢复意图，并公开事件、目标、原过滤参数、协商有效期、到期/刷新时间、持久化标记、恢复状态和最近 NOTIFY CSeq；页面刷新后应从服务端恢复这些状态，加载失败可重试且不得清空其他设备详情。接口不得泄露内部索引键、SIP 对话对象、认证材料、跨域身份内容或密钥。普通手工订阅会在当前进程内按协商期限自动续订，并保留原事件、目标、过滤条件和已验证的跨域用户身份；订阅意图在生产任务存储中持久化，服务重启或设备重新 REGISTER 后必须建立新的 SIP 对话恢复订阅，不能复用旧连接、Call-ID 或 tag，也不能把尚未恢复的意图误报为活动订阅；
- `Event: Catalog;id=num` 初始订阅、续订、取消，确认数字 id 与正文 `DeviceID` 独立且在原对话保持不变；
- 入向订阅只接受 2011+ Alarm/Catalog、2016+ MobilePosition、2022 PTZPosition；直连设备必须按当前已注册运行态版本执行门禁并忽略单次 SUBSCRIBE 自报的 `X-GB-Ver`，已注册 2011/2014/2016 设备不得伪造 3.0 开启高版本事件；级联仍以对应上级 worker 的协商版本为准；创建、续订和取消均要求 Query 根、正 SN、20 位 DeviceID、必填且匹配的 Event，并在可见目录查询和任何订阅状态副作用前拒绝重复/未知/乱序字段、属性/命名空间和简单字段嵌套；空目标不得扩大为通配订阅；旧版本取消既有高版本事件对话仍可完成清理；
- 入向 SUBSCRIBE 创建必须无 `To tag`；续订和取消必须匹配原 Call-ID、双向 tag、事件、目标和订阅方，并使用严格递增的 SUBSCRIBE CSeq。`Event` 必须且只能出现一次，可选 `Expires` 最多出现一次；重复单值头返回 400 且不得创建、改变或删除订阅及下游引用。缺失/错误 tag、相同或更小 CSeq、取消不存在的对话均应返回 481，且不得改变 Expires、过滤条件、下游引用或删除原订阅；创建、刷新和退订只在成功写出 SIP `200` 后提交本级状态，写失败保留原对话和 CSeq，同请求重传可继续完成；级联下级引用在响应前暂存，写失败必须撤销新增引用并恢复已释放引用；
- Alarm、Catalog SUBSCRIBE/NOTIFY 成功响应按版本区分：2011/2014/2016 必须携带与请求 SN、目标一致的 `Response/{CmdType}` MANSCDP 业务应答，2022 按 RFC 6665 返回空成功响应；旧三版出站订阅/通知收到非空业务 XML 时必须校验唯一 Content-Type、根、字段顺序、命令、SN、目标和 `Result=OK`，不得把 `ERROR`、未知字段或错配响应当作业务成功，空 2xx 保留存量厂商兼容；格式异常的 2xx 不得误删已建立 RFC 订阅；事件源侧旧 NOTIFY 在等待最终失败响应期间若同一对象已经完成 SUBSCRIBE 续订，失败清理必须在订阅操作锁内同时核对当前映射、`cascadeWorker` 和捕获的远端 SUBSCRIBE CSeq，不得因对象指针未变而删除新代次；
- Broadcast 通知与业务应答、接收者主动 INVITE、版本化 SDP、ZLM RTP 启停和 ACK/BYE 清理；入向级联 Notify 在 SIP `200` 和语音异步任务前拒绝重复/未知/乱序字段、属性/命名空间及简单字段嵌套；
- 统一设备查询必须拒绝 `action=broadcast`；语音广播只能进入 2014+ 的 `Notify/Broadcast` 专用流程，不得编码为不存在的 `Query/Broadcast`；
- 2016/2022 对讲在设备音频流建立后复用 RTP 连接反向发送 G.711 A-law 音频；
- 媒体注册、注销、RTP 超时状态竞争；
- 四版本 UDP 媒体抓包必须同时出现与会话匹配的 RTP/RTCP，验证 SR/RR、RTP+1 RTCP 端口、丢包率统计及丢包/拥塞行为；2016/2022 RTP over TCP 必须另外证明 RTCP 使用 RFC 4571 帧随协商的 TCP 媒体路径传输，不能以旁路 UDP RTCP 或仅 RTP 可播放代替；还须验证接收端只接纳协商 SSRC 和可信来源地址，不能以仅能播放代替串流隔离证据；记录目标环境 ZLMediaKit 镜像摘要/提交，禁止用浮动 `master` 标签作为发布证据；
- 1.1/2.0/3.0 能力矩阵和诊断字段。
- 附录 O `m=video ... tcp` SDP 构建/解析，与 `TCP/RTP/AVP` 严格隔离；
- 本地 TCP 文件发送端、已知/未知大小、SHA-256、零字节、大小超限、提前 EOF、超量数据；
- 连接/首字节/空闲/总超时、取消、MediaStatus、单设备/全局并发和终态竞争；正文件大小收满后必须等待 MediaStatus 或 EOF，设备保持连接且不通知时仅允许空闲超时按已验证完整大小兜底；
- 下载倍速按 Catalog 广告列表校验；RTP 下载有文件大小时按接收字节计算近似进度，无文件大小时按媒体轨道时长计算，媒体服务器不提供轨道时长时必须返回 `progress_known=false`；下载进度查询、取消接口、白名单、注册地址校验和原子落盘。
- MediaStatus/121 与入向 BYE 必须同时匹配 Call-ID 和已鉴权设备编号，跨设备同 Call-ID 不得终止 RTP 或 Direct TCP 会话；
- 媒体服务器注销/超时事件对普通直播、回放和 RTP 下载必须原子删除流、向直接下级设备发送递增 CSeq 的会话内 BYE、关闭 RTP 接收端口，并终止引用同一媒体源对象的上级级联会话；重复事件不得重复发送 BYE 或关闭端口；旧流迟到清理不得凭复用的设备、通道和 StreamID 终止重建后的新代次级联会话；
- 注册、目录、媒体会话、异常断流和直接 TCP 下载灰度计数器。
- LALMAX `code/msg` 与 `error_code/desp` 响应兼容；管理请求携带配置 token，主动健康检查必须真实访问配置接口；`stat/group` 映射媒体存在性、音视频轨道、码率及累计字节，但当前不暴露媒体时长，因此无 `filesize` 的 RTP 下载进度必须保持未知；直播、回放和语音接收结束后通过 `stop_rtp_pub` 幂等释放 RTP 接收会话；其公开 `start_rtp_pub` 请求当前没有 SSRC 字段，不能把 LALMAX 路径标记为已完成 SSRC 防串流过滤，真实验收必须单独记录该能力差异；新版 ZLM 兼容接口覆盖关闭流与开始/停止录像，缺失录像状态列表时必须返回错误而非空成功；
- 媒体节点连接、动态 Hook 配置和状态持久化必须全部成功后才能显示在线；初次失败保持离线，探测恢复重新执行完整配置，数据库失败返回错误且不得触发后台 `panic`；应用清理必须等待节点检查器退出；
- Webhook 目标为空时 GB 报警事件分发器必须是真正的 nil 接口；并发分发/关闭不得向已关闭队列写入，关闭需等待已入队事件。事件清理、录像清理和录像同步 worker 必须响应应用取消并在清理函数返回前退出；Wire 注入代码必须可重复生成；
- 四版本级联 REGISTER、Catalog、UDP/TCP/TLS、下载倍速、订阅能力矩阵；
- 四版本协议时间固定按北京时间编码/解析；查询与控制 SN 为单调正整数，RecordInfo 无响应、空结果、部分分包、重叠区间及午夜分日能够区分；
- 上级级联信令可配置 UDP/TCP/TLS；TCP/TLS 同一连接完成 REGISTER Digest 与 Keepalive，断线后同一 Call-ID 事务切换新连接，301/302 可切换到 TCP/TLS 或 `sips:` 目标，且 SIPS 不允许降级到明文传输；REGISTER 同时覆盖 `401 + WWW-Authenticate → Authorization` 和 `407 + Proxy-Authenticate → Proxy-Authorization`，两类挑战各最多认证一次，先后挑战时最终请求必须同时保留两类凭据、持续递增 CSeq 并保持 Call-ID；歧义或不支持的 Digest 挑战不得触发认证请求；
- 四版本上级 Keepalive 失败进入重注册窗口后，配置热更新或停服仍须对尚未到期的最后成功绑定发送 `REGISTER Expires: 0`；不得因为本地已暂时标记为未注册而在上级留下幽灵绑定，从未成功注册或已过期绑定不额外发送退注册；停止已经开始但上级随后才确认的正有效期 REGISTER 同样必须立即退注册，期间不得把 worker 重新暴露为已注册；停止导致的 Keepalive 返回不得启动下一轮 REGISTER；Keepalive 成功响应跨过 `ExpiresAt` 时记录心跳结果但保持绑定过期；
- 2022 上级 REGISTER 301/302 重定向，校验 Contact 的 ServerID、SIPS/transport，重定向后重新 Digest 注册，后续心跳、通知、媒体请求和入向身份校验绑定新地址；
- 共享通道 DeviceInfo、DeviceStatus 和 RecordInfo 查询，录像响应分包并映射为上级可见编码；本级平台 DeviceInfo 在共享目录存储读取失败时须向四版本上级发送业务层 `Result=ERROR`，不能只返回最初的空 SIP `200 OK` 后让业务查询超时；本级系统 RecordInfo 只读取显式共享通道的中心录像，过滤待删除及不重叠记录。2014/2016/2022 缺省或 `IndistinctQuery=0` 按 SIP `To` 选择中心或前端，`IndistinctQuery=1` 合并两端结果并去重；2011 保留系统目标查中心、共享通道目标查前端的兼容路由；NVR 代表已知子通道返回 DeviceInfo 时只更新子通道元数据，四版本 `Channel` 及旧三版 `MaxCamera/MaxAlarm` 计数须非负，2014 新增 DeviceName 不得向 2011 透传；DeviceInfo 尾部基本扩展按 2011/2014/2016 纯文本 `Info`、2022 纯文本 `ExtraInfo` 校验并限制每项 1024 个 Unicode 字符，2022 结构化 `<Info>` 仅接受已识别附录 A.4 对象；
- DeviceInfo 入向应答必须按版本保持标准字段顺序和单例结构，拒绝重复/未知字段、属性/命名空间、简单字段嵌套和乱序，且不得写状态、解除等待、持久化或发布事件；2011 禁止 DeviceName，2014+ 的可选 DeviceName 必须位于 Result 前。`DeviceType/MaxCamera/MaxAlarm` 仅兼容 2011/2014/2016，出向级联保持 `DeviceName → Result` 和 `MaxCamera → MaxAlarm → Channel` 顺序；2022 入向必须拒绝、出向不得生成这三个已删除字段，并按 `DeviceName → Result → Manufacturer → Model → Firmware → Channel` 输出；
- 上级 Query 请求的固定根、正 SN、20 位目标、命令白名单、协商版本及专属载荷校验；2016/2022 RecordInfo 缺失或四版本倒置时间、2011 携带 `IndistinctQuery`、非 2022 携带兼容别名 `DistinctQuery`、旧版携带 2022 过滤字段、MobilePosition 负间隔和 CruiseTrackQuery 缺失/越界 Number 必须在返回 SIP `200` 和启动下级任务前拒绝；2011/2014 省略的 RecordInfo 起止时间不得在下发时伪造；标准出站字段必须为 `IndistinctQuery`，两个字段名同时出现必须拒绝；
- 共享通道 1.1 PresetQuery、2.0 MobilePosition、3.0 HomePositionQuery/PTZPosition/SDCardStatus 查询转发；所有上级入向 Query 在 SIP `200` 和异步转发前按命令拒绝重复/未知/乱序字段、属性/命名空间、简单字段嵌套和跨命令载荷；2014 设备查询和级联响应必须发 `PersetQuery`，2016/2022 必须发 `PresetQuery`，入站兼容两种拼写；MobilePosition 必须在下级 SIP `200` 后结束查询受理，并把后续真实 `MESSAGE/Notify/MobilePosition` 按原上级、暴露通道和 SN 路由转发，不得等待、接受或伪造不存在的业务 Response；真实 SIP/TCP 分派必须进入位置通知处理器并保存状态；查询与订阅级联必须执行 2016 无目标单点 ↔ 2022 批量结构转换，2022 批量只保留显式共享 Item，降到 2016 时逐 Item 发送且不输出 `DeviceID/SumNum/DeviceList/Height`；2016 无目标时从已匹配对话或明确路由恢复目标，同一来源多候选时不得猜测；2022 本级系统编码目标只向显式共享、在线且由 `MobileDeviceType` 或 138 类型编码识别的移动通道扇出，最多 16 路并发，过滤未受理来源并聚合为按暴露编码稳定排序、最新位置去重且 `SumNum == DeviceList Num == Item 数` 的完整快照；重复查询重置旧快照，多上级和失败来源隔离；上下级 SN 转换、DeviceID/ParentID 安全映射、未知编码拒绝及多上级响应隔离；下级失败按命令 Schema 生成合法最小应答，Catalog/RecordInfo/Preset/巡航/存储卡不再统一携带非法 `Result`，DeviceStatus 补齐必填状态；
- 3.0 附录 A.4 扩展对象按 Catalog/RecordInfo ExtraInfo、Alarm 嵌套对象和 DeviceStatus 响应的真实承载路径级联；普通 `string` 的 `ExtraInfo` 在 A.4 快照、录像响应重建和 Catalog 级联提取中保留首尾空白，结构化 JSON 解析不得覆盖原值；RecordInfo 分包扩展随查询生命周期聚合、超时/发送失败/完成/停服均清理，并只在级联首包输出；共享对象递归编码映射，未知 20 位对象编码整条拒绝，且 1.0/1.1/2.0 不输出 3.0 ExtraInfo；1.0/1.1/2.0 入站携带 `doorType`、`detectorType`、`ExtraInfo/ExtralInfo` 等 A.4 对象时，必须在目录或录像聚合、状态更新、等待解除、持久化、回调和订阅转发前返回 400，通用解码层也不得保存；MobilePosition 四版本均不承载 A.4 或 `ExtraInfo/ExtralInfo`，发现即拒绝；3.0 查询只复用首次版本校验后的 A.4 结构化结果，处理期间设备运行态消失不得触发版本回退重解码或丢失扩展；
- 3.0 PTZ 精准位置变化事件订阅/通知及级联；1.0/1.1/2.0 版本门禁，通知通道编码映射和非共享通道隔离；上级 Catalog/Alarm/MobilePosition/PTZPosition 订阅自动建立下级订阅并覆盖续订、退订、引用计数、过期和上级移除清理；Catalog 会订阅承载共享通道的下级 NVR，并在目录快照变化后为新增或迁移通道补订阅；
- Catalog NOTIFY 的共享目录读取、过滤、分包发送和快照提交必须绑定任务开始时的上级 worker 与远端 SUBSCRIBE CSeq；续订发生后旧任务不得把剩余分包套用到新代次，也不得提交旧过滤条件生成的 `CatalogSnapshot`。旧任务已发送但未完整提交时保留最后一个完整成功快照，使后续通知能够重新计算并补发；
- 上级事件续订触发的下级 SUBSCRIBE 刷新必须在下级成功响应后才提交新的请求期限；刷新失败保留上次已确认输入。普通刷新和 terminated 自动重订的成功收尾不得清除网络等待期间由更新 terminated NOTIFY 写入的 `retry-after` 或禁止重订状态，终止策略必须按代次提交；
- 3.0 CruiseTrackListQuery/CruiseTrackQuery 的请求、结构化响应、轨迹编号参数和安全级联；
- 3.0 看守位、巡航和存储卡查询按 A.2.6 原始 Schema 校验整数：`ResetTime`、巡航点 `PresetIndex/StayTime`、`Capacity/FreeSpace` 不附加标准未定义的非负、字节范围或容量大小关系；明示的 `Enabled`、看守位 `PresetIndex`、巡航编号/速度及格式化进度范围继续严格执行。真实 SIP `MESSAGE/Response` 已覆盖成功确认、等待者解除和结构化状态保存，级联看守位控制采用相同 `ResetTime` 规则；
- 3.0 附录 H `X-PreferredPath` 首跳消费/剩余路径转发和 `X-RoutePath` 响应前置；平台编码、重复路径、错误首跳、下级确认不匹配及路由环拒绝；不同指定路径的实时、回放和下载媒体源隔离及正确释放；
- 3.0 DeviceConfig 命令使用 `SnapShotConfig`；DeviceConfig 业务应答的标准核心仍只有 `CmdType/SN/DeviceID/Result`，受控保留已发布的 `VendorResult` 和经附录 A.4 校验的 2022 `Info`，不接受 `SnapShotConfig` 回显。ConfigDownload 查询应答使用标准 `SnapShot`，其查询响应解析继续兼容厂商沿用的 `SnapShotConfig`，级联输出必须规范化为标准节点；
- 3.0 `DeviceUpgradeResult` 与 `UploadSnapShotFinished` 最终通知同时校验严格附录 A XML 结构、已鉴权设备、`SessionID` 和原目标通道，同一 NVR 的兄弟通道不能覆盖会话终态；命令受理后、最终通知前重启进程，直连及级联下游会话必须从 `gb_task_states` 恢复，未知会话不得自行建档；相同终态重传不得刷新首次结果，矛盾终态返回 409 且不得覆盖；级联最终通知还必须恢复并回送原发起上级，发送成功但完成墓碑落库失败时只重试持久化，不得重复上送；
- 3.0 `VideoUploadNotify` 校验严格附录 A XML 结构、固定命令类型、正 SN、设备编码和时间；经纬度按 A.2.5.8 的字段说明均可独立省略，出现时必须为有限值并分别位于 `-180..180`、`-90..90`，未知跨设备目标、未知 A.4 节点及其他非法结构不能写入查询/A.4 状态或触发级联；
- 2.0/3.0 HomePosition 及 3.0 PTZPreciseCtrl 按标准边界拒绝无效参数；`HomePosition Enabled=0` 时不得携带仅供开启状态使用的 `ResetTime/PresetIndex`，3.0 `HomePositionQuery.ResetTime` 按普通可选整数处理并接受 `0`，精准控制 Pan 为 `0..360`、Tilt 为 `-30..90`、光学变焦倍数 Zoom 不小于 `1.0`。统一 `ptz_cmd` 接口支持动作名编码和 16 位原始命令，原始命令校验 `A5 0F` 头与校验和。PTZCmdParams 必须伴随 PTZCmd，仅允许上下游均为 3.0，`PresetName` 仅允许设置预置位命令，`CruiseTrackName` 仅允许 `0x84..0x88` 巡航命令且按原始出站值限制为不超过 32 字节，两者不能同时出现；名称首尾空白原样输出，全空白值不作为有效参数，旧版本空扩展节点也拒绝；3.0 报警复位校验方式组合和按方式限定的报警类型，升级结果校验正 SN、结果枚举、固件和失败原因，抓拍完成通知校验正 SN、命令类型及最多 10 个文件标识；
- 升级结果通知的 `Firmware` 按必选元素而非非空文本校验，缺失拒绝、`<Firmware/>` 接受；首尾空白在状态保存和级联原报文中保持不变，失败原因作为枚举继续规范化；
- 级联 DeviceControl 拒绝脱离主命令的从属字段：孤立 `StreamNumber`、`Info`、`DeviceID2/TargetArea` 必须在 SIP `200` 前返回 `400`；录像控制 `StreamNumber` 只接受 2022 上级的非负整数，非零值只转发给 2022 下级，跨到旧版的零值按缺省主码流语义裁剪；2011/2014 PTZ `Info/ControlPriority` 只允许伴随 `PTZCmd` 且上下游均为 1.0/1.1，不得与报警复位 Info 混用；
- DeviceControl 普通文本扩展位于控制动作之后、允许多项且单项不超过 1024 个 Unicode 字符；HTTP/Core/Adapter 直连下发和四版本双向级联均保留首尾空白、空值及 XML 转义，2011/2014/2016 使用 `Info`，2022 使用 `ExtraInfo`，跨版本按目标版本自动改名。级联入口还必须在 SIP `200` 前拒绝重复/未知/乱序顶层字段、属性/命名空间、简单字段嵌套及缺少必填子字段的复杂控制参数，并区分结构化报警复位 `Info` 与普通文本扩展；
- A.4 ExtraInfo JSON 的整数、零值、数组和 20 位数值型编码保持原始精度；未知数值型对象编码同样拒绝转发；
- 共享通道 PTZ、录像和版本化通道控制转发；2011/2014/2016 录像控制不输出 `StreamNumber`，2022 接受任意非负码流编号并省略缺省 0；2011/2014 PTZ 可选 `Info/ControlPriority` 按规范性消息示范编码并在旧版间级联保留；强制关键帧在 2016 使用 `IFameCmd`、2022 使用 `IFrameCmd`，入向字段与版本不匹配或两字段并存时拒绝，2016↔2022 级联按下游版本转换；2022 TargetTrack 顶层球机通道与可选全景通道均需属于同一设备，级联时两者均需显式共享并独立映射；DeviceControl 按四版本标准区分有无业务应答：PTZ、TeleBoot、DragZoom、IFrame 及 2022 PTZPrecise/FormatSDCard/TargetTrack 在 SIP `200 OK` 后完成，不登记业务 pending；Record/Guard/AlarmReset/HomePosition/DeviceUpgrade 等仍等待业务 Response。级联上游无应答命令不发送 `Response/DeviceControl`，非法请求在 SIP `200` 前返回 `400`，设备迟到或额外发送的合法 Response 仍以 SIP `200` 兼容确认；设备级高影响控制不因共享单个通道而被级联放大；
- 通用查询严格校验 `MESSAGE + Response`、订阅事件严格校验 `NOTIFY + Notify`，并校验正 SN、命令版本和目标所有权；DeviceStatus、PresetQuery、HomePositionQuery、CruiseTrackListQuery、CruiseTrackQuery、PTZPosition、SDCardStatus 进一步拒绝重复/未知字段、属性/命名空间、简单字段嵌套、乱序及列表属性错误；NOTIFY 不能解除 MESSAGE 查询等待，2022 独立业务通知走专用包络，未知跨设备报文不能改变状态或解除等待；
- 通用 NOTIFY 只允许 2022 PTZPosition；DeviceStatus、Preset/HomePosition/CruiseTrack/SDCardStatus/ConfigDownload 等查询或状态命令的 NOTIFY 必须拒绝，MESSAGE 查询应答不得转发成订阅事件；
- DeviceInfo 固定 `Response` 根和必填 `OK/ERROR` 结果校验；空结果或 Notify 根不得更新父设备/子通道元数据或解除等待；
- PresetQuery/HomePositionQuery/CruiseTrackListQuery/CruiseTrackQuery/PTZPosition/SDCardStatus 业务载荷的标准字段顺序、出现次数、列表属性、必填字段、列表计数、枚举、数值范围和有限浮点校验；非法载荷不能写状态、解除等待或转发订阅。2014/2016 PresetQuery 不要求 `SumNum`，并受控兼容存量设备在 `PresetList` 前附带单例合法 `Result/SumNum`；
- DeviceInfo 非法根、命令、SN 或未知目标不得覆盖父设备/子通道信息或解除等待；
- DeviceStatus 缺失或非法 Result、Online、Status 枚举不得写状态或解除等待；可选 Encode/Record、DeviceTime 及四版本 Alarmstatus 计数、设备编码、枚举和版本字段名同样必须在副作用前校验；`Online` 独立决定设备在线态，`Status=OK/ERROR` 只表示是否正常工作，不能把 `OFFLINE + OK` 误判为在线；失败结果及子通道状态不得改写父设备在线运行态；
- DeviceControl 与 Broadcast 非法根、固定命令、SN、结果枚举、未知目标或不匹配在途请求目标不得解除业务等待；Broadcast Response 还必须拒绝重复/未知字段、属性/命名空间、简单字段嵌套和标准字段乱序；
- 普通设备 `MESSAGE/NOTIFY/SUBSCRIBE` 在进入命令 handler 前必须有当前注册运行态；公共门禁把业务上下文版本归一化为该运行态的有效版本，同时保留请求原始 `X-GB-Ver` 仅供诊断。未知设备及仅持久化但尚未恢复注册的设备返回 403，运行态存储不可用返回 503，且不得通过自报目标或版本写状态、解除等待、触发回调、开启高版本能力或级联转发。附录 G 外部系统和已注册上级平台仍按各自身份门禁处理；附录 G 分流必须在切换外部系统凭据域前严格确认唯一根与唯一直接简单、精确匹配 Schema `fixed` 值的 `CmdType`，允许无命名空间或标准附录 G 命名空间，拒绝命令首尾空白、重复命令、非命名空间属性、外来或中途变更的命名空间、嵌套值、多根和额外 XML 指令；
- 2011/2014/2016 附录 G `ConfigDefence.Type` 必须按 XML Schema `boolean` 仅接受 `true/false/1/0`，拒绝 Go 默认解析器额外接受的 `TRUE/False/t/f` 等非标准词法；2022 继续拒绝附录 G；
- Keepalive 非法根、命令、SN、源设备、状态枚举或故障设备编码不得补载设备、改在线态、写查询状态或触发 Catalog；重复/未知字段、属性/命名空间、简单字段嵌套和标准字段乱序同样必须在副作用前拒绝。合法包络但未注册、未持久化建档的未知设备在直接 handler 路径返回 403，设备存储不可用返回 503；进程重启后持久化存在但内存缺失的设备应恢复并只触发一次 Catalog。标准 `Status=ERROR` 表示设备工作异常，合法心跳仍证明发送方在线；空 Status 及 ON/OFF 厂商兼容路径须保留，其中既有 `OFF` 继续作为显式离线扩展；
- Alarm 非法根、命令、SN、目标、级别、方式、时间或非有限经纬度不得写附录 A.4、触发业务回调或向订阅方转发；2011/2014 只接受多项纯文本 `Info` 且单项不超过 1024 个 Unicode 字符，不得接受 2016 新增的结构化 AlarmType 和 AlarmTypeParam/EventType；2016 最多接受一个结构化报警类型 `Info`，出现时 `AlarmType` 必填且必须位于可多项纯文本 `Info` 之前；2022 将纯文本扩展改名为 `ExtraInfo`，跨版本字段、重复结构化项和未知结构化项必须拒绝；报警级联必须按目标版本转换普通文本 `Info/ExtraInfo`，向 2011/2014 裁剪结构化报警类型和 A.4，向 2016 保留可表达的纯文本扩展并裁剪 A.4，并将 2022 独有的方式 5/类型 13 降级为不带结构化类型字段、但保留基础报警和普通文本扩展的报警；2016/2022 的 EventType 仅允许方式 5、类型 6、值 1/2；
- 2022 Alarm 最多只允许一个结构化 `Info`；报警类型与附录 A.4 对象应在该节点内组合，纯文本扩展使用可多项的 `ExtraInfo`。第二个 `Info` 必须拒绝，向 2016 转换时必须保留合法报警类型并裁剪 A.4 对象；
- 四版本收到 `MESSAGE/Notify/Alarm` 后必须先返回空正文 SIP `200 OK`，再独立发送匹配原 SN、报警目标且 `Result=OK` 的 `MESSAGE/Response/Alarm` 并等待 SIP 确认；订阅 `NOTIFY/Alarm` 只能返回 SIP 确认，不得误发业务响应 MESSAGE；
- MediaStatus 对四版本开放；非法根、命令、SN、设备、空通知类型、重复/未知字段、属性/命名空间、简单字段嵌套、标准字段乱序及活动会话目标不匹配均不得结束任务；未知类型及已清理会话重复 121 应保持 200 幂等确认；上级转发可在本级响应前决定 4xx/5xx，但普通流删除、RTP/裸 TCP 下载终态和慢速清理只能在本级 200 实际写出成功后提交，写失败重传不得重复已成功的上级通知；
- ConfigDownload 的 BasicParam 按版本校验字段存在性：2014 十项必填、2016 四项必填、2022 四项可选；出现的 `Expiration` 必须满足 9.1.1 的注册过期时间下限 `3600s`，2016/2022 省略该字段仍合法。2016/2022 Schema 中心跳间隔/次数是无显式上限的 `integer`，正整数原值应保存。只有两项同时出现且均在本地 `uint16` 运行态范围内时才更新离线参数，超范围值不得截断或覆盖运行态；非正值仍在副作用前拒绝。失败响应和子通道 BasicParam 不得改写父设备心跳运行态；合法应答处理期间运行态恰好消失时仍须返回 SIP 200、保存查询状态并解除等待；
- DeviceConfig/ConfigDownload 缺失或非法 `Result` 不得写状态、改运行态或解除等待；DeviceConfig 响应必须匹配原目标通道，兄弟通道复用 SN 不得抢占配置或抓拍等待；
- DeviceConfig 非法版本、根、命令、SN 或目标，以及除受控 `VendorResult`/2022 A.4 `Info` 外的重复/未知/乱序字段、属性/命名空间、简单字段嵌套或非标准 `SnapShotConfig` 应答载荷，均不得写状态、附录 A.4 或解除等待；
- RecordInfo 响应在进入多响应聚合器前必须按版本校验顶层与条目字段顺序、单例、属性/命名空间和简单字段嵌套；2016 条目只在标准尾部增加 `FileSize`，2022 再增加 `RecordLocation/StreamNumber`，模糊查询返回项必须携带合法 `RecordLocation`，尾部扩展由旧版多项简单 `Info` 改为 2022 多项简单 `ExtraInfo`。级联按实际协商版本裁剪条目和扩展：即使平台配置上限为 2022，协商降级到 2011/2014/2016 后也不得输出 `ExtraInfo`。重复/未知/乱序/跨版本字段及非法列表或条目属性不得污染聚合结果；
- 2022 DeviceConfig/ConfigDownload 的 OSD、视频参数属性、录像计划、报警录像、PictureMask、FrameMirror 和 AlarmReport 配置必须校验必填字段、声明计数、列表上限、0/1 或 0~3 枚举、0~2 码流编号、周/时分秒、非负录像时间、遮挡区域及 OSD 文字长度；本地下发的 `VideoParamAttribute Num` 必须与 Item 数一致，非法本地或级联请求不得发往设备，非法查询应答不得写状态或解除等待；
- Catalog 非法根、命令、SN、非 20 位顶层目标、负 SumNum 或非法目录项值不得进入多响应聚合、写入目录或解除查询等待；1.0/1.1/2.0 稀疏厂商目录仍允许省略可兼容字段，3.0 只强制标准明确标为必选或附录 J 固定列出的条件字段；
- 四版本历史级联 `INVITE→ACK→INFO→BYE` 完整对话、媒体启动/释放和 MANSRTSP/RTSP 转换；非空 INFO 请求和业务响应都必须只有一个可解析、参数合法的 `Application/MANSRTSP` Content-Type；1.1+ 入向 INFO 不得继续接受 `MANSRTSP/1.0`；原始 Cmd 必须经解析并重写目标版本和新 CSeq，`clock`、负数、SMPTE 三段时间戳误带子帧及其他畸形 Range 必须拒绝；外层 SIP 200 的空正文只走存量兼容，非空正文必须校验 Content-Type、版本、关联 CSeq、业务状态和 `Range/RTP-Info/Scale`，下级业务 500 不能伪装为成功，合法成功/失败响应均须重写上级 CSeq 后保留业务头；未完成 ACK 建立或被校验拒绝的 INFO 不得刷新对话活动时间，只有成功写出 2xx 的合法控制及其精确重传可刷新，避免非法请求让待清理会话无限续命；
- SIP 响应添加 To-tag 时不得修改原请求；级联 INVITE 缓存响应只对 CSeq、Request-URI、顶层 Via 均一致的原事务重传复用，相同 Call-ID/tag 的另一事务返回 491 且不得获得原 200/SDP；历史 `TEARDOWN` 成功后收敛本地/级联历史会话与 RTP 资源，2014/2016 只允许 Scale 或 Range 单独出现，2022 的 Scale+Range 仅限负 Scale 倒放；
- 2011 结构化倍速控制携带 `seek_at` 时必须生成 `Scale + npt Range`，独立随机拖放仍生成 `smpte Range`；2014/2016 的倍速控制不得携带 Range。2022 负 Scale 与 Range 组合必须生成指定位置倒放控制；有界倒放只接受结束点小于起点的降序 Range，普通播放及 2011/2014/2016 不得接受降序 Range。2022 仅携带 Range 时必须沿用该历史会话最近一次成功的 Scale：默认正向、负倍率成功后接受降序并拒绝升序，恢复正倍率后方向门禁同步恢复；失败业务响应不得提交倍率。业务响应若返回 Scale，应以实际返回值更新会话状态。业务响应 `RTP-Info` 允许仅携带 `url/seq/rtptime` 中任一已知参数，但空条目、未知参数及重复参数必须拒绝；管理端分别在 2011 倍速控制和 2022 负倍率时显示可选播放起点；
- 缓存响应经事务发送并附加信令摘要/用户身份头后，原缓存对象仍不含本次发送产生的可变安全头；并发重传须通过 race 验证；
- 入向媒体对话 ACK 必须复用原 INVITE CSeq；CANCEL 必须匹配原 INVITE 的双向 tag、CSeq、Request-URI、顶层 Via 协议/传输/Sent-by/branch；CANCEL 的 `200 OK` 写出和原 INVITE 最终响应提交必须串行，阻塞写窗口内不能建立级联或广播媒体，写失败后仍允许同请求重试；INFO/BYE 的远端 CSeq 必须严格递增，合法 INFO 重传复用缓存响应且只执行一次下游控制，同 CSeq 改报文或跨方法回放不得产生媒体副作用；
- 无持久化通道记录的 `cascade-*` 指定路径/语音级联媒体流注册、注销及断流会话释放；
- 两个已注册上级之间的级联对话所有权隔离，非会话所属上级的 ACK/CANCEL/BYE 不得改变或终止会话；
- 同一已注册上级或设备复用 Call-ID 但改变远端 From tag 或本地 To tag 时，ACK/CANCEL/BYE/INFO 不得改变或终止原级联/广播对话；
- 普通设备直播/回放/下载/对讲及级联语音源收到 BYE 时校验 2xx 建立的双向 tag；同设备或上级复用 Call-ID 但改变 tag 不能终止原出向媒体对话；
- Catalog 查询和订阅均支持平台根或单个共享通道目标；2014/2016 还必须接受显式共享目录图中可见的行政区划、系统、业务分组、虚拟组织和父设备目标，拒绝图外编码，并将通知限制在目标子树；1.0 变化通知使用 `Response` 根，1.1+ 使用 `Notify` 根；目录快照只产生 ADD/DEL/ON/OFF/UPDATE 增量，分包中 `SumNum` 与本包 `DeviceList Num` 一致；同一订阅并发触发不重复发送，任一分包失败时不提交快照并在下次重试；
- 1.1+ 域间目录初始订阅只通知离线/异常目录，刷新订阅保留 NOTIFY CSeq；续订与过期清理并发时由续订保留有效会话，过期会话释放下级引用；空消息体 terminated NOTIFY 返回 200 并清理/按引用重建下级订阅；未重复携带 X-GB-Ver 的上级 SUBSCRIBE 复用注册协商版本；Alarm/MobilePosition 和 1.0 目录订阅不误发初始 Catalog；Alarm 订阅按级别、方式、类型和时间过滤，兼容 2011 `StartTime/EndTime` 示例别名；组合报警方式按无序“或”集合处理，`25/52/2/5/5/2` 规范化后共享订阅键和级联引用，出站按下级版本编码，`AlarmType` 至少匹配组合中的一种方式且遵循 2016/2022 值域；
- 设备和上级 NOTIFY 的任意 `2xx` 响应均保留订阅；2011/2014/2016 收到 `481` 时无条件删除，收到 401/407 或携带语法合法且仅出现一次的 `Retry-After` 时保留，其他确定错误删除；2022 仅在 `404/405/410/416/480–485/489/501/604` 时删除，500/503 等未列错误保留。事务失败按既有软状态策略处理；删除必须精确匹配订阅对象并只释放一次下级引用，保留时 Expires、Dialog 和下级引用不得变化；调用方取消不得删除订阅，旧 NOTIFY 的迟到失败不得删除同键替换对象；未重复携带 `X-GB-Ver` 的上级订阅必须保存协商版本；
- 四版本级联事件 NOTIFY 收到唯一且合法的 `401 + WWW-Authenticate` 或 `407 + Proxy-Authenticate` Digest 挑战后，必须使用目标上级独立配置的 LocalID/Password 仅认证重试一次；重试使用新的 Via branch 和递增 CSeq，保持 Call-ID、对话路由、Event、Subscription-State、XML 正文及 `Monitor-User-Identity` 不变。发送重试前必须重新核对级联 worker 和远端 SUBSCRIBE CSeq 代次；重复挑战、缺失/重复挑战头、缺失 `Digest` 方案、重复 `realm/nonce/algorithm/qop`、非法参数、未支持算法或 qop 均返回错误且不得循环，认证失败不得误删按版本规则应保留的订阅；
- 四版本级联普通 `MESSAGE`（含 Keepalive、查询结果、分包结果、控制/配置应答、任务通知及会话内 MediaStatus）收到唯一且合法的 `401 + WWW-Authenticate` 或 `407 + Proxy-Authenticate` Digest 挑战后，使用该上级独立 LocalID/Password 最多认证重试一次。重试只生成状态对应的 `Authorization` 或 `Proxy-Authorization`，刷新 Via branch、递增 CSeq，并保持 Call-ID、对话/目标 URI、XML 正文、`X-GB-Ver` 和 `Monitor-User-Identity`；启用 `Date + Note` 时移除旧摘要并重新签名。重复挑战不得发送第三个请求，畸形或歧义挑战不得发送认证重试；Keepalive 仅在最终 `200 OK` 后刷新成功时间和注册状态，认证失败不会伪造心跳成功；
- 2011/2014/2016 附录 G 出向 SIP MESSAGE 收到唯一且合法的 `401 + WWW-Authenticate` 或 `407 + Proxy-Authenticate` 后，必须使用目标外部系统独立 ID/Password 和配置 realm，仅认证重试一次；直连只生成 `Authorization`，代理只生成 `Proxy-Authorization`。重试刷新 Via branch、递增 CSeq，保持 Call-ID、目标 URI、XML 正文与 `X-GB-Ver` 不变；重复挑战不得发送第三个请求，缺失/重复挑战头、缺失 `Digest` 方案、重复关键参数、畸形参数、错误 realm、未支持算法或 qop 均不得发送认证重试；
- 四版本 Alarm、MobilePosition、PTZPosition 及本地通用事件向多个订阅者发布时，每个订阅方必须使用独立且受 GB 服务生命周期管理的 FIFO 发送 worker；级联批次还必须绑定具体上级 worker。首个订阅者不返回 SIP 最终响应时，其他本地订阅者和其他上级仍须立即开始 NOTIFY，发布入口不得等待任一目标响应；同一订阅最多一个 worker，并由单独发送锁串行 CSeq，MobilePosition 同一事件的多段版本转换保持同批次依次发送。等待队列上限为 128 个批次和 4 MiB 正文，溢出后必须按对象身份及远端 SUBSCRIBE CSeq 代次摘除慢订阅、清空未发送队列并异步释放下级引用，不能阻塞入站事件 handler；当前已发送批次允许完成。旧过载清理取得订阅操作锁时必须重查代次：同一对象已续订时保留新代次并只恢复对应旧过载标记，且不得清除更晚代次的新过载；续订提交应主动恢复旧过载状态。异步正文必须复制，批次在发布快照、入队和实际生成请求的状态快照重新核对当前订阅对象、到期状态、捕获的本地/级联 worker 身份及远端 SUBSCRIBE CSeq 对话代次；退订、替换、同对象续订、设备离线、worker 热更新或停服后的旧排队批次不得补发，续订后的新事件进入新代次；单个失效 worker 不能终止后续订阅遍历。自动化覆盖 `TestEventNotifyTargetsStartIndependentlyFourVersionMatrix`、`TestLocalEventNotifyTargetsStartIndependentlyFourVersionMatrix`、`TestEventNotifyUsesSingleFIFOQueuePerSubscriptionFourVersionMatrix`、`TestEventNotifyDropsQueuedBatchAfterSubscriptionRenewalFourVersionMatrix`、`TestEventNotifyQueueOverflowDoesNotDeleteRenewedSubscription`、`TestEventNotifyOverloadStateIsIsolatedBySubscriptionGeneration`、`TestEventNotifyQueueOverflowDetachesSlowSubscription`、`TestEventNotifyQueueByteLimitDetachesSubscription`、`TestEventNotifyDispatchRequiresCapturedCascadeIdentity` 以及过滤、版本转换、映射、失败删除和生命周期回归；
- 同一下级订阅对话的并发 NOTIFY 必须串行完成二次对话校验、业务校验、SIP 应答和 CSeq/期限/terminated 状态提交；较大 CSeq 不得越过已进入处理的较小 CSeq，不能出现两个请求均返回 200、但较小 CSeq 的已确认业务事件被丢弃。过期清理、设备删除、主动取消、SUBSCRIBE 失败和停服摘除不得越过已接纳请求；取得锁后必须重查对象和期限。出向 SUBSCRIBE 刷新或取消失败时，还必须保留等待期间合法 NOTIFY 推进的更大 CSeq、最新 Contact 远端目标和更短的权威期限，不能把下一次刷新导向旧目标或恢复更长期限，也不能误提交待确认刷新临时扩大的期限。`Subscription-State` 报告的 `expires` 和 terminated `retry-after` 必须从请求接纳时刻起算，慢业务处理或阻塞响应不得重新起算并延长期限/退避；重订状态的绝对 `RetryAt` 与实际定时器必须一致，已经流逝的退避窗口不能在提交后再次完整等待。主动 `Expires: 0` 的临时在途窗口允许早到 terminated NOTIFY，但成功 2xx 后的 Timer N 必须从最终响应重新启动完整 64×T1，慢响应不得预先消耗该窗口。接纳时有效的请求即使 TCP 响应写出跨过订阅到期时刻仍须完成提交，后续请求再按当前期限拒绝。不同订阅键保持并发；
- 主动退订成功后的 Timer N 内，取消前已经在途、随后迟到的 active/pending NOTIFY 仍须提交更大 CSeq、最新 Contact 和从接纳时刻计算的报告期限，但不得用较短 `expires` 提前缩短等待最终 terminated NOTIFY 的实际清理窗口；若退订最终失败，快照恢复必须再将该较短权威期限同步回 NOTIFY 与外层刷新状态；
- 取消等待期内连续收到多个合法 active/pending NOTIFY 时，报告期限必须保持绝对时间单调不延长；较晚请求可以推进 CSeq、更新 Contact，但较长 `expires` 不得覆盖先前较短期限。取消成功继续保留完整 Timer N，取消失败恢复到等待期间最短权威期限；
- 设备与上级事件订阅键包含订阅方身份；复用相同 Call-ID、From tag、事件类型和目标的其他订阅方不能覆盖、续订或取消原会话；
- 2022 H.265 SDP 按附录 C.2.5 使用动态负载类型 `100`，不与 SVAC 建议使用的 `99` 混用；设备级关闭 `download_speed` 或 `h265` 后，运行时分别不再发送下载倍速或 H.265 SDP 声明，能力快照与实际发送门禁一致；
- 1.1/2.0/3.0 级联 Broadcast 通知、上游语音源 `401/407` 严格 Digest INVITE、ZLM 音频接收、下游接收方主动 INVITE 和 RTP 转发；直连挑战必须生成 `Authorization`，代理挑战必须生成且只能生成 `Proxy-Authorization`，认证重试使用新 Via branch、递增 CSeq，并保持 Call-ID、目标 URI、Subject 和 SDP offer；重复挑战最多发送一次认证重试，失败后关闭已打开的 RTP 接收端。1.1 PS/90000 与 2.0/3.0 PCMA/8000 版本矩阵；多通道 Subject 定位、CANCEL、双侧 BYE、错误应答、来源隔离和资源回收；
- 1.1/2.0/3.0 已建立级联广播源的 ACK/BYE 必须携带协商后的单值 `X-GB-Ver`；2xx ACK 必须继续通过原 INVITE 客户端事务发送，并在写出前完成 `Date + Note` 签名。出向 BYE 收到唯一合法的 `401 + WWW-Authenticate` 或 `407 + Proxy-Authenticate` 后，必须使用对应 `Authorization` 或 `Proxy-Authorization` 最多认证重试一次；重试刷新 Via branch、递增 CSeq，保持 Call-ID、Request-URI、From/To 对话标识、`X-GB-Ver` 和 `Monitor-User-Identity`，启用 `Date + Note` 时重新签名。重复挑战不得发送第三个 BYE，本地 RTP 接收资源始终只清理一次；

### 静态检查

```bash
go vet ./...
```

结果：通过。

检查过程中修正了 `GB28181API` 含锁结构体被值接收者复制的问题。

### 竞态检查

```bash
env GOCACHE=/private/tmp/owl-go-cache go test -race ./... -count=1
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

结果：最新 REGISTER 持久顺序、精确重放、响应确认、内部元数据 API 隔离、级联历史 INFO 活动提交边界、同对话 NOTIFY 串行提交、生命周期删除互斥和接纳时刻固定改动已通过 `go test ./... -count=1`、`go test ./... -shuffle=2026083009 -count=3`、`go vet ./...`、`go test -race ./... -count=1`、`go test ./docs -count=1`、相关 NOTIFY 场景 `-count=10/20` 和 `git diff --check`；管理端 `pnpm build` 是此前当前累计工作树的通过记录，本轮仅修改 Go/文档，未重跑前端构建。

事件 NOTIFY 有界 FIFO 加固通过新增队列/身份测试 `-count=10`、核心场景 `-count=20`、四版本多目标与队列 race 场景 `-count=5`、`go test ./pkg/gbs ./pkg/gbs/sip -shuffle=on -count=3`、`go test ./... -count=1`、`go vet ./...`、`go test -race -vet=off ./... -count=1` 和 `git diff --check`。后续续订代次隔离修复以四版本矩阵和直接状态快照测试连续执行 `-count=20`；过载/续订竞争与代次隔离场景连续执行 `-count=50`、race `-count=20`。随机顺序回归同时发现级联广播“错误 SSRC”测试会把随机 offer SSRC 误替换到 `o=` 设备编码子串，现改为只替换完整 `y=` 行，目标场景连续执行 `-count=1000` 通过。最终重新通过 `go test ./pkg/gbs ./pkg/gbs/sip -shuffle=on -count=3`、`go test ./... -count=1`、`go vet ./...` 和 `go test -race -vet=off ./... -count=1`。

普通历史回放/下载和兼容 Talk 的 RTP 接收 SSRC 修复前，四版本真实 SIP 测试均稳定捕获 `OpenRTPServerRequest.SSRC=0` 与 INVITE `y=` 不一致；修复后两类路径复用同一预生成 SSRC，ZLM HTTP 测试同时断言 JSON 中的非零 `ssrc`。目标场景普通测试连续执行 `-count=20`、GB 竞态测试连续执行 `-count=20`，随后通过 `go test ./pkg/gbs ./pkg/gbs/sip ./pkg/zlm -shuffle=on -count=3`、`go test ./... -count=1`、`go vet ./...` 和 `go test -race -vet=off ./... -count=1`。LALMAX 公开 API 无 SSRC 字段及来源 IP 过滤仍作为外部验收边界记录。

出向 SUBSCRIBE 失败恢复的四版本交错测试修复前均稳定复现 Contact 回滚；后续加锁交错还稳定复现 notify 快照已缩短但外层订阅仍保留长期限。修复后 `TestFailedOutgoingSubscriptionRefreshPreservesConcurrentNotifyProgressFourVersionMatrix` 普通及 race 各连续执行 `-count=50`，同时验证 CSeq、Contact、报告期限、notify 截止时间和外层清理期限一致；相关 Contact/期限回归连续执行 `-count=50`。随后重新通过 `go test ./pkg/gbs ./pkg/gbs/sip -shuffle=on -count=3`、`go test ./... -count=1`、`go vet ./...`、`go test -race -vet=off ./... -count=1`、`go test ./docs -count=1` 和 `git diff --check`。

NOTIFY 权威期限接纳时刻测试修复前在四版本均稳定显示约一个业务处理延迟的错误延长，修复后 `TestActiveNotifyExpiryUsesAcceptanceTimeFourVersionMatrix` 普通及 race 各连续执行 `-count=50`。相关两阶段提交、权威缩短、不得延长和跨过期提交回归保持通过；随后重新通过 `go test ./pkg/gbs ./pkg/gbs/sip -shuffle=on -count=3`、`go test ./... -count=1`、`go vet ./...`、`go test -race -vet=off ./... -count=1`、`go test ./docs -count=1` 和 `git diff --check`。

terminated NOTIFY 接纳时刻测试修复前在四版本均稳定显示 `RetryAt` 从提交时刻重新起算；修复后 `TestTerminatedNotifyRetryAfterUsesAcceptanceTimeFourVersionMatrix` 普通及 race 各连续执行 `-count=50`，同时验证已流逝窗口立即重订。既有完整一秒 `retry-after`、终止原因策略和并发终止代次回归保持通过；随后重新通过 `go test ./pkg/gbs ./pkg/gbs/sip -shuffle=on -count=3`、`go test ./... -count=1`、`go vet ./...`、`go test -race -vet=off ./... -count=1`、`go test ./docs -count=1` 和 `git diff --check`。

主动退订 Timer N 测试修复前在四版本均稳定显示等待截止时间早于最终 2xx 响应基准；修复后 `TestSuccessfulUnsubscribeWaitStartsAtFinalResponseFourVersionMatrix` 以真实 SIP/TCP 阻塞交错普通及 race 各连续执行 `-count=50`。既有成功退订保留对话、最终 terminated NOTIFY 删除、取消失败回滚及并发 NOTIFY 状态合并回归保持通过；随后重新通过 `go test ./pkg/gbs ./pkg/gbs/sip -shuffle=on -count=3`、`go test ./... -count=1`、`go vet ./...`、`go test -race -vet=off ./... -count=1`、`go test ./docs -count=1` 和 `git diff --check`。

迟到 active/pending NOTIFY 的 Timer N 隔离测试修复前在四版本均稳定把约 32 秒等待窗口缩为约 1 秒；修复后 `TestLateActiveNotifyDoesNotShortenSuccessfulUnsubscribeWaitFourVersionMatrix` 与取消失败状态合并、接纳时刻期限和 Timer N 起算点场景普通及 race 各连续执行 `-count=20`，并通过 `go test ./pkg/gbs ./pkg/gbs/sip -shuffle=on -count=3`。合法迟到请求的 CSeq、Contact 和报告期限仍正常提交。

取消等待期多次 NOTIFY 的期限单调性测试修复前在四版本均稳定显示第二个较长 `expires` 覆盖第一个较短报告期限；修复后 `TestFailedUnsubscribePreservesShortestLateNotifyExpiryFourVersionMatrix` 与 Timer N 隔离、失败恢复、普通期限不得延长场景连续执行普通 `-count=50`、race `-count=20`，同时确认最新 CSeq 和 Contact 仍保留。

级联事件 NOTIFY Digest 测试修复前四版本的 `401/407` 均只返回错误而不会认证重试；修复后 `TestCascadeEventNotifyRetriesDigestChallengeFourVersionMatrix`、`TestCascadeEventNotifyDigestChallengeRetriesOnlyOnce`、订阅生命周期及 TCP 对话路由场景连续执行普通 `-count=50`、race `-count=20`，同时确认 CSeq、Call-ID、对话/业务头、正文、单次重试上限及认证失败保留订阅语义。继续审计发现原宽松解析会接受缺失 Digest 方案及重复关键参数；`TestCascadeEventNotifyRejectsAmbiguousDigestChallengeFourVersionMatrix` 修复前在四版本五类歧义挑战上均稳定错误重试，切换到兼容隔离的严格解析入口后与 SIP 解析单测普通 `-count=50`、race `-count=20` 通过。

级联广播源 INVITE 的代理 Digest 测试修复前在 1.1/2.0/3.0 均稳定直接返回 `407`；修复后 `TestCascadeBroadcastUpstreamInviteRetriesProxyDigestThreeVersionMatrix`、`TestCascadeBroadcastUpstreamInviteDigestRetriesOnlyOnceThreeVersionMatrix` 与原 `401`、事件 NOTIFY 认证回归普通连续执行 `-count=50`、race `-count=20`，同时验证 `Proxy-Authorization`、Call-ID/CSeq、Subject/SDP 保持、单次重试上限及失败资源释放。

附录 G 出向 SIP MESSAGE 的代理 Digest 测试修复前在 1.0/1.1/2.0 均稳定直接拒绝 `407`，宽松解析还会接受缺失 `Digest` 方案、重复关键参数和畸形参数；修复后 `TestBuildAnnexGDigestRetry`、`TestBuildAnnexGDigestRetryRejectsInvalidChallenges`、`TestSendAnnexGSIPRequestRetriesRemoteDigestChallenge` 和 `TestSendAnnexGSIPRequestDigestRetriesOnlyOnce` 普通连续执行 `-count=50`、race `-count=20`，验证正确的 `Authorization/Proxy-Authorization`、Call-ID/CSeq/Via、目标 URI、XML、`X-GB-Ver`、严格挑战拒绝与单次重试上限。

级联 REGISTER 的代理 Digest 测试修复前在 1.0/1.1/2.0/3.0 均直接拒绝 `407`，`407 → 401` 组合挑战无法携带两套凭据，重复 `realm` 还会发出错误认证请求；修复后 `TestCascadeRegisterRetriesProxyDigestFourVersionMatrix`、`TestCascadeRegisterCombinesProxyAndRegistrarDigestFourVersionMatrix`、`TestCascadeRegisterRejectsAmbiguousDigestChallengeFourVersionMatrix` 和 `TestCascadeRegisterDigestChallengesRetryOnlyOnceFourVersionMatrix` 验证两类认证头、Call-ID/CSeq、严格解析及各一次重试上限。

级联普通 `MESSAGE`/Keepalive 及会话内 `MediaStatus` 的 Digest 测试修复前在 1.0/1.1/2.0/3.0 收到 `401/407` 均只发送首个请求并立即失败，Keepalive 因此会进入不必要的重注册流程，MediaStatus 则把认证挑战错误回传下级；修复后 `TestCascadeKeepaliveRetriesDigestChallengeFourVersionMatrix`、`TestCascadeMessageRetriesDigestChallengeFourVersionMatrix`、`TestCascadeMediaStatusRetriesDigestChallengeFourVersionMatrix`、`TestCascadeMessageDigestRetriesOnlyOnceFourVersionMatrix`、`TestCascadeKeepaliveDigestFailureDoesNotRefreshStateFourVersionMatrix`、`TestCascadeMessageDigestRetryReplacesSignalDigestHeaders` 和严格挑战拒绝矩阵覆盖正确认证头、新 Via branch、CSeq 递增、Call-ID/目标/XML/版本/用户身份保持、`Date + Note` 重签、单次重试上限及失败不伪造心跳成功；会话内认证重试还会在发送前原子预留新的对话 CSeq，后续 BYE 或其他请求不会复用已发送序号。新增矩阵普通连续执行 `-count=50`、race `-count=20`，并重新通过包级随机顺序、全仓 test/vet/race、docs 测试和 `git diff --check`。

入向级联视频对话的异步清理 BYE 测试修复前在 1.0/1.1/2.0/3.0 的 `401/407` 矩阵中均只写出首个请求；修复后 `TestInboundCascadeBYERetriesDigestChallengeFourVersionMatrix` 以真实 SIP/TCP 事务验证初始写出不等待挑战、正确且唯一的认证头、新 Via branch、CSeq 递增、对话/版本/用户身份保持、`Date + Note` 有效以及重复挑战最多两个请求。响应等待改由 worker 跟踪，不绑定已经停止的业务 context；`TestCascadeWorkerStopCancelsAndWaitsResponseTasks` 验证完整停止会取消并等待后台响应任务，既有 `TestTerminateCascadeMediaDetachesBeforeUpstreamBYEWriteCompletes` 继续证明本地对话、共享源和 RTP 清理先于阻塞网络写完成。三项定向回归普通连续执行 `-count=50`、race `-count=20`，随后重新通过包级随机顺序、全仓 test/vet/race、docs 测试和 `git diff --check`。

2022 附录 I 的 `X-GB-Ver` 只要求 REGISTER 及其成功/失败响应携带；后续交互按已获知的对端能力档案选择可识别消息，并不要求普通设备 ACK/INFO/BYE 重复携带版本头。发布抓包不应把普通对话缺少该头误判为不合规；级联侧在对话请求中保留该头属于既有上级兼容策略。

普通设备无需等待业务结果的 BYE 现在由生命周期任务异步消费 SIP 最终响应并及时关闭事务，避免每次直播/对讲/媒体丢失或停服收尾后固定滞留约 20 秒；初始写错误仍同步返回，调用方不等待最终响应，普通设备 401/407 仍不做无标准依据的认证重试，需要结果的历史控制继续同步等待。`TestConsumeDialogResponseAsyncClosesTransactionAfterFinalResponse` 通过真实 SIP/UDP 事务验证 200 后即时回收，并与停服、阻塞写测试分别执行普通、race 各 `-count=20`；相关媒体生命周期回归另执行普通 `-count=3`。

同步等待最终响应的非 INVITE 请求现同样在返回响应后关闭客户端事务，不再让 `MESSAGE/NOTIFY/SUBSCRIBE/OPTIONS/INFO/BYE` 固定滞留到 20 秒空闲超时；INVITE 继续走独立包装器保留原事务，用于 2xx ACK 和重传处理。`TestSIPResponseContextClosesNonInviteTransactionAfterFinalResponse` 通过真实 SIP/TCP 管道验证边界，并与取消 INVITE、487 自动 ACK 和 ACK 回归分别执行普通、race `-count=20`。

独立 `OPTIONS` 探测原先未经过通用非 INVITE 响应包装器，成功响应后仍会保留客户端事务约 20 秒；现由 `sipResponseWithTimeout` 在成功、超时和取消返回时统一关闭。`TestSIPResponseWithTimeoutClosesTransactionAfterFinalResponse` 使用真实 SIP/TCP 管道验证 `200 OK` 后即时回收，并通过普通与 race `-count=20`；附录 G、级联同步/异步交换和无需业务等待的对话收尾等其余直接响应等待路径已复核存在明确关闭或生命周期策略。随后重新通过包级随机顺序、全仓 test/vet/race、docs 测试和 `git diff --check`。

SIP 客户端事务 watcher 的兜底空闲窗口由 20 秒修正为 RFC 3261 Timer B/F 的 `64*T1` 默认值 32 秒，避免未被业务层更早关闭的事务提前丢失迟到最终响应或 INVITE 最终响应重传。已有成功、取消、业务超时及 detached 路径仍按原策略即时回收。客户端和服务端事务另增加从创建起不可刷新、默认 64 秒的绝对上限；持续 1xx、最终响应/请求重传及缓存响应重放仍可刷新 32 秒空闲窗口，但不能让事务无限续期。`TestClientTransactionIdleTimeoutCoversRFC3261TimerBF`、`TestTransactionActivityCannotExtendLifetimePastHardLimit` 与事务角色、分叉清理和缓存重放测试已执行普通 `-count=100`、race `-count=20`。

SIP 客户端事务键由原来的 `Call-ID/CSeq/方法` 扩展为同时绑定顶层 Via 的传输、Sent-by 和 branch，修复相同对话序号和方法、不同 branch 的并发请求错误碰撞；响应使用同一算法准确关联。服务端事务基础键保持不变并继续独立追加 Via 字段；缺失 CSeq 的异常兼容路径仍只使用 Call-ID。`TestClientTransactionKeySeparatesViaBranches` 与同对话隔离、无 CSeq 回退、服务端重传回归已执行普通 `-count=50`、race `-count=20`。

验签通过的 1xx 只刷新客户端事务活动，不进入只供最终响应使用的业务队列；等待者尚未启动时连续超过队列容量的临时响应不得阻塞 `handlerResponse` 或同一 UDP/TCP 处理循环，随后最终响应仍须正常交付。INVITE 的 3xx～6xx 最终响应仅首个交付业务层，后续重传仍逐次发送复用原事务 branch/CSeq 的 ACK，但不得重复入队。调用方取消或超时后，detached INVITE 已无业务消费者，后续 1xx 必须丢弃，非 2xx 只 ACK 不入队，迟到 2xx 继续 ACK 后发送独立 BYE。`TestInviteDiscardsProvisionalFloodBeforeResponseWaiter`、`TestHandlerResponseAutomaticallyAcknowledgesNon2xxInvite`、`TestDetachedInviteDiscardsProvisionalResponses` 与 2xx ACK/迟到清理回归已执行普通 `-count=50`、race `-count=20`。

同一 INVITE 的多个不同 To-tag 2xx 必须按独立对话处理：首个响应交给业务层选择，ACK 前已到达的额外响应按对话暂存，首选 ACK 成功后以及此后新到达的分叉响应均自动发送各自新 branch ACK 和独立 BYE；每个 `Call-ID + INVITE CSeq + To-tag` 保存自己的 ACK，重传原样重发对应 ACK，不重复 BYE、不覆盖其他对话，也不得进入无人消费的业务队列。单事务最多跟踪 32 个独立 To-tag，超限后不得继续增长响应副本、ACK/BYE 状态或后台清理任务，并只告警一次；分叉清理任务必须受原事务关闭等待约束。`TestHandlerResponseRetransmitsCached2xxInviteACK`、`TestHandlerResponseCleansUpForked2xxArrivingBeforeSelectedACK`、`TestInviteForked2xxTrackingIsBoundedBeforeSelectedACK` 已执行普通 `-count=100`、race `-count=20`。

级联同步交换在取得 REGISTER/MESSAGE/NOTIFY/SUBSCRIBE 最终响应后现同样关闭非 INVITE 客户端事务；2xx INVITE 保留供显式 ACK，非 2xx INVITE 保留完成窗口供重复最终响应自动 ACK。`TestCascadeExchangeCompletionClosesOnlyNonInviteTransaction` 与级联注册、心跳、普通消息、事件通知和 ACK 回归执行普通 `-count=20`，关键事务边界另执行 race `-count=20`。

普通设备 INVITE 取消现在为 CANCEL 写入设置统一 5 秒上限；业务层不等待 CANCEL 结果时，独立事务收到最终响应即回收，原 INVITE 事务继续保留非 2xx ACK 完成窗口。CANCEL 与最终响应竞态若产生迟到 INVITE `2xx`，平台会自动发送 ACK 和下一 CSeq 的 BYE；同一 `2xx` 重传只重发原 ACK，不重复终止对话。级联超时取消复用相同 detached 生命周期，但保留原 1 秒调用方期限和“CANCEL 写出后再失效超时 TCP”的顺序；UDP 或连接关闭前仍能到达的最终响应不再形成无人消费事务。默认/调用方写入期限、CANCEL 回收、`487 + ACK`、`2xx + ACK + BYE` 和级联取消顺序测试均已执行普通与 race `-count=20`。

本轮最终重新通过 `go test ./pkg/gbs ./pkg/gbs/sip -shuffle=on -count=3`、`go test ./... -count=1`、`go vet ./...`、`go test -race -vet=off ./... -count=1`、`go test ./docs -count=1` 和 `git diff --check`。

四版本核心报文矩阵补强：`TestGBProtocolVersionFixtureMatrix` 现将每版 Keepalive、Catalog、RecordInfo、Alarm、INVITE SDP 和 MediaStatus/121 夹具分别送入生产处理/解析路径，覆盖心跳提交且不覆盖协商版本、目录结构校验与多响应聚合、1.0/1.1 UDP 和 2.0/3.0 TCP offer 的 `setup/connection`、SSRC/Payload/端口，以及既有录像、报警、A.4 和媒体终态断言。`TestSuccessfulRegisterStateAcrossVersions` 让四版成功 REGISTER 直接经过生产 handler，验证响应、绑定持久化、版本状态和指标；固定 Digest 夹具仍只验证 SIP parser，认证生产链继续使用服务端签发 nonce 的独立防重放测试。新增矩阵已通过定向普通测试、race `-count=10` 与 `go test ./pkg/gbs/... -count=1`；真实设备和真实上级抓包阶段门不变。

管理端国标扩展闭环增加后，通道详情已覆盖四版本基础查询/控制、后端公开的全部 A.2.3 统一控制动作、按版本能力开放的 A.2.4 扩展、2022 抓拍和升级会话及最终状态；云台、录像启停、报警复位、拉框缩放、看守位、存储卡和目标跟踪的结构化参数均可提交。设备运维页已覆盖完整 DeviceConfig：2014 的视频/音频/SVAC、2016 的 SVAC 和 2022 的视频属性、录像计划、报警录像、画面遮挡、翻转、报警上报、OSD、抓拍等配置均有 JSON/XML 编辑入口、版本说明和门禁。能力字段存在时严格使用声明，字段缺失时才按四版本协议档案回退；显式空能力数组仍严格处理，不依赖扩展能力的设备状态、远程启动和布撤防保持可用。2022 `ConfigDownload/SnapShot` 另受 `snapshot` 能力门禁，不会在 2014/2016 错误开放。升级 `Firmware/FileURL/Manufacturer` 在 Core 和管理端均只裁剪判空、不修改提交值。设备删除还会在协议锁内捕获 Catalog 晚到通道，并清理其抓拍任务和封面。Swagger 已补齐升级/抓拍状态路由、修正 PTZ/升级响应模型，并列出此前遗漏的 `home_position_query`。

语音能力矩阵已按 2011 第 7.2 纠正：1.0/1.1/2.0/3.0 均可使用 UDP `Play + m=audio + sendrecv` 兼容 Talk，2011/2014 仍拒绝 RTP over TCP；只有 2016/2022 可选择“实时点播上行 + 广播下行”的 `talk_standard` 标准双流程。设备详情、通道语音选项、诊断能力矩阵和四版本页面均使用相同门禁，真实双向可听性仍列为下述设备验收项。

本次收口验证通过：`admin-ui` 的 `pnpm test` 共 40 项、`pnpm build`；GB 定向包测试；2014 附录 O 级联中继字节转发、上级来源授权、MediaStatus、超时、文件上限和版本门禁测试；`go test ./... -count=1`；`go test ./... -shuffle=2026083101 -count=3`；`go vet ./...`；中继聚焦 race 10 轮及 `go test -race -vet=off ./... -count=1`；`go test ./docs -count=1`；`git diff --check`。自动化结果不替代下述真实设备、真实上级、证书体系、媒体抓包和灰度阶段门。

## 4. 未完成和阻断项

### RTP/RTCP 真实媒体验证

四版标准均要求或宜采用 RTCP，且媒体传输能力表包含 RTP/RTCP。项目融合镜像和调试编排固定 ZLMediaKit 源码提交 `2fa539807370c4bcfca9801bde1dd2fdd95ba113`，并在构建时应用仓库内 RFC 4571 RTP/RTCP 补丁，不再以浮动 `master` 或未打补丁官方镜像作为默认依赖。UDP 路径保持原有 RTP+1 RTCP、SR/RR 和丢包统计；补丁为 TCP 接收、发送路径增加默认关闭的 `tcp_rtcp` 参数以及同连接 RTCP 解复用、SR/RR 处理和周期发送，发送侧网络回调现在把畸形 RFC 4571 帧的解析异常收敛为连接级错误，Owl 仅为 2016/2022 TCP 会话开启。`arm64` 目标镜像 `gowvp/zlmediakit:2fa539-tcp-rtcp` 已从当前工作树重建，镜像 ID/仓库摘要均为 `sha256:32fa733d16cfca95659b64f67b277f8e81e24aaf754bd7e23c87931e3f0bd992`，镜像内 `MediaServer` SHA-256 为 `ef64203fb4b74f7cf9c6356564ee695a194dc2584c3e88bfab1e3867b53241ec`。一次性容器的短时运行日志确认 `git hash:2fa5398`，HTTP/API、RTSP、RTMP、RTP TCP/UDP 监听均成功，并能在受控 SIGTERM 后完整退出；向 `openRtpServer(tcp_rtcp=true)` 发送畸形 RFC 4571/RTCP 帧后进程保持存活。接收侧合成烟测通过 `openRtpServer(tcp_rtcp=true)` 写入同连接 RTP/SR 并收到 32 字节 RR；发送侧通过 `startSendRtp(tcp_rtcp=true)` 收到 28 字节 SR 并成功回送 RR。关闭参数后，接收侧不返回 RTCP，发送侧连续 55 帧 RTP 也没有 SR，默认兼容路径保持不变。目标镜像门已关闭；真实设备/上级的 UDP、TCP SR/RR 抓包仍未完成，发布前还必须确认 RFC 4571 帧、会话 SSRC、周期、丢包率及拥塞行为。原厂商兼容 Talk 的双向同连接 TCP RTCP 仍不在承诺范围。该项属于外部发布阶段门，现有 SIP/SDP、镜像启动与合成媒体烟测通过不等于媒体面生产完整。

独立 ZLM 调试部署现在使用共享的必填 `OWL_MEDIA_SECRET`，入口脚本在空持久卷中从镜像模板生成 `config.ini` 并更新 `[api].secret`；Owl 通过运行时环境指向 `zlm:80`，ZLM Webhook 回指 `gowvp`。真实容器烟测确认正确密钥 API 成功、错误密钥被拒绝，默认 `api.apiDebug=0`，两次请求日志均不包含 secret，且运行期间不再随机改写 secret。该证据关闭配置卷遮蔽、密钥失配和默认 API URL 泄密问题，但不替代目标环境网络、端口映射和外部设备可达性验证。

### 2014 直接 TCP 下载及级联中继真实验证

ADR：`docs/adr/gb28181-2014-direct-tcp-download.md`。AI-403 已按 Owl 内置 TCP 客户端方案实现并通过本地发送端模拟器验证；附录 O 消息 1/2/5 的独立客户端控制腿由进程内调用替代，设备发送方和级联媒体入口严格承接必须携带 SDP 的消息 3。上级平台级联中继也已完成独立监听、来源授权、原始 PS 转发、文件大小/并发/超时限制及终态清理的本地自动化。尚无真实 1.1 设备证明不同厂商对占位 SDP 端口、`filesize/fileszie` 和 MediaStatus 时序的兼容性，也没有真实 1.1 上级平台验证中继宣告地址、NAT/防火墙、大文件背压及完整双侧 SIP/媒体抓包。

自动化还必须覆盖上级 ACK 晚于中继总期限的顺序：监听结束后应保存一次 `cancelled/total_timeout` 终态、释放一次并发额度，迟到 ACK 只能补交一次媒体会话结束回调，重复 ACK 不得重新拨号或重复回调。`TestDirectTCPRelayReportsContextTimeoutBeforeStart` 已覆盖该本地状态机；真实上级应在高延迟和丢包条件下确认 BYE/会话释放时序。

2022 `VideoUploadNotify` 多上级投递还必须注入回执数据库写失败：某一上级已经 SIP 成功后，同一进程的后续轮询只能补写回执，不能再次发送业务 MESSAGE；其他尚未成功的平台仍应独立重试。`TestVideoUploadNotifyReceiptPersistenceRetryDoesNotResendUpstream` 已覆盖单目标瞬时写失败。进程恰好在网络成功与回执持久化之间崩溃仍属于至少一次投递窗口，真实上级必须验证相同通知的幂等接收策略。

级联任务完成墓碑热更新交错已补测：最终升级/抓拍通知 SIP 成功后，若完成墓碑落库失败，`CascadeManager.Apply` 替换旧上级 worker 不会删除已完成运行态路由；路由标记为与旧 worker 逻辑脱离并保留幂等索引，运行态维护器每秒自动补写最多 8 个墓碑，不依赖设备重传，也不重复向新 worker 发送业务通知。补写与设备删除使用相同设备操作锁，删除路由的 retired 标记可阻止已排队补写在删除事务后重建孤儿记录。`TestCascadeCompletedRouteSurvivesWorkerReplacementWhenCompletionPersistenceFails`、`TestCascadeTaskCompletionPersistenceRetriesWithoutDeviceRetransmission`、`TestCascadeTaskCompletionPersistenceRetryIsBounded` 和 `TestCascadeTaskCompletionRetryCannotRecreateRouteAfterDeviceDeletion` 已通过普通及 race 定向测试；真实上级配置热更新、进程崩溃和数据库故障仍需现场验证。

级联升级/抓拍路由的七天 TTL 现按最后成功更新时间计算，数据库索引不再误用创建时间；同一路由 start/pending 与最终 completed 保存由同一锁串行到数据库提交，迟到 pending 写不能覆盖完成墓碑。旧记录缺少更新时间时仍按创建时间兼容恢复，损坏的倒序时间拒绝。阻塞存储交错、更新时间索引和 TTL 边界测试已通过普通及 race 重复验证。

附录 G 恢复压测需同时准备超过 8 条已保存 Response，验证每轮只按“系统、命令、SN”处理一个有界批次，剩余记录可在后续周期继续收敛，关闭不会等待整库同步消费。`TestAnnexGStoredResponseReplayUsesBoundedDeterministicBatches` 已覆盖 10 条本地记录的两批处理；真实数据库延迟和 4096 条容量上限仍须目标环境压测。

附录 G SIP-TLS 现场验收必须使用目标 CA 签发的正常与已撤销服务端证书分别握手，并覆盖叶证书与中间 CA 撤销、有效、过期、错误签名及无适用签发者等 CRL 场景；配置 `TLSCRL` 而缺少 `TLSCA` 必须在启动配置校验阶段拒绝。自动化 `TestAnnexGTLSCRLRejectsRevokedServerCertificate` 和 `TestAnnexGTLSCRLRejectsRevokedIntermediateCertificate` 已证明本地叶证书/中间 CA 撤销门禁，但不能替代真实 CRL 发布、轮换和系统时钟漂移验证。

### 语音真实设备验证

自动化已证明 SIP/SDP 状态机和 ZLMediaKit RTP API 调用，但尚无真实设备证明扬声器实际出声、设备主动 INVITE 的厂商差异、NAT 下 RTP 地址以及长时间广播/对讲稳定性。语音发送当前要求 ZLMediaKit 中存在已就绪的 G.711 A-law 音频源；对 LALMAX 官方公开源码提交 `0845640`（2026-05-28）的核验仍未发现 RTP 发送接口，因此 LALMAX 明确返回不支持。

### 四版本真实上级平台级联验证

自动化已经覆盖注册、目录、核心及版本化扩展查询、订阅、直播、回放、下载、语音广播/对讲、INFO 控制和资源释放；2022 附录 H 仍缺至少三层真实平台对 `X-PreferredPath`/`X-RoutePath` 的路径选择和媒体结果证明；尚无 1.0/1.1/2.0/3.0 真实上级平台分别执行完整链路的脱敏 SIP 报文和媒体结果，不能据此宣称生产互通完成。

语音广播/对讲 B2BUA 与 2022 A.4 嵌入对象的安全级联均已完成自动化闭环，但尚无真实上级语音源与真实下级扬声器证明跨域音频可听、长时间稳定及厂商 SDP 差异兼容，也尚无真实 3.0 平台验证各厂商 A.4 ExtraInfo 结构。自动化能力不能替代真实平台和设备验收。

### 安全认证与跨域用户身份真实验证

2011 在 9.1.2.2/J.2 定义、2014 修改补充文件继续沿用且 2016 保留的 `Capability/Asymmetric` 证书 REGISTER，已完成服务端和级联客户端 1.0/1.1/2.0 自动化矩阵；CA/CRL、`Date + Note` 和 `Monitor-User-Identity` 也已形成自动化闭环，但尚无真实证书体系、真实用户目录及至少两级 `211` 安全路由网关的互通证据。Required 防降级、吊销证书、nonce 重放、用户属性越权、超跳和路由环必须在真实链路分别验证；UDP/TCP 明文还必须提供 IPsec 或等价可信边界证据。2022 正文不再定义 `Capability/Asymmetric`，其高安全级别按 GB 35114 单独验收，不能由上述能力组合替代。

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

### A.2.4 报警查询管理端入口

通道详情的“国标扩展”页已增加 `Alarm` 主动查询入口，四个版本均可填写报警级别、方式和时间范围；`AlarmType` 仅在 GB/T 28181-2016/2022（2.0/3.0）启用，前端会拦截反向级别和时间范围后再调用 `/devices/{id}/gb/query`。该页面能力与协议层版本门禁一致，真实设备/平台互通仍需按本清单验收。
