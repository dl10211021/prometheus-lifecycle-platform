package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/core/deployer"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/credentials"
	"github.com/promlifecycle/platform/services"
	"gorm.io/gorm"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func upgradeBatchFixture(t *testing.T) (*Handler, []models.Component) {
	t.Helper()
	h, hosts := batchFixture(t)
	if err := h.db.AutoMigrate(&models.Cluster{}, &models.User{}, &models.ComponentAdoption{}, &models.ComponentOperationClaim{}, &models.HostOperationClaim{}, &models.ChangePlan{}, &models.ChangeBaseline{}, &models.Package{}, &models.ComponentVersion{}, &models.Operation{}, &services.ComponentBackupPause{}); err != nil {
		t.Fatal(err)
	}
	cluster := models.Cluster{Name: "upgrade-fixture"}
	if err := h.db.Create(&cluster).Error; err != nil {
		t.Fatal(err)
	}
	user := models.User{Username: "batch-admin", Role: "system_admin", IsActive: true}
	if err := h.db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	h.basePath = base
	h.deployerFactory = deployer.NewFactory(base)
	var components []models.Component
	for i, host := range hosts {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("node_exporter_build_info{version=\"1.8.2\"} 1\n"))
		}))
		t.Cleanup(server.Close)
		u, _ := url.Parse(server.URL)
		port, _ := strconv.Atoi(u.Port())
		ip := "127.0.0.1"
		if i == 1 {
			ip = "localhost"
		}
		h.db.Model(&host).Updates(map[string]interface{}{"ip": ip, "connection_type": "local"})
		c := models.Component{ClusterID: cluster.ID, HostID: host.ID, Type: models.ComponentNodeExporter, Name: host.Name, Version: "1.8.2", DeployType: "binary", Status: models.StatusRunning, Port: port, InternalURL: server.URL, InstallPath: base, ServiceName: "fixture-" + host.Name}
		if err := h.db.Create(&c).Error; err != nil {
			t.Fatal(err)
		}
		components = append(components, c)
	}
	pkg := models.Package{Name: "node", Type: "node_exporter", Version: "1.9.0", StoragePath: base + "/node.tar.gz", Size: 1}
	if err := h.db.Create(&pkg).Error; err != nil {
		t.Fatal(err)
	}
	return h, components
}
func upgradeBatchPayload(cs []models.Component) map[string]interface{} {
	items := []map[string]interface{}{}
	for _, c := range cs {
		items = append(items, map[string]interface{}{"component_id": c.ID, "current_version": "1.8.2", "new_version": "1.9.0"})
	}
	return map[string]interface{}{"kind": "upgrade", "name": "existing exporters", "request_id": "batch-upgrade-12345678", "reason": "统一升级已有实例", "upgrades": items}
}
func upgradeActorHandler(fn gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set("user_id", uint(1))
		c.Set("username", "batch-admin")
		c.Set("role", "system_admin")
		fn(c)
	}
}

func TestUpgradeBatchQueuesExistingComponentsAtomically(t *testing.T) {
	h, cs := upgradeBatchFixture(t)
	w := batchRequest(t, upgradeActorHandler(h.CreateDeploymentBatch), "POST", "/batches", upgradeBatchPayload(cs), 0)
	if w.Code != http.StatusAccepted {
		t.Fatalf("existing managed instances must queue as an upgrade batch: status=%d body=%s", w.Code, w.Body)
	}
	var body struct{ Batch models.DeploymentBatch }
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Batch.Stacks) != 2 || body.Batch.Summary.TotalSteps != 2 {
		t.Fatalf("wrong host/step grouping: %s", w.Body)
	}
	var count int64
	h.db.Model(&models.Component{}).Count(&count)
	if count != 2 {
		t.Fatal("upgrade created replacement component records")
	}
	h.db.Model(&models.ChangePlan{}).Count(&count)
	if count != 2 {
		t.Fatal("upgrade plans not atomically linked")
	}
	again := batchRequest(t, upgradeActorHandler(h.CreateDeploymentBatch), "POST", "/batches", upgradeBatchPayload(cs), 0)
	if again.Code != 200 {
		t.Fatalf("idempotent replay failed: %s", again.Body)
	}
	h.db.Model(&models.ChangePlan{}).Count(&count)
	if count != 2 {
		t.Fatal("replay created duplicate plans")
	}
}

func queueUpgradeFixture(t *testing.T, h *Handler, cs []models.Component) models.DeploymentBatch {
	t.Helper()
	w := batchRequest(t, upgradeActorHandler(h.CreateDeploymentBatch), "POST", "/batches", upgradeBatchPayload(cs), 0)
	if w.Code != 202 {
		t.Fatalf("queue: %d %s", w.Code, w.Body)
	}
	var body struct{ Batch models.DeploymentBatch }
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if err := h.db.Preload("Steps").Where("batch_id = ?", body.Batch.ID).Find(&body.Batch.Stacks).Error; err != nil {
		t.Fatal(err)
	}
	return body.Batch
}
func assertNoUpgradeWork(t *testing.T, h *Handler) {
	t.Helper()
	for _, model := range []interface{}{&models.DeploymentBatch{}, &models.DeploymentStack{}, &models.DeploymentStackStep{}, &models.ChangePlan{}, &models.ChangeBaseline{}} {
		var n int64
		if err := h.db.Model(model).Count(&n).Error; err != nil || n != 0 {
			t.Fatalf("unexpected persisted %T: %d %v", model, n, err)
		}
	}
}
func TestUpgradeBatchPreflightAndAtomicRejection(t *testing.T) {
	t.Run("preview writes no work", func(t *testing.T) {
		h, cs := upgradeBatchFixture(t)
		w := batchRequest(t, upgradeActorHandler(h.PreflightUpgradeBatch), "POST", "/preflight", upgradeBatchPayload(cs), 0)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"passed":true`) {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		assertNoUpgradeWork(t, h)
	})
	t.Run("stale member rejects all", func(t *testing.T) {
		h, cs := upgradeBatchFixture(t)
		h.db.Model(&cs[1]).Update("version", "1.8.3")
		w := batchRequest(t, upgradeActorHandler(h.CreateDeploymentBatch), "POST", "/batches", upgradeBatchPayload(cs), 0)
		if w.Code != 409 {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		assertNoUpgradeWork(t, h)
	})
	t.Run("storage failure rolls back plans and batch", func(t *testing.T) {
		h, cs := upgradeBatchFixture(t)
		h.db.Callback().Create().Before("gorm:create").Register("fail-steps", func(tx *gorm.DB) {
			if tx.Statement.Table == "deployment_stack_steps" {
				tx.AddError(errors.New("injected persistence failure"))
			}
		})
		w := batchRequest(t, upgradeActorHandler(h.CreateDeploymentBatch), "POST", "/batches", upgradeBatchPayload(cs), 0)
		if w.Code != 503 {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		assertNoUpgradeWork(t, h)
	})
	t.Run("same host keeps separate instance steps", func(t *testing.T) {
		h, cs := upgradeBatchFixture(t)
		h.db.Model(&cs[1]).Update("host_id", cs[0].HostID)
		b := queueUpgradeFixture(t, h, cs)
		if len(b.Stacks) != 1 || len(b.Stacks[0].Steps) != 2 {
			t.Fatalf("%+v", b)
		}
	})
}
func TestUpgradeBatchExecutionGuards(t *testing.T) {
	for _, tc := range []string{"disabled requester", "stopped instance", "edited plan", "unknown failed outcome", "verified resume"} {
		t.Run(tc, func(t *testing.T) {
			h, cs := upgradeBatchFixture(t)
			b := queueUpgradeFixture(t, h, cs[:1])
			step := b.Stacks[0].Steps[0]
			h.db.Model(&step).Update("status", "running")
			var operationAttempts int
			h.db.Callback().Create().Before("gorm:create").Register("never-touch-host", func(tx *gorm.DB) {
				if tx.Statement.Table == "operations" {
					operationAttempts++
					tx.AddError(errors.New("test blocks all remote mutations"))
				}
			})
			switch tc {
			case "disabled requester":
				h.db.Session(&gorm.Session{SkipHooks: true}).Model(&models.User{}).Where("id = ?", 1).Update("is_active", false)
			case "stopped instance":
				h.db.Model(&cs[0]).Update("status", models.StatusStopped)
			case "edited plan":
				h.db.Model(&models.ChangePlan{}).Where("id = ?", *step.ChangePlanID).Update("reason", "changed after queue")
			case "unknown failed outcome":
				h.db.Model(&models.ChangePlan{}).Where("id = ?", *step.ChangePlanID).Updates(map[string]interface{}{"status": models.ChangeStatusFailed, "operation_id": 99})
			case "verified resume":
				h.db.Model(&models.ChangePlan{}).Where("id = ?", *step.ChangePlanID).Update("status", models.ChangeStatusVerified)
			}
			err := h.runStackStep(context.Background(), &step)
			if tc == "verified resume" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("execution must reject changed authorization/state")
			}
			if tc == "stopped instance" {
				var preflight *services.StackPreflightFailure
				if !errors.As(err, &preflight) {
					t.Fatalf("missing actionable preflight failure: %v", err)
				}
			}
			if operationAttempts != 0 {
				t.Fatalf("guard reached operation creation: %d", operationAttempts)
			}
			if tc == "disabled requester" || tc == "stopped instance" {
				var plan models.ChangePlan
				h.db.First(&plan, *step.ChangePlanID)
				if plan.Status != models.ChangeStatusDraft {
					t.Fatalf("guard changed plan to %s", plan.Status)
				}
			}
		})
	}
}
func TestQueuedUpgradePlanCannotBeEditedDeletedOrExecutedDirectly(t *testing.T) {
	for _, action := range []string{"edit", "delete", "execute"} {
		t.Run(action, func(t *testing.T) {
			h, cs := upgradeBatchFixture(t)
			b := queueUpgradeFixture(t, h, cs[:1])
			id := *b.Stacks[0].Steps[0].ChangePlanID
			svc := h.changeService()
			actor := services.ChangeActor{ID: 1, Username: "batch-admin", Role: "system_admin"}
			var attempts int
			h.db.Callback().Create().Before("gorm:create").Register("block-execution", func(tx *gorm.DB) {
				if tx.Statement.Table == "operations" {
					attempts++
					tx.AddError(errors.New("test blocks remote"))
				}
			})
			var err error
			switch action {
			case "edit":
				_, err = svc.UpdateDraft(context.Background(), id, services.ChangePlanRequest{Reason: "edited", After: json.RawMessage(`{"new_version":"1.9.0"}`)}, actor)
			case "delete":
				err = svc.DeleteDraft(id, actor)
			case "execute":
				_, err = svc.Execute(context.Background(), id, actor)
			}
			if err == nil {
				t.Fatal("queued plan must be controlled by its batch")
			}
			if attempts != 0 {
				t.Fatal("direct execute reached remote mutation")
			}
			var plan models.ChangePlan
			if err := h.db.First(&plan, id).Error; err != nil || plan.Status != models.ChangeStatusDraft || plan.Reason == "edited" {
				t.Fatalf("queued plan changed: %+v %v", plan, err)
			}
		})
	}
}

func TestUpgradeBatchRetriesKeepHistoryAndStopBeforeRemoteOnFailure(t *testing.T) {
	for _, status := range []models.ChangeStatus{models.ChangeStatusDraft, models.ChangeStatusFailed, models.ChangeStatusRolledBack} {
		t.Run(string(status), func(t *testing.T) {
			h, cs := upgradeBatchFixture(t)
			b := queueUpgradeFixture(t, h, cs[:1])
			step := b.Stacks[0].Steps[0]
			old := *step.ChangePlanID
			if err := h.db.Model(&step).Update("status", "running").Error; err != nil {
				t.Fatal(err)
			}
			if err := h.db.Model(&models.ChangePlan{}).Where("id = ?", old).Update("status", status).Error; err != nil {
				t.Fatal(err)
			}
			// Runtime polling changes timestamps but not managed desired state.
			if err := h.db.Model(&cs[0]).Update("status", models.StatusRunning).Error; err != nil {
				t.Fatal(err)
			}
			var attempts int
			h.db.Callback().Create().Before("gorm:create").Register("fail-before-remote", func(tx *gorm.DB) {
				if tx.Statement.Table == "operations" {
					attempts++
					tx.AddError(errors.New("injected operation persistence failure"))
				}
			})
			if err := h.runStackStep(context.Background(), &step); err == nil || attempts != 1 {
				t.Fatalf("expected exactly one admitted attempt, got %d: %v", attempts, err)
			}
			want := 1
			if status != models.ChangeStatusDraft {
				want = 2
			}
			if len(step.ChangePlanHistory) != want || step.ChangePlanHistory[0] != old {
				t.Fatalf("lost history: %+v", step)
			}
			var saved models.DeploymentStackStep
			if err := h.db.First(&saved, step.ID).Error; err != nil {
				t.Fatal(err)
			}
			if len(saved.ChangePlanHistory) != want || saved.ChangePlanHistory[0] != old || *saved.ChangePlanID != *step.ChangePlanID {
				t.Fatalf("retry history was not persisted: %+v", saved)
			}

			var count int64
			h.db.Model(&models.Component{}).Count(&count)
			if count != 2 {
				t.Fatal("retry created replacement component")
			}
		})
	}
}

func TestUpgradeBatchOperationLinkFailureDoesNotApply(t *testing.T) {
	h, cs := upgradeBatchFixture(t)
	b := queueUpgradeFixture(t, h, cs[:1])
	step := b.Stacks[0].Steps[0]
	defer func() {
		if value := recover(); value != nil {
			t.Errorf("remote apply boundary reached after operation link failure: %v", value)
		}
	}()
	h.db.Callback().Update().Before("gorm:update").Register("fail-operation-link", func(tx *gorm.DB) {
		if tx.Statement.Table == "change_plans" {
			if values, ok := tx.Statement.Dest.(map[string]interface{}); ok && values["operation_id"] != nil {
				tx.AddError(errors.New("injected operation link persistence failure"))
			}
		}
		if tx.Statement.Table == "components" {
			panic("component mutation must not start")
		}
	})
	if err := h.runStackStep(context.Background(), &step); err == nil {
		t.Fatal("missing persistence error")
	}
	var count int64
	h.db.Model(&models.Operation{}).Count(&count)
	if count != 0 {
		t.Fatalf("unlinked operation survived transaction: %d", count)
	}
	var plan models.ChangePlan
	h.db.First(&plan, *step.ChangePlanID)
	if plan.OperationID != nil || plan.Status != models.ChangeStatusFailed {
		t.Fatalf("unexpected plan state: %+v", plan)
	}
}

// Target drift must be rejected by the real queue before health/disk preflight,
// retry-plan persistence, or the operation boundary that permits remote writes.
func TestUpgradeBatchRejectsChangedTargetBeforePreflight(t *testing.T) {
	mutations := []struct {
		name, table, field string
		value              interface{}
	}{
		{"host IP", "hosts", "ip", "127.0.0.2"},
		{"host port", "hosts", "port", 2222},
		{"host username", "hosts", "username", "another-user"},
		{"connection type", "hosts", "connection_type", "ssh"},
		{"host OS", "hosts", "os", "windows"},
		{"Windows transport", "hosts", "is_windows", true},
		{"WinRM port", "hosts", "win_rm_port", 5985},
		{"WinRM scheme", "hosts", "win_rm_scheme", "http"},
		{"WinRM verification", "hosts", "win_rm_skip_verify", true},
		{"binary path", "components", "binary_path", "/different/node_exporter"},
		{"imported args", "components", "imported_args", "--web.listen-address=:9199"},
		{"compose path", "components", "compose_path", "/different/compose.yml"},
		{"health endpoint", "components", "internal_url", "http://127.0.0.2:9199"},
	}
	for _, status := range []models.ChangeStatus{models.ChangeStatusDraft, models.ChangeStatusFailed, models.ChangeStatusRolledBack} {
		for _, mutation := range mutations {
			t.Run(string(status)+"/"+mutation.name, func(t *testing.T) {
				h, cs := upgradeBatchFixture(t)
				var healthRequests atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					healthRequests.Add(1)
					_, _ = w.Write([]byte("node_exporter_build_info{version=\"1.8.2\"} 1\n"))
				}))
				defer server.Close()
				probeURL, _ := url.Parse(server.URL)
				probePort, _ := strconv.Atoi(probeURL.Port())
				if err := h.db.Model(&cs[0]).Updates(map[string]interface{}{"internal_url": server.URL, "port": probePort}).Error; err != nil {
					t.Fatal(err)
				}
				b := queueUpgradeFixture(t, h, cs[:1])
				step := b.Stacks[0].Steps[0]
				originalPlan := *step.ChangePlanID
				if err := h.db.Model(&step).Update("status", "running").Error; err != nil {
					t.Fatal(err)
				}
				if err := h.db.Model(&models.ChangePlan{}).Where("id = ?", originalPlan).Update("status", status).Error; err != nil {
					t.Fatal(err)
				}
				id := cs[0].ID
				if mutation.table == "hosts" {
					id = cs[0].HostID
				}
				if err := h.db.Table(mutation.table).Where("id = ?", id).Update(mutation.field, mutation.value).Error; err != nil {
					t.Fatal(err)
				}
				beforeHealth := healthRequests.Load()
				if beforeHealth == 0 {
					t.Fatal("test did not observe initial health preflight")
				}
				attempts := blockUpgradeOperations(t, h)
				err := h.runStackStep(context.Background(), &step)
				if !errors.Is(err, services.ErrStaleChangePlan) {
					t.Errorf("target drift must reject as stale, got %v", err)
				}
				if got := healthRequests.Load(); got != beforeHealth {
					t.Errorf("drift reached health preflight: requests %d -> %d", beforeHealth, got)
				}
				assertUpgradeAttemptNotStarted(t, h, step, originalPlan, attempts.Load())
			})
		}
	}
}

func blockUpgradeOperations(t *testing.T, h *Handler) *atomic.Int32 {
	t.Helper()
	attempts := &atomic.Int32{}
	if err := h.db.Callback().Create().Before("gorm:create").Register("block-upgrade-remote-writes", func(tx *gorm.DB) {
		if tx.Statement.Table == "operations" {
			attempts.Add(1)
			tx.AddError(errors.New("test blocks all remote writes"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	return attempts
}

func assertUpgradeAttemptNotStarted(t *testing.T, h *Handler, step models.DeploymentStackStep, originalPlan uint, attempts int32) {
	t.Helper()
	if attempts != 0 {
		t.Errorf("target guard reached operation creation: %d", attempts)
	}
	var plans int64
	if err := h.db.Model(&models.ChangePlan{}).Count(&plans).Error; err != nil {
		t.Fatal(err)
	}
	if plans != 1 {
		t.Errorf("target drift created a retry plan: %d plans", plans)
	}
	var saved models.DeploymentStackStep
	if err := h.db.First(&saved, step.ID).Error; err != nil {
		t.Fatal(err)
	}
	if saved.ChangePlanID == nil || *saved.ChangePlanID != originalPlan || len(saved.ChangePlanHistory) != 1 || saved.ChangePlanHistory[0] != originalPlan {
		t.Errorf("rejected target changed queued history: %+v", saved)
	}
}

func TestUpgradeBatchLegacyTargetBindingFailsClosed(t *testing.T) {
	for _, status := range []models.ChangeStatus{models.ChangeStatusDraft, models.ChangeStatusFailed, models.ChangeStatusRolledBack, models.ChangeStatusVerified} {
		t.Run(string(status), func(t *testing.T) {
			h, cs := upgradeBatchFixture(t)
			b := queueUpgradeFixture(t, h, cs[:1])
			step := b.Stacks[0].Steps[0]
			originalPlan := *step.ChangePlanID
			plain, err := credentials.Decrypt(step.EncryptedRequest)
			if err != nil {
				t.Fatal(err)
			}
			var legacy map[string]interface{}
			if err := json.Unmarshal([]byte(plain), &legacy); err != nil {
				t.Fatal(err)
			}
			delete(legacy, "target_fingerprint")
			payload, err := json.Marshal(legacy)
			if err != nil {
				t.Fatal(err)
			}
			step.EncryptedRequest, err = credentials.Encrypt(string(payload))
			if err != nil {
				t.Fatal(err)
			}
			if err := h.db.Model(&step).Updates(map[string]interface{}{"status": "running", "encrypted_request": step.EncryptedRequest}).Error; err != nil {
				t.Fatal(err)
			}
			if err := h.db.Model(&models.ChangePlan{}).Where("id = ?", originalPlan).Update("status", status).Error; err != nil {
				t.Fatal(err)
			}
			attempts := blockUpgradeOperations(t, h)
			err = h.runStackStep(context.Background(), &step)
			if status == models.ChangeStatusVerified {
				if err != nil {
					t.Errorf("settled successful step must remain nonreplayed: %v", err)
				}
			} else if !errors.Is(err, services.ErrStaleChangePlan) || !strings.Contains(err.Error(), "重新创建升级批次") {
				t.Errorf("legacy pending target must fail closed with recreate guidance: %v", err)
			}
			assertUpgradeAttemptNotStarted(t, h, step, originalPlan, attempts.Load())
		})
	}
}

func TestUpgradeBatchRejectsTargetDriftAtExecutionReload(t *testing.T) {
	h, cs := upgradeBatchFixture(t)
	b := queueUpgradeFixture(t, h, cs[:1])
	step := b.Stacks[0].Steps[0]
	originalPlan := *step.ChangePlanID
	attempts := blockUpgradeOperations(t, h)
	var changed atomic.Bool
	if err := h.db.Callback().Update().After("gorm:commit_or_rollback_transaction").Register("retarget-after-plan-claim", func(tx *gorm.DB) {
		if tx.Statement.Table != "change_plans" {
			return
		}
		values, ok := tx.Statement.Dest.(map[string]interface{})
		if !ok || values["status"] != models.ChangeStatusExecuting || changed.Swap(true) {
			return
		}
		if err := h.db.Model(&models.Host{}).Where("id = ?", cs[0].HostID).Update("ip", "127.0.0.2").Error; err != nil {
			tx.AddError(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	err := h.runStackStep(context.Background(), &step)
	if !changed.Load() {
		t.Fatal("test did not reach execution reload window")
	}
	if !errors.Is(err, services.ErrStaleChangePlan) {
		t.Errorf("execution reload retarget must reject as stale: %v", err)
	}
	assertUpgradeAttemptNotStarted(t, h, step, originalPlan, attempts.Load())
}

func TestUpgradeBatchCredentialRotationKeepsTarget(t *testing.T) {
	h, cs := upgradeBatchFixture(t)
	b := queueUpgradeFixture(t, h, cs[:1])
	step := b.Stacks[0].Steps[0]
	if err := h.db.Model(&models.Host{}).Where("id = ?", cs[0].HostID).Updates(map[string]interface{}{"password": "rotated-test-secret", "private_key": "rotated-test-key"}).Error; err != nil {
		t.Fatal(err)
	}
	attempts := blockUpgradeOperations(t, h)
	err := h.runStackStep(context.Background(), &step)
	if err == nil || errors.Is(err, services.ErrStaleChangePlan) || attempts.Load() != 1 {
		t.Fatalf("credential rotation must preserve reviewed target and admit one operation: attempts=%d error=%v", attempts.Load(), err)
	}
}

func TestUpgradeBatchWorkerPersistsTargetRecreationGuidance(t *testing.T) {
	for _, status := range []models.ChangeStatus{models.ChangeStatusDraft, models.ChangeStatusFailed, models.ChangeStatusRolledBack} {
		for _, cause := range []string{"missing binding", "changed target"} {
			t.Run(string(status)+"/"+cause, func(t *testing.T) {
				h, cs := upgradeBatchFixture(t)
				b := queueUpgradeFixture(t, h, cs[:1])
				step := b.Stacks[0].Steps[0]
				if err := h.db.Model(&models.ChangePlan{}).Where("id = ?", *step.ChangePlanID).Update("status", status).Error; err != nil {
					t.Fatal(err)
				}
				if cause == "missing binding" {
					plain, err := credentials.Decrypt(step.EncryptedRequest)
					if err != nil {
						t.Fatal(err)
					}
					var legacy map[string]interface{}
					if err := json.Unmarshal([]byte(plain), &legacy); err != nil {
						t.Fatal(err)
					}
					delete(legacy, "target_fingerprint")
					payload, err := json.Marshal(legacy)
					if err != nil {
						t.Fatal(err)
					}
					encrypted, err := credentials.Encrypt(string(payload))
					if err != nil {
						t.Fatal(err)
					}
					if err := h.db.Model(&step).Update("encrypted_request", encrypted).Error; err != nil {
						t.Fatal(err)
					}
				} else if err := h.db.Model(&models.Host{}).Where("id = ?", cs[0].HostID).Update("username", "target-value-must-not-be-error-text").Error; err != nil {
					t.Fatal(err)
				}
				attempts := blockUpgradeOperations(t, h)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				worker := services.NewDeploymentStackService(h.db, func(ctx context.Context, step *models.DeploymentStackStep) error {
					defer cancel()
					return h.runStackStep(ctx, step)
				})
				worker.Run(ctx)
				saved, err := worker.Get(b.Stacks[0].ID)
				if err != nil {
					t.Fatal(err)
				}
				if saved.Status != "failed" || len(saved.Steps) != 1 || saved.Steps[0].Status != "failed" {
					t.Fatalf("worker did not persist failed target: %+v", saved)
				}
				const guidance = "升级目标已变化或缺少绑定，请重新创建升级批次"
				for _, message := range []string{saved.LastError, saved.Steps[0].Error} {
					if !strings.Contains(message, guidance) {
						t.Errorf("persisted worker error hides recreation guidance: %q", message)
					}
					if strings.Contains(message, "target-value-must-not-be-error-text") {
						t.Errorf("persisted worker error exposes target input: %q", message)
					}
				}
				response := batchRequest(t, h.GetDeploymentBatch, "GET", "/batches", nil, b.ID)
				if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), guidance) {
					t.Errorf("batch API hides recreation guidance: status=%d body=%s", response.Code, response.Body)
				}
				assertUpgradeAttemptNotStarted(t, h, step, *step.ChangePlanID, attempts.Load())
			})
		}
	}
}
