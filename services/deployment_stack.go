package services

import (
	"context"
	"errors"
	"github.com/promlifecycle/platform/models"
	"gorm.io/gorm"
	"strings"
	"time"
)

// StackStepRunner returns only after the linked deployment operation settles.
type StackStepRunner func(context.Context, *models.DeploymentStackStep) error

// StackPreflightFailure carries check identifiers, never request values or command output.
type StackPreflightFailure struct{ Checks []string }

func (e *StackPreflightFailure) Error() string {
	return "部署预检未通过（" + strings.Join(e.Checks, ", ") + "）；请修正对应预检项后重试，尚未创建部署操作"
}

type DeploymentStackService struct {
	db  *gorm.DB
	run StackStepRunner
}

func NewDeploymentStackService(db *gorm.DB, run StackStepRunner) *DeploymentStackService {
	return &DeploymentStackService{db: db, run: run}
}
func (s *DeploymentStackService) Get(id uint) (*models.DeploymentStack, error) {
	var stack models.DeploymentStack
	err := s.db.Preload("Steps", func(tx *gorm.DB) *gorm.DB { return tx.Order("position ASC") }).First(&stack, id).Error
	return &stack, err
}
func (s *DeploymentStackService) Retry(id uint) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		var stack models.DeploymentStack
		if err := tx.First(&stack, id).Error; err != nil {
			return err
		}
		if stack.Status != "failed" && stack.Status != "interrupted" {
			return errors.New("only failed or interrupted stacks can be retried")
		}
		result := tx.Model(&stack).Where("status IN ?", []string{"failed", "interrupted"}).Updates(map[string]interface{}{"status": "pending", "last_error": ""})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("stack already claimed")
		}
		return tx.Model(&models.DeploymentStackStep{}).Where("stack_id = ? AND status IN ?", id, []string{"failed", "interrupted"}).Updates(map[string]interface{}{"status": "pending", "error": ""}).Error
	})
}
func (s *DeploymentStackService) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		var stack models.DeploymentStack
		next := s.db.Where("status = ?", "pending").Order("id ASC").Limit(1).Find(&stack)
		if next.Error == nil && next.RowsAffected != 0 {
			s.execute(ctx, stack.ID)
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (s *DeploymentStackService) execute(ctx context.Context, id uint) {
	result := s.db.Model(&models.DeploymentStack{}).Where("id = ? AND status = ?", id, "pending").Update("status", "running")
	if result.Error != nil || result.RowsAffected != 1 {
		return
	}
	stack, err := s.Get(id)
	if err != nil {
		s.fail(id, nil, err)
		return
	}
	for i := range stack.Steps {
		step := &stack.Steps[i]
		if step.Status == "success" {
			continue
		}
		if ctx.Err() != nil {
			s.fail(id, step, ctx.Err())
			return
		}
		if err := s.db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Model(&models.DeploymentStack{}).Where("id = ?", id).Update("current_index", step.Position).Error; err != nil {
				return err
			}
			return tx.Model(step).Updates(map[string]interface{}{"status": "running", "error": ""}).Error
		}); err != nil {
			s.fail(id, step, err)
			return
		}
		if err := s.run(ctx, step); err != nil {
			s.fail(id, step, err)
			return
		}
		if err := s.db.Model(step).Updates(map[string]interface{}{"status": "success", "error": ""}).Error; err != nil {
			s.fail(id, step, err)
			return
		}
	}
	if err := s.db.Model(&models.DeploymentStack{}).Where("id = ?", id).Updates(map[string]interface{}{"status": "success", "current_index": stack.Total, "last_error": ""}).Error; err != nil {
		s.fail(id, nil, err)
	}
}
func (s *DeploymentStackService) fail(id uint, step *models.DeploymentStackStep, cause error) {
	status := "failed"
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		status = "interrupted"
	}
	// Error details live on the component/operation protected DTO. Never persist
	// a remote command containing supplied secrets in the public queue DTO.
	message := "Deployment step did not complete; inspect its operation and retry explicitly"
	var preflight *StackPreflightFailure
	if errors.As(cause, &preflight) {
		message = preflight.Error()
	}
	s.db.Transaction(func(tx *gorm.DB) error {
		if step != nil {
			if err := tx.Model(step).Updates(map[string]interface{}{"status": status, "error": message}).Error; err != nil {
				return err
			}
		}
		return tx.Model(&models.DeploymentStack{}).Where("id = ?", id).Updates(map[string]interface{}{"status": status, "last_error": message}).Error
	})
}
