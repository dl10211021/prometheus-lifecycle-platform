package config

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/promlifecycle/platform/models"
	"gopkg.in/yaml.v3"
)

func TestGetDefaultTemplate(t *testing.T) {
	componentTypes := []models.ComponentType{
		models.ComponentPrometheus,
		models.ComponentGrafana,
		models.ComponentAlertmanager,
		models.ComponentLoki,
		models.ComponentNodeExporter,
		models.ComponentPushgateway,
		models.ComponentBlackbox,
		models.ComponentThanos,
	}

	for _, ct := range componentTypes {
		template := GetDefaultTemplate(ct)

		if template.Type != ct {
			t.Errorf("Template type mismatch for %s: got %s", ct, template.Type)
		}

		if template.Name == "" {
			t.Errorf("Template name should not be empty for %s", ct)
		}

		if template.Version == "" && ct != models.ComponentThanos {
			t.Errorf("Template version should not be empty for %s", ct)
		}

		t.Logf("OK: Template for %s: name=%s, version=%s, replicas=%d", ct, template.Name, template.Version, template.Replicas)
	}
}

func TestGetDefaultTemplate_Prometheus(t *testing.T) {
	template := GetDefaultTemplate(models.ComponentPrometheus)

	if template.Type != models.ComponentPrometheus {
		t.Errorf("Expected type prometheus, got %s", template.Type)
	}

	if template.Name != "prometheus" {
		t.Errorf("Expected name 'prometheus', got '%s'", template.Name)
	}

	if template.Replicas != 1 {
		t.Errorf("Expected replicas 1, got %d", template.Replicas)
	}

	if template.HealthCheck.Endpoint != "/-/healthy" {
		t.Errorf("Expected health check endpoint '/-/healthy', got '%s'", template.HealthCheck.Endpoint)
	}

	if !template.BackupPolicy.Enabled {
		t.Errorf("Expected backup policy enabled")
	}

	if !template.UpgradePolicy.BackupBeforeUpgrade {
		t.Errorf("Expected backup before upgrade enabled")
	}

	t.Logf("OK: Prometheus template validated")
}

func TestGetDefaultTemplate_Grafana(t *testing.T) {
	template := GetDefaultTemplate(models.ComponentGrafana)

	if template.Type != models.ComponentGrafana {
		t.Errorf("Expected type grafana, got %s", template.Type)
	}

	if template.Name != "grafana" {
		t.Errorf("Expected name 'grafana', got '%s'", template.Name)
	}

	if template.HealthCheck.Endpoint != "/api/health" {
		t.Errorf("Expected health check endpoint '/api/health', got '%s'", template.HealthCheck.Endpoint)
	}

	t.Logf("OK: Grafana template validated")
}
func TestDefaultGrafanaYAMLUsesUnifiedAlerting(t *testing.T) {
	t.Parallel()

	cfg := DefaultGrafanaYAML("")
	if strings.Contains(cfg, "\n[alerting]\n") {
		t.Fatal("legacy [alerting] section prevents Grafana 11 from starting")
	}
	if !strings.Contains(cfg, "\n[unified_alerting]\nenabled = true\n") {
		t.Fatal("unified Grafana alerting is not enabled")
	}
	if !strings.Contains(cfg, "\nlogs = /var/lib/grafana/logs\n") {
		t.Fatal("Grafana logs must stay under the writable data directory")
	}
	if !strings.Contains(cfg, "\ndefault_language = zh-Hans\n") || !strings.Contains(cfg, "\ndefault_timezone = browser\n") {
		t.Fatal("Grafana must default to Chinese and browser-local time")
	}
	if strings.Contains(cfg, "admin_password =") || strings.Contains(cfg, "secret_key =") {
		t.Fatal("Grafana config must not contain fixed credentials or encryption secrets")
	}
}

func TestGetDefaultTemplate_Alertmanager(t *testing.T) {
	template := GetDefaultTemplate(models.ComponentAlertmanager)

	if template.Replicas != 3 {
		t.Errorf("Alertmanager should have 3 replicas by default (HA), got %d", template.Replicas)
	}

	if template.HealthCheck.Endpoint != "/-/healthy" {
		t.Errorf("Expected health check endpoint '/-/healthy', got '%s'", template.HealthCheck.Endpoint)
	}

	t.Logf("OK: Alertmanager template validated")
}

func TestPrometheusSpec_MarshalYAML(t *testing.T) {
	spec := PrometheusSpec{
		Global: GlobalConfig{
			ScrapeInterval:     "15s",
			EvaluationInterval: "15s",
			ExternalLabels: map[string]string{
				"cluster": "test",
			},
		},
		ScrapeConfigs: []ScrapeConfig{
			{
				JobName: "prometheus",
				StaticConfigs: []StaticConfig{
					{
						Targets: []string{"localhost:9090"},
					},
				},
			},
		},
	}

	data, err := yaml.Marshal(spec)
	if err != nil {
		t.Fatalf("Failed to marshal PrometheusSpec: %v", err)
	}

	// Verify YAML contains expected content
	yamlStr := string(data)
	if !contains(yamlStr, "scrape_interval: 15s") {
		t.Errorf("YAML should contain scrape_interval")
	}

	if !contains(yamlStr, "job_name: prometheus") {
		t.Errorf("YAML should contain job_name")
	}

	if !contains(yamlStr, "localhost:9090") {
		t.Errorf("YAML should contain target")
	}

	t.Logf("OK: PrometheusSpec marshals to YAML correctly")
	t.Logf("YAML:\n%s", yamlStr)
}

func TestAlertmanagerSpec_MarshalYAML(t *testing.T) {
	spec := AlertmanagerSpec{
		Global: &AlertmanagerGlobal{
			ResolveTimeout: "5m",
		},
		Route: &AlertmanagerRoute{
			Receiver:       "default",
			GroupBy:        []string{"alertname"},
			GroupWait:      "30s",
			GroupInterval:  "5m",
			RepeatInterval: "4h",
		},
		Receivers: []Receiver{
			{
				Name: "default",
				EmailConfigs: []EmailConfig{
					{
						To:      "alerts@example.com",
						From:    "alertmanager@example.com",
						Headers: map[string]string{"Subject": "[ALERT]"},
					},
				},
			},
		},
	}

	data, err := yaml.Marshal(spec)
	if err != nil {
		t.Fatalf("Failed to marshal AlertmanagerSpec: %v", err)
	}

	yamlStr := string(data)
	if !contains(yamlStr, "receiver: default") {
		t.Errorf("YAML should contain default receiver")
	}

	if !contains(yamlStr, "alerts@example.com") {
		t.Errorf("YAML should contain email")
	}

	t.Logf("OK: AlertmanagerSpec marshals to YAML correctly")
}

func TestLokiSpec_MarshalJSON(t *testing.T) {
	spec := LokiSpec{
		AuthEnabled: false,
		ServerConfig: &LokiServerConfig{
			HTTPListenPort: 3100,
		},
		CommonConfig: &LokiCommonConfig{
			InstanceAddr: "127.0.0.1",
			PathPrefix:   "/var/loki",
		},
		SchemaConfig: &LokiSchemaConfig{
			Configs: []LokiSchemaConfigItem{
				{
					From:   "",
					Store:  "tsdb",
					Schema: "v13",
				},
			},
		},
	}

	data, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		t.Fatalf("Failed to marshal LokiSpec: %v", err)
	}

	jsonStr := string(data)
	if !contains(jsonStr, "3100") {
		t.Errorf("JSON should contain port 3100")
	}

	t.Logf("OK: LokiSpec marshals to JSON correctly")
}

func TestPrometheusGlobalIntervalsUsesDeploymentSnapshot(t *testing.T) {
	t.Parallel()
	componentConfig, err := models.EmbedDeploymentParameters("{}", map[string]interface{}{
		"scrape_interval": "30s", "evaluation_interval": "20s", "scrape_timeout": "10s",
	})
	if err != nil {
		t.Fatal(err)
	}
	scrapeInterval, evaluationInterval, scrapeTimeout := PrometheusGlobalIntervals(componentConfig)
	if scrapeInterval != "30s" || evaluationInterval != "20s" || scrapeTimeout != "10s" {
		t.Fatalf("unexpected global intervals: %s %s %s", scrapeInterval, evaluationInterval, scrapeTimeout)
	}
}

func TestAlertmanagerConfigIntervalsUsesDeploymentSnapshot(t *testing.T) {
	t.Parallel()
	componentConfig, err := models.EmbedDeploymentParameters("{}", map[string]interface{}{
		"resolve_timeout": "10m", "group_wait": "45s",
		"group_interval": "10m", "repeat_interval": "8h",
	})
	if err != nil {
		t.Fatal(err)
	}
	resolveTimeout, groupWait, groupInterval, repeatInterval := AlertmanagerConfigIntervals(componentConfig)
	if resolveTimeout != "10m" || groupWait != "45s" || groupInterval != "10m" || repeatInterval != "8h" {
		t.Fatalf("unexpected Alertmanager intervals: %s %s %s %s", resolveTimeout, groupWait, groupInterval, repeatInterval)
	}
}

func TestExporterSpec_Blackbox(t *testing.T) {
	spec := ExporterSpec{
		Type: "blackbox",
		BlackboxConfig: &BlackboxExporterConfig{
			Modules: map[string]BlackboxModule{
				"http_2xx": {
					Prober: "http",
					HTTPConfig: &HTTPProbeConfig{
						ValidHTTPVersions: []string{"HTTP/2"},
						ValidStatusCodes:  []int{200, 302},
						Method:            "GET",
						FollowRedirects:   true,
					},
				},
				"tcp_connect": {
					Prober: "tcp",
				},
				"icmp": {
					Prober: "icmp",
					ICMPConfig: &ICMPProbeConfig{
						PreferredIPProtocol: "ip4",
						PayloadSize:         56,
					},
				},
			},
		},
	}

	data, err := yaml.Marshal(spec)
	if err != nil {
		t.Fatalf("Failed to marshal ExporterSpec: %v", err)
	}

	yamlStr := string(data)
	if !contains(yamlStr, "http_2xx") {
		t.Errorf("YAML should contain http_2xx module")
	}

	if !contains(yamlStr, "tcp_connect") {
		t.Errorf("YAML should contain tcp_connect module")
	}

	t.Logf("OK: Blackbox ExporterSpec marshals correctly")
}

func TestComponentTemplate_Structure(t *testing.T) {
	template := ComponentTemplate{
		Type:     models.ComponentPrometheus,
		Name:     "prometheus",
		Version:  "v2.51.0",
		Image:    "prom/prometheus:v2.51.0",
		Replicas: 1,
		Resources: ResourceQuota{
			CPURequest:    "500m",
			CPULimit:      "2000m",
			MemoryRequest: "1Gi",
			MemoryLimit:   "4Gi",
		},
		HealthCheck: HealthCheckConfig{
			Enabled:          true,
			Endpoint:         "/-/healthy",
			Interval:         "30s",
			Timeout:          "10s",
			SuccessThreshold: 1,
			FailureThreshold: 3,
		},
		BackupPolicy: BackupPolicyConfig{
			Enabled:      true,
			Schedule:     "0 2 * * *",
			Retention:    "30d",
			StorageType:  "local",
			BackupConfig: true,
			BackupData:   true,
		},
		UpgradePolicy: UpgradePolicyConfig{
			Strategy:            "rolling",
			AutoUpgrade:         false,
			BackupBeforeUpgrade: true,
			RollbackOnFailure:   true,
		},
	}

	// Marshal to JSON and back to verify structure
	data, err := json.Marshal(template)
	if err != nil {
		t.Fatalf("Failed to marshal template: %v", err)
	}

	var decoded ComponentTemplate
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal template: %v", err)
	}

	if decoded.Type != template.Type {
		t.Errorf("Type mismatch after round-trip")
	}

	if decoded.HealthCheck.Endpoint != template.HealthCheck.Endpoint {
		t.Errorf("HealthCheck.Endpoint mismatch after round-trip")
	}

	t.Logf("OK: ComponentTemplate round-trips correctly")
}

func TestServiceDiscoveryConfigs(t *testing.T) {
	// Kubernetes SD
	k8sSD := ServiceDiscoveryConfig{
		Type: "kubernetes_sd_configs",
		KubernetesSDConfig: &KubernetesSDConfig{
			Roles: []string{"pod", "service"},
			Namespaces: []NamespaceConfig{
				{Name: "monitoring"},
				{Name: "default"},
			},
		},
	}

	if k8sSD.KubernetesSDConfig == nil {
		t.Errorf("KubernetesSDConfig should not be nil")
	}

	if len(k8sSD.KubernetesSDConfig.Roles) != 2 {
		t.Errorf("Expected 2 roles, got %d", len(k8sSD.KubernetesSDConfig.Roles))
	}

	// File SD
	fileSD := ServiceDiscoveryConfig{
		Type: "file_sd_configs",
		FileSDConfig: &FileSDConfig{
			Files:           []string{"/etc/prometheus/targets/*.yml"},
			RefreshInterval: "30s",
		},
	}

	if len(fileSD.FileSDConfig.Files) != 1 {
		t.Errorf("Expected 1 file, got %d", len(fileSD.FileSDConfig.Files))
	}

	t.Logf("OK: ServiceDiscoveryConfigs validated")
}

func TestRelabelConfigs(t *testing.T) {
	relabel := RelabelConfig{
		SourceLabels: []string{"__meta_kubernetes_pod_label_app"},
		Regex:        "prometheus",
		TargetLabel:  "job",
		Replacement:  "${1}",
		Action:       "replace",
	}

	if len(relabel.SourceLabels) != 1 {
		t.Errorf("Expected 1 source label")
	}

	if relabel.Action != "replace" {
		t.Errorf("Expected action 'replace'")
	}

	t.Logf("OK: RelabelConfigs validated")
}

func TestSlackConfig(t *testing.T) {
	receiver := Receiver{
		Name: "slack-notifications",
		SlackConfigs: []SlackConfig{
			{
				APIURL:  "https://hooks.slack.com/services/xxx/yyy/zzz",
				Channel: "#alerts",
				Color:   "danger",
				Title:   "{{ .CommonLabels.alertname }}",
				Fields: []SlackField{
					{Title: "Severity", Value: "{{ .CommonLabels.severity }}", Short: true},
				},
			},
		},
	}

	if len(receiver.SlackConfigs) != 1 {
		t.Errorf("Expected 1 SlackConfig")
	}

	if receiver.SlackConfigs[0].Channel != "#alerts" {
		t.Errorf("Expected channel '#alerts'")
	}

	t.Logf("OK: SlackConfig validated")
}

func TestPagerdutyConfig(t *testing.T) {
	receiver := Receiver{
		Name: "pagerduty-critical",
		PagerdutyConfigs: []PagerdutyConfig{
			{
				RoutingKey:  "your-routing-key",
				Description: "{{ .CommonAnnotations.summary }}",
				Client:      "Prometheus",
			},
		},
	}

	if len(receiver.PagerdutyConfigs) != 1 {
		t.Errorf("Expected 1 PagerdutyConfig")
	}

	t.Logf("OK: PagerdutyConfig validated")
}

func TestWebhookConfig(t *testing.T) {
	receiver := Receiver{
		Name: "webhook",
		WebhookConfigs: []WebhookConfig{
			{
				URL: "http://localhost:5001/alerts",
				HTTPConfig: &WebhookHTTPConfig{
					Authorization: &AuthConfig{
						Type:        "Bearer",
						Credentials: "secret-token",
					},
				},
			},
		},
	}

	if receiver.WebhookConfigs[0].URL != "http://localhost:5001/alerts" {
		t.Errorf("Expected webhook URL mismatch")
	}

	t.Logf("OK: WebhookConfig validated")
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsAt(s, substr, 0))
}

func containsAt(s, substr string, start int) bool {
	for i := start; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
