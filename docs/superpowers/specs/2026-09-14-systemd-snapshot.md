# Step 10.1: durable systemd startup snapshots

This is the first independently testable part of step 10. It adds capture and verification primitives to BackupService. It does not rewrite units, start/stop services, apply migration arguments, or claim that existing full backups now include units.

Capture a fresh DB component under host then component claims, using the existing local/SSH runner. Support binary Prometheus services with stable loaded systemd state. Record actual Id, FragmentPath, ordered DropInPaths, EnvironmentFiles (including optional missing files), UnitFileState, ActiveState, SubState, MainPID and ControlPID. Reject aliases, transient/masked/unloaded units, pending daemon reload, unstable process states, malformed properties, wildcard/escaped/noncanonical paths, symlinks, hard links, nonregular files, writable/unowned sources, and oversized inventories.

Copy exact bytes only into a new root-owned 0700 directory on the owning host. Payloads and manifest are root-owned 0600. Record source mode/uid/gid/size/hash, and bind to component/host/runtime identity without storing credentials in metadata. Limits: 64 files, 1 MiB each, 8 MiB total. Recheck source metadata/hashes and systemd properties after copying. Publish manifest last, sync files and directory, then return its pinned SHA256 reference. An interrupted or failed capture has no returned reference; protected incomplete directories remain for diagnosis.

Verification requires the caller's trusted pinned reference, reopens durable host files, validates identity and structure and recomputes all hashes. Optionally verify the current live baseline. Never trust cached verification. A new BackupService instance must verify the same artifact after process restart.

Scope excludes process environment, manager-global environment, credentials, ACL/xattr semantics, binaries, config/TSDB data, boot-time enablement symlink reconstruction, restore execution and change-plan/backup pairing. Snapshot success is evidence only and must never authorize a unit rewrite or rollback. Step 10.2 must enforce restore-specific admission and pair these artifacts with the existing durable version/data transaction before any application change.

Validation: parser negative matrix; real filesystem capture/permission/hash/corruption/race/error tests; DB stale identity and lock exclusion; opt-in owned systemd service acceptance; Go regressions/race/vet/build; UBI8 build/runtime; independent review; complete GitNexus detect_changes and source identity audit before commit.

Each claimed remote capture or verification reuses one SSH transport. Dialing/handshake and command execution share the operation deadline. Local/custom runners retain their existing behavior. The remote acceptance fixture opens a separate bounded cleanup transport so an expired main request cannot strand its test unit.
