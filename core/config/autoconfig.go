package config

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"

	"github.com/promlifecycle/platform/models"
	"gopkg.in/yaml.v3"
	"gorm.io/gorm"
)

// AutoConfig handles automatic configuration generation for Prometheus ecosystem components.
type AutoConfig struct {
	db       *gorm.DB
	basePath string
}

// NewAutoConfig creates a new AutoConfig instance.
func NewAutoConfig(db *gorm.DB, basePath string) *AutoConfig {
	return &AutoConfig{
		db:       db,
		basePath: basePath,
	}
}

func (ac *AutoConfig) resolveClusterScope(clusterID uint) (uint, error) {
	if clusterID != 0 {
		return clusterID, nil
	}
	var node models.Cluster
	if err := ac.db.Where("name = ?", "single-node").First(&node).Error; err != nil {
		return 0, fmt.Errorf("节点范围不存在，请重启服务完成初始化后重试: %w", err)
	}
	return node.ID, nil
}

// FindPrometheusComponent returns the running prometheus component for a cluster.

// PrometheusSpec represents a structured Prometheus configuration.
type PrometheusSpec struct {
	Global        GlobalConfig   `yaml:"global"`
	ScrapeConfigs []ScrapeConfig `yaml:"scrape_configs"`
}

// AlertmanagerSpec represents a structured Alertmanager configuration.
type AlertmanagerSpec struct {
	Global    *AlertmanagerGlobal `yaml:"global,omitempty"`
	Route     *AlertmanagerRoute  `yaml:"route,omitempty"`
	Receivers []Receiver          `yaml:"receivers"`
}

// LokiServerConfig represents Loki server configuration.
type LokiServerConfig struct {
	HTTPListenPort int `json:"http_listen_port"`
}

// LokiCommonConfig represents Loki common configuration.
type LokiCommonConfig struct {
	InstanceAddr string `json:"instance_addr"`
	PathPrefix   string `json:"path_prefix"`
}

// LokiSchemaConfigItem represents a schema config entry.
type LokiSchemaConfigItem struct {
	From   string `json:"from"`
	Store  string `json:"store"`
	Schema string `json:"schema"`
}

// LokiSchemaConfig represents Loki schema configuration.
type LokiSchemaConfig struct {
	Configs []LokiSchemaConfigItem `json:"configs"`
}

// LokiSpec represents a structured Loki configuration.
type LokiSpec struct {
	AuthEnabled  bool              `json:"auth_enabled"`
	ServerConfig *LokiServerConfig `json:"server"`
	CommonConfig *LokiCommonConfig `json:"common"`
	SchemaConfig *LokiSchemaConfig `json:"schema_config"`
}

// HTTPProbeConfig represents an HTTP probe configuration for Blackbox Exporter.
type HTTPProbeConfig struct {
	ValidHTTPVersions []string `yaml:"valid_http_versions,omitempty"`
	ValidStatusCodes  []int    `yaml:"valid_status_codes,omitempty"`
	Method            string   `yaml:"method,omitempty"`
	FollowRedirects   bool     `yaml:"follow_redirects,omitempty"`
}

// ICMPProbeConfig represents an ICMP probe configuration for Blackbox Exporter.
type ICMPProbeConfig struct {
	PreferredIPProtocol string `yaml:"preferred_ip_protocol,omitempty"`
	PayloadSize         int    `yaml:"payload_size,omitempty"`
}

// BlackboxModule represents a single Blackbox Exporter probe module.
type BlackboxModule struct {
	Prober     string           `yaml:"prober"`
	HTTPConfig *HTTPProbeConfig `yaml:"http,omitempty"`
	ICMPConfig *ICMPProbeConfig `yaml:"icmp,omitempty"`
}

// BlackboxExporterConfig represents the Blackbox Exporter configuration.
type BlackboxExporterConfig struct {
	Modules map[string]BlackboxModule `yaml:"modules"`
}

// ComponentTemplate represents a reusable deployment template for a component.
type ComponentTemplate struct {
	Type          models.ComponentType `json:"type"`
	Name          string               `json:"name"`
	Version       string               `json:"version"`
	Image         string               `json:"image"`
	Replicas      int                  `json:"replicas"`
	Resources     ResourceQuota        `json:"resources"`
	HealthCheck   HealthCheckConfig    `json:"health_check"`
	BackupPolicy  BackupPolicyConfig   `json:"backup_policy"`
	UpgradePolicy UpgradePolicyConfig  `json:"upgrade_policy"`
}

// ExporterSpec represents a structured exporter configuration.
type ExporterSpec struct {
	Type           string                  `yaml:"type"`
	BlackboxConfig *BlackboxExporterConfig `yaml:"blackbox,omitempty"`
}

// ResourceQuota represents resource limits for a component.
type ResourceQuota struct {
	CPURequest    string `json:"cpu_request"`
	CPULimit      string `json:"cpu_limit"`
	MemoryRequest string `json:"memory_request"`
	MemoryLimit   string `json:"memory_limit"`
}

// HealthCheckConfig represents health check configuration.
type HealthCheckConfig struct {
	Enabled          bool   `json:"enabled"`
	Endpoint         string `json:"endpoint"`
	Interval         string `json:"interval"`
	Timeout          string `json:"timeout"`
	SuccessThreshold int    `json:"success_threshold"`
	FailureThreshold int    `json:"failure_threshold"`
}

// BackupPolicyConfig represents backup policy configuration.
type BackupPolicyConfig struct {
	Enabled      bool   `json:"enabled"`
	Schedule     string `json:"schedule"`
	Retention    string `json:"retention"`
	StorageType  string `json:"storage_type"`
	BackupConfig bool   `json:"backup_config"`
	BackupData   bool   `json:"backup_data"`
}

// UpgradePolicyConfig represents upgrade policy configuration.
type UpgradePolicyConfig struct {
	Strategy            string `json:"strategy"`
	AutoUpgrade         bool   `json:"auto_upgrade"`
	BackupBeforeUpgrade bool   `json:"backup_before_upgrade"`
	RollbackOnFailure   bool   `json:"rollback_on_failure"`
}

// ServiceDiscoveryConfig represents a service discovery configuration entry.
type ServiceDiscoveryConfig struct {
	Type               string              `yaml:"type"`
	KubernetesSDConfig *KubernetesSDConfig `yaml:"kubernetes_sd_configs,omitempty"`
	FileSDConfig       *FileSDConfig       `yaml:"file_sd_configs,omitempty"`
}

// KubernetesSDConfig represents Kubernetes service discovery configuration.
type KubernetesSDConfig struct {
	Roles      []string          `yaml:"roles"`
	Namespaces []NamespaceConfig `yaml:"namespaces,omitempty"`
}

// NamespaceConfig represents a namespace configuration for Kubernetes SD.
type NamespaceConfig struct {
	Name string `yaml:"name"`
}

func (ac *AutoConfig) FindPrometheusComponent(clusterID uint) (*models.Component, error) {
	clusterID, err := ac.resolveClusterScope(clusterID)
	if err != nil {
		return nil, err
	}
	var comp models.Component
	err = ac.db.Where("cluster_id = ? AND type = ? AND status = ?", clusterID, models.ComponentPrometheus, models.StatusRunning).
		Preload("Host").
		First(&comp).Error
	if err != nil {
		return nil, fmt.Errorf("未找到运行中的 Prometheus，请先启动组件后重试: %w", err)
	}
	return &comp, nil
}

// FindAlertmanagerComponent returns the running alertmanager component for a cluster.
func (ac *AutoConfig) FindAlertmanagerComponent(clusterID uint) (*models.Component, error) {
	clusterID, err := ac.resolveClusterScope(clusterID)
	if err != nil {
		return nil, err
	}
	var comp models.Component
	err = ac.db.Where("cluster_id = ? AND type = ? AND status = ?", clusterID, models.ComponentAlertmanager, models.StatusRunning).
		Preload("Host").
		First(&comp).Error
	if err != nil {
		return nil, fmt.Errorf("未找到运行中的 Alertmanager，请先启动组件后重试: %w", err)
	}
	return &comp, nil
}

// GetPrometheusEndpoint returns the HTTP endpoint for the prometheus instance in the given cluster.
func (ac *AutoConfig) GetPrometheusEndpoint(clusterID uint) (string, error) {
	comp, err := ac.FindPrometheusComponent(clusterID)
	if err != nil {
		return "", err
	}
	port := comp.Port
	if port == 0 {
		port = 9090
	}
	hostIP := comp.Host.IP
	return fmt.Sprintf("http://%s", net.JoinHostPort(hostIP, strconv.Itoa(port))), nil
}

// GetAlertmanagerEndpoint returns the HTTP endpoint for the alertmanager instance in the given cluster.
func (ac *AutoConfig) GetAlertmanagerEndpoint(clusterID uint) (string, error) {
	comp, err := ac.FindAlertmanagerComponent(clusterID)
	if err != nil {
		return "", err
	}
	port := comp.Port
	if port == 0 {
		port = 9093
	}
	hostIP := comp.Host.IP
	return fmt.Sprintf("http://%s", net.JoinHostPort(hostIP, strconv.Itoa(port))), nil
}

// ScrapeConfig represents a single scrape configuration entry.
type ScrapeConfig struct {
	JobName        string              `yaml:"job_name"`
	StaticConfigs  []StaticConfig      `yaml:"static_configs,omitempty"`
	FileSDConfigs  []FileSDConfig      `yaml:"file_sd_configs,omitempty"`
	HTTPSDConfigs  []HTTPSDConfig      `yaml:"http_sd_configs,omitempty"`
	MetricsPath    string              `yaml:"metrics_path,omitempty"`
	Interval       string              `yaml:"scrape_interval,omitempty"`
	ScrapeTimeout  string              `yaml:"scrape_timeout,omitempty"`
	Scheme         string              `yaml:"scheme,omitempty"`
	Params         map[string][]string `yaml:"params,omitempty"`
	RelabelConfigs []RelabelConfig     `yaml:"relabel_configs,omitempty"`
}

// StaticConfig represents static targets for a scrape job.
type StaticConfig struct {
	Targets []string          `yaml:"targets"`
	Labels  map[string]string `yaml:"labels,omitempty"`
}

// FileSDConfig represents file-based service discovery configuration.
type FileSDConfig struct {
	Files           []string `yaml:"files"`
	RefreshInterval string   `yaml:"refresh_interval,omitempty"`
}

// HTTPSDConfig represents HTTP-based service discovery configuration.
type HTTPSDConfig struct {
	URL             string     `yaml:"url"`
	RefreshInterval string     `yaml:"refresh_interval,omitempty"`
	BasicAuth       *BasicAuth `yaml:"basic_auth,omitempty"`
	BearerToken     string     `yaml:"bearer_token,omitempty"`
}

// RelabelConfig represents a relabeling rule.
type RelabelConfig struct {
	SourceLabels []string `yaml:"source_labels,omitempty"`
	Separator    string   `yaml:"separator,omitempty"`
	Regex        string   `yaml:"regex,omitempty"`
	Modulus      uint64   `yaml:"modulus,omitempty"`
	TargetLabel  string   `yaml:"target_label,omitempty"`
	Replacement  string   `yaml:"replacement,omitempty"`
	Action       string   `yaml:"action,omitempty"`
}

// BasicAuth represents basic authentication configuration.
type BasicAuth struct {
	Username string `yaml:"username"`
	Password string `yaml:"password,omitempty"`
}

// RemoteWriteConfig represents a Prometheus remote write configuration.
type RemoteWriteConfig struct {
	URL           string     `yaml:"url"`
	RemoteTimeout string     `yaml:"remote_timeout,omitempty"`
	BasicAuth     *BasicAuth `yaml:"basic_auth,omitempty"`
	BearerToken   string     `yaml:"bearer_token,omitempty"`
}

// PrometheusConfig represents the full Prometheus configuration structure.
type PrometheusConfig struct {
	Global       GlobalConfig        `yaml:"global"`
	Alerting     AlertingConfig      `yaml:"alerting,omitempty"`
	RuleFiles    []string            `yaml:"rule_files,omitempty"`
	ScrapeConfig []ScrapeConfig      `yaml:"scrape_configs"`
	RemoteWrite  []RemoteWriteConfig `yaml:"remote_write,omitempty"`
}

// GlobalConfig represents Prometheus global settings.
type GlobalConfig struct {
	ScrapeInterval     string            `yaml:"scrape_interval"`
	EvaluationInterval string            `yaml:"evaluation_interval"`
	ScrapeTimeout      string            `yaml:"scrape_timeout"`
	ExternalLabels     map[string]string `yaml:"external_labels,omitempty"`
}

// AlertingConfig represents Prometheus alerting settings.
type AlertingConfig struct {
	Alertmanagers []AlertmanagerTargetConfig `yaml:"alertmanagers"`
}

// AlertmanagerTargetConfig represents an alertmanager target configuration.
type AlertmanagerTargetConfig struct {
	StaticConfigs []StaticConfig `yaml:"static_configs"`
	PathPrefix    string         `yaml:"path_prefix,omitempty"`
}

// GenerateScrapeConfigs uses intended membership, never observed health.
func (ac *AutoConfig) GenerateScrapeConfigs(clusterID uint) ([]ScrapeConfig, error) {
	clusterID, err := ac.resolveClusterScope(clusterID)
	if err != nil {
		return nil, err
	}
	var components []models.Component
	if err := ac.db.Where("cluster_id = ? AND status NOT IN ?", clusterID, []models.ComponentStatus{models.StatusPending, models.StatusStopped, models.StatusStopping, "removed"}).
		Preload("Host").
		Find(&components).Error; err != nil {
		return nil, fmt.Errorf("读取组件配置失败，请检查数据库后重试: %w", err)
	}

	var scrapeConfigs []ScrapeConfig

	// Generate scrape configs from deployed components (existing behavior)
	for _, comp := range components {
		hostIP := comp.Host.IP
		port := comp.Port
		if port == 0 {
			switch comp.Type {
			case models.ComponentPrometheus:
				port = 9090
			case models.ComponentGrafana:
				port = 3000
			case models.ComponentAlertmanager:
				port = 9093
			case models.ComponentNodeExporter:
				port = 9100
			case models.ComponentWindowsExporter:
				port = 9182
			case models.ComponentPushgateway:
				port = 9091
			case models.ComponentBlackbox:
				port = 9115
			case models.ComponentLoki:
				port = 3100
			case models.ComponentPromtail:
				port = 9080
			default:
				port = 9100
			}
		}

		metricsPath := "/metrics"

		sc := ScrapeConfig{
			JobName:     string(comp.Type) + "_" + comp.Name,
			MetricsPath: metricsPath,
			StaticConfigs: []StaticConfig{
				{
					Targets: []string{net.JoinHostPort(hostIP, strconv.Itoa(port))},
					Labels: map[string]string{
						"component": string(comp.Type),
						"name":      comp.Name,
						"host":      comp.Host.Name,
					},
				},
			},
		}

		scrapeConfigs = append(scrapeConfigs, sc)
	}

	// Link Blackbox Exporter into Prometheus automatically. The exporter
	// itself is scraped above; this job probes the HTTP health endpoints of
	// the deployed monitoring stack through the production http_2xx module.
	var blackbox *models.Component
	probeTargets := make([]string, 0, len(components))
	for i := range components {
		comp := &components[i]
		if comp.Type == models.ComponentBlackbox {
			blackbox = comp
			continue
		}
		path := ""
		switch comp.Type {
		case models.ComponentPrometheus, models.ComponentAlertmanager, models.ComponentPushgateway:
			path = "/-/healthy"
		case models.ComponentGrafana:
			path = "/api/health"
		case models.ComponentLoki, models.ComponentPromtail:
			path = "/ready"
		}
		if path == "" {
			continue
		}
		port := comp.Port
		if port == 0 {
			switch comp.Type {
			case models.ComponentPrometheus:
				port = 9090
			case models.ComponentAlertmanager:
				port = 9093
			case models.ComponentPushgateway:
				port = 9091
			case models.ComponentGrafana:
				port = 3000
			case models.ComponentLoki:
				port = 3100
			case models.ComponentPromtail:
				port = 9080
			}
		}
		probeTargets = append(probeTargets, "http://"+net.JoinHostPort(comp.Host.IP, strconv.Itoa(port))+path)
	}
	if blackbox != nil && len(probeTargets) > 0 {
		port := blackbox.Port
		if port == 0 {
			port = 9115
		}
		scrapeConfigs = append(scrapeConfigs, ScrapeConfig{
			JobName:       "blackbox_http",
			MetricsPath:   "/probe",
			Interval:      "30s",
			ScrapeTimeout: "10s",
			Params:        map[string][]string{"module": {"http_2xx"}},
			StaticConfigs: []StaticConfig{{Targets: probeTargets, Labels: map[string]string{"probe_module": "http_2xx"}}},
			RelabelConfigs: []RelabelConfig{
				{SourceLabels: []string{"__address__"}, TargetLabel: "__param_target"},
				{SourceLabels: []string{"__param_target"}, TargetLabel: "instance"},
				{TargetLabel: "__address__", Replacement: net.JoinHostPort(blackbox.Host.IP, strconv.Itoa(port))},
			},
		})
	}

	// Add a self-scrape job for prometheus if it exists in this cluster
	for _, comp := range components {
		if comp.Type == models.ComponentPrometheus {
			found := false
			for _, sc := range scrapeConfigs {
				if sc.JobName == "prometheus_self" {
					found = true
					break
				}
			}
			if !found {
				port := 9090
				if comp.Port != 0 {
					port = comp.Port
				}
				scrapeConfigs = append(scrapeConfigs, ScrapeConfig{
					JobName:     "prometheus_self",
					MetricsPath: "/metrics",
					StaticConfigs: []StaticConfig{
						{
							Targets: []string{net.JoinHostPort("localhost", strconv.Itoa(port))},
							Labels:  map[string]string{"component": "prometheus"},
						},
					},
				})
			}
			break
		}
	}

	// Generate scrape configs from the ScrapeConfigModel table (custom user-defined scrape configs)
	var modelConfigs []models.ScrapeConfigModel
	if err := ac.db.Where("cluster_id = ? AND enabled = ?", clusterID, true).Order("id").Find(&modelConfigs).Error; err != nil {
		return nil, fmt.Errorf("read scrape configs: %w", err)
	}
	for _, mc := range modelConfigs {
		sc, err := ac.modelConfigToScrapeConfig(mc)
		if err != nil {
			return nil, fmt.Errorf("scrape config %d: %w", mc.ID, err)
		}
		scrapeConfigs = append(scrapeConfigs, sc)
	}

	return scrapeConfigs, nil
}

// modelConfigToScrapeConfig converts a ScrapeConfigModel database record into a Prometheus ScrapeConfig.
func (ac *AutoConfig) modelConfigToScrapeConfig(mc models.ScrapeConfigModel) (ScrapeConfig, error) {
	sc := ScrapeConfig{
		JobName:       mc.JobName,
		Interval:      mc.ScrapeInterval,
		ScrapeTimeout: mc.ScrapeTimeout,
	}

	// The create API stores RawMessage values verbatim, so a JSON-array that
	// arrived as a JSON string ("[\"1.2.3.4:9100\"]") is persisted with the
	// surrounding quotes. Normalize both forms (and legacy rows) by unwrapping
	// a JSON string before parsing as an array/map.
	unwrapJSONString := func(data []byte) []byte {
		var s string
		if json.Unmarshal(data, &s) == nil {
			return []byte(s)
		}
		return data
	}

	// Parse targets (JSON array of strings)
	if mc.Targets != "" {
		var targets []string
		targetsData := unwrapJSONString([]byte(mc.Targets))
		if err := json.Unmarshal(targetsData, &targets); err != nil {
			return sc, fmt.Errorf("invalid targets: %w", err)
		}
		if len(targets) > 0 {
			var labels map[string]string
			if mc.Labels != "" {
				if err := json.Unmarshal(unwrapJSONString([]byte(mc.Labels)), &labels); err != nil {
					return sc, fmt.Errorf("invalid labels: %w", err)
				}
			}
			sc.StaticConfigs = []StaticConfig{
				{Targets: targets, Labels: labels},
			}
		}
	}

	// Parse file_sd_configs (JSON array of {files, refresh_interval})
	if mc.FileSDConfigs != "" {
		var fileSD []FileSDConfig
		if err := json.Unmarshal(unwrapJSONString([]byte(mc.FileSDConfigs)), &fileSD); err != nil {
			return sc, fmt.Errorf("invalid file_sd_configs: %w", err)
		}
		if len(fileSD) > 0 {
			sc.FileSDConfigs = fileSD
		}
	}

	// Parse http_sd_configs (JSON array of {url, refresh_interval, basic_auth, bearer_token})
	if mc.HTTPSDConfigs != "" {
		var httpSD []HTTPSDConfig
		if err := json.Unmarshal(unwrapJSONString([]byte(mc.HTTPSDConfigs)), &httpSD); err != nil {
			return sc, fmt.Errorf("invalid http_sd_configs: %w", err)
		}
		if len(httpSD) > 0 {
			sc.HTTPSDConfigs = httpSD
		}
	}

	// Parse relabel_configs (JSON array of relabel rules)
	if mc.RelabelConfigs != "" {
		var relabels []RelabelConfig
		if err := json.Unmarshal(unwrapJSONString([]byte(mc.RelabelConfigs)), &relabels); err != nil {
			return sc, fmt.Errorf("invalid relabel_configs: %w", err)
		}
		if len(relabels) > 0 {
			sc.RelabelConfigs = relabels
		}
	}

	// Parse labels (JSON map)
	if mc.Labels != "" && len(sc.StaticConfigs) == 0 {
		var labels map[string]string
		if err := json.Unmarshal(unwrapJSONString([]byte(mc.Labels)), &labels); err != nil {
			return sc, fmt.Errorf("invalid labels: %w", err)
		}
		if len(labels) > 0 {
			// Create an empty static config just for labels if no targets
			sc.StaticConfigs = []StaticConfig{{Labels: labels}}
		}
	}

	return sc, nil
}

// SyncPrometheusConfig generates a full prometheus.yml from cluster components and writes it to the prometheus host.
func (ac *AutoConfig) SyncPrometheusConfig(clusterID uint, basePath string) error {
	clusterID, err := ac.resolveClusterScope(clusterID)
	if err != nil {
		return err
	}
	return WithMonitoringLock(func() error {
		err := ac.syncPrometheusConfigLocked(clusterID, basePath)
		return finishPublication(ac.db, clusterID, monitoringModels(), err)
	})
}

func (ac *AutoConfig) syncPrometheusConfigLocked(clusterID uint, basePath string) error {
	promComp, err := ac.FindPrometheusComponent(clusterID)
	if err != nil {
		log.Printf("[AutoConfig] Prometheus unavailable for cluster %d: %v", clusterID, err)
		return err
	}
	log.Printf("[AutoConfig] Syncing prometheus config for cluster %d using prometheus %s (host: %s)", clusterID, promComp.Name, promComp.Host.IP)

	// Respect manually saved configs: if the user saved a custom prometheus.yml via the
	// platform config manager, do not overwrite it on scheduled syncs.
	var manualCount int64
	if err := ac.db.Model(&models.ComponentConfig{}).Where("component_id = ? AND name = ? AND is_active = ?",
		promComp.ID, "prometheus.yml", true).Count(&manualCount).Error; err != nil {
		return err
	}
	if manualCount > 0 {
		return ErrManualPrometheusConfig
	}

	// Generate scrape configs
	scrapeConfigs, err := ac.GenerateScrapeConfigs(clusterID)
	if err != nil {
		log.Printf("[AutoConfig] Prometheus generation failed: %v", err)
		return fmt.Errorf("Prometheus 配置生成失败，请检查组件和抓取设置后重试")
	}

	// Build alertmanager target
	var alertmanagers []AlertmanagerTargetConfig
	if amComp, err := ac.FindAlertmanagerComponent(clusterID); err == nil {
		port := amComp.Port
		if port == 0 {
			port = 9093
		}
		alertmanagers = []AlertmanagerTargetConfig{
			{
				StaticConfigs: []StaticConfig{
					{Targets: []string{net.JoinHostPort(amComp.Host.IP, strconv.Itoa(port))}},
				},
				PathPrefix: models.ManagedRoutePrefix(amComp),
			},
		}
	} else {
		alertmanagers = []AlertmanagerTargetConfig{}
	}
	// Build rule files list: alert rules + recording rules
	ruleFiles := []string{"/etc/prometheus/rules/*.yml"}
	if promComp.DeployType == "binary" && promComp.ConfigPath != "" {
		ruleFiles = []string{filepath.Join(filepath.Dir(promComp.ConfigPath), "rules", "*.yml")}
	}

	// Build remote_write configs from the database
	var remoteWrites []RemoteWriteConfig
	var rwModels []models.RemoteWriteModel
	if err := ac.db.Where("cluster_id = ? AND enabled = ?", clusterID, true).Order("id").Find(&rwModels).Error; err != nil {
		return fmt.Errorf("read remote write configs: %w", err)
	} else {
		for _, rw := range rwModels {
			rwc := RemoteWriteConfig{
				URL:           rw.URL,
				RemoteTimeout: rw.RemoteTimeout,
			}
			if rw.BasicAuthUser != "" {
				rwc.BasicAuth = &BasicAuth{
					Username: rw.BasicAuthUser,
					Password: rw.BasicAuthPassword,
				}
			}
			if rw.BearerToken != "" {
				rwc.BearerToken = rw.BearerToken
			}
			remoteWrites = append(remoteWrites, rwc)
		}
	}

	scrapeInterval, evaluationInterval, scrapeTimeout := PrometheusGlobalIntervals(promComp.Config)

	// Build full config
	config := PrometheusConfig{
		Global: GlobalConfig{
			ScrapeInterval:     scrapeInterval,
			EvaluationInterval: evaluationInterval,
			ScrapeTimeout:      scrapeTimeout,
			ExternalLabels:     map[string]string{"node": "single-node"},
		},
		Alerting: AlertingConfig{
			Alertmanagers: alertmanagers,
		},
		RuleFiles:    ruleFiles,
		ScrapeConfig: scrapeConfigs,
		RemoteWrite:  remoteWrites,
	}

	// Marshal to YAML
	data, err := yaml.Marshal(config)
	if err != nil {
		return fmt.Errorf("生成 Prometheus YAML 失败，请修正配置后重试: %w", err)
	}

	var cluster models.Cluster
	if err := ac.db.First(&cluster, clusterID).Error; err != nil {
		return fmt.Errorf("节点配置不存在，请刷新页面后重试: %w", err)
	}
	configDir := filepath.Join(basePath, "configs", cluster.Name, promComp.Name)
	if err := os.MkdirAll(filepath.Join(configDir, "rules"), 0755); err != nil {
		return fmt.Errorf("创建规则目录失败，请检查目录权限: %w", err)
	}
	configPath := filepath.Join(configDir, "prometheus.yml")
	liveComp := *promComp
	if liveComp.DeployType == "binary" && liveComp.ConfigPath == "" {
		liveComp.ConfigPath = "/etc/prometheus/prometheus.yml"
	}
	if err := ac.publishMonitoringBundle(clusterID, &liveComp, configPath, data); err != nil {
		return fmt.Errorf("publish Prometheus snapshot: %w", err)
	}
	return nil
}

// AlertmanagerConfig represents the Alertmanager configuration structure.
type AlertmanagerConfig struct {
	Global       AlertmanagerGlobal `yaml:"global,omitempty"`
	Route        AlertmanagerRoute  `yaml:"route"`
	Receivers    []Receiver         `yaml:"receivers"`
	InhibitRules []InhibitRule      `yaml:"inhibit_rules,omitempty"`
	Templates    []string           `yaml:"templates,omitempty"`
}

// AlertmanagerGlobal represents global Alertmanager settings.
type AlertmanagerGlobal struct {
	ResolveTimeout   string `yaml:"resolve_timeout,omitempty"`
	SMTPFrom         string `yaml:"smtp_from,omitempty"`
	SMTPSmarthost    string `yaml:"smtp_smarthost,omitempty"`
	SMTPAuthUsername string `yaml:"smtp_auth_username,omitempty"`
	SMTPAuthPassword string `yaml:"smtp_auth_password,omitempty"`
	SMTPRequireTLS   *bool  `yaml:"smtp_require_tls,omitempty"`
	SlackAPIURL      string `yaml:"slack_api_url,omitempty"`
	WeChatAPIURL     string `yaml:"wechat_api_url,omitempty"`
	DingTalkAPIURL   string `yaml:"dingtalk_api_url,omitempty"`
}

// InhibitRule is persisted as JSON in AlertmanagerSettings and rendered to YAML.
type InhibitRule struct {
	SourceMatch   map[string]string `json:"source_match,omitempty" yaml:"source_match,omitempty"`
	SourceMatchRE map[string]string `json:"source_match_re,omitempty" yaml:"source_match_re,omitempty"`
	TargetMatch   map[string]string `json:"target_match,omitempty" yaml:"target_match,omitempty"`
	TargetMatchRE map[string]string `json:"target_match_re,omitempty" yaml:"target_match_re,omitempty"`
	Equal         []string          `json:"equal,omitempty" yaml:"equal,omitempty"`
}

// AlertmanagerRoute represents routing rules.
type AlertmanagerRoute struct {
	Receiver       string              `yaml:"receiver"`
	GroupBy        []string            `yaml:"group_by,omitempty"`
	GroupWait      string              `yaml:"group_wait,omitempty"`
	GroupInterval  string              `yaml:"group_interval,omitempty"`
	RepeatInterval string              `yaml:"repeat_interval,omitempty"`
	Routes         []AlertmanagerRoute `yaml:"routes,omitempty"`
	Match          map[string]string   `yaml:"match,omitempty"`
	MatchRE        map[string]string   `yaml:"match_re,omitempty"`
}

// PagerdutyConfig represents PagerDuty notification configuration.
type PagerdutyConfig struct {
	RoutingKey  string `yaml:"routing_key,omitempty"`
	Description string `yaml:"description,omitempty"`
	Client      string `yaml:"client,omitempty"`
}

// Receiver represents a notification receiver.
type Receiver struct {
	Name             string            `yaml:"name"`
	EmailConfigs     []EmailConfig     `yaml:"email_configs,omitempty"`
	SlackConfigs     []SlackConfig     `yaml:"slack_configs,omitempty"`
	WebhookConfigs   []WebhookConfig   `yaml:"webhook_configs,omitempty"`
	WeChatConfigs    []WeChatConfig    `yaml:"wechat_configs,omitempty"`
	PagerdutyConfigs []PagerdutyConfig `yaml:"pagerduty_configs,omitempty"`
}

// EmailConfig represents email notification configuration.
type EmailConfig struct {
	To           string            `yaml:"to,omitempty"`
	From         string            `yaml:"from,omitempty"`
	Smarthost    string            `yaml:"smarthost,omitempty"`
	AuthUsername string            `yaml:"auth_username,omitempty"`
	AuthPassword string            `yaml:"auth_password,omitempty"`
	Headers      map[string]string `yaml:"headers,omitempty"`
}

// SlackField represents a field in a Slack notification.
type SlackField struct {
	Title string `yaml:"title,omitempty"`
	Value string `yaml:"value,omitempty"`
	Short bool   `yaml:"short,omitempty"`
}

// SlackConfig represents Slack notification configuration.
type SlackConfig struct {
	APIURL    string       `yaml:"api_url,omitempty"`
	Channel   string       `yaml:"channel,omitempty"`
	Title     string       `yaml:"title,omitempty"`
	Text      string       `yaml:"text,omitempty"`
	Username  string       `yaml:"username,omitempty"`
	IconEmoji string       `yaml:"icon_emoji,omitempty"`
	Color     string       `yaml:"color,omitempty"`
	Fields    []SlackField `yaml:"fields,omitempty"`
}

// AuthConfig represents HTTP authorization configuration.
type AuthConfig struct {
	Type        string `yaml:"type,omitempty"`
	Credentials string `yaml:"credentials,omitempty"`
}

// WebhookHTTPConfig represents HTTP configuration for webhook notifications.
type WebhookHTTPConfig struct {
	Authorization *AuthConfig `yaml:"authorization,omitempty"`
}

// WebhookConfig represents webhook notification configuration.
type WebhookConfig struct {
	URL        string             `yaml:"url"`
	Headers    map[string]string  `yaml:"headers,omitempty"`
	HTTPConfig *WebhookHTTPConfig `yaml:"http_config,omitempty"`
}

// WeChatConfig represents WeChat notification configuration.
type WeChatConfig struct {
	APISecret string `yaml:"api_secret,omitempty"`
	CorpID    string `yaml:"corp_id,omitempty"`
	ToUser    string `yaml:"to_user,omitempty"`
	ToParty   string `yaml:"to_party,omitempty"`
	ToTag     string `yaml:"to_tag,omitempty"`
	AgentID   string `yaml:"agent_id,omitempty"`
	MsgType   string `yaml:"msg_type,omitempty"`
	Content   string `yaml:"content,omitempty"`
	Title     string `yaml:"title,omitempty"`
}
