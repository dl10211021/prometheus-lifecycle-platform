package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/promlifecycle/platform/models"
	"gorm.io/gorm"
)

const restoreJournalMaxPayload = 64 << 10
const restoreJournalMaxPending = 1024

type prometheusRestoreJournalPayload struct {
	Schema                                 int
	PlanID, PointID, BackupID              uint
	Current                                unitSnapshotBinding
	PointSHA256, BeforeSHA256, AfterSHA256 string
	PlanStatus                             models.ChangeStatus
	CurrentVersion, HistoricalVersion      string
	ObservedAt                             time.Time
}

func restoreJournalReject(reason string) error {
	return fmt.Errorf("%w: paired restore transaction: %s", ErrPrometheusRecoveryAdmission, reason)
}

func restoreJournalReadError(ctx context.Context, reason string, cause error) error {
	result := []error{restoreJournalReject(reason)}
	for _, kind := range []error{context.Canceled, context.DeadlineExceeded} {
		if errors.Is(cause, kind) || errors.Is(ctx.Err(), kind) {
			result = append(result, kind)
		}
	}
	return errors.Join(result...)
}

func decodeRestoreJournal(row *models.PrometheusRestoreTransaction) (*prometheusRestoreJournalPayload, error) {
	if row != nil && executionPhase(row.Phase) {
		_, admission, err := decodeRestoreExecution(row)
		return admission, err
	}
	if row != nil && (len(row.Execution) != 0 || row.ExecutionSHA256 != "") {
		return nil, restoreJournalReject("legacy phase contains execution evidence")
	}
	if row == nil || row.ID == 0 || row.PlanID == 0 || row.PointID == 0 || row.BackupID == 0 || row.ComponentID == 0 || row.HostID == 0 ||
		!unitSnapshotHash.MatchString(row.Token) || len(row.Payload) == 0 || len(row.Payload) > restoreJournalMaxPayload ||
		!unitSnapshotHash.MatchString(row.PayloadSHA256) || pointDigest(row.Payload) != row.PayloadSHA256 ||
		row.CreatedAt.IsZero() || row.UpdatedAt.IsZero() {
		return nil, restoreJournalReject("invalid durable journal identity")
	}
	switch row.Phase {
	case "prepared", "checkpointing", "checkpoint_stopping", "checkpoint_ready":
		expectedRevision := map[string]uint64{"prepared": 1, "checkpointing": 2, "checkpoint_stopping": 3, "checkpoint_ready": 4}[row.Phase]
		if row.Revision != expectedRevision || row.FinishedAt != nil || row.ActiveComponentID == nil || *row.ActiveComponentID != row.ComponentID ||
			row.ActiveHostID == nil || *row.ActiveHostID != row.HostID {
			return nil, restoreJournalReject("incomplete prepared ownership")
		}
	case "restore_stage_intent", "restore_stage_copying", "restore_stage_reclaim", "restore_staged":
		if row.Revision < 5 || row.Revision > 100 || row.FinishedAt != nil || row.ActiveComponentID == nil || *row.ActiveComponentID != row.ComponentID || row.ActiveHostID == nil || *row.ActiveHostID != row.HostID {
			return nil, restoreJournalReject("invalid staged ownership or revision")
		}
	case "aborted":
		if row.Revision != 2 || row.FinishedAt == nil || row.FinishedAt.IsZero() || row.ActiveComponentID != nil || row.ActiveHostID != nil {
			return nil, restoreJournalReject("incomplete aborted ownership")
		}
	default:
		return nil, restoreJournalReject("unsupported transaction phase remains fenced")
	}
	var payload prometheusRestoreJournalPayload
	decoder := json.NewDecoder(bytes.NewReader(row.Payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return nil, restoreJournalReject("invalid journal payload")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, restoreJournalReject("trailing journal payload")
	}
	if payload.Schema != 1 || payload.PlanID != row.PlanID || payload.PointID != row.PointID || payload.BackupID != row.BackupID ||
		payload.Current.ComponentID != row.ComponentID || payload.Current.HostID != row.HostID || !validUnitSnapshotBinding(payload.Current) ||
		!unitSnapshotHash.MatchString(payload.PointSHA256) || !unitSnapshotHash.MatchString(payload.BeforeSHA256) ||
		!unitSnapshotHash.MatchString(payload.AfterSHA256) || payload.ObservedAt.IsZero() ||
		(payload.PlanStatus != models.ChangeStatusFailed && payload.PlanStatus != models.ChangeStatusVerified) {
		return nil, restoreJournalReject("journal evidence does not bind its owner and history")
	}
	if _, ok := stablePrometheusVersion(payload.CurrentVersion); !ok {
		return nil, restoreJournalReject("unknown journal current version")
	}
	if _, ok := stablePrometheusVersion(payload.HistoricalVersion); !ok {
		return nil, restoreJournalReject("unknown journal historical version")
	}
	if _, err := decodeRestoreCheckpoint(row, &payload); err != nil {
		return nil, err
	}
	if _, err := decodeRestoreStage(row); err != nil {
		return nil, err
	}
	return &payload, nil
}

func pendingPairedRestoreJournals(ctx context.Context, db *gorm.DB) ([]models.PrometheusRestoreTransaction, error) {
	if db == nil {
		return nil, restoreJournalReject("database required")
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(restoreJournalReject("journal read cancelled"), err)
	}
	// Explicit schema read distinguishes legacy absence from a failed read.
	// Migrator.HasTable alone cannot make that distinction.
	var tables int64
	if err := db.WithContext(ctx).Raw("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?", "prometheus_restore_transactions").Scan(&tables).Error; err != nil {
		return nil, restoreJournalReadError(ctx, "journal schema unavailable", err)
	}
	if tables == 0 {
		return nil, nil
	}
	if tables != 1 {
		return nil, restoreJournalReject("ambiguous journal schema")
	}
	// Terminal execution rows are validated too, but do not consume the pending
	// bound. Page history so completed recoveries cannot eventually fence startup.
	var pending []models.PrometheusRestoreTransaction
	var cursor uint
	firstPage := true
	for {
		var rows []models.PrometheusRestoreTransaction
		query := db.WithContext(ctx).Where("phase IS NULL OR phase <> ? OR revision IS NULL OR revision <> 2 OR active_host_id IS NOT NULL OR active_component_id IS NOT NULL OR finished_at IS NULL", "aborted")
		if !firstPage {
			query = query.Where("id > ?", cursor)
		}
		if err := query.Order("id ASC").Limit(256).Find(&rows).Error; err != nil {
			return nil, restoreJournalReadError(ctx, "journal inventory unavailable", err)
		}
		for i := range rows {
			if _, err := decodeRestoreJournal(&rows[i]); err != nil {
				return nil, err
			}
			if !executionTerminal(rows[i].Phase) {
				pending = append(pending, rows[i])
			}
			if len(pending) > restoreJournalMaxPending {
				return nil, restoreJournalReject("pending journal inventory exceeds supported bound")
			}
			cursor = rows[i].ID
		}
		if len(rows) < 256 {
			return pending, nil
		}
		firstPage = false
	}
}

func rejectPairedRestoreJournalClaim(ctx context.Context, db *gorm.DB, hostID, componentID uint) error {
	rows, err := pendingPairedRestoreJournals(ctx, db)
	if err != nil || len(rows) == 0 {
		return err
	}
	if componentID != 0 {
		var current models.Component
		result := db.WithContext(ctx).Select("id", "host_id").Where("id = ?", componentID).Find(&current)
		if result.Error != nil {
			return restoreJournalReadError(ctx, "claim owning host unavailable", result.Error)
		}
		hostID = current.HostID
	}
	for _, row := range rows {
		if restoreJournalAccessAllows(ctx, &row, hostID, componentID) {
			continue
		}
		if row.HostID == hostID || (componentID != 0 && row.ComponentID == componentID) {
			return restoreJournalReject("pending recovery must be resolved before another lifecycle operation")
		}
	}
	return nil
}

func rejectPendingPairedRestoreJournals(ctx context.Context, db *gorm.DB) error {
	rows, err := pendingPairedRestoreJournals(ctx, db)
	if err != nil {
		return err
	}
	if len(rows) != 0 {
		return restoreJournalReject("pending recovery must be resolved before startup")
	}
	return nil
}
