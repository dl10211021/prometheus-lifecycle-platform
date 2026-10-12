package handlers

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestUploadPackagePreservesBuildMetadataAndRejectsUnsafeMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)
	base := t.TempDir()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "metadata.db")), &gorm.Config{})
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
	router := gin.New()
	router.POST("/packages/upload", NewPackageHandler(db, base).UploadImage)
	var payload bytes.Buffer
	gz := gzip.NewWriter(&payload)
	archive := tar.NewWriter(gz)
	data := []byte("metadata package payload")
	if err := archive.WriteHeader(&tar.Header{Name: "prometheus/prometheus", Mode: 0755, Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	checksum := sha256.Sum256(payload.Bytes())
	upload := func(version string) *httptest.ResponseRecorder {
		t.Helper()
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		for key, value := range map[string]string{"name": "prometheus", "type": "prometheus", "version": version} {
			if err := form.WriteField(key, value); err != nil {
				t.Fatal(err)
			}
		}
		part, err := form.CreateFormFile("file", "prometheus.tar.gz")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(payload.Bytes()); err != nil {
			t.Fatal(err)
		}
		if err := form.Close(); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/packages/upload", &body)
		req.Header.Set("Content-Type", form.FormDataContentType())
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		return response
	}
	versions := []string{"3.13.4+promlifecycle.1", "3.13.4+promlifecycle.2"}
	for _, version := range versions {
		response := upload(version)
		if response.Code != http.StatusCreated {
			t.Fatalf("upload %s status=%d body=%s", version, response.Code, response.Body.String())
		}
		var saved models.Package
		if err := db.Where("version = ?", version).First(&saved).Error; err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(saved.StoragePath)
		if err != nil || !bytes.Equal(got, payload.Bytes()) || saved.Checksum != hex.EncodeToString(checksum[:]) {
			t.Fatalf("upload bytes or checksum changed: %v", err)
		}
		if filepath.Dir(saved.StoragePath) != filepath.Join(base, "packages") {
			t.Fatalf("upload escaped managed directory: %s", saved.StoragePath)
		}
		if err := models.ValidatePrometheusArgumentVersion(saved.Version, []string{"--web.listen-address=:9090"}); err != nil {
			t.Fatalf("stable build rejected by Prometheus version gate: %v", err)
		}
	}
	if err := models.ValidatePrometheusArgumentVersion("3.13.4-rc.1+promlifecycle.1", nil); err == nil {
		t.Fatal("build metadata bypassed Prometheus prerelease restriction")
	}
	for _, version := range []string{"3.13.4+../escape", "3.13.4+build..1", "3.13.4+build/escape", "3.13.4+build;id"} {
		if response := upload(version); response.Code != http.StatusBadRequest {
			t.Fatalf("unsafe upload %q status=%d", version, response.Code)
		}
	}
	var count int64
	if err := db.Model(&models.Package{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(base, "packages"))
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 || len(entries) != 2 {
		t.Fatalf("unexpected records/files after invalid upload: %d/%d", count, len(entries))
	}
}
