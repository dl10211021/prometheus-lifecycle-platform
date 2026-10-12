package services

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func TestDiscoverComponentsReturnsDistinctProcessesOfSameType(t *testing.T) {
	first := exec.Command("sleep", "30")
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = first.Process.Kill()
		_, _ = first.Process.Wait()
	}()

	second := exec.Command("sleep", "30")
	if err := second.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = second.Process.Kill()
		_, _ = second.Process.Wait()
	}()

	spec := componentDiscoverySpec{kind: "test_exporter", name: "Test Exporter", port: 41901, bins: []string{"sleep"}, healthPath: "/metrics"}
	service := NewOnboardingService(nil, t.TempDir())
	items := service.discoverComponents(spec, map[int]int{
		41901: first.Process.Pid,
		41902: second.Process.Pid,
	})

	if len(items) != 2 {
		t.Fatalf("discovered %d instances, want 2: %#v", len(items), items)
	}
	seen := map[int]bool{}
	fingerprints := map[string]bool{}
	for _, item := range items {
		seen[item.Port] = true
		if item.PID == 0 || item.Binary == "" || item.Fingerprint == "" {
			t.Fatalf("incomplete discovery evidence: %#v", item)
		}
		if fingerprints[item.Fingerprint] {
			t.Fatalf("duplicate fingerprint %q", item.Fingerprint)
		}
		fingerprints[item.Fingerprint] = true
	}
	if !seen[41901] || !seen[41902] {
		t.Fatalf("ports=%v, want 41901 and 41902", seen)
	}
}

func TestPreferredDiscoveryPortDeduplicatesOneProcess(t *testing.T) {
	ports := []int{42100, 42000, 41900}
	if got := preferredDiscoveryPort(ports, 42000); got != 42000 {
		t.Fatalf("preferred port=%d", got)
	}
	if got := preferredDiscoveryPort(ports, 43000); got != 41900 {
		t.Fatalf("fallback port=%d", got)
	}
}

func TestSystemdExecBinaryFindsArbitraryUnitPathAndPort(t *testing.T) {
	spec := componentDiscoverySpec{kind: "node_exporter", bins: []string{"node_exporter"}}
	content := `[Unit]
Description=legacy monitor
[Service]
ExecStart=/opt/legacy/exporters/node_exporter --web.listen-address=:19191
`
	binary := systemdExecBinary(content, discoveryBinaryNames(spec))
	if binary != "/opt/legacy/exporters/node_exporter" {
		t.Fatalf("binary=%q", binary)
	}
	if port := systemdDiscoveryPort(content, 9100); port != 19191 {
		t.Fatalf("port=%d", port)
	}
}

func TestPackageOwnershipMustMatchDiscoveredComponent(t *testing.T) {
	if !packageMatchesDiscoveryType("prometheus-node-exporter-1.8.2-1.el8.x86_64", "node_exporter", "node_exporter") {
		t.Fatal("dedicated node exporter RPM was rejected")
	}
	if !packageMatchesDiscoveryType("prometheus:amd64", "prometheus", "prometheus") {
		t.Fatal("dedicated Prometheus DEB was rejected")
	}
	if packageMatchesDiscoveryType("python3-libs-3.11", "node_exporter", "node_exporter") {
		t.Fatal("unrelated package was accepted for automatic uninstall")
	}
}

func TestDiscoveryBinaryTypeAllowsLegacySystemPaths(t *testing.T) {
	for _, path := range []string{
		"/usr/bin/prometheus",
		"/usr/local/bin/prometheus",
		"/opt/legacy/prometheus",
		"/usr/bin/node_exporter",
		"/usr/local/promlifecycle/node_exporter",
	} {
		componentType := "prometheus"
		if filepath.Base(path) == "node_exporter" {
			componentType = "node_exporter"
		}
		if !discoveryBinaryMatchesType(path, componentType) {
			t.Fatalf("legacy path %q was rejected", path)
		}
	}
}

func TestStackTemplatesExposePushgatewayDeployment(t *testing.T) {
	for _, template := range StackTemplates() {
		if template.Key != "batch_metrics" {
			continue
		}
		if len(template.ComponentOrder) != 3 || template.ComponentOrder[0] != "pushgateway" {
			t.Fatalf("Pushgateway stack order is incomplete: %#v", template.ComponentOrder)
		}
		return
	}
	t.Fatal("Pushgateway has no reachable built-in deployment template")
}

func TestStackTemplatesExposeThanosSidecarDeployment(t *testing.T) {
	for _, template := range StackTemplates() {
		if template.Key != "thanos_sidecar" {
			continue
		}
		if len(template.ComponentOrder) != 2 || template.ComponentOrder[0] != "prometheus" || template.ComponentOrder[1] != "thanos" {
			t.Fatalf("Thanos Sidecar stack order is incomplete: %#v", template.ComponentOrder)
		}
		return
	}
	t.Fatal("Thanos Sidecar has no reachable built-in deployment template")
}

func TestStackTemplatesCoverEveryBuiltInComponent(t *testing.T) {
	expected := map[string]bool{
		"prometheus": false, "alertmanager": false, "grafana": false,
		"node_exporter": false, "windows_exporter": false, "blackbox_exporter": false,
		"pushgateway": false, "loki": false, "promtail": false, "thanos": false,
	}
	for _, template := range StackTemplates() {
		for _, componentType := range template.ComponentOrder {
			if _, exists := expected[componentType]; exists {
				expected[componentType] = true
			}
		}
	}
	for componentType, covered := range expected {
		if !covered {
			t.Fatalf("built-in component %s has no reachable deployment template", componentType)
		}
	}
}
