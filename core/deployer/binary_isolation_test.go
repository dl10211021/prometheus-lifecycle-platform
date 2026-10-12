package deployer

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/promlifecycle/platform/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestDeployPersistsSelectedBinaryRuntimeIdentity(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "components.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.Component{}); err != nil {
		t.Fatal(err)
	}
	installPath := "/opt/promlifecycle/components/prometheus-runtime-identity-test"
	component := models.Component{
		Type: models.ComponentPrometheus, Name: "runtime-identity-test", HostID: 991,
		Host:    models.Host{ID: 991, IP: "127.0.0.1", ConnectionType: "local"},
		Version: "2.55.0", DeployType: "binary", Port: 19098,
		InstallPath: installPath, DataPathOverride: "/var/lib/promlifecycle/runtime-identity-test",
	}
	if err := db.Create(&component).Error; err != nil {
		t.Fatal(err)
	}

	// /proc is intentionally unwritable, so Deploy stops immediately after it
	// records the selected runtime identity and before any installation occurs.
	err = NewBinaryDeployer(db, "/proc/promlifecycle-runtime-identity-test").Deploy(context.Background(), &component)
	if err == nil || !strings.Contains(err.Error(), "create bin cache dir") {
		t.Fatalf("deployment did not reach the post-identity package boundary: %v", err)
	}
	var persisted models.Component
	if err := db.First(&persisted, component.ID).Error; err != nil {
		t.Fatal(err)
	}
	if want := installPath + "/prometheus"; persisted.BinaryPath != want {
		t.Fatalf("persisted binary path=%q, want selected unit executable %q", persisted.BinaryPath, want)
	}
}

func TestInstancePathsRemainIsolatedAfterRecording(t *testing.T) {
	a := &models.Component{ID: 71, Type: models.ComponentPrometheus}
	b := &models.Component{ID: 72, Type: models.ComponentPrometheus}
	sa, err := binarySpecForComponent(a)
	if err != nil {
		t.Fatal(err)
	}
	sb, err := binarySpecForComponent(b)
	if err != nil {
		t.Fatal(err)
	}
	if pathsOverlap(sa.ConfigDir, sb.ConfigDir) || pathsOverlap(installDirFor(sa, a), installDirFor(sb, b)) || pathsOverlap(effectiveDataDir(sa, a), effectiveDataDir(sb, b)) || serviceUnit(sa, a) == serviceUnit(sb, b) {
		t.Fatal("instances share mutable artifacts")
	}
	args := strings.Join(effectiveArgs(sa, a), " ")
	if !strings.Contains(args, "--config.file="+filepath.Join(sa.ConfigDir, sa.ConfigFile)) || !strings.Contains(args, "--storage.tsdb.path="+effectiveDataDir(sa, a)) {
		t.Fatalf("runtime does not use isolated paths: %s", args)
	}
	original := systemdUnit(sa, a)
	a.InstallPath = installDirFor(sa, a)
	a.DataPath = effectiveDataDir(sa, a)
	a.ConfigPath = filepath.Join(sa.ConfigDir, sa.ConfigFile)
	a.ServiceName = serviceUnit(sa, a)
	now := time.Now()
	a.LastDeployedAt = &now
	after, err := binarySpecForComponent(a)
	if err != nil {
		t.Fatal(err)
	}
	if got := systemdUnit(after, a); got != original {
		t.Fatalf("recording/deployment changes runtime paths:\n%s\n%s", original, got)
	}
	if !strings.Contains(defaultConfigForComponent(a), filepath.Join(sa.ConfigDir, "rules")) {
		t.Fatal("rules were left in a shared directory")
	}
}

func TestAdoptedPathsAndArgumentsSurviveUnitRegeneration(t *testing.T) {
	argv := []string{"--config.file", "/srv/external/custom.yml", "--storage.tsdb.path=/srv/external/data", "--web.listen-address=127.0.0.1:19090", "--query.timeout", "7m"}
	raw, _ := json.Marshal(argv)
	component := &models.Component{ID: 80, Type: models.ComponentPrometheus, Port: 19090, ConfigPath: "/srv/external/custom.yml", DataPath: "/srv/external/data", InstallPath: "/usr/local/bin", BinaryPath: "/usr/local/bin/prometheus-custom", ServiceName: "external-prometheus.service", ImportedArgs: string(raw)}
	spec, err := binarySpecForComponent(component)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Join(spec.ConfigDir, spec.ConfigFile) != component.ConfigPath || effectiveDataDir(spec, component) != component.DataPath || systemdExecPath(spec, component) != component.BinaryPath {
		t.Fatal("adopted paths relocated")
	}
	if got := effectiveArgs(spec, component); !reflect.DeepEqual(got, argv) {
		t.Fatalf("adopted runtime changed: %#v", got)
	}
	if got := serviceUnit(spec, component) + ".service"; got != "external-prometheus.service" {
		t.Fatalf("duplicate service suffix: %s", got)
	}
	component.CustomArgs = "--query.timeout=9m"
	if got := strings.Join(effectiveArgs(spec, component), " "); strings.Contains(got, "7m") || !strings.Contains(got, "--query.timeout=9m") {
		t.Fatalf("split imported flag was not safely overridden: %s", got)
	}
}

func TestRemoteTreeExtractionUsesExactCustomDestination(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "grafana-v11", "bin")
	if err := os.MkdirAll(source, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "grafana"), []byte("fixture"), 0755); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(root, "grafana.tar.gz")
	if out, err := exec.Command("tar", "czf", archive, "-C", root, "grafana-v11").CombinedOutput(); err != nil {
		t.Fatalf("tar: %s: %v", out, err)
	}
	target := filepath.Join(root, "monitoring", "grafana-prod")
	if out, err := exec.Command("sh", "-c", extractTreeCommand(archive, target)).CombinedOutput(); err != nil {
		t.Fatalf("extract: %s: %v", out, err)
	}
	if data, err := os.ReadFile(filepath.Join(target, "bin", "grafana")); err != nil || string(data) != "fixture" {
		t.Fatalf("custom installation was not populated: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(target), "grafana")); !os.IsNotExist(err) {
		t.Fatal("extracted a wrong sibling basename")
	}
}

func TestRestartWaitsForActualReadiness(t *testing.T) {
	// No real systemd action: only the command transport is replaced; HTTP
	// readiness uses a real local server and the production Restart path.
	commands := t.TempDir()
	if err := os.WriteFile(filepath.Join(commands, "systemctl"), []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", commands)
	var requests atomic.Int32
	var ready atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/-/ready" || !ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	_, portText, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	port, _ := strconv.Atoi(portText)
	component := &models.Component{Type: models.ComponentPrometheus, Port: port, ServiceName: "fixture.service", Host: models.Host{IP: "127.0.0.1", ConnectionType: "local"}}
	d := &BinaryDeployer{}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	if err := d.Restart(ctx, component); err == nil {
		t.Fatal("restart succeeded while the actual readiness endpoint failed")
	}
	ready.Store(true)
	requests.Store(0)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	if err := d.Restart(ctx2, component); err != nil {
		t.Fatal(err)
	}
	if requests.Load() < 2 {
		t.Fatal("restart accepted a single transient readiness response")
	}
}
