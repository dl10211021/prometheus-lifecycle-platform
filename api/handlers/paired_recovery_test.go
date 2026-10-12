package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/api/middleware"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/services"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type pairedRecoveryHandlerService struct {
	transaction  *models.PrometheusRestoreTransaction
	preview      *services.PrometheusPairedRestorePreview
	getErr       error
	previewErr   error
	restoreErr   error
	getCalls     int
	previewCalls int
	restoreCalls int
}

func (s *pairedRecoveryHandlerService) GetPrometheusPairedRestoreTransaction(context.Context, uint) (*models.PrometheusRestoreTransaction, error) {
	s.getCalls++
	return s.transaction, s.getErr
}
func (s *pairedRecoveryHandlerService) PreviewPrometheusRecoveryPointRestore(context.Context, uint) (*services.PrometheusPairedRestorePreview, error) {
	s.previewCalls++
	return s.preview, s.previewErr
}
func (s *pairedRecoveryHandlerService) RestorePrometheusRecoveryPoint(context.Context, uint) (*models.PrometheusRestoreTransaction, error) {
	s.restoreCalls++
	return s.transaction, s.restoreErr
}

func pairedRecoveryTestRouter(service pairedRecoveryService, role string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("role", role); c.Next() })
	api := router.Group("/api/v1")
	read := api.Group("")
	read.Use(middleware.RequireRole("viewer", "operator", "change_admin", "system_admin", "auditor"))
	write := api.Group("")
	write.Use(middleware.RequireRole("change_admin", "system_admin"))
	RegisterPairedRecoveryRoutes(read, write, NewPairedRecoveryHandler(service))
	return router
}
func pairedRecoveryRequest(router http.Handler, method, path string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(method, path, nil))
	return response
}
func decodeObject(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v: %s", err, response.Body.String())
	}
	return body
}

func TestPairedRecoveryRoutesAllowReadRolesAndGateExecution(t *testing.T) {
	preview := &services.PrometheusPairedRestorePreview{PointID: 9, PlanID: 7, ComponentID: 4, HostID: 3, CurrentVersion: "2.55.0", HistoricalVersion: "2.54.1", RequiresDowntime: true, RequiresCurrentCheckpoint: true}
	viewerService := &pairedRecoveryHandlerService{preview: preview}
	viewer := pairedRecoveryTestRouter(viewerService, "viewer")
	if response := pairedRecoveryRequest(viewer, http.MethodGet, "/api/v1/change-plans/7/paired-recovery"); response.Code != http.StatusOK {
		t.Fatalf("viewer GET: %d %s", response.Code, response.Body.String())
	}
	if response := pairedRecoveryRequest(viewer, http.MethodPost, "/api/v1/change-plans/7/paired-recovery"); response.Code != http.StatusForbidden {
		t.Fatalf("viewer POST: %d %s", response.Code, response.Body.String())
	}
	if viewerService.restoreCalls != 0 {
		t.Fatalf("forbidden POST reached recovery service: %d", viewerService.restoreCalls)
	}

	adminService := &pairedRecoveryHandlerService{transaction: &models.PrometheusRestoreTransaction{ID: 11, PlanID: 7, PointID: 9, ComponentID: 4, HostID: 3, Phase: "restore_completed", Revision: 12}}
	admin := pairedRecoveryTestRouter(adminService, "change_admin")
	if response := pairedRecoveryRequest(admin, http.MethodPost, "/api/v1/change-plans/7/paired-recovery"); response.Code != http.StatusOK {
		t.Fatalf("change_admin POST: %d %s", response.Code, response.Body.String())
	}
	if adminService.restoreCalls != 1 {
		t.Fatalf("authorized POST calls = %d", adminService.restoreCalls)
	}
}

func TestPairedRecoveryGetValidatesIDsAndReportsEligibility(t *testing.T) {
	for _, id := range []string{"not-a-number", "0"} {
		service := &pairedRecoveryHandlerService{}
		response := pairedRecoveryRequest(pairedRecoveryTestRouter(service, "viewer"), http.MethodGet, "/api/v1/change-plans/"+id+"/paired-recovery")
		if response.Code != http.StatusBadRequest || service.getCalls != 0 {
			t.Fatalf("invalid id %q: %d calls=%d body=%s", id, response.Code, service.getCalls, response.Body.String())
		}
	}
	missing := &pairedRecoveryHandlerService{getErr: gorm.ErrRecordNotFound}
	response := pairedRecoveryRequest(pairedRecoveryTestRouter(missing, "viewer"), http.MethodGet, "/api/v1/change-plans/71/paired-recovery")
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing plan: %d %s", response.Code, response.Body.String())
	}

	unsupported := &pairedRecoveryHandlerService{previewErr: errors.New("unsupported /private/path token=secret")}
	response = pairedRecoveryRequest(pairedRecoveryTestRouter(unsupported, "viewer"), http.MethodGet, "/api/v1/change-plans/72/paired-recovery")
	body := decodeObject(t, response)
	if response.Code != http.StatusOK || body["eligible"] != false {
		t.Fatalf("unsupported history: %d %#v", response.Code, body)
	}
	if strings.Contains(response.Body.String(), "/private/path") || strings.Contains(response.Body.String(), "secret") {
		t.Fatalf("unsupported diagnostic leaked: %s", response.Body.String())
	}
}

func TestPairedRecoveryGetDistinguishesExistingPlanWithoutPointFromMissingPlan(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:paired-recovery-eligibility?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&models.Host{},
		&models.Component{},
		&models.ChangePlan{},
		&models.PrometheusRecoveryPoint{},
		&models.PrometheusRestoreTransaction{},
		&models.ComponentOperationClaim{},
		&models.HostOperationClaim{},
		&services.ComponentBackupPause{},
	); err != nil {
		t.Fatal(err)
	}
	host := models.Host{Name: "paired-eligibility-host", IP: "127.0.0.1", ConnectionType: "local", Username: "root"}
	if err := db.Create(&host).Error; err != nil {
		t.Fatal(err)
	}
	component := models.Component{
		Name: "paired-eligibility-prometheus", HostID: host.ID, Type: models.ComponentPrometheus,
		DeployType: "binary", Version: "2.55.0", Status: models.StatusRunning,
	}
	if err := db.Create(&component).Error; err != nil {
		t.Fatal(err)
	}
	plan := models.ChangePlan{
		Kind: models.ChangeKindUpgrade, ResourceType: "component", ResourceID: component.ID,
		Status: models.ChangeStatusVerified, Before: []byte("{}"), After: []byte("{}"),
	}
	if err := db.Create(&plan).Error; err != nil {
		t.Fatal(err)
	}
	service := services.NewBackupService(db)
	router := pairedRecoveryTestRouter(service, "viewer")

	response := pairedRecoveryRequest(router, http.MethodGet, "/api/v1/change-plans/"+strconv.FormatUint(uint64(plan.ID), 10)+"/paired-recovery")
	body := decodeObject(t, response)
	if response.Code != http.StatusOK || body["eligible"] != false || body["error"] != nil {
		t.Fatalf("existing plan without paired point: %d %#v", response.Code, body)
	}

	response = pairedRecoveryRequest(router, http.MethodGet, "/api/v1/change-plans/999999/paired-recovery")
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing plan: %d %s", response.Code, response.Body.String())
	}

	if err := db.Migrator().DropTable(&models.PrometheusRecoveryPoint{}); err != nil {
		t.Fatal(err)
	}
	proof, err := service.PreviewPrometheusRecoveryPointRestore(context.Background(), plan.ID)
	if proof != nil || err == nil || errors.Is(err, services.ErrPrometheusPairedRecoveryPointNotFound) || errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("database failure misclassified as absent point: proof=%+v err=%v", proof, err)
	}
}

func TestPairedRecoveryGetReturnsSafePreviewAndTransaction(t *testing.T) {
	finished := time.Date(2026, 9, 17, 4, 5, 6, 0, time.UTC)
	transaction := &models.PrometheusRestoreTransaction{ID: 12, PlanID: 7, PointID: 9, BackupID: 99, ComponentID: 4, HostID: 3, Token: "secret-token", Phase: "restore_compensated", Revision: 14, Payload: json.RawMessage(`{"command":"rm -rf /"}`), Checkpoint: json.RawMessage(`{"path":"/private"}`), Stage: json.RawMessage(`{"token":"stage"}`), Execution: json.RawMessage(`{"token":"execution"}`), CreatedAt: finished.Add(-time.Minute), UpdatedAt: finished, FinishedAt: &finished}
	service := &pairedRecoveryHandlerService{transaction: transaction}
	response := pairedRecoveryRequest(pairedRecoveryTestRouter(service, "viewer"), http.MethodGet, "/api/v1/change-plans/7/paired-recovery")
	body := decodeObject(t, response)
	if response.Code != http.StatusOK || body["eligible"] != true || service.previewCalls != 0 {
		t.Fatalf("transaction status response: %d %#v previewCalls=%d", response.Code, body, service.previewCalls)
	}
	tx, ok := body["transaction"].(map[string]any)
	if !ok {
		t.Fatalf("missing transaction: %#v", body)
	}
	allowed := map[string]bool{"id": true, "plan_id": true, "point_id": true, "component_id": true, "host_id": true, "phase": true, "revision": true, "created_at": true, "updated_at": true, "finished_at": true}
	for key := range tx {
		if !allowed[key] {
			t.Fatalf("unsafe transaction field %q in %#v", key, tx)
		}
	}
	for _, secret := range []string{"secret-token", "rm -rf", "/private", "backup_id", "payload", "checkpoint", "stage", "execution"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("private transaction data %q leaked: %s", secret, response.Body.String())
		}
	}
	preview := &services.PrometheusPairedRestorePreview{PointID: 9, PlanID: 8, BackupID: 101, ComponentID: 4, HostID: 3, CurrentVersion: "2.55.0", HistoricalVersion: "2.54.1", Scope: "paired_restore_admission_preview_only", RequiresDowntime: true, RequiresCurrentCheckpoint: true, Replacements: []services.PrometheusPairedRestoreReplacement{{Path: "/secret/live/path", Role: "binary", Action: "replace"}}, Checks: []string{"trusted_paired_history"}, ObservedAt: finished}
	service = &pairedRecoveryHandlerService{preview: preview}
	response = pairedRecoveryRequest(pairedRecoveryTestRouter(service, "viewer"), http.MethodGet, "/api/v1/change-plans/8/paired-recovery")
	body = decodeObject(t, response)
	safePreview, ok := body["preview"].(map[string]any)
	if response.Code != http.StatusOK || body["eligible"] != true || !ok || safePreview["replacement_count"] != float64(1) {
		t.Fatalf("preview response: %d %#v", response.Code, body)
	}
	if strings.Contains(response.Body.String(), "/secret/live/path") || strings.Contains(response.Body.String(), "backup_id") {
		t.Fatalf("preview path or backup id leaked: %s", response.Body.String())
	}
}

func TestPairedRecoveryPostReturnsSafeDurableOutcomeOnControlledFailure(t *testing.T) {
	transaction := &models.PrometheusRestoreTransaction{ID: 15, PlanID: 7, PointID: 9, BackupID: 99, ComponentID: 4, HostID: 3, Token: "secret", Phase: "restore_compensated", Revision: 19}
	service := &pairedRecoveryHandlerService{transaction: transaction, restoreErr: errors.New("remote command /private/path token=secret failed")}
	response := pairedRecoveryRequest(pairedRecoveryTestRouter(service, "system_admin"), http.MethodPost, "/api/v1/change-plans/7/paired-recovery")
	body := decodeObject(t, response)
	if response.Code != http.StatusConflict || body["error"] == nil {
		t.Fatalf("controlled failure: %d %#v", response.Code, body)
	}
	tx, ok := body["transaction"].(map[string]any)
	if !ok || tx["phase"] != "restore_compensated" {
		t.Fatalf("durable outcome missing: %#v", body)
	}
	for _, secret := range []string{"/private/path", "token=secret", "backup_id"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("failure leaked %q: %s", secret, response.Body.String())
		}
	}
}
