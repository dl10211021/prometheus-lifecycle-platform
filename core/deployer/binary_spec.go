package deployer

import (
	"encoding/json"
	"fmt"
	"log"
	neturl "net/url"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/promlifecycle/platform/core/config"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
)

// BinarySpec describes how to extract and run a single-binary component.
type BinarySpec struct {
	ComponentType  models.ComponentType // platform component type, e.g. prometheus
	ImageName      string               // base image name, e.g. prom/prometheus
	BinPath        string               // path of the binary inside the image
	BinName        string               // installed binary name (also used as systemd unit name)
	RunArgs        []string             // extra arguments for the binary
	ValidationTool string               // optional native config checker from the same package
	ConfigDir      string               // config directory on target (empty = no config upload)
	ConfigFile     string               // config file name inside ConfigDir
	HasConfig      bool
	DataDir        string // data directory that must exist and be writable
	User           string
	ArchiveFormat  string // "" = tar.gz, "zip" (grafana/loki official packages)
	FullDir        bool   // deploy the whole extracted tree (grafana needs public/ etc.)
	InstallDir     string // target install dir for FullDir mode, e.g. /usr/share/grafana
	DefaultPort    int
	HealthPath     string
	PackageURL     string
	Custom         bool
	Capabilities   []string
}

func binarySpecFor(t models.ComponentType) (BinarySpec, error) {
	switch t {
	case models.ComponentNodeExporter:
		return BinarySpec{ComponentType: models.ComponentNodeExporter, ImageName: "prom/node-exporter", BinPath: "/bin/node_exporter", BinName: "node_exporter",
			User: "nobody", DefaultPort: 9100, HealthPath: "/metrics",
			RunArgs: []string{"--web.listen-address=:9100"}}, nil
	case models.ComponentPrometheus:
		return BinarySpec{ComponentType: models.ComponentPrometheus, ImageName: "prom/prometheus", BinPath: "/bin/prometheus", BinName: "prometheus",
			ValidationTool: "promtool", User: "nobody", ConfigDir: "/etc/prometheus", ConfigFile: "prometheus.yml", HasConfig: true,
			DataDir: "/var/lib/promlifecycle/prometheus", DefaultPort: 9090, HealthPath: "/-/ready",
			RunArgs: []string{"--config.file=/etc/prometheus/prometheus.yml", "--storage.tsdb.path=/var/lib/promlifecycle/prometheus",
				"--storage.tsdb.retention.time=30d", "--storage.tsdb.wal-compression",
				"--query.timeout=2m", "--query.max-concurrency=20",
				"--web.enable-lifecycle", "--web.listen-address=:9090"}}, nil
	case models.ComponentAlertmanager:
		return BinarySpec{ComponentType: models.ComponentAlertmanager, ImageName: "prom/alertmanager", BinPath: "/bin/alertmanager", BinName: "alertmanager",
			ValidationTool: "amtool", User: "nobody", ConfigDir: "/etc/alertmanager", ConfigFile: "alertmanager.yml", HasConfig: true,
			DataDir: "/var/lib/promlifecycle/alertmanager", DefaultPort: 9093, HealthPath: "/-/ready",
			RunArgs: []string{"--config.file=/etc/alertmanager/alertmanager.yml", "--storage.path=/var/lib/promlifecycle/alertmanager",
				"--web.listen-address=:9093"}}, nil
	case models.ComponentBlackbox:
		return BinarySpec{ComponentType: models.ComponentBlackbox, ImageName: "prom/blackbox-exporter", BinPath: "/bin/blackbox_exporter", BinName: "blackbox_exporter",
			User: "nobody", ConfigDir: "/etc/blackbox_exporter", ConfigFile: "blackbox.yml", HasConfig: true,
			DefaultPort: 9115, HealthPath: "/-/healthy", Capabilities: []string{"CAP_NET_RAW"},
			RunArgs: []string{"--config.file=/etc/blackbox_exporter/blackbox.yml", "--web.listen-address=:9115"}}, nil
	case models.ComponentPushgateway:
		return BinarySpec{ComponentType: models.ComponentPushgateway, ImageName: "prom/pushgateway", BinPath: "/bin/pushgateway", BinName: "pushgateway",
			User: "nobody", DataDir: "/var/lib/promlifecycle/pushgateway", DefaultPort: 9091, HealthPath: "/-/healthy",
			RunArgs: []string{"--web.listen-address=:9091", "--persistence.file=/var/lib/promlifecycle/pushgateway/metrics.db"}}, nil
	case models.ComponentLoki:
		return BinarySpec{ComponentType: models.ComponentLoki, ImageName: "grafana/loki", BinPath: "/usr/bin/loki", BinName: "loki",
			User: "nobody", ConfigDir: "/etc/loki", ConfigFile: "config.yaml", HasConfig: true, ArchiveFormat: "zip",
			DataDir: "/var/lib/promlifecycle/loki", DefaultPort: 3100, HealthPath: "/ready",
			RunArgs: []string{"-config.file=/etc/loki/config.yaml", "-config.expand-env=true"}}, nil
	case models.ComponentPromtail:
		return BinarySpec{ComponentType: models.ComponentPromtail, ImageName: "grafana/promtail", BinPath: "/usr/bin/promtail", BinName: "promtail",
			User: "root", ConfigDir: "/etc/promtail", ConfigFile: "config.yaml", HasConfig: true, ArchiveFormat: "zip",
			DataDir: "/var/lib/promlifecycle/promtail", DefaultPort: 9080, HealthPath: "/ready",
			RunArgs: []string{"-config.file=/etc/promtail/config.yaml"}}, nil
	case models.ComponentThanos:
		return BinarySpec{ComponentType: models.ComponentThanos, ImageName: "quay.io/thanos/thanos", BinPath: "/bin/thanos", BinName: "thanos",
			User: "nobody", DefaultPort: 10902, HealthPath: "/-/ready",
			RunArgs: []string{"sidecar", "--http-address=:10902", "--grpc-address=:10901", "--prometheus.url=http://localhost:9090"}}, nil
	case models.ComponentGrafana:
		return BinarySpec{ComponentType: models.ComponentGrafana, ImageName: "grafana/grafana", BinPath: "/usr/share/grafana/bin/grafana", BinName: "grafana",
			User: "nobody", ConfigDir: "/etc/grafana", ConfigFile: "grafana.ini", HasConfig: true,
			DataDir: "/var/lib/promlifecycle/grafana", FullDir: true, InstallDir: "/opt/promlifecycle/components/grafana",
			DefaultPort: 3000, HealthPath: "/api/health",
			RunArgs: []string{"server", "-homepath", "/opt/promlifecycle/components/grafana", "-config", "/etc/grafana/grafana.ini"}}, nil
	default:
		return BinarySpec{}, fmt.Errorf("binary deploy not supported for component type %s", t)
	}
}

// IsBinarySupported reports whether a component type can be deployed without Docker.
func IsBinarySupported(t models.ComponentType) bool {
	_, err := binarySpecFor(t)
	return err == nil
}

// IsBinarySupportedComponent also recognizes immutable custom-definition
// snapshots embedded in Component.Config.
func IsBinarySupportedComponent(component *models.Component) bool {
	_, err := binarySpecForComponent(component)
	return err == nil
}

func binarySpecForComponent(component *models.Component) (BinarySpec, error) {
	if component == nil {
		return BinarySpec{}, fmt.Errorf("component is required")
	}
	if component.ImportedArgs != "" {
		var args []string
		if err := json.Unmarshal([]byte(component.ImportedArgs), &args); err != nil {
			return BinarySpec{}, fmt.Errorf("invalid imported argument snapshot: %w", err)
		}
	}
	if spec, err := binarySpecFor(component.Type); err == nil {
		spec.HealthPath = models.ManagedHealthPath(component)
		return instanceBinarySpec(spec, component), nil
	}
	snapshot, found, err := models.ComponentDefinitionSnapshotFromConfig(component.Config)
	if err != nil {
		return BinarySpec{}, err
	}
	if !found {
		return BinarySpec{}, fmt.Errorf("binary deploy not supported for component type %s", component.Type)
	}
	spec, err := binarySpecFromSnapshot(component.Type, snapshot)
	return instanceBinarySpec(spec, component), err
}

// Runtime files must not sit below the platform's root-only secret directory.
const instanceConfigRoot = "/etc/promlifecycle-components"

// InstanceKey is the durable filesystem identity for a newly managed instance.
func InstanceKey(component *models.Component) string {
	return fmt.Sprintf("%s-%d", component.Type, component.ID)
}

func instanceBinarySpec(spec BinarySpec, component *models.Component) BinarySpec {
	oldConfig := filepath.Join(spec.ConfigDir, spec.ConfigFile)
	if spec.HasConfig {
		if component.ConfigPath != "" {
			spec.ConfigDir, spec.ConfigFile = filepath.Dir(component.ConfigPath), filepath.Base(component.ConfigPath)
		} else if component.ID != 0 && component.ServiceName == "" && component.LastDeployedAt == nil {
			spec.ConfigDir = filepath.Join(instanceConfigRoot, InstanceKey(component))
		}
		spec.RunArgs = append([]string(nil), spec.RunArgs...)
		for i, arg := range spec.RunArgs {
			spec.RunArgs[i] = strings.ReplaceAll(arg, oldConfig, filepath.Join(spec.ConfigDir, spec.ConfigFile))
		}
	}
	return spec
}

func binarySpecFromSnapshot(componentType models.ComponentType, snapshot models.ComponentDefinitionSnapshot) (BinarySpec, error) {
	if snapshot.Type == "" || snapshot.Type != string(componentType) {
		return BinarySpec{}, fmt.Errorf("component definition snapshot type mismatch")
	}
	if !utils.SafePathSegment(snapshot.Type) || !utils.SafePathSegment(snapshot.BinName) {
		return BinarySpec{}, fmt.Errorf("component definition contains an unsafe type or binary name")
	}
	if snapshot.DefaultPort < 1 || snapshot.DefaultPort > 65535 {
		return BinarySpec{}, fmt.Errorf("component definition contains an invalid default port")
	}
	if !validSnapshotPath(snapshot.ConfigDir) || !validSnapshotPath(snapshot.DataDir) {
		return BinarySpec{}, fmt.Errorf("component definition contains an unsafe path")
	}
	if snapshot.HealthPath != "" && (!strings.HasPrefix(snapshot.HealthPath, "/") ||
		filepath.Clean(snapshot.HealthPath) != snapshot.HealthPath ||
		strings.ContainsAny(snapshot.HealthPath, "?#\\\r\n\x00")) {
		return BinarySpec{}, fmt.Errorf("component definition contains an unsafe health path")
	}
	packageURL := strings.ReplaceAll(snapshot.PackageURL, "{version}", "1.0.0")
	parsed, err := neturl.Parse(packageURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return BinarySpec{}, fmt.Errorf("component definition package URL must use HTTPS")
	}
	args := strings.Fields(snapshot.Command)
	if len(args) > 0 && args[0] == snapshot.BinName {
		args = args[1:]
	}
	for _, arg := range args {
		if !validDefinitionArg(arg) {
			return BinarySpec{}, fmt.Errorf("component definition contains an unsafe command argument")
		}
	}
	format := ""
	if strings.HasSuffix(strings.ToLower(parsed.Path), ".zip") {
		format = "zip"
	}
	return BinarySpec{
		ComponentType: componentType, BinName: snapshot.BinName, RunArgs: args,
		ConfigDir: snapshot.ConfigDir, DataDir: snapshot.DataDir, User: "nobody",
		ArchiveFormat: format, DefaultPort: snapshot.DefaultPort,
		HealthPath: snapshot.HealthPath, PackageURL: snapshot.PackageURL, Custom: true,
	}, nil
}

func validSnapshotPath(value string) bool {
	if value == "" {
		return true
	}
	if !filepath.IsAbs(value) || filepath.Clean(value) != value || value == "/" {
		return false
	}
	for _, part := range strings.Split(strings.TrimPrefix(value, "/"), "/") {
		if !utils.SafePathSegment(part) {
			return false
		}
	}
	return true
}

func validDefinitionArg(value string) bool {
	if value == "" || len(value) > 512 || strings.ContainsAny(value, "\r\n\x00%$;'\"`|&<>\\") {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("-_=.:/,{}+@", r):
		default:
			return false
		}
	}
	return !strings.Contains(value, "{") || strings.Count(value, "{port}") == strings.Count(value, "{")
}

// imageFor returns "name:tag" with a sensible default tag per component.
func (s BinarySpec) imageFor(version string) string {
	if version == "" {
		switch s.ImageName {
		case "prom/node-exporter":
			version = "v1.7.0"
		case "prom/prometheus":
			version = "v2.53.0"
		case "prom/alertmanager":
			version = "v0.27.0"
		case "prom/blackbox-exporter":
			version = "v0.24.0"
		case "prom/pushgateway":
			version = "v1.6.2"
		default:
			version = "latest"
		}
	}
	if !strings.HasPrefix(version, "v") && s.ImageName != "grafana/loki" && s.ImageName != "grafana/promtail" {
		version = "v" + version
	}
	return s.ImageName + ":" + version
}

// defaultConfigFor returns the platform default config YAML for components that need one.
func defaultConfigFor(t models.ComponentType) string {
	switch t {
	case models.ComponentPrometheus:
		cfg := config.DefaultPrometheusConfig()
		if b, err := config.ToYAML(cfg); err == nil {
			return string(b)
		}
	case models.ComponentAlertmanager:
		cfg := config.DefaultAlertmanagerConfig()
		if b, err := config.ToYAML(cfg); err == nil {
			return string(b)
		}
	case models.ComponentLoki:
		return config.DefaultLokiYAML()
	case models.ComponentPromtail:
		return config.DefaultPromtailYAML()
	case models.ComponentGrafana:
		return config.DefaultGrafanaYAML("")
	case models.ComponentBlackbox:
		return config.DefaultBlackboxYAML()
	}
	return ""
}

func defaultConfigForComponent(component *models.Component) string {
	if component == nil {
		return ""
	}
	spec, _ := binarySpecForComponent(component)
	dataDir := effectiveDataDir(spec, component)
	switch component.Type {
	case models.ComponentPrometheus:
		cfg := config.DefaultPrometheusConfig()
		cfg["rule_files"] = []string{filepath.Join(spec.ConfigDir, "rules", "*.yml")}
		scrapeInterval, evaluationInterval, scrapeTimeout := config.PrometheusGlobalIntervals(component.Config)
		if global, ok := cfg["global"].(map[string]interface{}); ok {
			global["scrape_interval"] = scrapeInterval
			global["evaluation_interval"] = evaluationInterval
			global["scrape_timeout"] = scrapeTimeout
		}
		if encoded, err := config.ToYAML(cfg); err == nil {
			return string(encoded)
		}
		return defaultConfigFor(component.Type)
	case models.ComponentAlertmanager:
		cfg := config.DefaultAlertmanagerConfig()
		resolveTimeout, groupWait, groupInterval, repeatInterval := config.AlertmanagerConfigIntervals(component.Config)
		if global, ok := cfg["global"].(map[string]interface{}); ok {
			global["resolve_timeout"] = resolveTimeout
		}
		if route, ok := cfg["route"].(map[string]interface{}); ok {
			route["group_wait"] = groupWait
			route["group_interval"] = groupInterval
			route["repeat_interval"] = repeatInterval
		}
		if encoded, err := config.ToYAML(cfg); err == nil {
			return string(encoded)
		}
		return defaultConfigFor(component.Type)
	case models.ComponentGrafana:
		port := component.Port
		if port == 0 {
			port = 3000
		}
		return strings.ReplaceAll(config.GrafanaDeploymentYAML(dataDir, port, component.Config), "/etc/grafana/provisioning", filepath.Join(spec.ConfigDir, "provisioning"))
	case models.ComponentBlackbox:
		if encoded, err := config.ToYAML(config.BlackboxDeploymentConfig(component.Config)); err == nil {
			return string(encoded)
		}
		return defaultConfigFor(component.Type)
	case models.ComponentLoki:
		if encoded, err := config.ToYAML(config.LokiDeploymentConfig(dataDir, component.Port, component.Config)); err == nil {
			return string(encoded)
		}
		return defaultConfigFor(component.Type)
	case models.ComponentPromtail:
		if encoded, err := config.ToYAML(config.PromtailDeploymentConfig(dataDir, component.Port, component.Config)); err == nil {
			return string(encoded)
		}
		return defaultConfigFor(component.Type)
	default:
		return defaultConfigFor(component.Type)
	}
}

// BestPracticeConfig returns the platform's reviewed baseline configuration
// for a component, honoring its selected port and data path.
func BestPracticeConfig(component *models.Component) string {
	return defaultConfigForComponent(component)
}

// BestPracticeResourceLimits returns the single-node baseline systemd limits.
func BestPracticeResourceLimits(componentType models.ComponentType) string {
	return map[models.ComponentType]string{
		models.ComponentNodeExporter: `{"cpu":"0.25","memory":"256M"}`,
		models.ComponentBlackbox:     `{"cpu":"0.25","memory":"256M"}`,
		models.ComponentPrometheus:   `{"cpu":"1","memory":"2G"}`,
		models.ComponentAlertmanager: `{"cpu":"0.5","memory":"512M"}`,
		models.ComponentGrafana:      `{"cpu":"1","memory":"1G"}`,
		models.ComponentPushgateway:  `{"cpu":"0.25","memory":"256M"}`,
		models.ComponentLoki:         `{"cpu":"1","memory":"2G"}`,
		models.ComponentPromtail:     `{"cpu":"0.25","memory":"256M"}`,
		models.ComponentThanos:       `{"cpu":"0.5","memory":"512M"}`,
	}[componentType]
}

// effectiveDataDir returns the component's data directory, honoring the
// user-provided data path override when set.
func effectiveDataDir(spec BinarySpec, comp *models.Component) string {
	if comp != nil && comp.DataPathOverride != "" {
		return comp.DataPathOverride
	}
	if comp != nil && comp.DataPath != "" {
		return comp.DataPath
	}
	if comp != nil && comp.ID != 0 && comp.ServiceName == "" && comp.LastDeployedAt == nil && spec.DataDir != "" {
		return filepath.Join("/var/lib/promlifecycle", InstanceKey(comp))
	}
	return spec.DataDir
}

// effectiveArgs returns the run arguments with the listen port replaced by the
// component's configured port (defaults to the spec port when unset).
func effectiveArgs(spec BinarySpec, comp *models.Component) []string {
	args := append([]string(nil), spec.RunArgs...)
	// Adopted argv is an immutable import, not an extra-flag list. Preserve
	// subcommands, split flag values, and flags absent from platform defaults.
	imported := comp != nil && comp.ImportedArgs != ""
	if imported {
		var original []string
		if json.Unmarshal([]byte(comp.ImportedArgs), &original) == nil {
			args = original
		}
	}
	port := 0
	if comp != nil {
		port = comp.Port
	}
	if port == 0 {
		port = spec.DefaultPort
	}
	if port > 0 && !imported {
		for i, a := range args {
			a = strings.ReplaceAll(a, "{port}", strconv.Itoa(port))
			if strings.Contains(a, "--web.listen-address=:") {
				a = fmt.Sprintf("--web.listen-address=:%d", port)
			}
			if strings.Contains(a, "--http-address=:") {
				a = fmt.Sprintf("--http-address=:%d", port)
			}
			args[i] = a
		}
	}
	dataDir := effectiveDataDir(spec, comp)
	if dataDir != "" && spec.DataDir != "" && dataDir != spec.DataDir {
		for i, a := range args {
			args[i] = strings.ReplaceAll(a, spec.DataDir, dataDir)
		}
	}
	if comp != nil && spec.FullDir && spec.InstallDir != "" {
		installDir := installDirFor(spec, comp)
		if installDir != spec.InstallDir {
			for i, a := range args {
				args[i] = strings.ReplaceAll(a, spec.InstallDir, installDir)
			}
		}
	}
	if !imported && comp != nil && comp.Type == models.ComponentNodeExporter {
		args = applyNodeExporterDeploymentArgs(args, comp.Config)
	}
	if !imported && comp != nil && comp.Type == models.ComponentPrometheus {
		args = applyPrometheusDeploymentArgs(args, comp.Config)
	}
	if !imported && comp != nil && comp.Type == models.ComponentBlackbox {
		args = applyBlackboxDeploymentArgs(args, comp.Config)
	}
	if !imported && comp != nil && comp.Type == models.ComponentPushgateway {
		args = applyPushgatewayDeploymentArgs(args, comp.Config)
	}
	if !imported && comp != nil && comp.Type == models.ComponentAlertmanager {
		args = applyAlertmanagerDeploymentArgs(args, comp.Config)
	}
	if !imported && comp != nil && comp.Type == models.ComponentThanos {
		args = applyThanosDeploymentArgs(args, comp.Config)
	}
	// User-provided launch args override matching tunables and append new
	// flags. Paths and listen addresses stay controlled by dedicated fields.
	if comp != nil && comp.CustomArgs != "" {
		for _, line := range strings.Split(comp.CustomArgs, "\n") {
			a := strings.TrimSpace(line)
			if a == "" {
				continue
			}
			if !validCustomArg(a) {
				log.Printf("[Deploy] ignoring unsafe custom arg %q for %s", a, comp.Name)
				continue
			}
			key := a
			if i := strings.IndexByte(a, '='); i > 0 {
				key = a[:i]
			}
			replaced := false
			endpointLocked := (comp.Type == models.ComponentNodeExporter || comp.Type == models.ComponentBlackbox || comp.Type == models.ComponentPushgateway) &&
				(key == "--web.telemetry-path" || key == "--web.route-prefix") ||
				(comp.Type == models.ComponentBlackbox || comp.Type == models.ComponentPushgateway) && key == "--web.external-url" ||
				comp.Type == models.ComponentPushgateway && key == "--persistence.file" ||
				comp.Type == models.ComponentThanos && (key == "--http-address" || key == "--grpc-address" || key == "--tsdb.path" ||
					key == "--reloader.config-file" || key == "--reloader.config-envsubst-file" || key == "--reloader.rule-dir")
			locked := key == "--web.listen-address" || key == "--config.file" ||
				key == "--storage.tsdb.path" || key == "--storage.path" ||
				key == "-config.file" || key == "-config" || key == "-homepath" || endpointLocked
			for i, existing := range args {
				existingKey := existing
				if p := strings.IndexByte(existing, '='); p > 0 {
					existingKey = existing[:p]
				}
				if existingKey != key {
					continue
				}
				if locked {
					log.Printf("[Deploy] keeping platform-managed flag %q for %s", key, comp.Name)
				} else {
					if existing == key && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
						args = append(args[:i+1], args[i+2:]...)
					}
					args[i] = a
				}
				replaced = true
				break
			}
			if !replaced && locked {
				log.Printf("[Deploy] ignoring platform-managed flag %q for %s", key, comp.Name)
			} else if !replaced {
				args = append(args, a)
			}
		}
	}
	return args
}

func applyPrometheusDeploymentArgs(args []string, componentConfig string) []string {
	parameters, err := models.DeploymentParametersFromConfig(componentConfig)
	if err != nil {
		log.Printf("[Deploy] ignoring invalid Prometheus deployment parameters: %v", err)
		return args
	}
	stringFlag := func(field, flag string) {
		if value, ok := parameters[field].(string); ok && value != "" {
			args = replaceOrAppendFlag(args, flag, value)
		}
	}
	stringFlag("retention_time", "--storage.tsdb.retention.time")
	stringFlag("retention_size", "--storage.tsdb.retention.size")
	stringFlag("query_timeout", "--query.timeout")
	stringFlag("external_url", "--web.external-url")
	if value, ok := parameters["query_max_concurrency"].(float64); ok {
		args = replaceOrAppendFlag(args, "--query.max-concurrency", strconv.Itoa(int(value)))
	} else if value, ok := parameters["query_max_concurrency"].(int); ok {
		args = replaceOrAppendFlag(args, "--query.max-concurrency", strconv.Itoa(value))
	}
	if enabled, exists := parameters["enable_lifecycle"].(bool); exists {
		args = removeFlag(args, "--web.enable-lifecycle")
		if enabled {
			args = append(args, "--web.enable-lifecycle")
		}
	}
	return args
}

func applyAlertmanagerDeploymentArgs(args []string, componentConfig string) []string {
	parameters, err := models.DeploymentParametersFromConfig(componentConfig)
	if err != nil {
		log.Printf("[Deploy] ignoring invalid Alertmanager deployment parameters: %v", err)
		return args
	}
	stringFlag := func(field, flag string) {
		if value, ok := parameters[field].(string); ok && value != "" {
			args = replaceOrAppendFlag(args, flag, value)
		}
	}
	stringFlag("data_retention", "--data.retention")
	stringFlag("data_maintenance_interval", "--data.maintenance-interval")
	stringFlag("alerts_gc_interval", "--alerts.gc-interval")
	stringFlag("external_url", "--web.external-url")
	stringFlag("route_prefix", "--web.route-prefix")
	stringFlag("web_timeout", "--web.timeout")
	stringFlag("log_level", "--log.level")
	stringFlag("log_format", "--log.format")
	if value, ok := parameters["web_get_concurrency"].(float64); ok {
		args = replaceOrAppendFlag(args, "--web.get-concurrency", strconv.Itoa(int(value)))
	} else if value, ok := parameters["web_get_concurrency"].(int); ok {
		args = replaceOrAppendFlag(args, "--web.get-concurrency", strconv.Itoa(value))
	}
	if enabled, exists := parameters["enable_cluster"].(bool); exists {
		args = removeFlag(args, "--cluster.listen-address")
		if !enabled {
			args = append(args, "--cluster.listen-address=")
		}
	}
	return args
}

func applyNodeExporterDeploymentArgs(args []string, componentConfig string) []string {
	parameters, err := models.DeploymentParametersFromConfig(componentConfig)
	if err != nil {
		log.Printf("[Deploy] ignoring invalid Node Exporter deployment parameters: %v", err)
		return args
	}
	if disabled, ok := parameters["disabled_collectors"].([]interface{}); ok {
		for _, value := range disabled {
			if collector, ok := value.(string); ok {
				args = append(args, "--no-collector."+collector)
			}
		}
	}
	if enabled, ok := parameters["enabled_collectors"].([]interface{}); ok {
		for _, value := range enabled {
			if collector, ok := value.(string); ok {
				args = append(args, "--collector."+collector)
			}
		}
	}
	stringFlag := func(field, flag string) {
		if value, ok := parameters[field].(string); ok && value != "" {
			args = replaceOrAppendFlag(args, flag, value)
		}
	}
	stringFlag("filesystem_mount_exclude", "--collector.filesystem.mount-points-exclude")
	stringFlag("filesystem_type_exclude", "--collector.filesystem.fs-types-exclude")
	stringFlag("disk_device_exclude", "--collector.diskstats.device-exclude")
	stringFlag("network_device_exclude", "--collector.netdev.device-exclude")
	stringFlag("log_level", "--log.level")
	stringFlag("log_format", "--log.format")
	for field, flag := range map[string]string{
		"web_max_requests": "--web.max-requests", "runtime_gomaxprocs": "--runtime.gomaxprocs",
	} {
		if value, ok := parameters[field].(float64); ok {
			args = replaceOrAppendFlag(args, flag, strconv.Itoa(int(value)))
		} else if value, ok := parameters[field].(int); ok {
			args = replaceOrAppendFlag(args, flag, strconv.Itoa(value))
		}
	}
	if disabled, exists := parameters["disable_exporter_metrics"].(bool); exists {
		args = removeFlag(args, "--web.disable-exporter-metrics")
		if disabled {
			args = append(args, "--web.disable-exporter-metrics")
		}
	}
	return args
}

func applyBlackboxDeploymentArgs(args []string, componentConfig string) []string {
	parameters, err := models.DeploymentParametersFromConfig(componentConfig)
	if err != nil {
		log.Printf("[Deploy] ignoring invalid Blackbox Exporter deployment parameters: %v", err)
		return args
	}
	stringFlag := func(field, flag string) {
		if value, ok := parameters[field].(string); ok && value != "" {
			args = replaceOrAppendFlag(args, flag, value)
		}
	}
	stringFlag("external_url", "--web.external-url")
	stringFlag("probe_log_level", "--log.prober")
	stringFlag("log_level", "--log.level")
	stringFlag("log_format", "--log.format")
	if value, ok := parameters["history_limit"].(float64); ok {
		args = replaceOrAppendFlag(args, "--history.limit", strconv.Itoa(int(value)))
	} else if value, ok := parameters["history_limit"].(int); ok {
		args = replaceOrAppendFlag(args, "--history.limit", strconv.Itoa(value))
	}
	return args
}

func applyPushgatewayDeploymentArgs(args []string, componentConfig string) []string {
	parameters, err := models.DeploymentParametersFromConfig(componentConfig)
	if err != nil {
		log.Printf("[Deploy] ignoring invalid Pushgateway deployment parameters: %v", err)
		return args
	}
	if enabled, exists := parameters["persistence_enabled"].(bool); exists {
		if enabled {
			if interval, ok := parameters["persistence_interval"].(string); ok && interval != "" {
				args = replaceOrAppendFlag(args, "--persistence.interval", interval)
			}
		} else {
			args = removeFlag(args, "--persistence.file")
			args = removeFlag(args, "--persistence.interval")
		}
	}
	booleanFlag := func(field, flag string, invert bool) {
		value, exists := parameters[field].(bool)
		if !exists {
			return
		}
		args = removeFlag(args, flag)
		if value != invert {
			args = append(args, flag)
		}
	}
	booleanFlag("consistency_check_enabled", "--push.disable-consistency-check", true)
	booleanFlag("enable_admin_api", "--web.enable-admin-api", false)
	booleanFlag("enable_lifecycle_api", "--web.enable-lifecycle", false)
	stringFlag := func(field, flag string) {
		if value, ok := parameters[field].(string); ok && value != "" {
			args = replaceOrAppendFlag(args, flag, value)
		}
	}
	stringFlag("external_url", "--web.external-url")
	stringFlag("log_level", "--log.level")
	stringFlag("log_format", "--log.format")
	return args
}

func applyThanosDeploymentArgs(args []string, componentConfig string) []string {
	parameters, err := models.DeploymentParametersFromConfig(componentConfig)
	if err != nil {
		log.Printf("[Deploy] ignoring invalid Thanos deployment parameters: %v", err)
		return args
	}
	stringFlag := func(field, flag string) {
		if value, ok := parameters[field].(string); ok && value != "" {
			args = replaceOrAppendFlag(args, flag, value)
		}
	}
	stringFlag("prometheus_url", "--prometheus.url")
	stringFlag("prometheus_ready_timeout", "--prometheus.ready_timeout")
	stringFlag("prometheus_get_config_interval", "--prometheus.get_config_interval")
	stringFlag("prometheus_get_config_timeout", "--prometheus.get_config_timeout")
	stringFlag("tsdb_path", "--tsdb.path")
	stringFlag("http_grace_period", "--http-grace-period")
	stringFlag("grpc_grace_period", "--grpc-grace-period")
	stringFlag("grpc_max_connection_age", "--grpc-server-max-connection-age")
	stringFlag("log_level", "--log.level")
	stringFlag("log_format", "--log.format")
	if enabled, _ := parameters["reloader_enabled"].(bool); enabled {
		stringFlag("reloader_config_file", "--reloader.config-file")
		if value, ok := parameters["reloader_rule_dir"].(string); ok && value != "" {
			args = append(args, "--reloader.rule-dir="+value)
		}
		stringFlag("reloader_watch_interval", "--reloader.watch-interval")
		stringFlag("reloader_retry_interval", "--reloader.retry-interval")
		stringFlag("reloader_method", "--reloader.method")
	}
	return args
}
func replaceOrAppendFlag(args []string, key, value string) []string {
	replacement := key + "=" + value
	for index, argument := range args {
		argumentKey := argument
		if position := strings.IndexByte(argument, '='); position > 0 {
			argumentKey = argument[:position]
		}
		if argumentKey == key {
			args[index] = replacement
			return args
		}
	}
	return append(args, replacement)
}

func removeFlag(args []string, key string) []string {
	result := args[:0]
	for _, argument := range args {
		argumentKey := argument
		if position := strings.IndexByte(argument, '='); position > 0 {
			argumentKey = argument[:position]
		}
		if argumentKey != key {
			result = append(result, argument)
		}
	}
	return result
}

// validCustomArg accepts safe -flag=value or --flag=value arguments.
func validCustomArg(a string) bool {
	if len(a) < 2 || len(a) > 512 || !strings.HasPrefix(a, "-") || a == "-" {
		return false
	}
	for _, r := range a {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '=' || r == '.' || r == ':' || r == '/' || r == ',':
		default:
			return false
		}
	}
	return true
}

// BinaryRuntimeIdentity resolves the same paths and unit used by lifecycle
// actions, including older platform records created before BinaryPath existed.
func BinaryRuntimeIdentity(component *models.Component) (string, string, error) {
	if component == nil {
		return "", "", fmt.Errorf("component is required")
	}
	spec, err := binarySpecForComponent(component)
	if err != nil {
		return "", "", err
	}
	return systemdExecPath(spec, component), serviceUnit(spec, component), nil
}
