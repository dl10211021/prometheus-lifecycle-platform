# Prometheus 参数迁移预览

在“新建生命周期变更”中选择 Linux 二进制 Prometheus、选择“升级”、填写目标版本后，点击“预览参数迁移”。只有 change_admin 和 system_admin 可以访问。

预览按数据库中最新保存的组件参数计算。导入快照优先，自定义参数按现有执行器规则覆盖；它不是实时读取 systemd 的结果。页面展示参数来源、源/目标版本、转换前后已知规则的检查结果、保留值和阻塞原因。

本步骤只提供建议，不修改组件、启动参数、systemd unit、计划、备份或审计记录，不颁发执行令牌。“检查并执行”仍使用当前参数并重新走完整准入；有旧参数的 3.x 升级不会因预览成功而放行。

## 首批规则

只把 `--storage.tsdb.retention` 改为 `--storage.tsdb.retention.time`，保留位置、等号/分离 token 形式以及时长文字。例如：

```text
--storage.tsdb.retention=045d
→ --storage.tsdb.retention.time=045d
```

- 目标低于 2.7：没有可用的新参数名，不做替换；已有参数仍须通过已知规则。
- 目标 2.7–2.x：可以提前规范化旧参数。
- 目标 3.x：旧参数已经移除，必须先完成迁移后再升级。
- 重复旧参数、新旧同时存在、缺失/无效时长、选项终止符、源参数不兼容、转换后仍不兼容、未知/预发布版本都会阻塞。
- 两端均不低于 2.55 时，支持单独的 `0` 及按顺序排列的整数 y、w、d、h、m、s、ms，检查有符号纳秒溢出，最长 128 字符。
- 任一端低于 2.55 时，只为**正整数单一单位**提供建议。复合时长、裸零和 `0s` 等零值需要人工核查，避免旧解析器语法和零值含义差异。2.55 是本规则采用的保守验证基线，不是这些语法的实际引入版本。
- 不删除 WAL 压缩等不兼容参数；不把无法转换的值换成默认值。未知参数保持原样，但不在这组规则的兼容认证范围。

完整有效参数限制为 4096 个、合计 1 MiB；原始导入 JSON 最多 2 MiB、自定义参数文本最多 1 MiB。过大或格式无效的保存参数返回受阻结果，未完成的校验不会标记为通过。

## 接口

```http
GET /api/v1/components/41/parameter-migration-preview?target_version=3.1.0
Authorization: Bearer <token>
```

响应含 `component_id` 和 `preview`。状态为 `available`、`not_needed`、`blocked` 或 `not_applicable`，且 `preview_only=true`、`scope=known_cli_parameter_rules_only`。不返回完整 argv、环境变量、主机凭据或可执行计划。

接口要求唯一且非空的目标版本，不接受源版本或参数覆盖字段，沿用平台的单节点范围中间件。不存在的组件返回 404，读取失败返回不带数据库细节的 500；受阻/不适用是可展示的 200 预览结果。响应禁止缓存。

页面切换组件、版本、变更类型、账号会话/权限或关闭窗口会清除旧结果；晚到的旧请求无法覆盖当前建议。

## 验证与边界

- 模型和部署器：值与 token 保留、输入不变、冲突、版本/时长边界、导入和自定义覆盖、敏感参数不回传。
- 服务：升级被旧参数阻止时仍有预览证据，数据库、归档命令和运行时保持不变。
- 真实 HTTP：隔离服务器与数据库，覆盖五种角色、已有 JWT 撤权、最新保存参数、失败响应和零写入。
- 真实 Vue：渲染、只读请求、提交字段不变、失效与请求乱序、错误和文本转义。
- 官方 CLI：通过校验哈希的 2.6.1、2.7.0、2.55.0、3.1.0 二进制检查转换结果和边界。

CLI 值验证不能使用 `--version`：它会在部分参数值校验前退出。探针使用故意无效的 external URL，在参数值解析之后、配置/监听/TSDB 初始化之前退出，并确认专属目录没有产生运行文件。通过 `PROMLIFECYCLE_PARAMETER_CLI_BINARIES` 指定已有官方二进制清单，再运行：

```sh
go test -v ./core/deployer -run '^TestRealPrometheusParameterMigrationCLI$' -count=1
python3 test-scripts/parameter-migration-api-test.py --server /absolute/server --output /absolute/new-private-directory
python3 test-scripts/parameter-migration-preview-test.py
```

本步骤不代表完整升级路径、原生配置、未知参数、真实 unit 内容或 TSDB 跨版本语义已获认证。实际参数应用之前，下一步需要持久化完整原 unit、配套配置/数据快照，验证失败时能恢复原二进制、参数、unit 和数据。

依据：[3.0 参数移除说明](https://prometheus.io/docs/prometheus/3.0/migration/)、[2.7 主程序和旧保留时间选择逻辑](https://github.com/prometheus/prometheus/blob/v2.7.0/cmd/prometheus/main.go)、[2.55 主程序](https://github.com/prometheus/prometheus/blob/v2.55.0/cmd/prometheus/main.go)、[common/model 时长解析](https://github.com/prometheus/common/blob/v0.59.1/model/time.go)。
