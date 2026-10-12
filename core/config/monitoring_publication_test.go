package config

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/promlifecycle/platform/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func monitoringTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "monitoring.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.Host{}, &models.Cluster{}, &models.Component{}, &models.ComponentConfig{}, &models.AlertRule{}, &models.RecordingRule{}, &models.ScrapeConfigModel{}, &models.RemoteWriteModel{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestMonitoringMembershipRetainsOutageUntilIntentionalStop(t *testing.T) {
	db := monitoringTestDB(t)
	node := models.Cluster{Name: "single-node"}
	if err := db.Create(&node).Error; err != nil {
		t.Fatal(err)
	}
	host := models.Host{Name: "h", IP: "127.0.0.1"}
	if err := db.Create(&host).Error; err != nil {
		t.Fatal(err)
	}
	component := models.Component{ClusterID: node.ID, HostID: host.ID, Name: "node-a", Type: models.ComponentNodeExporter, Status: models.StatusRunning, Port: 9100}
	if err := db.Create(&component).Error; err != nil {
		t.Fatal(err)
	}
	ac := NewAutoConfig(db, t.TempDir())
	for _, status := range []models.ComponentStatus{models.StatusRunning, models.StatusError, models.StatusDegraded, models.StatusStopped, "removed"} {
		if err := db.Model(&component).Update("status", status).Error; err != nil {
			t.Fatal(err)
		}
		jobs, err := ac.GenerateScrapeConfigs(node.ID)
		if err != nil {
			t.Fatal(err)
		}
		present := false
		for _, job := range jobs {
			if job.JobName == "node_exporter_node-a" {
				present = true
			}
		}
		if present != (status != models.StatusStopped && status != "removed") {
			t.Fatalf("status %s: monitored=%v", status, present)
		}
	}
}

func TestMonitoringBundleReloadReceiptRollbackAndDeleteRetry(t *testing.T) {
	// No native tools or services: the fake runtime is the reload authority.
	t.Setenv("PATH", t.TempDir())
	db := monitoringTestDB(t)
	base := t.TempDir()
	var reloads atomic.Int32
	var reject atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/-/reload" {
			http.NotFound(w, r)
			return
		}
		reloads.Add(1)
		if reject.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	node := models.Cluster{Name: "single-node"}
	if err := db.Create(&node).Error; err != nil {
		t.Fatal(err)
	}
	host := models.Host{Name: "h", IP: "127.0.0.1"}
	if err := db.Create(&host).Error; err != nil {
		t.Fatal(err)
	}
	prom := models.Component{ClusterID: node.ID, HostID: host.ID, Name: "prom", Type: models.ComponentPrometheus, Status: models.StatusRunning, DeployType: "docker", ExternalURL: server.URL}
	if err := db.Create(&prom).Error; err != nil {
		t.Fatal(err)
	}
	rule := models.AlertRule{ClusterID: node.ID, Name: "Down", Expression: "up == 0", Enabled: true}
	if err := db.Create(&rule).Error; err != nil {
		t.Fatal(err)
	}
	ac := NewAutoConfig(db, base)
	if err := ac.SyncPrometheusConfig(node.ID, base); err != nil {
		t.Fatal(err)
	}
	if reloads.Load() != 1 {
		t.Fatalf("initial snapshot reloaded %d times", reloads.Load())
	}
	if err := ac.SyncPrometheusConfig(node.ID, base); err != nil {
		t.Fatal(err)
	}
	if reloads.Load() != 1 {
		t.Fatalf("unchanged snapshot reloaded: %d", reloads.Load())
	}
	rulePath := filepath.Join(base, "configs", node.Name, prom.Name, "rules", "rules.yml")
	old, err := os.ReadFile(rulePath)
	if err != nil {
		t.Fatal(err)
	}
	reject.Store(true)
	if err := db.Model(&rule).Updates(map[string]interface{}{"enabled": false, "delete_pending": true, "apply_status": "pending"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := ac.SyncPrometheusConfig(node.ID, base); err == nil {
		t.Fatal("failed reload reported success")
	}
	var retained models.AlertRule
	if err := db.First(&retained, rule.ID).Error; err != nil {
		t.Fatalf("failed deletion lost row: %v", err)
	}
	if !retained.DeletePending || retained.ApplyStatus != "error" || retained.LastError == "" {
		t.Fatalf("failed deletion not tracked: %#v", retained.PublicationState)
	}
	restored, err := os.ReadFile(rulePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(old, restored) {
		t.Fatal("failed snapshot did not restore old rules")
	}
	reject.Store(false)
	if err := ac.SyncPrometheusConfig(node.ID, base); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&models.AlertRule{}, rule.ID).Error; !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("acknowledged deletion remains: %v", err)
	}
}

func TestManualPrometheusOwnershipNeverClaimsApplied(t *testing.T) {
	db := monitoringTestDB(t)
	base := t.TempDir()
	node := models.Cluster{Name: "single-node"}
	if err := db.Create(&node).Error; err != nil {
		t.Fatal(err)
	}
	prom := models.Component{ClusterID: node.ID, Name: "prom", Type: models.ComponentPrometheus, Status: models.StatusRunning}
	if err := db.Create(&prom).Error; err != nil {
		t.Fatal(err)
	}
	manual := models.ComponentConfig{ComponentID: prom.ID, Name: "prometheus.yml", IsActive: true}
	if err := db.Create(&manual).Error; err != nil {
		t.Fatal(err)
	}
	rule := models.AlertRule{ClusterID: node.ID, Name: "Down", Expression: "up == 0", Enabled: true}
	if err := db.Create(&rule).Error; err != nil {
		t.Fatal(err)
	}
	if err := NewAutoConfig(db, base).SyncPrometheusConfig(node.ID, base); !errors.Is(err, ErrManualPrometheusConfig) {
		t.Fatalf("manual ownership error=%v", err)
	}
	if err := db.First(&rule, rule.ID).Error; err != nil {
		t.Fatal(err)
	}
	if rule.ApplyStatus != "manual" || rule.AppliedAt != nil {
		t.Fatalf("manual desired state falsely applied: %#v", rule.PublicationState)
	}
}
