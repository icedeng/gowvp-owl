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
