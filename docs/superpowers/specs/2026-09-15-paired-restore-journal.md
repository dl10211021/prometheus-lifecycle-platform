# Paired restore journal and crash fencing
Step 10.2.2.2, continuing commit 5e19bae7a76aa0ac7938900c235515f1dbe49734.

## Goal and scope
Persist a server-owned restore intent after the existing full admission preview. An unfinished intent must fence every normal operation for its component and host, including reentrant claims and generic startup recovery. A prepared intent can be explicitly aborted because this step never stops a service or mutates live files. This is the first durable boundary of the actual paired restore transaction.

The user has authorized continued stepwise development and commits. Execute this continuation in the existing feature checkout. Preserve previous immutable deliveries.

## Contract
- Internal methods: BackupService.PreparePrometheusPairedRestoreTransaction(ctx, planID) and BackupService.AbortPreparedPrometheusPairedRestoreTransaction(ctx, transactionID), returning a PrometheusRestoreTransaction or an error.
- Prepare repeats PreviewPrometheusRecoveryPointRestore under host-then-component claims, rechecks trusted database identity, and inserts one journal in a SQLite transaction.
- The journal binds plan, recovery point, backup, component, host, current/historical versions, current runtime/host/argv binding, hashes of immutable history and plan payloads, and the preview observation timestamp. Payload and claim token are excluded from public JSON. No plaintext argv, environment, or SSH credentials.
- Phases implemented now: prepared (revision 1) and aborted (revision 2). A prepared row has active host/component unique keys and no finish time. An aborted row clears both keys and sets finish time atomically, retaining all original evidence.
- This step has no host-mutation phase or generic phase-advance API. Future stop/replacement work must durably leave prepared before its first host mutation. Unknown phases are fenced and cannot use prepared abort.
- Preparation does not reuse a caller-provided preview, mutate historical points/plans/components, or authorize file replacement.

## Exclusion and races
- Check pending journals before reentrant claim return and both before and after acquiring a new host/component claim.
- A pending journal fences its host, own component even if later moved, and other registered components on that host. Separate hosts remain independent.
- Immediately before journal insert, verify both own claim tokens still exist and no other component claim on that host exists, in the same SQLite transaction. The post-acquire guard closes the race where another component claim starts while the journal is committed.
- Only private journal-bound context held by prepared abort may cross its own journal fence. The permission must bind row ID, random token, host and component, and cannot cross other journal fences or authorize another component.
- Do not relax existing backup pause or version-transition barriers. Generic restore-recovery and arbitrary operation-kind strings have no journal bypass.
- Bound pending inventory to 1024 records. Excess, unreadable, malformed or unknown pending records fail closed. Absence of the new table preserves legacy standalone test/database compatibility until normal server migration adds it.

## Abort and persistence
- Abort acquires host then component claims, reloads and validates the journal, and validates the still-bound trusted plan/point/component identity.
- Only prepared/revision 1 may change to aborted/revision 2 by a conditional one-row update inside a DB transaction. Already validly aborted calls are idempotent and never touch a host.
- Cancellation, failed insert/update, wrong token, competing claims, stale identity, corrupted payload, unknown phase, or missing durable rows cannot clear an active fence.
- Under the actual server OS process lock, startup transactionally releases only abandoned transient claims for fully validated prepared journal hosts (including their registered sibling components and the original owner component). Unknown/corrupt inventory prevents cleanup. Startup still exits fenced, preserving journal, business rows, unrelated claims and staged input. Ordinary Abort never steals possibly-live claims. Generic interrupted restore handling rejects pending journals before reading or restarting any managed application.
- Check the existing database under the server process lock before MigrateLegacyRestore/ApplyPendingRestore, and check again after schema migration before metadata migrations. A pending journal must not be erased by an older whole-database restore or re-bound by startup migrations. Read failures are not proof of absence.
- For interrupted whole-database restore status applying, require a readable regular .restore-original, ValidateSnapshot, and full journal inventory validation before the existing rollback. Valid original prepared intent is copied back and then fenced; candidate intent or corruption is not authoritative before restore initialization commits. Missing/corrupt originals leave candidate and restore state unchanged.
- No auto-abort on startup. Explicit prepared abort after reopening the database is the only supported resolution in this step.

## Compatibility and validation
Keep Go 1.26.8 vendored/offline builds, Node 24.15.0 checks and UBI8 / GLIBC <= 2.28 compatibility. No dependency additions.
Test fresh and previous-schema SQLite startup migration, unique live ownership with retained terminal history, crash/reopen persistence, single-owner startup, normal/reentrant/competing claims, deterministic acquisition races, exact affected-row failures, cancellation and redaction.
File-system fixtures must prove live files, history, process identity and business metadata stay unchanged. Authorized the two isolated acceptance hosts real Prometheus fixtures must prove prepare/fence/abort leaves PID, startup evidence and a fixed historical sample unchanged. WSL's known non-absolute systemd executable rejection remains a tested unsupported boundary.
Final gates: full tests, race tests on affected packages, vet, build, 46 closure checks, existing page/API and official CLI regression, UBI8 build/runtime, graph changes with complete coverage, independent review, clean committed source and native Windows SHA256 delivery verification.

## Next boundary
Current stopped executable/startup/config/TSDB checkpoint, durable stop/apply phases, actual replacements, matching-runtime compensation and crash recovery remain the next transaction implementation. This step must not be described as executing historical restoration.
