package handlers

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func acceptanceHandlerDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "handler-acceptance.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.Cluster{}, &models.Host{}, &models.Component{}, &models.DeploymentStack{}, &models.DeploymentStackStep{}, &models.OnboardingCheckRun{}, &models.Backup{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestDeploymentAcceptanceHandlersValidateIDsAndConflicts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := acceptanceHandlerDB(t)
	cluster := models.Cluster{Name: "handler"}
	host := models.Host{Name: "handler-host", IP: "192.0.2.40", OS: "linux"}
	if err := db.Create(&cluster).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&host).Error; err != nil {
		t.Fatal(err)
	}
	exporters := []models.Component{
		{ClusterID: cluster.ID, HostID: host.ID, Name: "one", Type: models.ComponentNodeExporter, Status: models.StatusRunning},
		{ClusterID: cluster.ID, HostID: host.ID, Name: "two", Type: models.ComponentNodeExporter, Status: models.StatusRunning},
	}
	if err := db.Create(&exporters).Error; err != nil {
		t.Fatal(err)
	}
	stack := models.DeploymentStack{Name: "pending", HostID: host.ID, Status: "running", Total: 1, Steps: []models.DeploymentStackStep{{Position: 0, Type: string(models.ComponentNodeExporter), Status: "running", EncryptedRequest: "opaque"}}}
	if err := db.Create(&stack).Error; err != nil {
		t.Fatal(err)
	}

	handler := &Handler{db: db}
	router := gin.New()
	router.GET("/hosts/:id/monitoring-status", handler.GetHostMonitoringStatus)
	router.GET("/deployments/stacks/:id/acceptance", handler.GetDeploymentStackAcceptance)
	for _, test := range []struct {
		path string
		want int
	}{
		{path: "/hosts/nope/monitoring-status", want: http.StatusBadRequest},
		{path: fmt.Sprintf("/hosts/%d/monitoring-status?component_id=nope", host.ID), want: http.StatusBadRequest},
		{path: fmt.Sprintf("/hosts/%d/monitoring-status?prometheus_id=0", host.ID), want: http.StatusBadRequest},
		{path: "/hosts/999999/monitoring-status", want: http.StatusNotFound},
		{path: fmt.Sprintf("/hosts/%d/monitoring-status", host.ID), want: http.StatusConflict},
		{path: fmt.Sprintf("/deployments/stacks/%d/acceptance", stack.ID), want: http.StatusConflict},
		{path: fmt.Sprintf("/deployments/stacks/%d/acceptance?prometheus_id=nope", stack.ID), want: http.StatusBadRequest},
		{path: "/deployments/stacks/999999/acceptance", want: http.StatusNotFound},
	} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
		if response.Code != test.want {
			t.Fatalf("%s status=%d want=%d body=%s", test.path, response.Code, test.want, response.Body.String())
		}
	}
}

func TestHostAcceptanceHandlerReturnsScopedReportWithoutWrites(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/-/ready":
			w.WriteHeader(http.StatusOK)
		case "/api/v1/targets":
			_, _ = fmt.Fprintf(w, `{"status":"success","data":{"activeTargets":[{"scrapeUrl":"http://192.0.2.41:9100/metrics","health":"up","lastScrape":%q}]}}`, time.Now().UTC().Format(time.RFC3339Nano))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	serverURL, _ := url.Parse(server.URL)
	serverPort, _ := strconv.Atoi(serverURL.Port())
	db := acceptanceHandlerDB(t)
	cluster := models.Cluster{Name: "handler-success"}
	targetHost := models.Host{Name: "handler-target", IP: "192.0.2.41", OS: "linux"}
	prometheusHost := models.Host{Name: "handler-prometheus", IP: serverURL.Hostname(), OS: "linux"}
	for _, value := range []interface{}{&cluster, &targetHost, &prometheusHost} {
		if err := db.Create(value).Error; err != nil {
			t.Fatal(err)
		}
	}
	exporter := models.Component{ClusterID: cluster.ID, HostID: targetHost.ID, Name: "node", Type: models.ComponentNodeExporter, Port: 9100, Status: models.StatusRunning}
	prometheus := models.Component{ClusterID: cluster.ID, HostID: prometheusHost.ID, Name: "prom", Type: models.ComponentPrometheus, Port: serverPort, Status: models.StatusRunning}
	if err := db.Create(&exporter).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&prometheus).Error; err != nil {
		t.Fatal(err)
	}
	handler := &Handler{db: db}
	router := gin.New()
	router.GET("/hosts/:id/monitoring-status", handler.GetHostMonitoringStatus)
	response := httptest.NewRecorder()
	path := fmt.Sprintf("/hosts/%d/monitoring-status?prometheus_id=%d&component_id=%d", targetHost.ID, prometheus.ID, exporter.ID)
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	for _, required := range []string{`"checked_at"`, `"passed":true`, `"scope"`, `"checks"`, `"targets"`, `"prometheus_id"`, `"message"`} {
		if !bytes.Contains(response.Body.Bytes(), []byte(required)) {
			t.Fatalf("response missing %s: %s", required, response.Body.String())
		}
	}
	for _, model := range []interface{}{&models.OnboardingCheckRun{}, &models.Backup{}} {
		var count int64
		if err := db.Model(model).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("read-only acceptance wrote %T rows: %d", model, count)
		}
	}
}

func TestHostAcceptanceHandlerReturnsInternalServerErrorForDatabaseFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := acceptanceHandlerDB(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	handler := &Handler{db: db}
	router := gin.New()
	router.GET("/hosts/:id/monitoring-status", handler.GetHostMonitoringStatus)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/hosts/1/monitoring-status", nil))
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
