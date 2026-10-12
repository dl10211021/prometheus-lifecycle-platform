# Stopped current checkpoint for paired Prometheus restore
Step 10.2.2.3, continuing feaf1bbc3fe58c64ecd3edb782dcb2c65602f74f.

## Goal and scope
Build an internal, durable current-state checkpoint before actual paired historical replacement. The user authorized continued stepwise development and correct commits in the existing feature checkout. Keep this deliverable independently verifiable. Success intentionally leaves the original Prometheus stopped and the journal fenced. Historical file replacement, compensation/resume, recovery of an ambiguous in-progress checkpoint, HTTP routes and automatic upgrade integration are subsequent boundaries.

## Interfaces and state
BackupService.CreatePrometheusPairedRestoreCheckpoint(ctx, transactionID) and VerifyPrometheusPairedRestoreCheckpoint(ctx, transactionID) return a PrometheusRestoreTransaction or a redacted error. Create accepts prepared; a repeat on checkpoint_ready performs only fresh verification. Other in-progress/terminal/unknown phases reject without host mutation. Verify accepts checkpoint_ready only.

Preserve admission Payload and PayloadSHA256 bytes, owner token, IDs and CreatedAt throughout. Add nullable JSON-hidden Checkpoint and CheckpointSHA256 columns. Existing prepared/revision1 and aborted/revision2 rows require empty checkpoint fields and remain compatible, including reads of pre-column databases before startup AutoMigrate.

Transitions, each one-row CAS inside a SQLite transaction:
1. prepared/revision1 -> checkpointing/revision2: persist a bounded strict reservation containing schema1, transaction ID, admission digest, start timestamp and exclusive directory under the configured backup root. Directory name binds transaction ID and private token. This COMMIT precedes the FIRST host mutation, including artifact mkdir.
2. checkpointing/revision2 -> checkpoint_stopping/revision3: after current startup and executable capture, pin their complete references, current binding and pre-stop process/root evidence. Archive and stopped timestamp are absent. This COMMIT precedes the stop command.
3. checkpoint_stopping/revision3 -> checkpoint_ready/revision4: after verified shutdown and durable complete config/TSDB archive, pin its digest/size/source paths/current version and stopped timestamp.

Every phase retains both active unique keys and FinishedAt=nil. Each phase has an exact evidence shape. Malformed or unknown data remains fenced. Partial artifacts never become trusted by discovery or recomputing a new reference. Every artifact, including orphaned partial capture output, stays beneath the already journaled transaction directory.

## Admission, ownership and execution
Acquire owning host before component. Reject inverted reentrant ownership. Private access is bound to exact journal ID, token, host, component, phase, revision and admission/checkpoint digests; prepared-abort access cannot authorize checkpoint phases. Explicit Abort checks prepared both initially and after transactional reload.

Before reservation, repeat full existing paired restore preview and trusted journal identity, including old pause/version barriers even on reentrant claims. Recheck both claim tokens, same-host sibling exclusion and trusted DB identity in every phase CAS. Re-read original journal evidence before each transition; cancellation, zero affected rows and COMMIT errors never authorize the next host action.

Use one bounded local/SSH owning-host runner for capture. Reuse existing literal startup/namespace/runtime probes, bounded startup/executable copying, stopped process and root checks, archive creation and integrity validation. Do not invoke historical capture's pause/resume/business publication wrapper. Existing history, plans, backup rows and component metadata remain unchanged.

Before stop, repeat the live baseline and owning-host reference inventory after artifact copying; reject observed process/startup/binary/reference drift. After stop, require inactive/dead, MainPID=0, ControlPID=0 and the original PID/start identity gone. Verify root identities/mount constraints, capture full configuration and TSDB, synchronize files/directories, and repeat stopped baseline and artifact checks before ready CAS. No stop/start/reload/enablement or original file replacement beyond this single intended stop.

## Verification and failures
Fresh ready verification reopens the pinned current startup/binary/archive on the owning host, verifies trusted DB binding and held claims before/after reads, and confirms the original unit is still stopped. It issues no artifact writes, repair, cleanup, stop or resume. Running/restarted services, missing/corrupt artifacts, changed evidence, identities or barriers reject. Retained history must still bind the original journal.

Bound one operation to ten minutes. Preserve cancellation/deadline and remote-termination-unconfirmed identities without exposing command output, SQL details, argv, environment contents or credentials. After any durable new phase, failures leave that phase/evidence and fencing intact; do not auto-resume, remove evidence, reset to prepared, or retry ambiguous capture.

Startup remains fenced. Prepared-only abandoned-claim cleanup must NOT clear checkpoint phase claims, including mixed inventories. Structural decoding permits valid reservation/pre-stop/ready phases during authoritative whole-database rollback without host/artifact access; invalid evidence still blocks rollback. Actual runtime recovery is a later protocol.

## Evidence bounds and compatibility
Checkpoint JSON <=256 KiB. Existing startup limits:64files,1MiB/file,8MiB total,256KiB manifest; binary<=512MiB. Reserve root and child directories root-owned0700, payloads0600. Only canonical supported paths under reservation root are accepted; no overlap with original binary/config/data/startup or history. Follow existing supported namespace/reference/path constraints; no ACL/xattr restoration or protection against hostile external privileged writers is claimed.

Go1.26.8 vendored/offline, Node24.15.0 checks, UBI8 and GLIBC<=2.28; no new dependency.

## Acceptance
Test actual SQLite commits and real filesystem archive bytes, distinct current-vs-historical content, exact ordering at host boundaries, idempotent read-only reopen, old schema migration, invalid phases/shapes, private access scope, reentrant pause/transition, identity/claim drift, concurrent attempts, write/zero-row/COMMIT/cancellation faults, real controller exit at durable boundaries, startup/wholeDB fences, source/artifact/stop failures and redaction.

Authorized150/151 exclusive Prometheus2.55.0 fixtures prove real stop, pinned current artifacts, reopened verification and retained historical samples; fixture cleanup independently verifies files/processes removed. WSL's unsupported reference boundary must reject before reservation. Final full unit/race/vet/build,46closure,page/API/officialCLI,UBI8,index/detect/impact with disclosed limitations, independent review, immutable committed-source delivery and native Windows SHA/exact-file checks are required.
