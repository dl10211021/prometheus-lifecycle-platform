package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	coressh "github.com/promlifecycle/platform/core/ssh"
	"github.com/promlifecycle/platform/models"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"time"
)

// Stage payloads contain private writer/claim tokens. Suppress SQL logging on
// this scoped session even when the application's ordinary logger is verbose.
func (s *BackupService) quietRestoreStageService() *BackupService {
	if s == nil || s.db == nil {
		return s
	}
	scoped := *s
	scoped.db = s.db.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)})
	return &scoped
}

type restoreStageAccessKey struct{}
type restoreStageAccess struct {
	Row models.PrometheusRestoreTransaction
}

func (a restoreStageAccess) allows(row *models.PrometheusRestoreTransaction, hostID, componentID uint) bool {
	if row == nil || row.Phase != "checkpoint_ready" || !sameRestoreStageRow(&a.Row, row) {
		return false
	}
	if componentID != 0 {
		return componentID == row.ComponentID
	}
	return hostID == row.HostID
}
func sameRestoreStageRow(a, b *models.PrometheusRestoreTransaction) bool {
	return sameRestoreCheckpointRow(a, b) && a.StageSHA256 == b.StageSHA256 && bytes.Equal(a.Payload, b.Payload) && bytes.Equal(a.Checkpoint, b.Checkpoint) && bytes.Equal(a.Stage, b.Stage)
}
func restoreStageSafeError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	out := []error{restoreJournalReject("stage operation rejected; durable recovery remains authoritative")}
	for _, kind := range []error{context.Canceled, context.DeadlineExceeded, coressh.ErrRemoteTerminationUnconfirmed, ErrComponentBusy, errRestoreStageFenceBusy} {
		if errors.Is(err, kind) || errors.Is(ctx.Err(), kind) {
			out = append(out, kind)
		}
	}
	return errors.Join(out...)
}
func heldRestoreStageClaims(ctx context.Context, row *models.PrometheusRestoreTransaction) (restoreStageClaims, error) {
	h, ok := ctx.Value(hostClaimContextKey{}).(hostClaimContext)
	c, _ := ctx.Value(componentClaimContextKey{}).(*componentClaimContext)
	for c != nil && c.id != row.ComponentID {
		c = c.parent
	}
	if !ok || h.id != row.HostID || c == nil {
		return restoreStageClaims{}, restoreJournalReject("owning stage claims required")
	}
	claims := restoreStageClaims{h.token, c.token}
	if !validRestoreStageClaims(claims) {
		return claims, restoreJournalReject("invalid stage claims")
	}
	return claims, nil
}
func (s *BackupService) stageOwner(ctx context.Context, expected *models.PrometheusRestoreTransaction, pinned bool) (*models.Component, error) {
	row, _, c, err := s.checkpointOwner(ctx, expected)
	if err != nil {
		return nil, err
	}
	if !sameRestoreStageRow(row, expected) {
		return nil, restoreJournalReject("stage owner changed")
	}
	if pinned {
		p, err := decodeRestoreStage(row)
		if err != nil {
			return nil, err
		}
		claims, err := heldRestoreStageClaims(ctx, row)
		if err != nil || p == nil || claims != p.Claims {
			return nil, restoreJournalReject("stage writer claims changed")
		}
	}
	return c, nil
}
func restoreStageCAS(ctx context.Context, tx *gorm.DB, old, next *models.PrometheusRestoreTransaction) error {
	result := tx.Model(&models.PrometheusRestoreTransaction{}).
		Where("id = ? AND token = ? AND plan_id = ? AND point_id = ? AND backup_id = ? AND host_id = ? AND component_id = ? AND active_host_id = ? AND active_component_id = ? AND finished_at IS NULL AND phase = ? AND revision = ? AND payload_sha256 = ? AND payload = ? AND checkpoint_sha256 = ? AND checkpoint = ? AND COALESCE(stage_sha256, '') = ? AND COALESCE(stage, '') = ?", old.ID, old.Token, old.PlanID, old.PointID, old.BackupID, old.HostID, old.ComponentID, old.HostID, old.ComponentID, old.Phase, old.Revision, old.PayloadSHA256, string(old.Payload), old.CheckpointSHA256, string(old.Checkpoint), old.StageSHA256, string(old.Stage)).
		Updates(map[string]any{"phase": next.Phase, "revision": next.Revision, "stage": string(next.Stage), "stage_sha256": next.StageSHA256, "updated_at": next.UpdatedAt})
	return restoreJournalWriteResult(ctx, result, "stage phase")
}
func (s *BackupService) nextRestoreStage(old *models.PrometheusRestoreTransaction, phase string, p *prometheusRestoreStagePayload) (*models.PrometheusRestoreTransaction, error) {
	if old.Revision >= 100 {
		return nil, restoreJournalReject("stage revision bound reached")
	}
	raw, err := json.Marshal(p)
	if err != nil || len(raw) > restoreStageMaxPayload {
		return nil, restoreJournalReject("cannot encode stage")
	}
	next := *old
	next.Phase = phase
	next.Revision++
	next.Stage = raw
	next.StageSHA256 = pointDigest(raw)
	next.UpdatedAt = s.now().UTC()
	if _, err := decodeRestoreJournal(&next); err != nil {
		return nil, err
	}
	return &next, nil
}
func (s *BackupService) advanceRestoreStage(ctx context.Context, old *models.PrometheusRestoreTransaction, phase string, p *prometheusRestoreStagePayload) (*models.PrometheusRestoreTransaction, error) {
	if !(old.Phase == "checkpoint_ready" && phase == "restore_stage_intent" || old.Phase == "restore_stage_intent" && phase == "restore_stage_copying" || old.Phase == "restore_stage_copying" && phase == "restore_staged") {
		return nil, restoreJournalReject("unsupported stage transition")
	}
	next, err := s.nextRestoreStage(old, phase, p)
	if err != nil {
		return nil, err
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		scoped := *s
		scoped.db = tx
		if _, err := scoped.stageOwner(ctx, old, old.Phase != "checkpoint_ready"); err != nil {
			return err
		}
		claims, err := heldRestoreStageClaims(ctx, old)
		if err != nil || claims != p.Claims {
			return restoreJournalReject("stage publication lost claims")
		}
		return restoreStageCAS(ctx, tx, old, next)
	})
	if err != nil {
		return nil, err
	}
	return next, nil
}
func (s *BackupService) StagePrometheusPairedRestore(ctx context.Context, id uint) (*models.PrometheusRestoreTransaction, error) {
	return s.runRestoreStage(ctx, id, true)
}
func (s *BackupService) VerifyPrometheusPairedRestoreStage(ctx context.Context, id uint) (*models.PrometheusRestoreTransaction, error) {
	return s.runRestoreStage(ctx, id, false)
}
func (s *BackupService) runRestoreStage(ctx context.Context, id uint, create bool) (result *models.PrometheusRestoreTransaction, resultErr error) {
	s = s.quietRestoreStageService()
	if err := s.requireRestoreStageDatabase(ctx, id); err != nil {
		return nil, restoreStageSafeError(ctx, err)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	defer func() { resultErr = restoreStageSafeError(ctx, resultErr) }()
	var row models.PrometheusRestoreTransaction
	if err := s.db.WithContext(ctx).First(&row, id).Error; err != nil {
		return nil, err
	}
	if _, err := decodeRestoreJournal(&row); err != nil {
		return nil, err
	}
	if row.Phase != "restore_staged" && (!create || row.Phase != "checkpoint_ready") {
		return nil, restoreJournalReject("stage requires explicit interrupted-stage recovery")
	}
	if row.Phase == "restore_staged" {
		return s.observeRestoreStage(ctx, &row)
	}
	if c, _ := ctx.Value(componentClaimContextKey{}).(*componentClaimContext); c != nil {
		h, ok := ctx.Value(hostClaimContextKey{}).(hostClaimContext)
		if !ok || h.id != row.HostID {
			return nil, restoreJournalReject("host claim must precede component claim")
		}
	}
	ctx = context.WithValue(ctx, restoreStageAccessKey{}, restoreStageAccess{row})
	ctx, releaseH, err := ClaimHostOperation(ctx, s.db, row.HostID, "paired-restore-stage")
	if err != nil {
		return nil, err
	}
	defer releaseH()
	ctx, releaseC, err := ClaimComponentOperation(ctx, s.db, row.ComponentID, "paired-restore-stage")
	if err != nil {
		return nil, err
	}
	defer releaseC()
	c, err := s.stageOwner(ctx, &row, false)
	if err != nil {
		return nil, err
	}
	r, release, err := s.pointRunner(ctx, c)
	if err != nil {
		return nil, err
	}
	defer release()
	scoped := s.onPointRunner(r)
	if _, _, err := scoped.restoreStageSources(ctx, r, &row, c); err != nil {
		return nil, err
	}
	token, err := randomRestoreStageToken(32)
	if err != nil {
		return nil, err
	}
	claims, err := heldRestoreStageClaims(ctx, &row)
	if err != nil {
		return nil, err
	}
	p := &prometheusRestoreStagePayload{Schema: 1, TransactionID: row.ID, AdmissionSHA256: row.PayloadSHA256, CheckpointSHA256: row.CheckpointSHA256,
		Fence: restoreStageFence{BackupRoot: s.backupRoot, Root: restoreStageRoot(s.backupRoot, &row), Generation: token}, Generations: []string{token}, Claims: claims, StartedAt: s.now().UTC()}
	intent, err := s.advanceRestoreStage(ctx, &row, "restore_stage_intent", p)
	if err != nil {
		return nil, err
	}
	if _, err := s.stageOwner(ctx, intent, true); err != nil {
		return nil, err
	}
	p.Fence, err = initializeRestoreStageFence(ctx, r, p.Fence)
	if err != nil {
		return nil, err
	}
	copying, err := s.advanceRestoreStage(ctx, intent, "restore_stage_copying", p)
	if err != nil {
		return nil, err
	}
	return scoped.finishRestoreStage(ctx, r, copying, c, p)
}
func (s *BackupService) finishRestoreStage(ctx context.Context, r hostCommandRunner, row *models.PrometheusRestoreTransaction, c *models.Component, p *prometheusRestoreStagePayload) (*models.PrometheusRestoreTransaction, error) {
	current, historical, err := s.restoreStageSources(ctx, r, row, c)
	if err != nil {
		return nil, err
	}
	if _, err := s.stageOwner(ctx, row, true); err != nil {
		return nil, err
	}
	if err := copyRestoreStageArtifacts(ctx, r, p.Fence, current, historical); err != nil {
		return nil, err
	}
	if err := s.verifyRestoreStageHost(ctx, r, row, c, p); err != nil {
		return nil, err
	}
	p.CompletedAt = s.now().UTC()
	return s.advanceRestoreStage(ctx, row, "restore_staged", p)
}

// Public stage work must own each real SQLite COMMIT. A GORM outer transaction
// turns Transaction into SAVEPOINT and cannot authorize subsequent host writes.
func (s *BackupService) requireRestoreStageDatabase(ctx context.Context, id uint) error {
	if s != nil && s.db != nil && s.db.Statement != nil {
		if _, transactional := s.db.Statement.ConnPool.(gorm.TxCommitter); transactional {
			return restoreJournalReject("stage service requires a non-transactional database")
		}
	}
	return s.requireRestoreJournal(ctx, id)
}
