package handlers

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/credentials"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestStackEnqueueChecksEffectiveVersions(t *testing.T) {
	for _, tt := range []struct {
		name, prometheus, override, thanos string
		duplicate                          bool
		want                               int
	}{
		{name: "reject incompatible", prometheus: "3.0.0", thanos: "0.36.1", want: 400},
		{name: "parameter override incompatible", prometheus: "2.55.0", override: "3.0.0", thanos: "0.36.1", want: 400},
		{name: "parameter override compatible", prometheus: "3.0.0", override: "2.55.0", thanos: "0.36.1", want: 202},
		{name: "minimum compatible", prometheus: "3.0.0", thanos: "0.37.0", want: 202},
		{name: "duplicate types", prometheus: "2.55.0", thanos: "0.37.0", duplicate: true, want: 400},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PROMLIFECYCLE_JWT_SECRET", strings.Repeat("stack-test-fixture-", 4))
			db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "stack.db")), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			sqlDB, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = sqlDB.Close() })
			if err = db.AutoMigrate(&models.Host{}, &models.Component{}, &models.DeploymentStack{}, &models.DeploymentStackStep{}); err != nil {
				t.Fatal(err)
			}
			host := models.Host{Name: "fixture", IP: "192.0.2.10", OS: "linux", ConnectionType: "ssh", Status: "online"}
			if err = db.Create(&host).Error; err != nil {
				t.Fatal(err)
			}
			components := []map[string]interface{}{
				{"type": "prometheus", "name": "prometheus", "version": tt.prometheus},
				{"type": "thanos", "name": "thanos", "version": tt.thanos, "planned_components": []string{"prometheus", "thanos"}, "planned_versions": map[string]string{"prometheus": "2.55.0"}, "preflight_token": "untrusted-preview"},
			}
			if tt.override != "" {
				components[0]["parameters"] = map[string]interface{}{"version": tt.override}
			}
			if tt.duplicate {
				components = append(components, map[string]interface{}{"type": "prometheus", "name": "prometheus-two", "version": "2.55.0"})
			}
			raw, err := json.Marshal(map[string]interface{}{"name": "fixture-stack", "host_id": host.ID, "components": components})
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(response)
			c.Request = httptest.NewRequest("POST", "/api/v1/deployments/stacks", bytes.NewReader(raw))
			c.Request.Header.Set("Content-Type", "application/json")
			(&Handler{db: db}).CreateDeploymentStack(c)
			if response.Code != tt.want {
				t.Fatalf("status=%d want %d: %s", response.Code, tt.want, response.Body)
			}
			var stacks, steps int64
			if err = db.Model(&models.DeploymentStack{}).Count(&stacks).Error; err != nil {
				t.Fatal(err)
			}
			if err = db.Model(&models.DeploymentStackStep{}).Count(&steps).Error; err != nil {
				t.Fatal(err)
			}
			if tt.want == 400 {
				if stacks != 0 || steps != 0 {
					t.Fatalf("invalid plan persisted %d stacks, %d steps", stacks, steps)
				}
				if !strings.Contains(response.Body.String(), "依赖") && !strings.Contains(response.Body.String(), "重复") {
					t.Fatalf("unexpected rejection: %s", response.Body)
				}
				return
			}
			if stacks != 1 || steps != 2 {
				t.Fatalf("wrong queue size: %d/%d", stacks, steps)
			}
			var rows []models.DeploymentStackStep
			if err = db.Order("position ASC").Find(&rows).Error; err != nil {
				t.Fatal(err)
			}
			for i, row := range rows {
				plaintext, err := credentials.Decrypt(row.EncryptedRequest)
				if err != nil {
					t.Fatal(err)
				}
				var stored map[string]interface{}
				if err = json.Unmarshal([]byte(plaintext), &stored); err != nil {
					t.Fatal(err)
				}
				for _, field := range []string{"planned_components", "planned_versions", "preflight_token"} {
					if value, ok := stored[field]; ok && value != "" && value != nil {
						t.Fatalf("queue retains %s: %v", field, value)
					}
				}
				effective := tt.prometheus
				if tt.override != "" {
					effective = tt.override
				}
				if i == 0 && stored["version"] != effective {
					t.Fatalf("effective version lost: %v", stored["version"])
				}
			}
		})
	}
}
