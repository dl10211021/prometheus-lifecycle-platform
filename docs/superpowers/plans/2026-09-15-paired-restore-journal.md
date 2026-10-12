# Paired restore journal implementation plan

> **For agentic workers:** Use superpowers:executing-plans for inline implementation, with an independent final code review. Follow every RED/GREEN gate.

**Goal:** Complete Step 10.2.2.2: persist restore intent and prevent lifecycle or startup bypass until safe prepared abort.
**Architecture:** A new models.PrometheusRestoreTransaction row owns a durable host/component fence. Existing preview remains authoritative for initial admission. Small shared journal guards integrate with existing claims and startup without changing their existing pause semantics.
**Tech Stack:** Go 1.26.8, vendored GORM/SQLite, systemd owning-host runners; UBI8 compatibility.
**Spec:** docs/superpowers/specs/2026-09-15-paired-restore-journal.md

## Global constraints
- Host claim precedes component claim.
- No live-file, stop/start/reload, historical evidence, component or plan mutation.
- Only prepared/revision 1 to aborted/revision 2 is implemented.
- No HTTP endpoint or automatic upgrade integration.
- No plaintext argv, environment or SSH credential in persisted/public output.
- Keep Go 1.26.8 vendored/offline builds, Node 24.15.0 checks and UBI8 / GLIBC <= 2.28 compatibility. No dependency additions.

## One independently reviewable deliverable: durable intent, fencing, and prepared abort
Files:
- Create models/prometheus_restore_transaction.go (journal storage).
- Create services/prometheus_restore_journal.go (strict private payload, bounded pending reads and scoped fence).
- Create services/prometheus_restore_transaction.go (prepare, trusted identity/CAS, abort).
- Modify services/component_claim.go and services/host_claim.go (pre/reentrant/post-acquire checks).
- Modify services/backup_service.go (first startup-recovery gate).
- Create services/prometheus_restore_startup.go (read-only existing-database fence).
- Modify cmd/server/main.go (pre-database-restore guard, migration registration, pre-metadata-migration guard).
- Create cmd/server/prometheus_restore_startup.go (locked applying-state supervisor), prometheus_restore_startup_test.go, prometheus_restore_database_guard_test.go, prometheus_restore_crash_guard_test.go.
- Create services/prometheus_restore_journal_test.go, prometheus_restore_journal_boundary_test.go, prometheus_restore_journal_race_test.go, prometheus_restore_transaction_test.go, prometheus_restore_transaction_fault_test.go, prometheus_restore_transaction_crash_test.go, prometheus_restore_transaction_integration_test.go.
- Modify services/prometheus_paired_restore_integration_test.go only to invoke the opt-in journal acceptance helper.
- Create docs/paired-prometheus-restore-journal.md.

Interfaces:
- PreparePrometheusPairedRestoreTransaction(context.Context, uint) (*models.PrometheusRestoreTransaction, error)
- AbortPreparedPrometheusPairedRestoreTransaction(context.Context, uint) (*models.PrometheusRestoreTransaction, error)
- rejectPairedRestoreJournalClaim(context.Context, *gorm.DB, uint /* host */, uint /* component */) error
- rejectPendingPairedRestoreJournals(context.Context, *gorm.DB) error
The journal model exposes IDs, phase, revision and timestamps; ownership tokens, active unique keys and evidence payload are JSON-hidden.

- [x] Refresh baseline graph and run upstream impact for every modified existing symbol. Resolve UNKNOWN by checking actual callers. Warn on HIGH/CRITICAL before editing.
- [x] RED migration: launch the real main with startupTestCommand from a previous-schema database; fail if the new table is missing. Add journal model and main registration only, then verify GREEN. Assert old component data is retained and SQLite integrity_check returns ok.
- [x] RED fencing: create hand-derived pending records; ordinary, restore-recovery, arbitrary-kind and reentrant host/component claims must fail without leaked claims; another host must work. Read-only corruption and unknown-phase cases must fence, not clear. Add strict payload reads and guards, then verify GREEN.
- [x] RED race: interleave journal publication with a component claim using GORM callbacks/channels. Assert that no admitted operation and committed journal coexist; include both acquisition orders. Add post-acquire guard and transaction-owned claim/token checks; verify GREEN without sleeps.
- [x] RED prepare: exercise the actual existing file-backed preview fixture, assert a journal is durably linked while filesystem/PID/business snapshots are identical. Missing phase/point/identity or corrupt historical artifacts must cause no journal. Implement Prepare with fresh preview plus atomic identity and sibling-claim checks, then verify GREEN.
- [x] RED abort: reopen SQLite and create a new service; abort must close only a valid prepared transaction, preserve immutable payload/history, issue no host commands, release both active unique keys, and be idempotent. Inject update failure, zero affected rows, cancellation, stale identity and a later/unknown phase; each must retain fencing. Implement bounded conditional abort and private journal context, then verify GREEN.
- [x] RED startup: stage an older database restore while the current database has a pending intent and prove the original journal and staged files are untouched; also verify metadata migrations cannot rebind a pending component. Then crash/reopen with pending intent, run actual server child and assert nonzero exit before serving or application commands. Valid aborted history permits startup. Add startup guard before generic recovery and verify GREEN.
- [ ] Add real prepare/fence/abort helper, enabled by PROMLIFECYCLE_PAIRED_RESTORE_JOURNAL=1; run owned fixtures on 150 and 151 plus WSL rejection boundary. Verify PID/startup/fixed-time sample unchanged and cleanup complete.
- [ ] Review errors, cancellation, DB failure, migration, race, runtime integrity and supported boundaries independently. Correct confirmed findings with reproducing RED/GREEN tests.
- [ ] Freeze source, refresh complete source/CI graph, run required final gates against that source, verify identities and no skipped required acceptance.
- [ ] Commit only validated changes, build a new immutable delivery with patch/logs/UBI8 server/SHA256SUMS, and verify native Windows hashes and exact file set. Record that actual replacement is still the next boundary.

Representative behavioral assertions (literal expectations, real DB/runner boundaries):

~~~go
if db.Migrator().HasTable("prometheus_restore_transactions") == false {
    t.Fatal("server startup did not migrate durable paired restore journal")
}
if _, release, err := ClaimComponentOperation(ctx, db, ownerID, "restore-recovery"); err == nil {
    release()
    t.Fatal("generic recovery bypassed pending paired transaction")
}
if got.Phase != "aborted" || got.Revision != 2 || got.ActiveHostID != nil || got.ActiveComponentID != nil {
    t.Fatal("prepared abort did not atomically release ownership")
}
~~~

Run focused cycles with:
~~~sh
go test -count=1 -timeout=10m ./services ./cmd/server -run 'Test(PrometheusRestoreJournal|PrometheusRestoreTransaction|StartupPrometheusRestore)'
go test -race -count=1 -timeout=15m ./services ./cmd/server -run 'Test(PrometheusRestoreJournal|PrometheusRestoreTransaction|StartupPrometheusRestore)'
~~~
Use the existing final verification harness for complete gates after focused cases pass. Test-only fault hooks stay in Go tests/GORM callbacks, never in production APIs.

## Review corrections before source freeze
- Reproduce actual exit after prepared COMMIT with transient claims still held. Ordinary abort refuses those claims; exclusive server startup cleans only related abandoned claims, remains fenced, then explicit DB-only abort can resolve the intent.
- Reproduce interrupted whole-database applying state. Validate authoritative original before rollback; test candidate/prepared, candidate/corrupt journal or database, original/prepared, missing/corrupt original and corrupt original journal. Preserve original bytes and unrelated state.
- Test during-write cancellation and actual deferred foreign-key COMMIT failure, 1024/1025 inventory, schema read failures, moved owner, separate unique keys, reentrant pause, token drift and missing rows.

Final gate/commit checkboxes are closed in the immutable delivery receipt for the frozen source tree, avoiding post-verification edits to the tested tree.
