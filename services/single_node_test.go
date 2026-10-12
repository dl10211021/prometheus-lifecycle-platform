package services

import (
	"testing"

	"github.com/promlifecycle/platform/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestMigrateToSingleNodePreservesScopedData(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:single-node-migration?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	if err := db.AutoMigrate(
		&models.Cluster{},
		&models.Component{},
		&models.AlertRule{},
		&models.AlertHistory{},
		&models.ScrapeConfigModel{},
		&models.RecordingRule{},
		&models.RemoteWriteModel{},
		&models.AlertmanagerRoute{},
		&models.AlertSilence{},
		&models.Dashboard{},
		&models.AlertmanagerSettings{},
		&models.Operation{},
		&models.Backup{},
		&models.NotificationChannel{},
		&models.ScheduledBackup{},
		&models.MaintenanceWindow{},
	); err != nil {
		t.Fatalf("migrate schema: %v", err)
	}

	first := models.Cluster{Name: "production", Status: "active"}
	second := models.Cluster{Name: "testing", Status: "active"}
	if err := db.Create(&first).Error; err != nil {
		t.Fatalf("create first scope: %v", err)
	}
	if err := db.Create(&second).Error; err != nil {
		t.Fatalf("create second scope: %v", err)
	}

	component := models.Component{ClusterID: second.ID, Name: "prometheus-test", Type: models.ComponentPrometheus}
	rule := models.AlertRule{ClusterID: second.ID, Name: "NodeDown", Expression: "up == 0"}
	dashboard := models.Dashboard{ClusterID: second.ID, Name: "Overview", Content: "{}"}
	backupClusterID := second.ID
	backup := models.Backup{ClusterID: &backupClusterID, Name: "config-backup", Path: "/tmp/config-backup"}
	channelClusterID := second.ID
	channel := models.NotificationChannel{ClusterID: &channelClusterID, Name: "ops", Type: "webhook"}
	globalChannel := models.NotificationChannel{Name: "global", Type: "webhook"}
	for name, value := range map[string]interface{}{
		"component":      &component,
		"alert rule":     &rule,
		"dashboard":      &dashboard,
		"backup":         &backup,
		"channel":        &channel,
		"global channel": &globalChannel,
	} {
		if err := db.Create(value).Error; err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}

	if err := MigrateToSingleNode(db); err != nil {
		t.Fatalf("migrate to single node: %v", err)
	}
	if err := MigrateToSingleNode(db); err != nil {
		t.Fatalf("repeat migration must be idempotent: %v", err)
	}

	node, err := EnsureSingleNode(db)
	if err != nil {
		t.Fatalf("load singleton: %v", err)
	}
	if node.ID != first.ID || node.Name != singleNodeName {
		t.Fatalf("unexpected singleton: id=%d name=%q", node.ID, node.Name)
	}
	var clusterCount int64
	if err := db.Model(&models.Cluster{}).Count(&clusterCount).Error; err != nil {
		t.Fatalf("count scopes: %v", err)
	}
	if clusterCount != 1 {
		t.Fatalf("expected one internal scope, got %d", clusterCount)
	}

	assertRequiredScope := func(name string, model interface{}, id uint) {
		t.Helper()
		if err := db.First(model, id).Error; err != nil {
			t.Fatalf("reload %s: %v", name, err)
		}
		var scoped struct{ ClusterID uint }
		if err := db.Model(model).Select("cluster_id").First(&scoped, id).Error; err != nil {
			t.Fatalf("read %s scope: %v", name, err)
		}
		if scoped.ClusterID != node.ID {
			t.Fatalf("%s retained cluster_id=%d, want %d", name, scoped.ClusterID, node.ID)
		}
	}
	assertRequiredScope("component", &models.Component{}, component.ID)
	assertRequiredScope("alert rule", &models.AlertRule{}, rule.ID)
	assertRequiredScope("dashboard", &models.Dashboard{}, dashboard.ID)

	var migratedBackup models.Backup
	if err := db.First(&migratedBackup, backup.ID).Error; err != nil || migratedBackup.ClusterID == nil || *migratedBackup.ClusterID != node.ID {
		t.Fatalf("backup not migrated: cluster=%v err=%v", migratedBackup.ClusterID, err)
	}
	var migratedChannel models.NotificationChannel
	if err := db.First(&migratedChannel, channel.ID).Error; err != nil || migratedChannel.ClusterID == nil || *migratedChannel.ClusterID != node.ID {
		t.Fatalf("channel not migrated: cluster=%v err=%v", migratedChannel.ClusterID, err)
	}
	var preservedGlobal models.NotificationChannel
	if err := db.First(&preservedGlobal, globalChannel.ID).Error; err != nil {
		t.Fatalf("reload global channel: %v", err)
	}
	if preservedGlobal.ClusterID != nil {
		t.Fatalf("global channel should remain unscoped, got %v", *preservedGlobal.ClusterID)
	}
}
