# 多 ZLM 节点录像存储

平台录像现在按生成录像的媒体节点定位文件，而不是按通道当前绑定或统一的本机目录定位。通道后来改绑，不会改变已有录像的归属。

每个片段保存 `media_server_id`、`vhost/app/stream`、录制时的 `storage_root`、ZLM 内部绝对路径 `file_path` 和根目录内相对路径 `path`。绝对文件路径不作为 HTTP URL。回调来源按节点独立密钥鉴权，节点、vhost、文件路径及开始时间构成幂等键，重试不重复新增。

## 管理端配置

在“媒体节点 → 新增 / 编辑”设置：

| 字段 | 含义 | 例子 |
| --- | --- | --- |
| ZLM 录像目录 `record_path` | ZLM 进程或容器内部的录像根目录，平台把它传给该节点的 startRecord | `/opt/zlm/recordings` |
| 平台录像挂载目录 `record_mount_path` | GoWVP 进程或容器可访问的同一存储根目录，需要读写权限 | `/mnt/edge-1/recordings` |

`local` 两项留空时继续沿用 `Server.Recording.StorageDir`，现有 Compose 的共享目录映射无需修改。若 local 的 ZLM 与 GoWVP 容器路径不同，也可分别填写两项。

远端节点须配置 `record_path` 才能启动平台录像。远程回调不再对平台本机执行文件存在性检查，因此共享存储暂时不可用仍能保存索引。播放、下载、手动删除、按保留天数和磁盘阈值清理，均通过挂载目录访问所属节点的文件；这版使用 NFS 等共享挂载，不通过 ZLM HTTP 静态目录代理文件，也不要求 ZLM 提供单文件删除 API。只配置 ZLM 的 IP 和密钥不能让平台直接读取其磁盘。

## 远端挂载示例

假定节点 ID 为 `edge-1`：

1. 在 ZLM 主机准备录像目录。若 ZLM 使用容器，把宿主机实际录像目录映射到 `/opt/zlm/recordings`，并通过 NFS 等方式提供给 GoWVP 主机。
2. **在实际的远端录像根目录**创建身份文件 `.gowvp-recording-node`，内容为 `edge-1`，例如：

   ```bash
   # 此路径是 ZLM 主机上的实际录像目录，请按部署修改。
   printf '%s\n' 'edge-1' > /data/zlm/recordings/.gowvp-recording-node
   ```

   标记文件不能在未挂载的 GoWVP 本地空目录中补建。平台不会自动创建标记；缺失或内容不匹配时，拒绝读取和删除并保留录像索引。每个节点使用独立录像根目录。

3. 把远端目录挂载到 GoWVP 宿主机，例如 `/mnt/edge-1/recordings`。确保挂载读写权限和开机依赖正确，避免 NFS 掉线时写入本地空目录。
4. GoWVP 使用 Docker 时，再把这个挂载目录传入容器；必须填**容器内**的路径，例如补充 Compose override：

   ```yaml
   services:
     gowvp:
       volumes:
         - type: bind
           source: /mnt/edge-1/recordings
           target: /mnt/edge-1/recordings
           bind:
             create_host_path: false
   ```

5. 在节点编辑窗口填写 `record_path=/opt/zlm/recordings`、`record_mount_path=/mnt/edge-1/recordings`。通道绑定 `edge-1` 后，在通道录像页手动开启录制。

例如，ZLM 上报 `/opt/zlm/recordings/record/rtp/ch1/2026-11-11/10-00-00.mp4`，数据库保留原始绝对路径，平台访问 `/mnt/edge-1/recordings/record/rtp/ch1/2026-11-11/10-00-00.mp4`。

## 播放、删除和清理

- 管理端和播放列表按 `/recordings/{id}/file` 访问片段，支持鉴权、Range、If-Range 和 HEAD；下载沿用 `/recordings/{id}/download`。开放平台使用相同的节点解析器。浏览器不会拿到节点密钥。
- 停用节点会阻止新媒体业务，仍允许读取已有录像及接收收尾 MP4 回调。节点离线时只要共享存储仍可访问，就能播放历史录像。
- 手动删除会先删除所属存储的原文件，成功后删除数据库索引。文件删除失败、挂载失联、目录不一致或身份标记错误时，保留记录并返回错误。
- 保留天数沿用 `Server.Recording.RetainDays`；磁盘阈值沿用 `DiskUsageThreshold`，对每个可访问的节点挂载目录分别检测。平台磁盘超标不会触发删除另一节点磁盘上的录像。不可用节点不会阻断其他节点的清理。
- 有历史录像时禁止删除节点或更换 `record_path`。可以修正挂载位置以恢复对**同一存储**的访问，不可借此映射到其他数据。通道改绑不会迁移文件。
- 旧的 `/static/recordings/...` 仅保留本地兼容；新客户端始终使用录像 ID。

## 升级与旧数据

数据库结构版本为 `0.0.32`，增加节点存储字段、节点与开始时间复合索引，以及可空的唯一幂等键。旧数据的幂等键保持 NULL，已有重复记录不会阻止升级。

发布前先备份数据库。已有 PostgreSQL 大表建议先在目标库执行 [字段及并发索引脚本](v1/sql/20261111-recording-node-storage.sql)，再启动新版；脚本不可放入事务。新建库由 AutoMigrate 完成。若并发建索引中断，检查 `pg_index.indisvalid`，按脚本说明删除无效索引后重试；不要在迁移失败时继续发布。

启动时仅对平台原本录像目录中实际存在、大小匹配的旧文件回填 `local`。未知归属或缺失文件的旧索引保持原样，不根据当前通道绑定猜测来源，也不自动清除。对于这类记录，需人工确认原节点与存储后再做数据修复。

上线后重点确认：两个节点相同相对路径读取不同文件；通道改绑后仍可读取旧节点录像；共享挂载失联时清理不会删除该节点索引。
