package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/services"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// runScheduledBackups checks every 60 seconds for enabled ScheduledBackup entries
// whose cron expressions match the current time, then executes the backup.
func (s *Scheduler) runScheduledBackups(ctx context.Context) {
	defer s.wg.Done()
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					observeLoop("scheduled_backup_worker", s.executeQueuedBackup(ctx))
				}
			}
		}()
	}
	defer workers.Wait()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	observeLoop("scheduled_backup", s.executeDueBackups(ctx))
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			observeLoop("scheduled_backup", s.executeDueBackups(ctx))
		}
	}
}

// executeDueBackups finds enabled ScheduledBackup records matching current time and runs them.
func (s *Scheduler) executeDueBackups(ctx context.Context) error {
	return s.enqueueDueBackups(ctx, time.Now())
}

// ScheduledBackupRun is the durable queue: (schedule, minute) is unique even
// when detection repeats or an earlier run spans multiple due slots.
type ScheduledBackupRun struct {
	ID                uint       `gorm:"primaryKey" json:"id"`
	ScheduledBackupID uint       `gorm:"uniqueIndex:backup_due_slot;not null" json:"scheduled_backup_id"`
	Slot              time.Time  `gorm:"uniqueIndex:backup_due_slot;not null" json:"slot"`
	Status            string     `gorm:"index" json:"status"`
	Error             string     `json:"error"`
	StartedAt         *time.Time `json:"started_at"`
	CompletedAt       *time.Time `json:"completed_at"`
}

func (s *Scheduler) enqueueDueBackups(ctx context.Context, now time.Time) error {
	var backups []models.ScheduledBackup
	if err := s.db.WithContext(ctx).Where("enabled = ?", true).Find(&backups).Error; err != nil {
		return err
	}
	slot := now.Truncate(time.Minute)
	for i := range backups {
		backup := &backups[i]
		if !cronMatches(backup.Cron, now) {
			continue
		}
		run := ScheduledBackupRun{ScheduledBackupID: backup.ID, Slot: slot, Status: "pending"}
		if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&run).Error; err != nil {
			return err
		}
	}
	return nil
}

func (s *Scheduler) executeQueuedBackup(ctx context.Context) error {
	var run ScheduledBackupRun
	next := s.db.WithContext(ctx).Where("status IN ?", []string{"pending", "interrupted"}).Order("slot ASC, id ASC").Limit(1).Find(&run)
	if next.Error != nil {
		return next.Error
	}
	if next.RowsAffected == 0 {
		return nil
	}
	now := time.Now()
	claim := s.db.WithContext(ctx).Model(&run).Where("status IN ?", []string{"pending", "interrupted"}).Updates(map[string]any{"status": "running", "started_at": now, "completed_at": nil})
	if claim.Error != nil {
		return claim.Error
	}
	if claim.RowsAffected == 0 {
		return nil
	}
	var scheduled models.ScheduledBackup
	err := s.db.First(&scheduled, run.ScheduledBackupID).Error
	if err == nil && !scheduled.Enabled {
		err = errors.New("schedule disabled before execution")
	}
	if err == nil {
		err = s.doBackup(ctx, &scheduled)
	}
	status, lastError := "success", ""
	if err != nil {
		status, lastError = "failed", err.Error()
		if ctx.Err() != nil {
			status = "interrupted"
		}
	}
	finished := time.Now()
	persistCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if errors.Is(err, services.ErrComponentBusy) {
		return s.db.WithContext(persistCtx).Model(&run).Updates(map[string]any{"status": "pending", "error": err.Error(), "started_at": nil, "completed_at": nil}).Error
	}
	return errors.Join(err, s.db.WithContext(persistCtx).Model(&run).Updates(map[string]any{"status": status, "error": lastError, "completed_at": finished}).Error)
}

func RecoverInterruptedBackupRuns(db *gorm.DB) error {
	return db.Model(&ScheduledBackupRun{}).Where("status = ?", "running").Updates(map[string]any{
		"status": "interrupted", "error": "Server stopped during backup; no application recovery was verified", "completed_at": time.Now(),
	}).Error
}

// cronMatches performs a robust minute-level cron match against the current time.
// Supports standard 5-field cron syntax and simple minute-only expressions.
func cronMatches(expr string, t time.Time) bool {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return false
	}

	parts := strings.Fields(expr)
	if len(parts) != 5 {
		return false
	}

	minute := t.Minute()
	hour := t.Hour()
	dom := t.Day()
	month := int(t.Month())
	dow := int(t.Weekday())

	if !matchField(parts[0], minute, 0, 59) {
		return false
	}
	if !matchField(parts[1], hour, 0, 23) {
		return false
	}
	if !matchField(parts[2], dom, 1, 31) {
		return false
	}
	if !matchField(parts[3], month, 1, 12) {
		return false
	}
	if !matchField(parts[4], dow, 0, 6) {
		return false
	}

	return true
}

// ValidCronExpression validates the same five-field, minute-level syntax used
// by scheduled backups and inspections.
func ValidCronExpression(expression string) bool {
	fields := strings.Fields(strings.TrimSpace(expression))
	if len(fields) != 5 {
		return false
	}
	ranges := [][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 6}}
	for index, field := range fields {
		if !validCronField(field, ranges[index][0], ranges[index][1]) {
			return false
		}
	}
	return true
}

func validCronField(field string, min, max int) bool {
	if field == "" {
		return false
	}
	for _, item := range strings.Split(field, ",") {
		parts := strings.Split(item, "/")
		if len(parts) > 2 || parts[0] == "" {
			return false
		}
		if len(parts) == 2 {
			step, err := strconv.Atoi(parts[1])
			if err != nil || step < 1 || step > max-min+1 {
				return false
			}
		}
		base := parts[0]
		if base == "*" {
			continue
		}
		if strings.Contains(base, "-") {
			bounds := strings.Split(base, "-")
			if len(bounds) != 2 {
				return false
			}
			start, startErr := strconv.Atoi(bounds[0])
			end, endErr := strconv.Atoi(bounds[1])
			if startErr != nil || endErr != nil || start < min || end > max || start > end {
				return false
			}
			continue
		}
		value, err := strconv.Atoi(base)
		if err != nil || value < min || value > max {
			return false
		}
	}
	return true
}

// matchField matches a single cron field value.
func matchField(field string, value, min, max int) bool {
	if !validCronField(field, min, max) {
		return false
	}
	for _, item := range strings.Split(field, ",") {
		parts := strings.Split(item, "/")
		step := 1
		if len(parts) == 2 {
			step, _ = strconv.Atoi(parts[1])
		}
		start, end := min, max
		switch base := parts[0]; {
		case base == "*":
		case strings.Contains(base, "-"):
			bounds := strings.Split(base, "-")
			start, _ = strconv.Atoi(bounds[0])
			end, _ = strconv.Atoi(bounds[1])
		default:
			start, _ = strconv.Atoi(base)
			end = start
		}
		if value >= start && value <= end && (value-start)%step == 0 {
			return true
		}
	}
	return false
}

// doBackup executes a scheduled backup through the same integrity-preserving
// service used by manual backups.
func (s *Scheduler) doBackup(ctx context.Context, scheduled *models.ScheduledBackup) error {
	startedAt := time.Now()
	backupName := fmt.Sprintf("%s-%s", scheduled.Name, startedAt.Format("20060102-150405"))
	var backup *models.Backup
	var execErr error

	if scheduled.ComponentID == nil {
		execErr = errors.New("scheduled component backup requires component_id")
	} else {
		var component models.Component
		if err := s.db.Preload("Host").First(&component, *scheduled.ComponentID).Error; err != nil {
			execErr = fmt.Errorf("component %d not found: %w", *scheduled.ComponentID, err)
		} else {
			backup, _, execErr = services.NewBackupService(s.db).
				CreateScheduledComponentBackup(ctx, &component, backupName, scheduled.Type, scheduled.ID)
		}
	}
	if errors.Is(execErr, services.ErrComponentBusy) {
		return execErr
	}

	finishedAt := time.Now()
	execErr = errors.Join(execErr, s.db.Model(scheduled).Update("last_run", finishedAt).Error)
	status := "success"
	lastError := ""
	var backupID *uint
	if execErr != nil {
		status = "failed"
		if ctx.Err() != nil {
			status = "interrupted"
		}
		lastError = execErr.Error()
		log.Printf("[Scheduler] %s incomplete: %v", backupName, execErr)
		observeLoop("scheduled_backup", execErr)
	} else {
		backupID = &backup.ID
		observeBackupSuccess(finishedAt)
	}
	execErr = errors.Join(execErr, s.recordScheduledBackupStatus(scheduled.ID, status, lastError, finishedAt.Sub(startedAt), backupID, finishedAt))
	if execErr != nil {
		return execErr
	}

	if scheduled.KeepCount > 0 {
		if err := s.cleanupOldBackups(scheduled); err != nil {
			return errors.Join(err, s.recordScheduledBackupStatus(scheduled.ID, "failed", "Backup created; retention incomplete: "+err.Error(), time.Since(startedAt), backupID, finishedAt))
		}
	}
	return nil
}

func (s *Scheduler) recordScheduledBackupStatus(scheduledID uint, status, lastError string, duration time.Duration, backupID *uint, lastRun time.Time) error {
	current := models.ScheduledBackupStatus{
		ScheduledBackupID: scheduledID, LastStatus: status, LastError: lastError,
		DurationMS: duration.Milliseconds(), BackupID: backupID, LastRun: lastRun,
	}
	return s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "scheduled_backup_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"last_status", "last_error", "duration_ms", "backup_id", "last_run", "updated_at"}),
	}).Create(&current).Error
}

// cleanupOldBackups removes backups beyond the configured keep count.
func (s *Scheduler) cleanupOldBackups(sb *models.ScheduledBackup) error {
	if sb.KeepCount < 1 {
		return nil
	}
	var cleanupErr error
	var backups []models.Backup
	query := s.db.Model(&models.Backup{}).Where("scheduled_backup_id = ?", sb.ID).
		Order("created_at DESC, id DESC").Offset(sb.KeepCount)

	if sb.ComponentID != nil {
		query = query.Where("component_id = ?", *sb.ComponentID)
	} else if sb.ClusterID != nil {
		query = query.Where("cluster_id = ?", *sb.ClusterID)
	}

	if err := query.Find(&backups).Error; err != nil {
		return err
	}

	if len(backups) > 0 {
		for i := range backups {
			backup := &backups[i]
			if err := s.removeBackupFile(backup); err != nil {
				log.Printf("[Scheduler] keeping backup record %d because file deletion failed: %v", backup.ID, err)
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("backup %d: %w", backup.ID, err))
				continue
			}
			if err := s.db.Transaction(func(tx *gorm.DB) error {
				if err := tx.Where("backup_id = ?", backup.ID).Delete(&models.BackupManifest{}).Error; err != nil {
					return err
				}
				return tx.Delete(backup).Error
			}); err != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("backup %d retention record: %w", backup.ID, err))
			}
		}
	}
	return cleanupErr
}

// removeBackupFile deletes on the host that produced the backup. A lookup or
// remote deletion failure is returned so rotation retains the DB reference and
// can retry instead of orphaning a remote file.
func (s *Scheduler) removeBackupFile(backup *models.Backup) error {
	if backup == nil || backup.Path == "" {
		return nil
	}
	var component *models.Component
	if backup.ComponentID != nil {
		var resolved models.Component
		if err := s.db.Preload("Host").First(&resolved, *backup.ComponentID).Error; err != nil {
			return fmt.Errorf("resolve backup host: %w", err)
		}
		component = &resolved
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return services.NewBackupService(s.db).DeleteBackupArtifact(ctx, backup, component)
}
