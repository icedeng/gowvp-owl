# 网关默认运行监控发布

2026-11-11T12:59:13+08:00 发布到 `10.144.0.20:15123`。入口：[网关详情](http://10.144.0.20:15123/web/#/access-gateways/b-121-37-88-13)。

默认页签改为“运行监控”并排在第一位；无 tab 参数或无效参数回落到运行监控，显式指定其他有效页签的链接继续兼容。以此前线上快照为基础，仅覆盖 `GatewayDetailView.vue`，未混入工作区其他修改。文档一致性、Vue 类型检查、Vite 生产构建及发布脚本语法检查通过。

仅替换前端 `/app/www`，只重建 `gowvp` 服务。新镜像标签 `owl-v1-service:v2.0.9-gateway-default-20261111-125512`，镜像 ID `sha256:89e8694745e22a5278152fa157177172be47875e85fdfaaf1e6d7f1ce7c60308`。后端仍为 `v2.0.9` / `recording-v209-b06d78f9ed62`，SHA256 `bd91191df5d97d9bae070201be6c880d1bd77a6e786a9b81305b8de070948469`。固定源码、摘要、元数据和报告位于本机 `build/deploy-v1/gateway-default-20261111-125512/`，服务器对应 `packages/gateway-default-20261111-125512/`。

上线后应用 healthy、重启次数 0，容器 546 个静态文件及服务器 HTTP 7 个文件摘要通过，本机外部访问首页、详情 JS/CSS 和列表 JS 共 4 个文件摘要通过。管理员加密登录及匿名访问限制通过。4 台设备在线、128 个通道、1 条启用抓拍计划，公网网关 connected / ready。后端、数据库结构、业务配置、PostgreSQL/ZLM 容器身份及启动时间、网关配置和十年证书均保持一致。未做浏览器视觉验收或真实设备业务操作。

首次发布因核验脚本未指定媒体节点分页大小，仅返回 1 条节点记录而误判列表变化，触发自动回滚并恢复健康。核实总数为 2，明确 page=1&size=100 后返回 local 和 zlm-15，数据库既有节点未丢失。修正核验脚本后发布同一前端产物成功；首次记录保存在服务器和本机 `attempt1/`。

成功发布前备份在 `/www2/gowvp-v1/backups/before-gateway-default-20261111-125512-retry1/`，包含 `.env`、Compose 和前端归档。回滚前确认没有后续发布并持有 `/run/lock/owl-v1-release.lock`，恢复该备份 `.env`，使用现有 Compose 项目 owl-v1 仅重建 gowvp，回到 `owl-v1-service:v2.0.9-gateway-tabs-20261111-124632`（`sha256:4eedc9fa8f67846decf1fd4aa70d7a47e371bc2b2dd7bf92c5629ffc4895ca2f`）。不恢复数据库，不重建 PostgreSQL 或 ZLM。
