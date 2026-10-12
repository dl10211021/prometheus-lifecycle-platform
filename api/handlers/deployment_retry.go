package handlers

import (
	"encoding/json"
	"errors"

	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/credentials"
	"github.com/promlifecycle/platform/services"
	"gorm.io/gorm"
)

var errDeploymentRetryTargetChanged = errors.New("部署重试目标已移除或发生变化，请重新创建部署批次")

// Only a persisted queue step can authorize this binding. It is carried through
// the internal request context, never through client-controlled JSON or headers.
type deploymentRetryTargetContextKey struct{}
type deploymentRetryTarget struct {
	ComponentID uint
	HostID      uint
	Type        string
	DeployType  string
}

func deploymentRetryTargetForStep(step *models.DeploymentStackStep, request *componentCreateRequest) (deploymentRetryTarget, error) {
	if step.ComponentID == nil || *step.ComponentID == 0 || request.HostID == nil || *request.HostID == 0 || step.Type != request.Type {
		return deploymentRetryTarget{}, errDeploymentRetryTargetChanged
	}
	return deploymentRetryTarget{ComponentID: *step.ComponentID, HostID: *request.HostID, Type: request.Type, DeployType: request.DeployType}, nil
}

func (target deploymentRetryTarget) validate(component *models.Component) error {
	if component == nil || component.ID != target.ComponentID || component.HostID != target.HostID || string(component.Type) != target.Type || component.DeployType != target.DeployType || component.Status == "removed" {
		return errDeploymentRetryTargetChanged
	}
	return nil
}

func (target deploymentRetryTarget) load(db *gorm.DB) (*models.Component, error) {
	var component models.Component
	if err := db.Preload("Host").First(&component, target.ComponentID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errDeploymentRetryTargetChanged
		}
		return nil, err
	}
	if err := target.validate(&component); err != nil {
		return nil, err
	}
	return &component, nil
}

func deploymentRetryOperation(db *gorm.DB, step *models.DeploymentStackStep) (*models.Operation, error) {
	if step.OperationID == nil {
		return nil, nil
	}
	var operation models.Operation
	if err := db.First(&operation, *step.OperationID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errDeploymentRetryTargetChanged
		}
		return nil, err
	}
	if step.ComponentID == nil || operation.ComponentID == nil || *operation.ComponentID != *step.ComponentID || operation.Type != string(models.OpDeploy) {
		return nil, errDeploymentRetryTargetChanged
	}
	return &operation, nil
}

// The caller's transaction makes validation and requeueing atomic. Execution
// repeats this check because the target may change after the retry is queued.
func retryDeploymentStack(db *gorm.DB, id uint) error {
	var steps []models.DeploymentStackStep
	if err := db.Where("stack_id = ? AND status IN ?", id, []string{"failed", "interrupted"}).Find(&steps).Error; err != nil {
		return err
	}
	for i := range steps {
		step := &steps[i]
		// Legacy deployment rows may have SQL NULL in the later-added action column.
		if step.Action != "" {
			continue
		}
		operation, err := deploymentRetryOperation(db, step)
		if err != nil {
			return err
		}
		// A successful receipt only needs worker acknowledgement, no replay.
		if operation != nil && operation.Status == "success" {
			continue
		}
		if step.ComponentID == nil {
			continue
		}
		plain, err := credentials.Decrypt(step.EncryptedRequest)
		if err != nil {
			return err
		}
		var request componentCreateRequest
		if err := json.Unmarshal([]byte(plain), &request); err != nil {
			return err
		}
		target, err := deploymentRetryTargetForStep(step, &request)
		if err != nil {
			return err
		}
		if _, err := target.load(db); err != nil {
			return err
		}
	}
	return services.NewDeploymentStackService(db, nil).Retry(id)
}
