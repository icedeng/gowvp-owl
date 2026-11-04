# Owl v1 真机测试部署

## 发布模式

`deploy/v1` 采用“本机编译产物 → 本机生成 Docker 镜像 → Docker Compose 启动”的模式。

脚本不会导出 `.tar` 镜像包，也不会推送到 Docker Hub 或私有镜像仓库。发布时使用当前工作树生成以下三个本地镜像：

| Compose 服务 | 本地镜像 | 内容 |
| --- | --- | --- |
| `gowvp` | `owl-v1-service:<OWL_IMAGE_TAG>` | Alpine、FFmpeg 和编译后的 Owl 服务 |
| `zlm` | `owl-v1-zlm:<OWL_IMAGE_TAG>` | 固定补丁版本的 ZLMediaKit |
| `postgres` | `owl-v1-postgres:<OWL_IMAGE_TAG>` | 固定摘要的 PostgreSQL 17，独立持久化数据库 |

前端会和 Owl 服务一起打包到 `gowvp` 镜像的 `/app/www`，由 Owl 自身提供静态资源。启动时设置了 `pull_policy: never`，只使用本地镜像，不会自动拉取同名远程镜像。

运行环境使用 Linux Docker Engine。ZLM 使用宿主机网络直接监听媒体端口，避免为 `30000–50000/tcp+udp` 创建数万条 Docker 映射；Owl 仍使用桥接网络，通过 `zlm:host-gateway` 访问宿主机 ZLM，ZLM 通过宿主机 LAN 地址回调 Owl。已有部署升级时需同时更新 Compose、ZLM 入口脚本和运行配置。

已有 SIP UDP 设备的部署应保持 Owl 容器原 IP，避免已有 UDP DNAT 会话仍指向旧地址。服务器专用编排已固定原有子网及 Owl IP；其他服务器应先核对自身网段，再设置独立地址，不能直接复制本服务器的固定地址。

录像默认关闭（`Server.Recording.DefaultMode='none'`、`Disabled=false`）。在通道详情的“录像”页手动开启或停止，开启后持续录到手动停止，关闭页面不会停止。未开启录像的视频在最后一名观看者退出后等待 180 秒自动断流，释放 RTP 端口并通知设备停止推流；手动录像期间保留推流。管理端“SIP 设置 → 视频断流”可配置 1–86400 秒，保存并持久化到 `Media.StreamNoneReaderDelaySeconds`，无需重启；已启动的倒计时按原值结束，新一轮计时采用新值。

管理员首次登录使用 `.env` 中的凭据。在“账号安全”页面修改时需要当前密码，新密码至少 8 个字符；提交复用登录的 RSA-OAEP 加密。网页保存后的凭据位于 `data/configs/admin-credentials.toml`（权限 `0600`），优先于初始环境变量，重启后仍生效。备份和恢复时须包含该文件；需要回退到环境变量凭据时，应先备份并移走此文件，再重启服务。

已有 PostgreSQL 部署升级录像查询版本前，先在目标库执行 [并发建索引脚本](sql/20261104-recording-query-indexes.sql)，创建录像 `(cid, started_at DESC)` 和通道 `(did, created_at DESC)` 索引，检查两条索引均有效后再启动新版。执行方法及失败处理见 [PostgreSQL 说明](../postgresql.md#录像查询索引升级)；不要把该脚本放入事务。新建数据库由 AutoMigrate 创建对应索引。

## 首次发布

```bash
# 生成 .env，并填写真机可访问的宿主机局域网 IP
./deploy/v1/deploy.sh init 192.168.1.100

# 编辑 WVP 配置
vim deploy/v1/data/configs/config.toml

# 编译、构建三个本地镜像并启动 Compose
./deploy/v1/deploy.sh release
```

`192.168.1.100` 必须替换为摄像机或上级平台能够访问的宿主机局域网地址。

发布前至少确认以下 WVP 配置：

```toml
[Sip]
Host = '宿主机局域网IP'
Port = 15060
ID = '本平台20位国标ID'
Domain = '本平台10位国标域'
Password = '设备注册密码'
```

同时确认 `deploy/v1/.env` 中的：

```env
OWL_MEDIA_SDP_IP=宿主机局域网IP
OWL_MEDIA_SECRET=媒体服务共享密钥
```

## 编译与构建流程

正式版本统一为 `v主版本.次版本.修订号`（例如 `v2.1.1`），默认读取仓库根目录 `VERSION`，可通过 `OWL_RELEASE_VERSION` 显式覆盖。构建时服务版本与镜像标签一致，不再使用日期加功能名；Git 分支、提交哈希和构建时间仍独立写入后端。后续兼容修复递增修订号，兼容功能递增次版本，不兼容调整递增主版本。

`build` 只生成镜像，不改写已有 `.env`；分步执行 `build` 和 `up` 时，需将 `.env` 中的 `OWL_IMAGE_TAG` 更新为本次正式版本。`release` 在同一进程中直接使用本次版本启动。服务器专用部署按 [运行说明](../192.168.6.20/README.md) 仅替换 GoWVP 镜像。

执行 `./deploy/v1/deploy.sh build` 时，脚本依次执行：

1. 自动识别 Docker Engine 架构，支持 `amd64` 和 `arm64`；
2. 通过 Go 交叉编译 Linux Owl 服务，并写入版本、分支、提交哈希和构建时间；
3. 使用 `pnpm install --frozen-lockfile` 安装前端依赖，并以 `VITE_API_BASE_URL=/` 构建管理端，避免本机开发配置的 `/api` 代理前缀进入发布包；
4. 把产物写入 `build/deploy-v1/<架构>/`；
5. 通过 Compose 生成 `gowvp`、`zlm`、`postgres` 三个本地镜像，前端静态资源已经复制到 `gowvp` 镜像的 `/app/www`。

产物目录示例：

```text
build/deploy-v1/arm64/
├── service/owl
└── frontend/
    ├── index.html
    └── assets/
```

其中 ZLMediaKit 使用仓库现有的 `Dockerfile_zlm`，固定提交为：

```text
2fa539807370c4bcfca9801bde1dd2fdd95ba113
```

并应用仓库内的 RTP/RTCP 补丁。

## 启动关系

Compose 服务依赖关系如下：

```text
zlm 和 postgres 健康检查通过
            ↓
    gowvp 健康检查通过
```

Owl 服务同时提供 API、WebSocket 和 `/app/www` 下的前端静态资源：

```text
浏览器 :15123
      ↓
gowvp:15123
```

## 端口映射

| 端口 | 用途 |
| --- | --- |
| `15123/tcp` | 管理端前端入口 |
| `15060/tcp+udp` | SIP |
| `15061/tcp` | SIP-TLS |
| `3702/udp` | ONVIF WS-Discovery |
| `1935/tcp` | RTMP |
| `554/tcp` | RTSP |
| `8080/tcp` | ZLMediaKit HTTP/API |
| `30000/tcp+udp` | WebRTC，ZLM 在宿主机直接监听 |
| `30001-50000/tcp+udp` | GB/T 28181 RTP/RTCP 动态端口池，按需绑定 |
| `50200-50300/tcp` | 2014 附录 O 裸 TCP 下载/级联中继，移出媒体端口池避免冲突 |

## 配置与持久化数据

部署目录下的持久化数据位于 `deploy/v1/data/`：

- `configs/config.toml`：WVP 主配置，包含 SIP、GB/T 28181、录像、数据库等设置；
- `postgres/`：PostgreSQL 数据目录；旧 `configs/data.db` 升级后保留作迁移前副本，不再写入；
- `configs/certs/`：SIP-TLS、证书 REGISTER 等证书文件；
- `logs/`：Owl 日志；
- `recordings/`：Owl 与 ZLMediaKit 共享的录像目录；
- `zlm-config/`：ZLMediaKit 运行配置。

两个容器将录像挂载到相同绝对路径 `/app/configs/recordings`，`StorageDir` 同样使用此路径，保证录像完成回调可校验、入库并回放。默认不自动补录升级前未入库的历史文件。

修改 `config.toml` 后重启 Owl：

```bash
docker compose \
  --project-directory deploy/v1 \
  --env-file deploy/v1/.env \
  --file deploy/v1/docker-compose.yml \
  restart gowvp
```

`.env` 中的密钥和 `OWL_MEDIA_SDP_IP` 由 Compose 注入，优先级高于配置文件中的同名媒体字段。数据库连接串从 `OWL_DATABASE_DSN` 注入，主配置只保留无密码示例。`init` 为 PostgreSQL 管理员与普通应用账号 `gowvp` 分别生成密码；首次空库通过 `postgres-init.sh` 创建应用角色，已有数据目录不会重新执行初始化脚本，修改 `.env` 不会自动修改已有角色密码。

PostgreSQL 在 Compose 网络提供 `postgres:5432`，不映射宿主机端口，不占用服务器其他业务已有的 5432。使用 `docker compose exec postgres psql -U postgres -d gowvp` 管理，数据库数据和配置须一并备份，不能复制运行中的 `postgres/` 目录作在线备份。心跳、成功注册历史仍存入按时间索引的独立历史表，当前状态保存在设备表，详见 [PostgreSQL 与时序历史方案](../postgresql.md)。

旧版 SQLite 升级必须先停服、备份一致性快照、迁移并核对，再启动新版本。只修改 DSN 不会迁移数据，不能直接对旧部署执行 `release` 代替迁移。仓库提供 `tools/db-migrate`，限定只读源快照、空目标库、事务导入和逐行校验，失败整批回滚。

发布脚本不会清理 `deploy/v1/data/`。同一数据库类型内回滚时修改 `.env` 中的 `OWL_IMAGE_TAG`，确保对应标签的三个本地镜像存在，再执行 `./deploy/v1/deploy.sh up`。跨数据库回滚需要恢复匹配的 Compose、连接配置和数据库备份，不能只切镜像；切换后新增的数据不会自动同步回旧 SQLite。

## 常用命令

```bash
# 只编译并构建本地镜像，不启动容器
./deploy/v1/deploy.sh build

# 启动已有本地镜像；镜像不存在时不会从远程拉取
./deploy/v1/deploy.sh up

# 编译、构建并启动
./deploy/v1/deploy.sh release

# 查看服务状态
./deploy/v1/deploy.sh status

# 查看全部日志或指定服务日志
./deploy/v1/deploy.sh logs
./deploy/v1/deploy.sh logs gowvp

# 停止并移除容器，保留 data/ 数据
./deploy/v1/deploy.sh down
```

## 架构与网络要求

脚本默认按 Docker Engine 架构生成 `amd64` 或 `arm64` 镜像。交叉构建可设置：

```bash
OWL_TARGET_ARCH=amd64 ./deploy/v1/deploy.sh release
```

交叉构建 ZLMediaKit 需要 Docker 支持对应平台模拟，构建时间会明显增加。

真机测试前请确认：

- 宿主机防火墙开放 SIP、RTP/RTCP、RTSP、RTMP 和附录 O TCP 端口；
- 设备能够访问 `OWL_MEDIA_SDP_IP`；
- `Sip.Host` 与 `OWL_MEDIA_SDP_IP` 使用真机可达的宿主机地址；
- Docker bridge 模式下 ONVIF 组播发现可能受网络环境限制，必要时可直接配置设备地址测试，不要把组播发现成功作为 GB/T 28181 信令验收的前置条件。
