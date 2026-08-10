# GB/T 28181 1.0/1.1 开发测试基线

记录时间：2026-08-10（Asia/Shanghai）

## 环境

- Git commit：`4c39ced`
- Go：`go1.26.4 darwin/arm64`
- CodeGraph：已启用

## 已执行命令

### `go test ./pkg/gbs/...`

结果：通过。

```text
ok  github.com/gowvp/owl/pkg/gbs
?   github.com/gowvp/owl/pkg/gbs/m [no test files]
ok  github.com/gowvp/owl/pkg/gbs/sip
```

### `go test ./internal/core/ipc/...`

结果：失败，属于实现前已有测试问题。

失败用例：

- `internal/core/ipc/store/ipcdb.TestChannelGet`
- `internal/core/ipc/store/ipcdb.TestDeviceGet`

共同原因：sqlmock 的 `ExpectedQuery` 只设置了 SQL 和参数，没有设置应返回的 `driver.Rows`，实际查询因此报错。该问题不在 GB/T 28181 1.0/1.1 开发范围内，本阶段不修改。

## 后续判断规则

- `pkg/gbs/...` 新增失败视为本次开发回归。
- `internal/core/ipc/...` 必须区分上述两个基线失败和新增失败。
- 涉及设备持久化改造时，应为新增逻辑编写独立测试，不能以上述基线失败作为跳过验证的理由。
