# A / B / ZLM 统一发布工具

`release.py` 由运维机执行，不向管理页面开放 SSH 或远程命令。默认仅预检；只有 `--apply` 才排空、备份和替换程序。本工具本轮没有在生产服务器执行。

## 准备清单

复制 `manifest.example.json`，填入实际路径、版本和 SHA256。必须列出 A/B/ZLM 三个组件的当前身份，只在 `steps` 列出需要升级的组件。A/ZLM 的身份是 Docker image ID（不含 `sha256:`），`file_sha256` 是 `docker save` 归档摘要；B 的身份是 Linux ELF 二进制摘要。版本标签用于展示，摘要用于核对。不要使用镜像 tag 代替摘要。

`compatible_sets` 必须包含当前组合以及每一步升级后的组合；需先审核协议和数据库兼容性。工具校验清单一致性，不能代替兼容性测试。`schema_backward_compatible` 必须为 true；不可逆迁移不得用此工具自动回退程序。

要求：Python 3.9+、SSH/SCP、已确认的 known_hosts、只读权限受控的 SSH 密钥；目标机 Python 3、Linux flock。B 采用 `directory/current` 指向 `releases/<release_id>` 的软链，unit 的 ExecStart 使用该路径和清单配置路径。A/ZLM 使用 Docker Compose，服务须配置原生 healthcheck。A 的数据库备份当前支持 Compose 中 PostgreSQL 的 `pg_dump -Fc`；其他数据库应使用对应发布流程，不能填写虚假 PostgreSQL 参数。

`backup_paths` 除必填 Compose 文件或 B 配置/状态目录，还应列出所有实际配置、证书、A 凭据目录和部署 env 文件。归档含敏感信息，远端目录0700、文件0600；备份加密和异地保留由运维流程完成。数据库先停 A 写入再备份；如还有外部写入者，须在执行前停止。

通过环境变量 `OWL_RELEASE_TOKEN` 提供有效的当前管理员 Bearer（不是开放平台 AK/SK）。`api_base` 指向实际管理 API 根路径，例如 `https://owl.example.com/api/v1`，不含 `/access_gateways`。不要把 token 写入清单、命令行参数或仓库。HTTP 地址仅供受控内网环境。

```sh
python3 tools/gateway-release/release.py /secure/release.json --report /secure/preflight-result.json
python3 tools/gateway-release/release.py /secure/release.json --apply --report /secure/apply-result.json
```

报告路径必须不存在，工具先以0600独占创建。每次发布使用新的 `release_id`；预检与 apply 可以使用同一清单，但报告路径须不同。

## 执行与失败处理

1. 只读核对三个线上组件身份、健康、B unit 路径、备份源和网关管理状态。
2. 在安装目录取得持久发布租约，并逐网关非强制排空；未收敛则停止，不中断现有业务硬升级。
3. 按 `steps` 上传并校验产物，停目标组件、备份、原子切换 B 软链或 Compose 的该服务镜像；不重启其他 Compose 服务。
4. 每一步核对当前组合的摘要与健康，等待 A/B 控制连接、配置版本、ready 状态恢复，再进行下一步。最后恢复 enabled，并回写管理审计。
5. 失败按逆序恢复已开始步骤的程序。**不自动恢复数据库或 B 状态**，以免删除期间收到的新报警。只有通过回退组合健康验证后才解除租约；回退失败保留维护状态和租约。

安装目录中的 `.owl-release-active/owner.json` 阻止另一个发布交错；`.gateway-release.lock` 只保护一次 SSH 调用。进程崩溃/被杀后租约不会自动过期：先检查私有报告、`backups/<release_id>/<role>/previous.json`、configuration.tar、A database.dump、current 和实际容器/进程，完成恢复后才人工清除租约。报告中的 lease_token 也属于私有运维数据。CLI 被强杀时本地报告可能未完整写入，远端备份和租约是恢复依据。不要用删除状态目录或数据库来解除故障。

新 A 报告 API 暂不可用或已回退旧 A 时，报告保存在本地 `pending_report`，不会因为审计回写失败撤销已成功程序；随后按原发布信息补报。报告出现 `release_lease: manual_check_required` 时仍需人工核对释放状态。配置代理/反向代理的读取超时需覆盖单次通道诊断（建议至少65秒）。

首次启用数据库0.0.36前，停旧 A 并备份，执行 `go run ./tools/accessgateway-migrate`，再执行 `-check`。环境变量 `OWL_DATABASE_DSN` 必须指向原库，SQLite使用已有数据库绝对路径。若同批包括开放平台1.1变更，还须按开放平台手册执行 `tools/openplatform-migrate`；本工具不自动猜测和执行迁移。
