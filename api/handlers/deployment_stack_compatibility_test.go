package handlers

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promlifecycle/platform/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestStackPlannedDependencyVersions(t *testing.T) {
	for _, tt := range []struct {
		name, plan, versions, existing, want, compatibility string
	}{
		{"future incompatible overrides running", `["prometheus","thanos"]`, `{"prometheus":"3.0.0","thanos":"0.36.1"}`, "2.55.0", "fail", "incompatible"},
		{"compatible future overrides running", `["prometheus","thanos"]`, `{"prometheus":"2.55.0","thanos":"0.36.1"}`, "3.0.0", "pass", "compatible"},
		{"missing version", `["prometheus","thanos"]`, `{}`, "2.55.0", "fail", ""},
		{"unknown version", `["prometheus","thanos"]`, `{"prometheus":"unknown","thanos":"0.36.1"}`, "", "fail", "unknown"},
		{"wrong order even if running", `["thanos","prometheus"]`, `{"prometheus":"2.55.0","thanos":"0.36.1"}`, "2.55.0", "fail", ""},
		{"duplicate dependency", `["prometheus","thanos","prometheus"]`, `{"prometheus":"2.55.0","thanos":"0.36.1"}`, "2.55.0", "fail", ""},
		{"unlisted consumer", `["prometheus"]`, `{"prometheus":"2.55.0"}`, "2.55.0", "fail", ""},
		{"map alone is no proof", `[]`, `{"prometheus":"2.55.0"}`, "", "fail", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "plan.db")), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			sqlDB, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = sqlDB.Close() })
			if err = db.AutoMigrate(&models.Component{}); err != nil {
				t.Fatal(err)
			}
			host := models.Host{ID: 7}
			if tt.existing != "" {
				if err = db.Create(&models.Component{HostID: host.ID, Name: "existing", Type: models.ComponentPrometheus, Status: models.StatusRunning, DeployType: "binary", Version: tt.existing}).Error; err != nil {
					t.Fatal(err)
				}
			}
			var request componentCreateRequest
			if err = json.Unmarshal([]byte(`{"type":"thanos","version":"0.36.1","planned_components":`+tt.plan+`,"planned_versions":`+tt.versions+`}`), &request); err != nil {
				t.Fatal(err)
			}
			var checks []deploymentPreflightCheck
			(&Handler{db: db}).preflightDependencies(&request, &host, &checks)
			if len(checks) != 1 || checks[0].Status != tt.want || !checks[0].Blocking {
				t.Fatalf("checks=%+v, want %s", checks, tt.want)
			}
			if tt.compatibility != "" {
				raw, err := json.Marshal(checks[0].Evidence)
				if err != nil {
					t.Fatal(err)
				}
				var evidence map[string]interface{}
				if err = json.Unmarshal(raw, &evidence); err != nil {
					t.Fatal(err)
				}
				if evidence["source"] != "planned" || evidence["compatibility"] != tt.compatibility {
					t.Fatalf("wrong version evidence: %s", raw)
				}
				if _, ok := evidence["dependency_id"]; ok {
					t.Fatalf("planned dependency must not claim an existing instance: %s", raw)
				}
			}
		})
	}
}

func TestStackPlannedVersionsBoundToCredential(t *testing.T) {
	h, request, definition, host, cleanup := preflightCredentialFixture(t)
	defer cleanup()
	if err := json.Unmarshal([]byte(`{"planned_versions":{"prometheus":"2.55.0"}}`), &request); err != nil {
		t.Fatal(err)
	}
	credential, err := h.issueDeploymentPreflight(request, definition, nil, nil, nil, "tester")
	if err != nil {
		t.Fatal(err)
	}
	request.PreflightToken = credential.Token
	if err = json.Unmarshal([]byte(`{"planned_versions":{"prometheus":"3.0.0"}}`), &request); err != nil {
		t.Fatal(err)
	}
	if err = h.validateAndConsumeDeploymentPreflight(request, definition, nil, host); err == nil || !strings.Contains(err.Error(), "发生变化") {
		t.Fatalf("changed planned version accepted: %v", err)
	}
	var stored models.DeploymentPreflight
	if err = h.db.First(&stored, "token = ?", credential.Token).Error; err != nil {
		t.Fatal(err)
	}
	if stored.ConsumedAt != nil {
		t.Fatal("tamper consumed the original credential")
	}
}

// An unchanged, valid preview token is still only a preview: execution must
// find a real compatible dependency, not accept the planned version as proof.
func TestStackExecutionRechecksActualDependency(t *testing.T) {
	for _, existing := range []string{"", "3.0.0"} {
		t.Run("actual_"+existing, func(t *testing.T) {
			h, request, _, host, cleanup := preflightCredentialFixture(t)
			defer cleanup()
			if err := h.db.AutoMigrate(&models.Component{}, &models.Package{}); err != nil {
				t.Fatal(err)
			}
			host.ConnectionType = "local"
			request.Type = "thanos"
			request.Name = "thanos-fixture"
			request.Version = "0.36.1"
			request.DeployType = "binary"
			request.PlannedComponents = []string{"prometheus", "thanos"}
			request.PlannedVersions = map[string]string{"prometheus": "2.55.0", "thanos": "0.36.1"}
			definition, err := h.normalizeDeploymentRequest(&request, nil)
			if err != nil {
				t.Fatal(err)
			}
			if existing != "" {
				if err = h.db.Create(&models.Component{HostID: host.ID, Name: "actual", Type: models.ComponentPrometheus, Status: models.StatusRunning, DeployType: "binary", Version: existing}).Error; err != nil {
					t.Fatal(err)
				}
			}
			var preview []deploymentPreflightCheck
			h.preflightDependencies(&request, &host, &preview)
			if len(preview) != 1 || preview[0].Status != "pass" {
				t.Fatalf("fixture preview did not pass: %+v", preview)
			}
			credential, err := h.issueDeploymentPreflight(request, definition, nil, preview, nil, "tester")
			if err != nil {
				t.Fatal(err)
			}
			request.PreflightToken = credential.Token
			err = h.validateAndConsumeDeploymentPreflight(request, definition, nil, host)
			if err == nil || !strings.Contains(err.Error(), "部署前重新检查失败：组件依赖") {
				t.Fatalf("execution trusted future plan or failed for wrong reason: %v", err)
			}
			var stored models.DeploymentPreflight
			if err = h.db.First(&stored, "token = ?", credential.Token).Error; err != nil {
				t.Fatal(err)
			}
			if stored.ConsumedAt != nil {
				t.Fatal("failed actual dependency consumed preview token")
			}
		})
	}
}
