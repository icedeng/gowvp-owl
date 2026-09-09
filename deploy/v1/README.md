# Owl v1 真机测试部署

## 发布模式

`deploy/v1` 采用“本机编译产物 → 本机生成 Docker 镜像 → Docker Compose 启动”的模式。

它不是生成一个 `.tar` 压缩包，也不会把镜像推送到 Docker Hub 或私有镜像仓库。发布时使用当前工作树生成以下三个本地镜像：

| Compose 服务 | 本地镜像 | 内容 |
| --- | --- | --- |
| `gowvp` | `owl-v1-service:<OWL_IMAGE_TAG>` | Alpine、FFmpeg 和编译后的 Owl 服务 |
| `frontend` | `owl-v1-frontend:<OWL_IMAGE_TAG>` | Nginx 和编译后的 Vue 管理端 |
| `zlm` | `owl-v1-zlm:<OWL_IMAGE_TAG>` | 固定补丁版本的 ZLMediaKit |

启动时设置了 `pull_policy: never`，只使用本地镜像，不会自动拉取同名远程镜像。

## 首次发布

```bash
# 生成 .env，并填写真机可访问的宿主机局域网 IP
./deploy/v1/deploy.sh init 192.168.1.100

# 编辑 WVP 配置
vim deploy/v1/data/configs/config.toml

# 编译、构建三个本地镜像并启动 Compose
./deploy/v1/deploy.sh release
```

`192.168.1.100` 必须替换为摄像机或上级平台能够访问的宿主机局域网地址。

发布前至少确认以下 WVP 配置：

```toml
[Sip]
Host = '宿主机局域网IP'
Port = 15060
ID = '本平台20位国标ID'
Domain = '本平台10位国标域'
Password = '设备注册密码'
```

同时确认 `deploy/v1/.env` 中的：

```env
OWL_MEDIA_SDP_IP=宿主机局域网IP
OWL_MEDIA_SECRET=媒体服务共享密钥
```

## 编译与构建流程

执行 `./deploy/v1/deploy.sh build` 时，脚本依次执行：

1. 自动识别 Docker Engine 架构，支持 `amd64` 和 `arm64`；
2. 通过 Go 交叉编译 Linux Owl 服务，并写入版本、分支、提交哈希和构建时间；
3. 使用 `pnpm install --frozen-lockfile` 安装前端依赖并构建管理端；
4. 把产物写入 `build/deploy-v1/<架构>/`；
5. 通过 Compose 生成 `gowvp`、`frontend`、`zlm` 三个本地镜像。

产物目录示例：

```text
build/deploy-v1/arm64/
├── service/owl
└── frontend/
    ├── index.html
    └── assets/
```

其中 ZLMediaKit 使用仓库现有的 `Dockerfile_zlm`，固定提交为：

```text
2fa539807370c4bcfca9801bde1dd2fdd95ba113
```

并应用仓库内的 RTP/RTCP 补丁。

## 启动关系

Compose 服务依赖关系如下：

```text
zlm 健康检查通过
        ↓
    gowvp 健康检查通过
        ↓
     frontend
```

前端由 Nginx 提供静态资源，并将 API、WebSocket 和未知路径代理到 Owl：

```text
浏览器 :15123
      ↓
frontend/Nginx
      ↓
gowvp:15123
```

## 端口映射

| 端口 | 用途 |
| --- | --- |
| `15123/tcp` | 管理端前端入口 |
| `15060/tcp+udp` | SIP |
| `15061/tcp` | SIP-TLS |
| `3702/udp` | ONVIF WS-Discovery |
| `1935/tcp` | RTMP |
| `554/tcp` | RTSP |
| `8080/tcp` | ZLMediaKit HTTP/API |
| `10000/tcp+udp` | ZLMediaKit 默认 RTC 端口兼容映射 |
| `20000-20100/tcp+udp` | GB/T 28181 RTP/RTCP 和 WebRTC 端口段 |
| `30200-30300/tcp` | 2014 附录 O 裸 TCP 下载/级联中继 |

## 配置与持久化数据

部署目录下的持久化数据位于 `deploy/v1/data/`：

- `configs/config.toml`：WVP 主配置，包含 SIP、GB/T 28181、录像、数据库等设置；
- `configs/data.db`：默认 SQLite 数据库；
- `configs/certs/`：SIP-TLS、证书 REGISTER 等证书文件；
- `logs/`：Owl 日志；
- `recordings/`：Owl 与 ZLMediaKit 共享的录像目录；
- `zlm-config/`：ZLMediaKit 运行配置。

修改 `config.toml` 后重启 Owl：

```bash
docker compose \
  --project-directory deploy/v1 \
  --env-file deploy/v1/.env \
  --file deploy/v1/docker-compose.yml \
  restart gowvp
```

`.env` 中的密钥和 `OWL_MEDIA_SDP_IP` 由 Compose 注入，优先级高于配置文件中的同名媒体字段。

发布脚本不会清理 `deploy/v1/data/`。回滚时修改 `.env` 中的 `OWL_IMAGE_TAG`，确保对应标签的三个本地镜像存在，然后执行 `./deploy/v1/deploy.sh up`。

## 常用命令

```bash
# 只编译并构建本地镜像，不启动容器
./deploy/v1/deploy.sh build

# 启动已有本地镜像；镜像不存在时不会从远程拉取
./deploy/v1/deploy.sh up

# 编译、构建并启动
./deploy/v1/deploy.sh release

# 查看服务状态
./deploy/v1/deploy.sh status

# 查看全部日志或指定服务日志
./deploy/v1/deploy.sh logs
./deploy/v1/deploy.sh logs gowvp

# 停止并移除容器，保留 data/ 数据
./deploy/v1/deploy.sh down
```

## 架构与网络要求

脚本默认按 Docker Engine 架构生成 `amd64` 或 `arm64` 镜像。交叉构建可设置：

```bash
OWL_TARGET_ARCH=amd64 ./deploy/v1/deploy.sh release
```

交叉构建 ZLMediaKit 需要 Docker 支持对应平台模拟，构建时间会明显增加。

真机测试前请确认：

- 宿主机防火墙开放 SIP、RTP/RTCP、RTSP、RTMP 和附录 O TCP 端口；
- 设备能够访问 `OWL_MEDIA_SDP_IP`；
- `Sip.Host` 与 `OWL_MEDIA_SDP_IP` 使用真机可达的宿主机地址；
- Docker bridge 模式下 ONVIF 组播发现可能受网络环境限制，必要时可直接配置设备地址测试，不要把组播发现成功作为 GB/T 28181 信令验收的前置条件。
