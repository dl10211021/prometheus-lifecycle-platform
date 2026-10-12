package scheduler

import (
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/promlifecycle/platform/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestSnapshotPlatformDBAtCreatesDailyAndKeepsSeven(t *testing.T) {
	dir := t.TempDir()
	db, err := gorm.Open(sqlite.Open(filepath.Join(dir, "source.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	if err := db.AutoMigrate(&models.User{}, &models.Host{}, &models.Component{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TABLE smoke (id INTEGER PRIMARY KEY, value TEXT)").Error; err != nil {
		t.Fatalf("create smoke table: %v", err)
	}
	if err := db.Exec("INSERT INTO smoke(value) VALUES (?)", "preserved").Error; err != nil {
		t.Fatalf("insert smoke row: %v", err)
	}

	lastDate := ""
	start := time.Date(2026, time.August, 1, 2, 3, 4, 0, time.Local)
	for day := range 9 {
		now := start.AddDate(0, 0, day)
		created, err := snapshotPlatformDBAtResult(db, dir, &lastDate, now)
		if err != nil {
			t.Fatalf("snapshot day %d: %v", day+1, err)
		}
		if !created {
			t.Fatalf("expected snapshot on day %d", day+1)
		}
		createdAgain, err := snapshotPlatformDBAtResult(db, dir, &lastDate, now.Add(time.Hour))
		if err != nil {
			t.Fatalf("repeat snapshot day %d: %v", day+1, err)
		}
		if createdAgain {
			t.Fatalf("created a second snapshot on day %d", day+1)
		}
	}

	files, err := filepath.Glob(filepath.Join(dir, "platform-db-*.db"))
	if err != nil {
		t.Fatalf("glob backups: %v", err)
	}
	sort.Strings(files)
	if len(files) != platformBackupKeep {
		t.Fatalf("expected %d retained backups, got %d", platformBackupKeep, len(files))
	}
	if got := filepath.Base(files[0]); got != "platform-db-2026-08-03-020304.db" {
		t.Fatalf("unexpected oldest retained backup %q", got)
	}
	if got := filepath.Base(files[len(files)-1]); got != "platform-db-2026-08-09-020304.db" {
		t.Fatalf("unexpected newest retained backup %q", got)
	}

	freshLastDate := ""
	created, err := snapshotPlatformDBAtResult(db, dir, &freshLastDate, start.AddDate(0, 0, 8).Add(2*time.Hour))
	if err != nil {
		t.Fatalf("restart same-day check: %v", err)
	}
	if created || freshLastDate != "2026-08-09" {
		t.Fatalf("restart should reuse same-day snapshot, created=%v lastDate=%q", created, freshLastDate)
	}
}
