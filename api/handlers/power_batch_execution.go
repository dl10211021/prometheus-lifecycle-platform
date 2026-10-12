package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/credentials"
	"github.com/promlifecycle/platform/services"
	"gorm.io/gorm"
	"net/http"
)

type powerStepContextKey struct{}
type powerStepExecution struct {
	StepID  uint
	Request queuedPower
}

func (h *Handler) runPowerStackStep(ctx context.Context, step *models.DeploymentStackStep) error {
	plain, err := credentials.Decrypt(step.EncryptedRequest)
	if err != nil {
		return err
	}
	var q queuedPower
	if err = json.Unmarshal([]byte(plain), &q); err != nil {
		return err
	}
	if q.Action != step.Action || (q.Action != "start" && q.Action != "stop") || step.ComponentID == nil || *step.ComponentID != q.ComponentID || q.Fingerprint == "" {
		return errors.New("queued power identity changed")
	}
	var stored models.DeploymentStackStep
	if err = h.db.First(&stored, step.ID).Error; err != nil {
		return err
	}
	if stored.ComponentID == nil || *stored.ComponentID != q.ComponentID || stored.Action != q.Action || stored.EncryptedRequest != step.EncryptedRequest {
		return errors.New("queued power step changed")
	}
	step.OperationID = stored.OperationID
	step.Result = stored.Result
	if step.OperationID != nil {
		var op models.Operation
		if err = h.db.First(&op, *step.OperationID).Error; err != nil {
			return err
		}
		if op.ComponentID == nil || *op.ComponentID != q.ComponentID || op.Type != q.Action {
			return errors.New("power receipt belongs to another operation")
		}
		if op.Status == "success" {
			if step.Result != powerResult(q.Action) {
				return errors.New("power receipt is incomplete")
			}
			return nil
		}
		if op.Status != "failed" && op.Status != "interrupted" {
			return errors.New("power operation outcome is not settled")
		}
	}
	execution := powerStepExecution{StepID: step.ID, Request: q}
	req, err := http.NewRequestWithContext(context.WithValue(ctx, powerStepContextKey{}, execution), http.MethodPost, fmt.Sprintf("http://internal/components/%d/power", q.ComponentID), nil)
	if err != nil {
		return err
	}
	response := &deploymentResponse{header: make(http.Header)}
	engine := gin.New()
	engine.POST("/components/:id/power", func(c *gin.Context) {
		if q.Action == "start" {
			h.StartComponent(c)
		} else {
			h.StopComponent(c)
		}
	})
	engine.ServeHTTP(response, req)
	if err = h.db.First(&stored, step.ID).Error; err != nil {
		return err
	}
	step.OperationID = stored.OperationID
	step.Result = stored.Result
	if response.status < 200 || response.status >= 300 {
		if step.OperationID == nil {
			return &services.StackPreflightFailure{Checks: []string{"启停准入：权限、所有权、实例身份或操作冲突，请核对后重试"}}
		}
		return errors.New("power operation failed; inspect its linked operation")
	}
	if step.OperationID == nil || step.Result != powerResult(q.Action) {
		return errors.New("power result was not persisted")
	}
	return nil
}
func powerResult(action string) string {
	if action == "start" {
		return "started"
	}
	return "stopped"
}

// The single-instance handler has acquired the component claim. Link the
// operation to its durable step before sending a remote command.
func (h *Handler) preparePowerOperation(ctx context.Context, c *models.Component, action string) (*models.Operation, error) {
	execution, batch := ctx.Value(powerStepContextKey{}).(powerStepExecution)
	if !batch {
		return h.createOperation(c.ID, &c.ClusterID, action, "pending", "api", nil), nil
	}
	q := execution.Request
	if q.ComponentID != c.ID || q.Action != action {
		return nil, errors.New("启停队列身份不一致")
	}
	actor, err := h.powerActor(q.ActorID)
	if err != nil {
		return nil, err
	}
	current, err := h.powerTarget(c.ID)
	if err != nil {
		return nil, err
	}
	if powerIdentity(current) != q.Fingerprint {
		return nil, errors.New("组件或主机身份已改变，请重新创建批次")
	}
	var step models.DeploymentStackStep
	if err = h.db.First(&step, execution.StepID).Error; err != nil {
		return nil, err
	}
	var stack models.DeploymentStack
	if err = h.db.First(&stack, step.StackID).Error; err != nil {
		return nil, err
	}
	if step.Status != "running" || step.ComponentID == nil || *step.ComponentID != current.ID || step.Action != action || step.Type != string(current.Type) || step.Port != current.Port || stack.HostID != current.HostID {
		return nil, errors.New("启停步骤已改变")
	}
	*c = *current
	var op *models.Operation
	err = h.db.Transaction(func(tx *gorm.DB) error {
		if step.OperationID != nil {
			var previous models.Operation
			if err := tx.First(&previous, *step.OperationID).Error; err != nil {
				return err
			}
			if previous.ComponentID == nil || *previous.ComponentID != current.ID || previous.Type != action || (previous.Status != "failed" && previous.Status != "interrupted") {
				return errors.New("前一次启停结果尚未确定")
			}
		}
		op = services.NewOperationService(tx).Create(current.ID, &current.ClusterID, action, "pending", map[string]interface{}{"source": "batch", "actor_id": actor.ID, "username": actor.Username, "step_id": step.ID, "reason": q.Reason})
		if op.ID == 0 {
			return errors.New("保存启停操作失败")
		}
		update := tx.Model(&models.DeploymentStackStep{}).Where("id = ? AND status = ? AND action = ? AND component_id = ?", step.ID, "running", action, current.ID)
		if step.OperationID == nil {
			update = update.Where("operation_id IS NULL")
		} else {
			update = update.Where("operation_id = ?", *step.OperationID)
		}
		result := update.Updates(map[string]interface{}{"operation_id": op.ID, "result": ""})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("启停步骤已被其他任务修改")
		}
		return nil
	})
	if err != nil {
		return nil, errors.New("启停任务关联未保存，请核对平台存储和任务状态")
	}
	return op, nil
}

// Commit state, operation success and the receipt together. A worker resuming
// after this commit must never repeat the service command.
func (h *Handler) completePowerOperation(ctx context.Context, c *models.Component, op *models.Operation, status models.ComponentStatus, action string) error {
	execution, batch := ctx.Value(powerStepContextKey{}).(powerStepExecution)
	if !batch {
		if err := persistComponentPowerState(h.db, c, status); err != nil {
			return err
		}
		h.updateOperation(op.ID, true, "组件已"+action, nil)
		return nil
	}
	return h.db.Transaction(func(tx *gorm.DB) error {
		if err := persistComponentPowerState(tx, c, status); err != nil {
			return err
		}
		services.NewOperationService(tx).Complete(op.ID, true, "组件已"+action, nil)
		var receipt models.Operation
		if err := tx.First(&receipt, op.ID).Error; err != nil {
			return err
		}
		if receipt.Status != "success" {
			return errors.New("启停操作结果未保存")
		}
		result := tx.Model(&models.DeploymentStackStep{}).Where("id = ? AND status = ? AND operation_id = ?", execution.StepID, "running", op.ID).Update("result", powerResult(execution.Request.Action))
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("启停步骤关联已改变")
		}
		return nil
	})
}
