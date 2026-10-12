package services

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/promlifecycle/platform/models"
	platformsqlite "github.com/promlifecycle/platform/storage/sqlite"
)

type journalCrashInfo struct {
	Database, BackupRoot, FilesRoot string
	ID                              uint
}

func TestPrometheusRestoreTransactionCrashAfterCommitBeforeRelease(t *testing.T) {
	owned := t.TempDir()
	infoPath := filepath.Join(owned, "crash-info.json")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(exe, "-test.run=^TestPrometheusRestoreTransactionCrashHelper$", "-test.v")
	command.Env = append(os.Environ(), "TMPDIR="+owned, "PROMLIFECYCLE_JOURNAL_CRASH_INFO="+infoPath)
	output, err := command.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 77 {
		t.Fatalf("crash helper did not reach durable boundary: %v\n%s", err, output)
	}
	raw, err := os.ReadFile(infoPath)
	if err != nil {
		t.Fatal(err)
	}
	var info journalCrashInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		t.Fatal(err)
	}
	db, err := platformsqlite.OpenDB(info.Database)
	if err != nil {
		t.Fatal(err)
	}
	connection, _ := db.DB()
	defer connection.Close()
	s := NewBackupServiceWithRoot(db, info.BackupRoot)
	s.runnerFactory = func(*models.Component) (hostCommandRunner, error) {
		t.Fatal("crash recovery abort contacted host")
		return nil, errors.New("unexpected host access")
	}
	var count int64
	for _, model := range []any{&models.HostOperationClaim{}, &models.ComponentOperationClaim{}} {
		if err := db.Model(model).Count(&count).Error; err != nil || count != 1 {
			t.Fatal("crash did not retain transient owner", err)
		}
	}
	if row, err := s.AbortPreparedPrometheusPairedRestoreTransaction(context.Background(), info.ID); row != nil || !errors.Is(err, ErrComponentBusy) {
		t.Fatal("ordinary abort stole possibly-live owner", err)
	}
	before := pairedPreviewFiles(t, info.FilesRoot)
	// The child is confirmed dead; this isolated fixture has a single owner.
	// Actual server process-lock wiring is independently tested in cmd/server.
	if err := RecoverPreparedPrometheusRestoreClaimsForStartup(context.Background(), info.Database); !errors.Is(err, ErrPrometheusRecoveryAdmission) {
		t.Fatal("startup failed to retain fence", err)
	}
	journalAssertClaimsEmpty(t, db)
	row, err := s.AbortPreparedPrometheusPairedRestoreTransaction(context.Background(), info.ID)
	if err != nil || row.Phase != "aborted" || row.Revision != 2 {
		t.Fatal("durable intent could not be safely resolved after crash", err)
	}
	if !equalJournalFiles(before, pairedPreviewFiles(t, info.FilesRoot)) {
		t.Fatal("crash recovery changed live or historical files")
	}
}

func TestPrometheusRestoreTransactionCrashHelper(t *testing.T) {
	path := os.Getenv("PROMLIFECYCLE_JOURNAL_CRASH_INFO")
	if path == "" {
		return
	}
	f, _, _, api := transactionFixture(t)
	ctx, releaseH, err := ClaimHostOperation(context.Background(), f.db, f.c.HostID, "prepare")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseH()
	ctx, releaseC, err := ClaimComponentOperation(ctx, f.db, f.c.ID, "prepare")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseC()
	row, err := api.PreparePrometheusPairedRestoreTransaction(ctx, f.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(journalCrashInfo{Database: f.dbPath, BackupRoot: f.s.backupRoot, FilesRoot: f.root, ID: row.ID})
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	file.Close()
	// Exit without Go defers, testing the durable COMMIT / claim-release window.
	os.Exit(77)
}
