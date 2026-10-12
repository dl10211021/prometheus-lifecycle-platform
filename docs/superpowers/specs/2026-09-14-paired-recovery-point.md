# Paired Prometheus recovery point (Step 10.2.1)

## Purpose and sequence

Step 10.2 is split at the durable evidence / mutation boundary. This substep creates and verifies a trusted recovery point containing the exact installed executable, the Step 10.1 startup snapshot, and a stopped-application full configuration/TSDB archive. Step 10.2.2 will admit and apply historical recovery, including startup-file restoration and crash recovery. This substep has no new HTTP endpoint and is not invoked by existing upgrade/apply/restore routes.

The existing standalone full-backup format retains its declared configuration/data scope. A new internal BackupService protocol owns the stronger ordering required here. Generic deletion and live restoration reject pinned paired backups; archive-only verification remains available with its existing evidence-only scope.

## Global constraints

- Binary Prometheus only, with a recorded canonical absolute executable and configuration/data roots; the original service must be active/running.
- Persisted executing upgrade plan with a complete matching historical before snapshot, imported-argv hash and no existing backup/recovery point. One immutable payload per plan.
- Host claim before component claim; reload plan, component and owning host under claims; keep claims through bounded compensation.
- One ten-minute operation deadline. Cleanup uses a separate two-minute context and a fresh bounded SSH transport. Refresh the pause timestamp before compensation. Persist a fail-closed recovery block as the first pause row and keep it until post-start paired validation completes; interrupted capture must never enter generic unchecked auto-resume. Final quarantine writes have a separate 30-second DB bound.
- Startup limits remain 64 files, 1 MiB per file, 8 MiB total and 256 KiB manifest. Executable copies read at most 512 MiB + 1 byte and reject overflow. Proc argv is limited to 64 KiB; stat/version output to 4 KiB.
- Root-owned 0700 artifact directories and 0600 payloads, no aliases/hardlinks/group-or-other writable/special files. Artifacts remain on their owning host. Do not return raw startup contents, argv or credentials.
- No binary, startup file, configuration or data replacement. Capture intentionally stops and resumes the service to create the full archive.
- Verification reopens pinned artifacts and checks DB associations. Capture status and artifact integrity do not authorize restoration.

## Runtime and consistency admission

Before stopping, inspect the actual systemd service and /proc main process. Bind the executable path and bounded SHA256 to the recorded binary; verify its reported stable version. Parse actual NUL-separated argv without substituting platform defaults and require its absolute config/data flags to match the declared archive roots. Reject agent mode, duplicate/missing/relative paths and unsupported private temporary roots. Match host-visible and process-visible config/data inode/device identity and reject nested mount points within those roots. Preserve a process start-time token and argv hash, not raw argv.

Require the loaded direct ExecStart executable and exact argv from typed busctl D-Bus output to match the observed process before stopping and before resuming. Parse array counts and quoted token boundaries, enforce numeric types/ranges and output bounds, and reject escapes/non-ASCII tokens. Inspect the protected fragment/drop-in literal ExecStart to reject legacy execution modifiers, expansion, physical continuations, includes and unsupported sections. Reject loaded root/image/bind/temp mappings, PAM sessions, inaccessible paths and unsupported newer namespace directives. Reject lifecycle pre/post start/stop/condition hooks and ambiguous quoting, whitespace, environment or systemd-specifier expansion. Capture the startup reference and executable, then recheck the active baseline. Persist ComponentBackupPause in termination_unconfirmed state before stopping. Confirm inactive/dead with zero main/control PIDs and disappearance of the original process identity before archiving. Check unit files and binary metadata/hashes again while stopped before and after archive creation. A drift or unconfirmed command must not trigger an unsafe restart.

Publish archive files with sync before committing Backup, BackupManifest, PrometheusRecoveryPoint and ChangePlan.BackupID in one database transaction. The point pins before/after plan hashes, historical component/host binding, startup reference, binary metadata/checksum, actual argv hash, and archive path/checksum/size/source roots.

Only after that commit resume the original service, revalidate its runtime and unchanged startup/binary bytes, and mark capture ready. If a failure occurs earlier, resume only after rechecking the original baseline, using the independent cleanup context. Preserve a durable recovery block when restart or termination is unconfirmed. A committed captured point survives a later resume/final-record failure but is never returned as a successfully completed capture.

## Historical verification

Load the point by plan ID from the database; accept no caller-supplied hashes or directory scan. Check schema/size, plan hashes and IDs, current owning-host identity, immutable historical runtime binding, and pinned backup/manifest association before commands. Reopen and hash all three artifacts; reject missing/extra/unsafe files and metadata reassociation. Do not rely on a prior VerifiedAt.

The historical verifier uses the trusted before snapshot and stored imported-argv hash. A later current version or launch-argument change does not substitute current identity into historical artifact admission. Existing claim barriers still exclude active version transitions. Verification returns IDs, version, sizes, capture state and explicit evidence-only scope.

## Deliberate limits

Capture records bytes and Unix metadata, not ACL/xattr restoration semantics, enablement-symlink reconstruction or application-format compatibility. Shared/external startup layouts still require restoration-specific admission in Step 10.2.2. Platform claims cannot exclude external privileged writers; repeated observations detect ordinary drift but are not a hostile-root filesystem transaction. Archive-only verification never proves that a future restart is safe.

Failed unpublished captures intentionally retain private artifacts for diagnosis, including a possible full archive staging copy. There are no automatic retries or retention deletion in this substep. Operators must account for free space and remove only proven unreferenced owned directories after confirming no active claims/remote commands and a safe recovery state. Never delete a directory referenced by a paired point, plan or backup. Automatic retirement and interrupted paired recovery remain later work.

## Acceptance

Test actual path parsing, bounded file reads/growth, artifact integrity, DB atomicity/reassociation, claims, cancellation, stop confirmation, baseline drift and compensation. Reopen a file-backed database in a new process for historical verification. Use an owned actual Prometheus systemd fixture to prove quiescence/resume, query continuity and unchanged startup/binary bytes; exercise the same protocol over SSH on the two authorized test hosts. Verify cleanup and no leaked credentials. Run relevant full regressions, race, UBI8 and graph/source identity gates before commit.
