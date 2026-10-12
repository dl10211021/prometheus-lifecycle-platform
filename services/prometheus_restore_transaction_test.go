package services

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/promlifecycle/platform/models"
	platformsqlite "github.com/promlifecycle/platform/storage/sqlite"
	"gorm.io/gorm"
)

type pairedRestoreTransactionAPI interface {
	PreparePrometheusPairedRestoreTransaction(context.Context, uint) (*models.PrometheusRestoreTransaction, error)
	AbortPreparedPrometheusPairedRestoreTransaction(context.Context, uint) (*models.PrometheusRestoreTransaction, error)
}

func transactionAPI(t *testing.T, s *BackupService) pairedRestoreTransactionAPI {
	t.Helper()
	api, ok := any(s).(pairedRestoreTransactionAPI)
	if !ok {
		t.Fatal("paired restore transaction preparation and safe abort are not implemented")
	}
	return api
}

func transactionFixture(t *testing.T) (*pointFixture, *models.PrometheusRecoveryPoint, *pairedPreviewRunner, pairedRestoreTransactionAPI) {
	t.Helper()
	transactionAPI(t, NewBackupService(nil))
	f, point, r := pairedPreviewFixture(t)
	if err := f.db.AutoMigrate(&models.PrometheusRestoreTransaction{}); err != nil {
		t.Fatal(err)
	}
	return f, point, r, transactionAPI(t, f.s)
}

func transactionBusinessState(t *testing.T, db *gorm.DB) string {
	t.Helper()
	var values []any
	for _, table := range []string{"components", "hosts", "change_plans", "prometheus_recovery_points", "backups", "backup_manifests", "component_backup_pauses"} {
		var rows []map[string]any
		if err := db.Raw("SELECT * FROM " + table).Scan(&rows).Error; err != nil {
			t.Fatal(err)
		}
		values = append(values, rows)
	}
	raw, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestPrometheusRestoreTransactionPrepareAbortPreservesState(t *testing.T) {
	f, point, r, api := transactionFixture(t)
	if err := os.WriteFile(f.runner.environment, []byte("TOKEN=current-private-journal\n"), 0600); err != nil {
		t.Fatal(err)
	}
	before := pairedPreviewFiles(t, f.root)
	business := transactionBusinessState(t, f.db)
	pid := f.runner.pid
	r.calls = nil
	row, err := api.PreparePrometheusPairedRestoreTransaction(context.Background(), f.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row == nil || row.Phase != "prepared" || row.Revision != 1 || row.PointID != point.ID || row.PlanID != f.plan.ID || row.BackupID != point.BackupID ||
		row.ComponentID != f.c.ID || row.HostID != f.c.HostID || row.ActiveComponentID == nil || row.ActiveHostID == nil || row.FinishedAt != nil {
		t.Fatalf("incomplete prepared transaction: %+v", row)
	}
	for _, secret := range []string{"current-private-journal", "private-env-sentinel", f.c.ImportedArgs} {
		if strings.Contains(string(row.Payload), secret) {
			t.Fatal("journal persisted private source content")
		}
	}
	assertPairedPreviewNoMutation(t, f, r, before, pid)
	if business != transactionBusinessState(t, f.db) {
		t.Fatal("prepare changed business/history metadata")
	}
	calls := len(r.calls)
	if next, err := api.PreparePrometheusPairedRestoreTransaction(context.Background(), f.plan.ID); next != nil || err == nil {
		t.Fatal("duplicate prepare admitted")
	}
	if len(r.calls) != calls {
		t.Fatal("duplicate prepare issued host commands")
	}
	if err := RecoverInterruptedComponentRestores(context.Background(), f.db); !errors.Is(err, ErrPrometheusRecoveryAdmission) {
		t.Fatal("prepared journal did not fence startup", err)
	}
	r.calls = nil
	closed, err := api.AbortPreparedPrometheusPairedRestoreTransaction(context.Background(), row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if closed == nil || closed.ID != row.ID || closed.Phase != "aborted" || closed.Revision != 2 || closed.ActiveComponentID != nil || closed.ActiveHostID != nil ||
		closed.FinishedAt == nil || string(closed.Payload) != string(row.Payload) || closed.Token != row.Token {
		t.Fatal("abort did not atomically close only prepared ownership")
	}
	if len(r.calls) != 0 {
		t.Fatal("prepared abort contacted managed host")
	}
	repeated, err := api.AbortPreparedPrometheusPairedRestoreTransaction(context.Background(), row.ID)
	if err != nil || repeated.Revision != 2 || !repeated.UpdatedAt.Equal(closed.UpdatedAt) {
		t.Fatal("abort is not idempotent", err)
	}
	if business != transactionBusinessState(t, f.db) {
		t.Fatal("abort changed business/history metadata")
	}
	assertPairedPreviewNoMutation(t, f, r, before, pid)
	next, err := api.PreparePrometheusPairedRestoreTransaction(context.Background(), f.plan.ID)
	if err != nil || next.ID == row.ID {
		t.Fatal("aborted history prevented a fresh transaction", err)
	}
	if _, err := api.AbortPreparedPrometheusPairedRestoreTransaction(context.Background(), next.ID); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := f.db.Model(&models.PrometheusRestoreTransaction{}).Count(&count).Error; err != nil || count != 2 {
		t.Fatal("terminal transaction history lost", err)
	}
}

func TestPrometheusRestoreTransactionInputAndCancellation(t *testing.T) {
	var missing *BackupService
	for _, s := range []*BackupService{missing, NewBackupService(nil)} {
		api := transactionAPI(t, s)
		if row, err := api.PreparePrometheusPairedRestoreTransaction(context.Background(), 0); row != nil || !errors.Is(err, ErrPrometheusRecoveryAdmission) {
			t.Fatal("invalid preparation admitted", err)
		}
		if row, err := api.AbortPreparedPrometheusPairedRestoreTransaction(context.Background(), 0); row != nil || !errors.Is(err, ErrPrometheusRecoveryAdmission) {
			t.Fatal("invalid abort admitted", err)
		}
	}
	f, _, r, api := transactionFixture(t)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	r.calls = nil
	if row, err := api.PreparePrometheusPairedRestoreTransaction(cancelled, f.plan.ID); row != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("prepare lost cancellation", err)
	}
	if len(r.calls) != 0 {
		t.Fatal("cancelled prepare contacted host")
	}
	journalAssertClaimsEmpty(t, f.db)
}

func TestPrometheusRestoreTransactionNewProcess(t *testing.T) {
	f, _, r, api := transactionFixture(t)
	before := pairedPreviewFiles(t, f.root)
	pid := f.runner.pid
	r.calls = nil
	row, err := api.PreparePrometheusPairedRestoreTransaction(context.Background(), f.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"check", "abort"} {
		command := exec.Command(executable, "-test.run=^TestPrometheusRestoreTransactionProcessHelper$", "-test.v")
		command.Env = append(os.Environ(), "PROMLIFECYCLE_JOURNAL_TEST_DB="+f.dbPath, "PROMLIFECYCLE_JOURNAL_TEST_ROOT="+f.s.backupRoot,
			"PROMLIFECYCLE_JOURNAL_TEST_ID="+strconv.FormatUint(uint64(row.ID), 10), "PROMLIFECYCLE_JOURNAL_TEST_MODE="+mode)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("new process %s failed: %v\n%s", mode, err, output)
		}
	}
	var persisted models.PrometheusRestoreTransaction
	if err := f.db.First(&persisted, row.ID).Error; err != nil || persisted.Phase != "aborted" || persisted.Revision != 2 {
		t.Fatal("new-process abort did not persist", err)
	}
	assertPairedPreviewNoMutation(t, f, r, before, pid)
}

func TestPrometheusRestoreTransactionProcessHelper(t *testing.T) {
	path := os.Getenv("PROMLIFECYCLE_JOURNAL_TEST_DB")
	if path == "" {
		return
	}
	db, err := platformsqlite.OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	connection, _ := db.DB()
	defer connection.Close()
	id, err := strconv.ParseUint(os.Getenv("PROMLIFECYCLE_JOURNAL_TEST_ID"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("PROMLIFECYCLE_JOURNAL_TEST_MODE") == "check" {
		if err := RecoverInterruptedComponentRestores(context.Background(), db); !errors.Is(err, ErrPrometheusRecoveryAdmission) {
			t.Fatal("new process bypassed durable journal", err)
		}
		return
	}
	service := NewBackupServiceWithRoot(db, os.Getenv("PROMLIFECYCLE_JOURNAL_TEST_ROOT"))
	service.runnerFactory = func(*models.Component) (hostCommandRunner, error) {
		t.Fatal("prepared abort must not contact any host")
		return nil, errors.New("unexpected host access")
	}
	row, err := transactionAPI(t, service).AbortPreparedPrometheusPairedRestoreTransaction(context.Background(), uint(id))
	if err != nil || row == nil || row.Phase != "aborted" {
		t.Fatal("prepared abort after process restart", err)
	}
}
