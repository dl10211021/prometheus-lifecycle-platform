package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/api/middleware"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
)

func TestPasswordResetsRevokeTokensAndRejectOverlength(t *testing.T) {
	db, handler := userHandlerTestSetup(t)
	user := models.User{Username: "revocation-user", Password: utils.HashPassword("old-password"), Role: "system_admin", IsActive: true}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	router.POST("/login", middleware.Login(db))
	auth := router.Group("", middleware.AuthRequired(db))
	auth.GET("/me", middleware.Me)
	auth.POST("/password", middleware.ChangePassword(db))
	auth.PUT("/users/:id", handler.UpdateUser)
	request := func(method, path string, body any, token string) *httptest.ResponseRecorder {
		req := jsonRequest(t, method, path, body)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	login := func(password string) string {
		rec := request("POST", "/login", gin.H{"username": user.Username, "password": password}, "")
		if rec.Code != 200 {
			t.Fatalf("login: %d %s", rec.Code, rec.Body.String())
		}
		var result struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result.Token
	}
	token := login("old-password")
	path := "/users/" + strconv.Itoa(int(user.ID))
	rejected := request("PUT", path, gin.H{"password": strings.Repeat("密", 25)}, token)
	if rejected.Code != 400 {
		t.Fatalf("long reset: %d %s", rejected.Code, rejected.Body.String())
	}
	var saved models.User
	if err := db.First(&saved, user.ID).Error; err != nil {
		t.Fatal(err)
	}
	if saved.Password != user.Password || saved.SessionVersion != user.SessionVersion {
		t.Fatal("failed reset mutated credentials")
	}
	if rec := request("POST", "/password", gin.H{"current_password": "old-password", "new_password": "self-password"}, token); rec.Code != 200 {
		t.Fatalf("self change: %d %s", rec.Code, rec.Body.String())
	}
	if rec := request("GET", "/me", nil, token); rec.Code != 401 {
		t.Fatalf("old self token accepted: %d", rec.Code)
	}
	token = login("self-password")
	if rec := request("PUT", path, gin.H{"password": "admin-password"}, token); rec.Code != 200 {
		t.Fatalf("admin reset: %d %s", rec.Code, rec.Body.String())
	}
	if rec := request("GET", "/me", nil, token); rec.Code != 401 {
		t.Fatalf("old admin-reset token accepted: %d", rec.Code)
	}
	login("admin-password")
}

func TestViewerConfigCopiesAndAuthorizedRawEditing(t *testing.T) {
	db, handler := userHandlerTestSetup(t)
	if err := db.AutoMigrate(&models.Cluster{}, &models.Host{}, &models.Component{}, &models.ComponentConfig{}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "prometheus.yml")
	content := "remote_write:\n- authorization:\n    credentials: CANARY_CONFIG\n  url: https://example.com/write\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	host := models.Host{Name: "security-host", IP: "127.0.0.1", ConnectionType: "local"}
	if err := db.Create(&host).Error; err != nil {
		t.Fatal(err)
	}
	comp := models.Component{Name: "security-prom", Type: models.ComponentPrometheus, HostID: host.ID, ConfigPath: path}
	if err := db.Create(&comp).Error; err != nil {
		t.Fatal(err)
	}
	cfg := models.ComponentConfig{ComponentID: comp.ID, Name: "prometheus.yml", ContentType: "yaml", Content: content, Version: 1, IsActive: true}
	if err := db.Create(&cfg).Error; err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("role", c.GetHeader("X-Test-Role")) })
	router.GET("/components/:id/config/current", handler.GetCurrentComponentConfig)
	router.GET("/components/:id/configs", handler.ListComponentConfigs)
	router.GET("/configs/:id", handler.GetComponentConfig)
	router.PUT("/components/:id/config", handler.UpdateComponentConfig)
	paths := []string{"/components/" + strconv.Itoa(int(comp.ID)) + "/config/current", "/components/" + strconv.Itoa(int(comp.ID)) + "/configs", "/configs/" + strconv.Itoa(int(cfg.ID))}
	for _, path := range paths {
		for _, role := range []string{"viewer", "auditor", "operator", "system_admin"} {
			req := httptest.NewRequest("GET", path, nil)
			req.Header.Set("X-Test-Role", role)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != 200 || strings.Contains(rec.Body.String(), "CANARY_CONFIG") {
				t.Fatalf("%s %s: %d %s", role, path, rec.Code, rec.Body.String())
			}
		}
		req := httptest.NewRequest("GET", path+"?raw=1", nil)
		req.Header.Set("X-Test-Role", "viewer")
		denied := httptest.NewRecorder()
		router.ServeHTTP(denied, req)
		if denied.Code != 403 {
			t.Fatalf("viewer raw read: %d", denied.Code)
		}
		req = httptest.NewRequest("GET", path+"?raw=1", nil)
		req.Header.Set("X-Test-Role", "change_admin")
		raw := httptest.NewRecorder()
		router.ServeHTTP(raw, req)
		if raw.Code != 200 || !strings.Contains(raw.Body.String(), "CANARY_CONFIG") {
			t.Fatalf("authorized edit source damaged: %d %s", raw.Code, raw.Body.String())
		}
	}
	comp.Configs = []models.ComponentConfig{cfg}
	nested, _ := json.Marshal(publicComponent(comp))
	if strings.Contains(string(nested), "CANARY_CONFIG") {
		t.Fatalf("nested component leaked: %s", nested)
	}
	req := jsonRequest(t, "PUT", "/components/"+strconv.Itoa(int(comp.ID))+"/config", gin.H{"name": cfg.Name, "content": "password: [REDACTED]", "reason": "edit"})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("redacted preview accepted for publication: %d %s", rec.Code, rec.Body.String())
	}
	var saved models.ComponentConfig
	if err := db.First(&saved, cfg.ID).Error; err != nil {
		t.Fatal(err)
	}
	if saved.Content != content {
		t.Fatal("rejected edit corrupted stored credentials")
	}
}

func TestProxyRequestHeadersDoNotCrossAuthenticationBoundary(t *testing.T) {
	captured := make(chan http.Header, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { captured <- r.Header.Clone(); w.WriteHeader(204) }))
	defer upstream.Close()
	req, _ := http.NewRequest("POST", upstream.URL, bytes.NewReader([]byte(`{}`)))
	incoming := http.Header{"Authorization": {"Bearer PLATFORM_CANARY"}, "Cookie": {"session=COOKIE_CANARY"}, "Proxy-Authorization": {"PROXY_CANARY"}, "X-Api-Key": {"KEY_CANARY"}, "Content-Type": {"application/json"}, "Accept": {"application/json"}}
	copyAlertmanagerRequestHeaders(req.Header, incoming)
	resp, err := upstream.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	headers := <-captured
	encoded, _ := json.Marshal(headers)
	if strings.Contains(string(encoded), "CANARY") {
		t.Fatalf("upstream received platform credentials: %s", encoded)
	}
	if headers.Get("Content-Type") != "application/json" {
		t.Fatal("upstream lost content negotiation")
	}
}

func TestAIContextWithUnknownCredentialStructuresFailsClosed(t *testing.T) {
	db := newAIHandlerTestDB(t, &models.ComponentConfig{})
	cfg := models.ComponentConfig{ComponentID: 9, Name: "prometheus.yml", ContentType: "yaml", Content: "authorization:\n  credentials: CANARY_AUTH\ncustom_login:\n  opaque: CANARY_UNKNOWN\n", IsActive: true}
	if err := db.Create(&cfg).Error; err != nil {
		t.Fatal(err)
	}
	id := uint(9)
	value, _, err := NewAIHandler(db).buildTrustedContextWithEvidence(context.Background(), "config", &id)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(value, "CANARY") {
		t.Fatalf("AI context leaked: %s", value)
	}
}

func TestChangePlanOffsetPagesAndRawDraftAuthorization(t *testing.T) {
	db, handler := userHandlerTestSetup(t)
	if err := db.AutoMigrate(&models.ChangePlan{}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		plan := models.ChangePlan{Kind: models.ChangeKindConfig, ResourceType: "component", ResourceID: 1, RequestedByID: 1, RequestedBy: "admin", Status: models.ChangeStatusDraft, Reason: "test", Before: json.RawMessage(`{}`), After: json.RawMessage(`{"content_type":"yaml","content":"authorization:\n  credentials: CANARY_DRAFT"}`)}
		if err := db.Create(&plan).Error; err != nil {
			t.Fatal(err)
		}
	}
	plans := NewChangePlanHandler(handler.changeService())
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("role", c.GetHeader("X-Test-Role")) })
	router.GET("/plans", plans.List)
	router.GET("/plans/:id", plans.Get)
	fetch := func(path, role string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("X-Test-Role", role)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	var firstID uint
	for offset := range 2 {
		rec := fetch("/plans?limit=1&offset="+strconv.Itoa(offset), "viewer")
		if rec.Code != 200 || strings.Contains(rec.Body.String(), "CANARY") {
			t.Fatalf("page: %d %s", rec.Code, rec.Body.String())
		}
		var page struct {
			Plans  []models.ChangePlan `json:"plans"`
			Total  int                 `json:"total"`
			Offset int                 `json:"offset"`
			Limit  int                 `json:"limit"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		if page.Total != 3 || page.Offset != offset || page.Limit != 1 || len(page.Plans) != 1 {
			t.Fatalf("wrong page: %+v", page)
		}
		if offset == 0 {
			firstID = page.Plans[0].ID
		} else if firstID == page.Plans[0].ID {
			t.Fatal("offset repeated first plan")
		}
	}
	path := "/plans/" + strconv.Itoa(int(firstID)) + "?raw=1"
	if rec := fetch(path, "viewer"); rec.Code != 403 {
		t.Fatalf("viewer raw draft: %d", rec.Code)
	}
	if rec := fetch(path, "change_admin"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "CANARY_DRAFT") {
		t.Fatalf("draft edit source lost: %d %s", rec.Code, rec.Body.String())
	}
	if rec := fetch("/plans?offset=-1", "viewer"); rec.Code != 400 {
		t.Fatalf("negative offset accepted: %d", rec.Code)
	}
}
