package services

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/promlifecycle/platform/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func TestPrometheusRestoreTransactionPrepareFailuresLeaveNoIntent(t *testing.T) {
	f, _, r, api := transactionFixture(t)
	for _, mode := range []string{"insert-error", "zero-insert", "cancel-during-insert", "commit-failure", "sibling-claim"} {
		t.Run(mode, func(t *testing.T) {
			before := pairedPreviewFiles(t, f.root)
			pid := f.runner.pid
			r.calls = nil
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "commit-failure" {
				journalDeferredCommitFault(t, f.db)
			}
			if mode == "sibling-claim" {
				sibling := models.Component{HostID: f.c.HostID, ClusterID: f.c.ClusterID, Name: "journal-sibling", Type: models.ComponentThanos, DeployType: "binary", Version: "0.36.1", Status: "running"}
				if err := f.db.Create(&sibling).Error; err != nil {
					t.Fatal(err)
				}
				_, release, err := ClaimComponentOperation(context.Background(), f.db, sibling.ID, "restart")
				if err != nil {
					t.Fatal(err)
				}
				defer release()
			} else {
				name := "journal-prepare-fault"
				if err := f.db.Callback().Create().Before("gorm:create").Register(name, func(tx *gorm.DB) {
					if tx.Statement.Table != "prometheus_restore_transactions" {
						return
					}
					if mode == "cancel-during-insert" {
						cancel()
						return
					}
					if mode == "commit-failure" {
						tx.AddError(tx.Exec("INSERT INTO journal_fault_child(parent_id) VALUES (999)").Error)
						return
					}
					if mode == "insert-error" {
						tx.AddError(errors.New("private-insert-failure-sentinel"))
						return
					}
					tx.Statement.SQL.WriteString("SELECT 1 WHERE 1=0")
				}); err != nil {
					t.Fatal(err)
				}
				defer f.db.Callback().Create().Remove(name)
			}
			row, err := api.PreparePrometheusPairedRestoreTransaction(ctx, f.plan.ID)
			if row != nil || !errors.Is(err, ErrPrometheusRecoveryAdmission) || strings.Contains(err.Error(), "private-insert") {
				t.Fatal("failed prepare admitted or leaked", err)
			}
			if mode == "cancel-during-insert" && !errors.Is(err, context.Canceled) {
				t.Fatal("insert lost cancellation", err)
			}
			var count int64
			if err := f.db.Model(&models.PrometheusRestoreTransaction{}).Count(&count).Error; err != nil || count != 0 {
				t.Fatal("failed prepare persisted intent", err)
			}
			// The sibling claim belongs to this test and is deliberately still held.
			if mode != "sibling-claim" {
				assertPairedPreviewNoMutation(t, f, r, before, pid)
			} else {
				if f.runner.pid != pid || !f.runner.running {
					t.Fatal("failed prepare changed live process")
				}
				if got := pairedPreviewFiles(t, f.root); !equalJournalFiles(before, got) {
					t.Fatal("failed prepare changed live files")
				}
			}
		})
	}
	journalAssertClaimsEmpty(t, f.db)
}

func equalJournalFiles(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}

func TestPrometheusRestoreTransactionAbortFailuresKeepFence(t *testing.T) {
	f, _, r, api := transactionFixture(t)
	row, err := api.PreparePrometheusPairedRestoreTransaction(context.Background(), f.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	before := pairedPreviewFiles(t, f.root)
	pid := f.runner.pid
	r.calls = nil
	for _, mode := range []string{"update-error", "zero-update", "cancel-during-update", "commit-failure", "cancelled", "plan-drift", "host-drift", "point-drift", "unknown-phase", "pause", "version-barrier"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			switch mode {
			case "update-error", "zero-update", "cancel-during-update", "commit-failure":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
				if mode == "commit-failure" {
					journalDeferredCommitFault(t, f.db)
				}
				name := "journal-abort-fault"
				if err := f.db.Callback().Update().Before("gorm:update").Register(name, func(tx *gorm.DB) {
					if tx.Statement.Table != "prometheus_restore_transactions" {
						return
					}
					if mode == "cancel-during-update" {
						cancel()
						return
					}
					if mode == "commit-failure" {
						tx.AddError(tx.Exec("INSERT INTO journal_fault_child(parent_id) VALUES (999)").Error)
						return
					}
					if mode == "update-error" {
						tx.AddError(errors.New("private-update-failure-sentinel"))
						return
					}
					tx.Statement.AddClause(clause.Where{Exprs: []clause.Expression{clause.Expr{SQL: "1=0"}}})
				}); err != nil {
					t.Fatal(err)
				}
				defer f.db.Callback().Update().Remove(name)
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "plan-drift":
				if err := f.db.Model(&models.ChangePlan{}).Where("id = ?", f.plan.ID).Update("status", models.ChangeStatusVerified).Error; err != nil {
					t.Fatal(err)
				}
				defer f.db.Model(&models.ChangePlan{}).Where("id = ?", f.plan.ID).Update("status", models.ChangeStatusFailed)
			case "host-drift":
				var original models.Host
				if err := f.db.First(&original, f.c.HostID).Error; err != nil {
					t.Fatal(err)
				}
				if err := f.db.Model(&models.Host{}).Where("id = ?", original.ID).Update("ip", "192.0.2.99").Error; err != nil {
					t.Fatal(err)
				}
				defer f.db.Model(&models.Host{}).Where("id = ?", original.ID).Update("ip", original.IP)
			case "point-drift":
				var original models.PrometheusRecoveryPoint
				if err := f.db.First(&original, row.PointID).Error; err != nil {
					t.Fatal(err)
				}
				if err := f.db.Model(&models.PrometheusRecoveryPoint{}).Where("id = ?", row.PointID).Update("payload", strings.Replace(string(original.Payload), `"Schema":1`, `"Schema":2`, 1)).Error; err != nil {
					t.Fatal(err)
				}
				defer f.db.Model(&models.PrometheusRecoveryPoint{}).Where("id = ?", row.PointID).Update("payload", string(original.Payload))
			case "unknown-phase":
				if err := f.db.Model(&models.PrometheusRestoreTransaction{}).Where("id = ?", row.ID).Update("phase", "applying").Error; err != nil {
					t.Fatal(err)
				}
				defer f.db.Model(&models.PrometheusRestoreTransaction{}).Where("id = ?", row.ID).Update("phase", "prepared")
			case "pause", "version-barrier":
				pause := ComponentBackupPause{ComponentID: f.c.ID, Status: "paused"}
				if mode == "version-barrier" {
					pause.Status = "resumed"
					pause.VersionTransitionPlanID = f.plan.ID
				}
				if err := f.db.Save(&pause).Error; err != nil {
					t.Fatal(err)
				}
				defer f.db.Where("component_id = ?", f.c.ID).Delete(&ComponentBackupPause{})
			}
			result, err := api.AbortPreparedPrometheusPairedRestoreTransaction(ctx, row.ID)
			if result != nil || !errors.Is(err, ErrPrometheusRecoveryAdmission) || strings.Contains(err.Error(), "private-update") {
				t.Fatal("unsafe abort admitted or leaked", err)
			}
			if (mode == "cancelled" || mode == "cancel-during-update") && !errors.Is(err, context.Canceled) {
				t.Fatal("abort lost cancellation", err)
			}
			var retained models.PrometheusRestoreTransaction
			if err := f.db.First(&retained, row.ID).Error; err != nil {
				t.Fatal(err)
			}
			if retained.Revision != 1 || retained.ActiveHostID == nil || retained.ActiveComponentID == nil || retained.FinishedAt != nil || retained.Token != row.Token || string(retained.Payload) != string(row.Payload) {
				t.Fatal("failed abort changed durable ownership")
			}
			if len(r.calls) != 0 {
				t.Fatal("abort failure contacted host")
			}
			if err := RecoverInterruptedComponentRestores(context.Background(), f.db); !errors.Is(err, ErrPrometheusRecoveryAdmission) {
				t.Fatal("failed abort dropped startup fence", err)
			}
			assertPairedPreviewNoMutation(t, f, r, before, pid)
		})
	}
	if _, err := api.AbortPreparedPrometheusPairedRestoreTransaction(context.Background(), row.ID); err != nil {
		t.Fatal("valid abort after faults", err)
	}
}

func TestPrometheusRestoreJournalPrivateAccessCannotEscapeOwner(t *testing.T) {
	db, cs := journalGuardFixture(t)
	owner, neighbor, other := cs[0], cs[1], cs[2]
	row := journalGuardRecord(t, db, owner)
	for _, mode := range []string{"wrong-id", "wrong-token", "wrong-host", "wrong-component", "neighbor", "different-host"} {
		t.Run(mode, func(t *testing.T) {
			access := restoreJournalAccess{ID: row.ID, Token: row.Token, HostID: row.HostID, ComponentID: row.ComponentID}
			hostID, componentID := row.HostID, row.ComponentID
			switch mode {
			case "wrong-id":
				access.ID++
			case "wrong-token":
				access.Token = "wrong"
			case "wrong-host":
				access.HostID++
			case "wrong-component":
				access.ComponentID++
			case "neighbor":
				componentID = neighbor.ID
			case "different-host":
				hostID = other.HostID
				componentID = 0
			}
			ctx := context.WithValue(context.Background(), restoreJournalAccessKey{}, access)
			if restoreJournalAccessAllows(ctx, row, hostID, componentID) {
				t.Fatal("private access escaped bound owner")
			}
		})
	}
}

func journalDeferredCommitFault(t *testing.T, db *gorm.DB) {
	t.Helper()
	for _, sql := range []string{"CREATE TABLE IF NOT EXISTS journal_fault_parent (id INTEGER PRIMARY KEY)",
		"CREATE TABLE IF NOT EXISTS journal_fault_child (parent_id INTEGER REFERENCES journal_fault_parent(id) DEFERRABLE INITIALLY DEFERRED)"} {
		if err := db.Exec(sql).Error; err != nil {
			t.Fatal(err)
		}
	}
}

func TestPrometheusRestoreTransactionAbortRevalidatesReentrantPause(t *testing.T) {
	f, _, r, api := transactionFixture(t)
	row, err := api.PreparePrometheusPairedRestoreTransaction(context.Background(), f.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
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
	r.calls = nil
	for _, mode := range []string{"pause", "version"} {
		t.Run(mode, func(t *testing.T) {
			pause := ComponentBackupPause{ComponentID: f.c.ID, Status: "paused"}
			if mode == "version" {
				pause.Status = "resumed"
				pause.VersionTransitionPlanID = f.plan.ID
			}
			if err := f.db.Save(&pause).Error; err != nil {
				t.Fatal(err)
			}
			defer f.db.Where("component_id = ?", f.c.ID).Delete(&ComponentBackupPause{})
			if result, err := api.AbortPreparedPrometheusPairedRestoreTransaction(ctx, row.ID); result != nil || !errors.Is(err, ErrPrometheusRecoveryAdmission) {
				t.Fatal("reentrant abort bypassed old pause/barrier", err)
			}
			var got models.PrometheusRestoreTransaction
			if err := f.db.First(&got, row.ID).Error; err != nil || got.Phase != "prepared" || got.Revision != 1 {
				t.Fatal("reentrant rejection changed journal", err)
			}
			if len(r.calls) != 0 {
				t.Fatal("reentrant rejection contacted host")
			}
		})
	}
	if _, err := api.AbortPreparedPrometheusPairedRestoreTransaction(ctx, row.ID); err != nil {
		t.Fatal(err)
	}
}

func TestPrometheusRestoreTransactionAbortReloadRejectsMissingOrChangedIdentity(t *testing.T) {
	f, _, r, api := transactionFixture(t)
	row, err := api.PreparePrometheusPairedRestoreTransaction(context.Background(), f.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"token-changed", "row-disappeared"} {
		t.Run(mode, func(t *testing.T) {
			fired := false
			r.calls = nil
			name := "journal-abort-reload-fault"
			if err := f.db.Callback().Query().After("gorm:after_query").Register(name, func(tx *gorm.DB) {
				if fired || tx.Statement.Table != "prometheus_restore_transactions" {
					return
				}
				fired = true
				var err error
				if mode == "token-changed" {
					err = f.db.Model(&models.PrometheusRestoreTransaction{}).Where("id = ?", row.ID).Update("token", strings.Repeat("b", 64)).Error
				} else {
					err = f.db.Delete(&models.PrometheusRestoreTransaction{}, row.ID).Error
				}
				if err != nil {
					tx.AddError(err)
				}
			}); err != nil {
				t.Fatal(err)
			}
			result, err := api.AbortPreparedPrometheusPairedRestoreTransaction(context.Background(), row.ID)
			f.db.Callback().Query().Remove(name)
			if !fired || result != nil || !errors.Is(err, ErrPrometheusRecoveryAdmission) {
				t.Fatal("abort accepted changed durable identity", err)
			}
			if len(r.calls) != 0 {
				t.Fatal("invalid abort contacted host")
			}
			journalAssertClaimsEmpty(t, f.db)
			if mode == "token-changed" {
				var retained models.PrometheusRestoreTransaction
				if err := f.db.First(&retained, row.ID).Error; err != nil || retained.Phase != "prepared" || retained.Revision != 1 {
					t.Fatal("abort closed changed identity", err)
				}
				if err := f.db.Model(&models.PrometheusRestoreTransaction{}).Where("id = ?", row.ID).Update("token", row.Token).Error; err != nil {
					t.Fatal(err)
				}
			} else {
				// Only the explicit test fault deleted this row. Restore the fixture so the
				// next assertion can exercise valid resolution; production never recreates it.
				if err := f.db.Create(row).Error; err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	if _, err := api.AbortPreparedPrometheusPairedRestoreTransaction(context.Background(), row.ID); err != nil {
		t.Fatal(err)
	}
}
