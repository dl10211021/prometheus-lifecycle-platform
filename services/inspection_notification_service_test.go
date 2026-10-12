package services

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/promlifecycle/platform/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestInspectionNotificationsThresholdDedupAndRecovery(t *testing.T) {
	t.Setenv("PROMLIFECYCLE_ALLOW_PRIVATE_FETCH", "1")
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "inspection-notifications.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&models.Host{}, &models.Cluster{}, &models.Component{},
		&models.NotificationChannel{}, &models.AlertNotification{},
		&models.InspectionNotificationPolicy{}, &models.InspectionNotificationState{},
		&models.InspectionRun{}, &models.InspectionCheck{}, &models.InspectionTransition{}, &models.InspectionDelivery{},
	); err != nil {
		t.Fatal(err)
	}
	var messageMu sync.Mutex
	messages := make([]string, 0, 2)
	webhook := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		buffer, _ := io.ReadAll(request.Body)
		messageMu.Lock()
		messages = append(messages, string(buffer))
		messageMu.Unlock()
		response.WriteHeader(http.StatusNoContent)
	}))
	defer webhook.Close()

	host := models.Host{Name: "host-a", IP: "192.0.2.80", OS: "linux", ConnectionType: "ssh"}
	cluster := models.Cluster{Name: "single-node"}
	if err := db.Create(&host).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&cluster).Error; err != nil {
		t.Fatal(err)
	}
	component := models.Component{HostID: host.ID, ClusterID: cluster.ID, Name: "prom-a", Type: models.ComponentPrometheus}
	if err := db.Create(&component).Error; err != nil {
		t.Fatal(err)
	}
	channel := models.NotificationChannel{Name: "inspection-webhook", Type: "webhook", Config: `{"url":"` + webhook.URL + `"}`, Enabled: true}
	if err := db.Create(&channel).Error; err != nil {
		t.Fatal(err)
	}
	policy := models.InspectionNotificationPolicy{Enabled: true, ConsecutiveFailures: 2, NotifyRecovery: true, ChannelIDs: []uint{channel.ID}}
	if err := db.Create(&policy).Error; err != nil {
		t.Fatal(err)
	}

	service := NewInspectionNotificationService(db)
	failed := models.InspectionCheck{ComponentID: component.ID, Component: component.Name, Status: "failed", Message: "endpoint unavailable"}
	passed := models.InspectionCheck{ComponentID: component.ID, Component: component.Name, Status: "passed", Message: "healthy"}
	process := func(id uint, check models.InspectionCheck) []InspectionNotificationEvent {
		run := models.InspectionRun{ID: id, Status: "completed"}
		if err := db.Create(&run).Error; err != nil {
			t.Fatal(err)
		}
		check.RunID = run.ID
		if err := db.Create(&check).Error; err != nil {
			t.Fatal(err)
		}
		events, err := service.Process(run, nil)
		if err != nil {
			t.Fatal(err)
		}
		return events
	}
	if events := process(1, failed); len(events) != 0 {
		t.Fatalf("first failure sent %d events", len(events))
	}
	if events := process(2, failed); len(events) != 1 || events[0].Event != "failure" || len(events[0].Deliveries) != 1 || events[0].Deliveries[0].Status != "sent" {
		t.Fatalf("threshold event=%#v", events)
	}
	if events := process(3, failed); len(events) != 0 {
		t.Fatalf("stable failure sent duplicate events: %#v", events)
	}
	if events := process(4, passed); len(events) != 1 || events[0].Event != "recovery" {
		t.Fatalf("recovery event=%#v", events)
	}
	if events := process(5, passed); len(events) != 0 {
		t.Fatalf("stable health sent duplicate events: %#v", events)
	}

	var state models.InspectionNotificationState
	if err := db.Where("component_id = ?", component.ID).First(&state).Error; err != nil {
		t.Fatal(err)
	}
	if state.State != "healthy" || state.ConsecutiveFailures != 0 || state.LastInspectionRunID != 5 {
		t.Fatalf("unexpected final state: %#v", state)
	}
	var notifications int64
	if err := db.Model(&models.AlertNotification{}).Count(&notifications).Error; err != nil {
		t.Fatal(err)
	}
	if notifications != 2 {
		t.Fatalf("notifications=%d, want failure+recovery", notifications)
	}
	messageMu.Lock()
	defer messageMu.Unlock()
	if len(messages) != 2 {
		t.Fatalf("webhook deliveries=%d", len(messages))
	}
}
