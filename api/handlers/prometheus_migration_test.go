package handlers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/services"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestPrometheusMigrationDirectAPIsRejectWithoutWrites(t *testing.T) {
	for _, mode := range []string{"upgrade downgrade", "rollback shortcut", "intermediate", "historical no snapshot"} {
		t.Run(mode, func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "migration.db")), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			sqlDB, _ := db.DB()
			t.Cleanup(func() { sqlDB.Close() })
			if err := db.AutoMigrate(&models.Component{}, &models.Host{}, &models.Cluster{}, &models.Operation{}, &models.Backup{}, &models.BackupManifest{}, &models.ChangePlan{}, &models.ChangeBaseline{}, &models.ComponentOperationClaim{}, &models.HostOperationClaim{}, &services.ComponentBackupPause{}); err != nil {
				t.Fatal(err)
			}
			version := "3.12.0"
			if mode == "intermediate" {
				version = "2.54.0"
			}
			component := models.Component{Type: models.ComponentPrometheus, Name: "prom", HostID: 7, Version: version, DeployType: "binary", Status: models.StatusRunning, ConfigPath: "/etc/prometheus.yml", DataPath: "/var/lib/prometheus"}
			if err := db.Create(&component).Error; err != nil {
				t.Fatal(err)
			}
			h := &Handler{db: db, basePath: t.TempDir()}
			gin.SetMode(gin.TestMode)
			router := gin.New()
			router.Use(func(c *gin.Context) {
				c.Set("user_id", uint(1))
				c.Set("username", "migration-tester")
				c.Set("role", "system_admin")
			})
			router.POST("/components/:id/upgrade", h.UpgradeComponent)
			router.POST("/components/:id/rollback", h.RollbackComponent)
			route := fmt.Sprintf("/components/%d/upgrade", component.ID)
			body := `{"new_version":"3.11.0","reason":"migration test"}`
			want := "requires_restore"
			if mode == "rollback shortcut" {
				route = fmt.Sprintf("/components/%d/rollback", component.ID)
				body = `{"target_version":"2.55.0","reason":"rollback test"}`
			}
			if mode == "intermediate" {
				body = `{"new_version":"3.0.0","reason":"staged migration"}`
				want = "2.55.x"
			}
			if mode == "historical no snapshot" {
				plan := models.ChangePlan{Kind: models.ChangeKindUpgrade, ResourceType: "component", ResourceID: component.ID, Status: models.ChangeStatusFailed, Before: []byte(`{"version":"2.55.0","image":"old"}`)}
				if err := db.Create(&plan).Error; err != nil {
					t.Fatal(err)
				}
				svc := services.NewChangeService(db, nil, nil, nil, t.TempDir())
				svcHandler := NewChangePlanHandler(svc)
				router.POST("/plans/:id/rollback", svcHandler.Rollback)
				route = fmt.Sprintf("/plans/%d/rollback", plan.ID)
				body = `{}`
				want = "snapshot"
			}
			counts := func() [3]int64 {
				var n [3]int64
				db.Model(&models.ChangePlan{}).Count(&n[0])
				db.Model(&models.Backup{}).Count(&n[1])
				db.Model(&models.Operation{}).Count(&n[2])
				return n
			}
			before := counts()
			request := httptest.NewRequest(http.MethodPost, route, strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code < 400 || !strings.Contains(response.Body.String(), want) {
				t.Fatalf("API admitted unsupported transition: %d %s", response.Code, response.Body.String())
			}
			if counts() != before {
				t.Error("rejection wrote plan/backup/operation")
			}
			var current models.Component
			db.First(&current, component.ID)
			if current.Version != version || current.Status != models.StatusRunning {
				t.Error("API mutated component")
			}
			_, release, err := services.ClaimComponentVersionChange(context.Background(), db, component.ID, "after-api-rejection")
			if err != nil {
				t.Fatal(err)
			}
			release()
		})
	}
}
