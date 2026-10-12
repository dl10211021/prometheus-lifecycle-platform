package deployer

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/promlifecycle/platform/models"
)

func TestBuildMetadataPackageScanExtractionAndCacheIsolation(t *testing.T) {
	db, base := acquisitionDB(t), t.TempDir()
	if err := db.AutoMigrate(&models.Component{}); err != nil {
		t.Fatal(err)
	}
	versions := []string{"1.12.1+promlifecycle.1", "1.12.1+promlifecycle.2"}
	for _, version := range versions {
		dir := filepath.Join(base, "packages")
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		f, err := os.Create(filepath.Join(dir, "node_exporter-"+version+".linux-amd64.tar.gz"))
		if err != nil {
			t.Fatal(err)
		}
		gz := gzip.NewWriter(f)
		archive := tar.NewWriter(gz)
		contents := []byte("release " + version)
		if err := archive.WriteHeader(&tar.Header{Name: "node_exporter-" + version + ".linux-amd64/node_exporter", Mode: 0755, Size: int64(len(contents))}); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write(contents); err != nil {
			t.Fatal(err)
		}
		for _, close := range []func() error{archive.Close, gz.Close, f.Close} {
			if err := close(); err != nil {
				t.Fatal(err)
			}
		}
	}
	ScanLocalPackages(db, base)
	var packages []models.Package
	if err := db.Order("version").Find(&packages).Error; err != nil {
		t.Fatal(err)
	}
	if len(packages) != 2 {
		t.Fatalf("metadata archives registered = %d, want 2", len(packages))
	}
	spec, err := binarySpecFor(models.ComponentNodeExporter)
	if err != nil {
		t.Fatal(err)
	}
	deployer := NewBinaryDeployer(db, base)
	paths := make([]string, len(versions))
	for i, version := range versions {
		if packages[i].Version != version {
			t.Fatalf("version identity changed: %q", packages[i].Version)
		}
		if err := ValidatePackage(base, packages[i]); err != nil {
			t.Fatal(err)
		}
		paths[i], err = deployer.extractBinary(context.Background(), spec, version)
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Dir(paths[i]) != filepath.Join(base, "bin") {
			t.Fatalf("cache escaped bin directory: %s", paths[i])
		}
		got, err := os.ReadFile(paths[i])
		if err != nil || string(got) != "release "+version {
			t.Fatalf("wrong extracted build: %q, %v", got, err)
		}
	}
	if paths[0] == paths[1] {
		t.Fatal("different builds share extracted cache")
	}
	component := models.Component{Name: "active build", Type: models.ComponentNodeExporter, Version: versions[0]}
	if err := db.Create(&component).Error; err != nil {
		t.Fatal(err)
	}
	if err := RemoveUnusedPackageCache(db, base, &packages[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths[0]); err != nil {
		t.Fatalf("active build cache removed: %v", err)
	}
	if err := db.Delete(&component).Error; err != nil {
		t.Fatal(err)
	}
	if err := RemoveUnusedPackageCache(db, base, &packages[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths[0]); !os.IsNotExist(err) {
		t.Fatalf("unused metadata cache remains: %v", err)
	}
	if got, err := os.ReadFile(paths[1]); err != nil || string(got) != "release "+versions[1] {
		t.Fatalf("other build cache altered: %q %v", got, err)
	}
}

func TestBuildMetadataArchiveNameIdentity(t *testing.T) {
	for _, name := range []string{"prometheus-3.13.4+promlifecycle.1.linux-amd64.tar.gz", "loki-3.13.4+promlifecycle.1.linux-amd64.zip"} {
		component, version := parseOfficialArchiveName(name)
		if component == "" || version != "3.13.4+promlifecycle.1" {
			t.Fatalf("archive identity lost for %s: %q %q", name, component, version)
		}
	}
	for _, name := range []string{"node_exporter-1.2.3+../escape.linux-amd64.tar.gz", "loki-1.2.3+build..1.linux-amd64.zip", "prometheus-1.2.3+.linux-amd64.tar.gz"} {
		if component, _ := parseOfficialArchiveName(name); component != "" {
			t.Fatalf("unsafe archive registered: %s", name)
		}
	}
}
