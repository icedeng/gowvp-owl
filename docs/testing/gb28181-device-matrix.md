# GB/T 28181 四版本真实设备验收矩阵

执行步骤、诊断接口和回滚方式见 [四版本联调执行手册](gb28181-interoperability-runbook.md)。

## 1. 设备登记

| 档案 | 厂商 | 型号 | 固件 | 传输 | NAT | 设备 ID | 测试人 | 日期 | 结果 |
|---|---|---|---|---|---|---|---|---|---|
| 1.0 | 待填写 |  |  | UDP/TCP |  |  |  |  | 待测 |
| 1.0 | 待填写 |  |  | UDP/TCP |  |  |  |  | 待测 |
| 1.1 | 待填写 |  |  | UDP/TCP |  |  |  |  | 待测 |
| 1.1 | 待填写 |  |  | UDP/TCP |  |  |  |  | 待测 |
| 2.0 | 待填写 |  |  | UDP/TCP |  |  |  |  | 待测 |
| 3.0 | 待填写 |  |  | UDP/TCP/TLS |  |  |  |  | 待测 |

### 1.1 上级平台登记

| 档案 | 厂商/平台 | 版本 | 传输 | 上级平台 ID | Owl 本级 ID | 下级设备 | 测试人 | 日期 | 结果 |
|---|---|---|---|---|---|---|---|---|---|
| 1.0 | 待填写 |  | UDP/TCP |  |  |  |  |  | 待测 |
| 1.1 | 待填写 |  | UDP/TCP |  |  |  |  |  | 待测 |
| 2.0 | 待填写 |  | UDP/TCP/TLS |  |  |  |  |  | 待测 |
| 3.0 | 待填写 |  | UDP/TCP/TLS |  |  |  |  |  | 待测 |

3.0 三级路径另登记“上级平台 → Owl → 下级平台 → 设备”四个节点的平台/设备编码、地址和版本；平台编码必须是 20 位数字且类型码为 `200`。

## 2. 通用验收

每台设备都需验证并保留脱敏抓包：

- REGISTER 首次、401、鉴权、200、注销和重新注册；
- Keepalive、离线判定和恢复；
- Catalog、目录更新和通道上下线；
- UDP 实时点播、ACK、BYE、异常断流；
- PTZ、RecordInfo、历史回放/下载、Alarm；
- 诊断接口显示的声明版本、有效版本、来源和能力集与预期一致。
- 对固件缺失能力设置 `gb_disabled_capabilities` 后，对应命令应在发送 SIP 前被拒绝；清空列表后恢复版本默认能力。
- `GET /gb28181/metrics` 中注册、目录、媒体、断流和下载计数与实际操作一致。
- 真实上级按版本发起 Catalog/Alarm/MobilePosition/PTZPosition 订阅时，抓取 Owl 向真实下级发送的 SUBSCRIBE、续订和 Expires=0 退订；Catalog 应订阅承载共享通道的 NVR，新增共享通道或将通道迁移到另一 NVR 后应自动补订阅；两个上级订阅同一资源时，确认首个上级退订不会提前取消共享下级订阅；
- 分别订阅平台根目录和单个共享通道：2011 Catalog 变化通知根节点应为 `Response`，2014/2016/2022 应为 `Notify`；新增、删除、上下线、元数据修改应分别收到 ADD、DEL、ON/OFF、UPDATE，且每包 `SumNum` 等于 `DeviceList Num`；下级发送空消息体 `Subscription-State: terminated` 后 Owl 应返回 200，并在上级订阅仍有效时重新建立下级订阅；
- 2022 上级返回 REGISTER 301/302 和新的 Contact 后，确认 Owl 向新地址完成 Digest 注册，后续 Keepalive/NOTIFY/INVITE 使用新地址，旧地址不再具有上级身份；
- Alarm 分别使用级别、方式、类型和时间过滤，确认不匹配报警不会发往上级；2011 厂商分别验证 `StartAlarmTime/EndAlarmTime` 和示例别名 `StartTime/EndTime`。
- 四版本真实上级分别完成 REGISTER（401/Digest/200）、Keepalive、Catalog、DeviceInfo/DeviceStatus、直播、回放、下载、INFO 控制、BYE 和资源释放；1.1+ 再执行订阅，2.0/3.0 再执行 RTP over TCP 与语音流程。
- 3.0 三级路径分别执行直播、回放和下载：上级发送 `X-PreferredPath`，每一跳只消费本级编码并向下转发剩余路径，响应 `X-RoutePath` 顺序与实际路径一致；错误首跳、重复节点、下级确认不匹配和路由环必须被拒绝。
- `Date + Note` 先以关闭状态建立兼容基线，再分别验证 Enabled 和 Required；至少覆盖双方约定 seed、Base64 与 hex、一个传统摘要算法和 SM3，并保存缺失签名、正文篡改和超时 Date 的拒绝证据。REGISTER 不应被该摘要要求误拦截。
- 对支持 2016 `Capability/Asymmetric` 的设备及上级分别验证证书 REGISTER：有效证书成功，未知设备证书、错误设备 ID 绑定、过期证书、吊销证书、错误平台证明、重放 nonce 和算法不一致必须失败；Required 模式不得降级到口令 Digest。保存脱敏挑战/应答、证书序列号、CA/CRL 版本和校验结论，不保存私钥或随机秘密。
- 使用真实用户目录和至少两级类型码 `211` 的安全路由网关验证 `Monitor-User-Identity`：本域身份生成、逐级前置、直接网关绑定、用户类型码 `300-499`、机构/类别/职级授权、缺失/重复头、未知网关、重复网关、超跳和路由环拒绝。UDP/TCP 场景必须同时保存 IPsec 或等价可信网络边界证据；否则只验证 TLS 绑定场景，不得把源 IP 记作密码学身份。
- 2022 高安全级别应用按 GB 35114 建立独立验收项；SIP-TLS、证书 REGISTER、`Date + Note` 和 `Monitor-User-Identity` 的通过结果不能替代该专项。

## 3. 版本专项

### 1.0

- 不自动发送 ConfigDownload；
- 拒绝广播、对讲、RTP over TCP、IFrame、DragZoom 等未定义能力；
- 直播、回放、下载 SDP 仅使用 `RTP/AVP`。

### 1.1

- Catalog 扩展字段和目录树节点；
- ConfigDownload/BasicParam 写入；
- Catalog 订阅初始、续订、取消；
- Broadcast 标准时序；
- 使用已就绪的 ZLMediaKit G.711 A-law 音频源，确认设备主动 INVITE、平台返回 `PT 96/PS/90000`、扬声器实际出声、BYE 后 RTP 停止；
- MediaStatus/121；
- 直接 TCP 下载：`m=video ... tcp`、发送端地址/端口、`filesize`、文件 SHA-256、取消、断网恢复；
- 确认未误用 `TCP/RTP/AVP`。

启用直接 TCP 示例配置：

```toml
[Sip.DirectTCPDownload]
Enabled = true
DeviceAllowlist = ["34020000001320000001"]
StorageDir = "./configs/downloads/gb28181"
RetainDays = 7
OfferPort = 9
MaxFileSize = 10737418240
GlobalConcurrency = 4
DeviceConcurrency = 1
DialTimeout = "5s"
FirstByteTimeout = "15s"
IdleTimeout = "30s"
TotalTimeout = "2h"
AllowAddressMismatch = false
AllowedAddressCIDRs = []
```

### 2.0

- RTP over TCP 主动/被动模式；
- 语音对讲；
- 对讲需分别确认设备上行音频可听、平台下行 `PT 8/PCMA/8000` 可听，并记录 RTP 收发字节数；
- PresetQuery、MobilePosition、HomePosition；
- 确认直接 TCP 下载默认不启用。

### 3.0

- H.265、设备升级、抓拍；
- 精确 PTZ、PTZPosition；
- PTZ 精准位置变化 SUBSCRIBE/NOTIFY，确认 2.0 及更早档案不会发送该订阅；
- CruiseTrackListQuery/CruiseTrackQuery，核对 0/1 轨迹编号、预置位、停留时间和速度；
- SDCardStatus/FormatSDCard；
- ConfigDownload 抓拍配置使用标准 `SnapShotConfig` 节点，并记录厂商是否仅接受旧 `SnapShot` 别名；
- `UploadSnapShotFinished` 的文件标识应为“20 位设备编码 + `02` + 17 位真实生成时间 + 2 位序号”，记录设备是否会产生非法日期时间；
- 附录 A.4 扩展对象；
- SIP-TLS（目标设备支持时）。
- 广播/对讲沿用 `PT 8/PCMA/8000`，确认双向音频和停止清理。

## 4. 证据目录约定

```text
docs/testing/evidence/gb28181/<version>/<vendor>-<model>-<firmware>/
├── device.md
├── register.sip.txt
├── catalog.sip.txt
├── live.sip.txt
├── history.sip.txt
├── alarm.sip.txt
├── voice.sip.txt
├── voice-rtp.md
├── direct-download.sip.txt
├── expected.sha256
└── result.md

docs/testing/evidence/gb28181/platforms/<version>/<vendor>-<platform>/
├── platform.md
├── register.sip.txt
├── catalog.sip.txt
├── live.sip.txt
├── history.sip.txt
├── subscribe.sip.txt
├── voice.sip.txt
├── signal-digest.md
├── certificate-register.sip.txt
├── monitor-user-identity.sip.txt
├── security.md
├── route-path.sip.txt
└── result.md
```

禁止提交口令、Authorization 响应值、真实公网地址、人员信息或未脱敏录像。

## 5. 通过规则

- 所有必测项有实际报文，不以界面显示代替协议证据；
- 设备缺陷必须登记为厂商兼容项，不改变所有设备默认行为；
- 文件下载必须核对字节数和 SHA-256；
- 任一 P0/P1 兼容性回归、崩溃、数据竞争、资源泄漏或无法回滚均判定不通过。
