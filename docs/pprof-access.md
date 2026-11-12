# pprof 诊断入口访问

pprof 默认关闭。开启 `Server.HTTP.PProf.Enabled` 后，`/debug/pprof/*` 与 `/debug/vars` 同时要求：

1. 真实 TCP 对端 IP 在 `Server.HTTP.PProf.AccessIps` 中。
2. `Authorization: Bearer <token>` 携带有效的平台管理员登录令牌；签名、有效期和当前管理员用户名必须匹配。

白名单仅支持精确 IP，不支持 CIDR；空列表拒绝全部访问，非法条目不会匹配。IPv4 映射 IPv6 地址按 IPv4 比较。配置修改后需重启服务。

诊断鉴权不读取 `X-Forwarded-For`、`X-Real-IP`、`Forwarded`，不调用第三方 `AuthURL`，不接受 URL 中的 `token` 或 `access_token`。即使请求来自回环地址，也必须提供管理员令牌。

## 本机采集

将通过平台正常登录取得的令牌放入当前终端的 `OWL_ADMIN_TOKEN` 环境变量，勿写入仓库、URL 或共享脚本。以下示例假设 HTTP 端口为 `15123`，按实际配置替换：

```sh
curl --fail \
  -H "Authorization: Bearer ${OWL_ADMIN_TOKEN}" \
  http://127.0.0.1:15123/debug/pprof/

# 采集 10 秒 CPU profile，再离线分析。
curl --fail \
  -H "Authorization: Bearer ${OWL_ADMIN_TOKEN}" \
  'http://127.0.0.1:15123/debug/pprof/profile?seconds=10' \
  -o /tmp/owl-cpu.pprof

go tool pprof /path/to/matching/owl-binary /tmp/owl-cpu.pprof
```

二进制应与采集实例的构建版本匹配。旧的匿名采集脚本或直接在浏览器中打开诊断地址会被拒绝，需使用能添加请求头的采集方式。

## 代理与容器

经过 Nginx 等代理时，白名单匹配的是代理连接后端时的实际来源地址，代理需透传管理员的 `Authorization` 请求头。不能在代理上为所有请求自动注入固定管理员令牌，否则会失去按调用者认证的保护。远程采集应通过 HTTPS 或 SSH 隧道传递令牌。

Docker 容器中的 `127.0.0.1` 指容器自身；通过宿主机映射端口连接时，服务看到的来源可能是网桥或网关地址。按实际连接来源配置精确白名单，不要用转发头中的地址代替。仅增加代理或网桥 IP 不会放行匿名请求。

此变更仅保护诊断路由，没有更改普通业务接口的 Gin 全局可信代理或 CORS 设置，管理员改密后诊断令牌也会失效，需重新登录取得令牌，详见 [账号安全与迁移说明](admin-session-security.md)。

## 状态码排查

| 状态码 | 含义 |
|---|---|
| 401 | 来源允许，但缺少有效管理员 Bearer 令牌，或使用了 URL 令牌、重复 Authorization 请求头 |
| 403 | 真实连接来源不在白名单内，或白名单为空/没有有效匹配项 |
| 404 | 诊断未开启、路径不存在，或上游代理没有转发该路径 |

默认继续保持关闭；需要诊断时再开启，采集完关闭。生产代理仍应限制诊断路径的可达范围。
