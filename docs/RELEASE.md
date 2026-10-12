# 发布与使用指南

本源码发布版对应已验收的内部提交 `487e7cd4938b73b688bf7db756ab28ac5e1727d8`。发布历史从干净源码快照开始；原开发 Git 历史、现场会话及旧内部审查报告未导入。平台生产代码、原有测试实现、`vendor/`、`deploy.sh` 和 `Makefile` 与该提交保持字节一致。发布版补充了文档与 GitHub Packages 分发工作流、发布脚本及其格式测试，并匿名化了文档中的环境信息；CI 单元测试调整为以 root 运行，以满足既有测试夹具的权限要求。这些交付自动化变化不修改平台运行逻辑。

## 源码与离线安装包

源码仓库含 Go 依赖的 `vendor/` 和内嵌前端资源。构建需要 Go 1.26.9 或更新的兼容安全补丁版本，以及 C 编译器；使用 `make build` 按已提交 vendor 构建。开发验证入口见根目录 README。GitHub 自动生成的源码压缩包不含预编译平台、独立 Python 或五类组件二进制，不能代替离线安装包。

另行发布的 Linux x86_64 离线包包含平台程序、独立 Python 3.13.16（SSL/SQLite）、`promtool`、`amtool`、安装器、五类组件归档及校验清单。目标机无需 Go、Node.js、pip、Docker 或外部数据库；安装器不替换系统 Python。组件均为固定上游源码基础上的本地安全重构建，不能将构建后缀删去或称为未经修改的官方二进制。

| 组件 | 本次随包版本 |
| --- | --- |
| Prometheus | `3.13.4+promlifecycle.1` |
| Grafana | `13.1.7+promlifecycle.1` |
| Alertmanager | `0.34.1+promlifecycle.1` |
| node_exporter | `1.12.1+promlifecycle.1` |
| blackbox_exporter | `0.29.0+promlifecycle.1` |

归档摘要、完整构建来源、依赖更新、许可证及扫描依据，以对应 Release 资产内的 `SHA256SUMS`、`manifest.json`、`packages.json`、`provenance/`、`licenses/` 和 `evidence/` 为准。Grafana 随包的 elasticsearch、zipkin 后端插件也经过本地重构建，仅这两个明确插件 ID 获准加载未签名构建，没有全局关闭签名验证。

## 运行条件与安装

本次实测环境为 **RHEL 8.5 x86_64、glibc 2.28、systemd 239、SELinux Disabled**。运行条件是 Linux amd64 / GOAMD64=v1、glibc ≥ 2.28、运行中的 systemd，以及 Bash、GNU coreutils、grep、tar、gzip 等常用系统命令。预检会检查实际命令、运行库、随包 Python、可执行文件和校验和。

下载对应 Release 的离线包并解压，先核对该资产的发布摘要，再进入解压目录：

```bash
sha256sum -c SHA256SUMS
sudo bash install.sh --check
sudo bash install.sh
```

`--check` 只校验安装包与系统条件，不安装或重启服务。新安装如需仅本机访问，可使用 `sudo bash install.sh --listen-address 127.0.0.1`；默认新装监听所有接口。初始随机管理员口令文件的位置由安装器显示，首次登录后修改密码。不要把口令、JWT 密钥、数据库或 SSH 私钥提交到源码库。

重复运行安装命令会升级平台，并保留现有端口、路径、账号、密钥、服务配置和已有安装包。升级失败按事务恢复原二进制、数据库及服务配置。现有平台升级前按 README 备份数据库、原 JWT/凭据密钥和所需组件恢复点。

在平台「包管理」中，按 `packages.json` 的类型和**完整版本**导入组件归档，再对具体实例进行部署或升级预检。`+promlifecycle.1` 是包身份的一部分，同数值版本的官方包或其他构建不能替代。升级状态要结合实例健康与操作记录确认，单次 API 接受响应不代表任务完成。

## 验证范围

验证分为历史基线与后续修复专项，不能把它们合并描述为当前提交重新执行了全部测试：

- 历史源码基线 `3010e2a`：651 个顶层测试通过，6 个需显式环境的用例跳过；整包超时记录保留，未完成用例分段续跑。双主机 11 阶段核心生命周期验收对应该基线及上一平台二进制，覆盖部署、升级失败恢复、回滚、批量启停、解除纳管/发现再接管、保留数据卸载和 Grafana 数据保护。
- 上传时限修复 `6aad56c`：真实 HTTP 慢速上传红绿测试、相关路由/handler 回归、竞态检测和 vet 通过；实际跨网络 274324165 字节 Grafana 包上传超过 15 秒后成功，归档 SHA 一致。
- 当前构建身份修复 `487e7cd`：9 个新增顶层测试、53 个叶子场景及竞态检测通过；9 个相关旧回归和 vet 通过。精确构建选包、执行、配对恢复身份及历史回滚均有回归证据。
- 当前平台二进制：46 项本地产品闭环检查、10 阶段真实 RHEL 8.5 安装器验证、平台升级和六个已纳管组件升级通过；安装器范围包括预检、新装/首次改密、五包导入、重复升级保留状态、失败事务回退和系统 Python 保持不变。

扫描与来源证据保留已解释的剩余匹配，不作“零安全公告”承诺。这里没有宣称其他发行版、SELinux Enforcing、FIPS、容量或高可用已经认证；CentOS/RHEL 7 的 glibc 2.17、Alpine/musl、ARM64、Windows 平台主机和无 systemd 容器不属于本安装包支持目标。当前验收采用单实例管理网络部署；HTTPS 与来源访问控制需按使用环境另行配置和验证。

本次没有新增项目开源许可证；保留的第三方许可证以各自文件为准。

## GitHub Packages 分发

公开安装包为 [`promlifecycle-v1.0.0-linux-amd64-offline.tar.gz`](https://github.com/dl10211021/prometheus-lifecycle-platform/releases/download/v1.0.0/promlifecycle-v1.0.0-linux-amd64-offline.tar.gz)；最终 SHA-256 以同一 [Release](https://github.com/dl10211021/prometheus-lifecycle-platform/releases/tag/v1.0.0) 提供的同名 `.tar.gz.sha256` 校验文件为准。公开版只清理现场元数据并重新打包，保留 `487e7cd` 源码来源和已验收平台、runtime、组件及许可证；不表示重新编译或新增业务验证。

GitHub Packages 中的 `prometheus-lifecycle-platform-installer` 是 OCI 安装归档，包含离线 `.tar.gz` 及同名 `.tar.gz.sha256`，不是可直接运行的 Docker 镜像。安装 [官方 ORAS](https://oras.land/docs/installation/) 后，在空目录执行：

```bash
oras pull ghcr.io/dl10211021/prometheus-lifecycle-platform-installer:v1.0.0
sha256sum -c promlifecycle-v1.0.0-linux-amd64-offline.tar.gz.sha256
tar -xzf promlifecycle-v1.0.0-linux-amd64-offline.tar.gz
```

进入解压生成的目录，继续执行前文的 `install.sh --check` 和安装步骤。需要锁定内容时，用发布工作流结果中的 `@sha256:...` manifest digest 替换 `:v1.0.0` 后拉取，再校验归档；manifest digest 与安装包文件 SHA-256 是不同值。

维护者在 `main` 分支手动运行 **Publish offline installer to GitHub Packages**，输入已公开 Release 的标签、资产文件名和独立核验的 SHA-256。本次输入分别是 `v1.0.0`、`promlifecycle-v1.0.0-linux-amd64-offline.tar.gz` 和上面的归档摘要。工作流固定官方 ORAS 1.3.4 及发行校验值，只用 job 的 `GITHUB_TOKEN`（`contents: read`、`packages: write`），给 artifact 添加本仓库来源注解，验证实际 OCI 文件格式后推送，再按 manifest digest 拉回核对两份文件。输入通过环境变量传递，不拼接到 shell 脚本。

工作流串行发布，已存在的版本标签会被拒绝；权限、网络或状态不明也停止，不自动覆盖或删除标签。推送后校验失败时需检查已上传制品，不能盲目重跑覆盖。此检查不能代替外部发布者之间的 registry 原子锁，发布权限应由仓库维护者统一管理。

GHCR 新包默认 **Private**，关联公开仓库也不会自动变成 Public。维护者首次发布后需在 [Package settings](https://github.com/users/dl10211021/packages/container/package/prometheus-lifecycle-platform-installer/settings) 调整可见性并验证匿名拉取。此工作流不修改可见性，也不需要额外 PAT。依据：[GitHub Container registry](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry)、[Packages 访问与可见性](https://docs.github.com/en/packages/learn-github-packages/configuring-a-packages-access-control-and-visibility)。
