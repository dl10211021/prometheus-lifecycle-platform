package handlers

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func scheduledInspectionHandlerDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "scheduled-inspection-handler.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&models.ScheduledInspection{}, &models.ScheduledInspectionStatus{},
		&models.InspectionRun{}, &models.InspectionCheck{},
		&models.InspectionClaim{},
	); err != nil {
		t.Fatal(err)
	}
	return db
}

func inspectionRequestContext(method, path, body string) (*gin.Context, *httptest.ResponseRecorder) {
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(method, path, strings.NewReader(body))
	context.Request.Header.Set("Content-Type", "application/json")
	context.Set("username", "operator-a")
	return context, recorder
}

func TestScheduledInspectionCRUDValidationAndRunningProtection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := scheduledInspectionHandlerDB(t)
	handler := &Handler{db: db}

	context, recorder := inspectionRequestContext(http.MethodPost, "/api/v1/inspections/schedules", `{"name":"bad","cron":"60 * * * *","keep_count":30,"enabled":true}`)
	handler.CreateScheduledInspection(context)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid cron status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	context, recorder = inspectionRequestContext(http.MethodPost, "/api/v1/inspections/schedules", `{"name":"daily","cron":"0 2 * * *","keep_count":14,"enabled":false}`)
	handler.CreateScheduledInspection(context)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var schedule models.ScheduledInspection
	if err := db.First(&schedule).Error; err != nil {
		t.Fatal(err)
	}
	if schedule.Enabled || schedule.KeepCount != 14 || schedule.CreatedBy != "operator-a" {
		t.Fatalf("unexpected schedule: %#v", schedule)
	}

	context, recorder = inspectionRequestContext(http.MethodPut, "/api/v1/inspections/schedules/1", `{"cron":"*/5 * * * *","enabled":true}`)
	context.Params = gin.Params{{Key: "id", Value: "1"}}
	handler.UpdateScheduledInspection(context)
	if recorder.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if err := db.First(&schedule, schedule.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !schedule.Enabled || schedule.Cron != "*/5 * * * *" {
		t.Fatalf("schedule was not updated: %#v", schedule)
	}

	status := models.ScheduledInspectionStatus{ScheduledInspectionID: schedule.ID, LastStatus: "running"}
	if err := db.Create(&status).Error; err != nil {
		t.Fatal(err)
	}
	run := models.InspectionRun{ScheduledInspectionID: &schedule.ID, Status: "completed", Result: "healthy"}
	if err := db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	context, recorder = inspectionRequestContext(http.MethodDelete, "/api/v1/inspections/schedules/1", "")
	context.Params = gin.Params{{Key: "id", Value: "1"}}
	handler.DeleteScheduledInspection(context)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("running delete status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	if err := db.Model(&status).Update("last_status", "success").Error; err != nil {
		t.Fatal(err)
	}
	context, recorder = inspectionRequestContext(http.MethodDelete, "/api/v1/inspections/schedules/1", "")
	context.Params = gin.Params{{Key: "id", Value: "1"}}
	handler.DeleteScheduledInspection(context)
	if recorder.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if err := db.First(&run, run.ID).Error; err != nil {
		t.Fatal(err)
	}
	if run.ScheduledInspectionID != nil {
		t.Fatalf("deleted schedule remained attached to inspection run: %#v", run.ScheduledInspectionID)
	}
}
