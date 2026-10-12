# Step 10.2.2.1 paired restore admission preview

## Goal and sequence
Deliver an internal, non-mutating admission preview for the paired historical restore transaction. This is the first independently verifiable part of Step 10.2.2. It directly consumes Step 10.2.1 persisted evidence. Live replacement, compensation and crash-resume journal execution remain the next transaction substep, not an implicit effect of this preview.

## Contract
BackupService.PreviewPrometheusRecoveryPointRestore(ctx, planID) accepts only a stored ready paired point belonging to a failed or verified component upgrade plan. The current owning component must be binary Prometheus, active/running, have no unresolved pause or version transition, retain the same host, paths, service and launch identity as the historical snapshot (except version/image), and retain the imported-argv hash. The current version must match this plan's before or after version; a verified plan requires the after version. Target and current versions must be known stable 2.x/3.x.

Host claim precedes component claim; reentrant callers must pass the same pause and transition checks. Reload trusted records under the claims. Apply existing retained same-host Thanos compatibility checks to the historical target. No common claim protocol changes.

## Artifact, runtime and topology checks
Reopen the historical startup manifest, executable and stopped configuration/TSDB archive from trusted DB associations; verify their existing integrity/permission bounds. Compare loaded direct launch arguments with the captured runtime argv hash, inspect actual current executable/version/process/root identities and require supported namespace/hook behavior. Startup topology must be identical (same file paths, roles and optional flags); content and Unix metadata differences are previewed. Unit enablement must be unchanged. Startup paths, binary, configuration/data roots and backup root must not overlap ambiguously.

Inspect current startup files and binary twice and reject drift, including PID/start-time/root identity changes. Reload plan, point and component identity at the end. Preserve raw argv, environment and host errors as private data.

## Shared references
Reject overlapping runtime paths or service identity belonging to any other retained registered component on the owning host. Inspect direct systemd fragment/drop-in/environment and executable references from installed non-template services plus loaded instances/transient services. Uninstantiated templates are not executable instances and are excluded; this is a snapshot of configured direct references, not proof about future services or arbitrary helper processes.

A bounded, read-only shell collector enumerates names, reads Id/Names/LoadState and file/executable properties, and emits a complete framed result. Maximum 2048 names, 512 KiB listing input, 2 MiB properties. Require every enumerated name to be accounted for by returned Id/Names, reject malformed, ambiguous, oversized or unstable inventories, and resolve literal absolute reference paths on the owning host before comparing. Unknown dynamic/specifier/glob/escaped paths fail closed. Hook executable references count too. No command output or raw launch lines are returned in errors.

The collector uses systemctl interfaces present in [systemd 239](https://github.com/systemd/systemd/blob/v239/man/systemctl.xml). Empty properties may be omitted by systemctl; mandatory identity/name/load-state fields and complete result framing prove enumeration completion. Current and final parsed inventories must agree.

## Result and limits
Return plan/point/component/host/backup IDs, source/target versions, intended replacement paths/roles, startup/binary content and metadata change flags, retained dependency evidence, check names and observed time. Scope is paired_restore_admission_preview_only; RestoreAuthorized is always false, RequiresDowntime and RequiresCurrentCheckpoint are true. The preview is not a reusable execution token. Actual restore must re-admit under its claims, capture a matching current-state point and persist a crash-safe journal before any live mutation.

Unix metadata preview does not certify ACL/xattr, enablement reconstruction, external configuration dependencies or application semantics. Exact historical binary plus its own historical archive establishes pairing, not permission to feed new-version data to an old binary. Claims exclude platform writers; external privileged writers remain outside that guarantee.

## Acceptance
Use real filesystem/file-backed SQLite fixtures. Observe a failing behavior test before implementation. Verify supported same-version and post-upgrade metadata previews, all association/status/argv/version/paused/reentrant/dependency failures, overlapping roots, registered and direct systemd sharing, aliases, optional files, malformed/bounded inventories and mid-preview drift. Assert no lifecycle command, file mutation, durable business-row update or claim leak, including cancellation and runner errors. Run the owned real Prometheus fixture locally and through the authorized 150/151 SSH transports; keep fixtures isolated, verify unchanged process/query history, and clean only owned resources. Review independently, run relevant complete regressions/race/UBI8, refresh full source/CI graph, bind verification to one frozen source tree, commit, and verify Windows delivery hashes.

Execution-reference parsing accepts complete systemctl records, including repeated command rows and ExecReload. Only standard environment-variable brace tokens are accepted in argv; arbitrary braces that could impersonate records reject admission. Every alias has exactly one canonical owner. Raw byte bounds preserve trailing newlines before measuring.

Names reported solely as absent dependencies remain accounted for: LoadState=not-found is accepted only with ActiveState=inactive, SubState=dead, zero MainPID/ControlPID, and no direct file/executable references. Any inconsistent or incomplete absent state rejects admission.

Actual host acceptance refinement: WSL vendor units use bare executable names, outside this absolute-reference contract. The owned local fixture must reject this explicit boundary without PID/startup/historical-sample changes; the two isolated acceptance hosts exercise positive preview plus shared ExecReload rejection. Supporting bare command search/environment resolution remains outside this step.

Namespace ownership: nonempty RootDirectory, RootImage, BindPaths, BindReadOnlyPaths, TemporaryFileSystem, MountImages, ExtensionImages or ExtensionDirectories reject host-path admission. systemd 239 [unprintable] values for the two bind arrays and tmpfs array require separate exact typed-empty D-Bus verification with 4096-byte reply bounds and 48 KiB command batches. Literal @ and parent segments in external reference paths are resolved on the owning host before canonical comparison. Same-ID Names may be unioned only with identical direct paths and namespace requirements; every alias still has one owner. Registered aliases are checked against the owning unit.
