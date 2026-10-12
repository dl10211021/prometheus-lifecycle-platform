package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/services"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestComponentInspectionEndpointMapping(t *testing.T) {
	component := models.Component{InternalURL: "http://127.0.0.1:9090", Type: models.ComponentPrometheus}
	if got := services.ComponentInspectionEndpoint(&component); got != "http://127.0.0.1:9090/-/ready" {
		t.Fatalf("prometheus endpoint=%q", got)
	}
	component.Type = models.ComponentWindowsExporter
	component.InternalURL = ""
	component.Host.IP = "192.0.2.20"
	component.Port = 9182
	if got := services.ComponentInspectionEndpoint(&component); got != "http://192.0.2.20:9182/metrics" {
		t.Fatalf("windows exporter endpoint=%q", got)
	}
}

func TestRunComponentInspectionRecordsPassAndFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "inspection-handler.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.Host{}, &models.Cluster{}, &models.Component{}, &models.Operation{}, &models.InspectionRun{}, &models.InspectionCheck{}, &models.InspectionClaim{}); err != nil {
		t.Fatal(err)
	}
	host := models.Host{Name: "inspection-host", IP: "192.0.2.30", OS: "linux", ConnectionType: "ssh"}
	cluster := models.Cluster{Name: "single-node"}
	if err := db.Create(&host).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&cluster).Error; err != nil {
		t.Fatal(err)
	}
	passing := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusOK)
	}))
	defer passing.Close()
	failing := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusInternalServerError)
	}))
	defer failing.Close()
	components := []models.Component{
		{HostID: host.ID, ClusterID: cluster.ID, Name: "passing-target", Type: models.ComponentNodeExporter, DeployType: "target", InternalURL: passing.URL},
		{HostID: host.ID, ClusterID: cluster.ID, Name: "failing-target", Type: models.ComponentNodeExporter, DeployType: "target", InternalURL: failing.URL},
	}
	if err := db.Create(&components).Error; err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/inspections/run", nil)
	context, _ := gin.CreateTestContext(recorder)
	context.Request = request
	(&Handler{db: db}).RunComponentInspection(context)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Summary struct {
			Total  int `json:"total"`
			Passed int `json:"passed"`
			Failed int `json:"failed"`
		} `json:"summary"`
		Run    models.InspectionRun     `json:"run"`
		Checks []models.InspectionCheck `json:"checks"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Summary.Total != 2 || response.Summary.Passed != 1 || response.Summary.Failed != 1 {
		t.Fatalf("unexpected summary: %#v", response.Summary)
	}
	if response.Run.ID == 0 || response.Run.Status != "completed" || response.Run.Result != "issues" {
		t.Fatalf("unexpected persisted run: %#v", response.Run)
	}
	var operationCount int64
	if err := db.Model(&models.Operation{}).Where("type = ?", models.OpHealthCheck).Count(&operationCount).Error; err != nil {
		t.Fatal(err)
	}
	if operationCount != 2 {
		t.Fatalf("health-check operations=%d", operationCount)
	}
	var runCount, checkCount int64
	if err := db.Model(&models.InspectionRun{}).Count(&runCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.InspectionCheck{}).Where("run_id = ?", response.Run.ID).Count(&checkCount).Error; err != nil {
		t.Fatal(err)
	}
	if runCount != 1 || checkCount != 2 {
		t.Fatalf("persisted runs=%d checks=%d", runCount, checkCount)
	}

	detailRecorder := httptest.NewRecorder()
	detailContext, _ := gin.CreateTestContext(detailRecorder)
	detailContext.Params = gin.Params{{Key: "id", Value: strconv.FormatUint(uint64(response.Run.ID), 10)}}
	detailContext.Request = httptest.NewRequest(http.MethodGet, "/api/v1/inspections/1", nil)
	(&Handler{db: db}).GetInspectionRun(detailContext)
	if detailRecorder.Code != http.StatusOK || !strings.Contains(detailRecorder.Body.String(), "passing-target") {
		t.Fatalf("inspection detail status=%d body=%s", detailRecorder.Code, detailRecorder.Body.String())
	}
}
