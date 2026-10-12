package services

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/promlifecycle/platform/models"
	"gorm.io/gorm"
)

func TestPrometheusTransitionBarrierMissingFinalRecords(t *testing.T) {
	for _, mode := range []string{"forward operation", "forward plan", "rollback operation", "rollback plan", "baseline"} {
		t.Run(mode, func(t *testing.T) {
			f, plan, log := newPrometheusForward(t)
			if err := os.WriteFile(filepath.Join(f.service.factory.BasePath(), "bin", "prometheus-2.55.0"), []byte("current-binary"), 0755); err != nil {
				t.Fatal(err)
			}
			var injected, baseline atomic.Bool
			f.db.Callback().Update().Before("gorm:update").Register("test:delete-final-record", func(tx *gorm.DB) {
				values, _ := tx.Statement.Dest.(map[string]interface{})
				clean := tx.Session(&gorm.Session{NewDB: true})
				if tx.Statement.Table == "change_baselines" {
					if mode == "baseline" && !injected.Swap(true) {
						tx.AddError(clean.Where("plan_id = ?", plan.ID).Delete(&models.ChangeBaseline{}).Error)
					}
					if strings.HasPrefix(mode, "rollback") && !baseline.Swap(true) {
						tx.AddError(errors.New("post-forward baseline failure"))
					}
				}
				if strings.Contains(mode, "operation") && tx.Statement.Table == "operations" {
					if _, ok := values["completed_at"]; ok && !injected.Swap(true) {
						tx.AddError(clean.Delete(tx.Statement.Model).Error)
					}
				}
				if strings.Contains(mode, "plan") && tx.Statement.Table == "change_plans" {
					status := values["status"]
					if (status == models.ChangeStatusVerified || status == models.ChangeStatusRolledBack) && !injected.Swap(true) {
						tx.AddError(clean.Delete(&models.ChangePlan{}, plan.ID).Error)
					}
				}
			})
			result, err := f.service.Execute(context.Background(), plan.ID, ChangeActor{Role: "system_admin"})
			if !injected.Load() {
				t.Fatal("final record deletion not reached")
			}
			var pause ComponentBackupPause
			if e := f.db.First(&pause, "component_id = ?", f.component.ID).Error; e != nil {
				t.Fatal(e)
			}
			if mode == "baseline" {
				if err == nil || result == nil || result.Status != models.ChangeStatusRolledBack {
					t.Errorf("missing baseline accepted as forward success: %+v %v", result, err)
				}
				if pause.VersionTransitionPlanID != 0 {
					t.Errorf("safe paired rollback did not release barrier: %+v", pause)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "blocked") {
					t.Errorf("missing final record cleared barrier without an error: %v", err)
				}
				if pause.VersionTransitionPlanID != plan.ID {
					t.Errorf("missing final record erased barrier: %+v", pause)
				}
				assertTransitionClosed(t, f)
			}
			expected := 1
			if mode == "baseline" || strings.HasPrefix(mode, "rollback") {
				expected = 2
			}
			if raw, _ := os.ReadFile(log); strings.Count(string(raw), "restart compatibility-fixture") != expected {
				t.Errorf("unexpected runtime compensation: %s", raw)
			}
		})
	}
}

type transitionFailureTransport struct{ cause error }

func (r transitionFailureTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, r.cause
}

func TestPrometheusTransitionBarrierRequiresPostflightRecord(t *testing.T) {
	for _, mode := range []string{"write failure", "missing plan", "verification and write failure"} {
		t.Run(mode, func(t *testing.T) {
			f, plan, log := newPrometheusForward(t)
			if err := os.WriteFile(filepath.Join(f.service.factory.BasePath(), "bin", "prometheus-2.55.0"), []byte("current-binary"), 0755); err != nil {
				t.Fatal(err)
			}
			cause := errors.New("postflight evidence persistence failed")
			verifyCause := errors.New("postflight health request failed")
			if mode == "verification and write failure" {
				f.service.client = &http.Client{Transport: transitionFailureTransport{cause: verifyCause}}
			}
			var injected atomic.Bool
			f.db.Callback().Update().Before("gorm:update").Register("test:postflight-record-failure", func(tx *gorm.DB) {
				values, _ := tx.Statement.Dest.(map[string]interface{})
				_, verification := values["verification"]
				_, status := values["status"]
				if tx.Statement.Table == "change_plans" && verification && !status && !injected.Swap(true) {
					if mode == "missing plan" {
						tx.AddError(tx.Session(&gorm.Session{NewDB: true}).Delete(&models.ChangePlan{}, plan.ID).Error)
					} else {
						tx.AddError(cause)
					}
				}
			})
			result, err := f.service.Execute(context.Background(), plan.ID, ChangeActor{Role: "system_admin"})
			if !injected.Load() || err == nil {
				t.Fatalf("postflight evidence loss accepted: injected=%v err=%v", injected.Load(), err)
			}
			if mode == "missing plan" {
				assertTransitionClosed(t, f)
			} else {
				if !errors.Is(err, cause) || result == nil || result.Status != models.ChangeStatusRolledBack {
					t.Errorf("postflight loss did not safely compensate with original cause: %v", err)
				}
				if mode == "verification and write failure" && !strings.Contains(err.Error(), verifyCause.Error()) {
					t.Errorf("verification failure discarded: %v", err)
				}
				_, release, e := ClaimComponentOperation(context.Background(), f.db, f.component.ID, "after-postflight-compensation")
				if e != nil {
					t.Error(e)
				}
				if release != nil {
					release()
				}
			}
			expected := 2
			if mode == "missing plan" {
				expected = 1
			}
			if raw, _ := os.ReadFile(log); strings.Count(string(raw), "restart compatibility-fixture") != expected {
				t.Errorf("postflight failure used wrong recovery path: %s", raw)
			}
		})
	}
}
