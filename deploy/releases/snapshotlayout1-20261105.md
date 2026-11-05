# 抓拍设置分区与排版

2026-11-05 14:48:48（Asia/Shanghai）发布至 `10.144.0.20:15123`，服务镜像为 `owl-v1-service:v2.0.4-snapshotlayout1`，后端与健康版本保持 `v2.0.4`。本轮仅修改 `admin-ui/src/views/SnapshotSettingsView.vue`，基于已上线v2.0.3前端源码构建页面，部署基于刚发布完成的v2.0.4镜像，保留v2.0.4后端和开放平台文档。没有修改其他会话的VERSION或打包其在途文件。

页面补齐统一page-content边距，分为“设置配置”和“抓拍历史”两个Tab，使用与设备详情一致的连续白色内容面板。宽屏中任务调度、实时流取帧两组配置并列，中等窗口顺序排列，640px以下字段单列。高级参数保留默认折叠，保存区独立；沿用原配置字段、范围和接口。Tab状态写入query，刷新保留当前Tab；左右方向键、Home/End及可见焦点可用。两个面板保持挂载以保留未保存表单和历史分页；历史组件首次打开才加载，图片操作及分页复用原SnapshotWorkspace。

仅叠加本页面的固定前端源码、逐文件摘要、构建及发布产物保存在 `build/deploy-v1/snapshotlayout1-20261105/` 与服务器 `packages/snapshotlayout1-20261105/`。前端类型检查、正式构建和最终差异检查通过。发布持互斥锁、核对v2.0.4镜像和环境，先备份配置及一致性PostgreSQL dump并验证pg_restore --list，失败时恢复旧环境、仅重建gowvp。

发布后523个前端文件在容器内摘要一致，HTTP入口与本页JS/CSS共3个文件匹配。真实管理员加密登录、抓拍配置和历史列表读取通过，匿名配置/历史接口401。后端摘要、开放平台8个文档资源、业务配置、其余环境及PG/ZLM容器身份与启动时间保持。4台设备全部在线、128通道，数据库仍0.0.29；38条抓拍历史保留（既有计划自然执行）。没有修改配置、发起新抓拍或删除图片。未启动浏览器，Tab切换、视觉和窄屏效果由人工验收。

- 传输包SHA256：`2cb14758f0d366c54088bbc50d66370a4b56cbfe5bfefb51e3a379e3e14ebf15`。
- 应用镜像身份：`sha256:dfc2857de11c0c5a4328cc5fc61db6b8fe494426aa72677bb55a4ab8e67ca31d`。
- 保持的后端SHA256：`1b05368dd03864846eb1a2c600a73abed5e989bc8c8b38c90db1ec05531c1dd7`。
- 备份目录：`backups/before-snapshotlayout1-20261105/`，目录700、文件600，含凭据不得公开。
- PG dump SHA256：`197a518ccbe0a38782bb42740552c07d90015ed484edbbd10568a36f0f4d0351`。

紧急回滚仅恢复原v2.0.4应用环境，不恢复在线数据库或媒体文件：

```bash
cd /www2/gowvp-v1
exec 9>/run/lock/owl-v1-release.lock
flock -n 9
cp -p backups/before-snapshotlayout1-20261105/.env .env
docker compose -p owl-v1 --env-file .env -f docker-compose.yml \
  up -d --no-deps --no-build --pull never --wait --wait-timeout 180 gowvp
```
