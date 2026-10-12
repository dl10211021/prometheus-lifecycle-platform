package handlers

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/services"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestDiscoveryPreservesInstanceIdentityAndPrivateArguments(t *testing.T) {
	encode := func(value string) string { return base64.StdEncoding.EncodeToString([]byte(value)) }
	process := func(pid int, start string) string {
		stat := fmt.Sprintf("%d (prometheus) S %s %s", pid, strings.Repeat("0 ", 18), start)
		return fmt.Sprintf("PROC\t%d\t/opt/instance-%d/prometheus\t%s\t%s\t%s\n", pid, pid, encode("prometheus\x00--config.file\x00/opt/config with spaces/prometheus.yml\x00--private-token=CANARY\x00"), encode("0::/system.slice/prometheus-second.service\n"), encode(stat))
	}
	snapshot := "LISTENERS\nLISTEN 0 4096 127.0.0.1:19090 0.0.0.0:* users:((\"prometheus\",pid=101,fd=7))\nLISTEN 0 4096 [::]:29090 [::]:* users:((\"prometheus\",pid=102,fd=7))\nPROCESSES\n" + process(101, "1000") + process(102, "2000")
	rows := parseHostDiscovery(models.Host{ID: 7}, snapshot)
	if len(rows) != 2 || rows[0].component.Port != 19090 || rows[1].component.Port != 29090 {
		t.Fatalf("independent listeners lost: %+v", rows)
	}
	if rows[0].component.Fingerprint == rows[1].component.Fingerprint {
		t.Fatal("different instances share fingerprint")
	}
	if rows[0].component.Config != "/opt/config with spaces/prometheus.yml" {
		t.Fatalf("argv path lost: %+v", rows[0].component)
	}
	public, err := json.Marshal(rows[0].component)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(public), "CANARY") {
		t.Fatal("discovery exposed process credential")
	}
	changed := parseHostDiscovery(models.Host{ID: 7}, strings.Replace(snapshot, process(101, "1000"), process(101, "3000"), 1))
	if changed[0].component.Fingerprint == rows[0].component.Fingerprint {
		t.Fatal("PID reuse kept stale fingerprint")
	}
	if got := parseHostDiscovery(models.Host{ID: 7}, strings.ReplaceAll(snapshot, "/prometheus\t", "/unrelated\t")); len(got) != 0 {
		t.Fatal("unknown executable acquired monitoring identity")
	}
}

func TestForcedHostRemovalRetainsFailedItemsAndCanRetry(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "host.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if err := db.AutoMigrate(&models.Host{}, &models.Cluster{}, &models.Component{}, &models.HostOperationClaim{}, &models.ComponentOperationClaim{}, &services.ComponentBackupPause{}, &models.Backup{}, &models.ComponentConfig{}, &models.ResourceUsage{}, &models.InspectionNotificationState{}, &models.ComponentAdoption{}, &models.MaintenanceWindow{}, &models.ScheduledBackup{}, &models.ScheduledBackupStatus{}, &models.Operation{}, &models.OperationLog{}, &models.TopologyEdge{}); err != nil {
		t.Fatal(err)
	}
	host := models.Host{Name: "remote-owned-fixture", IP: "192.0.2.48", ConnectionType: "ssh"}
	if err := db.Create(&host).Error; err != nil {
		t.Fatal(err)
	}
	first := models.Component{Name: "observed", HostID: host.ID, Type: models.ComponentNodeExporter, DeployType: "target", Status: models.StatusRunning}
	second := models.Component{Name: "legacy", HostID: host.ID, Type: models.ComponentPrometheus, DeployType: "docker", Status: models.StatusRunning}
	for _, item := range []*models.Component{&first, &second} {
		if err := db.Create(item).Error; err != nil {
			t.Fatal(err)
		}
	}
	h := &Handler{db: db, basePath: t.TempDir()}
	r := gin.New()
	r.DELETE("/hosts/:id", h.DeleteHost)
	remove := func() int {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("DELETE", fmt.Sprintf("/hosts/%d?force=1", host.ID), nil))
		return w.Code
	}
	if status := remove(); status != 207 {
		t.Fatalf("partial removal status=%d", status)
	}
	if err := db.First(&host, host.ID).Error; err != nil {
		t.Fatal("failed cleanup removed host", err)
	}
	var count int64
	db.Model(&models.Component{}).Where("id = ?", first.ID).Count(&count)
	if count != 0 {
		t.Fatal("successful target-only release remained")
	}
	if err := db.First(&second, second.ID).Error; err != nil {
		t.Fatal("failed component metadata was lost", err)
	}
	if err := db.Model(&second).Update("status", "removed").Error; err != nil {
		t.Fatal(err)
	}
	if status := remove(); status != 200 {
		t.Fatalf("retry after external cleanup status=%d", status)
	}
	db.Model(&models.Host{}).Where("id = ?", host.ID).Count(&count)
	if count != 0 {
		t.Fatal("successful retry retained host")
	}
}
