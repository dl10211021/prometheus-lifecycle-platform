# Paired Prometheus restore: stopped current checkpoint

Step10.2.2.3 extends the prepared restore journal with a stopped checkpoint of the current owning-host state. The checkpoint is a prerequisite for a later historical replacement protocol. It does not replace historical/live files or automatically start a service.

## Internal service contract

~~~go
CreatePrometheusPairedRestoreCheckpoint(ctx context.Context, transactionID uint) (*models.PrometheusRestoreTransaction, error)
VerifyPrometheusPairedRestoreCheckpoint(ctx context.Context, transactionID uint) (*models.PrometheusRestoreTransaction, error)
~~~

Capture accepts a structurally valid prepared transaction after fresh current admission. Success returns checkpoint_ready/revision4 and intentionally leaves the original Prometheus inactive/dead, its original PID/start identity gone, and both durable ownership keys active. A ready capture request performs fresh verification only. Verification accepts ready only.

These methods have no HTTP route or automatic upgrade integration. Do not expose them as a complete restore operation: historical replacement, compensation, interrupted-stage recovery and authorized original-service resumption require subsequent steps.

## Durable ordering

| Committed phase | Revision | Private checkpoint evidence | Next permitted operation |
| --- | --- | --- | --- |
| prepared | 1 | Empty | Fresh admission; reserve current checkpoint |
| checkpointing | 2 | Transaction-bound exclusive directory and start time | Create current startup and executable copies |
| checkpoint_stopping | 3 | Complete startup/executable/pre-stop process evidence | Stop original service, verify stopped state, archive current config/TSDB |
| checkpoint_ready | 4 | All three verified artifacts and stopped timestamp | Fresh read-only verification |
| aborted | 2 | Empty; original prepared-only terminal state | No checkpoint operation |

The dedicated root is derived from the transaction ID and token and committed before the first host write. It is created exclusively beneath the configured backup root. Current startup and binary/archive directories are nested inside that reserved root, so a crash before child paths are pinned still leaves forensic files attributable to one journal.

Every transition uses a single-row SQLite compare-and-swap under a transaction and checks exact row identity, phase/revision, admission/checkpoint digests, trusted component/host/plan/history, prior pause/version barriers, own host and component claim tokens, and absence of conflicting sibling component claims. The next host stage cannot run until COMMIT succeeds. A deferred foreign-key failure at COMMIT is a tested failure boundary, not a successful publication.

## Current state and evidence

The immutable admission Payload/PayloadSHA256, token, associations and CreatedAt remain unchanged. Nullable JSON-hidden Checkpoint/CheckpointSHA256 columns contain private phase-bound evidence. They never contain source file bodies, environment values or raw argv.

Capture repeats the full current restore preview, then observes current startup files, loaded unit state, executable bytes/version, process PID/start identity, argv digest and application root identities. The captured startup manifest must match that initial observation. Direct systemd and registered component references are rechecked after copies and around stop/archive boundaries.

Before stopping, the complete current startup and executable evidence is durable. After stopping, the service must be inactive/dead with MainPID=ControlPID=0, the original PID/start identity absent, supported mount/namespace/launch conditions unchanged, and the application roots still bound. Only then is the full config/data archive copied, synchronized and checked. Original live file paths and historical artifacts are retained; business/history rows are unchanged.

Ready verification reopens the pinned artifacts, validates directory/file ownership, permissions, exact entries, integrity and component binding, and checks the original stopped baseline and safe references again. It rechecks database identity/claims before returning. It never creates, replaces, synchronizes, repairs or deletes host files, and never starts/stops a service. Repeated ready calls retain revision and UpdatedAt.

## Failures and compatibility

Any error retains the last committed phase, durable ownership and forensic artifacts. Capture does not reset an interrupted phase, recapture it, clear its fence, or attempt automatic resume. The old prepared abort explicitly rejects every checkpoint phase. Ordinary lifecycle, recovery and startup paths remain fenced.

Only existing prepared intent qualifies for old startup claim reclamation. Checkpoint phases retain potentially live transient owners even after a controller process exits. Recovery for those phases must be implemented explicitly before operators can safely continue them.

Structural journal validation performs no host/artifact reads. Valid partial checkpoint phases remain readable when resolving an interrupted whole-database rollback, including when their directories were never created. Corrupt original journal evidence blocks unsafe database replacement. Old prepared/aborted tables lacking checkpoint columns remain readable before migration; permitted startup adds the nullable columns without changing retained admission/history.

The protocol excludes platform writers through claims and rechecks ordinary drift. It does not lock out an external privileged writer or claim arbitrary live TSDB byte equality after later external edits. Existing supported startup, file, mount and namespace constraints continue to apply; ACL/xattr restoration and actual historical replacement are outside this step.

## Verification

Focused commands (vendored/offline Go toolchain):

~~~sh
go test -count=1 -timeout=30m ./services ./cmd/server -run 'Test(PrometheusRestoreCheckpoint|StartupPrometheusRestoreCheckpoint)'
go test -race -count=1 -timeout=40m ./services ./cmd/server -run 'Test(PrometheusRestoreCheckpoint|StartupPrometheusRestoreCheckpoint)'
~~~

Tests cover literal current-vs-historical content, COMMIT-before-mkdir/stop/archive ordering, new-connection verification, corruption/missing artifacts, changed identities, lost/replaced/sibling claims, reentrant barriers, cancellation/error redaction, exact phase/capability bounds, and real child process exits at four durable boundaries. Actual server tests cover old columns, owner retention and valid/corrupt interrupted database rollback.

The real-host fixture uses PROMLIFECYCLE_PAIRED_RESTORE_CHECKPOINT=1 together with the existing paired-point/preview/journal opt-ins. It preserves a current marker, verifies ready idempotence, confirms the original service remains stopped, and starts a separate owned probe from the pinned binary and extracted checkpoint archive to query a fixed TSDB sample. Probe cleanup is test machinery, not a production resume or historical restore operation. Run the original source-loss acceptance separately with the checkpoint opt-in unset. Unsupported reference inventories must reject before a durable preparation/checkpoint.

Final acceptance also retains full unit/vet/build/product closure/frontend/API/CLI/race gates, source+CI graph checks, UBI8 build/runtime checks and immutable source/delivery hashes. Exact final results belong to the delivery verification receipt.
