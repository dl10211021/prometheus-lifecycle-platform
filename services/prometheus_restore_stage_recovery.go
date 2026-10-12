package services

import (
	"context"
	"github.com/promlifecycle/platform/models"
	platformsqlite "github.com/promlifecycle/platform/storage/sqlite"
	"gorm.io/gorm"
	"sync"
	"time"
)

// Only exact surviving pinned tokens are admissible. Missing tokens represent
// completed portions of interrupted release, never permission to remove others.
func survivingRestoreStageClaims(ctx context.Context, tx *gorm.DB, row *models.PrometheusRestoreTransaction, allowed restoreStageClaims) (restoreStageClaims, error) {
	var hosts []models.HostOperationClaim
	var components []models.ComponentOperationClaim
	if err := tx.WithContext(ctx).Where("host_id = ?", row.HostID).Find(&hosts).Error; err != nil {
		return restoreStageClaims{}, err
	}
	if err := tx.WithContext(ctx).Where("component_id = ? OR component_id IN (SELECT id FROM components WHERE host_id = ?)", row.ComponentID, row.HostID).Find(&components).Error; err != nil {
		return restoreStageClaims{}, err
	}
	got := restoreStageClaims{}
	if len(hosts) > 1 || len(components) > 1 {
		return got, restoreJournalReject("sibling stage claim prevents takeover")
	}
	if len(hosts) == 1 {
		if hosts[0].Token != allowed.Host {
			return got, restoreJournalReject("host claim replaced")
		}
		got.Host = hosts[0].Token
	}
	if len(components) == 1 {
		if components[0].ComponentID != row.ComponentID || components[0].Token != allowed.Component {
			return got, restoreJournalReject("component claim replaced")
		}
		got.Component = components[0].Token
	}
	return got, nil
}
func (s *BackupService) reclaimStageOwner(ctx context.Context, tx *gorm.DB, expected *models.PrometheusRestoreTransaction) (*prometheusRestoreStagePayload, *models.Component, restoreStageClaims, error) {
	var row models.PrometheusRestoreTransaction
	var c models.Component
	if err := tx.WithContext(ctx).First(&row, expected.ID).Error; err != nil {
		return nil, nil, restoreStageClaims{}, err
	}
	admission, err := decodeRestoreJournal(&row)
	if err != nil {
		return nil, nil, restoreStageClaims{}, err
	}
	if !sameRestoreStageRow(&row, expected) {
		return nil, nil, restoreStageClaims{}, restoreJournalReject("reclaim journal changed")
	}
	p, err := decodeRestoreStage(&row)
	if err != nil || p == nil {
		return nil, nil, restoreStageClaims{}, restoreJournalReject("only fenced stage phases can be reclaimed")
	}
	scoped := *s
	scoped.db = tx
	if err := scoped.checkRestoreJournalIdentity(ctx, &row, admission); err != nil {
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
	if err := tx.WithContext(ctx).Preload("Host").First(&c, row.ComponentID).Error; err != nil {
		return nil, nil, claims, err
	}
	return p, &c, claims, nil
}
func (s *BackupService) RecoverPrometheusPairedRestoreStageForStartup(ctx context.Context, id uint, ownership *platformsqlite.DatabaseProcessLock) (result *models.PrometheusRestoreTransaction, resultErr error) {
	s = s.quietRestoreStageService()
	if err := s.requireRestoreStageDatabase(ctx, id); err != nil {
		return nil, restoreStageSafeError(ctx, err)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	defer func() { resultErr = restoreStageSafeError(ctx, resultErr) }()
	actual, err := platformsqlite.DatabasePath(s.db.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	err = ownership.WithOwnership(ctx, actual, func() error {
		if err := platformsqlite.ValidateRestoreStageRecoveryState(actual); err != nil {
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
		p, c, _, err := s.reclaimStageOwner(ctx, s.db, &row)
		if err != nil {
			return err
		}
		if p.Fence.BackupRoot != s.backupRoot {
			return restoreJournalReject("stage backup root changed")
		}
		if row.Phase != "restore_stage_reclaim" {
			if len(p.Generations) >= restoreStageMaxGenerations {
				return restoreJournalReject("stage generation bound reached")
			}
			desired, err := randomRestoreStageToken(32)
			if err != nil {
				return err
			}
			h, err := randomRestoreStageToken(24)
			if err != nil {
				return err
			}
			co, err := randomRestoreStageToken(24)
			if err != nil {
				return err
			}
			var next *models.PrometheusRestoreTransaction
			err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				proof, _, claims, e := s.reclaimStageOwner(ctx, tx, &row)
				if e != nil {
					return e
				}
				proof.CompletedAt = time.Time{}
				proof.Reclaim = &restoreStageReclaim{FromPhase: row.Phase, Desired: desired, Captured: claims, Replacement: restoreStageClaims{h, co}, StartedAt: s.now().UTC()}
				next, e = s.nextRestoreStage(&row, "restore_stage_reclaim", proof)
				if e != nil {
					return e
				}
				return restoreStageCAS(ctx, tx, &row, next)
			})
			if err != nil {
				return err
			}
			row = *next
		}
		// Reopening persisted intent makes uncertain rotation/COMMIT retry use the
		// exact same expected and desired generations, not another generated token.
		p, c, _, err = s.reclaimStageOwner(ctx, s.db, &row)
		if err != nil {
			return err
		}
		r, release, err := s.pointRunner(ctx, c)
		if err != nil {
			return err
		}
		defer release()
		scoped := s.onPointRunner(r)
		if _, _, err := scoped.restoreStageSources(ctx, r, &row, c); err != nil {
			return err
		}
		if _, _, _, err := s.reclaimStageOwner(ctx, s.db, &row); err != nil {
			return err
		}
		rotated, err := rotateRestoreStageFence(ctx, r, p.Fence, p.Reclaim.Desired)
		if err != nil {
			return err
		}
		next, claimCtx, releaseClaims, err := s.publishRestoreStageTakeover(ctx, &row, rotated)
		if err != nil {
			return err
		}
		defer releaseClaims()
		proof, err := decodeRestoreStage(next)
		if err != nil {
			return err
		}
		result, err = scoped.finishRestoreStage(claimCtx, r, next, c, proof)
		return err
	})
	return result, err
}
func (s *BackupService) publishRestoreStageTakeover(ctx context.Context, old *models.PrometheusRestoreTransaction, fence restoreStageFence) (*models.PrometheusRestoreTransaction, context.Context, func(), error) {
	var next *models.PrometheusRestoreTransaction
	var tokens restoreStageClaims
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		p, _, surviving, err := s.reclaimStageOwner(ctx, tx, old)
		if err != nil {
			return err
		}
		if p.Reclaim == nil || fence.Generation != p.Reclaim.Desired || fence.Root != p.Fence.Root || fence.BackupRoot != p.Fence.BackupRoot {
			return restoreJournalReject("host rotation does not bind reclaim")
		}
		if p.Fence.RootIdentity != "" && (fence.RootIdentity != p.Fence.RootIdentity || fence.LockIdentity != p.Fence.LockIdentity) {
			return restoreJournalReject("host fence identity changed")
		}
		if surviving.Host != "" {
			if err := restoreJournalWriteResult(ctx, tx.Where("host_id = ? AND token = ?", old.HostID, surviving.Host).Delete(&models.HostOperationClaim{}), "retired host claim"); err != nil {
				return err
			}
		}
		if surviving.Component != "" {
			if err := restoreJournalWriteResult(ctx, tx.Where("component_id = ? AND token = ?", old.ComponentID, surviving.Component).Delete(&models.ComponentOperationClaim{}), "retired component claim"); err != nil {
				return err
			}
		}
		tokens = p.Reclaim.Replacement
		if err := restoreJournalWriteResult(ctx, tx.Create(&models.HostOperationClaim{HostID: old.HostID, Token: tokens.Host, Kind: "paired-restore-stage", CreatedAt: s.now().UTC()}), "replacement host claim"); err != nil {
			return err
		}
		if err := restoreJournalWriteResult(ctx, tx.Create(&models.ComponentOperationClaim{ComponentID: old.ComponentID, Token: tokens.Component, Kind: "paired-restore-stage", CreatedAt: s.now().UTC()}), "replacement component claim"); err != nil {
			return err
		}
		p.Fence = fence
		p.Generations = append(p.Generations, fence.Generation)
		p.Claims = tokens
		p.Reclaim = nil
		next, err = s.nextRestoreStage(old, "restore_stage_copying", p)
		if err != nil {
			return err
		}
		return restoreStageCAS(ctx, tx, old, next)
	})
	if err != nil {
		return nil, ctx, nil, err
	}
	// A failed COMMIT must never construct authority or register cleanup that can
	// act against an already rolled-back transaction handle.
	ctx = context.WithValue(ctx, hostClaimContextKey{}, hostClaimContext{id: old.HostID, token: tokens.Host})
	ctx = context.WithValue(ctx, componentClaimContextKey{}, &componentClaimContext{id: old.ComponentID, token: tokens.Component})
	var once sync.Once
	release := func() {
		once.Do(func() {
			clean, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			s.db.WithContext(clean).Where("component_id = ? AND token = ?", old.ComponentID, tokens.Component).Delete(&models.ComponentOperationClaim{})
			s.db.WithContext(clean).Where("host_id = ? AND token = ?", old.HostID, tokens.Host).Delete(&models.HostOperationClaim{})
		})
	}
	return next, ctx, release, nil
}
