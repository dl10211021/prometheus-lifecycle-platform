package services

import (
	"context"
	"errors"
	"fmt"

	"github.com/promlifecycle/platform/models"
	"gorm.io/gorm"
)

// A completed full backup supplies the pause row. Commit the independent barrier
// after final admission and before even the first version metadata write.
func (s *ChangeService) beginPrometheusTransition(ctx context.Context, plan *models.ChangePlan, component *models.Component) error {
	if plan.ID == 0 {
		return errors.Join(ErrPrometheusMigrationAdmission, errors.New("version transition requires a persisted plan"))
	}
	result := s.db.WithContext(ctx).Model(&ComponentBackupPause{}).
		Where("component_id = ? AND status = ? AND version_transition_plan_id = 0", component.ID, "resumed").
		Update("version_transition_plan_id", plan.ID)
	if result.Error != nil {
		return errors.Join(ErrPrometheusMigrationAdmission, fmt.Errorf("persist Prometheus version transition barrier: %w", result.Error))
	}
	if result.RowsAffected != 1 {
		return errors.Join(ErrPrometheusMigrationAdmission, errors.New("Prometheus version transition requires a completed full backup and no existing transition"))
	}
	return nil
}

// Never defer this release: process failure, uncertain recovery and failed final
// records must leave the durable barrier for startup and subsequent claims.
func (s *ChangeService) finishPrometheusTransition(plan *models.ChangePlan) error {
	result := s.db.Model(&ComponentBackupPause{}).
		Where("component_id = ? AND version_transition_plan_id = ? AND status = ?", plan.ResourceID, plan.ID, "resumed").
		Update("version_transition_plan_id", uint(0))
	if result.Error != nil {
		return fmt.Errorf("Prometheus version transition remains blocked; failed to clear barrier: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return errors.New("Prometheus version transition remains blocked; owning plan or safe resumed state could not be confirmed")
	}
	return nil
}

// Diagnostic writes are best effort; the previously persisted barrier provides
// safety even when the same database outage also prevents these writes.
func (s *ChangeService) recordPrometheusTransitionFailure(plan *models.ChangePlan, op *models.Operation, cause error) error {
	failure := fmt.Errorf("Prometheus version transition remains blocked pending recovery: %w", cause)
	s.markFailed(plan.ID, failure)
	_ = s.db.Model(op).Updates(map[string]interface{}{"status": "failed", "message": failure.Error()}).Error
	s.observeChangeFailure(plan, nil, "version_transition", failure, nil)
	return failure
}

func prometheusTransitionRecordError(result *gorm.DB, record string) error {
	if result.Error != nil {
		return fmt.Errorf("save Prometheus transition %s: %w", record, result.Error)
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("save Prometheus transition %s: expected one durable record, updated %d", record, result.RowsAffected)
	}
	return nil
}
