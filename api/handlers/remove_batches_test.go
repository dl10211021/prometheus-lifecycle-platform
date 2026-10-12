package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/promlifecycle/platform/models"
)

func removalBatchFixture(t *testing.T) (*Handler, []models.Component) {
	t.Helper()
	h, components := upgradeBatchFixture(t)
	if err := h.db.AutoMigrate(&models.ComponentConfig{}, &models.ResourceUsage{}, &models.Backup{}, &models.ScheduledBackup{}, &models.MaintenanceWindow{}, &models.InspectionNotificationState{}, &models.TopologyEdge{}, &models.Incident{}); err != nil {
		t.Fatal(err)
	}
	return h, components
}

func removalPayload(ids ...uint) map[string]interface{} {
	return map[string]interface{}{"kind": "remove", "name": "批量移出管理", "mode": "keep_service", "reason": "计划调整", "request_id": "remove-batch-12345678", "component_ids": ids}
}

func TestRemovalBatchPersistsPlansAtomicallyAndReplays(t *testing.T) {
	h, cs := removalBatchFixture(t)
	payload := removalPayload(cs[0].ID, cs[1].ID)
	w := batchRequest(t, upgradeActorHandler(h.CreateDeploymentBatch), "POST", "/batches", payload, 0)
	if w.Code != http.StatusAccepted {
		t.Fatalf("create %d: %s", w.Code, w.Body)
	}
	var response struct {
		Batch models.DeploymentBatch `json:"batch"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	b, err := h.readDeploymentBatch(response.Batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Kind != "remove" || len(b.Stacks) != 2 || b.Summary.TotalSteps != 2 {
		t.Fatalf("wrong batch grouping: %+v", b)
	}
	for _, stack := range b.Stacks {
		for _, step := range stack.Steps {
			if step.Action != "remove" || step.ChangePlanID == nil || step.ComponentID == nil || step.EncryptedRequest == "" {
				t.Fatalf("unlinked step: %+v", step)
			}
			var plan models.ChangePlan
			if err := h.db.First(&plan, *step.ChangePlanID).Error; err != nil {
				t.Fatal(err)
			}
			if !plan.QueueManaged || plan.Kind != models.ChangeKindRemove || plan.ResourceID != *step.ComponentID {
				t.Fatalf("wrong plan: %+v", plan)
			}
		}
	}
	h.db.Model(&cs[0]).Update("status", "removed")
	w = batchRequest(t, upgradeActorHandler(h.CreateDeploymentBatch), "POST", "/batches", payload, 0)
	if w.Code != http.StatusOK {
		t.Fatalf("replay %d: %s", w.Code, w.Body)
	}
	payload["mode"] = "keep_data"
	w = batchRequest(t, upgradeActorHandler(h.CreateDeploymentBatch), "POST", "/batches", payload, 0)
	if w.Code != http.StatusConflict {
		t.Fatalf("changed mode replay accepted: %d", w.Code)
	}
}

func TestRemovalBatchRejectsInvalidScopeWithoutPartialPlan(t *testing.T) {
	for _, scenario := range []string{"duplicate", "missing", "removed", "observe", "busy", "unsupported", "inactive", "no_reason", "full_delete", "mixed_upgrade"} {
		t.Run(scenario, func(t *testing.T) {
			h, cs := removalBatchFixture(t)
			payload := removalPayload(cs[0].ID, cs[1].ID)
			switch scenario {
			case "duplicate":
				payload["component_ids"] = []uint{cs[0].ID, cs[0].ID}
			case "missing":
				payload["component_ids"] = []uint{cs[0].ID, 99999}
			case "removed":
				h.db.Model(&cs[1]).Update("status", "removed")
			case "observe":
				h.db.Create(&models.ComponentAdoption{ComponentID: cs[1].ID, Ownership: "observe"})
			case "busy":
				h.db.Model(&cs[1]).Update("status", "upgrading")
			case "unsupported":
				h.db.Model(&cs[1]).Update("deploy_type", "target")
			case "inactive":
				if result := h.db.Exec("UPDATE users SET is_active = 0 WHERE id = ?", uint(1)); result.Error != nil || result.RowsAffected != 1 {
					t.Fatalf("disable fixture actor: %v / %d", result.Error, result.RowsAffected)
				}
			case "no_reason":
				payload["reason"] = " "
			case "full_delete":
				payload["mode"] = "full_delete"
			case "mixed_upgrade":
				payload["upgrades"] = []map[string]interface{}{{"component_id": cs[0].ID}}
			}
			w := batchRequest(t, upgradeActorHandler(h.CreateDeploymentBatch), "POST", "/batches", payload, 0)
			if w.Code < 400 || w.Code >= 500 {
				t.Fatalf("unsafe batch accepted: %d %s", w.Code, w.Body)
			}
			for _, model := range []interface{}{&models.DeploymentBatch{}, &models.DeploymentStackStep{}, &models.ChangePlan{}} {
				var count int64
				h.db.Model(model).Count(&count)
				if count != 0 {
					t.Fatalf("partial queue/plan persisted: %d", count)
				}
			}
		})
	}
}

func TestRemovalBatchKeepServiceVerifiesAndDoesNotReplay(t *testing.T) {
	h, cs := removalBatchFixture(t)
	w := batchRequest(t, upgradeActorHandler(h.CreateDeploymentBatch), "POST", "/batches", removalPayload(cs[0].ID), 0)
	if w.Code != http.StatusAccepted {
		t.Fatalf("create %d: %s", w.Code, w.Body)
	}
	var step models.DeploymentStackStep
	if err := h.db.First(&step).Error; err != nil {
		t.Fatal(err)
	}
	if err := h.db.Model(&step).Update("status", "running").Error; err != nil {
		t.Fatal(err)
	}
	step.Status = "running"
	if err := h.runStackStep(context.Background(), &step); err != nil {
		t.Fatalf("run: %v", err)
	}
	var component models.Component
	if err := h.db.First(&component, cs[0].ID).Error; err != nil {
		t.Fatal(err)
	}
	if component.Status != "removed" || step.Result != "released" || step.OperationID == nil {
		t.Fatalf("missing verified removal receipt: %+v %+v", component, step)
	}
	if err := h.runStackStep(context.Background(), &step); err != nil {
		t.Fatalf("resume repeated or failed: %v", err)
	}
}

func TestRemovalBatchExecutionRejectsChangedAuthorityAndTarget(t *testing.T) {
	for _, scenario := range []string{"inactive_actor", "observe_ownership", "host_endpoint", "binary_path", "plan_tamper", "held_claim"} {
		t.Run(scenario, func(t *testing.T) {
			h, cs := removalBatchFixture(t)
			w := batchRequest(t, upgradeActorHandler(h.CreateDeploymentBatch), "POST", "/batches", removalPayload(cs[0].ID), 0)
			if w.Code != http.StatusAccepted {
				t.Fatalf("queue %d: %s", w.Code, w.Body)
			}
			var step models.DeploymentStackStep
			if err := h.db.First(&step).Error; err != nil {
				t.Fatal(err)
			}
			if err := h.db.Model(&step).Update("status", "running").Error; err != nil {
				t.Fatal(err)
			}
			step.Status = "running"
			switch scenario {
			case "inactive_actor":
				h.db.Exec("UPDATE users SET is_active = 0 WHERE id = ?", uint(1))
			case "observe_ownership":
				h.db.Create(&models.ComponentAdoption{ComponentID: cs[0].ID, Ownership: "observe"})
			case "host_endpoint":
				h.db.Model(&models.Host{}).Where("id = ?", cs[0].HostID).Update("ip", "127.0.0.2")
			case "binary_path":
				h.db.Model(&models.Component{}).Where("id = ?", cs[0].ID).Update("binary_path", "/tmp/unrelated")
			case "plan_tamper":
				h.db.Model(&models.ChangePlan{}).Where("id = ?", *step.ChangePlanID).Update("reason", "changed")
			case "held_claim":
				h.db.Create(&models.ComponentOperationClaim{ComponentID: cs[0].ID, Token: "other-operation", Kind: "upgrade"})
			}
			if err := h.runStackStep(context.Background(), &step); err == nil {
				t.Fatal("queued removal ignored changed authority or target")
			}
			var component models.Component
			if err := h.db.First(&component, cs[0].ID).Error; err != nil {
				t.Fatal(err)
			}
			if component.Status == "removed" {
				t.Fatal("unsafe removal changed component")
			}
			var count int64
			h.db.Model(&models.Operation{}).Count(&count)
			if count != 0 {
				t.Fatalf("unsafe removal created %d operations", count)
			}
		})
	}
}

func TestRemovalBatchVerifiedPlanReconcilesReceiptWithoutReplay(t *testing.T) {
	h, cs := removalBatchFixture(t)
	w := batchRequest(t, upgradeActorHandler(h.CreateDeploymentBatch), "POST", "/batches", removalPayload(cs[0].ID), 0)
	if w.Code != http.StatusAccepted {
		t.Fatalf("queue %d: %s", w.Code, w.Body)
	}
	var step models.DeploymentStackStep
	if err := h.db.First(&step).Error; err != nil {
		t.Fatal(err)
	}
	h.db.Model(&step).Update("status", "running")
	step.Status = "running"
	if err := h.runStackStep(context.Background(), &step); err != nil {
		t.Fatal(err)
	}
	originalOperation := *step.OperationID
	h.db.Model(&step).Updates(map[string]interface{}{"operation_id": nil, "result": ""})
	step.OperationID = nil
	step.Result = ""
	if err := h.runStackStep(context.Background(), &step); err != nil {
		t.Fatalf("reconcile verified plan: %v", err)
	}
	if step.OperationID == nil || *step.OperationID != originalOperation || step.Result != "released" {
		t.Fatalf("verified receipt mismatch: %+v", step)
	}
	var count int64
	h.db.Model(&models.Operation{}).Count(&count)
	if count != 1 {
		t.Fatalf("verified plan replayed remote operation: %d", count)
	}
}
