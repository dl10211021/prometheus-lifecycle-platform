package services

import (
	"context"
	"errors"
	"testing"

	"github.com/promlifecycle/platform/models"
)

// Called only by the existing explicitly opted-in owned real-host fixture.
func pointRealRestoreJournal(t *testing.T, ctx context.Context, s *BackupService, planID uint, unsupported bool, unchanged func()) {
	t.Helper()
	if err := s.db.AutoMigrate(&models.PrometheusRestoreTransaction{}); err != nil {
		t.Fatal(err)
	}
	business := transactionBusinessState(t, s.db)
	row, err := s.PreparePrometheusPairedRestoreTransaction(ctx, planID)
	if unsupported {
		if row != nil || !errors.Is(err, ErrPrometheusRecoveryAdmission) {
			t.Fatal("unsupported host prepared a restore transaction", err)
		}
		var count int64
		if err := s.db.Model(&models.PrometheusRestoreTransaction{}).Count(&count).Error; err != nil || count != 0 {
			t.Fatal("rejected host retained intent", err)
		}
		unchanged()
		t.Log("real paired restore journal: unsupported reference rejected before durable intent; PID/startup/fixed sample unchanged")
		return
	}
	if err != nil || row == nil || row.Phase != "prepared" || row.Revision != 1 {
		t.Fatal("real host preparation failed", err)
	}
	unchanged()
	for _, kind := range []string{"upgrade", "restore-recovery", "caller-invented"} {
		if _, release, err := ClaimComponentOperation(ctx, s.db, row.ComponentID, kind); !errors.Is(err, ErrPrometheusRecoveryAdmission) {
			if release != nil {
				release()
			}
			t.Fatal("real host journal allowed competing lifecycle", err)
		}
		if _, release, err := ClaimHostOperation(ctx, s.db, row.HostID, kind); !errors.Is(err, ErrPrometheusRecoveryAdmission) {
			if release != nil {
				release()
			}
			t.Fatal("real host journal allowed competing host operation", err)
		}
	}
	if err := RecoverInterruptedComponentRestores(ctx, s.db); !errors.Is(err, ErrPrometheusRecoveryAdmission) {
		t.Fatal("real host journal did not fence startup", err)
	}
	fresh := NewBackupServiceWithRoot(s.db, s.backupRoot)
	fresh.runnerFactory = func(*models.Component) (hostCommandRunner, error) {
		t.Fatal("prepared abort contacted host")
		return nil, errors.New("unexpected host call")
	}
	closed, err := fresh.AbortPreparedPrometheusPairedRestoreTransaction(ctx, row.ID)
	if err != nil || closed == nil || closed.Phase != "aborted" || closed.Revision != 2 || closed.ActiveComponentID != nil || closed.ActiveHostID != nil {
		t.Fatal("real host prepared abort failed", err)
	}
	if _, err := fresh.AbortPreparedPrometheusPairedRestoreTransaction(ctx, row.ID); err != nil {
		t.Fatal("real host abort idempotence", err)
	}
	unchanged()
	if business != transactionBusinessState(t, s.db) {
		t.Fatal("real journal changed business or historical metadata")
	}
	journalAssertClaimsEmpty(t, s.db)
	t.Log("real paired restore journal: prepared intent persisted; ordinary/recovery/host/startup fenced; fresh service performed DB-only idempotent abort; PID/startup/fixed TSDB sample/history unchanged; ownership released")
}
