package config

import (
	"github.com/promlifecycle/platform/models"
	"gorm.io/gorm"
	"strconv"
	"strings"
)

// DefaultPrometheusTemplate returns a production-ready default template for Prometheus.
func DefaultPrometheusTemplate() *ComponentTemplate {
	return &ComponentTemplate{
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
}

// DefaultGrafanaTemplate returns a production-ready default template for Grafana.
func DefaultGrafanaTemplate() *ComponentTemplate {
	return &ComponentTemplate{
		Type:     models.ComponentGrafana,
		Name:     "grafana",
		Version:  "10.1.0",
		Image:    "grafana/grafana:10.1.0",
		Replicas: 1,
		Resources: ResourceQuota{
			CPURequest:    "250m",
			CPULimit:      "1000m",
			MemoryRequest: "512Mi",
			MemoryLimit:   "2Gi",
		},
		HealthCheck: HealthCheckConfig{
			Enabled:          true,
			Endpoint:         "/api/health",
			Interval:         "30s",
			Timeout:          "10s",
			SuccessThreshold: 1,
			FailureThreshold: 3,
		},
		BackupPolicy: BackupPolicyConfig{
			Enabled:      true,
			Schedule:     "0 3 * * *",
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
}

// DefaultAlertmanagerTemplate returns a production-ready default template for Alertmanager.
func DefaultAlertmanagerTemplate() *ComponentTemplate {
	return &ComponentTemplate{
		Type:     models.ComponentAlertmanager,
		Name:     "alertmanager",
		Version:  "v0.26.0",
		Image:    "prom/alertmanager:v0.26.0",
		Replicas: 3,
		Resources: ResourceQuota{
			CPURequest:    "100m",
			CPULimit:      "500m",
			MemoryRequest: "256Mi",
			MemoryLimit:   "512Mi",
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
			Schedule:     "0 4 * * *",
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
}

// DefaultLokiTemplate returns a production-ready default template for Loki.
func DefaultLokiTemplate() *ComponentTemplate {
	return &ComponentTemplate{
		Type:     models.ComponentLoki,
		Name:     "loki",
		Version:  "v2.9.0",
		Image:    "grafana/loki:2.9.0",
		Replicas: 1,
		Resources: ResourceQuota{
			CPURequest:    "250m",
			CPULimit:      "1000m",
			MemoryRequest: "1Gi",
			MemoryLimit:   "4Gi",
		},
		HealthCheck: HealthCheckConfig{
			Enabled:          true,
			Endpoint:         "/ready",
			Interval:         "30s",
			Timeout:          "10s",
			SuccessThreshold: 1,
			FailureThreshold: 3,
		},
		BackupPolicy: BackupPolicyConfig{
			Enabled:      true,
			Schedule:     "0 5 * * *",
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
}

// DefaultThanosTemplate returns a production-ready default template for Thanos.
func DefaultThanosTemplate() *ComponentTemplate {
	return &ComponentTemplate{
		Type:     models.ComponentThanos,
		Name:     "thanos",
		Version:  "v0.35.0",
		Image:    "quay.io/thanos/thanos:v0.35.0",
		Replicas: 1,
		Resources: ResourceQuota{
			CPURequest:    "250m",
			CPULimit:      "1000m",
			MemoryRequest: "512Mi",
			MemoryLimit:   "2Gi",
		},
		HealthCheck: HealthCheckConfig{
			Enabled:          true,
			Endpoint:         "/-/healthy",
			Interval:         "30s",
			Timeout:          "10s",
			SuccessThreshold: 1,
			FailureThreshold: 3,
		},
		BackupPolicy: BackupPolicyConfig{Enabled: false},
		UpgradePolicy: UpgradePolicyConfig{
			Strategy:            "rolling",
			AutoUpgrade:         false,
			BackupBeforeUpgrade: true,
			RollbackOnFailure:   true,
		},
	}
}

// GetDefaultTemplate returns a production-ready default template for the given component type.
func GetDefaultTemplate(componentType models.ComponentType) *ComponentTemplate {
	switch componentType {
	case models.ComponentPrometheus:
		return DefaultPrometheusTemplate()
	case models.ComponentGrafana:
		return DefaultGrafanaTemplate()
	case models.ComponentAlertmanager:
		return DefaultAlertmanagerTemplate()
	case models.ComponentLoki:
		return DefaultLokiTemplate()
	case models.ComponentPromtail:
		return &ComponentTemplate{
			Type:     models.ComponentPromtail,
			Name:     "promtail",
			Version:  "v2.9.0",
			Image:    "grafana/promtail:v2.9.0",
			Replicas: 1,
			Resources: ResourceQuota{
				CPURequest:    "100m",
				CPULimit:      "500m",
				MemoryRequest: "256Mi",
				MemoryLimit:   "512Mi",
			},
			HealthCheck: HealthCheckConfig{
				Enabled:          false,
				Endpoint:         "",
				Interval:         "30s",
				Timeout:          "10s",
				SuccessThreshold: 1,
				FailureThreshold: 3,
			},
			BackupPolicy: BackupPolicyConfig{Enabled: false},
			UpgradePolicy: UpgradePolicyConfig{
				Strategy:            "rolling",
				AutoUpgrade:         false,
				BackupBeforeUpgrade: true,
				RollbackOnFailure:   true,
			},
		}
	case models.ComponentBlackbox:
		return &ComponentTemplate{
			Type:     models.ComponentBlackbox,
			Name:     "blackbox-exporter",
			Version:  "v0.24.0",
			Image:    "prom/blackbox-exporter:v0.24.0",
			Replicas: 1,
			Resources: ResourceQuota{
				CPURequest:    "50m",
				CPULimit:      "200m",
				MemoryRequest: "64Mi",
				MemoryLimit:   "128Mi",
			},
			HealthCheck: HealthCheckConfig{
				Enabled:          true,
				Endpoint:         "/-/healthy",
				Interval:         "30s",
				Timeout:          "10s",
				SuccessThreshold: 1,
				FailureThreshold: 3,
			},
			BackupPolicy: BackupPolicyConfig{Enabled: false},
			UpgradePolicy: UpgradePolicyConfig{
				Strategy:            "recreate",
				AutoUpgrade:         false,
				BackupBeforeUpgrade: false,
				RollbackOnFailure:   true,
			},
		}
	case models.ComponentNodeExporter:
		return &ComponentTemplate{
			Type:     models.ComponentNodeExporter,
			Name:     "node-exporter",
			Version:  "v1.7.0",
			Image:    "prom/node-exporter:v1.7.0",
			Replicas: 1,
			Resources: ResourceQuota{
				CPURequest:    "50m",
				CPULimit:      "200m",
				MemoryRequest: "64Mi",
				MemoryLimit:   "128Mi",
			},
			HealthCheck: HealthCheckConfig{
				Enabled:          true,
				Endpoint:         "/-/healthy",
				Interval:         "30s",
				Timeout:          "10s",
				SuccessThreshold: 1,
				FailureThreshold: 3,
			},
			BackupPolicy: BackupPolicyConfig{Enabled: false},
			UpgradePolicy: UpgradePolicyConfig{
				Strategy:            "rolling",
				AutoUpgrade:         false,
				BackupBeforeUpgrade: true,
				RollbackOnFailure:   true,
			},
		}
	case models.ComponentPushgateway:
		return &ComponentTemplate{
			Type:     models.ComponentPushgateway,
			Name:     "pushgateway",
			Version:  "v1.8.0",
			Image:    "prom/pushgateway:v1.8.0",
			Replicas: 1,
			Resources: ResourceQuota{
				CPURequest:    "50m",
				CPULimit:      "200m",
				MemoryRequest: "64Mi",
				MemoryLimit:   "128Mi",
			},
			HealthCheck: HealthCheckConfig{
				Enabled:          true,
				Endpoint:         "/-/healthy",
				Interval:         "30s",
				Timeout:          "10s",
				SuccessThreshold: 1,
				FailureThreshold: 3,
			},
			BackupPolicy: BackupPolicyConfig{Enabled: false},
			UpgradePolicy: UpgradePolicyConfig{
				Strategy:            "recreate",
				AutoUpgrade:         false,
				BackupBeforeUpgrade: false,
				RollbackOnFailure:   true,
			},
		}
	case models.ComponentThanos:
		return DefaultThanosTemplate()
	default:
		return nil
	}
}

// LoadDefaultTemplates populates the database with default templates if none exist.
func LoadDefaultTemplates(db *gorm.DB) error {
	// Check if defaults already exist
	var count int64
	if err := db.Model(&models.ConfigTemplate{}).Where("is_default = ?", true).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return nil
	}

	defaults := []models.ConfigTemplate{}

	tmpl := GetDefaultTemplate(models.ComponentPrometheus)
	if tmpl != nil {
		defaults = append(defaults, models.ConfigTemplate{
			Name:          "prometheus.yml",
			Description:   "Default Prometheus configuration with scrape intervals, alerting, and self-monitoring",
			ComponentType: string(tmpl.Type),
			Content:       DefaultPrometheusYAML(),
			IsDefault:     true,
		})
	}

	tmpl = GetDefaultTemplate(models.ComponentAlertmanager)
	if tmpl != nil {
		defaults = append(defaults, models.ConfigTemplate{
			Name:          "alertmanager.yml",
			Description:   "Default Alertmanager configuration with routing and webhook receiver",
			ComponentType: string(tmpl.Type),
			Content:       DefaultAlertmanagerYAML(),
			IsDefault:     true,
		})
	}

	tmpl = GetDefaultTemplate(models.ComponentGrafana)
	if tmpl != nil {
		defaults = append(defaults, models.ConfigTemplate{
			Name:          "grafana.ini",
			Description:   "Default Grafana configuration with server, security, users, auth, and dashboards",
			ComponentType: string(tmpl.Type),
			Content:       DefaultGrafanaYAML(""),
			IsDefault:     true,
		})
	}

	tmpl = GetDefaultTemplate(models.ComponentLoki)
	if tmpl != nil {
		defaults = append(defaults, models.ConfigTemplate{
			Name:          "loki.yml",
			Description:   "Default Loki configuration with authentication disabled and filesystem storage",
			ComponentType: string(tmpl.Type),
			Content:       DefaultLokiYAML(),
			IsDefault:     true,
		})
	}

	tmpl = GetDefaultTemplate(models.ComponentPromtail)
	if tmpl != nil {
		defaults = append(defaults, models.ConfigTemplate{
			Name:          "promtail.yml",
			Description:   "Default Promtail configuration with client pointing to Loki",
			ComponentType: string(tmpl.Type),
			Content:       DefaultPromtailYAML(),
			IsDefault:     true,
		})
	}

	tmpl = GetDefaultTemplate(models.ComponentBlackbox)
	if tmpl != nil {
		defaults = append(defaults, models.ConfigTemplate{
			Name:          "blackbox.yml",
			Description:   "Default Blackbox Exporter configuration with HTTP, TCP, and ICMP modules",
			ComponentType: string(tmpl.Type),
			Content:       DefaultBlackboxYAML(),
			IsDefault:     true,
		})
	}

	if err := db.Create(&defaults).Error; err != nil {
		return err
	}
	return nil
}

// DefaultPrometheusYAML returns a production-ready Prometheus configuration.
func DefaultPrometheusYAML() string {
	return `global:
  scrape_interval: 15s
  evaluation_interval: 15s
  scrape_timeout: 10s

alerting:
  alertmanagers:
    - static_configs:
        - targets:
            - alertmanager:9093

rule_files:
  - /etc/prometheus/rules/*.yml

scrape_configs:
  - job_name: prometheus
    static_configs:
      - targets:
          - localhost:9090
        labels:
          group: prometheus

  - job_name: node_exporter
    static_configs:
      - targets:
          - node_exporter:9100
        labels:
          group: infrastructure

  - job_name: blackbox
    metrics_path: /probe
    params:
      module:
        - http_2xx
    static_configs:
      - targets:
          - http://prometheus:9090
          - http://grafana:3000
    relabel_configs:
      - source_labels: [__address__]
        target_label: __param_target
      - target_label: __address__
        replacement: blackbox_exporter:9115

storage:
  tsdb:
    path: /prometheus/data
    retention.time: 15d
`
}

// DefaultAlertmanagerYAML returns a production-ready Alertmanager configuration.
func DefaultAlertmanagerYAML() string {
	return `global:
  resolve_timeout: 5m
  smtp_smarthost: 'localhost:25'
  smtp_from: 'alertmanager@example.com'

route:
  receiver: default-webhook
  group_by:
    - alertname
    - cluster
    - severity
  group_wait: 30s
  group_interval: 5m
  repeat_interval: 4h
  routes:
    - match:
        severity: critical
      receiver: default-webhook
      repeat_interval: 1h
    - match:
        severity: warning
      receiver: default-webhook
      repeat_interval: 4h

receivers:
  - name: default-webhook
    webhook_configs:
      - url: 'http://localhost:5001/alerts'
        send_resolved: true
`
}

// DefaultGrafanaYAML returns a production-ready Grafana ini configuration.
// If dataDir is non-empty, it configures the [paths] data directory.
func DefaultGrafanaYAML(dataDir string) string {
	if dataDir == "" {
		dataDir = "/var/lib/grafana"
	}
	return `[server]
http_port = 3000
domain = localhost
root_url = %(protocol)s://%(domain)s:%(http_port)s/
serve_from_sub_path = false

[analytics]
reporting_enabled = false
check_for_updates = false

[security]
admin_user = admin
cookie_secure = false
disable_gravatar = true

[users]
allow_sign_up = false
allow_org_create = false
auto_assign_org = true
auto_assign_org_role = Viewer
default_language = zh-Hans

[auth]
disable_login_form = false
oauth_auto_login = false

[auth.anonymous]
enabled = false
org_role = Viewer

[auth.generic_oauth]
enabled = false

[dashboards]
versions_to_keep = 20
min_refresh_interval = 5s

[date_formats]
default_timezone = browser

[unified_alerting]
enabled = true

[log]
mode = file
level = info

[log.file]
daily_rotate = true
max_days = 7

[paths]
data = ` + dataDir + `
logs = ` + dataDir + `/logs
plugins = ` + dataDir + `/plugins
provisioning = /etc/grafana/provisioning
`
}

func GrafanaDeploymentYAML(dataDir string, port int, componentConfig string) string {
	content := DefaultGrafanaYAML(dataDir)
	if port > 0 {
		content = replaceINIValue(content, "server", "http_port", strconv.Itoa(port))
	}
	parameters, err := models.DeploymentParametersFromConfig(componentConfig)
	if err != nil {
		return content
	}
	values := map[string]struct {
		section string
		key     string
	}{
		"domain":                   {"server", "domain"},
		"root_url":                 {"server", "root_url"},
		"serve_from_sub_path":      {"server", "serve_from_sub_path"},
		"reporting_enabled":        {"analytics", "reporting_enabled"},
		"check_for_updates":        {"analytics", "check_for_updates"},
		"disable_gravatar":         {"security", "disable_gravatar"},
		"cookie_secure":            {"security", "cookie_secure"},
		"allow_sign_up":            {"users", "allow_sign_up"},
		"allow_org_create":         {"users", "allow_org_create"},
		"auto_assign_org_role":     {"users", "auto_assign_org_role"},
		"default_language":         {"users", "default_language"},
		"anonymous_enabled":        {"auth.anonymous", "enabled"},
		"anonymous_org_role":       {"auth.anonymous", "org_role"},
		"versions_to_keep":         {"dashboards", "versions_to_keep"},
		"min_refresh_interval":     {"dashboards", "min_refresh_interval"},
		"default_timezone":         {"date_formats", "default_timezone"},
		"unified_alerting_enabled": {"unified_alerting", "enabled"},
		"log_mode":                 {"log", "mode"},
		"log_level":                {"log", "level"},
		"log_retention_days":       {"log.file", "max_days"},
	}
	for field, target := range values {
		value, exists := parameters[field]
		if !exists || value == "" {
			continue
		}
		switch item := value.(type) {
		case string:
			content = replaceINIValue(content, target.section, target.key, item)
		case bool:
			content = replaceINIValue(content, target.section, target.key, strconv.FormatBool(item))
		case float64:
			content = replaceINIValue(content, target.section, target.key, strconv.Itoa(int(item)))
		case int:
			content = replaceINIValue(content, target.section, target.key, strconv.Itoa(item))
		}
	}
	return content
}

func replaceINIValue(content, section, key, value string) string {
	header := "[" + section + "]"
	start := strings.Index(content, header)
	if start < 0 {
		return content
	}
	end := len(content)
	if relativeEnd := strings.Index(content[start+len(header):], "\n["); relativeEnd >= 0 {
		end = start + len(header) + relativeEnd
	}
	segment := content[start:end]
	lines := strings.Split(segment, "\n")
	for index, line := range lines {
		if strings.TrimSpace(strings.SplitN(line, "=", 2)[0]) == key && strings.Contains(line, "=") {
			lines[index] = key + " = " + value
			return content[:start] + strings.Join(lines, "\n") + content[end:]
		}
	}
	return content
}

// DefaultLokiYAML returns a production-ready Loki configuration.
func DefaultLokiYAML() string {
	return `auth_enabled: false

server:
  http_listen_port: 3100
  grpc_listen_port: 9096

common:
  instance_addr: 127.0.0.1
  path_prefix: /loki
  storage:
    type: filesystem
  replication_factor: 1
  ring:
    kvstore:
      store: inmemory

schema_config:
  configs:
    - from: "2023-01-01"
      store: tsdb
      schema: v13
      index:
        prefix: index_
        period: 24h

storage_config:
  filesystem:
    chunks_directory: /loki/chunks
    rules_directory: /loki/rules
  tsdb:
    ship_interval: 10m

limits_config:
  retention_period: 720h
  max_query_length: 720h
  reject_old_samples: true
  reject_old_samples_max_age: 168h
  ingestion_rate_mb: 16
  ingestion_burst_size_mb: 24

table_manager:
  retention_deletes_enabled: true
  retention_period: 720h

ruler:
  alertmanager_url: http://alertmanager:9093
`
}

// DefaultPromtailYAML returns a production-ready Promtail configuration.
func DefaultPromtailYAML() string {
	return `server:
  http_listen_port: 9080
  grpc_listen_port: 0

positions:
  filename: /tmp/positions.yaml

clients:
  - url: http://loki:3100/loki/api/v1/push

scrape_configs:
  - job_name: system
    static_configs:
      - targets:
          - localhost
        labels:
          job: varlogs
          __path__: /var/log/*log

  - job_name: journal
    journal:
      max_age: 12h
      labels:
        job: systemd-journal
    relabel_configs:
      - source_labels: [__journal__systemd_unit]
        target_label: unit
      - source_labels: [__journal__hostname]
        target_label: hostname
`
}

// DefaultBlackboxYAML returns a production-ready Blackbox Exporter configuration.
func DefaultBlackboxYAML() string {
	return `modules:
  http_2xx:
    prober: http
    timeout: 5s
    http:
      valid_http_versions:
        - HTTP/1.1
        - HTTP/2.0
      valid_status_codes:
        - 200
        - 301
        - 302
      method: GET
      preferred_ip_protocol: ip4

  http_post_2xx:
    prober: http
    timeout: 5s
    http:
      valid_http_versions:
        - HTTP/1.1
        - HTTP/2.0
      valid_status_codes:
        - 200
        - 302
      method: POST

  tcp_connect:
    prober: tcp
    timeout: 5s
    tcp:
      preferred_ip_protocol: ip4

  icmp:
    prober: icmp
    timeout: 5s
    icmp:
      preferred_ip_protocol: ip4
      payload_size: 32

  dns:
    prober: dns
    timeout: 5s
    dns:
      query_name: example.com
      query_type: A
`
}
