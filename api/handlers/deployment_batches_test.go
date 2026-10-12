package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/credentials"
	"github.com/promlifecycle/platform/services"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func batchFixture(t *testing.T) (*Handler, []models.Host) {
	t.Helper()
	t.Setenv("PROMLIFECYCLE_JWT_SECRET", strings.Repeat("batch-test-secret-", 4))
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "batch.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.Host{}, &models.Component{}, &models.DeploymentBatch{}, &models.DeploymentStack{}, &models.DeploymentStackStep{}); err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { sqlDB.Close() })
	hosts := []models.Host{{Name: "host-a", IP: "192.0.2.10", OS: "linux", ConnectionType: "ssh"}, {Name: "host-b", IP: "192.0.2.11", OS: "linux", ConnectionType: "ssh"}}
	if err := db.Create(&hosts).Error; err != nil {
		t.Fatal(err)
	}
	return &Handler{db: db}, hosts
}

func batchPayload(hosts []models.Host) map[string]interface{} {
	stacks := make([]map[string]interface{}, 0, len(hosts))
	for _, host := range hosts {
		stacks = append(stacks, map[string]interface{}{"host_id": host.ID, "name": "metrics", "components": []map[string]interface{}{
			{"type": "node_exporter", "name": "node", "version": "1.8.2", "custom_env": `{"FIXTURE_SECRET":"batch-only-secret"}`},
			{"type": "blackbox_exporter", "name": "blackbox", "version": "0.26.0"},
		}})
	}
	return map[string]interface{}{"name": "batch-fixture", "request_id": "batch-request-12345678", "stacks": stacks}
}

func batchRequest(t *testing.T, handler gin.HandlerFunc, method, path string, payload interface{}, id uint) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, path, bytes.NewReader(raw))
	c.Request.Header.Set("Content-Type", "application/json")
	if id > 0 {
		c.Params = gin.Params{{Key: "id", Value: fmt.Sprint(id)}}
	}
	handler(c)
	return w
}

func createdBatch(t *testing.T, h *Handler, hosts []models.Host) models.DeploymentBatch {
	t.Helper()
	w := batchRequest(t, h.CreateDeploymentBatch, "POST", "/batches", batchPayload(hosts), 0)
	if w.Code != http.StatusAccepted {
		t.Fatalf("create status=%d: %s", w.Code, w.Body)
	}
	var body struct {
		Batch models.DeploymentBatch `json:"batch"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Batch.ID == 0 {
		t.Fatal("batch identity missing")
	}
	return body.Batch
}

func TestDeploymentBatchPersistsWholeBatchAndDeduplicates(t *testing.T) {
	h, hosts := batchFixture(t)
	batch := createdBatch(t, h, hosts)
	repeated := batchRequest(t, h.CreateDeploymentBatch, "POST", "/batches", batchPayload(hosts), 0)
	if repeated.Code != http.StatusOK {
		t.Fatalf("repeat status=%d: %s", repeated.Code, repeated.Body)
	}
	var again struct {
		Batch models.DeploymentBatch `json:"batch"`
	}
	json.Unmarshal(repeated.Body.Bytes(), &again)
	if again.Batch.ID != batch.ID {
		t.Fatal("duplicate batch created")
	}
	var batches, stacks, steps int64
	h.db.Model(&models.DeploymentBatch{}).Count(&batches)
	h.db.Model(&models.DeploymentStack{}).Count(&stacks)
	h.db.Model(&models.DeploymentStackStep{}).Count(&steps)
	if batches != 1 || stacks != 2 || steps != 4 {
		t.Fatalf("queue size %d/%d/%d", batches, stacks, steps)
	}
	if strings.Contains(repeated.Body.String(), "batch-only-secret") || strings.Contains(repeated.Body.String(), "encrypted_request") {
		t.Fatal("public DTO leaks private deployment request")
	}
	var rows []models.DeploymentStackStep
	h.db.Find(&rows)
	for _, row := range rows {
		if strings.Contains(row.EncryptedRequest, "batch-only-secret") {
			t.Fatal("queue contains plaintext secret")
		}
		if _, err := credentials.Decrypt(row.EncryptedRequest); err != nil {
			t.Fatal(err)
		}
	}
	payload := batchPayload(hosts)
	payload["name"] = "changed"
	conflict := batchRequest(t, h.CreateDeploymentBatch, "POST", "/batches", payload, 0)
	if conflict.Code != http.StatusConflict {
		t.Fatalf("reused request key accepted different input: %d", conflict.Code)
	}
}

func TestDeploymentBatchRejectsInvalidMemberAtomically(t *testing.T) {
	for _, scenario := range []string{"missing-host", "duplicate-host", "foreign-host", "invalid-component", "too-many-hosts"} {
		t.Run(scenario, func(t *testing.T) {
			h, hosts := batchFixture(t)
			payload := batchPayload(hosts)
			stacks := payload["stacks"].([]map[string]interface{})
			switch scenario {
			case "missing-host":
				stacks[1]["host_id"] = 99999
			case "duplicate-host":
				stacks[1]["host_id"] = hosts[0].ID
			case "foreign-host":
				stacks[1]["components"].([]map[string]interface{})[0]["host_id"] = hosts[0].ID
			case "invalid-component":
				stacks[1]["components"].([]map[string]interface{})[0]["name"] = "../unsafe"
			case "too-many-hosts":
				for len(stacks) < 51 {
					stacks = append(stacks, stacks[0])
				}
				payload["stacks"] = stacks
			}
			w := batchRequest(t, h.CreateDeploymentBatch, "POST", "/batches", payload, 0)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status=%d %s", w.Code, w.Body)
			}
			var n int64
			for _, model := range []interface{}{&models.DeploymentBatch{}, &models.DeploymentStack{}, &models.DeploymentStackStep{}} {
				if err := h.db.Model(model).Count(&n).Error; err != nil {
					t.Fatal(err)
				}
				if n != 0 {
					t.Fatalf("invalid batch partially persisted %T: %d", model, n)
				}
			}
		})
	}
}

func TestDeploymentBatchRetryPreservesSuccessfulHostsAndSteps(t *testing.T) {
	h, hosts := batchFixture(t)
	batch := createdBatch(t, h, hosts)
	var stacks []models.DeploymentStack
	h.db.Preload("Steps").Where("batch_id = ?", batch.ID).Order("id").Find(&stacks)
	h.db.Model(&stacks[0]).Update("status", "success")
	h.db.Model(&models.DeploymentStackStep{}).Where("stack_id = ?", stacks[0].ID).Update("status", "success")
	h.db.Model(&stacks[1]).Updates(map[string]interface{}{"status": "failed", "last_error": "fixture"})
	h.db.Model(&stacks[1].Steps[0]).Update("status", "success")
	h.db.Model(&stacks[1].Steps[1]).Updates(map[string]interface{}{"status": "failed", "error": "fixture"})
	before := batchRequest(t, h.GetDeploymentBatch, "GET", "/batches/1", nil, batch.ID)
	if before.Code != 200 || !strings.Contains(before.Body.String(), `"status":"partial"`) {
		t.Fatalf("partial result missing: %s", before.Body)
	}
	retry := batchRequest(t, h.RetryDeploymentBatch, "POST", "/batches/1/retry", map[string]interface{}{}, batch.ID)
	if retry.Code != 202 {
		t.Fatalf("retry=%d %s", retry.Code, retry.Body)
	}
	h.db.Preload("Steps").Where("batch_id = ?", batch.ID).Order("id").Find(&stacks)
	if stacks[0].Status != "success" || stacks[1].Status != "pending" || stacks[1].Steps[0].Status != "success" || stacks[1].Steps[1].Status != "pending" {
		t.Fatalf("retry replayed completed work: %+v", stacks)
	}
	repeat := batchRequest(t, h.RetryDeploymentBatch, "POST", "/batches/1/retry", map[string]interface{}{}, batch.ID)
	if repeat.Code != 409 {
		t.Fatalf("running/pending work retried: %d", repeat.Code)
	}
}

func TestDeploymentBatchCanBeReadByFreshHandler(t *testing.T) {
	h, hosts := batchFixture(t)
	batch := createdBatch(t, h, hosts)
	fresh := &Handler{db: h.db.Session(&gorm.Session{NewDB: true})}
	w := batchRequest(t, fresh.GetDeploymentBatch, "GET", "/batches/1", nil, batch.ID)
	if w.Code != 200 {
		t.Fatalf("batch unavailable after handler recreation: %d", w.Code)
	}
	var body struct {
		Batch models.DeploymentBatch `json:"batch"`
	}
	json.Unmarshal(w.Body.Bytes(), &body)
	if len(body.Batch.Stacks) != 2 || len(body.Batch.Stacks[0].Steps) != 2 {
		t.Fatalf("durable details missing: %s", w.Body)
	}
}

// Existing steps reference deployment_stacks with foreign_keys enabled in production.
func TestDeploymentBatchMigrationPreservesExistingQueue(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "legacy.db")+"?_foreign_keys=on"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()
	if err := db.AutoMigrate(&models.DeploymentStack{}, &models.DeploymentStackStep{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("DROP INDEX idx_deployment_stacks_batch_id").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("ALTER TABLE deployment_stacks DROP COLUMN batch_id").Error; err != nil {
		t.Fatal(err)
	}
	stack := models.DeploymentStack{Name: "legacy", HostID: 1, Status: "pending", Total: 1, Steps: []models.DeploymentStackStep{{Position: 0, Type: "node_exporter", Status: "pending", EncryptedRequest: "legacy-fixture"}}}
	if err := db.Omit("BatchID").Create(&stack).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.DeploymentBatch{}, &models.DeploymentStack{}, &models.DeploymentStackStep{}); err != nil {
		t.Fatalf("upgrade must preserve populated queue: %v", err)
	}
	var count int64
	db.Model(&models.DeploymentStackStep{}).Where("stack_id = ?", stack.ID).Count(&count)
	if count != 1 {
		t.Fatalf("legacy steps lost: %d", count)
	}
}

func TestDeploymentBatchRollsBackOnStepInsertFailure(t *testing.T) {
	h, hosts := batchFixture(t)
	if err := h.db.Callback().Create().Before("gorm:create").Register("fixture:step-failure", func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Table == "deployment_stack_steps" {
			tx.AddError(errors.New("injected step insert failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	w := batchRequest(t, h.CreateDeploymentBatch, "POST", "/batches", batchPayload(hosts), 0)
	if w.Code != 503 {
		t.Fatalf("status=%d %s", w.Code, w.Body)
	}
	for _, model := range []interface{}{&models.DeploymentBatch{}, &models.DeploymentStack{}, &models.DeploymentStackStep{}} {
		var count int64
		if err := h.db.Model(model).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("transaction leaked %T: %d", model, count)
		}
	}
}
func TestDeploymentBatchConcurrentReplayCreatesOneQueue(t *testing.T) {
	h, hosts := batchFixture(t)
	sqlDB, _ := h.db.DB()
	sqlDB.SetMaxOpenConns(1)
	var wg sync.WaitGroup
	results := make(chan int, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := batchRequest(t, h.CreateDeploymentBatch, "POST", "/batches", batchPayload(hosts), 0)
			results <- w.Code
		}()
	}
	wg.Wait()
	close(results)
	for status := range results {
		if status != 200 && status != 202 {
			t.Errorf("concurrent replay status=%d", status)
		}
	}
	for _, item := range []struct {
		model interface{}
		want  int64
	}{{&models.DeploymentBatch{}, 1}, {&models.DeploymentStack{}, 2}, {&models.DeploymentStackStep{}, 4}} {
		var count int64
		h.db.Model(item.model).Count(&count)
		if count != item.want {
			t.Fatalf("duplicate %T: %d", item.model, count)
		}
	}
}
func TestDeploymentBatchReopensAndRecoversInterruptedWork(t *testing.T) {
	h, hosts := batchFixture(t)
	if err := h.db.AutoMigrate(&models.Operation{}, &models.ChangePlan{}, &models.ComponentOperationClaim{}, &models.HostOperationClaim{}); err != nil {
		t.Fatal(err)
	}
	batch := createdBatch(t, h, hosts)
	persisted, err := h.readDeploymentBatch(batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	batch = *persisted
	h.db.Omit("Steps").Model(&batch.Stacks[0]).Update("status", "running")
	h.db.Model(&batch.Stacks[0].Steps[0]).Update("status", "success")
	h.db.Model(&batch.Stacks[0].Steps[1]).Update("status", "running")
	dsn := h.db.Dialector.(*sqlite.Dialector).DSN
	sqlDB, _ := h.db.DB()
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ = reopened.DB()
	defer sqlDB.Close()
	if err := services.RecoverInterruptedLifecycle(reopened); err != nil {
		t.Fatal(err)
	}
	fresh := &Handler{db: reopened}
	recovered, err := fresh.readDeploymentBatch(batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Stacks[0].Status != "interrupted" || recovered.Stacks[0].Steps[0].Status != "success" || recovered.Stacks[0].Steps[1].Status != "interrupted" || recovered.Stacks[1].Status != "pending" {
		t.Fatalf("recovery lost durable state: %+v", recovered.Stacks)
	}
	w := batchRequest(t, fresh.RetryDeploymentBatch, "POST", "/batches/1/retry", nil, batch.ID)
	if w.Code != 202 {
		t.Fatalf("retry after restart=%d %s", w.Code, w.Body)
	}
	recovered, _ = fresh.readDeploymentBatch(batch.ID)
	if recovered.Stacks[0].Steps[0].Status != "success" || recovered.Stacks[0].Steps[1].Status != "pending" {
		t.Fatal("retry after restart replayed successful step")
	}
}
func TestDeploymentBatchWorkerContinuesAfterHostFailure(t *testing.T) {
	h, hosts := batchFixture(t)
	batch := createdBatch(t, h, hosts)
	persisted, err := h.readDeploymentBatch(batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	batch = *persisted
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var calls []uint
	worker := services.NewDeploymentStackService(h.db, func(_ context.Context, step *models.DeploymentStackStep) error {
		calls = append(calls, step.ID)
		if step.StackID == batch.Stacks[0].ID {
			return errors.New("first host unavailable")
		}
		if step.ID == batch.Stacks[1].Steps[1].ID {
			cancel()
		}
		return nil
	})
	worker.Run(ctx)
	result, err := h.readDeploymentBatch(batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "partial" || result.Stacks[0].Status != "failed" || result.Stacks[1].Status != "success" || len(calls) != 3 {
		t.Fatalf("host failure blocked queue: status=%s calls=%v", result.Status, calls)
	}
}
