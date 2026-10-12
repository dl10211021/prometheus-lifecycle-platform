package handlers

import (
	"bytes"
	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/services"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
)

func TestComponentEditRejectsDataRelocationWithoutChangingRecord(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "edit.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.Component{}, &models.ComponentOperationClaim{}, &services.ComponentBackupPause{}); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	component := models.Component{Name: "prom", Type: models.ComponentPrometheus, DeployType: "binary", Status: models.StatusRunning, InstallPath: "/opt/prom/original", DataPathOverride: "/data/prom-original"}
	if err := db.Create(&component).Error; err != nil {
		t.Fatal(err)
	}
	handler := &Handler{db: db}
	router := gin.New()
	router.PUT("/components/:id", handler.UpdateComponent)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/components/"+strconv.FormatUint(uint64(component.ID), 10), bytes.NewBufferString(`{"data_path_override":"/data/prom-new","apply_custom_args":true}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("unsafe relocation accepted: %d %s", response.Code, response.Body.String())
	}
	var persisted models.Component
	if err := db.First(&persisted, component.ID).Error; err != nil {
		t.Fatal(err)
	}
	if persisted.DataPathOverride != "/data/prom-original" || persisted.Status != models.StatusRunning {
		t.Fatalf("rejected migration mutated state: %#v", persisted)
	}
}
