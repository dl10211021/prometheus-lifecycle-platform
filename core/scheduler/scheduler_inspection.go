package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/services"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (s *Scheduler) runScheduledInspections(ctx context.Context) {
	defer s.wg.Done()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := services.NewInspectionNotificationService(s.db).RetryPending(time.Now()); err != nil {
				log.Printf("[Scheduler] inspection notification retries: %v", err)
			}
			observeLoop("scheduled_inspection", s.executeDueInspections(ctx, time.Now()))
		}
	}
}

func (s *Scheduler) executeDueInspections(ctx context.Context, now time.Time) error {
	var schedules []models.ScheduledInspection
	if err := s.db.Where("enabled = ?", true).Order("id ASC").Find(&schedules).Error; err != nil {
		return err
	}
	for index := range schedules {
		schedule := &schedules[index]
		if !cronMatches(schedule.Cron, now) {
			continue
		}
		if schedule.LastRun != nil && schedule.LastRun.Truncate(time.Minute).Equal(now.Truncate(time.Minute)) {
			continue
		}
		if _, err := s.ExecuteInspectionSchedule(ctx, schedule, "schedule:"+schedule.Name, now); err != nil && !errors.Is(err, services.ErrInspectionBusy) {
			return err
		}
	}
	return nil
}

// ExecuteInspectionSchedule runs one schedule through the same path used by
// the minute loop. HTTP "run now" calls this method as well.
func (s *Scheduler) ExecuteInspectionSchedule(ctx context.Context, schedule *models.ScheduledInspection, triggeredBy string, startedAt time.Time) (services.InspectionExecution, error) {
	claimedContext, release, err := services.ClaimInspection(ctx, s.db)
	if err != nil {
		return services.InspectionExecution{}, err
	}
	defer release()
	var current models.ScheduledInspection
	if err := s.db.First(&current, schedule.ID).Error; err != nil {
		return services.InspectionExecution{}, err
	}
	if strings.HasPrefix(triggeredBy, "schedule:") && !current.Enabled {
		return services.InspectionExecution{}, services.ErrInspectionBusy
	}
	if current.LastRun != nil && !current.LastRun.Before(startedAt.Truncate(time.Minute)) {
		return services.InspectionExecution{}, services.ErrInspectionBusy
	}
	if err := s.db.Model(&models.ScheduledInspection{}).Where("id = ?", schedule.ID).Update("last_run", startedAt).Error; err != nil {
		return services.InspectionExecution{}, err
	}
	if err := s.recordScheduledInspectionStatus(schedule.ID, "running", "unknown", "", 0, nil, startedAt); err != nil {
		return services.InspectionExecution{}, err
	}
	execution, runErr := services.NewInspectionService(s.db, s.basePath).Run(claimedContext, services.InspectionRequest{
		TriggeredBy: triggeredBy, ScheduledInspectionID: &schedule.ID,
	})
	finishedAt := time.Now()
	status := "success"
	result := execution.Run.Result
	lastError := ""
	var runID *uint
	if execution.Run.ID != 0 {
		runID = &execution.Run.ID
	}
	if runErr != nil {
		status = "failed"
		result = "unknown"
		lastError = runErr.Error()
		log.Printf("[Scheduler] inspection schedule %s failed: %v", schedule.Name, runErr)
	}
	if err := s.recordScheduledInspectionStatus(schedule.ID, status, result, lastError, finishedAt.Sub(startedAt), runID, startedAt); err != nil {
		return execution, errors.Join(runErr, err)
	}
	if runErr == nil && schedule.KeepCount > 0 {
		s.cleanupOldInspectionRuns(schedule)
	}
	return execution, runErr
}

func (s *Scheduler) recordScheduledInspectionStatus(scheduleID uint, status, result, lastError string, duration time.Duration, runID *uint, lastRun time.Time) error {
	current := models.ScheduledInspectionStatus{
		ScheduledInspectionID: scheduleID, LastStatus: status, LastResult: result,
		LastError: lastError, DurationMS: duration.Milliseconds(), InspectionRunID: runID, LastRun: lastRun,
	}
	return s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "scheduled_inspection_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"last_status", "last_result", "last_error", "duration_ms", "inspection_run_id", "last_run", "updated_at"}),
	}).Create(&current).Error
}

func (s *Scheduler) cleanupOldInspectionRuns(schedule *models.ScheduledInspection) {
	var oldRuns []models.InspectionRun
	if err := s.db.Where("scheduled_inspection_id = ?", schedule.ID).
		Order("created_at DESC").Offset(schedule.KeepCount).Find(&oldRuns).Error; err != nil {
		log.Printf("[Scheduler] list old inspection runs for schedule %d: %v", schedule.ID, err)
		return
	}
	for _, run := range oldRuns {
		if err := s.db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Where("run_id = ?", run.ID).Delete(&models.InspectionCheck{}).Error; err != nil {
				return err
			}
			return tx.Delete(&models.InspectionRun{}, run.ID).Error
		}); err != nil {
			log.Printf("[Scheduler] cleanup inspection run %d: %v", run.ID, err)
		}
	}
}

func ValidateInspectionSchedule(schedule *models.ScheduledInspection) error {
	if schedule == nil || schedule.Name == "" {
		return fmt.Errorf("巡检计划名称不能为空")
	}
	if !ValidCronExpression(schedule.Cron) {
		return fmt.Errorf("cron 必须是有效的五段表达式")
	}
	if schedule.KeepCount < 1 || schedule.KeepCount > 365 {
		return fmt.Errorf("巡检历史保留数量必须在 1 到 365 之间")
	}
	return nil
}
