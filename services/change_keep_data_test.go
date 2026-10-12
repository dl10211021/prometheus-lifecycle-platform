package services

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promlifecycle/platform/core/deployer"
	"github.com/promlifecycle/platform/models"
	"gorm.io/gorm"
)

func TestKeepDataRemovalRejectsOverlapBeforeRecoveryPointOrOperation(t *testing.T) {
	db := lifecycleQueueDB(t)
	if err := db.AutoMigrate(&models.Host{}, &models.Cluster{}, &models.Backup{}, &models.BackupManifest{}, &models.ComponentConfig{}, &models.ComponentVersion{}); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	host := models.Host{IP: "127.0.0.1", ConnectionType: "local"}
	if err := db.Create(&host).Error; err != nil {
		t.Fatal(err)
	}
	c := models.Component{Type: models.ComponentGrafana, Name: "keep-data-fixture", DeployType: "binary", HostID: host.ID, Host: host, Status: models.StatusRunning, InstallPath: filepath.Join(root, "grafana"), DataPath: filepath.Join(root, "grafana", "data"), ConfigPath: filepath.Join(root, "grafana.ini"), ServiceName: "plc-keep-data-fixture"}
	if err := os.MkdirAll(c.DataPath, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(c.DataPath, "customer.db")
	if err := os.WriteFile(marker, []byte("retained data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.ConfigPath, []byte("[paths]\ndata = "+c.DataPath), 0600); err != nil {
		t.Fatal(err)
	}
	if err := db.Omit("Host").Create(&c).Error; err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(removeChange{Mode: "keep_data"})
	plan := models.ChangePlan{Kind: models.ChangeKindRemove, ResourceType: "component", ResourceID: c.ID, Status: models.ChangeStatusPending, After: after}
	if err := db.Create(&plan).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.ChangeBaseline{PlanID: plan.ID, Fingerprint: componentFingerprint(&c)}).Error; err != nil {
		t.Fatal(err)
	}
	var backupCommands []string
	backups := NewBackupServiceWithRoot(db, filepath.Join(root, "backups"))
	backups.runnerFactory = func(*models.Component) (hostCommandRunner, error) {
		return backupControlRunner{before: func(_ context.Context, command string) error {
			backupCommands = append(backupCommands, command)
			return errors.New("fixture blocks unexpected backup host I/O")
		}}, nil
	}
	service := NewChangeService(db, deployer.NewFactory(root), nil, backups, root)
	service.incidents = nil
	result, err := service.Execute(context.Background(), plan.ID, ChangeActor{Role: "system_admin"})
	if err == nil || !strings.Contains(err.Error(), "keep_data") {
		t.Errorf("expected keep_data preflight rejection, got %v", err)
	}
	if result == nil || result.Status != models.ChangeStatusFailed || result.RolledBackAt != nil {
		t.Errorf("expected failed preflight without compensation: %#v", result)
	}
	if len(backupCommands) != 0 {
		t.Errorf("preflight reached recovery-point host I/O: %v", backupCommands)
	}
	var count int64
	if err := db.Model(&models.Operation{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("preflight created mutation operation: %d", count)
	}
	var stored models.Component
	if err := db.First(&stored, c.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != models.StatusRunning {
		t.Errorf("preflight changed component status: %s", stored.Status)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "retained data" {
		t.Errorf("data changed: %q %v", data, err)
	}
}

func TestKeepDataFinalAdmissionFailureDoesNotRedeploy(t *testing.T) {
	db := lifecycleQueueDB(t)
	if err := db.AutoMigrate(&models.Host{}, &models.Cluster{}); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	commandDir := filepath.Join(root, "commands")
	if err := os.Mkdir(commandDir, 0700); err != nil {
		t.Fatal(err)
	}
	commandLog := filepath.Join(root, "commands.log")
	for _, name := range []string{"systemctl", "docker"} {
		script := "#!/bin/sh\nprintf '%s\\n' \"$0 $*\" >> \"$KEEP_DATA_COMMAND_LOG\"\nexit 1\n"
		if err := os.WriteFile(filepath.Join(commandDir, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("KEEP_DATA_COMMAND_LOG", commandLog)
	t.Setenv("PATH", commandDir+":"+os.Getenv("PATH"))
	host := models.Host{IP: "127.0.0.1", ConnectionType: "local"}
	if err := db.Create(&host).Error; err != nil {
		t.Fatal(err)
	}
	install := filepath.Join(root, "install")
	data := filepath.Join(root, "retained")
	for _, path := range []string{install, data} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	binary := filepath.Join(install, "node_exporter")
	if err := os.WriteFile(binary, []byte("original binary"), 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(data, "marker")
	if err := os.WriteFile(marker, []byte("retained data"), 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "data-alias")
	if err := os.Symlink(data, alias); err != nil {
		t.Fatal(err)
	}
	c := models.Component{Type: models.ComponentNodeExporter, Name: "keep-data-race-fixture", DeployType: "binary", HostID: host.ID, Host: host, Status: models.StatusRunning, Version: "fixture", InstallPath: install, DataPath: alias, ServiceName: "plc-keep-data-race-fixture"}
	if err := db.Omit("Host").Create(&c).Error; err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(removeChange{Mode: "keep_data"})
	plan := models.ChangePlan{Kind: models.ChangeKindRemove, ResourceType: "component", ResourceID: c.ID, Status: models.ChangeStatusPending, After: after}
	if err := db.Create(&plan).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.ChangeBaseline{PlanID: plan.ID, Fingerprint: componentFingerprint(&c)}).Error; err != nil {
		t.Fatal(err)
	}
	retargeted := false
	if err := db.Callback().Create().Before("gorm:create").Register("fixture:retarget-data", func(tx *gorm.DB) {
		if tx.Statement.Schema == nil || tx.Statement.Schema.Name != "Operation" {
			return
		}
		if err := os.Remove(alias); err != nil {
			tx.AddError(err)
			return
		}
		if err := os.Symlink(install, alias); err != nil {
			tx.AddError(err)
			return
		}
		retargeted = true
	}); err != nil {
		t.Fatal(err)
	}
	service := NewChangeService(db, deployer.NewFactory(root), nil, NewBackupServiceWithRoot(db, filepath.Join(root, "backups")), root)
	service.incidents = nil
	result, err := service.Execute(context.Background(), plan.ID, ChangeActor{Role: "system_admin"})
	if !retargeted {
		t.Fatal("fixture never reached final admission boundary")
	}
	if !errors.Is(err, deployer.ErrKeepDataUnsafe) {
		t.Errorf("final admission error lost: %v", err)
	}
	if result == nil || result.Status != models.ChangeStatusFailed || result.RolledBackAt != nil {
		t.Errorf("unsafe final admission triggered compensation: %#v", result)
	}
	if commands, _ := os.ReadFile(commandLog); len(commands) != 0 {
		t.Errorf("unsafe final admission triggered runtime command: %s", commands)
	}
	for path, want := range map[string]string{binary: "original binary", marker: "retained data"} {
		if data, err := os.ReadFile(path); err != nil || string(data) != want {
			t.Errorf("retained path changed: %s: %q %v", path, data, err)
		}
	}
	var op models.Operation
	if err := db.First(&op, "component_id = ?", c.ID).Error; err != nil {
		t.Fatal(err)
	}
	if op.Status != "failed" {
		t.Errorf("operation not failed: %s", op.Status)
	}
}
