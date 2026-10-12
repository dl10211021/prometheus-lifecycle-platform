# Systemd startup snapshot implementation plan

> **For agentic workers:** Use superpowers:executing-plans inline, with test-driven development and review checkpoints.

**Goal:** Deliver Step 10.1 durable startup snapshot capture and verification without changing service execution.
**Architecture:** BackupService owns identity and claims; a small host-runner implementation reads systemd properties and hashes/copies files on the owning host. A manifest published last is pinned by a caller-held reference.
**Tech Stack:** Go, GORM, existing local/SSH runners, systemctl, GNU coreutils.
**Spec:** docs/superpowers/specs/2026-09-14-systemd-snapshot.md

## Global constraints
- Binary Prometheus only; host claim before component claim; fresh DB identity.
- 64 files, 1 MiB each, 8 MiB total; root-owned 0700 artifact directory and 0600 payload/manifest.
- No service mutation, no new HTTP endpoint, no automatic parameter conversion.
- Capture success is evidence only; trusted manifest hash required to verify.

### Task 1: inventory admission
Files: services/systemd_snapshot.go, services/systemd_snapshot_test.go.
Interfaces: parseUnitSnapshotProperties(unit, output string) (unitSnapshotState, []unitSnapshotFile, error).
- [ ] Write tables: valid fragment/drop-in/environment; missing/duplicate properties; optional absent env; alias, reload, transient, masked, activating, wildcard, traversal, escaped paths.
- [ ] Run: go test ./services -run '^TestUnitSnapshotProperties' -count=1; observe missing implementation.
- [ ] Implement bounded strict property parsing and file inventory deduplication.
- [ ] Repeat focused test and review every admitted property.

### Task 2: durable capture and verification
Files: services/systemd_snapshot_host.go, services/systemd_snapshot_test.go.
Interfaces: captureUnitSnapshot(ctx, runner, root, binding), verifyUnitSnapshot(ctx, runner, ref, binding, live).
- [ ] Add real-filesystem tests checking byte-for-byte payload preservation, permissions, optional absence, fresh-instance verification, corrupt payload/manifest, symlink/hardlink/FIFO/oversize denial, source drift and cancelled/failed publication.
- [ ] Run focused tests and record RED.
- [ ] Implement read-only probes, private exclusive directory, copies, repeated baseline checks, bounded manifest, final sync, pinned reference and verification.
- [ ] Run focused tests plus race; inspect logs for secret sentinel absence.

### Task 3: claimed service interface and acceptance
Files: services/systemd_snapshot.go, services/systemd_snapshot_integration_test.go, docs/systemd-startup-snapshots.md.
Interfaces: BackupService.CaptureComponentUnitSnapshot(ctx, componentID); BackupService.VerifyComponentUnitSnapshot(ctx, ref, checkLive).
- [ ] Add DB/claim tests for stale DB identity, concurrent host/component ownership, unrelated reference, caller cancellation and no lifecycle writes.
- [ ] Implement fresh identity binding through existing local/SSH runner; retain no raw unit/env bytes in Go errors or public metadata.
- [ ] Add opt-in owned real systemd fixture; compare enabled/running/PID state and original bytes before/after capture and verify; remove only owned fixture.
- [ ] Run complete relevant gates, UBI8, independent review, GitNexus refresh/detect and source hash audit.
- [ ] Commit verified files and deliver a checksummed report with explicit Step 10.2 boundary.
