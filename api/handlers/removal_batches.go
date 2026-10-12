package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/core/deployer"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/credentials"
	"github.com/promlifecycle/platform/pkg/utils"
	"github.com/promlifecycle/platform/services"
	"gorm.io/gorm"
)

type queuedRemoval struct {
	PlanDigest  string `json:"plan_digest"`
	Fingerprint string `json:"fingerprint"`
}

func removalMode(plan *models.ChangePlan) string {
	var selected struct {
		Mode string `json:"mode"`
	}
	if json.Unmarshal(plan.After, &selected) != nil {
		return ""
	}
	return selected.Mode
}

func removalResult(mode string) string {
	if mode == "keep_service" {
		return "released"
	}
	return "uninstalled_keep_data"
}

func (h *Handler) createRemovalBatch(c *gin.Context, req deploymentBatchRequest) {
	req.Name = strings.TrimSpace(req.Name)
	req.Reason = strings.TrimSpace(req.Reason)
	if req.Kind != "remove" || (req.Mode != "keep_service" && req.Mode != "keep_data") || len(req.Stacks) != 0 || len(req.Adoptions) != 0 || len(req.Upgrades) != 0 || len(req.ComponentIDs) < 1 || len(req.ComponentIDs) > 200 || req.Name == "" || len(req.Name) > 256 || req.Reason == "" || len(req.Reason) > 2000 || len(req.RequestID) < 8 || len(req.RequestID) > 128 || !utils.SafePathSegment(req.RequestID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请明确移除方式、原因和 1–200 个受管实例（最多 50 台主机）；批量不支持删除数据"})
		return
	}
	actor, err := h.powerActor(changeActor(c).ID)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "移除发起人已无变更权限"})
		return
	}
	raw, _ := json.Marshal(req)
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	if h.existingDeploymentBatch(c, req.RequestID, digest) {
		return
	}
	prepared := make([]*services.PreparedRemoval, 0, len(req.ComponentIDs))
	positions := map[uint]bool{}
	seen := map[uint]bool{}
	service := h.changeService()
	for _, id := range req.ComponentIDs {
		if id == 0 || seen[id] {
			c.JSON(400, gin.H{"error": "实例 ID 不能为空或重复"})
			return
		}
		seen[id] = true
		item, err := service.PrepareRemovalPlan(c.Request.Context(), id, req.Mode, req.Reason, services.ChangeActor{ID: actor.ID, Username: actor.Username, Role: actor.Role})
		if err != nil {
			c.JSON(409, gin.H{"error": fmt.Sprintf("组件 %d 移除预检失败: %s", id, err)})
			return
		}
		prepared = append(prepared, item)
		positions[item.Component.HostID] = true
		if len(positions) > 50 {
			c.JSON(400, gin.H{"error": "一个批次最多包含 50 台主机"})
			return
		}
	}
	batch := models.DeploymentBatch{Kind: "remove", Name: req.Name, RequestID: req.RequestID, RequestDigest: digest}
	err = h.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Omit("Stacks").Create(&batch).Error; err != nil {
			return err
		}
		stackPositions := map[uint]int{}
		for _, item := range prepared {
			if err := tx.Create(&item.Plan).Error; err != nil {
				return err
			}
			item.Baseline.PlanID = item.Plan.ID
			if err := tx.Create(&item.Baseline).Error; err != nil {
				return err
			}
			pos, ok := stackPositions[item.Component.HostID]
			if !ok {
				pos = len(batch.Stacks)
				stackPositions[item.Component.HostID] = pos
				batch.Stacks = append(batch.Stacks, models.DeploymentStack{BatchID: &batch.ID, HostID: item.Component.HostID, Name: req.Name, TemplateKey: "remove", Status: "pending"})
			}
			payload, _ := json.Marshal(queuedRemoval{PlanDigest: services.RemovalPlanDigest(&item.Plan), Fingerprint: item.Fingerprint})
			encrypted, err := credentials.Encrypt(string(payload))
			if err != nil {
				return err
			}
			componentID, planID := item.Component.ID, item.Plan.ID
			stack := &batch.Stacks[pos]
			stack.Steps = append(stack.Steps, models.DeploymentStackStep{Action: "remove", Position: len(stack.Steps), Type: string(item.Component.Type), Port: item.Component.Port, Status: "pending", ComponentID: &componentID, ChangePlanID: &planID, ChangePlanHistory: []uint{planID}, EncryptedRequest: encrypted})
			stack.Total = len(stack.Steps)
		}
		return tx.Create(&batch.Stacks).Error
	})
	if err != nil {
		if h.existingDeploymentBatch(c, req.RequestID, digest) {
			return
		}
		c.JSON(503, gin.H{"error": "移除批次未确认入库，请使用相同提交标识查询或重试"})
		return
	}
	summarizeDeploymentBatch(&batch)
	c.JSON(202, gin.H{"batch": batch})
}

func (h *Handler) finishRemovalReceipt(ctx context.Context, step *models.DeploymentStackStep, plan *models.ChangePlan, mode string) error {
	if plan.Status != models.ChangeStatusVerified || plan.OperationID == nil {
		return errors.New("移除计划尚未完成验证")
	}
	var op models.Operation
	if err := h.db.First(&op, *plan.OperationID).Error; err != nil {
		return err
	}
	if op.Status != "success" || op.Type != "change-remove" || op.ComponentID == nil || step.ComponentID == nil || *op.ComponentID != *step.ComponentID {
		return errors.New("移除执行记录不匹配")
	}
	var component models.Component
	if err := h.db.First(&component, *step.ComponentID).Error; err != nil {
		return err
	}
	if component.Status != "removed" {
		return errors.New("组件移除状态未确认")
	}
	if step.OperationID != nil && *step.OperationID != op.ID {
		return errors.New("移除步骤关联了其他执行记录")
	}
	// A verified plan can be revisited after a lost worker acknowledgement.
	// Repeat only the idempotent integration sync, never the remote removal.
	if h.autoConfig != nil && component.Type != models.ComponentPrometheus {
		var runningPrometheus int64
		if err := h.db.Model(&models.Component{}).Where("cluster_id = ? AND type = ? AND status = ?", component.ClusterID, models.ComponentPrometheus, models.StatusRunning).Count(&runningPrometheus).Error; err != nil {
			return err
		}
		if runningPrometheus > 0 {
			if err := h.autoConfig.SyncPrometheusConfig(component.ClusterID, h.basePath); err != nil {
				return fmt.Errorf("组件已移除，但 Prometheus 配置同步失败: %w", err)
			}
		}
	}
	if component.Type == models.ComponentLoki {
		var grafana models.Component
		err := h.db.Preload("Host").Where("cluster_id = ? AND type = ? AND status = ?", component.ClusterID, models.ComponentGrafana, models.StatusRunning).First(&grafana).Error
		if err == nil {
			if err := deployer.NewBinaryDeployer(h.db, h.basePath).SyncGrafanaDatasource(ctx, &grafana); err != nil {
				return fmt.Errorf("Loki 已移除，但 Grafana 数据源同步失败: %w", err)
			}
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
	}
	result := h.db.Model(&models.DeploymentStackStep{}).Where("id = ? AND status = ? AND change_plan_id = ? AND component_id = ?", step.ID, "running", plan.ID, *step.ComponentID).Updates(map[string]interface{}{"operation_id": op.ID, "result": removalResult(mode)})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errors.New("移除步骤已改变")
	}
	step.OperationID = &op.ID
	step.Result = removalResult(mode)
	return nil
}

func (h *Handler) runRemovalStackStep(ctx context.Context, step *models.DeploymentStackStep) error {
	if step.ChangePlanID == nil || step.ComponentID == nil || step.Action != "remove" {
		return errors.New("remove step has no linked component/plan")
	}
	plain, err := credentials.Decrypt(step.EncryptedRequest)
	if err != nil {
		return err
	}
	var queued queuedRemoval
	if err := json.Unmarshal([]byte(plain), &queued); err != nil {
		return err
	}
	if queued.PlanDigest == "" || queued.Fingerprint == "" {
		return errors.New("remove request identity missing")
	}
	var stored models.DeploymentStackStep
	if err := h.db.First(&stored, step.ID).Error; err != nil {
		return err
	}
	var stack models.DeploymentStack
	if err := h.db.First(&stack, stored.StackID).Error; err != nil {
		return err
	}
	if stored.Status != "running" || stored.Action != "remove" || stored.ComponentID == nil || *stored.ComponentID != *step.ComponentID || stored.ChangePlanID == nil || *stored.ChangePlanID != *step.ChangePlanID || stored.EncryptedRequest != step.EncryptedRequest || stack.HostID == 0 || stack.HostID != storedHostID(h.db, *step.ComponentID) {
		return errors.New("remove step identity changed")
	}
	service := h.changeService()
	plan, err := service.Get(*step.ChangePlanID)
	if err != nil {
		return err
	}
	mode := removalMode(plan)
	if !plan.QueueManaged || plan.Kind != models.ChangeKindRemove || plan.ResourceID != *step.ComponentID || (mode != "keep_service" && mode != "keep_data") || services.RemovalPlanDigest(plan) != queued.PlanDigest {
		return errors.New("queued remove plan changed")
	}
	if plan.Status == models.ChangeStatusVerified {
		return h.finishRemovalReceipt(ctx, step, plan, mode)
	}
	if plan.Status == models.ChangeStatusExecuting {
		return errors.New("移除执行结果尚未确定，请先核对变更计划")
	}
	if plan.Status == models.ChangeStatusFailed && plan.OperationID != nil {
		return errors.New("请先恢复失败的移除变更再重试批次")
	}
	if plan.Status == models.ChangeStatusFailed || plan.Status == models.ChangeStatusRolledBack {
		var user models.User
		if err := h.db.First(&user, plan.RequestedByID).Error; err != nil || !user.IsActive || (user.Role != "change_admin" && user.Role != "system_admin") {
			return errors.New("移除发起人已无变更权限")
		}
		next, err := service.PrepareRemovalPlan(ctx, *step.ComponentID, mode, plan.Reason, services.ChangeActor{ID: user.ID, Username: user.Username, Role: user.Role})
		if err != nil {
			return err
		}
		if next.Fingerprint != queued.Fingerprint {
			return services.ErrStaleChangePlan
		}
		oldID := plan.ID
		err = h.db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Create(&next.Plan).Error; err != nil {
				return err
			}
			next.Baseline.PlanID = next.Plan.ID
			if err := tx.Create(&next.Baseline).Error; err != nil {
				return err
			}
			payload, _ := json.Marshal(queuedRemoval{PlanDigest: services.RemovalPlanDigest(&next.Plan), Fingerprint: queued.Fingerprint})
			encrypted, err := credentials.Encrypt(string(payload))
			if err != nil {
				return err
			}
			history := append(append([]uint{}, step.ChangePlanHistory...), next.Plan.ID)
			historyJSON, _ := json.Marshal(history)
			updated := tx.Model(&models.DeploymentStackStep{}).Where("id = ? AND status = ? AND change_plan_id = ?", step.ID, "running", oldID).Updates(map[string]interface{}{"change_plan_id": next.Plan.ID, "change_plan_history": string(historyJSON), "encrypted_request": encrypted, "operation_id": nil, "result": ""})
			if updated.Error != nil {
				return updated.Error
			}
			if updated.RowsAffected != 1 {
				return errors.New("重试时移除步骤已改变")
			}
			step.ChangePlanID = &next.Plan.ID
			step.ChangePlanHistory = history
			step.EncryptedRequest = encrypted
			step.OperationID = nil
			step.Result = ""
			return nil
		})
		if err != nil {
			return err
		}
		plan = &next.Plan
		queued.PlanDigest = services.RemovalPlanDigest(plan)
	}
	if plan.Status != models.ChangeStatusDraft {
		return fmt.Errorf("unsupported queued remove status %s", plan.Status)
	}
	executeCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	_, executeErr := service.ExecuteQueuedRemoval(executeCtx, plan.ID, queued.PlanDigest, queued.Fingerprint, mode)
	settled, readErr := service.Get(plan.ID)
	if readErr != nil {
		return readErr
	}
	if settled.Status == models.ChangeStatusVerified {
		return h.finishRemovalReceipt(ctx, step, settled, mode)
	}
	if executeErr != nil {
		return executeErr
	}
	return errors.New("移除计划未通过验证")
}

func storedHostID(db *gorm.DB, componentID uint) uint {
	var component models.Component
	if db.Select("host_id").First(&component, componentID).Error != nil {
		return 0
	}
	return component.HostID
}
