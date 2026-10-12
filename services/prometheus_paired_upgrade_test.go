package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/promlifecycle/platform/models"
	"gorm.io/gorm"
)

type pairedCaptureFailureRunner struct {
	next hostCommandRunner
}

func (r pairedCaptureFailureRunner) Run(ctx context.Context, command string) (string, error) {
	if strings.Contains(command, "# promlifecycle:recovery-point-runtime") {
		return "", errors.New("injected paired capture failure")
	}
	return r.next.Run(ctx, command)
}

func preparePairedUpgradePlan(t *testing.T, f *pointFixture, paired bool) *ChangeService {
	t.Helper()
	if err := f.db.AutoMigrate(&models.ChangeBaseline{}, &models.Operation{}, &models.PrometheusRestoreTransaction{}); err != nil {
		t.Fatal(err)
	}
	after, err := json.Marshal(upgradeChange{NewVersion: "3.1.0", PairedRecovery: paired})
	if err != nil {
		t.Fatal(err)
	}
	updates := map[string]any{
		"after":                    after,
		"status":                   models.ChangeStatusDraft,
		"backup_id":                nil,
		"paired_recovery_point_id": nil,
	}
	if err := f.db.Model(&models.ChangePlan{}).Where("id = ?", f.plan.ID).Updates(updates).Error; err != nil {
		t.Fatal(err)
	}
	f.plan.After = after
	f.plan.Status = models.ChangeStatusDraft
	f.plan.BackupID = nil
	f.plan.PairedRecoveryPointID = nil
	if err := f.db.Create(&models.ChangeBaseline{PlanID: f.plan.ID, Fingerprint: componentFingerprint(f.c)}).Error; err != nil {
		t.Fatal(err)
	}
	service := NewChangeService(f.db, nil, nil, f.s, f.root)
	service.incidents = nil
	return service
}

func stopBeforePairedUpgradeApply(t *testing.T, db *gorm.DB) error {
	t.Helper()
	stop := errors.New("stop before paired upgrade apply")
	if err := db.Callback().Create().Before("gorm:create").Register("test:stop-before-paired-upgrade-apply", func(tx *gorm.DB) {
		if tx.Statement.Table == "operations" {
			tx.AddError(stop)
		}
	}); err != nil {
		t.Fatal(err)
	}
	return stop
}

func TestPairedRecoveryUpgradeCapturesBeforeOrdinaryBackupAndApply(t *testing.T) {
	f := newPointFixture(t)
	service := preparePairedUpgradePlan(t, f, true)
	stop := stopBeforePairedUpgradeApply(t, f.db)

	result, err := service.Execute(context.Background(), f.plan.ID, ChangeActor{Role: "system_admin"})
	if !errors.Is(err, stop) || result == nil {
		t.Fatalf("execution did not reach the pre-apply boundary: result=%+v err=%v", result, err)
	}
	var stored models.ChangePlan
	if err := f.db.First(&stored, f.plan.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.PairedRecoveryPointID == nil || stored.BackupID == nil {
		t.Fatalf("paired and ordinary recovery links were not both retained: %+v", stored)
	}
	var point models.PrometheusRecoveryPoint
	if err := f.db.First(&point, "plan_id = ?", f.plan.ID).Error; err != nil {
		t.Fatal(err)
	}
	if *stored.PairedRecoveryPointID != point.ID || *stored.BackupID == point.BackupID {
		t.Fatalf("wrong paired/ordinary identities: plan=%+v point=%+v", stored, point)
	}
	var ordinary models.Backup
	if err := f.db.First(&ordinary, *stored.BackupID).Error; err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ordinary.Name, fmt.Sprintf("change-%d-", f.plan.ID)) {
		t.Fatalf("automatic rollback did not retain the ordinary backup: %+v", ordinary)
	}
	var current models.Component
	if err := f.db.First(&current, f.c.ID).Error; err != nil {
		t.Fatal(err)
	}
	if current.Version != "2.55.0" {
		t.Fatalf("upgrade apply ran before operation boundary: version=%s", current.Version)
	}
	proof, err := f.s.VerifyPrometheusRecoveryPoint(context.Background(), f.plan.ID)
	if err != nil || proof == nil || proof.PointID != point.ID || proof.BackupID != point.BackupID {
		t.Fatalf("paired history did not survive ordinary backup binding: proof=%+v err=%v", proof, err)
	}
	props := pairedReferenceBlock(f.c.ServiceName, "FragmentPath="+f.runner.fragment+"\nEnvironmentFiles="+f.runner.environment+" (ignore_errors=no)\nExecStart={ path="+f.c.BinaryPath+" ; argv[]="+f.c.BinaryPath+" ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0 }\n")
	preview := &pairedPreviewRunner{fixture: f.runner, refs: pairedReferenceFrame(f.c.ServiceName, props)}
	f.s.runnerFactory = func(*models.Component) (hostCommandRunner, error) { return preview, nil }
	prepared, err := f.s.PreparePrometheusPairedRestoreTransaction(context.Background(), f.plan.ID)
	if err != nil || prepared == nil || prepared.PointID != point.ID || prepared.BackupID != point.BackupID {
		t.Fatalf("paired restore preparation did not survive ordinary backup binding: transaction=%+v err=%v", prepared, err)
	}
}

func TestPairedRecoveryUpgradeCaptureFailurePreventsApply(t *testing.T) {
	f := newPointFixture(t)
	service := preparePairedUpgradePlan(t, f, true)
	f.s.runnerFactory = func(*models.Component) (hostCommandRunner, error) {
		return pairedCaptureFailureRunner{next: f.runner}, nil
	}

	result, err := service.Execute(context.Background(), f.plan.ID, ChangeActor{Role: "system_admin"})
	if !errors.Is(err, ErrPrometheusRecoveryPoint) || result == nil {
		t.Fatalf("requested paired capture failure was not returned: result=%+v err=%v", result, err)
	}
	var stored models.ChangePlan
	if err := f.db.First(&stored, f.plan.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != models.ChangeStatusFailed || stored.PairedRecoveryPointID != nil || stored.BackupID != nil {
		t.Fatalf("capture failure left unsafe plan state: %+v", stored)
	}
	var operations int64
	if err := f.db.Model(&models.Operation{}).Count(&operations).Error; err != nil {
		t.Fatal(err)
	}
	if operations != 0 {
		t.Fatalf("capture failure reached apply operation: operations=%d", operations)
	}
	var current models.Component
	if err := f.db.First(&current, f.c.ID).Error; err != nil {
		t.Fatal(err)
	}
	if current.Version != "2.55.0" {
		t.Fatalf("capture failure mutated component: version=%s", current.Version)
	}
}

func TestUpgradeWithoutPairedRecoveryKeepsOrdinaryBackupPath(t *testing.T) {
	f := newPointFixture(t)
	service := preparePairedUpgradePlan(t, f, false)
	stop := stopBeforePairedUpgradeApply(t, f.db)

	if _, err := service.Execute(context.Background(), f.plan.ID, ChangeActor{Role: "system_admin"}); !errors.Is(err, stop) {
		t.Fatalf("execution did not reach the ordinary backup boundary: %v", err)
	}
	var stored models.ChangePlan
	if err := f.db.First(&stored, f.plan.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.PairedRecoveryPointID != nil || stored.BackupID == nil {
		t.Fatalf("option-absent upgrade changed recovery behavior: %+v", stored)
	}
	var points int64
	if err := f.db.Model(&models.PrometheusRecoveryPoint{}).Where("plan_id = ?", f.plan.ID).Count(&points).Error; err != nil {
		t.Fatal(err)
	}
	if points != 0 {
		t.Fatalf("option-absent upgrade captured paired history: %d", points)
	}
}

func TestPairedRecoveryUpgradeRejectsUnsupportedComponents(t *testing.T) {
	for _, tc := range []struct {
		name      string
		component models.Component
	}{
		{name: "docker Prometheus", component: models.Component{Type: models.ComponentPrometheus, DeployType: "docker"}},
		{name: "binary Thanos", component: models.Component{Type: models.ComponentThanos, DeployType: "binary"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			after := json.RawMessage(`{"new_version":"3.1.0","paired_recovery":true}`)
			_, _, _, _, _, err := (&ChangeService{}).prepare(context.Background(), ChangePlanRequest{Kind: models.ChangeKindUpgrade, After: after}, &tc.component)
			if err == nil || !strings.Contains(err.Error(), "paired recovery") {
				t.Fatalf("unsupported paired capture was not rejected: %v", err)
			}
		})
	}
}

func TestChangePlanRequestCannotAssignPairedRecoveryPoint(t *testing.T) {
	var request ChangePlanRequest
	if err := json.Unmarshal([]byte(`{"kind":"upgrade","resource_type":"component","resource_id":1,"reason":"upgrade","after":{"new_version":"3.1.0"},"paired_recovery_point_id":99}`), &request); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "paired_recovery_point_id") {
		t.Fatalf("trusted association entered the client request contract: %s", raw)
	}
}

func TestExplicitPairedAssociationMustMatchPointEvenWhenLegacyBackupMatches(t *testing.T) {
	f := newPointFixture(t)
	after, err := json.Marshal(upgradeChange{NewVersion: "3.1.0", PairedRecovery: true})
	if err != nil {
		t.Fatal(err)
	}
	f.plan.After = after
	if err := f.db.Model(f.plan).Update("after", after).Error; err != nil {
		t.Fatal(err)
	}
	point, err := f.s.CreatePrometheusRecoveryPoint(context.Background(), f.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	wrong := point.ID + 1
	if err := f.db.Model(&models.ChangePlan{}).Where("id = ?", f.plan.ID).Updates(map[string]any{
		"paired_recovery_point_id": wrong,
		"backup_id":                point.BackupID,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if proof, err := f.s.VerifyPrometheusRecoveryPoint(context.Background(), f.plan.ID); err == nil || proof != nil {
		t.Fatalf("wrong explicit association was accepted through legacy backup: proof=%+v err=%v", proof, err)
	}
}
