# ADR：GB/T 28181-2014 直接 TCP 文件下载

- 状态：已采用（按推荐方案实施）
- 日期：2026-08-10
- 对应任务：AI-402 / AI-403
- 决策范围：平台作为 SIP 服务端接入下级 IPC/NVR/网关时的历史文件下载

## 1. 背景

2014 修改补充文件附录 O 定义了一种直接 TCP 下载方式：媒体文件发送方作为 TCP 服务端，接收方作为 TCP 客户端；视音频数据直接在 TCP 连接上传输，不再进行 RTP 封装。发送方通过 SIP/SDP 返回监听地址、端口和文件大小，传输结束后发送 `MediaStatus/121`。

该能力不能等同于 GB/T 28181-2016 的 `TCP/RTP/AVP`：后者仍然是 RTP 数据，只是 RTP 承载于 TCP。

## 2. 当前实现证据

当前媒体抽象只有 RTP 接收能力：

- `sms.Driver.OpenRTPServer` / `CloseRTPServer`；
- ZLMediaKit 使用 `/index/api/openRtpServer`，请求包含 `tcp_mode`，用途是 GB28181 RTP 接收；
- LALMAX 使用 `ApiCtrlStartRtpPub`，同样进入 RTP 发布处理；
- `StartHistory` 无论 UDP 还是 TCP 都先开启 RTP 端口，再生成 `RTP/AVP` 或 `TCP/RTP/AVP` SDP；
- 当前没有裸 TCP 文件接收、文件大小校验、进度、断点、取消或临时文件原子落盘接口。

结论：现有 ZLM/LALMAX 驱动均不能直接承接 2014 附录 O 数据，且把 `tcp_mode` 用于此场景会错误地按 RTP 解包。

## 3. 决策

建议由 Owl 实现独立的 `DirectTCPDownloadManager`，不扩展 ZLM/LALMAX RTP 接口，也不复用设备 `StreamMode`。

原因：

1. 附录 O 的数据面是文件字节流，不需要转码、分发或直播协议输出；
2. Owl 已负责 SIP 会话、历史下载生命周期和 `MediaStatus`，在同一进程管理文件状态更直接；
3. 可以明确隔离裸 TCP 与 RTP-over-TCP，避免破坏 2.0/3.0；
4. 两种媒体服务器行为一致，不需要分别实现私有扩展。

## 4. 连接角色与信令

推荐角色：

- 下级设备/NVR：TCP 服务端、文件发送方；
- Owl：TCP 客户端、文件接收方；
- ZLM/LALMAX：不参与裸 TCP 数据面。

拟实现流程：

1. 仅当有效版本为 `1.1` 且显式选择 `direct_tcp` 下载模式时启用；
2. Owl 发起 `s=Download` 的 INVITE，SDP 传输标识使用 2014 定义的 `tcp`，不得生成 `TCP/RTP/AVP`；
3. 解析设备 200 OK SDP 中的发送 IP、端口、`a=filesize`、`y`；
4. 校验地址后，由 Owl 主动连接设备 TCP 服务端；
5. 数据先写入同目录临时文件，持续更新已接收字节数；
6. 收满声明大小、收到 EOF 或 `MediaStatus/121` 后按一致性规则结束；
7. 校验成功后原子重命名为目标文件；失败、取消或超时删除临时文件并关闭 SIP 会话。

设备未返回 `filesize` 时允许 EOF 收敛，但状态标记为“大小未验证”，不能报告为强校验成功。

## 5. 建议接口

```go
type DirectTCPDownloadRequest struct {
    SessionID string
    DeviceID  string
    ChannelID string
    Address   string
    FileSize  int64
    Output    string
    Timeout   time.Duration
}

type DirectTCPDownloadState struct {
    SessionID string
    Status    string
    Received  int64
    FileSize  int64
    StartedAt time.Time
    UpdatedAt time.Time
    Error     string
}

type DirectTCPDownloadManager interface {
    Start(context.Context, DirectTCPDownloadRequest) error
    Cancel(sessionID string) error
    State(sessionID string) (DirectTCPDownloadState, bool)
}
```

该接口放在 GB 协议模块或独立内部包，不加入 `sms.Driver`，因为它不是流媒体驱动能力。

## 6. 文件、进度、超时和取消

- 文件名由平台生成，禁止直接使用设备返回路径；
- 输出根目录必须来自配置，并经过 `filepath.Clean` 与根目录约束；
- 默认单文件最大值、单设备并发数和全局并发数必须可配置；
- 连接超时、首字节超时、空闲超时、总时长分别控制；
- 进度以 `Received/FileSize` 计算；未知大小只报告已接收字节数；
- `Cancel` 关闭连接、取消上下文、发送 BYE，并清理临时文件；
- 状态更新需要并发安全，完成/取消/`MediaStatus` 竞争时只允许一次终态迁移。

## 7. 安全限制

- 默认只允许连接 REGISTER/Keepalive 确认的设备地址；
- 禁止环回、链路本地、组播和与注册地址不一致的任意地址，除非设备兼容配置显式放行；
- 端口必须在 `1..65535`；
- 限制文件大小、连接数和写入速率，防止磁盘耗尽；
- 临时文件权限使用最小权限，不执行、不自动解析设备提供的文件名；
- 日志不得记录鉴权口令或完整私有地址凭据。

## 8. 与现有版本的隔离

| 有效档案 | UDP 下载 | RTP over TCP 下载 | 2014 直接 TCP 下载 |
|---|---:|---:|---:|
| 1.0 | 支持 | 禁止 | 禁止 |
| 1.1 | 支持 | 禁止 | 显式开启 |
| 2.0 | 支持 | 支持 | 默认禁止 |
| 3.0 | 支持 | 支持 | 默认禁止 |

直接 TCP 必须使用独立枚举或开关，例如 `download_transport=direct_tcp`，不能复用 `StreamMode=1/2`。

## 9. 备选方案

### 方案 A：扩展 ZLM/LALMAX

不采用。两者当前接口面向 RTP；需要为两个项目分别新增裸文件协议、状态和文件 API，改动面更大，且文件下载并不需要媒体服务器的流分发能力。

### 方案 B：把裸 TCP 当作 RTP over TCP

禁止。协议语义和字节格式不同，会导致解包失败或文件损坏。

### 方案 C：第一阶段只保留 UDP 下载

可作为发布降级方案。若真实设备不要求附录 O，可先将 1.1 标记为“直接 TCP 下载未启用”，不影响其余 2014 信令能力。

## 10. 验证要求

AI-403 至少覆盖：

- 本地 TCP 发送端模拟器与 SHA-256 文件校验；
- 声明大小一致、大小缺失、大小超限、提前 EOF、超量数据；
- 连接失败、首字节超时、空闲超时、取消；
- 重复完成、BYE 与 `MediaStatus/121` 竞争；
- 单设备和全局并发限制；
- 1.0 拒绝、1.1 允许、2.0/3.0 原 RTP-over-TCP 回归。

真实设备不可用时只能声明“模拟器通过”，不能声明生产兼容。

## 11. 影响与估算

- Owl 下载管理器、状态 API、配置与单元/集成测试：3～5 个 AI 开发日；
- 协议与安全人工评审：1 人日；
- 每个设备型号联调：0.5～1.5 人日；
- 若技术负责人改选媒体服务器扩展方案，需重新估算并分别评估 ZLM/LALMAX。

## 12. 批准项

进入 AI-403 前需明确批准：

1. 采用“Owl 内置 TCP 客户端”，还是改为媒体服务器扩展；
2. 文件输出根目录和默认保留策略；
3. 单文件大小、并发数和超时默认值；
4. 是否允许连接与设备注册地址不同的 SDP 地址；
5. 1.1 直接 TCP 是否默认关闭并按设备白名单启用。

## 13. 实施决策

根据“按计划直接执行开发”的授权，采用本文推荐方案：

1. Owl 内置独立 TCP 客户端，不修改 ZLM/LALMAX RTP 接口；
2. 默认输出目录为 `./configs/downloads/gb28181`，默认保留 7 天；
3. 默认单文件上限 10 GiB、全局并发 4、单设备并发 1；
4. 默认连接超时 5 秒、首字节超时 15 秒、空闲超时 30 秒、总超时 2 小时；
5. 默认禁止连接与 REGISTER 地址不同的 SDP 地址；显式允许地址不一致时还必须匹配 CIDR 白名单，并始终禁止环回、链路本地、组播和未指定地址；
6. 功能默认关闭，启用后仍要求设备 ID 出现在白名单；
7. 下载文件使用平台生成名称、0600 临时文件、SHA-256 校验记录和同目录原子重命名。
