# 192.168.6.20 专用运行文件

用于 `192.168.6.20`（EasyTier：`10.144.0.20`），部署设计和实施记录见 [服务器部署方案](../server-192.168.6.20.md)。本目录不含真实凭据或业务数据库。

当前版本（2026-11-04）：Owl 为 `v1-20261104-queryindex1`，ZLM 保持 `v1-20261103-manualrecord1`，独立 PostgreSQL 17.11（`owl-v1-postgres:17-20261104`）。设备、通道、录像索引、心跳和注册历史已从 SQLite 迁移到 PostgreSQL；已补齐录像与通道复合索引，并移除时间轴/月统计的固定条数截断。默认手动录像、媒体 `30000-50000/tcp+udp`、无人观看默认 180 秒断流等行为保留。日常启动命令：

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

更新镜像前完成备份，导入对应镜像，再修改 `.env` 标签，执行 `./run.sh up`。`OWL_SERVICE_IMAGE_TAG`、`OWL_ZLM_IMAGE_TAG` 优先于 `OWL_IMAGE_TAG`；当前分别为 `v1-20261104-queryindex1`、`v1-20261103-manualrecord1`。PostgreSQL 通过独立变量 `OWL_POSTGRES_IMAGE` 固定版本，升级数据库大版本需单独执行迁移，不能直接换标签。不要使用通用 `deploy.sh release/up` 启动本服务器，不要重新执行首次准备脚本。

本次仅更新后端时使用 `docker compose -p owl-v1 --env-file .env -f docker-compose.yml up -d --no-deps --no-build --pull never --wait gowvp`，保留 ZLM、PG 容器。`sql/20261104-recording-query-indexes.sql` 已执行，两条索引均有效。交付目录 `packages/queryindex1-20261104/` 包含服务镜像归档、二进制及校验文件；更新前备份在 `backups/before-queryindex1-20261104/`。需要回滚时只把 `OWL_SERVICE_IMAGE_TAG` 恢复为 `v1-20261104-postgres1` 并更新 `gowvp`，兼容索引保留，不恢复旧库覆盖新数据。

## 维护与访问控制

```bash
systemctl list-timers 'owl-v1-*'
systemctl start owl-v1-backup.service
journalctl -u owl-v1-backup.service -n 30 --no-pager
systemctl start owl-v1-monitor.service
journalctl -u owl-v1-monitor.service -n 30 --no-pager
```

启用 timer 后，每日 03:30 左右使用 PostgreSQL 容器内同版本 `pg_dump` 生成在线一致性备份（`gowvp.dump`），并执行 `pg_restore --list` 检查，连同配置和凭据归档；保留约 7 天日备份、4 周周备份，位置为 `backups/daily`、`backups/weekly`。旧日期的备份仍可能包含 SQLite `configs/data.db`，恢复前先确认备份格式。归档含密钥，保持目录 `0700`、文件 `0600`，不得提交 Git；不含录像。定期异机备份的目标地址仍需另行配置。

心跳、成功注册事件写入 `device_history_records`，按设备、事件类型、时间建立联合索引；设备当前在线状态、最近心跳及注册有效期仍保存于 `devices`。沿用现有保留设置：每台设备每种事件最多 1000 条、最长 30 天，两者先到先清理，可在 SIP 设置的历史保留区修改。当前不引入 TimescaleDB 或分区，详见 [PostgreSQL 与时序历史方案](../postgresql.md)。

每小时检查磁盘与媒体目录预算，将超过 80% 的告警写入系统日志；清理超过 3 天的 `.video-cache/*.mp4`。ZLM HLS 当前为 3 个切片、额外保留 5 个切片、关闭流后延时清理；缓存目录预算 100GiB。以上不构成硬配额或外部通知。原始录像由应用按 3 天、磁盘 80% 阈值清理，默认分片 300 秒。

ZLM 使用宿主机网络，直接监听 `554/1935/8080` 和媒体端口，`30000-50000/tcp+udp` 不再经过 Docker 端口映射。`access.sh` 对 GoWVP 网桥 `br-owl-v1` 继续允许所有来源；宿主机 INPUT 同样须保持所有来源允许。已有业务占用的动态端口由系统绑定检查跳过，不停止其他服务。

GoWVP 保留现网桥接地址 `172.19.0.3`，网桥子网固定 `172.19.0.0/16`、网关 `172.19.0.1`，避免更新容器改变 IP 导致录像机已有 UDP DNAT 会话投递到旧地址。此固定网段只适用于本服务器，迁移时应重新核对。

录像默认关闭，通道详情“录像”页手动开启或停止。未开启手动录像的流在无人观看 180 秒后停止推流；管理端“SIP 设置 → 视频断流”可调整秒数，保存后对新一轮计时生效。配置写入 `Media.StreamNoneReaderDelaySeconds`。手动录像期间不自动断流，停止录像后重新计时。两个容器的录像路径统一为 `/app/configs/recordings`，确保 MP4 完成回调可入库。

2026-11-03 已用 BH1-1-1 验证手动录像约 205 秒、停止后 MP4 入库和下载，以及无人观看约 180 秒后发送 BYE、释放流和 RTP 接收端；验证过程中四台设备保持真实心跳在线。测试录像已停止，管理端需要刷新以加载新设置。

ZLM 持久化配置 `data/zlm-config/config.ini` 中，`[http]` 的 `allow_ip_range` 置空（允许任意 IP），`[general]` 的 `listen_ip=0.0.0.0`，`[rtc]` 的 `interfaces` 为空。迁移或重新初始化 ZLM 配置时须保留这些值；镜像默认的 HTTP 内网白名单需要清空。API 密钥、管理登录及推流认证不受影响。`rtc.externIP` 和 `Media.SDPIP` 是媒体宣告地址，并非来源白名单；当前仍宣告 `192.168.6.20`，播放端须有到该地址的路由。

`prepare-host.sh` 仅用于容器运行时为空的首次准备，已部署后会拒绝重复执行。它保留原 containerd 数据与配置，将新镜像存储放在 `/www2/containerd`，并增加挂载依赖；不要在已有容器运行后再次迁移。
