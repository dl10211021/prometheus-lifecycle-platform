# Paired Recovery Point Implementation Plan

> **For agentic workers:** Execute inline using superpowers:executing-plans and TDD; the capture/verification boundaries share types and invariants. Use independent code review after implementation.

**Goal:** Complete Step 10.2.1 durable paired capture and historical artifact verification.
**Architecture:** New BackupService orchestration reuses existing protected snapshot, archive, pause and SSH primitives. A dedicated model pins the three artifacts and plan association atomically; historical verification reconstructs its binding from trusted stored history.
**Tech Stack:** Go, SQLite/GORM, systemd/procfs, GNU coreutils, existing SSH client.
**Spec:** docs/superpowers/specs/2026-09-14-paired-recovery-point.md

## Global Constraints

- Binary Prometheus only; initially active/running; canonical declared executable/config/data roots.
- Host-before-component claims and fresh DB identity, with one ten-minute operation deadline.
- Cleanup: separate two-minute context, fresh SSH transport and refreshed durable pause timestamp; first pause is fail-closed and remains so through paired validation; final quarantine DB writes have a 30-second bound.
- Startup: 64 files, 1 MiB/file, 8 MiB total, 256 KiB manifest.
- Executable: 512 MiB maximum, streaming maximum-plus-one overflow detection; argv 64 KiB, stat/version 4 KiB.
- Root 0700 directories / 0600 artifacts; exact historical hashes persisted before resume.
- No new HTTP route, automatic upgrade integration, live file replacement or restore authorization.
- One immutable payload per persisted executing upgrade plan; point and full-backup association commit together.

### Task 1: shared bounded evidence helpers

Files: core/deployer/prometheus_recovery_scope.go; new core/deployer/prometheus_argv_paths_test.go; services/systemd_snapshot_host.go; services/systemd_snapshot_service.go; new services/recovery_point_file_test.go.

Interfaces:
~~~go
func PrometheusArgvPaths(args []string) (string, string, error)
func inspectBoundedSnapshotFile(ctx context.Context, r hostCommandRunner, f unitSnapshotFile, maxSize int64) (unitSnapshotFile, error)
func makeUnitSnapshotBinding(c *models.Component, argsHash string) unitSnapshotBinding
~~~

- [x] Add literal table tests for actual argv paths, including relative defaults that must not be substituted. Add real-file tests accepting files over 1 MiB only under the larger binary limit and rejecting overflow/unsafe ownership or links.
- [x] Run: go test ./core/deployer ./services -run 'Test(PrometheusArgvPaths|RecoveryPointBoundedFile)' -count=1. Record the missing-feature failure.
- [x] Extract the existing strict argv parser after effective-argument compilation; parameterize the file inspector while preserving its one-MiB wrapper; extract the existing binding hash calculation unchanged.
- [x] Run new tests and existing parameter/recovery-scope/systemd-snapshot regressions.

### Task 2: claimed paired capture and historical verification

Files: new models/prometheus_recovery_point.go; cmd/server/main.go migration registration; new services/prometheus_recovery_point.go, services/prometheus_recovery_point_runtime.go, services/prometheus_recovery_point_capture.go, services/prometheus_recovery_point_verify.go; matching test files.

Interfaces:
~~~go
func (s *BackupService) CreatePrometheusRecoveryPoint(ctx context.Context, planID uint) (*models.PrometheusRecoveryPoint, error)
func (s *BackupService) VerifyPrometheusRecoveryPoint(ctx context.Context, planID uint) (*PrometheusRecoveryPointVerification, error)
~~~

- [x] Add a real filesystem / file-backed SQLite fixture, faking only systemctl/proc probes. Write an end-to-end service test requiring one associated backup and point, protected exact payloads and a successful fresh-service historical verification after current-version drift.
- [x] Run: go test ./services -run '^TestPrometheusRecoveryPoint' -count=1. Record RED.
- [x] Implement bounded runtime probes, independent private executable/archive directories, before/after-stop comparisons, atomic DB pinning, checked resume and fresh read-only verification.
- [x] Add fault cases before stop, while stopped, at archive/pin/resume/final-record boundaries; assert observable DB/artifact/service state and preserved quarantine. Inject DB failures through SQLite transaction triggers and host failures at the runner boundary.
- [x] Verify association/schema/path/integrity failures reject before any lifecycle command. Test duplicate creation, claims, cancellation, a retained captured point after resume failure, and a fresh process reopening persisted records.
- [x] Register only the new model in startup migration and verify startup against an existing database.

### Task 3: real acceptance, review and delivery

Files: new services/prometheus_recovery_point_integration_test.go and docs/paired-prometheus-recovery-points.md; evidence outside the repository.

- [ ] Build an exclusively owned real Prometheus fixture using a verified official binary, its own unit/drop-in/env/config/TSDB and loopback port. Verify stop/resume with query history preserved; delete original source files only after stopping this owned fixture and prove historical artifact verification still works.
- [ ] Repeat paired capture and reopen over SSH on the two authorized test hosts, retaining no credentials in source or evidence.
- [ ] Independently review the complete diff and fix consequential findings with covering regressions.
- [ ] Freeze one source tree; run unit, vet, build, business/page/API regressions, race, official CLI and UBI8 build/runtime gates.
- [ ] Refresh complete source/CI indexes, run nontruncated detect_changes(scope=all), compare all staged/source hashes, then commit.
- [ ] Deliver source diff, UBI8 binary and receipts with SHA256SUMS; verify each delivered file natively on Windows. State Step 10.2.2 restore/crash-transaction work remains.
