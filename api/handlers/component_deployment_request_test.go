package handlers

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/promlifecycle/platform/models"
)

func TestNormalizeDeploymentRequestValidatesSchemaPayload(t *testing.T) {
	handler := &Handler{}
	tests := []struct {
		name       string
		parameters map[string]interface{}
		contains   string
	}{
		{name: "unknown", parameters: map[string]interface{}{"version": "3.5.0", "shell": "rm -rf /"}, contains: "不属于"},
		{name: "wrong number type", parameters: map[string]interface{}{"version": "3.5.0", "port": "9090"}, contains: "必须是整数"},
		{name: "out of range", parameters: map[string]interface{}{"version": "3.5.0", "port": float64(70000)}, contains: "必须在"},
		{name: "missing version", parameters: map[string]interface{}{"port": float64(9090)}, contains: "version"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := componentCreateRequest{Type: "prometheus", Name: "prom", DeployType: "binary", Parameters: test.parameters}
			_, err := handler.normalizeDeploymentRequest(&request, nil)
			if err == nil || !strings.Contains(err.Error(), test.contains) {
				t.Fatalf("error=%v, want containing %q", err, test.contains)
			}
		})
	}
}

func TestNormalizeWindowsDeploymentCollectorsAndHostPlatform(t *testing.T) {
	handler := &Handler{}
	request := componentCreateRequest{
		Type: "windows_exporter", Name: "windows-a", DeployType: "windows",
		Parameters: map[string]interface{}{
			"version": "0.31.8", "port": float64(9182),
			"collectors": []interface{}{"cpu", "memory", "service"},
		},
	}
	definition, err := handler.normalizeDeploymentRequest(&request, nil)
	if err != nil {
		t.Fatal(err)
	}
	collectors, ok := request.Config["collectors"].([]string)
	if !ok || len(collectors) != 3 || request.Port != 9182 {
		t.Fatalf("windows parameters were not normalized: %#v", request)
	}
	if err := validateDeploymentHost(definition, &request, &models.Host{OS: "linux", ConnectionType: "ssh"}); err == nil {
		t.Fatal("windows deployment accepted a Linux SSH host")
	}
	if err := validateDeploymentHost(definition, &request, &models.Host{OS: "windows", ConnectionType: "winrm"}); err != nil {
		t.Fatalf("valid Windows host rejected: %v", err)
	}

	request.Parameters["collectors"] = []interface{}{"cpu", "unsupported"}
	if _, err := handler.normalizeDeploymentRequest(&request, nil); err == nil || !strings.Contains(err.Error(), "不支持的选项") {
		t.Fatalf("unsupported collector error=%v", err)
	}
}

func TestNormalizeGrafanaInitialPasswordIntoEnvironment(t *testing.T) {
	handler := &Handler{}
	request := componentCreateRequest{
		Type: "grafana", Name: "grafana-a", DeployType: "binary",
		Parameters: map[string]interface{}{
			"version": "11.1.0", "port": float64(3000),
			"install_path":       "/opt/promlifecycle/components/grafana",
			"data_path_override": "/var/lib/promlifecycle/grafana",
			"initial_password":   "Strong!Password123",
		},
	}
	if _, err := handler.normalizeDeploymentRequest(&request, nil); err != nil {
		t.Fatal(err)
	}
	var environment map[string]string
	if err := json.Unmarshal([]byte(request.CustomEnv), &environment); err != nil {
		t.Fatal(err)
	}
	if environment["GF_SECURITY_ADMIN_USER"] != "admin" || environment["GF_SECURITY_ADMIN_PASSWORD"] != "Strong!Password123" {
		t.Fatalf("Grafana credentials missing: %#v", environment)
	}
}

func TestNormalizeCustomDefinitionUsesStoredSchema(t *testing.T) {
	handler := &Handler{}
	custom := &models.ComponentDefinition{
		Type: "acme_exporter", DisplayName: "Acme", DefaultPort: 9123,
		ParamSchema: `[{"field":"version","label":"版本","type":"package-version","required":true},{"field":"port","label":"端口","type":"number","default":9123,"min":1,"max":65535},{"field":"install_path","label":"路径","type":"path","default":"/opt/promlifecycle/components/acme","required":true}]`,
	}
	request := componentCreateRequest{Type: custom.Type, Name: "acme", Version: "1.0.0"}
	definition, err := handler.normalizeDeploymentRequest(&request, custom)
	if err != nil {
		t.Fatal(err)
	}
	if definition.BuiltIn || request.DeployType != "binary" || request.Port != 9123 || request.InstallPath == "" {
		t.Fatalf("custom schema was not applied: %#v %#v", definition, request)
	}
}

func TestNormalizePrometheusStructuredParameters(t *testing.T) {
	handler := &Handler{}
	request := componentCreateRequest{
		Type: "prometheus", Name: "prom-structured", DeployType: "binary",
		Parameters: map[string]interface{}{
			"version": "3.5.0", "retention_time": "45d",
			"retention_size": "50GB", "scrape_interval": "30s",
			"evaluation_interval": "20s", "scrape_timeout": "10s",
			"query_timeout": "3m", "query_max_concurrency": float64(40),
			"external_url":     "https://prometheus.example.com/metrics",
			"enable_lifecycle": true,
		},
	}
	if _, err := handler.normalizeDeploymentRequest(&request, nil); err != nil {
		t.Fatal(err)
	}
	if request.Port != 9090 || request.StructuredParameters["retention_time"] != "45d" ||
		request.StructuredParameters["query_max_concurrency"] != 40 {
		t.Fatalf("structured parameters were not normalized: %#v", request.StructuredParameters)
	}
}

func TestNormalizePrometheusStructuredParameterBoundaries(t *testing.T) {
	base := map[string]interface{}{
		"version": "3.5.0", "retention_time": "30d",
		"scrape_interval": "15s", "evaluation_interval": "15s",
		"scrape_timeout": "10s", "query_timeout": "2m",
		"query_max_concurrency": float64(20), "enable_lifecycle": true,
	}
	tests := []struct {
		name, field string
		value       interface{}
		contains    string
	}{
		{name: "duration", field: "retention_time", value: "30days", contains: "正数时长"},
		{name: "size", field: "retention_size", value: "50", contains: "容量值"},
		{name: "url credentials", field: "external_url", value: "https://user:pass@example.com", contains: "无凭据"},
		{name: "timeout exceeds interval", field: "scrape_timeout", value: "20s", contains: "不能大于"},
		{name: "lifecycle disabled", field: "enable_lifecycle", value: false, contains: "必须启用"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parameters := make(map[string]interface{}, len(base)+1)
			for key, value := range base {
				parameters[key] = value
			}
			parameters[test.field] = test.value
			request := componentCreateRequest{Type: "prometheus", Name: "prom", DeployType: "binary", Parameters: parameters}
			_, err := (&Handler{}).normalizeDeploymentRequest(&request, nil)
			if err == nil || !strings.Contains(err.Error(), test.contains) {
				t.Fatalf("error=%v, want containing %q", err, test.contains)
			}
		})
	}
}

func TestNormalizeAlertmanagerStructuredParameters(t *testing.T) {
	request := componentCreateRequest{
		Type: "alertmanager", Name: "alertmanager-structured", DeployType: "binary",
		Parameters: map[string]interface{}{
			"version": "0.27.0", "data_retention": "240h",
			"resolve_timeout": "10m", "group_wait": "45s",
			"group_interval": "10m", "repeat_interval": "8h",
			"data_maintenance_interval": "20m", "alerts_gc_interval": "45m",
			"external_url": "https://alerts.example.com/am", "route_prefix": "/am",
			"web_get_concurrency": float64(64), "web_timeout": "30s",
			"enable_cluster": false, "log_level": "warn", "log_format": "json",
		},
	}
	if _, err := (&Handler{}).normalizeDeploymentRequest(&request, nil); err != nil {
		t.Fatal(err)
	}
	if request.Port != 9093 || request.StructuredParameters["data_retention"] != "240h" ||
		request.StructuredParameters["web_get_concurrency"] != 64 ||
		request.StructuredParameters["enable_cluster"] != false {
		t.Fatalf("structured parameters were not normalized: %#v", request.StructuredParameters)
	}
}

func TestNormalizeAlertmanagerStructuredParameterBoundaries(t *testing.T) {
	base := map[string]interface{}{
		"version": "0.27.0", "data_retention": "120h",
		"resolve_timeout": "5m", "group_wait": "30s",
		"group_interval": "5m", "repeat_interval": "4h",
		"data_maintenance_interval": "15m", "alerts_gc_interval": "30m",
		"route_prefix": "/", "web_get_concurrency": float64(0),
		"enable_cluster": false, "log_level": "info", "log_format": "logfmt",
	}
	tests := []struct {
		name, field string
		value       interface{}
		contains    string
	}{
		{name: "duration", field: "data_retention", value: "120hours", contains: "正数时长"},
		{name: "url credentials", field: "external_url", value: "https://user:pass@example.com", contains: "无凭据"},
		{name: "unsafe route prefix", field: "route_prefix", value: "/am/../admin", contains: "不安全路径段"},
		{name: "negative concurrency", field: "web_get_concurrency", value: float64(-1), contains: "不小于 0"},
		{name: "unsupported log level", field: "log_level", value: "trace", contains: "不支持选项"},
		{name: "group wait exceeds interval", field: "group_wait", value: "10m", contains: "不能大于"},
		{name: "group interval exceeds repeat", field: "group_interval", value: "5h", contains: "不能大于"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parameters := make(map[string]interface{}, len(base)+1)
			for key, value := range base {
				parameters[key] = value
			}
			parameters[test.field] = test.value
			request := componentCreateRequest{Type: "alertmanager", Name: "alertmanager", DeployType: "binary", Parameters: parameters}
			_, err := (&Handler{}).normalizeDeploymentRequest(&request, nil)
			if err == nil || !strings.Contains(err.Error(), test.contains) {
				t.Fatalf("error=%v, want containing %q", err, test.contains)
			}
		})
	}
}

func TestNormalizeGrafanaStructuredParameters(t *testing.T) {
	request := componentCreateRequest{
		Type: "grafana", Name: "grafana-structured", DeployType: "binary",
		Parameters: map[string]interface{}{
			"version": "11.1.0", "initial_password": "Strong!Password123",
			"domain": "grafana.example.com", "root_url": "https://grafana.example.com/monitoring/",
			"serve_from_sub_path": true, "default_language": "en-US",
			"default_timezone": "Asia/Shanghai", "auto_assign_org_role": "Editor",
			"allow_sign_up": false, "allow_org_create": false,
			"anonymous_enabled": false, "anonymous_org_role": "Viewer",
			"unified_alerting_enabled": true, "reporting_enabled": false,
			"check_for_updates": false, "disable_gravatar": true,
			"cookie_secure": true, "versions_to_keep": float64(30),
			"min_refresh_interval": "10s", "log_mode": "console",
			"log_level": "warn", "log_retention_days": float64(14),
		},
	}
	if _, err := (&Handler{}).normalizeDeploymentRequest(&request, nil); err != nil {
		t.Fatal(err)
	}
	if request.Port != 3000 || request.StructuredParameters["domain"] != "grafana.example.com" ||
		request.StructuredParameters["versions_to_keep"] != 30 {
		t.Fatalf("structured parameters were not normalized: %#v", request.StructuredParameters)
	}
	if _, exists := request.StructuredParameters["initial_password"]; exists {
		t.Fatal("initial password leaked into persisted deployment parameters")
	}
}

func TestNormalizeGrafanaStructuredParameterBoundaries(t *testing.T) {
	base := map[string]interface{}{
		"version": "11.1.0", "initial_password": "Strong!Password123",
		"domain": "localhost", "serve_from_sub_path": false,
		"default_language": "zh-Hans", "default_timezone": "browser",
		"auto_assign_org_role": "Viewer", "allow_sign_up": false,
		"allow_org_create": false, "anonymous_enabled": false,
		"anonymous_org_role": "Viewer", "unified_alerting_enabled": true,
		"reporting_enabled": false, "check_for_updates": false,
		"disable_gravatar": true, "cookie_secure": false,
		"versions_to_keep": float64(20), "min_refresh_interval": "5s",
		"log_mode": "file", "log_level": "info", "log_retention_days": float64(7),
	}
	tests := []struct {
		name          string
		changes       map[string]interface{}
		errorContains string
	}{
		{name: "invalid domain", changes: map[string]interface{}{"domain": "https://grafana.example.com"}, errorContains: "主机名或 IP"},
		{name: "invalid timezone", changes: map[string]interface{}{"default_timezone": "Asia/Shanghai\n[server]"}, errorContains: "IANA 时区"},
		{name: "subpath trailing slash", changes: map[string]interface{}{"root_url": "https://grafana.example.com/monitoring", "serve_from_sub_path": true}, errorContains: "必须以 / 结尾"},
		{name: "subpath serving disabled", changes: map[string]interface{}{"root_url": "https://grafana.example.com/monitoring/"}, errorContains: "必须启用"},
		{name: "secure cookie over http", changes: map[string]interface{}{"root_url": "http://grafana.example.com/", "cookie_secure": true}, errorContains: "HTTPS"},
		{name: "unsupported role", changes: map[string]interface{}{"auto_assign_org_role": "Owner"}, errorContains: "不支持选项"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parameters := make(map[string]interface{}, len(base)+len(test.changes))
			for key, value := range base {
				parameters[key] = value
			}
			for key, value := range test.changes {
				parameters[key] = value
			}
			request := componentCreateRequest{Type: "grafana", Name: "grafana", DeployType: "binary", Parameters: parameters}
			_, err := (&Handler{}).normalizeDeploymentRequest(&request, nil)
			if err == nil || !strings.Contains(err.Error(), test.errorContains) {
				t.Fatalf("error=%v, want containing %q", err, test.errorContains)
			}
		})
	}
}

func TestGrafanaDeploymentEnvironmentKeepsSecretsAndRemovesManagedOverrides(t *testing.T) {
	environment, err := grafanaDeploymentEnvironment(
		`{"GF_SECURITY_ADMIN_PASSWORD":"Strong!Password123","GF_USERS_DEFAULT_LANGUAGE":"en-US","CUSTOM":"value"}`,
		`{"GF_SECURITY_SECRET_KEY":"existing-secret","GF_DATE_FORMATS_DEFAULT_TIMEZONE":"utc"}`,
	)
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]string
	if err := json.Unmarshal([]byte(environment), &values); err != nil {
		t.Fatal(err)
	}
	if values["GF_SECURITY_ADMIN_PASSWORD"] != "Strong!Password123" ||
		values["GF_SECURITY_SECRET_KEY"] != "existing-secret" || values["CUSTOM"] != "value" {
		t.Fatalf("Grafana secrets or custom environment lost: %#v", values)
	}
	if values["GF_USERS_DEFAULT_LANGUAGE"] != "" || values["GF_DATE_FORMATS_DEFAULT_TIMEZONE"] != "" {
		t.Fatalf("managed Grafana INI values still overridden by environment: %#v", values)
	}
	public := publicComponent(models.Component{Type: models.ComponentGrafana, CustomEnv: environment})
	var publicValues map[string]string
	if err := json.Unmarshal([]byte(public.CustomEnv), &publicValues); err != nil {
		t.Fatal(err)
	}
	if publicValues["CUSTOM"] != "value" || publicValues["GF_SECURITY_ADMIN_PASSWORD"] != "" ||
		publicValues["GF_SECURITY_SECRET_KEY"] != "" {
		t.Fatalf("Grafana public component leaked secrets or lost safe values: %#v", publicValues)
	}
}

func TestNormalizeGrafanaRedeployKeepsExistingPassword(t *testing.T) {
	request := componentCreateRequest{
		Type: "grafana", Name: "grafana-existing", DeployType: "binary",
		ReplaceExisting: true,
		Parameters:      map[string]interface{}{"version": "11.1.0"},
	}
	if _, err := (&Handler{}).normalizeDeploymentRequest(&request, nil); err != nil {
		t.Fatalf("Grafana redeploy without a replacement password was rejected: %v", err)
	}
	if request.CustomEnv != "" {
		t.Fatalf("Grafana redeploy fabricated replacement credentials: %s", request.CustomEnv)
	}
}

func TestNormalizeNodeExporterStructuredParameters(t *testing.T) {
	request := componentCreateRequest{
		Type: "node_exporter", Name: "node-structured", DeployType: "binary",
		Parameters: map[string]interface{}{
			"version":                  "1.8.2",
			"disabled_collectors":      []interface{}{"bcache", "nfs"},
			"enabled_collectors":       []interface{}{"systemd", "processes"},
			"filesystem_mount_exclude": `^/(dev|proc|sys)($|/)`,
			"filesystem_type_exclude":  `^(proc|sysfs|tmpfs)$`,
			"disk_device_exclude":      `^(loop|ram)\d+$`,
			"network_device_exclude":   `^(veth.*|docker\d+)$`,
			"web_max_requests":         float64(80),
			"disable_exporter_metrics": true,
			"runtime_gomaxprocs":       float64(2),
			"log_level":                "warn",
			"log_format":               "json",
		},
	}
	if _, err := (&Handler{}).normalizeDeploymentRequest(&request, nil); err != nil {
		t.Fatal(err)
	}
	if request.Port != 9100 || request.StructuredParameters["web_max_requests"] != 80 ||
		request.StructuredParameters["disable_exporter_metrics"] != true {
		t.Fatalf("structured parameters were not normalized: %#v", request.StructuredParameters)
	}
}

func TestNormalizeNodeExporterStructuredParameterBoundaries(t *testing.T) {
	tests := []struct {
		name       string
		parameters map[string]interface{}
		contains   string
	}{
		{name: "invalid regex", parameters: map[string]interface{}{"filesystem_mount_exclude": "("}, contains: "不是有效正则"},
		{name: "multiline regex", parameters: map[string]interface{}{"network_device_exclude": "^veth\n.*$"}, contains: "必须是单行"},
		{name: "negative requests", parameters: map[string]interface{}{"web_max_requests": float64(-1)}, contains: "不小于 0"},
		{name: "critical collector", parameters: map[string]interface{}{"disabled_collectors": []interface{}{"filesystem"}}, contains: "默认主机告警依赖"},
		{name: "unsupported collector", parameters: map[string]interface{}{"enabled_collectors": []interface{}{"made_up"}}, contains: "不支持的选项"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parameters := map[string]interface{}{"version": "1.8.2"}
			for key, value := range test.parameters {
				parameters[key] = value
			}
			request := componentCreateRequest{Type: "node_exporter", Name: "node", DeployType: "binary", Parameters: parameters}
			_, err := (&Handler{}).normalizeDeploymentRequest(&request, nil)
			if err == nil || !strings.Contains(err.Error(), test.contains) {
				t.Fatalf("error=%v, want containing %q", err, test.contains)
			}
		})
	}
}

func TestNormalizeBlackboxStructuredParameters(t *testing.T) {
	request := componentCreateRequest{
		Type: "blackbox_exporter", Name: "blackbox-structured", DeployType: "binary",
		Parameters: map[string]interface{}{
			"version": "0.26.0", "probe_timeout": "10s",
			"preferred_ip_protocol": "ip6", "http_method": "HEAD",
			"http_valid_status_codes": []interface{}{"200", "204"},
			"http_follow_redirects":   false, "dns_query_name": "health.example.com",
			"dns_query_type": "AAAA", "http_valid_versions": []interface{}{"HTTP/2.0"},
			"http_fail_if_ssl": false, "http_fail_if_not_ssl": true,
			"icmp_payload_size": float64(64), "dns_valid_rcodes": []interface{}{"NOERROR", "NXDOMAIN"},
			"history_limit": float64(200), "external_url": "https://blackbox.example.com/",
			"probe_log_level": "debug", "log_level": "warn", "log_format": "json",
		},
	}
	if _, err := (&Handler{}).normalizeDeploymentRequest(&request, nil); err != nil {
		t.Fatal(err)
	}
	if request.Port != 9115 || request.StructuredParameters["probe_timeout"] != "10s" ||
		request.StructuredParameters["history_limit"] != 200 {
		t.Fatalf("structured parameters were not normalized: %#v", request.StructuredParameters)
	}
}

func TestNormalizeBlackboxStructuredParameterBoundaries(t *testing.T) {
	tests := []struct {
		name       string
		parameters map[string]interface{}
		contains   string
	}{
		{name: "invalid timeout", parameters: map[string]interface{}{"probe_timeout": "five"}, contains: "正数时长"},
		{name: "conflicting TLS checks", parameters: map[string]interface{}{"http_fail_if_ssl": true, "http_fail_if_not_ssl": true}, contains: "不能同时启用"},
		{name: "unsafe DNS name", parameters: map[string]interface{}{"dns_query_name": "example.com\nmodules:"}, contains: "DNS 查询名称"},
		{name: "negative history", parameters: map[string]interface{}{"history_limit": float64(-1)}, contains: "不小于 0"},
		{name: "external subpath", parameters: map[string]interface{}{"external_url": "https://blackbox.example.com/probes/"}, contains: "暂不支持子路径"},
		{name: "unsupported status", parameters: map[string]interface{}{"http_valid_status_codes": []interface{}{"599"}}, contains: "不支持的选项"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parameters := map[string]interface{}{"version": "0.26.0"}
			for key, value := range test.parameters {
				parameters[key] = value
			}
			request := componentCreateRequest{Type: "blackbox_exporter", Name: "blackbox", DeployType: "binary", Parameters: parameters}
			_, err := (&Handler{}).normalizeDeploymentRequest(&request, nil)
			if err == nil || !strings.Contains(err.Error(), test.contains) {
				t.Fatalf("error=%v, want containing %q", err, test.contains)
			}
		})
	}
}

func TestNormalizePushgatewayStructuredParameters(t *testing.T) {
	request := componentCreateRequest{
		Type: "pushgateway", Name: "pushgateway-structured", DeployType: "binary",
		Parameters: map[string]interface{}{
			"version": "1.6.2", "persistence_enabled": true,
			"persistence_interval": "10m", "consistency_check_enabled": false,
			"enable_admin_api": true, "enable_lifecycle_api": true,
			"external_url": "https://pushgateway.example.com/",
			"log_level":    "warn", "log_format": "json",
		},
	}
	if _, err := (&Handler{}).normalizeDeploymentRequest(&request, nil); err != nil {
		t.Fatal(err)
	}
	if request.Port != 9091 || request.StructuredParameters["persistence_interval"] != "10m" ||
		request.StructuredParameters["consistency_check_enabled"] != false {
		t.Fatalf("structured parameters were not normalized: %#v", request.StructuredParameters)
	}
}

func TestNormalizePushgatewayStructuredParameterBoundaries(t *testing.T) {
	tests := []struct {
		name       string
		parameters map[string]interface{}
		contains   string
	}{
		{name: "interval too short", parameters: map[string]interface{}{"persistence_interval": "500ms"}, contains: "1s 到 24h"},
		{name: "interval too long", parameters: map[string]interface{}{"persistence_interval": "25h"}, contains: "1s 到 24h"},
		{name: "external subpath", parameters: map[string]interface{}{"external_url": "https://pushgateway.example.com/gateway/"}, contains: "暂不支持子路径"},
		{name: "unsupported log format", parameters: map[string]interface{}{"log_format": "text"}, contains: "不支持选项"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parameters := map[string]interface{}{"version": "1.6.2"}
			for key, value := range test.parameters {
				parameters[key] = value
			}
			request := componentCreateRequest{Type: "pushgateway", Name: "pushgateway", DeployType: "binary", Parameters: parameters}
			_, err := (&Handler{}).normalizeDeploymentRequest(&request, nil)
			if err == nil || !strings.Contains(err.Error(), test.contains) {
				t.Fatalf("error=%v, want containing %q", err, test.contains)
			}
		})
	}
}

func TestNormalizeLokiStructuredParameters(t *testing.T) {
	request := componentCreateRequest{
		Type: "loki", Name: "loki-structured", DeployType: "binary",
		Parameters: map[string]interface{}{
			"version": "3.2.0", "retention_period": "1440h",
			"max_query_length": "720h", "reject_old_samples": true,
			"reject_old_samples_max_age": "336h", "ingestion_rate_mb": float64(32),
			"ingestion_burst_size_mb": float64(48), "query_timeout": "2m",
			"max_query_parallelism": float64(64), "per_stream_rate_limit": "6MB",
			"per_stream_rate_limit_burst": "30MB", "compactor_interval": "15m",
			"retention_delete_delay": "3h", "retention_delete_worker_count": float64(40),
			"results_cache_max_size_mb": float64(256),
			"alertmanager_url":          "http://127.0.0.1:9093",
			"log_level":                 "warn", "analytics_reporting_enabled": false,
		},
	}
	if _, err := (&Handler{}).normalizeDeploymentRequest(&request, nil); err != nil {
		t.Fatal(err)
	}
	if request.Port != 3100 || request.StructuredParameters["retention_period"] != "1440h" ||
		request.StructuredParameters["ingestion_rate_mb"] != 32 {
		t.Fatalf("structured parameters were not normalized: %#v", request.StructuredParameters)
	}
}

func TestNormalizeLokiStructuredParameterBoundaries(t *testing.T) {
	tests := []struct {
		name       string
		parameters map[string]interface{}
		contains   string
	}{
		{name: "retention too short", parameters: map[string]interface{}{"retention_period": "23h"}, contains: "不能小于 24h"},
		{name: "old sample age exceeds retention", parameters: map[string]interface{}{"retention_period": "48h", "reject_old_samples_max_age": "72h"}, contains: "不能大于"},
		{name: "burst below rate", parameters: map[string]interface{}{"ingestion_rate_mb": float64(32), "ingestion_burst_size_mb": float64(16)}, contains: "不能小于"},
		{name: "compactor too fast", parameters: map[string]interface{}{"compactor_interval": "30s"}, contains: "1m 到 1h"},
		{name: "invalid stream size", parameters: map[string]interface{}{"per_stream_rate_limit": "3"}, contains: "容量值"},
		{name: "invalid alertmanager URL", parameters: map[string]interface{}{"alertmanager_url": "file:///tmp/socket"}, contains: "HTTP/HTTPS"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parameters := map[string]interface{}{"version": "3.2.0"}
			for key, value := range test.parameters {
				parameters[key] = value
			}
			request := componentCreateRequest{Type: "loki", Name: "loki", DeployType: "binary", Parameters: parameters}
			_, err := (&Handler{}).normalizeDeploymentRequest(&request, nil)
			if err == nil || !strings.Contains(err.Error(), test.contains) {
				t.Fatalf("error=%v, want containing %q", err, test.contains)
			}
		})
	}
}

func TestNormalizePromtailStructuredParameters(t *testing.T) {
	request := componentCreateRequest{
		Type: "promtail", Name: "promtail-structured", DeployType: "binary",
		Parameters: map[string]interface{}{
			"version": "3.2.0", "client_url": "http://127.0.0.1:3100/loki/api/v1/push",
			"file_log_enabled": true, "file_glob": "/var/log/*.log",
			"journal_enabled": true, "journal_max_age": "24h",
			"positions_sync_period": "15s", "file_job_name": "host-files",
			"journal_job_name": "host-journal", "journal_path": "/var/log/journal",
			"batch_wait": "2s", "batch_size_bytes": float64(2097152),
			"client_timeout": "15s", "backoff_min_period": "1s",
			"backoff_max_period": "2m", "backoff_max_retries": float64(20),
			"tenant_id": "tenant-a", "drop_rate_limited_batches": true,
			"positions_ignore_invalid_yaml": true, "log_level": "warn",
		},
	}
	if _, err := (&Handler{}).normalizeDeploymentRequest(&request, nil); err != nil {
		t.Fatal(err)
	}
	if request.Port != 9080 || request.StructuredParameters["positions_sync_period"] != "15s" ||
		request.StructuredParameters["batch_size_bytes"] != 2097152 {
		t.Fatalf("structured parameters were not normalized: %#v", request.StructuredParameters)
	}
}

func TestNormalizePromtailStructuredParameterBoundaries(t *testing.T) {
	tests := []struct {
		name       string
		parameters map[string]interface{}
		contains   string
	}{
		{name: "no sources", parameters: map[string]interface{}{"file_log_enabled": false, "journal_enabled": false}, contains: "至少启用一种"},
		{name: "wrong client endpoint", parameters: map[string]interface{}{"client_url": "http://127.0.0.1:3100/"}, contains: "/loki/api/v1/push"},
		{name: "unsafe file glob", parameters: map[string]interface{}{"file_glob": "/var/log/../secret"}, contains: "安全的绝对路径匹配"},
		{name: "unsafe journal path", parameters: map[string]interface{}{"journal_path": "/var/log/*"}, contains: "安全的绝对目录"},
		{name: "invalid tenant", parameters: map[string]interface{}{"tenant_id": "tenant/a"}, contains: "安全名称"},
		{name: "sync too fast", parameters: map[string]interface{}{"positions_sync_period": "500ms"}, contains: "1s 到 5m"},
		{name: "batch exceeds timeout", parameters: map[string]interface{}{"batch_wait": "20s", "client_timeout": "10s"}, contains: "不能大于"},
		{name: "backoff reversed", parameters: map[string]interface{}{"backoff_min_period": "10s", "backoff_max_period": "1s"}, contains: "不能大于"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parameters := map[string]interface{}{"version": "3.2.0"}
			for key, value := range test.parameters {
				parameters[key] = value
			}
			request := componentCreateRequest{Type: "promtail", Name: "promtail", DeployType: "binary", Parameters: parameters}
			_, err := (&Handler{}).normalizeDeploymentRequest(&request, nil)
			if err == nil || !strings.Contains(err.Error(), test.contains) {
				t.Fatalf("error=%v, want containing %q", err, test.contains)
			}
		})
	}
}

func TestNormalizeThanosStructuredParameters(t *testing.T) {
	request := componentCreateRequest{
		Type: "thanos", Name: "thanos-structured", DeployType: "binary",
		Parameters: map[string]interface{}{
			"version": "0.36.1", "prometheus_url": "http://127.0.0.1:9090",
			"prometheus_ready_timeout": "15m", "prometheus_get_config_interval": "45s",
			"prometheus_get_config_timeout": "10s", "tsdb_path": "/var/lib/promlifecycle/prometheus",
			"reloader_enabled": true, "reloader_config_file": "/etc/prometheus/prometheus.yml",
			"reloader_rule_dir": "/etc/prometheus/rules", "reloader_watch_interval": "2m",
			"reloader_retry_interval": "10s", "reloader_method": "http",
			"http_grace_period": "3m", "grpc_grace_period": "3m",
			"grpc_max_connection_age": "90m", "log_level": "warn", "log_format": "json",
		},
	}
	if _, err := (&Handler{}).normalizeDeploymentRequest(&request, nil); err != nil {
		t.Fatal(err)
	}
	if request.Port != 10902 || request.StructuredParameters["prometheus_ready_timeout"] != "15m" ||
		request.StructuredParameters["reloader_enabled"] != true {
		t.Fatalf("structured parameters were not normalized: %#v", request.StructuredParameters)
	}
}

func TestNormalizeThanosStructuredParameterBoundaries(t *testing.T) {
	tests := []struct {
		name       string
		parameters map[string]interface{}
		contains   string
	}{
		{name: "unsafe TSDB path", parameters: map[string]interface{}{"tsdb_path": "/var/lib/../etc"}, contains: "安全的绝对路径"},
		{name: "unsafe rule path", parameters: map[string]interface{}{"reloader_rule_dir": "/etc/prometheus/*"}, contains: "安全的绝对路径"},
		{name: "config timeout exceeds interval", parameters: map[string]interface{}{"prometheus_get_config_interval": "10s", "prometheus_get_config_timeout": "20s"}, contains: "不能大于"},
		{name: "retry exceeds watch", parameters: map[string]interface{}{"reloader_watch_interval": "10s", "reloader_retry_interval": "20s"}, contains: "不能大于"},
		{name: "unsupported reload method", parameters: map[string]interface{}{"reloader_method": "exec"}, contains: "不支持选项"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parameters := map[string]interface{}{"version": "0.36.1"}
			for key, value := range test.parameters {
				parameters[key] = value
			}
			request := componentCreateRequest{Type: "thanos", Name: "thanos", DeployType: "binary", Parameters: parameters}
			_, err := (&Handler{}).normalizeDeploymentRequest(&request, nil)
			if err == nil || !strings.Contains(err.Error(), test.contains) {
				t.Fatalf("error=%v, want containing %q", err, test.contains)
			}
		})
	}
}

func TestPublicHostRedactsNestedGrafanaSecrets(t *testing.T) {
	host := publicHost(models.Host{Components: []models.Component{{
		Type:      models.ComponentGrafana,
		CustomEnv: `{"GF_SECURITY_ADMIN_PASSWORD":"secret","GF_SECURITY_SECRET_KEY":"key","SAFE":"value"}`,
	}}})
	if len(host.Components) != 1 || strings.Contains(host.Components[0].CustomEnv, "secret") ||
		strings.Contains(host.Components[0].CustomEnv, "GF_SECURITY_SECRET_KEY") ||
		!strings.Contains(host.Components[0].CustomEnv, `"SAFE":"value"`) {
		t.Fatalf("nested Grafana environment was not safely redacted: %#v", host.Components)
	}
}

func TestPublicChangePlanRedactsNestedSecrets(t *testing.T) {
	plan := &models.ChangePlan{
		Before: json.RawMessage(`{"component":{"custom_env":"{\"GF_SECURITY_ADMIN_PASSWORD\":\"secret\",\"SAFE\":\"value\"}","password":"secret"}}`),
		After:  json.RawMessage(`{"token":"secret","mode":"keep_data"}`),
	}
	public := publicChangePlan(plan)
	encoded, err := json.Marshal(public)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "secret") {
		t.Fatalf("public change plan leaked secrets: %s", encoded)
	}
	if !strings.Contains(string(plan.Before), "secret") {
		t.Fatal("public redaction mutated the internal rollback snapshot")
	}
}
