package services

import (
	"context"
	"errors"
	"github.com/promlifecycle/platform/models"
	platformsqlite "github.com/promlifecycle/platform/storage/sqlite"
	"gorm.io/gorm"
	"strings"
	"testing"
)

func TestPrometheusRestoreStageRejectsOuterSQLiteTransaction(t *testing.T) {
	f, r, prepared, cp := checkpointFixture(t)
	ready, err := cp.CreatePrometheusPairedRestoreCheckpoint(context.Background(), prepared.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, lock, err := platformsqlite.AcquireDatabaseProcessLock(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	check := func(t *testing.T, row *models.PrometheusRestoreTransaction, recover bool) {
		t.Helper()
		tx := f.db.Begin()
		if tx.Error != nil {
			t.Fatal(tx.Error)
		}
		defer tx.Rollback()
		scoped := *f.s
		scoped.db = tx
		writes := 0
		callback := func(db *gorm.DB) {
			if db.Statement.Table == "host_operation_claims" || db.Statement.Table == "component_operation_claims" || db.Statement.Table == "prometheus_restore_transactions" {
				writes++
			}
		}
		f.db.Callback().Create().Before("gorm:create").Register("outer-transaction-observer", callback)
		f.db.Callback().Update().Before("gorm:update").Register("outer-transaction-observer", callback)
		defer f.db.Callback().Create().Remove("outer-transaction-observer")
		defer f.db.Callback().Update().Remove("outer-transaction-observer")
		r.calls = nil
		r.before = func(context.Context, string) error { return errors.New("host must not be contacted") }
		if recover {
			_, err = scoped.RecoverPrometheusPairedRestoreStageForStartup(context.Background(), row.ID, lock)
		} else {
			_, err = scoped.StagePrometheusPairedRestore(context.Background(), row.ID)
		}
		if err == nil {
			t.Error("outer transaction admitted")
		}
		after := checkpointJournal(t, tx, row.ID)
		if !sameRestoreStageRow(row, &after) || writes != 0 || len(r.calls) != 0 {
			t.Errorf("transaction-bound call wrote phase/claims or contacted host: writes=%d host=%d phase=%s", writes, len(r.calls), after.Phase)
		}
		journalAssertClaimsEmpty(t, tx)
	}
	t.Run("stage", func(t *testing.T) { check(t, ready, false) })
	r.calls = nil
	r.before = func(_ context.Context, cmd string) error {
		if strings.Contains(cmd, "# promlifecycle:restore-stage-copy") {
			return errors.New("prepare interrupted stage")
		}
		return nil
	}
	if _, err := f.s.StagePrometheusPairedRestore(context.Background(), ready.ID); err == nil {
		t.Fatal("copy fault absent")
	}
	interrupted := checkpointJournal(t, f.db, ready.ID)
	if interrupted.Phase != "restore_stage_copying" {
		t.Fatal("ordinary silent session failed", interrupted.Phase)
	}
	t.Run("recovery", func(t *testing.T) { check(t, &interrupted, true) })
}
