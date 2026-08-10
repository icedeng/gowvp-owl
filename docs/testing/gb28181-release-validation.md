# GB/T 28181 1.0/1.1 开发验证报告

记录时间：2026-08-10（Asia/Shanghai）

## 1. 当前结论

- 1.0：代码与模拟器核心流程已完成，尚未完成真实设备矩阵验收；
- 1.1：Catalog 扩展、BasicParam、MediaStatus、多响应、目录订阅、广播和附录 O 直接 TCP 文件下载已完成自动化/模拟器验证；
- 2.0/3.0：相关包回归通过，RTP over TCP 与 2022 能力门禁未被 1.0/1.1 打开或降级；
- 上下级平台完整级联：仍不在本阶段范围内；
- 全仓 test/vet/race 已通过；对外状态仍不得标记为“生产完整支持”，真实设备矩阵和目标环境灰度/回滚仍是阶段门。

## 2. 已执行验证

### 全仓测试

```bash
go test ./... -count=1
```

结果：通过。

覆盖内容包括：

- 四版本 REGISTER、SIP/XML/SDP 夹具；
- 版本解析、协商、持久化、手动覆盖和下行版本；
- 1.0 功能门禁、直播/回放/下载 SDP golden；
- 1.0 REGISTER、Keepalive、Catalog、RecordInfo、Alarm、INVITE、ACK、BYE 模拟流程；
- 1.1 Keepalive、Catalog、DeviceConfig、MediaStatus/121、Broadcast Response 串联模拟流程；
- 1.1 Catalog 扩展与目录节点、未知 XML 保留；
- BasicParam 查询与写入报文、原始响应保存；
- MediaStatus/121 幂等收敛；
- Catalog/RecordInfo 多响应乱序、重复、总数冲突、超时部分结果和同设备并发；
- `Event: Catalog;id=...` 初始订阅、续订、取消；
- Broadcast 通知与业务应答；
- 媒体注册、注销、RTP 超时状态竞争；
- 1.1/2.0/3.0 能力矩阵和诊断字段。
- 附录 O `m=video ... tcp` SDP 构建/解析，与 `TCP/RTP/AVP` 严格隔离；
- 本地 TCP 文件发送端、已知/未知大小、SHA-256、零字节、大小超限、提前 EOF、超量数据；
- 连接/首字节/空闲/总超时、取消、MediaStatus、单设备/全局并发和终态竞争；
- 下载进度查询、取消接口、白名单、注册地址校验和原子落盘。
- 注册、目录、媒体会话、异常断流和直接 TCP 下载灰度计数器。

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

1. 使用至少两个厂商的 1.1 设备验证直接 TCP 下载并归档抓包与文件哈希；
2. 完成真实设备矩阵与抓包归档；
3. 在最终发布候选提交上重新执行全量 test/vet/race；
4. 执行按设备版本回退演练；
5. 记录注册失败率、目录超时率、播放成功率和异常断流指标。
