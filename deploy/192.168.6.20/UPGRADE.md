# 应用签名升级

此功能用于 `/www2/gowvp-v1` 的 Linux/amd64 Docker Compose 部署。发布源是本平台的本地签名发布库，不读取上游 GitHub，不自动下载或安装。其他部署未配置宿主机服务时，页面明确提示不可用。

管理员打开“平台运维 → 版本升级”，选择 `.owl-release.tar.gz` 后点“上传并校验”。验证通过后，页面展示目标版本、更新说明及兼容来源版本。选择发布包，点“安装所选版本”，确认维护时间后执行。上传本身不会修改运行服务。

执行阶段：校验 → 配置和数据库备份 → 导入镜像 → 切换应用 → 健康检查。期间实时预览可能中断，页面自动重连；进度和最后执行结果保存在宿主机，刷新页面不会丢失。健康检查、镜像或配置核对失败时自动切回旧应用。自动回滚失败会禁止继续安装，需运维检查。

## 首次配置

应用不挂载 Docker Socket。宿主机 `owl-upgrader.service` 通过权限为600的 Unix Socket 提供专用升级操作，应用只挂载 socket 目录。签名私钥、发布库、任务及备份不挂载至应用容器。

1. 编译 `./tools/upgrade-agent` 和 `./tools/release-package`，安装到 `upgrader/bin/owl-upgrade-agent`、`upgrader/bin/owl-release-package`，权限700。
2. 创建权限700的 `upgrader/signing`；以 root 执行以下初始化命令，仅首次使用，已有密钥时禁止覆盖：

```bash
cd /www2/gowvp-v1
upgrader/bin/owl-release-package -key upgrader/signing/release.pem -init-key
install -m 600 upgrader/signing/release.pem.public.pem upgrader/release-public.pem
```

3. 安装本目录 `owl-upgrader.service` 到 `/etc/systemd/system/`，执行 `systemctl daemon-reload` 和 `systemctl enable --now owl-upgrader`。
4. 为现有 Compose 的 gowvp 服务增量添加本目录所示 `OWL_UPGRADER_SOCKET` 环境变量及 socket 目录只读挂载，保留现网其余配置。目录必须由宿主服务提前创建。只重建 gowvp，不重启 PostgreSQL/ZLM。

签名私钥须另行安全备份，仅授权运维签包；升级服务只读取公钥。部署任务与页面升级共用 `/run/lock/owl-v1-release.lock`，人工发布也必须持有该锁。`upgrader/agent.lock` 防止多个升级服务同时启动。

## 制作发布包

先以固定源码构建镜像，镜像标签及 `org.opencontainers.image.version` 标签必须一致。例如，发布 v2.0.3，仅允许从 v2.0.2 升级：

```bash
cd /www2/gowvp-v1
upgrader/bin/owl-release-package \
  -key upgrader/signing/release.pem \
  -image owl-v1-service:v2.0.3 \
  -from v2.0.2 \
  -notes '本次更新说明' \
  -out packages/v2.0.3.owl-release.tar.gz
```

工具输出发布包 SHA256；输出已存在会拒绝覆盖。发布包严格包含 `manifest.json`、Ed25519签名 `manifest.sig`、`docker save` 镜像 `image.tar`，不接受包内安装脚本。清单限定产品、Linux/amd64、唯一目标镜像及摘要、兼容来源版本。压缩包最大2GiB，镜像归档最大6GiB。上传盘至少需7GiB空闲，安装前另查 Docker 数据盘余量。

仅支持无数据库迁移的兼容应用升级。清单 `migration` 必须为 false；签发者须审核固定源码与数据库兼容性，签名不替代该审核。普通升级不允许降级、同基础版本覆盖或未列入兼容范围的来源版本。仅编号体系调整时，运维可显式添加 `-replaces-version <唯一来源版本>`；签名清单必须仅允许该一个来源，页面显示编号调整说明。本次按用户要求从 v2.4.1-openapisnapshot1 改用 v2.0.2 编号，使用此一次性兼容声明；后续如 v2.0.2 到 v2.0.3 使用普通升级。已导入同摘要镜像可复用，禁止覆盖不同摘要的同名标签。

## 接口与持久记录

- `GET /app/upgrade/status`：已验证发布包、最近任务、执行事件；只展示最近一次任务，全部历史保存在宿主机。
- `POST /app/upgrade/packages`：原始二进制，`Content-Type: application/octet-stream`，必须有 Content-Length。成功201，仅入发布库。
- `POST /app/upgrade/install`：`{"package_id":"上传返回的id"}`。成功202，后台独立执行。
- 上述接口只接受本平台管理员 Header Bearer JWT；第三方鉴权、URL令牌不能升级。JWT不会转发到宿主服务。
- `GET /app/version/check` 是状态接口的兼容路径，返回新结构；旧 `POST /app/upgrade` 返回410，不再虚报下载成功。

`upgrader/catalog/<id>` 保存验证包；`upgrader/current.json` 与 `upgrader/tasks/<id>.json` 保存任务；备份在 `backups/web-<任务id>`，含环境、配置归档、配置摘要和可读性验证过的 PostgreSQL custom dump。目录700、文件600，包含凭据，不能公开下载。

只更新 gowvp 镜像标签，并检查健康版本、镜像ID、业务配置摘要以及PG/ZLM容器身份和启动时间。回滚只恢复升级前环境文件并重建旧应用，不覆盖在线数据库和媒体文件。服务重启时，未安装任务标记失败；安装中断则核对旧/新镜像身份后恢复旧版本；发现人工切换的其他镜像时停止恢复，不覆盖人工操作。任务状态文件损坏时拒绝启动/继续安装。

## 故障处理

先查看 `systemctl status owl-upgrader`、`journalctl -u owl-upgrader` 和任务文件。不要在任务活动期间手工换镜像、编辑配置或删除发布库。若自动回滚失败，持发布锁后核对备份环境与旧镜像ID，人工恢复服务并检查健康；确认服务与数据正常后，由运维归档故障任务再解除阻塞。不要直接删除任务文件来绕过未完成操作。

发布库、镜像和备份不会自动清理；运维应保留当前版本、最近可回滚版本及必要备份，在无活动任务时按保留策略归档历史资料。
