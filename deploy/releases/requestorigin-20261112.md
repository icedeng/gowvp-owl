# 开放平台媒体地址跟随请求入口

2026-11-12T11:08:58+08:00 发布至 `192.168.6.20` / `10.144.0.20`，运行镜像 `owl-v1-service:v2.0.9-requestorigin-20261112-105556`，后端版本保持 `v2.0.9`，源码构建标识 `requestorigin-86f60f4ec1f5`。

## 行为

- 服务器 `.env` 的 `OWL_OPEN_PLATFORM_PUBLIC_URL` 改为 `http://192.168.6.20:15123`。
- 同步响应的直播、回放、录像及图片票据，抓拍任务和永久图片地址，统一跟随当前请求的协议、Host 和端口；HLS 播放列表内的分片、初始化文件及密钥地址同步处理。
- 只有直接连接方位于 `OWL_OPEN_PLATFORM_TRUSTED_PROXIES` 时才接受单值 `X-Forwarded-Host` / `X-Forwarded-Proto`；多值或非法转发头不采用，非法请求 Host 回退到固定配置。现网可信代理列表为空。
- 异步告警回调图片继续使用固定配置；公开元信息 `public_url` 仍表示此固定配置。HTTPS、IPv6 和外部端口保留；代理路径前缀不在本次支持范围。

## 发布与检查

基于已部署 `alarmthumb-12c6bf24e693` 后端源码快照构建，仅叠加本次9个文件，排除工作区其他在途修改；以现网 eventrefresh 镜像为基础仅覆盖 `/app/owl`，前端全部文件摘要保持 `86197c2207dd28d26deec79782b6442f23d17b48d94ad2467f903bbb0322ef0a`。

固定快照的开放平台 API 与核心包检查通过，含请求入口、TLS、IPv6、可信代理、伪造/多值转发头、HLS 子资源与票据鉴权。Linux/amd64 静态构建通过，二进制 SHA256 为 `955a0e4b3f4eea68c3026f64f42c7c1d36c60980e5efcfc0c0fe1c69c31a9c95`，线上文件一致。

线上使用已有应用凭据在服务器内签名调用只读图片列表，分别验证 `http://192.168.6.20:15123` 与 `http://10.144.0.20:15123` 返回相应图片入口，图片内容均可读取，非可信转发头被忽略。未触发真实直播取流、云台或新增抓拍；SmartLinks 浏览器播放由人工验收。

发布后应用 healthy、5台设备全部在线、129通道，公网网关 connected/ready。依赖容器ID与启动时间、主配置、管理员凭据与密钥环摘要、数据库版本 `0.0.34`、应用/凭据数量与网关配置核对保持一致。

首次发布因3分钟设备恢复窗口偏短自动回退，原版本下设备恢复在线后，采用现有部署检查的20分钟恢复窗口重试成功；未修改设备参数或网关服务。首轮证据位于产物目录 `attempt1/`。

## 产物与回滚

本地产物：`build/deploy-v1/requestorigin-20261112-105556/`；服务器：`/www2/gowvp-v1/packages/requestorigin-20261112-105556/`。最终镜像ID `sha256:1077cc9bffb7de990d590ebdf2fd45a7c49048a0c2b810fcf6dbb498e94a4599`。

最终发布前备份：`/www2/gowvp-v1/backups/before-requestorigin-20261112-105556-retry1/`，含环境文件、原二进制、配置归档及经 pg_restore --list 检查的 PostgreSQL 备份。失败脚本自动恢复环境文件并仅重建应用，不恢复业务数据库。

紧急回滚前确认没有后续发布；恢复上述备份 `.env` 后执行：

```sh
cd /www2/gowvp-v1
cp -p backups/before-requestorigin-20261112-105556-retry1/.env .env
docker compose -p owl-v1 --env-file .env -f docker-compose.yml up -d --no-deps --no-build --pull never --wait --wait-timeout 150 gowvp
```

该回滚同时恢复旧固定地址 `http://10.144.0.20:15123`。异步回调接收方后续须能访问新的局域网图片入口；已经发出的旧媒体URL不会自动改写，调用方应重新获取票据。
