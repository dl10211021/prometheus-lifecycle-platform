package services

import (
	"encoding/json"
	"time"

	"github.com/promlifecycle/platform/models"
	"gorm.io/gorm"
)

// OperationService owns durable lifecycle-task progress updates so HTTP
// handlers and background schedulers use exactly the same task contract.
type OperationService struct {
	db *gorm.DB
}

func NewOperationService(db *gorm.DB) *OperationService {
	return &OperationService{db: db}
}

func (s *OperationService) Create(componentID uint, clusterID *uint, operationType, status string, input interface{}) *models.Operation {
	now := time.Now()
	initialEvent, _ := json.Marshal([]models.OperationStageEvent{{
		Stage: "queued", Message: "任务已创建，等待执行", Progress: 0, CreatedAt: now,
	}})
	id := componentID
	operation := models.Operation{
		ComponentID: &id, ClusterID: clusterID, Type: operationType, Status: status,
		Stage: "queued", Progress: 0, StageHistory: string(initialEvent),
	}
	if input != nil {
		if encoded, err := json.Marshal(input); err == nil {
			operation.Input = string(encoded)
		}
	}
	if err := s.db.Create(&operation).Error; err != nil {
		operation.ID = 0
	}
	return &operation
}

func (s *OperationService) Progress(id uint, stage, message string, progress int) {
	if progress < 0 {
		progress = 0
	}
	if progress > 100 {
		progress = 100
	}
	now := time.Now()
	_ = s.db.Transaction(func(tx *gorm.DB) error {
		var operation models.Operation
		if err := tx.First(&operation, id).Error; err != nil {
			return err
		}
		var history []models.OperationStageEvent
		if operation.StageHistory != "" {
			_ = json.Unmarshal([]byte(operation.StageHistory), &history)
		}
		if len(history) == 0 || history[len(history)-1].Stage != stage || history[len(history)-1].Message != message {
			history = append(history, models.OperationStageEvent{
				Stage: stage, Message: message, Progress: progress, CreatedAt: now,
			})
		}
		encoded, err := json.Marshal(history)
		if err != nil {
			return err
		}
		updates := map[string]interface{}{
			"status": "running", "stage": stage, "progress": progress,
			"message": message, "stage_history": string(encoded), "updated_at": now,
		}
		if operation.StartedAt == nil {
			updates["started_at"] = &now
		}
		return tx.Model(&models.Operation{}).Where("id = ?", id).Updates(updates).Error
	})
}

func (s *OperationService) Complete(id uint, success bool, message string, operationErr error) {
	stage := "completed"
	if !success {
		stage = "failed"
	}
	s.Progress(id, stage, message, 100)
	now := time.Now()
	updates := map[string]interface{}{
		"status": map[bool]string{true: "success", false: "failed"}[success],
		"stage":  stage, "progress": 100, "message": message,
		"updated_at": now, "completed_at": &now,
	}
	if operationErr != nil {
		updates["output"] = operationErr.Error()
	}
	_ = s.db.Model(&models.Operation{}).Where("id = ?", id).Updates(updates).Error
}
