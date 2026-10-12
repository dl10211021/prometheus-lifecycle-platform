package handlers

import (
	"encoding/json"
	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func notificationAuditDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "notification.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.Cluster{}, &models.Host{}, &models.Component{}, &models.NotificationChannel{}, &models.AlertmanagerRoute{}, &models.AlertSilence{}, &models.MaintenanceWindow{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestRouteFailedPublicationRestoresImmutablePreviousValue(t *testing.T) {
	db := notificationAuditDB(t)
	cluster := models.Cluster{Name: "single-node"}
	db.Create(&cluster)
	channel := models.NotificationChannel{Name: "ops", Type: "webhook", Enabled: true, ClusterID: &cluster.ID}
	db.Create(&channel)
	old := models.AlertmanagerRoute{ClusterID: cluster.ID, Name: "original", Receiver: "ops", Match: `{"severity":"warning"}`, Enabled: true}
	if err := db.Create(&old).Error; err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPut, "/routes/1", strings.NewReader(`{"name":"changed","receiver":"ops","match":{"severity":"critical"},"enabled":false}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: strconv.Itoa(int(old.ID))}}
	NewAlertmanagerSettingsHandler(db, nil, t.TempDir()).UpdateAlertmanagerRoute(c)
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var restored models.AlertmanagerRoute
	if err := db.First(&restored, old.ID).Error; err != nil {
		t.Fatal(err)
	}
	if restored.Name != old.Name || restored.Match != old.Match || restored.Enabled != old.Enabled {
		t.Fatalf("failed route persisted mutated snapshot: %+v", restored)
	}
}

func TestSilenceV2ExpiredStateAndRefreshFailure(t *testing.T) {
	db := notificationAuditDB(t)
	cluster := models.Cluster{Name: "single-node"}
	db.Create(&cluster)
	var unavailable atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if unavailable.Load() {
			w.WriteHeader(503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"id":"expired-id","status":{"state":"expired"}}]`))
	}))
	defer server.Close()
	am := models.Component{ClusterID: cluster.ID, Name: "am", Type: models.ComponentAlertmanager, Status: models.StatusRunning, ExternalURL: server.URL}
	db.Create(&am)
	silence := models.AlertSilence{ClusterID: cluster.ID, ExternalID: "expired-id", Status: "active"}
	db.Create(&silence)
	handler := NewAlertSilenceHandler(db)
	response := serveHandlerRequest(handler.ListSilences, http.MethodGet, "/silences", "")
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	db.First(&silence, silence.ID)
	if silence.Status != "expired" {
		t.Fatalf("v2 state ignored: %s", silence.Status)
	}
	unavailable.Store(true)
	response = serveHandlerRequest(handler.ListSilences, http.MethodGet, "/silences", "")
	var payload struct {
		Stale        bool   `json:"stale"`
		RefreshError string `json:"refresh_error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.Stale || payload.RefreshError == "" {
		t.Fatalf("refresh failure hidden: %s", response.Body.String())
	}
}

func TestMaintenanceAdmissionRejectsGlobalAndDeletedScope(t *testing.T) {
	db := notificationAuditDB(t)
	for _, scope := range []string{"", `,"component_id":999`, `,"cluster_id":999`} {
		body := `{"name":"maintenance","start_at":"` + time.Now().Format(time.RFC3339) + `","end_at":"` + time.Now().Add(time.Hour).Format(time.RFC3339) + `"` + scope + `}`
		response := serveHandlerRequest((&OpsHandler{db: db}).CreateMaintenanceWindow, http.MethodPost, "/maintenance", body)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("unsafe scope admitted: %s", response.Body.String())
		}
	}
	var count int64
	db.Model(&models.MaintenanceWindow{}).Count(&count)
	if count != 0 {
		t.Fatalf("unsafe windows persisted: %d", count)
	}
}
