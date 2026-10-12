package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestValidateCustomEnvironment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{name: "empty", raw: ""},
		{name: "object", raw: `{"LOG_LEVEL":"info","MESSAGE":"hello world"}`},
		{name: "malformed JSON", raw: "not-json", wantErr: true},
		{name: "null is not an object", raw: "null", wantErr: true},
		{name: "non-string value", raw: `{"COUNT":3}`, wantErr: true},
		{name: "leading digit", raw: `{"9INVALID":"value"}`, wantErr: true},
		{name: "directive injection", raw: "{\"BAD\\nExecStart\":\"/bin/false\"}", wantErr: true},
		{name: "NUL value", raw: `{"BAD":"\u0000"}`, wantErr: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateCustomEnvironment(tt.raw)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateCustomEnvironment(%q) error = %v, wantErr %v", tt.raw, err, tt.wantErr)
			}
		})
	}
}

func TestRemainingHandlersRejectInvalidIDsBeforeDatabaseAccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	operations := &OpsHandler{}
	notifications := &NotificationHandler{}
	tests := []struct {
		method  string
		path    string
		pattern string
		handler gin.HandlerFunc
	}{
		{http.MethodPost, "/notifications/channels/not-a-number/test", "/notifications/channels/:id/test", notifications.TestChannel},
		{http.MethodGet, "/components/not-a-number/logs/stream", "/components/:id/logs/stream", operations.StreamLogs},
		{http.MethodDelete, "/maintenance/not-a-number", "/maintenance/:id", operations.DeleteMaintenanceWindow},
		{http.MethodPut, "/backups/scheduled/not-a-number", "/backups/scheduled/:id", operations.UpdateScheduledBackup},
		{http.MethodDelete, "/backups/scheduled/not-a-number", "/backups/scheduled/:id", operations.DeleteScheduledBackup},
		{http.MethodGet, "/components/not-a-number/resource", "/components/:id/resource", operations.GetResourceUsage},
		{http.MethodGet, "/nodes/not-a-number/resource", "/nodes/:id/resource", operations.GetClusterResourceUsage},
	}
	for _, test := range tests {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			router := gin.New()
			router.Handle(test.method, test.pattern, test.handler)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(test.method, test.path, nil))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestConfigValidationNativeRejectionNeverFallsBack(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := &ConfigValidateHandler{}
	endpoints := []struct {
		tool    string
		content string
		handle  gin.HandlerFunc
	}{
		{
			tool:    "promtool",
			content: "scrape_configs:\n  - job_name: test\n    static_configs:\n      - targets: ['localhost:9090']\n",
			handle:  handler.ValidatePrometheusConfig,
		},
		{
			tool:    "amtool",
			content: "route:\n  receiver: default\nreceivers:\n  - name: default\n",
			handle:  handler.ValidateAlertmanagerConfig,
		},
	}
	for _, endpoint := range endpoints {
		t.Run(endpoint.tool, func(t *testing.T) {
			for _, scenario := range []struct {
				name       string
				script     string
				wantStatus int
				wantMethod string
			}{
				{
					name:       "native success",
					script:     "#!/bin/sh\nexit 0\n",
					wantStatus: http.StatusOK,
					wantMethod: endpoint.tool,
				},
				{
					name:       "semantic not found rejection",
					script:     "#!/bin/sh\necho 'checking candidate configuration'\necho 'referenced file not found' >&2\nexit 1\n",
					wantStatus: http.StatusConflict,
					wantMethod: endpoint.tool,
				},
				{
					name:       "missing checker",
					wantStatus: http.StatusOK,
					wantMethod: "basic",
				},
			} {
				t.Run(scenario.name, func(t *testing.T) {
					binDir := t.TempDir()
					t.Setenv("PATH", binDir)
					if scenario.script != "" {
						if err := os.WriteFile(filepath.Join(binDir, endpoint.tool), []byte(scenario.script), 0700); err != nil {
							t.Fatal(err)
						}
					}
					payload, err := json.Marshal(gin.H{"cluster_id": 1, "content": endpoint.content})
					if err != nil {
						t.Fatal(err)
					}
					router := gin.New()
					router.POST("/validate", endpoint.handle)
					response := httptest.NewRecorder()
					request := httptest.NewRequest(http.MethodPost, "/validate", bytes.NewReader(payload))
					request.Header.Set("Content-Type", "application/json")
					router.ServeHTTP(response, request)
					if response.Code != scenario.wantStatus {
						t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
					}
					var result struct {
						Valid  bool   `json:"valid"`
						Method string `json:"method"`
						Error  string `json:"error"`
					}
					if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
						t.Fatal(err)
					}
					if result.Valid != (scenario.wantStatus == http.StatusOK) || result.Method != scenario.wantMethod {
						t.Fatalf("unexpected validation result: %+v", result)
					}
					if scenario.wantStatus == http.StatusConflict {
						for _, diagnostic := range []string{"checking candidate configuration", "referenced file not found"} {
							if !strings.Contains(result.Error, diagnostic) {
								t.Fatalf("missing diagnostic %q in %q", diagnostic, result.Error)
							}
						}
					}
				})
			}
		})
	}
}

func TestConfigValidationBasicFallbackRejectsMalformedStructure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("PATH", t.TempDir())
	handler := &ConfigValidateHandler{}
	for _, scenario := range []struct {
		name    string
		content string
		handle  gin.HandlerFunc
	}{
		{"static config is not a mapping", "scrape_configs:\n  - job_name: test\n    static_configs: [broken]\n", handler.ValidatePrometheusConfig},
		{"remote write is not a mapping", "scrape_configs:\n  - job_name: test\nremote_write: [broken]\n", handler.ValidatePrometheusConfig},
		{"route is not a mapping", "route: null\n", handler.ValidateAlertmanagerConfig},
		{"root receiver is absent", "route: {}\nreceivers:\n  - name: default\n", handler.ValidateAlertmanagerConfig},
		{"root receiver is undefined", "route:\n  receiver: missing\nreceivers:\n  - name: default\n", handler.ValidateAlertmanagerConfig},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			payload, err := json.Marshal(gin.H{"content": scenario.content})
			if err != nil {
				t.Fatal(err)
			}
			router := gin.New()
			router.POST("/validate", scenario.handle)
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/validate", bytes.NewReader(payload))
			request.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(response, request)
			if response.Code != http.StatusConflict {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			var result struct {
				Valid  bool   `json:"valid"`
				Method string `json:"method"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Valid || result.Method != "basic" {
				t.Fatalf("unexpected validation result: %+v", result)
			}
		})
	}
}
