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
