# 抓拍历史默认页

2026-11-05 14:59（Asia/Shanghai）发布到10.144.0.20:15123。服务镜像为 `owl-v1-service:v2.0.4-snapshothistory1`，后端与健康版本仍为v2.0.4。基于snapshotlayout1仅调整SnapshotSettingsView：抓拍历史排列首位，未指定tab或无效tab时默认显示历史；显式 `?tab=settings` 仍打开配置。保留Tab状态、分页及表单行为。

正式前端构建、最终差异检查通过；发布前持锁核对镜像与环境，配置和PG一致性备份完成，失败回滚保护保留。发布后523个前端文件和3个HTTP资源摘要一致，管理员配置/历史读取、匿名401检查通过。业务配置、后端、开放文档、PG/ZLM保持；4台设备全部在线、128通道，数据库0.0.29。页面切换效果由人工验收。

产物与验证结果：`build/deploy-v1/snapshothistory1-20261105/`，服务器同名packages目录。传输包SHA256为 `dc5ff4f2e80903dc3b8f23652a7a5bb7e0bd034f904993f2042cab30d838b807`。备份位于 `backups/before-snapshothistory1-20261105/`，回滚时持 `/run/lock/owl-v1-release.lock`，恢复其中.env并仅重建gowvp，不恢复数据库或媒体文件。
