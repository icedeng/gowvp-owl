# WSS v1 开发交付记录

日期：2026-11-10。对应[完整方案](../../../docs/国标公网接入网关-WSS方案.md)。开发实现及下列检查已完成，尚未部署到公网或做厂商真机验收。功能默认关闭，首次启用按[部署手册](operations.md)操作。

## 实现范围

| 方案范围 | 实现入口与行为 |
| --- | --- |
| 共享 WSS 协议、mTLS、身份与隔离 | `protocol/`、`transport/`、`gateway/internal/server/`；严格消息解码、配置摘要、单次附着票据、owner/generation/epoch fencing、独立 probe |
| 独立 B 程序 | `gateway/cmd/owl-gateway/`、`gateway/internal/`；SIP UDP/TCP、公网媒体端口、租约/额度/队列、任务 HTTPS、本地原子恢复日志；不依赖 A 业务包或 GORM |
| A 多网关管理 | `platform/runtime/`、`platform/service/`；逐网关连接/重连、配置 prepare/apply、排空/启停、幂等操作、证书轮换及失败恢复旧凭据 |
| 数据与权限 | `platform/store/`、`platform/migrations/`、`internal/web/api/access_gateway_mount.go`；迁移0.0.31、引用与唯一约束、持久化操作/指标/事件、严格当前管理员 Bearer 门禁、受控凭据目录 |
| SIP 与入口 | `integration/owl/`、`pkg/gbs/external_*.go`、`pkg/gbs/sip/external.go`；复用原鉴权/事务/业务，注册成功才提交绑定；直连与多 B 共存、入口冲突保护和认证后迁移 |
| 平台独立绑定 | `access_gateway_platform_bindings` 区分 upstream/downstream；国标系统编码类型200沿用宿主已有档案/目录 API，独立绑定且不计摄像机/NVR数，平台通道仍计通道数；到期/注销不显示有效在线 |
| 媒体与 ZLM | `integration/owl/media.go`、`pkg/gbs/external_media.go`、`pkg/gbs/external_rtcp.go`、`pkg/zlm/rtp.go`；UDP、TCP主被动、双向 RTP/RTCP、独立公网与本地端点、共享源引用保护、失败补偿 |
| 原有业务适配 | `pkg/gbs/` 中点播、录像目录/回放、RTP及裸TCP下载、语音/广播、级联各腿；按主体绑定选入口，direct保留原路径，不绕过网关主动直拨设备 |
| 文件任务 | `gateway/internal/transfer/`、`integration/owl/transfer.go`、`pkg/gbs/external_transfer.go`；抓拍raw/multipart、升级文件获取/Range、长度/hash/EOF与持久提交分离，断链不伪装成功 |
| 管理 API 与前端 | [OpenAPI](../contracts/admin.openapi.yaml)、`platform/httpapi/`、`admin-ui/src/features/access-gateway/`；向导、列表/详情、三维状态、配置对照、诊断、证书、设备/平台、媒体/任务、迁移与审计；原设备/SIP/总览页面联动 |
| 数量与指标 | A绑定聚合设备/平台/通道和活动源，B上报流量/端口/资源，心跳携带会话流量采样；设备与WSS方向分列、未知用null、过期标记、历史聚合及保留策略 |
| 部署与维护 | `deploy/`、`scripts/`、`Dockerfile_zlm`、`third_party/zlmediakit/patches/`；B二进制/容器/systemd、A配置增量、私有CA签发、升级/回滚/备份说明 |

以上目录均相对 `extensions/accessgateway/`，以 `internal/`、`pkg/`、`admin-ui/` 等开头的宿主路径相对仓库根目录。原宿主业务保持在原目录；B 构建依赖闭包不引入宿主包。

## 已执行检查

| 检查 | 结果与边界 |
| --- | --- |
| `go test ./extensions/accessgateway/... ./pkg/gbs` | 通过；覆盖协议/帧/严格解码、mTLS所有者隔离、配置幂等/过期、租约票据、恢复日志容量与重启、文件提交、绑定事务/迁移、证书回退、下级平台计数及原国标回归 |
| 最终变更回归 | `go test ./extensions/accessgateway/integration/owl ./extensions/accessgateway/platform/...` 通过；含注册到期后的设备筛选/通道统计、旧代次采样隔离及清理不覆盖流量 |
| 管理员门禁 | `go test ./internal/web/api -run '^TestAccessGatewayRequiresCurrentAdminBearerOnEveryRoute$' -count=1` 通过；全部管理路由拒绝普通用户、无到期/过期JWT、query token/access_token及重复认证头；管理员名变更后旧身份失效 |
| 宿主装配 | `go test ./internal/app -run '^$'` 通过，验证装配可编译，不启动业务服务 |
| 前端类型 | `admin-ui/node_modules/.bin/vue-tsc --noEmit` 通过；未启动浏览器或进行页面视觉验收 |
| B独立构建 | `scripts/build-gateway.sh` 通过，输出二进制及build-info；`check-boundaries.sh` 检查实际传递依赖，无A业务/GORM；工作树构建标识带dirty |
| ZLM实际构建 | `docker build --target zlm-builder -f Dockerfile_zlm --build-arg ZLM_BUILD_JOBS=4 -t owl-zlm-gateway-build-check:local .` 通过；固定提交 `2fa539807370c4bcfca9801bde1dd2fdd95ba113`，实际应用并编译0001/0002/0003补丁，产出本地构建检查镜像 |
| 接口契约 | OpenAPI 3.0.3 校验通过，19个path / 22个method、41个schema；与已挂载路由一一核对，全部本地引用可解析；校验依赖仅置临时目录，未新增项目依赖 |
| 运维文件 | 三个shell脚本 `bash -n`、B Compose `config --quiet` 通过；CA脚本实际签发并验证sslserver/sslclient及DNS/IP SAN，CA私钥加密；临时签发目录已清理 |
| 最终差异 | `git diff --check` 通过；检查新增功能目录的忽略规则，集成源码可以纳入版本管理 |

无关的已知检查失败：`go test ./internal/web/api` 中原 `TestAuthMiddleware/authURL网络不可达返回502` 预期502、实际403；清除代理环境变量后仍复现。本次未改通用AuthURL鉴权行为；网关管理独立严格门禁回归通过。

## 部署前输入与人工验收

提供 A/B 实际系统架构、公网IP/域名、开放端口、证书身份、A→B出口许可及实际ZLM节点地址，使用匹配A/B/ZLM产物。2026-11-12增量已实现受控HTTP CONNECT引用；使用方式与上线限制见可靠性说明。设备HTTPS任务入口须满足厂商设备的证书信任要求。

1. 在两个B和原direct设备共存的环境，检查注册/设备与平台计数、页面管理动作及迁移/取消/证书轮换，B1故障不影响B2/direct。
2. 用实际厂商设备验证UDP/TCP主被动、双向RTCP、点播/回放/下载、语音/广播、上下级平台各腿、抓拍与升级任务；确认公网地址与容器内ZLM端点可达。
3. 断网/重启/排空时确认两端资源最终回收，迟到消息不覆盖新绑定；按目标并发和码率做容量验收。构建与源码回归不证明公网互通、性能或厂商异常路径已经通过。

2026-11-11，A 端已发布至 10.144.0.20，模块已启用，数据库迁移及现网只读冒烟检查通过。现场未配置 B，公网和厂商设备互通仍待验收。详见 [A 端发布记录](../../../deploy/releases/v2.0.8-wss-gateway1.md)。

2026-11-12，P0～P2源码增量见 [可靠性改进](reliability.md)，尚未部署。本轮不开发多网关迁移，已从网关详情设备页移除迁移动作，兼容保留既有后端接口；历史交付表中的迁移不代表本轮新增。
