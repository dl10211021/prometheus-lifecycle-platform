# 组件栈版本预检 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [x]`) syntax for tracking.

**Goal:** 整栈预览与入队时校验计划内版本组合，避免把旧的已运行依赖误用作未来部署版本证据。

**Architecture:** 保留现有 planned_components 顺序，添加 planned_versions（类型→版本）作为预览证据，并纳入 canonical request hash。整栈服务端从实际请求重新规范化并构建计划；执行时清除未来承诺，继续检查实际已部署组件。

**Tech Stack:** Go/Gin/GORM/SQLite、内嵌 Vue、Python Playwright。

**Spec:** docs/plans/2026-09-10-lifecycle-assessment.md；用户已要求按前述第二步之后的版本组合检查继续。

## Global Constraints

- 修改前刷新 GitNexus 并运行 impact；UNKNOWN 用源码补证；不缩减 vendor 索引。
- 不增加依赖，不更改正式版本规则；Thanos/Prometheus 规则复用第二步。
- 正式新部署为 Linux binary/systemd；本步不扩展 Windows MSI 或容器部署支持。
- 计划类型在现有单类型栈模型中必须唯一；不借此实现任意多实例拓扑。
- 凭据不写入明文计划/记录；队列继续加密。
- 150/151 只运行隔离测试，不能把本步说成生产升级。

## Task 1: 预览与执行证据绑定

**Files:** component_deployment_request.go、deployment_preflight.go、deployment_preflight_credential.go；新增 deployment_stack_compatibility_test.go，补既有 dependency 测试夹具。

**Interfaces:** componentCreateRequest.PlannedVersions map[string]string；canonicalDeploymentRequest 同字段。现有 preflightDependencies 无签名变化。

- [x] 红测：计划中 Prom3+旧 Thanos 即使机器已有 Prom2 也失败；已计划依赖缺版本失败；正确组合通过并给出 planned 来源；反向顺序/重复类型失败。
- [x] 红测：预检后更改 planned_versions 导致 token 不匹配；版本映射不能单独冒充已运行依赖。
- [x] 实现：计划优先表达未来版本；同栈依赖必须位于前序；复用版本兼容性判定；无计划依赖仍查询真实组件。
- [x] 执行复核：请求 hash 校验后同时清除 planned_components 和 planned_versions，不授予未来组件实际存在的资格。
- [x] 绿测与全量回归；不降低不兼容/未知保护。

## Task 2: 整栈入队与前端传递

**Files:** deployment_stacks.go、api/router/web/index.html；新增浏览器测试与整栈 handler 测试。

- [x] 红测：绕过浏览器直接 POST 不兼容栈，返回明确阻断，数据库没有 Stack/Step。
- [x] 红测：服务端采用 normalizeDeploymentRequest 后的有效版本（包括 parameters.version），不能相信客户端附带计划映射；正确计划入队且清除预览字段。
- [x] 实现：入队前先规范化完整请求、构造服务端计划并验证依赖，然后按已有加密方式持久化。
- [x] 前端：payload 附带当前选中版本映射，提交 stack 时移除预览字段；设置变化继续清空旧预检。
- [x] 浏览器验证：真实 Vue 生成版本映射，改变版本重新生成，提交栈不保留预览凭证。

## Task 3: 验证与交接

- [x] make unit、make vet、浏览器回归、git diff --check。
- [x] UBI8/glibc2.28 构建 handlers 测试，在150/151临时目录运行计划/凭证测试，校验服务 PID/状态不变并清理。
- [x] requesting-code-review 独立复核，仅比较本步快照；修复实际缺陷。
- [x] GitNexus detect_changes；记录图谱实际新鲜度，不能把历史大范围变更说成本步风险。
- [x] 保存限定补丁、验证与 SESSION_NOTE。历史工作区前置代码尚未提交，本步不拼凑无法独立构建的提交，不推送或升级生产。

## Execution record

- 任务 1/2 共用请求结构、哈希和依赖检查，按紧耦合任务顺序手工执行；独立 reviewer 复核整步结果。
- 分支：feature/stack-version-preflight-20260910。正常工作区保留所有历史未提交内容；未新建缺少历史未跟踪代码的 worktree，未重置或提交历史内容。
- 三组失败验证（预览/凭证、整栈入队、真实 Vue）均先复现原问题，随后转绿。
- 独立复核提出执行阶段证据测试建议，已补 missing/incompatible 两场景；复核无未处理项。
- 最终测试、远程校验、差异和图谱证据见 `<external-evidence-directory>/verification.json`。
- 修改前主图谱刷新覆盖 2301 文件，6 个待改文件 SHA256 全匹配；修改后图谱尚未刷新，新增测试和计划尚未入图。全工作区 detect_changes 含历史改动，不能替代限定快照审查。
