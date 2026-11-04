# PostgreSQL 与设备时序历史

生产部署采用独立 PostgreSQL，Owl 使用普通应用账号；数据库保存在持久化磁盘。ZLM 和前端不直接连接数据库。通用 v1 Compose 使用固定镜像摘要生成本地 PostgreSQL 镜像，服务器专用 Compose 使用导入的本地镜像，均不复用其他业务的数据库。

## 当前状态与历史事件

| 数据 | 存放方式 | 用途 |
| --- | --- | --- |
| 设备、通道及当前在线状态 | PostgreSQL 业务表 `devices`、`channels` | 注册有效期、认证、实时列表、恢复运行态 |
| 心跳与成功注册历史 | `device_history_records`，按时间追加 | 心跳间隔、掉线排查、注册回溯 |
| 录像索引、协议任务与配置 | 各自业务表，JSONB 保存协议扩展状态 | 保留关联、幂等键、状态恢复 |
| 视频文件 | 原媒体目录 | 数据库只存索引与相对路径，不存视频二进制 |

时序是数据的组织方式，不要求再引入一种数据库。现有历史记录含设备编码、事件类型、服务器接收时间、来源地址、状态和间隔，联合索引为 `(device_id, kind, recorded_at)`；时间列在 PostgreSQL 中为 `timestamptz`。这些历史可以计算心跳延迟和频率，但不作为当前在线状态或认证判断的唯一来源。

现有保留设置是每设备、每事件类型最多 1000 条和最长 30 天，先到的限制生效。60 秒一次心跳时，1000 条约覆盖 16.7 小时，并不等于完整保留 30 天。管理端“SIP 设置 → 历史保留”可调；将条数设为 0 表示仅按天数限制，两项都设为 0 则不自动限制，不建议长期如此。

当前 4 台设备、约 4000 条历史无需时序扩展。设备数或历史量增长后，可按实际写入量选择 PostgreSQL 按天/月分区、分区删除与统计汇总；再需要压缩、连续聚合时评估 TimescaleDB。分区/超表的唯一键通常必须包含时间分区键，现有单列 `id` 主键不能直接原样转换，应同时处理迁移和 ORM 兼容，不在本次切库时引入。

当前历史记录的是已经处理成功的注册和心跳，并非全量 SIP 报文审计；注册失败等审计需求需要另行设计，历史中不保存密码、Digest 或完整认证报文。

## SQLite 迁移流程

1. 在独立测试库演练，检查迁移和失败回滚；备份 `.env`、Compose、主配置、管理员凭据和 SQLite 一致性快照。
2. 仅暂停 GoWVP 的数据库写入，制作最终 SQLite 一致性快照。保留 ZLM 和其他业务运行。
3. 使用 `tools/db-migrate` 向独立空 PostgreSQL 数据库导入。工具以只读方式打开源文件；拒绝未知表、缺失列和非空目标；在一个事务内创建表、导入、逐行核对 JSON/时间/布尔/ID 等内容并推进序列，失败全部回滚。工具只输出表名与行数，不输出业务数据或连接密码。
4. 成功后用 `-check` 再核对原快照，配置 `OWL_DATABASE_DSN` 并启动支持该变量的 Owl。不要让新旧应用同时写两种数据库。
5. 验证真实设备新心跳、注册历史查询、设备/通道数量、录像索引与下载、备份和独立恢复；观察超过心跳超时周期。

工具构建及用法示例（目标必须是专用空库，URL 中密码须按 URL 规则转义）：

```bash
go build -o db-migrate ./tools/db-migrate
# 通过安全环境注入 OWL_DATABASE_DSN，不把真实密码写入命令历史。
./db-migrate -source /absolute/path/to/offline-snapshot.db
./db-migrate -source /absolute/path/to/offline-snapshot.db -check
```

`-check` 用于停服快照与刚导入的数据库。上线后真实心跳等会继续改变内容，不能要求运行中的库与切换前快照完全一致。迁移工具读取源表时保留数据库字段，不经 API JSON 序列化，避免丢失对外隐藏的 REGISTER 幂等状态。

迁移失败可直接继续使用未修改的 SQLite。成功切换后产生的新数据不会自动回写旧库，回滚必须先停止写入并保存 PostgreSQL 备份，明确新数据的保留策略；不能简单切回旧镜像并声称数据无损。

## 备份和恢复

服务器 `maintenance.sh backup` 使用容器内同版本 `pg_dump --format=custom --no-owner --no-acl`，与配置及凭据一并归档，权限为 `0600`。在线备份不能直接复制 PostgreSQL 数据目录；备份归档不包含录像文件。

恢复时先在独立空数据库中使用 `pg_restore --exit-on-error --no-owner --no-acl`；以应用角色执行恢复或指定 `--role=gowvp`，使应用继续拥有所需表和序列，核对数据后再停服切换连接。仅 `pg_restore --list` 能证明归档可读，不能代替真实恢复演练。

PostgreSQL 初始化脚本只在空数据目录执行。修改 `.env` 中的密码不会修改已经存在的角色，必须先执行对应 `ALTER ROLE`，再同步应用环境。大版本升级需要专门备份/恢复或升级流程，禁止直接复用旧数据目录更换大版本镜像。

## 录像查询索引升级

模型新增 `recordings (cid, started_at DESC)` 和 `channels (did, created_at DESC)` 复合索引；保留原单列索引，兼容全通道录像按时间查询等场景。设备历史已有 `(device_id, kind, recorded_at)` 索引，不重复创建。

生产 PostgreSQL 应在启动新版本前执行 `deploy/v1/sql/20261104-recording-query-indexes.sql`，以并发方式创建索引，避免启动阶段普通建索引阻塞写入。新库或 SQLite/MySQL 开发环境由现有 AutoMigrate 创建同名索引。

本服务器在部署目录执行（SQL 文件需随版本一并复制到 `sql/`）：

```bash
docker compose --project-name owl-v1 --env-file .env --file docker-compose.yml \
  exec -T postgres psql -X -U gowvp -d gowvp -v ON_ERROR_STOP=1 \
  < sql/20261104-recording-query-indexes.sql
```

不要使用 `--single-transaction` 或外层事务。脚本可重复执行，末尾必须返回两条索引，且 `valid`、`ready` 均为 `t`，定义与上述一致。如果并发创建失败遗留无效索引，应先核实目标，再在事务外使用 `DROP INDEX CONCURRENTLY public.<失败的索引名>` 删除该无效索引并重试，不能直接依赖 `IF NOT EXISTS` 修复。应用升级前失败可停止发布；回滚旧应用无需删除这两个兼容索引。

时间轴继续返回原有 `items` 结构，按开始时间和 ID 排序，返回查询范围内全部重叠录像，仅逐行读取展示字段，不再固定截断到 1000 条。范围过大会相应增加响应体积，前端仍按所选日期查询。月统计改为一次 SQL 按天 `EXISTS` 查询，最多返回 31 个日期，不再读取前 10000 条完整录像，也不受录像条数上限影响。日边界沿用服务所在时区（本服务器为 `Asia/Shanghai`），按录像开始日期归属，跨日录像不额外改变统计口径。
