package handlers

import (
	"encoding/json"
	"github.com/promlifecycle/platform/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"path/filepath"
	"testing"
)

func TestThanosCompatibilityRejectsUnprovenVersions(t *testing.T) {
	tests := []struct {
		name, thanos, prometheus string
		want                     bool
	}{
		{"old supported pair", "v0.34.1", "v2.53.0", true},
		{"minimum stable sidecar", "0.37.0", "3.0.0", true},
		{"old sidecar", "0.36.1", "3.0.0", false},
		{"build metadata", "v0.37.0+build.12", "3.1.0+build.7", true},
		{"surrounding whitespace", " v0.39.2 ", " 3.1.0 ", true},
		{"missing prometheus", "0.39.2", "", false},
		{"missing thanos on prometheus 2", "", "2.55.0", false},
		{"unknown prometheus", "0.39.2", "unknown", false},
		{"bad major", "0.39.2", "x.3.0", false},
		{"bad minor", "0.39.2", "3.x.0", false},
		{"bad patch", "0.39.2", "3.1.x", false},
		{"short version", "0.39", "3.0.0", false},
		{"leading zero", "0.037.0", "3.0.0", false},
		{"negative minor", "0.-1.0", "2.55.0", false},
		{"overflow", "0.39.2", "9999999999999999999999.0.0", false},
		{"release candidate", "0.37.0-rc.0", "3.0.0", false},
		{"prometheus prerelease", "0.39.2", "3.0.0-rc.0", false},
		{"malformed metadata", "0.37.0+", "3.0.0", false},
		{"future prometheus major", "0.39.2", "4.0.0", false},
		{"future thanos major", "1.0.0", "3.0.0", false},
		{"ancient prometheus", "0.39.2", "1.8.0", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := thanosSupportsPrometheus(tt.thanos, tt.prometheus); got != tt.want {
				t.Fatalf("compatibility(%q,%q)=%v, want %v", tt.thanos, tt.prometheus, got, tt.want)
			}
		})
	}
}

func TestDeploymentDependencyDistinguishesUnknownCompatibility(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "compatibility.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.AutoMigrate(&models.Component{}); err != nil {
		t.Fatal(err)
	}
	host := models.Host{ID: 7}
	dependency := models.Component{HostID: host.ID, Name: "prometheus", Type: models.ComponentPrometheus, Status: models.StatusRunning, DeployType: "binary", Version: "unknown"}
	if err = db.Create(&dependency).Error; err != nil {
		t.Fatal(err)
	}
	h := Handler{db: db}
	request := componentCreateRequest{Type: string(models.ComponentThanos), Version: "0.39.2"}
	for _, tt := range []struct {
		version, status, compatibility string
	}{
		{"unknown", "fail", "unknown"},
		{"3.x.0", "fail", "unknown"},
		{"3.0.0-rc.0", "fail", "unknown"},
		{"3.1.0", "pass", "compatible"},
	} {
		t.Run(tt.version, func(t *testing.T) {
			if err = db.Model(&dependency).Update("version", tt.version).Error; err != nil {
				t.Fatal(err)
			}
			var checks []deploymentPreflightCheck
			h.preflightDependencies(&request, &host, &checks)
			if len(checks) != 1 {
				t.Fatalf("checks: %#v", checks)
			}
			c := checks[0]
			if c.Status != tt.status || (c.Status == "fail" && !c.Blocking) {
				t.Fatalf("unexpected outcome: %#v", c)
			}
			// Check public evidence without relying on its concrete Go map type.
			raw, err := json.Marshal(c.Evidence)
			if err != nil {
				t.Fatal(err)
			}
			var evidence map[string]interface{}
			if err = json.Unmarshal(raw, &evidence); err != nil {
				t.Fatal(err)
			}
			if evidence["compatibility"] != tt.compatibility {
				t.Fatalf("missing compatibility reason: %s", raw)
			}
		})
	}
	request.Version = "0.36.1"
	if err = db.Model(&dependency).Update("version", "3.1.0").Error; err != nil {
		t.Fatal(err)
	}
	var checks []deploymentPreflightCheck
	h.preflightDependencies(&request, &host, &checks)
	if len(checks) != 1 || checks[0].Status != "fail" || !checks[0].Blocking {
		t.Fatalf("known incompatibility was not blocked: %#v", checks)
	}
	raw, err := json.Marshal(checks[0].Evidence)
	if err != nil {
		t.Fatal(err)
	}
	var evidence map[string]interface{}
	if err = json.Unmarshal(raw, &evidence); err != nil {
		t.Fatal(err)
	}
	if evidence["compatibility"] != "incompatible" {
		t.Fatalf("known incompatibility misclassified: %s", raw)
	}

}
