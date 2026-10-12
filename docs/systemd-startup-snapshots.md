# systemd startup snapshots (Step 10.1)

This step provides internal BackupService primitives:

- CaptureComponentUnitSnapshot(ctx, componentID) returns a caller-pinned UnitSnapshotReference.
- VerifyComponentUnitSnapshot(ctx, reference, checkLive) reopens and verifies the owning-host artifact. With checkLive=true it also compares current systemd properties and original file metadata/hashes, including PID.

There is no new HTTP endpoint or automatic invocation from upgrade, configuration apply, restore or startup reconciliation. Existing full backups still cover their declared config/data paths. Capture success does not authorize parameter changes or recovery.

## Captured evidence

A fresh binary Prometheus component is loaded under host-then-component claims. The existing local/SSH runner reads the actual loaded service fragment, ordered drop-ins and environment file references from systemctl show. Repeated EnvironmentFiles rows are preserved in order. Optional absent environment files are recorded as absent. The manifest records source mode, uid, gid, size and SHA256, plus stable enablement/running state and process IDs. Runtime and host identity are bound using hashes; plaintext component credentials and launch arguments are not copied into manifest metadata.

Exact source bytes are stored only on the owning host in an exclusively created root-owned 0700 directory beneath BackupService's configured root. Every payload and manifest is root-owned 0600. Contents are not returned to the application. The manifest is published last and files/directories are synchronized before a pinned reference is returned. All source metadata/hashes and systemd properties are checked again after copying. Failed/interrupted attempts may retain protected incomplete artifacts, but produce no trusted reference. Do not turn a directory discovered on disk into a trusted reference by computing a new hash.

The reference must be persisted by the future transaction owner before any mutation. Recreating BackupService does not lose the snapshot: verification reads the manifest and payloads again and compares them with the stored reference and current DB identity. It never accepts an old successful verification as fresh evidence. Changing host/runtime metadata invalidates this current-identity verifier; a future historical-recovery path needs its own trusted transaction binding.

## Admission and bounds

Supported: loaded, non-transient, non-aliased services; enabled/disabled/static unit-file states; active/running, inactive/dead, or failed/failed process states with consistent PID/control PID. A pending daemon reload, masked or transitional state is rejected.

Paths must be absolute, canonical, and use the deliberately limited character set accepted by this implementation. Wildcards, escaped/whitespace paths, /proc, /sys, /dev and /run sources are rejected. Sources must be root-owned ordinary files without symlink aliases, hard links, special permission bits, or group/other write permission. Missing files are accepted only when the environment reference is optional.

Limits are 64 distinct files, 1 MiB per source and 8 MiB total. Hashing and copying read at most 1 MiB + 1 byte and reject overflow, including a source that grows after initial inspection. Manifest size is bounded at 256 KiB. Root-only filesystem tests explicitly skip when running unprivileged; parser tests still run.

## Practical limits and next step

This captures startup-file evidence, not a consistent checkpoint of the Prometheus application. It excludes process/manager-global environment, credentials supplied by systemd mechanisms, ACL/xattr restoration semantics, binary/config/TSDB snapshots, and reconstruction of enablement symlinks. Platform claims exclude cooperating platform operations; they cannot exclude an external root writer. Repeated probes detect ordinary drift but are not a filesystem transaction against a hostile privileged writer.

Step 10.2 must bind this reference durably to the same historical binary/config/data recovery point, establish restore-specific admission for shared/external unit structures, record checkpoints before changing any live files, and test stopped-state restoration, daemon-reload/restart failures, health failures and server crash recovery. Actual parameter migration remains preview-only until that transaction is complete.

## Verification

Focused tests:
`go test ./services -run 'Test(UnitSnapshot|ComponentUnitSnapshot)' -count=1`

Opt-in real systemd acceptance requires root, a verified Prometheus 2.55.0 binary and these environment variables:
`PROMLIFECYCLE_REAL_SYSTEMD_SNAPSHOT=1`, `PROMLIFECYCLE_SNAPSHOT_PROMETHEUS_BINARY=/absolute/path/to/prometheus`.
Run `go test ./services -run '^TestRealSystemdUnitSnapshot$' -count=1 -v`.
It creates an exclusive unit and private data directory, verifies unchanged process/enablement, detects disk drift and cleans up only its owned unit/files.

The environment-file scope follows the systemd project documentation: [systemd.exec](https://github.com/systemd/systemd/blob/main/man/systemd.exec.xml). Acceptance uses actual systemctl output instead of assuming its list rendering.

SSH transport acceptance uses TestSSHSystemdUnitSnapshot with explicitly supplied PROMLIFECYCLE_SNAPSHOT_SSH_HOST and PROMLIFECYCLE_SNAPSHOT_SSH_PASSWORD. Credentials are read from the process environment and are not written to the repository. It creates an exclusive sleep-service fixture on the selected test host, verifies remote capture/reopen and unchanged PID/enablement, then removes only its owned unit and directory. This tests the SSH transport; the separate local acceptance uses real Prometheus.

Each claimed remote capture or verification reuses one SSH transport. Dialing/handshake and command execution share the operation deadline. Local/custom runners retain their existing behavior. The remote acceptance fixture opens a separate bounded cleanup transport so an expired main request cannot strand its test unit.
