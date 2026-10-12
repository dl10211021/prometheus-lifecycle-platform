# 多主机批量发现与接管 Implementation Plan

> **For agentic workers:** Use superpowers:executing-plans inline; finish with a fresh reviewer.

**Goal:** 多选 Linux 主机扫描已有实例，批量纳管并保留逐实例结果，显式重试失败任务。

**Architecture:** 扫描复用 hosts/:id/detect，浏览器最多三个并发请求。接管使用现有 DeploymentBatch/Stack 队列和 AdoptComponent 严格校验路径；接管记录与队列结果在同一事务写入，避免提交成功后重试重复接管。沿用批次提交标识恢复机制。

**Tech Stack:** Go / Gin / GORM / SQLite / Vue / Element Plus.
**Spec:** 用户已确认的本任务方案：统一扫描、选择实例接管、逐实例结果与失败重试；保留原进程、配置、目录。不扩展批量停止或卸载。

## Global Constraints
- 最多 50 台主机、200 个实例。按主机、端口、类型和进程指纹识别，不按组件类型合并。
- 已纳管实例只在核实身份后跳过，不改变所有权。
- 队列执行时再次验证发起人权限及进程指纹。错误不泄漏 SSH 输出。
- 使用已有持久队列；同主机失败后后续步骤等待显式重试，其他主机继续。
- 仅使用自建验收服务；150/151 现有业务进程保持不变。

## Review Focus
- 中断发生在纳管事务提交后、步骤成功标记前：读取持久关联，不能二次插入。
- 同一端口不同类型或二进制被替换：拒绝，不能当作已纳管跳过。
- 多个同类型实例、部分扫描失败：每个实例独立，失败主机可单独重扫。
- 创建失败和响应丢失：批次原子写入、同标识恢复，不重复创建。
- 切换主机或退出登录：迟到的扫描响应不可污染新列表。

### Task 1: Durable adoption queue
Files: api/handlers/adoption_batches.go and adoption_batches_test.go; adopt.go; deployment_batches.go; deployment_stacks.go; models/lifecycle_queue.go.
- [x] RED: TestAdoptionBatchPersistsInstancesAndReplays sends kind=adopt and three instances on two hosts; require HTTP 202, 2 stacks, 3 steps, same request replay 200.
- [x] Implement request validation, actor-bound encrypted payload, host grouping, dispatcher.
- [x] RED/GREEN: real SSH fixture proves fingerprint rejection, existing identity skip, transaction rollback, committed outcome recovery, disabled actor rejection.
- [x] Existing single-host adoption and deployment/upgrade batch tests pass.

### Task 2: Discovery UI and batch submission
Files: api/router/web/assets/adoption-batches.js; deployment-batches.js; index.html; test-scripts/adoption-batches-ui.test.cjs.
- [x] RED: VM test exercises three concurrent host scans, failure-only retry, same-type different port selection, stale response cancellation.
- [x] Implement host multiselect, scan progress table, instance table selection and managed/observe review, common durable batch submission.
- [x] RED/GREEN: lost POST response retains original request identity; already owned rows cannot be submitted.
- [x] Browser verification of selection, submit, per-instance results and existing batch upgrade UI.

### Task 3: Acceptance and delivery
- [x] Review changes against e7056ab; resolve correctness findings with failing regression tests.
- [x] Go project tests, UI tests, vet, portable build and product closure.
- [x] Isolated services on 150/151: multi-instance discovery/adopt, repeated skip, failure retry; compare service PID/config before and after.
- [x] Remove only owned fixtures; deploy verified portable build to authorized test portal and verify existing inventory/targets.
- [x] Full GitNexus index and detect_changes(scope=all), README acceptance notes, commit verified changes; no push.

## Acceptance evidence (2026-09-22)

- Services: 318 passed, 4 existing skips; the two development-host timeout cases passed in the isolated UBI8 container. Other Go packages, vet and all three batch UI suites passed. Product closure: 46/46.
- Two hosts / four owned node_exporter fixtures: discovery, adoption, controller restart recovery, failed-only retry and already-owned skip passed. Original PIDs, service units, binaries and business configurations were preserved; owned fixtures were removed.
- Portal updated and verified: 6 existing components, 11/11 targets healthy. All six legacy records are correctly identified without rewriting their identity fields.
- Fresh review findings resolved: version discovery reads protocol metadata without executing discovered binaries; ownership checks use actual type/binary/service identity, including legacy lifecycle path resolution.
- Full source/CI indexing completed. detect_changes(scope=all) returned 17 files and 29 affected flows with critical aggregate risk; shared queue/adoption/discovery paths have regression coverage. The process graph has global trace limits and does not prove exhaustive call coverage; inline frontend paths and legacy identity resolution were additionally checked in source and tests.
- Detailed evidence is outside the repository: `<external-batch-adoption-evidence-directory>`. Test portal binary SHA-256: ac12191c2a76bbe7773407fbe49bc9e32fd3f441280fc1f631dc798572e5a8d4.
