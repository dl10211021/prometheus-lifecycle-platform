package services

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/promlifecycle/platform/models"
	platformsqlite "github.com/promlifecycle/platform/storage/sqlite"
	"gorm.io/gorm"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPrometheusRestoreStageControllerExits(t *testing.T) {
	for _, boundary := range []string{"intent", "copying", "staged", "rotated", "handoff"} {
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
			cmd := exec.CommandContext(ctx, exe, "-test.run=^TestPrometheusRestoreStageControllerExitHelper$", "-test.v")
			cmd.Env = append(os.Environ(), "TMPDIR="+owned, "PROMLIFECYCLE_STAGE_CRASH_INFO="+infoPath, "PROMLIFECYCLE_STAGE_CRASH_BOUNDARY="+boundary)
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
			expected := map[string]string{"intent": "restore_stage_intent", "copying": "restore_stage_copying", "staged": "restore_staged", "rotated": "restore_stage_reclaim", "handoff": "restore_stage_copying"}[boundary]
			if row.Phase != expected {
				t.Fatal("wrong last durable phase", row.Phase)
			}
			if boundary == "intent" {
				if _, err := os.Lstat(p.Fence.Root); !os.IsNotExist(err) {
					t.Fatal("init before intent COMMIT")
				}
			}
			if boundary == "rotated" {
				raw, err := os.ReadFile(filepath.Join(p.Fence.Root, "generation"))
				if err != nil || string(raw) != p.Reclaim.Desired+"\n" {
					t.Fatal("rotation boundary not durable", err)
				}
			}
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
func TestPrometheusRestoreStageControllerExitHelper(t *testing.T) {
	infoPath := os.Getenv("PROMLIFECYCLE_STAGE_CRASH_INFO")
	if infoPath == "" {
		return
	}
	boundary := os.Getenv("PROMLIFECYCLE_STAGE_CRASH_BOUNDARY")
	f, r, prepared, cp := checkpointFixture(t)
	ready, err := cp.CreatePrometheusPairedRestoreCheckpoint(context.Background(), prepared.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(journalCrashInfo{Database: f.dbPath, BackupRoot: f.s.backupRoot, FilesRoot: f.root, ID: ready.ID})
	info, err := os.OpenFile(infoPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	info.Write(raw)
	info.Sync()
	info.Close()
	_, lock, err := platformsqlite.AcquireDatabaseProcessLock(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	r.before = func(_ context.Context, cmd string) error {
		if boundary == "intent" && strings.Contains(cmd, "# promlifecycle:restore-stage-fence") {
			os.Exit(77)
		}
		if strings.Contains(cmd, "# promlifecycle:restore-stage-copy") {
			if boundary == "copying" {
				os.Exit(77)
			}
			if boundary == "rotated" || boundary == "handoff" {
				return errors.New("interrupt before takeover")
			}
		}
		return nil
	}
	if boundary == "staged" {
		f.db.Callback().Delete().Before("gorm:delete").Register("exit-after-staged", func(tx *gorm.DB) {
			if tx.Statement.Table == "component_operation_claims" {
				row := checkpointJournal(t, tx.Session(&gorm.Session{NewDB: true}), ready.ID)
				if row.Phase == "restore_staged" {
					os.Exit(77)
				}
			}
		})
	}
	_, err = f.s.StagePrometheusPairedRestore(context.Background(), ready.ID)
	if boundary != "rotated" && boundary != "handoff" {
		t.Fatal("requested stage exit not reached", err)
	}
	r.before = nil
	if boundary == "rotated" {
		f.db.Callback().Create().Before("gorm:create").Register("exit-after-rotation", func(tx *gorm.DB) {
			if tx.Statement.Table == "host_operation_claims" {
				os.Exit(77)
			}
		})
	} else {
		r.before = func(_ context.Context, cmd string) error {
			if strings.Contains(cmd, "# promlifecycle:restore-stage-copy") {
				os.Exit(77)
			}
			return nil
		}
	}
	_, err = f.s.RecoverPrometheusPairedRestoreStageForStartup(context.Background(), ready.ID, lock)
	t.Fatal("requested recovery exit not reached", err)
}
