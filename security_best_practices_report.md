# Owl 安全审计报告

审计日期：2026-08-10
审计范围：Go HTTP/SIP 服务、Python AI gRPC 服务、配置文件、Docker/Compose 构建与部署配置
审计方式：静态代码与调用链审计；未对运行中的生产实例发送攻击请求

## 执行摘要

当前代码不适合直接暴露到互联网或不可信局域网。审计确认 **3 个严重（Critical）、8 个高危（High）、3 个中危（Medium）问题**。

最严重的攻击链无需登录：攻击者可伪造 ZLMediaKit 录像回调，将任意绝对路径写入录像表，再通过未鉴权的录像列表/下载接口读取该文件；录像清理任务还可能按同一路径删除文件。结合仓库中已提交的 JWT、媒体服务等明文密钥，可进一步伪造管理令牌并控制媒体服务。

建议在任何继续公网部署之前，先隔离 15123、15060、50051 等端口，立即轮换仓库中出现过的全部密钥，并优先修复 SEC-001～SEC-006。

## 严重（Critical）

### SEC-001：未鉴权录像回调可形成任意文件读取与延迟删除链

- Rule ID：GO-PATH-001 / GO-UPLOAD-001
- Severity：Critical
- Location：
  - `internal/web/api/api.go:117,125,134`
  - `internal/web/api/zlm_webhook.go:45-57,380-429`
  - `internal/web/api/recording.go:51-70,198-221`
  - `internal/core/recording/core.go:58-68`
  - `internal/core/recording/cleanup.go:166-177,270-283`
- Evidence：
  - `/webhook/on_record_mp4` 注册时没有认证或回调签名中间件。
  - `onRecordMP4` 接受外部 `file_path`/`url`，经 `filepath.Clean` 后直接存入 `Recording.Path`，未验证路径位于录像根目录。
  - `/recordings`、`/recordings/:id/download` 注册时未传入 JWT 中间件。
  - `GetFullPath` 对绝对路径直接返回；下载处理随后调用 `c.File(filePath)`。
  - 清理任务对数据库中的绝对路径直接调用 `os.Remove(filePath)`。
- Impact：**未认证远程攻击者可读取进程账户有权读取的任意文件，并可诱导定时清理任务删除进程账户有权删除的任意文件，最终可导致密钥泄露、系统接管或服务破坏。**
- Fix：
  1. 对 ZLM Webhook 强制校验独立的 HMAC/随机共享密钥，并限制来源网络。
  2. 录像表仅保存经过服务端生成的相对对象标识；拒绝绝对路径、`..`、符号链接逃逸。
  3. 在读写/删除前通过 `filepath.Abs` + `filepath.Rel` 做根目录包含检查，拒绝不在 `StorageDir` 内的目标。
  4. 给所有录像管理与下载路由挂载 JWT/授权中间件；静态录像不要直接使用 `gin.Static`。
- Mitigation：立即在反向代理/防火墙阻断公网对 `/webhook/*`、`/recordings*`、`/static/recordings*` 的访问，仅允许媒体服务器到达 Webhook。
- False positive notes：若入口代理已严格按源 IP 和路径隔离，远程可利用性会降低；应用代码中未看到该控制，且容器配置直接映射了管理端口。

### SEC-002：版本库提交了可用格式的明文密钥与默认管理凭据

- Rule ID：GO-CONFIG-001 / GO-AUTH-001
- Severity：Critical
- Location：
  - `configs/config.toml:4,6-8,24,76,136`（具体值已脱敏）
  - `internal/conf/default.go:12-18`
  - `internal/web/api/user.go:132-143,170-177`
  - `.gitignore:24-48`
- Evidence：
  - `configs/config.toml` 被 Git 跟踪，包含管理密码、RTMP 密钥、JWT 签名密钥和媒体 API 密钥；文件权限为 `0644`。
  - 默认和空凭据回退均为固定的 `admin/admin`。
  - `.gitignore` 未排除真实配置文件。
- Impact：**任何能读取仓库、构建产物或 SEC-001 任意文件读取链的攻击者，都可能直接登录、伪造 JWT、接管推流或调用媒体服务管理 API。**
- Fix：立即轮换所有已经出现过的密钥；从 Git 历史中清除敏感值；提交无秘密的示例配置；生产密钥从环境变量/Secret Manager 注入；配置文件权限设为 `0600`；启动时拒绝默认或空管理密码。
- Mitigation：在轮换完成前撤销公网访问并将相关服务隔离到可信管理网。
- False positive notes：即使这些值只用于测试，也应按已泄露处理，因为无法证明其从未用于部署或从未进入历史/镜像缓存。

### SEC-003：录像与事件管理面整体缺失实际鉴权

- Rule ID：GO-HTTP-005（鉴权边界）/ CWE-862
- Severity：Critical
- Location：
  - `internal/web/api/api.go:127-134`
  - `internal/web/api/event.go:45-55,73-122`
  - `internal/web/api/recording.go:51-70,96-164,198-221`
- Evidence：`RegisterEvent(r, uc.EventAPI)` 和 `RegisterRecording(r, uc.RecordingAPI)` 调用时未传入已经创建的 `auth` 中间件，尽管 Swagger 注释声明 `BearerAuth`。因此事件查询、修改、删除，录像查询、修改、删除、下载以及整个静态录像目录均为匿名访问。
- Impact：**未认证攻击者可批量查看监控事件与录像、下载隐私视频、篡改统计并删除证据数据。**
- Fix：默认对整个管理 API 组应用认证；只对明确需要匿名的健康检查建立单独公开组；录像文件使用短期、单资源、不可复用的签名 URL，而不是公开静态目录。
- Mitigation：在边缘代理临时要求统一认证，并关闭目录静态服务。
- False positive notes：若边缘代理已经为这些精确路径强制认证，可作为临时补偿控制；应用本身没有防护。

## 高危（High）

### SEC-004：AI Webhook 未认证，可任意目录写入、伪造告警并耗尽磁盘

- Rule ID：GO-PATH-001 / GO-UPLOAD-001 / GO-HTTP-002
- Severity：High
- Location：
  - `internal/web/api/api.go:127-130`
  - `internal/web/api/ai_webhook.go:51-58,121-185,360-385`
- Evidence：
  - `/ai/events` 未校验 Python 服务发送的 `Authorization`/回调密钥。
  - `camera_id` 直接参与 `filepath.Join(eventsDir, cid, filename)`，未做根目录包含检查。
  - Base64 快照未限制大小、未验证图片类型，直接 `os.WriteFile`；限流键也是攻击者可任意变化的 `camera_id`。
- Impact：攻击者可伪造检测事件、污染取证数据，把任意内容写入任意可创建目录下的随机 `.jpg` 文件，并通过变化 `camera_id` 持续占用数据库与磁盘。
- Fix：强制 HMAC 回调签名和时间戳/重放保护；验证 `camera_id` 必须对应现有通道；使用服务端目录映射；限制请求体、解码后大小、像素和文件类型。
- Mitigation：只允许回环地址访问 `/ai/*`。
- False positive notes：Go 进程当前把 Python 回调配置为回环地址，但 HTTP 服务监听所有网卡，路由本身并未限制来源。

### SEC-005：默认开启的 pprof IP 白名单可被伪造转发头绕过

- Rule ID：GO-DEPLOY-002 / GO-HTTP-003
- Severity：High
- Location：
  - `internal/conf/default.go:19-22`
  - `configs/config.toml:26-30`
  - `internal/web/api/provider.go:87-94`
  - `internal/web/api/api.go:69-86`
- Evidence：应用使用 `gin.New()` 后没有调用 `SetTrustedProxies`。Gin 1.11 默认信任 `0.0.0.0/0` 和 `::/0` 的 `X-Forwarded-For`/`X-Real-IP`；`goddd/pkg/web` 的 pprof 白名单通过 `c.ClientIP()` 判断，默认配置又启用了 pprof 且允许回环 IP。
- Impact：远程攻击者可伪造客户端 IP 访问 heap、goroutine、cmdline、trace、profile 和 expvar，泄露内部信息并通过昂贵 profile 请求造成资源消耗。
- Fix：生产默认关闭 pprof；独立绑定到回环/管理监听器；显式配置可信代理 CIDR，并使用实际 TCP 对端做最后一道限制。
- Mitigation：边缘阻断 `/debug/*` 并丢弃外部提供的转发头。
- False positive notes：若外部入口已不可绕过地阻断 `/debug/*` 且重写转发头，则风险降低；当前 Compose 直接映射 15123。

### SEC-006：HTTP 与 SIP 多处无请求大小上限，可远程内存耗尽

- Rule ID：GO-HTTP-002 / GO-HTTP-001
- Severity：High
- Location：
  - `internal/web/api/api.go:45-66`
  - `internal/web/api/ipc.go:612-643`
  - `internal/web/api/ipc_snapshot_upload.go:33-52`
  - `pkg/gbs/sip/server.go:254-300`
- Evidence：
  - 全局 `LoggerWithBody` 内部先通过 `gin.Context.GetRawData()` 读取完整请求体，再只截取 100 字节用于日志；路由前没有 `MaxBytesReader`。
  - 匿名 `/gb28181/snapshot` 使用 `io.ReadAll`，multipart 分片再次使用 `io.ReadAll`，随后执行图片解码。
  - SIP/TCP 直接信任 `Content-Length` 并 `make([]byte, bodyLen)`，没有最大报文/头行限制或连接读取截止时间。
- Impact：未认证攻击者可用超大 HTTP/SIP 报文或慢连接耗尽内存、goroutine 和连接资源，导致进程崩溃或长期不可用。
- Fix：全局和逐路由设置请求体上限；日志中间件使用有限读取；限制 multipart 部件数/大小和图片像素；SIP 限制首行、头部、正文长度并设置读超时。
- Mitigation：入口代理设置 body/header/连接速率上限；在防火墙限制 SIP 来源。
- False positive notes：边缘限流只能防 HTTP；SIP 端口由应用直接监听，需要应用层限制。

### SEC-007：SIP 默认关闭认证与来源校验，允许设备冒充和事件注入

- Rule ID：CWE-306 / CWE-345
- Severity：High
- Location：
  - `internal/conf/default.go:46-57`
  - `configs/config.toml:68-88`
  - `pkg/gbs/register.go:198-235`
  - `pkg/gbs/access_control.go:19-42,45-100`
- Evidence：默认 SIP 密码为空、`StrictSourceCheck=false`、`RequireMessageAuth=false`。REGISTER 在密码为空时跳过 Digest；MESSAGE/NOTIFY 中间件在两个开关关闭时直接放行。
- Impact：能访问 SIP 端口的攻击者可冒充设备注册、伪造心跳/目录/报警/媒体状态并污染设备状态和事件数据。
- Fix：生产环境强制非空的每设备随机密钥；启用 MESSAGE/NOTIFY Digest 与来源绑定；未知设备默认拒绝自动建档；支持的环境优先启用 SIP-TLS。
- Mitigation：按设备网段/IP 在防火墙设置白名单。
- False positive notes：若 15060 仅存在于物理隔离且可信的设备 VLAN，风险面降低，但配置与 Compose 均未表达这种保证。

### SEC-008：AI gRPC 在所有网卡明文、无认证开放，并接受任意 RTSP/回调 URL

- Rule ID：GO-SSRF-001（跨语言适用）/ CWE-306
- Severity：High
- Location：
  - `analysis/main.py:276-318,336-364,367-392,468-481`
  - `Dockerfile_ai:95`
- Evidence：gRPC 使用 `add_insecure_port("[::]:{port}")`，无拦截器、认证或 TLS；`StartCamera` 直接使用调用者提供的 `rtsp_url`、`callback_url` 和 `callback_secret`，创建摄像头任务并异步 POST 回调。镜像声明暴露 50051。
- Impact：当 50051 可达时，攻击者可启动无限摄像头任务、探测/访问内网 RTSP 服务、向内网 HTTP 目标发送回调并读取任务状态，造成 SSRF（服务端请求伪造）和资源耗尽。
- Fix：仅绑定 `127.0.0.1`/Unix Socket；增加 mTLS 或短期服务凭据；对 RTSP 和回调目标执行固定来源映射/主机白名单；限制并发任务与请求速率。
- Mitigation：禁止发布 50051，避免在 host network 下运行该服务。
- False positive notes：当前 Compose 未显式发布 50051，但 README/Compose 建议的 host network 或其他部署方式会使其可达。

### SEC-009：匿名媒体反向代理可访问内网媒体服务任意路径且几乎无超时

- Rule ID：GO-SSRF-001 / GO-HTTPCLIENT-001
- Severity：High
- Location：`internal/web/api/api.go:124-125,364-402`
- Evidence：`r.Any("/proxy/sms/*path", uc.proxySMS)` 未挂认证；路径、查询和 HTTP 方法被转发到配置的内网媒体主机；读写 deadline 被设置到 99 年后。结合 SEC-002/SEC-001 泄露的媒体密钥，可通过该代理访问 ZLM 管理 API。
- Impact：攻击者可匿名读取已知媒体路径、占用长连接，并在获得媒体密钥后从公网穿透到原本只监听内网的媒体管理面。
- Fix：拆分只读媒体播放代理与管理 API；路径/方法严格白名单；播放使用短期签名；恢复合理的连接、响应头、空闲和总超时；管理代理必须认证。
- Mitigation：边缘仅开放确需播放的路径，拒绝 `/index/api/*` 等管理路径。
- False positive notes：媒体服务部分 API 自带 secret，但代理本身没有做边界控制，且密钥已出现在仓库配置中。

### SEC-010：敏感请求体和查询 JWT 会进入日志

- Rule ID：GO-CONFIG-001
- Severity：High
- Location：
  - `internal/web/api/api.go:53-65`
  - `internal/web/api/user.go:154-177`
  - `internal/web/api/ipc.go:2133-2141`
  - `internal/web/api/recording.go:224-253,320-340`
  - `configs/config.toml:56-66`
- Evidence：生产 `Debug=false` 时反而不会跳过 `LoggerWithBody`；当前日志级别为 debug。修改凭据、设备密码和其他敏感 JSON 的前 100 字节可能被记录。JWT 中间件允许 `?token=Bearer ...`，普通 Logger 又记录完整 RawQuery；快照和 M3U8 代码主动把令牌拼入 URL。
- Impact：任何能读取日志、代理访问日志、浏览器历史或 Referer 的主体都可能获得管理密码或可复用三天的 JWT。
- Fix：默认禁用 body 日志；按字段结构化脱敏；永不记录 Authorization、密码、secret 和 token 查询参数；媒体 URL 使用短期单资源签名而非管理 JWT。
- Mitigation：限制日志权限和保留期，立即清理并轮换可能已记录的凭据。
- False positive notes：只有 debug 级别真正输出 body，但当前跟踪配置明确使用 debug 日志级别。

### SEC-011：构建供应链不可复现且存在明文下载

- Rule ID：GO-SUPPLY-001（广义供应链完整性）
- Severity：High
- Location：
  - `Dockerfile:1`
  - `Dockerfile_ai:4,19-28,61-77`
  - `Dockerfile_zlm:1,30-32`
  - `docker-compose.yml:5`
  - `docker-compose.debug.yml:3,16`
  - `analysis/requirements.txt:1-12`
- Evidence：使用 `alpine:latest`、`gospace/gowvp:latest`、`zlmediakit/zlmediakit:master` 和未固定版本/哈希的 Python 包；从 HTTP 镜像下载旧版 `libssl1.1`，没有哈希或签名校验，并以 `dpkg ... || true` 忽略失败；最终镜像没有 `USER`，默认以 root 运行。
- Impact：上游标签漂移、镜像/镜像站被污染或依赖解析变化都可能把恶意或未审计代码带入生产；root 运行会放大 SEC-001 的文件影响。
- Fix：全部镜像固定 digest；Python 使用锁文件和哈希；禁止 HTTP 包下载及忽略安装失败；使用受支持的 OpenSSL；生成 SBOM 并在 CI 做漏洞/签名验证；最终镜像切换非 root 用户。
- Mitigation：只从内部可信制品库拉取已签名镜像。
- False positive notes：`go mod verify` 已通过，且未发现关闭 Go checksum DB 的配置；这不覆盖容器和 Python 供应链。

## 中危（Medium）

### SEC-012：CORS 对任意 Origin 开放并允许凭据

- Rule ID：GO-HTTP-007
- Severity：Medium
- Location：`internal/web/api/api.go:69-86`
- Evidence：`AllowOriginFunc` 无条件返回 `true`，同时 `AllowCredentials=true`，并允许 Authorization 与全部修改方法。
- Impact：任意网站都能从浏览器跨域调用并读取该系统 API；如果未来使用 Cookie、浏览器插件自动附加凭据或令牌被同源脚本取得，风险会升级为跨站控制。
- Fix：按部署配置维护精确 Origin 白名单；不需要 Cookie 时关闭 credentials；限制方法和请求头。
- Mitigation：边缘覆盖应用返回的 CORS 头。
- False positive notes：当前主要使用 Bearer Token 时，浏览器不会自动附加 Authorization，因此不是传统 Cookie-CSRF；它仍破坏了预期同源边界。

### SEC-013：口令和会话生命周期设计薄弱

- Rule ID：GO-AUTH-001
- Severity：Medium
- Location：`internal/web/api/user.go:119-151,154-180`
- Evidence：管理密码以明文存储并使用普通字符串比较；登录没有速率限制；JWT 有效期固定三天；修改密码不轮换 JWT secret、没有 token version/jti 撤销机制，因此旧令牌继续有效。
- Impact：配置泄露即暴露原始密码；登录可被持续猜测；密码修改后，被盗会话仍可继续控制系统。
- Fix：使用 Argon2id/bcrypt 保存口令哈希；恒定时间比较适用的服务密钥；登录按 IP+账户限速；JWT 缩短有效期并增加 token version/撤销机制；修改密码时失效所有旧会话。
- Mitigation：边缘限流并定期轮换 JWT secret。
- False positive notes：这是单管理员系统，但单账户并不会降低凭据泄露后的影响。

### SEC-014：HTTP 基线与信息暴露控制不足

- Rule ID：GO-HTTP-001 / GO-HTTP-004 / GO-DEPLOY-002
- Severity：Medium
- Location：
  - `internal/app/app.go:76-81`
  - `internal/web/api/api.go:104-116,166-208`
- Evidence：底层 HTTP server 只设置 Read/WriteTimeout，未显式设置 `ReadHeaderTimeout` 和 `MaxHeaderBytes`；应用未设置 CSP、`X-Content-Type-Options`、frame-ancestors/X-Frame-Options、Referrer-Policy；Swagger、进程指标和版本/内存统计匿名开放。
- Impact：增加慢头/大头拒绝服务、浏览器内容嗅探/点击劫持及内部版本与容量信息泄露风险。
- Fix：完善 HTTP server 限制与集中安全头；生产关闭或认证 Swagger/详细指标；保留最小化健康检查。
- Mitigation：在可信反向代理设置等价限制和响应头。
- False positive notes：安全头和部分超时可能由未提交的边缘配置提供，需要在实际响应和代理配置中复核。

## 验证结果

已执行：

- CodeGraph 调用链审计：路由、认证、文件、Webhook、SIP、gRPC、代理、清理任务和部署链路。
- `go mod verify`：通过（`all modules verified`）。
- `GOCACHE=/private/tmp/owl-go-build go test ./internal/web/api ./internal/core/recording ./pkg/gbs/... -count=1`：
  - `internal/web/api` 通过；
  - `pkg/gbs/sip` 通过；
  - `pkg/gbs` 中需要绑定 `127.0.0.1:0` 的测试被沙箱拒绝，不能据此判断通过或失败。

未执行：

- 未对生产/运行实例发送利用请求，未验证外围 WAF、反向代理、VLAN 或防火墙补偿控制。
- 当前环境未安装 `govulncheck`、`gosec`、`pip-audit`，因此未完成 Go/Python CVE 与 SAST 工具扫描。
- 未执行动态浏览器测试、容器镜像扫描、SBOM、秘密历史扫描和竞态检测全量运行。

## 建议修复顺序

1. 立即隔离端口并轮换所有已提交/可能已记录的密钥。
2. 修复 SEC-001：Webhook 认证、路径根目录约束、读写删除统一安全封装。
3. 修复 SEC-003～SEC-006：管理路由默认鉴权、关闭可绕过的 pprof、设置全局/协议级资源上限。
4. 修复 SIP 与 AI 服务边界（SEC-007～SEC-009）。
5. 完成日志、供应链、CORS、认证生命周期和 HTTP 基线加固。
