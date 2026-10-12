package models

import (
	"encoding/json"
	"testing"
)

func TestEmbedComponentDefinitionSnapshotPreservesConfiguration(t *testing.T) {
	t.Parallel()
	definition := ComponentDefinition{
		Type: "acme_exporter", DisplayName: "Acme Exporter", BinName: "acme_exporter",
		DefaultPort: 9123, HealthPath: "/health", PackageURL: "https://example.com/acme-{version}.tar.gz",
	}
	config, err := EmbedComponentDefinitionSnapshot(`{"retention":"7d"}`, definition)
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal([]byte(config), &values); err != nil {
		t.Fatal(err)
	}
	if string(values["retention"]) != `"7d"` {
		t.Fatalf("existing component config was lost: %s", config)
	}
	snapshot, found, err := ComponentDefinitionSnapshotFromConfig(config)
	if err != nil || !found {
		t.Fatalf("snapshot missing: found=%v err=%v", found, err)
	}
	if snapshot.Type != definition.Type || snapshot.BinName != definition.BinName || snapshot.PackageURL != definition.PackageURL {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
}

func TestEmbedComponentDefinitionSnapshotRejectsNonObjectConfig(t *testing.T) {
	t.Parallel()
	if _, err := EmbedComponentDefinitionSnapshot(`null`, ComponentDefinition{}); err == nil {
		t.Fatal("JSON null config was accepted")
	}
	if _, err := EmbedComponentDefinitionSnapshot(`[]`, ComponentDefinition{}); err == nil {
		t.Fatal("JSON array config was accepted")
	}
}

func TestEmbedDeploymentParametersPreservesConfiguration(t *testing.T) {
	t.Parallel()
	config, err := EmbedDeploymentParameters(`{"collectors":["cpu"]}`, map[string]interface{}{
		"retention_time":        "45d",
		"query_max_concurrency": 40,
	})
	if err != nil {
		t.Fatal(err)
	}
	parameters, err := DeploymentParametersFromConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	if parameters["retention_time"] != "45d" || parameters["query_max_concurrency"] != float64(40) {
		t.Fatalf("unexpected deployment parameter snapshot: %#v", parameters)
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal([]byte(config), &values); err != nil {
		t.Fatal(err)
	}
	if string(values["collectors"]) != `["cpu"]` {
		t.Fatalf("existing component config was lost: %s", config)
	}
}

func TestManagedHealthPathTracksConfiguredSubpaths(t *testing.T) {
	tests := []struct {
		componentType ComponentType
		parameters    map[string]interface{}
		wantPrefix    string
		wantHealth    string
	}{
		{ComponentPrometheus, map[string]interface{}{"external_url": "https://example.com/prom/"}, "/prom", "/prom/-/ready"},
		{ComponentAlertmanager, map[string]interface{}{"route_prefix": "/alerts"}, "/alerts", "/alerts/-/ready"},
		{ComponentGrafana, map[string]interface{}{"root_url": "https://example.com/grafana/", "serve_from_sub_path": true}, "/grafana", "/grafana/api/health"},
		{ComponentBlackbox, map[string]interface{}{"external_url": "https://example.com/"}, "", "/-/healthy"},
		{ComponentThanos, map[string]interface{}{}, "", "/-/ready"},
	}
	for _, test := range tests {
		config, err := EmbedDeploymentParameters("{}", test.parameters)
		if err != nil {
			t.Fatal(err)
		}
		component := &Component{Type: test.componentType, Config: config}
		if got := ManagedRoutePrefix(component); got != test.wantPrefix {
			t.Fatalf("%s route prefix=%q, want %q", test.componentType, got, test.wantPrefix)
		}
		if got := ManagedHealthPath(component); got != test.wantHealth {
			t.Fatalf("%s health path=%q, want %q", test.componentType, got, test.wantHealth)
		}
	}
}

func TestManagedHealthPathUsesCustomDefinitionSnapshot(t *testing.T) {
	definition := ComponentDefinition{
		Type: "acme_exporter", DisplayName: "Acme Exporter", BinName: "acme_exporter",
		DefaultPort: 9123, HealthPath: "/healthz", PackageURL: "https://example.com/acme-{version}.tar.gz",
	}
	config, err := EmbedComponentDefinitionSnapshot("{}", definition)
	if err != nil {
		t.Fatal(err)
	}
	component := &Component{Type: ComponentType(definition.Type), Config: config}
	if got := ManagedHealthPath(component); got != "/healthz" {
		t.Fatalf("custom health path=%q, want /healthz", got)
	}
}
