package services

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/promlifecycle/platform/models"
	"gorm.io/gorm"
)

// A missing reverse compatibility check must fail here before any health
// probing; the cancelled context keeps the old implementation isolated.
func TestPrometheusUpgradeRejectsExistingThanos(t *testing.T) {
	for _, version := range []string{"0.36.1", "", "unknown", "0.37.0-rc.0"} {
		t.Run(version, func(t *testing.T) {
			db := lifecycleQueueDB(t)
			component := models.Component{Name: "prom", Type: models.ComponentPrometheus, HostID: 7, Version: "2.55.0", Status: models.StatusRunning, DeployType: "binary"}
			if err := db.Create(&component).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Create(&models.Component{Name: "sidecar", Type: models.ComponentThanos, HostID: 7, Version: version, Status: models.StatusRunning, DeployType: "binary"}).Error; err != nil {
				t.Fatal(err)
			}
			service := NewChangeService(db, nil, nil, nil, t.TempDir())
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			evidence, risk, err := service.preflightUpgrade(ctx, &component, upgradeChange{NewVersion: "3.0.0"})
			if err == nil || !strings.Contains(err.Error(), "Thanos") {
				t.Fatalf("incompatible dependent was not blocked: %v", err)
			}
			if risk != models.ChangeRiskHigh {
				t.Fatalf("risk=%s", risk)
			}
			raw, _ := json.Marshal(evidence)
			if !strings.Contains(string(raw), "sidecar") || !strings.Contains(string(raw), "3.0.0") {
				t.Fatalf("missing actionable dependency evidence: %s", raw)
			}
		})
	}
}

func TestPrometheusUpgradeChecksAllRetainedDependents(t *testing.T) {
	for _, tt := range []struct {
		name, version, status, deploy string
		host                          uint
		block                         bool
	}{
		{"compatible", "0.37.0", "running", "binary", 7, false},
		{"stopped old instance", "0.36.1", "stopped", "binary", 7, true},
		{"failed old instance", "0.36.1", "failed", "binary", 7, true},
		{"removed instance", "0.36.1", "removed", "binary", 7, false},
		{"observed target", "0.36.1", "running", "target", 7, false},
		{"different host", "0.36.1", "running", "binary", 8, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			db := lifecycleQueueDB(t)
			// The first instance is compatible: scanning only First would miss the second.
			for _, dependent := range []models.Component{
				{Name: "first", Type: models.ComponentThanos, HostID: 7, Version: "0.39.2", Status: models.StatusRunning, DeployType: "binary"},
				{Name: "second", Type: models.ComponentThanos, HostID: tt.host, Version: tt.version, Status: models.ComponentStatus(tt.status), DeployType: tt.deploy},
			} {
				if err := db.Create(&dependent).Error; err != nil {
					t.Fatal(err)
				}
			}
			service := NewChangeService(db, nil, nil, nil, t.TempDir())
			component := models.Component{Type: models.ComponentPrometheus, HostID: 7, Version: "2.55.0", Status: models.StatusRunning, DeployType: "binary"}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			evidence, _, err := service.preflightUpgrade(ctx, &component, upgradeChange{NewVersion: "3.0.0"})
			if err == nil {
				t.Fatal("cancelled health probe unexpectedly succeeded")
			}
			if got := strings.Contains(err.Error(), "Thanos"); got != tt.block {
				t.Fatalf("block=%v want %v: %v", got, tt.block, err)
			}
			if !tt.block {
				raw, _ := json.Marshal(evidence)
				if !strings.Contains(string(raw), "compatible") {
					t.Fatalf("compatible dependency proof missing: %s", raw)
				}
			}
		})
	}
}

func TestPrometheusUpgradeExecutionRejectsDependentDrift(t *testing.T) {
	db := lifecycleQueueDB(t)
	component := models.Component{Name: "prom", Type: models.ComponentPrometheus, HostID: 7, Version: "2.55.0", Status: models.StatusRunning, DeployType: "binary"}
	if err := db.Create(&component).Error; err != nil {
		t.Fatal(err)
	}
	dependent := models.Component{Name: "sidecar", Type: models.ComponentThanos, HostID: 7, Version: "0.37.0", Status: models.StatusRunning, DeployType: "binary"}
	if err := db.Create(&dependent).Error; err != nil {
		t.Fatal(err)
	}
	plan := models.ChangePlan{Kind: models.ChangeKindUpgrade, ResourceID: component.ID, Status: models.ChangeStatusDraft, After: json.RawMessage(`{"new_version":"3.0.0"}`)}
	if err := db.Create(&plan).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.ChangeBaseline{PlanID: plan.ID, Fingerprint: componentFingerprint(&component)}).Error; err != nil {
		t.Fatal(err)
	}
	// Prometheus's own baseline did not change; its dependent changed after review.
	if err := db.Model(&dependent).Update("version", "0.36.1").Error; err != nil {
		t.Fatal(err)
	}
	service := NewChangeService(db, nil, nil, nil, t.TempDir())
	_, err := service.executeClaimed(context.Background(), &plan)
	if err == nil || !strings.Contains(err.Error(), "Thanos") {
		t.Fatalf("execution missed dependent drift: %v", err)
	}
	var stored models.ChangePlan
	if err = db.First(&stored, plan.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != models.ChangeStatusDraft {
		t.Fatalf("blocked plan was claimed/mutated: %s", stored.Status)
	}
	if err = db.First(&component, component.ID).Error; err != nil {
		t.Fatal(err)
	}
	if component.Version != "2.55.0" || component.Status != models.StatusRunning {
		t.Fatalf("blocked execution mutated component: %+v", component)
	}
}

func TestPrometheusUpgradeApplyRejectsBeforeMutation(t *testing.T) {
	db := lifecycleQueueDB(t)
	// Metadata-only mode makes the old implementation safe to exercise; the
	// compatibility gate must precede every mode's metadata/deployer mutation.
	component := models.Component{Name: "prom", Type: models.ComponentPrometheus, HostID: 7, Version: "2.55.0", Status: models.StatusRunning, DeployType: "target"}
	if err := db.Create(&component).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.Component{Name: "sidecar", Type: models.ComponentThanos, HostID: 7, Version: "0.36.1", Status: models.StatusRunning, DeployType: "binary"}).Error; err != nil {
		t.Fatal(err)
	}
	service := NewChangeService(db, nil, nil, nil, t.TempDir())
	plan := models.ChangePlan{Kind: models.ChangeKindUpgrade, After: json.RawMessage(`{"new_version":"3.0.0"}`)}
	err := service.apply(context.Background(), &plan, &component)
	if err == nil || !strings.Contains(err.Error(), "Thanos") {
		t.Fatalf("apply missed reverse guard: %v", err)
	}
	if err = db.First(&component, component.ID).Error; err != nil {
		t.Fatal(err)
	}
	if component.Version != "2.55.0" || component.Status != models.StatusRunning {
		t.Fatalf("blocked apply mutated component: %+v", component)
	}
}

func TestPrometheusUpgradeDependencyQueryFailureBlocks(t *testing.T) {
	db := lifecycleQueueDB(t)
	if err := db.Migrator().DropTable(&models.Component{}); err != nil {
		t.Fatal(err)
	}
	service := NewChangeService(db, nil, nil, nil, t.TempDir())
	component := models.Component{Type: models.ComponentPrometheus, HostID: 7, Version: "2.55.0", Status: models.StatusRunning, DeployType: "binary"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := service.preflightUpgrade(ctx, &component, upgradeChange{NewVersion: "3.0.0"})
	if err == nil || !strings.Contains(err.Error(), "Thanos") || !strings.Contains(err.Error(), "查询") {
		t.Fatalf("DB error treated as no dependents: %v", err)
	}
}

func TestPrometheusUpgradeLateRejectionDoesNotRollbackUntouchedService(t *testing.T) {
	// A managed Prometheus plan now needs a real paired recovery scope.
	f := newCompatibilityFixture(t, models.ComponentPrometheus, "2.55.0", "0.37.0")
	f.binary(t, "3.0.0")
	db, component := f.db, f.component
	dependent := f.companion
	before, _ := json.Marshal(prometheusBefore{Component: &component, Version: component.Version, Image: component.Image, ImportedArgsSHA256: importedArgsHash(component.ImportedArgs)})
	plan := models.ChangePlan{Kind: models.ChangeKindUpgrade, ResourceType: "component", Before: before, ResourceID: component.ID, Status: models.ChangeStatusDraft, After: json.RawMessage(`{"new_version":"3.0.0"}`)}
	if err := db.Create(&plan).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.ChangeBaseline{PlanID: plan.ID, Fingerprint: componentFingerprint(&component)}).Error; err != nil {
		t.Fatal(err)
	}
	changed := false
	if err := db.Callback().Create().After("gorm:create").Register("test:dependent-drift", func(tx *gorm.DB) {
		if tx.Statement.Table == "operations" && !changed {
			changed = true
			if err := tx.Session(&gorm.Session{NewDB: true}).Model(&dependent).Update("version", "0.36.1").Error; err != nil {
				tx.AddError(err)
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	service := f.service
	_, err := service.executeClaimed(context.Background(), &plan)
	if !changed {
		t.Fatalf("fixture never reached post-admission drift boundary: %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "Thanos") || strings.Contains(err.Error(), "回滚") {
		t.Fatalf("late admission rejection attempted compensation: %v", err)
	}
	if err = db.First(&component, component.ID).Error; err != nil {
		t.Fatal(err)
	}
	if component.Version != "2.55.0" || component.Status != models.StatusRunning {
		t.Fatalf("untouched component mutated: %+v", component)
	}
	var op models.Operation
	if err = db.First(&op).Error; err != nil {
		t.Fatal(err)
	}
	if op.Status != "failed" {
		t.Fatalf("rejection left operation %s", op.Status)
	}
}

func TestPrometheusUpgradeFinalCheckExcludesConcurrentVersionMutations(t *testing.T) {
	// A managed Prometheus plan now needs a real paired recovery scope.
	f := newCompatibilityFixture(t, models.ComponentPrometheus, "2.55.0", "0.37.0")
	f.binary(t, "3.0.0")
	db, component := f.db, f.component
	thanos := f.companion
	before, _ := json.Marshal(prometheusBefore{Component: &component, Version: component.Version, Image: component.Image, ImportedArgsSHA256: importedArgsHash(component.ImportedArgs)})
	plan := models.ChangePlan{Kind: models.ChangeKindUpgrade, ResourceType: "component", Before: before, ResourceID: component.ID, Status: models.ChangeStatusDraft, After: json.RawMessage(`{"new_version":"3.0.0"}`)}
	if err := db.Create(&plan).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.ChangeBaseline{PlanID: plan.ID, Fingerprint: componentFingerprint(&component)}).Error; err != nil {
		t.Fatal(err)
	}
	other := models.ChangePlan{Kind: models.ChangeKindUpgrade, ResourceID: thanos.ID, Status: models.ChangeStatusDraft, After: json.RawMessage(`{"new_version":"0.36.1"}`)}
	if err := db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	ready, proceed := make(chan struct{}), make(chan struct{})
	queries := 0
	if err := db.Callback().Query().After("gorm:query").Register("test:final-check-barrier", func(tx *gorm.DB) {
		if _, ok := tx.Statement.Dest.(*[]models.Component); ok && tx.Statement.Table == "components" {
			queries++
			if queries == 2 {
				close(ready)
				<-proceed
				tx.AddError(context.Canceled)
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	svc := f.service
	actor := ChangeActor{ID: 1, Username: "fixture", Role: "system_admin"}
	done := make(chan error, 1)
	go func() { _, err := svc.Execute(context.Background(), plan.ID, actor); done <- err }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("never reached final check: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("final check barrier timed out")
	}
	// Always unblock the worker before asserting to avoid a leaked test goroutine.
	_, release, deployErr := ClaimHostOperation(context.Background(), db, 7, "deploy")
	if release != nil {
		release()
	}
	_, executeErr := svc.Execute(context.Background(), other.ID, actor)
	_, rollbackErr := svc.Rollback(context.Background(), other.ID, actor)
	_, releaseOther, otherHostErr := ClaimHostOperation(context.Background(), db, 8, "deploy")
	if releaseOther != nil {
		releaseOther()
	}
	close(proceed)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("execution did not release barrier")
	}
	for name, err := range map[string]error{"deploy": deployErr, "other version execute": executeErr, "other version rollback": rollbackErr} {
		if !errors.Is(err, ErrComponentBusy) {
			t.Errorf("%s was not excluded: %v", name, err)
		}
	}
	if otherHostErr != nil {
		t.Errorf("unrelated host blocked: %v", otherHostErr)
	}
	_, release, err := ClaimHostOperation(context.Background(), db, 7, "after-failure")
	if err != nil {
		t.Fatalf("failed execution leaked host claim: %v", err)
	}
	release()
}
