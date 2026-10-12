//go:build linux

package services

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/promlifecycle/platform/core/deployer"
	"github.com/promlifecycle/platform/models"
	storesqlite "github.com/promlifecycle/platform/storage/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type realPrometheusSample struct {
	Metric    map[string]string `json:"metric"`
	Timestamp float64           `json:"timestamp"`
	Value     string            `json:"value"`
}

type realPrometheusEvidence struct {
	Schema      int            `json:"schema"`
	Test        string         `json:"test"`
	RunID       string         `json:"run_id"`
	Namespace   string         `json:"namespace"`
	Unit        string         `json:"unit"`
	RunRoot     string         `json:"run_root"`
	StartedAt   time.Time      `json:"started_at"`
	CompletedAt time.Time      `json:"completed_at"`
	Status      string         `json:"status"`
	Packages    map[string]any `json:"packages"`
	Baseline    map[string]any `json:"baseline"`
	Upgrade     map[string]any `json:"upgrade"`
	Rejections  map[string]any `json:"rejections"`
	Rollback    map[string]any `json:"rollback"`
	WALRecovery map[string]any `json:"wal_recovery"`
	Cleanup     map[string]any `json:"cleanup"`
}
type realPrometheusStopObservation struct {
	InitialMainPID    int
	InitialMainPIDErr error
	StopErr           error
	ActiveState       string
	ActiveStateErr    error
	FinalMainPID      int
	FinalMainPIDErr   error
	TrackedPIDExited  bool
}

type realPrometheusUnitFileIdentity struct {
	device uint64
	inode  uint64
}

func validateRealPrometheusStopped(observation realPrometheusStopObservation) error {
	switch {
	case observation.InitialMainPIDErr != nil:
		return fmt.Errorf("initial MainPID lookup: %w", observation.InitialMainPIDErr)
	case observation.InitialMainPID < 0 || observation.InitialMainPID == 1:
		return fmt.Errorf("invalid initial MainPID %d", observation.InitialMainPID)
	case observation.StopErr != nil:
		return fmt.Errorf("stop owned unit: %w", observation.StopErr)
	case observation.ActiveStateErr != nil:
		return fmt.Errorf("final ActiveState lookup: %w", observation.ActiveStateErr)
	case observation.ActiveState != "inactive" && observation.ActiveState != "failed":
		return fmt.Errorf("final ActiveState=%q is not stopped", observation.ActiveState)
	case observation.FinalMainPIDErr != nil:
		return fmt.Errorf("final MainPID lookup: %w", observation.FinalMainPIDErr)
	case observation.FinalMainPID != 0:
		return fmt.Errorf("final MainPID=%d, want 0", observation.FinalMainPID)
	case observation.InitialMainPID > 1 && !observation.TrackedPIDExited:
		return fmt.Errorf("tracked PID %d did not exit", observation.InitialMainPID)
	default:
		return nil
	}
}

type realPrometheusMetricFixture struct {
	mu    sync.RWMutex
	phase string
}

func (m *realPrometheusMetricFixture) setPhase(phase string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.phase = phase
}

func (m *realPrometheusMetricFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/metrics" {
		http.NotFound(w, r)
		return
	}
	m.mu.RLock()
	phase := m.phase
	m.mu.RUnlock()
	w.Header().Set("Content-Type", "application/openmetrics-text; version=1.0.0; charset=utf-8")
	switch phase {
	case "baseline":
		fmt.Fprintln(w, "# TYPE plc_head gauge")
		fmt.Fprintln(w, `plc_head{phase="baseline"} 255`)
	case "post_upgrade":
		fmt.Fprintln(w, "# TYPE plc_post_upgrade gauge")
		fmt.Fprintln(w, `plc_post_upgrade{phase="after_upgrade"} 310`)
	case "wal":
		fmt.Fprintln(w, "# TYPE plc_wal_recovery gauge")
		fmt.Fprintln(w, `plc_wal_recovery{phase="wal"} 2551`)
	}
	fmt.Fprintln(w, "# EOF")
}

type realPrometheusFixture struct {
	t               *testing.T
	token           string
	parentRoot      string
	runRoot         string
	namespace       string
	unit            string
	unitPath        string
	binaryPath      string
	configPath      string
	dataPath        string
	basePath        string
	backupPath      string
	evidencePath    string
	logsPath        string
	promPort        int
	scrapePort      int
	metric          *realPrometheusMetricFixture
	metricServer    *http.Server
	metricListener  net.Listener
	db              *gorm.DB
	service         *ChangeService
	component       models.Component
	evidence        realPrometheusEvidence
	unitInstalled   bool
	provisionalUnit *realPrometheusUnitFileIdentity
	cleaned         bool
}

func newRealPrometheusFixture(t *testing.T) *realPrometheusFixture {
	t.Helper()
	rootValue := strings.TrimSpace(os.Getenv(realPrometheusRootEnv))
	namespace := strings.TrimSpace(os.Getenv(realPrometheusNSenv))
	archive255 := strings.TrimSpace(os.Getenv(realPrometheus255Env))
	archive310 := strings.TrimSpace(os.Getenv(realPrometheus310Env))
	for name, value := range map[string]string{
		realPrometheusRootEnv: rootValue, realPrometheusNSenv: namespace,
		realPrometheus255Env: archive255, realPrometheus310Env: archive310,
	} {
		if value == "" {
			t.Fatalf("%s=1 requires nonempty %s", realPrometheusOptInEnv, name)
		}
	}
	parentRoot, err := validateRealPrometheusEnvironmentRoot(rootValue)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateRealPrometheusNamespace(namespace); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"systemctl", "journalctl"} {
		if _, err := exec.LookPath(command); err != nil {
			t.Fatalf("required command %s: %v", command, err)
		}
	}
	if output, err := exec.Command("systemctl", "is-system-running").CombinedOutput(); err != nil && strings.TrimSpace(string(output)) == "offline" {
		t.Fatalf("systemd is unavailable: %s: %v", strings.TrimSpace(string(output)), err)
	}
	token, err := newRealPrometheusRunToken()
	if err != nil {
		t.Fatal(err)
	}
	runRoot := filepath.Join(parentRoot, "run-"+token)
	if err := os.Mkdir(runRoot, 0700); err != nil {
		t.Fatalf("create unique run root: %v", err)
	}
	unit := namespace + "-" + token
	owner := realPrometheusOwner{Schema: 1, Namespace: namespace, Unit: unit, RunRoot: runRoot, CreatedBy: realPrometheusCreatedBy}
	if err := writeRealPrometheusOwner(runRoot, owner); err != nil {
		t.Fatal(err)
	}
	f := &realPrometheusFixture{
		t: t, token: token, parentRoot: parentRoot, runRoot: runRoot, namespace: namespace, unit: unit,
		unitPath:   filepath.Join("/etc/systemd/system", unit+".service"),
		binaryPath: filepath.Join(runRoot, "install", "prometheus"), configPath: filepath.Join(runRoot, "config", "prometheus.yml"),
		dataPath: filepath.Join(runRoot, "data"), basePath: filepath.Join(runRoot, "platform"), backupPath: filepath.Join(runRoot, "backups"),
		evidencePath: filepath.Join(parentRoot, "evidence-"+token+".json"), logsPath: filepath.Join(parentRoot, "logs-"+token),
	}
	f.evidence = realPrometheusEvidence{
		Schema: 1, Test: realPrometheusCreatedBy, RunID: token, Namespace: namespace, Unit: unit, RunRoot: runRoot,
		StartedAt: time.Now().UTC(), Status: "running", Packages: map[string]any{}, Baseline: map[string]any{}, Upgrade: map[string]any{},
		Rejections: map[string]any{}, Rollback: map[string]any{}, WALRecovery: map[string]any{}, Cleanup: map[string]any{},
	}
	t.Cleanup(f.cleanup)
	for _, path := range []string{filepath.Dir(f.binaryPath), filepath.Dir(f.configPath), f.dataPath, filepath.Join(f.basePath, "packages"), f.backupPath, f.logsPath} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	package255 := f.preparePackage(t, archive255, "2.55.0", realPrometheus255SHA256)
	package310 := f.preparePackage(t, archive310, "3.1.0", realPrometheus310SHA256)
	promtoolPath := filepath.Join(runRoot, "tools", "promtool-2.55.0")
	if err := os.MkdirAll(filepath.Dir(promtoolPath), 0700); err != nil {
		t.Fatal(err)
	}
	prometheus310Path := filepath.Join(runRoot, "tools", "prometheus-3.1.0")
	if err := extractRealPrometheusArchiveFile(package255.StoragePath, "prometheus", f.binaryPath); err != nil {
		t.Fatal(err)
	}
	if err := extractRealPrometheusArchiveFile(package255.StoragePath, "promtool", promtoolPath); err != nil {
		t.Fatal(err)
	}
	if err := extractRealPrometheusArchiveFile(package310.StoragePath, "prometheus", prometheus310Path); err != nil {
		t.Fatal(err)
	}
	f.evidence.Packages["2.55.0"] = packageEvidence(t, package255, f.binaryPath, promtoolPath, "2.55.0")
	f.evidence.Packages["3.1.0"] = packageEvidence(t, package310, prometheus310Path, "", "3.1.0")
	f.prepareDatabase(t, package255, package310)
	f.startMetricFixture(t)
	f.promPort = reserveRealPrometheusPort(t)
	f.writeConfigAndHistoricalBlock(t, promtoolPath)
	f.installUnit(t)
	f.requireCheckpoint()
	t.Logf("real acceptance evidence: %s", f.evidencePath)
	return f
}

func validateRealPrometheusRootSyntax(root string) error {
	for _, character := range root {
		if character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			strings.ContainsRune("/._-", character) {
			continue
		}
		return fmt.Errorf("acceptance root contains unsafe character %q", character)
	}
	if root == "" || root[0] != '/' {
		return errors.New("acceptance root must use an absolute Linux path")
	}
	return nil
}

func (f *realPrometheusFixture) preparePackage(t *testing.T, source, version, wantSHA string) models.Package {
	t.Helper()
	gotSHA, size, err := realPrometheusFileSHA256(source)
	if err != nil {
		t.Fatalf("inspect Prometheus %s archive: %v", version, err)
	}
	if gotSHA != wantSHA {
		t.Fatalf("Prometheus %s archive SHA256=%s, want %s", version, gotSHA, wantSHA)
	}
	destination := filepath.Join(f.basePath, "packages", "prometheus-"+version+".linux-amd64.tar.gz")
	if err := copyRealPrometheusFile(source, destination, 0600); err != nil {
		t.Fatal(err)
	}
	copiedSHA, copiedSize, err := realPrometheusFileSHA256(destination)
	if err != nil || copiedSHA != wantSHA || copiedSize != size {
		t.Fatalf("copied Prometheus %s package binding invalid: sha=%s size=%d err=%v", version, copiedSHA, copiedSize, err)
	}
	return models.Package{Name: "prometheus-" + version, Type: models.ComponentPrometheus, Version: version, Format: "binary", Size: size, Checksum: wantSHA, StoragePath: destination, Description: "real acceptance official package"}
}

func packageEvidence(t *testing.T, pkg models.Package, prometheusPath, promtoolPath, version string) map[string]any {
	t.Helper()
	result := map[string]any{"archive_path": pkg.StoragePath, "archive_sha256": pkg.Checksum, "archive_size": pkg.Size, "version": version}
	for name, path := range map[string]string{"prometheus": prometheusPath, "promtool": promtoolPath} {
		if path == "" {
			continue
		}
		output, err := exec.Command(path, "--version").CombinedOutput()
		if err != nil || !strings.Contains(string(output), "version "+version) {
			t.Fatalf("%s %s version binding failed: %s: %v", name, version, strings.TrimSpace(string(output)), err)
		}
		hash, size, err := realPrometheusFileSHA256(path)
		if err != nil {
			t.Fatal(err)
		}
		result[name+"_version_output"] = strings.TrimSpace(string(output))
		result[name+"_sha256"] = hash
		result[name+"_size"] = size
	}
	return result
}

func (f *realPrometheusFixture) prepareDatabase(t *testing.T, packages ...models.Package) {
	t.Helper()
	db, err := storesqlite.OpenDB(filepath.Join(f.runRoot, "state", "acceptance.db"))
	if err != nil {
		t.Fatal(err)
	}
	f.db = db
	db.Logger = logger.Default.LogMode(logger.Silent)
	if err := db.AutoMigrate(
		&models.Host{}, &models.Cluster{}, &models.Component{}, &models.Operation{}, &models.Backup{}, &models.Package{},
		&models.ComponentVersion{}, &models.ChangePlan{}, &models.ChangeBaseline{}, &models.ComponentOperationClaim{},
		&models.HostOperationClaim{}, &models.Incident{}, &models.IncidentEvent{}, &models.BackupManifest{},
		&ComponentRestoreCheckpoint{}, &ComponentBackupPause{},
	); err != nil {
		t.Fatal(err)
	}
	for i := range packages {
		if err := deployer.ValidatePackage(f.basePath, packages[i]); err != nil {
			t.Fatalf("validate package %s: %v", packages[i].Version, err)
		}
		if err := db.Create(&packages[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
}

func (f *realPrometheusFixture) startMetricFixture(t *testing.T) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.metricListener = listener
	f.scrapePort = listener.Addr().(*net.TCPAddr).Port
	f.metric = &realPrometheusMetricFixture{phase: "baseline"}
	f.metricServer = &http.Server{Handler: f.metric, ReadHeaderTimeout: 2 * time.Second}
	go func() { _ = f.metricServer.Serve(listener) }()
}

func reserveRealPrometheusPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func (f *realPrometheusFixture) writeConfigAndHistoricalBlock(t *testing.T, promtool string) {
	t.Helper()
	config := fmt.Sprintf("global:\n  scrape_interval: 250ms\n  scrape_timeout: 200ms\nscrape_configs:\n  - job_name: prometheus\n    static_configs:\n      - targets: ['127.0.0.1:%d']\n  - job_name: owned_fixture\n    static_configs:\n      - targets: ['127.0.0.1:%d']\n", f.promPort, f.scrapePort)
	if err := os.WriteFile(f.configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	checkOutput, err := exec.Command(promtool, "check", "config", f.configPath).CombinedOutput()
	if err != nil {
		t.Fatalf("package-associated promtool config check: %s: %v", strings.TrimSpace(string(checkOutput)), err)
	}
	historical := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)
	openMetrics := filepath.Join(f.runRoot, "historical.openmetrics")
	payload := fmt.Sprintf("# TYPE plc_historical gauge\nplc_historical{phase=\"baseline\"} 255 %d\n# EOF\n", historical.Unix())
	if err := os.WriteFile(openMetrics, []byte(payload), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	output, err := realPrometheusRun(ctx, promtool, "tsdb", "create-blocks-from", "openmetrics", openMetrics, f.dataPath)
	if err != nil {
		t.Fatalf("create real historical TSDB block: %s: %v", output, err)
	}
	f.evidence.Baseline["historical_input_timestamp"] = float64(historical.Unix())
	f.evidence.Baseline["promtool_config_check"] = strings.TrimSpace(string(checkOutput))
	f.evidence.Baseline["promtool_block_output"] = strings.TrimSpace(output)
}

func (f *realPrometheusFixture) importedArgs() []string {
	return []string{
		"--config.file=" + f.configPath,
		"--storage.tsdb.path=" + f.dataPath,
		"--storage.tsdb.retention.time=30d",
		"--storage.tsdb.wal-compression",
		"--query.timeout=2m",
		"--query.max-concurrency=20",
		"--web.enable-lifecycle",
		fmt.Sprintf("--web.listen-address=127.0.0.1:%d", f.promPort),
	}
}

func (f *realPrometheusFixture) installUnit(t *testing.T) {
	t.Helper()
	if _, err := os.Lstat(f.unitPath); err == nil {
		t.Fatalf("refusing to overwrite preexisting unit %s", f.unitPath)
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	args := f.importedArgs()
	unit := fmt.Sprintf("# promlifecycle-real-acceptance schema=1 namespace=%s run_root=%s\n[Unit]\nDescription=Owned Prometheus lifecycle acceptance %s\nAfter=network.target\n[Service]\nType=simple\nExecStart=%s %s\nWorkingDirectory=%s\nRestart=no\nTimeoutStopSec=30s\n[Install]\nWantedBy=multi-user.target\n", f.namespace, f.runRoot, f.token, f.binaryPath, strings.Join(args, " "), f.runRoot)
	if err := f.writeUnitFileExclusive(unit, func(file *os.File, value string) (int, error) {
		return io.WriteString(file, value)
	}, func(file *os.File) error { return file.Close() }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if output, err := realPrometheusRun(ctx, "systemctl", "daemon-reload"); err != nil {
		t.Fatalf("systemctl daemon-reload: %s: %v", output, err)
	}
}

func (f *realPrometheusFixture) writeUnitFileExclusive(unit string, write func(*os.File, string) (int, error), closeFile func(*os.File) error) error {
	file, err := os.OpenFile(f.unitPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return err
	}
	info, statErr := file.Stat()
	if statErr != nil {
		return errors.Join(statErr, file.Close())
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() {
		return errors.Join(errors.New("created unit fragment has unprovable identity"), file.Close())
	}
	f.provisionalUnit = &realPrometheusUnitFileIdentity{device: uint64(stat.Dev), inode: stat.Ino}
	_, writeErr := write(file, unit)
	closeErr := closeFile(file)
	if writeErr != nil || closeErr != nil {
		return errors.Join(writeErr, closeErr, f.removeProvisionalUnitFile())
	}
	f.provisionalUnit = nil
	f.unitInstalled = true
	return nil
}

func (f *realPrometheusFixture) removeProvisionalUnitFile() error {
	identity := f.provisionalUnit
	if identity == nil {
		return nil
	}
	info, err := os.Lstat(f.unitPath)
	if os.IsNotExist(err) {
		f.provisionalUnit = nil
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect provisional unit fragment: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || uint64(stat.Dev) != identity.device || stat.Ino != identity.inode {
		return errors.New("provisional unit fragment was replaced; refusing removal")
	}
	if err := os.Remove(f.unitPath); err != nil {
		return fmt.Errorf("remove exact provisional unit fragment: %w", err)
	}
	f.provisionalUnit = nil
	return nil
}

func (f *realPrometheusFixture) createComponent(t *testing.T) {
	t.Helper()
	host := models.Host{Name: "real-local-" + f.token, IP: "127.0.0.1", OS: "linux", ConnectionType: "local", Status: "online"}
	cluster := models.Cluster{Name: "real-cluster-" + f.token, Environment: "test", DeploymentType: "binary", Status: "active"}
	if err := f.db.Create(&host).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Create(&cluster).Error; err != nil {
		t.Fatal(err)
	}
	imported, err := json.Marshal(f.importedArgs())
	if err != nil {
		t.Fatal(err)
	}
	f.component = models.Component{
		ClusterID: cluster.ID, Cluster: cluster, HostID: host.ID, Host: host, Type: models.ComponentPrometheus,
		Name: "real-prometheus-" + f.token, Version: "2.55.0", Port: f.promPort, DeployType: "binary", Status: models.StatusRunning,
		ConfigPath: f.configPath, DataPath: f.dataPath, InstallPath: filepath.Dir(f.binaryPath), BinaryPath: f.binaryPath,
		ServiceName: f.unit, ImportedArgs: string(imported), InternalURL: fmt.Sprintf("http://127.0.0.1:%d", f.promPort),
	}
	if err := f.db.Omit("Host", "Cluster").Create(&f.component).Error; err != nil {
		t.Fatal(err)
	}
	backups := NewBackupServiceWithRoot(f.db, f.backupPath)
	f.service = NewChangeService(f.db, deployer.NewFactory(f.basePath), nil, backups, f.basePath)
}

func (f *realPrometheusFixture) startPrometheus(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", f.promPort))
	if err != nil {
		t.Fatalf("selected Prometheus port is no longer free: %v", err)
	}
	_ = listener.Close()
	if err := f.verifyUnitOwnership(); err != nil {
		t.Fatalf("owned unit preflight before start: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if output, err := realPrometheusRun(ctx, "systemctl", "start", f.unit); err != nil {
		t.Fatalf("start owned Prometheus unit: %s: %v", output, err)
	}
	if err := f.waitReady(ctx); err != nil {
		t.Fatal(err)
	}
	pid, err := f.mainPID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.validateOwnedPID(pid); err != nil {
		t.Fatal(err)
	}
	return pid
}

func (f *realPrometheusFixture) waitReady(ctx context.Context) error {
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/-/ready", f.promPort)
	for {
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("Prometheus readiness timeout: %w", ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (f *realPrometheusFixture) query(ctx context.Context, expression string, evaluation *float64) ([]realPrometheusSample, error) {
	values := url.Values{"query": []string{expression}}
	if evaluation != nil {
		values.Set("time", strconv.FormatFloat(*evaluation, 'f', 3, 64))
	}
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/api/v1/query?%s", f.promPort, values.Encode())
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return nil, fmt.Errorf("query HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var envelope struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Value  []json.RawMessage `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&envelope); err != nil {
		return nil, err
	}
	if envelope.Status != "success" {
		return nil, errors.New("Prometheus query status is not success")
	}
	result := make([]realPrometheusSample, 0, len(envelope.Data.Result))
	for _, item := range envelope.Data.Result {
		if len(item.Value) != 2 {
			return nil, errors.New("Prometheus vector sample has invalid value tuple")
		}
		var timestamp float64
		var value string
		if err := json.Unmarshal(item.Value[0], &timestamp); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(item.Value[1], &value); err != nil {
			return nil, err
		}
		result = append(result, realPrometheusSample{Metric: item.Metric, Timestamp: timestamp, Value: value})
	}
	return result, nil
}

func (f *realPrometheusFixture) waitSample(ctx context.Context, expression, wantValue string) (realPrometheusSample, error) {
	for {
		samples, err := f.query(ctx, expression, nil)
		if err == nil && len(samples) == 1 && samples[0].Value == wantValue {
			return samples[0], nil
		}
		select {
		case <-ctx.Done():
			return realPrometheusSample{}, fmt.Errorf("wait for %s=%s: %w", expression, wantValue, ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (f *realPrometheusFixture) buildVersion(ctx context.Context) (string, map[string]any, error) {
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/api/v1/status/buildinfo", f.promPort)
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", nil, err
	}
	defer response.Body.Close()
	var envelope struct {
		Status string         `json:"status"`
		Data   map[string]any `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&envelope); err != nil {
		return "", nil, err
	}
	version, _ := envelope.Data["version"].(string)
	if response.StatusCode != http.StatusOK || envelope.Status != "success" || version == "" {
		return "", envelope.Data, errors.New("invalid Prometheus buildinfo response")
	}
	return version, envelope.Data, nil
}

func (f *realPrometheusFixture) mainPID(ctx context.Context) (int, error) {
	pid, err := f.mainPIDAllowZero(ctx)
	if err != nil {
		return 0, err
	}
	if pid <= 1 {
		return 0, fmt.Errorf("invalid owned MainPID %d", pid)
	}
	return pid, nil
}

func (f *realPrometheusFixture) mainPIDAllowZero(ctx context.Context) (int, error) {
	output, err := realPrometheusRun(ctx, "systemctl", "show", f.unit, "--property=MainPID", "--value")
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(output))
	if err != nil || pid < 0 || pid == 1 {
		return 0, fmt.Errorf("invalid owned MainPID %q", strings.TrimSpace(output))
	}
	return pid, nil
}

func (f *realPrometheusFixture) validateOwnedPID(pid int) error {
	if err := validateRealPrometheusOwnership(f.parentRoot, f.runRoot, f.namespace, f.unit); err != nil {
		return err
	}
	executable, err := filepath.EvalSymlinks(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
	if err != nil {
		return fmt.Errorf("resolve owned PID executable: %w", err)
	}
	binary, err := filepath.EvalSymlinks(f.binaryPath)
	if err != nil {
		return err
	}
	if executable != binary {
		return fmt.Errorf("PID %d executable %s is not owned binary %s", pid, executable, binary)
	}
	cgroup, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return fmt.Errorf("read owned PID cgroup: %w", err)
	}
	if !realPrometheusPIDInExactUnitCgroup(string(cgroup), f.unit) {
		return fmt.Errorf("PID %d cgroup does not identify owned unit %s", pid, f.unit)
	}
	return nil
}

func (f *realPrometheusFixture) verifyUnitOwnership() error {
	if err := validateRealPrometheusOwnership(f.parentRoot, f.runRoot, f.namespace, f.unit); err != nil {
		return err
	}
	unit, err := os.ReadFile(f.unitPath)
	if err != nil {
		return err
	}
	marker := fmt.Sprintf("# promlifecycle-real-acceptance schema=1 namespace=%s run_root=%s\n", f.namespace, f.runRoot)
	if !strings.HasPrefix(string(unit), marker) || !strings.Contains(string(unit), "\nExecStart="+f.binaryPath+" ") {
		return errors.New("unit file does not carry the exact owned marker and executable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, err := realPrometheusRun(ctx, "systemctl", "show", f.unit,
		"--property=FragmentPath", "--property=DropInPaths", "--property=ExecStart", "--property=ExecStartPre",
		"--property=ExecStartPost", "--property=ExecStop", "--property=ExecStopPost", "--property=ExecCondition", "--property=KillMode")
	if err != nil {
		return err
	}
	properties := map[string]string{}
	for _, line := range strings.Split(output, "\n") {
		key, value, found := strings.Cut(line, "=")
		if found {
			properties[key] = value
		}
	}
	if properties["FragmentPath"] != f.unitPath {
		return fmt.Errorf("unit fragment path %q is not owned file %q", properties["FragmentPath"], f.unitPath)
	}
	if properties["DropInPaths"] != "" {
		return fmt.Errorf("owned unit has unexpected drop-ins %q", properties["DropInPaths"])
	}
	for _, hook := range []string{"ExecStartPre", "ExecStartPost", "ExecStop", "ExecStopPost", "ExecCondition"} {
		if properties[hook] != "" {
			return fmt.Errorf("owned unit has unexpected %s hook", hook)
		}
	}
	if !realPrometheusEffectiveExecStartMatches(properties["ExecStart"], f.binaryPath) {
		return fmt.Errorf("effective ExecStart does not use owned binary: %q", properties["ExecStart"])
	}
	if properties["KillMode"] != "control-group" {
		return fmt.Errorf("owned unit KillMode=%q, want control-group", properties["KillMode"])
	}
	return nil
}

func (f *realPrometheusFixture) processEvidence(pid int) (map[string]any, error) {
	if err := f.validateOwnedPID(pid); err != nil {
		return nil, err
	}
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return nil, err
	}
	closeParen := strings.LastIndexByte(string(stat), ')')
	if closeParen < 0 {
		return nil, errors.New("owned PID stat has invalid shape")
	}
	fields := strings.Fields(string(stat)[closeParen+1:])
	if len(fields) <= 19 {
		return nil, errors.New("owned PID stat has invalid shape")
	}
	cgroup, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return nil, err
	}
	executable, _ := filepath.EvalSymlinks(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
	return map[string]any{"pid": pid, "start_time_ticks": fields[19], "cgroup": strings.TrimSpace(string(cgroup)), "executable": executable}, nil
}

func (f *realPrometheusFixture) checkpoint() error {
	return f.checkpointWithWriter(writeRealPrometheusAtomicFile)
}

func (f *realPrometheusFixture) checkpointWithWriter(writer func(string, []byte, os.FileMode) error) error {
	f.evidence.CompletedAt = time.Now().UTC()
	data, err := json.MarshalIndent(f.evidence, "", "  ")
	if err != nil {
		return err
	}
	return writer(f.evidencePath, append(data, '\n'), 0600)
}

func (f *realPrometheusFixture) requireCheckpoint() {
	f.t.Helper()
	if err := f.checkpoint(); err != nil {
		f.t.Fatalf("persist real Prometheus evidence: %v", err)
	}
}
func (f *realPrometheusFixture) cleanup() {
	if f.cleaned {
		return
	}
	f.cleaned = true
	testAlreadyFailed := f.t.Failed()
	cleanup := map[string]any{
		"unit": f.unit, "run_root": f.runRoot, "started_at": time.Now().UTC(), "runtime_removed": false,
	}
	var cleanupErrors []string
	addError := func(stage string, err error) {
		if err != nil {
			cleanupErrors = append(cleanupErrors, stage+": "+err.Error())
		}
	}
	if f.metricServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		addError("metric fixture shutdown", f.metricServer.Shutdown(ctx))
		cancel()
	}
	if f.provisionalUnit != nil {
		addError("provisional unit cleanup", f.removeProvisionalUnitFile())
	}
	if f.unitInstalled {
		journalCtx, journalCancel := context.WithTimeout(context.Background(), 10*time.Second)
		journal, journalErr := realPrometheusRun(journalCtx, "journalctl", "-u", f.unit, "--no-pager", "-n", "500")
		journalCancel()
		addError("journal capture", journalErr)
		addError("journal log persistence", writeRealPrometheusAtomicFile(filepath.Join(f.logsPath, "journal.log"), []byte(journal), 0600))
		unit, unitErr := os.ReadFile(f.unitPath)
		addError("unit capture", unitErr)
		if unitErr == nil {
			addError("unit log persistence", writeRealPrometheusAtomicFile(filepath.Join(f.logsPath, "unit.service"), unit, 0600))
		}
		stopState, stopErr := f.stopOwnedUnit()
		if stopErr != nil {
			addError("stop verification", stopErr)
		} else if err := os.Remove(f.unitPath); err != nil {
			addError("remove unit", err)
		} else {
			reloadCtx, reloadCancel := context.WithTimeout(context.Background(), 10*time.Second)
			reloadOutput, reloadErr := realPrometheusRun(reloadCtx, "systemctl", "daemon-reload")
			reloadCancel()
			if reloadErr != nil {
				addError("daemon-reload", fmt.Errorf("%w; output=%s", reloadErr, strings.TrimSpace(reloadOutput)))
			} else {
				addError("reset failed unit", resetRealPrometheusFailedUnit(stopState, f.unit, func(unit string) error {
					resetCtx, resetCancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer resetCancel()
					output, err := realPrometheusRun(resetCtx, "systemctl", "reset-failed", unit)
					if err != nil {
						return fmt.Errorf("%w; output=%s", err, strings.TrimSpace(output))
					}
					return nil
				}))
			}
		}
	}
	if f.db != nil {
		if sqlDB, err := f.db.DB(); err != nil {
			addError("database handle", err)
		} else {
			addError("database close", sqlDB.Close())
		}
	}
	refreshEvidence := func(terminal bool) {
		unitFileAbsent := func() bool { _, err := os.Lstat(f.unitPath); return os.IsNotExist(err) }()
		updateRealPrometheusCleanupEvidence(f, cleanup, cleanupErrors, testAlreadyFailed, terminal)
		cleanup["unit_file_absent"] = unitFileAbsent
		f.evidence.Cleanup = cleanup
	}
	writeCleanupLog := func() error {
		data, err := json.MarshalIndent(cleanup, "", "  ")
		if err != nil {
			return err
		}
		return writeRealPrometheusAtomicFile(filepath.Join(f.logsPath, "cleanup.json"), append(data, '\n'), 0600)
	}
	refreshEvidence(false)
	addError("cleanup log persistence", writeCleanupLog())
	refreshEvidence(false)
	evidenceReady := true
	if err := f.checkpoint(); err != nil {
		addError("evidence persistence", err)
		evidenceReady = false
	}
	if evidenceReady && !testAlreadyFailed && len(cleanupErrors) == 0 {
		if err := validateRealPrometheusOwnership(f.parentRoot, f.runRoot, f.namespace, f.unit); err != nil {
			addError("runtime ownership", err)
		} else if err := os.RemoveAll(f.runRoot); err != nil {
			addError("remove runtime", err)
		} else {
			cleanup["runtime_removed"] = true
		}
	}
	refreshEvidence(true)
	addError("final cleanup log persistence", writeCleanupLog())
	refreshEvidence(true)
	if err := f.checkpoint(); err != nil {
		addError("final evidence persistence", err)
		refreshEvidence(true)
	}
	if len(cleanupErrors) != 0 {
		f.t.Errorf("real Prometheus owned cleanup failed: %s", strings.Join(cleanupErrors, "; "))
	}
}

func updateRealPrometheusCleanupEvidence(f *realPrometheusFixture, cleanup map[string]any, cleanupErrors []string, testAlreadyFailed, terminal bool) {
	cleanup["errors"] = cleanupErrors
	if terminal {
		if _, exists := cleanup["completed_at"]; !exists {
			cleanup["completed_at"] = time.Now().UTC()
		}
	} else {
		delete(cleanup, "completed_at")
	}
	f.evidence.Cleanup = cleanup
	f.evidence.Status = "cleanup_pending"
	if !terminal {
		return
	}
	f.evidence.Status = "passed"
	runtimeRemoved, _ := cleanup["runtime_removed"].(bool)
	if testAlreadyFailed || len(cleanupErrors) != 0 || !runtimeRemoved {
		f.evidence.Status = "failed"
	}
}

func resetRealPrometheusFailedUnit(state, unit string, reset func(string) error) error {
	if state != "failed" {
		return nil
	}
	return reset(unit)
}

func realPrometheusRun(ctx context.Context, name string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, name, args...)
	output, err := command.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return string(output), nil
}

func realPrometheusFileSHA256(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}

func copyRealPrometheusFile(source, destination string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	return errors.Join(copyErr, closeErr)
}

func extractRealPrometheusArchiveFile(archive, baseName, destination string) error {
	file, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gzipReader.Close()
	reader := tar.NewReader(gzipReader)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("%s missing from archive", baseName)
		}
		if err != nil {
			return err
		}
		if header.Typeflag != tar.TypeReg || filepath.Base(filepath.Clean(header.Name)) != baseName {
			continue
		}
		if header.Size <= 0 || header.Size > 1<<30 {
			return fmt.Errorf("unsafe %s archive entry size", baseName)
		}
		output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0755)
		if err != nil {
			return err
		}
		_, copyErr := io.CopyN(output, reader, header.Size)
		closeErr := output.Close()
		return errors.Join(copyErr, closeErr)
	}
}

func flipRealPrometheusArchiveByte(path string) (int64, byte, error) {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return 0, 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() < 2 {
		return 0, 0, errors.New("backup archive is too small to corrupt safely")
	}
	offset := info.Size() / 2
	var value [1]byte
	if _, err := file.ReadAt(value[:], offset); err != nil {
		return 0, 0, err
	}
	changed := []byte{value[0] ^ 0xff}
	if _, err := file.WriteAt(changed, offset); err != nil {
		return 0, 0, err
	}
	return offset, value[0], file.Sync()
}

func restoreRealPrometheusArchiveByte(path string, offset int64, value byte) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.WriteAt([]byte{value}, offset); err != nil {
		return err
	}
	return file.Sync()
}

func equalRealPrometheusSamples(left, right []realPrometheusSample) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i].Timestamp != right[i].Timestamp || left[i].Value != right[i].Value || len(left[i].Metric) != len(right[i].Metric) {
			return false
		}
		for key, value := range left[i].Metric {
			if right[i].Metric[key] != value {
				return false
			}
		}
	}
	return true
}

func waitRealPrometheusPIDExit(pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("owned PID %d did not exit", pid)
}
func realPrometheusPIDInExactUnitCgroup(data, unit string) bool {
	suffix := "/" + unit + ".service"
	for _, line := range strings.Split(data, "\n") {
		fields := strings.SplitN(line, ":", 3)
		if len(fields) == 3 && strings.HasSuffix(strings.TrimSpace(fields[2]), suffix) {
			return true
		}
	}
	return false
}

func realPrometheusEffectiveExecStartMatches(value, binary string) bool {
	value = strings.TrimSpace(value)
	pathPrefix := "{ path=" + binary + " ;"
	if !strings.HasPrefix(value, pathPrefix) {
		return false
	}
	value = strings.TrimSpace(strings.TrimPrefix(value, pathPrefix))
	argvPrefix := "argv[]=" + binary
	if !strings.HasPrefix(value, argvPrefix) {
		return false
	}
	tail := strings.TrimPrefix(value, argvPrefix)
	return strings.HasPrefix(tail, " ") || strings.HasPrefix(tail, " ;")
}

func writeRealPrometheusAtomicFile(path string, data []byte, mode os.FileMode) error {
	return writeRealPrometheusAtomicFileWithRename(path, data, mode, os.Rename)
}

func writeRealPrometheusAtomicFileWithRename(path string, data []byte, mode os.FileMode, rename func(string, string) error) error {
	directory := filepath.Dir(path)
	file, err := os.CreateTemp(directory, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
		_ = os.Remove(temporary)
	}()
	if err := file.Chmod(mode); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	closed = true
	if err := rename(temporary, path); err != nil {
		return err
	}
	directoryHandle, err := os.Open(directory)
	if err != nil {
		return err
	}
	return errors.Join(directoryHandle.Sync(), directoryHandle.Close())
}

func captureRealPrometheusEmergencyRestoration(restore func() error, hash func() (string, error), wantSHA256 string) map[string]any {
	result := map[string]any{"attempted": true, "restored": false}
	if err := restore(); err != nil {
		result["error"] = err.Error()
		return result
	}
	restoredSHA256, err := hash()
	if err != nil {
		result["error"] = fmt.Sprintf("hash restored artifact: %v", err)
		return result
	}
	result["restored_sha256"] = restoredSHA256
	if restoredSHA256 != wantSHA256 {
		result["error"] = fmt.Sprintf("restored artifact SHA256=%s, want %s", restoredSHA256, wantSHA256)
		return result
	}
	result["restored"] = true
	return result
}

func (f *realPrometheusFixture) stopOwnedUnit() (string, error) {
	if err := f.verifyUnitOwnership(); err != nil {
		return "", fmt.Errorf("ownership: %w", err)
	}
	observation := realPrometheusStopObservation{}
	lookupCtx, lookupCancel := context.WithTimeout(context.Background(), 10*time.Second)
	observation.InitialMainPID, observation.InitialMainPIDErr = f.mainPIDAllowZero(lookupCtx)
	lookupCancel()
	if observation.InitialMainPIDErr != nil || observation.InitialMainPID == 1 {
		return "", validateRealPrometheusStopped(observation)
	}
	if observation.InitialMainPID > 1 {
		if err := f.validateOwnedPID(observation.InitialMainPID); err != nil {
			return "", fmt.Errorf("initial PID ownership: %w", err)
		}
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
	stopOutput, stopErr := realPrometheusRun(stopCtx, "systemctl", "stop", f.unit)
	stopCancel()
	if stopErr != nil {
		observation.StopErr = fmt.Errorf("%w; output=%s", stopErr, strings.TrimSpace(stopOutput))
		return "", validateRealPrometheusStopped(observation)
	}
	stateCtx, stateCancel := context.WithTimeout(context.Background(), 10*time.Second)
	activeOutput, activeErr := realPrometheusRun(stateCtx, "systemctl", "show", f.unit, "--property=ActiveState", "--value")
	stateCancel()
	observation.ActiveState = strings.TrimSpace(activeOutput)
	observation.ActiveStateErr = activeErr
	finalPIDCtx, finalPIDCancel := context.WithTimeout(context.Background(), 10*time.Second)
	observation.FinalMainPID, observation.FinalMainPIDErr = f.mainPIDAllowZero(finalPIDCtx)
	finalPIDCancel()
	if observation.InitialMainPID > 1 {
		observation.TrackedPIDExited = waitRealPrometheusPIDExit(observation.InitialMainPID, 15*time.Second) == nil
	}
	return observation.ActiveState, validateRealPrometheusStopped(observation)
}
