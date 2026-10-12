package services

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/promlifecycle/platform/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestDiscoveryDeletionRejectsSharedExecutableAliases(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "node_exporter")
	alias := filepath.Join(root, "exporter-alias")
	if err := os.WriteFile(binary, []byte("fixture"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(binary, alias); err != nil {
		t.Fatal(err)
	}
	selected := DiscoveredComponent{Name: "external", Binary: binary, Unit: "external.service", Fingerprint: "selected"}
	for _, sibling := range []DiscoveredComponent{
		{Name: "discovered sibling", Binary: alias, Fingerprint: "sibling", Unit: "other.service"},
		{Name: "managed sibling", Binary: binary, Fingerprint: "managed", ComponentID: 9},
	} {
		if err := sharedDiscoveryReference(selected, []DiscoveredComponent{selected, sibling}); !errors.Is(err, ErrDiscoveredSharedArtifact) {
			t.Fatalf("shared executable was not blocked: %v", err)
		}
	}
	independent := DiscoveredComponent{Binary: filepath.Join(root, "independent", "node_exporter"), Fingerprint: "independent", Unit: "independent.service"}
	if err := sharedDiscoveryReference(selected, []DiscoveredComponent{selected, independent}); err != nil {
		t.Fatalf("isolated instances must remain supported: %v", err)
	}
	if _, err := os.Stat(binary); err != nil {
		t.Fatalf("safety check removed executable: %v", err)
	}
}

func TestOnboardingComponentLookupExcludesRetiredButRetainsUnhealthy(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "onboarding.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&models.Host{}, &models.Component{}); err != nil {
		t.Fatal(err)
	}
	component := models.Component{Name: "prometheus", Type: models.ComponentPrometheus, Status: models.StatusRunning}
	if err := db.Create(&component).Error; err != nil {
		t.Fatal(err)
	}
	service := NewOnboardingService(db, t.TempDir())
	for _, status := range []models.ComponentStatus{models.StatusRunning, "removed", models.StatusError, models.StatusDegraded} {
		if err := db.Model(&component).Update("status", status).Error; err != nil {
			t.Fatal(err)
		}
		found, err := service.runningComponent(component.Type)
		if status == "removed" {
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				t.Fatalf("retired component selected for onboarding checks: component=%#v err=%v", found, err)
			}
			var retained models.Component
			if err := db.First(&retained, component.ID).Error; err != nil {
				t.Fatalf("recovery metadata lost: %v", err)
			}
			if retained.Status != "removed" {
				t.Fatalf("retired metadata changed: %s", retained.Status)
			}
			continue
		}
		if err != nil || found.ID != component.ID || found.Status != status {
			t.Fatalf("active %s component unavailable for onboarding checks: component=%#v err=%v", status, found, err)
		}
	}
}
