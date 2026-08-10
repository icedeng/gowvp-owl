# GB/T 28181 1.0/1.1 开发计划完成审计

审计时间：2026-08-10（Asia/Shanghai）

## 1. 审计结论

代码建设、自动化测试和本地协议模拟器已完成到开发计划的 AI-502；AI-403 的 2014 附录 O 裸 TCP 下载已经实现，不再是代码阻断项。

目标尚不能标记为生产完成，原因是 AI-503 真实设备矩阵和 AI-602 部署灰度/回滚必须在外部设备及目标环境执行。完整上下级平台级联不属于本阶段范围。

## 2. 任务逐项证据

| 任务 | 状态 | 当前证据 |
|---|---|---|
| AI-000 基线 | 完成 | `docs/testing/gb28181-baseline.md` |
| AI-001 四版本夹具 | 完成 | `pkg/gbs/testdata/gb28181/{1.0,1.1,2.0,3.0}`；`TestGBVersionSIPFixturesParse`、`TestGBVersionXMLAndSDPFixtures` |
| AI-101 版本/能力矩阵 | 完成 | `pkg/gbs/version.go`；`version_test.go`、`version_gate_test.go` |
| AI-102 X-GB-Ver 1.1 | 完成 | SIP context/header 测试及四版本 SIP 夹具 |
| AI-103 持久化/手动覆盖 | 完成 | `DeviceExt` 版本档案、设备编辑输入、在线会话刷新；`device_version_test.go` |
| AI-104 REGISTER 协商 | 完成 | 401/200/失败响应平台 3.0、声明/有效版本；`register_version_test.go`、`gb10_core_flow_test.go` |
| AI-105 下行版本/探测 | 完成 | 下行使用有效版本；1.0 不发 ConfigDownload；版本夹具与门禁测试 |
| AI-201 1.0 门禁 | 完成 | Config、广播/对讲、RTP over TCP、IFrame、DragZoom 等能力门禁测试 |
| AI-202 1.0 SDP | 完成 | 统一 SDP Builder；直播/回放/下载 golden；Subject、u/t/y/f 测试 |
| AI-203 1.0 核心流程 | 完成（模拟器） | REGISTER→Keepalive→Catalog→INVITE→ACK→BYE、RecordInfo、Alarm 流程测试 |
| AI-301 Catalog 扩展 | 完成 | 目录树五类节点、2014 字段、厂商 XML 保留；`catalog_extension_test.go` |
| AI-302 配置读写 | 完成 | ConfigDownload、BasicParam 写入、原始 XML、Web/Core/Adapter API；`config_11_test.go` |
| AI-303 MediaStatus | 完成 | Call-ID 关联、121、未知/重复幂等；`media_status_test.go`；并纳入 1.1 串联模拟流程 |
| AI-304 多响应 | 完成 | 乱序、重复、总数冲突、超时部分结果、同设备并发；`multi_response_test.go` |
| AI-305 目录订阅 | 完成 | 初始、续订、取消、Event id；`subscribe_11_test.go` |
| AI-401 广播时序 | 完成 | Broadcast MESSAGE/业务 Response/INVITE；`broadcast_11_test.go` |
| AI-402 技术验证 | 完成 | `docs/adr/gb28181-2014-direct-tcp-download.md`，已采用 Owl 内置客户端 |
| AI-403 裸 TCP 下载 | 完成（模拟器） | 独立 `tcp` SDP、文件客户端、进度/取消 API、SHA-256、原子落盘、大小/并发/超时/地址策略；`direct_tcp_*_test.go` |
| AI-404 媒体保活 | 完成 | ZLM/LALMAX 流事件收敛、MediaStatus/BYE 竞争；`media_lifecycle_test.go` |
| AI-501 2.0/3.0 回归 | 完成（自动化） | RTP over TCP、对讲隔离、2022 升级/抓拍/精确 PTZ/存储卡/A.4；`version_regression_test.go` |
| AI-502 诊断/API | 完成 | 有效版本、来源、能力、最近拒绝；手动版本设置/清除；下载状态、取消和灰度指标 API |
| AI-503 真实设备矩阵 | 待外部执行 | `docs/testing/gb28181-device-matrix.md` 提供证据模板，`docs/testing/gb28181-interoperability-runbook.md` 提供逐步执行与回滚手册 |
| AI-601 发布候选验证 | 完成（自动化） | 全仓 test/vet/race 和补丁检查通过；外部真实设备与部署验收归入 AI-503/AI-602 |
| AI-602 灰度/回滚 | 待目标环境执行 | 已具备按设备手动版本覆盖、诊断、`GET /gb28181/metrics` 和关闭下载即取消活动任务；尚无目标环境指标及回滚演练证据 |

## 3. 阶段门

| 阶段门 | 结果 |
|---|---|
| G0 版本映射/夹具 | 自动化证据通过 |
| G1 四版本 REGISTER/未知版本保守处理 | 自动化证据通过 |
| G2 1.0 模拟核心流程 | 模拟器通过；真实设备项待 AI-503 |
| G3 1.1 信令和数据 | 模拟器通过；新增 Keepalive→Catalog→DeviceConfig→MediaStatus 串联回归 |
| G4 1.1 广播和下载 | 模拟器通过；真实设备兼容待 AI-503 |
| G5 生产发布 | 未通过：设备矩阵、目标环境灰度和回滚未完成 |

## 4. 最近验证

通过：

```bash
go test ./pkg/gbs/... ./internal/conf ./internal/adapter/gbadapter ./internal/core/ipc ./internal/web/api -run '^Test' -count=1
go test ./... -count=1
go vet ./...
go test -race -vet=off ./... -count=1
git diff --check
```

## 5. 生产完成所需证据

1. 按设备矩阵完成 1.0 两厂商、1.1 两厂商、2.0 一厂商、3.0 一厂商联调；
2. 每台设备归档厂商、型号、固件、网络模式、脱敏 SIP 报文、文件 SHA-256 和结果；
3. 在最终发布候选提交上重跑全仓 test/vet/race；
4. 白名单灰度观察注册失败率、目录超时率、播放成功率、下载失败率和异常断流；
5. 演练按设备清除/设置手动版本、关闭直接 TCP 开关和回退发布版本。
