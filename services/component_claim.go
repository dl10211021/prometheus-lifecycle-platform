package services

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/promlifecycle/platform/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrComponentBusy = errors.New("component has an active lifecycle operation")

type componentClaimContextKey struct{}
type componentClaimContext struct {
	parent *componentClaimContext
	id     uint
	token  string
}

// ClaimComponentOperation is reentrant only through the returned context. The
// database primary key arbitrates competing HTTP requests and background jobs.
func ClaimComponentOperation(ctx context.Context, db *gorm.DB, id uint, kind string) (context.Context, func(), error) {
	if err := rejectPairedRestoreJournalClaim(ctx, db, 0, id); err != nil {
		return ctx, nil, err
	}
	parent, _ := ctx.Value(componentClaimContextKey{}).(*componentClaimContext)
	for held := parent; held != nil; held = held.parent {
		if held.id == id {
			var count int64
			if err := db.WithContext(ctx).Model(&models.ComponentOperationClaim{}).Where("component_id = ? AND token = ?", id, held.token).Count(&count).Error; err != nil {
				return ctx, nil, err
			}
			if count != 1 {
				return ctx, nil, ErrComponentBusy
			}
			return ctx, func() {}, nil
		}
	}
	var random [24]byte
	if _, err := rand.Read(random[:]); err != nil {
		return ctx, nil, err
	}
	token := hex.EncodeToString(random[:])
	claim := models.ComponentOperationClaim{ComponentID: id, Token: token, Kind: kind, CreatedAt: time.Now()}
	result := db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&claim)
	if result.Error != nil {
		return ctx, nil, result.Error
	}
	if result.RowsAffected != 1 {
		return ctx, nil, ErrComponentBusy
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			// Cancellation must never strand a claim. Failed deletion remains durable
			// and startup reconciliation exposes it instead of permitting overlap.
			cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			db.WithContext(cleanup).Where("component_id = ? AND token = ?", id, token).Delete(&models.ComponentOperationClaim{})
		})
	}
	// Only a valid reentrant token above can cross a version transition barrier.
	// Startup checkpoint recovery may bypass a backup pause, never this barrier.
	var paused int64
	blocked := db.WithContext(ctx).Model(&ComponentBackupPause{}).Where("component_id = ?", id)
	if kind == "restore-recovery" {
		blocked = blocked.Where("version_transition_plan_id <> 0")
	} else {
		blocked = blocked.Where("status <> ? OR version_transition_plan_id <> 0", "resumed")
	}
	if err := blocked.Count(&paused).Error; err != nil {
		release()
		return ctx, nil, err
	}
	if paused != 0 {
		release()
		return ctx, nil, errors.New("component recovery is unresolved; complete restore recovery before another lifecycle operation")
	}

	if err := rejectPairedRestoreJournalClaim(ctx, db, 0, id); err != nil {
		release()
		return ctx, nil, err
	}
	return context.WithValue(ctx, componentClaimContextKey{}, &componentClaimContext{parent: parent, id: id, token: token}), release, nil
}

// ComponentOperationRelease binds a transaction-created claim to the live DB.
func ComponentOperationRelease(ctx context.Context, db *gorm.DB, id uint) func() {
	held, _ := ctx.Value(componentClaimContextKey{}).(*componentClaimContext)
	for held != nil && held.id != id {
		held = held.parent
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			if held == nil {
				return
			}
			cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			db.WithContext(cleanup).Where("component_id = ? AND token = ?", id, held.token).Delete(&models.ComponentOperationClaim{})
		})
	}
}

// RecoverInterruptedLifecycle runs once before accepting traffic or workers.
// Runtime health is deliberately not treated as proof that an interrupted
// configuration/removal transaction completed.
func RecoverInterruptedLifecycle(db *gorm.DB) error {
	now := time.Now()
	return db.Transaction(func(tx *gorm.DB) error {
		reason := "Server stopped before lifecycle verification completed; retry or recover explicitly"
		if err := tx.Model(&models.Operation{}).Where("status IN ?", []string{"pending", "running"}).Updates(map[string]interface{}{"status": "interrupted", "stage": "interrupted", "message": reason, "completed_at": now}).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.ChangePlan{}).Where("status = ?", models.ChangeStatusExecuting).Update("status", models.ChangeStatusFailed).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.Component{}).Where("status IN ?", []models.ComponentStatus{models.StatusDeploying, models.StatusUpgrading, models.StatusPending}).Updates(map[string]interface{}{"status": models.StatusError, "error": reason}).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.DeploymentStackStep{}).Where("status = ?", "running").Updates(map[string]interface{}{"status": "interrupted", "error": reason}).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.DeploymentStack{}).Where("status = ?", "running").Updates(map[string]interface{}{"status": "interrupted", "last_error": reason}).Error; err != nil {
			return err
		}
		if err := tx.Where("1 = 1").Delete(&models.ComponentOperationClaim{}).Error; err != nil {
			return fmt.Errorf("release abandoned lifecycle claims: %w", err)
		}
		if err := tx.Where("1 = 1").Delete(&models.HostOperationClaim{}).Error; err != nil {
			return err
		}
		return nil
	})
}
