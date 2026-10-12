package handlers

import (
	"context"
	"errors"
	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/services"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrometheusRedeployRejectsCurrentOldThanosDespiteFuturePlan(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "dependents.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err = db.AutoMigrate(&models.Component{}); err != nil {
		t.Fatal(err)
	}
	if err = db.Create(&models.Component{Name: "sidecar", Type: models.ComponentThanos, HostID: 7, Version: "0.36.1", Status: models.StatusRunning, DeployType: "binary"}).Error; err != nil {
		t.Fatal(err)
	}
	request := componentCreateRequest{Type: "prometheus", Version: "3.0.0", PlannedComponents: []string{"prometheus", "thanos"}, PlannedVersions: map[string]string{"prometheus": "3.0.0", "thanos": "0.39.2"}}
	var checks []deploymentPreflightCheck
	(&Handler{db: db}).preflightDependencies(&request, &models.Host{ID: 7}, &checks)
	blocked := false
	for _, check := range checks {
		if check.Blocking && check.Status == "fail" && strings.Contains(check.Message, "Thanos") {
			blocked = true
		}
	}
	if !blocked {
		t.Fatalf("later planned sidecar update bypassed existing incompatibility: %+v", checks)
	}
}

func TestPrometheusVersionRequestRetainsHostClaimAcrossNestedService(t *testing.T) {
	for _, kind := range []string{"upgrade", "rollback"} {
		t.Run(kind, func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "claims.db")), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			sqlDB, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = sqlDB.Close() })
			if err := db.AutoMigrate(&models.Component{}, &models.ComponentOperationClaim{}, &models.HostOperationClaim{}, &services.ComponentBackupPause{}); err != nil {
				t.Fatal(err)
			}
			component := models.Component{Name: "prom", Type: models.ComponentPrometheus, HostID: 7, Status: models.StatusRunning}
			if err := db.Create(&component).Error; err != nil {
				t.Fatal(err)
			}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/", nil)
			release, ok := (&Handler{db: db}).claimComponentRequest(c, component.ID, kind)
			if !ok {
				t.Fatal("request claim rejected")
			}
			defer release()
			_, nested, err := services.ClaimComponentVersionChange(c.Request.Context(), db, component.ID, "change-execute")
			if err != nil {
				t.Fatal(err)
			}
			nested()
			_, competing, err := services.ClaimHostOperation(context.Background(), db, 7, "deploy")
			if competing != nil {
				competing()
			}
			if !errors.Is(err, services.ErrComponentBusy) {
				t.Fatalf("nested release lost outer host exclusion: %v", err)
			}
			release()
			_, after, err := services.ClaimHostOperation(context.Background(), db, 7, "deploy")
			if err != nil {
				t.Fatalf("request leaked host claim: %v", err)
			}
			after()
		})
	}
}
