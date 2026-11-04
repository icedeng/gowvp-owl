# 192.168.6.20 专用运行文件

用于 `192.168.6.20`（EasyTier：`10.144.0.20`），部署设计和实施记录见 [服务器部署方案](../server-192.168.6.20.md)。本目录不含真实凭据或业务数据库。

当前版本（2026-11-04）：Owl 服务镜像为 `v1-20261104-deviceui3`（设备列表操作列留白修正），后端版本保持 `v1-20261104-resourcecodes2`（补齐录像、告警、会话/任务/命令记录和订阅的编码定位），ZLM 保持 `v1-20261103-manualrecord1`，独立 PostgreSQL 17.11（`owl-v1-postgres:17-20261104`）。设备、通道、录像索引、心跳和注册历史已从 SQLite 迁移到 PostgreSQL；已补齐录像与通道复合索引，并移除时间轴/月统计的固定条数截断。默认手动录像、媒体 `30000-50000/tcp+udp`、无人观看默认 180 秒断流等行为保留。日常启动命令：

```bash
cd /www2/gowvp-v1
./run.sh up
systemctl enable --now owl-v1-backup.timer owl-v1-monitor.timer
```

## 目录和入口

- 服务器运行目录：`/www2/gowvp-v1`。
- 主配置：`/www2/gowvp-v1/data/configs/config.toml`，保留通用模板的中文注释。
- 首次管理员凭据及服务密钥：`/www2/gowvp-v1/.env`，仅 root 可读；网页修改管理员凭据后，`data/configs/admin-credentials.toml` 优先于初始环境变量，重启不会恢复旧密码。此文件权限为 `0600`，随每日备份保存。
- 数据库：Compose 服务 `postgres:5432/gowvp`，数据目录 `/www2/gowvp-v1/data/postgres`，容器内 `/var/lib/postgresql/data`。应用使用普通账号 `gowvp`，连接凭据由 `.env` 的 `OWL_DATABASE_DSN` 注入。旧 `data/configs/data.db` 保留作迁移前副本，不再写入。
- 数据库初始化脚本：`/www2/gowvp-v1/postgres-init.sh`，来源为仓库 `deploy/v1/postgres-init.sh`，仅空目录首次启动时执行。
- 录像、下载、事件图片、封面：`/www/gowvp-data/{recordings,downloads,events,cover}`。
- ZLM HLS/抓图缓存：`/www/gowvp-data/zlm-cache`，对应 `/opt/media/cache`；通过 ZLM 配置调整 HTTP 根目录和缓存路径，保留镜像自带的 `bin/www`。
- ZLM FFmpeg 日志：`/www2/gowvp-v1/data/zlm-fflogs`，由系统 logrotate 每日检查，100MB 阈值、保留 7 份。
- 管理页面：`http://192.168.6.20:15123/` 或 `http://10.144.0.20:15123/`。
- ZLM HTTP/API：`http://192.168.6.20:8080/` 或 `http://10.144.0.20:8080/`；Owl 通过 `zlm:host-gateway` 访问宿主机的 `8080`，API 保留密钥认证。

前端由 Owl 从 `/app/www` 提供，不增加 Nginx 容器。Compose 仅运行已经生成的 `linux/amd64` 镜像，不自动拉取或编译。

## 国标参数

| 参数 | 配置 |
| --- | --- |
| SIP 地址 | `192.168.6.20:15060`，TCP / UDP |
| 平台 ID | `50010700002000000001` |
| 国标域 | `5001070000` |
| 行政区划 | 重庆市九龙坡区，`500107` |
| 注册密码 | `.env` 的 `OWL_SIP_PASSWORD` |
| 媒体地址与端口 | `192.168.6.20`，`30000-50000/tcp+udp`；30000 为 WebRTC |

平台 ID 按指定区划编排，接入上级前确认编号分配和唯一性。不同设备必须使用各自的设备及通道编码，不共用平台 ID。SIP-TLS、附录 O 等可选能力按设备联调需要启用后再补充端口映射。

## 日常操作

```bash
cd /www2/gowvp-v1
./run.sh status
./run.sh up           # 核对磁盘与配置后启动或更新
./run.sh logs gowvp
./run.sh logs zlm
curl -fsS http://192.168.6.20:15123/health
```

修改 `config.toml` 或 `.env` 后执行 `./run.sh up`；仅修改绑定配置文件时，Compose 不会自动重启容器，需执行：

```bash
docker compose -p owl-v1 --env-file .env -f docker-compose.yml restart gowvp
```

更新镜像前完成备份，导入对应镜像，再修改 `.env` 标签，执行 `./run.sh up`。`OWL_SERVICE_IMAGE_TAG`、`OWL_ZLM_IMAGE_TAG` 优先于 `OWL_IMAGE_TAG`；当前分别为 `v1-20261104-deviceui3`、`v1-20261103-manualrecord1`。PostgreSQL 通过独立变量 `OWL_POSTGRES_IMAGE` 固定版本，升级数据库大版本需单独执行迁移，不能直接换标签。不要使用通用 `deploy.sh release/up` 启动本服务器，不要重新执行首次准备脚本。

此前 apibasefix1 仅更新前端，修复开发环境 `/api` 代理前缀混入生产构建、导致开放平台和系统状态等接口 404 的问题。`admin-ui/.env.production` 将生产默认 API 地址设为同源 `/`，保留本地开发配置；生产构建需检查编译后的实际 API base，再用管理员身份检查页面接口，不能只检查静态文件和容器健康。无后端或数据库修改，ZLM、PG 容器未重启。交付目录为 `packages/apibasefix1-20261104/`，更新前环境配置和运行信息位于 `backups/before-apibasefix1-20261104/`。紧急回滚可恢复最近正常的 `v1-20261104-openplatformdocs1` 并仅更新 gowvp；不要回退到带有错误代理前缀的 openplatformui1，不恢复数据库或更换密钥。

## 紧凑界面发布（2026-11-04）

14:59:54 发布 `owl-v1-service:v1-20261104-compactui1`，统一全局配色、按钮/表单密度、响应式与运维菜单折叠。基于现网 apibasefix1 镜像仅覆盖前端，并保留旧哈希资源，避免未刷新页面加载异步模块时 404。后端二进制、API 根地址 `/`、数据库和媒体配置保持不变，ZLM、PostgreSQL 容器未重启。

发布前在线数据库备份已通过 `pg_restore --list`；发布后 517 个静态文件与交付摘要一致，HTTP 获取的 17 个关键资源与本地产物一致，加密管理员登录及系统状态、媒体节点、资源列表、开放平台读取正常，未登录保护接口返回 401。4 台设备全部在线、128 个通道保留，启动日志未发现 ERROR/panic/fatal。页面视觉、触屏与视频业务由人工验收；刷新浏览器加载新界面。

交付目录为 `packages/compactui1-20261104/`，更新前备份位于 `backups/before-compactui1-20261104/`。镜像摘要为 `sha256:5d24eb9718799db0c6df4c6e166eaad40b5b7d16ed75a27f8a1b7f7be6e821f9`。如需回滚，将 `.env` 的 `OWL_SERVICE_IMAGE_TAG` 恢复为 `v1-20261104-apibasefix1`，只更新 gowvp，不恢复数据库或更换密钥：

```bash
cd /www2/gowvp-v1
docker compose -p owl-v1 --env-file .env -f docker-compose.yml up -d --no-deps --no-build --pull never --wait gowvp
```

## 三方应用开放平台

- 完整开发文档：`http://10.144.0.20:15123/web/#/open-platform/docs`，无需管理员登录，包含全部接口参数、返回值、权限与示例，以及签名、回调和客户端下载。公开元信息 `/open-platform/docs/config` 仅含 enabled/public_url/audience/allow_http，不公开凭据或设备数据。
- 应用详情独立打开为 `/web/#/open-platform/apps/{id}`；分区通过 `tab` 查询参数保留，返回列表恢复搜索条件与页码。
- 已启用，管理入口 `http://10.144.0.20:15123/web/#/open-platform`；API `/openapi/v1/`，受控媒体 `/openmedia/v1/`。
- 按用户要求保留 HTTP，`.env` 设置 `OWL_OPEN_PLATFORM_ALLOW_HTTP=true`、`OWL_OPEN_PLATFORM_PUBLIC_URL=http://10.144.0.20:15123`，Audience 为 `owl-192-168-6-20`。HTTP 开关同时允许 HTTP 回调和密钥的一次性响应；HMAC 仅签名，不加密传输。
- 独立主密钥文件 `data/configs/open-platform-keyring.json`，权限 `0600`，容器内 `/app/configs/open-platform-keyring.json`；每日受控备份已加入该文件。密钥丢失将无法恢复应用 SK，不得提交或输出密钥内容。
- 15 张 `open_*` 表已通过迁移工具建立，原业务表结构未变。当前无预置三方应用；从页面创建、授权设备和 Scope，再生成 AK/SK 并启用。
- 旧业务、媒体端口与来源放行保持现状，未建立只允许三方访问新入口的网络隔离；应用级媒体授权只覆盖新 `/openmedia/v1` 入口。完整接入与人工验收重点见 [开放平台说明](../../docs/open-platform/README.md)。

## 维护与访问控制

```bash
systemctl list-timers 'owl-v1-*'
systemctl start owl-v1-backup.service
journalctl -u owl-v1-backup.service -n 30 --no-pager
systemctl start owl-v1-monitor.service
journalctl -u owl-v1-monitor.service -n 30 --no-pager
```

启用 timer 后，每日 03:30 左右使用 PostgreSQL 容器内同版本 `pg_dump` 生成在线一致性备份（`gowvp.dump`），并执行 `pg_restore --list` 检查，连同配置、管理员凭据和开放平台主密钥环归档；保留约 7 天日备份、4 周周备份，位置为 `backups/daily`、`backups/weekly`。旧日期的备份仍可能包含 SQLite `configs/data.db`，恢复前先确认备份格式。归档含密钥，保持目录 `0700`、文件 `0600`，不得提交 Git；不含录像。定期异机备份的目标地址仍需另行配置。

心跳、成功注册事件写入 `device_history_records`，按设备、事件类型、时间建立联合索引；设备当前在线状态、最近心跳及注册有效期仍保存于 `devices`。沿用现有保留设置：每台设备每种事件最多 1000 条、最长 30 天，两者先到先清理，可在 SIP 设置的历史保留区修改。当前不引入 TimescaleDB 或分区，详见 [PostgreSQL 与时序历史方案](../postgresql.md)。

每小时检查磁盘与媒体目录预算，将超过 80% 的告警写入系统日志；清理超过 3 天的 `.video-cache/*.mp4`。ZLM HLS 当前为 3 个切片、额外保留 5 个切片、关闭流后延时清理；缓存目录预算 100GiB。以上不构成硬配额或外部通知。原始录像由应用按 3 天、磁盘 80% 阈值清理，默认分片 300 秒。

ZLM 使用宿主机网络，直接监听 `554/1935/8080` 和媒体端口，`30000-50000/tcp+udp` 不再经过 Docker 端口映射。`access.sh` 对 GoWVP 网桥 `br-owl-v1` 继续允许所有来源；宿主机 INPUT 同样须保持所有来源允许。已有业务占用的动态端口由系统绑定检查跳过，不停止其他服务。

GoWVP 保留现网桥接地址 `172.19.0.3`，网桥子网固定 `172.19.0.0/16`、网关 `172.19.0.1`，避免更新容器改变 IP 导致录像机已有 UDP DNAT 会话投递到旧地址。此固定网段只适用于本服务器，迁移时应重新核对。

录像默认关闭，通道详情“录像”页手动开启或停止。未开启手动录像的流在无人观看 180 秒后停止推流；管理端“SIP 设置 → 视频断流”可调整秒数，保存后对新一轮计时生效。配置写入 `Media.StreamNoneReaderDelaySeconds`。手动录像期间不自动断流，停止录像后重新计时。两个容器的录像路径统一为 `/app/configs/recordings`，确保 MP4 完成回调可入库。

2026-11-03 已用 BH1-1-1 验证手动录像约 205 秒、停止后 MP4 入库和下载，以及无人观看约 180 秒后发送 BYE、释放流和 RTP 接收端；验证过程中四台设备保持真实心跳在线。测试录像已停止，管理端需要刷新以加载新设置。

ZLM 持久化配置 `data/zlm-config/config.ini` 中，`[http]` 的 `allow_ip_range` 置空（允许任意 IP），`[general]` 的 `listen_ip=0.0.0.0`，`[rtc]` 的 `interfaces` 为空。迁移或重新初始化 ZLM 配置时须保留这些值；镜像默认的 HTTP 内网白名单需要清空。API 密钥、管理登录及推流认证不受影响。`rtc.externIP` 和 `Media.SDPIP` 是媒体宣告地址，并非来源白名单；当前仍宣告 `192.168.6.20`，播放端须有到该地址的路由。

`prepare-host.sh` 仅用于容器运行时为空的首次准备，已部署后会拒绝重复执行。它保留原 containerd 数据与配置，将新镜像存储放在 `/www2/containerd`，并增加挂载依赖；不要在已有容器运行后再次迁移。

## 运行总览浅色优化（2026-11-04 15:14:00）

发布 `owl-v1-service:v1-20261104-overviewlight1`，移除总览深色态势条和实蓝快捷入口标题，协议及媒体服务区统一白底；不足一小时运行时长显示分钟，顶栏、总览、系统状态和升级页共用。517 个静态文件校验一致，加密登录与总览相关只读接口正常，4 台设备在线、128 个通道保留；后端、ZLM、PostgreSQL 保持原样。页面效果由人工验收。

包目录 `packages/overviewlight1-20261104/`，备份 `backups/before-overviewlight1-20261104/`。镜像摘要 `sha256:dfea62d3159c31867890f15978bf443d12df1f4a61b6f390bb2acc1714049ef0`。回滚时恢复本轮备份 `.env`，仅重建 gowvp 至 `v1-20261104-compactui1`，不恢复数据库。

## 顶栏身份文案（2026-11-04 15:18:35）

发布 `owl-v1-service:v1-20261104-userlabel1`，顶栏显示“管理员”，账号移至用户菜单并标注“登录账号”。生产构建、517 个静态文件及 6 个 HTTP 关键资源校验通过，容器健康；后端和依赖容器保持原样。发布包 `packages/userlabel1-20261104/`，备份 `backups/before-userlabel1-20261104/`；回滚恢复本轮备份 .env 后仅重建 gowvp 至 overviewlight1，不恢复数据库。

## 录像中心检索与回放（2026-11-04 15:36:43）

发布 `owl-v1-service:v1-20261104-recordings2`，默认展示最新录像，完整月历标记有录像日期，支持通道/月/日检索及页内MP4回放、当天连续播放。浏览器已验证单片段正常播放和自动切至下一片段；518个静态文件、9个HTTP关键资源校验通过，后端与PG/ZLM容器保持原样。包目录 `packages/recordings2-20261104/`，备份 `backups/before-recordings2-20261104/`；回滚恢复本轮备份.env并仅重建gowvp至userlabel1，不恢复数据库。

## 录像弹窗播放器（2026-11-04 16:01:47）

发布 `owl-v1-service:v1-20261104-recordmodal1`，录像回放改为宽版弹窗，保留播放与连续队列；关闭停止播放，列表与筛选保持原位。正式构建及519个静态文件、9个HTTP关键资源校验通过，容器健康，后端/PG/ZLM保持原样。包目录 `packages/recordmodal1-20261104/`，备份 `backups/before-recordmodal1-20261104/`；回滚恢复本轮备份.env后仅重建gowvp至recordings2，不恢复数据库。

## 开放平台编码入口（2026-11-04 16:54:29）

发布 `v1-20261104-resourcecodes1`，三方可直接使用设备编码和完整通道编码查询设备/通道、查询云台能力、控制云台、创建播放会话和抓拍任务。路径统一为 `/openapi/v1/devices/by-code/{device_code}`，通道入口追加 `/channels/{channel_code}`；列表支持 `device_code`、`channel_code` 精确筛选。`device_id` 查询参数仍指内部设备 ID，旧入口保持兼容。完整文档已同步到在线文档页，现为 62 个接口操作。

编码解析始终限制在应用授权设备内，Scope 规则保持；不存在或无权访问统一 404，同设备重复通道编码返回 409，不任选控制目标。设备授权绑定内部 ID，同编码重建设备不会继承旧授权。

仅覆盖后端和开放平台文档，其他页面资源摘要一致，无数据库迁移；ZLM、PostgreSQL 未重启。线上签名只读查询和能力接口通过，4 台设备在线、128 个通道保留；未执行真实云台、抓拍或播放。发布包 `packages/resourcecodes1-20261104/`，备份 `backups/before-resourcecodes1-20261104/`。回滚将 `.env` 的 `OWL_SERVICE_IMAGE_TAG` 恢复为 `v1-20261104-recordmodal1`，仅更新 gowvp，不恢复数据库或更换密钥。

## 国标设备与通道工作区（2026-11-04）

发布 `v1-20261104-deviceui1`：设备列表去重复状态、重要字段前移，名称和箭头进入详情；设备详情默认通道资源，统一表头、全设备通道检索/状态筛选/分页，横向导航及浅色紧凑布局。通道详情预览优先，低频配置、语音和播放地址按需展开，窄屏重排并保留触屏44px目标。

34项相关检查和生产构建通过；519静态文件及10项HTTP资源匹配。线上只读核对4台设备/128路通道，目标设备32路、分页25+7、搜索及在线筛选正常。后端保持resourcecodes1，数据库和媒体容器未重启。包目录 `packages/deviceui1-20261104/`，备份 `backups/before-deviceui1-20261104/`；回滚恢复本轮备份.env后仅重建gowvp至resourcecodes1。未进行浏览器或真实设备操作验收。

## 编码入口完整覆盖（2026-11-04 17:13:17）

发布 `v1-20261104-resourcecodes2`，以现网 deviceui1 镜像仅更新后端与开放平台文档。设备编码路径和设备+完整通道编码路径均支持 GET `/recordings`、`/events`、`/live-sessions`、`/snapshot-tasks`、`/ptz-commands`，设备路径新增 GET `/channels`。原列表统一支持 `device_code`、`channel_code` 精确筛选；原播放/抓拍创建 JSON 可直接用这两个编码，订阅创建/修改支持 `channel_codes` 数组。文档优先展示编码入口，原设备/通道 ID 接口标记为兼容；共 76 个接口操作。

具体录像、事件、会话、任务、命令和订阅仍用各自结果 ID 获取详情、票据、停止或修改；同通道多条记录不能用编码替代结果 ID。先按编码查列表，再选择结果即可，无需设备或通道内部 ID。编码路径严格解析、列表过滤取交集，禁止路径和查询/请求体的冲突定位；Scope、设备授权、幂等与安全停止规则保持。

本地 API/目标 race 检查和构建通过，线上真实签名按编码检索到 3 条录像，其他记录列表及未知编码/冲突/401/404 检查通过，8 个文档文件摘要一致。非文档页面资源不变，4/4 设备在线、128 通道保留，ZLM/PG 未重启；未触发真实设备动作或修改线上订阅。包目录 `packages/resourcecodes2-20261104/`，备份 `backups/before-resourcecodes2-20261104/`。回滚将服务标签恢复 `v1-20261104-deviceui1`，仅更新 gowvp，无需恢复数据库或密钥。

## 设备详情布局修正（2026-11-04）

发布 `v1-20261104-deviceui2`：Tab与当前内容共享连续面板，桌面/窄屏内边距16/12px；通道名称和编码独立成列，查询框填满分配宽度。生产构建及519个静态文件、6项HTTP资源校验通过，后端保持resourcecodes2，数据库及媒体容器未重启。包目录 `packages/deviceui2-20261104/`，备份 `backups/before-deviceui2-20261104/`；回滚恢复本轮备份.env后仅重建gowvp至resourcecodes2。页面效果由人工验收。

## 设备列表操作列留白（2026-11-04）

发布 `v1-20261104-deviceui3`：操作列固定104px，右侧留白16px、按钮间距8px，桌面触屏列128px，避免图标溢出。构建、519静态文件和6项HTTP资源检查通过，后端/PG/ZLM保持。包目录 `packages/deviceui3-20261104/`，备份 `backups/before-deviceui3-20261104/`；回滚恢复本轮备份.env后仅重建gowvp至deviceui2。页面效果由人工验收。
