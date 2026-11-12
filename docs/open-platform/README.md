# Owl 三方应用开放平台

首期与 Owl 同进程，独立认证与表空间，**仅支持单实例**。已提供设备/通道、实时播放、设备录像目录与回放、平台落盘录像、事件/图片、独立抓拍任务、云台控制、应用告警订阅和管理页面。真实设备、播放器与生产网络效果由人工验收。

## 在线开发文档

管理端“接入文档”打开独立公开页面 `/web/#/open-platform/docs`，开发者无需管理员账号即可阅读。包含全部业务、媒体和管理员接口的字段、权限、请求/响应示例，签名、回调、错误码及标准库客户端下载；管理员接口仍要求严格 JWT。公开元信息 `/open-platform/docs/config` 仅返回 enabled/public_url/audience/allow_http。

契约以 `openapi.yaml` 为准，指南为 `developer-guide.json`。修改后运行 `go run ./tools/openplatform-docs` 同步静态资源；生成工具核对真实路由和引用，前端构建检查生成文件未过期。文档页面不接收 AK/SK，不提供设备动作在线调试。

## 部署与迁移

1. 在维护窗口备份原数据库；保留开放平台密钥环的独立受控备份。不要把密钥环加入 Git、镜像或普通配置备份。
2. 对已初始化的同一 Owl 数据库执行：

   ```sh
   # SQLite 必须绝对路径；PostgreSQL 使用 postgres:// URL。
   # 使用环境注入 DSN，不要把有密码的连接串写进脚本或命令历史。
   go run ./tools/openplatform-migrate
   ```

   工具从 `OWL_DATABASE_DSN` 读取目标，仅新增 `open_*` 表和索引，不改设备/录像/事件表。MySQL 支持项目原有的 `mysql://user:password@host:3306/db?sslmode=disable` URL，以及 `mysql:user:password@tcp(host:3306)/db?parseTime=true` 驱动格式；服务与迁移工具统一解析，并启用日期时间读取。MySQL DDL 不能保证整批回滚，失败时保持模块关闭，修复后重新执行。不要连接空库替代现有数据库。
3. 按 [环境配置示例](./environment.env.example) 注入独立主密钥和固定 Audience、Public URL（默认 HTTPS；HTTP 需显式设置 `OWL_OPEN_PLATFORM_ALLOW_HTTP=true`），再设置 `OWL_OPEN_PLATFORM_ENABLED=true` 重启 Owl。不自动迁移，也不自动生成替代主密钥。未启用不需要密钥、不启动 worker；启用配置/表不完整时新入口返回 503，旧业务仍启动。
4. HTTPS 三方网关仅放行 `/openapi/v1/` 和 `/openmedia/v1/`，详见 [Nginx 示例](./nginx.conf.example)。管理员从管理网络访问 `/web/#/open-platform`；管理 API `/admin/open-platform/*` 只接受当前管理员 Bearer JWT，不回退到 AuthURL，不接受 query token。
5. 禁止三方直连旧业务 API、`/proxy/sms/`、流媒体 HTTP/API/直连播放端口及内部 Webhook。按设备用途保留 SIP、RTP、RTMP 入站规则，避免影响设备接入。未完成此网络隔离不能宣称媒体资源授权闭环。

当前服务器的 `OWL_OPEN_PLATFORM_PUBLIC_URL` 为 `http://192.168.6.20:15123`，配置 `OWL_OPEN_PLATFORM_ALLOW_HTTP=true`；该开关同时允许 HTTP 回调和密钥一次性响应，HMAC 不加密传输内容。HTTPS 网关示例可留作后续切换。现网旧业务与媒体端口保持原配置，尚未建立仅开放新入口的网络隔离，不能宣称旧播放入口已受应用资源授权保护。

同步返回的播放/回放票据、抓拍图片及 HLS 播放列表内资源地址跟随当前请求的协议、Host 和端口；通过 `192.168.6.20:15123` 或 `10.144.0.20:15123` 请求即返回对应入口。仅当直接连接方属于 `OWL_OPEN_PLATFORM_TRUSTED_PROXIES` 时读取单值 `X-Forwarded-Host` / `X-Forwarded-Proto`；代理须覆盖这些头并保留外部端口，不支持通过转发头声明路径前缀。非法 Host 回退到配置地址。异步告警回调无当前请求，图片地址继续使用 `OWL_OPEN_PLATFORM_PUBLIC_URL`；公开元信息的 `public_url` 仍表示此固定配置。调用方必须使用浏览器可达的 OWL 入口，不能使用仅后端可达的容器内部地址。

所有前端页面与管理 API 默认同源。开放业务 API 面向服务端，不提供浏览器跨域签名调用；媒体入口允许不带 cookie 的跨域 GET。开发管理页面可使用 Vite 代理。

首期内存 QPS/出口带宽与活动连接控制是单实例额度，重启允许短时突发恢复。防重放、幂等、资源配额与租约持久化。不允许多个实例共同使用这套开放表运行 worker；多实例需另加 owner/lease、共享限流、撤销广播与密钥创建行锁。

## 创建应用与权限

管理端“开放平台”提供应用创建/编辑、启停/归档、接口权限、云台动作白名单、设备授权、来源 IP/额度、AK/SK、回调目标审核、订阅/投递/抓拍/云台记录与审计。

- 应用默认停用；Scope 默认不选；设备默认指定模式、没有已选项，保存前必须勾选设备或明确选择全部。来源策略空白拒绝所有来源；不限来源须显式勾选。
- `all` 自动包含未来新增设备；`selected_devices` 使用内部设备 ID，自动包含这些设备的新通道，新设备需重新授权。跨页勾选不会丢失；勾选当前页不代表全部模式。已删除设备显示失效，不继承到同名新设备。
- Scope 与设备授权同时检查。轮换 AK/SK 不改变设备权限。任何凭据只能操作所属应用的任务/会话。
- 完成权限、来源、有效凭据和配额后启用。最多两组有效 AK/SK；新建凭据可设到期时间，旧组切换后撤销，建议重叠不超过 24 小时。SK 仅一次显示，创建/轮换密钥接口默认要求 TLS 或受信代理覆盖的 X-Forwarded-Proto=https；HTTP 部署需显式启用 ALLOW_HTTP，关闭弹窗清除内存；响应丢失需撤销未知组后重建。
- 编辑使用 revision 乐观锁。首期采用保守撤权：应用任何编辑会使旧媒体会话与票据失效，暂停订阅；接入方应重新创建会话并确认订阅恢复。停用/到期/凭据撤销禁止新业务，活动命令进入安全停止。
- 停用后可归档；归档同时撤销凭据并保留记录，无物理删除。

## 业务调用

所有需要定位设备或通道的业务均支持完整编码，无须先获取内部 ID。`device_code` 对应设备 `device_id`，`channel_code` 对应设备上报完整通道编码（非数字序号）。编码入口统一为 `/openapi/v1/devices/by-code/{device_code}`，通道入口追加 `/channels/{channel_code}`：

| 用途 | 设备或通道编码路径后缀 | 说明 |
| --- | --- | --- |
| 列出设备通道 | 设备路径 `/channels` | GET，通道详情使用完整通道路径 |
| 录像、告警查询 | `/recordings`、`/events` | GET，设备级或通道级均支持 |
| 播放会话、抓拍任务、云台命令记录 | `/live-sessions`、`/snapshot-tasks`、`/ptz-commands` | GET，仅本应用记录；设备级或通道级均支持 |
| 云台能力和控制 | 通道路径 `/ptz/capabilities`、`/ptz` | GET 能力、POST 控制 |
| 创建播放和抓拍 | 通道路径 `/live-sessions`、`/snapshot-tasks` | POST，播放 body 为 `{"protocol":"hls"}`，抓拍为 `{"mode":"sync"}` 或 `{"mode":"async"}`（省略兼容异步） |

原通道、录像、告警、会话、任务、命令列表也统一支持 `device_code`、`channel_code` 精确筛选；所有条件取交集，仅传 `channel_code` 匹配授权设备下的同码通道。列表未知编码返回空列表，严格编码路径返回 404；编码路径不接受额外资源筛选参数。会话/任务/命令列表支持 `state`，记录按 `start/end` 默认最近24小时、最大7天，使用 `size/cursor` 分页。

原 `POST /live-sessions`、`/snapshot-tasks` 的 JSON 也可用 `device_code` 和 `channel_code` 定位，与内部 `channel_id` 互斥。订阅创建/修改支持 `channel_codes:[{"device_code":"设备编码","channel_code":"完整通道编码"}]`，与旧 `channel_ids` 互斥；空数组表示全部授权资源，null 不合法。编码保存后仍绑定内部通道，不因同码重建设备而自动扩大范围。

旧内部 ID 入口和参数语义保持。录像、事件、会话、任务、命令、订阅的结果 ID 仍用于具体结果的详情、票据、停止/释放和修改，因为同一通道可能有多条结果；先按编码查列表，再用目标记录的 id。编码入口只需对应业务 Scope，不额外要求设备/通道读取权限，继续检查设备授权、来源和额度。在线文档优先展示编码入口，并将原设备/通道 ID 入口标注为兼容。

签名见 [signing-v1.md](./signing-v1.md)，机器契约见 [openapi.yaml](./openapi.yaml)。JSON 响应统一为 `{code,message,request_id,data}`；鉴权 401、无 Scope/IP 403、资源不存在或未授权 404、状态/幂等冲突 409、过载 429、依赖错误 503。返回专用字段，不返回设备口令、节点内网地址、录像/图片文件路径、原始配置。

设备、通道、录像、事件列表使用 `size`（1—100）和 `cursor`，返回 `items/next_cursor`。录像/事件默认最近 24 小时，`start/end` 为 RFC3339，最大 7 天；时间轴由录像返回的 started_at/ended_at 分段绘制。订阅与其投递列表使用同样的游标分页；管理记录使用 `page/size`、`items/total`。

1. 直接使用设备完整编码与完整通道编码；也可按编码查询设备/通道详情。旧客户端仍可使用列表返回的内部 id。
2. `POST /live-sessions`，body `{device_code,channel_code,protocol:"hls"}` 或 `"flv"`（也兼容内部 channel_id），带 Idempotency-Key；返回会话 id/状态，源尚未就绪返回失败状态；就绪后另 `POST /live-sessions/{id}/tickets` 申请媒体 URL。会话最长 30 分钟、票据 5 分钟，换票不延长会话。
3. `DELETE /live-sessions/{id}` 只释放自身会话，不关闭共享源，不影响管理员观看、录像、级联或其他应用。最后引用释放后仍由原宿主无人观看策略回收源。
4. HLS 清单、init、切片、密钥全部经 `/openmedia/v1/*`；客户端在票据到期前经业务后端换票并重载清单。FLV/下载连接每秒检查当前授权，撤权时取消上游并中断写入；已缓冲或已下载的数据无法追回。
5. 录像 `/recordings/{id}/tickets` 的 purpose 为 play/download，下载需独立 `recording:download`；媒体读取仅允许单段 Range。告警图片使用 `/events/{id}/image-tickets`。
6. `POST /snapshot-tasks` body `{device_code,channel_code,mode:"sync",timeout_seconds:60}`（兼容内部 channel_id），带幂等键。同步等成功返回图片地址；异步 `mode:"async"` 或省略立即返回任务和地址。两种均自动返回 `image_url`、`image_expires_at`；异步未完成访问图片地址返回 409 `SNAPSHOT_PENDING` 和 `Retry-After:1`，成功后同一地址可取图。同步等待 1—120 秒，超时 504 `SNAPSHOT_TIMEOUT`，任务继续；同幂等键及原 body 重试不会重复抓拍。任务详情/列表自动刷新约 5 分钟有效的地址；失败、未知、取消或到期任务无图片地址。底层复用通道持久化抓拍，按原协议选择设备上传或 FFmpeg，保留宿主上传超时及并发控制。临时三方任务与结果约24小时到期，通道持久化图片独立保留。
7. 图片记录：`GET /captures` 或编码设备/通道路径 `/captures`，返回与管理端通道抓图页相同的成功图片记录，含手动、定时和三方抓拍。顶层列表支持 `device_code/channel_code` 或兼容内部ID，`size/cursor`分页；默认全部历史，显式 `start/end` 时最大7天。`GET /captures/{capture}` 查询详情/刷新图片地址，`POST /captures/{capture}/image-tickets` 独立换票。权限沿用 `snapshot:capture` 与应用设备授权，本期只读；管理员重命名后按ID解析最新路径，图片删除或设备撤权后拒绝地址访问。图片记录ID与三方任务ID不同；永久图片不因三方任务到期删除。
8. 云台先读编码通道路径 `/ptz/capabilities`；`POST` 同一路径 `/ptz` body `{action:"left",speed:30,duration:1}`，带幂等键。连续动作 1—3 秒；后端独立下发停止，查 command 状态；协议确认不代表实际到位。停止自身命令走 `/ptz-commands/{id}/stop`，管理员可从记录执行安全停止。停止失败保留通道租约，阻止后续控制；旧定时器只停止自己的代次。GB 聚焦/光圈、巡航/扫描采用对应停止码；预置位写入等须显式授权。ONVIF 首期仅声明方向、变倍、预置位，聚焦缺少成像停止链路，光圈未声明；不支持的动作拒绝。RTSP/RTMP 无云台能力。

媒体入口首期支持 **ZLM 的 HLS fMP4 / HTTP-FLV** 和平台本地落盘录像/图片；设备侧录像目录查询支持 GB28181；设备侧录像下载、RTSP/RTMP 直连、WS-FLV、WebRTC 尚未开放。抓拍复用当前通道的设备上传/实时流 FFmpeg 能力，不再另走 ZLM getSnap。

## 设备本地录像目录查询

平台 `/recordings` 只包含平台落盘录像。设备 SD 卡/NVR 录像目录使用：

```http
POST /openapi/v1/devices/by-code/{device_code}/channels/{channel_code}/records/query
Content-Type: application/json
```

```json
{"start_at":1710864000,"end_at":1710950400,"timeout":10,"type":"all"}
```

- AK/SK 签名，沿用 `recording:read` 和设备授权；只读 POST 无需 `Idempotency-Key`。兼容 `POST /openapi/v1/channels/{id}/records/query`，id为内部通道ID。
- `start_at/end_at` 必填 Unix 秒，结束大于开始，跨度最多7天；`timeout` 默认10秒、1—60秒，信令另预留最多5秒；`type` 可选 `time/alarm/manual/all`，默认全部。
- 仅支持在线且支持 RecordInfo 的 GB28181 设备，每应用最多2个并发查询。不创建平台录像、播放会话或下载任务。
- `data` 返回 `daynum/timenum/list:[{date,items:[{start,end}]}]/complete`。按平台查询时区分组，跨日切分，片段为Unix秒。完整无录像返回 `complete=true,list=[]`。
- 部分分包返回200、`complete=false` 和 `incomplete:{received_count,expected_count}`；零应答504 `DEVICE_RECORD_TIMEOUT`，不可当作无录像。离线409 `DEVICE_OFFLINE`，不支持协议400 `UNSUPPORTED_RECORD_QUERY`，其他设备错误503 `DEVICE_RECORD_QUERY_FAILED`。
- 返回前再次检查当前授权。结果不含设备文件路径、原始XML或播放地址，也不是 `/recordings/{id}/tickets` 所需的平台录像ID；设备录像回放通过 `/playback-sessions` 按时间段申请，设备录像下载尚未开放。

## 应用告警订阅

1. 管理员为应用配置独立回调目标并审核（默认 HTTPS，ALLOW_HTTP=true 时也可使用 HTTP）。批准时检查 DNS 结果，连接时重新解析、全部校验并固定连接 IP；不跟重定向。默认仅公网，内网目标须明确批准 CIDR。始终拒绝回环、链路本地、常见云元数据地址、宿主网卡地址及 `OWL_OPEN_PLATFORM_CALLBACK_DENY_CIDRS` 禁止网段。该配置必须覆盖管理网段、数据库/媒体节点和集群控制面；新增管理网络后同步更新配置并重启。
2. 应用 `POST /subscriptions`，body 包括 callback_target_id、可选 channel_codes（兼容 channel_ids）/labels/expires_at，带幂等键；空 channel_ids 表示当前应用全部可见资源，**不扩大应用设备范围**。初始 paused。
3. 应用 `POST /subscriptions/{id}/signing-keys`，接收一次性独立回调 SK/key_id。与 API SK、主密钥完全独立；轮换后旧 key_id 最多保留 24 小时用于在途校验，新投递立即用最新 key_id。
4. `PATCH /subscriptions/{id}` body `{revision,status:"active"}` 启用；可暂停，DELETE 取消。应用密钥轮换后使用新 AK PATCH 绑定新凭据再撤销旧 AK。撤销旧 AK 会暂停仍绑定旧组的订阅。
5. events 与 outbox 同事务；worker 按订阅当前授权筛选，创建 `(subscription,event)` 唯一 delivery。每次投递、重试、人工重投都重新检查应用、签发凭据、订阅、目标、事件设备授权；投递中尽力取消撤权请求。
6. 至少一次语义。接收方按 [回调签名规范](./signing-v1.md#回调签名) 验签并持久化防重放，按 delivery_id 幂等落库再返回 2xx。delivery_id 重试保持不变，nonce/timestamp 每次更新。签名无效不能返回成功；重复已处理 delivery 返回 2xx。图片只在具备 event:image 时临时签发 URL，票据不写入 outbox。
7. 2xx 成功；重定向/其他 4xx（408/429 除外）failed；网络/超时/5xx/408/429 退避重试最多 8 次后 dead。并发全局 8、每应用默认 2；云台停止在独立循环运行。失败/死信允许管理员或应用受控重投并审计。投递记录和已完成 outbox 默认 30 天清理；临时三方抓拍文件 24 小时、通道图片记录永久保留（管理员可删除）、审计 90 天；nonce/票据/幂等记录分批清理。

## 运维与人工检查重点

- 主密钥轮换：停服维护，加载新旧密钥环，切换当前 KEY_ID，执行 `go run ./tools/openplatform-migrate -rewrap`；全部解密/重加密/核对后事务提交。恢复启动确认后才能移除旧主密钥。备份恢复必须同时恢复匹配密钥环；丢失无法找回 SK。
- 回滚：停止接入、正常停止 Owl（触发控制收敛），将 enabled 置 false 后回滚程序，保留所有新表与密钥环，不删表。不强杀正在控制的进程；发生异常重启，租约核对完成前该通道拒绝新移动。
- 关注日志中的审计写入失败计数、云台停止失败、配置/迁移失败；不得把应用请求/响应体、完整 URL query 或 Authorization 写入网关、APM 或调试日志。新增入口已从旧请求/响应体日志中排除。
- 人工重点：真实设备抓拍与最长控制/断连/停止效果；指定应用对其他设备及历史图片/录像无法访问；网关隔离、HLS换票/FLV撤权、接收方验签与幂等重试。


## 三方设备录像回放

- 申请：`POST /openapi/v1/devices/by-code/{device_code}/channels/{channel_code}/playback-sessions`，body为 `{"start_at":1700000000,"end_at":1700000060,"protocol":"hls","record_type":0}`。也可用 `POST /openapi/v1/playback-sessions` 在body传 `device_code`+`channel_code`（或兼容内部 `channel_id`，不能混用）。需要 `recording:play`、设备授权和 `Idempotency-Key`。仅在线GB28181设备、ZLM；Unix秒时间范围最大7天。
- 轮询：`GET /openapi/v1/playback-sessions/{session}`，创建时 `preparing`，最长约35秒准备，成功 `ready`，失败 `failed`（固定 `PLAYBACK_START_FAILED`）。同幂等键同请求返回原会话，不重复起流。
- 播放：`POST /openapi/v1/playback-sessions/{session}/tickets`，body `{}`，返回 `url`、`expires_at`。支持HLS fMP4和HTTP-FLV，均经独立 `playback` 票据代理。会话最长30分钟、票据约5分钟；换票不续会话。
- 控制：`POST /openapi/v1/playback-sessions/{session}/control`，需幂等键。`{"action":"pause"}`、`{"action":"play"}`、`{"action":"speed","scale":2}`、`{"action":"seek","seek_at":1700000030}`。倍速支持0.25/0.5/1/2/4，跳转在会话时间范围内；seek/speed下发PLAY并恢复播放。设备协议/固件决定支持度，未确认返回503 `PLAYBACK_CONTROL_FAILED`。HLS缓冲会延迟效果，可重载清单。
- 列表：`GET /openapi/v1/playback-sessions`，支持编码、state、size/cursor和start/end筛选；设备及通道编码路径同样支持 `/playback-sessions` 列表。详情和控制使用会话结果ID，只能操作本应用资源。
- 停止：`DELETE /openapi/v1/playback-sessions/{session}`，重复停止幂等，不影响管理员或其他应用的会话。503 `PLAYBACK_STOP_PENDING`表示票据访问已禁用，设备/媒体回收后台退避重试；详情中终态 `stop_pending=true`表示待回收，false表示完成。活动会话的stop_pending本来就是true。终态待回收仍占用会话配额，避免媒体故障时无限起流。

每个回放使用独立UUID、SIP会话键、SSRC和ZLM流。应用/凭据撤销、授权变化、到期、失流或正常关闭服务时失效并回收；异常重启后旧会话不会恢复，按持久化的原媒体节点关闭独立RTP接收端。重启丢失SIP对话后无法补发原BYE，设备侧资源仍依赖设备的超时释放。首期仍仅支持单实例。

本能力新增 `open_playback_sessions` 表。已有开放平台升级须先在维护窗口备份并运行 `go run ./tools/openplatform-migrate`（使用现有库的 `OWL_DATABASE_DSN`），再启动新版；不执行迁移会使开放平台入口返回503。普通无迁移升级通道不能直接用于本版。迁移只增开放表，原设备和录像数据不变；回滚程序后保留新增表，避免删除回放记录影响排查。完整参数、返回字段、状态及错误说明已同步到公开在线开发文档。
