# GoWVP 服务器部署方案：192.168.6.20 / 10.144.0.20

## 1. 目标与适用范围

在现有业务服务器上部署 GoWVP（Owl）和带国标补丁的 ZLMediaKit，开展 GB/T 28181-2011、2014、2016、2022 真机联调。

采用“构建机生成 Linux amd64 镜像 → 导出并传输 → 服务器导入 → Docker Compose 启动”的方式。前端随 Owl 镜像放在 `/app/www`，由 Owl 提供静态资源、API 和 WebSocket，不增加 Nginx 容器。

本文依据本任务中 **2026-10-28 的 SSH 检查**整理。容量、负载、端口占用和网络连通性属于检查时的快照，后续发布前需要复核。第 1—9 节描述部署设计，实施结果和与原计划的差异见第 10 节。

**当前状态（2026-11-04）：GoWVP、ZLM、独立 PostgreSQL 和维护定时任务运行中，数据库已从 SQLite 迁移。** 迁移与验证见第 10.14 节，历史清理范围纠正记录保留在第 10.6、10.7 节。

通用部署文件见 [v1 部署说明](v1/README.md)、[Compose](v1/docker-compose.yml)、[配置模板](v1/data/configs/config.toml)和[构建脚本](v1/deploy.sh)。本机专用运行文件及维护脚本见 [192.168.6.20](192.168.6.20/)，服务器使用专用 `run.sh` 启动。

## 2. 服务器现状

| 项目 | 检查结果 | 部署判断 |
| --- | --- | --- |
| 系统 | CentOS Stream 9，Linux 5.14，x86_64 | 交付 `linux/amd64` 镜像 |
| CPU | 双路 Xeon Silver 4310，共 48 线程 | 有运行余量；本次 ZLM 受限构建的例外见第 10 节 |
| 内存 | 62GiB，可用约 21GiB，Swap 已使用约 9.5GiB | 与已有业务共用，需要资源限制 |
| 物理网络 | `ens16f0`：`192.168.6.20/24`，千兆，MTU 1500 | 局域网设备默认接入地址 |
| 虚拟网络 | EasyTier `tun0`：`10.144.0.20/15`，MTU 1360 | 运维入口；媒体跨隧道需另行验证 |
| Docker | 29.8.0，Compose 5.5.1；检查时无镜像、容器 | 已具备容器运行环境 |
| 构建工具 | 未找到 Go、Node、pnpm | 服务器不执行 `build` 或 `release` |
| 镜像及依赖网络 | Docker Hub、默认 Go 代理直连探测超时；Docker 加速地址可响应 | 优先导入完整镜像，响应成功不代表下载已验证 |
| 时间同步 | Asia/Shanghai，NTP 已同步 | 满足联调基础条件 |
| 访问控制 | SELinux 关闭，firewalld 未运行 | 发布前核对网络 ACL 和 Docker 转发访问策略 |

服务器已有 Nginx、数据库、MinIO 和多个 Java 服务。本方案使用独立 Compose 项目 `owl-v1`，不修改已有应用配置，也不复用已有数据库账户或 MinIO 存储桶。

## 3. 运行结构与网络配置

```text
局域网摄像机 / 上级平台
        │ SIP、RTP/RTCP
        ▼
192.168.6.20（管理端桥接映射，ZLM 使用宿主机网络）
        │
        ├── gowvp:15123：前端 / API / WebSocket / 媒体 HTTP 代理
        ├── postgres:5432：独立业务数据库与设备时序历史（Compose 内部）
        └── zlm：收流 / 转发 / WebRTC / 录像
              │
              └── /www/gowvp-data/recordings（与 gowvp 共享）
```

`gowvp` 保留 Docker bridge，通过 `zlm:host-gateway` 访问宿主机 `8080` 的 ZLM API。ZLM 改用 `network_mode: host`，通过 `192.168.6.20:15123` 回调，媒体动态端口按需绑定，避免创建数万条 Docker 映射。

### 3.1 地址选择

默认假设摄像机、上级平台能通过物理局域网访问 `192.168.6.20`：

| 配置 | 值 | 作用 |
| --- | --- | --- |
| `Sip.Host` | `192.168.6.20` | SIP 对外宣告地址 |
| `.env` 中 `OWL_MEDIA_SDP_IP` | `192.168.6.20` | 媒体 SDP / WebRTC 宣告地址 |
| `Media.SDPIP` | `192.168.6.20` | 与环境变量保持一致，便于独立排障 |
| `OWL_MEDIA_IP` | `zlm` | Compose 内部媒体服务地址 |
| `OWL_MEDIA_HTTP_PORT` | `8080` | ZLM 宿主机 HTTP 端口 |
| `OWL_MEDIA_WEBHOOK_IP` | `192.168.6.20` | 由宿主机地址回调 Owl |

`10.144.0.20` 可作为 SSH 和管理页面入口，但不能仅因 SSH 可达就作为摄像机推流地址。只有设备、上级平台及播放端确实具有对应隧道路由时，才改为该地址。

通过 EasyTier 打开管理页面时，WebRTC 播放端还必须能到达宣告的媒体地址及端口。页面能打开不代表 WebRTC 媒体可达；HTTP-FLV / WebSocket-FLV 可优先通过 Owl 的媒体代理验证。物理网络设备与隧道客户端混用时，应先确认路由，再决定是否需要调整媒体节点宣告地址，不能假设一个地址天然覆盖两张网络。

### 3.2 端口与访问来源

检查时，当前 v1 Compose 所需 TCP/UDP 监听端口未发现冲突；该结论不包含设备侧、防火墙或隧道端到端连通性验证。

| 宿主机端口 | 用途 | 来源 / 启用条件 |
| --- | --- | --- |
| `15123/tcp` | 前端、API、WebSocket | 所有来源，绑定 `0.0.0.0` |
| `15060/tcp+udp` | 国标 SIP | 所有来源，绑定 `0.0.0.0` |
| `30000-50000/tcp+udp` | WebRTC 与国标 RTP/RTCP | 所有来源；30000 为 WebRTC，ZLM 使用宿主机网络 |
| `554/tcp` | RTSP | 所有来源，绑定 `0.0.0.0` |
| `1935/tcp` | RTMP | 所有来源，绑定 `0.0.0.0` |
| `8080/tcp` | ZLM HTTP/API | 所有来源，直接监听宿主机，API 保留密钥认证 |
| `15061/tcp` | SIP-TLS | 证书配置完成且启用 TLS 后放行 |
| `50200-50300/tcp` | 附录 O 级联裸 TCP 中继 | 启用对应功能后放行，避免与 RTP 池重叠 |
| `3702/udp` | ONVIF WS-Discovery | 按需；bridge 下组播发现需单独验证 |
| `8443/tcp`、`10000/tcp+udp`、`8000/udp`、`9000/udp` | 通用编排中的 TLS / 兼容映射 | 按实际生成的 ZLM 配置和功能需求决定是否保留 |

首版不随意改动媒体协议端口；未启用功能不额外开放访问来源。不要直接启用或清空整机防火墙规则；Docker 发布端口需要结合转发链 / 网络 ACL 控制，不能只检查宿主机 INPUT 链。

## 4. 磁盘现状与风险

| 挂载点 | 来源 | 文件系统 | 总容量 / 可用空间 | 规划 |
| --- | --- | --- | --- | --- |
| `/` | `cs-root` | XFS | 70GiB / 约 62GiB | 仅系统数据，不存放媒体和镜像归档 |
| `/www2` | `/dev/sda4` | ext4 | 498GiB / 约 360GiB | 程序、PostgreSQL、配置、日志、容器存储 |
| `/www` | `/dev/sdb2` | XFS | 41.4TiB / 约 39.8TiB | 录像、下载、图片 |

现有主要目录占用：MinIO 约 929GiB，`/www/mnt` 约 367GiB，`/www/mnt.rar` 约 80GiB；`/www2/wwwroot` 约 29GiB，GeoServer 约 28GiB，网站日志约 17GiB。目录统计存在限时扫描未完成的部分，整盘容量以 `df` 为准。上述数据归属已有业务，不作为本次清理对象。

实施时注意：

- `/www/server`、`/www/wwwroot`、`/www/wwwlogs`、`/www/backup` 均为指向 `/www2` 的链接，不能按路径前缀推断实际落盘位置。
- 根卷跨用了 `sda3` 和 `sdb1`，属于线性 LVM，卷组没有空闲空间。部署不重分区、不调整根卷，不将 `/www` 盘视为可以随意移除的数据盘。
- 两个逻辑盘位于同一 SmartPQI 控制器下；缺少阵列管理工具，尚未核实 RAID 级别、物理盘健康及冗余状态。近期内核日志未发现存储错误，不等于完成磁盘健康验收。
- `/www` 当前为 XFS `noquota`。本文容量预算不具备硬配额隔离；如需项目配额，应另行安排挂载配置调整和维护窗口。

## 5. 目录与挂载方案

### 5.1 宿主机目录

| 内容 | 宿主机路径 | 容器路径 |
| --- | --- | --- |
| Compose、启动脚本、`.env` | `/www2/gowvp-v1` | 不挂载 |
| 主配置、证书、旧 SQLite 副本 | `/www2/gowvp-v1/data/configs` | `/app/configs` |
| PostgreSQL 数据 | `/www2/gowvp-v1/data/postgres` | `/var/lib/postgresql/data` |
| Owl / SIP 日志 | `/www2/gowvp-v1/data/logs` | `/app/logs` |
| ZLM 配置 | `/www2/gowvp-v1/data/zlm-config` | `/opt/media/conf` |
| 录像及 `.video-cache` 回放缓存 | `/www/gowvp-data/recordings` | 两个容器统一为 `/app/configs/recordings` |
| 附录 O 下载及下载临时文件 | `/www/gowvp-data/downloads` | `/app/configs/downloads` |
| 事件图片 | `/www/gowvp-data/events` | `/app/configs/events` |
| 通道封面 | `/www/gowvp-data/cover` | `/app/configs/cover` |
| ZLM HLS / 抓图缓存 | `/www/gowvp-data/zlm-cache` | `/opt/media/cache` |
| ZLM FFmpeg 日志 | `/www2/gowvp-v1/data/zlm-fflogs` | `/opt/media/bin/fflogs` |
| 待导入镜像、校验文件 | `/www2/gowvp-v1/packages` | 不挂载 |
| 配置与数据库的本机恢复副本 | `/www2/gowvp-v1/backups` | 不挂载；另行复制到异机 |

录像使用两个容器一致的绝对路径，保证回调校验可读取同一文件。通过更具体的子目录挂载，把录像、下载和图片从 `/app/configs` 中分流到大盘；配置和 PostgreSQL 留在 `/www2`，旧 SQLite 仅保留作迁移前副本。

ZLM 首次生成 `data/zlm-config/config.ini` 后，将 `http.rootPath`、`protocol.hls_save_path` 设为 `/opt/media/cache`，`api.snapRoot` 设为 `/opt/media/cache/snap/`，默认 `protocol.mp4_save_path` 设为共享录像目录。保留镜像自带的 `/opt/media/bin/www`，不在其上覆盖挂载。HLS 保持 3 个切片、额外保留 5 个切片、关闭流后延时清理；FFmpeg 日志使用系统 logrotate。高并发前仍需检查容器可写层和缓存增长。

### 5.2 服务器专用 Compose 调整

交付时复制通用 Compose 为 `/www2/gowvp-v1/docker-compose.yml`，在该运行文件中完成以下调整：

1. 三个服务固定 `platform: linux/amd64`，分别锁定交付镜像，保留 `pull_policy: never`；删除 `build` 段，服务器只运行已有镜像。
2. 完整替换各服务的 `volumes`，按上表使用绝对路径；不能只追加 override，避免留下通用文件的旧挂载。首次启动前从仓库 `deploy/v1/postgres-init.sh` 复制到运行根目录，空数据目录初始化普通应用账号。
3. 所有绑定挂载使用长语法，并设置 `bind.create_host_path: false`，防止拼写错误或目录缺失时自动创建。
4. 保留 `gowvp → zlm、postgres` 的健康检查依赖与共享密钥；Owl 添加 `zlm:host-gateway`，使用 `8080`，ZLM 回调宿主机 `192.168.6.20:15123`。数据库仅在 Compose 网络使用 5432，不占用其他业务的宿主机 5432。
5. ZLM 使用宿主机网络，HTTP 监听 `0.0.0.0:8080`，所有来源允许，API 保留密钥认证。

绑定挂载示例，其他目录同样设置：

```yaml
# gowvp.volumes 中的一项
- type: bind
  source: /www/gowvp-data/recordings
  target: /app/configs/recordings
  bind:
    create_host_path: false
```

初始资源限制建议为 `gowvp`：`cpus: "2.0"`、`mem_limit: 2g`；`zlm`：`cpus: "4.0"`、`mem_limit: 4g`。这是联调起点，不是并发容量结论，需根据实际通道数、转码需求和 OOM / CPU 指标调整。

三个服务均增加控制台日志轮转：

```yaml
logging:
  driver: json-file
  options:
    max-size: "50m"
    max-file: "5"
```

### 5.3 防止系统盘被意外写满

- Docker 主目录已为 `/www2/docker`，但当前启用 containerd 镜像存储，containerd 的有效根目录仍为 `/var/lib/containerd`。导入镜像前，安排将其存储根迁至 `/www2/containerd`；同步核对服务配置、数据复制和重启顺序，不能只修改 Docker `data-root`。
- 每次启动前检查 `/www`、`/www2` 均为预期文件系统的挂载点，且所有数据目录存在。`create_host_path: false` 不能替代挂载检查，因为挂载丢失时，系统盘上可能仍有同名目录。
- 配合 `restart: unless-stopped`，还需为 Docker / containerd 的开机启动配置磁盘挂载依赖，例如 systemd 的 `RequiresMountsFor=`，避免自动恢复容器绕过手工检查。该项涉及整机容器服务，在维护窗口实施。
- 保持现有数据目录归属和最小访问权限；`.env`、主配置和私钥证书限制读取。不要递归修改 `/www`、`/www2` 或已有业务目录的权限。

## 6. 配置与保留策略

以通用 `config.toml` 为基础修改已有配置段，以下是相关字段摘录，不是可单独覆盖整份配置的文件：

```toml
[Sip]
Host = '192.168.6.20'
Port = 15060
# 按用户指定的重庆市九龙坡区（区划 500107）编排；上级接入前须确认编码无冲突。
ID = '50010700002000000001'
Domain = '5001070000'
# 设备注册密码与 OWL_SIP_PASSWORD 约定一致。

[Media]
IP = 'zlm'
HTTPPort = 8080
WebHookIP = '192.168.6.20'
SDPIP = '192.168.6.20'
RTPPortRange = '30000-50000'
StreamNoneReaderDelaySeconds = 180

[Server.Recording]
Disabled = false
DefaultMode = 'none'
StorageDir = '/app/configs/recordings'
RetainDays = 3
DiskUsageThreshold = 80.0
SegmentSeconds = 300

[Data.Database]
# 完整密码由 .env 的 OWL_DATABASE_DSN 注入。
Dsn = 'postgres://gowvp@postgres:5432/gowvp?sslmode=disable&TimeZone=Asia/Shanghai'

[Log]
Dir = './logs'
Level = 'info'
MaxDays = 7
MaxSize = 100

[Sip.DirectTCPDownload]
Enabled = false
StorageDir = './configs/downloads/gb28181'
RetainDays = 7
```

`.env` 中设定同一个发布标签、`OWL_MEDIA_SDP_IP=192.168.6.20`、管理端口及随机密钥。服务器首次可使用随包附带的 `deploy.sh init 192.168.6.20` 生成 `.env`，随后把 `OWL_IMAGE_TAG` 改为交付标签；已有 `.env` 时保留并按需更新，不覆盖凭据。环境变量优先于配置文件中的相关字段，详见通用部署说明。

AI 默认禁用，SIP-TLS、证书注册认证、信令摘要、附录 G 和附录 O 等功能按现场联调用例逐项启用，证书放入 `data/configs/certs`。四个国标版本的基础接入与可选能力分别验收，不将默认关闭的能力标记为已启用。

初始管理预算如下，均需监控，未配置硬配额：

| 数据 | 预算 | 策略 |
| --- | --- | --- |
| 录像 | 16TiB | 先保留 3 天；目录预算达 80% 时告警，复核路数与保留天数 |
| 下载 | 512GiB | 保留 7 天；单文件上限和并发限制不等于目录总容量限制 |
| 图片与封面 | 100GiB | 事件图片随事件保留策略清理，封面按通道更新 |
| Owl / SIP 日志 | 20GiB | 轮转、保留 7 天；抓包日志按需临时开启 |
| Docker 控制台日志 | 每容器约 250MB | 每文件 50MB，最多 5 个文件 |

录像容量按连续录制估算：`每天容量(GB) ≈ 路数 × 单路码率(Mbps) × 10.8`。例如 100 路、每路 4Mbps，3 天约 12.96TB，7 天约 30.24TB，未计缓存、音频及文件系统额外开销；这里 TB / GB 为十进制单位，预算 TiB 为二进制单位。

当前录像清理阈值检查的是录像所在的整个文件系统，不是 GoWVP 目录占用。MinIO 等业务增长也可能触发 GoWVP 提前清理自己的录像；该机制不能保证 16TiB 的目录预算，也不能保证磁盘紧张时仍保留完整 3 天。应同时监控 `/www` 使用率、GoWVP 目录容量以及录像实际保留时间。

回放生成的 `.video-cache` 需要另行落实缓存清理，不能假设删除原录像会自动删除对应缓存。录像优先使用应用已有清理流程，不直接定时删除 MP4，否则容易留下数据库记录与文件不一致。

## 7. 镜像交付与发布步骤

### 7.1 构建机准备

构建机安装项目要求的 Go（当前 `go.mod` 为 1.26）、兼容 Node.js、项目指定的 pnpm（当前为 11.11.0）、Docker 和 Compose，并具备依赖下载能力。优先使用 amd64 构建机；ARM 构建机生成 ZLM amd64 镜像需要对应平台模拟。

复核交付工作树已经包含“前端进入 `/app/www`、移除独立前端容器”的修改，再进行构建。两镜像分别为：

- `owl-v1-service:<tag>`：Owl、前端、FFmpeg。
- `owl-v1-zlm:<tag>`：ZLM，固定提交 `2fa539807370c4bcfca9801bde1dd2fdd95ba113`，应用仓库内 RTP/RTCP 补丁。

以下标签仅作示例，每次发布使用唯一标签，并记录源码提交和工作树差异：

```bash
set -euo pipefail

# 仓库根目录；缺少构建用 .env 时，先按 v1 说明执行 init。
export OWL_IMAGE_TAG=v1-20261028-r1
OWL_TARGET_ARCH=amd64 ./deploy/v1/deploy.sh build

docker image inspect "owl-v1-service:${OWL_IMAGE_TAG}" --format '{{.Os}}/{{.Architecture}}'
docker image inspect "owl-v1-zlm:${OWL_IMAGE_TAG}" --format '{{.Os}}/{{.Architecture}}'

# 上述架构结果均须为 linux/amd64；确认后进入独立交付目录导出。
mkdir -p "build/deploy-v1/release/${OWL_IMAGE_TAG}"
cd "build/deploy-v1/release/${OWL_IMAGE_TAG}"
docker image save \
  "owl-v1-service:${OWL_IMAGE_TAG}" \
  "owl-v1-zlm:${OWL_IMAGE_TAG}" | gzip > images.tar.gz
shasum -a 256 images.tar.gz > SHA256SUMS
```

交付包包含镜像归档及校验文件、已按第 5 节调整的运行 Compose、通用 `deploy.sh`、带注释的配置模板、本文及发布版本记录。构建机 `.env`、SSH 私钥、真实设备密码和业务数据库不打入交付包。归档先传到服务器 `/www2/gowvp-v1/packages`，不放 `/root` 或 `/tmp`。

当前仓库脚本没有镜像导出、上传、导入或服务器专用挂载功能；上述是交付步骤，不代表已提供自动化发布命令。

### 7.2 服务器准备顺序

1. 复核挂载、剩余空间、所需端口，以及摄像机 / 上级平台的实际可达网络。
2. 在维护窗口落实 containerd 存储迁移和服务挂载依赖，确认 Docker 仍能正常运行。
3. 创建第 5 节目录；安装运行 Compose、脚本和配置模板，不覆盖既有业务文件。
4. 首次初始化 `.env`，设置正确镜像标签、平台编码、国标域和设备注册密码，按第 6 节完成主配置。
5. 确认运行 Compose 已移除 `build`，完整替换挂载，设置资源与日志限制。
6. 核对访问来源控制；挂载失败、配置缺失、标签错误时停止发布。

### 7.3 导入与启动

在服务器上执行以下步骤前，应已完成上面的准备；这些命令在编写方案时未执行：

```bash
set -euo pipefail

cd /www2/gowvp-v1/packages
sha256sum -c SHA256SUMS
gzip -dc images.tar.gz | docker image load

cd /www2/gowvp-v1
docker compose --project-name owl-v1 --env-file .env --file docker-compose.yml config --quiet

# 示例为本次交付版本；后续以 .env 中的实际标签为准。
docker image inspect owl-v1-service:v1-20261104-postgres1 --format '{{.Os}}/{{.Architecture}}'
docker image inspect owl-v1-zlm:v1-20261103-manualrecord1 --format '{{.Os}}/{{.Architecture}}'
docker image inspect owl-v1-postgres:17-20261104 --format '{{.Os}}/{{.Architecture}}'

# 发布前还需用 findmnt 核对挂载来源与第 4 节一致。
mountpoint -q /www && \
mountpoint -q /www2 && \
test -f data/configs/config.toml && \
docker compose --project-name owl-v1 --env-file .env --file docker-compose.yml \
  up -d --no-build --pull never --wait --wait-timeout 180

docker compose --project-name owl-v1 --env-file .env --file docker-compose.yml ps
curl -fsS http://192.168.6.20:15123/health
```

管理端入口为局域网 `http://192.168.6.20:15123/`；授权 EasyTier 客户端可使用 `http://10.144.0.20:15123/`。前端入口会由 Owl 重定向到 `/web/index.html`。服务器不执行构建或 `release`，也不替换整机已有 Nginx。

## 8. 备份、升级与回滚

- 每次升级前备份 `.env`、`config.toml`、证书、ZLM 配置及 PostgreSQL。使用同版本 `pg_dump` 在线一致性备份，不直接复制运行中的数据目录；历史 SQLite 备份按原格式恢复。
- 本机恢复副本放在 `/www2/gowvp-v1/backups`，建议保留最近 7 次日备份和 4 次周备份，并复制到异机。`/www/backup` 实际也在 `/www2`，同一服务器的另一目录不构成异机备份。
- 录像默认循环覆盖；需长期保留的取证片段应单独归档到循环清理范围之外，并按业务要求异机保存。
- 升级导入新标签后，保留旧镜像、旧运行配置和升级前数据库备份，更新 `.env` 标签，再按第 7 节启动。
- 仅当数据库结构和运行配置向后兼容时才只切回旧镜像；涉及迁移时，应停止服务并恢复匹配版本的数据库和配置，避免把旧程序直接连接到不兼容的新库。
- 回滚期间不删除 `/www/gowvp-data`。停止容器不等于清理业务文件，不执行全局镜像 / 数据卷清理。

## 9. 落实状态与人工验收

| 项目 | 当前状态 / 完成标准 |
| --- | --- |
| 摄像机与平台网络 | 待提供设备网段和上级平台地址，确认使用物理网还是 EasyTier |
| 业务参数 | 九龙坡区平台 ID、域、随机密码已配置；初始录像 3 天、300 秒分片；实际通道数、码率和录像范围待联调 |
| 存储可靠性 | 待通过控制器管理信息核实 RAID、物理盘和缓存保护状态 |
| 运行文件 | 已落实专用 Compose、绝对路径、资源限制、日志轮转 |
| 存储保护 | 已落实 containerd 迁移、启动挂载依赖、容量日志告警和回放缓存清理 |
| 交付与启动 | 三个 amd64 镜像已就位，容器健康运行，详见第 10 节 |
| 备份 | 每日备份已启用、首份一致性备份已验证；定期异机备份目的地待明确 |

人工业务验收集中在三项：

1. 按实际设备分别验证四个国标版本的注册、心跳、目录查询和实时播放，扩展能力按启用项验证。
2. 验证局域网及实际需要的 EasyTier 播放路径；重点确认 WebRTC 媒体可达和多路视频下的带宽、丢包、资源占用。
3. 确认录像实际落在 `/www`，完成分片回放和下载，检查容器重建后配置、PostgreSQL、证书和媒体文件仍在，并验证备份恢复。

本文不包含磁盘吞吐压测、实际设备联调或并发容量保证，服务器资源与镜像网络检查不能替代这些业务验收。

## 10. 2026-10-28 部署实施记录

### 10.1 已部署版本与构建差异

运行目录为 `/www2/gowvp-v1`，Compose 项目为 `owl-v1`，只运行 `gowvp` 与 `zlm` 两个服务。统一镜像标签：`v1-20261028T164800-0b9e324`。Owl 健康接口版本为 `v1.4.21-203-g0b9e324-dirty`，基于分支 `codex/v2.1.1` 的 `0b9e324` 及本次未提交的部署修改；不将该标签解释为纯净 Git 提交构建。

| 镜像 | 已核验架构 | Docker image ID |
| --- | --- | --- |
| `owl-v1-service:v1-20261028T164800-0b9e324` | `linux/amd64` | `sha256:e3fb148453c6c373554ffc34c04159180c9680b47adc0c8997295a989587a5ab` |
| `owl-v1-zlm:v1-20261028T164800-0b9e324` | `linux/amd64` | `sha256:cb993d5827739a407ff4d6a7003b00c7630ea9073070e6e298b48fbbe484bec3` |

Owl 和前端在本机编译并打成镜像，上传归档、校验后导入服务器。本机 Docker 虚拟磁盘空间不足，ZLM 改用服务器现有 BuildKit 在临时构建容器中编译；没有给宿主机安装 Go、Node、pnpm 或 C++ 编译工具链。构建参数为每个构建执行容器限制 8GiB、4CPU，编译并发 `ZLM_BUILD_JOBS=4`，不代表整个多阶段构建合计只使用 4CPU。

服务器无法直连 GitHub，因此由本机获取固定 ZLM 提交及对应子模块、libsrtp 2.3.0，再传输到 `/www2/gowvp-v1/packages/build-zlm`。该目录保留实际使用的 `Dockerfile.zlm-offline` 与源码；仍应用仓库中的 RTP/RTCP 补丁，没有替换为无补丁的第三方媒体镜像。该 Dockerfile 仅将源码和 libsrtp 下载步骤改为本地 COPY，Ubuntu 软件包仍需网络获取。

源码归档 `zlm-source.tar.gz` 的 SHA256 为 `1723bd8e4216e016e58e7140f167897f9f3a3d43e724aea591e4b887399b89cb`；libsrtp 归档 SHA256 为 `94093a5d04c5f4743e8d81182b76938374df6d393b45322f24960d250b0110e8`。

交付归档保存在服务器 `packages`，已经校验：

| 文件 | 大小约 | SHA256 |
| --- | --- | --- |
| `service.tar.gz` | 84MiB | `c003c43c58b479141c0e647608a025e8b3d95f84fd364eaddff2287d176c5d98` |
| `zlm.tar.gz` | 317MiB | `a69093063b707aef04c657ec11523568fe63a9ba31c2ab794bda9b1203a13a0e` |

### 10.2 实际配置与保护措施

- 正式区划使用重庆市九龙坡区 `500107`，平台 ID 为 `50010700002000000001`、国标域为 `5001070000`，SIP 地址为 `192.168.6.20:15060`。编码按用户指定区划编排，上级平台分配与唯一性仍需对接确认。
- 随机管理密码、注册密码和其他密钥保存在服务器 `.env`，权限 `0600`；主配置同样为 `0600` 并保留中文注释。管理员账号 `admin`；查看登录密码可在服务器执行 `grep '^OWL_ADMIN_PASSWORD=' /www2/gowvp-v1/.env`，不要复制整份 `.env` 到聊天或 Git。
- WebRTC 为 `20000/tcp+udp`，国标 RTP 动态范围为 `20001-20100`。运行配置中的 `rtc.externIP` 为 `192.168.6.20,zlm`，Owl 按现有实现同时填入宣告地址及内部媒体主机名。
- 已核验全部实际绑定挂载；两个容器共享同一录像目录，并完成跨容器写入读取检查，检查文件已删除。ZLM 缓存和抓图在大盘，FFmpeg 日志在 `/www2` 并设置 logrotate。
- containerd 根目录已迁移至 `/www2/containerd`，原目录 `/var/lib/containerd` 保留，原配置备份在 `backups/host-before-deploy`。Docker / containerd 已增加磁盘挂载依赖。
- 2026-11-03 按用户要求取消来源网段限制：`DOCKER-USER` 对 `br-owl-v1` 的 GoWVP 和 ZLM 已发布端口允许所有来源，包括 SIP、RTSP、RTMP、WebRTC 和 RTP/RTCP。`access.sh` 原子替换本项目专用链，Docker 启动后自动重新应用；Compose 已发布端口统一绑定 `0.0.0.0`，ZLM HTTP 的 `allow_ip_range` 置空，详见第 10.9 节。
- 已启用每日备份与每小时容量检查 timer，首份 SQLite 在线备份执行成功并通过完整性检查。服务器归档在 `backups/daily/2026-10-28.tar.gz`，并复制一份到本机仓库的忽略目录 `build/deploy-v1/server-backups/2026-10-28.tar.gz`，两端 SHA256 一致，目录 `0700`、文件 `0600`。此为首次异机副本，不代表已配置定期异机备份；定期目标地址和告警通知接收方尚未配置。

### 10.3 已完成检查与待现场验证

已完成镜像架构及归档校验、存储保护检查、Compose 配置检查、两个容器健康检查；通过 HTTP 验证局域网和 EasyTier 健康接口、前端首页及资源、管理员加密登录、SIP 参数及密码、媒体节点在线、媒体 API / 回调配置、缓存路径和 SQLite 完整性。最终启动后的日志未发现 `level=ERROR` 或 panic。既有 Nginx、MySQL、MinIO、EasyTier 仍为 active。

空载快照中 Owl 内存约 20MiB，ZLM 约 38MiB；该结果不是设备接入后的容量结论。未进行真实摄像机注册、四版本协议联调、流播放、录像回放或并发测试，按第 9 节人工验收；RAID 健康和定期异机恢复演练仍待完成。

日常使用 [专用运行说明](192.168.6.20/README.md) 中的命令；不要在此服务器使用通用脚本的 `build/release/up`，也不要重复运行已完成的 `prepare-host.sh`。

### 10.4 同日登录问题修复

首次前端构建误读取开发机 `admin-ui/.env.local` 中的 `VITE_API_BASE_URL=/api`。移除 Nginx 后，Owl API 直接位于根路径，页面请求 `/api/login/key` 返回 404；管理密码本身正确。已将 v1 脚本改为显式使用 `VITE_API_BASE_URL=/`，重新构建前端并更新服务镜像。

当前运行标签为 `v1-20261028T164800-0b9e324-loginfix1`，服务镜像 ID 为 `sha256:c43602dd3989ab1ed44e6a39afdf27cea900d10a5e4d5c8e2cd515985e7a9645`；ZLM 仅增加同版本标签，镜像内容与上一版一致。前端修复通过基于旧服务镜像替换 `/app/www` 生成，后端二进制和密码未更改。旧镜像保留供回退，修复镜像另存为 `packages/service-loginfix1.tar.gz`，校验文件为 `packages/SHA256SUMS-loginfix1`。

已通过 VPN 地址核验服务器实际返回的新前端资源使用根路径 API，并按 HTTP 页面相同的 RSA-OAEP 算法验证登录和携带 token 访问配置接口均返回 200；两个容器健康。该次检查未操作浏览器，已有页面需强制刷新加载新资源。

### 10.5 现场接入密码调整

按用户要求更新服务器 `.env` 中的 `OWL_SIP_PASSWORD` 并重建 Owl 容器，已通过认证后的 `/configs/info` 接口核验接入密码生效，管理登录密码保持原值，备份已更新。实际接入密码以服务器 `.env` 为准，设备注册配置需同步修改。

当前部署使用 Owl 与 ZLM，尚未提供 JT/T 808、JT/T 1078 部标设备接入服务；前端部标设备页为预留入口，因此没有运行中的独立部标接入密码可调整。`pkg/lalmax` 中虽有 JT1078 配置模型，但本次未部署 lalmax，不将其视为已启用的部标服务。

### 10.6 同日停机与清理

本次处理误将用户要求的本机清理扩大到服务器，执行了专用 `run.sh down`，停止并移除本项目两个容器及网桥，停用 `owl-v1-backup.timer`、`owl-v1-monitor.timer`。这是错误的范围判断。清理完成时服务器无 Docker 容器；既有 Nginx、MySQL、MinIO、EasyTier 保持 active。

仅保留最后的 `v1-20261028T164800-0b9e324-loginfix1` 服务与 ZLM 镜像，交付归档为 `packages/service-loginfix1.tar.gz`、`packages/zlm.tar.gz` 和 `packages/SHA256SUMS-loginfix1`，校验通过。已删除旧镜像标签、临时 Ubuntu 镜像、构建源码副本、临时 Dockerfile、前端中间包、构建日志、旧交付包、过期凭据备份和本次构建缓存；第 10.1 节提及的构建目录已清理，不再保留。删除内容无法直接回滚，需要从项目源码重新构建；最后版本仍可直接启动。

运行配置、密钥、SQLite、媒体目录、宿主机迁移前配置以及最新日备份均保留。最新日备份已同步至本机 `build/deploy-v1/server-backups/2026-10-28.tar.gz`，两端 SHA256 一致。本机生成的 amd64/arm64 产物、源码下载、旧归档、前端 dist 和 GoWVP 旧镜像已删除，仅定向清除本次构建缓存，其他项目资源未清理。

服务器 `/www2` 文件系统实际占用减少约 2.4GB；本机删除约 500MiB 编译文件及 1.08GB 本次 Docker 构建缓存，并移除项目旧镜像。保留镜像共享的基础层，不以构建缓存账面大小作为额外可回收空间。

恢复时在服务器执行（不需要重新编译或导入镜像）：

```bash
cd /www2/gowvp-v1
./run.sh up
systemctl enable --now owl-v1-backup.timer owl-v1-monitor.timer
```

### 10.7 2026-11-02 纠正停机

用户明确清理范围仅为本机后，使用保留的最后版本镜像重新创建并启动 Owl、ZLM，两个容器均 healthy，重新启用备份与监控 timer。配置、SQLite 和媒体目录使用保留的原有数据，未重新初始化。管理入口的健康接口和前端首页已恢复 HTTP 200，并核验管理员登录、九龙坡区平台编码、国标接入密码及媒体节点在线状态。

先前删除的服务器旧镜像和临时构建文件未恢复；最后版本镜像、交付归档、业务数据和最新备份仍保留。今后本机清理操作仅作用于本机；服务器停止、删除或清理必须有明确的服务器范围授权。

### 10.8 2026-11-02 账号密码修改修复

账号安全页面此前明文提交 `{username,password}`，与后端要求的 RSA-OAEP 加密 `{data}` 和 `old_password` 契约不一致，因此提示“请求参数有误”。已复用登录的加密方法，并增加“当前密码”输入及旧密码校验。

新增 `data/configs/admin-credentials.toml` 单独持久化网页保存的管理员凭据，权限 `0600`；启动时优先于 `.env` 的初始管理员凭据，避免服务重启后恢复旧密码。原子写入成功后才更新内存，保存失败保持旧凭据，不再为修改账号重写整份主配置。每日备份已包含该文件，迁移、恢复和回滚必须保留它；旧程序不识别此文件，回滚到旧程序前需同步旧版本可识别的凭据配置。

Owl 更新为 `owl-v1-service:v1-20261102-accountfix1`，镜像 ID 为 `sha256:b7450ffeab82ebe2b3613c4fc7cfc667211954f8d891b0942756cde1f646a571`。通过 `.env` 的 `OWL_SERVICE_IMAGE_TAG` 单独覆盖服务镜像，ZLM 镜像标签、容器和启动时间保持不变。服务镜像归档为 `packages/service-accountfix1.tar.gz`，校验文件为 `packages/SHA256SUMS-accountfix1`。

已通过相关 Go 测试、前端类型检查和构建；通过实际前端认证服务代码的 HTTP 加密分支验证错误旧密码拒绝、正确旧密码提交成功及后续登录。生产验证沿用原管理员密码，新密码由用户在页面设置。已核验 Owl 重启后仍正常登录、国标配置未变、媒体节点在线，并确认备份包含新凭据文件。测试环境另验证不同新密码在旧环境变量存在时重启仍生效，旧密码被拒绝。本次未自动操作浏览器，用户需刷新页面加载更新后的表单。

### 10.9 2026-11-03 取消 GoWVP 与 ZLM 来源限制

录像机 `192.168.21.250` 的 SIP 注册曾被项目网段白名单拦截。已将 `OWL-V1-ACCESS` 改为所有来源 ACCEPT，并将 GoWVP、ZLM 已发布端口统一绑定 `0.0.0.0`，包括原先只绑定回环地址的 ZLM HTTP/API `8080`。

ZLM `data/zlm-config/config.ini` 的 `[http] allow_ip_range` 已置空，`[general] listen_ip=0.0.0.0`，`[rtc] interfaces` 为空，不设置来源网段或网卡白名单。重新初始化 ZLM 配置时需清空镜像自带的 HTTP 内网白名单。管理员、SIP、媒体 API 及推流认证继续保留；媒体 SDP 宣告地址不属于来源访问控制，仍为 `192.168.6.20`。

变更前的 Compose 和 ZLM 配置保存在服务器 `backups/before-zlm-open-20261103/`，包含密钥的配置保持 `0600`，不提交 Git。未调整整机其他业务的防火墙规则。

已重建两个容器并确认 healthy；通过 ZLM API 核验运行值与持久化配置一致，核验所有已发布端口的 HostIp 均为 `0.0.0.0`。在宿主机通过回环、LAN 和 EasyTier 三个地址验证 Owl 健康接口、ZLM HTTP 可达及 API 密钥认证，`554/1935/15060/20000` TCP 可连接。以上是服务器侧检查，未完成外部播放端的视频播放验证。

### 10.10 2026-11-03 真机 SDP 空媒体描述兼容修复

录像机以国标 `1.0 / 2011` 接入，流模式需使用 UDP；原先的 TCP 被动模式被当前协议能力规则拒绝。切换 UDP 后，录像机返回点播 `200 OK`，但 SDP 末尾带空 `f=` 行，底层 SDP 解析器报错，平台随即发送 BYE 并释放 RTP 接收端。

RTP 应答解析现在将空的可选 `f=` 行按未提供处理，RTP 下载文件大小解析复用相同处理；其余字段、SSRC、传输模式与负载类型校验继续保留。已添加现场应答结构的回归用例，验证 CRLF/LF、TCP/UDP、正常非空 `f=` 兼容，以及空必填字段、错误 SSRC、未协商负载类型仍被拒绝，相关 Go 测试通过。

仅更新 Owl 为 `v1-20261103-sdpfix1`，基于现有服务镜像替换后端二进制，沿用前端与依赖；ZLM 镜像、容器及启动时间保持不变。原 `.env` 保存在 `backups/before-sdpfix1/env`，旧服务镜像保留供回滚。交付包为 `packages/service-sdpfix1.tar.gz` 和 `packages/SHA256SUMS-sdpfix1`。

上线后通过现有管理员加密登录、点播 API 与播放代理验证：BH1-1-1（`34020000001320000011`）已由 ZLM 接收 RTP 推流，识别为 H.264、1280×720、25fps，代理返回 HTTP 200 及有效 FLV 数据。此为服务端收流与输出验证，浏览器画面由用户刷新后验收。

### 10.11 2026-11-03 新设备首次注册空缓存修复

设备 `34020000001110000202` 首次 REGISTER 自动建档时尚无来源地址，运行态初始化报 `host is empty` 后，缓存仍保存了空设备对象。后续 REGISTER 虽将数据库更新为在线，却无法替换此对象；设备信息查询报 `SIP request target URI is unavailable`，目录刷新报 `device offline`。新接入的 `34020000001110000203` 也受此问题影响。

现在无地址建档不创建运行态，缓存拒绝保存空对象，并允许后续注册原子替换遗留空对象；有效运行态保持不被并发注册覆盖。已验证首次建档后上线、遗留空对象恢复、并发缓存写入及相关注册流程，缓存包竞态检查和 Linux amd64 编译通过。

11:29 单独更新 Owl 为 `owl-v1-service:v1-20261103-registerfix1`，镜像 ID 为 `sha256:39ddad4e2cf7f7eca8376634d287c8e1326968047daccca0d9d123a9442eb519`。基于 `sdpfix1` 镜像替换后端二进制，前端、运行依赖和 ZLM 容器保持原样。更新前配置、管理员凭据和通过完整性检查的 SQLite 在线备份位于 `backups/before-registerfix1/`；回滚时恢复该目录中的 `env` 为 `.env`，仅重建 `gowvp` 服务，正常回滚不覆盖业务数据库。

交付归档为 `packages/service-registerfix1.tar.gz`，校验文件为 `packages/SHA256SUMS-registerfix1`；归档 SHA256 为 `2e1ce5be2af93053979d8af2ef1fe84443b1ab16313fa757c2872e383f9ce264`。已核验容器内二进制 SHA256 与本机产物一致，管理登录、首页、VPN 健康接口正常。

发布后 `34020000001110000203` 目录查询返回 200，已入库 32 个通道，其中 29 个在线。`34020000001110000202` 在旧版本期间未更新心跳，启动后按超时规则关闭注册绑定；11:30 抓包确认其心跳收到 403，观察至 11:34 仍未收到新 REGISTER。需在录像机 `192.168.21.251` 上重新启用国标接入以触发注册，再验证目录刷新，不能通过手工标记在线代替重新注册。

### 10.12 2026-11-03 心跳超时自动恢复

按用户要求，心跳超时或信令连接中断只将设备标记为离线，保留尚在有效期内的 REGISTER 绑定。后续有效心跳自动恢复在线并补查 Catalog，无需人工开关录像机国标接入。连续 REGISTER 仍正常处理，注册密码验证保留；设备主动注销、修改注册密码或注册有效期结束仍按对应协议流程处理，不把临时心跳离线当作注销。

11:54 单独发布 `owl-v1-service:v1-20261103-heartbeatfix1`，镜像 ID 为 `sha256:3b91e0da8d60fc9dc23e5bb41a09ce370f6c97d4a04ef8bb90818fbf8b1f1a93`。基于 `registerfix1` 替换后端，ZLM 容器及启动时间保持不变。更新前配置、管理员凭据和 SQLite 一致性备份保存在 `backups/before-heartbeatfix1/`。交付归档为 `packages/service-heartbeatfix1.tar.gz`，校验文件为 `packages/SHA256SUMS-heartbeatfix1`，归档 SHA256 为 `3aca97e5f75c148542db28142caf7b2a22ecb123824595bd84d327323e1f52ba`。

对 `34020000001110000202`，核对旧备份中的注册代次、已确认的成功响应、来源地址、未改变的凭据及未到期的有效期后，仅恢复旧逻辑错误关闭的绑定标记；未修改在线状态、注册时间或心跳时间，原标记保存在 `backups/before-heartbeatfix1/binding-before.json`。11:54:46 收到真实心跳后，设备自动上线并补查出 32 个通道，其中 29 个在线；随后实际目录刷新 API 返回 200。第 10.11 节中待人工重新注册的状态已由本次处理解除，无需用户操作录像机。

已通过心跳超时恢复、离线落库重试、注册有效期、并发新心跳与续注册等相关测试和竞态检查；管理登录、首页、健康接口、容器内二进制校验及 SQLite 快速检查正常。发布后四台设备均在线且各有 32 个通道。临时构建文件和运维私钥副本已清理，交付归档及回滚备份保留。

### 10.13 2026-11-03 手动录像、扩大媒体端口与无人观看断流

Owl 与 ZLM 均更新为 `v1-20261103-manualrecord1`，保留此前账号、SDP、注册缓存及心跳恢复修复。服务镜像 ID 为 `sha256:ef4e67f49a9204977d6498efe67b2fb55a7ac6f122e0f2c391554a17ca10d856`，ZLM 镜像 ID 为 `sha256:ddd9fd87f7d16d2cc61d70daefccf1b7c72e235276e23e7d25541c18ca64a058`。ZLM 沿用已有补丁二进制，仅更新入口脚本；服务包含本次前后端修改。

- 默认录像策略改为 `none`，录像模块保持启用；未设置独立策略的通道不再自动拉流录像。在通道详情“录像”页或实时视频操作区手动开启/停止。开启后持续录制到手动停止，关闭页面不会停止；已有显式开启的通道策略保留。
- 媒体范围为 `30000-50000/tcp+udp`，其中 WebRTC 使用 30000，国标 RTP/RTCP 动态端口池为 30001–50000。ZLM 使用宿主机网络，HTTP/API 使用 8080，避免数万条 Docker 端口映射；Owl 仍为桥接网络，使用 `zlm:host-gateway` 访问媒体节点。所有来源继续允许。
- 管理端“SIP 设置 → 视频断流”可设置无人观看等待时间，范围 1–86400 秒，默认 180 秒，保存到 `Media.StreamNoneReaderDelaySeconds` 并下发 ZLM，无需重启。新值用于下一轮计时，已经开始的倒计时沿用原值。手动录像作为消费者保持推流，停止录像且无人观看后重新计时；到期关闭流、释放 RTP 接收端并通知设备停止推流。
- 录像共享目录统一为两个容器内的 `/app/configs/recordings`，实际仍位于宿主机 `/www/gowvp-data/recordings`，修复 MP4 完成回调因容器内绝对路径不同而无法入库的问题。裸 TCP 下载中继默认范围移到 50200–50300，避免与媒体端口池重叠。

ZLM 离开原桥接网络后，Owl 动态地址曾从 `172.19.0.3` 变成 `.2`，设备已有 SIP UDP 转发会话仍指向旧地址，导致心跳中断和提前清流。专用 Compose 已固定原网段 `172.19.0.0/16` 和 Owl 地址 `172.19.0.3`，实际容器地址已核验，设备真实心跳恢复；后续更新应保留此配置。

更新前 `.env`、Compose、主配置、管理员凭据、ZLM 配置、SQLite 一致性备份及容器信息位于 `backups/before-manualrecord1-20261103/`，敏感文件保持 `0600`。旧镜像保留供回滚；网络与挂载也有变更，回滚时须结合备份的 Compose 和配置，不宜只切换镜像标签，正常回滚不覆盖业务数据库。

交付目录为 `packages/manualrecord1-20261103/`，包含最终镜像归档、二进制、前端包、构建文件、Compose 和 `SHA256SUMS`。最终 `images.tar.gz` 的 SHA256 为 `dc3e3febdfb04f6c80bc0a36eba93ae651deb5c877b96aac78a7b2e2b8a1620b`，后端二进制为 `7691782232fc219115fefae81033a4a2247df4a72b21d601b980f54867ee82c2`，前端包为 `1f42ca05aee59a791803e28d43954980445e4a6cefe9d300311ab740781b12d2`。

已通过相关 Go 回归检查、前端构建、Compose 与入口脚本检查，以及实际管理登录、配置保存和媒体节点热更新检查。真机 BH1-1-1 手动录像连续约 205 秒，停止后 MP4 入库成功，管理端下载返回 206，FFprobe 确认为 H.264、1280×720。浏览器播放画面由用户刷新页面后验收。

停止手动录像后，实测第 121 秒流仍存在、观看数为 0；约 180 秒到期，14:45:43 捕获到平台向录像机发送 BYE 并收到 200 OK。随后媒体列表及 RTP 接收端列表均为空，四台设备仍在线且真实心跳持续更新，确认本次断流由无人观看超时触发。验证结束已停止测试通道录像，128 个通道均未启用录像；保留本次生成的测试录像供回放验收。两个容器均 healthy、无重启或 OOM。

此次流注销的多个协议回调中，有一次对已停止录像再次调用 `stopRecord` 遇到 `http: server closed idle connection` 日志；上述 MP4 已完成入库，流及 RTP 接收端也已释放。本次未扩大修改范围处理重复停止录像的日志。

### 10.14 2026-11-04 PostgreSQL 迁移与时序历史

09:50 完成切换，09:59 更新下述时间类型修复。Owl 为 `owl-v1-service:v1-20261104-postgres1`，最终镜像 ID 为 `sha256:9c67a06b6469e4606c0e375376756329b3893c9cdcb2fe44c488fc087b1eadef`，后端二进制 SHA256 为 `15705193f1c67ebb5a264a00552a81d6b444189f05c4fdb458735b4fd9620604`。基于上一版本镜像替换后端，前端保持原版，增加 `OWL_DATABASE_DSN` 环境注入支持，主配置不保存实际数据库密码。ZLM 镜像、容器及启动时间保持原样。

新增独立 `owl-v1-postgres:17-20261104`，PostgreSQL 17.11，官方镜像摘要为 `sha256:266d3c257594e195b309ae9d1805e54c788f1d7b375f56bf6c367aac6c917e7b`。数据库为 `gowvp`，应用角色同名，非超级用户、不可建库或创建角色；初始化管理员使用独立凭据。数据目录为 `/www2/gowvp-v1/data/postgres`，容器地址 `172.19.0.4`，仅 Compose 内部提供 5432。服务器已有 `/www/server/pgsql` 实例及其账号、数据、认证配置均未调整。

仅停止 Owl 写入后制作最终 SQLite 快照，使用仓库 `tools/db-migrate` 向空 PostgreSQL 导入。12 张表逐行核对通过：4 台设备、128 个通道、132 个已占用业务 ID、3 条录像索引、4019 条历史（4000 条心跳、19 条成功注册），以及版本、媒体节点和其他表。原主键、JSONB 扩展及 REGISTER 私有幂等状态保留；时间转换为 PostgreSQL 微秒精度，自增序列已推进。随后启动新 Owl，并观察超过 180 秒，四台设备的真实心跳持续写入新库，未出现切换 IP 导致的心跳中断。

心跳和成功注册历史沿用独立历史表，按 `(device_id, kind, recorded_at)` 索引、使用 `timestamptz`，当前状态仍保留在设备业务表。现有策略每台设备每种事件保留 1000 条、最长 30 天，先到先清理，可在管理端调整；当前规模不引入 TimescaleDB 或分区。设计与后续扩展边界见 [PostgreSQL 与设备时序历史](postgresql.md)。

已通过真实 PostgreSQL 的迁移回归：JSONB/布尔/时间与隐藏注册字段保留、序列推进、重复迁移拒绝、坏数据导致 DDL 和数据整体回滚。实际快照已先在独立库预演，通过后才正式切换。已核验管理员加密登录、设备/通道列表、每台设备两类历史分页、录像索引与 MP4 Range 下载（206）、首页与健康接口、SIP 编码和 180 秒断流配置。没有强制录像机重新 REGISTER，本次验证包括既有注册恢复及真实新心跳，不能视为新增注册流程的全量真机验收。

备份脚本改为同版本 PostgreSQL 容器内 `pg_dump`，2026-11-04 日备份已生成并通过 `pg_restore --list`；另恢复到独立空库，确认设备、通道、录像、业务 ID 和历史数量一致，应用角色拥有所有业务表。恢复演练库和迁移预演库随后删除，业务库保留。配置、原运行信息和最终 SQLite 快照位于 `backups/before-postgres1-20261104/`，旧 `data/configs/data.db` 未删除，均不再由应用写入。切换后新增数据不会同步回旧库，回滚前应停止写入并保存新库备份，不能直接切回旧连接并声称无数据损失。

切换后的日志发现告警清理任务将毫秒整数传给 `timestamptz` 查询，导致参数编码失败；现已与录像清理一致，传入 `orm.Time`，补充 PostgreSQL 时间绑定的回归检查并更新 Owl。最终启动日志确认告警清理正常结束。启动时另有一次 HealthCheck 错误，经源码复核属于未运行的 Python AI gRPC 服务探测，并非 ZLM 媒体节点探测；未持续重复。

交付目录 `packages/postgres1-20261104/` 包含服务/数据库/ZLM 三个本地镜像归档、后端与迁移工具、Compose、初始化与备份脚本及校验文件。`images.tar.gz` SHA256 为 `c9cd859acb98fc7aab1b3eca82634c8c61c92ab22185e410ce15ae8971b66202`。三个容器均 healthy，Owl 地址仍为 `172.19.0.3`，未改动媒体来源放行规则和录像文件位置。预演数据库、测试快照与临时运维脚本已清理；保留最终交付归档、迁移前备份与业务数据。

### 10.15 2026-11-04 录像查询与复合索引发布

10:36:23 完成后端切换，服务镜像为 `owl-v1-service:v1-20261104-queryindex1`，镜像 ID 为 `sha256:5ef963a32756e8719e096d7fc0571bb94d723685bfd8c4a0117776a02001294d`，二进制 SHA256 为 `f5e53ab849463033a90d191fd94e757e44a6a6ed6f1b07c71d8bd4adeb5391c7`。以现网 `postgres1` 镜像为基础仅替换后端，前端入口文件校验值保持 `3af8c9db1e469dcb8afff5ad01d0a00874cf2276b304a1bca5a23f968f123fcc`，ZLM 和 PostgreSQL 容器启动时间未改变。仅调整 `.env` 的 `OWL_SERVICE_IMAGE_TAG`，更新 `gowvp` 服务，容器 IP 仍为 `172.19.0.3`。

更新前在线 `pg_dump`、配置、管理员凭据和运行信息保存在 `backups/before-queryindex1-20261104/`；dump 已通过归档目录检查，SHA256 为 `d75186c4eb1cc969d0ad5d89cbd36ab667d671fb9bb58b552e640fd194d5b576`。随后以表所有者并发执行 `sql/20261104-recording-query-indexes.sql`，创建 `recordings (cid, started_at DESC)` 和 `channels (did, created_at DESC)`，确认两者 `valid=true`、`ready=true` 后才切换应用。保留旧单列索引和旧镜像，正常回滚只需恢复旧服务标签，新增索引兼容旧应用，不覆盖业务数据库。

时间轴取消 1000 条上限，逐行读取展示字段，按开始时间和 ID 稳定排序，接口仍返回原 `items`。月统计改为单次 SQL 按天 `EXISTS`，取消 10000 条截断，沿用服务 `Asia/Shanghai` 日边界及录像开始日归属。发布前在服务器 PG17.11 独立测试库验证 1105 条时间轴、超过 10000 条月统计、通道筛选、跨时区日期边界和复合索引，全部通过；测试库和临时测试程序已删除，未向业务表写入测试数据。

发布后已验证实际加密登录、录像列表、时间轴、月统计与空结果接口，三条现有录像的查询结果与数据库一致；设备 4 台在线、通道 128 个。EasyTier 地址的健康接口返回新版本。应用启动日志未发现 SQL 错误、慢 SQL、panic 或告警清理异常；仍有一次已知的未启用 AI 服务健康探测错误，本次未扩大范围调整 AI 初始化逻辑。浏览器页面效果和视频播放仍由人工验收。

交付目录 `packages/queryindex1-20261104/` 保存二进制、Dockerfile、索引 SQL、`SHA256SUMS` 和 `service-image.tar.gz`；镜像归档 SHA256 为 `072ff132af965aa5947722302bd92efd48222332e57609c7a760dc35be8d21cb`，归档完整性与校验值已验证。


### 10.16 2026-11-04 三方应用开放平台发布

12:12:06 完成 Owl 前后端切换，镜像为 `owl-v1-service:v1-20261104-openplatform1`，镜像摘要 `sha256:1e525f8a70472c53ac6b8b21df37b64d04b80614aa8213ce4e9ff7a11f03eb03`，后端 SHA256 为 `591fc1122b18331957dc178ac7801bc96249a9d9752a4be9c1113aafd86dc2c2`。基于已部署 queryindex1 的运行环境，仅替换后端与前端并加入迁移工具；保留 ZLM、PostgreSQL 容器和启动时间，Owl 仍为 `172.19.0.3`。本地交叉编译 linux/amd64，服务器只做基于本地旧镜像的 COPY 封装，无拉取或依赖安装。

新增独立 AK/SK、全部/指定设备授权、媒体票据、抓拍、云台安全停止、应用告警订阅与管理页面。按用户要求继续使用 `http://10.144.0.20:15123`，设置 `OWL_OPEN_PLATFORM_ENABLED=true`、`OWL_OPEN_PLATFORM_ALLOW_HTTP=true`、Audience `owl-192-168-6-20`。该 HTTP 开关默认关闭，本机显式允许明文入口、密钥响应和 HTTP 回调；不改变 HMAC、防重放、Scope 和资源权限检查。旧媒体端口和现网访问规则未改，未实施三方专用网络隔离，不宣称旧播放入口已受应用设备授权保护。

更新前在线 PG dump SHA256 为 `f9f966aa0cc7617ca2d22bac5e16aa9e96917b7fea35784d7eb0b0efb0d89f12`，已通过 `pg_restore --list`；数据库、运行配置、管理员凭据及容器信息位于 `backups/before-openplatform1-20261104/`，目录 `0700`。迁移新增 15 张开放表，以 information_schema 核对原业务表列定义完全一致。独立主密钥在服务器以系统随机数生成，仅写入 `data/configs/open-platform-keyring.json`，权限 `0600`；受控备份保存副本，日备份脚本也已加入该文件。未输出密钥，未创建示例应用或触发真实抓拍、云台及外部告警。

检查包括：开放平台、管理 API、数据层相关 Go 测试及正式前端构建；HTTP 默认拒绝与显式允许的回归；发现并修复旧 GB 控制包装入口对已取消 context 的返回顺序，直接相关取消/控制用例通过。发布后实际加密管理员登录成功，状态接口确认启用与 HTTP 配置，应用列表为空、授权设备列表为 4 台；无凭据和 query token 无法访问新增管理入口，管理员 JWT 不能调用应用接口。线上 OpenPlatformView 资源内容与发布包一致。健康接口返回新版本，四台设备仍在线、128 个通道保留，新增模块启动错误为 0。真实设备动作、播放器和接收方业务效果仍由人工验收。

交付目录 `packages/openplatform1-20261104/` 保存二进制、前端、OpenAPI、源文件摘要、Dockerfile、运行信息和镜像归档。`service-image.tar.gz` SHA256 为 `7ee37f4ec4d517132084aaa8d27d647330a1d2371ab476dcafabe5de8ba30aca`，归档已验证。回滚应停止三方接入，正常关闭模块后恢复 queryindex1 服务镜像，仅更新 gowvp；保留新增表和主密钥，不用更新前备份覆盖当前业务数据。

### 10.17 2026-11-04 完整开发者文档发布

13:12:16 完成 Owl 前后端切换，镜像为 `owl-v1-service:v1-20261104-openplatformdocs1`，镜像摘要 `sha256:33f6b5764c90117f4915257468229e8bdf33c8c3ab83e485d3ef08bb222141bb`，后端 SHA256 为 `ed2925dfe75d498bb8182b04748e86c114509e88cc54a4925ea792f27b1ba0a5`。基于现网 openplatform1 镜像仅替换二进制和前端，无数据库迁移，未调整应用授权、端口、设备配置或防火墙。ZLM、PostgreSQL 容器 ID 和启动时间均未改变。

公开文档入口为 `http://10.144.0.20:15123/web/#/open-platform/docs`，无需管理员登录，管理页“接入文档”直接跳转。包含 56 个接口操作、42 个字段模型、6 个指南章节、47 个错误码，以及完整参数、权限、请求和响应示例。8 个公开下载包括 OpenAPI JSON/YAML、结构化指南、签名说明及向量、Go/Python/Java 标准库客户端。三个客户端支持显式 `OWL_ALLOW_HTTP=true`，默认仍要求 HTTPS。新增 `/open-platform/docs/config` 仅公开 enabled/public_url/audience/allow_http；原业务和管理员接口认证保持有效。

完成正式前端构建、文档生成一致性和路由双向核对、80 个请求/响应示例的字段约束检查、公开配置及认证相关 Go 测试；三种语言固定签名向量一致。发布后健康接口返回新版本，公开配置和全部 8 个下载无需登录可访问，下载 SHA256 与发布产物一致；线上页面资源与产物一致。无 JWT 的管理接口、无签名的业务接口仍返回 401。三个容器健康，4 台设备均在线、128 个通道保留。未启动浏览器，页面效果由人工验收；未触发真实抓拍、云台或外部告警。

更新前在线数据库和配置备份位于 `backups/before-openplatformdocs1-20261104/`，PG dump 已通过 `pg_restore --list`，SHA256 为 `0d2c7bd6a6a1fcb286363a933d1c277d3c1f2c7a73c32700e940991579834df6`。交付目录 `packages/openplatformdocs1-20261104/` 保存二进制、前端、文档、Dockerfile、SHA256SUMS 和镜像归档；`service-image.tar.gz` 已通过 gzip 完整性检查，SHA256 为 `2043d76f1ca4006b8c1a4848fad6a837c212eb7c58acc761ccbf29a5b79ab0eb`。上传包 `openplatformdocs1-20261104.tar.gz` SHA256 为 `d9e42286cd4ad679278e2e770300cdf41fef84639f5620524b2e48e960992203`。旧镜像保留，正常回滚仅恢复 `v1-20261104-openplatform1` 标签并更新 gowvp，不恢复数据库、不更换密钥。

### 10.18 2026-11-04 开放平台界面统一

13:37:49 切换服务镜像为 `owl-v1-service:v1-20261104-openplatformui1`，镜像摘要 `sha256:4a9f7bb19d0838bfd33fd18554316921d244781b9416ea91102dd8828a3fc8ac`。本次仅替换前端，以 openplatformdocs1 为基础封装，后端二进制 SHA256 保持 `ed2925dfe75d498bb8182b04748e86c114509e88cc54a4925ea792f27b1ba0a5`，健康接口后端版本仍为 `v1-20261104-openplatformdocs1`。无数据库迁移，未调整应用授权、设备配置、HTTP 地址或防火墙。

开放平台复用既有标题、表格、工具栏、分页、状态标签与 ModalDialog，移除局部通用表格样式覆盖。应用名称和“详情”进入独立路由 `/web/#/open-platform/apps/{id}`，采用与设备详情一致的返回入口和分区导航；刷新保留当前分区，返回列表恢复搜索条件与页码。应用概览、接口/设备授权、AK/SK、回调目标、订阅及各类记录统一信息层级，记录弹窗展示字段名称、原字段标识和格式化时间。保留原权限、设备勾选、密钥一次展示及业务操作逻辑；增加窄屏重排、加载/失败/空状态及长标识换行。公开开发文档保持可直接分享。

正式前端构建（文档一致性、vue-tsc、Vite）通过，最终差异检查通过。线上入口 HTML、主 JS/CSS 和 OpenPlatformView JS/CSS 与本地产物逐一校验一致，健康接口和公开文档正常，无凭据的管理/业务接口仍返回 401。ZLM、PostgreSQL 容器 ID 和启动时间均未改变，4 台设备全部在线、128 个通道保留。按项目规则未启动浏览器，页面展示与完整交互由人工验收；未创建测试应用、未触发抓拍、云台或外部告警。

交付目录为 `packages/openplatformui1-20261104/`，包含前端、Dockerfile、文件/源文件摘要、运行信息和镜像归档。上传包 SHA256 为 `7eb45d737c9d753cc90c248ae4ffcb36748ed555d17c5bc897e3ed728f777cc0`；`service-image.tar.gz` 已通过 gzip 完整性检查，SHA256 为 `32ea84bf02e255b0a92cdfda65f13a5277655cd83636005a754ca6d2b37938ed`。更新前 `.env`、Compose、容器信息、后端摘要和设备计数保存于受限目录 `backups/before-openplatformui1-20261104/`。旧镜像保留，回滚只需恢复 `v1-20261104-openplatformdocs1` 标签并仅更新 gowvp，不覆盖数据库、不更换密钥。

### 10.19 2026-11-04 生产 API 地址修复

13:46:23 将服务镜像切换为 `owl-v1-service:v1-20261104-apibasefix1`，镜像摘要 `sha256:d0121442a04cc73c935b4d48368cca6065fb184e537c9151f08fe18952dca8a5`。本次仅更新前端，后端版本仍为 openplatformdocs1，二进制 SHA256 保持 `ed2925dfe75d498bb8182b04748e86c114509e88cc54a4925ea792f27b1ba0a5`。ZLM、PostgreSQL 容器 ID 和启动时间保持不变，无数据库迁移或权限、设备、HTTP 地址及防火墙调整。

用户反馈开放平台出现“来到了无人的荒漠”，系统状态 4/4 指标失败。根因为 openplatformui1 的 Vite 构建读取了 `.env.local` 中的 `VITE_API_BASE_URL=/api`；该前缀只由开发代理重写，生产 Owl 不提供 `/api/*`，导致全站请求 404。上一轮静态资源/健康检查未覆盖页面实际请求地址，不足以证明页面可用。新增受版本管理的 `.env.production`，生产默认同源 `/` 覆盖开发配置，保留本地 `.env.local`，仍支持通过构建进程环境变量显式设置生产网关，并更新构建说明。

新增回归用例，使用含 `/api` 的临时开发环境验证生产地址为 `/`、开发地址保留以及显式环境变量覆盖，测试通过；正式构建（文档检查、vue-tsc、Vite）通过，发布前确认编译后的 Axios 配置为 `baseURL:"/"`。发布后逐一核对在线入口、主 JS/CSS、http、SystemStatusView 和 OpenPlatformView 资源与产物一致，线上 Axios 确为同源根路径。通过 RSA-OAEP 加密管理员登录，实测 `/health`、`/app/metrics/api`、`/stats`、`/media_servers` 全部 200；开放平台状态、应用列表、已有应用详情、授权设备、凭据、回调、订阅、投递、抓拍、云台和审计分区读取全部正常。验证只读业务接口，未创建测试应用或触发设备动作；凭据与 JWT 仅存在验证进程内存，不输出、不写入文件。未启动浏览器，用户需刷新以替换当前已加载的旧脚本。

交付目录 `packages/apibasefix1-20261104/` 包含前端、Dockerfile、摘要、运行信息与镜像归档，上传包 SHA256 为 `307e1300a319d968b9b8df96be693bacdcd562f475b3c702185740aa2aa16d5e`；镜像归档已通过 gzip 完整性检查，SHA256 为 `90bb4964ecdfa157dc5043ec7934710bd3bfe6a01a08026dcf9eae61550de694`。更新前环境及容器信息保存在受限目录 `backups/before-apibasefix1-20261104/`。旧镜像保留；紧急回滚应选最近正常的 `v1-20261104-openplatformdocs1`，仅更新 gowvp，不恢复数据库或更换密钥。回退 openplatformui1 会重新引入错误的 API 前缀。

### 10.20 2026-11-04 全局紧凑界面发布

14:59:54 将服务镜像切换为 `owl-v1-service:v1-20261104-compactui1`，镜像摘要 `sha256:5d24eb9718799db0c6df4c6e166eaad40b5b7d16ed75a27f8a1b7f7be6e821f9`。在本机从现网 apibasefix1 镜像生成 linux/amd64 前端更新镜像，服务器只导入镜像并重建 gowvp。后端二进制 SHA256 保持 `ed2925dfe75d498bb8182b04748e86c114509e88cc54a4925ea792f27b1ba0a5`，健康接口版本仍为 openplatformdocs1。ZLM、PostgreSQL 容器 ID 和启动时间均保持不变，无数据库迁移或凭据、设备配置、媒体及访问规则变更。

统一蓝灰白配色、32px 桌面控件/28px 行内按钮、标题与面板间距、表单宽度和中文系统字体；平台运维默认收起，进入对应路由时展开，窄屏导航/工具栏/表格与弹窗适配，触屏保留 44px 操作目标。资源选择框固定定位并按可用空间翻转，支持视口变化和键盘操作。镜像保留前一版哈希资源，让尚未刷新页面继续加载原异步模块。

正式构建（文档一致性、vue-tsc、Vite）、生产 API 地址回归和实际编译产物 `baseURL:"/"` 检查通过。发布后 517 个静态文件与 SHA256SUMS 一致；通过 HTTP 获取的入口、主 JS/CSS、HTTP、系统状态、开放平台、国标能力、级联和拉流等 17 个关键文件与本地一致。RSA-OAEP 加密管理员登录成功，健康、API 指标、资源统计、媒体节点、设备/通道、开放平台状态/应用列表和公开文档配置均 200，未登录资源接口仍 401。4 台设备全部在线、128 个通道保留；启动日志未发现 ERROR、panic 或 fatal。凭据和 JWT 仅在验证进程内存中使用，不输出、不持久化。未启动浏览器，页面效果、真实触屏、视频与设备操作由人工验收。

更新前配置与一致性数据库备份在 `backups/before-compactui1-20261104/`，PG dump 已通过 `pg_restore --list`，归档 SHA256 为 `eb40836a4537f4f716b58e55024bce6c33b9d28b03feae402bfb843eaeb0f0ff`。交付目录 `packages/compactui1-20261104/` 含前端、Dockerfile、源文件/产物摘要、镜像归档与带失败回滚的发布脚本。上传包 SHA256 为 `c6f905d2961bad5d348471f6645ea45fefd0efac952d4b2f731bda5d77b95d8f`；镜像归档通过 gzip 检查，SHA256 为 `924ac5cae7f5755566cd5feb9d22c5142a1e7af77154292a9de95d3c621c05f6`。旧镜像保留，正常回滚到 apibasefix1，只更新 gowvp，不恢复数据库或更换密钥。

### 10.21 2026-11-04 运行总览浅色界面与运行时长修复

15:14:00 将服务镜像切换为 `owl-v1-service:v1-20261104-overviewlight1`，镜像摘要 `sha256:dfea62d3159c31867890f15978bf443d12df1f4a61b6f390bb2acc1714049ef0`。基于已部署 compactui1 镜像仅覆盖前端，保留旧哈希资源以兼容未刷新页面。后端版本仍为 openplatformdocs1，二进制 SHA256 保持 `ed2925dfe75d498bb8182b04748e86c114509e88cc54a4925ea792f27b1ba0a5`；ZLM 和 PostgreSQL 的容器 ID、启动时间均未改变。

运行总览移除深蓝态势条、实蓝快捷入口标题和媒体服务多层蓝底，使用白色状态摘要、四列业务数据、轻量快捷入口及协议列表，统一数字与辅助文字层级。窄屏摘要自动换行，640px 以下业务数据为两列。统一时长函数：不足一分钟显示“不足 1 分钟”，不足一小时显示分钟，小时内补充分钟，超过一天显示天/小时；顶栏、总览、系统状态、升级页同步生效，异常日期显示破折号。

运行时长边界及生产 API 地址回归、正式构建（文档一致性、vue-tsc、Vite）、CSS 语法和源码差异检查通过。发布后 517 个静态文件摘要匹配；通过 HTTP 获取的入口、主 JS/CSS、HTTP、时间格式、总览、系统状态和升级页等 9 个文件与本地产物一致，生产 Axios 地址仍为 `/`。RSA-OAEP 管理员登录成功，健康、API 指标、系统资源、媒体节点、设备、通道及开放平台状态接口均 200，未登录设备接口仍 401。4 台设备均在线、128 个通道保留，启动日志未见 ERROR/panic/fatal。凭据和 JWT 只在验证进程内存中使用，未输出或写入文件。未启动浏览器，页面效果和真实设备业务由人工验收。

配置与一致性数据库备份位于 `backups/before-overviewlight1-20261104/`，备份归档 SHA256 `3dc0474086bf113c2aa76c85205ce67d83e4a5aaad7b4d8b36e90996c0c40e32`。交付目录为 `packages/overviewlight1-20261104/`，含前端、Dockerfile、文件/源文件摘要及带失败回滚的发布脚本。上传包 SHA256 `7a818069346e9f5466cb8ab6aae457a530a49cd0f4e92a11f87e49b71ab04a84`；镜像归档通过 gzip 完整性检查，SHA256 `2ceac64b0a9038c8489a66648f2ab520d45119549cc1c8ee17877460db5615f6`。旧镜像保留，正常回滚恢复本轮备份 `.env` 后仅重建 gowvp 至 compactui1，不恢复数据库或更换密钥。

### 10.22 2026-11-04 顶栏管理员身份文案

15:18:35 发布 `owl-v1-service:v1-20261104-userlabel1`，镜像摘要 `sha256:e68b0cb92fba5c8f9096f9e3c1f8ccba565cf41674347bc9953dbefc2658be0c`。顶栏显示“管理员”，展开菜单显示“系统管理员”和“登录账号：实际账号”，补充菜单按钮无障碍名称；不修改认证、账号或权限。

本地生产构建完成文档一致性、Vue 类型、Vite 检查，源码差异检查通过。上传已编译前端后，在服务器以本地 overviewlight1 镜像纯 COPY 封装，不拉取镜像、安装依赖或编译后端。仅重建 gowvp，后端二进制 SHA256 保持 `ed2925dfe75d498bb8182b04748e86c114509e88cc54a4925ea792f27b1ba0a5`，ZLM/PostgreSQL 容器 ID 与启动时间不变。发布后容器健康，517 个静态文件摘要匹配，通过 HTTP 获取的入口、主资源、AppShell 和 HTTP 模块共 6 个文件与本地产物一致，并确认包含新身份文案。未启动浏览器，实际菜单展示由人工验收。

发布包 `packages/userlabel1-20261104.tar.gz` SHA256 为 `a76c74d9dbd44fa0d59bbcc10221c80759a26675477aaf008d2c2de297d9fcf7`，解包目录包含前端、Dockerfile、摘要和带自动回滚的 release.sh。配置与一致性数据库备份位于 `backups/before-userlabel1-20261104/`，备份归档 SHA256 为 `d34910b393fb5325124f946c94e454f07e247c5ae2e41dde4a270935984a779b`。回滚恢复本轮备份 .env 并仅重建 gowvp 至 overviewlight1，不恢复数据库。

### 10.23 2026-11-04 录像中心检索重排与页内回放

15:36:43 发布 `owl-v1-service:v1-20261104-recordings2`，镜像摘要 `sha256:7b23ff68d79a4e370fcfc35fb7e70ff674fbaa2d020ec71f03670357fbea69cc`。基于现网 userlabel1 镜像纯 COPY 已编译前端，保留旧哈希资源，仅重建 gowvp。后端版本仍为 openplatformdocs1，二进制 SHA256 保持 `ed2925dfe75d498bb8182b04748e86c114509e88cc54a4925ea792f27b1ba0a5`，ZLM/PostgreSQL 的容器 ID 与启动时间未改变。

录像中心默认展示全部日期的最新录像，月历自动定位最新录像所在月，完整月份以蓝点和状态文字标记有录像日期；支持通道、整月、单日筛选，列表明确范围、总量、分页及已加载结果搜索。左侧为通道/月历，右侧为结果及页内回放；仅单日单通道显示紧凑时间轴。900px 以下上下重排，640px 以下列表为字段卡片，月历触控目标44px。事件入口读取事件时间定位对应日期，通道入口保留筛选。

线上复现旧播放入口：REC-3 的精确起止时间 HLS 查询返回404/no recordings found，而原始 MP4 支持 Range 并返回206。旧列表时间只显示到秒，再次按完全包含范围查询会遗漏底层带更高时间精度的记录；同时将 m3u8 直接打开到新窗口也不是可跨浏览器使用的播放器。新回放直接使用已有鉴权静态文件服务播放原始 MP4，先检查文件可读性，再在页内原生播放器回放，支持暂停、进度拖动和全屏；当天连续播放完整分页收集片段、按时间顺序切换并跳过空档。未修改 HLS 后端接口，其他旧客户端调用保持原样。缺失文件、格式不支持及查询失败分别提供恢复入口。

文档一致性、Vue 类型和 Vite 正式构建通过，生产 API 仍为同源 `/`；完整月历、跨年/闰年、自然日/月范围、文件路径编码及路径校验3项回归通过，既有生产API地址回归通过，CSS语法和差异检查通过。发布后518个静态文件摘要一致，通过HTTP读取入口及主资源、AppShell、RecordingsView、API/HTTP/日期工具共9个关键文件与本地产物一致。列表总数3，月历11月3日有录像，通道单日列表/时间轴均3段，三份原始文件均返回Range 206，未登录文件访问401。

按用户报告的页面与播放问题，复用内嵌浏览器进行相关路由验证：默认直接显示3条历史录像；月历标记、单日及通道筛选有效；REC-3页内播放readyState=4、1280×720、无媒体错误且进度推进；连续播放从REC-1自动切至REC-2，显示2/3、进度22秒、readyState=4且无错误。已保存 `packages/recordings2-20261104/playback.png` 对应本地证明截图；未进行移动端浏览器或设备控制验证。

配置与一致性数据库备份位于 `backups/before-recordings2-20261104/`，归档 SHA256 `74228f2ed609310d8df9bd8cfdb0bc868d82263bf57f54c922c3bd3e5ce79a62`。发布包 SHA256 `441ea3022599373f2b28a55255f6ac143fce5716ddb8f9319d97ffadd9b95e03`，包目录含前端、Dockerfile、源文件/产物摘要、带失败回滚的release.sh。正常回滚恢复本轮备份.env后仅重建gowvp至userlabel1，不恢复数据库或替换凭据。临时SSH私钥副本于交付前删除。

### 10.24 2026-11-04 录像弹窗播放器

16:01:47 发布 `owl-v1-service:v1-20261104-recordmodal1`，镜像摘要 `sha256:4f19f731ffd0e1e40f4d5a0b6017363527828d4c39566e3554ff812ef49f46ac`。录像回放由内嵌区域改为宽版 ModalDialog，列表与筛选保持原位置；保留MP4播放、进度控制、全屏、连续播放及失败恢复，删除原滚动到播放器的行为。复用遮罩/Escape/关闭按钮、背景滚动锁定和焦点恢复，关闭会暂停并卸载视频、清空队列、作废迟到响应。ModalDialog新增可选wide尺寸，其他弹窗保持默认尺寸；焦点集合纳入带controls的视频。

正式构建（文档一致性、Vue类型、Vite）、源码差异检查通过；发布后519个静态文件SHA256匹配，通过HTTP获取的入口及主资源、AppShell、RecordingsView、ModalDialog共9个关键文件与本地产物一致，并确认录像页产物启用wide弹窗。未启动浏览器，弹窗展示及键盘交互由人工验收。本次基于服务器原recordings2镜像纯COPY已编译前端，仅重建gowvp；后端SHA256保持 `ed2925dfe75d498bb8182b04748e86c114509e88cc54a4925ea792f27b1ba0a5`，ZLM/PostgreSQL容器ID和启动时间未改变。

发布包 `packages/recordmodal1-20261104.tar.gz` SHA256为 `d8c4aff8d34f4e945e7a38239653533e1e97b30cae16dc60adfc0224cf8d0fd7`。配置及一致性数据库备份位于 `backups/before-recordmodal1-20261104/`，归档SHA256为 `34a3e11fec1e0b6630012b1e47f2af3098ce30fc311b67507c4bd557737be5e7`。发布脚本带失败自动回滚；正常回滚恢复本轮备份.env并仅重建gowvp至recordings2，不恢复数据库或更改凭据。临时SSH私钥副本于交付前删除。

### 10.25 2026-11-04 三方设备/完整通道编码接口

16:54:29 发布 `owl-v1-service:v1-20261104-resourcecodes1`，镜像摘要 `sha256:5a16c64f30f8541b413a2155f63fa0ac22ad18356056b0becf2e0ea0e221b6c4`，后端二进制 SHA256 为 `9a27f5548e45fc57feefc8cb12239f722c9bc794792a2758d59121dd3d15b5e4`。以服务器现有 recordmodal1 镜像为基础，仅替换后端和 `/app/www/open-platform/` 文档文件；其他页面文件逐一摘要一致，ZLM、PostgreSQL 容器 ID 和启动时间不变。HTTP 地址、Audience、数据库、凭据和设备权限配置保持原样，无数据库迁移。

新增 6 个 HMAC 业务入口：按设备编码查设备；按设备和完整通道编码查通道、查云台能力、控制云台、创建播放会话、创建抓拍任务。通道路径为 `/openapi/v1/devices/by-code/{device_code}/channels/{channel_code}`，能力/控制/播放/抓拍分别追加 `/ptz/capabilities`、`/ptz`、`/live-sessions`、`/snapshot-tasks`。设备及通道列表增加编码精确筛选；旧内部 ID 入口和 `device_id` 参数语义保持。通道编码采用设备上报完整编码，不推测录像机序号。

编码先在应用设备授权范围内解析，再按父设备内部 ID 和完整通道编码定位；不使用冗余设备编码判定归属。不存在或无权限统一 404，同设备重复通道编码返回 409，不选择任意目标。控制入口只要求原动作 Scope，不额外要求读取 Scope；授权仍绑定内部设备 ID，同编码删除重建不会继承旧授权。复用原业务权限、动作限制、幂等、停止与任务流程，编码播放/抓拍禁止同时传内部 channel_id。在线文档、OpenAPI、参数、响应示例及错误表同步，接口操作从 56 增至 62。

本地开放 API 包测试、针对编码和签名隔离的 race 检查、Linux/amd64 构建、文档一致性及源码差异检查通过。回归覆盖跨设备同通道编码、未授权、重复编码、Scope、幂等、模拟播放/抓拍/云台和同编码重建设备授权隔离。线上 8 个文档下载 SHA256 与产物一致，真实 AK/SK 签名只读设备/通道编码查询、能力查询、列表筛选和原 ID 查询正常；不存在编码 404，无签名和 Bearer 请求 401。凭据仅在验证进程内存中使用，不输出、不持久化。4 台设备全部在线、128 个通道保留，启动日志未发现 ERROR/panic/fatal；未启动浏览器、未触发真实云台、抓拍、播放或外部告警。

更新前 `.env`、Compose、容器信息、前端/后端摘要和一致性 PostgreSQL 备份位于 `backups/before-resourcecodes1-20261104/`，PG dump 通过 `pg_restore --list`，SHA256 为 `e28fa7336c6b64d0ff82aef66766df468975e5b10fbb1fdc6d5f49b40aa947b9`。交付目录 `packages/resourcecodes1-20261104/` 含二进制、文档、Dockerfile、源文件/产物摘要及带失败回滚的发布脚本。上传包 SHA256 为 `9a387eb6c1c3c29de246e67943c6493d0dcfe8f021b01a974cdd160e532424f6`；镜像归档通过 gzip 完整性检查，SHA256 为 `e8ce5ca60acc1a518c19d01ebd0961f33dc78aa07eb55adbc52f952b3d2c0453`。旧镜像保留，正常回滚将 `.env` 中 `OWL_SERVICE_IMAGE_TAG` 恢复为 `v1-20261104-recordmodal1`，只更新 gowvp，不恢复数据库或更换密钥。临时私钥副本于交付前删除。

### 10.26 2026-11-04 国标设备与通道体验优化

17:06:02 发布 `owl-v1-service:v1-20261104-deviceui1`，镜像摘要 `sha256:e2e4538587165a37270e979ef801ca30dfca486fdfc82286fddc6d82331207ef`。发布前核实线上已更新至 resourcecodes1，因此基于该最新镜像仅 COPY 已编译前端，保留旧哈希资源；后端版本仍为 resourcecodes1，二进制 SHA256 保持 `9a27f5548e45fc57feefc8cb12239f722c9bc794792a2758d59121dd3d15b5e4`。仅重建 gowvp，PostgreSQL/ZLM 容器 ID 和启动时间不变，无数据库迁移。

设备列表按名称、在线状态、通道数及接入链路优先排列；正常在线不重复注册说明，异常保留原因，协议版本去重复。设备名称和带提示/无障碍名称的箭头可进入详情。设备详情默认通道资源，横向分区替换侧栏及深蓝摘要；统一通道表头，设备下全部通道支持名称/编码及在线状态检索与分页。设备信息、运维和诊断保持独立，低频基础/高级配置及原始快照按需展开。通道详情预览优先、录像入口前移，收纳播放地址、语音参数及升级任务，不支持云台时不展示禁用方向盘。900px以下重排，640px以下通道表转字段卡片，导航可横向滚动，触屏操作44px；保留协议能力及设备控制门禁。

34项相关既有检查、文档一致性、Vue类型、Vite构建、CSS语法、生产API根路径及差异检查通过。发布后519个静态文件摘要一致，入口、主JS/CSS、设备列表、设备/通道详情、AppShell、API/HTTP与录像页共10项HTTP资源匹配本地产物。管理员加密登录后只读核对：4台设备、128路通道；目标设备32路，分页25+7无重复，名称搜索1条，在线30/离线2，未登录设备接口401。凭据/JWT仅在进程内存中使用，未输出或保存；无浏览器验收或真实云台、抓拍、播放、配置下发等设备操作。

配置与一致性数据库备份位于 `backups/before-deviceui1-20261104/`，归档 SHA256 `b098e36ded9152a362432eaf7d8cb89ac7d6cf2b2f8b03c3bb0ab76a999d6d32`。发布包 `packages/deviceui1-20261104.tar.gz` SHA256 `48db42b2b1edb3a701635df642111c6512e2bfef3680a1435c874f6a16327cf0`；目录包含前端、Dockerfile、源文件/产物摘要、构建/检查日志及带失败回滚的release.sh。正常回滚恢复本轮备份.env后仅重建gowvp至resourcecodes1，不恢复数据库或更换凭据。临时SSH私钥副本于交付前删除。

### 10.27 2026-11-04 设备与通道编码业务覆盖补齐

17:13:17 发布 `owl-v1-service:v1-20261104-resourcecodes2`，镜像摘要 `sha256:0f612866d0626d9a1eb0a0b9c4028cfda2d06fff06376864e82f0f8711721ab2`，后端二进制 SHA256 为 `2d964553bf10ab2cd4817222578a941ec661836c8d986e342f84a750fb0e164f`。发布前重新核对现网已为 deviceui1，因此基于该镜像仅替换后端与开放平台文档；所有非文档页面资源 SHA256 保持一致。ZLM、PostgreSQL 容器 ID 和启动时间未改变，无数据库迁移，HTTP 地址及原应用配置保持。

上一轮只补齐设备/通道查询、云台、播放和抓拍创建，未覆盖录像、告警、任务记录和订阅。本次新增设备编码下的通道列表，设备级与通道级 `/recordings`、`/events`、`/live-sessions`、`/snapshot-tasks`、`/ptz-commands` 列表，以及原顶层会话/任务/命令 GET 列表，共增加 14 个接口操作。原通道、录像、告警、会话、任务、命令列表统一支持 device_code/channel_code 精确筛选；原播放/抓拍创建 JSON 支持编码对，订阅创建/修改支持 channel_codes 数组，均保留内部 ID 兼容。订阅编码在保存时解析为内部通道绑定，空数组表示全部授权资源、null 或混用 channel_ids 拒绝；不会因同码重建设备自动继承绑定。

具体录像、告警、会话、任务、命令、订阅的结果 ID 仍用于选定结果的详情、票据、停止/释放及修改，同一通道多条结果不能仅用编码推断。按编码查列表后可直接使用结果 ID，无需查询设备或通道内部 ID。列表未知编码返回空，所有条件取交集；严格编码路径未知/未授权统一 404、重复通道编码 409，路径与额外资源定位参数冲突返回 400。业务 Scope 与设备授权保持，不额外要求读取 Scope；其他应用记录不可见，使用实体序列化保留隐藏凭据、路径和媒体来源规则。列表支持时间、游标及会话/任务/命令状态筛选。公开文档优先显示编码接口，原设备/通道 ID 页面保留并标记兼容；契约共 76 个操作，与实际路由双向一致。

本地 API 包测试、编码/签名隔离目标 race 检查、Linux/amd64 构建、文档生成一致性和本轮源码差异检查通过。新增回归覆盖所有记录列表、跨应用/设备隔离、空/未知编码不扩大筛选、路径冲突、游标路径绑定、Scope、编码请求体与订阅解析/互斥/null/空数组。线上真实 AK/SK 签名核对：编码路径与查询参数结果一致，按编码检索到现有 3 条录像；告警、会话、抓拍任务和云台命令列表正常返回空，设备级列表正常，未知编码空列表、路径冲突 400、未知编码路径 404、未签名 401 和旧 ID 兼容均通过。8 个公开文档文件 SHA256 与产物一致。4 台设备全部在线，128 个通道保留，启动日志未发现 ERROR/panic/fatal；未启动浏览器、未触发真实云台/抓拍/播放或改动线上订阅。SK 与主密钥仅在验证进程内存处理，不输出、不持久化。

备份位于 `backups/before-resourcecodes2-20261104/`，包含环境/Compose、容器及页面摘要和一致性 PG dump，`pg_restore --list` 通过；dump SHA256 为 `47878203875f7b39161d1edda976a0be955de89daed1f3beba4179e70350e7ef`。发布目录 `packages/resourcecodes2-20261104/` 含二进制、文档、Dockerfile、源文件/产物摘要、带失败回滚的 release.sh 和镜像归档。上传包 SHA256 `de17ceed39156bebf0d1af3e62a96c6b4a54401378af592d781b8a11a35d6270`；镜像归档通过 gzip 完整性检查，SHA256 `451aa96c5f03c8bf853cc379f8e0c5c6fe17464b10ca924e2a591e893f7fcb1d`。旧镜像保留，正常回滚将 `.env` 的 OWL_SERVICE_IMAGE_TAG 恢复为 `v1-20261104-deviceui1`，只更新 gowvp，不恢复数据库或更换密钥。临时 SSH 私钥与验证脚本于交付前删除。

### 10.28 2026-11-04 设备详情 Tab 归属与通道分列修正

17:27:08 发布 `owl-v1-service:v1-20261104-deviceui2`，镜像摘要 `sha256:828302075c12127a8ff88914b3408f1171251502fc3a930ce8a8f0ef2ea4850f`。基于现网 resourcecodes2 仅 COPY 前端，后端版本及二进制 SHA256 保持 `2d964553bf10ab2cd4817222578a941ec661836c8d986e342f84a750fb0e164f`；仅重建 gowvp，PostgreSQL/ZLM 容器 ID、启动时间不变。

修正工具栏左右贴边及查询框分配宽度未填满的问题。设备各分区统一为包含Tab与当前内容的连续白色面板，移除中间灰色间隔与通道列表独立卡片；桌面内容内边距16px、窄屏12px。通道名称和通道编码改为独立列，桌面不再上下堆叠，窄屏保留各自字段；搜索、状态筛选、分页及设备控制语义保持。

文档一致性、Vue类型、Vite生产构建、CSS语法、生产API根路径和源码差异检查通过。发布后519个静态文件摘要与产物一致，入口、主JS/CSS、DeviceDetailView、AppShell、HTTP共6项HTTP资源摘要匹配；容器健康，未登录设备接口401。本次为样式和局部模板修正，未新增或运行业务测试，未启动浏览器，页面效果由人工验收。

配置及一致性数据库备份位于 `backups/before-deviceui2-20261104/`，归档SHA256 `ef582b8383bbdb7bf7124c2afa947d23a0fb613c048ef04344bcfa7b45db9d41`。发布包 `packages/deviceui2-20261104.tar.gz` SHA256 `ab0c39a089cb0f91f04105556504e0c4f68b642494a94b761b10762e7fed2258`；目录含前端、Dockerfile、摘要、构建日志及失败回滚脚本。回滚恢复本轮备份.env后仅重建gowvp至resourcecodes2，不恢复数据库或更换凭据。临时SSH私钥副本于交付前删除。

### 10.29 2026-11-04 设备列表操作列右侧留白

17:36:23 发布 `owl-v1-service:v1-20261104-deviceui3`，镜像摘要 `sha256:235c3b577891a908a45d837a26579414128ac481d5c42a01e963497bb546f93a`。操作列原6%宽度扣除单元格内边距后不足以容纳两个图标按钮，导致向右溢出；现桌面固定104px，左右16px留白，按钮间距8px并右对齐，首列承接剩余宽度。桌面触屏列宽128px以容纳两个44px按钮；900px以下卡片布局保持。

生产构建（文档一致性、Vue类型、Vite）、CSS语法、生产API根路径及差异检查通过。发布后519静态文件摘要、入口/主JS/CSS/DevicesView/AppShell/HTTP共6项HTTP资源匹配，容器健康。局部样式未新增或运行业务测试，未启动浏览器，效果由人工验收。仅基于现网deviceui2覆盖前端，后端resourcecodes2及SHA256 `2d964553bf10ab2cd4817222578a941ec661836c8d986e342f84a750fb0e164f` 保持；PG/ZLM容器ID和启动时间不变。

发布包 `packages/deviceui3-20261104.tar.gz` SHA256 `0ba5ec43fd1016b8c20d6d5deb9a272bdfb9516af96d4568af638154fd905930`。配置及一致性数据库备份 `backups/before-deviceui3-20261104/`，归档SHA256 `8f42b9537f971c559fdf968e79d1715f765a757088cb6e294ed723d8ccfdea8c`。发布脚本具备失败回滚；正常回滚恢复本轮备份.env后仅重建gowvp至deviceui2，不恢复数据库或更换凭据。临时SSH私钥副本于交付前删除。

### 10.30 2026-11-04 心跳与注册记录固定头部

17:40:21 发布 `owl-v1-service:v1-20261104-historymodal1`，镜像摘要 `sha256:fd2cd297acc449c8c3ff4e9bf101abbe334d573c2221ffcf25feb0aaa62a70f8`。ModalDialog新增可选scrollBody模式，设备列表及详情的心跳/注册记录统一启用：头部标题、说明及关闭按钮不参与滚动，仅内容区域滚动；内容区可键盘聚焦，保留焦点约束、Escape和关闭后焦点恢复。其他弹层默认布局保持。

文档一致性、Vue类型、Vite生产构建、组件CSS语法、生产API根路径及源码差异检查通过。发布后519静态文件摘要、入口/主资源/ModalDialog及两设备页面等10项HTTP资源匹配；编译产物包含scrollBody模式，容器健康。局部布局未新增或运行业务测试，未启动浏览器，实际滚动与页面效果由人工验收。基于现网deviceui3仅覆盖前端；后端保持resourcecodes2，二进制SHA256 `2d964553bf10ab2cd4817222578a941ec661836c8d986e342f84a750fb0e164f`，PG/ZLM容器ID及启动时间不变。

发布包 `packages/historymodal1-20261104.tar.gz` SHA256 `4901d2ed2eb22ea422cacd480f5a9a71f9a8f99db921c35dc383da11b2d497fe`。配置及一致性数据库备份 `backups/before-historymodal1-20261104/`，归档SHA256 `a4d7ba637d73870228d25fce64e03f87967368caab311b092788a6ed8152a5b6`。发布脚本提供失败回滚；正常回滚恢复本轮备份.env后仅重建gowvp至deviceui3，不恢复数据库或更换凭据。临时SSH私钥副本于交付前删除。

### 10.31 2026-11-04 通道详情 Tab 连续面板

17:46:53 发布 `owl-v1-service:v1-20261104-channeltabs1`，镜像摘要 `sha256:aee58b49fb6e96ee83d8f6eadd940e3ad7badaaf4fc812854ddbfbaaf36f8c5e`。移除通道详情Tab与当前内容之间的14px间隔，统一连续白底、外框和上下圆角，五个分区共享桌面16px、640px以下12px内边距；保留分区切换、键盘导航和业务操作。

文档一致性、Vue类型及Vite生产构建、CSS语法、生产API根路径、发布脚本语法和源码差异检查通过。发布后519个静态文件摘要与产物一致，入口/主JS/CSS/ChannelDetailView/AppShell/HTTP共6项HTTP资源匹配，容器健康。未启动浏览器或运行业务测试，页面效果由人工验收。基于现网historymodal1仅覆盖前端，后端resourcecodes2及二进制SHA256 `2d964553bf10ab2cd4817222578a941ec661836c8d986e342f84a750fb0e164f` 保持，PG/ZLM容器ID和启动时间不变。

发布包 `packages/channeltabs1-20261104.tar.gz` SHA256 `d41d95aa4fd964e5a5e6608fc49f6fc268152de9c1353b25dd05489dcddbf43e`。配置及一致性数据库备份 `backups/before-channeltabs1-20261104/`，归档SHA256 `f173f76df59f7c6153f6fa6fca5411f1a5526960066e38d1aacc6459946a1c35`。发布脚本具备失败回滚；正常回滚恢复本轮备份.env后仅重建gowvp至historymodal1，不恢复数据库或更换凭据。临时SSH私钥副本于交付前删除。

### 10.32 2026-11-04 通道录像按云端与设备本地分流

18:13:52 发布 `owl-v1-service:v1-20261104-recordworkspace1`，镜像摘要 `sha256:7851b6d61fea20f467f00bb6b53b2d7926f221c9dc6b51b0cff12c789f95f5af`，后端二进制SHA256 `92bc846a83dc12c3e880e82b54b0886f4ff9701d3d751664da148b9b71b0496d`。基于现网channeltabs1镜像覆盖本轮前后端；为避免带入其他线程的在途PTZ/预览修改，构建源使用已部署resourcecodes2对应Git 8e841d9基线，加本轮录像增量及已发布historymodal1/channeltabs1前端修正。实际相关构建源保存在发布目录source/，含SOURCE-SHA256SUMS；工作区其他修改保留。仅重建gowvp，PG/ZLM容器ID和启动时间不变，无数据库迁移。

录像与回看分为平台云端录像和设备本地录像。云端提供保存后录制状态、日期筛选、分页、MP4弹窗回放和下载；设备本地提供月份目录查询、有录像日期标记、当天录像时段及分页，未查询/无录像/失败/分包不完整分别提示，常用列表优先，下载与自定义会话收纳在高级区域。点击时段按原始秒级边界发起历史推流，并使用新返回的兼容playback字段打开带鉴权播放器。设备回放与直播、RTP下载采用不同收流标识，复用媒体节点绑定与播放鉴权；关闭弹窗/离开通道停止本次回放，迟到成功响应补做停止，失败保留恢复入口。900px以下上下重排、640px以下表格转字段行，触屏日期目标44px。

针对历史收流标识、媒体清理不影响直播/下载、回放地址媒体节点及token隔离、历史控制和目录响应等目标Go检查通过；既有录像日期/路径3项测试通过。文档一致性、Vue类型、Vite生产构建、CSS语法、API根路径及发布脚本语法检查通过。发布后519静态文件摘要与产物一致，入口/主JS/CSS/ChannelDetailView/AppShell/HTTP共6项HTTP资源匹配，两个录像来源文案存在于线上产物；未认证目录/回放入口均返回401，容器健康。未启动浏览器，未向真实设备发送查询/回放/云台命令，实际目录、推流画面及暂停/跳转由人工验收。

发布包 `packages/recordworkspace1-20261104.tar.gz` SHA256 `7e03a117be8e5fdd4048af5276c0c96db00cd0f8b0ce00dcc640b2b3261655f8`。配置及一致性数据库备份 `backups/before-recordworkspace1-20261104/`，归档SHA256 `7372a0a48211866a0520857f91173cd29bdcd8d9aa4d1ac6cf14f398d3575891`。发布脚本带失败回滚；正常回滚恢复本轮备份.env后仅重建gowvp至channeltabs1，不恢复数据库或更换凭据。临时SSH私钥副本于交付前删除。

### 10.33 2026-11-04 v2.1.1 全量应用发布与版本规范

18:25:50 发布 `owl-v1-service:v2.1.1`，统一纳入当前工作区全部源码，包括 PTZ 探测/反馈/自动停止、录像回放流隔离、播放器和布局调整。版本从根目录 `VERSION` 读取，统一为 `v主版本.次版本.修订号`，镜像与后端版本一致，构建信息独立记录。相关 Go 六包、前端构建与 51 项测试通过；线上二进制和 519 个静态文件及 32 项 HTTP 资源一致，真实登录与相关只读接口通过，4 台设备在线、128 通道保留。仅重建 GoWVP，PG/ZLM、配置及数据保留。发布包、在线一致性备份、实际检查限制与回滚命令见 [v2.1.1 发布记录](releases/v2.1.1.md)。

### 10.34 2026-11-04 v2.1.2 设备图标按钮

18:34:54 发布 `owl-v1-service:v2.1.2`，设备列表与详情的图标操作、返回和分页按钮无边框，箭头20px，保留交互反馈与触屏区域。从v2.1.1线上源码快照构建，仅样式与VERSION变化，后端业务源码保持。生产构建、519静态文件及8项HTTP资源校验通过，健康接口v2.1.2；PG/ZLM容器及配置保持。未运行浏览器或业务测试，视觉效果由人工验收。产物、备份及回滚见 [v2.1.2 发布记录](releases/v2.1.2.md)。

### 10.35 2026-11-04 v2.1.3 播放器统一与全部工作区改动发布

19:44:58 发布 `owl-v1-service:v2.1.3`，全部工作区源码固定快照构建。统一五类播放入口的组件、浅灰状态、点播控制及实例清理，纳入实时监控布局和共享 PTZ 控件；相对 v2.1.2 后端业务源码无新增差异，重编译版本元数据。10 项相关检查、正式构建、Linux 编译及线上后端/522 前端文件/23 项 HTTP 资源核验通过，真实管理员登录与相关只读接口正常，4 台设备全部在线、128 通道保留。仅重建 GoWVP，PG/ZLM 与配置保持，在线一致性备份完成；真实视频及设备操作由人工验收。实际产物、备份与回滚见 [v2.1.3 发布记录](releases/v2.1.3.md)。

### 10.36 2026-11-04 v2.1.4 播放器圆形图标入口

21:25:03 发布 `owl-v1-service:v2.1.4`，全部未发布工作区改动从固定源码快照纳入。播放器空态与重试统一为播放图标加圆形边框，无按钮文字，保留提示与无障碍名称；分屏样式同步。前后端正式构建通过，后端与522个前端文件、23项HTTP资源匹配，真实管理员登录及相关只读接口正常，4台设备全部在线、128通道保留。仅重建GoWVP，PG/ZLM与配置保持，配置和在线一致性数据库备份完成。视觉效果人工验收；产物与回滚见 [v2.1.4 发布记录](releases/v2.1.4.md)。

### 10.37 2026-11-04 v2.2.0 通道实际推流状态

22:57:43 发布 `owl-v1-service:v2.2.0`，设备详情通道表新增推流中、未推流、未知三态及手动刷新。按节点实际直播流判断，排除历史回放和下载；可选接口字段保持旧调用兼容，无数据库迁移。3项针对性测试、前后端正式构建、522静态文件和23项HTTP资源摘要核验通过，线上128通道与媒体节点逐一核对一致。首次因核对脚本容器域名解析失败自动回滚，修正脚本后使用同一镜像发布成功。仅重建GoWVP，PG/ZLM与配置保持。源码、产物、备份及回滚见 [v2.2.0 发布记录](releases/v2.2.0.md)。

### 10.38 2026-11-04 v2.2.1 平台运维分组移除图标

23:54:22 发布 `owl-v1-service:v2.2.1`。移除平台运维文字左侧扳手图标，保留展开箭头；侧栏收窄后仍有可见展开入口。以v2.2.0快照为基线，仅AppShell、局部CSS和版本变化。构建及发布校验通过，后端与521前端文件、23项HTTP资源匹配，服务健康，4台设备及128通道保留；PG/ZLM及配置不变。视觉效果人工验收，产物和回滚见 [v2.2.1 发布记录](releases/v2.2.1.md)。

### 10.39 2026-11-05 v2.2.2 热门API列宽

00:04:25 发布 `owl-v1-service:v2.2.2`。热门API表右侧请求量固定112px右对齐，地址超长省略、悬停title显示全文，局部覆盖全局表格最小宽度。基于v2.2.1快照仅页面及版本变化；构建及发布检查通过，后端和522前端文件、25项HTTP资源摘要匹配，4台设备及128通道保留，PG/ZLM和配置保持。未运行浏览器，页面效果人工验收；产物及回滚见 [v2.2.2 发布记录](releases/v2.2.2.md)。

### 2026-11-05 v2.2.3 水印开关修复

00:11:43 发布v2.2.3，仅基于v2.2.2修正两个水印开关label关联，开启后可编辑下方字段。正式构建与线上文件/26项HTTP资源核验通过，内嵌浏览器实际点击及输入验证通过；未保存测试配置，真实水印渲染人工验收。4台设备在线、128通道保留，PG/ZLM与配置保持。详见[v2.2.3发布记录](releases/v2.2.3.md)。

### 2026-11-05 v2.3.0 全量应用与媒体节点管理

00:40:28 发布 `v2.3.0`，以全部未提交源码的固定提交 `350afea` 构建，新增媒体节点六分区与 14 类只读查询；此前已上线的 PTZ、录像、播放器及界面修改同步纳入 Git。63 项前端测试、相关 Go 回归、生产构建和 Linux/amd64 编译通过。配置与 PG 一致性备份完成，仅重建 GoWVP，PG/ZLM 和配置保持。525 个前端文件及 45 项 HTTP 资源匹配，外部健康接口正常；12 类节点查询成功，4 台设备在线、128 通道保留。当前无活动流，单路媒体详情与观看连接待人工验收。源码、摘要、备份与回滚见 [v2.3.0 发布记录](releases/v2.3.0.md)。

### 2026-11-05 v2.3.1 系统指标与播放器摘要

08:32:51 发布 `v2.3.1`，修复协程历史数组直接显示导致布局撑开的问题，新增实时协程数量并保留旧接口兼容；播放器策略摘要改为浅灰底、深色文字。固定源码正式构建通过，后端与 525 前端文件、29 项 HTTP 资源校验一致；线上实时数量返回整数，外部健康接口正常，4 台设备在线、128 通道保留。配置与 PG 一致性备份完成，仅更新 GoWVP，PG/ZLM 及配置保持。详见 [v2.3.1 发布记录](releases/v2.3.1.md)。

### 2026-11-05 v2.3.2 SIP 设置页内导航

09:24:07 发布 `v2.3.2`，将 SIP 设置菜单的普通锚点改为路由链接，修复误跳转，按锚点更新高亮，并统一菜单与内容顺序、标题定位。此前 23 项相关源码契约检查通过；本次固定快照的文档一致性、Vue 类型、正式前端构建和 Linux/amd64 编译通过，后端与 525 个前端文件、30 项 HTTP 资源校验一致，外部健康及网页正常。配置与 PG 一致性备份完成，仅重建 GoWVP，PG/ZLM 未重启、配置保持，4 台设备在线、128 通道保留。页面交互由人工验收。详见 [v2.3.2 发布记录](releases/v2.3.2.md)。

### 2026-11-05 v2.3.3 SIP 菜单滚动

09:46:11 发布 `v2.3.3`，桌面 SIP 页内菜单随页面滚动停靠顶栏下方，限制高度并支持菜单内部纵向滚动，窄屏保留横向导航。固定快照构建、类型及正式 CSS 检查通过；二进制与 525 个前端文件、30 项 HTTP 资源校验一致，外部健康及网页正常。配置与 PG 一致性备份完成，仅重建 GoWVP，PG/ZLM 未重启、配置保持，4 台设备在线、128 通道保留。滚动交互由人工验收。详见 [v2.3.3 发布记录](releases/v2.3.3.md)。

### 2026-11-05 v2.3.4 通道资源折叠按钮

10:22:31 发布 `v2.3.4`，资源标题与收起按钮固定同排，统计独占下一行；X替换为侧栏折叠图标，顶部入口同步展开/收起语义。基于v2.3.3快照仅LiveView及版本变化，构建与源码检查通过；后端及526前端文件、30项HTTP资源匹配，服务健康。只重建GoWVP，PG/ZLM与配置保持，备份完成，4台在线设备及128通道保留。页面交互由人工验收，详见 [v2.3.4 发布记录](releases/v2.3.4.md)。

### 2026-11-05 v2.3.5 设备状态标签宽度

11:11:06 发布 `v2.3.5`，国标设备表状态标签按内容宽度靠左，修复网格拉伸带来的背景空白。基于v2.3.4快照仅单条局部CSS与版本变化，构建和源码检查通过；后端与526前端文件、30项HTTP资源匹配，服务健康。仅重建GoWVP，PG/ZLM及配置保持，备份完成，4台在线设备与128通道保留。页面效果由人工验收，详见 [v2.3.5 发布记录](releases/v2.3.5.md)。

### 2026-11-05 v2.4.0 永久抓拍与定时抓拍

11:33:59 发布 `v2.4.0`，新增永久抓拍、图片预览/下载/重命名/删除、播放器封面及每通道定时计划；按协议直接选择上传或实时流取帧，上传等待默认 120 秒、FFmpeg 默认并发 2 路，可管理端配置。图片保存在既有大盘 `/www/gowvp-data/cover/captures/`，默认永久保留，计划默认关闭；纳入 SIP 设置分段标题样式。固定源码正式构建、相关快速检查通过，配置与 PG 在线一致性备份完成，数据库迁移至 `0.0.29`，新增两表及三个索引。后端与 531 个前端文件、34 项 HTTP 资源匹配，管理员登录、抓拍配置/历史/计划读取及鉴权正常；4 台设备全部在线、128 通道保留。仅重建 GoWVP，PG/ZLM 未重启，固定应用 IP 及配置保持。未触发真实相机抓拍、计划或业务图片删除，相关效果由人工验收。产物、备份及回滚详见 [v2.4.0 发布记录](releases/v2.4.0.md)。
