package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/services"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestScheduledConfigBackupUsesRealConfigAndRecordsEvidence(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "scheduler.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	if err := db.AutoMigrate(
		&models.Cluster{}, &models.Host{}, &models.Component{},
		&models.ScheduledBackup{}, &models.Backup{}, &models.BackupManifest{},
		&models.ScheduledBackupStatus{}, &models.ComponentOperationClaim{},
		&services.ComponentBackupPause{},
	); err != nil {
		t.Fatalf("migrate scheduler models: %v", err)
	}
	backupRoot := filepath.Join(t.TempDir(), "backups")
	t.Setenv("BACKUP_ROOT", backupRoot)
	configPath := filepath.Join(t.TempDir(), "actual-prometheus.yml")
	configBody := []byte("global:\n  scrape_interval: 11s\n")
	if err := os.WriteFile(configPath, configBody, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cluster := models.Cluster{Name: "single", Status: "active"}
	if err := db.Create(&cluster).Error; err != nil {
		t.Fatalf("create cluster: %v", err)
	}
	host := models.Host{Name: "local", IP: "127.0.0.1", ConnectionType: "local"}
	if err := db.Create(&host).Error; err != nil {
		t.Fatalf("create host: %v", err)
	}
	component := models.Component{
		ClusterID: cluster.ID, HostID: host.ID, Name: "prometheus", Type: models.ComponentPrometheus,
		Version: "3.5.0", ConfigPath: configPath, DataPath: filepath.Join(t.TempDir(), "unrelated-data"),
	}
	if err := db.Create(&component).Error; err != nil {
		t.Fatalf("create component: %v", err)
	}
	schedule := models.ScheduledBackup{Name: "config-only", Cron: "* * * * *", ComponentID: &component.ID, Type: "config", Enabled: true, KeepCount: 2}
	if err := db.Create(&schedule).Error; err != nil {
		t.Fatalf("create schedule: %v", err)
	}

	NewScheduler(db, t.TempDir()).doBackup(context.Background(), &schedule)

	var backup models.Backup
	if err := db.Where("component_id = ?", component.ID).First(&backup).Error; err != nil {
		t.Fatalf("load scheduled backup: %v", err)
	}
	if backup.Type != "config" || strings.HasSuffix(backup.Path, ".tar.gz") {
		t.Fatalf("scheduled config generated wrong artifact: type=%s path=%s", backup.Type, backup.Path)
	}
	if backup.ScheduledBackupID == nil || *backup.ScheduledBackupID != schedule.ID {
		t.Fatalf("scheduled backup source = %v, want schedule %d", backup.ScheduledBackupID, schedule.ID)
	}
	artifactBody, err := os.ReadFile(backup.Path)
	if err != nil {
		t.Fatalf("read config artifact: %v", err)
	}
	if string(artifactBody) != string(configBody) {
		t.Fatalf("artifact did not copy ConfigPath: got %q want %q", artifactBody, configBody)
	}
	var manifest models.BackupManifest
	if err := db.Where("backup_id = ?", backup.ID).First(&manifest).Error; err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if manifest.Scope != "config" || manifest.Format != "file" || len(manifest.SourcePaths) != 1 || manifest.SourcePaths[0] != configPath {
		t.Fatalf("scheduled manifest mismatch: %#v", manifest)
	}
	var status models.ScheduledBackupStatus
	if err := db.Where("scheduled_backup_id = ?", schedule.ID).First(&status).Error; err != nil {
		t.Fatalf("load schedule status: %v", err)
	}
	if status.LastStatus != "success" || status.LastError != "" || status.BackupID == nil || *status.BackupID != backup.ID {
		t.Fatalf("success evidence mismatch: %#v", status)
	}

	manualPath := filepath.Join(backupRoot, "manual-config")
	if err := os.WriteFile(manualPath, configBody, 0o600); err != nil {
		t.Fatalf("write manual backup: %v", err)
	}
	manual := models.Backup{
		ComponentID: &component.ID, ClusterID: &cluster.ID, Name: "manual-config",
		Path: manualPath, Size: int64(len(configBody)), Type: "config",
	}
	if err := db.Create(&manual).Error; err != nil {
		t.Fatalf("create manual backup record: %v", err)
	}
	scheduler := NewScheduler(db, t.TempDir())
	schedule.KeepCount = 10
	for range 6 {
		scheduler.doBackup(context.Background(), &schedule)
	}
	schedule.KeepCount = 2
	scheduler.cleanupOldBackups(&schedule)
	var scheduledCount int64
	if err := db.Model(&models.Backup{}).Where("scheduled_backup_id = ?", schedule.ID).Count(&scheduledCount).Error; err != nil {
		t.Fatalf("count scheduled backups: %v", err)
	}
	if scheduledCount != 2 {
		t.Fatalf("scheduled backup count = %d, want keep_count 2", scheduledCount)
	}
	if err := db.First(&models.Backup{}, manual.ID).Error; err != nil {
		t.Fatalf("manual backup record was rotated: %v", err)
	}
	if _, err := os.Stat(manualPath); err != nil {
		t.Fatalf("manual backup artifact was rotated: %v", err)
	}
}

func TestScheduledBackupFailureRecordsRealEvidence(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "scheduler-failure.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	if err := db.AutoMigrate(
		&models.Cluster{}, &models.Host{}, &models.Component{},
		&models.ScheduledBackup{}, &models.Backup{}, &models.BackupManifest{},
		&models.ScheduledBackupStatus{}, &models.ComponentOperationClaim{},
		&services.ComponentBackupPause{},
	); err != nil {
		t.Fatalf("migrate scheduler models: %v", err)
	}
	t.Setenv("BACKUP_ROOT", filepath.Join(t.TempDir(), "backups"))
	cluster := models.Cluster{Name: "single", Status: "active"}
	host := models.Host{Name: "local", IP: "127.0.0.1", ConnectionType: "local"}
	if err := db.Create(&cluster).Error; err != nil {
		t.Fatalf("create cluster: %v", err)
	}
	if err := db.Create(&host).Error; err != nil {
		t.Fatalf("create host: %v", err)
	}
	component := models.Component{ClusterID: cluster.ID, HostID: host.ID, Name: "broken", Type: models.ComponentPrometheus}
	if err := db.Create(&component).Error; err != nil {
		t.Fatalf("create component: %v", err)
	}
	schedule := models.ScheduledBackup{Name: "broken-config", Cron: "* * * * *", ComponentID: &component.ID, Type: "config", Enabled: true}
	if err := db.Create(&schedule).Error; err != nil {
		t.Fatalf("create schedule: %v", err)
	}

	NewScheduler(db, t.TempDir()).doBackup(context.Background(), &schedule)

	var count int64
	if err := db.Model(&models.Backup{}).Count(&count).Error; err != nil {
		t.Fatalf("count backups: %v", err)
	}
	if count != 0 {
		t.Fatalf("failed scheduled backup recorded %d successful artifacts", count)
	}
	var status models.ScheduledBackupStatus
	if err := db.Where("scheduled_backup_id = ?", schedule.ID).First(&status).Error; err != nil {
		t.Fatalf("load failure evidence: %v", err)
	}
	if status.LastStatus != "failed" || !strings.Contains(status.LastError, "配置路径") || status.BackupID != nil {
		t.Fatalf("failure evidence mismatch: %#v", status)
	}
}

func TestDueSlotsPersistWhileEarlierBackupRuns(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "due.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.ScheduledBackup{}, &ScheduledBackupRun{}); err != nil {
		t.Fatal(err)
	}
	schedule := models.ScheduledBackup{Name: "overlap", Cron: "* * * * *", Enabled: true, Type: "config"}
	if err := db.Create(&schedule).Error; err != nil {
		t.Fatal(err)
	}
	s := NewScheduler(db, t.TempDir())
	first := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	if err := s.enqueueDueBackups(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&ScheduledBackupRun{}).Where("slot = ?", first).Update("status", "running").Error; err != nil {
		t.Fatal(err)
	}
	// Repeated detection must not duplicate the current minute, and detecting
	// the next minute must not wait for the first application's snapshot.
	for _, now := range []time.Time{first.Add(10 * time.Second), first.Add(time.Minute), first.Add(time.Minute + 10*time.Second)} {
		if err := s.enqueueDueBackups(context.Background(), now); err != nil {
			t.Fatal(err)
		}
	}
	var runs []ScheduledBackupRun
	if err := db.Order("slot").Find(&runs).Error; err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].Status != "running" || runs[1].Status != "pending" {
		t.Fatalf("due slots lost, duplicated or blocked: %#v", runs)
	}
	if err := RecoverInterruptedBackupRuns(db); err != nil {
		t.Fatal(err)
	}
	if err := db.Order("slot").Find(&runs).Error; err != nil {
		t.Fatal(err)
	}
	if runs[0].Status != "interrupted" || runs[1].Status != "pending" {
		t.Fatalf("restart lost pending work or fabricated success: %#v", runs)
	}
}
