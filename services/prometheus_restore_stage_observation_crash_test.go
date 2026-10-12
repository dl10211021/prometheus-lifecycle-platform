package services

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/promlifecycle/platform/models"
	platformsqlite "github.com/promlifecycle/platform/storage/sqlite"
	"gorm.io/gorm"
)

func TestPrometheusRestoreStageReadyObservationExits(t *testing.T) {
	for _, boundary := range []string{"stage/pre", "stage/in", "stage/post", "verify/pre", "verify/in", "verify/post"} {
		t.Run(boundary, func(t *testing.T) {
			t.Parallel()
			owned := t.TempDir()
			infoPath := filepath.Join(owned, "stage-crash.json")
			t.Cleanup(func() {
				raw, err := os.ReadFile(infoPath)
				if os.IsNotExist(err) {
					return
				}
				if err != nil {
					t.Error(err)
					return
				}
				var info journalCrashInfo
				if json.Unmarshal(raw, &info) != nil {
					t.Error("invalid owned child cleanup record")
					return
				}
				root, err := filepath.EvalSymlinks(info.FilesRoot)
				if os.IsNotExist(err) {
					return
				}
				if err != nil || root != info.FilesRoot || filepath.Dir(root) != "/var/lib" || !strings.HasPrefix(filepath.Base(root), "plc-point-unit-") {
					t.Error("invalid owned child cleanup root")
					return
				}
				if err := os.RemoveAll(root); err != nil {
					t.Error(err)
				}
			})
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, exe, "-test.run=^TestPrometheusRestoreStageReadyObservationExitHelper$", "-test.v")
			cmd.Env = append(os.Environ(), "TMPDIR="+owned, "PROMLIFECYCLE_STAGE_OBSERVATION_INFO="+infoPath, "PROMLIFECYCLE_STAGE_OBSERVATION_BOUNDARY="+boundary)
			output, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 77 {
				t.Fatalf("actual controller exit missing: %v\n%s", err, output)
			}
			raw, err := os.ReadFile(infoPath)
			if err != nil {
				t.Fatal(err)
			}
			var info journalCrashInfo
			if err := json.Unmarshal(raw, &info); err != nil {
				t.Fatal(err)
			}
			root, err := filepath.EvalSymlinks(info.FilesRoot)
			if err != nil || root != info.FilesRoot || filepath.Dir(root) != "/var/lib" || !strings.HasPrefix(filepath.Base(root), "plc-point-unit-") {
				t.Fatal("unowned fixture cleanup")
			}

			db, err := platformsqlite.OpenDB(info.Database)
			if err != nil {
				t.Fatal(err)
			}
			conn, _ := db.DB()
			defer conn.Close()
			row := checkpointJournal(t, db, info.ID)
			p, err := decodeRestoreStage(&row)
			if err != nil {
				t.Fatal(err)
			}
			if row.Phase != "restore_staged" || row.Revision != 7 {
				t.Fatal("observer changed ready journal")
			}
			journalAssertClaimsEmpty(t, db)
			var c models.Component
			if err := db.Preload("Host").First(&c, row.ComponentID).Error; err != nil {
				t.Fatal(err)
			}
			fixture := &pointFixtureRunner{c: &c, fragment: filepath.Join(root, c.ServiceName), environment: filepath.Join(root, "protected.env"), running: false, pid: 1234, generation: 100}
			refs := pairedReferenceBlock(c.ServiceName, "FragmentPath="+fixture.fragment+"\nEnvironmentFiles="+fixture.environment+" (ignore_errors=no)\nExecStart={ path="+c.BinaryPath+" ; argv[]="+c.BinaryPath+" ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0 }\n")
			runner := &pairedPreviewRunner{fixture: fixture, refs: pairedReferenceFrame(c.ServiceName, refs)}
			s := NewBackupServiceWithRoot(db, info.BackupRoot)
			s.runnerFactory = func(*models.Component) (hostCommandRunner, error) { return runner, nil }
			_, lock, err := platformsqlite.AcquireDatabaseProcessLock(info.Database)
			if err != nil {
				t.Fatal("child OS lock not released", err)
			}
			defer lock.Close()
			before := pairedPreviewFiles(t, root)
			got, err := s.RecoverPrometheusPairedRestoreStageForStartup(context.Background(), row.ID, lock)
			if err != nil {
				t.Fatal("explicit takeover after controller exit", err)
			}
			if string(got.Payload) != string(row.Payload) || string(got.Checkpoint) != string(row.Checkpoint) {
				t.Fatal("recovery changed immutable source evidence")
			}
			proof, err := decodeRestoreStage(got)
			if err != nil || got.Phase != "restore_staged" || fixture.running {
				t.Fatal("takeover failed", err)
			}
			after := pairedPreviewFiles(t, root)
			for path, value := range before {
				if path == filepath.Join(p.Fence.Root, "generation") {
					continue
				}
				if after[path] != value {
					t.Fatal("takeover changed retained file", path)
				}
			}
			if proof.Fence.Generation == p.Fence.Generation {
				t.Fatal("takeover reused old generation")
			}
			journalAssertClaimsEmpty(t, db)
			t.Logf("controller exit=77 boundary=%s durable=%s recovered=%s generations=%d", boundary, row.Phase, got.Phase, len(proof.Generations))
		})
	}
}

type readyObservationExitRunner struct{ hostCommandRunner }

func (r readyObservationExitRunner) Run(ctx context.Context, cmd string) (string, error) {
	out, err := r.hostCommandRunner.Run(ctx, cmd)
	if err == nil {
		os.Exit(77)
	}
	return out, err
}
func TestPrometheusRestoreStageReadyObservationExitHelper(t *testing.T) {
	infoPath := os.Getenv("PROMLIFECYCLE_STAGE_OBSERVATION_INFO")
	if infoPath == "" {
		return
	}
	boundary := strings.Split(os.Getenv("PROMLIFECYCLE_STAGE_OBSERVATION_BOUNDARY"), "/")
	if len(boundary) != 2 {
		t.Fatal("invalid observation boundary")
	}
	f, r, prepared, cp := checkpointFixture(t)
	ready, err := cp.CreatePrometheusPairedRestoreCheckpoint(context.Background(), prepared.ID)
	if err != nil {
		t.Fatal(err)
	}
	staged, err := f.s.StagePrometheusPairedRestore(context.Background(), ready.ID)
	if err != nil {
		t.Fatal(err)
	}
	journalAssertClaimsEmpty(t, f.db)
	raw, err := json.Marshal(journalCrashInfo{Database: f.dbPath, BackupRoot: f.s.backupRoot, FilesRoot: f.root, ID: staged.ID})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.OpenFile(infoPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = info.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err = info.Sync(); err != nil {
		t.Fatal(err)
	}
	if err = info.Close(); err != nil {
		t.Fatal(err)
	}
	_, lock, err := platformsqlite.AcquireDatabaseProcessLock(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	// Any durable observation mutation is a distinct failure exit, including cleanup.
	mutation := func(tx *gorm.DB) {
		if tx.Statement.Table == "host_operation_claims" || tx.Statement.Table == "component_operation_claims" || tx.Statement.Table == "prometheus_restore_transactions" {
			os.Exit(78)
		}
	}
	if err = f.db.Callback().Create().Before("gorm:create").Register("ready-exit-create", mutation); err != nil {
		t.Fatal(err)
	}
	if err = f.db.Callback().Delete().Before("gorm:delete").Register("ready-exit-delete", mutation); err != nil {
		t.Fatal(err)
	}
	if err = f.db.Callback().Update().Before("gorm:update").Register("ready-exit-update", mutation); err != nil {
		t.Fatal(err)
	}
	f.s.runnerFactory = func(*models.Component) (hostCommandRunner, error) {
		if boundary[1] == "pre" {
			os.Exit(77)
		}
		if boundary[1] == "in" {
			return readyObservationExitRunner{r}, nil
		}
		return r, nil
	}
	op := f.s.StagePrometheusPairedRestore
	if boundary[0] == "verify" {
		op = f.s.VerifyPrometheusPairedRestoreStage
	}
	got, err := op(context.Background(), staged.ID)
	if err != nil || !sameRestoreStageRow(got, staged) {
		t.Fatal("ready observation failed before exit", err)
	}
	if boundary[1] == "post" {
		os.Exit(77)
	}
	t.Fatal("requested observation exit not reached")
}
