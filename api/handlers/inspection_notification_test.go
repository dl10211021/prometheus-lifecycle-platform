package handlers

import (
	"encoding/json"
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

func TestInspectionNotificationPolicyValidationAndSave(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "inspection-policy.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.NotificationChannel{}, &models.InspectionNotificationPolicy{}, &models.InspectionNotificationState{}); err != nil {
		t.Fatal(err)
	}
	handler := &Handler{db: db}

	context, recorder := inspectionPolicyContext(http.MethodGet, "", "")
	handler.GetInspectionNotificationPolicy(context)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"consecutive_failures":2`) {
		t.Fatalf("default policy status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	context, recorder = inspectionPolicyContext(http.MethodPut, "", `{"enabled":true,"consecutive_failures":2,"notify_recovery":true,"channel_ids":[]}`)
	handler.UpdateInspectionNotificationPolicy(context)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("empty channels status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	channel := models.NotificationChannel{Name: "ops", Type: "webhook", Config: `{"url":"https://example.com/hook"}`, Enabled: true}
	if err := db.Create(&channel).Error; err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(inspectionNotificationPolicyInput{
		Enabled: true, ConsecutiveFailures: 3, NotifyRecovery: true,
		ChannelIDs: []uint{channel.ID, channel.ID},
	})
	context, recorder = inspectionPolicyContext(http.MethodPut, "", string(body))
	handler.UpdateInspectionNotificationPolicy(context)
	if recorder.Code != http.StatusOK {
		t.Fatalf("save policy status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var policy models.InspectionNotificationPolicy
	if err := db.First(&policy).Error; err != nil {
		t.Fatal(err)
	}
	if !policy.Enabled || policy.ConsecutiveFailures != 3 || len(policy.ChannelIDs) != 1 || policy.UpdatedBy != "system-admin" {
		t.Fatalf("unexpected policy: %#v", policy)
	}
	wechat := models.NotificationChannel{Name: "corporate", Type: "wechat", Config: `{"corp_id":"corp","agent_id":"1001"}`, Enabled: true}
	if err := db.Create(&wechat).Error; err != nil {
		t.Fatal(err)
	}
	body, _ = json.Marshal(inspectionNotificationPolicyInput{Enabled: true, ConsecutiveFailures: 2, NotifyRecovery: true, ChannelIDs: []uint{wechat.ID}})
	context, recorder = inspectionPolicyContext(http.MethodPut, "", string(body))
	handler.UpdateInspectionNotificationPolicy(context)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("corporate WeChat admitted for direct inspection: %s", recorder.Body.String())
	}
}

func TestDeleteComponentRecordRemovesInspectionNotificationState(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "component-delete.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&models.Host{}, &models.Cluster{}, &models.Component{}, &models.InspectionNotificationState{},
	); err != nil {
		t.Fatal(err)
	}
	host := models.Host{Name: "delete-state-host", IP: "192.0.2.10"}
	cluster := models.Cluster{Name: "single-node"}
	if err := db.Create(&host).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&cluster).Error; err != nil {
		t.Fatal(err)
	}
	component := models.Component{
		HostID: host.ID, ClusterID: cluster.ID, Name: "delete-state-component",
		Type: models.ComponentNodeExporter, Status: models.StatusRunning,
	}
	if err := db.Create(&component).Error; err != nil {
		t.Fatal(err)
	}
	state := models.InspectionNotificationState{
		ComponentID: component.ID, State: "alerting", LastStatus: "failed",
		ConsecutiveFailures: 2,
	}
	if err := db.Create(&state).Error; err != nil {
		t.Fatal(err)
	}

	if err := (&Handler{db: db}).deleteComponentRecord(&component); err != nil {
		t.Fatal(err)
	}
	var componentCount, stateCount int64
	if err := db.Model(&models.Component{}).Where("id = ?", component.ID).Count(&componentCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.InspectionNotificationState{}).Where("component_id = ?", component.ID).Count(&stateCount).Error; err != nil {
		t.Fatal(err)
	}
	if componentCount != 0 || stateCount != 0 {
		t.Fatalf("component=%d notification_state=%d, want both deleted", componentCount, stateCount)
	}
}

func inspectionPolicyContext(method, path, body string) (*gin.Context, *httptest.ResponseRecorder) {
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(method, "/api/v1/inspections/notification-policy"+path, strings.NewReader(body))
	context.Request.Header.Set("Content-Type", "application/json")
	context.Set("username", "system-admin")
	return context, recorder
}

func TestInspectionPolicyCanDisableRecoveryOnFirstSave(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "recovery-disabled.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.InspectionNotificationPolicy{}); err != nil {
		t.Fatal(err)
	}
	handler := &Handler{db: db}
	context, recorder := inspectionPolicyContext(http.MethodPut, "", `{"enabled":false,"consecutive_failures":2,"notify_recovery":false,"channel_ids":[]}`)
	handler.UpdateInspectionNotificationPolicy(context)
	if recorder.Code != http.StatusOK {
		t.Fatalf("save: %s", recorder.Body.String())
	}
	context, recorder = inspectionPolicyContext(http.MethodGet, "", "")
	handler.GetInspectionNotificationPolicy(context)
	var policy models.InspectionNotificationPolicy
	if err := json.Unmarshal(recorder.Body.Bytes(), &policy); err != nil {
		t.Fatal(err)
	}
	if policy.NotifyRecovery || policy.Enabled {
		t.Fatalf("explicit disabled behavior was reenabled: %+v", policy)
	}
}
