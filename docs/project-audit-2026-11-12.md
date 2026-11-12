# 项目审查与修复清单（2026-11-12）

本轮已修复可独立验证的问题，修改仅在工作区，未部署、发布或删除运行数据。下面的待审事项需要业务策略、部署信息或进一步复现；不是已完成修复清单。

## 范围与证据边界

- 起始工作区无 Git 修改；清点 1,044 个跟踪文件，其中 677 个 Go 文件，另含 Vue/TypeScript、Python、容器与部署脚本、协议夹具和文档。
- 做了全仓文件/引用扫描、源码包检查，并重点审阅启动与配置、HTTP 认证/日志、媒体与文件路径、事件清理、AI 生命周期、设备筛选、ONVIF 心跳、开放平台、接入网关边界和发布链路。CodeGraph 恢复后用于调用关系与影响范围复核。
- 这是全仓范围的静态审查与检查，不是每个文件逐行验证，也不能证明“所有问题已找完”。生成代码、第三方媒体实现、每份历史发布记录没有逐行审计。
- 没有访问生产实例、启动浏览器或连接真实摄像头；真实 GB28181 四版本、ONVIF、网关穿透、媒体收发、录像存储挂载与升级回滚仍需按项目既有现场流程验收。

## 已修复

| 编号 | 问题与影响 | 本次处理 / 主要文件 |
|---|---|---|
| F01 | Docker 前端构建缺少在线文档校验使用的仓库文档和三语言示例；通用 YAML 忽略规则与必需构建输入冲突 | 三个根 Dockerfile 复制 `docs/open-platform`、`examples/open-platform`；`.dockerignore` 显式保留锁文件、工作区配置和 OpenAPI YAML |
| F02 | Release 工作流未安装 pnpm/UPX，Go 使用预发布版本且浅克隆可能缺失版本标签；上传裸二进制丢失管理端 | 使用 `go.mod` 的 Go 版本、完整标签历史，安装 Node/pnpm/UPX；保留原裸二进制资产，并新增含 `bin + www` 的架构压缩包 |
| F03 | Makefile 注入不存在的 `main.buildTimeAt` 和未定义的 `BRANCH_NAME`，Python 安装目标指向不存在的目录 | 改用 `main.buildTime`、`BRANCH` 和 `analysis/requirements.txt` |
| F04 | Python 依赖最低版本低于已提交的生成代码要求，且没有声明 protobuf | 声明 `grpcio>=1.76.0`、`protobuf>=6.31.1,<7`；AI Dockerfile 同步约束 |
| F05 | AI Health 将 `is_ready` 方法对象当布尔值，模型未加载也返回 SERVING；启动回调端口写死 | 调用 `is_ready()`；Go 启动 Python 时使用配置的 HTTP 端口 |
| F06 | FFmpeg 每轮重连都重置失败计数，启动失败不计数；日志管道读端不关闭 | 仅收到完整帧后重置预算，启动失败也计入重试，关闭管道两端；重试等待可被停止事件唤醒 |
| F07 | macOS gopsutil 解析异常 netstat 输出会 panic，后台指标采集可终止整个服务；CPU 更新错误依赖内存采集成功 | 在网络采集边界将第三方 panic 转为采集错误，跳过无效样本；CPU 独立更新。此修复保护服务存活，不保证异常平台上仍有网络指标 |
| F08 | 并发公钥初始化可能互相替换密钥；用户修改接口重复执行鉴权 | 获取写锁后再次检查密钥，读锁内获取公钥快照；路由只挂载一次鉴权 |
| F09 | 请求日志记录完整 query 和正文/响应片段，可能泄露 token/secret；未知 Content-Length 的正文会在路由限额和鉴权前被读取 | 替换为只记录路径、方法、来源、状态、耗时与追踪信息的日志中间件；保留既有忽略路径。日志不再提供 query/body 调试信息 |
| F10 | 旧接口兼容分支切换网关后复用旧网关缓存；版本筛选每次翻页重复拉取全量；防抖/卸载时旧响应仍可提交 | 缓存绑定网关，复用同入口数据；筛选变化和卸载立即使旧请求失效 |
| F11 | 前端源码断言未适配新增网关筛选；HTTP 网络失败测试实际受环境代理影响 | 更新断言并增加执行页面脚本的行为回归；网络失败测试改用已关闭的回环监听器 |
| F12 | 遗留 OTA 文件仅含未使用常量和约 300 行注释代码；默认录像文档与实现相反；历史安全报告容易被当作现状 | 删除 `pkg/ota/lin.go`，保留仍被调用的版本查询及可能被外部使用的公开 API；修正文档为 `none`，标明旧报告的历史性质；修正 pprof 示例注释 |

## 待审核：优先处理

### R01 · pprof 边界已修复；CORS 仍待审核

位置：`internal/web/api/pprof.go`、`internal/web/api/provider.go`，依赖 `goddd/pkg/web/pprof.go`。

原问题：pprof 默认关闭，但启用后依赖的白名单使用 `ClientIP()`，Gin 默认信任转发头，可能被伪造来源绕过。现已为全部诊断路由增加独立保护：真实 TCP 对端精确 IP 白名单与有效的平台管理员 Bearer 令牌必须同时通过；空白名单拒绝，不接受 URL 令牌或第三方 AuthURL 放行。本机代理转发也必须携带管理员令牌。修复在工作区，未部署。

兼容性：旧的匿名诊断采集需添加管理员请求头；代理/容器白名单按真实连接来源填写。配置及采集示例见 [pprof 访问说明](pprof-access.md)。普通接口 CORS 同时接受任意 Origin 和凭据（`internal/web/api/api.go`），仍需确认实际跨域客户端清单后收紧。本次没有修改全局代理/CORS 行为。

### R02 · 已修复：密码修改后的会话策略与密钥轮换

位置：`internal/web/api/user.go:149`、`internal/web/api/user.go:192`、`internal/conf/admin_credentials.go`。

管理员口令已改为带随机盐的 PBKDF2 哈希，启动时迁移旧主配置及凭据文件。按用户确认拆分 ONVIF 独立账号密码，避免协议摘要认证使用管理员哈希。改密时原子保存哈希与新会话版本，旧管理令牌及平台播放代理令牌失效，旧通知 WebSocket 连接关闭；重启保持撤销状态。RSA 登录响应增加 key ID，上一把私钥保留至旧密钥到期后 5 分钟，兼容不携带 ID 的旧客户端。

发布影响：首次升级需重新登录，ONVIF 客户端需重新配置独立凭据，新部署需显式初始化管理员密码；不能只回滚旧二进制读取新凭据格式。第三方 AuthURL 与开放平台独立应用凭据保持各自生命周期；已建立的媒体流及在途请求不强制终止。迁移、会话边界和人工检查见 [账号安全说明](admin-session-security.md)。修改仅在工作区，未部署。

### R03 · 已修复：AI 任务恢复与规模上限

在线通道第一页 999 条截断和 Go 内存缓存对账已移除：改为唯一 ID 游标完整扫描，通过 Python GetStatus 获取实际任务状态，分页或状态查询失败时不启停。Python 重启后补建缺失任务；终态任务必须先确认旧线程/采集进程停止才重建，停止超时保留任务及容量。手工开关和单步同步协调，执行前重读配置，迟到回调不影响新任务。

按用户确认默认 32 路，可通过 `Server.AI.MaxTasks` 配置；手动新启用超限返回错误，原已启用任务保留配置等待空位；离线停止，上线及失败任务由后续同步恢复。同步间隔为每轮结束后 5 分钟，整轮及 RPC 均设超时。推理异常计数和停止后的背景图释放同时补齐；回调并发策略仍属 R07。

检查：Go 定向 AI 回归及 race 通过，包含 1001 通道、空实际状态模拟重启、终态恢复、查询失败不启停、关闭竞态和容量等待；Python 9 项模拟测试通过，包含 40 请求只准入 32 个和停止未完成不重建。未做真实模型/摄像头负载验收。发布需配套升级 Go/Python；超过 32 路的现有部署需先调整上限。详见 [AI 任务恢复说明](ai-task-recovery.md)。修改仅在工作区，未部署。

### R04 · 已修复：ONVIF 状态并发与失败重试

ONVIF 连接、心跳字段、状态提交、目录同步、凭据更新和设备删除纳入共同设备操作锁。状态按设备主键定位，设备/通道在线状态在同一事务提交，成功后才确认内存状态；失败保留差异继续重试，首次探测失败不再被零心跳时间跳过。

后台改为可取消、最多 8 并发的完整设备扫描，自动重试首次连接失败、目录保存失败和删除回滚的设备。凭据事务提交后废弃旧连接，后续初始化失败仍使用已提交的新配置重试；迟到初始化重读数据库，不恢复旧密码或重建设备。沿用心跳轮次间隔 30 秒、离线阈值 70 秒及 HTTP 单请求 3 秒超时。

检查：8 项 ONVIF 模拟回归及 race、现有 IPC 编辑/删除定向回归及 race 通过，API/app 包编译通过；未连接真实设备。无需数据库迁移和新配置，更新 Go 程序生效。已有媒体代理会话不主动中断。详见 [ONVIF 状态恢复说明](onvif-state-recovery.md)。修改仅在工作区，未部署。

### R05 · 已修复：事件清理的文件/数据库一致性

位置：`internal/core/event/cleanup.go:50`、`:94`、`:99`。

自动清理和手工删除统一为数据库事务删除事件及关联记录、同时保存附件清理任务，提交后才删除文件。新增持久任务表，文件删除失败或进程中断后每分钟重试；共享文件仍被引用时保留。取证入队、领取和删除使用统一事件行锁，事务内再次核对任务状态。清理查询和事务传递调用方 context，取消后不继续新批次。

发布需创建 `event_media_cleanup_tasks` 表及事件附件路径索引，已纳入现有 AutoMigrate 与数据库迁移工具。保留天数和实时取证策略未改；逐事件短事务比旧批量 SQL 增加数据库操作次数，大量历史积压需观察耗时。故障回归仅操作临时数据库和测试文件，未清理生产数据。实现和恢复说明见 [事件清理一致性](event-cleanup-consistency.md)。

## 待审核：性能、维护与兼容

| 编号 / 优先级 | 证据与影响 | 建议审核的下一步 |
|---|---|---|
| R06 / 中 | `analysis/main.py:200` 的每个区域都对同一整帧调用检测；`AnalysisServiceServicer` 多任务共享 detector，TFLite 后端修改同一 interpreter（`analysis/detect.py:297`） | 确认 ONNX/TFLite 实际使用范围，比较单次推理后区域筛选、串行锁和每 worker 模型实例。区域过滤顺序及 NMS 语义需回归，未盲目重写 |
| R07 / 中（部分修复） | 推理异常预算与终态重建已随 R03 修复；`send_callback` 仍每事件新建线程，异常处理包围线程创建而非网络请求 | 尚需定义回调并发上限、丢弃/重试策略，避免不受控线程。FFmpeg 的独立重试问题已在 F06 修复 |
| R08 / 中 | `analysis/frame_capture.py:104` 等仍打印 RTSP 地址，地址可能带用户名/密码；本轮 HTTP 日志修复不覆盖这些业务日志 | 统一媒体 URL 脱敏，并检查已有日志的权限/保留期；不把所有历史日志自动清空 |
| R09 / 中 | `internal/web/api/api.go` 的指标采集和 AI 同步使用后台 context；ONVIF 也在构造器启动后台 worker；`internal/rpc/rpc.go` 没有显式连接关闭接口 | 将后台任务和 RPC 连接纳入宿主关闭链，确认退出时待完成任务的期限，避免重建 handler 或嵌入式运行时泄漏 |
| R10 / 中 | `internal/web/api/auth.go:91` 第三方鉴权转发会完整读入 body 并忽略读取错误；移除 body logger 只消除了更早的一层读取 | 按 JSON、快照、升级包分别定义限额及流式鉴权契约。不能简单给全站加 1 MiB 限额，否则大文件升级会被误伤 |
| R11 / 中 | `Dockerfile_ai:31` 只应用 ZLM 补丁 0001；`Dockerfile_zlm:29` 同时应用 0001/0002/0003；仍有浮动镜像标签和未锁定 Python 间接依赖 | 确认 AI 融合镜像是否也需支持接入网关 RTCP；决定共享构建阶段、补丁一致性和制品锁定策略。未运行耗时镜像构建或发布 |
| R12 / 中 | 裸 `go test ./...` 纳入被 Git 忽略的 `build/deploy-v1/.../source`；其中两处残缺历史源码编译失败。当前 `build` 约 12 GB，`.pnpm-store` 约 254 MB | 确认哪些发布归档用于回滚，再迁出源码树或使用独立模块/明确源码包检查范围。不要直接 `rm -rf build`；`make audit` 的全树写格式化也需避免触碰这些归档 |
| R13 / 低 | `internal/web/api/proxy.go`、`push.go` 的注册函数未在现有路由装配中调用；对应旧 core 包仍保留。`recording_video.go` 未挂载，M3U8 辅助函数返回空串。`pkg/ota/ota.go:55` 的 Download 只返回自身 | 确认是否仍有外部分支/包消费者，决定删除或标记废弃。未仅凭本仓库“无调用”批量删除公开 API、数据迁移类型或旧表支持 |
| R14 / 低 | `security_best_practices_report.md` 是旧快照；部分结论已被当前 webhook 认证、录像路径校验、受保护事件/录像路由、loopback AI 服务、播放凭据代理等修复覆盖 | 保留历史证据，以本轮清单追踪当前事项；需要完整安全结论时另做依赖漏洞、密钥历史和实际部署检查，不能复用旧漏洞总数 |

## 检查结果

1. 当前源码包 Go 检查通过：44 个含测试的包通过，其余声明无测试。命令：

   ```sh
   GOCACHE=/tmp/owl-audit-go-cache go test . ./docs/... ./internal/... ./pkg/... ./plugin/... ./extensions/... ./tools/... ./examples/...
   ```

   首轮默认 Go 缓存目录受到权限限制，改为临时缓存后继续。裸 `go test ./...` 的历史构建目录错误见 R12；认证测试的外部代理依赖已在 F11 修复。未将裸全树命令描述为通过。

2. 认证/日志/指标的定向 race 检查通过：

   ```sh
   GOCACHE=/tmp/owl-audit-go-cache go test -race ./plugin/stat ./internal/web/api -run 'Test(NetworkCounterFailureDoesNotCrashService|CollectTopConcurrentReaders|ConcurrentLoginKeysRemainUsable|UserUpdateRunsAuthenticationOnce|RequestLoggerPreservesRequestWithoutLoggingSecrets)$'
   ```

3. `pnpm --dir admin-ui test`：82 项通过；`pnpm --dir admin-ui build`：在线文档一致性、Vue 类型检查与 Vite 生产构建通过。
4. `python3 -m unittest discover -s analysis -p 'test_runtime.py'`：3 项通过，覆盖未就绪健康检查、持续退出/无法启动的重试上限、日志管道关闭。使用模拟的模型/gRPC 边界；本机没有 OpenCV/gRPC Python 运行依赖，不代表真实模型推理通过。
5. 以临时目录模拟 Docker 前端 COPY 布局，运行在线文档校验脚本通过；Makefile dry-run 确认 Python 依赖路径和链接变量；`git diff --check` 通过。完整 Docker 构建、GitHub Actions 执行与发布资产启动未验证。
6. R01 后续修复检查：`GOCACHE=/tmp/owl-audit-go-cache go test ./internal/web/api ./internal/conf` 与 `GOCACHE=/tmp/owl-audit-go-cache go test -race ./internal/web/api -run '^TestPProf' -count=1` 均通过。覆盖全部已注册诊断路由、伪造转发头、匿名本机代理、无效/过期令牌、空白名单、禁用状态、管理员访问及普通路由行为。生产代理和容器实际来源配置仍需现场确认。
7. R02 后续修复检查：`go test ./internal/web/api ./internal/conf ./internal/web/onvifserver` 通过（使用上述临时 Go 缓存）；会话、密码迁移、RSA 轮换、ONVIF、pprof 的定向 race 检查通过。最后补充配置快照与改密并发保护后，重新执行 `go test -race ./internal/web/api -run '^TestSession' -count=1` 通过。`pnpm --dir admin-ui exec vue-tsc --noEmit`、两个配置示例的 TOML 结构检查和 `git diff --check` 通过。覆盖旧明文迁移、环境变量优先级、保存失败、各管理入口/播放代理的旧令牌拒绝、重启后的撤销状态、WebSocket 断开重连及 ONVIF 独立认证；未连接真实 ONVIF 客户端或执行发布。
8. R05 后续修复检查：`go test ./internal/core/event/... ./internal/web/api ./tools/db-migrate` 通过；`go test -race ./internal/core/event/... -run 'Test(Cleanup|ExpiredEventCleanup|ManualDeletion|EventDelete|EventEvidence|Alarm)' -count=1` 通过（均使用上述临时 Go 缓存）。新增附件路径索引后重跑事件包和迁移工具检查通过，`git diff --check` 通过。故障回归覆盖事务回滚保留文件、零影响行、取消、缺表恢复、提交后重启、文件失败重试、清理任务提交失败、共享文件、候选查询后入队及执行中的取证保护。仅使用临时数据库和文件，未执行生产迁移或清理。

现场验收优先三项：多设备并发登录与凭据修改；设备列表切换网关/协议/分页；配置非默认 HTTP 端口启动 AI，并模拟摄像头持续掉线确认重试与告警状态。此处为人工待验，不是已通过项。
