# 公网网关部署与维护

## 1. 产物与网络准备

A 使用原 Owl 镜像及发布流程，并包含 `Dockerfile_zlm` 的三个 ZLM 补丁。B 单独构建；不在 B 部署 Owl、ZLM、前端或数据库。

```sh
make build/access-gateway
./build/accessgateway/owl-gateway -version
# 可选镜像构建；在仓库根目录执行，版本和提交由发布系统填入。
docker build -f extensions/accessgateway/deploy/gateway/Dockerfile \
  --build-arg GATEWAY_VERSION=YOUR_VERSION --build-arg SOURCE_COMMIT=YOUR_COMMIT \
  -t your-registry/owl-gateway:YOUR_VERSION .
```

B 需要固定公网入口、WSS TCP 443、SIP UDP/TCP 15060、媒体 UDP/TCP 30000–30199；如启用抓拍/升级，还需 HTTPS TCP 8443。安全组由运维配置，应用不修改防火墙或 IP forwarding。A 只需主动访问 B WSS；验证 A 到实际 ZLM 管理 API、RTP 和桥接地址的容器内可达性。WSS 端口/域名如受出口限制，先完成组织网络准入。

## 2. 私有 CA

在受控签发机执行（不在 B 或仓库保存真实密钥）：

```sh
bash extensions/accessgateway/scripts/private-ca.sh /secure/offline/owl-certs \
  DNS:gateway.example.com,IP:YOUR_PUBLIC_IP owl-a
```

脚本加密 CA 私钥，提示输入口令；无人值守可设置 `CA_KEY_PASSWORD_FILE` 指向权限受控的密码文件。B 使用 ca.crt（改名 client-ca.crt）、server.crt、server.key；A 管理端导入 ca.crt、client.crt、client.key。客户端 CN 与 B 的 client_certificate_cn 一致；平台 ID 与 A 启动配置一致。CA 私钥留在签发机。验证服务器 SAN 覆盖实际 WSS/任务 URL；有效期与系统时钟必须正确。

## 3. B 安装

复制 `deploy/gateway/gateway.example.toml` 为部署目录的 gateway.toml，替换全部身份、地址和配额。示例 TEST-NET 地址不能用于生产。状态目录需运行账号拥有、0700；证书私钥0600，并确保只有运行账号能读取。

Compose：复制 compose.yaml、gateway.toml、tls/ 到同一部署目录；预建 state/，属主为 UID/GID 10001，0700。设置 OWL_GATEWAY_IMAGE 为固定 tag/digest 后运行 `docker compose up -d`。容器通过独立网络命名空间 sysctl 允许普通账号监听 443，无额外主机权限。若改用 host network，请用系统低端口权限设置并核对全部监听地址，不能照搬容器 sysctl。

systemd：将二进制安装到 /usr/local/bin/owl-gateway；创建 owl-gateway 运行账号，配置及证书放 /etc/owl-gateway/。安装提供的 unit 到 /etc/systemd/system/，执行 daemon-reload 和 enable --now。StateDirectory 自动创建 /var/lib/owl-gateway/。进程只有绑定低端口能力，状态目录之外只读。

首次启动日志、证书错误和端口占用可通过 `journalctl -u owl-gateway` 或 `docker compose logs owl-gateway` 检查。B 无健康状态伪成功 HTTP 端点；A 中的独立 probe 验证 mTLS、身份、边界、端口和能力。

## 4. A 启用与首次接入

将 `deploy/platform/config.example.toml` 的 AccessGateway 增量合入现有配置。CredentialDir 相对 ConfigDir 解析，目录0700。容器挂载示例见 compose.override.yaml，保留原数据库、SIP、媒体配置与卷；此处不替换原部署。

重启 A 使模块开关生效。以管理员登录，新增网关草稿，填写稳定 ID 与 `wss://gateway.example.com/gateway/v1/control`、B 公网 SIP/媒体范围等；导入客户端凭据并绑定；完成连接诊断，再启用。后续增加 B2/B3、修改单个配置由数据库管理，不需要重启 A。期望版本和实际版本一致且健康为 ready 后接入设备。设备注册地址指向指定 B；A 仍负责设备原口令认证。

上级平台在 SIP 设置中选择入口；更换已有认证入口时先创建平台迁移计划，再保存新入口配置。设备迁移还需运维修改设备地址。目标注册早于排空完成时返回503/Retry-After，不能先返回200。

## 5. 备份、升级、回滚

- A 备份原数据库（包含网关扩展表）、ConfigDir 配置和凭据目录；保证数据库与文件引用一致。敏感备份加密且限制读取。迁移0.0.31只新增扩展表，历史设备按 direct 处理，不复制原档案。
- B 备份本地配置、服务端证书和状态目录；备份中不包含 CA 私钥。状态文件采用原子替换，不能手改 generation/epoch/票据以恢复旧会话。
- 升级前在 A 排空该 B，确认活动/待清理资源收敛；备份后替换固定版本 B 镜像/二进制及匹配 A/ZLM 产物。保留网关 ID、平台 ID 和状态卷。先升级单 B，确认管理健康及配置版本后继续其他 B。
- B 重启创建新 generation；A 新控制连接需核对并回收旧会话，再重新认证。端口进入冷却期是正常保护，不能删状态文件来抢先复用。
- 回滚先排空、停止新业务，按已记录的固定产物恢复匹配版本。支持同协议的数据版本可保留新增表回退程序；未知 schema 不降级写入。不要删除扩展表、绑定或密钥来绕过保护。恢复数据库时同时恢复对应凭据文件，并让设备重新注册；旧 socket/运行态不恢复。
- 证书轮换先签发、在 B 的受控 CA 信任范围内验证、在 A 使用轮换操作试连并切换；确认新连接后才撤销旧证书。CA 轮换使用过渡信任包、先扩信任再切证书，避免同时断开所有 B。

## 6. 监控及人工验收

关注管理端独立管理/连接/健康状态、期望/应用版本差、指标过期、证书到期、端口/配额占用、队列和拥塞错误、cleanup_pending 与 migration draining 长时间不收敛。不可用采样为 null，不用0代替。日志不要采集设备密码、完整 SIP Authorization、票据 URL 或 PEM 私钥；Compose 使用有界日志轮转，systemd 沿用 journald 策略。

部署验收至少检查：两个 B 相同端口范围互不串流；UDP/TCP 主被动及双向 RTCP、回放/下载、语音及级联各腿；断网/重启后会话回收与重新注册；抓拍 raw/multipart 和升级任务失败；迁移提前注册503、提交及取消。原 direct 设备继续可用。协议/源码回归不能作为上述真机验收已通过的证明。

## 7. 可靠性版本升级

本轮数据库0.0.36须停 A 并备份原库后执行 `tools/accessgateway-migrate`，再以 `-check` 校验；签名报警不绕过 A 验签。报警队列、历史不取证、设备续接、故障防抖、代理配置和人工验收重点见 [可靠性说明](reliability.md)。

采用 `/data/owl-gateway/current` 软链部署的 B 可使用 [统一发布工具](../../../tools/gateway-release/README.md)。既有 `/usr/local/bin` 安装方式仍兼容运行，但工具会拒绝与清单不符的布局，不自动迁移目录。统一工具默认只读预检；先审核兼容组合、备份范围及数据库迁移，再决定执行。新旧 A/B 混合版本通过扩展能力协商降级，旧 B 无持久补传或安全快速续接能力，不能承诺断线无报警丢失。

网关故障事件在事件中心查看并处理；历史报警显示“历史补报、现场取证不可用”，不会补拍当前现场。B重启后等待设备重新注册属于安全边界，保留报警状态卷；不删除目录绕过 generation/epoch。报警队列损坏时保留原文件、检查磁盘与权限，修复后自动重试正常记录。
