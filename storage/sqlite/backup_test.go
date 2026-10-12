package sqlite

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/promlifecycle/platform/models"
	"gorm.io/gorm"
)

func platformTestDB(t *testing.T, path, name string) *gorm.DB {
	t.Helper()
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.User{}, &models.Host{}, &models.Component{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.User{Username: name, Password: "hash", Role: "system_admin"}).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connection, _ := db.DB(); connection.Close() })
	return db
}
func closeTestDB(t *testing.T, db *gorm.DB) {
	t.Helper()
	connection, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
}
func username(t *testing.T, path string) string {
	t.Helper()
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestDB(t, db)
	var user models.User
	if err := db.First(&user).Error; err != nil {
		t.Fatal(err)
	}
	return user.Username
}

func TestSnapshotRejectsHeaderOnlyAndForeignSchemaWithoutMutation(t *testing.T) {
	dir := t.TempDir()
	corrupt := filepath.Join(dir, "header.db")
	bytes := append([]byte("SQLite format 3\x00"), make([]byte, 4096)...)
	if err := os.WriteFile(corrupt, bytes, 0600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSnapshot(corrupt); err == nil {
		t.Fatal("header-only corruption accepted")
	}
	after, err := os.ReadFile(corrupt)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(bytes) {
		t.Fatal("validation mutated source")
	}
	foreign := filepath.Join(dir, "foreign.db")
	db, err := OpenDB(foreign)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TABLE unrelated(id INTEGER)").Error; err != nil {
		t.Fatal(err)
	}
	closeTestDB(t, db)
	if err := ValidateSnapshot(foreign); err == nil {
		t.Fatal("unrelated SQLite schema accepted")
	}
	for _, path := range []string{corrupt, foreign} {
		for _, suffix := range []string{"-wal", "-shm"} {
			if _, err := os.Stat(path + suffix); !os.IsNotExist(err) {
				t.Fatalf("read-only validation produced %s", path+suffix)
			}
		}
	}
}

func TestRestoreRollsBackFailedMigrationAndRecordsFailure(t *testing.T) {
	t.Setenv("PROMLIFECYCLE_SNAPSHOT_RESERVE_BYTES", "0")
	live := filepath.Join(t.TempDir(), "live.db")
	db := platformTestDB(t, live, "original")
	candidateDB := platformTestDB(t, filepath.Join(t.TempDir(), "candidate.db"), "candidate")
	snapshot := filepath.Join(t.TempDir(), "platform-db-candidate.db")
	if err := CreateSnapshot(candidateDB, snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := StageRestore(live, snapshot); err != nil {
		t.Fatal(err)
	}
	closeTestDB(t, db)
	tx, err := ApplyPendingRestore(live)
	if err != nil {
		t.Fatal(err)
	}
	migrated, err := OpenDB(live)
	if err != nil {
		t.Fatal(err)
	}
	// A partially executed migration must not contaminate the reversible original.
	if err := migrated.Exec("UPDATE users SET username='partially-migrated'").Error; err != nil {
		t.Fatal(err)
	}
	migrationErr := migrated.Exec("ALTER TABLE users ADD COLUMN id INTEGER").Error
	if migrationErr == nil {
		t.Fatal("expected duplicate-column migration to fail")
	}
	closeTestDB(t, migrated)
	if err := tx.Rollback(migrationErr); err != nil {
		t.Fatal(err)
	}
	if got := username(t, live); got != "original" {
		t.Fatalf("rollback retained %q", got)
	}
	state, err := RestoreStatus(live)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != "failed" || !strings.Contains(state.LastError, "duplicate column") {
		t.Fatalf("lost restore failure: %+v", state)
	}
	if _, err := StageRestore(live, snapshot); err != nil {
		t.Fatalf("failed restore cannot retry: %v", err)
	}
	next, err := ApplyPendingRestore(live)
	if err != nil {
		t.Fatal(err)
	}
	if err := next.Commit(); err != nil {
		t.Fatal(err)
	}
	if got := username(t, live); got != "candidate" {
		t.Fatalf("commit kept %q", got)
	}
	state, err = RestoreStatus(live)
	if err != nil || state.Status != "applied" {
		t.Fatalf("applied state: %+v %v", state, err)
	}
}

func TestInterruptedRestoreRevertsInsteadOfClaimingSuccess(t *testing.T) {
	t.Setenv("PROMLIFECYCLE_SNAPSHOT_RESERVE_BYTES", "0")
	live := filepath.Join(t.TempDir(), "live.db")
	db := platformTestDB(t, live, "original")
	snapshot := filepath.Join(t.TempDir(), "snapshot.db")
	if err := CreateSnapshot(db, snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := StageRestore(live, snapshot); err != nil {
		t.Fatal(err)
	}
	closeTestDB(t, db)
	if _, err := ApplyPendingRestore(live); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyPendingRestore(live); err != nil {
		t.Fatal(err)
	}
	state, err := RestoreStatus(live)
	if err != nil || state.Status != "failed" {
		t.Fatalf("interrupted restore status %+v %v", state, err)
	}
	if got := username(t, live); got != "original" {
		t.Fatal(got)
	}
}

func TestSnapshotCreationAndRestoreShareConfiguredLimit(t *testing.T) {
	t.Setenv("PROMLIFECYCLE_SNAPSHOT_RESERVE_BYTES", "0")
	live := filepath.Join(t.TempDir(), "live.db")
	db := platformTestDB(t, live, "original")
	snapshot := filepath.Join(t.TempDir(), "snapshot.db")
	if err := CreateSnapshot(db, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := RestoreEligibility(live, snapshot); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROMLIFECYCLE_SNAPSHOT_MAX_BYTES", "1")
	if err := RestoreEligibility(live, snapshot); err == nil {
		t.Fatal("restore ignored configured limit")
	}
	rejected := filepath.Join(t.TempDir(), "rejected.db")
	if err := CreateSnapshot(db, rejected); err == nil {
		t.Fatal("creation ignored configured limit")
	}
	if _, err := os.Stat(rejected); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed creation advertised an artifact")
	}
}

func TestRestoreStagesAcrossFilesystems(t *testing.T) {
	t.Setenv("PROMLIFECYCLE_SNAPSHOT_RESERVE_BYTES", "0")
	data := t.TempDir()
	var a, b syscall.Stat_t
	if err := syscall.Stat(data, &a); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Stat("/dev/shm", &b); err != nil || a.Dev == b.Dev {
		t.Skip("requires a separate writable tmpfs")
	}
	backupDir, err := os.MkdirTemp("/dev/shm", "platform-restore-")
	if err != nil {
		t.Skip(err)
	}
	defer os.RemoveAll(backupDir)
	live := filepath.Join(data, "live.db")
	db := platformTestDB(t, live, "original")
	snapshot := filepath.Join(backupDir, "snapshot.db")
	if err := CreateSnapshot(db, snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := StageRestore(live, snapshot); err != nil {
		t.Fatalf("cross-filesystem staging: %v", err)
	}
	closeTestDB(t, db)
	tx, err := ApplyPendingRestore(live)
	if err != nil {
		t.Fatalf("cross-filesystem cutover: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got := username(t, live); got != "original" {
		t.Fatal(got)
	}
}
