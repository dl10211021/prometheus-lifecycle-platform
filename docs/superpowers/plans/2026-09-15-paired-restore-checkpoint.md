# Stopped current checkpoint implementation plan

> **For agentic workers:** Use superpowers:executing-plans for the accepted inline continuation and an independent requesting-code-review gate. Follow each RED/GREEN cycle.

**Goal:** Complete Step10.2.2.3: durable stopped current checkpoint and fresh read-only verification.
**Architecture:** Extend the private journal evidence and exact phase validation; add focused checkpoint data, orchestration and host files. Reuse established artifact/runtime primitives without historical publication or resume wrappers.
**Tech Stack:** Go1.26.8 vendored/offline, Node24.15.0, SQLite/GORM, local/SSH systemd, UBI8.
**Spec:** docs/superpowers/specs/2026-09-15-paired-restore-checkpoint.md

## Global constraints
- Existing feature checkout; base feaf1bbc3fe58c64ecd3edb782dcb2c65602f74f. Preserve earlier deliveries.
- Host claim precedes component; old pause/version barriers apply on reentry.
- Preserve admission bytes, history/business rows and original files; successful capture intentionally stops original service.
- No new dependency, historical replacement, resume/compensation, ambiguous-stage retry or product route.
- Exact phases/revisions: prepared1, checkpointing2, checkpoint_stopping3, checkpoint_ready4; aborted2 remains the old terminal state.
- Full final gates and immutable source identity required before commit.

## One reviewable deliverable
Files:
- Modify models/prometheus_restore_transaction.go: private nullable checkpoint JSON/digest.
- Modify services/prometheus_restore_journal.go: phase-aware structural validation.
- Modify services/prometheus_restore_transaction.go: exact scoped private access and explicit Abort guards.
- Create services/prometheus_restore_checkpoint.go: strict reservation/evidence validation.
- Create services/prometheus_restore_checkpoint_service.go: API, claims, identity/CAS and orchestration.
- Create services/prometheus_restore_checkpoint_host.go: exclusive root, capture and fresh stopped verification.
- Create services/prometheus_restore_checkpoint_test.go plus fault, verify, ownership, concurrency, crash, probe and integration test files.
- Create cmd/server/prometheus_restore_checkpoint_test.go for actual old-schema/startup behavior.
- Modify services/prometheus_paired_restore_integration_test.go to invoke the opt-in checkpoint helper after existing journal checks; adapt the parent real test to keep this stopped acceptance separate from its original source-loss acceptance. Upstream UNKNOWN was resolved as the native Go test entrypoint by source search before editing.
- Create docs/paired-prometheus-restore-checkpoint.md.

Interfaces:
~~~go
CreatePrometheusPairedRestoreCheckpoint(context.Context, uint) (*models.PrometheusRestoreTransaction, error)
VerifyPrometheusPairedRestoreCheckpoint(context.Context, uint) (*models.PrometheusRestoreTransaction, error)
~~~

- [x] Confirm clean committed source matches prior2515file verification; refresh full source/CI graph.
- [x] Query capture/preview/stop/archive flows and upstream impact before existing symbol edits. Warn HIGH decoder / CRITICAL private access; resolve Abort UNKNOWN via callers. Independent early protocol review.
- [x] RED public contract using interface assertion, existing file-backed transactionFixture and literal current content. Capture must produce checkpoint_ready/rev4, leave stopped, retain admission/history, archive current bytes and fence old Abort. Add schema and minimal implementation, then GREEN.
- [x] RED phase/host ordering: inspect committed journal through actual DB at first mkdir/stop/archive boundaries. Reservation must precede all writes; pinned pre-stop proof precedes stop. Add transitions and verify GREEN.
- [x] RED verifier reopens using new service/connection; reject current-vs-history substitution, corrupt/missing files, changed evidence or restarted unit. Ready repeat must be read-only and stable. Implement verification and GREEN.
- [x] RED database/ownership faults: zero rows, insert/update and real deferred-FK COMMIT failures, cancellation before/during writes, lost or sibling claims, token/history/component drift and reentrant barriers. Failures keep last durable phase, no unintended host command. Verify GREEN.
- [x] RED crash/startup: real child exits at reservation/pre-stop/post-stop/ready boundaries. Verify persisted phase/evidence, forensic directory ownership, claims/fence and no automatic resume. Actual server old schema migration and applying rollback retain valid interrupted phase; corrupt data rejects. Verify GREEN.
- [ ] Add opt-in real host checkpoint helper and validate150/151 plus WSL pre-reservation rejection; validate owned fixture cleanup.
- [x] Independent final code review; reproduce/correct actionable findings with RED/GREEN.
- [ ] Freeze source; final full verification, source+CI refresh and complete change reports, UBI8/real acceptance, source hashes and sensitive-content checks.
- [ ] Commit verified changes, create fresh immutable Windows delivery and native hash/exact-file verification.

Behavioral assertions use actual outputs and effects:
~~~go
if row.Phase != "checkpoint_ready" || row.Revision != 4 || row.FinishedAt != nil {
    t.Fatal("checkpoint was not published with durable active ownership")
}
if f.runner.running { t.Fatal("checkpoint must remain stopped") }
if archived != "current-checkpoint-sample" { t.Fatal("checkpoint substituted historical data") }
if oldPayload != string(row.Payload) { t.Fatal("admission evidence changed") }
if _, err := s.AbortPreparedPrometheusPairedRestoreTransaction(ctx, row.ID); err == nil {
    t.Fatal("prepared abort cleared a transaction that may have stopped the host")
}
~~~
Run each focused test through the output-directory run.py:
~~~sh
go test -count=1 -timeout=30m ./services ./cmd/server -run 'Test(PrometheusRestoreCheckpoint|StartupPrometheusRestoreCheckpoint)'
go test -race -count=1 -timeout=40m ./services ./cmd/server -run 'Test(PrometheusRestoreCheckpoint|StartupPrometheusRestoreCheckpoint)'
~~~
Final gate/commit checkboxes are closed by the immutable verification receipt for the frozen tree, avoiding post-verification source edits.
