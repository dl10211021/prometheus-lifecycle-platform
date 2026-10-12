package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"time"

	"github.com/promlifecycle/platform/models"
	"gorm.io/gorm"
)

const restoreExecutionMaxPayload = 512 << 10
const restoreExecutionMaxSteps = 96

type restoreExecutionPayload struct {
	Schema        int
	TransactionID uint
	StageRevision uint64
	StageSHA256   string
	Historical    json.RawMessage
	Claims        restoreStageClaims
	Fence         restoreStageFence
	Generations   []string
	Steps         []string
	StartedAt     time.Time
	VerifiedAt    time.Time
	Runtime       *recoveryPointRuntime
	Reclaim       *restoreStageReclaim
}

func executionTerminal(phase string) bool {
	return phase == "restore_completed" || phase == "restore_compensated"
}
func executionPhase(phase string) bool {
	switch phase {
	case "restore_executing", "restore_starting", "restore_compensating", "restore_compensation_starting", "restore_execution_reclaim", "restore_completed", "restore_compensated":
		return true
	}
	return false
}
func executionTransition(from, to string) bool {
	if to == "restore_execution_reclaim" {
		return executionPhase(from) && !executionTerminal(from)
	}
	switch from {
	case "restore_staged":
		return to == "restore_executing"
	case "restore_executing":
		return to == "restore_starting" || to == "restore_compensating"
	case "restore_starting":
		return to == "restore_completed" || to == "restore_compensating"
	case "restore_compensating":
		return to == "restore_compensation_starting"
	case "restore_compensation_starting":
		return to == "restore_compensated"
	case "restore_execution_reclaim":
		return to == "restore_compensating"
	}
	return false
}
func frozenExecutionStage(row *models.PrometheusRestoreTransaction, p *restoreExecutionPayload) models.PrometheusRestoreTransaction {
	frozen := *row
	frozen.Phase = "restore_staged"
	frozen.Revision = p.StageRevision
	frozen.Execution = nil
	frozen.ExecutionSHA256 = ""
	frozen.FinishedAt = nil
	cid, hid := row.ComponentID, row.HostID
	frozen.ActiveComponentID = &cid
	frozen.ActiveHostID = &hid
	return frozen
}
func decodeRestoreExecution(row *models.PrometheusRestoreTransaction) (*restoreExecutionPayload, *prometheusRestoreJournalPayload, error) {
	reject := func() (*restoreExecutionPayload, *prometheusRestoreJournalPayload, error) {
		return nil, nil, restoreJournalReject("invalid execution evidence")
	}
	if row == nil || !executionPhase(row.Phase) || len(row.Execution) == 0 || len(row.Execution) > restoreExecutionMaxPayload || !unitSnapshotHash.MatchString(row.ExecutionSHA256) || pointDigest(row.Execution) != row.ExecutionSHA256 {
		return reject()
	}
	var p restoreExecutionPayload
	d := json.NewDecoder(bytes.NewReader(row.Execution))
	d.DisallowUnknownFields()
	if d.Decode(&p) != nil || d.Decode(new(any)) != io.EOF || p.Schema != 1 || p.TransactionID != row.ID || p.StageSHA256 != row.StageSHA256 || p.StartedAt.IsZero() || !validRestoreStageClaims(p.Claims) || len(p.Steps) == 0 || len(p.Steps) > restoreExecutionMaxSteps || row.Revision != p.StageRevision+uint64(len(p.Steps)) {
		return reject()
	}
	previous := "restore_staged"
	for _, step := range p.Steps {
		if !executionTransition(previous, step) {
			return reject()
		}
		previous = step
	}
	if previous != row.Phase {
		return reject()
	}
	frozen := frozenExecutionStage(row, &p)
	admission, err := decodeRestoreJournal(&frozen)
	if err != nil {
		return reject()
	}
	stage, err := decodeRestoreStage(&frozen)
	if err != nil || stage == nil || p.StartedAt.Before(stage.CompletedAt) || p.StartedAt.Before(row.CreatedAt) || p.Claims.Host == stage.Claims.Host || p.Claims.Component == stage.Claims.Component {
		return reject()
	}
	historical, err := decodeRecoveryPointPayload(p.Historical)
	if err != nil || pointDigest(p.Historical) != admission.PointSHA256 || historical.BeforeSHA256 != admission.BeforeSHA256 || historical.AfterSHA256 != admission.AfterSHA256 || historical.Binding.ComponentID != row.ComponentID || historical.Binding.HostID != row.HostID || historical.Binding.Unit != admission.Current.Unit {
		return reject()
	}
	if p.Fence.BackupRoot != stage.Fence.BackupRoot || p.Fence.Root != stage.Fence.Root || p.Fence.RootIdentity != stage.Fence.RootIdentity || p.Fence.LockIdentity != stage.Fence.LockIdentity || len(p.Generations) == 0 || len(p.Generations) > restoreStageMaxGenerations || p.Generations[0] != stage.Fence.Generation || p.Fence.Generation != p.Generations[len(p.Generations)-1] {
		return reject()
	}
	handoffs := 0
	for i, step := range p.Steps {
		if step == "restore_compensating" && i > 0 && p.Steps[i-1] == "restore_execution_reclaim" {
			handoffs++
		}
	}
	if len(p.Generations) != handoffs+1 {
		return reject()
	}
	seen := map[string]bool{}
	for _, g := range p.Generations {
		if !unitSnapshotHash.MatchString(g) || seen[g] {
			return reject()
		}
		seen[g] = true
	}
	if row.Phase == "restore_execution_reclaim" {
		r := p.Reclaim
		if r == nil || len(p.Steps) < 2 || r.FromPhase != p.Steps[len(p.Steps)-2] || !unitSnapshotHash.MatchString(r.Desired) || seen[r.Desired] || !validRestoreStageClaims(r.Replacement) || r.StartedAt.Before(p.StartedAt) || r.Captured.Host != "" && r.Captured.Host != p.Claims.Host || r.Captured.Component != "" && r.Captured.Component != p.Claims.Component || r.Replacement.Host == p.Claims.Host || r.Replacement.Component == p.Claims.Component {
			return reject()
		}
	} else if p.Reclaim != nil {
		return reject()
	}
	if executionTerminal(row.Phase) {
		expectedArgv := historical.Runtime.ArgvSHA256
		if row.Phase == "restore_compensated" {
			cp, err := decodeRestoreCheckpoint(&frozen, admission)
			if err != nil {
				return reject()
			}
			expectedArgv = cp.Evidence.Runtime.ArgvSHA256
		}
		if p.Runtime == nil || p.Runtime.ArgvSHA256 != expectedArgv || row.UpdatedAt.Before(p.VerifiedAt) {
			return reject()
		}
		if row.FinishedAt == nil || row.FinishedAt.IsZero() || row.ActiveHostID != nil || row.ActiveComponentID != nil || p.VerifiedAt.IsZero() || p.VerifiedAt.Before(p.StartedAt) || p.Runtime == nil || !validExecutionRuntime(*p.Runtime) || row.FinishedAt.Before(p.VerifiedAt) {
			return reject()
		}
	} else if row.FinishedAt != nil || row.ActiveHostID == nil || *row.ActiveHostID != row.HostID || row.ActiveComponentID == nil || *row.ActiveComponentID != row.ComponentID || !p.VerifiedAt.IsZero() || p.Runtime != nil {
		return reject()
	}
	return &p, admission, nil
}
func validExecutionRuntime(r recoveryPointRuntime) bool {
	pid, pe := strconv.ParseUint(r.PID, 10, 32)
	start, se := strconv.ParseUint(r.StartTime, 10, 64)
	return pe == nil && se == nil && pid > 0 && start > 0 && unitSnapshotHash.MatchString(r.ArgvSHA256) && recoveryRootIdentity.MatchString(r.ConfigIdentity) && recoveryRootIdentity.MatchString(r.DataIdentity)
}

// GetPrometheusPairedRestoreTransaction is DB-only. It never claims, dispatches,
// repairs, or consults a live host. Malformed evidence is never a public result.
func (s *BackupService) GetPrometheusPairedRestoreTransaction(ctx context.Context, planID uint) (*models.PrometheusRestoreTransaction, error) {
	if err := s.requireRestoreJournal(ctx, planID); err != nil {
		return nil, err
	}
	var rows []models.PrometheusRestoreTransaction
	if err := s.db.WithContext(ctx).Where("plan_id = ?", planID).Order("id DESC").Limit(restoreJournalMaxPending + 1).Find(&rows).Error; err != nil {
		return nil, restoreJournalReadError(ctx, "transaction unavailable", err)
	}
	if len(rows) > restoreJournalMaxPending {
		return nil, restoreJournalReject("plan history exceeds supported bound")
	}
	for i := range rows {
		if _, err := decodeRestoreJournal(&rows[i]); err != nil {
			return nil, err
		}
	}
	if len(rows) == 0 {
		return nil, nil
	}
	for i := range rows {
		if !executionTerminal(rows[i].Phase) && rows[i].Phase != "aborted" {
			return &rows[i], nil
		}
	}
	return &rows[0], nil
}
func (s *BackupService) RestorePrometheusRecoveryPoint(ctx context.Context, planID uint) (result *models.PrometheusRestoreTransaction, resultErr error) {
	s = s.quietRestoreStageService()
	if err := s.requireRestoreStageDatabase(ctx, planID); err != nil {
		return nil, err
	}
	existing, err := s.GetPrometheusPairedRestoreTransaction(ctx, planID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if existing.Phase == "restore_completed" {
			return existing, nil
		}
		if existing.Phase == "restore_compensated" {
			return existing, restoreJournalReject("requested restore failed; previous state recovered")
		}
		return existing, restoreJournalReject("existing restore requires explicit recovery; request was not replayed")
	}
	if !s.db.Migrator().HasColumn(&models.PrometheusRestoreTransaction{}, "execution") || !s.db.Migrator().HasColumn(&models.PrometheusRestoreTransaction{}, "execution_sha256") {
		return nil, restoreJournalReject("execution migration required before preparation")
	}
	defer func() {
		if resultErr != nil {
			read, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if row, e := s.GetPrometheusPairedRestoreTransaction(read, planID); e == nil && row != nil {
				result = row
			}
			resultErr = restoreStageSafeError(ctx, resultErr)
		}
	}()
	result, resultErr = s.PreparePrometheusPairedRestoreTransaction(ctx, planID)
	if resultErr != nil {
		return
	}
	result, resultErr = s.CreatePrometheusPairedRestoreCheckpoint(ctx, result.ID)
	if resultErr != nil {
		return
	}
	result, resultErr = s.StagePrometheusPairedRestore(ctx, result.ID)
	if resultErr != nil {
		return
	}
	return s.ExecutePrometheusPairedRestore(ctx, result.ID)
}

type restoreExecutionAccessKey struct{}
type restoreExecutionAccess struct {
	Row models.PrometheusRestoreTransaction
}

func (a restoreExecutionAccess) allows(row *models.PrometheusRestoreTransaction, hostID, componentID uint) bool {
	if row == nil || row.Phase != "restore_staged" || !sameExecutionRow(&a.Row, row) {
		return false
	}
	if componentID != 0 {
		return componentID == row.ComponentID
	}
	return hostID == row.HostID
}
func sameExecutionRow(a, b *models.PrometheusRestoreTransaction) bool {
	return sameRestoreStageRow(a, b) && a.ExecutionSHA256 == b.ExecutionSHA256 && bytes.Equal(a.Execution, b.Execution) && a.FinishedAt == nil && b.FinishedAt == nil && a.ActiveHostID != nil && b.ActiveHostID != nil && *a.ActiveHostID == *b.ActiveHostID && a.ActiveComponentID != nil && b.ActiveComponentID != nil && *a.ActiveComponentID == *b.ActiveComponentID
}
func (s *BackupService) executionOwner(ctx context.Context, old *models.PrometheusRestoreTransaction) (*models.Component, error) {
	var row models.PrometheusRestoreTransaction
	if err := s.db.WithContext(ctx).First(&row, old.ID).Error; err != nil {
		return nil, err
	}
	admission, err := decodeRestoreJournal(&row)
	if err != nil {
		return nil, err
	}
	if !sameExecutionRow(&row, old) {
		return nil, restoreJournalReject("execution owner changed")
	}
	if err := s.checkRestoreJournalIdentity(ctx, &row, admission); err != nil {
		return nil, err
	}
	if err := requireRestoreJournalClaims(ctx, s.db, &row); err != nil {
		return nil, err
	}
	if executionPhase(row.Phase) {
		p, _, err := decodeRestoreExecution(&row)
		if err != nil {
			return nil, err
		}
		claims, err := heldRestoreStageClaims(ctx, &row)
		if err != nil || claims != p.Claims {
			return nil, restoreJournalReject("execution claims changed")
		}
	}
	var c models.Component
	if err := s.db.WithContext(ctx).Preload("Host").First(&c, row.ComponentID).Error; err != nil {
		return nil, err
	}
	return &c, nil
}
func (s *BackupService) nextRestoreExecution(old *models.PrometheusRestoreTransaction, phase string, p *restoreExecutionPayload) (*models.PrometheusRestoreTransaction, error) {
	if !executionTransition(old.Phase, phase) {
		return nil, restoreJournalReject("unsupported execution transition")
	}
	p.Steps = append(append([]string(nil), p.Steps...), phase)
	raw, err := json.Marshal(p)
	if err != nil || len(raw) > restoreExecutionMaxPayload {
		return nil, restoreJournalReject("cannot encode execution")
	}
	next := *old
	next.Phase = phase
	next.Revision++
	next.Execution = raw
	next.ExecutionSHA256 = pointDigest(raw)
	next.UpdatedAt = s.now().UTC()
	if executionTerminal(phase) {
		next.FinishedAt = &next.UpdatedAt
		next.ActiveHostID = nil
		next.ActiveComponentID = nil
	}
	if _, err := decodeRestoreJournal(&next); err != nil {
		return nil, err
	}
	return &next, nil
}
func restoreExecutionCAS(ctx context.Context, tx *gorm.DB, old, next *models.PrometheusRestoreTransaction) error {
	result := tx.Model(&models.PrometheusRestoreTransaction{}).
		Where("id = ? AND token = ? AND plan_id = ? AND point_id = ? AND backup_id = ? AND host_id = ? AND component_id = ? AND active_host_id = ? AND active_component_id = ? AND finished_at IS NULL AND phase = ? AND revision = ? AND payload_sha256 = ? AND payload = ? AND checkpoint_sha256 = ? AND checkpoint = ? AND stage_sha256 = ? AND stage = ? AND COALESCE(execution_sha256, '') = ? AND COALESCE(execution, '') = ?", old.ID, old.Token, old.PlanID, old.PointID, old.BackupID, old.HostID, old.ComponentID, old.HostID, old.ComponentID, old.Phase, old.Revision, old.PayloadSHA256, string(old.Payload), old.CheckpointSHA256, string(old.Checkpoint), old.StageSHA256, string(old.Stage), old.ExecutionSHA256, string(old.Execution)).
		Updates(map[string]any{"phase": next.Phase, "revision": next.Revision, "execution": string(next.Execution), "execution_sha256": next.ExecutionSHA256, "updated_at": next.UpdatedAt, "active_host_id": next.ActiveHostID, "active_component_id": next.ActiveComponentID, "finished_at": next.FinishedAt})
	return restoreJournalWriteResult(ctx, result, "execution phase")
}
func (s *BackupService) advanceRestoreExecution(ctx context.Context, old *models.PrometheusRestoreTransaction, phase string, p *restoreExecutionPayload, configs ...restoredConfigActivation) (*models.PrometheusRestoreTransaction, error) {
	next, err := s.nextRestoreExecution(old, phase, p)
	if err != nil {
		return nil, err
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		check := *s
		check.db = tx
		c, err := check.executionOwner(ctx, old)
		if err != nil {
			return err
		}
		if executionTerminal(phase) {
			if err := activateRestoredConfigs(tx, configs); err != nil {
				return err
			}
			admission, err := decodeRestoreJournal(old)
			if err != nil {
				return err
			}
			version, image := admission.CurrentVersion, c.Image
			planUpdates := map[string]any{}
			if phase == "restore_completed" {
				var plan models.ChangePlan
				if err := tx.First(&plan, old.PlanID).Error; err != nil {
					return err
				}
				if pointDigest(plan.Before) != admission.BeforeSHA256 || pointDigest(plan.After) != admission.AfterSHA256 {
					return restoreJournalReject("historical metadata binding changed")
				}
				before, err := prometheusUpgradeSnapshot(&plan, c)
				if err != nil {
					return err
				}
				version, image = admission.HistoricalVersion, before.Component.Image
				planUpdates["status"] = models.ChangeStatusRolledBack
				planUpdates["rolled_back_at"] = next.FinishedAt
			}
			evidence, _ := json.Marshal(map[string]any{"transaction_id": old.ID, "point_id": old.PointID, "phase": phase, "verified_at": p.VerifiedAt, "runtime_verified": true})
			planUpdates["rollback_evidence"] = string(evidence)
			if err := restoreJournalWriteResult(ctx, tx.Model(&models.ChangePlan{}).Where("id = ? AND status = ? AND CAST(before AS BLOB) = ? AND CAST(after AS BLOB) = ?", old.PlanID, admission.PlanStatus, sqlPlanBytes(tx, old.PlanID, "before"), sqlPlanBytes(tx, old.PlanID, "after")).Updates(planUpdates), "restore plan outcome"); err != nil {
				return err
			}
			if err := restoreJournalWriteResult(ctx, tx.Model(&models.Component{}).Where("id = ? AND version = ? AND image = ?", c.ID, admission.CurrentVersion, c.Image).Updates(map[string]any{"version": version, "image": image, "status": models.StatusRunning, "error": ""}), "restored component"); err != nil {
				return err
			}
			if err := restoreJournalWriteResult(ctx, tx.Where("host_id = ? AND token = ?", old.HostID, p.Claims.Host).Delete(&models.HostOperationClaim{}), "released execution host claim"); err != nil {
				return err
			}
			if err := restoreJournalWriteResult(ctx, tx.Where("component_id = ? AND token = ?", old.ComponentID, p.Claims.Component).Delete(&models.ComponentOperationClaim{}), "released execution component claim"); err != nil {
				return err
			}
		}
		return restoreExecutionCAS(ctx, tx, old, next)
	})
	if err != nil {
		return nil, err
	}
	return next, nil
}

// Identity has already been checked inside this transaction; retaining exact
// snapshot bytes in the final UPDATE also defends callback-triggered changes.
// RawMessage map updates can use SQLite BLOB storage despite the TEXT column.
// The predicate casts storage to BLOB and binds bytes, preserving exact identity.
func sqlPlanBytes(tx *gorm.DB, id uint, column string) []byte {
	var plan models.ChangePlan
	if tx.Select(column).First(&plan, id).Error != nil {
		return nil
	}
	if column == "before" {
		return []byte(plan.Before)
	}
	return []byte(plan.After)
}

func executionSafeFailure(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	return errors.Join(restoreJournalReject("restore execution failed; inspect durable transaction outcome"), restoreStageSafeError(ctx, err))
}
