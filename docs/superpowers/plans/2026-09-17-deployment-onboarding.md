# 部署、批量接入与运行验收 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox syntax.

**Goal:** 完成现有部署和批量主机接入到明确实例采集结果的用户流程。
**Architecture:** 复用持久整栈任务和组件管理；新增只读按范围的验收服务，前端复用已有部署 API 并以任务 ID 跟踪。
**Tech Stack:** Go / Gin / GORM SQLite / 内嵌 Vue 3 / 本地 Playwright。
**Spec:** docs/superpowers/specs/2026-09-17-deployment-onboarding-design.md

## Global Constraints
- 当前仓库 `<repository>`；基线 b96e8c89b61f26aea8e8ac663287b4e3099f1398，继续现有 feature/stack-version-preflight-20260910 分支。
- 保留单平台进程、SQLite、Linux binary/systemd；不增加依赖、安装器、数据库队列表、通用证据框架。
- 不修改此前交付或恢复实现；不覆盖未知/已有实例；新网络调用有超时与取消，凭据不回显。
- 相关业务符号编辑前 GitNexus impact，UNKNOWN 用源码补齐、HIGH 明示；提交前 detect-changes。索引按现有完整范围脚本刷新。
- Go 使用 `<Go-toolchain>/bin/go`，GOFLAGS=-mod=vendor；Linux PATH 优先 `<Node-runtime>/bin`。浏览器 Python `<Python-runtime>/bin/python`，沿既有本机 Chromium 配置。
- 按 Spec 做必要行为测试；不进行无关恢复测试扩展。由主线程最终提交，工作者不自行提交、安装或访问测试机。

### Task 1: 按任务和实例核对运行采集结果
本任务需求以 Spec 为准；先读 Spec，尤其接口、状态和目标身份契约。

**Files:** 新增 services/deployment_acceptance.go 与相关测试、api/handlers/deployment_acceptance.go 与相关测试；修改 api/handlers/monitoring_onboarding.go、api/router/router.go。现有 OnboardingService 可复用，但不得改变全局首次向导通知/备份行为。相邻改动需说明必要性。

- [ ] 对将修改的已有符号做 impact（新文件通过所属类型/集成点分析），阅读现有 models.Component、DeploymentStack/Step、ManagedRoutePrefix 和自动配置的目标生成规则。
- [ ] 先写真实 SQLite + HTTP 服务行为测试，看到预期失败：指定第二个 Prometheus 不得访问第一个；任务绑定不随可选ID改变；主机多Exporter、多Prometheus和跨集群拒绝；期望目标缺失/相似地址/标签伪装/down/陈旧lastScrape/错误HTTP和JSON不能通过。
- [ ] 实现 Spec 的两个 GET 接口和报告。采用 context，超时/响应大小限制、精确目标匹配和保守错误状态；任务非成功不得做完成验收。复用已有地址/参数能力，正确处理保存的路径前缀、IPv6和 URL 非法值。
- [ ] 通过 handler 测试验证 ID 参数、错误码、只读无通知/备份写入和正常报告。报告字段严格遵循 Spec；若状态细节需确定，在报告写明供 Task 2 使用。
- [ ] 跑相关新测试及既有部署、接入测试，适当 race；自审并记录命令、结果、修改清单、已知范围；不要自行提交。

### Task 2: 贯通批量主机接入和部署后的验收页面
依赖 Task 1 的接口；先读 Spec 和已批准 Task 1 报告中的实际字段。

**Files:** api/router/web/index.html，可新增一个聚焦本流程的 assets/deployment-onboarding.js（沿已有页面结构接入）；test-scripts/deployment-onboarding-test.py。必要时更新已存在前端测试，但不得削弱断言来换通过。

- [ ] 对现有页面方法/文件做 impact；HTML 内联符号缺失时按已确认源码补齐。
- [ ] 写真实 Vue/Chromium 行为测试先复现缺失：批量成功/exists 返回的 host ID 被保留并传入监控接入入口；失败/未知行不混入成功列表；选择第二个 Prometheus 后验收发送正确 ID。
- [ ] 批量对话框保留成功/exists 的 ID，提供接续按钮；主机页可选择一批已有主机进入同一监控接入流程。
- [ ] 提供 Linux Node Exporter 批量接入表单，选择 Prometheus、已入库版本和端口，常用配置简洁，高级路径/参数/环境/资源选项折叠。已有 Exporter 默认复用；有多个时明确选择；未纳管实例可跳转既有发现接管，不自动覆盖。
- [ ] 对待部署行使用已有组件预检和 POST /deployments/stacks，只部署缺失 Node Exporter；按主机保存接受的 stack ID，展示明确失败、未知和持久任务状态。重试不重复成功对象；已接受任务只调用其 retry，HTTP 结果未知禁止自动再次创建。失败一行不阻止其余行接受。关闭页不改变已接受任务。
- [ ] 复用接口显示本次 Prometheus/组件和逐目标结果；整栈详情的运行验收调用 Task 1 GET 而非 runAcceptance。纯运行验收不发送通知或创建备份；不以数据库规则数量表示告警已生效。切换任务/实例清空旧结果并忽略旧请求返回。
- [ ] 适配已有 autoOnboardHost 和参数配置页的 monitoringOnboardingResult 显示：使用新报告 scope/checks/targets 和有意义的 message；单机接入也携带明确 Exporter ID，歧义不能静默回退。保留安装期间的阶段消息。
- [ ] 验证凭据清理、会话取消、行选择/失败重试、多实例、前端无异常；运行 batch-host-table-test、stack-version-preflight-test 和新增浏览器测试；自审报告，不自行提交。

### Task 3: 按业务路径验收并提交
由主线程组织；此任务验证前两项实际实现，不再扩展新功能。

- [ ] 复核两项 task review；运行相关服务/handler/router 回归、适当 race、vet、构建及实际浏览器流程；必要远程验收限定隔离或只读资源，记录实测范围。
- [ ] 运行最终图谱变化检查并核对范围，审查整轮 diff；修复真实阻断，不扩展无关工具/细节。
- [ ] 确认无凭据泄漏、本地 Git 提交正确；输出简明交付与验证材料，更新项目看板到实际业务结果。
