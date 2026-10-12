package config

import (
	"github.com/promlifecycle/platform/models"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAdoptedAlertmanagerPreservesManualConfiguration(t *testing.T) {
	db := monitoringTestDB(t)
	if err := db.AutoMigrate(&models.NotificationChannel{}, &models.AlertmanagerRoute{}, &models.AlertmanagerSettings{}); err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	node := models.Cluster{Name: "single-node"}
	if err := db.Create(&node).Error; err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(base, "original.yml")
	original := "route:\n  receiver: original\nreceivers:\n- name: original\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	host := models.Host{IP: "127.0.0.1", ConnectionType: "local"}
	if err := db.Create(&host).Error; err != nil {
		t.Fatal(err)
	}
	am := models.Component{ClusterID: node.ID, HostID: host.ID, Name: "existing-am", Type: models.ComponentAlertmanager, Status: models.StatusRunning, DeployType: "binary", ConfigPath: path}
	if err := db.Create(&am).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.ComponentConfig{ComponentID: am.ID, Name: "alertmanager.yml", IsActive: true, Content: original}).Error; err != nil {
		t.Fatal(err)
	}
	err := NewAutoConfig(db, base).SyncAlertmanagerConfig(node.ID, base)
	if err == nil || !strings.Contains(err.Error(), "手动") {
		t.Fatalf("manual ownership not respected: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != original {
		t.Fatalf("original alertmanager configuration changed: %v", err)
	}
}
