package services

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/promlifecycle/platform/models"
	"gorm.io/gorm"
)

type restoreJournalAccessKey struct{}
type restoreJournalAccess struct {
	ID                  uint
	Token               string
	HostID, ComponentID uint
}

func restoreJournalAccessAllows(ctx context.Context, row *models.PrometheusRestoreTransaction, hostID, componentID uint) bool {
	if access, ok := ctx.Value(restoreExecutionAccessKey{}).(restoreExecutionAccess); ok {
		return access.allows(row, hostID, componentID)
	}
	if access, ok := ctx.Value(restoreStageAccessKey{}).(restoreStageAccess); ok {
		return access.allows(row, hostID, componentID)
	}
	if access, ok := ctx.Value(restoreCheckpointAccessKey{}).(restoreCheckpointAccess); ok {
		return access.allows(row, hostID, componentID)
	}
	access, ok := ctx.Value(restoreJournalAccessKey{}).(restoreJournalAccess)
	if !ok || row.Phase != "prepared" || access.ID != row.ID || access.Token != row.Token || access.HostID != row.HostID || access.ComponentID != row.ComponentID {
		return false
	}
	if componentID != 0 {
		return componentID == row.ComponentID
	}
	return hostID == row.HostID
}

func (s *BackupService) requireRestoreJournal(ctx context.Context, id uint) error {
	if s == nil || s.db == nil || s.now == nil || id == 0 {
		return restoreJournalReject("valid service, database and ID required")
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(restoreJournalReject("operation cancelled"), err)
	}
	var tables int64
	if err := s.db.WithContext(ctx).Raw("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?", "prometheus_restore_transactions").Scan(&tables).Error; err != nil {
		return restoreJournalReadError(ctx, "journal schema unavailable", err)
	}
	if tables != 1 {
		return restoreJournalReject("journal migration required")
	}
	return nil
}

func restoreJournalWriteResult(ctx context.Context, result *gorm.DB, label string) error {
	if result.Error != nil {
		return restoreJournalReadError(ctx, "cannot persist "+label, result.Error)
	}
	if result.RowsAffected != 1 {
		return restoreJournalReject("expected one durable " + label + " row")
	}
	return nil
}

// Prepare persists only the admission intent. It leaves the running service and
// all historical artifacts untouched, and fences subsequent lifecycle actions.
func (s *BackupService) PreparePrometheusPairedRestoreTransaction(ctx context.Context, planID uint) (*models.PrometheusRestoreTransaction, error) {
	if err := s.requireRestoreJournal(ctx, planID); err != nil {
		return nil, err
	}
	var result *models.PrometheusRestoreTransaction
	err := s.withRecoveryPointClaim(ctx, planID, false, func(ctx context.Context, plan *models.ChangePlan, c *models.Component) error {
		point, _, historical, _, err := s.admitPairedRestorePreview(ctx, plan, c)
		if err != nil {
			return err
		}
		payload := prometheusRestoreJournalPayload{
			Schema: 1, PlanID: plan.ID, PointID: point.ID, BackupID: point.BackupID,
			Current:     makeUnitSnapshotBinding(c, importedArgsHash(c.ImportedArgs)),
			PointSHA256: pointDigest(point.Payload), BeforeSHA256: pointDigest(plan.Before), AfterSHA256: pointDigest(plan.After),
			PlanStatus: plan.Status, CurrentVersion: c.Version, HistoricalVersion: historical.Version,
		}
		proof, err := s.PreviewPrometheusRecoveryPointRestore(ctx, planID)
		if err != nil {
			return err
		}
		if proof.PointID != point.ID || proof.BackupID != point.BackupID || proof.ComponentID != c.ID || proof.HostID != c.HostID ||
			proof.CurrentVersion != c.Version || proof.HistoricalVersion != historical.Version {
			return restoreJournalReject("preview identity changed before preparation")
		}
		payload.ObservedAt = proof.ObservedAt
		raw, err := json.Marshal(payload)
		if err != nil || len(raw) > restoreJournalMaxPayload {
			return restoreJournalReject("cannot encode admission evidence")
		}
		var nonce [32]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return restoreJournalReject("cannot allocate transaction identity")
		}
		cid, hid := c.ID, c.HostID
		now := s.now().UTC()
		row := &models.PrometheusRestoreTransaction{
			PlanID: plan.ID, PointID: point.ID, BackupID: point.BackupID, ComponentID: cid, HostID: hid,
			ActiveComponentID: &cid, ActiveHostID: &hid, Token: hex.EncodeToString(nonce[:]), Phase: "prepared", Revision: 1,
			Payload: raw, PayloadSHA256: pointDigest(raw), CreatedAt: now, UpdatedAt: now,
		}
		if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			scoped := *s
			scoped.db = tx
			if err := scoped.checkRestoreJournalIdentity(ctx, row, &payload); err != nil {
				return err
			}
			if err := requireRestoreJournalClaims(ctx, tx, row); err != nil {
				return err
			}
			if err := rejectPairedRestoreJournalClaim(ctx, tx, hid, 0); err != nil {
				return err
			}
			if err := restoreJournalWriteResult(ctx, tx.Create(row), "prepared transaction"); err != nil {
				return err
			}
			_, err := decodeRestoreJournal(row)
			return err
		}); err != nil {
			return err
		}
		result = row
		return nil
	})
	if err != nil {
		return nil, errors.Join(ErrPrometheusRecoveryAdmission, err)
	}
	return result, nil
}

// checkRestoreJournalIdentity uses trusted database rows only. The caller has
// either just completed the full host preview, or is aborting before mutation.
func (s *BackupService) checkRestoreJournalIdentity(ctx context.Context, row *models.PrometheusRestoreTransaction, p *prometheusRestoreJournalPayload) error {
	var plan models.ChangePlan
	var c models.Component
	if err := s.db.WithContext(ctx).First(&plan, row.PlanID).Error; err != nil {
		return restoreJournalReadError(ctx, "bound plan unavailable", err)
	}
	if err := s.db.WithContext(ctx).Preload("Host").First(&c, row.ComponentID).Error; err != nil {
		return restoreJournalReadError(ctx, "bound component unavailable", err)
	}
	point, _, historical, _, err := s.admitPairedRestorePreview(ctx, &plan, &c)
	if err != nil {
		return errors.Join(ErrPrometheusRecoveryAdmission, err)
	}
	if plan.ID != p.PlanID || point.ID != p.PointID || point.BackupID != p.BackupID || row.ComponentID != c.ID || row.HostID != c.HostID ||
		plan.Status != p.PlanStatus || pointDigest(plan.Before) != p.BeforeSHA256 || pointDigest(plan.After) != p.AfterSHA256 ||
		pointDigest(point.Payload) != p.PointSHA256 || c.Version != p.CurrentVersion || historical.Version != p.HistoricalVersion ||
		makeUnitSnapshotBinding(&c, importedArgsHash(c.ImportedArgs)) != p.Current {
		return restoreJournalReject("trusted owner or history changed after admission")
	}
	return nil
}

// Both ownership checks and sibling exclusion run in the same SQLite write
// transaction as publication. New ordinary claim holders recheck after INSERT.
func requireRestoreJournalClaims(ctx context.Context, db *gorm.DB, row *models.PrometheusRestoreTransaction) error {
	host, ok := ctx.Value(hostClaimContextKey{}).(hostClaimContext)
	if !ok || host.id != row.HostID {
		return restoreJournalReject("host claim must precede component claim")
	}
	held, _ := ctx.Value(componentClaimContextKey{}).(*componentClaimContext)
	for held != nil && held.id != row.ComponentID {
		held = held.parent
	}
	if held == nil {
		return restoreJournalReject("owning component claim required")
	}
	var count int64
	if err := db.WithContext(ctx).Model(&models.HostOperationClaim{}).Where("host_id = ? AND token = ?", row.HostID, host.token).Count(&count).Error; err != nil {
		return restoreJournalReadError(ctx, "host claim unavailable", err)
	}
	if count != 1 {
		return errors.Join(restoreJournalReject("host claim lost"), ErrComponentBusy)
	}
	if err := db.WithContext(ctx).Model(&models.ComponentOperationClaim{}).Where("component_id = ? AND token = ?", row.ComponentID, held.token).Count(&count).Error; err != nil {
		return restoreJournalReadError(ctx, "component claim unavailable", err)
	}
	if count != 1 {
		return errors.Join(restoreJournalReject("component claim lost"), ErrComponentBusy)
	}
	if err := db.WithContext(ctx).Model(&models.ComponentOperationClaim{}).
		Where("component_id <> ? AND component_id IN (SELECT id FROM components WHERE host_id = ?)", row.ComponentID, row.HostID).Count(&count).Error; err != nil {
		return restoreJournalReadError(ctx, "same-host claims unavailable", err)
	}
	if count != 0 {
		return errors.Join(restoreJournalReject("another component operation owns this host"), ErrComponentBusy)
	}
	return nil
}

// This operation mints prepared-only journal access. It cannot
// clear a pause/version barrier or resolve any phase that may have mutated files.
func (s *BackupService) AbortPreparedPrometheusPairedRestoreTransaction(ctx context.Context, id uint) (*models.PrometheusRestoreTransaction, error) {
	if err := s.requireRestoreJournal(ctx, id); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var initial models.PrometheusRestoreTransaction
	if err := s.db.WithContext(ctx).First(&initial, id).Error; err != nil {
		return nil, restoreJournalReadError(ctx, "transaction unavailable", err)
	}
	if _, err := decodeRestoreJournal(&initial); err != nil {
		return nil, err
	}
	if initial.Phase == "aborted" {
		return &initial, nil
	}
	if initial.Phase != "prepared" {
		return nil, restoreJournalReject("prepared abort cannot resolve a checkpoint phase")
	}
	if held, _ := ctx.Value(componentClaimContextKey{}).(*componentClaimContext); held != nil {
		host, ok := ctx.Value(hostClaimContextKey{}).(hostClaimContext)
		if !ok || host.id != initial.HostID {
			return nil, restoreJournalReject("host claim must precede component claim")
		}
	}
	ctx = context.WithValue(ctx, restoreJournalAccessKey{}, restoreJournalAccess{ID: initial.ID, Token: initial.Token, HostID: initial.HostID, ComponentID: initial.ComponentID})
	ctx, releaseHost, err := ClaimHostOperation(ctx, s.db, initial.HostID, "paired-restore-abort")
	if err != nil {
		return nil, errors.Join(ErrPrometheusRecoveryAdmission, err)
	}
	defer releaseHost()
	ctx, releaseComponent, err := ClaimComponentOperation(ctx, s.db, initial.ComponentID, "paired-restore-abort")
	if err != nil {
		return nil, errors.Join(ErrPrometheusRecoveryAdmission, err)
	}
	defer releaseComponent()
	var result *models.PrometheusRestoreTransaction
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row models.PrometheusRestoreTransaction
		if err := tx.First(&row, id).Error; err != nil {
			return restoreJournalReadError(ctx, "transaction disappeared", err)
		}
		payload, err := decodeRestoreJournal(&row)
		if err != nil {
			return err
		}
		if row.Token != initial.Token || row.PayloadSHA256 != initial.PayloadSHA256 {
			return restoreJournalReject("transaction identity changed before abort")
		}
		if row.Phase == "aborted" {
			result = &row
			return nil
		}
		if row.Phase != "prepared" {
			return restoreJournalReject("prepared phase changed before abort")
		}
		scoped := *s
		scoped.db = tx
		if err := scoped.checkRestoreJournalIdentity(ctx, &row, payload); err != nil {
			return err
		}
		if err := requireRestoreJournalClaims(ctx, tx, &row); err != nil {
			return err
		}
		now := s.now().UTC()
		if err := restoreJournalWriteResult(ctx, tx.Model(&models.PrometheusRestoreTransaction{}).
			Where("id = ? AND token = ? AND phase = ? AND revision = ? AND payload_sha256 = ?", row.ID, row.Token, "prepared", uint64(1), row.PayloadSHA256).
			Updates(map[string]any{"phase": "aborted", "revision": uint64(2), "active_component_id": nil, "active_host_id": nil, "finished_at": now, "updated_at": now}), "aborted transaction"); err != nil {
			return err
		}
		row.Phase, row.Revision = "aborted", 2
		row.ActiveComponentID, row.ActiveHostID = nil, nil
		row.FinishedAt, row.UpdatedAt = &now, now
		result = &row
		return nil
	})
	if err != nil {
		return nil, errors.Join(ErrPrometheusRecoveryAdmission, err)
	}
	return result, nil
}
