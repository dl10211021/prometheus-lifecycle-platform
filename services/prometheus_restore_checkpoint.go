package services

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/promlifecycle/platform/models"
)

const restoreCheckpointMaxPayload = 256 << 10

type prometheusRestoreCheckpointPayload struct {
	Schema                int
	TransactionID         uint
	AdmissionSHA256, Root string
	StartedAt, StoppedAt  time.Time
	Evidence              *prometheusRecoveryPayload
}

func restoreCheckpointRoot(root string, row *models.PrometheusRestoreTransaction) string {
	return filepath.Join(root, fmt.Sprintf("restore-%d-%s", row.ID, row.Token[:32]))
}

// Structural decoding deliberately does not access a host. An interrupted
// reservation may have no directory yet, and remains valid rollback inventory.
func decodeRestoreCheckpoint(row *models.PrometheusRestoreTransaction, admission *prometheusRestoreJournalPayload) (*prometheusRestoreCheckpointPayload, error) {
	if row.Phase == "prepared" || row.Phase == "aborted" {
		if len(row.Checkpoint) != 0 || row.CheckpointSHA256 != "" {
			return nil, restoreJournalReject("pre-checkpoint phase contains checkpoint evidence")
		}
		return nil, nil
	}
	if len(row.Checkpoint) == 0 || len(row.Checkpoint) > restoreCheckpointMaxPayload || !unitSnapshotHash.MatchString(row.CheckpointSHA256) || pointDigest(row.Checkpoint) != row.CheckpointSHA256 {
		return nil, restoreJournalReject("invalid checkpoint evidence digest or size")
	}
	var p prometheusRestoreCheckpointPayload
	decoder := json.NewDecoder(bytes.NewReader(row.Checkpoint))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&p); err != nil {
		return nil, restoreJournalReject("invalid checkpoint evidence")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, restoreJournalReject("trailing checkpoint evidence")
	}
	if p.Schema != 1 || p.TransactionID != row.ID || p.AdmissionSHA256 != row.PayloadSHA256 || p.StartedAt.IsZero() || !validUnitSnapshotPath(p.Root) || !validUnitSnapshotPath(filepath.Dir(p.Root)) || p.Root != restoreCheckpointRoot(filepath.Dir(p.Root), row) {
		return nil, restoreJournalReject("checkpoint reservation does not bind its transaction")
	}
	if row.Phase == "checkpointing" {
		if p.Evidence != nil || !p.StoppedAt.IsZero() {
			return nil, restoreJournalReject("reservation contains premature stopped evidence")
		}
		return &p, nil
	}
	e := p.Evidence
	if e == nil || e.Schema != 1 || e.BeforeSHA256 != admission.BeforeSHA256 || e.AfterSHA256 != admission.AfterSHA256 || e.Binding != admission.Current {
		return nil, restoreJournalReject("checkpoint does not bind current admission")
	}
	pid, pe := strconv.ParseUint(e.Runtime.PID, 10, 32)
	start, se := strconv.ParseUint(e.Runtime.StartTime, 10, 64)
	if pe != nil || se != nil || pid == 0 || start == 0 || !unitSnapshotHash.MatchString(e.Runtime.ArgvSHA256) || !recoveryRootIdentity.MatchString(e.Runtime.ConfigIdentity) || !recoveryRootIdentity.MatchString(e.Runtime.DataIdentity) {
		return nil, restoreJournalReject("invalid checkpoint process/root identity")
	}
	b := e.Binary
	if !validUnitSnapshotPath(b.Directory) || filepath.Dir(b.Directory) != p.Root || !recoveryPointDirectory.MatchString(filepath.Base(b.Directory)) || !strings.HasPrefix(filepath.Base(b.Directory), fmt.Sprintf("point-%d-", row.ComponentID)) ||
		!validUnitSnapshotPath(b.File.Path) || b.File.Role != "binary" || !b.File.Present || b.File.Optional || b.File.Payload != "" || b.File.UID != 0 || b.File.Mode > 0777 || b.File.Mode&0022 != 0 || b.File.Mode&0100 == 0 || b.File.Size <= 0 || b.File.Size > recoveryPointMaxBinary || !unitSnapshotHash.MatchString(b.File.SHA256) || b.File.Stamp == "" {
		return nil, restoreJournalReject("invalid checkpoint executable evidence")
	}
	if e.Startup.ComponentID != row.ComponentID || e.Startup.HostID != row.HostID || !validUnitSnapshotPath(e.Startup.Path) || filepath.Dir(e.Startup.Path) != p.Root ||
		!unitSnapshotDirectory.MatchString(filepath.Base(e.Startup.Path)) || !strings.HasPrefix(filepath.Base(e.Startup.Path), fmt.Sprintf("systemd-%d-", row.ComponentID)) || !unitSnapshotHash.MatchString(e.Startup.SHA256) ||
		recoveryPathsOverlap(p.Root, b.File.Path) {
		return nil, restoreJournalReject("invalid checkpoint startup reference")
	}
	a := e.Archive
	switch row.Phase {
	case "checkpoint_stopping":
		if !p.StoppedAt.IsZero() || a.Path != "" || a.SHA256 != "" || a.Version != "" || a.Size != 0 || len(a.Sources) != 0 {
			return nil, restoreJournalReject("pre-stop evidence contains an unconfirmed archive")
		}
	case "checkpoint_ready", "restore_stage_intent", "restore_stage_copying", "restore_stage_reclaim", "restore_staged":
		if p.StoppedAt.IsZero() || p.StoppedAt.Before(p.StartedAt) || a.Path != filepath.Join(b.Directory, "payload.tar.gz") || !unitSnapshotHash.MatchString(a.SHA256) || a.Size <= 0 || a.Version != admission.CurrentVersion || len(a.Sources) != 2 {
			return nil, restoreJournalReject("incomplete stopped checkpoint archive")
		}
		paths := []string{p.Root, b.File.Path}
		for _, path := range a.Sources {
			if !validUnitSnapshotPath(path) {
				return nil, restoreJournalReject("invalid checkpoint archive source")
			}
			for _, previous := range paths {
				if recoveryPathsOverlap(path, previous) {
					return nil, restoreJournalReject("overlapping checkpoint source or artifact")
				}
			}
			paths = append(paths, path)
		}
	default:
		return nil, restoreJournalReject("unsupported checkpoint phase")
	}
	return &p, nil
}
