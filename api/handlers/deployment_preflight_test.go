package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestDeploymentPreflightPassesWithoutCreatingComponentOrOperation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	basePath := t.TempDir()
	packageDir := filepath.Join(basePath, "packages")
	if err := os.MkdirAll(packageDir, 0755); err != nil {
		t.Fatal(err)
	}
	packagePath := filepath.Join(packageDir, "pushgateway-1.0.0.tar.gz")
	content := []byte("preflight-package")
	if err := os.WriteFile(packagePath, content, 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "preflight.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.Host{}, &models.Component{}, &models.Operation{}, &models.Package{}, &models.ComponentDefinition{}, &models.DeploymentPreflight{}); err != nil {
		t.Fatal(err)
	}
	host := models.Host{Name: "local", IP: "127.0.0.1", OS: "linux", ConnectionType: "local", Status: "online"}
	if err := db.Create(&host).Error; err != nil {
		t.Fatal(err)
	}
	pkg := models.Package{Name: "pushgateway", Type: models.ComponentPushgateway, Version: "1.0.0", Format: "binary", StoragePath: packagePath, Checksum: hex.EncodeToString(digest[:]), Size: int64(len(content))}
	if err := db.Create(&pkg).Error; err != nil {
		t.Fatal(err)
	}
	body := `{"type":"pushgateway","name":"push-preflight","host_id":1,"deploy_type":"binary","parameters":{"version":"1.0.0","port":19091,"install_path":"/opt/promlifecycle/components/push-preflight","data_path_override":"/var/lib/promlifecycle/push-preflight"}}`
	recorder := runDeploymentPreflight(t, &Handler{db: db, basePath: basePath}, body)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Passed     bool                       `json:"passed"`
		Checks     []deploymentPreflightCheck `json:"checks"`
		Actions    []deploymentPreviewAction  `json:"actions"`
		Parameters map[string]interface{}     `json:"normalized_parameters"`
		Token      string                     `json:"preflight_token"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Passed || response.Token == "" || len(response.Checks) < 6 || len(response.Actions) != 6 || int(response.Parameters["port"].(float64)) != 19091 {
		t.Fatalf("unexpected preflight response: %#v body=%s", response, recorder.Body.String())
	}
	var components, operations, credentials int64
	_ = db.Model(&models.Component{}).Count(&components).Error
	_ = db.Model(&models.Operation{}).Count(&operations).Error
	_ = db.Model(&models.DeploymentPreflight{}).Count(&credentials).Error
	if components != 0 || operations != 0 || credentials != 1 {
		t.Fatalf("dry run created components=%d operations=%d credentials=%d", components, operations, credentials)
	}
}

func TestDeploymentPreflightReportsSchemaAndChecksumBlockers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	basePath := t.TempDir()
	packageDir := filepath.Join(basePath, "packages")
	if err := os.MkdirAll(packageDir, 0755); err != nil {
		t.Fatal(err)
	}
	packagePath := filepath.Join(packageDir, "pushgateway-1.0.0.tar.gz")
	if err := os.WriteFile(packagePath, []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "preflight-blockers.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.Host{}, &models.Component{}, &models.Operation{}, &models.Package{}, &models.ComponentDefinition{}, &models.DeploymentPreflight{}); err != nil {
		t.Fatal(err)
	}
	host := models.Host{Name: "local", IP: "127.0.0.1", OS: "linux", ConnectionType: "local", Status: "online"}
	if err := db.Create(&host).Error; err != nil {
		t.Fatal(err)
	}
	pkg := models.Package{Name: "pushgateway", Type: models.ComponentPushgateway, Version: "1.0.0", Format: "binary", StoragePath: packagePath, Checksum: "deadbeef"}
	if err := db.Create(&pkg).Error; err != nil {
		t.Fatal(err)
	}
	invalidSchema := runDeploymentPreflight(t, &Handler{db: db, basePath: basePath}, `{"type":"pushgateway","name":"bad","host_id":1,"deploy_type":"binary","parameters":{"version":"1.0.0","port":"9091"}}`)
	if !jsonResponseContainsFailedCheck(invalidSchema, "schema") {
		t.Fatalf("schema blocker missing: %s", invalidSchema.Body.String())
	}
	badChecksum := runDeploymentPreflight(t, &Handler{db: db, basePath: basePath}, `{"type":"pushgateway","name":"bad-package","host_id":1,"deploy_type":"binary","parameters":{"version":"1.0.0","port":19092,"install_path":"/opt/promlifecycle/components/bad-package","data_path_override":"/var/lib/promlifecycle/bad-package"}}`)
	if !jsonResponseContainsFailedCheck(badChecksum, "package") {
		t.Fatalf("package blocker missing: %s", badChecksum.Body.String())
	}
}

func runDeploymentPreflight(t *testing.T, handler *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodPost, "/api/v1/components/preflight", strings.NewReader(body))
	context.Request.Header.Set("Content-Type", "application/json")
	handler.PreflightComponentDeployment(context)
	return recorder
}

func jsonResponseContainsFailedCheck(recorder *httptest.ResponseRecorder, key string) bool {
	var response struct {
		Checks []deploymentPreflightCheck `json:"checks"`
	}
	if json.Unmarshal(recorder.Body.Bytes(), &response) != nil {
		return false
	}
	for _, check := range response.Checks {
		if check.Key == key && check.Status == "fail" && check.Blocking {
			return true
		}
	}
	return false
}

func TestDeploymentDependenciesRequireExistingOrEarlierPlannedComponent(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "dependencies.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.Component{}); err != nil {
		t.Fatal(err)
	}
	handler := &Handler{db: db}
	host := models.Host{ID: 7}
	request := componentCreateRequest{
		Type: string(models.ComponentThanos), Version: "0.39.2", PlannedComponents: []string{"thanos"},
	}
	var checks []deploymentPreflightCheck
	handler.preflightDependencies(&request, &host, &checks)
	if len(checks) != 1 || checks[0].Status != "fail" {
		t.Fatalf("missing intrinsic dependency was not blocked: %#v", checks)
	}

	request.PlannedComponents = []string{"prometheus", "thanos"}
	request.PlannedVersions = map[string]string{"prometheus": "2.55.0", "thanos": "0.39.2"}
	checks = nil
	handler.preflightDependencies(&request, &host, &checks)
	if len(checks) != 1 || checks[0].Status != "pass" {
		t.Fatalf("earlier planned dependency was not accepted: %#v", checks)
	}

	if err := db.Create(&models.Component{
		HostID: host.ID, Type: models.ComponentPrometheus,
		Name: "prometheus", Version: "2.55.0", Status: models.StatusRunning, DeployType: "binary",
	}).Error; err != nil {
		t.Fatal(err)
	}
	request.PlannedComponents = []string{"thanos"}
	checks = nil
	handler.preflightDependencies(&request, &host, &checks)
	if len(checks) != 1 || checks[0].Status != "pass" {
		t.Fatalf("existing running dependency was not accepted: %#v", checks)
	}
}

func TestPromtailPreflightRequiresExistingFileWithoutJournal(t *testing.T) {
	handler := &Handler{}
	host := models.Host{IP: "127.0.0.1", ConnectionType: "local"}
	request := componentCreateRequest{
		Type: string(models.ComponentPromtail),
		StructuredParameters: map[string]interface{}{
			"file_log_enabled": true, "journal_enabled": false,
			"file_glob": filepath.Join(t.TempDir(), "missing.log"),
		},
	}
	var checks []deploymentPreflightCheck
	handler.preflightPromtailSources(&request, &host, &checks)
	if len(checks) != 1 || checks[0].Status != "fail" || !checks[0].Blocking {
		t.Fatalf("missing only log source was not blocked: %#v", checks)
	}
	if err := os.WriteFile(request.StructuredParameters["file_glob"].(string), []byte("line\n"), 0644); err != nil {
		t.Fatal(err)
	}
	checks = nil
	handler.preflightPromtailSources(&request, &host, &checks)
	if len(checks) != 1 || checks[0].Status != "pass" {
		t.Fatalf("existing log source was not accepted: %#v", checks)
	}
}

func TestThanosPrometheusVersionCompatibility(t *testing.T) {
	if thanosSupportsPrometheus("v0.34.1", "v3.1.0") {
		t.Fatal("incompatible Thanos version accepted for Prometheus 3")
	}
	if !thanosSupportsPrometheus("v0.39.2", "v3.1.0") {
		t.Fatal("compatible Thanos version rejected for Prometheus 3")
	}
	if !thanosSupportsPrometheus("v0.34.1", "v2.53.0") {
		t.Fatal("Prometheus 2 compatibility was rejected")
	}
}

func TestGrafanaPreflightRequiresNewOrRetainedCredentials(t *testing.T) {
	handler := &Handler{basePath: t.TempDir()}
	host := models.Host{IP: "127.0.0.1", ConnectionType: "local"}
	request := componentCreateRequest{Type: string(models.ComponentGrafana), InitialPassword: "Strong!Password123"}
	var checks []deploymentPreflightCheck
	handler.preflightGrafanaCredentials(&request, &host, &checks)
	if len(checks) != 1 || checks[0].Status != "pass" {
		t.Fatalf("provided Grafana credentials were not accepted: %#v", checks)
	}
	request.InitialPassword = ""
	checks = nil
	handler.preflightGrafanaCredentials(&request, &host, &checks)
	if len(checks) != 1 || checks[0].Status != "fail" || !checks[0].Blocking {
		t.Fatalf("missing Grafana credentials were not blocked: %#v", checks)
	}
}

func TestCustomHTTPSPackageCanPassPreflightWithoutLocalCache(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "custom-online-package.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.Package{}); err != nil {
		t.Fatal(err)
	}
	handler := &Handler{db: db, basePath: t.TempDir()}
	request := componentCreateRequest{Type: "remote_acceptance_exporter", Version: "v1.0.0"}
	definition := models.DeploymentDefinition{Type: request.Type, Platform: "linux"}
	custom := models.ComponentDefinition{
		Type: request.Type, PackageURL: "https://packages.example.com/remote_acceptance_exporter-{version}.tar.gz",
	}
	var checks []deploymentPreflightCheck
	handler.preflightPackage(&request, &definition, &custom, &checks)
	if len(checks) != 1 || checks[0].Status != "warn" || checks[0].Blocking {
		t.Fatalf("custom HTTPS package was blocked before deployment: %#v", checks)
	}
	checks = nil
	handler.preflightPackage(&request, &definition, nil, &checks)
	if len(checks) != 1 || checks[0].Status != "fail" || !checks[0].Blocking {
		t.Fatalf("built-in missing package was not blocked: %#v", checks)
	}
}
