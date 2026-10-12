package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/credentials"
	"github.com/promlifecycle/platform/services"
	"gorm.io/gorm"
)

// The operation insert barrier preserves the real API, queue, preflight and
// component writes, but prevents a deployment goroutine touching the host.
func deploymentRetryFixture(t *testing.T) (*Handler, models.DeploymentBatch, models.DeploymentStack, models.Component) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h, hosts := batchFixture(t)
	h.basePath = t.TempDir()
	if err := h.db.AutoMigrate(&models.Cluster{}, &models.Operation{}, &models.Package{}, &models.ComponentDefinition{}, &models.DeploymentPreflight{}, &models.HostOperationClaim{}, &models.ComponentOperationClaim{}, &services.ComponentBackupPause{}); err != nil {
		t.Fatal(err)
	}
	host := hosts[0]
	host.IP, host.ConnectionType = "127.0.0.1", "local"
	if err := h.db.Save(&host).Error; err != nil {
		t.Fatal(err)
	}
	nodeID, err := services.SingleNodeID(h.db)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	port := listener.Addr().(*net.TCPAddr).Port
	original := models.Component{ClusterID: nodeID, HostID: host.ID, Type: models.ComponentPushgateway, Name: "original", Version: "1.0.0", Port: port, DeployType: "binary", Status: models.StatusError}
	if err := h.db.Create(&original).Error; err != nil {
		t.Fatal(err)
	}
	op := models.Operation{ComponentID: &original.ID, ClusterID: &nodeID, Type: "deploy", Status: "failed", Stage: "failed"}
	if err := h.db.Create(&op).Error; err != nil {
		t.Fatal(err)
	}
	request := componentCreateRequest{HostID: &host.ID, Type: "pushgateway", Name: "original", Version: "1.0.0", Port: port, DeployType: "binary", InstallPath: "/opt/promlifecycle/components/original", DataPathOverride: "/var/lib/promlifecycle/original"}
	if _, err := h.normalizeDeploymentRequest(&request, nil); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := credentials.Encrypt(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	batch := models.DeploymentBatch{Name: "failed-deploy", RequestID: "retry-identity-fixture", RequestDigest: "fixture"}
	if err := h.db.Create(&batch).Error; err != nil {
		t.Fatal(err)
	}
	stack := models.DeploymentStack{BatchID: &batch.ID, HostID: host.ID, Name: "failed-deploy", Status: "failed", Total: 1, Steps: []models.DeploymentStackStep{{Type: "pushgateway", Status: "failed", ComponentID: &original.ID, OperationID: &op.ID, EncryptedRequest: encrypted}}}
	if err := h.db.Create(&stack).Error; err != nil {
		t.Fatal(err)
	}
	packageDir := filepath.Join(h.basePath, "packages")
	if err := os.MkdirAll(packageDir, 0700); err != nil {
		t.Fatal(err)
	}
	packagePath := filepath.Join(packageDir, "pushgateway-1.0.0.tar.gz")
	content := []byte("checksum-valid fixture; never executed")
	if err := os.WriteFile(packagePath, content, 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	pkg := models.Package{Name: "pushgateway", Type: models.ComponentPushgateway, Version: "1.0.0", Format: "binary", StoragePath: packagePath, Checksum: hex.EncodeToString(digest[:]), Size: int64(len(content))}
	if err := h.db.Create(&pkg).Error; err != nil {
		t.Fatal(err)
	}
	if err := h.db.Callback().Create().Before("gorm:create").Register("retry-test:stop-deployment", func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Table == "operations" {
			tx.AddError(errors.New("test barrier: no deployment goroutine"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	return h, batch, stack, original
}

func replaceDeploymentRetryTarget(db *gorm.DB, original models.Component) (models.Component, error) {
	if err := db.Model(&models.Component{}).Where("id = ?", original.ID).Update("status", "removed").Error; err != nil {
		return models.Component{}, err
	}
	replacement := models.Component{ClusterID: original.ClusterID, HostID: original.HostID, Type: original.Type, Name: "new-live-instance", Version: "9.9.9", Port: original.Port, DeployType: "binary", Status: models.StatusRunning, ServiceName: "new-live-instance", InstallPath: "/opt/promlifecycle/components/new-live-instance", DataPath: "/var/lib/promlifecycle/new-live-instance"}
	return replacement, db.Create(&replacement).Error
}

func assertRetryTargetUnchanged(t *testing.T, h *Handler, expected models.Component) {
	t.Helper()
	var actual models.Component
	if err := h.db.First(&actual, expected.ID).Error; err != nil {
		t.Fatal(err)
	}
	if actual.Name != expected.Name || actual.Version != expected.Version || actual.Status != expected.Status || actual.ServiceName != expected.ServiceName || actual.InstallPath != expected.InstallPath || actual.DataPath != expected.DataPath {
		t.Fatalf("retry mutated another instance: got name/version/status=%s/%s/%s, want %s/%s/%s", actual.Name, actual.Version, actual.Status, expected.Name, expected.Version, expected.Status)
	}
	assertRetryHasNoOperationOrClaims(t, h)
}

func assertRetryHasNoOperationOrClaims(t *testing.T, h *Handler) {
	t.Helper()
	var count int64
	if err := h.db.Model(&models.Operation{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("retry created a new operation: %d", count)
	}
	for _, model := range []interface{}{&models.HostOperationClaim{}, &models.ComponentOperationClaim{}} {
		if err := h.db.Model(model).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("retry leaked %T: %d", model, count)
		}
	}
}

func TestDeploymentRetryAdmissionRejectsStaleTarget(t *testing.T) {
	for _, endpoint := range []string{"batch", "stack"} {
		for _, drift := range []string{"removed-and-replaced", "removed", "missing", "host", "type", "deploy-type", "operation-target", "legacy-action-null"} {
			t.Run(endpoint+"/"+drift, func(t *testing.T) {
				h, batch, stack, original := deploymentRetryFixture(t)
				switch drift {
				case "removed-and-replaced", "legacy-action-null":
					if drift == "legacy-action-null" {
						if err := h.db.Model(&stack.Steps[0]).Update("action", nil).Error; err != nil {
							t.Fatal(err)
						}
					}
					if _, err := replaceDeploymentRetryTarget(h.db, original); err != nil {
						t.Fatal(err)
					}
				case "removed":
					if err := h.db.Model(&original).Update("status", "removed").Error; err != nil {
						t.Fatal(err)
					}
				case "missing":
					if err := h.db.Delete(&original).Error; err != nil {
						t.Fatal(err)
					}
				case "host":
					if err := h.db.Model(&original).Update("host_id", 999).Error; err != nil {
						t.Fatal(err)
					}
				case "type":
					if err := h.db.Model(&original).Update("type", "node_exporter").Error; err != nil {
						t.Fatal(err)
					}
				case "deploy-type":
					if err := h.db.Model(&original).Update("deploy_type", "target").Error; err != nil {
						t.Fatal(err)
					}
				case "operation-target":
					if err := h.db.Model(&models.Operation{}).Where("id = ?", *stack.Steps[0].OperationID).Update("component_id", 999).Error; err != nil {
						t.Fatal(err)
					}
				}
				handler, id := h.RetryDeploymentBatch, batch.ID
				if endpoint == "stack" {
					handler, id = h.RetryDeploymentStack, stack.ID
				}
				response := batchRequest(t, handler, "POST", "/retry", nil, id)
				if response.Code != 409 {
					t.Fatalf("stale retry admitted: status=%d body=%s", response.Code, response.Body)
				}
				var saved models.DeploymentStack
				if err := h.db.Preload("Steps").First(&saved, stack.ID).Error; err != nil {
					t.Fatal(err)
				}
				if saved.Status != "failed" || saved.Steps[0].Status != "failed" {
					t.Fatalf("rejected retry changed queue: %+v", saved)
				}
			})
		}
	}
}

func TestDeploymentRetryWorkerRejectsReplacementAfterAdmission(t *testing.T) {
	h, batch, stack, original := deploymentRetryFixture(t)
	if response := batchRequest(t, h.RetryDeploymentBatch, "POST", "/retry", nil, batch.ID); response.Code != 202 {
		t.Fatalf("valid admission=%d %s", response.Code, response.Body)
	}
	replacement, err := replaceDeploymentRetryTarget(h.db, original)
	if err != nil {
		t.Fatal(err)
	}
	step := stack.Steps[0]
	if err := h.db.Model(&step).Update("status", "running").Error; err != nil {
		t.Fatal(err)
	}
	if err := h.runStackStep(context.Background(), &step); err == nil {
		t.Fatal("stale worker accepted replacement")
	}
	assertRetryTargetUnchanged(t, h, replacement)
	var preflights int64
	if err := h.db.Model(&models.DeploymentPreflight{}).Count(&preflights).Error; err != nil {
		t.Fatal(err)
	}
	if preflights != 0 {
		t.Fatalf("stale worker ran preflight: %d", preflights)
	}
}

func TestDeploymentRetryRechecksTargetUnderAdmissionClaims(t *testing.T) {
	for _, boundary := range []string{"host_operation_claims", "component_operation_claims"} {
		t.Run(boundary, func(t *testing.T) {
			h, _, stack, original := deploymentRetryFixture(t)
			var replacement models.Component
			changed := false
			if err := h.db.Callback().Create().After("gorm:create").Register("retry-test:replace-target", func(tx *gorm.DB) {
				if changed || tx.Statement.Schema == nil || tx.Statement.Schema.Table != boundary {
					return
				}
				changed = true
				var err error
				replacement, err = replaceDeploymentRetryTarget(tx.Session(&gorm.Session{NewDB: true}), original)
				if err != nil {
					tx.AddError(err)
				}
			}); err != nil {
				t.Fatal(err)
			}
			step := stack.Steps[0]
			if err := h.db.Model(&step).Update("status", "running").Error; err != nil {
				t.Fatal(err)
			}
			if err := h.runStackStep(context.Background(), &step); err == nil {
				t.Fatal("stale admission accepted")
			}
			if !changed || replacement.ID == 0 {
				t.Fatal("test did not reach claim boundary")
			}
			assertRetryTargetUnchanged(t, h, replacement)
			var after models.Component
			if err := h.db.First(&after, original.ID).Error; err != nil {
				t.Fatal(err)
			}
			if after.Status != "removed" {
				t.Fatalf("retry resurrected removed original: %s", after.Status)
			}
		})
	}
}

func TestDeploymentRetryKeepsOriginalTarget(t *testing.T) {
	h, batch, stack, original := deploymentRetryFixture(t)
	response := batchRequest(t, h.RetryDeploymentBatch, "POST", "/retry", nil, batch.ID)
	if response.Code != 202 {
		t.Fatalf("valid retry rejected: %d %s", response.Code, response.Body)
	}
	step := stack.Steps[0]
	if err := h.db.Model(&step).Update("status", "running").Error; err != nil {
		t.Fatal(err)
	}
	if err := h.runStackStep(context.Background(), &step); err == nil {
		t.Fatal("operation barrier not reached")
	}
	var after models.Component
	if err := h.db.First(&after, original.ID).Error; err != nil {
		t.Fatal(err)
	}
	if after.Status != models.StatusPending || after.Name != "original" || after.Version != "1.0.0" {
		t.Fatalf("valid retry failed before writing the original component: %+v", after)
	}
	var count int64
	if err := h.db.Model(&models.Component{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("valid retry created a different component: %d", count)
	}
	assertRetryHasNoOperationOrClaims(t, h)
}

func TestDeploymentRetryBatchRejectsStaleMemberAtomically(t *testing.T) {
	h, batch, first, _ := deploymentRetryFixture(t)
	second := models.DeploymentStack{BatchID: &batch.ID, HostID: first.HostID, Name: "stale-second", Status: "failed", Total: 1, Steps: []models.DeploymentStackStep{first.Steps[0]}}
	missingID := uint(999)
	second.Steps[0].ID, second.Steps[0].StackID = 0, 0
	second.Steps[0].ComponentID = &missingID
	if err := h.db.Create(&second).Error; err != nil {
		t.Fatal(err)
	}
	response := batchRequest(t, h.RetryDeploymentBatch, "POST", "/retry", nil, batch.ID)
	if response.Code != 409 {
		t.Fatalf("batch with stale member admitted: %d", response.Code)
	}
	var stacks []models.DeploymentStack
	if err := h.db.Preload("Steps").Where("batch_id = ?", batch.ID).Find(&stacks).Error; err != nil {
		t.Fatal(err)
	}
	for _, stack := range stacks {
		if stack.Status != "failed" || stack.Steps[0].Status != "failed" {
			t.Fatalf("stale member caused partial requeue: %+v", stack)
		}
	}
}

func TestDeploymentRetryPreservesSuccessfulReceiptAfterRemoval(t *testing.T) {
	h, batch, stack, original := deploymentRetryFixture(t)
	if err := h.db.Model(&models.Operation{}).Where("id = ?", *stack.Steps[0].OperationID).Update("status", "success").Error; err != nil {
		t.Fatal(err)
	}
	replacement, err := replaceDeploymentRetryTarget(h.db, original)
	if err != nil {
		t.Fatal(err)
	}
	response := batchRequest(t, h.RetryDeploymentBatch, "POST", "/retry", nil, batch.ID)
	if response.Code != 202 {
		t.Fatalf("completed receipt rejected: %d %s", response.Code, response.Body)
	}
	if err := h.runStackStep(context.Background(), &stack.Steps[0]); err != nil {
		t.Fatalf("completed receipt replayed: %v", err)
	}
	assertRetryTargetUnchanged(t, h, replacement)
	var preflights int64
	if err := h.db.Model(&models.DeploymentPreflight{}).Count(&preflights).Error; err != nil {
		t.Fatal(err)
	}
	if preflights != 0 {
		t.Fatalf("completed receipt ran deployment preflight: %d", preflights)
	}
}

func TestDeploymentRetryTargetModeKeepsOriginalTarget(t *testing.T) {
	h, batch, stack, original := deploymentRetryFixture(t)
	original.Type, original.DeployType = models.ComponentNodeExporter, "target"
	if err := h.db.Save(&original).Error; err != nil {
		t.Fatal(err)
	}
	request := componentCreateRequest{HostID: &original.HostID, Type: "node_exporter", Name: "original", Version: "1.0.0", Port: original.Port, DeployType: "target"}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := credentials.Encrypt(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	step := stack.Steps[0]
	step.Type, step.EncryptedRequest = "node_exporter", encrypted
	if err := h.db.Save(&step).Error; err != nil {
		t.Fatal(err)
	}
	response := batchRequest(t, h.RetryDeploymentBatch, "POST", "/retry", nil, batch.ID)
	if response.Code != 202 {
		t.Fatalf("target retry rejected: %d %s", response.Code, response.Body)
	}
	if err := h.db.Model(&step).Update("status", "running").Error; err != nil {
		t.Fatal(err)
	}
	if err := h.runStackStep(context.Background(), &step); err == nil {
		t.Fatal("operation barrier not reached")
	}
	var after models.Component
	if err := h.db.First(&after, original.ID).Error; err != nil {
		t.Fatal(err)
	}
	if after.Status != models.StatusPending || after.DeployType != "target" {
		t.Fatalf("target retry did not reuse the original: %+v", after)
	}
	var count int64
	if err := h.db.Model(&models.Component{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("target retry created another registration: %d", count)
	}
	assertRetryHasNoOperationOrClaims(t, h)
}
