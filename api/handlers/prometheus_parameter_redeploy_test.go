package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/services"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Re-deploy merges retained ImportedArgs after request normalization. A valid
// form must not authorize incompatible flags inherited from that instance.
func TestPrometheusParameterVersionRedeployChecksMergedInstance(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "redeploy.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { sqlDB.Close() })
	if err := db.AutoMigrate(&models.Component{}, &models.Host{}, &models.Cluster{}, &models.Package{}, &models.DeploymentPreflight{}, &models.ComponentOperationClaim{}, &models.HostOperationClaim{}, &models.ChangePlan{}, &models.ChangeBaseline{}, &services.ComponentBackupPause{}); err != nil {
		t.Fatal(err)
	}
	cluster := models.Cluster{Name: "single-node", Host: "127.0.0.1"}
	if err := db.Create(&cluster).Error; err != nil {
		t.Fatal(err)
	}
	host := models.Host{Name: "isolated", IP: "127.0.0.1", OS: "linux", ConnectionType: "local"}
	if err := db.Create(&host).Error; err != nil {
		t.Fatal(err)
	}
	component := models.Component{Name: "retained", ClusterID: cluster.ID, HostID: host.ID, Type: models.ComponentPrometheus, DeployType: "binary", Version: "3.1.0", Status: models.StatusError, Port: 9090, ImportedArgs: `["--storage.tsdb.retention=45d"]`}
	if err := db.Create(&component).Error; err != nil {
		t.Fatal(err)
	}
	h := &Handler{db: db, basePath: t.TempDir()}
	packageDir := filepath.Join(h.basePath, "packages")
	if err := os.MkdirAll(packageDir, 0700); err != nil {
		t.Fatal(err)
	}
	packagePath := filepath.Join(packageDir, "prometheus.tar.gz")
	data := []byte("isolated preflight package fixture")
	if err := os.WriteFile(packagePath, data, 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	pkg := models.Package{Name: "prometheus", Type: models.ComponentPrometheus, Version: "3.1.0", Format: "binary", StoragePath: packagePath, Checksum: hex.EncodeToString(digest[:]), Size: int64(len(data))}
	if err := db.Create(&pkg).Error; err != nil {
		t.Fatal(err)
	}

	request := componentCreateRequest{Type: "prometheus", Name: "retained", Version: "3.1.0", HostID: &host.ID, ReplaceExisting: true, DeployType: "binary"}
	definition, err := h.normalizeDeploymentRequest(&request, nil)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := h.issueDeploymentPreflight(request, definition, nil, nil, nil, "tester")
	if err != nil {
		t.Fatal(err)
	}
	request.PreflightToken = credential.Token
	body, _ := json.Marshal(request)
	saves := 0
	if err := db.Callback().Update().Before("gorm:update").Register("merged_parameter_write_guard", func(tx *gorm.DB) {
		if tx.Statement.Table == "components" {
			saves++
			tx.AddError(errors.New("unexpected merged component write"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/components", h.CreateComponent)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/components", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "storage.tsdb.retention") || saves != 0 {
		t.Fatalf("code=%d saves=%d response=%s", rec.Code, saves, rec.Body.String())
	}
	var after models.Component
	if err := db.First(&after, component.ID).Error; err != nil {
		t.Fatal(err)
	}
	if after.ImportedArgs != component.ImportedArgs || after.Status != component.Status || after.Version != component.Version {
		t.Fatal("re-deploy rejection changed retained instance")
	}
}
