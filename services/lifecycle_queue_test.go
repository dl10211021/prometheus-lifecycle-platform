package services

import (
	"context"
	"errors"
	"github.com/promlifecycle/platform/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"path/filepath"
	"testing"
)

func lifecycleQueueDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "lifecycle.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.Component{}, &models.Operation{}, &models.ChangePlan{}, &models.ChangeBaseline{}, &models.ComponentOperationClaim{}, &models.HostOperationClaim{}, &models.DeploymentStack{}, &models.DeploymentStackStep{}, &ComponentBackupPause{}); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	return db
}
func TestComponentClaimsExcludeCompetingPlansAndSurviveCallerCancel(t *testing.T) {
	db := lifecycleQueueDB(t)
	request, cancel := context.WithCancel(context.Background())
	claimed, release, err := ClaimComponentOperation(request, db, 11, "plan-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ClaimComponentOperation(context.Background(), db, 11, "plan-b"); !errors.Is(err, ErrComponentBusy) {
		t.Fatalf("competing plan acquired claim: %v", err)
	}
	_, nested, err := ClaimComponentOperation(claimed, db, 11, "backup")
	if err != nil {
		t.Fatal(err)
	}
	nested()
	cancel()
	recovery := context.WithoutCancel(claimed)
	_, nested, err = ClaimComponentOperation(recovery, db, 11, "rollback")
	if err != nil {
		t.Fatal(err)
	}
	nested()
	release()
	_, release, err = ClaimComponentOperation(context.Background(), db, 11, "retry")
	if err != nil {
		t.Fatal(err)
	}
	release()
}
func TestReviewedPlanRejectsChangedRuntimeIdentity(t *testing.T) {
	db := lifecycleQueueDB(t)
	component := models.Component{Name: "original", DeployType: "target", Version: "1"}
	if err := db.Create(&component).Error; err != nil {
		t.Fatal(err)
	}
	plan := models.ChangePlan{Kind: models.ChangeKindRemove, ResourceID: component.ID}
	if err := db.Create(&plan).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.ChangeBaseline{PlanID: plan.ID, Fingerprint: componentFingerprint(&component)}).Error; err != nil {
		t.Fatal(err)
	}
	component.ResourceLimits = `{"memory":"1G"}`
	service := &ChangeService{db: db}
	if err := service.validateBaseline(context.Background(), &plan, &component); !errors.Is(err, ErrStaleChangePlan) {
		t.Fatalf("changed limits were not rejected: %v", err)
	}
}
func TestInterruptedStackRequiresRetryAndSkipsCompletedSteps(t *testing.T) {
	db := lifecycleQueueDB(t)
	stack := models.DeploymentStack{Name: "restart", Status: "running", Total: 2, CurrentIndex: 1, Steps: []models.DeploymentStackStep{{Position: 0, Type: "first", Status: "success", EncryptedRequest: "encrypted"}, {Position: 1, Type: "second", Status: "running", EncryptedRequest: "encrypted"}}}
	if err := db.Create(&stack).Error; err != nil {
		t.Fatal(err)
	}
	operation := models.Operation{Status: "running", Type: "deploy"}
	if err := db.Create(&operation).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := ClaimComponentOperation(context.Background(), db, 5, "deploy"); err != nil {
		t.Fatal(err)
	}
	if err := RecoverInterruptedLifecycle(db); err != nil {
		t.Fatal(err)
	}
	var calls []string
	service := NewDeploymentStackService(db, func(ctx context.Context, step *models.DeploymentStackStep) error {
		calls = append(calls, step.Type)
		return nil
	})
	service.execute(context.Background(), stack.ID)
	if len(calls) != 0 {
		t.Fatal("interrupted stack resumed without explicit retry")
	}
	if err := db.First(&operation, operation.ID).Error; err != nil {
		t.Fatal(err)
	}
	if operation.Status != "interrupted" {
		t.Fatalf("operation falsely completed: %s", operation.Status)
	}
	if err := service.Retry(stack.ID); err != nil {
		t.Fatal(err)
	}
	service.execute(context.Background(), stack.ID)
	if len(calls) != 1 || calls[0] != "second" {
		t.Fatalf("replayed completed stack step: %v", calls)
	}
	finished, err := service.Get(stack.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.Status != "success" || finished.CurrentIndex != 2 {
		t.Fatalf("stack failed to complete: %#v", finished)
	}
	_, release, err := ClaimComponentOperation(context.Background(), db, 5, "retry")
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestUnresolvedRestoreBlocksLifecycleButAllowsRecovery(t *testing.T) {
	db := lifecycleQueueDB(t)
	if err := db.Create(&ComponentBackupPause{ComponentID: 7, Status: "termination_unconfirmed"}).Error; err != nil {
		t.Fatal(err)
	}
	if _, release, err := ClaimComponentOperation(context.Background(), db, 7, "restart"); err == nil {
		release()
		t.Fatal("restart bypassed unresolved restore")
	}
	_, release, err := ClaimComponentOperation(context.Background(), db, 7, "restore-recovery")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if err := db.Model(&ComponentBackupPause{}).Where("component_id = ?", 7).Update("status", "resumed").Error; err != nil {
		t.Fatal(err)
	}
	_, release, err = ClaimComponentOperation(context.Background(), db, 7, "restart")
	if err != nil {
		t.Fatal(err)
	}
	release()
}
