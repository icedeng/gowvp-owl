# WSS v1 协议入口

唯一字段定义见[方案第 15—18 节](../../../docs/国标公网接入网关-WSS方案.md)和 `protocol/` 中的 Go 类型。A 与 B 共享这些类型及严格解码/验证，不维护两套协议模型。

- 子协议：`owl-gateway.v1`；TLS 由 B 直接终止并验证受信客户端证书。
- 控制：`/gateway/v1/control`；每 B/A 组合只有当前有效控制代次。
- 媒体：`/gateway/v1/media`；每活动源一条独占连接，使用一次性 attachment ticket。
- 文件：`/gateway/v1/transfer`；任务与媒体隔离，长度/摘要/EOF/持久化提交独立确认。
- 诊断：`/gateway/v1/probe`；不接管生产控制连接。
- 身份：网关稳定 ID、平台 ID、B generation、connection_epoch、owner_epoch；十进制计数与版本采用字符串。

实现补充：

1. `sip.flow.open.configured_platform` 只用于已保存上级平台配置的公网字面 IP；subject 必须是 platform，设备建连继续核验已认证绑定。
2. `TransferSpec.subject_type` 可为 device/platform，省略兼容 device；下级平台按已认证绑定执行文件任务，上级级联裸 TCP 发送腿方向为 a_to_device。
3. UDP 接收及发送适配均使用媒体节点返回的实际 RTCP 端点。未安装对应 ZLM 补丁时明确拒绝，不推算端口。
4. 抓拍任务 HTTP 入口接收 raw 图片和 multipart/form-data 文件部分，转送的 FILE_BYTES 为文件内容；文件大小与请求包装开销分别限制。
5. 累计 device_rx_bytes/device_tx_bytes 统计已接入的 SIP 与已校验/转发的媒体、文件负载，不包括 IP/TCP/TLS 开销。WSS 计数单独记录。Linux 当前 RSS 来自 /proc；不支持的采样返回 null。
6. 心跳支持 `part_index/part_count` 分片，ACK 回显分片并可包含 `traffic:[{ref,rx_bytes,tx_bytes,queue_bytes}]`。仅采样本分片已续租的同代次资源，重复或越界条目拒绝。A 持久化最后采样并计算连续样本码率，生命周期清理不覆盖采样。
7. 国标系统编码类型200注册使用独立 platform/downstream 绑定；A 向上级注册使用 platform/upstream。摄像机/NVR使用device绑定，统计不混计平台；注册到期和注销会使有效在线状态失效。

## 可靠性扩展（仍为协议v1）

新 A 在 probe 和 WebSocket 握手中发送 `X-Owl-Gateway-Extensions: gateway_info_v1,media_inspect_v1,alarm_spool_v1,sip_resume_v1`。新 B 只宣告请求头明确列出的扩展，避免旧 A 对未知能力严格拒绝；能力交集为空时沿用原路径，不能发送未协商消息。

| 能力 | 请求与结果 | 边界 |
| --- | --- | --- |
| gateway_info_v1 | `gateway.info` → 构建/协议/队列条数字节/存储错误 | 只读，不含凭据 |
| alarm_spool_v1 | `alarm.pull {after}` → `{items,next,store_error}`；`alarm.ack {ids}` → `{removed}` | hash游标、每批≤64；A业务提交后ACK；损坏原件保留，显式错误但不阻塞正常记录 |
| sip_resume_v1 | `sip.resume.list {after}` → `{items,next}` | 每批≤64；仅仍有效的认证设备flow，不能替代首次REGISTER |
| media_inspect_v1 | `media.inspect {session_id}` → `{first_packet_ms}` | 同owner/代次的媒体资源，首包耗时在B本地计算，未观测为null |

`AlarmRecord` 只包含幂等ID、原主体/绑定版本、设备/通道编码、B接收时间、历史标志和原始报警时间/文字/类型/位置，不包含图片、视频、旧SIP事务或认证头。在线沿用A持久收件箱和原协议应答；断连时B持久化成功才确认设备，A重复拉取须幂等；历史补报永不触发现场抓拍或录像。带信令签名的报警继续原始SIP转送验签，强制验签时不启用文字队列。

`ResumeFlow` 包含主体ID、绑定版本、原flow route、原到期时间与最后观测时间。A必须核对同网关/主体/原generation/flow/版本及原到期时间，在当前所有权事务内提交新epoch；B重启、更换网关、过期或心跳陈旧不能续接。详见 [可靠性边界](reliability.md)。
