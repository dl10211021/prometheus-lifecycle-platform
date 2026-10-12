# Prometheus 生命周期管理 · 单节点版

本发布源码快照来自已验收提交 `487e7cd4938b73b688bf7db756ab28ac5e1727d8`，不携带原开发历史或现场会话记录。离线安装包、依赖、使用步骤及验证范围见 [发布与使用指南](docs/RELEASE.md)。

一个本地 SQLite、一个内嵌 Web 控制台，默认管理本机，也可通过 SSH 管理远程 Linux 主机。产品只围绕 Prometheus 监控体系从部署、配置、运行维护到备份恢复的完整生命周期。

## 下载

- [v1.0.0 Release 与校验文件](https://github.com/dl10211021/prometheus-lifecycle-platform/releases/tag/v1.0.0) · [Linux x86_64 离线安装包](https://github.com/dl10211021/prometheus-lifecycle-platform/releases/download/v1.0.0/promlifecycle-v1.0.0-linux-amd64-offline.tar.gz)
- [GitHub Packages 安装归档](https://github.com/users/dl10211021/packages/container/package/prometheus-lifecycle-platform-installer)：通过 ORAS 下载同一离线包与 `.sha256` 文件。它是 OCI 文件制品，不能用 `docker run` 运行平台；解压后仍按原生 systemd 方式安装。命令与发布流程见 [发布指南](docs/RELEASE.md#github-packages-分发)。

## 能力

- **运行总览**：组件健康率、活跃告警、采集与规则、备份、变更成功率、组件状态与类型分布全部图表化展示。
- **部署管理**：自动创建本机、环境预检、按 PID/端口/二进制路径发现同类型多实例、观察或托管接管；未纳管发现项可在证据复核后停止并删除，配置与数据默认保留。
- **组件生命周期**：统一使用 binary+systemd；本机直接安装，远程通过 SSH 上传。支持版本选择、部署进度、启动、停止、重启、参数编辑、健康、日志、升级、回滚和卸载；安装必须验证服务状态及就绪探测，卸载必须验证 PID、端口、unit 和文件状态。
- **旧安装治理**：按 PID、端口、任意二进制路径和任意 systemd unit 名发现 active/inactive 实例；RPM/DEB 走包管理卸载，独立二进制按证据路径卸载，配置与数据默认保留。
- **监控配置**：告警规则、采集目标、记录规则、远程写、Grafana Dashboard、通知渠道和 Alertmanager 路由/全局设置均支持新增、编辑、删除与同步。
- **运维维护**：配置先校验再执行，历史版本可回滚；升级和移除直接执行，执行前生成 Diff、风险和恢复点，执行后保留健康验证与回滚证据。
- **可验证恢复**：支持手动 config/data/full 备份、定时备份、SHA-256 manifest、恢复演练、平台 SQLite 手动快照、下载和暂存恢复；恢复前自动创建 `pre_restore`。
- **五角色权限**：`viewer`、`operator`、`change_admin`、`system_admin`、`auditor`；包、用户和高级系统设置仅 system_admin，审计导出仅 auditor/system_admin。
- **离线与安全**：vendor 离线构建、官方/自定义安装包永久入库、AES-256-GCM 凭据、SSH 主机密钥、SSRF、CORS 白名单、登录限流和失败操作审计。

## 要求

- 构建：Go **1.26.9 或更新的安全补丁版本**、C 编译器（`go-sqlite3` 启用 CGO）；离线构建前必须准备好工具链
- 发布包运行/安装：Linux amd64、glibc ≥ 2.28、systemd、Python 3；使用预编译二进制无需 Go/C 编译器
- 远程 SSH 密码测试脚本：可选的 `sshpass`

## 构建

仓库提交了与 `go.mod`/`go.sum` 对应的 `vendor/`，Makefile 和部署脚本都使用 `GOFLAGS=-mod=vendor`。干净 clone 不需要访问 Go module proxy。

```bash
# 使用已提交依赖构建
make build

# 统一验证入口
make unit
make vet
make test-local
make test-all       # 未提供远程凭据时，远程矩阵明确显示 SKIP
```
开发验证推荐使用 `make unit`：每个 Go 测试包默认超时为 20 分钟，包含完整生命周期恢复测试；可用 `make unit GO_TEST_TIMEOUT=30m` 覆盖。
联网环境也可以显式绕过 vendor、按 `go.sum` 从 module proxy 构建：

```bash
GOFLAGS=-mod=mod go build -trimpath -o promlifecycle-server ./cmd/server
```


需要升级依赖时，只能在联网环境更新并重新提交完整 vendor 树：

```bash
GOFLAGS=-mod=mod go mod download
go mod vendor
GOFLAGS=-mod=vendor make unit
```


## 安装与升级

正式交付包优先安装已验证的预编译二进制，先在解压目录执行校验：

```bash
sha256sum -c SHA256SUMS
sudo bash deploy.sh --binary ./bin/promlifecycle-server
```

`--binary` 同样适用于升级；安装前检查二进制能否在当前主机运行。以下是不带 `--binary` 时的源码构建安装方式：

```bash
sudo bash deploy.sh
# 可选参数：
sudo bash deploy.sh --port 8080 --base /opt/promlifecycle \
  --data /opt/promlifecycle/data
```

首次安装会：

1. 使用 `--binary` 指定的发布文件，或使用 vendor 依赖构建服务；
2. 在 loopback 临时初始化数据库；
3. 生成随机管理员初始密码并保存到 `/opt/promlifecycle/admin_initial_password`（`0600`）；
4. 生成 JWT/凭据加密密钥并保存到 `/opt/promlifecycle/jwt_secret`（`0600`）；
5. 安装并启动 systemd 服务。

查看初始密码需要 root 权限：

```bash
sudo cat /opt/promlifecycle/admin_initial_password
```

再次运行安装命令执行升级：保留现有有效 systemd unit/drop-ins、启动参数、SQLite 数据、管理员密码、JWT/凭据密钥和已入库安装包，不重新生成或迁移现有密钥。路径和端口选项只用于新装；自定义新装路径可通过参数或同名环境变量传入：`BASE_DIR`、`DATA_DIR`、`JWT_SECRET_FILE`、`ADMIN_PASSWORD_FILE`、`PORT`。

升级失败会保留失败证据并恢复原二进制、数据库及服务配置；原有密钥和运行参数不能被新安装默认值覆盖。构建启用 CGO，发布二进制必须兼容目标主机的 glibc；开发机编译成功不等于可直接部署到旧发行版。

## 启动参数与环境变量

```bash
PROMLIFECYCLE_JWT_SECRET_FILE=/opt/promlifecycle/jwt_secret \
./promlifecycle-server \
  --listen-address 127.0.0.1 \
  --port 8080 \
  --base-path /opt/promlifecycle \
  --data-dir /opt/promlifecycle/data
```

| 变量 | 说明 |
|---|---|
| `PROMLIFECYCLE_OFFLINE=1` | 禁止在线版本检查与下载；保留本地版本目录，已入库安装包与本地操作仍可用 |
| `PROMLIFECYCLE_JWT_SECRET_FILE` | JWT 签名及凭据加密密钥文件；生产必须放在持久存储并设为 `0600` |
| `PROMLIFECYCLE_JWT_SECRET` | 直接提供密钥；生产更推荐文件方式 |
| `PROMLIFECYCLE_ALLOW_PRIVATE_FETCH=1` | 明确允许仪表盘导入访问私网/loopback 地址 |
| `PROMLIFECYCLE_SSH_HOST_KEY_MODE` | `accept_new`、`known_hosts` 或不推荐的 `insecure` |
| `PROMLIFECYCLE_SSH_KNOWN_HOSTS` | known_hosts 文件路径 |
| `PROMLIFECYCLE_SSH_TIMEOUT` | SSH 远程命令超时 |
| `PROMLIFECYCLE_DOWNLOAD_MIRROR` | 官方包下载镜像前缀 |
| `PROMLIFECYCLE_SMTP_HOST/PORT/USERNAME/PASSWORD/FROM` | SMTP 通知参数 |
| `PROMLIFECYCLE_SMTP_REQUIRE_TLS` | 默认 true，传给 Alertmanager 的 `smtp_require_tls`；平台直发当前不应用此开关，详见下文 |
| `PROMLIFECYCLE_ADMIN_PASSWORD` / `_FILE` | 首次 system_admin 密码；未配置时生成一次性随机密码 |
| `PROMLIFECYCLE_ALLOWED_ORIGINS` | 允许跨域的来源白名单，逗号分隔；默认同源 |
| `PROMLIFECYCLE_RESOURCE_RETENTION_DAYS` | 资源采样保留天数，默认 30 |
| `PROMLIFECYCLE_SNAPSHOT_MAX_BYTES` | 平台快照创建与恢复共用大小上限；默认 `0` 不设固定上限，仍检查磁盘空间 |
| `PROMLIFECYCLE_SNAPSHOT_RESERVE_BYTES` | 平台快照操作后的磁盘预留，默认 `67108864`（64 MiB） |

组织邮件必须分别验收平台直发和 Alertmanager 路由，并在实际收件箱确认唯一测试标识，不能只看接口 `200/sent`。平台直发使用 Go `net/smtp` 的机会式 STARTTLS + AUTH PLAIN，不支持隐式 TLS 465，也不能承诺仅 LOGIN/OAuth 或匿名 relay 兼容性；组织若要求强制 STARTTLS，现有直发路径需先适配。已有受控 SMTP 收信证据不等于组织 SMTP/TLS 验收。


### 离线版本刷新与配置校验

手动刷新与每日调度共用版本检查流程，逐组件返回成功、失败、跳过或取消，并分别统计新增版本、离线包实际入库及下载失败。离线模式明确返回 `offline`，不会把未检查或全部失败报告为“已是最新”。Docker Hub 与 GitHub 的版本请求分别共享失败退避：单次请求超时 8 秒，失败后暂停 1–5 分钟，退避期间结果包含 `retry_at`。

`POST /api/v1/versions/refresh` 返回后台任务 ID；通过 `/api/v1/jobs/:id` 读取 `version_refresh` 汇总、逐项详情和 `finished_at`。指定 `?type=prometheus` 时直接返回 `summary` 与本地版本目录。离线模式返回 200；部分成功返回 207；上游失败或退避返回 503；取消返回 408。

平台进程的 `PATH` 中安装与受管版本一致的 `promtool`、`amtool` 后，配置预校验及公共配置发布校验会使用原生工具。工具可从对应版本的官方离线归档提取；不要仅因新版包可用而用新版校验器替代现网版本。只有可执行文件确实缺失时，预校验接口才使用明确标识为 `basic` 的基础校验；已安装工具的语义错误（包括 `not found` 诊断）返回 409，不降级放行。受管变更还会从组件同版本离线包准备专用校验器。

## 密钥与备份

`jwt_secret` 同时保护 JWT 与 SSH/WinRM 凭据。**只备份 SQLite 数据库是不完整的**：必须一起备份以下内容，并限制备份读取权限。

```text
/opt/promlifecycle/jwt_secret
/opt/promlifecycle/data/promlifecycle.db
/opt/promlifecycle/backups/
/opt/promlifecycle/packages/
```

丢失或错误替换 `jwt_secret` 后，数据库中的加密凭据无法恢复。升级时不要生成新密钥覆盖旧文件。恢复平台快照时也必须配套恢复原密钥。

### 恢复语义与中断处理

- config 备份保留原始权限和所有权，使用受保护的备份容器，不修改业务目录权限。data/full 备份先停止应用以取得一致性副本，再恢复操作前的运行或停止状态。
- 恢复前检查 manifest、校验和及一致性声明；缺失配置或数据路径也会记录为可回退的原始状态。旧备份缺少一致性声明时可校验或演练，但不能冒充一致性快照执行破坏性恢复。
- 演练结果明确区分归档完整性、解包验证与实际应用恢复验证；不能把文件可解包等同于业务可用。取消和超时后的恢复使用独立上下文；远程终止无法确认时保留阻断状态，必须先核实远端进程，不能直接重放破坏性操作。
- 有数据的组件升级创建 data/full 恢复点。回退必须先停止服务、恢复匹配的数据和配置，再安装并启动旧版本；尤其不能让 Grafana 11 先打开 Grafana 13 已迁移的数据库。底层二进制失败补偿只恢复文件并保持服务停止，由治理恢复流程完成数据恢复和就绪验证。二进制状态不明或跨版本恢复期间平台中断时保留持久阻断，不盲目启动数据格式不匹配的版本；成功后原子完成恢复记录并释放后续操作。
- 定时组件备份进入持久队列，由两个工作槽执行，避免占用调度主循环；减少保留数量后会继续清理积压。
- 平台快照执行 SQLite 完整性和 schema 检查，按目标数据库文件系统暂存。恢复接口返回 `202/pending` 不代表已切换；重启并完成迁移与初始化后才成为 `applied`。原库在启动确认前保留，失败或中断会回退；创建与恢复使用同一大小及空间策略。

### 发布、通知与权限证据

监控资源返回 `apply_status`、`last_error`、`applied_at` 和删除待发布状态。`pending/error/manual` 不等于生效，删除意图会保留到实际撤回成功。Prometheus 配置与规则统一校验、串行发布；内容不变不重复重载，故障组件仍保留为采集目标。手工配置与自动协调共享互斥边界，不能互相覆盖。

“通知与送达”分别展示平台直发能力、Alertmanager 路由能力与逐渠道结果。持久投递记录按故障/恢复转换去重，失败项可跨重启重试，成功项不重发。`unknown` 表示发送中断后结果不明，不自动重放；系统管理员通过 `POST /api/v1/notifications/deliveries/:id/resolve` 提交 `confirmed_sent` 或 `retry` 及核实理由，承担重复发送风险并留下审计记录。维护与静默必须提供有效范围，不完整身份标签不能静默扩大为全局匹配。

配置当前值、历史及变更差异默认结构化脱敏；需要编辑原文时显式使用 `raw=1`，服务端另行验证变更权限。平台认证头不会转发给下游服务。密码更新撤销旧会话，拒绝超过 bcrypt 72 字节边界的密码；退出会清除表单凭据与 AI 草稿。SSH 主机密钥模式拼写错误会被拒绝，不回退到宽松模式。

## 主机接入、状态与批量粘贴

“部署管理 → 主机”区分管理连接状态与 Prometheus 指标状态：

- 勾选“立即验证连接”：SSH 连接、握手和系统检测，或 WinRM 认证与请求，共用约 20 秒截止时间。验证通过才新建；失败明确返回原因，不留下没有任务执行的 `pending`。
- 关闭立即验证：只保存为 `unknown`（界面显示“未验证”），不伪造检查时间，也不意味着正在检测。
- 对已有主机执行“连通测试”：失败保存 `offline`、`last_error`、`last_checked_at`；成功改为 `online`，清除错误并更新 `last_online`。这不是周期性在线监测；实时指标请看“监控状态”。
- 升级启动时，旧的无任务 `pending` 主机迁移为 `unknown`，保留已有证据；不会凭空判为在线或离线。
- 同 IP 已存在时保留原记录、凭据和状态，不重新验证或自动部署。单台 API 返回 HTTP 200 `{status:"exists",id,host}`；新建成功返回 HTTP 201。

批量添加不需要 JSON。每行 `IPv4,用户名,密码[,SSH端口[,名称]]`，也支持 Excel Tab 或空白分隔；默认 Linux / SSH / 22，名称省略时使用 IP：

```text
192.0.2.10,root,ExamplePass!
192.0.2.11,deploy," p,a ss\word! ",2222,node-b
192.0.2.12 root ExamplePass! 22 node-c
```

密码含逗号、Tab 或空格时用双引号包裹；密码中的双引号写成两个双引号，反斜杠、`$`、`!` 原样粘贴。密码不裁剪空白。忽略空行，错误与结果保留原文行号；一批最多 200 台。API `POST /hosts/batch` 仍接收结构化数组，逐项返回零基 `index` 和 `created/exists/failed`。

批量最多同时检查 5 个不同 IP，同批重复 IP 按原顺序处理，因此大批离线主机的总耗时可能超过 20 秒。单台和批量请求不锁住其他主机添加：可关闭弹窗继续添加，迟到结果不会覆盖新表单。关闭批量窗口会清除可编辑草稿，已发送请求完成后清理临时凭据；重新打开可看结果，关闭过的失败项需重新填写凭据。结果缺失、重复索引或网络中断显示“结果未知”，先刷新列表核对，不自动重放。

本页提交记录不是持久后台任务。关闭弹窗不取消已发送请求，但刷新页面或退出可能中断尚未完成的检查；已经接受的主机不会因此自动撤销。浏览器不存储主机凭据。

## 界面说明与常用参数

所有主功能页、子页及组件详情共 34 处上下文说明：默认显示用途，展开查看实用场景、操作步骤与风险。说明对应当前功能，不需要跳转查找文档。

部署向导默认只显示版本、端口、保留时间或必要凭据；无默认值的必填自定义参数仍可见。路径、调优、环境变量、资源限制折叠到高级选项；组件参数编辑、采集发现、标签和远程写认证也分层展示。收起不重置、不清空已有配置。静态采集目标可按每行 `地址:端口` 填写，无需 JSON；动态服务发现、Relabel 等仍保留原生高级编辑。

## 多主机批量部署

在“部署管理 → 批量部署”新建批次，选择 1–50 台已登记主机和组件，复用部署模板、版本及参数，然后逐主机预检并提交。一个批次内各主机使用相同组件配置，不自动覆盖已有受管实例；需要替换时使用原单主机部署流程。

批次及每台主机的组件步骤一次性持久化到 SQLite。第一版复用现有串行执行队列；某台主机失败后继续后续主机，页面关闭不影响执行。详情显示主机结果、组件步骤和关联操作，可仅重试失败或中断主机，并保留成功步骤。服务器重启后进行中的步骤标记为中断，需显式重试。

浏览器在发送前保存非敏感提交标记。响应丢失时，可按原标识查询或在原表单中重试；关闭或刷新后仍可查回原批次。草稿中的凭据不写入浏览器存储。API 使用调用方生成的唯一 `request_id`：同标识同内容返回原批次，不重复入队；同标识不同内容返回 409。

接口：`POST/GET /api/v1/deployments/batches`、`GET /api/v1/deployments/batches/:id`、`POST /api/v1/deployments/batches/:id/retry`。列表展示最近 50 个批次，也支持按 `request_id` 精确查询。写操作沿用部署管理权限。前端行为回归：`node test-scripts/deployment-batches-ui.test.cjs`。

### 升级已有组件

同一页点击“升级已有组件”，按实例勾选组件（同一主机可选多个实例），为各类型指定目标版本、填写原因并预检。每批最多 200 个实例、50 台主机，沿用服务端串行队列和原单组件升级引擎，不新建替代组件记录。

已有主机上的服务先通过“发现接管”扫描；仅观察实例需要完成托管接管后才能升级。组件详情中的停止、启动、卸载继续适用；卸载可选择只移出管理、保留数据或完整删除。

预检检查当前版本、托管状态、运行健康、目标安装包、磁盘和现有兼容性规则；执行前在组件操作锁内重新检查。排队期间账号被停用或失去变更权限、组件停止或配置变化会阻止升级。批次生成的计划不可单独编辑、删除或执行，成功后可从“升级详情 / 回退”进入原回退流程。

批次按主机显示进度、原版本和目标版本。重试只处理失败或中断项，成功步骤不重复执行；历史尝试及回退记录保留。结果未确定的失败操作须先恢复，避免直接重放远程修改。前端断线沿用原提交标识查询，刷新和服务重启后可读取持久记录。

升级预检接口为 `POST /api/v1/deployments/batches/preflight`；创建仍使用 `POST /api/v1/deployments/batches`，传入 `kind: "upgrade"`、`name`、`reason`、`request_id` 和 `upgrades: [{component_id, current_version, new_version}]`。读取、重试与新装批次共用接口。前端回归：`node test-scripts/upgrade-batches-ui.test.cjs`。

## 部署向导

部署组件栈前，向导会扫描目标主机。缺失组件默认勾选；已存在组件会提示风险但不禁用，用户可以自行勾选重新部署。外部同类型实例需要使用不同监听端口，受管组件则复用原记录执行覆盖部署。

版本下拉框区分“安装包已入库”和“安装包未入库”。下载或上传成功的归档永久保存在 `packages/`，部署和重启不会删除；只有系统管理员明确删除安装包才会移除文件。

“系统设置 → 离线包与版本”展示安全升级验收组合：Prometheus **3.13.3 LTS**、Alertmanager **0.27.0**、Grafana **13.1.5**、Node Exporter **1.8.2**、Blackbox Exporter **0.26.0**。2026-09-09 已在 Linux amd64 / binary+systemd / RHEL 8 glibc 2.28 验证安全升级版本实际运行、历史数据查询、Grafana SQLite 副本迁移和匹配旧数据的回退。组合不包含 Windows、日志栈或 Thanos，验收日期之后仍需持续核查公告。

此前的 Prometheus 3.1.0 / Grafana 11.1.0 组合仅是历史兼容性基线，分别缺少 [CVE-2026-42154](https://github.com/prometheus/prometheus/security/advisories/GHSA-8rm2-7qqf-34qm) 和 [CVE-2025-4123](https://grafana.com/security/security-advisories/cve-2025-4123/) 等修复，不再推荐。当前目标还包含后续官方安全更新；[Prometheus 3.13 支持至 2027-07-31](https://prometheus.io/docs/introduction/release-cycle/)，[Grafana 13.1 支持至 2027-03-20](https://grafana.com/docs/grafana/latest/upgrade-guide/when-to-upgrade/)。平台自身依赖升级不会替代这些独立组件升级。Grafana 13 的统一存储迁移要求恢复旧数据库才能可靠降级，必须同时保留原 Secret Key、配置、provisioning 和插件；只换回旧二进制不是数据回退。

Prometheus 与 Thanos 的升级目标及历史升级计划回滚目标，均按当前同主机 Sidecar 模型重新检查所有保留实例（包括停止、失败和未知状态；排除已移除和仅观察的 target 记录）。Prometheus 3 要求 Thanos 0.37.0 或更高的已识别正式版本；Thanos 缺少同主机 Prometheus、任一版本无法确认或依赖查询失败时拒绝执行，不采用其他计划中尚未生效的版本。手动回滚被拒绝时不改动组件或恢复数据；前向变更失败后若不能确认回滚兼容性，会保留恢复阻断及证据，等待人工处理。自定义跨主机端点拓扑和数据迁移兼容性不在此规则范围。

点击“使用此版本组合”只填入部署计划，不升级现有服务。向导按组合锁定版本，已有组件仍需显式勾选；缺少对应离线包时保留锁定版本并阻止部署，不自动换成其他版本。手工修改任一所选组件版本后，页面标记为自定义组合，需要重新验收。API 通过现有 `GET /api/v1/stack-templates` 的 `full_monitoring.version_profile` 提供版本、范围、日期和验收依据。

向导同时展示所有已选组件的独立配置卡片；常用字段默认可见，安装路径、数据路径、启动参数、环境变量和 systemd CPU/内存限制放在默认折叠的高级选项。“选择路径”可浏览本机或远程目标主机目录。确认后创建服务端持久整栈任务，关闭页面或退出不影响已接受任务；中断会明确记录，重试保留已完成步骤。安装成功但联调失败时可仅重试集成，不重新安装服务。修改已有组件的安装或数据路径不会隐式迁移文件，需先单独完成迁移。

路径必须是无 `..`、空白或引号的绝对路径。安装目录会在卸载时被平台清理，因此不能直接指向 `/`、`/etc`、`/usr`、`/var` 等系统目录；数据目录允许使用组件专用的 `/var/lib/<平台>/<组件>` 子目录。新实例的路径输入默认留空，由服务端按持久组件 ID 分配独立目录（下表 `{id}` 为组件 ID）；配置目录为 `/etc/promlifecycle-components/<type>-{id}`，与可能为 `0700` 的平台私密目录 `/etc/promlifecycle` 分离，不放宽后者权限。已有及接管实例留空重装时保留实际路径和启动参数；共享文件存在其他使用者时拒绝破坏性变更。单文件组件卸载只清理平台按 ID 分配且已为空的安装目录，不递归删除自定义或仍有文件的目录。

| 组件 | 默认安装路径 | 默认数据路径 |
|---|---|---|
| Node Exporter | `/opt/promlifecycle/components/node_exporter-{id}` | 无状态，留空 |
| Blackbox Exporter | `/opt/promlifecycle/components/blackbox_exporter-{id}` | 无状态，留空 |
| Prometheus | `/opt/promlifecycle/components/prometheus-{id}` | `/var/lib/promlifecycle/prometheus-{id}` |
| Alertmanager | `/opt/promlifecycle/components/alertmanager-{id}` | `/var/lib/promlifecycle/alertmanager-{id}` |
| Grafana | `/opt/promlifecycle/components/grafana-{id}` | `/var/lib/promlifecycle/grafana-{id}` |
| Pushgateway | `/opt/promlifecycle/components/pushgateway-{id}` | `/var/lib/promlifecycle/pushgateway-{id}` |
| Loki | `/opt/promlifecycle/components/loki-{id}` | `/var/lib/promlifecycle/loki-{id}` |
| Promtail | `/opt/promlifecycle/components/promtail-{id}` | `/var/lib/promlifecycle/promtail-{id}` |
| Thanos Sidecar | `/opt/promlifecycle/components/thanos-{id}` | 无状态，留空 |

组件卡片展示平台实际启用的最佳实践参数；覆盖参数留空即使用这些默认值。所有 binary 部署都会写入 systemd，执行 `enable` 和 `restart`，并同时验证 `is-enabled`、`is-active` 及组件就绪；进程存在不等于服务可用。

Blackbox 的探测日志级别不强制填入默认启动参数，避免旧版本不支持 `--log.prober` 而立即退出；显式填写前需核对所选版本。安装后会用该版本二进制及有效启动参数执行 `--config.check`，通过后才启动服务。

“完整监控”默认联调 Node Exporter、Blackbox Exporter、Prometheus、Alertmanager 和 Grafana：Prometheus 自动生成组件抓取、Blackbox HTTP 探测、告警路由和规则配置；Blackbox 内置 HTTP/TCP/ICMP/DNS 模块及 ICMP 能力；Grafana 默认中文、浏览器时区、关闭外部遥测，并自动配置 Prometheus 数据源。

部署完成结果和组件列表都会显示访问地址及“打开”按钮。本机组件保存的 loopback 地址会在浏览器端替换为当前平台主机名，避免用户从远程浏览器打开 `127.0.0.1`。Grafana 首次部署生成随机管理员密码，账号固定为 `admin`；凭据在向导和结果表中显示，密码写入 root-only `0600` EnvironmentFile，不嵌入 systemd unit。首次登录后应立即修改密码。

自定义整栈模板保存可复用的组件顺序、版本、端口、路径及安全参数，不保存主机凭据、Grafana 初始化密码、秘密环境变量或含凭据的参数。部署角色通过 `GET /api/v1/deployment-templates` 读取安全视图，无需开放系统模板管理权限；完整模板管理仍仅限系统管理员。

组件详情的“实际配置”读取目标主机当前生效文件：Prometheus `prometheus.yml`、Alertmanager `alertmanager.yml`、Blackbox `blackbox.yml`、Grafana `grafana.ini`、Loki/Promtail `config.yaml`。编辑会先进行结构/原生工具校验，再原子写入、重载或重启并验证健康；失败恢复原文件。Prometheus 告警规则仍在“监控配置 / 告警规则”独立管理并同步到 `rule_files`。

“监控配置 / 参数配置”按三步组织：①添加 Linux 监控机；②管理采集目标；③管理告警规则。常用操作不需要理解 YAML 或生命周期变更。下方组件配置使用友好卡片显示 Prometheus 采集、Alertmanager 通知、Blackbox 探测和 Grafana 界面配置；点击“编辑配置”只显示实际文件、配置内容、校验和“保存并应用”，内部组件选择、操作类型和变更原因均自动处理。平台仍保留原始 YAML/INI 编辑能力供高级场景使用。

每个组件卡片会实时判断“推荐基线已应用”或“已自定义/待优化”。“应用推荐配置”会恢复平台核对过的单节点默认配置、资源限制和 systemd 参数，再执行配置同步、重载与健康验证；原始 YAML/INI 被降级为“高级编辑”，普通接入无需修改启动参数或环境变量。

页面包含组件联调矩阵：Prometheus 运行状态、Prometheus→Node Exporter live targets、Prometheus→Blackbox 探测、Prometheus→Alertmanager、Grafana→Prometheus 数据源、Prometheus 告警规则。每项显示实时证据；“一键重新联调”会重新生成 Prometheus/Alertmanager/规则配置、Grafana 数据源并等待组件恢复健康。

Grafana 卡片提供“一键重置管理员密码”。平台先停止 Grafana，再通过 `runuser -u nobody` 以 Grafana 实际 systemd 用户运行官方 `grafana cli admin reset-admin-password`，显式传入 `homepath`、`grafana.ini` 和实际 `paths.data`；随后启动服务并调用网页同源 `/login` 验证新密码。只有登录返回 HTTP 200 才显示成功凭据。新密码同步到 root-only `0600` EnvironmentFile，systemd unit 不包含密码明文。

自动接入 Linux 监控机后，平台验证 SSH、安装并启用 Node Exporter、自动重建 `prometheus.yml`、Reload Prometheus，再查询 `/api/v1/targets`；只有配置中包含目标且 live target 为 `up` 才报告接入成功。

首次接入 Node Exporter 时自动创建并同步四条基础告警：`NodeExporterDown`、`HostHighCPU`、`HostHighMemory`、`HostDiskSpaceLow`。规则通过 Prometheus rule API 验证为健康后才算生效；用户仍可在“告警规则”页面修改阈值、持续时间、级别和注解。

默认首页参考 [containeroo/heartbeats](https://github.com/containeroo/heartbeats) 的交互模式，改为“监控状态”：只展示目标总数、正常、延迟、失联，以及状态、目标、最后采集、通知和历史。点击目标查看检查间隔、采集耗时、错误和可复制采集 URL。复杂图表继续放在运行总览；底层参数继续放在参数配置。

Grafana 联调会同时写入固定 UID `promlifecycle-prometheus` 的 Prometheus 数据源和 `PromLifecycle Heartbeat Overview` Dashboard。Dashboard 的所有 Panel 显式绑定该 UID，包含目标总数、正常/异常目标、可用率、在线状态、CPU、内存和 Blackbox 探测；数据源和 Dashboard 都通过 Grafana provisioning 持久化。

默认值依据官方文档核对： [Prometheus storage](https://prometheus.io/docs/prometheus/latest/storage/)、[Prometheus configuration](https://prometheus.io/docs/prometheus/latest/configuration/configuration/)、[Blackbox multi-target exporter](https://prometheus.io/docs/guides/multi-target-exporter/)、[Alertmanager configuration](https://prometheus.io/docs/alerting/latest/configuration/)、[Grafana configuration](https://grafana.com/docs/grafana/latest/setup-grafana/configure-grafana/) 与 [Grafana provisioning](https://grafana.com/docs/grafana/latest/administration/provisioning/)。

### 远程已有服务托管接管

在“部署管理 → 发现接管”选择 SSH 主机，扫描后选择 `managed`。支持现有 Prometheus、Alertmanager、node_exporter 和 blackbox_exporter 的 Linux 二进制/systemd 服务；接管仅登记管理关系，不重启或改写原服务。

- 原 systemd 单元须为已启用（`enabled`）的服务，避免停机后单元被卸载而使历史恢复失去核验依据。目标主机需要 root SSH、Python 3、systemd、`busctl`、`ss` 和 `curl`。平台验证监听端口、进程启动时间、原启动参数、systemd MainPID 与 loaded ExecStart，读取前后再次核对；服务单元有未加载修改或下次启动参数与当前进程不一致时拒绝接管。平台必须能访问主机实际 IP 上的健康端点。
- 配置、数据和二进制必须使用明确、规范且不含符号链接的绝对路径。Prometheus/Alertmanager 的原启动参数须明确指定配置和数据目录；blackbox_exporter 须明确指定配置文件。默认或相对目录以及其它尚不能完整核实的布局可使用 `observe`。
- 接管快照保存目标主机原配置和完整 `systemctl cat` 内容，包括 drop-in；文件权限为 `0600`，记录 SHA-256。远程配置上限 512 KiB，服务单元上限 64 KiB。快照不包含时序数据。
- 原配置与组件在同一事务中登记为手动配置，定时同步不会覆盖原 Prometheus/Alertmanager 配置。随后可通过配置管理编辑或回滚；只有明确“应用推荐配置”才切回自动生成。原配置引用的规则、凭据等外部文件继续由原服务使用。
- 升级沿用原二进制位置、启动参数和服务名。接管本身不规范化原 unit；明确应用启动参数/推荐配置属于另外一次变更。
- “解除纳管”保留原进程、服务单元、配置和数据；有关联备份时先处理这些备份。接管快照继续保留。

## API 概览

公共前缀 `/api/v1`：

- 工作区与首次部署：`/node`、`/onboarding/status`、`/onboarding/preflight`、`/onboarding/discovery`、`/onboarding/acceptance`、`/stack-templates`
- 主机、组件、接管：`/hosts`、`/hosts/:id/monitoring-status`、`/components`、组件推荐基线、Grafana 密码重置、`/components/:id/adoption`
- 组件联调与 Heartbeat：`/monitoring/integration/status`、`/monitoring/integration/reconcile`、`/monitoring/heartbeat`
- 监控配置：组件实际配置、`/alerts`、`/notifications`、`/scrape-configs`、`/recording-rules`、`/remote-writes`、`/dashboards`、Alertmanager routes/settings
- 生命周期变更：`/change-plans`、execute、verify、rollback
- 运维恢复：`/maintenance`、`/alerts/silences`、`/backups`、verify、restore、manifest/status、`/recovery-exercises`
- 状态与审计：`/stats`、`/operations`、`/operations/logs`、`/operations/logs/export`、公开 `/metrics`
- 持久任务与安全模板：`/deployments/stacks`、`/deployments/stacks/:id/retry`、`/components/:id/integration/retry`、`/deployment-templates`
- 通知证据与人工核实：`/notifications/status`、`/notifications/deliveries/:id/resolve`

历史列表提供分页、搜索及总数；页面地址保存页签、分页与资源详情，刷新可恢复。加载失败保留上次数据并显示最近成功时间，不把失败伪装为空列表。批量主机导入逐项显示结果，仅允许重试明确失败项；结果不明时先核对，禁止自动重放。

原 `/clusters` 与证书生成 API 已移除；数据库内部 singleton scope 仅用于旧外键迁移，不是产品概念。

### WinRM vendor 补丁维护

`vendor/github.com/masterzen/winrm/encryption.go` 与 `http.go` 包含连接检查所需的最小 context 补丁：`Encryption.SetContext` 覆盖加密认证、加密 SOAP 和 NTLM fallback，默认拨号使用 `DialContext`。上游 `RunPSWithContext` 不覆盖创建 Shell 的 HTTP 请求，仅在外层返回超时会遗留真实连接。更新 vendor 时必须保留等效取消能力，并运行 `core/winrm` 中真实挂起 HTTP peer 回归；不改变已有 NTLM/加密算法，也不代表已验收真实 Windows HTTPS 环境。

## 测试矩阵

| 入口 | 范围 | 结果/要求 |
|---|---|---|
| `make unit` | Go 单元、迁移、备份、生命周期变更、部署器与通知渠道 | 无远程依赖 |
| `make vet` | Go 静态检查 | 无远程依赖 |
| `make build` | vendor 离线构建 | 无网络 |
| `make test-local` | 自动启动服务并运行生命周期闭环 | 角色、部署向导、接管、备份 manifest/checksum、损坏拒绝、pre_restore、演练、直接变更、执行/验证/回滚、漂移与审计 CSV |
| `make test-remote` | 远程 SSH 与真实二进制生命周期 | `REMOTE_HOST`/`REMOTE_PASS`；缺失时明确 SKIP |
| `make test-all` | unit、vet、本地闭环、条件远程 | 串行执行 |

其他回归资产包括 regression、CRUD matrix、Full E2E、Round3、adversarial、配置回滚落盘以及需要真实离线包的 binary/Prometheus/Alertmanager/告警链路套件。需要包的脚本会先验证版本，缺失时明确 SKIP，不把环境缺失报告成产品失败。

统一变量：`BASE`、`API`、`DATA_DIR`、`BASE_PATH`、`ADMIN_PASSWORD`/`ADMIN_PASSWORD_FILE`；远程增加 `REMOTE_HOST`、`REMOTE_USER`、`REMOTE_PASS`。`make test-remote` 必须显式设置 `BASE` 为已启动的专用测试平台地址；前面的 localhost smoke 在退出时会停止自己的服务，不能供远程阶段复用。GitHub Actions 使用仓库变量 `REMOTE_PLATFORM_BASE`（以及可选 `REMOTE_USER`）、SSH secrets 和该测试平台的管理员密码。Runner 必须能够访问平台和 SSH 主机，安装 `ssh`、`sshpass`、`timeout`、`curl`、`python3`，并预先核验和配置 SSH `known_hosts`。GitHub Actions 从 secret `REMOTE_SSH_KNOWN_HOSTS` 写入已由运维独立核验的主机公钥，不自动信任现场扫描结果。测试平台需准备脚本要求的离线包。缺少 SSH 凭据时明确 SKIP；已配置 SSH 凭据但缺少平台地址/认证、工具或连接不可用时失败，不记为通过。不要将生产平台或生产主机用于该破坏性生命周期测试。

## 服务进程所有权

Linux 平台服务在恢复、迁移和启动后台任务之前，对实际 SQLite 路径旁的 `.lock` 文件取得非阻塞 OS 排他锁，并持有至进程退出。相同数据库即使使用不同 HTTP 端口、数据目录符号链接或数据库符号链接，也只能运行一个平台服务；冲突会明确报错，不能通过删除锁文件绕过。锁文件保留在磁盘上，正常退出或进程崩溃后内核会释放锁；待恢复的数据库替换不会替换这个锁文件。其他操作系统上的平台服务启动会明确拒绝，避免在无有效锁时执行恢复。

## 数据目录

```text
{base-path}/
├── jwt_secret      # JWT 与凭据加密密钥文件
├── configs/        # 组件配置与规则
├── packages/       # 官方包与离线包
├── bin/            # 解压后的二进制缓存
├── backups/        # 组件备份与平台 SQLite 快照
└── data/           # 默认 SQLite 数据目录
```

## 代码结构

```text
cmd/server/          服务入口、数据库迁移与调度器启动
api/                 REST handlers、路由、中间件和内嵌 Web 控制台
core/                部署、配置、调度、SSH 与 WinRM target 接入
services/            单节点迁移、首次部署、备份恢复、生命周期变更与版本服务
models/              GORM 数据模型
pkg/                 凭据、网络与通用安全工具
storage/sqlite/      SQLite 初始化
vendor/              已提交的离线 Go 依赖
test-scripts/        localhost、功能和显式远程测试
```


### Prometheus 托管版本迁移与历史恢复

- 托管 Prometheus 的版本变更只接受可确认的 2.x/3.x 正式版本。比较会规范化空白、`v` 前缀和 build metadata；预发布、未知、溢出及未来主版本失败关闭，相同规范版本不构成升级。
- 2.55 之前的 2.x 升至 3.x，必须先登记并执行一次 2.55.x 中间升级，再创建新的 3.x 计划。同主版本递增及 2.55.x → 3.x 仍须通过既有依赖、健康、安装包、空间等检查，并登记可生成 full 恢复点的独立配置与数据绝对路径。恢复范围还须与实际生效路径一致：binary 使用最终 argv（含导入参数和锁定的自定义参数规则）；Docker 按平台默认主机卷映射核对，遮盖配置/数据的额外挂载失败关闭。无法证明的部署方式或路径映射返回 `unsupported-scope`，不自动改写参数。此恢复协议支持 server TSDB；agent 模式、systemd 变量展开路径、`..` 路径、binary 未登记为规范宿主路径的符号链接别名及配置/数据树内符号链接、binary 的 PrivateTmp（`/tmp`、`/var/tmp`）目录及歧义卷语法失败关闭；Windows 尚未实现此停机 full 恢复协议。
- 所有直接数值降级返回 `requires_restore`；组件页的版本回滚快捷入口也会给出此指引。请使用原升级变更计划的历史回滚，提供与历史版本、组件身份、路径及启动配置匹配的停机一致性 full 配置/数据备份。历史快照不完整、导入参数缺乏证据、归档损坏或绑定不一致时拒绝恢复。新计划仅额外保存导入启动参数的 SHA256，不新增其明文。
- 普通备份恢复不能切换版本：在获得组件锁后重新确认当前身份，只接受已知且规范版本相等的归档；含数据的恢复必须为 full，同版本 config-only 配置恢复保留。Verify/Rehearse 仍只是归档检查。Docker 历史恢复使用记录的精确 image 引用（包括 v 标签或 digest），不重新拼接标签；恢复后的版本/image 必须保持配对。历史运行时回调失败或历史版本落库失败将保留持久恢复隔离，禁止自动重放另一版本的数据或再次启动未知二进制；版本/镜像须先落库再解除隔离。
- 正向迁移通过最终准入后、写入版本元数据或改变运行时前，必须成功持久化独立的版本变更屏障。备份/恢复内部的 pause/resume 不会清除它；只有完整执行或安全成对回滚、必需元数据与操作/计划记录均确认保存后才解除。平台中断、隔离补写失败、收尾记录丢失或解除屏障落库失败时，后续操作与启动恢复保持拒绝（普通 `restore-recovery` 不能绕过），API 返回待恢复错误；不得仅凭服务健康或普通 pause 已 resumed 手工清除屏障。旧暂停表自动迁移为默认无屏障，保持既有 WasRunning 语义。
- 此政策仅约束平台托管 Prometheus 的 upgrade 及其配套历史恢复。target 观测记录的元数据操作保留既有依赖检查；不认证 remove-kind 灾备全流程（其调用通用恢复时，Prometheus 旧 data-only 归档也会失败关闭）；未扩展自定义端点拓扑、参数自动改写或数据格式转换。full 证明声明配置/数据路径的归档完整性与版本绑定，不认证 TSDB/WAL 或配置语义、PromQL、抓取协议、外部规则/证书/卷、手工改动的 service unit，亦不独立证明数据库记录等于实际运行时版本。

#### Opt-in real Linux acceptance

`TestRealPrometheusMigrationAndRecovery` exercises the production `ChangeService` and
`BackupService` with real Prometheus binaries, SQLite, systemd, TSDB blocks, WAL,
readiness, and PromQL. It is skipped when the opt-in variable is absent; if the
variable is present, every prerequisite fails closed.

Run as root on Linux amd64 with a running systemd, `systemctl`, `journalctl`,
Go 1.26.8, and the two official archives available offline. Supply a new canonical,
root-owned mode-0700 directory outside `/tmp` and `/var/tmp`, plus a unique
lowercase namespace of 8-48 characters. The root path may contain only ASCII
letters, digits, `/`, `.`, `_`, and `-`, so it can be embedded in the owned
systemd unit without quoting ambiguity:

```bash
env PROMETHEUS_REAL_ACCEPTANCE=1 \
  PROMETHEUS_REAL_ACCEPTANCE_ROOT=/srv/promlifecycle-real-acceptance/run-parent \
  PROMETHEUS_REAL_ACCEPTANCE_NAMESPACE=plc-real-example01 \
  PROMETHEUS_255_ARCHIVE=/srv/packages/prometheus-2.55.0.linux-amd64.tar.gz \
  PROMETHEUS_310_ARCHIVE=/srv/packages/prometheus-3.1.0.linux-amd64.tar.gz \
  GOTOOLCHAIN=local GOPROXY=off GOFLAGS=-mod=vendor GOMAXPROCS=2 \
  go test -p=2 ./services -run '^TestRealPrometheusMigrationAndRecovery$' \
  -count=1 -v -timeout=15m
```

The selected matrix is Prometheus 2.55.0 to 3.1.0 and paired historical rollback
to 2.55.0. The test also covers direct-downgrade rejection, corrupt-backup
rejection, fixed-time baseline and post-upgrade queries, and owned-process WAL
recovery. It uses only loopback listeners and scrape targets.

Each run owns one direct child named `run-<12 hex>`, one unit named
`<namespace>-<12 hex>`, and an exact ownership marker. Evidence is retained as
`evidence-<12 hex>.json` and `logs-<12 hex>/` under the supplied parent. The
owned unit is stopped and removed on every normal exit; the child runtime is
removed on success and retained on failure for diagnosis. Checkpoints and logs
use atomic replacement. A pre-removal persistence failure fails the test and
retains the runtime. Before a final rename, a publication failure leaves the
previous durable record at `cleanup_pending`. After a successful rename, a
directory open, sync, or close error means the readable terminal record may
already exist while its directory durability is uncertain; the runtime may
already have been safely removed. Acceptance therefore requires the matching
Go command to exit zero, the named test to report `PASS`, and complete evidence;
JSON status alone does not certify success. An uncertain PID lookup, stop result, final `ActiveState`, final
`MainPID`, or tracked-process exit also retains the unit and runtime for the
external exact-ownership cleanup procedure. Rollback evidence names the retained
upgrade record as `rollback.forward_operation_after_rollback` and separately
asserts the persisted manual rollback evidence and backup association. This test certifies
only the selected binary/systemd matrix and owned-process crash recovery. It
does not certify Docker, other versions, arbitrary PromQL behavior, host power
loss, physical-storage failure, or the product's SSH transfer path.

依据：[Prometheus 3.0 迁移建议](https://prometheus.io/blog/2024/11/14/prometheus-3-0/)、[稳定性边界](https://prometheus.io/docs/prometheus/latest/stability/)、[迁移指南](https://prometheus.io/docs/prometheus/latest/migration/)。直接降级一律要求配套历史恢复是保守产品策略，不是声称上游普遍禁止所有降级（部分兼容支持存在回移）。

## Prometheus 参数版本准入

Linux binary/systemd 流程会在部署预检、创建/重新部署、编辑启动参数、应用推荐配置、升级和配对恢复入口检查已知 CLI 版本规则。检查使用与执行器一致的有效参数；重新部署保留导入的 `ImportedArgs` 时，预检会说明参数来源，结构化默认值不会改写该快照。

| 参数 | 已知版本边界 |
| --- | --- |
| `--storage.tsdb.retention.time`、`--storage.tsdb.retention.size` | 从 2.7.0 引入 |
| `--storage.tsdb.wal-compression`、`--no-storage.tsdb.wal-compression` | 从 2.11.0 引入 |
| `--storage.tsdb.retention` | 3.0.0 移除，替代项为 `--storage.tsdb.retention.time` |

旧、新保留时间参数同时存在时，平台要求显式解决冲突，避免一个值被另一个参数覆盖。未知版本、预发布版本、溢出版本和未来主版本不予放行。部署定义的 `version_rule` 提供最低版本、移除版本、替代项及来源字段，执行校验复用同一规则目录。

当前平台默认参数包含 WAL 压缩，因此新建默认配置在 2.11.0 之前会被明确拒绝；这不表示所有旧版本实例都不能管理。保留的老版本导入参数按其真实参数列表检查。本步不自动重写参数或 systemd 文件；跨版本参数转换及对应服务文件回滚需要单独完成。未列出的参数、配置文件、PromQL、Docker、实际启动和任意版本组合不在这组 CLI 规则的认证范围内。

普通回归：
```bash
go test -count=1 ./api/handlers ./services ./core/deployer -run TestPrometheusParameterVersion
```

真实解析器验收使用经官方 SHA256 校验的 2.6.1、2.7.0、2.11.0、2.55.0、3.1.0 Linux amd64 二进制。清单为版本到 `{"path":"/absolute/path/prometheus","sha256":"<binary SHA256>"}` 的 JSON 映射：
```bash
PROMLIFECYCLE_PARAMETER_CLI_BINARIES=/absolute/path/binaries.json \
go test -count=1 -v ./core/deployer -run '^TestRealPrometheusParameterVersionCLI$'
```
该测试默认跳过；启用后检查二进制摘要，并通过 35 个场景验证未知选项、版本边界及平台默认参数。命令均以 `--version` 退出，不启动监听器或访问 TSDB。未知选项探针确保版本输出没有绕过参数解析。

依据：[Prometheus 2.7.0 变更记录](https://github.com/prometheus/prometheus/blob/main/CHANGELOG.md#270--2019-01-28)、[2.11.0 变更记录](https://github.com/prometheus/prometheus/blob/main/CHANGELOG.md#2110--2019-07-09)、[3.0 迁移指南](https://prometheus.io/docs/prometheus/3.0/migration/)。


### 多主机批量发现与接管

在「部署管理 → 发现接管」选择「多主机批量发现与接管」，最多选择 50 台 Linux 主机扫描，勾选最多 200 个已有实例后提交接管批次。界面按主机、端口、服务单元与进程分别展示；扫描最多三台并发，可仅重扫失败主机。版本来自现有 HTTP 指标或健康接口，扫描不执行被发现的程序。

- 托管模式沿用原服务严格预检及快照要求，保留原服务运行、配置和目录；观察模式只读。远程托管支持 Prometheus、Alertmanager、node_exporter、blackbox_exporter。
- 已纳管实例必须与记录中的类型、二进制和服务身份一致才显示为已纳管；端口被其他实例占用时显示身份冲突。
- 接管批次使用现有服务端持久队列。接管记录与步骤结果在同一事务保存，提交响应丢失可按原提交标识找回。
- 每台主机串行执行，同主机失败后的等待项在显式重试时继续；其他主机继续执行。成功步骤保留，重试不会重复纳管。
- 执行时重新验证发起人权限和进程指纹。原进程重启导致指纹变化时，应重新扫描创建批次；批次不会把新进程自动当作原实例。


### 已有组件批量启动与停止

在「部署管理 → 组件」或「批量部署」打开「批量启动/停止」，选择具体受管实例、操作及原因后提交。每批最多 200 个实例、50 台主机；同类型不同端口分别执行。观察模式、已移除或正在部署/升级的实例不能提交。

- 启动沿用单实例行为：启用并启动服务，验证服务状态与健康接口。停止保留配置、数据及开机启动设置，对应监控或页面会暂时不可用。
- 执行时再次核对发起人权限、所有权及组件/主机身份；与其他生命周期操作共用组件锁。
- 命令执行前持久关联操作记录；成功状态与批次结果原子保存。控制端中断后可显式重试失败或中断项，已成功步骤不重复操作。
- 同一主机失败后暂停后续步骤，其他主机继续。重试时保留成功结果；组件或主机身份变化需重新创建批次。

### 已有组件批量解除纳管与卸载

在「部署管理 → 组件」或「批量部署」打开「批量解除纳管/卸载」，选择具体受管实例、移除方式和原因后提交。每批最多 200 个实例、50 台主机。只支持 `keep_service`（移出平台管理，原服务继续运行）和 `keep_data`（停止并移除服务，保留数据）；需要删除数据时仍须逐实例明确操作。

- 批次、逐实例变更计划与基线一次性持久化，执行时重新核对发起人权限、所有权以及组件和主机身份。观察模式、已移除及正在部署或升级的实例不能入队。
- 复用原有变更执行、验证、回退和持久任务队列。成功后若集群有运行中的 Prometheus 则同步其配置；移除 Loki 时也同步 Grafana 数据源。集成失败可重试，已验证的远端移除不会再次执行。
- 同一主机失败后暂停后续步骤，其他主机继续；可仅重试失败或中断的主机任务，成功步骤保留。变更计划状态、历史尝试及操作记录均可在批次详情查看。
- 创建接口为 `POST /api/v1/deployments/batches`，传入 `kind: "remove"`、`mode`、`name`、`reason`、`request_id` 和 `component_ids`。查询、响应丢失恢复及重试复用其他批次接口。前端回归：`node test-scripts/remove-batches-ui.test.cjs`。
