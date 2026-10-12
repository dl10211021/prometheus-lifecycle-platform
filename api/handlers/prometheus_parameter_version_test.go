package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestPrometheusParameterVersionNormalization(t *testing.T) {
	for _, tc := range []struct{ name, version, args, want string }{
		{"retention before introduction", "2.6.1", "", "storage.tsdb.retention.time"},
		{"wal before introduction", "2.10.9", "", "wal-compression"},
		{"first default-compatible release", "2.11.0", "", ""},
		{"modern release preserves value", "3.1.0", "--query.timeout=7m", ""},
		{"removed legacy flag", "3.1.0", "--storage.tsdb.retention=90d", "storage.tsdb.retention"},
		{"conflicting aliases", "2.55.0", "--storage.tsdb.retention=90d", "冲突"},
		{"unknown version", "latest", "", "正式版本"},
		{"prerelease", "3.1.0-rc.1", "", "正式版本"},
		{"future major", "4.0.0", "", "正式版本"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := componentCreateRequest{Type: "prometheus", Name: "prom", Version: tc.version, CustomArgs: tc.args, Parameters: map[string]interface{}{"retention_time": "45d"}}
			_, err := (&Handler{}).normalizeDeploymentRequest(&request, nil)
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("error=%v, want %q", err, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if request.StructuredParameters["retention_time"] != "45d" || request.CustomArgs != tc.args {
				t.Fatal("normalization changed explicit values")
			}
		})
	}
}

// A schema rejection must happen before any DB lookup, SSH access or credential issuance.
func TestPrometheusParameterVersionPreflightRejectsBeforeAccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/preflight", strings.NewReader(`{"type":"prometheus","name":"prom","version":"3.1.0","custom_args":"--storage.tsdb.retention=90d"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	(&Handler{}).PreflightComponentDeployment(c)
	var response map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || response["passed"] != false {
		t.Fatalf("unexpected response %d %s", rec.Code, rec.Body.String())
	}
	if _, ok := response["preflight_token"]; ok {
		t.Fatal("incompatible request got execution credential")
	}
	checks, ok := response["checks"].([]interface{})
	if !ok || len(checks) == 0 {
		t.Fatal("missing checks")
	}
	first := checks[0].(map[string]interface{})
	if first["key"] != "schema" || first["status"] != "fail" || !strings.Contains(first["message"].(string), "storage.tsdb.retention") {
		t.Fatalf("missing parameter diagnosis: %s", rec.Body.String())
	}
}

func TestPrometheusParameterVersionCatalogEvidence(t *testing.T) {
	definition, err := deploymentDefinitionForRequest("prometheus", nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(definition)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Parameters []struct {
			Field       string                 `json:"field"`
			VersionRule map[string]interface{} `json:"version_rule"`
		}
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, parameter := range decoded.Parameters {
		if parameter.Field != "retention_time" && parameter.Field != "retention_size" {
			continue
		}
		count++
		if parameter.VersionRule["minimum_version"] != "2.7.0" || parameter.VersionRule["evidence"] == "" || parameter.VersionRule["evidence"] == nil {
			t.Fatalf("missing usable version evidence for %s: %#v", parameter.Field, parameter.VersionRule)
		}
	}
	if count != 2 {
		t.Fatalf("missing retention parameters: %d", count)
	}
}
