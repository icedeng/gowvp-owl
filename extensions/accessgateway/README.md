# 国标公网接入网关

A（Owl）主动通过 mTLS WSS 连接 B（独立 `owl-gateway`）。B 提供设备 SIP、媒体和受限任务入口，A 保留鉴权、目录、业务与 ZLM。

- [设计与交付范围](../../docs/国标公网接入网关-WSS方案.md)
- [部署、升级和回滚](docs/operations.md)
- [线协议入口与变更](docs/websocket-v1.md)
- [兼容性和验证边界](docs/compatibility.md)
- [开发交付记录与检查结果](docs/development.md)
- [管理 API OpenAPI](contracts/admin.openapi.yaml)
- B 部署：`deploy/gateway/`；A 配置增量：`deploy/platform/`
- 源码：`protocol/`、`transport/`、`gateway/`、`platform/`、`integration/owl/`
- 前端：`admin-ui/src/features/access-gateway/`

从仓库根目录执行：

```sh
make check/access-gateway-boundaries
make build/access-gateway
./build/accessgateway/owl-gateway -version
go test ./extensions/accessgateway/...
```

默认启动开关关闭。首次使用先在 A 启用模块，创建网关草稿、导入客户端凭据、绑定凭据并完成连接测试，再启用网关。新增和修改单个网关由数据库管理，不需要重启 A。实际公网接入、媒体节点补丁及厂商设备行为按部署手册进行人工验收。
