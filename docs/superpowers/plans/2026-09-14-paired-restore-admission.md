# Paired Restore Admission Implementation Plan

> **For agentic workers:** Execute inline using superpowers:executing-plans and TDD; shared fixtures and admission invariants are coupled. Use independent code review after implementation.

**Goal:** Complete Step 10.2.2.1 paired historical restore admission preview, leaving live mutation to the following journaled transaction.
**Architecture:** A new BackupService entry point composes existing historical evidence verification, current runtime probes and same-host claims with a bounded direct systemd reference inventory. It returns private-data-free preview evidence and never enables restoration.
**Tech Stack:** Go, existing GORM/SQLite, systemd 239-compatible commands, existing local/SSH runners.
**Spec:** docs/superpowers/specs/2026-09-14-paired-restore-admission.md

## Global constraints
- Ten-minute claimed operation; host before component; reentrant pause/transition checks.
- Existing startup bounds 64 files, 1 MiB/file, 8 MiB total, 256 KiB manifest; binary 512 MiB, argv 64 KiB.
- Inventory at most 2048 names, 512 KiB listing input, 2 MiB property output; unsupported indirect paths fail closed.
- No HTTP route, database schema, common claim protocol change, live file write, lifecycle operation or automatic upgrade integration.
- Failed/verified plan, ready historical point, active current service; matching paths/argv/topology/enablement; current version belongs to the plan.
- Result is preview-only and always RestoreAuthorized=false; current checkpoint and full transaction remain mandatory.

### Task 1: independent reference inventory
Files: services/prometheus_paired_restore_references.go and services/prometheus_paired_restore_references_test.go.
Interfaces:
~~~go
func parsePairedRestoreUnitReferences(raw string) ([]pairedRestoreUnitReference, error)
func readPairedRestoreUnitReferences(ctx context.Context, r hostCommandRunner) ([]pairedRestoreUnitReference, error)
~~~
- [x] Write literal fixture tests requiring complete name coverage, canonical direct references and rejection of shared aliases, dynamic/escaped paths, malformed/truncated/oversized replies; test the collector against real stub systemctl executables so shell failures and argument boundaries are exercised.
- [x] Run go test ./services -run '^TestPairedRestoreReferences' -count=1; record RED.
- [x] Implement bounded collection and parsing; resolve absolute reference paths on the owning host; preserve generic redacted errors.
- [x] Run all reference tests, including shell execution and maximum-plus-one boundaries.

### Task 2: claimed admission preview
Files: services/prometheus_paired_restore_preview.go and services/prometheus_paired_restore_preview_test.go.
Interface:
~~~go
func (s *BackupService) PreviewPrometheusRecoveryPointRestore(ctx context.Context, planID uint) (*PrometheusPairedRestorePreview, error)
~~~
First behavior:
~~~go
point, err := f.s.CreatePrometheusRecoveryPoint(ctx, f.plan.ID)
if err != nil { t.Fatal(err) }
if err := f.db.Model(f.plan).Update("status", models.ChangeStatusFailed).Error; err != nil { t.Fatal(err) }
proof, err := f.s.PreviewPrometheusRecoveryPointRestore(ctx, f.plan.ID)
if err != nil || proof.PointID != point.ID || proof.RestoreAuthorized || proof.Scope != "paired_restore_admission_preview_only" { t.Fatalf("preview: %+v %v", proof, err) }
~~~
- [x] Add the real-file/file-backed fixture and observable no-mutation assertions; record a failing runtime result before implementing admission.
- [x] Compose trusted historical binding, status/version/argv checks, existing target dependency policy, runtime/file topology and overlap checks, and direct reference inventory.
- [x] Reinspect current startup/binary/runtime/reference inventory and trusted DB identity before returning evidence; return no authorization token.
- [x] Add isolated metadata, pause/reentrant, dependency, shared path/reference, optional file, artifact corruption, cancellation and drift regressions; verify no lifecycle/file/business-state mutation and no leaked claims.
- Release gate: run the complete frozen service suite, including all recovery-point, paired-preview, dependency and startup tests.

### Task 3: real acceptance and final gate
Files: services/prometheus_recovery_point_integration_test.go; docs/paired-prometheus-restore-admission.md. Evidence outside the repository.
- [x] Extend the opt-in owned real fixture with preview acceptance and a second owned non-running unit demonstrating sharing rejection; assert unchanged PID and fixed historical query.
- Required release evidence: Run actual local unsupported-reference rejection and positive 150/151 SSH acceptance with independent cleanup checks. No host credentials in source/evidence.
- Required release evidence: Independent review and covering regressions for confirmed findings.
- Required release evidence: Freeze source; run full unit/vet/build, business/page/API, race, official binary regressions and UBI8 build/runtime gates appropriate to the final change.
- Required release evidence: Refresh complete source/CI index; verify complete detect_changes and all source identities. Commit only this completed admission step.
- Required release evidence: Deliver patch, changed files, UBI8 server, complete receipts and SHA256SUMS; verify exact Windows artifact set and hashes. Explicitly keep Step 10.2.2 transaction execution pending.

Final release results are recorded against the frozen source tree in the delivered verification.md and machine-readable receipts. They must all pass before the commit and delivery are declared complete; this implementation plan does not pre-certify them.
