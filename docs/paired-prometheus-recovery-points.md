# Paired Prometheus recovery points

BackupService provides paired capture and historical verification. Binary Prometheus upgrades can now opt in with `paired_recovery: true`; capture occurs before the ordinary automatic-rollback backup and runtime apply. For the maintenance recovery workflow, outcomes and interrupted-execution command, see [paired recovery execution](paired-prometheus-recovery-execution.md).

## Supported capture

CreatePrometheusRecoveryPoint(ctx, planID) accepts a persisted executing binary-Prometheus upgrade plan with a complete matching before snapshot and fresh imported-argv hash. The component must be running, have explicit canonical executable/configuration/TSDB paths and a managed systemd service. One immutable point is permitted per plan.

The loaded direct ExecStart is read as typed D-Bus data via busctl, preserving each argument boundary on systemd 239 and newer. It must exactly match the observed process executable and argv. A strict literal ASCII source check rejects modifiers that legacy D-Bus does not expose. This first version rejects escaped/non-ASCII argv, ambiguous whitespace/quoting, environment/specifier expansion, physical continuations including comment continuations, external include directives, unsupported sections and start/stop/condition hooks. Required typed namespace checks reject root/image/bind/temp filesystem remapping, dynamic users, private temporary roots, home isolation, PAM sessions and inaccessible paths; unsupported newer image/extension remapping directives are rejected in source. It also rejects unsupported agent/relative paths, nested mounts, unsafe file ownership/permissions, links, pending daemon reload and path overlap. These are conservative admission limits; rejection happens before service stop when discovered at initial admission.

The operation claims the owning host before the component, copies the installed executable and startup snapshot, persists a fail-closed pause, confirms shutdown and archives configuration/TSDB. Backup, manifest, point and plan association are committed in one database transaction before restarting the original service. Only validated startup, binary, runtime paths/argv and readiness allow the pause to clear and the point to become ready.

The first pause record blocks generic startup recovery. A controller crash, ambiguous remote termination or unsafe restart leaves a visible termination_unconfirmed recovery block. Explicit interrupted paired recovery is not implemented in this step; generic resume must not be used to bypass that block.

Opted-in upgrades atomically publish `ChangePlan.PairedRecoveryPointID` and retain a separate ordinary `BackupID`. Legacy direct capture without the option still uses the original `BackupID` association. A nonnil explicit point association must match exactly; without it, the exact legacy backup match remains required. Requested capture failure prevents applying the upgrade.

## Historical verification

VerifyPrometheusRecoveryPoint(ctx, planID) reopens the persisted references on their owning host. It checks plan hashes, host/component identity, backup association, startup manifest, executable and full archive. It reconstructs identity from the historical before snapshot, so later current version/argv changes or missing live source files do not substitute a new historical baseline.

Its scope is paired_historical_artifact_evidence_only. A ready point means capture and original-service resume completed; a captured point can survive a subsequent resume/final-record failure. Neither state or verification result authorizes restoration or proves compatibility with another application version.

Generic backup deletion and generic live restoration reject paired artifacts, including internal rollback callbacks. Historical paired restoration uses the dedicated [execution workflow](paired-prometheus-recovery-execution.md); it does not grant generic mutation permission over paired artifacts. Existing standalone backups keep their normal behavior. Generic archive verification/rehearsal only assesses its declared archive scope.

## Limits and retained evidence

One capture/verification has a ten-minute deadline. Compensation uses a separate two-minute context and fresh SSH connection; final quarantine writes are bounded to 30 seconds. Startup limits are 64 files, 1 MiB per file, 8 MiB total and 256 KiB manifest; the executable limit is 512 MiB with maximum-plus-one overflow checks. Root-owned directories are 0700 and artifacts 0600. Raw argv, environment contents and credentials are not returned by the point API.

The full archive requires space for a staging copy plus its compressed output, startup copies and executable. An unsuccessful unpublished capture retains its private directories as forensic evidence, potentially including a TSDB staging copy. This step has no automated retry or retention cleanup. Operators should verify available space before retrying.

For manual cleanup, first confirm the controller/remote operation has terminated, no host/component claim is active and the service recovery state has been resolved. Cross-check directory references in PrometheusRecoveryPoint payloads, ChangePlan.PairedRecoveryPointID, ChangePlan.BackupID and Backup.Path; preserve every referenced directory. Only then remove the exact unreferenced directories created by the failed capture on the owning host. Never glob-delete the backup root or clear a recovery block merely to allow deletion.

Snapshots record bytes and Unix metadata; they do not implement ACL/xattr restoration, enablement reconstruction, shared-startup restoration admission or a transaction against hostile privileged writers. Paired historical execution and explicit interrupted-execution compensation are documented separately; compatibility admission remains required.

## Verification

The test suite covers actual argv parsing, bounded files, atomic database publication, process identity/stop confirmation, loaded-launch drift, claims/reentrancy, cancellation, immutable associations, corruption, generic-route protection and fail-closed compensation. A subprocess reopens a file-backed database. Opt-in real fixtures exercise the production local/SSH runners with an actual Prometheus 2.55.0 executable, unit/drop-in/environment/config/TSDB, fixed historical query checks, source loss and explicit cleanup.

## Interface references

The implementation uses the typed ExecStart property defined by the [systemd D-Bus interface](https://github.com/systemd/systemd/blob/main/man/org.freedesktop.systemd1.xml). The compatibility parser follows the argument-array counts and quoted-string boundaries emitted by [systemd 239 busctl](https://github.com/systemd/systemd/blob/v239/src/busctl/busctl.c), accepting a conservative unescaped ASCII subset. The source guard rejects continuations before comments because [systemd 239 joins physical lines before parsing them](https://github.com/systemd/systemd/blob/v239/src/shared/conf-parser.c).
