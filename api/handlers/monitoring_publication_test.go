package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/core/config"
	"github.com/promlifecycle/platform/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func monitoringHandlerDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "monitoring.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.Cluster{}, &models.Host{}, &models.Component{}, &models.AlertRule{}, &models.RecordingRule{}, &models.ScrapeConfigModel{}, &models.RemoteWriteModel{}, &models.Dashboard{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestExplicitDisabledMonitoringRecordsStayDisabled(t *testing.T) {
	db := monitoringHandlerDB(t)
	records := []interface{}{
		&models.AlertRule{ClusterID: 1, Name: "draft", Expression: "up == 0", Enabled: false},
		&models.RecordingRule{ClusterID: 1, Name: "draft:up", Expression: "up", Enabled: false},
		&models.ScrapeConfigModel{ClusterID: 1, Name: "draft", JobName: "draft", Targets: `["localhost:9999"]`, Enabled: false},
		&models.RemoteWriteModel{ClusterID: 1, Name: "draft", URL: "http://localhost:9999/write", Enabled: false},
		&models.Dashboard{ClusterID: 1, Name: "draft", Content: `{"title":"Draft"}`, Enabled: false},
	}
	for _, record := range records {
		if err := createMonitoring(db, record); err != nil {
			t.Fatal(err)
		}
		stored := reflect.New(reflect.TypeOf(record).Elem()).Interface()
		if err := db.First(stored, reflect.ValueOf(record).Elem().FieldByName("ID").Uint()).Error; err != nil {
			t.Fatal(err)
		}
		if reflect.ValueOf(stored).Elem().FieldByName("Enabled").Bool() {
			t.Fatalf("explicit false became enabled: %T", record)
		}
	}
	jobs, err := config.NewAutoConfig(db, t.TempDir()).GenerateScrapeConfigs(1)
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range jobs {
		if job.JobName == "draft" {
			t.Fatal("disabled scrape was published")
		}
	}
}

func TestRemoteWriteSecretOmissionPreservesAndEmptyClears(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := monitoringHandlerDB(t)
	item := models.RemoteWriteModel{ClusterID: 1, Name: "rw", URL: "http://example.test/write", BasicAuthUser: "user", BasicAuthPassword: "private-password", BearerToken: "private-token", Enabled: true}
	if err := createMonitoring(db, &item); err != nil {
		t.Fatal(err)
	}
	h := NewRemoteWriteHandler(db, nil, t.TempDir())
	router := gin.New()
	router.PUT("/remote/:id", h.Update)
	router.GET("/remote/:id", h.Get)
	update := func(body string) {
		req := httptest.NewRequest(http.MethodPut, "/remote/1", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		result := httptest.NewRecorder()
		router.ServeHTTP(result, req)
		if result.Code != http.StatusAccepted {
			t.Fatalf("update status=%d body=%s", result.Code, result.Body.String())
		}
	}
	update(`{"name":"renamed"}`)
	if err := db.First(&item, item.ID).Error; err != nil {
		t.Fatal(err)
	}
	if item.BasicAuthPassword != "private-password" || item.BearerToken != "private-token" {
		t.Fatal("omission erased credentials")
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/remote/1", nil))
	if strings.Contains(response.Body.String(), "private-password") || strings.Contains(response.Body.String(), "private-token") {
		t.Fatal("DTO leaked credentials")
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["password_configured"] != true {
		t.Fatal("configured password not exposed safely")
	}
	update(`{"basic_auth_password":"","bearer_token":""}`)
	if err := db.First(&item, item.ID).Error; err != nil {
		t.Fatal(err)
	}
	if item.BasicAuthPassword != "" || item.BearerToken != "" {
		t.Fatal("explicit empty did not clear credentials")
	}
}
