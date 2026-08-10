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

## 5. 证据归档

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

`result.md` 必须写明通过/失败、失败步骤、抓包文件名、下载字节数、SHA-256 和是否回滚。未经脱敏的报文不得进入 Git。

## 6. 灰度与回滚

1. 首批只对白名单设备启用 1.0/1.1 自动协商和所需扩展；
2. 观察注册失败率、目录超时率、播放成功率、异常断流和直接 TCP 成功/失败/取消；
3. 单设备问题优先通过 `gb_version` 设置为已验证版本或清除手动覆盖；
4. 直接 TCP 出现异常时关闭 `Sip.DirectTCPDownload.Enabled`，活动下载会被取消并保留失败状态；
5. 回滚后重新查询诊断和指标，并归档操作时间。
