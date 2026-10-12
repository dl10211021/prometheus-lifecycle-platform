package services

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	coressh "github.com/promlifecycle/platform/core/ssh"
	"github.com/promlifecycle/platform/models"
	"gorm.io/gorm"
)

type restoreCheckpointAccessKey struct{}
type restoreCheckpointAccess struct {
	ID, HostID, ComponentID                         uint
	Token, Phase, AdmissionSHA256, CheckpointSHA256 string
	Revision                                        uint64
}

func (a restoreCheckpointAccess) allows(row *models.PrometheusRestoreTransaction, hostID, componentID uint) bool {
	if row == nil || (a.Phase != "prepared" && a.Phase != "checkpoint_ready") || row.ID != a.ID || row.Token != a.Token || row.HostID != a.HostID || row.ComponentID != a.ComponentID || row.Phase != a.Phase || row.Revision != a.Revision || row.PayloadSHA256 != a.AdmissionSHA256 || row.CheckpointSHA256 != a.CheckpointSHA256 {
		return false
	}
	if componentID != 0 {
		return componentID == a.ComponentID
	}
	return hostID == a.HostID
}
func restoreCheckpointSafeError(ctx context.Context, cause error) error {
	if cause == nil {
		return nil
	}
	out := []error{restoreJournalReject("checkpoint operation rejected; durable recovery remains authoritative")}
	for _, kind := range []error{context.Canceled, context.DeadlineExceeded, coressh.ErrRemoteTerminationUnconfirmed, ErrComponentBusy} {
		if errors.Is(cause, kind) || errors.Is(ctx.Err(), kind) {
			out = append(out, kind)
		}
	}
	return errors.Join(out...)
}
func sameRestoreCheckpointRow(a, b *models.PrometheusRestoreTransaction) bool {
	return a.ID == b.ID && a.Token == b.Token && a.PlanID == b.PlanID && a.PointID == b.PointID && a.BackupID == b.BackupID && a.HostID == b.HostID && a.ComponentID == b.ComponentID &&
		a.Phase == b.Phase && a.Revision == b.Revision && a.PayloadSHA256 == b.PayloadSHA256 && a.CheckpointSHA256 == b.CheckpointSHA256
}
func (s *BackupService) checkpointOwner(ctx context.Context, expected *models.PrometheusRestoreTransaction) (*models.PrometheusRestoreTransaction, *prometheusRestoreJournalPayload, *models.Component, error) {
	var row models.PrometheusRestoreTransaction
	var c models.Component
	var admission *prometheusRestoreJournalPayload
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&row, expected.ID).Error; err != nil {
			return err
		}
		var err error
		admission, err = decodeRestoreJournal(&row)
		if err != nil {
			return err
		}
		if !sameRestoreCheckpointRow(&row, expected) {
			return restoreJournalReject("checkpoint transaction changed")
		}
		check := *s
		check.db = tx
		if err := check.checkRestoreJournalIdentity(ctx, &row, admission); err != nil {
			return err
		}
		if err := requireRestoreJournalClaims(ctx, tx, &row); err != nil {
			return err
		}
		return tx.Preload("Host").First(&c, row.ComponentID).Error
	})
	return &row, admission, &c, err
}
func (s *BackupService) advanceRestoreCheckpoint(ctx context.Context, previous *models.PrometheusRestoreTransaction, phase string, proof *prometheusRestoreCheckpointPayload) (*models.PrometheusRestoreTransaction, error) {
	raw, err := json.Marshal(proof)
	if err != nil || len(raw) > restoreCheckpointMaxPayload {
		return nil, restoreJournalReject("cannot encode checkpoint")
	}
	next := *previous
	next.Phase = phase
	next.Revision++
	next.Checkpoint = raw
	next.CheckpointSHA256 = pointDigest(raw)
	next.UpdatedAt = s.now().UTC()
	if _, err := decodeRestoreJournal(&next); err != nil {
		return nil, err
	}
	allowed := (previous.Phase == "prepared" && phase == "checkpointing") || (previous.Phase == "checkpointing" && phase == "checkpoint_stopping") || (previous.Phase == "checkpoint_stopping" && phase == "checkpoint_ready")
	if !allowed {
		return nil, restoreJournalReject("checkpoint transition is not supported")
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row models.PrometheusRestoreTransaction
		if err := tx.First(&row, previous.ID).Error; err != nil {
			return err
		}
		admission, err := decodeRestoreJournal(&row)
		if err != nil {
			return err
		}
		if !sameRestoreCheckpointRow(&row, previous) {
			return restoreJournalReject("checkpoint transaction changed before commit")
		}
		check := *s
		check.db = tx
		if err := check.checkRestoreJournalIdentity(ctx, &row, admission); err != nil {
			return err
		}
		if err := requireRestoreJournalClaims(ctx, tx, &row); err != nil {
			return err
		}
		result := tx.Model(&models.PrometheusRestoreTransaction{}).Where("id = ? AND token = ? AND phase = ? AND revision = ? AND payload_sha256 = ? AND COALESCE(checkpoint_sha256, '') = ?", row.ID, row.Token, row.Phase, row.Revision, row.PayloadSHA256, row.CheckpointSHA256).
			Updates(map[string]any{"phase": next.Phase, "revision": next.Revision, "checkpoint": string(raw), "checkpoint_sha256": next.CheckpointSHA256, "updated_at": next.UpdatedAt})
		return restoreJournalWriteResult(ctx, result, "checkpoint phase")
	})
	if err != nil {
		return nil, err
	}
	return &next, nil
}

// Capture success leaves the original service stopped and its durable fence
// active. A repeated ready call only verifies the original pinned checkpoint.
func (s *BackupService) CreatePrometheusPairedRestoreCheckpoint(ctx context.Context, id uint) (*models.PrometheusRestoreTransaction, error) {
	return s.runRestoreCheckpoint(ctx, id, true)
}
func (s *BackupService) VerifyPrometheusPairedRestoreCheckpoint(ctx context.Context, id uint) (*models.PrometheusRestoreTransaction, error) {
	return s.runRestoreCheckpoint(ctx, id, false)
}
func (s *BackupService) runRestoreCheckpoint(ctx context.Context, id uint, create bool) (result *models.PrometheusRestoreTransaction, resultErr error) {
	if err := s.requireRestoreJournal(ctx, id); err != nil {
		return nil, restoreCheckpointSafeError(ctx, err)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	// Classify before our own cleanup cancellation runs.
	defer func() { resultErr = restoreCheckpointSafeError(ctx, resultErr) }()
	var initial models.PrometheusRestoreTransaction
	if err := s.db.WithContext(ctx).First(&initial, id).Error; err != nil {
		return nil, err
	}
	if _, err := decodeRestoreJournal(&initial); err != nil {
		return nil, err
	}
	if initial.Phase != "checkpoint_ready" && (!create || initial.Phase != "prepared") {
		return nil, restoreJournalReject("checkpoint phase cannot be retried")
	}
	if held, _ := ctx.Value(componentClaimContextKey{}).(*componentClaimContext); held != nil {
		host, ok := ctx.Value(hostClaimContextKey{}).(hostClaimContext)
		if !ok || host.id != initial.HostID {
			return nil, restoreJournalReject("host claim must precede component claim")
		}
	}
	access := restoreCheckpointAccess{ID: initial.ID, HostID: initial.HostID, ComponentID: initial.ComponentID, Token: initial.Token, Phase: initial.Phase, Revision: initial.Revision, AdmissionSHA256: initial.PayloadSHA256, CheckpointSHA256: initial.CheckpointSHA256}
	ctx = context.WithValue(ctx, restoreCheckpointAccessKey{}, access)
	ctx, releaseHost, err := ClaimHostOperation(ctx, s.db, initial.HostID, "paired-restore-checkpoint")
	if err != nil {
		return nil, err
	}
	defer releaseHost()
	ctx, releaseComponent, err := ClaimComponentOperation(ctx, s.db, initial.ComponentID, "paired-restore-checkpoint")
	if err != nil {
		return nil, err
	}
	defer releaseComponent()
	row, admission, c, err := s.checkpointOwner(ctx, &initial)
	if err != nil {
		return nil, err
	}
	runner, release, err := s.pointRunner(ctx, c)
	if err != nil {
		return nil, err
	}
	defer release()
	scoped := s.onPointRunner(runner)
	if row.Phase == "checkpoint_ready" {
		proof, err := decodeRestoreCheckpoint(row, admission)
		if err != nil {
			return nil, err
		}
		if err := scoped.verifyRestoreCheckpointHost(ctx, runner, c, proof); err != nil {
			return nil, err
		}
		checked, _, _, err := s.checkpointOwner(ctx, row)
		if err != nil {
			return nil, err
		}
		return checked, nil
	}
	return scoped.capturePairedRestoreCheckpointHost(ctx, runner, row, admission, c)
}
