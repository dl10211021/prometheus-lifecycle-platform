package handlers

import (
	"encoding/json"
	"testing"

	"github.com/promlifecycle/platform/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestOperationProgressPersistsOrderedHistory(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.Operation{}); err != nil {
		t.Fatal(err)
	}
	handler := &Handler{db: db}
	operation := handler.createOperation(7, nil, "deploy", "pending", "test", nil)

	handler.updateOperationProgress(operation.ID, "prechecking", "检查目标", 5)
	handler.updateOperationProgress(operation.ID, "installing", "安装服务", 60)
	handler.updateOperation(operation.ID, true, "部署完成", nil)

	var stored models.Operation
	if err := db.First(&stored, operation.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != "success" || stored.Stage != "completed" || stored.Progress != 100 {
		t.Fatalf("unexpected final operation: status=%s stage=%s progress=%d", stored.Status, stored.Stage, stored.Progress)
	}
	if stored.StartedAt == nil || stored.CompletedAt == nil {
		t.Fatalf("operation timestamps missing: started=%v completed=%v", stored.StartedAt, stored.CompletedAt)
	}
	var history []models.OperationStageEvent
	if err := json.Unmarshal([]byte(stored.StageHistory), &history); err != nil {
		t.Fatalf("decode history: %v", err)
	}
	want := []string{"queued", "prechecking", "installing", "completed"}
	if len(history) != len(want) {
		t.Fatalf("history length=%d, want %d: %#v", len(history), len(want), history)
	}
	for index, stage := range want {
		if history[index].Stage != stage {
			t.Fatalf("history[%d].stage=%q, want %q", index, history[index].Stage, stage)
		}
	}
}

func TestOperationProgressClampsPercent(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:progress-clamp?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.Operation{}); err != nil {
		t.Fatal(err)
	}
	handler := &Handler{db: db}
	operation := handler.createOperation(9, nil, "deploy", "pending", "test", nil)
	handler.updateOperationProgress(operation.ID, "invalid", "clamp", 101)
	if err := db.First(operation, operation.ID).Error; err != nil {
		t.Fatal(err)
	}
	if operation.Progress != 100 {
		t.Fatalf("progress=%d, want 100", operation.Progress)
	}
}

func TestOperationResponsesIncludeComponentAndLegacyFallback(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:operation-view?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.Host{}, &models.Cluster{}, &models.Component{}, &models.Operation{}); err != nil {
		t.Fatal(err)
	}
	host := models.Host{Name: "target-a", IP: "192.0.2.10", OS: "linux", ConnectionType: "ssh"}
	if err := db.Create(&host).Error; err != nil {
		t.Fatal(err)
	}
	cluster := models.Cluster{Name: "single-node"}
	if err := db.Create(&cluster).Error; err != nil {
		t.Fatal(err)
	}
	component := models.Component{HostID: host.ID, ClusterID: cluster.ID, Name: "prom-a", Type: models.ComponentPrometheus}
	if err := db.Create(&component).Error; err != nil {
		t.Fatal(err)
	}
	operation := models.Operation{ComponentID: &component.ID, Type: "restart", Status: "success", Message: "legacy complete"}
	if err := db.Create(&operation).Error; err != nil {
		t.Fatal(err)
	}
	handler := NewOperationsHandler(db)
	response := handler.operationResponses([]models.Operation{operation})
	if len(response) != 1 {
		t.Fatalf("responses=%d", len(response))
	}
	if response[0].ComponentName != component.Name || response[0].HostIP != host.IP {
		t.Fatalf("component context missing: %#v", response[0])
	}
	if response[0].Stage != "success" || response[0].Progress != 100 {
		t.Fatalf("legacy fallback stage=%q progress=%d", response[0].Stage, response[0].Progress)
	}
}
