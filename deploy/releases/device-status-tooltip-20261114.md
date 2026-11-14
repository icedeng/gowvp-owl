# 设备列表状态说明悬浮提示发布

2026-11-14 13:23:27（Asia/Shanghai）发布到 `192.168.6.20` / `10.144.0.20` 的 `/www2/gowvp-v1`。

## 修改范围

- 设备列表状态下方不再常驻显示“注册绑定仍有效”等说明；鼠标悬停状态标签时，通过原生 `title` 提示显示完整说明。
- 仅修改 `admin-ui/src/views/DevicesView.vue`。基于前次线上版本的固定源码构建，排除本地 `.env.local`，生产 API 地址固定为同源 `/`。
- 以当前心跳修复镜像为基础覆盖前端文件，保留旧哈希资源。后端二进制、配置、数据库结构和授权不变，只重建 Compose 项目 `owl-v1` 的 `gowvp` 服务；不更新公网 B 端程序。

## 构建与发布

- 新镜像：`owl-v1-service:v2.0.9-device-status-tooltip-20261114-132013`。
- 镜像 ID：`sha256:91baf9015ed40ceb0d33f7d13ba6a907b104112aecc5ca4f56781137ded88460`。
- 后端保持 `v2.0.9` / `offline-fix-becd15ce137f`，SHA256 为 `b565cd6272c9171875f4e7580212f0cb73cbe659cea1012fa188ae2bec7fb855`，保留此前心跳超时离线修复。
- 发布包 SHA256：`3c14f89df524924f3bc3dc742c4c667166f407e26b4a08a44337a2436b3bdd72`。
- 本地产物：`build/deploy-v1/device-status-tooltip-20261114-132013/`；服务器交付目录：`/www2/gowvp-v1/packages/device-status-tooltip-20261114-132013/`。

复用已安装依赖，执行项目构建脚本中的文档一致性、Vue 类型检查和 Vite 生产构建，全部通过。发布脚本语法检查通过。停服前检查交付包、镜像后端及前端文件摘要；持有 `/run/lock/owl-v1-release.lock`，备份后非强制排空网关，只更新 `OWL_SERVICE_IMAGE_TAG` 并重建 `gowvp`，恢复网关后执行只读核验。异常自动恢复原环境文件及原镜像，不自动恢复数据库。

## 线上核验

- 发布脚本退出码 0；应用 healthy，重启计数 0。
- 容器内全部 550 个新前端文件摘要与发布包一致。两个访问入口的首页、设备列表 JS、API 客户端 JS 共 6 次 HTTP 文件摘要校验通过。
- 设备列表编译产物已包含状态标签 `title` 绑定，且不再渲染状态说明的 `resource-state-note` 元素。
- 管理员加密登录、设备列表及设备详情读取正常；匿名设备详情返回 401。
- Office-Dev-64 保持离线，其他 4 台设备在线；5 台设备、129 路通道、253 条事件保留。
- 网关恢复 `enabled / connected / ready`，配置版本仍为 `1/1`；PostgreSQL、ZLM 的容器身份、启动时间和健康状态未变。
- 后端摘要、Compose、主配置、管理员凭据、开放平台密钥环、ZLM 配置、网关凭据摘要及数据库版本 `0.0.36` 保持一致。

未启动浏览器或执行设备控制；悬停提示的实际页面体验由人工刷新设备列表后确认。

## 备份与回退

备份目录：`/www2/gowvp-v1/backups/before-device-status-tooltip-20261114-132013/`，包含原 `.env`、Compose、前端归档、配置归档及在线一致性 PostgreSQL 备份；数据库备份已通过 `pg_restore --list` 检查。

回退目标：`owl-v1-service:v2.0.9-heartbeat-offline-20261114-111637`，镜像 ID 为 `sha256:635daa6a3c6ca49796e8225c556290f278750e3eaa957a5ff5877bd5114267cb`。确认无后续发布并持有发布锁后，非强制排空网关，仅恢复原 `OWL_SERVICE_IMAGE_TAG`，执行 Compose `up -d --no-deps --no-build --pull never --wait gowvp` 并恢复网关。不恢复整库，不重建 PostgreSQL 或 ZLM；该回退目标保留心跳离线修复。

真实凭据、数据库备份和发布产物不提交 Git；临时 SSH 身份副本在核验完成后清理。
