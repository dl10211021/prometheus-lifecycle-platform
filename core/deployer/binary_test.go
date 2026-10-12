package deployer

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promlifecycle/platform/models"
)

func TestSystemdUnitQuotesAndOrdersEnvironment(t *testing.T) {
	t.Parallel()

	spec := BinarySpec{BinName: "node_exporter", User: "nobody"}
	component := &models.Component{CustomEnv: `{"Z_LAST":"tail","A_FIRST":"hello world","QUOTED":"say \"hi\"","MULTILINE":"first\nsecond","PERCENT":"95%"}`}
	unit := systemdUnit(spec, component)
	content := environmentFileContent(component)
	want := "A_FIRST=\"hello world\"\n" +
		"MULTILINE=\"first\\nsecond\"\n" +
		"PERCENT=\"95%\"\n" +
		"QUOTED=\"say \\\"hi\\\"\"\n" +
		"Z_LAST=\"tail\"\n"
	if content != want {
		t.Fatalf("protected environment file is unsafe or unstable\nwant:\n%s\ngot:\n%s", want, content)
	}
	if !strings.Contains(unit, "EnvironmentFile=-/etc/promlifecycle/environment/promlifecycle-node_exporter.env") || strings.Contains(unit, "hello world") {
		t.Fatalf("systemd unit must reference the protected environment file without embedding secrets:\n%s", unit)
	}
}

func TestSystemdUnitOmitsInvalidEnvironmentNames(t *testing.T) {
	t.Parallel()

	component := &models.Component{CustomEnv: "{\"SAFE_NAME\":\"ok\",\"BAD\\nExecStart\":\"/bin/false\",\"9INVALID\":\"bad\"}"}
	content := environmentFileContent(component)
	if content != "SAFE_NAME=\"ok\"\n" {
		t.Fatalf("unsafe environment name reached protected environment file:\n%s", content)
	}
}

func TestBinarySpecUsesEmbeddedCustomDefinitionSnapshot(t *testing.T) {
	t.Parallel()
	component := &models.Component{
		Type:   "acme_exporter",
		Config: `{"_component_definition":{"type":"acme_exporter","display_name":"Acme","bin_name":"acme_exporter","default_port":9123,"data_dir":"/var/lib/acme_exporter","health_path":"/health","command":"acme_exporter --listen=:{port}","package_url":"https://downloads.example.com/acme-{version}.tar.gz"}}`,
	}
	spec, err := binarySpecForComponent(component)
	if err != nil {
		t.Fatal(err)
	}
	if !spec.Custom || spec.BinName != "acme_exporter" || spec.DefaultPort != 9123 || spec.HealthPath != "/health" {
		t.Fatalf("unexpected custom binary spec: %+v", spec)
	}
	args := effectiveArgs(spec, &models.Component{Port: 9130})
	if len(args) != 1 || args[0] != "--listen=:9130" {
		t.Fatalf("port placeholder was not resolved: %v", args)
	}
}

func TestBinarySpecRejectsMutableOrUnsafeSnapshotValues(t *testing.T) {
	t.Parallel()
	component := &models.Component{
		Type:   "acme_exporter",
		Config: `{"_component_definition":{"type":"other_exporter","bin_name":"../../bin/sh","default_port":9123,"package_url":"http://127.0.0.1/pkg.tar.gz"}}`,
	}
	if _, err := binarySpecForComponent(component); err == nil {
		t.Fatal("unsafe definition snapshot was accepted")
	}
}

func TestComponentEndpointHostCanonicalizesLocalExecution(t *testing.T) {
	t.Parallel()
	if got := componentEndpointHost(models.Host{IP: "127.0.0.64", ConnectionType: "local"}); got != "127.0.0.1" {
		t.Fatalf("local endpoint host=%q", got)
	}
	if got := componentEndpointHost(models.Host{IP: "192.0.2.10", ConnectionType: "ssh"}); got != "192.0.2.10" {
		t.Fatalf("remote endpoint host=%q", got)
	}
}

func TestRemoteSystemdActivationRequiresEnabledAndActive(t *testing.T) {
	t.Parallel()
	command := remoteSystemdActivation("promlifecycle-node_exporter")
	for _, required := range []string{
		"systemctl enable",
		"systemctl restart",
		"systemctl is-enabled --quiet",
		"systemctl is-active --quiet",
	} {
		if !strings.Contains(command, required) {
			t.Fatalf("activation command missing %q: %s", required, command)
		}
	}
}

func TestExtractBinaryPrefersExactNameOverPrefixedAsset(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()
	packagePath := filepath.Join(tempDir, "prometheus.tar.gz")
	file, err := os.Create(packagePath)
	if err != nil {
		t.Fatal(err)
	}
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	entries := []struct {
		name    string
		mode    int64
		content string
	}{
		{"prometheus-2.55.0/consoles/prometheus-overview.html", 0644, "<html>not a binary</html>"},
		{"prometheus-2.55.0/prometheus", 0755, "\x7fELFexact-binary"},
	}
	for _, entry := range entries {
		if err := tarWriter.WriteHeader(&tar.Header{Name: entry.name, Mode: entry.mode, Size: int64(len(entry.content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write([]byte(entry.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	destination := filepath.Join(tempDir, "prometheus")
	if err := extractBinaryFromPackage(packagePath, "prometheus", destination, "tar.gz"); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "\x7fELFexact-binary" {
		t.Fatalf("extracted prefixed asset instead of exact binary: %q", content)
	}
}

func TestFullMonitoringBinaryDefaultsAndOverrides(t *testing.T) {
	t.Parallel()

	prometheus, err := binarySpecFor(models.ComponentPrometheus)
	if err != nil {
		t.Fatal(err)
	}
	if prometheus.DefaultPort != 9090 || prometheus.HealthPath != "/-/ready" {
		t.Fatalf("prometheus readiness defaults are incomplete: %+v", prometheus)
	}
	args := effectiveArgs(prometheus, &models.Component{
		Port:             19090,
		DataPathOverride: "/srv/prometheus",
		CustomArgs:       "--storage.tsdb.retention.time=15d\n--query.max-concurrency=40\n--config.file=/tmp/unsafe.yml",
	})
	joined := strings.Join(args, " ")
	for _, required := range []string{
		"--web.listen-address=:19090",
		"--storage.tsdb.path=/srv/prometheus",
		"--storage.tsdb.retention.time=15d",
		"--query.max-concurrency=40",
		"--config.file=/etc/prometheus/prometheus.yml",
	} {
		if !strings.Contains(joined, required) {
			t.Fatalf("effective prometheus args missing %q: %s", required, joined)
		}
	}
	if strings.Contains(joined, "--storage.tsdb.retention.time=30d") || strings.Contains(joined, "/tmp/unsafe.yml") {
		t.Fatalf("custom tunable override or protected flag handling failed: %s", joined)
	}

	grafana, err := binarySpecFor(models.ComponentGrafana)
	if err != nil {
		t.Fatal(err)
	}
	grafanaComponent := &models.Component{
		Type:             models.ComponentGrafana,
		Port:             3300,
		InstallPath:      "/srv/grafana",
		DataPathOverride: "/srv/grafana-data",
	}
	grafanaArgs := strings.Join(effectiveArgs(grafana, grafanaComponent), " ")
	if !strings.Contains(grafanaArgs, "-homepath /srv/grafana") {
		t.Fatalf("grafana custom install path did not reach homepath: %s", grafanaArgs)
	}
	grafanaConfig := defaultConfigForComponent(grafanaComponent)
	for _, required := range []string{"http_port = 3300", "default_language = zh-Hans", "data = /srv/grafana-data"} {
		if !strings.Contains(grafanaConfig, required) {
			t.Fatalf("grafana config missing %q:\n%s", required, grafanaConfig)
		}
	}

	blackbox, err := binarySpecFor(models.ComponentBlackbox)
	if err != nil {
		t.Fatal(err)
	}
	unit := systemdUnit(blackbox, &models.Component{ResourceLimits: `{"cpu":"0.25","memory":"256M"}`})
	for _, required := range []string{
		"AmbientCapabilities=CAP_NET_RAW",
		"CapabilityBoundingSet=CAP_NET_RAW",
		"CPUQuota=25%",
		"MemoryMax=256M",
		"NoNewPrivileges=true",
	} {
		if !strings.Contains(unit, required) {
			t.Fatalf("blackbox systemd unit missing %q:\n%s", required, unit)
		}
	}
}

func TestPrometheusStructuredParametersCompileToArgsAndConfig(t *testing.T) {
	t.Parallel()
	componentConfig, err := models.EmbedDeploymentParameters("{}", map[string]interface{}{
		"retention_time": "45d", "retention_size": "50GB",
		"scrape_interval": "30s", "evaluation_interval": "20s", "scrape_timeout": "10s",
		"query_timeout": "3m", "query_max_concurrency": 40,
		"external_url": "https://prometheus.example.com/metrics", "enable_lifecycle": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	component := &models.Component{
		Type: models.ComponentPrometheus, Port: 9090, Config: componentConfig,
		CustomArgs: "--query.timeout=4m",
	}
	spec, err := binarySpecFor(models.ComponentPrometheus)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(effectiveArgs(spec, component), " ")
	for _, required := range []string{
		"--storage.tsdb.retention.time=45d",
		"--storage.tsdb.retention.size=50GB",
		"--query.timeout=4m",
		"--query.max-concurrency=40",
		"--web.external-url=https://prometheus.example.com/metrics",
		"--web.enable-lifecycle",
	} {
		if !strings.Contains(args, required) {
			t.Fatalf("compiled Prometheus args missing %q: %s", required, args)
		}
	}
	if strings.Contains(args, "--query.timeout=3m") {
		t.Fatalf("custom arguments must retain final priority: %s", args)
	}
	generatedConfig := defaultConfigForComponent(component)
	for _, required := range []string{
		"scrape_interval: 30s",
		"evaluation_interval: 20s",
		"scrape_timeout: 10s",
	} {
		if !strings.Contains(generatedConfig, required) {
			t.Fatalf("generated Prometheus config missing %q:\n%s", required, generatedConfig)
		}
	}
}

func TestAlertmanagerStructuredParametersCompileToArgsAndConfig(t *testing.T) {
	t.Parallel()
	componentConfig, err := models.EmbedDeploymentParameters("{}", map[string]interface{}{
		"data_retention": "240h", "resolve_timeout": "10m",
		"group_wait": "45s", "group_interval": "10m", "repeat_interval": "8h",
		"data_maintenance_interval": "20m", "alerts_gc_interval": "45m",
		"external_url": "https://alerts.example.com/am", "route_prefix": "/am",
		"web_get_concurrency": 64, "web_timeout": "30s",
		"enable_cluster": false, "log_level": "warn", "log_format": "json",
	})
	if err != nil {
		t.Fatal(err)
	}
	component := &models.Component{
		Type: models.ComponentAlertmanager, Port: 9093, Config: componentConfig,
		CustomArgs: "--data.retention=360h",
	}
	spec, err := binarySpecFor(models.ComponentAlertmanager)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(effectiveArgs(spec, component), " ")
	for _, required := range []string{
		"--data.retention=360h",
		"--data.maintenance-interval=20m",
		"--alerts.gc-interval=45m",
		"--web.external-url=https://alerts.example.com/am",
		"--web.route-prefix=/am",
		"--web.get-concurrency=64",
		"--web.timeout=30s",
		"--cluster.listen-address=",
		"--log.level=warn",
		"--log.format=json",
	} {
		if !strings.Contains(args, required) {
			t.Fatalf("compiled Alertmanager args missing %q: %s", required, args)
		}
	}
	if strings.Contains(args, "--data.retention=240h") {
		t.Fatalf("custom arguments must retain final priority: %s", args)
	}
	generatedConfig := defaultConfigForComponent(component)
	for _, required := range []string{
		"resolve_timeout: 10m",
		"group_wait: 45s",
		"group_interval: 10m",
		"repeat_interval: 8h",
	} {
		if !strings.Contains(generatedConfig, required) {
			t.Fatalf("generated Alertmanager config missing %q:\n%s", required, generatedConfig)
		}
	}
}

func TestGrafanaStructuredParametersCompileToINI(t *testing.T) {
	t.Parallel()
	componentConfig, err := models.EmbedDeploymentParameters("{}", map[string]interface{}{
		"domain": "grafana.example.com", "root_url": "https://grafana.example.com/monitoring/",
		"serve_from_sub_path": true, "default_language": "en-US",
		"default_timezone": "Asia/Shanghai", "auto_assign_org_role": "Editor",
		"allow_sign_up": false, "allow_org_create": false,
		"anonymous_enabled": true, "anonymous_org_role": "Viewer",
		"unified_alerting_enabled": true, "reporting_enabled": false,
		"check_for_updates": false, "disable_gravatar": true,
		"cookie_secure": true, "versions_to_keep": 30,
		"min_refresh_interval": "10s", "log_mode": "console",
		"log_level": "warn", "log_retention_days": 14,
	})
	if err != nil {
		t.Fatal(err)
	}
	component := &models.Component{
		Type: models.ComponentGrafana, Port: 3300,
		InstallPath: "/srv/grafana", DataPathOverride: "/srv/grafana-data",
		Config: componentConfig, CustomArgs: "-homepath=/tmp/unsafe",
	}
	generatedConfig := defaultConfigForComponent(component)
	for _, required := range []string{
		"http_port = 3300",
		"domain = grafana.example.com",
		"root_url = https://grafana.example.com/monitoring/",
		"serve_from_sub_path = true",
		"default_language = en-US",
		"default_timezone = Asia/Shanghai",
		"auto_assign_org_role = Editor",
		"enabled = true",
		"cookie_secure = true",
		"versions_to_keep = 30",
		"min_refresh_interval = 10s",
		"mode = console",
		"level = warn",
		"max_days = 14",
		"data = /srv/grafana-data",
	} {
		if !strings.Contains(generatedConfig, required) {
			t.Fatalf("generated Grafana config missing %q:\n%s", required, generatedConfig)
		}
	}
	spec, err := binarySpecFor(models.ComponentGrafana)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(effectiveArgs(spec, component), " ")
	if !strings.Contains(args, "-homepath /srv/grafana") || strings.Contains(args, "/tmp/unsafe") {
		t.Fatalf("Grafana protected launch paths were overridden: %s", args)
	}
}

func TestNodeExporterStructuredParametersCompileToArgs(t *testing.T) {
	t.Parallel()
	componentConfig, err := models.EmbedDeploymentParameters("{}", map[string]interface{}{
		"disabled_collectors":      []string{"bcache", "nfs"},
		"enabled_collectors":       []string{"systemd", "processes"},
		"filesystem_mount_exclude": `^/(dev|proc|sys)($|/)`,
		"filesystem_type_exclude":  `^(proc|sysfs|tmpfs)$`,
		"disk_device_exclude":      `^(loop|ram)\d+$`,
		"network_device_exclude":   `^(veth.*|docker\d+)$`,
		"web_max_requests":         80,
		"disable_exporter_metrics": true,
		"runtime_gomaxprocs":       2,
		"log_level":                "warn",
		"log_format":               "json",
	})
	if err != nil {
		t.Fatal(err)
	}
	component := &models.Component{
		Type: models.ComponentNodeExporter, Port: 19100, Config: componentConfig,
		CustomArgs: "--web.max-requests=120\n--web.listen-address=:9999",
	}
	spec, err := binarySpecFor(models.ComponentNodeExporter)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(effectiveArgs(spec, component), " ")
	for _, required := range []string{
		"--web.listen-address=:19100",
		"--no-collector.bcache", "--no-collector.nfs",
		"--collector.systemd", "--collector.processes",
		"--collector.filesystem.mount-points-exclude=^/(dev|proc|sys)($|/)",
		"--collector.filesystem.fs-types-exclude=^(proc|sysfs|tmpfs)$",
		`--collector.diskstats.device-exclude=^(loop|ram)\d+$`,
		`--collector.netdev.device-exclude=^(veth.*|docker\d+)$`,
		"--web.max-requests=120",
		"--web.disable-exporter-metrics",
		"--runtime.gomaxprocs=2",
		"--log.level=warn", "--log.format=json",
	} {
		if !strings.Contains(args, required) {
			t.Fatalf("compiled Node Exporter args missing %q: %s", required, args)
		}
	}
	if strings.Contains(args, "--web.max-requests=80") || strings.Contains(args, ":9999") {
		t.Fatalf("custom priority or protected listen address handling failed: %s", args)
	}
	unit := systemdUnit(spec, component)
	if !strings.Contains(unit, `"--collector.diskstats.device-exclude=^(loop|ram)\\d+$"`) {
		t.Fatalf("systemd unit did not preserve regex escaping:\n%s", unit)
	}
}

func TestBlackboxStructuredParametersCompileToArgsAndConfig(t *testing.T) {
	t.Parallel()
	componentConfig, err := models.EmbedDeploymentParameters("{}", map[string]interface{}{
		"probe_timeout": "10s", "preferred_ip_protocol": "ip6",
		"http_method": "HEAD", "http_valid_status_codes": []string{"200", "204"},
		"http_follow_redirects": false, "dns_query_name": "health.example.com",
		"dns_query_type": "AAAA", "http_valid_versions": []string{"HTTP/2.0"},
		"http_fail_if_ssl": false, "http_fail_if_not_ssl": true,
		"icmp_payload_size": 64, "dns_valid_rcodes": []string{"NOERROR", "NXDOMAIN"},
		"history_limit": 200, "external_url": "https://blackbox.example.com/",
		"probe_log_level": "debug", "log_level": "warn", "log_format": "json",
	})
	if err != nil {
		t.Fatal(err)
	}
	component := &models.Component{
		Type: models.ComponentBlackbox, Port: 19115, Config: componentConfig,
		CustomArgs: "--history.limit=300\n--web.listen-address=:9999",
	}
	spec, err := binarySpecFor(models.ComponentBlackbox)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(effectiveArgs(spec, component), " ")
	for _, required := range []string{
		"--web.listen-address=:19115",
		"--history.limit=300",
		"--web.external-url=https://blackbox.example.com/",
		"--log.prober=debug", "--log.level=warn", "--log.format=json",
	} {
		if !strings.Contains(args, required) {
			t.Fatalf("compiled Blackbox Exporter args missing %q: %s", required, args)
		}
	}
	if strings.Contains(args, "--history.limit=200") || strings.Contains(args, ":9999") {
		t.Fatalf("custom priority or protected listen address handling failed: %s", args)
	}
	generatedConfig := defaultConfigForComponent(component)
	for _, required := range []string{
		"timeout: 10s", "preferred_ip_protocol: ip6",
		"method: HEAD", "- 204", "follow_redirects: false",
		"fail_if_not_ssl: true", "payload_size: 64",
		"query_name: health.example.com", "query_type: AAAA", "- NXDOMAIN",
	} {
		if !strings.Contains(generatedConfig, required) {
			t.Fatalf("generated Blackbox config missing %q:\n%s", required, generatedConfig)
		}
	}
}

func TestPushgatewayStructuredParametersCompileToArgs(t *testing.T) {
	t.Parallel()
	componentConfig, err := models.EmbedDeploymentParameters("{}", map[string]interface{}{
		"persistence_enabled": true, "persistence_interval": "10m",
		"consistency_check_enabled": false, "enable_admin_api": true,
		"enable_lifecycle_api": true, "external_url": "https://pushgateway.example.com/",
		"log_level": "warn", "log_format": "json",
	})
	if err != nil {
		t.Fatal(err)
	}
	component := &models.Component{
		Type: models.ComponentPushgateway, Port: 19091,
		DataPathOverride: "/srv/pushgateway", Config: componentConfig,
		CustomArgs: "--log.level=error\n--persistence.file=/tmp/unsafe\n--web.external-url=https://unsafe.example.com/sub/",
	}
	spec, err := binarySpecFor(models.ComponentPushgateway)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(effectiveArgs(spec, component), " ")
	for _, required := range []string{
		"--web.listen-address=:19091",
		"--persistence.file=/srv/pushgateway/metrics.db",
		"--persistence.interval=10m",
		"--push.disable-consistency-check",
		"--web.enable-admin-api", "--web.enable-lifecycle",
		"--web.external-url=https://pushgateway.example.com/",
		"--log.level=error", "--log.format=json",
	} {
		if !strings.Contains(args, required) {
			t.Fatalf("compiled Pushgateway args missing %q: %s", required, args)
		}
	}
	if strings.Contains(args, "/tmp/unsafe") || strings.Contains(args, "unsafe.example.com") || strings.Contains(args, "--log.level=warn") {
		t.Fatalf("protected parameter or custom priority handling failed: %s", args)
	}

	disabledConfig, err := models.EmbedDeploymentParameters("{}", map[string]interface{}{
		"persistence_enabled": false, "persistence_interval": "5m",
	})
	if err != nil {
		t.Fatal(err)
	}
	disabledArgs := strings.Join(effectiveArgs(spec, &models.Component{Type: models.ComponentPushgateway, Config: disabledConfig}), " ")
	if strings.Contains(disabledArgs, "--persistence.file") || strings.Contains(disabledArgs, "--persistence.interval") {
		t.Fatalf("disabled persistence still generated storage flags: %s", disabledArgs)
	}
}

func TestLokiStructuredParametersCompileToConfig(t *testing.T) {
	t.Parallel()
	componentConfig, err := models.EmbedDeploymentParameters("{}", map[string]interface{}{
		"retention_period": "1440h", "max_query_length": "720h",
		"reject_old_samples": true, "reject_old_samples_max_age": "336h",
		"ingestion_rate_mb": 32, "ingestion_burst_size_mb": 48,
		"query_timeout": "2m", "max_query_parallelism": 64,
		"per_stream_rate_limit": "6MB", "per_stream_rate_limit_burst": "30MB",
		"compactor_interval": "15m", "retention_delete_delay": "3h",
		"retention_delete_worker_count": 40, "results_cache_max_size_mb": 256,
		"alertmanager_url": "http://127.0.0.1:9093",
		"log_level":        "warn", "analytics_reporting_enabled": false,
	})
	if err != nil {
		t.Fatal(err)
	}
	component := &models.Component{
		Type: models.ComponentLoki, Port: 13100,
		DataPathOverride: "/srv/loki-data", Config: componentConfig,
	}
	generatedConfig := defaultConfigForComponent(component)
	for _, required := range []string{
		"http_listen_port: 13100",
		"log_level: warn",
		"path_prefix: /srv/loki-data",
		"chunks_directory: /srv/loki-data/chunks",
		"active_index_directory: /srv/loki-data/tsdb-index",
		"working_directory: /srv/loki-data/compactor",
		"retention_enabled: true",
		"retention_period: 1440h",
		"reject_old_samples_max_age: 336h",
		"ingestion_rate_mb: 32",
		"ingestion_burst_size_mb: 48",
		"query_timeout: 2m",
		"max_query_parallelism: 64",
		"per_stream_rate_limit: 6MB",
		"per_stream_rate_limit_burst: 30MB",
		"compaction_interval: 15m",
		"retention_delete_delay: 3h",
		"retention_delete_worker_count: 40",
		"max_size_mb: 256",
		"alertmanager_url: http://127.0.0.1:9093",
		"reporting_enabled: false",
		"object_store: filesystem",
	} {
		if !strings.Contains(generatedConfig, required) {
			t.Fatalf("generated Loki config missing %q:\n%s", required, generatedConfig)
		}
	}
	if strings.Contains(generatedConfig, "table_manager:") || strings.Contains(generatedConfig, "http://alertmanager:9093") {
		t.Fatalf("legacy Docker-only or incompatible retention configuration remained:\n%s", generatedConfig)
	}
}

func TestPromtailStructuredParametersCompileToConfig(t *testing.T) {
	t.Parallel()
	componentConfig, err := models.EmbedDeploymentParameters("{}", map[string]interface{}{
		"client_url":       "http://127.0.0.1:3100/loki/api/v1/push",
		"file_log_enabled": true, "file_glob": "/var/log/*.log",
		"journal_enabled": true, "journal_max_age": "24h",
		"positions_sync_period": "15s", "file_job_name": "host-files",
		"journal_job_name": "host-journal", "journal_path": "/var/log/journal",
		"batch_wait": "2s", "batch_size_bytes": 2097152,
		"client_timeout": "15s", "backoff_min_period": "1s",
		"backoff_max_period": "2m", "backoff_max_retries": 20,
		"tenant_id": "tenant-a", "drop_rate_limited_batches": true,
		"positions_ignore_invalid_yaml": true, "log_level": "warn",
	})
	if err != nil {
		t.Fatal(err)
	}
	component := &models.Component{
		Type: models.ComponentPromtail, Port: 19080,
		DataPathOverride: "/srv/promtail-data", Config: componentConfig,
	}
	spec, err := binarySpecFor(models.ComponentPromtail)
	if err != nil {
		t.Fatal(err)
	}
	if spec.User != "root" {
		t.Fatalf("Promtail cannot read system logs and journal as %q", spec.User)
	}
	generatedConfig := defaultConfigForComponent(component)
	for _, required := range []string{
		"http_listen_port: 19080",
		"log_level: warn",
		"filename: /srv/promtail-data/positions.yaml",
		"sync_period: 15s",
		"ignore_invalid_yaml: true",
		"url: http://127.0.0.1:3100/loki/api/v1/push",
		"batchwait: 2s", "batchsize: 2097152", "timeout: 15s",
		"min_period: 1s", "max_period: 2m", "max_retries: 20",
		"tenant_id: tenant-a", "drop_rate_limited_batches: true",
		"job_name: host-files", "__path__: /var/log/*.log",
		"job_name: host-journal", "max_age: 24h", "path: /var/log/journal",
	} {
		if !strings.Contains(generatedConfig, required) {
			t.Fatalf("generated Promtail config missing %q:\n%s", required, generatedConfig)
		}
	}
	if strings.Contains(generatedConfig, "http://loki:3100") || strings.Contains(generatedConfig, "/tmp/positions.yaml") {
		t.Fatalf("legacy Docker-only or volatile Promtail configuration remained:\n%s", generatedConfig)
	}
}

func TestThanosStructuredParametersCompileToArgs(t *testing.T) {
	t.Parallel()
	componentConfig, err := models.EmbedDeploymentParameters("{}", map[string]interface{}{
		"prometheus_url":           "http://127.0.0.1:9090",
		"prometheus_ready_timeout": "15m", "prometheus_get_config_interval": "45s",
		"prometheus_get_config_timeout": "10s", "tsdb_path": "/var/lib/promlifecycle/prometheus",
		"reloader_enabled": true, "reloader_config_file": "/etc/prometheus/prometheus.yml",
		"reloader_rule_dir": "/etc/prometheus/rules", "reloader_watch_interval": "2m",
		"reloader_retry_interval": "10s", "reloader_method": "http",
		"http_grace_period": "3m", "grpc_grace_period": "3m",
		"grpc_max_connection_age": "90m", "log_level": "warn", "log_format": "json",
	})
	if err != nil {
		t.Fatal(err)
	}
	component := &models.Component{
		Type: models.ComponentThanos, Port: 11902, Config: componentConfig,
		CustomArgs: "--log.level=error\n--grpc-address=:9999\n--tsdb.path=/tmp/unsafe",
	}
	spec, err := binarySpecFor(models.ComponentThanos)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(effectiveArgs(spec, component), " ")
	for _, required := range []string{
		"sidecar", "--http-address=:11902", "--grpc-address=:10901",
		"--prometheus.url=http://127.0.0.1:9090",
		"--prometheus.ready_timeout=15m",
		"--prometheus.get_config_interval=45s",
		"--prometheus.get_config_timeout=10s",
		"--tsdb.path=/var/lib/promlifecycle/prometheus",
		"--reloader.config-file=/etc/prometheus/prometheus.yml",
		"--reloader.rule-dir=/etc/prometheus/rules",
		"--reloader.watch-interval=2m", "--reloader.retry-interval=10s",
		"--reloader.method=http", "--http-grace-period=3m",
		"--grpc-grace-period=3m", "--grpc-server-max-connection-age=90m",
		"--log.level=error", "--log.format=json",
	} {
		if !strings.Contains(args, required) {
			t.Fatalf("compiled Thanos args missing %q: %s", required, args)
		}
	}
	if strings.Contains(args, ":9999") || strings.Contains(args, "/tmp/unsafe") || strings.Contains(args, "--log.level=warn") {
		t.Fatalf("protected parameter or custom priority handling failed: %s", args)
	}
}
