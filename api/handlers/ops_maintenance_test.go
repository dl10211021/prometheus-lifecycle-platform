package handlers

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestListMaintenanceWindowsUsesAbsoluteTimeInNonUTCZone(t *testing.T) {
	originalLocation := time.Local
	time.Local = time.FixedZone("UTC+8", 8*60*60)
	defer func() { time.Local = originalLocation }()

	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "maintenance-timezone.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.MaintenanceWindow{}, &models.MaintenanceSilence{}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	window := models.MaintenanceWindow{
		Name: "timezone-window", StartAt: now.Add(-time.Minute), EndAt: now.Add(time.Hour),
	}
	if err := db.Create(&window).Error; err != nil {
		t.Fatal(err)
	}

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest("GET", "/api/v1/maintenance", nil)
	(&OpsHandler{db: db}).ListMaintenanceWindows(context)
	if recorder.Code != 200 || !strings.Contains(recorder.Body.String(), "timezone-window") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
