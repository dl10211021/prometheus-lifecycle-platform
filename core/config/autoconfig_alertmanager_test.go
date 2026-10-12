package config

import (
	"path/filepath"
	"testing"

	"github.com/promlifecycle/platform/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestGenerateAlertmanagerConfigUsesDeploymentIntervals(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "alertmanager-config.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&models.Component{},
		&models.NotificationChannel{},
		&models.AlertmanagerRoute{},
		&models.AlertmanagerSettings{},
	); err != nil {
		t.Fatal(err)
	}
	componentConfig, err := models.EmbedDeploymentParameters("{}", map[string]interface{}{
		"resolve_timeout": "10m", "group_wait": "45s",
		"group_interval": "10m", "repeat_interval": "8h",
	})
	if err != nil {
		t.Fatal(err)
	}
	component := models.Component{
		ClusterID: 1, Type: models.ComponentAlertmanager,
		Name: "alertmanager", Status: models.StatusRunning, Config: componentConfig,
	}
	if err := db.Create(&component).Error; err != nil {
		t.Fatal(err)
	}

	generated, err := NewAutoConfig(db, t.TempDir()).GenerateAlertmanagerConfig(1)
	if err != nil {
		t.Fatal(err)
	}
	if generated.Global.ResolveTimeout != "10m" || generated.Route.GroupWait != "45s" ||
		generated.Route.GroupInterval != "10m" || generated.Route.RepeatInterval != "8h" {
		t.Fatalf("deployment intervals missing from generated config: %#v", generated)
	}

	settings := models.AlertmanagerSettings{ClusterID: 1, ResolveTimeout: "7m"}
	if err := db.Create(&settings).Error; err != nil {
		t.Fatal(err)
	}
	generated, err = NewAutoConfig(db, t.TempDir()).GenerateAlertmanagerConfig(1)
	if err != nil {
		t.Fatal(err)
	}
	if generated.Global.ResolveTimeout != "7m" {
		t.Fatalf("explicit Alertmanager settings must override deployment baseline: %#v", generated.Global)
	}
}
