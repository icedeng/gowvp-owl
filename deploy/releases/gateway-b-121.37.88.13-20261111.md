# 公网网关节点 121.37.88.13

2026-11-11 11:18:07（Asia/Shanghai）通过 `root@121.37.88.13:6022` 完成独立 B 节点安装，网关 ID 为 `b-121-37-88-13`，允许的 A 平台身份和客户端证书 CN 均为 `owl-a`。

服务器为 Ubuntu 24.04 x86_64，公网 IP 经云平台映射到本机内网地址。已有 `gowvp_001` 占用 SIP 15060 和媒体 20000–20500，443 也已占用。此次使用独立端口，不更新或重启已有容器：

| 用途 | 地址 / 端口 |
| --- | --- |
| WSS 控制入口 | `wss://121.37.88.13:8443/gateway/v1/control` |
| mTLS 只读探测 | `https://121.37.88.13:8443/gateway/v1/probe` |
| 设备 SIP | `121.37.88.13:25060`，UDP/TCP |
| 媒体 | `30000–30199`，UDP/TCP |
| 设备 HTTPS 任务 | `https://121.37.88.13:9443` |

程序和发布目录为 `/data/owl-gateway/releases/b-121-37-88-13-20261111/`，`/data/owl-gateway/current` 指向该版本。配置、TLS 文件和运行状态分别位于 `/data/owl-gateway/config/gateway.toml`、`/data/owl-gateway/config/tls/`、`/data/owl-gateway/state/`。unit 为 `/etc/systemd/system/owl-gateway.service`，使用专用账号 `owl-gateway`，已启用开机自启。配置和状态目录 0700，证书及私钥文件 0600。非特权监听端口，无绑定低端口能力。

B 从上次 A 端 WSS 发布的固定源码快照构建，版本 `v2.0.8-gateway1`，源码标识 `581e747b-wss-gateway1-20261111`，协议版本及兼容版本均为 1。64 个媒体会话、8 个文件任务，上下行带宽边界分别为 100000000 bps。本机固定源码、二进制和摘要保存在 `build/accessgateway/releases/b-121-37-88-13-20261111/`，服务器对应发布目录保留安装日志和核对结果。

使用独立私有 CA，服务端 SAN 为 `IP:121.37.88.13`；服务端和客户端证书最初有效期一年，已按用户要求改为 3650 天，与 CA 一致，均有效至 **2036-11-08 11:15:55（Asia/Shanghai）**。沿用原 CA、原私钥、身份及 SAN。签发材料保存在本机 `/Users/dengbing/work/keys/owl-gateway-121.37.88.13/`，CA 私钥已加密，目录 0700、敏感文件 0600。B 仅持有服务端证书、服务端私钥和客户端 CA，未上传 CA 私钥或 A 客户端私钥。A 接入时导入本机该目录中的 `ca.crt`、`client.crt`、`client.key`，不得导入 `server.key` 或 `ca.key`。设备 HTTPS 任务同样使用此私有 CA，设备端需兼容其证书信任。

主机已有 UFW 规则覆盖 8443、9443、25060 及 30000，仅新增 `30001:30199/tcp`、`30001:30199/udp`，备注 `Owl gateway media`；变更前的 UFW 文件保存在 `/data/owl-gateway/backups/ufw-before/`。没有更改 IP forwarding 或云安全组。

已验证：systemd active/running、开机自启 enabled、重启次数 0；公网 mTLS 探测返回正确网关身份及协议能力；未携客户端证书的探测被拒绝；公网 SIP TCP 可连接；任务 HTTPS 未知路径返回 404。核对原有 8 个 Docker 容器 ID、镜像、启动时间与运行状态均未变化。未实际验证 UDP 媒体映射、设备取流、任务传输和容量；能力列表不代表真机业务通过。

2026-11-11 11:37:34（Asia/Shanghai）按用户授权接入 A `10.144.0.20` 并启用。网关名称为“公网网关 121.37.88.13”，分组“公网网关”，默认媒体节点沿用现有选择策略。通过管理 API 创建草稿、导入专用客户端凭据、绑定凭据、诊断并启用；不修改现有设备或平台绑定，不重启 A/B。启用操作 ID 为 `cfc85f69-2390-4585-9542-38cb92406bc4`，状态 succeeded。

A 端配置、身份、DNS、TCP、TLS、mTLS、协议和能力 8 项诊断均通过；实际状态为 `enabled / connected / ready`，期望配置及生效版本均为 `1`，状态与指标未过期。再次间隔 16 秒读取确认心跳推进；B 侧独立 mTLS 探测确认 `owner_connected=true`、`config_revision=1`、`cooldown_remaining_ms=0`，systemd 重启次数仍为 0。A 端原有 4 台设备全部在线、128 个通道、1 条启用抓拍计划，应用、ZLM、数据库容器身份和启动时间以及配置摘要均未变化。新网关尚无设备绑定和媒体会话，连接诊断中的 `media_path_verified=false` 保留，真机取流待验收。

A 凭据由后端保存在配置指定的受控目录；上传所用 `/run/` 临时客户端证书目录已删除，未向 A 上传 B 服务端私钥或 CA 私钥。A 接入报告在 `/www2/gowvp-v1/packages/connect-b-121-37-88-13-20261111/`，本机脱敏报告保存在该 B 发布目录的 `a-connection/`。管理入口：[该网关详情](http://10.144.0.20:15123/web/#/access-gateways/b-121-37-88-13)。

同日完成 10 年证书切换：先通过 A 的证书轮换 API 导入并启用新客户端证书，操作 succeeded；再原子替换 B 的服务端证书并重启单个 `owl-gateway` 服务。A、ZLM、数据库未重启，原有 4 台设备仍在线。公网 TLS 核对新服务端 SHA256 为 `65db199e31686b966e028d488ab718948f99c04a00657158776fab0c2703001e`；A 使用中新客户端 SHA256 为 `e2f92b85d160aa7b7c7aa5233755ec9e99b3555e7fdd2543499834af668cbc02`，凭据 ID 为 `ad5f6969-d228-4252-9929-a5e77a2585cb`。恢复后心跳推进，状态 enabled/connected/ready，配置版本仍为 1/1，8 项连接诊断通过。A 临时上传目录已清理。

证书切换报告位于 A 的 `packages/cert10y-b-121-37-88-13-20261111/`，本机该 B 发布目录的 `cert10y/`；B 原服务端证书备份位于 `/data/owl-gateway/backups/cert10y-20261111/`。本机原一年证书保留在签发目录的 `backup-1year-20261111/`，当前目录中的 `server.crt`、`client.crt` 已同步为线上生效证书。

维护：`systemctl status owl-gateway`、`journalctl -u owl-gateway`。撤销新安装时先确认 A 未连接且无业务资源，然后 `systemctl disable --now owl-gateway`；仅移除本次新增的两个 UFW 规则，不整体恢复旧防火墙覆盖后续变化。保留配置、证书、运行状态与发布材料，避免无意丢失后续业务状态。
