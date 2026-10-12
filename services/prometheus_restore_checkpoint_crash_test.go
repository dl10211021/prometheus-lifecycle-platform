package services

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/promlifecycle/platform/models"
	platformsqlite "github.com/promlifecycle/platform/storage/sqlite"
)

func TestPrometheusRestoreCheckpointCrashBoundaries(t *testing.T) {
	for _, boundary := range []string{"reservation", "before-stop", "after-stop", "ready"} {
		t.Run(boundary, func(t *testing.T) {
			owned := t.TempDir()
			infoPath := filepath.Join(owned, "checkpoint-crash.json")
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(exe, "-test.run=^TestPrometheusRestoreCheckpointCrashHelper$", "-test.v")
			cmd.Env = append(os.Environ(), "TMPDIR="+owned, "PROMLIFECYCLE_CHECKPOINT_CRASH_INFO="+infoPath, "PROMLIFECYCLE_CHECKPOINT_CRASH_BOUNDARY="+boundary)
			output, err := cmd.CombinedOutput()
			var exited *exec.ExitError
			if !errors.As(err, &exited) || exited.ExitCode() != 77 {
				t.Fatalf("checkpoint crash boundary not reached: %v\n%s", err, output)
			}
			raw, err := os.ReadFile(infoPath)
			if err != nil {
				t.Fatal(err)
			}
			var info journalCrashInfo
			if err := json.Unmarshal(raw, &info); err != nil {
				t.Fatal(err)
			}
			// Only remove the exact fixture directory reported by this confirmed-dead
			// child, after checking canonical ownership beneath /var/lib.
			resolved, err := filepath.EvalSymlinks(info.FilesRoot)
			if err != nil || resolved != info.FilesRoot || filepath.Dir(resolved) != "/var/lib" || !strings.HasPrefix(filepath.Base(resolved), "plc-point-unit-") {
				t.Fatal("invalid crash fixture cleanup root", err)
			}
			t.Cleanup(func() {
				if err := os.RemoveAll(resolved); err != nil {
					t.Error(err)
				}
			})
			db, err := platformsqlite.OpenDB(info.Database)
			if err != nil {
				t.Fatal(err)
			}
			connection, _ := db.DB()
			defer connection.Close()
			row := checkpointJournal(t, db, info.ID)
			want, revision := "checkpoint_stopping", uint64(3)
			if boundary == "reservation" {
				want, revision = "checkpointing", 2
			}
			if boundary == "ready" {
				want, revision = "checkpoint_ready", 4
			}
			if row.Phase != want || row.Revision != revision {
				t.Fatal("crash lost last committed phase", row.Phase, row.Revision)
			}
			if _, err := decodeRestoreJournal(&row); err != nil {
				t.Fatal("crashed evidence structurally unreadable", err)
			}
			proof, _ := checkpointEvidence(t, db, row.ID)
			if boundary == "reservation" {
				if _, err := os.Lstat(proof.Root); !os.IsNotExist(err) {
					t.Fatal("reservation was not durable before first mkdir", err)
				}
			}
			before := pairedPreviewFiles(t, info.FilesRoot)
			if err := RecoverPreparedPrometheusRestoreClaimsForStartup(context.Background(), info.Database); !errors.Is(err, ErrPrometheusRecoveryAdmission) {
				t.Fatal("startup accepted interrupted checkpoint", err)
			}
			for _, model := range []any{&models.HostOperationClaim{}, &models.ComponentOperationClaim{}} {
				var count int64
				if err := db.Model(model).Count(&count).Error; err != nil || count != 1 {
					t.Fatal("startup reclaimed a potentially stopped checkpoint owner", count, err)
				}
			}
			s := NewBackupServiceWithRoot(db, info.BackupRoot)
			s.runnerFactory = func(*models.Component) (hostCommandRunner, error) {
				t.Fatal("crashed checkpoint resumed host work")
				return nil, errors.New("unexpected host")
			}
			for _, op := range []func(context.Context, uint) (*models.PrometheusRestoreTransaction, error){s.CreatePrometheusPairedRestoreCheckpoint, s.VerifyPrometheusPairedRestoreCheckpoint, s.AbortPreparedPrometheusPairedRestoreTransaction} {
				if result, err := op(context.Background(), row.ID); result != nil || !errors.Is(err, ErrPrometheusRecoveryAdmission) {
					t.Fatal("crash fence cleared or retried", err)
				}
			}
			if !reflect.DeepEqual(before, pairedPreviewFiles(t, info.FilesRoot)) {
				t.Fatal("crash handling changed forensic files")
			}
		})
	}
}
func TestPrometheusRestoreCheckpointCrashHelper(t *testing.T) {
	path := os.Getenv("PROMLIFECYCLE_CHECKPOINT_CRASH_INFO")
	if path == "" {
		return
	}
	boundary := os.Getenv("PROMLIFECYCLE_CHECKPOINT_CRASH_BOUNDARY")
	f, r, row, api := checkpointFixture(t)
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
	r.before = func(ctx context.Context, command string) error {
		crash := boundary == "reservation" && strings.Contains(command, "# promlifecycle:restore-checkpoint-root") ||
			boundary == "before-stop" && strings.HasPrefix(command, "systemctl stop ") ||
			boundary == "after-stop" && strings.Contains(command, "cp -aL ")
		if crash {
			if boundary == "after-stop" && f.runner.running {
				t.Fatal("post-stop crash preceded stop")
			}
			os.Exit(77)
		}
		return nil
	}
	ready, err := api.CreatePrometheusPairedRestoreCheckpoint(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if boundary != "ready" || ready.Phase != "checkpoint_ready" || f.runner.running {
		t.Fatal("unexpected crash completion")
	}
	os.Exit(77)
}
