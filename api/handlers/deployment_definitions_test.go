package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestBuiltInDeploymentDefinitionsCoverManagedComponents(t *testing.T) {
	definitions := builtInDeploymentDefinitions()
	if len(definitions) != 10 {
		t.Fatalf("built-in definitions=%d, want 10", len(definitions))
	}
	seen := make(map[string]models.DeploymentDefinition, len(definitions))
	for _, definition := range definitions {
		seen[definition.Type] = definition
		if len(definition.Parameters) == 0 {
			t.Fatalf("definition %s has no parameters", definition.Type)
		}
	}
	windows := seen[string(models.ComponentWindowsExporter)]
	if windows.Platform != "windows" || windows.DeployType != "windows" || windows.ServiceManager != "windows-scm" {
		t.Fatalf("unexpected windows definition: %#v", windows)
	}
	fields := make(map[string]bool)
	for _, parameter := range windows.Parameters {
		fields[parameter.Field] = true
	}
	for _, field := range []string{"version", "port", "collectors"} {
		if !fields[field] {
			t.Fatalf("windows definition missing %s", field)
		}
	}
}

func TestCustomDeploymentParameterSchemaValidation(t *testing.T) {
	valid := json.RawMessage(`[
		{"field":"version","label":"版本","type":"package-version","required":true},
		{"field":"port","label":"端口","type":"number","default":9123,"min":1,"max":65535},
		{"field":"install_path","label":"安装路径","type":"path","default":"/opt/promlifecycle/components/acme"},
		{"field":"custom_args","label":"启动参数","type":"textarea","advanced":true}
	]`)
	normalized, err := normalizeCustomDeploymentParameterSchema(valid)
	if err != nil || normalized == "" {
		t.Fatalf("valid schema rejected: %v", err)
	}
	invalid := []json.RawMessage{
		json.RawMessage(`[{"field":"version","label":"版本","type":"text"}]`),
		json.RawMessage(`[{"field":"shell_command","label":"命令","type":"textarea"}]`),
		json.RawMessage(`[{"field":"version","label":"版本","type":"package-version"},{"field":"port","label":"端口","type":"number"},{"field":"install_path","label":"安装路径","type":"path","default":"../../etc"}]`),
	}
	for _, raw := range invalid {
		if _, err := normalizeCustomDeploymentParameterSchema(raw); err == nil {
			t.Fatalf("invalid schema accepted: %s", raw)
		}
	}
}

func TestListDeploymentDefinitionsMergesCustomSchema(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "deployment-definitions.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.ComponentDefinition{}); err != nil {
		t.Fatal(err)
	}
	definition := models.ComponentDefinition{
		Type: "acme_exporter", DisplayName: "Acme Exporter", BinName: "acme_exporter",
		DefaultPort: 9123, HealthPath: "/metrics", PackageURL: "https://example.com/acme-{version}.tar.gz", Enabled: true,
		ParamSchema: `[{"field":"version","label":"版本","type":"package-version"},{"field":"port","label":"端口","type":"number","default":9123},{"field":"install_path","label":"安装路径","type":"path","default":"/opt/promlifecycle/components/acme_exporter"}]`,
	}
	if err := db.Create(&definition).Error; err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodGet, "/api/v1/deployment-definitions", nil)
	(&Handler{db: db}).ListDeploymentDefinitions(context)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Definitions []models.DeploymentDefinition `json:"definitions"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range response.Definitions {
		if item.Type == definition.Type {
			found = true
			if item.BuiltIn || len(item.Parameters) != 3 {
				t.Fatalf("unexpected custom definition: %#v", item)
			}
		}
	}
	if !found {
		t.Fatal("custom deployment definition missing from catalog")
	}
}

func TestBuiltInDeploymentDefaultsNormalizeWithoutMissingParameters(t *testing.T) {
	for _, definition := range builtInDeploymentDefinitions() {
		t.Run(definition.Type, func(t *testing.T) {
			parameters := map[string]interface{}{"version": "1.0.0"}
			if definition.Type == string(models.ComponentPrometheus) {
				parameters["version"] = "3.1.0"
			}
			if definition.Type == string(models.ComponentGrafana) {
				parameters["initial_password"] = "Strong!Password123"
			}
			request := componentCreateRequest{
				Type: definition.Type, Name: definition.Type + "-matrix",
				DeployType: definition.DeployType, Parameters: parameters,
			}
			if _, err := (&Handler{}).normalizeDeploymentRequest(&request, nil); err != nil {
				t.Fatalf("default deployment parameters do not normalize: %v", err)
			}
			if _, leaked := request.StructuredParameters["initial_password"]; leaked {
				t.Fatal("password leaked into structured deployment snapshot")
			}
			seen := make(map[string]bool, len(definition.Parameters))
			for _, parameter := range definition.Parameters {
				if seen[parameter.Field] {
					t.Fatalf("duplicate deployment parameter %q", parameter.Field)
				}
				seen[parameter.Field] = true
				if !deploymentParameterTypes[parameter.Type] {
					t.Fatalf("unsupported deployment parameter type %q", parameter.Type)
				}
			}
		})
	}
}
