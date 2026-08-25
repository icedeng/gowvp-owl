# GB/T 28181 四版本联调执行手册

本手册用于执行 [真实设备矩阵](gb28181-device-matrix.md) 的 AI-503/AI-602 阶段。自动化测试通过不等于真实设备验收通过；每台设备必须保存脱敏报文和结果。

## 1. 联调前准备

1. 为设备登记厂商、型号、固件、网络模式、设备 ID 和测试时间。
2. 确认设备 SIP Server 地址、端口、域、平台 ID、注册密码和媒体地址。
3. 确认抓包只覆盖测试网卡或测试 VLAN，禁止提交密码、Authorization、公网地址和原始录像。
4. 发布前先保留当前配置和版本号，便于回滚。

## 2. 版本覆盖与诊断

设备手动版本覆盖接口为 `PUT /devices/{id}`，请求体示例：

```json
{"gb_version":"1.1"}
```

清除手动覆盖并恢复自动协商：

```json
{"gb_version":""}
```

检查设备诊断：

```text
GET /devices/{id}/gb/diagnostics
```

至少记录 `declared_version`、`effective_version`、`version_source`、`capabilities` 和最近一次拒绝命令。

## 3. 通用验收顺序

对每台设备按以下顺序执行并保存对应报文：

1. REGISTER 首次、401、鉴权 REGISTER、200、注销和重新注册；
2. Keepalive 正常、离线判定、恢复；
3. Catalog 查询、分包响应、目录变化；
4. UDP 实时点播、ACK、BYE、异常断流；
5. PTZ、RecordInfo、历史回放/下载、Alarm；
6. 查询 `GET /gb28181/metrics`，核对计数与操作次数。

## 4. 版本专项

### 1.0（2011）

- 预期 `effective_version=1.0`；
- 不应自动收到 ConfigDownload；
- 广播、对讲、RTP over TCP、IFrame、DragZoom 等扩展应返回明确不支持；
- 直播、回放、下载 SDP 使用 `RTP/AVP`，不得出现 `TCP/RTP/AVP`。

### 1.1（2014 修改补充文件）

- 验证 Catalog 扩展字段和目录树节点；
- 验证 BasicParam 查询、写入和原始 XML 保存；
- 验证 Catalog 订阅初始、续订、取消；
- 验证 Broadcast 通知、业务应答和后续会话；
- 验证 MediaStatus/121 结束通知的幂等收敛；
- 验证直接 TCP 下载：`m=video ... tcp`、`filesize/fileszie`、大小、SHA-256、取消和超时；
- 确认报文中没有混用 `TCP/RTP/AVP`。

直接 TCP 下载必须先在配置中开启，并加入设备白名单：

```toml
[Sip.DirectTCPDownload]
Enabled = true
DeviceAllowlist = ["34020000001320000001"]
```

### 2.0（2016）

- 回归 RTP over TCP 主动/被动、语音对讲、PresetQuery、MobilePosition 和 HomePosition；
- 确认直接 TCP 下载默认关闭。

### 3.0（2022）

- 回归 H.265、升级、抓拍、精准 PTZ、存储卡和附录 A.4；
- 目标设备支持时再验证 SIP-TLS。

## 5. 真实上级与三级级联

### 5.1 四版本真实上级

为 1.0、1.1、2.0、3.0 各准备一个真实上级档案，Owl 共享至少一个已真实注册且可播放的下级通道。每个档案按以下顺序执行：

1. Owl 向上级发起 REGISTER，保存首次请求、401、携带 Digest 的请求、200、续注册、Keepalive 和注销；核对 `X-GB-Ver` 与配置档案一致。
2. 上级查询平台根和共享通道的 Catalog、DeviceInfo、DeviceStatus、RecordInfo；确认返回编码已映射，不泄露真实下级编码。
3. 上级分别发起直播、回放和下载，完成 `INVITE → 200 → ACK → INFO（历史）→ BYE`，核对下级实际媒体、停止后 SIP 对话和 RTP 端口均释放。
4. 1.1+ 执行 Catalog、Alarm、MobilePosition 订阅；3.0 增加 PTZPosition。核对续订、过滤、增量通知、空 terminated NOTIFY 重订和 `Expires: 0` 退订。
5. 执行语音广播/对讲，必须证明真实上级音频源、Owl 媒体转发和真实下级扬声器可听，不能只保存 SIP 200。
6. 同时注册两个上级并访问同一共享通道，确认对话所有权隔离、下级订阅引用计数和一方注销/退订不影响另一方。

### 5.2 3.0 三级指定路径

搭建“真实 3.0 上级 → Owl → 真实 3.0 下级平台 → 设备”，平台编码使用 20 位数字且类型码为 `200`：

1. 对直播、回放、下载分别发送包含完整路径的 `X-PreferredPath`。
2. 抓取 Owl 向下级转发的 INVITE，确认只移除 Owl 本级编码并保留剩余路径。
3. 核对返回给上级的 `X-RoutePath` 按实际经过顺序前置 Owl 编码，且媒体确实来自指定下级设备。
4. 分别构造错误首跳、重复平台、下级路径确认不一致和包含 Owl 的回环路径，预期明确拒绝且不遗留 SIP/RTP 会话。
5. 同一通道使用两条不同指定路径并发点播，确认媒体源、Call-ID、控制 CSeq 和释放互相隔离。

### 5.3 `Date + Note` 互通

1. 摘要关闭时完成 REGISTER、MESSAGE 和级联基线。
2. 开启 Enabled，协商双方 seed，分别验证 Base64 与 hex；至少验证 MD5/SHA-256 中一种及 SM3。
3. 开启 Required，确认缺失 Note、错误 seed、正文篡改和超出时间窗的 Date 被拒绝；合法响应正常解除事务等待。
4. 确认 REGISTER 请求/响应保持免签，`RequireMessageAuth` 的 RFC Digest 与 `Date + Note` 没有被混为同一开关。

### 5.4 证书 REGISTER 与 CRL

该项只在目标设备或上级明确支持 2016 `Authorization: Capability/Asymmetric` 时启用，且必须和 SIP-TLS 客户端证书分开记录：

1. 记录平台证书、设备证书、签发 CA、CRL 的序列号、有效期和指纹；私钥、注册随机秘密和完整 Authorization 不得进入证据库。
2. 先在非 Required 模式完成 `Capability/Asymmetric` 双向挑战，核对两段 Base64 nonce、协商算法、平台身份证明、设备秘密解密和最终 response。
3. 开启 Required，分别使用未知设备证书、错误设备 ID 映射、过期证书、已吊销证书、错误平台签名、过期/重放 nonce 和声明算法不一致报文，确认均被拒绝且不会回退到口令 Digest。
4. 设备侧和级联侧各完成一轮成功与失败用例；归档脱敏 SIP 报文、CA/CRL 版本、拒绝原因和服务端日志关联 ID。

### 5.5 `Monitor-User-Identity` 跨域身份

搭建“真实上级用户目录 → 直接上级 211 网关 → Owl 211 网关 → 下级”链路，每个上级使用独立策略：

1. 配置真实用户 ID、机构、类别、职级，本域发起查询、控制、直播、回放/下载、广播和订阅，确认下行请求及事务响应携带同一身份。
2. 从上级发送已验证外域身份，确认 Owl 只在原值最前面追加本地网关 ID；续订、终止重建和退订必须保留身份，不同用户不得复用同一条下级订阅。
3. 分别构造缺失头、重复头、非法分段、非 `211` 网关、非 `300-499` 用户编码、直接网关不匹配、未知/重复网关、超出 MaxHops、路由环和机构/类别/职级越权，确认 Required 策略在发送下级信令前拒绝。
4. TLS 场景确认身份绑定已验证连接；UDP/TCP 明文场景必须记录 IPsec 或等价可信边界。没有该网络证据时，不得把仅源 IP 匹配记为通过。
5. 保存脱敏 `Monitor-User-Identity` 报文、授权目录快照版本、网关拓扑和每个拒绝用例的审计记录。

## 6. 证据归档

每台设备使用以下目录：

```text
docs/testing/evidence/gb28181/<version>/<vendor>-<model>-<firmware>/
├── device.md
├── register.sip.txt
├── catalog.sip.txt
├── live.sip.txt
├── history.sip.txt
├── alarm.sip.txt
├── direct-download.sip.txt
├── expected.sha256
└── result.md
```

真实上级使用独立目录：

```text
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

`result.md` 必须写明通过/失败、失败步骤、抓包文件名、下载字节数、SHA-256 和是否回滚。未经脱敏的报文不得进入 Git。

## 7. 灰度与回滚

1. 首批只对白名单设备启用 1.0/1.1 自动协商和所需扩展；
2. 观察注册失败率、目录超时率、播放成功率、异常断流和直接 TCP 成功/失败/取消；
3. 单设备问题优先通过 `gb_version` 设置为已验证版本或清除手动覆盖；
4. 直接 TCP 出现异常时关闭 `Sip.DirectTCPDownload.Enabled`，活动下载会被取消并保留失败状态；
5. 回滚后重新查询诊断和指标，并归档操作时间。
