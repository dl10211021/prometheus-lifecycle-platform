package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func scheduledBackupHandlerTestSetup(t *testing.T) (*gorm.DB, *OpsHandler, models.Component) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "scheduled-backups.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.Component{}, &models.ScheduledBackup{}); err != nil {
		t.Fatal(err)
	}
	component := models.Component{
		ClusterID: 1, Name: "alertmanager", Type: models.ComponentAlertmanager,
		DeployType: "binary", ConfigPath: "/etc/alertmanager/alertmanager.yml",
		DataPath: "/var/lib/alertmanager",
	}
	if err := db.Create(&component).Error; err != nil {
		t.Fatal(err)
	}
	return db, &OpsHandler{db: db}, component
}

func TestCreateScheduledBackupRejectsInvalidCron(t *testing.T) {
	db, handler, component := scheduledBackupHandlerTestSetup(t)
	router := gin.New()
	router.POST("/backups/scheduled", handler.CreateScheduledBackup)
	payload, _ := json.Marshal(map[string]interface{}{
		"name": "invalid", "cron": "61 * * * *", "component_id": component.ID,
		"type": "config", "keep_count": 2, "enabled": true,
	})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/backups/scheduled", bytes.NewReader(payload)))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid cron status=%d body=%s", response.Code, response.Body.String())
	}
	var count int64
	if err := db.Model(&models.ScheduledBackup{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("invalid cron created %d schedule records", count)
	}
}

func TestUpdateScheduledBackupValidatesResultBeforeSaving(t *testing.T) {
	db, handler, component := scheduledBackupHandlerTestSetup(t)
	schedule := models.ScheduledBackup{
		Name: "valid", Cron: "* * * * *", ComponentID: &component.ID,
		Type: "config", KeepCount: 2, Enabled: true,
	}
	if err := db.Create(&schedule).Error; err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	router.PUT("/backups/scheduled/:id", handler.UpdateScheduledBackup)
	payload := []byte(`{"cron":"not-a-cron","keep_count":0}`)
	response := httptest.NewRecorder()
	path := "/backups/scheduled/" + strconv.FormatUint(uint64(schedule.ID), 10)
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPut, path, bytes.NewReader(payload)))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid update status=%d body=%s", response.Code, response.Body.String())
	}
	var stored models.ScheduledBackup
	if err := db.First(&stored, schedule.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Cron != schedule.Cron || stored.KeepCount != schedule.KeepCount {
		t.Fatalf("invalid update was persisted: cron=%q keep_count=%d", stored.Cron, stored.KeepCount)
	}
}

func TestDeleteScheduledBackupReturnsNotFoundForMissingRecord(t *testing.T) {
	_, handler, _ := scheduledBackupHandlerTestSetup(t)
	router := gin.New()
	router.DELETE("/backups/scheduled/:id", handler.DeleteScheduledBackup)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/backups/scheduled/999", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing schedule status=%d body=%s", response.Code, response.Body.String())
	}
}
