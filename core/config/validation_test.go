package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateConfigArtifactWithBinaryRequiresAndRunsNativeTool(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "prometheus.yml")
	if err := os.WriteFile(configPath, []byte("global:\n  scrape_interval: 15s\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateConfigArtifactWithBinary("prometheus", configPath, ""); err == nil {
		t.Fatal("missing native validator was accepted")
	}

	toolPath := filepath.Join(t.TempDir(), "promtool")
	if err := os.WriteFile(toolPath, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	result, err := ValidateConfigArtifactWithBinary("prometheus", configPath, toolPath)
	if err != nil {
		t.Fatal(err)
	}
	if result.Level != "full" || result.Tool != "promtool" {
		t.Fatalf("unexpected validation result: %#v", result)
	}

	if err := os.WriteFile(toolPath, []byte("#!/bin/sh\necho semantic-error >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateConfigArtifactWithBinary("prometheus", configPath, toolPath); err == nil || !strings.Contains(err.Error(), "semantic-error") {
		t.Fatalf("native semantic failure not returned: %v", err)
	}
}

func TestGrafanaReconciliationRemovesRetiredFilesWithoutTouchingOtherDashboards(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"heartbeat.json":   `{"uid":"overview"}`,
		"custom.json":      `{"uid":"custom"}`,
		"dashboard-1.json": `{"uid":"retired"}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	active := []byte(`{"uid":"active","title":"Renamed dashboard"}`)
	if err := reconcileGrafanaDashboardFiles(dir, map[string][]byte{"dashboard-2.json": active}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "dashboard-1.json")); !os.IsNotExist(err) {
		t.Fatalf("retired dashboard still present: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "dashboard-2.json")); err != nil || string(data) != string(active) {
		t.Fatalf("active dashboard not published: %s, %v", data, err)
	}
	if err := reconcileGrafanaDashboardFiles(dir, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "dashboard-2.json")); !os.IsNotExist(err) {
		t.Fatalf("last disabled/deleted dashboard still present: %v", err)
	}
	for name, want := range map[string]string{"heartbeat.json": `{"uid":"overview"}`, "custom.json": `{"uid":"custom"}`} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(data) != want {
			t.Fatalf("unowned dashboard %s changed: %s, %v", name, data, err)
		}
	}
}
