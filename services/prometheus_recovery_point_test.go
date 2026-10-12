package services

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/promlifecycle/platform/models"
	platformsqlite "github.com/promlifecycle/platform/storage/sqlite"
	"gorm.io/gorm"
)

type pointFixtureRunner struct {
	c                     *models.Component
	fragment, environment string
	running               bool
	pid, generation       int
	calls                 []string
	before                func(context.Context, string) error
	afterRestart          func()
}

func pointProcStat(pid, generation int) string {
	fields := make([]string, 20)
	for i := range fields {
		fields[i] = "0"
	}
	fields[0] = "S"
	fields[19] = strconv.Itoa(generation)
	return fmt.Sprintf("%d (prometheus) %s\n", pid, strings.Join(fields, " "))
}
func (r *pointFixtureRunner) properties() string {
	active, sub, pid := "inactive", "dead", 0
	if r.running {
		active, sub, pid = "active", "running", r.pid
	}
	return fmt.Sprintf("Id=%s\nLoadState=loaded\nFragmentPath=%s\nDropInPaths=\nEnvironmentFiles=%s (ignore_errors=no)\nUnitFileState=disabled\nActiveState=%s\nSubState=%s\nMainPID=%d\nControlPID=0\nNeedDaemonReload=no\nTransient=no\n", r.c.ServiceName, r.fragment, r.environment, active, sub, pid)
}
func (r *pointFixtureRunner) Run(ctx context.Context, command string) (string, error) {
	r.calls = append(r.calls, command)
	if r.before != nil {
		if err := r.before(ctx, command); err != nil {
			return "", err
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	switch {
	case strings.Contains(command, "# promlifecycle:recovery-point-launch"):
		return fmt.Sprintf("a(sasbttttuii) 1 %q 3 %q %q %q false 0 0 0 0 0 0 0\n", r.c.BinaryPath, r.c.BinaryPath, "--config.file="+r.c.ConfigPath, "--storage.tsdb.path="+r.c.DataPath), nil
	case strings.Contains(command, "# promlifecycle:recovery-point-namespace"):
		return "s \"\"\ns \"\"\na(ssbt) 0\na(ssbt) 0\na(ss) 0\nb false\nb false\ns \"no\"\ns \"\"\nas 0\n", nil
	case strings.Contains(command, "# promlifecycle:recovery-point-hooks"):
		return "ExecStartPre=\nExecStartPost=\nExecStop=\nExecStopPost=\nExecCondition=\n", nil
	case strings.Contains(command, "# promlifecycle:recovery-point-runtime"):
		if !r.running {
			return "", errors.New("fixture process stopped")
		}
		args := []string{r.c.BinaryPath, "--config.file=" + r.c.ConfigPath, "--storage.tsdb.path=" + r.c.DataPath}
		raw := strings.Join(args, "\x00") + "\x00"
		roots, err := localBackupRunner{}.Run(ctx, "stat -Lc '%d:%i' -- "+r.c.ConfigPath+" "+r.c.DataPath)
		if err != nil {
			return "", err
		}
		return pointProcStat(r.pid, r.generation) + base64.StdEncoding.EncodeToString([]byte(raw)) + "\n" + roots + pointProcStat(r.pid, r.generation), nil
	case strings.Contains(command, "# promlifecycle:recovery-point-version"):
		return "prometheus, version 2.55.0 (fixture)\n", nil
	case strings.Contains(command, "# promlifecycle:recovery-point-gone"):
		if r.running {
			return pointProcStat(r.pid, r.generation), nil
		}
		return "", nil
	case strings.HasPrefix(command, "systemctl show --no-pager"):
		return r.properties(), nil
	case strings.Contains(command, "state=$(systemctl show --property=LoadState"):
		if r.running {
			return "true", nil
		}
		return "false", nil
	case strings.HasPrefix(command, "systemctl stop "):
		r.running = false
		return "", nil
	case strings.HasPrefix(command, "systemctl restart "):
		r.running = true
		r.pid++
		r.generation++
		if r.afterRestart != nil {
			r.afterRestart()
		}
		return "", nil
	case strings.HasPrefix(command, "systemctl is-active --quiet "):
		if !r.running {
			return "", errors.New("fixture stopped")
		}
		return "", nil
	case strings.HasPrefix(command, "curl -fsS"):
		return "", nil
	case strings.Contains(command, "systemctl "):
		return "", errors.New("unexpected fixture lifecycle command")
	}
	return localBackupRunner{}.Run(ctx, command)
}

type pointFixture struct {
	s            *BackupService
	db           *gorm.DB
	c            *models.Component
	plan         *models.ChangePlan
	runner       *pointFixtureRunner
	root, dbPath string
}

func newPointFixture(t *testing.T) *pointFixture {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("root-only owning-host evidence")
	}
	root, err := os.MkdirTemp("/var/lib", "plc-point-unit-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	dbPath := filepath.Join(t.TempDir(), "point.db")
	db, err := platformsqlite.OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if err := db.AutoMigrate(&models.Component{}, &models.ComponentConfig{}, &models.Host{}, &models.ChangePlan{}, &models.PrometheusRecoveryPoint{}, &models.Backup{}, &models.BackupManifest{}, &models.ComponentOperationClaim{}, &models.HostOperationClaim{}, &ComponentBackupPause{}); err != nil {
		t.Fatal(err)
	}
	host := models.Host{Name: "point-host", IP: "127.0.0.1", ConnectionType: "local", Username: "root"}
	if err := db.Create(&host).Error; err != nil {
		t.Fatal(err)
	}
	c := &models.Component{Name: "point-prometheus", HostID: host.ID, Host: host, Type: models.ComponentPrometheus, DeployType: "binary", Version: "2.55.0", ServiceName: "point-prometheus.service", BinaryPath: filepath.Join(root, "prometheus"), ConfigPath: filepath.Join(root, "prometheus.yml"), DataPath: filepath.Join(root, "data"), Status: models.StatusRunning}
	args, _ := json.Marshal([]string{"--config.file=" + c.ConfigPath, "--storage.tsdb.path=" + c.DataPath})
	c.ImportedArgs = string(args)
	cluster := models.Cluster{Name: "point-cluster"}
	if err := db.Create(&cluster).Error; err != nil {
		t.Fatal(err)
	}
	c.ClusterID = cluster.ID
	if err := db.Create(c).Error; err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(c.DataPath, 0700); err != nil {
		t.Fatal(err)
	}
	files := map[string]struct {
		content string
		mode    os.FileMode
	}{
		c.BinaryPath:                         {strings.Repeat("binary-evidence", 100000), 0755},
		c.ConfigPath:                         {"global:\n  scrape_interval: 1s\n", 0600},
		filepath.Join(c.DataPath, "sample"):  {"historical-sample", 0600},
		filepath.Join(root, c.ServiceName):   {"[Service]\n# private-unit-sentinel\nExecStart=" + c.BinaryPath + " --config.file=" + c.ConfigPath + " --storage.tsdb.path=" + c.DataPath + "\n", 0644},
		filepath.Join(root, "protected.env"): {"TOKEN=private-env-sentinel\n", 0600},
	}
	for path, f := range files {
		if err := os.WriteFile(path, []byte(f.content), f.mode); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := json.Marshal(prometheusBefore{Component: c, Version: c.Version, Image: c.Image, ImportedArgsSHA256: importedArgsHash(c.ImportedArgs)})
	after, _ := json.Marshal(upgradeChange{NewVersion: "3.1.0"})
	plan := &models.ChangePlan{Kind: models.ChangeKindUpgrade, ResourceType: "component", ResourceID: c.ID, Status: models.ChangeStatusExecuting, Before: before, After: after}
	if err := db.Create(plan).Error; err != nil {
		t.Fatal(err)
	}
	runner := &pointFixtureRunner{c: c, fragment: filepath.Join(root, c.ServiceName), environment: filepath.Join(root, "protected.env"), running: true, pid: 1234, generation: 100}
	s := NewBackupServiceWithRoot(db, filepath.Join(root, "backups"))
	s.runnerFactory = func(*models.Component) (hostCommandRunner, error) { return runner, nil }
	return &pointFixture{s, db, c, plan, runner, root, dbPath}
}
func TestPrometheusRecoveryPointPersistsPairedHistory(t *testing.T) {
	f := newPointFixture(t)
	f.runner.afterRestart = func() {
		if err := os.WriteFile(filepath.Join(f.c.DataPath, "sample"), []byte("new-live-sample"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	point, err := f.s.CreatePrometheusRecoveryPoint(context.Background(), f.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if point.PlanID != f.plan.ID || point.ComponentID != f.c.ID || point.BackupID == 0 || point.State != "ready" {
		t.Fatalf("incomplete point: %+v", point)
	}
	var plan models.ChangePlan
	if err := f.db.First(&plan, f.plan.ID).Error; err != nil {
		t.Fatal(err)
	}
	if plan.BackupID == nil || *plan.BackupID != point.BackupID {
		t.Fatal("plan and recovery point not atomically associated")
	}
	var backup models.Backup
	if err := f.db.First(&backup, point.BackupID).Error; err != nil {
		t.Fatal(err)
	}
	archived, err := localBackupRunner{}.Run(context.Background(), "tar -xOzf "+backup.Path+" ./data/data/sample")
	if err != nil || archived != "historical-sample" {
		t.Fatalf("archive did not preserve stopped history: %q %v", archived, err)
	}
	if !f.runner.running || f.runner.pid == 1234 {
		t.Fatal("original service not resumed")
	}
	var payload prometheusRecoveryPayload
	if err := json.Unmarshal(point.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(payload.Startup.Path, "manifest.json"), filepath.Join(payload.Binary.Directory, "prometheus"), backup.Path} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("unprotected artifact %s: %v", path, err)
		}
	}
	if strings.Contains(string(point.Payload), "private-env-sentinel") || strings.Contains(string(point.Payload), f.c.ImportedArgs) {
		t.Fatal("plaintext secret/argv in pinned payload")
	}
	if err := f.db.Model(f.c).Updates(map[string]any{"version": "3.1.0", "imported_args": "changed-later"}).Error; err != nil {
		t.Fatal(err)
	}
	f.runner.running = false
	for _, path := range []string{f.c.BinaryPath, f.c.ConfigPath, f.runner.fragment, f.runner.environment} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.RemoveAll(f.c.DataPath); err != nil {
		t.Fatal(err)
	}
	fresh := NewBackupServiceWithRoot(f.db, f.s.backupRoot)
	fresh.runnerFactory = f.s.runnerFactory
	f.runner.calls = nil
	proof, err := fresh.VerifyPrometheusRecoveryPoint(context.Background(), f.plan.ID)
	if err != nil || proof.HistoricalVersion != "2.55.0" || proof.CaptureState != "ready" || proof.StartupFiles != 2 || proof.BinaryBytes != 1500000 {
		t.Fatalf("historical verification: %+v %v", proof, err)
	}
	for _, cmd := range f.runner.calls {
		if strings.Contains(cmd, "systemctl ") || strings.Contains(cmd, "/proc/") {
			t.Fatal("historical verification queried or mutated current runtime")
		}
	}
	var count int64
	for _, model := range []any{&models.HostOperationClaim{}, &models.ComponentOperationClaim{}} {
		if err := f.db.Model(model).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("leaked claim: %d %v", count, err)
		}
	}
}
