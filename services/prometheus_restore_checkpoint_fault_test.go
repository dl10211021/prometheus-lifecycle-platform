package services

import (
	"context"
	"errors"
	coressh "github.com/promlifecycle/platform/core/ssh"
	"github.com/promlifecycle/platform/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"reflect"
	"strings"
	"testing"
)

func TestPrometheusRestoreCheckpointRejectIsNotCancellation(t *testing.T) {
	db, cs := journalGuardFixture(t)
	row := journalGuardRecord(t, db, cs[0])
	s := NewBackupService(db)
	got, err := s.VerifyPrometheusPairedRestoreCheckpoint(context.Background(), row.ID)
	if got != nil || !errors.Is(err, ErrPrometheusRecoveryAdmission) {
		t.Fatal("prepared verification admitted", err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("ordinary rejection misreported as cancellation", err)
	}
}

func checkpointAssertRetained(t *testing.T, f *pointFixture, r *pairedPreviewRunner, original *models.PrometheusRestoreTransaction, phase string, revision uint64, running bool) {
	t.Helper()
	row := checkpointJournal(t, f.db, original.ID)
	if row.Phase != phase || row.Revision != revision || row.Token != original.Token || row.PayloadSHA256 != original.PayloadSHA256 || string(row.Payload) != string(original.Payload) || row.ActiveHostID == nil || row.ActiveComponentID == nil || row.FinishedAt != nil || f.runner.running != running {
		t.Fatal("failure lost last committed checkpoint ownership/runtime", row.Phase, row.Revision, f.runner.running)
	}
	if _, err := decodeRestoreJournal(&row); err != nil {
		t.Fatal("retained phase is structurally unreadable", err)
	}
	journalAssertClaimsEmpty(t, f.db)
	if err := RecoverInterruptedComponentRestores(context.Background(), f.db); !errors.Is(err, ErrPrometheusRecoveryAdmission) {
		t.Fatal("failure lost startup fence", err)
	}
	if phase == "prepared" {
		return
	}
	files := pairedPreviewFiles(t, f.root)
	r.calls = nil
	r.before = nil
	for _, op := range []func(context.Context, uint) (*models.PrometheusRestoreTransaction, error){f.s.CreatePrometheusPairedRestoreCheckpoint, f.s.VerifyPrometheusPairedRestoreCheckpoint, f.s.AbortPreparedPrometheusPairedRestoreTransaction} {
		if got, err := op(context.Background(), row.ID); got != nil || !errors.Is(err, ErrPrometheusRecoveryAdmission) {
			t.Fatal("ambiguous phase was retried or cleared", err)
		}
	}
	if len(r.calls) != 0 || !reflect.DeepEqual(files, pairedPreviewFiles(t, f.root)) {
		t.Fatal("ambiguous phase retry touched host")
	}
}
func TestPrometheusRestoreCheckpointCommitFailures(t *testing.T) {
	for stage, phase := range []string{"checkpointing", "checkpoint_stopping", "checkpoint_ready"} {
		for _, mode := range []string{"error", "zero", "cancel", "commit"} {
			t.Run(phase+"/"+mode, func(t *testing.T) {
				f, r, original, api := checkpointFixture(t)
				before := pairedPreviewFiles(t, f.root)
				business := transactionBusinessState(t, f.db)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if mode == "commit" {
					journalDeferredCommitFault(t, f.db)
				}
				fired := false
				name := "checkpoint-commit-fault"
				if err := f.db.Callback().Update().Before("gorm:update").Register(name, func(tx *gorm.DB) {
					if tx.Statement.Table != "prometheus_restore_transactions" {
						return
					}
					values, ok := tx.Statement.Dest.(map[string]any)
					if !ok || values["phase"] != phase {
						return
					}
					fired = true
					switch mode {
					case "error":
						tx.AddError(errors.New("private-checkpoint-sql-sentinel"))
					case "zero":
						tx.Statement.AddClause(clause.Where{Exprs: []clause.Expression{clause.Expr{SQL: "1=0"}}})
					case "cancel":
						cancel()
					case "commit":
						tx.AddError(tx.Exec("INSERT INTO journal_fault_child(parent_id) VALUES (999)").Error)
					}
				}); err != nil {
					t.Fatal(err)
				}
				got, err := api.CreatePrometheusPairedRestoreCheckpoint(ctx, original.ID)
				f.db.Callback().Update().Remove(name)
				if !fired || got != nil || !errors.Is(err, ErrPrometheusRecoveryAdmission) || strings.Contains(err.Error(), "private-checkpoint") {
					t.Fatal("phase COMMIT fault admitted or leaked", err)
				}
				if errors.Is(err, context.Canceled) != (mode == "cancel") {
					t.Fatal("incorrect cancellation identity", err)
				}
				if transactionBusinessState(t, f.db) != business {
					t.Fatal("phase failure altered business/history")
				}
				after := pairedPreviewFiles(t, f.root)
				for path, value := range before {
					if after[path] != value {
						t.Fatal("phase failure changed original/history", path)
					}
				}
				if stage == 0 {
					checkpointAssertReadOnly(t, r.calls)
				}
				if stage < 2 {
					for _, call := range r.calls {
						if strings.HasPrefix(call, "systemctl stop ") {
							t.Fatal("stop preceded pre-stop COMMIT")
						}
					}
				}
				for _, call := range r.calls {
					if strings.Contains(call, "systemctl restart ") || strings.Contains(call, "systemctl start ") {
						t.Fatal("failure restarted application")
					}
				}
				previous := []string{"prepared", "checkpointing", "checkpoint_stopping"}[stage]
				checkpointAssertRetained(t, f, r, original, previous, uint64(stage+1), stage < 2)
			})
		}
	}
}
func TestPrometheusRestoreCheckpointHostFailures(t *testing.T) {
	for _, mode := range []string{"root", "copy", "pre-stop", "stop-unconfirmed", "archive", "sync", "process-reuse"} {
		t.Run(mode, func(t *testing.T) {
			f, r, row, api := checkpointFixture(t)
			fired, reserved := false, false
			r.before = func(ctx context.Context, command string) error {
				root := strings.Contains(command, "# promlifecycle:restore-checkpoint-root")
				if root {
					reserved = true
				}
				match := mode == "root" && root ||
					mode == "copy" && reserved && strings.Contains(command, "set -C") ||
					mode == "pre-stop" && strings.HasPrefix(command, "systemctl stop ") ||
					mode == "stop-unconfirmed" && strings.HasPrefix(command, "systemctl stop ") ||
					mode == "archive" && strings.Contains(command, "cp -aL ") ||
					mode == "sync" && strings.HasPrefix(command, "sync -f ") && strings.Contains(command, "payload.tar.gz") ||
					mode == "process-reuse" && root
				if !match || fired {
					return nil
				}
				fired = true
				if mode == "process-reuse" {
					f.runner.generation++
					return nil
				}
				if mode == "stop-unconfirmed" {
					f.runner.running = false
					return coressh.ErrRemoteTerminationUnconfirmed
				}
				return errors.New("private-host-checkpoint-sentinel")
			}
			got, err := api.CreatePrometheusPairedRestoreCheckpoint(context.Background(), row.ID)
			if !fired || got != nil || !errors.Is(err, ErrPrometheusRecoveryAdmission) || strings.Contains(err.Error(), "private-host") {
				t.Fatal("host fault admitted or leaked", err)
			}
			if errors.Is(err, coressh.ErrRemoteTerminationUnconfirmed) != (mode == "stop-unconfirmed") {
				t.Fatal("lost termination identity", err)
			}
			phase, revision, running := "checkpointing", uint64(2), true
			if mode == "pre-stop" || mode == "stop-unconfirmed" || mode == "archive" || mode == "sync" {
				phase, revision = "checkpoint_stopping", 3
			}
			if mode == "stop-unconfirmed" || mode == "archive" || mode == "sync" {
				running = false
			}
			checkpointAssertRetained(t, f, r, row, phase, revision, running)
		})
	}
}
func TestPrometheusRestoreCheckpointReentrantBarriers(t *testing.T) {
	f, r, row, api := checkpointFixture(t)
	ctx := context.WithValue(context.Background(), restoreJournalAccessKey{}, restoreJournalAccess{ID: row.ID, Token: row.Token, HostID: row.HostID, ComponentID: row.ComponentID})
	ctx, releaseH, err := ClaimHostOperation(ctx, f.db, row.HostID, "prepared-abort")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseH()
	ctx, releaseC, err := ClaimComponentOperation(ctx, f.db, row.ComponentID, "prepared-abort")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseC()
	for _, mode := range []string{"pause", "version"} {
		t.Run(mode, func(t *testing.T) {
			pause := ComponentBackupPause{ComponentID: row.ComponentID, Status: "paused"}
			if mode == "version" {
				pause.Status = "resumed"
				pause.VersionTransitionPlanID = row.PlanID
			}
			if err := f.db.Save(&pause).Error; err != nil {
				t.Fatal(err)
			}
			r.calls = nil
			if got, err := api.CreatePrometheusPairedRestoreCheckpoint(ctx, row.ID); got != nil || !errors.Is(err, ErrPrometheusRecoveryAdmission) {
				t.Fatal("reentry bypassed prior barrier", err)
			}
			if len(r.calls) != 0 || checkpointJournal(t, f.db, row.ID).Phase != "prepared" {
				t.Fatal("barrier rejection touched host/journal")
			}
			if err := f.db.Where("component_id = ?", row.ComponentID).Delete(&ComponentBackupPause{}).Error; err != nil {
				t.Fatal(err)
			}
		})
	}
}
