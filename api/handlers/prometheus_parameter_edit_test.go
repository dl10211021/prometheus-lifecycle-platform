package handlers

import (
	"errors"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/services"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrometheusParameterVersionEditsRejectBeforeSave(t *testing.T) {
	for _, mode := range []string{"save-only", "apply-now", "best-practice"} {
		t.Run(mode, func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "parameters.db")), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			sqlDB, _ := db.DB()
			t.Cleanup(func() { sqlDB.Close() })
			if err := db.AutoMigrate(&models.Component{}, &models.Host{}, &models.ComponentOperationClaim{}, &models.HostOperationClaim{}, &models.ChangePlan{}, &models.ChangeBaseline{}, &services.ComponentBackupPause{}); err != nil {
				t.Fatal(err)
			}
			c := models.Component{Type: models.ComponentPrometheus, Name: "prom", Version: "3.1.0", DeployType: "binary", Status: models.StatusRunning}
			if mode == "best-practice" {
				c.ImportedArgs = `["--storage.tsdb.retention=45d"]`
				c.ResourceLimits = `{"cpu":"3"}`
			}
			if err := db.Create(&c).Error; err != nil {
				t.Fatal(err)
			}
			saves := 0
			// Stop at the first unexpected component write; no runtime dependencies are
			// substituted, and the failing baseline cannot escape into real host work.
			if err := db.Callback().Update().Before("gorm:update").Register("parameter_admission_write_guard", func(tx *gorm.DB) {
				if tx.Statement.Table == "components" {
					saves++
					tx.AddError(errors.New("unexpected component write before parameter admission"))
				}
			}); err != nil {
				t.Fatal(err)
			}
			h := &Handler{db: db}
			gin.SetMode(gin.TestMode)
			router := gin.New()
			router.PUT("/components/:id", h.UpdateComponent)
			router.POST("/components/:id/best-practice", h.ApplyComponentBestPractice)
			method, path, body := http.MethodPut, fmt.Sprintf("/components/%d", c.ID), `{"custom_args":"--storage.tsdb.retention=90d","apply_custom_args":false}`
			if mode == "apply-now" {
				body = `{"custom_args":"--storage.tsdb.retention=90d","apply_custom_args":true}`
			}
			if mode == "best-practice" {
				method = http.MethodPost
				path += "/best-practice"
				body = "{}"
			}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(method, path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "storage.tsdb.retention") || saves != 0 {
				t.Fatalf("code=%d saves=%d response=%s", rec.Code, saves, rec.Body.String())
			}
			var after models.Component
			if err := db.First(&after, c.ID).Error; err != nil {
				t.Fatal(err)
			}
			if after.CustomArgs != c.CustomArgs || after.ImportedArgs != c.ImportedArgs || after.ResourceLimits != c.ResourceLimits || after.Status != c.Status {
				t.Fatal("invalid edit changed saved component")
			}
		})
	}
}
