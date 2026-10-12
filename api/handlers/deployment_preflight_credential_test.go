package handlers

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/promlifecycle/platform/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func preflightCredentialFixture(t *testing.T) (*Handler, componentCreateRequest, models.DeploymentDefinition, models.Host, func()) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "preflight-credential.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.DeploymentPreflight{}, &models.Host{}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusOK)
	}))
	parsed, _ := url.Parse(server.URL)
	hostname, portText, _ := net.SplitHostPort(parsed.Host)
	port, _ := strconv.Atoi(portText)
	host := models.Host{Name: "target", IP: hostname, OS: "linux", ConnectionType: "ssh", Status: "online"}
	if err := db.Create(&host).Error; err != nil {
		server.Close()
		t.Fatal(err)
	}
	request := componentCreateRequest{Type: "node_exporter", Name: "target-a", HostID: &host.ID, Port: port, DeployType: "target", Version: "1.9.0"}
	definition, err := (&Handler{}).normalizeDeploymentRequest(&request, nil)
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	return &Handler{db: db}, request, definition, host, server.Close
}

func TestDeploymentPreflightCredentialSingleUseTamperAndExpiry(t *testing.T) {
	handler, request, definition, host, cleanup := preflightCredentialFixture(t)
	defer cleanup()
	if err := handler.validateAndConsumeDeploymentPreflight(request, definition, nil, host); err == nil || !strings.Contains(err.Error(), "缺少") {
		t.Fatalf("missing credential error=%v", err)
	}
	credential, err := handler.issueDeploymentPreflight(request, definition, nil, nil, nil, "tester")
	if err != nil {
		t.Fatal(err)
	}
	request.PreflightToken = credential.Token
	if err := handler.validateAndConsumeDeploymentPreflight(request, definition, nil, host); err != nil {
		t.Fatalf("first consume failed: %v", err)
	}
	if err := handler.validateAndConsumeDeploymentPreflight(request, definition, nil, host); err == nil || !strings.Contains(err.Error(), "已经使用") {
		t.Fatalf("reused credential error=%v", err)
	}

	tamperCredential, err := handler.issueDeploymentPreflight(request, definition, nil, nil, nil, "tester")
	if err != nil {
		t.Fatal(err)
	}
	tampered := request
	tampered.PreflightToken = tamperCredential.Token
	tampered.Name = "changed-after-preflight"
	if err := handler.validateAndConsumeDeploymentPreflight(tampered, definition, nil, host); err == nil || !strings.Contains(err.Error(), "发生变化") {
		t.Fatalf("tampered request error=%v", err)
	}
	structuredCredential, err := handler.issueDeploymentPreflight(request, definition, nil, nil, nil, "tester")
	if err != nil {
		t.Fatal(err)
	}
	structuredTamper := request
	structuredTamper.PreflightToken = structuredCredential.Token
	structuredTamper.StructuredParameters = map[string]interface{}{"retention_time": "90d"}
	if err := handler.validateAndConsumeDeploymentPreflight(structuredTamper, definition, nil, host); err == nil || !strings.Contains(err.Error(), "发生变化") {
		t.Fatalf("structured parameter tamper error=%v", err)
	}

	expiredCredential, err := handler.issueDeploymentPreflight(request, definition, nil, nil, nil, "tester")
	if err != nil {
		t.Fatal(err)
	}
	if err := handler.db.Model(&models.DeploymentPreflight{}).Where("token = ?", expiredCredential.Token).Update("expires_at", time.Now().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	expired := request
	expired.PreflightToken = expiredCredential.Token
	if err := handler.validateAndConsumeDeploymentPreflight(expired, definition, nil, host); err == nil || !strings.Contains(err.Error(), "已过期") {
		t.Fatalf("expired credential error=%v", err)
	}

	changedDefinitionCredential, err := handler.issueDeploymentPreflight(request, definition, nil, nil, nil, "tester")
	if err != nil {
		t.Fatal(err)
	}
	changedDefinition := definition
	changedDefinition.Description = "changed"
	changed := request
	changed.PreflightToken = changedDefinitionCredential.Token
	if err := handler.validateAndConsumeDeploymentPreflight(changed, changedDefinition, nil, host); err == nil || !strings.Contains(err.Error(), "部署定义") {
		t.Fatalf("changed definition error=%v", err)
	}
}

func TestDeploymentPreflightCredentialConcurrentConsumption(t *testing.T) {
	handler, request, definition, host, cleanup := preflightCredentialFixture(t)
	defer cleanup()
	credential, err := handler.issueDeploymentPreflight(request, definition, nil, nil, nil, "tester")
	if err != nil {
		t.Fatal(err)
	}
	request.PreflightToken = credential.Token
	start := make(chan struct{})
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for index := 0; index < 2; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			results <- handler.validateAndConsumeDeploymentPreflight(request, definition, nil, host)
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	successes := 0
	for result := range results {
		if result == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent successes=%d, want 1", successes)
	}
}

func TestGrafanaReinstallPreflightUsesManagedInstanceCredentials(t *testing.T) {
	handler, _, _, host, cleanup := preflightCredentialFixture(t)
	defer cleanup()
	if err := handler.db.AutoMigrate(&models.Component{}); err != nil {
		t.Fatal(err)
	}
	host.IP = "192.0.2.1"
	component := models.Component{
		HostID: host.ID, Type: models.ComponentGrafana, Name: "retained-grafana",
		DeployType: "binary", Status: models.StatusStopped, Port: 19300,
		ServiceName: "custom-retained-grafana",
		CustomEnv:   `{"GF_SECURITY_ADMIN_PASSWORD":"retained-password","GF_SECURITY_SECRET_KEY":"retained-encryption-key"}`,
	}
	if err := handler.db.Create(&component).Error; err != nil {
		t.Fatal(err)
	}
	request := componentCreateRequest{
		Type: "grafana", Name: "renamed-grafana", HostID: &host.ID,
		DeployType: "binary", Port: 19300, ReplaceExisting: true,
	}
	var checks []deploymentPreflightCheck
	handler.preflightGrafanaCredentials(&request, &host, &checks)
	if len(checks) != 1 || checks[0].Status != "pass" {
		t.Fatalf("managed instance with retained credentials cannot be reinstalled: %+v", checks)
	}
}
