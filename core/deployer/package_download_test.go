package deployer

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promlifecycle/platform/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestPackageDownloadProgressWriterReportsBytes(t *testing.T) {
	t.Parallel()

	var events []PackageDownloadProgress
	writer := &packageDownloadProgressWriter{
		progress: func(progress PackageDownloadProgress) {
			events = append(events, progress)
		},
		detail: "github.com / package.tar.gz",
		total:  2 << 20,
		next:   1 << 20,
	}

	chunk := make([]byte, 600<<10)
	for range [3]struct{}{} {
		if _, err := writer.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	writer.finish()

	if len(events) != 2 {
		t.Fatalf("expected throttled update plus final update, got %d: %+v", len(events), events)
	}
	last := events[len(events)-1]
	if last.Stage != "下载文件" || last.Downloaded != int64(len(chunk)*3) || last.Total != 2<<20 {
		t.Fatalf("unexpected final progress: %+v", last)
	}
}

func TestOfficialDownloadRedirectPolicy(t *testing.T) {
	t.Parallel()

	client := officialDownloadHTTPClient()
	httpsReq, _ := http.NewRequest(http.MethodGet, "https://github.com/file", nil)
	httpsCDN, _ := http.NewRequest(http.MethodGet, "https://objects.githubusercontent.com/file", nil)
	if err := client.CheckRedirect(httpsCDN, []*http.Request{httpsReq}); err != nil {
		t.Fatalf("trusted HTTPS-to-HTTPS redirect rejected: %v", err)
	}

	httpReq, _ := http.NewRequest(http.MethodGet, "http://example.com/file", nil)
	if err := client.CheckRedirect(httpReq, []*http.Request{httpsReq}); err == nil || !strings.Contains(err.Error(), "downgrade") {
		t.Fatalf("HTTPS downgrade was not rejected: %v", err)
	}

	fileReq, _ := http.NewRequest(http.MethodGet, "file:///tmp/package", nil)
	if err := client.CheckRedirect(fileReq, []*http.Request{httpsReq}); err == nil {
		t.Fatal("non-HTTP redirect scheme was not rejected")
	}
}

func TestOfficialPackageURLRejectsUnsafeVersionAndUnknownType(t *testing.T) {
	t.Parallel()
	if _, _, err := OfficialPackageURL("prometheus", "../../etc/passwd"); err == nil {
		t.Fatal("path-traversal version was accepted")
	}
	if _, _, err := OfficialPackageURL("custom_exporter", "1.0.0"); err == nil {
		t.Fatal("unknown official component type was accepted")
	}
	url, name, err := OfficialPackageURL("prometheus", "v2.53.0")
	if err != nil || !strings.HasPrefix(url, "https://") || name != "prometheus-2.53.0.linux-amd64.tar.gz" {
		t.Fatalf("valid official package metadata rejected: %q %q %v", url, name, err)
	}
}

func TestRemoveUnusedPackageCachePreservesActiveComponentThenCleans(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:package-cache-cleanup?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.Component{}, &models.Package{}, &models.ComponentDefinition{}); err != nil {
		t.Fatal(err)
	}
	basePath := t.TempDir()
	cachePath := filepath.Join(basePath, "bin", "acceptance_exporter-v1.0.0")
	if err := os.MkdirAll(filepath.Dir(cachePath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, []byte("binary"), 0755); err != nil {
		t.Fatal(err)
	}
	pkg := models.Package{Name: "acceptance_exporter", Type: "acceptance_exporter", Version: "v1.0.0", Format: "binary"}
	if err := db.Create(&pkg).Error; err != nil {
		t.Fatal(err)
	}
	component := models.Component{Type: "acceptance_exporter", Version: "v1.0.0", Name: "acceptance"}
	if err := db.Create(&component).Error; err != nil {
		t.Fatal(err)
	}
	if err := RemoveUnusedPackageCache(db, basePath, &pkg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("active component cache was removed: %v", err)
	}
	if err := db.Delete(&component).Error; err != nil {
		t.Fatal(err)
	}
	if err := RemoveUnusedPackageCache(db, basePath, &pkg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Fatalf("unused cache still exists: %v", err)
	}
}
