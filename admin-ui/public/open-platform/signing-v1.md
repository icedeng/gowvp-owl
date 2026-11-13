# AK/SK 请求签名 v1

每次服务端请求自行签名，无登录/Token交换。AK/SK 不能传给浏览器。SK 为 Base64URL 无填充字符串，**先解码成32字节再作HMAC key**。

下列十行以 LF 连接，末尾不加换行，UTF-8 编码：

```text
OWL-HMAC-SHA256
<audience>
<HTTP_METHOD_UPPERCASE>
<canonical_path>
<canonical_query>
<canonical_content_type>
<timestamp>
<nonce>
<idempotency_key_or_empty>
<body_sha256_hex>
```

请求头：

```http
Authorization: OWL-HMAC-SHA256 Credential=<AK>,Signature=<HMAC-SHA256小写hex>
X-Owl-Audience: <固定部署标识>
X-Owl-Timestamp: <UTC Unix秒十进制>
X-Owl-Nonce: <至少16随机字节的Base64URL无填充>
X-Owl-Content-SHA256: <原始body SHA256小写hex>
Content-Type: application/json
Idempotency-Key: <写操作幂等键>
```

规则：

- path 包含 `/openapi/v1`；ASCII 安全集，不能有尾斜线、双斜线、点路径、反斜线、百分号编码。代理保留原路径，不重定向/重写前缀。
- query 按严格百分号解码，`+` 是字面加号；空参数值保留。重复解码后的参数名和非法 UTF-8/转义均拒绝。名称和值按 UTF-8 字节 RFC3986 编码（仅字母数字 `-._~` 保留，其余大写 `%HH`、空格 `%20`），按编码后名称/值排序，再用 `&` 连接 `name=value`。
- GET/DELETE 不带 body、Content-Type 空；其他写请求仅 `application/json`，不带 charset 或 Content-Encoding。body 最多 1MiB，签名使用实际发送字节，不重新序列化。未知 JSON 属性/重复属性拒绝。
- 空 body 摘要为 `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`。不要把 hash 的 hex 文本再做一次hash。
- 时间窗±300秒；nonce按 `(AK,nonce)` 原子持久化，保留到签名时间+360秒。重试更换nonce/时间/签名；不得复用整条签名请求。
- live-sessions、snapshot-tasks、云台指令、创建订阅、受控重投必须带 Idempotency-Key（≤128字符）。相同应用/操作/键返回同一资源，不同参数409，执行中/崩溃结果未核对409。保留24小时；设备动作不会自动重发。失败请求若尚未生成资源，首期保守保留pending，修正后使用新幂等键，不能把409理解为允许重复下发。
- 认证头不允许重复，凭据不放query；不依赖Host判断部署身份，用签名内的Audience。

[固定测试向量](./signing-vectors.json) 使用公开测试密钥，不能上线。Go/Python/Java 的 `--vector` 输出均应等于文件 signature。示例可直接运行：

```sh
go run ./examples/open-platform/go --vector
python3 examples/open-platform/python/owl_client.py --vector
java examples/open-platform/java/OwlClient.java --vector
```

去掉 `--vector` 后从服务端环境 `OWL_API_BASE`（开放 origin；当前 HTTP 部署还需 `OWL_ALLOW_HTTP=true`）、`OWL_AUDIENCE`、`OWL_AK`、`OWL_SK` 读取，调用设备列表。示例不记录凭据或签名。生产接入按错误分类重试，只对安全读取/已知幂等操作重试。

## 回调签名

回调签名使用独立 callback SK，通过 key_id 选择密钥。除 Content-Type: application/json 外，头包括：

```http
X-Owl-Callback-Key-ID: <独立签名密钥ID>
X-Owl-Timestamp: <UTC Unix秒>
X-Owl-Nonce: <随机Base64URL>
X-Owl-Event-ID: <事件ID>
X-Owl-Delivery-ID: <持久化投递ID>
X-Owl-Content-SHA256: <实际body的SHA256小写hex>
Authorization: OWL-CALLBACK-HMAC-SHA256 Signature=<HMAC-SHA256小写hex>
```

待签名串六行，末尾无 LF：

```text
OWL-CALLBACK-HMAC-SHA256
<timestamp>
<nonce>
<event_id>
<delivery_id>
<body_sha256_hex>
```

接收方首先限制body大小并拒绝重复安全头，按 key_id 取独立回调密钥，核对时间窗、实际body hash和签名（常量时间比较）；之后按 `(subscription_id,key_id,nonce)` 持久化防重放，保留至 timestamp+360秒。解析JSON后核对 event_id/delivery_id 与头相同，按 delivery_id 幂等提交再响应2xx。不同投递重试有新nonce，同一 delivery_id 重复业务数据应返回成功而不重复处理。示例 verifyCallback 只做密码学验证，调用方负责持久化去重与header重复检查。

`device.online` / `device.offline` 沿用上述签名，`X-Owl-Event-ID` 固定为字符串 `0`，六行待签名串的第四行也使用 `0`。设备通知正文无 `event_id`，改为核对该头为 `0` 以及 `delivery_id` 与头一致。正文包含设备内部 `device_id`、编码 `device_code`、名称、协议类型、`previous_is_online`、`is_online`、`occurred_at`、`reason`；不含告警媒体。`sequence` 与 `notification_id` 相同，均为持久化通知序号；按 `delivery_id` 去重，并为每台设备仅应用较大的序号，不能按固定为0的事件头去重。接收方须先验签、防重放，再根据 `event_type` 区分告警与设备状态正文。
