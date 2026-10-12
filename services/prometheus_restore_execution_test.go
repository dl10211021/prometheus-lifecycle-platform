package services

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promlifecycle/platform/models"
	"gorm.io/gorm/logger"
)

type pairedExecutionAPI interface {
	ExecutePrometheusPairedRestore(context.Context, uint) (*models.PrometheusRestoreTransaction, error)
	RestorePrometheusRecoveryPoint(context.Context, uint) (*models.PrometheusRestoreTransaction, error)
	GetPrometheusPairedRestoreTransaction(context.Context, uint) (*models.PrometheusRestoreTransaction, error)
}

func executionAPI(t *testing.T, s *BackupService) pairedExecutionAPI {
	t.Helper()
	api, ok := any(s).(pairedExecutionAPI)
	if !ok {
		t.Fatal("matched live restore execution and compensation are not implemented")
	}
	return api
}

// Real filesystem commands, with only systemd/process/HTTP boundaries emulated.
type executionFixtureRunner struct {
	base                                    *pairedPreviewRunner
	f                                       *pointFixture
	ctl, stopped                            string
	failHistorical, failCurrent, failHealth bool
	before                                  func(context.Context, string) error
}

func (r *executionFixtureRunner) Run(ctx context.Context, command string) (string, error) {
	if r.before != nil {
		if err := r.before(ctx, command); err != nil {
			return "", err
		}
	}
	if strings.Contains(command, "# promlifecycle:restore-execution-") && strings.Contains(command, "# promlifecycle:restore-stage-fence") {
		for name, fail := range map[string]bool{"fail-historical": r.failHistorical, "fail-current": r.failCurrent} {
			path := filepath.Join(r.f.root, name)
			if fail {
				_ = os.WriteFile(path, []byte("1"), 0600)
			} else {
				_ = os.Remove(path)
			}
		}
		out, err := (localBackupRunner{}).Run(ctx, strings.ReplaceAll(command, "systemctl ", r.ctl+" "))
		_, stopped := os.Stat(r.stopped)
		running := os.IsNotExist(stopped)
		if running && !r.f.runner.running {
			r.f.runner.pid++
			r.f.runner.generation++
		}
		r.f.runner.running = running
		return out, err
	}
	if strings.Contains(command, "# promlifecycle:restore-execution-listener") {
		return "listener-owned\n", nil
	}
	if strings.HasPrefix(command, "curl -fsS") {
		if r.failHealth {
			sample, _ := os.ReadFile(filepath.Join(r.f.c.DataPath, "sample"))
			if string(sample) == "historical-sample" {
				return "", errors.New("injected historical health failure")
			}
		}
		return "", nil
	}
	if strings.Contains(command, "# promlifecycle:recovery-point-version") {
		binary, err := os.ReadFile(r.f.c.BinaryPath)
		if err != nil {
			return "", err
		}
		version := "2.55.0"
		if strings.HasPrefix(string(binary), "current-executable") {
			version = "3.1.0"
		}
		return "prometheus, version " + version + " (fixture)\n", nil
	}
	return r.base.Run(ctx, command)
}
func executionFixture(t *testing.T) (*pointFixture, *executionFixtureRunner, *models.PrometheusRestoreTransaction) {
	t.Helper()
	executionAPI(t, NewBackupService(nil))
	f := newPointFixture(t)
	f.db.Logger = logger.Default.LogMode(logger.Silent)
	f.c.Port = 19090
	f.c.Image = "historical-image-reference"
	if err := f.db.Model(f.c).Updates(map[string]any{"port": f.c.Port, "image": f.c.Image}).Error; err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(prometheusBefore{Component: f.c, Version: f.c.Version, Image: f.c.Image, ImportedArgsSHA256: importedArgsHash(f.c.ImportedArgs)})
	if err := f.db.Model(f.plan).Update("before", string(before)).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.AutoMigrate(&models.PrometheusRestoreTransaction{}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.CreatePrometheusRecoveryPoint(context.Background(), f.plan.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Model(f.plan).Update("status", models.ChangeStatusFailed).Error; err != nil {
		t.Fatal(err)
	}
	base := &pairedPreviewRunner{fixture: f.runner, refs: pairedReferenceFrame(f.c.ServiceName, pairedReferenceBlock(f.c.ServiceName, "FragmentPath="+f.runner.fragment+"\nEnvironmentFiles="+f.runner.environment+" (ignore_errors=no)\nExecStart="+pairedReferenceExec(f.c.BinaryPath)+"\n"))}
	r := &executionFixtureRunner{f: f, base: base, ctl: filepath.Join(f.root, "fixture-systemctl"), stopped: filepath.Join(f.root, "fixture-stopped")}
	script := "#!/bin/bash\nset -eu\nroot='" + f.root + "'\ncase \"$1\" in\nstop) touch \"$root/fixture-stopped\";;\ndaemon-reload) :;;\nstart)\n sample=$(cat \"$root/data/sample\")\n if [ \"$sample\" = historical-sample ]; then test ! -e \"$root/fail-historical\"; else test ! -e \"$root/fail-current\"; fi\n rm -f \"$root/fixture-stopped\";;\nshow)\n case \"$2\" in\n --property=ActiveState) if [ -e \"$root/fixture-stopped\" ]; then echo inactive; else echo active; fi;;\n --property=SubState) if [ -e \"$root/fixture-stopped\" ]; then echo dead; else echo running; fi;;\n --property=MainPID|--property=ControlPID) echo 0;;\n *) exit 3;;\n esac;;\n*) exit 3;;\nesac\n"
	if err := os.WriteFile(r.ctl, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	f.s.runnerFactory = func(*models.Component) (hostCommandRunner, error) { return r, nil }
	for p, v := range map[string]string{filepath.Join(f.c.DataPath, "sample"): "current-checkpoint-sample", f.c.ConfigPath: "global:\n  scrape_interval: 9s\n", f.c.BinaryPath: strings.Repeat("current-executable", 1000), f.runner.environment: "TOKEN=current-checkpoint-private\n"} {
		if err := os.WriteFile(p, []byte(v), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(f.c.BinaryPath, 0755); err != nil {
		t.Fatal(err)
	}
	f.c.Version = "3.1.0"
	f.c.Image = "current-image-reference"
	if err := f.db.Model(f.c).Updates(map[string]any{"version": f.c.Version, "image": f.c.Image}).Error; err != nil {
		t.Fatal(err)
	}
	// Match the real fixture's RawMessage update storage. Both TEXT and BLOB
	// must keep exact snapshot bytes without blocking terminal publication.
	var storedPlan models.ChangePlan
	if err := f.db.First(&storedPlan, f.plan.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Model(&models.ChangePlan{}).Where("id = ?", f.plan.ID).UpdateColumns(map[string]any{"before": []byte(storedPlan.Before), "after": []byte(storedPlan.After)}).Error; err != nil {
		t.Fatal(err)
	}
	var snapshotStorage struct{ BeforeType, AfterType string }
	if err := f.db.Raw("SELECT typeof(before) AS before_type, typeof(after) AS after_type FROM change_plans WHERE id = ?", f.plan.ID).Scan(&snapshotStorage).Error; err != nil {
		t.Fatal(err)
	}
	if snapshotStorage.BeforeType != "blob" || snapshotStorage.AfterType != "blob" {
		t.Fatal("fixture did not retain BLOB snapshot storage")
	}
	row, err := f.s.PreparePrometheusPairedRestoreTransaction(context.Background(), f.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.CreatePrometheusPairedRestoreCheckpoint(context.Background(), row.ID); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(r.stopped, []byte("1"), 0600); err != nil {
		t.Fatal(err)
	}
	row, err = f.s.StagePrometheusPairedRestore(context.Background(), row.ID)
	if err != nil {
		t.Fatal(err)
	}
	return f, r, row
}
func assertExecutionFiles(t *testing.T, f *pointFixture, historical bool) {
	t.Helper()
	wantSample, wantEnv, wantConfig, wantBinary := "current-checkpoint-sample", "TOKEN=current-checkpoint-private\n", "global:\n  scrape_interval: 9s\n", strings.Repeat("current-executable", 1000)
	if historical {
		wantSample = "historical-sample"
		wantEnv = "TOKEN=private-env-sentinel\n"
		wantConfig = "global:\n  scrape_interval: 1s\n"
		wantBinary = strings.Repeat("binary-evidence", 100000)
	}
	for path, want := range map[string]string{filepath.Join(f.c.DataPath, "sample"): wantSample, f.runner.environment: wantEnv, f.c.ConfigPath: wantConfig, f.c.BinaryPath: wantBinary} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("matched restore bytes mismatch for %s: %v", filepath.Base(path), err)
		}
	}
	if !f.runner.running {
		t.Fatal("verified terminal requires running exact service")
	}
	var component models.Component
	if err := f.db.First(&component, f.c.ID).Error; err != nil {
		t.Fatal(err)
	}
	version, image := "3.1.0", "current-image-reference"
	if historical {
		version, image = "2.55.0", "historical-image-reference"
	}
	if component.Version != version || component.Image != image {
		t.Fatalf("terminal metadata does not match verified runtime: version=%s image=%s", component.Version, component.Image)
	}
}
func TestPrometheusRestoreExecutionHistoricalBytesAndTerminalRetry(t *testing.T) {
	t.Parallel()
	f, r, staged := executionFixture(t)
	api := executionAPI(t, f.s)
	row, err := api.ExecutePrometheusPairedRestore(context.Background(), staged.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row == nil || row.Phase != "restore_completed" || row.ActiveHostID != nil || row.ActiveComponentID != nil || row.FinishedAt == nil {
		t.Fatal("historical execution did not publish released terminal")
	}
	assertExecutionFiles(t, f, true)
	if string(row.Payload) != string(staged.Payload) || string(row.Checkpoint) != string(staged.Checkpoint) || string(row.Stage) != string(staged.Stage) {
		t.Fatal("execution changed frozen evidence")
	}
	var plan models.ChangePlan
	f.db.First(&plan, f.plan.ID)
	if plan.Status != models.ChangeStatusRolledBack || len(plan.RollbackEvidence) == 0 {
		t.Fatal("historical outcome not published atomically")
	}
	r.before = func(context.Context, string) error { t.Fatal("terminal repeat contacted host"); return nil }
	repeated, err := api.ExecutePrometheusPairedRestore(context.Background(), row.ID)
	if err != nil || repeated.Revision != row.Revision {
		t.Fatal("terminal execution replay", err)
	}
	repeated, err = api.RestorePrometheusRecoveryPoint(context.Background(), f.plan.ID)
	if err != nil || repeated.ID != row.ID {
		t.Fatal("plan terminal replay", err)
	}
	if err := CheckPrometheusRestoreJournalBeforeStartup(context.Background(), f.db); err != nil {
		t.Fatal("valid terminal blocks startup", err)
	}
	journalAssertClaimsEmpty(t, f.db)
}
func TestPrometheusRestoreExecutionFailedHistoricalStartCompensatesCurrentBytes(t *testing.T) {
	t.Parallel()
	f, r, staged := executionFixture(t)
	r.failHistorical = true
	row, err := executionAPI(t, f.s).ExecutePrometheusPairedRestore(context.Background(), staged.ID)
	if err == nil || row == nil || row.Phase != "restore_compensated" || row.ActiveHostID != nil || row.ActiveComponentID != nil {
		t.Fatal("failed restore must report verified compensation", row, err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatal("noncancelled failed restore reported context.Canceled")
	}
	assertExecutionFiles(t, f, false)
	var plan models.ChangePlan
	f.db.First(&plan, f.plan.ID)
	if plan.Status != models.ChangeStatusFailed || len(plan.RollbackEvidence) == 0 {
		t.Fatal("compensation changed historical plan status or omitted evidence")
	}
	journalAssertClaimsEmpty(t, f.db)
}
