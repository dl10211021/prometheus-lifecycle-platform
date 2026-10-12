package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestDecodeDashboardJSONIsBounded(t *testing.T) {
	t.Parallel()
	if _, err := decodeDashboardJSON(strings.NewReader(`{"title":"ok"}`), 64); err != nil {
		t.Fatalf("valid bounded dashboard rejected: %v", err)
	}
	oversized := `{"title":"` + strings.Repeat("x", 128) + `"}`
	if _, err := decodeDashboardJSON(strings.NewReader(oversized), 64); !errors.Is(err, errDashboardTooLarge) {
		t.Fatalf("oversized dashboard error = %v", err)
	}
	if _, err := decodeDashboardJSON(strings.NewReader(`{"title":"one"}{"title":"two"}`), 64); err == nil {
		t.Fatal("multiple JSON values were accepted")
	}
}

func TestHasMSIMagic(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "windows_exporter.msi")
	if err := os.WriteFile(path, []byte{0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1, 0x00}, 0600); err != nil {
		t.Fatal(err)
	}
	if !hasMSIMagic(path) {
		t.Fatal("valid MSI compound-file signature was rejected")
	}
	if err := os.WriteFile(path, []byte("not-an-msi"), 0600); err != nil {
		t.Fatal(err)
	}
	if hasMSIMagic(path) {
		t.Fatal("non-MSI payload was accepted")
	}
}

func TestValidateComponentDefinitionSecurityBoundaries(t *testing.T) {
	t.Parallel()
	valid := models.ComponentDefinition{
		Type: "acme_exporter", DisplayName: "Acme Exporter", BinName: "acme_exporter",
		DefaultPort: 9123, ConfigDir: "/etc/acme_exporter", DataDir: "/var/lib/acme_exporter",
		HealthPath: "/health", Command: "acme_exporter --listen=:{port}",
		PackageURL: "https://downloads.example.com/acme-{version}.tar.gz", Enabled: true,
	}
	if err := ValidateComponentDefinition(&valid); err != nil {
		t.Fatalf("valid definition rejected: %v", err)
	}

	unsafe := valid
	unsafe.Type = "../../evil"
	if err := ValidateComponentDefinition(&unsafe); err == nil {
		t.Fatal("unsafe component type accepted")
	}
	unsafe = valid
	unsafe.PackageURL = "http://127.0.0.1/package.tar.gz"
	if err := ValidateComponentDefinition(&unsafe); err == nil {
		t.Fatal("non-HTTPS package URL accepted")
	}
	unsafe = valid
	unsafe.Command = "acme_exporter; systemctl stop sshd"
	if err := ValidateComponentDefinition(&unsafe); err == nil {
		t.Fatal("command injection accepted")
	}
}

func TestPackageJSONHidesStoragePath(t *testing.T) {
	t.Parallel()
	encoded, err := json.Marshal(models.Package{
		ID: 1, Name: "prometheus", StoragePath: "/opt/promlifecycle/packages/internal.tar.gz",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "storage_path") || strings.Contains(string(encoded), "/opt/promlifecycle") {
		t.Fatalf("internal storage path exposed: %s", encoded)
	}
}

func TestPruneStalePackageDownloads(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	oldTemp := filepath.Join(dir, "grafana.tar.gz.deadbeef.downloading")
	newTemp := filepath.Join(dir, "prometheus.tar.gz.deadbeef.downloading")
	regular := filepath.Join(dir, "registered.tar.gz")
	for _, path := range []string{oldTemp, newTemp, regular} {
		if err := os.WriteFile(path, []byte("package"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	oldTime := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(oldTemp, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	pruneStalePackageDownloads(dir, time.Now().Add(-time.Hour))
	if _, err := os.Stat(oldTemp); !os.IsNotExist(err) {
		t.Fatalf("stale download remains: %v", err)
	}
	for _, path := range []string{newTemp, regular} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("live package removed: %s: %v", path, err)
		}
	}
}

func TestDeletePackagePreservesSharedArtifactUntilLastReference(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "shared-package.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.Package{}, &models.Component{}); err != nil {
		t.Fatal(err)
	}
	basePath := t.TempDir()
	packageDir := filepath.Join(basePath, "packages")
	if err := os.MkdirAll(packageDir, 0755); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(packageDir, "node_exporter-1.8.2.tar.gz")
	if err := os.WriteFile(artifact, []byte("shared package"), 0600); err != nil {
		t.Fatal(err)
	}
	first := models.Package{Type: "node_exporter", Version: "1.8.2", Format: "binary", StoragePath: artifact}
	second := models.Package{Type: "node_exporter", Version: "v1.8.2", Format: "binary", StoragePath: artifact}
	if err := db.Create(&first).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&second).Error; err != nil {
		t.Fatal(err)
	}
	handler := &PackageHandler{db: db, basePath: basePath}
	router := gin.New()
	router.DELETE("/packages/:id", handler.DeletePackage)

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/packages/"+strconv.FormatUint(uint64(first.ID), 10), nil))
	if response.Code != http.StatusOK {
		t.Fatalf("delete first reference status=%d body=%s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(artifact); err != nil {
		t.Fatalf("shared artifact removed with live reference: %v", err)
	}
	if err := db.First(&models.Package{}, second.ID).Error; err != nil {
		t.Fatalf("second package reference removed: %v", err)
	}

	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/packages/"+strconv.FormatUint(uint64(second.ID), 10), nil))
	if response.Code != http.StatusOK {
		t.Fatalf("delete final reference status=%d body=%s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(artifact); !os.IsNotExist(err) {
		t.Fatalf("unreferenced artifact remains: %v", err)
	}
}
