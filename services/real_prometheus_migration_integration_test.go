//go:build linux

package services

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/promlifecycle/platform/models"
)

func TestRealPrometheusMigrationAndRecovery(t *testing.T) {
	optIn := strings.TrimSpace(os.Getenv(realPrometheusOptInEnv))
	if optIn == "" {
		t.Skip("set PROMETHEUS_REAL_ACCEPTANCE=1 with the documented isolated environment to run")
	}
	if optIn != "1" {
		t.Fatalf("%s must be exactly 1 when supplied", realPrometheusOptInEnv)
	}

	f := newRealPrometheusFixture(t)
	f.createComponent(t)
	baselinePID := f.startPrometheus(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	baselineProcess, err := f.processEvidence(baselinePID)
	if err != nil {
		t.Fatal(err)
	}
	baselineVersion, baselineBuild, err := f.buildVersion(ctx)
	if err != nil || baselineVersion != "2.55.0" {
		t.Fatalf("baseline build version=%q, want 2.55.0: %v", baselineVersion, err)
	}
	historicalTime, ok := f.evidence.Baseline["historical_input_timestamp"].(float64)
	if !ok {
		t.Fatal("historical evaluation time missing")
	}
	historical, err := f.query(ctx, `plc_historical{phase="baseline"}`, &historicalTime)
	if err != nil {
		t.Fatal(err)
	}
	assertRealPrometheusSingleSample(t, historical, "plc_historical", "baseline", "255", historicalTime)
	head, err := f.waitSample(ctx, `plc_head{phase="baseline"}`, "255")
	if err != nil {
		t.Fatal(err)
	}
	headAt := head.Timestamp
	headFixed, err := f.query(ctx, `plc_head{phase="baseline"}`, &headAt)
	if err != nil {
		t.Fatal(err)
	}
	assertRealPrometheusSingleSample(t, headFixed, "plc_head", "baseline", "255", head.Timestamp)
	up, err := f.waitSample(ctx, `up{job="prometheus"}`, "1")
	if err != nil {
		t.Fatal(err)
	}
	baselineConfigHash, _, err := realPrometheusFileSHA256(f.configPath)
	if err != nil {
		t.Fatal(err)
	}
	baselineBinaryHash, _, err := realPrometheusFileSHA256(f.binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	f.evidence.Baseline["process"] = baselineProcess
	f.evidence.Baseline["buildinfo"] = baselineBuild
	f.evidence.Baseline["historical_query"] = historical
	f.evidence.Baseline["head_query"] = headFixed
	f.evidence.Baseline["up_query"] = up
	f.evidence.Baseline["config_sha256"] = baselineConfigHash
	f.evidence.Baseline["binary_sha256"] = baselineBinaryHash
	f.requireCheckpoint()

	actor := ChangeActor{ID: 1, Username: "real-acceptance", Role: "system_admin"}
	after, _ := json.Marshal(upgradeChange{NewVersion: "3.1.0"})
	plan, err := f.service.Create(ctx, ChangePlanRequest{
		Kind: models.ChangeKindUpgrade, ResourceType: "component", ResourceID: f.component.ID,
		Reason: "real selected-matrix acceptance", After: after,
	}, actor)
	if err != nil {
		t.Fatalf("create real upgrade plan: %v", err)
	}
	if err := f.verifyUnitOwnership(); err != nil {
		t.Fatalf("owned unit preflight before upgrade: %v", err)
	}
	result, err := f.service.Execute(ctx, plan.ID, actor)
	if err != nil {
		t.Fatalf("execute real upgrade: %v", err)
	}
	if result.Status != models.ChangeStatusVerified || result.BackupID == nil || result.OperationID == nil {
		t.Fatalf("upgrade records incomplete: status=%s backup=%v operation=%v", result.Status, result.BackupID, result.OperationID)
	}
	upgradedPID, err := f.mainPID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if upgradedPID == baselinePID {
		t.Fatalf("upgrade retained original PID %d", baselinePID)
	}
	upgradedProcess, err := f.processEvidence(upgradedPID)
	if err != nil {
		t.Fatal(err)
	}
	upgradedVersion, upgradedBuild, err := f.buildVersion(ctx)
	if err != nil || upgradedVersion != "3.1.0" {
		t.Fatalf("upgraded build version=%q, want 3.1.0: %v", upgradedVersion, err)
	}
	upgradedBinaryHash, _, err := realPrometheusFileSHA256(f.binaryPath)
	if err != nil || upgradedBinaryHash == baselineBinaryHash {
		t.Fatalf("upgrade binary hash did not change: %s: %v", upgradedBinaryHash, err)
	}
	historicalAfterUpgrade, err := f.query(ctx, `plc_historical{phase="baseline"}`, &historicalTime)
	if err != nil || !equalRealPrometheusSamples(historical, historicalAfterUpgrade) {
		t.Fatalf("historical query changed across upgrade: before=%+v after=%+v err=%v", historical, historicalAfterUpgrade, err)
	}
	headAfterUpgrade, err := f.query(ctx, `plc_head{phase="baseline"}`, &headAt)
	if err != nil || !equalRealPrometheusSamples(headFixed, headAfterUpgrade) {
		t.Fatalf("head query changed across upgrade: before=%+v after=%+v err=%v", headFixed, headAfterUpgrade, err)
	}
	var backup models.Backup
	var manifest models.BackupManifest
	var operation models.Operation
	var pause ComponentBackupPause
	if err := f.db.First(&backup, *result.BackupID).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.First(&manifest, "backup_id = ?", backup.ID).Error; err != nil {
		t.Fatal(err)
	}
	if manifest.Scope != "full" || manifest.Consistency != "stopped_application" || manifest.ComponentVersion != "2.55.0" {
		t.Fatalf("upgrade backup is not paired stopped_application 2.55.0 full: %+v", manifest)
	}
	if err := f.db.First(&operation, *result.OperationID).Error; err != nil || operation.Status != "success" {
		t.Fatalf("upgrade operation not successful: %+v %v", operation, err)
	}
	if err := f.db.First(&pause, "component_id = ?", f.component.ID).Error; err != nil || pause.Status != "resumed" || pause.VersionTransitionPlanID != 0 {
		t.Fatalf("upgrade transition barrier not resumed: %+v %v", pause, err)
	}
	f.metric.setPhase("post_upgrade")
	postUpgrade, err := f.waitSample(ctx, `plc_post_upgrade{phase="after_upgrade"}`, "310")
	if err != nil {
		t.Fatal(err)
	}
	postAt := postUpgrade.Timestamp
	postFixed, err := f.query(ctx, `plc_post_upgrade{phase="after_upgrade"}`, &postAt)
	if err != nil {
		t.Fatal(err)
	}
	assertRealPrometheusSingleSample(t, postFixed, "plc_post_upgrade", "after_upgrade", "310", postAt)
	postSentinel := filepath.Join(f.dataPath, "post-upgrade-owned-sentinel")
	if err := os.WriteFile(postSentinel, []byte("post-backup-data\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.evidence.Upgrade["plan"] = result
	f.evidence.Upgrade["operation"] = operation
	f.evidence.Upgrade["backup"] = backup
	f.evidence.Upgrade["manifest"] = manifest
	f.evidence.Upgrade["transition_pause"] = pause
	f.evidence.Upgrade["process"] = upgradedProcess
	f.evidence.Upgrade["buildinfo"] = upgradedBuild
	f.evidence.Upgrade["binary_sha256"] = upgradedBinaryHash
	f.evidence.Upgrade["historical_query_at_baseline_time"] = historicalAfterUpgrade
	f.evidence.Upgrade["head_query_at_baseline_time"] = headAfterUpgrade
	f.evidence.Upgrade["post_upgrade_query"] = postFixed
	f.requireCheckpoint()

	beforeReject := readRealPrometheusState(t, f, plan.ID, upgradedPID, &postAt)
	downgradeAfter, _ := json.Marshal(upgradeChange{NewVersion: "2.55.0"})
	_, downgradeErr := f.service.Create(ctx, ChangePlanRequest{
		Kind: models.ChangeKindUpgrade, ResourceType: "component", ResourceID: f.component.ID,
		Reason: "direct downgrade must reject", After: downgradeAfter,
	}, actor)
	if !errors.Is(downgradeErr, ErrPrometheusMigrationAdmission) {
		t.Fatalf("direct downgrade returned %v, want Prometheus migration rejection", downgradeErr)
	}
	afterDowngradeReject := readRealPrometheusState(t, f, plan.ID, upgradedPID, &postAt)
	if !equalRealPrometheusState(beforeReject, afterDowngradeReject) {
		t.Fatalf("direct downgrade rejection changed owned runtime/state: before=%+v after=%+v", beforeReject, afterDowngradeReject)
	}
	f.evidence.Rejections["direct_downgrade"] = map[string]any{"error": downgradeErr.Error(), "before": beforeReject, "after": afterDowngradeReject}
	f.requireCheckpoint()

	originalArchiveHash, _, err := realPrometheusFileSHA256(backup.Path)
	if err != nil || originalArchiveHash != manifest.SHA256 {
		t.Fatalf("paired backup hash before corruption=%s, want %s: %v", originalArchiveHash, manifest.SHA256, err)
	}
	offset, originalByte, err := flipRealPrometheusArchiveByte(backup.Path)
	if err != nil {
		t.Fatal(err)
	}
	restoredArtifact := false
	defer func() {
		if !restoredArtifact {
			outcome := captureRealPrometheusEmergencyRestoration(
				func() error { return restoreRealPrometheusArchiveByte(backup.Path, offset, originalByte) },
				func() (string, error) {
					hash, _, err := realPrometheusFileSHA256(backup.Path)
					return hash, err
				},
				originalArchiveHash,
			)
			f.evidence.Rejections["corrupt_backup_emergency_restoration"] = outcome
			if err := f.checkpoint(); err != nil {
				t.Errorf("checkpoint emergency corrupt-backup restoration: %v", err)
			}
			if outcome["restored"] != true {
				t.Errorf("restore temporarily corrupted owned backup: %v", outcome["error"])
			}
		}
	}()
	if err := f.verifyUnitOwnership(); err != nil {
		t.Fatalf("owned unit preflight before corrupt-backup rejection: %v", err)
	}
	_, corruptErr := f.service.Rollback(ctx, plan.ID, actor)
	if !errors.Is(corruptErr, ErrPrometheusRecoveryAdmission) {
		t.Fatalf("corrupt paired rollback returned %v, want recovery admission rejection", corruptErr)
	}
	afterCorruptReject := readRealPrometheusState(t, f, plan.ID, upgradedPID, &postAt)
	if !equalRealPrometheusState(beforeReject, afterCorruptReject) {
		t.Fatalf("corrupt rollback rejection changed owned runtime/state: before=%+v after=%+v", beforeReject, afterCorruptReject)
	}
	if err := restoreRealPrometheusArchiveByte(backup.Path, offset, originalByte); err != nil {
		t.Fatal(err)
	}
	restoredArtifact = true
	restoredArchiveHash, _, err := realPrometheusFileSHA256(backup.Path)
	if err != nil || restoredArchiveHash != originalArchiveHash {
		t.Fatalf("owned backup restoration failed: hash=%s want=%s err=%v", restoredArchiveHash, originalArchiveHash, err)
	}
	f.evidence.Rejections["corrupt_backup"] = map[string]any{
		"error": corruptErr.Error(), "before": beforeReject, "after": afterCorruptReject,
		"corruption_offset": offset, "artifact_restored": true, "restored_sha256": restoredArchiveHash,
	}
	f.requireCheckpoint()

	f.metric.setPhase("disabled")
	if err := f.verifyUnitOwnership(); err != nil {
		t.Fatalf("owned unit preflight before paired rollback: %v", err)
	}
	rolledBack, err := f.service.Rollback(ctx, plan.ID, actor)
	if err != nil {
		t.Fatalf("execute real paired rollback: %v", err)
	}
	if rolledBack.Status != models.ChangeStatusRolledBack {
		t.Fatalf("paired rollback plan status=%s", rolledBack.Status)
	}
	if rolledBack.OperationID == nil || *rolledBack.OperationID != operation.ID {
		t.Fatalf("rolled-back plan lost forward operation association: plan=%v upgrade=%d", rolledBack.OperationID, operation.ID)
	}
	var forwardOperationAfterRollback models.Operation
	if err := f.db.First(&forwardOperationAfterRollback, *rolledBack.OperationID).Error; err != nil {
		t.Fatal(err)
	}
	if forwardOperationAfterRollback.ID != operation.ID || forwardOperationAfterRollback.Type != "change-upgrade" ||
		forwardOperationAfterRollback.Status != "success" || forwardOperationAfterRollback.ComponentID == nil ||
		*forwardOperationAfterRollback.ComponentID != f.component.ID {
		t.Fatalf("persisted forward operation after rollback is invalid: %+v", forwardOperationAfterRollback)
	}
	var persistedRollbackEvidence struct {
		Manual     bool      `json:"manual"`
		BackupID   uint      `json:"backup_id"`
		RestoredAt time.Time `json:"restored_at"`
	}
	if err := json.Unmarshal(rolledBack.RollbackEvidence, &persistedRollbackEvidence); err != nil {
		t.Fatalf("decode persisted rollback evidence: %v", err)
	}
	if !persistedRollbackEvidence.Manual || persistedRollbackEvidence.BackupID != backup.ID ||
		persistedRollbackEvidence.RestoredAt.IsZero() || rolledBack.RolledBackAt == nil || rolledBack.RolledBackAt.IsZero() {
		t.Fatalf("persisted rollback evidence is incomplete: plan=%+v evidence=%+v", rolledBack, persistedRollbackEvidence)
	}
	restoredPID, err := f.mainPID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if restoredPID == upgradedPID {
		t.Fatalf("paired rollback retained upgraded PID %d", upgradedPID)
	}
	restoredProcess, err := f.processEvidence(restoredPID)
	if err != nil {
		t.Fatal(err)
	}
	restoredVersion, restoredBuild, err := f.buildVersion(ctx)
	if err != nil || restoredVersion != "2.55.0" {
		t.Fatalf("restored build version=%q, want 2.55.0: %v", restoredVersion, err)
	}
	restoredBinaryHash, _, err := realPrometheusFileSHA256(f.binaryPath)
	if err != nil || restoredBinaryHash != baselineBinaryHash {
		t.Fatalf("restored binary hash=%s, want baseline %s: %v", restoredBinaryHash, baselineBinaryHash, err)
	}
	restoredConfigHash, _, err := realPrometheusFileSHA256(f.configPath)
	if err != nil || restoredConfigHash != baselineConfigHash {
		t.Fatalf("restored config hash=%s, want baseline %s: %v", restoredConfigHash, baselineConfigHash, err)
	}
	restoredHistorical, err := f.query(ctx, `plc_historical{phase="baseline"}`, &historicalTime)
	if err != nil || !equalRealPrometheusSamples(historical, restoredHistorical) {
		t.Fatalf("restored historical query mismatch: %+v %v", restoredHistorical, err)
	}
	restoredHead, err := f.query(ctx, `plc_head{phase="baseline"}`, &headAt)
	if err != nil || !equalRealPrometheusSamples(headFixed, restoredHead) {
		t.Fatalf("restored head query mismatch: %+v %v", restoredHead, err)
	}
	excludedPost, err := f.query(ctx, `plc_post_upgrade{phase="after_upgrade"}`, &postAt)
	if err != nil || len(excludedPost) != 0 {
		t.Fatalf("post-backup series survived paired restore: %+v %v", excludedPost, err)
	}
	if _, err := os.Stat(postSentinel); !os.IsNotExist(err) {
		t.Fatalf("post-backup data sentinel survived paired restore: %v", err)
	}
	var restoredComponent models.Component
	var restoredPause ComponentBackupPause
	var checkpoint ComponentRestoreCheckpoint
	if err := f.db.First(&restoredComponent, f.component.ID).Error; err != nil || restoredComponent.Version != "2.55.0" || restoredComponent.Status != models.StatusRunning {
		t.Fatalf("restored component metadata invalid: %+v %v", restoredComponent, err)
	}
	if err := f.db.First(&restoredPause, "component_id = ?", f.component.ID).Error; err != nil || restoredPause.Status != "resumed" || restoredPause.VersionTransitionPlanID != 0 {
		t.Fatalf("rollback transition barrier not resumed: %+v %v", restoredPause, err)
	}
	if err := f.db.Where("component_id = ?", f.component.ID).Order("id DESC").First(&checkpoint).Error; err != nil || checkpoint.Status != "applied" {
		t.Fatalf("restore checkpoint not applied: %+v %v", checkpoint, err)
	}
	f.evidence.Rollback["plan"] = rolledBack
	f.evidence.Rollback["forward_operation_after_rollback"] = forwardOperationAfterRollback
	f.evidence.Rollback["transition_pause"] = restoredPause
	f.evidence.Rollback["checkpoint"] = checkpoint
	f.evidence.Rollback["process"] = restoredProcess
	f.evidence.Rollback["buildinfo"] = restoredBuild
	f.evidence.Rollback["binary_sha256"] = restoredBinaryHash
	f.evidence.Rollback["config_sha256"] = restoredConfigHash
	f.evidence.Rollback["historical_query"] = restoredHistorical
	f.evidence.Rollback["head_query"] = restoredHead
	f.evidence.Rollback["post_upgrade_query_at_saved_time"] = excludedPost
	f.evidence.Rollback["post_upgrade_sentinel_absent"] = true
	f.requireCheckpoint()

	f.metric.setPhase("wal")
	walSample, err := f.waitSample(ctx, `plc_wal_recovery{phase="wal"}`, "2551")
	if err != nil {
		t.Fatal(err)
	}
	walAt := walSample.Timestamp
	walBeforeCrash, err := f.query(ctx, `plc_wal_recovery{phase="wal"}`, &walAt)
	if err != nil {
		t.Fatal(err)
	}
	assertRealPrometheusSingleSample(t, walBeforeCrash, "plc_wal_recovery", "wal", "2551", walAt)
	f.metric.setPhase("disabled")
	crashedProcess, err := f.processEvidence(restoredPID)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(restoredPID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill exact owned Prometheus PID %d: %v", restoredPID, err)
	}
	if err := waitRealPrometheusPIDExit(restoredPID, 15*time.Second); err != nil {
		t.Fatal(err)
	}
	recoveredPID := f.startPrometheus(t)
	if recoveredPID == restoredPID {
		t.Fatalf("WAL restart reused crashed PID %d", restoredPID)
	}
	recoveredProcess, err := f.processEvidence(recoveredPID)
	if err != nil {
		t.Fatal(err)
	}
	walAfterCrash, err := f.query(ctx, `plc_wal_recovery{phase="wal"}`, &walAt)
	if err != nil || !equalRealPrometheusSamples(walBeforeCrash, walAfterCrash) {
		t.Fatalf("WAL sample not recovered: before=%+v after=%+v err=%v", walBeforeCrash, walAfterCrash, err)
	}
	walVersion, walBuild, err := f.buildVersion(ctx)
	if err != nil || walVersion != "2.55.0" {
		t.Fatalf("WAL restart version=%q, want 2.55.0: %v", walVersion, err)
	}
	f.evidence.WALRecovery["scope"] = "owned process crash recovery only; no host power-loss or physical-storage claim"
	f.evidence.WALRecovery["before_crash_process"] = crashedProcess
	f.evidence.WALRecovery["after_restart_process"] = recoveredProcess
	f.evidence.WALRecovery["sample_before_crash"] = walBeforeCrash
	f.evidence.WALRecovery["sample_after_restart"] = walAfterCrash
	f.evidence.WALRecovery["buildinfo"] = walBuild
	f.requireCheckpoint()
}

type realPrometheusState struct {
	PID            int                    `json:"pid"`
	BinarySHA256   string                 `json:"binary_sha256"`
	ConfigSHA256   string                 `json:"config_sha256"`
	Component      models.Component       `json:"component"`
	Plan           models.ChangePlan      `json:"plan"`
	PlanCount      int64                  `json:"plan_count"`
	BackupCount    int64                  `json:"backup_count"`
	OperationCount int64                  `json:"operation_count"`
	PostQuery      []realPrometheusSample `json:"post_query"`
	Sentinel       string                 `json:"sentinel"`
}

func readRealPrometheusState(t *testing.T, f *realPrometheusFixture, planID uint, wantPID int, evaluation *float64) realPrometheusState {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pid, err := f.mainPID(ctx)
	if err != nil || pid != wantPID {
		t.Fatalf("owned PID changed: got %d want %d err=%v", pid, wantPID, err)
	}
	if err := f.validateOwnedPID(pid); err != nil {
		t.Fatal(err)
	}
	binaryHash, _, err := realPrometheusFileSHA256(f.binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	configHash, _, err := realPrometheusFileSHA256(f.configPath)
	if err != nil {
		t.Fatal(err)
	}
	var state realPrometheusState
	state.PID, state.BinarySHA256, state.ConfigSHA256 = pid, binaryHash, configHash
	if err := f.db.First(&state.Component, f.component.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.First(&state.Plan, planID).Error; err != nil {
		t.Fatal(err)
	}
	for model, destination := range map[any]*int64{&models.ChangePlan{}: &state.PlanCount, &models.Backup{}: &state.BackupCount, &models.Operation{}: &state.OperationCount} {
		if err := f.db.Model(model).Count(destination).Error; err != nil {
			t.Fatal(err)
		}
	}
	state.PostQuery, err = f.query(ctx, `plc_post_upgrade{phase="after_upgrade"}`, evaluation)
	if err != nil {
		t.Fatal(err)
	}
	sentinel, err := os.ReadFile(filepath.Join(f.dataPath, "post-upgrade-owned-sentinel"))
	if err != nil {
		t.Fatal(err)
	}
	state.Sentinel = string(sentinel)
	return state
}

func equalRealPrometheusState(left, right realPrometheusState) bool {
	left.Component.UpdatedAt, right.Component.UpdatedAt = time.Time{}, time.Time{}
	left.Plan.UpdatedAt, right.Plan.UpdatedAt = time.Time{}, time.Time{}
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	return string(leftJSON) == string(rightJSON)
}

func assertRealPrometheusSingleSample(t *testing.T, samples []realPrometheusSample, name, phase, value string, timestamp float64) {
	t.Helper()
	if len(samples) != 1 {
		t.Fatalf("%s samples=%d, want 1: %+v", name, len(samples), samples)
	}
	sample := samples[0]
	if sample.Metric["__name__"] != name || sample.Metric["phase"] != phase || sample.Value != value || sample.Timestamp != timestamp {
		t.Fatalf("unexpected %s sample: %+v; want phase=%s value=%s timestamp=%.3f", name, sample, phase, value, timestamp)
	}
}
