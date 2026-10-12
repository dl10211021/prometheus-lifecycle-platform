package services

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/promlifecycle/platform/models"
	"gorm.io/gorm"
)

func assertTransitionClosed(t *testing.T, f *compatibilityFixture) {
	t.Helper()
	if err := RecoverInterruptedLifecycle(f.db); err != nil {
		t.Fatal(err)
	}
	if err := RecoverInterruptedComponentRestores(context.Background(), f.db); err == nil {
		t.Error("startup accepted an unresolved version transition")
	}
	for _, kind := range []string{"later-operation", "restore-recovery"} {
		_, release, err := ClaimComponentOperation(context.Background(), f.db, f.component.ID, kind)
		if release != nil {
			release()
		}
		if err == nil {
			t.Errorf("external %s bypassed unresolved version transition", kind)
		}
	}
}

func TestPrometheusTransitionBarrierBeforeForwardMutation(t *testing.T) {
	f, plan, log := newPrometheusForward(t)
	cause := errors.New("transition barrier persistence unavailable")
	var injected atomic.Bool
	f.db.Callback().Update().Before("gorm:update").Register("test:barrier-create-fails", func(tx *gorm.DB) {
		if tx.Statement.Table == "component_backup_pauses" {
			if values, ok := tx.Statement.Dest.(map[string]interface{}); ok {
				if value, exists := values["version_transition_plan_id"]; exists && value != uint(0) {
					injected.Store(true)
					tx.AddError(cause)
				}
			}
		}
	})
	_, err := f.service.Execute(context.Background(), plan.ID, ChangeActor{Role: "system_admin"})
	if !injected.Load() || !errors.Is(err, cause) {
		t.Errorf("forward mutation did not require durable barrier: injected=%v error=%v", injected.Load(), err)
	}
	if raw, _ := os.ReadFile(log); len(raw) != 0 {
		t.Errorf("runtime mutated or compensated after failed barrier write: %s", raw)
	}
	var current models.Component
	if err := f.db.First(&current, f.component.ID).Error; err != nil {
		t.Fatal(err)
	}
	if current.Version != "2.55.0" {
		t.Errorf("metadata changed before barrier persisted: %s", current.Version)
	}
}

func TestPrometheusTransitionBarrierSurvivesQuarantineWriteFailure(t *testing.T) {
	f, plan, log := newPrometheusForward(t)
	var forward, quarantine atomic.Bool
	f.db.Callback().Update().Before("gorm:update").Register("test:forward-and-quarantine-write-fail", func(tx *gorm.DB) {
		if tx.Statement.Table == "component_backup_pauses" {
			if pause, ok := tx.Statement.Dest.(*ComponentBackupPause); ok && pause.Status == "termination_unconfirmed" {
				quarantine.Store(true)
				tx.AddError(errors.New("quarantine write unavailable"))
			}
		}
		if tx.Statement.Table == "change_baselines" && !forward.Swap(true) {
			var stored models.ChangePlan
			if err := tx.Session(&gorm.Session{NewDB: true}).First(&stored, plan.ID).Error; err != nil {
				tx.AddError(err)
				return
			}
			var backup models.Backup
			if stored.BackupID == nil {
				tx.AddError(errors.New("missing forward backup"))
				return
			}
			if err := tx.Session(&gorm.Session{NewDB: true}).First(&backup, *stored.BackupID).Error; err != nil {
				tx.AddError(err)
				return
			}
			if err := os.WriteFile(backup.Path, []byte("damaged after forward runtime mutation"), 0600); err != nil {
				tx.AddError(err)
				return
			}
			tx.AddError(errors.New("post-forward baseline failed"))
		}
	})
	_, err := f.service.Execute(context.Background(), plan.ID, ChangeActor{Role: "system_admin"})
	if !forward.Load() || !quarantine.Load() || !errors.Is(err, ErrPrometheusRecoveryAdmission) {
		t.Fatalf("failure boundary not reached: forward=%v quarantine=%v error=%v", forward.Load(), quarantine.Load(), err)
	}
	if raw, _ := os.ReadFile(log); strings.Count(string(raw), "restart compatibility-fixture") != 1 {
		t.Errorf("expected one forward mutation, no unsafe rollback: %s", raw)
	}
	assertTransitionClosed(t, f)
}

func TestPrometheusTransitionBarrierFinalization(t *testing.T) {
	for _, mode := range []string{"forward success", "rollback success", "forward clear failure", "rollback clear failure", "forward operation record failure", "forward plan record failure", "rollback operation record failure", "rollback plan record failure"} {
		t.Run(mode, func(t *testing.T) {
			f, plan, log := newPrometheusForward(t)
			rollback := strings.HasPrefix(mode, "rollback")
			if rollback {
				if err := os.WriteFile(filepath.Join(f.service.factory.BasePath(), "bin", "prometheus-2.55.0"), []byte("current-binary"), 0755); err != nil {
					t.Fatal(err)
				}
			}
			ctx, release, err := ClaimComponentVersionChange(context.Background(), f.db, f.component.ID, "transition-test")
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			var baseline, fault, clearing atomic.Bool
			cause := errors.New("injected final transition persistence failure")
			f.db.Callback().Update().Before("gorm:update").Register("test:transition-finalization", func(tx *gorm.DB) {
				values, _ := tx.Statement.Dest.(map[string]interface{})
				if rollback && tx.Statement.Table == "change_baselines" && !baseline.Swap(true) {
					tx.AddError(errors.New("forward baseline failure"))
					return
				}
				if tx.Statement.Table == "component_backup_pauses" {
					if value, exists := values["version_transition_plan_id"]; exists && value == uint(0) {
						clearing.Store(true)
						var pause ComponentBackupPause
						if err := tx.Session(&gorm.Session{NewDB: true}).First(&pause, "component_id = ?", f.component.ID).Error; err != nil {
							t.Error(err)
						}
						if pause.Status != "resumed" || pause.VersionTransitionPlanID != plan.ID {
							t.Errorf("internal restore/resume erased barrier before outer completion: %+v", pause)
						}
						if err := RecoverInterruptedComponentRestores(context.Background(), f.db); err == nil {
							t.Error("startup bypassed the completion-to-clear window")
						}
						_, nestedRelease, nestedErr := ClaimComponentOperation(ctx, f.db, f.component.ID, "restore")
						if nestedErr != nil {
							t.Errorf("valid reentrant restore rejected: %v", nestedErr)
						}
						if nestedRelease != nil {
							nestedRelease()
						}
						if strings.Contains(mode, "clear failure") {
							fault.Store(true)
							tx.AddError(cause)
						}
					}
				}
				if strings.Contains(mode, "operation record failure") && tx.Statement.Table == "operations" {
					if _, ok := values["completed_at"]; ok && !fault.Swap(true) {
						tx.AddError(cause)
					}
				}
				if strings.Contains(mode, "plan record failure") && tx.Statement.Table == "change_plans" {
					status := values["status"]
					if (status == models.ChangeStatusVerified || status == models.ChangeStatusRolledBack) && !fault.Swap(true) {
						tx.AddError(cause)
					}
				}
			})
			result, err := f.service.Execute(ctx, plan.ID, ChangeActor{Role: "system_admin"})
			release()
			failure := strings.Contains(mode, "failure")
			if failure {
				if !fault.Load() || !errors.Is(err, cause) {
					t.Fatalf("finalization failure missing: fault=%v err=%v", fault.Load(), err)
				}
				if !strings.Contains(err.Error(), "blocked") {
					t.Errorf("missing API blocked-state diagnosis: %v", err)
				}
				var stored models.ChangePlan
				if e := f.db.First(&stored, plan.ID).Error; e != nil {
					t.Fatal(e)
				}
				if stored.Status != models.ChangeStatusFailed {
					t.Errorf("failed release still presented as successful: %s", stored.Status)
				}
				if strings.Contains(mode, "record failure") && clearing.Load() {
					t.Error("barrier clear attempted after final record failure")
				}
				assertTransitionClosed(t, f)
			} else {
				if !clearing.Load() || (!rollback && err != nil) || (rollback && (err == nil || result == nil || result.Status != models.ChangeStatusRolledBack)) {
					t.Fatalf("safe completion failed: %+v %v", result, err)
				}
				if e := RecoverInterruptedComponentRestores(context.Background(), f.db); e != nil {
					t.Fatal(e)
				}
				_, next, e := ClaimComponentOperation(context.Background(), f.db, f.component.ID, "after-safe-completion")
				if e != nil {
					t.Fatal(e)
				}
				next()
			}
			expected := 1
			if rollback {
				expected = 2
			}
			if raw, _ := os.ReadFile(log); strings.Count(string(raw), "restart compatibility-fixture") != expected {
				t.Errorf("unexpected mutation/compensation after finalization: %s", raw)
			}
		})
	}
}

func TestPrometheusTransitionBarrierPreservesPauseStateAndClaims(t *testing.T) {
	for _, running := range []bool{true, false} {
		t.Run(fmt.Sprint(running), func(t *testing.T) {
			f, plan, _ := newPrometheusForward(t)
			ctx, release, err := ClaimComponentOperation(context.Background(), f.db, f.component.ID, "owner")
			if err != nil {
				t.Fatal(err)
			}
			if err := f.db.Save(&ComponentBackupPause{ComponentID: f.component.ID, Status: "resumed", VersionTransitionPlanID: plan.ID, WasRunning: !running}).Error; err != nil {
				t.Fatal(err)
			}
			f.service.backups.runnerFactory = func(*models.Component) (hostCommandRunner, error) {
				return backupControlRunner{inactive: !running}, nil
			}
			if err := f.service.backups.beginComponentPause(ctx, &f.component); err != nil {
				t.Fatal(err)
			}
			var pause ComponentBackupPause
			f.db.First(&pause, "component_id = ?", f.component.ID)
			if pause.VersionTransitionPlanID != plan.ID || pause.WasRunning != running || pause.Status != "paused" {
				t.Fatalf("pause overwrote owner or running state: %+v", pause)
			}
			if err := f.service.backups.resumeComponent(ctx, &f.component); err != nil {
				t.Fatal(err)
			}
			f.db.First(&pause, "component_id = ?", f.component.ID)
			if pause.VersionTransitionPlanID != plan.ID || pause.Status != "resumed" {
				t.Fatalf("resume erased transition: %+v", pause)
			}
			_, nested, err := ClaimComponentOperation(ctx, f.db, f.component.ID, "restore")
			if err != nil {
				t.Fatal(err)
			}
			nested()
			release()
			assertTransitionClosed(t, f)
		})
	}
}

func TestPrometheusTransitionBarrierMigratesExistingPauseRows(t *testing.T) {
	db := lifecycleQueueDB(t)
	if err := db.Exec("DROP TABLE component_backup_pauses").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TABLE component_backup_pauses (component_id integer PRIMARY KEY, status text, error text, was_running numeric, updated_at datetime)").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO component_backup_pauses (component_id,status,error,was_running) VALUES (41,'resumed','',1),(42,'paused','keep',0)").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&ComponentBackupPause{}); err != nil {
		t.Fatal(err)
	}
	var pauses []ComponentBackupPause
	if err := db.Order("component_id").Find(&pauses).Error; err != nil {
		t.Fatal(err)
	}
	if len(pauses) != 2 || pauses[0].VersionTransitionPlanID != 0 || pauses[1].VersionTransitionPlanID != 0 || !pauses[0].WasRunning || pauses[1].Status != "paused" || pauses[1].Error != "keep" {
		t.Fatalf("legacy pause rows changed: %+v", pauses)
	}
	var column struct {
		NotNull int    `gorm:"column:notnull"`
		Default string `gorm:"column:dflt_value"`
	}
	if err := db.Raw("SELECT [notnull],dflt_value FROM pragma_table_info('component_backup_pauses') WHERE name='version_transition_plan_id'").Scan(&column).Error; err != nil {
		t.Fatal(err)
	}
	if column.NotNull != 1 || column.Default != "0" {
		t.Fatalf("legacy schema lacks explicit non-null zero default: %+v", column)
	}
	_, release, err := ClaimComponentOperation(context.Background(), db, 41, "legacy-allowed")
	if err != nil {
		t.Fatal(err)
	}
	release()
	_, release, err = ClaimComponentOperation(context.Background(), db, 42, "legacy-paused")
	if release != nil {
		release()
	}
	if err == nil {
		t.Fatal("legacy pause bypassed")
	}
}

func TestPrometheusTransitionBarrierFinalAdmissionLeavesNoMarker(t *testing.T) {
	f, plan, log := newPrometheusForward(t)
	var loaded *models.Component
	var attempted atomic.Bool
	f.service.backups.runnerFactory = func(c *models.Component) (hostCommandRunner, error) { loaded = c; return backupControlRunner{}, nil }
	f.db.Callback().Create().After("gorm:create").Register("test:final-admission-drift", func(tx *gorm.DB) {
		if tx.Statement.Table == "operations" && loaded != nil {
			loaded.ImportedArgs = `["--unexpected"]`
		}
	})
	f.db.Callback().Update().Before("gorm:update").Register("test:unexpected-barrier", func(tx *gorm.DB) {
		if tx.Statement.Table == "component_backup_pauses" {
			if values, ok := tx.Statement.Dest.(map[string]interface{}); ok {
				if _, ok := values["version_transition_plan_id"]; ok {
					attempted.Store(true)
				}
			}
		}
	})
	_, err := f.service.Execute(context.Background(), plan.ID, ChangeActor{Role: "system_admin"})
	if !errors.Is(err, ErrPrometheusMigrationAdmission) || attempted.Load() {
		t.Fatalf("final rejection attempted transition: %v attempted=%v", err, attempted.Load())
	}
	if raw, _ := os.ReadFile(log); len(raw) != 0 {
		t.Errorf("final rejection mutated or compensated runtime: %s", raw)
	}
	if err := RecoverInterruptedComponentRestores(context.Background(), f.db); err != nil {
		t.Fatal(err)
	}
	_, release, err := ClaimComponentOperation(context.Background(), f.db, f.component.ID, "after-final-rejection")
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestPrometheusTransitionBarrierClearRequiresOwnerAndSafePause(t *testing.T) {
	for _, mode := range []string{"wrong plan", "paused", "termination_unconfirmed"} {
		t.Run(mode, func(t *testing.T) {
			f, plan, _ := newPrometheusForward(t)
			pause := ComponentBackupPause{ComponentID: f.component.ID, Status: "resumed", VersionTransitionPlanID: plan.ID, WasRunning: true}
			if mode != "wrong plan" {
				pause.Status = mode
			}
			if err := f.db.Save(&pause).Error; err != nil {
				t.Fatal(err)
			}
			requested := *plan
			if mode == "wrong plan" {
				requested.ID++
			}
			if err := f.service.finishPrometheusTransition(&requested); err == nil {
				t.Fatal("zero-row clear accepted wrong owner or unsafe pause")
			}
			assertTransitionClosed(t, f)
		})
	}
}
