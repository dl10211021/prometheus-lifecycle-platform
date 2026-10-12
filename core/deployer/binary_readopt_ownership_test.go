package deployer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promlifecycle/platform/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func readoptOwnershipFixture(t *testing.T) (*BinaryDeployer, *models.Component, string, string) {
	t.Helper()
	d, component, root, commandLog := keepDataFixture(t)
	db, err := gorm.Open(sqlite.Open(filepath.Join(root, "ownership.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.Component{}); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	component.HostID = 77
	component.Name = "readopted"
	component.Status = models.StatusRunning
	component.BinaryPath = filepath.Join(component.InstallPath, "bin", "grafana")
	component.DataPath = filepath.Join(root, "retained-data")
	if err := os.Mkdir(component.DataPath, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(component.DataPath, "customer.db"), []byte("retained data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := db.Omit("Host").Create(component).Error; err != nil {
		t.Fatal(err)
	}
	d.db = db
	return d, component, root, commandLog
}

func TestReadoptedBinaryRemovalIgnoresRemovedHistory(t *testing.T) {
	d, component, _, commandLog := readoptOwnershipFixture(t)
	retired := *component
	retired.ID = 0
	retired.Name = "released-history"
	retired.Status = "removed"
	if err := d.db.Omit("Host").Create(&retired).Error; err != nil {
		t.Fatal(err)
	}
	if err := d.RemoveKeepData(context.Background(), component); err != nil {
		t.Fatalf("historical removed owner blocked readopted removal: %v", err)
	}
	if _, err := os.Stat(component.InstallPath); !os.IsNotExist(err) {
		t.Errorf("readopted installation not removed: %v", err)
	}
	if marker, err := os.ReadFile(filepath.Join(component.DataPath, "customer.db")); err != nil || string(marker) != "retained data" {
		t.Errorf("retained data changed: %q %v", marker, err)
	}
	if commands, _ := os.ReadFile(commandLog); !strings.Contains(string(commands), "stop "+component.ServiceName) {
		t.Errorf("removal never reached fixture service: %s", commands)
	}
	var stored models.Component
	if err := d.db.First(&stored, retired.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != "removed" || stored.ServiceName != retired.ServiceName || stored.BinaryPath != retired.BinaryPath || stored.DataPath != retired.DataPath {
		t.Errorf("removal rewrote retired history: %#v", stored)
	}
}

func TestBinaryOwnershipKeepsEveryNonRemovedStateProtected(t *testing.T) {
	statuses := []models.ComponentStatus{models.StatusPending, models.StatusDeploying, models.StatusRunning, models.StatusDegraded, models.StatusStopping, models.StatusStopped, models.StatusError, models.StatusUpgrading, models.StatusRollingBack, models.StatusConfiguring, "unknown", "", "null"}
	for _, status := range statuses {
		for _, overlap := range []string{"unit", "binary", "data", "config"} {
			t.Run(string(status)+"/"+overlap, func(t *testing.T) {
				d, component, root, commandLog := readoptOwnershipFixture(t)
				owner := models.Component{Type: models.ComponentNodeExporter, Name: "retained-owner", HostID: component.HostID, DeployType: "binary", ServiceName: "other-owner-" + filepath.Base(filepath.Dir(root)), InstallPath: filepath.Join(root, "other-install"), BinaryPath: filepath.Join(root, "other-install", "node_exporter"), DataPath: filepath.Join(root, "other-data"), ConfigPath: filepath.Join(root, "other-config", "config.yml"), Status: status}
				switch overlap {
				case "unit":
					owner.ServiceName = component.ServiceName
				case "binary":
					owner.BinaryPath = component.BinaryPath
				case "data":
					owner.DataPath = component.DataPath
				case "config":
					owner.Type = models.ComponentPrometheus
					owner.ConfigPath = component.ConfigPath
				}
				if err := d.db.Omit("Host").Create(&owner).Error; err != nil {
					t.Fatal(err)
				}
				// Preserve explicit empty/NULL legacy values rather than GORM's pending default.
				if status == "" {
					if err := d.db.Model(&owner).Update("status", "").Error; err != nil {
						t.Fatal(err)
					}
				}
				if status == "null" {
					if err := d.db.Model(&owner).Update("status", nil).Error; err != nil {
						t.Fatal(err)
					}
				}
				err := d.RemoveKeepData(context.Background(), component)
				want := "overlaps component"
				if overlap == "unit" {
					want = "also owned by component"
				}
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("%s owner lost %s protection: %v", status, overlap, err)
				}
				if commands, _ := os.ReadFile(commandLog); len(commands) != 0 {
					t.Errorf("ownership rejection reached service mutation: %s", commands)
				}
				if _, err := os.Stat(component.BinaryPath); err != nil {
					t.Errorf("ownership rejection changed binary: %v", err)
				}
				if marker, err := os.ReadFile(filepath.Join(component.DataPath, "customer.db")); err != nil || string(marker) != "retained data" {
					t.Errorf("ownership rejection changed data: %q %v", marker, err)
				}
			})
		}
	}
}

func TestRemovedHistoryStillRequiresHostReferenceGuard(t *testing.T) {
	d, component, _, _ := readoptOwnershipFixture(t)
	retired := *component
	retired.ID = 0
	retired.Status = "removed"
	if err := d.db.Omit("Host").Create(&retired).Error; err != nil {
		t.Fatal(err)
	}
	host, commands := binaryMutationPeer(t, "for artifact in", 23)
	component.Host = host
	spec, err := binarySpecForComponent(component)
	if err != nil {
		t.Fatal(err)
	}
	err = d.guardBinaryArtifacts(context.Background(), component, spec)
	if err == nil || !strings.Contains(err.Error(), "shared artifact safety check failed") {
		t.Errorf("retired history bypassed host unit/process references: %v", err)
	}
	inspected := false
	for _, cmd := range commands() {
		inspected = inspected || strings.Contains(cmd, "for artifact in")
		if strings.Contains(cmd, "systemctl stop") || strings.Contains(cmd, "rm -") {
			t.Errorf("reference rejection dispatched mutation: %s", cmd)
		}
	}
	if !inspected {
		t.Error("host unit/process reference guard was not reached")
	}
}
