package config

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/promlifecycle/platform/models"
	"gorm.io/gorm"
)

func TestDashboardPublicationWaitsForGrafanaAndRetainsDelete(t *testing.T) {
	db := monitoringTestDB(t)
	if err := db.AutoMigrate(&models.Dashboard{}); err != nil {
		t.Fatal(err)
	}
	var visible atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok || user != "admin" || password != "test-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if !visible.Load() {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"dashboard":{"uid":"test-dashboard","title":"Test dashboard","version":7}}`))
	}))
	defer server.Close()
	host := models.Host{Name: "local", IP: "127.0.0.1", ConnectionType: "local"}
	if err := db.Create(&host).Error; err != nil {
		t.Fatal(err)
	}
	grafana := models.Component{ClusterID: 1, HostID: host.ID, Name: "grafana", Type: models.ComponentGrafana, Status: models.StatusRunning, DeployType: "binary", ConfigPath: filepath.Join(t.TempDir(), "grafana.ini"), ExternalURL: server.URL, CustomEnv: `{"GF_SECURITY_ADMIN_PASSWORD":"test-secret"}`}
	if err := db.Create(&grafana).Error; err != nil {
		t.Fatal(err)
	}
	dashboard := models.Dashboard{ClusterID: 1, Name: "Test dashboard", UID: "test-dashboard", Content: `{"uid":"test-dashboard","title":"Test dashboard"}`, Enabled: true}
	if err := db.Create(&dashboard).Error; err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	ac := NewAutoConfig(db, base)
	if err := ac.ProvisionGrafanaDashboards(1, base); !errors.Is(err, errDashboardAwaitingApply) {
		t.Fatalf("unobserved file falsely acknowledged: %v", err)
	}
	if err := db.First(&dashboard, dashboard.ID).Error; err != nil {
		t.Fatal(err)
	}
	if dashboard.ApplyStatus != "pending" || dashboard.AppliedAt != nil {
		t.Fatal("file write reported live success")
	}
	visible.Store(true)
	if err := ac.ProvisionGrafanaDashboards(1, base); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&dashboard).Updates(map[string]interface{}{"enabled": false, "delete_pending": true, "apply_status": "pending"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := ac.ProvisionGrafanaDashboards(1, base); !errors.Is(err, errDashboardAwaitingApply) {
		t.Fatalf("still-live deletion error=%v", err)
	}
	if err := db.First(&dashboard, dashboard.ID).Error; err != nil {
		t.Fatalf("unacknowledged deletion lost row: %v", err)
	}
	visible.Store(false)
	if err := ac.ProvisionGrafanaDashboards(1, base); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&models.Dashboard{}, dashboard.ID).Error; !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("confirmed absence did not finish deletion: %v", err)
	}
}
