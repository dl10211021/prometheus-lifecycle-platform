# Maintenance and paired recovery implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task.

**Goal:** Complete the existing paired restore through live replacement, healthy runtime or verified compensation, and expose it through maintenance.

**Architecture:** Extend the current durable journal and reuse the host fence, snapshot/artifact validation and change-plan model. Keep private evidence hidden, publish terminal outcomes atomically, use the existing exclusive recovery CLI after interruption.

**Tech Stack:** Vendored Go/GORM/SQLite, Linux binary/systemd/SSH, embedded Vue/Element Plus, Python Playwright.

**Spec:** docs/superpowers/specs/2026-09-17-maintenance-recovery-design.md

## Global Constraints

- Continue branch feature/stack-version-preflight-20260910 from bbc915688aee; no new dependencies or destructive business-host tests.
- Existing immutable admission/checkpoint/stage evidence remains compatible; no generic installer/queue or evidence framework.
- HIGH/CRITICAL graph impact must be announced before existing-symbol edits; UNKNOWN requires source fallback. Final full indexing and detect_changes required.
- Keep raw test/review evidence under this plan workspace; controller performs final commit after integrated checks.

### Task 1: Execute and compensate the matched restore

**Files:** new focused services/prometheus_restore_execution*.go files/tests; models/prometheus_restore_transaction.go; necessary narrow journal/checkpoint/stage validation and claim-access integration; cmd/server/prometheus_restore_execution.go plus existing CLI dispatch and tests. Do not edit web/handler files or unrelated shared restore behavior.

**Consumes:** StagePrometheusPairedRestore, VerifyPrometheusPairedRestoreStage, checkpointFixture; restoreStageFence and runRestoreStageFenced; verified unitSnapshotManifest and prometheusRecoveryPayload; DatabaseProcessLock.

**Produces:**
```go
func (s *BackupService) GetPrometheusPairedRestoreTransaction(ctx context.Context, planID uint) (*models.PrometheusRestoreTransaction, error)
func (s *BackupService) ExecutePrometheusPairedRestore(ctx context.Context, transactionID uint) (*models.PrometheusRestoreTransaction, error)
func (s *BackupService) RestorePrometheusRecoveryPoint(ctx context.Context, planID uint) (*models.PrometheusRestoreTransaction, error)
func (s *BackupService) RecoverPrometheusPairedRestoreExecutionForStartup(ctx context.Context, transactionID uint, ownership *platformsqlite.DatabaseProcessLock) (*models.PrometheusRestoreTransaction, error)
```
The plan-level method orchestrates prepared/checkpoint/staged/execute for a new point and returns its durable row on error. Existing pending rows are observed/rejected, never automatically replayed. Terminal phases are `restore_completed` and `restore_compensated`; pending execution is visibly nonterminal. Additional private intermediate phase names must be documented in the task report for UI mapping.

- [ ] Write failing interface/runtime tests using the existing fixture; verify actual distinct current/historical files, then an injected failed historical start returning current bytes. The red assertion must fail because behavior is missing, not because a test hook is absent.
```go
type pairedExecutionAPI interface {
 ExecutePrometheusPairedRestore(context.Context, uint) (*models.PrometheusRestoreTransaction, error)
 RestorePrometheusRecoveryPoint(context.Context, uint) (*models.PrometheusRestoreTransaction, error)
}
```
- [ ] Implement bounded private execution evidence and validation, exact CAS/claim ownership, the fenced host replacement and verification, current-state compensation, and terminal database updates. Keep host mutation after successful COMMIT and never permit terminal rows with active keys or invalid evidence.
- [ ] Implement exclusive CLI compensation recovery with the same database-process lock and exact host generation fencing; no scheduler or migration side effects. Preserve existing staging CLI.
- [ ] Test duplicate/foreign-claim, corruption, commit failure, health failure, compensation failure and interrupted execution recovery. Run focused regressions and race appropriate to this task; report commands/logs/phase semantics.
- [ ] Submit the complete diff to independent task review; controller resolves findings before UI integration.

### Task 2: Make paired recovery points reachable from upgrades

**Files:** services/change_service.go and a focused paired-upgrade helper/test; models/change_plan.go; services/prometheus_recovery_point_capture.go and services/prometheus_recovery_point_verify.go with narrow backward-compatible association changes. Keep Task 1 execution implementation untouched unless an interface correction is reviewed.
**Consumes:** existing CreatePrometheusRecoveryPoint(ctx, planID), ordinary CreateComponentBackup, immutable paired point history verification, existing executeClaimed ordering.
**Produces:** trusted nullable `ChangePlan.PairedRecoveryPointID` (`paired_recovery_point_id`) and upgradeChange `PairedRecovery bool` (`paired_recovery`) accepted only for supported binary Prometheus upgrades. Existing plans without the association continue exact legacy BackupID checks.

- [ ] Write a failing upgrade-flow test proving a requested paired point is created before runtime apply and preserved after the ordinary backup association, and that capture failure prevents apply. Validate untrusted client plan fields cannot assign the association.
- [ ] Capture when the plan is executing, before creating the ordinary automatic-recovery backup and before version-transition barriers. Atomically save the paired point association with its existing capture publication. Reload the plan after capture, then preserve the generic backup path. Never recapture one point or relax legacy missing/corrupt association rejection.
- [ ] Tests cover option absent, unsupported component, paired capture rejected, point+ordinary backup both bound, history verification with both links, and existing automatic rollback retaining the ordinary backup. Run focused compatibility/recovery regressions and independent review.

### Task 3: Connect maintenance and change-plan recovery UI/API

**Files:** new api/handlers/paired_recovery.go + tests; api/router/router.go; new api/router/web/assets/maintenance-recovery.js; embedded api/router/web/index.html; new test-scripts/maintenance-recovery-test.py. Narrow ChangeService integration if required to prevent paired generic rollback fallthrough; coordinate with Task 1 producer first.

**Consumes:** approved Task 1 service methods and Task 2 paired upgrade option/association, existing PreviewPrometheusRecoveryPointRestore and public transaction JSON (id/plan_id/point_id/component_id/host_id/phase/revision/created_at/updated_at/finished_at; private fields hidden).
**Produces:**
```
GET  /api/v1/change-plans/:id/paired-recovery
POST /api/v1/change-plans/:id/paired-recovery
```
GET reports `{eligible, preview, transaction}` (preview optional on persisted pending/terminal history) without host writes, restore intent creation or journal phase changes. The existing preview may acquire/release its short-lived serialization claims; persisted transaction lookup alone is strictly database-read-only; POST returns `{transaction}` on success and `{error, transaction}` on controlled failure. Eligibility is grounded in actual bound point/type/deploy mode/status, not name or arbitrary client payload. GetPrometheusPairedRestoreTransaction(ctx, planID) returns nil,nil when absent and validates any stored row before returning; Task 2 consumes this exact helper.

- [ ] Add API tests for role-gated route, invalid IDs/absent/unsupported history, safe public fields, terminal and failure result responses.
- [ ] Add the paired_recovery upgrade checkbox with extra downtime/disk explanation and a focused Vue mixin and maintenance/change drawer entry, selected-instance preview, explicit restore confirmation, clear completed/compensated/pending outcomes and refresh after disconnect. Reuse existing config/upgrade/backup/retirement controls; preserve data by default.
- [ ] Browser contract examples:
```python
# Actual Chromium clicks: eligible plan -> preview -> explicit confirm -> POST once.
# On restore_completed show recovered; on restore_compensated show previous state
# recovered and requested historical restore failed. Network uncertainty -> GET
# refresh only; close/switch/logout rejects stale results. Viewer sees no execute.
```
- [ ] Run new browser test plus existing change/deployment/onboarding regressions touched by page integration, and handler tests. Submit full Task 3 diff to independent review.

### Task 4: Integrated business acceptance and delivery

**Files:** concise operator documentation for paired recovery execution; update roadmap outside source; final test evidence and small delivery bundle.
**Consumes:** reviewed Tasks 1/2/3 and their exact interfaces/status semantics.

- [ ] Build the real server and run focused Go regressions/race/vet, browser checks and local product closure, once per unchanged candidate.
- [ ] Run actual isolated systemd/Prometheus fixture if available on 150/151; prove historical fixed sample returned and injected restore failure compensates current sample. Preserve service/data outside named fixture paths; document any unsupported interruption boundary rather than assert coverage.
- [ ] Full source+CI/group index refresh and structured detect_changes; match reviewed candidate hashes; final whole-change review and necessary fix wave.
- [ ] Commit correct changes locally, record SHA/tree, update roadmap with actual business completion and limitations, archive developer records and deliver byte-verified concise evidence.
