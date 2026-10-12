package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/credentials"
	"github.com/promlifecycle/platform/pkg/utils"
	"github.com/promlifecycle/platform/services"
	"gorm.io/gorm"
	"strings"
	"time"
)

type batchUpgradeRequest struct {
	ComponentID    uint   `json:"component_id"`
	CurrentVersion string `json:"current_version"`
	NewVersion     string `json:"new_version"`
}
type queuedUpgrade struct {
	PlanDigest        string `json:"plan_digest"`
	Fingerprint       string `json:"fingerprint"`
	TargetFingerprint string `json:"target_fingerprint"`
}
type upgradeBatchCheck struct {
	ComponentID    uint   `json:"component_id"`
	HostID         uint   `json:"host_id"`
	Name           string `json:"name"`
	Type           string `json:"type"`
	CurrentVersion string `json:"current_version"`
	NewVersion     string `json:"new_version"`
	Passed         bool   `json:"passed"`
	Risk           string `json:"risk,omitempty"`
	Error          string `json:"error,omitempty"`
}

func validateUpgradeBatch(req *deploymentBatchRequest) error {
	req.Name = strings.TrimSpace(req.Name)
	req.Reason = strings.TrimSpace(req.Reason)
	if req.Kind != "upgrade" || req.Mode != "" || len(req.ComponentIDs) > 0 || len(req.Stacks) > 0 || len(req.Adoptions) > 0 || len(req.Upgrades) < 1 || len(req.Upgrades) > 200 || req.Name == "" || len(req.Name) > 256 || req.Reason == "" || len(req.Reason) > 2000 {
		return errors.New("请填写批次名称、升级原因，并选择 1–200 个已有组件实例（最多 50 台主机）")
	}
	seen := map[uint]bool{}
	for _, item := range req.Upgrades {
		if item.ComponentID == 0 || seen[item.ComponentID] || item.CurrentVersion == "" || !utils.ValidVersion(item.NewVersion) {
			return errors.New("组件不能重复，当前版本和目标版本必须有效")
		}
		seen[item.ComponentID] = true
	}
	return nil
}
func (h *Handler) prepareUpgradeBatch(ctx context.Context, req deploymentBatchRequest, actor services.ChangeActor) ([]*services.PreparedUpgrade, []upgradeBatchCheck, bool) {
	prepared := make([]*services.PreparedUpgrade, len(req.Upgrades))
	checks := make([]upgradeBatchCheck, 0, len(req.Upgrades))
	passed := true
	hosts := map[uint]bool{}
	for i, item := range req.Upgrades {
		check := upgradeBatchCheck{ComponentID: item.ComponentID, CurrentVersion: item.CurrentVersion, NewVersion: item.NewVersion}
		plan, err := h.changeService().PrepareUpgradePlan(ctx, item.ComponentID, item.CurrentVersion, item.NewVersion, req.Reason, actor)
		if err != nil {
			check.Error = err.Error()
			passed = false
		} else {
			prepared[i] = plan
			check.HostID = plan.Component.HostID
			check.Name = plan.Component.Name
			check.Type = string(plan.Component.Type)
			check.Risk = plan.Plan.Risk
			check.Passed = true
			hosts[check.HostID] = true
			if len(hosts) > 50 {
				check.Passed = false
				check.Error = "一个批次最多包含 50 台主机"
				passed = false
			}
		}
		checks = append(checks, check)
	}
	return prepared, checks, passed
}
func (h *Handler) PreflightUpgradeBatch(c *gin.Context) {
	var req deploymentBatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": "批量升级请求格式无效"})
		return
	}
	if err := validateUpgradeBatch(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	_, checks, passed := h.prepareUpgradeBatch(c.Request.Context(), req, changeActor(c))
	c.JSON(200, gin.H{"passed": passed, "checks": checks})
}
func (h *Handler) createUpgradeBatch(c *gin.Context, req deploymentBatchRequest) {
	if err := validateUpgradeBatch(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	if len(req.RequestID) < 8 || len(req.RequestID) > 128 || !utils.SafePathSegment(req.RequestID) {
		c.JSON(400, gin.H{"error": "提交标识无效"})
		return
	}
	raw, _ := json.Marshal(req)
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	if h.existingDeploymentBatch(c, req.RequestID, digest) {
		return
	}
	prepared, checks, passed := h.prepareUpgradeBatch(c.Request.Context(), req, changeActor(c))
	if !passed {
		c.JSON(409, gin.H{"error": "升级预检未通过，未创建批次", "checks": checks})
		return
	}
	batch := models.DeploymentBatch{Kind: "upgrade", Name: req.Name, RequestID: req.RequestID, RequestDigest: digest}
	err := h.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&batch).Error; err != nil {
			return err
		}
		positions := map[uint]int{}
		for _, item := range prepared {
			if err := tx.Create(&item.Plan).Error; err != nil {
				return err
			}
			item.Baseline.PlanID = item.Plan.ID
			if err := tx.Create(&item.Baseline).Error; err != nil {
				return err
			}
			pos, ok := positions[item.Component.HostID]
			if !ok {
				pos = len(batch.Stacks)
				positions[item.Component.HostID] = pos
				batch.Stacks = append(batch.Stacks, models.DeploymentStack{BatchID: &batch.ID, HostID: item.Component.HostID, Name: req.Name, TemplateKey: "upgrade", Status: "pending"})
			}
			stack := &batch.Stacks[pos]
			var desired struct {
				NewVersion string `json:"new_version"`
			}
			if err := json.Unmarshal(item.Plan.After, &desired); err != nil {
				return err
			}
			payload, _ := json.Marshal(queuedUpgrade{PlanDigest: services.UpgradePlanDigest(&item.Plan), Fingerprint: item.Baseline.Fingerprint, TargetFingerprint: item.TargetFingerprint})
			encrypted, err := credentials.Encrypt(string(payload))
			if err != nil {
				return err
			}
			componentID, planID := item.Component.ID, item.Plan.ID
			stack.Steps = append(stack.Steps, models.DeploymentStackStep{Position: len(stack.Steps), Action: "upgrade", Type: string(item.Component.Type), Status: "pending", ComponentID: &componentID, ChangePlanID: &planID, ChangePlanHistory: []uint{planID}, FromVersion: item.Component.Version, TargetVersion: desired.NewVersion, EncryptedRequest: encrypted})
			stack.Total = len(stack.Steps)
		}
		return tx.Create(&batch.Stacks).Error
	})
	if err != nil {
		if h.existingDeploymentBatch(c, req.RequestID, digest) {
			return
		}
		c.JSON(503, gin.H{"error": "升级批次未确认入库，请使用相同提交标识查询或重试"})
		return
	}
	summarizeDeploymentBatch(&batch)
	c.JSON(202, gin.H{"batch": batch})
}
func (h *Handler) runUpgradeStackStep(ctx context.Context, step *models.DeploymentStackStep) error {
	if step.ChangePlanID == nil || step.ComponentID == nil {
		return errors.New("upgrade step has no linked component/plan")
	}
	plain, err := credentials.Decrypt(step.EncryptedRequest)
	if err != nil {
		return err
	}
	var queued queuedUpgrade
	if err := json.Unmarshal([]byte(plain), &queued); err != nil {
		return err
	}
	svc := h.changeService()
	plan, err := svc.Get(*step.ChangePlanID)
	if err != nil {
		return err
	}
	if plan.Kind != models.ChangeKindUpgrade || plan.ResourceID != *step.ComponentID || queued.PlanDigest != services.UpgradePlanDigest(plan) {
		return errors.New("queued upgrade plan changed; create a new batch")
	}
	if plan.Status == models.ChangeStatusVerified {
		return nil
	}
	var user models.User
	if err := h.db.First(&user, plan.RequestedByID).Error; err != nil {
		return errors.New("upgrade requester no longer exists")
	}
	actor := services.ChangeActor{ID: user.ID, Username: user.Username, Role: user.Role}
	if !user.IsActive || (actor.Role != "change_admin" && actor.Role != "system_admin") {
		return errors.New("upgrade requester no longer has change permission")
	}
	if plan.Status == models.ChangeStatusExecuting {
		return errors.New("upgrade execution outcome is not yet settled")
	}
	// A failed operation may have changed remote files. Only a verified rollback,
	// or failure before an operation was created, permits a fresh attempt.
	if plan.Status == models.ChangeStatusFailed && plan.OperationID != nil {
		return errors.New("recover the failed change before retrying this batch")
	}
	if plan.Status == models.ChangeStatusFailed || plan.Status == models.ChangeStatusRolledBack {
		next, err := svc.PrepareQueuedUpgradeRetry(ctx, *step.ComponentID, queued.TargetFingerprint, step.FromVersion, step.TargetVersion, plan.Reason, actor)
		if err != nil {
			if errors.Is(err, services.ErrStaleChangePlan) {
				return upgradeTargetPreflightFailure()
			}
			return err
		}
		if next.Baseline.Fingerprint != queued.Fingerprint {
			return upgradeTargetPreflightFailure()
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
			payload, _ := json.Marshal(queuedUpgrade{PlanDigest: services.UpgradePlanDigest(&next.Plan), Fingerprint: queued.Fingerprint, TargetFingerprint: queued.TargetFingerprint})
			encrypted, err := credentials.Encrypt(string(payload))
			if err != nil {
				return err
			}
			history := append(append([]uint{}, step.ChangePlanHistory...), next.Plan.ID)
			historyJSON, _ := json.Marshal(history)
			updated := tx.Model(&models.DeploymentStackStep{}).Where("id = ? AND status = ? AND change_plan_id = ?", step.ID, "running", oldID).Updates(map[string]interface{}{"change_plan_id": next.Plan.ID, "change_plan_history": string(historyJSON), "encrypted_request": encrypted, "operation_id": nil})
			if updated.Error != nil {
				return updated.Error
			}
			if updated.RowsAffected != 1 {
				return errors.New("upgrade step changed while retrying")
			}
			step.ChangePlanID = &next.Plan.ID
			step.ChangePlanHistory = history
			step.EncryptedRequest = encrypted
			step.OperationID = nil
			return nil
		})
		if err != nil {
			return err
		}
		plan = &next.Plan
		queued.PlanDigest = services.UpgradePlanDigest(plan)
	}
	if plan.Status != models.ChangeStatusDraft {
		return fmt.Errorf("unsupported queued upgrade status %s", plan.Status)
	}
	executeCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	_, executeErr := svc.ExecuteQueuedUpgrade(executeCtx, plan.ID, queued.PlanDigest, queued.Fingerprint, queued.TargetFingerprint, step.FromVersion, step.TargetVersion)
	settled, readErr := svc.Get(plan.ID)
	if readErr == nil && settled.OperationID != nil {
		step.OperationID = settled.OperationID
		if err := h.db.Model(&models.DeploymentStackStep{}).Where("id = ? AND change_plan_id = ?", step.ID, plan.ID).Update("operation_id", step.OperationID).Error; err != nil {
			return err
		}
	}
	if executeErr != nil {
		if errors.Is(executeErr, services.ErrStaleChangePlan) {
			return upgradeTargetPreflightFailure()
		}
		if readErr == nil && settled.Status == models.ChangeStatusDraft {
			return errors.Join(&services.StackPreflightFailure{Checks: []string{"已有实例升级：状态、健康、版本、安装包或磁盘；请重新预检"}}, executeErr)
		}
		return executeErr
	}
	if readErr != nil {
		return readErr
	}
	if settled.Status != models.ChangeStatusVerified {
		return errors.New("upgrade was not verified")
	}
	return nil
}

// Queue DTOs persist only the static preflight message, never target inputs or
// remote error details. Preserve the stale sentinel for callers and tests.
func upgradeTargetPreflightFailure() error {
	return errors.Join(&services.StackPreflightFailure{Checks: []string{"升级目标已变化或缺少绑定，请重新创建升级批次"}}, services.ErrStaleChangePlan)
}
