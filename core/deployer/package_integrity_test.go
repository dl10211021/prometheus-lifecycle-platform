package deployer

import (
	"archive/zip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/promlifecycle/platform/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func acquisitionDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "packages.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.Package{}); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	return db
}

func writeAcquisitionZIP(t *testing.T, base, filename, binary, contents string) string {
	t.Helper()
	dir := filepath.Join(base, "packages")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, filename)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(f)
	entry, err := archive.Create(binary)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte(contents)); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOfficialZIPVersionsNeverShareStorage(t *testing.T) {
	t.Setenv("PROMLIFECYCLE_OFFLINE", "1")
	for _, component := range []string{"loki", "promtail"} {
		t.Run(component, func(t *testing.T) {
			db, base := acquisitionDB(t), t.TempDir()
			firstPath := writeAcquisitionZIP(t, base, component+"-3.2.0.linux-amd64.zip", component+"-linux-amd64", "release 3.2.0")
			secondPath := writeAcquisitionZIP(t, base, component+"-3.3.0.linux-amd64.zip", component+"-linux-amd64", "release 3.3.0")
			first, err := DownloadOfficialPackage(context.Background(), db, base, component, "v3.2.0", nil)
			if err != nil {
				t.Fatal(err)
			}
			second, err := DownloadOfficialPackage(context.Background(), db, base, component, "3.3.0", nil)
			if err != nil {
				t.Fatal(err)
			}
			if first.StoragePath != firstPath || second.StoragePath != secondPath || first.ID == second.ID || first.Checksum == second.Checksum {
				t.Fatalf("release identity collision: %+v %+v", first, second)
			}
			if err := ValidatePackage(base, first); err != nil {
				t.Fatalf("second acquisition damaged first release: %v", err)
			}
		})
	}
}

func TestExplicitAcquisitionRepairsMissingRecordFileOffline(t *testing.T) {
	t.Setenv("PROMLIFECYCLE_OFFLINE", "1")
	db, base := acquisitionDB(t), t.TempDir()
	broken := models.Package{Type: models.ComponentLoki, Version: "v3.2.0", Format: "binary", StoragePath: filepath.Join(base, "packages", "missing.zip"), Checksum: "bad", Size: 99}
	if err := db.Create(&broken).Error; err != nil {
		t.Fatal(err)
	}
	path := writeAcquisitionZIP(t, base, "loki-3.2.0.linux-amd64.zip", "loki-linux-amd64", "repaired release")
	repaired, err := DownloadOfficialPackage(context.Background(), db, base, "loki", "3.2.0", nil)
	if err != nil {
		t.Fatal(err)
	}
	if repaired.ID != broken.ID || repaired.StoragePath != path || repaired.Checksum == broken.Checksum {
		t.Fatalf("record not repaired: %+v", repaired)
	}
	var saved models.Package
	if err := db.First(&saved, broken.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := ValidatePackage(base, saved); err != nil {
		t.Fatalf("persisted repair is unavailable: %v", err)
	}
}

func TestOfflineAcquisitionRejectsCorruptRecordedArchive(t *testing.T) {
	t.Setenv("PROMLIFECYCLE_OFFLINE", "1")
	db, base := acquisitionDB(t), t.TempDir()
	path := writeAcquisitionZIP(t, base, "loki-3.2.0.linux-amd64.zip", "loki-linux-amd64", "original release")
	pkg, err := DownloadOfficialPackage(context.Background(), db, base, "loki", "3.2.0", nil)
	if err != nil {
		t.Fatal(err)
	}
	writeAcquisitionZIP(t, base, filepath.Base(path), "loki-linux-amd64", "modified release")
	if err := ValidatePackage(base, pkg); err == nil {
		t.Fatal("modified archive reported available")
	}
	if _, err := DownloadOfficialPackage(context.Background(), db, base, "loki", "3.2.0", nil); !errors.Is(err, ErrPackageOffline) {
		t.Fatalf("offline corrupt archive falsely repaired: %v", err)
	}
	var saved models.Package
	if err := db.First(&saved, pkg.ID).Error; err != nil {
		t.Fatal(err)
	}
	if saved.Checksum != pkg.Checksum {
		t.Fatal("corrupt bytes blessed with a replacement checksum")
	}
	if _, err := DownloadOfficialPackage(context.Background(), db, base, "promtail", "3.2.0", nil); !errors.Is(err, ErrPackageOffline) {
		t.Fatalf("missing offline package succeeded: %v", err)
	}
}

func TestLegacyUnversionedZIPCannotInventReleaseIdentity(t *testing.T) {
	base := t.TempDir()
	path := writeAcquisitionZIP(t, base, "loki-linux-amd64.zip", "loki-linux-amd64", "unknown release")
	if err := ValidatePackage(base, models.Package{Type: models.ComponentLoki, Format: "binary", StoragePath: path}); err == nil {
		t.Fatal("ambiguous legacy archive accepted")
	}
	if component, _ := parseOfficialArchiveName(filepath.Base(path)); component != "" {
		t.Fatal("legacy archive assigned a guessed version")
	}
}
