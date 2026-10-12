package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	platformsqlite "github.com/promlifecycle/platform/storage/sqlite"
	"gorm.io/gorm"
)

type pairedRestoreCheckpointAPI interface {
	CreatePrometheusPairedRestoreCheckpoint(context.Context, uint) (*models.PrometheusRestoreTransaction, error)
	VerifyPrometheusPairedRestoreCheckpoint(context.Context, uint) (*models.PrometheusRestoreTransaction, error)
}

func checkpointAPI(t *testing.T, s *BackupService) pairedRestoreCheckpointAPI {
	t.Helper()
	api, ok := any(s).(pairedRestoreCheckpointAPI)
	if !ok {
		t.Fatal("stopped current checkpoint capture and verification are not implemented")
	}
	return api
}

type checkpointTestEvidence struct {
	Schema                int
	TransactionID         uint
	AdmissionSHA256, Root string
	StartedAt, StoppedAt  time.Time
	Evidence              *prometheusRecoveryPayload
}

func checkpointEvidence(t *testing.T, db *gorm.DB, id uint) (checkpointTestEvidence, string) {
	t.Helper()
	var stored struct{ Checkpoint, CheckpointSHA256 string }
	if err := db.Raw("SELECT checkpoint, checkpoint_sha256 FROM prometheus_restore_transactions WHERE id = ?", id).Scan(&stored).Error; err != nil {
		t.Fatal(err)
	}
	var proof checkpointTestEvidence
	if err := json.Unmarshal([]byte(stored.Checkpoint), &proof); err != nil {
		t.Fatal(err)
	}
	if stored.CheckpointSHA256 != pointDigest([]byte(stored.Checkpoint)) {
		t.Fatal("checkpoint digest did not bind persisted evidence")
	}
	return proof, stored.Checkpoint
}
func checkpointJournal(t *testing.T, db *gorm.DB, id uint) models.PrometheusRestoreTransaction {
	t.Helper()
	var row models.PrometheusRestoreTransaction
	if err := db.First(&row, id).Error; err != nil {
		t.Fatal(err)
	}
	return row
}
func checkpointFixture(t *testing.T) (*pointFixture, *pairedPreviewRunner, *models.PrometheusRestoreTransaction, pairedRestoreCheckpointAPI) {
	t.Helper()
	checkpointAPI(t, NewBackupService(nil))
	f, _, r, journal := transactionFixture(t)
	if err := os.WriteFile(filepath.Join(f.c.DataPath, "sample"), []byte("current-checkpoint-sample"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.runner.environment, []byte("TOKEN=current-checkpoint-private\n"), 0600); err != nil {
		t.Fatal(err)
	}
	row, err := journal.PreparePrometheusPairedRestoreTransaction(context.Background(), f.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	r.calls = nil
	return f, r, row, checkpointAPI(t, f.s)
}
func checkpointAssertReadOnly(t *testing.T, calls []string) {
	t.Helper()
	for _, command := range calls {
		for _, forbidden := range []string{"systemctl stop ", "systemctl restart ", "systemctl start ", "systemctl daemon-reload", "mkdir ", "cp -aL ", "rm -", "tar -czf ", "sync -f "} {
			if strings.Contains(command, forbidden) {
				t.Fatalf("checkpoint verification issued host mutation: %s", forbidden)
			}
		}
	}
}

// A capture using retained historical data, a premature stop, or a missing
// ready COMMIT must fail these literal file/phase assertions.
func TestPrometheusRestoreCheckpointCurrentStateAndReopen(t *testing.T) {
	f, r, prepared, api := checkpointFixture(t)
	ctx := context.Background()
	before := pairedPreviewFiles(t, f.root)
	business := transactionBusinessState(t, f.db)
	var reserved, stopSeen, archiveSeen bool
	r.before = func(ctx context.Context, command string) error {
		switch {
		case strings.Contains(command, "# promlifecycle:restore-checkpoint-root"):
			current := checkpointJournal(t, f.db, prepared.ID)
			proof, _ := checkpointEvidence(t, f.db, prepared.ID)
			if current.Phase != "checkpointing" || current.Revision != 2 || proof.Evidence != nil || proof.Schema != 1 || proof.TransactionID != prepared.ID || proof.AdmissionSHA256 != prepared.PayloadSHA256 || proof.StartedAt.IsZero() || !proof.StoppedAt.IsZero() {
				t.Fatal("first host write preceded durable checkpoint reservation")
			}
			if !strings.HasPrefix(proof.Root, f.s.backupRoot+fmt.Sprintf("/restore-%d-", prepared.ID)) {
				t.Fatal("reservation directory does not bind transaction")
			}
			if _, err := os.Lstat(proof.Root); !os.IsNotExist(err) {
				t.Fatal("reservation directory was created before its commit", err)
			}
			if reserved {
				t.Fatal("reserved root recreated")
			}
			reserved = true
		case strings.HasPrefix(command, "systemctl stop "):
			current := checkpointJournal(t, f.db, prepared.ID)
			proof, _ := checkpointEvidence(t, f.db, prepared.ID)
			if !reserved || current.Phase != "checkpoint_stopping" || current.Revision != 3 || proof.Evidence == nil || proof.Evidence.Archive.Path != "" || !proof.StoppedAt.IsZero() {
				t.Fatal("stop preceded durable current startup/binary evidence")
			}
			if _, err := os.Stat(filepath.Join(proof.Evidence.Startup.Path, "manifest.json")); err != nil {
				t.Fatal(err)
			}
			saved, err := os.ReadFile(filepath.Join(proof.Evidence.Binary.Directory, "prometheus"))
			actual, readErr := os.ReadFile(f.c.BinaryPath)
			if err != nil || readErr != nil || string(saved) != string(actual) {
				t.Fatal("stop preceded current executable checkpoint")
			}
			if stopSeen {
				t.Fatal("stop repeated")
			}
			stopSeen = true
		case strings.Contains(command, "cp -aL "):
			if !stopSeen || f.runner.running {
				t.Fatal("archive copied from a running application")
			}
			archiveSeen = true
		}
		return nil
	}
	row, err := api.CreatePrometheusPairedRestoreCheckpoint(ctx, prepared.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row == nil || row.Phase != "checkpoint_ready" || row.Revision != 4 || row.FinishedAt != nil || row.ActiveHostID == nil || row.ActiveComponentID == nil || f.runner.running || !reserved || !stopSeen || !archiveSeen {
		t.Fatal("checkpoint did not remain durably stopped and fenced")
	}
	if string(row.Payload) != string(prepared.Payload) || row.PayloadSHA256 != prepared.PayloadSHA256 || row.Token != prepared.Token || !row.CreatedAt.Equal(prepared.CreatedAt) {
		t.Fatal("checkpoint altered immutable admission")
	}
	proof, raw := checkpointEvidence(t, f.db, row.ID)
	if proof.Evidence == nil || proof.StoppedAt.IsZero() || proof.StoppedAt.Before(proof.StartedAt) || proof.Evidence.Archive.Version != "2.55.0" {
		t.Fatal("incomplete stopped evidence")
	}
	archived, err := (localBackupRunner{}).Run(ctx, "tar -xOzf "+utils.QuoteShell(proof.Evidence.Archive.Path)+" ./data/data/sample")
	if err != nil || archived != "current-checkpoint-sample" {
		t.Fatalf("checkpoint substituted historical data: %q %v", archived, err)
	}
	for _, secret := range []string{"current-checkpoint-private", "current-checkpoint-sample", f.c.ImportedArgs} {
		if strings.Contains(raw, secret) {
			t.Fatal("checkpoint metadata contains private file content")
		}
	}
	public, _ := json.Marshal(row)
	if strings.Contains(string(public), proof.Root) || strings.Contains(string(public), prepared.Token) {
		t.Fatal("public journal exposed private checkpoint")
	}
	after := pairedPreviewFiles(t, f.root)
	for path, original := range before {
		if after[path] != original {
			t.Fatal("checkpoint changed original or historical files:", path)
		}
	}
	if transactionBusinessState(t, f.db) != business {
		t.Fatal("checkpoint changed historical/business rows")
	}
	if closed, err := f.s.AbortPreparedPrometheusPairedRestoreTransaction(ctx, row.ID); closed != nil || !errors.Is(err, ErrPrometheusRecoveryAdmission) {
		t.Fatal("prepared abort cleared post-mutation fence", err)
	}
	if _, release, err := ClaimComponentOperation(ctx, f.db, f.c.ID, "restore-recovery"); !errors.Is(err, ErrPrometheusRecoveryAdmission) {
		if release != nil {
			release()
		}
		t.Fatal("checkpoint allowed generic recovery", err)
	}
	journalAssertClaimsEmpty(t, f.db)
	db, err := platformsqlite.OpenDB(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	connection, _ := db.DB()
	defer connection.Close()
	fresh := NewBackupServiceWithRoot(db, f.s.backupRoot)
	fresh.runnerFactory = f.s.runnerFactory
	r.before = nil
	r.calls = nil
	for _, operation := range []func(context.Context, uint) (*models.PrometheusRestoreTransaction, error){checkpointAPI(t, fresh).VerifyPrometheusPairedRestoreCheckpoint, checkpointAPI(t, fresh).CreatePrometheusPairedRestoreCheckpoint} {
		verified, err := operation(ctx, row.ID)
		if err != nil || verified == nil || verified.Phase != "checkpoint_ready" || verified.Revision != 4 || !verified.UpdatedAt.Equal(row.UpdatedAt) {
			t.Fatal("fresh ready verification/idempotence", err)
		}
	}
	checkpointAssertReadOnly(t, r.calls)
	if !reflect.DeepEqual(after, pairedPreviewFiles(t, f.root)) || f.runner.running {
		t.Fatal("fresh verification mutated filesystem/runtime")
	}
	if transactionBusinessState(t, f.db) != business {
		t.Fatal("verification changed business rows")
	}
	journalAssertClaimsEmpty(t, f.db)
}

func TestPrometheusRestoreCheckpointInputAndCancellation(t *testing.T) {
	var absent *BackupService
	for _, s := range []*BackupService{absent, NewBackupService(nil)} {
		api := checkpointAPI(t, s)
		for _, operation := range []func(context.Context, uint) (*models.PrometheusRestoreTransaction, error){api.CreatePrometheusPairedRestoreCheckpoint, api.VerifyPrometheusPairedRestoreCheckpoint} {
			if row, err := operation(context.Background(), 0); row != nil || !errors.Is(err, ErrPrometheusRecoveryAdmission) {
				t.Fatal("invalid checkpoint service/input admitted", err)
			}
		}
	}
	f, r, row, api := checkpointFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := api.CreatePrometheusPairedRestoreCheckpoint(ctx, row.ID); got != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("checkpoint lost cancellation", err)
	}
	if len(r.calls) != 0 || checkpointJournal(t, f.db, row.ID).Phase != "prepared" {
		t.Fatal("cancelled checkpoint contacted host or changed phase")
	}
	journalAssertClaimsEmpty(t, f.db)
}
