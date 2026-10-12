package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/promlifecycle/platform/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestPrometheusParameterVersionPreflightUsesRetainedArguments(t *testing.T) {
	for _, tc := range []struct {
		version string
		passed  bool
	}{{"2.6.1", true}, {"3.1.0", false}} {
		t.Run(tc.version, func(t *testing.T) {
			base := t.TempDir()
			db, err := gorm.Open(sqlite.Open(filepath.Join(base, "preflight.db")), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			sqlDB, _ := db.DB()
			t.Cleanup(func() { sqlDB.Close() })
			if err := db.AutoMigrate(&models.Host{}, &models.Component{}, &models.Package{}, &models.DeploymentPreflight{}); err != nil {
				t.Fatal(err)
			}
			host := models.Host{Name: "local", IP: "127.0.0.1", OS: "linux", ConnectionType: "local"}
			if err := db.Create(&host).Error; err != nil {
				t.Fatal(err)
			}
			c := models.Component{Name: "retained", HostID: host.ID, Type: models.ComponentPrometheus, Version: tc.version, DeployType: "binary", Status: models.StatusError, Port: 19097, ImportedArgs: `["--storage.tsdb.retention=45d"]`}
			if err := db.Create(&c).Error; err != nil {
				t.Fatal(err)
			}
			packageDir := filepath.Join(base, "packages")
			if err := os.MkdirAll(packageDir, 0700); err != nil {
				t.Fatal(err)
			}
			packagePath := filepath.Join(packageDir, "prometheus.tar.gz")
			content := []byte("preflight fixture")
			if err := os.WriteFile(packagePath, content, 0600); err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(content)
			if err := db.Create(&models.Package{Name: "prometheus", Type: models.ComponentPrometheus, Version: tc.version, Format: "binary", StoragePath: packagePath, Checksum: hex.EncodeToString(digest[:]), Size: int64(len(content))}).Error; err != nil {
				t.Fatal(err)
			}
			request := componentCreateRequest{Type: "prometheus", Name: "retained", Version: tc.version, HostID: &host.ID, Port: 19097, ReplaceExisting: true, DeployType: "binary"}
			body, _ := json.Marshal(request)
			rec := runDeploymentPreflight(t, &Handler{db: db, basePath: base}, string(body))
			var response struct {
				Passed bool                       `json:"passed"`
				Checks []deploymentPreflightCheck `json:"checks"`
				Token  string                     `json:"preflight_token"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if rec.Code != http.StatusOK || response.Passed != tc.passed || (response.Token != "") != tc.passed {
				t.Fatalf("response=%s", rec.Body.String())
			}
			found := false
			for _, check := range response.Checks {
				if check.Key == "parameter_version" {
					found = true
					if (check.Status == "pass") != tc.passed {
						t.Fatalf("wrong parameter check: %+v", check)
					}
				}
			}
			if !found {
				t.Fatal("no retained-argument parameter check")
			}
			var after models.Component
			if err := db.First(&after, c.ID).Error; err != nil {
				t.Fatal(err)
			}
			if after.ImportedArgs != c.ImportedArgs || after.Status != c.Status {
				t.Fatal("read-only preflight changed imported instance")
			}
		})
	}
}
