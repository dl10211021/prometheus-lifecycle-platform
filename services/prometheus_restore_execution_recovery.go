package services

import (
	"context"
	"time"

	"github.com/promlifecycle/platform/models"
	platformsqlite "github.com/promlifecycle/platform/storage/sqlite"
	"gorm.io/gorm"
)

func (s *BackupService) executionRecoveryOwner(ctx context.Context, tx *gorm.DB, expected *models.PrometheusRestoreTransaction) (*restoreExecutionPayload, *models.Component, restoreStageClaims, error) {
	var row models.PrometheusRestoreTransaction
	if err := tx.WithContext(ctx).First(&row, expected.ID).Error; err != nil {
		return nil, nil, restoreStageClaims{}, err
	}
	if !sameExecutionRow(&row, expected) {
		return nil, nil, restoreStageClaims{}, restoreJournalReject("recovery owner changed")
	}
	p, admission, err := decodeRestoreExecution(&row)
	if err != nil || executionTerminal(row.Phase) {
		return nil, nil, restoreStageClaims{}, restoreJournalReject("pending execution evidence required")
	}
	check := *s
	check.db = tx
	if err := check.checkRestoreJournalIdentity(ctx, &row, admission); err != nil {
		return nil, nil, restoreStageClaims{}, err
	}
	allowed := p.Claims
	if p.Reclaim != nil {
		allowed = p.Reclaim.Captured
	}
	claims, err := survivingRestoreStageClaims(ctx, tx, &row, allowed)
	if err != nil {
		return nil, nil, claims, err
	}
	var c models.Component
	if err := tx.WithContext(ctx).Preload("Host").First(&c, row.ComponentID).Error; err != nil {
		return nil, nil, claims, err
	}
	return p, &c, claims, nil
}

// Recovery is compensation only. It requires the exclusive process lock and
// rotates the protected host generation before retiring exact surviving claims.
// Legacy interrupted checkpoint/stage phases are deliberately not reinterpreted.
func (s *BackupService) RecoverPrometheusPairedRestoreExecutionForStartup(ctx context.Context, id uint, ownership *platformsqlite.DatabaseProcessLock) (result *models.PrometheusRestoreTransaction, resultErr error) {
	s = s.quietRestoreStageService()
	if err := s.requireRestoreStageDatabase(ctx, id); err != nil {
		return nil, executionSafeFailure(ctx, err)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	defer func() {
		if resultErr != nil {
			read, done := context.WithTimeout(context.Background(), 5*time.Second)
			defer done()
			var row models.PrometheusRestoreTransaction
			if s.db.WithContext(read).First(&row, id).Error == nil {
				if _, err := decodeRestoreJournal(&row); err == nil {
					result = &row
				}
			}
			resultErr = executionSafeFailure(ctx, resultErr)
		}
	}()
	path, err := platformsqlite.DatabasePath(s.db.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	err = ownership.WithOwnership(ctx, path, func() error {
		if err := platformsqlite.ValidateRestoreStageRecoveryState(path); err != nil {
			return err
		}
		var integrity string
		if err := s.db.WithContext(ctx).Raw("PRAGMA quick_check").Scan(&integrity).Error; err != nil {
			return err
		}
		if integrity != "ok" {
			return restoreJournalReject("database integrity unavailable")
		}
		var row models.PrometheusRestoreTransaction
		if err := s.db.WithContext(ctx).First(&row, id).Error; err != nil {
			return err
		}
		if _, err := decodeRestoreJournal(&row); err != nil {
			return err
		}
		result = &row
		if executionTerminal(row.Phase) {
			return nil
		}
		p, c, _, err := s.executionRecoveryOwner(ctx, s.db, &row)
		if err != nil {
			return err
		}
		if p.Fence.BackupRoot != s.backupRoot {
			return restoreJournalReject("execution backup root changed")
		}
		if row.Phase != "restore_execution_reclaim" {
			if len(p.Generations) >= restoreStageMaxGenerations || len(p.Steps) > restoreExecutionMaxSteps-4 {
				return restoreJournalReject("execution recovery bound reached")
			}
			desired, err := randomRestoreStageToken(32)
			if err != nil {
				return err
			}
			host, err := randomRestoreStageToken(24)
			if err != nil {
				return err
			}
			component, err := randomRestoreStageToken(24)
			if err != nil {
				return err
			}
			var next *models.PrometheusRestoreTransaction
			err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				proof, _, captured, err := s.executionRecoveryOwner(ctx, tx, &row)
				if err != nil {
					return err
				}
				proof.Reclaim = &restoreStageReclaim{FromPhase: row.Phase, Desired: desired, Captured: captured, Replacement: restoreStageClaims{host, component}, StartedAt: s.now().UTC()}
				next, err = s.nextRestoreExecution(&row, "restore_execution_reclaim", proof)
				if err != nil {
					return err
				}
				return restoreExecutionCAS(ctx, tx, &row, next)
			})
			if err != nil {
				return err
			}
			row = *next
			result = &row
		}
		p, c, _, err = s.executionRecoveryOwner(ctx, s.db, &row)
		if err != nil {
			return err
		}
		r, release, err := s.pointRunner(ctx, c)
		if err != nil {
			return err
		}
		defer release()
		// Idempotent expected->desired rotation handles a crash between durable
		// host rotation and DB claim publication without reviving an old writer.
		fence, err := rotateRestoreStageFence(ctx, r, p.Fence, p.Reclaim.Desired)
		if err != nil {
			return err
		}
		next, claimCtx, cleanup, err := s.publishRestoreExecutionTakeover(ctx, &row, fence)
		if err != nil {
			return err
		}
		defer cleanup()
		result, err = s.onPointRunner(r).performRestoreExecution(claimCtx, r, next, c, true)
		return err
	})
	return result, err
}
func (s *BackupService) publishRestoreExecutionTakeover(ctx context.Context, old *models.PrometheusRestoreTransaction, fence restoreStageFence) (*models.PrometheusRestoreTransaction, context.Context, func(), error) {
	var next *models.PrometheusRestoreTransaction
	var tokens restoreStageClaims
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		p, _, surviving, err := s.executionRecoveryOwner(ctx, tx, old)
		if err != nil {
			return err
		}
		if p.Reclaim == nil || fence.Generation != p.Reclaim.Desired || fence.Root != p.Fence.Root || fence.BackupRoot != p.Fence.BackupRoot || fence.RootIdentity != p.Fence.RootIdentity || fence.LockIdentity != p.Fence.LockIdentity {
			return restoreJournalReject("execution rotation does not bind reclaim")
		}
		if surviving.Host != "" {
			if err := restoreJournalWriteResult(ctx, tx.Where("host_id = ? AND token = ?", old.HostID, surviving.Host).Delete(&models.HostOperationClaim{}), "retired execution host claim"); err != nil {
				return err
			}
		}
		if surviving.Component != "" {
			if err := restoreJournalWriteResult(ctx, tx.Where("component_id = ? AND token = ?", old.ComponentID, surviving.Component).Delete(&models.ComponentOperationClaim{}), "retired execution component claim"); err != nil {
				return err
			}
		}
		tokens = p.Reclaim.Replacement
		if err := restoreJournalWriteResult(ctx, tx.Create(&models.HostOperationClaim{HostID: old.HostID, Token: tokens.Host, Kind: "paired-restore-execution", CreatedAt: s.now().UTC()}), "replacement execution host claim"); err != nil {
			return err
		}
		if err := restoreJournalWriteResult(ctx, tx.Create(&models.ComponentOperationClaim{ComponentID: old.ComponentID, Token: tokens.Component, Kind: "paired-restore-execution", CreatedAt: s.now().UTC()}), "replacement execution component claim"); err != nil {
			return err
		}
		p.Fence = fence
		p.Generations = append(p.Generations, fence.Generation)
		p.Claims = tokens
		p.Reclaim = nil
		next, err = s.nextRestoreExecution(old, "restore_compensating", p)
		if err != nil {
			return err
		}
		return restoreExecutionCAS(ctx, tx, old, next)
	})
	if err != nil {
		return nil, ctx, nil, err
	}
	ctx = context.WithValue(ctx, hostClaimContextKey{}, hostClaimContext{id: old.HostID, token: tokens.Host})
	ctx = context.WithValue(ctx, componentClaimContextKey{}, &componentClaimContext{id: old.ComponentID, token: tokens.Component})
	cleanup := func() {
		clean, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s.db.WithContext(clean).Where("component_id = ? AND token = ?", old.ComponentID, tokens.Component).Delete(&models.ComponentOperationClaim{})
		s.db.WithContext(clean).Where("host_id = ? AND token = ?", old.HostID, tokens.Host).Delete(&models.HostOperationClaim{})
	}
	return next, ctx, cleanup, nil
}
