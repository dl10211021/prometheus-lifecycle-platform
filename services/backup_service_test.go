package services

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/promlifecycle/platform/core/deployer"
	coressh "github.com/promlifecycle/platform/core/ssh"
	"github.com/promlifecycle/platform/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newBackupTestService(t *testing.T) (*BackupService, *gorm.DB, string) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("access test database: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&models.Backup{}, &models.BackupManifest{}, &models.ComponentConfig{}, &models.ComponentOperationClaim{}, &ComponentRestoreCheckpoint{}, &ComponentBackupPause{}); err != nil {
		t.Fatalf("migrate backup models: %v", err)
	}
	root := filepath.Join(t.TempDir(), "backups")
	service := NewBackupServiceWithRoot(db, root)
	service.runnerFactory = func(*models.Component) (hostCommandRunner, error) { return backupControlRunner{}, nil }
	return service, db, root
}

func backupTestComponent(t *testing.T) *models.Component {
	t.Helper()
	root := runtimeFixtureDir(t)
	configPath := filepath.Join(root, "prometheus.yml")
	dataPath := filepath.Join(root, "prometheus-data")
	if err := os.WriteFile(configPath, []byte("global:\n  scrape_interval: 15s\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.MkdirAll(dataPath, 0o700); err != nil {
		t.Fatalf("create data path: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataPath, "block"), []byte("series-data"), 0o600); err != nil {
		t.Fatalf("write data: %v", err)
	}
	return &models.Component{
		ID: 42, ClusterID: 1, Name: "prometheus", Type: models.ComponentPrometheus,
		Version: "3.5.0", ConfigPath: configPath, DataPath: dataPath,
		DeployType: "binary", ServiceName: "test-component", Host: models.Host{ID: 1, IP: "127.0.0.1", ConnectionType: "local"},
	}
}

func TestCreateComponentBackupScopeAndManifest(t *testing.T) {
	service, _, root := newBackupTestService(t)
	component := backupTestComponent(t)

	tests := []struct {
		scope       string
		format      string
		sourcePaths []string
		archive     bool
	}{
		{scope: "config", format: "file", sourcePaths: []string{component.ConfigPath}},
		{scope: "data", format: "tar.gz", sourcePaths: []string{component.DataPath}, archive: true},
		{scope: "full", format: "tar.gz", sourcePaths: []string{component.ConfigPath, component.DataPath}, archive: true},
	}
	for _, test := range tests {
		t.Run(test.scope, func(t *testing.T) {
			backup, manifest, err := service.CreateComponentBackup(context.Background(), component, "scheduled-"+test.scope, test.scope)
			if err != nil {
				t.Fatalf("create %s backup: %v", test.scope, err)
			}
			rootInfo, err := os.Stat(root)
			if err != nil || rootInfo.Mode().Perm() != 0o700 {
				t.Fatalf("backup root permissions = %v, want 0700 (err=%v)", rootInfo, err)
			}
			artifactInfo, err := os.Stat(backup.Path)
			if err != nil {
				t.Fatalf("stat backup artifact: %v", err)
			}
			if artifactInfo.Mode().Perm()&0o077 != 0 {
				t.Fatalf("backup artifact permissions = %04o, group/other access must be disabled", artifactInfo.Mode().Perm())
			}
			if backup.Type != test.scope || manifest.Scope != test.scope || manifest.Format != test.format {
				t.Fatalf("scope/format mismatch: backup=%s manifest=%s/%s", backup.Type, manifest.Scope, manifest.Format)
			}
			if strings.HasSuffix(backup.Path, ".tar.gz") != test.archive {
				t.Fatalf("scope %s archive path mismatch: %s", test.scope, backup.Path)
			}
			if strings.Join(manifest.SourcePaths, "|") != strings.Join(test.sourcePaths, "|") {
				t.Fatalf("source paths = %v, want %v", manifest.SourcePaths, test.sourcePaths)
			}
			if len(manifest.SHA256) != 64 || manifest.Size != backup.Size || manifest.VerifiedAt == nil {
				t.Fatalf("incomplete manifest integrity evidence: %#v", manifest)
			}
		})
	}
}

func TestVerifyComponentBackupRejectsCorruptionAndFormatMismatch(t *testing.T) {
	service, db, _ := newBackupTestService(t)
	component := backupTestComponent(t)
	backup, _, err := service.CreateComponentBackup(context.Background(), component, "config", "config")
	if err != nil {
		t.Fatalf("create config backup: %v", err)
	}
	if err := os.WriteFile(backup.Path, []byte("corrupted"), 0o600); err != nil {
		t.Fatalf("corrupt artifact: %v", err)
	}
	if _, err := service.VerifyComponentBackup(context.Background(), backup, component); err == nil || !strings.Contains(err.Error(), "不一致") {
		t.Fatalf("corrupt artifact verification error = %v", err)
	}

	if err := db.Model(&models.BackupManifest{}).Where("backup_id = ?", backup.ID).Update("format", "tar.gz").Error; err != nil {
		t.Fatalf("change manifest format: %v", err)
	}
	if _, err := service.VerifyComponentBackup(context.Background(), backup, component); err == nil || !strings.Contains(err.Error(), "格式不匹配") {
		t.Fatalf("format mismatch verification error = %v", err)
	}
}

type checksumFailRunner struct{ delegate hostCommandRunner }

func (runner checksumFailRunner) Run(ctx context.Context, command string) (string, error) {
	if strings.Contains(command, "sha256sum") {
		return "", errors.New("checksum unavailable")
	}
	return runner.delegate.Run(ctx, command)
}

func TestChecksumFailureDoesNotRecordSuccessfulBackup(t *testing.T) {
	service, db, root := newBackupTestService(t)
	component := backupTestComponent(t)
	service.runnerFactory = func(*models.Component) (hostCommandRunner, error) {
		return checksumFailRunner{delegate: localBackupRunner{}}, nil
	}
	if _, _, err := service.CreateComponentBackup(context.Background(), component, "checksum-failure", "config"); err == nil {
		t.Fatal("expected checksum failure")
	}
	var backupCount, manifestCount int64
	if err := db.Model(&models.Backup{}).Count(&backupCount).Error; err != nil {
		t.Fatalf("count backups: %v", err)
	}
	if err := db.Model(&models.BackupManifest{}).Count(&manifestCount).Error; err != nil {
		t.Fatalf("count manifests: %v", err)
	}
	if backupCount != 0 || manifestCount != 0 {
		t.Fatalf("checksum failure recorded backup=%d manifest=%d", backupCount, manifestCount)
	}
	entries, err := os.ReadDir(root)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read backup root: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("checksum failure left artifacts: %v", entries)
	}
}

func TestRestoreFailureAutomaticallyRollsBackPreRestoreState(t *testing.T) {
	service, db, _ := newBackupTestService(t)
	component := backupTestComponent(t)
	backup, _, err := service.CreateComponentBackup(context.Background(), component, "old-config", "config")
	if err != nil {
		t.Fatalf("create restore source: %v", err)
	}
	current := []byte("global:\n  scrape_interval: 5s\n")
	if err := os.WriteFile(component.ConfigPath, current, 0o600); err != nil {
		t.Fatalf("change live config: %v", err)
	}
	if err := os.Chmod(component.ConfigPath, 0o644); err != nil {
		t.Fatal(err)
	}
	failed := false
	service.runnerFactory = func(*models.Component) (hostCommandRunner, error) {
		return backupControlRunner{before: func(ctx context.Context, command string) error {
			if strings.HasPrefix(command, "systemctl restart") && !failed {
				failed = true
				return errors.New("restart failed")
			}
			return nil
		}}, nil
	}
	persistBackupRestoreComponent(t, service, component)
	if err := service.RestoreComponentBackup(context.Background(), backup, component); err == nil {
		t.Fatal("expected restart/readiness failure")
	}
	got, err := os.ReadFile(component.ConfigPath)
	if err != nil {
		t.Fatalf("read rolled-back config: %v", err)
	}
	if string(got) != string(current) {
		t.Fatalf("live config after rollback = %q, want %q", got, current)
	}
	info, err := os.Stat(component.ConfigPath)
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("rollback damaged service-readable mode: %v %v", info, err)
	}
	var checkpoints int64
	if err := db.Model(&ComponentRestoreCheckpoint{}).Where("status = ?", "rolled_back").Count(&checkpoints).Error; err != nil {
		t.Fatalf("count checkpoints: %v", err)
	}
	if checkpoints != 1 {
		t.Fatalf("pre_restore checkpoint count = %d, want 1", checkpoints)
	}
}

func TestStatefulRestoreRecoversRuntimeOnlyAfterSavedData(t *testing.T) {
	service, _, _ := newBackupTestService(t)
	component := backupTestComponent(t)
	backup, _, err := service.CreateComponentBackup(context.Background(), component, "before-migration", "full")
	if err != nil {
		t.Fatal(err)
	}
	block := filepath.Join(component.DataPath, "block")
	if err := os.WriteFile(block, []byte("new-version-only-format"), 0o600); err != nil {
		t.Fatal(err)
	}
	opened := filepath.Join(component.DataPath, "old-version-opened")
	restoreRuntime := func(context.Context) error {
		data, err := os.ReadFile(block)
		if err != nil {
			return err
		}
		if string(data) != "series-data" {
			return errors.New("old runtime cannot open migrated data")
		}
		return os.WriteFile(opened, data, 0o600)
	}
	if err := service.restoreComponentBackup(context.Background(), backup, component, restoreRuntime); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(opened)
	if err != nil || string(data) != "series-data" {
		t.Fatalf("old runtime did not recover saved data: %q, %v", data, err)
	}
	_, release, err := ClaimComponentOperation(context.Background(), service.db, component.ID, "next-upgrade")
	if err != nil {
		t.Fatalf("completed recovery blocked the next lifecycle operation: %v", err)
	}
	release()
}

func TestUnconfirmedRuntimeRecoveryDoesNotRestartUnknownBinary(t *testing.T) {
	service, db, _ := newBackupTestService(t)
	component := backupTestComponent(t)
	backup, _, err := service.CreateComponentBackup(context.Background(), component, "before-migration", "full")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(component.DataPath, "block"), []byte("migrated"), 0o600); err != nil {
		t.Fatal(err)
	}
	restarted := false
	service.runnerFactory = func(*models.Component) (hostCommandRunner, error) {
		return backupControlRunner{before: func(_ context.Context, command string) error {
			if strings.HasPrefix(command, "systemctl restart") {
				restarted = true
			}
			return nil
		}}, nil
	}
	err = service.restoreComponentBackup(context.Background(), backup, component, func(context.Context) error {
		return deployer.ErrBinaryRecoveryUnconfirmed
	})
	if !errors.Is(err, deployer.ErrBinaryRecoveryUnconfirmed) || restarted {
		t.Fatalf("unknown executable was restarted: restarted=%v err=%v", restarted, err)
	}
	var pause ComponentBackupPause
	if err := db.First(&pause, "component_id = ?", component.ID).Error; err != nil || pause.Status != "termination_unconfirmed" {
		t.Fatalf("unsafe recovery was not durably quarantined: %+v, %v", pause, err)
	}
}

func TestRehearsalValidatesArchiveWithoutTouchingLiveData(t *testing.T) {
	service, _, root := newBackupTestService(t)
	component := backupTestComponent(t)
	backup, _, err := service.CreateComponentBackup(context.Background(), component, "data", "data")
	if err != nil {
		t.Fatalf("create data backup: %v", err)
	}
	livePath := filepath.Join(component.DataPath, "block")
	live := []byte("new-live-series")
	if err := os.WriteFile(livePath, live, 0o600); err != nil {
		t.Fatalf("change live data: %v", err)
	}
	result, err := service.RehearseComponentBackup(context.Background(), backup, component)
	if err != nil {
		t.Fatalf("rehearse backup: %v", err)
	}
	if result.Scope != "data" || len(result.ExtractedPaths) != 1 || len(result.Checks) < 6 {
		t.Fatalf("incomplete rehearsal evidence: %#v", result)
	}
	got, err := os.ReadFile(livePath)
	if err != nil {
		t.Fatalf("read live data: %v", err)
	}
	if string(got) != string(live) {
		t.Fatalf("rehearsal changed live data: got %q want %q", got, live)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read backup root: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".rehearse-") {
			t.Fatalf("rehearsal temporary directory was not removed: %s", entry.Name())
		}
	}
}

type backupControlRunner struct {
	before   func(context.Context, string) error
	inactive bool
}

func (r backupControlRunner) Run(ctx context.Context, command string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if r.before != nil {
		if err := r.before(ctx, command); err != nil {
			return "", err
		}
	}
	if strings.HasPrefix(command, "state=$(systemctl show") {
		if r.inactive {
			return "false", nil
		}
		return "true", nil
	}
	if strings.HasPrefix(command, "systemctl ") || strings.HasPrefix(command, "curl ") {
		return "", nil
	}
	return (localBackupRunner{}).Run(ctx, command)
}

func TestConfigBackupRestoresOriginalModesInsideProtectedContainer(t *testing.T) {
	service, _, _ := newBackupTestService(t)
	component := backupTestComponent(t)
	component.ConfigPath = filepath.Join(runtimeFixtureDir(t), "config")
	if err := os.Mkdir(component.ConfigPath, 0o750); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(component.ConfigPath, "prometheus.yml")
	if err := os.WriteFile(config, []byte("global: {}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	stamp := time.Unix(1700000000, 0)
	if err := os.Chtimes(config, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	backup, _, err := service.CreateComponentBackup(context.Background(), component, "modes", "config")
	if err != nil {
		t.Fatal(err)
	}
	container, err := os.Stat(filepath.Dir(backup.Path))
	if err != nil || container.Mode().Perm() != 0o700 {
		t.Fatalf("container protection: %v %v", container, err)
	}
	if err := os.RemoveAll(component.ConfigPath); err != nil {
		t.Fatal(err)
	}
	persistBackupRestoreComponent(t, service, component)
	if err := service.RestoreComponentBackup(context.Background(), backup, component); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(config)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 || !info.ModTime().Equal(stamp) {
		t.Fatalf("restored file metadata: %v", info)
	}
	dir, err := os.Stat(component.ConfigPath)
	if err != nil || dir.Mode().Perm() != 0o750 {
		t.Fatalf("restored directory metadata: %v %v", dir, err)
	}
}

func TestCancelledRestoreRollsBackAbsentPaths(t *testing.T) {
	service, db, _ := newBackupTestService(t)
	component := backupTestComponent(t)
	backup, _, err := service.CreateComponentBackup(context.Background(), component, "full", "full")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(component.ConfigPath); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(component.DataPath); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelled := false
	service.runnerFactory = func(*models.Component) (hostCommandRunner, error) {
		return backupControlRunner{before: func(commandCtx context.Context, command string) error {
			if strings.Contains(command, ".restore-new-") && !cancelled {
				cancelled = true
				if _, err := (localBackupRunner{}).Run(commandCtx, command); err != nil {
					return err
				}
				cancel()
				return ctx.Err()
			}
			return nil
		}}, nil
	}
	persistBackupRestoreComponent(t, service, component)
	if err := service.RestoreComponentBackup(ctx, backup, component); err == nil {
		t.Fatal("expected cancellation")
	}
	if !cancelled {
		t.Fatal("did not reach live replacement")
	}
	for _, path := range []string{component.ConfigPath, component.DataPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("originally absent path was fabricated: %s (%v)", path, err)
		}
	}
	var checkpoint ComponentRestoreCheckpoint
	if err := db.First(&checkpoint).Error; err != nil {
		t.Fatal(err)
	}
	if checkpoint.Status != "rolled_back" {
		t.Fatalf("rollback status: %s", checkpoint.Status)
	}
}

func TestDataBackupQuiescesAndRestartsAfterCancellation(t *testing.T) {
	service, _, _ := newBackupTestService(t)
	component := backupTestComponent(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped, restarted := false, false
	service.runnerFactory = func(*models.Component) (hostCommandRunner, error) {
		return backupControlRunner{before: func(commandCtx context.Context, command string) error {
			if strings.HasPrefix(command, "systemctl stop") {
				stopped = true
			}
			if strings.Contains(command, "tar -chzf") {
				if !stopped {
					t.Fatal("archiving live application data")
				}
				cancel()
				return ctx.Err()
			}
			if strings.HasPrefix(command, "systemctl restart") {
				restarted = commandCtx.Err() == nil
			}
			return nil
		}}, nil
	}
	if _, _, err := service.CreateComponentBackup(ctx, component, "cancel-data", "data"); err == nil {
		t.Fatal("expected cancelled backup")
	}
	if !stopped || !restarted {
		t.Fatalf("quiescence/recovery: stopped=%v restarted=%v", stopped, restarted)
	}
}

func TestUnconfirmedRemoteTerminationRetainsRecoveryBlocker(t *testing.T) {
	service, db, _ := newBackupTestService(t)
	component := backupTestComponent(t)
	restarted := false
	service.runnerFactory = func(*models.Component) (hostCommandRunner, error) {
		return backupControlRunner{before: func(ctx context.Context, command string) error {
			if strings.Contains(command, "tar -chzf") {
				return coressh.ErrRemoteTerminationUnconfirmed
			}
			if strings.HasPrefix(command, "systemctl restart") {
				restarted = true
			}
			return nil
		}}, nil
	}
	_, _, err := service.CreateComponentBackup(context.Background(), component, "unknown-termination", "data")
	if !errors.Is(err, coressh.ErrRemoteTerminationUnconfirmed) {
		t.Fatalf("termination evidence lost: %v", err)
	}
	if restarted {
		t.Fatal("restarted while remote command might still be mutating the snapshot")
	}
	var pause ComponentBackupPause
	if err := db.First(&pause, "component_id = ?", component.ID).Error; err != nil {
		t.Fatal(err)
	}
	if pause.Status != "termination_unconfirmed" {
		t.Fatalf("unsafe follow-up operations were not durably blocked: %#v", pause)
	}
}

func TestLegacyBackupWithoutConsistencyEvidenceCannotMutateLivePaths(t *testing.T) {
	service, db, _ := newBackupTestService(t)
	component := backupTestComponent(t)
	backup, _, err := service.CreateComponentBackup(context.Background(), component, "legacy", "config")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.BackupManifest{}).Where("backup_id = ?", backup.ID).Update("consistency", "").Error; err != nil {
		t.Fatal(err)
	}
	current := []byte("new live configuration\n")
	if err := os.WriteFile(component.ConfigPath, current, 0o600); err != nil {
		t.Fatal(err)
	}
	stopped := false
	service.runnerFactory = func(*models.Component) (hostCommandRunner, error) {
		return backupControlRunner{before: func(ctx context.Context, command string) error {
			if strings.HasPrefix(command, "systemctl stop") {
				stopped = true
			}
			return nil
		}}, nil
	}
	persistBackupRestoreComponent(t, service, component)
	if err := service.RestoreComponentBackup(context.Background(), backup, component); err == nil {
		t.Fatal("accepted legacy unknown metadata/consistency")
	}
	if stopped {
		t.Fatal("stopped application before establishing restore eligibility")
	}
	got, err := os.ReadFile(component.ConfigPath)
	if err != nil || string(got) != string(current) {
		t.Fatalf("legacy restore mutated live configuration: %q %v", got, err)
	}
	if _, err := service.RehearseComponentBackup(context.Background(), backup, component); err != nil {
		t.Fatalf("legacy archive validation should remain available: %v", err)
	}
}

func TestDataBackupDoesNotStartOriginallyStoppedApplication(t *testing.T) {
	service, _, _ := newBackupTestService(t)
	component := backupTestComponent(t)
	restarted := false
	service.runnerFactory = func(*models.Component) (hostCommandRunner, error) {
		return backupControlRunner{inactive: true, before: func(ctx context.Context, command string) error {
			if strings.HasPrefix(command, "systemctl restart") {
				restarted = true
			}
			return nil
		}}, nil
	}
	if _, _, err := service.CreateComponentBackup(context.Background(), component, "stopped-data", "data"); err != nil {
		t.Fatal(err)
	}
	if restarted {
		t.Fatal("backup started an application that was originally stopped")
	}
}
