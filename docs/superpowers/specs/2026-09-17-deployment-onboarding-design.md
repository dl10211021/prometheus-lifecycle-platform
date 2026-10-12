# 部署、批量接入与运行验收

目标：沿既有平台完成“添加/选择主机 → 部署或复用监控组件 → 查看本次实例的真实采集结果”。承接总体计划第五步及用户对继续主线的授权。保留 Go 单进程、SQLite、内嵌 Vue 和 Linux binary/systemd；使用现有持久化整栈任务、预检、组件归属和显式失败重试。

## 本轮交付
1. 整栈任务可查看与该任务组件绑定的运行/采集验收；不能调用全局首次向导验收冒充任务验收。
2. 主机监控状态明确绑定 Exporter 和 Prometheus。可自动选取唯一匹配实例；多实例歧义必须让用户选择，不能 First/最新记录或子串匹配。
3. 批量添加保留成功/已存在结果的主机 ID，可直接进入 Linux 主机监控接入。已有主机列表也提供此入口。
4. 批量接入为所选主机复用现有 Node Exporter 或提交缺失 Exporter 的现有整栈任务。统一选版本、端口，保留高级路径、参数、环境和资源设置。已有实例不自动重新部署，观察/纳管仍使用已有发现入口。安装、任务失败、运行验收分别显示；只重试明确失败项；已接受任务按 ID 跟踪，网络结果未知不得自动重放。
5. 对新建监控栈仍复用已有部署向导；监控接入要求存在可选 Prometheus，没有时引导先部署栈。后端自动配置仍遵守既有单管理节点/集群范围；本轮不会增加多集群管理系统，也不会为验收偷偷发布配置。

## 运行验收接口
- GET /api/v1/deployments/stacks/:id/acceptance?prometheus_id=<可选ID>。只读，使用已有 protected 读权限。要求任务成功、步骤成功且所有组件 ID/主机/类型/归属仍匹配。任务内有 Prometheus 时唯一绑定它；与用户指定冲突时拒绝。没有时按组件集群和显式 ID 或唯一实例选择。
- GET /api/v1/hosts/:id/monitoring-status?prometheus_id=<可选ID>&component_id=<可选ExporterID>。保留此入口，修复错误选择和虚假通过。一个主机多个活跃 Exporter 必须指定，Windows 使用 Windows Exporter，但本轮批量安装仅 Linux。
- 正常得到完整报告返回 200，即使 passed=false；输入错误 400、不存在 404、范围歧义/状态冲突 409、数据库不可用 500。不得将数据库/HTTP/JSON 错误当作空集合或成功。
- 报告至少含 checked_at, passed, scope（kind/stack_id/host_id/prometheus_id）, checks（key/status/reason）, targets（component_id/host_id/type/address/status/reason/last_scrape）；前端只依赖这些字段。status 采用 pass/fail/unknown；目标项 status 可用 up/down/missing/unknown，但前后端须一致并记录。
- 目标从服务器保存的组件和主机推导；校验集群匹配。HTTP 对所选 Prometheus 发起真实 ready/targets 探测，支持已保存的路由前缀和 IPv6。不要把 ExternalURL 任意代理到别的实例，也不要跟随重定向将另一个实例算通过。
- 按精确 scrape URL 主机、端口和 /metrics 路径匹配组件目标；不能用 instance 标签或地址子串替代端点身份。至少一个无关 up、自采集 up、缺失、down、无效/失败 HTTP 和过期或零 lastScrape 均不能让期望目标通过。处理重复匹配时不得任选一个 up 掩盖冲突。
- 成套任务需检查其全部组件运行状态；有明确可采集组件时逐一核对对应目标。不把只有 Grafana 等不适用目标的任务伪装成采集通过；报告说明适用范围。不得将数据库规则数量当作 live 规则，也不声称此只读验收证明通知或备份成功。
- 不返回凭据、任意 HTTP 响应正文或带凭据的 URL；支持请求取消和超时，限制响应大小。

## 验证与交付
用真实 SQLite、httptest Prometheus HTTP 边界和真实 Vue/Chromium 页面验证：精确实例选择、多实例歧义、跨集群拒绝、任务绑定、缺目标/相似地址/伪造标签/down/失效响应、取消、正常成功；批量成功加失败只重试失败、未知不重放、已有组件复用、已接受任务持久跟踪、页面展示明确验收范围，保留权限与凭据清理。
执行相关 Go/竞态测试、既有部署/接入回归、前端合同检查、vet 和构建。必要时复用 150/151 做本轮范围的隔离或只读验收；不得影响已有业务。本轮不重跑无关的成套恢复小时级测试，也不新建验收框架。
交付本地提交、简明验收记录和更新项目看板；后续继续统一维护/恢复/退役。
