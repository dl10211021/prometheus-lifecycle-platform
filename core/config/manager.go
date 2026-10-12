package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/promlifecycle/platform/models"

	"gopkg.in/yaml.v3"
)

// Manager handles configuration generation and management
type Manager struct {
	configDir string
}

type keyedConfigMutex struct {
	mu    sync.Mutex
	locks map[string]*configLock
}

type configLock struct {
	mu   sync.Mutex
	refs int
}

var publicationLocks = keyedConfigMutex{locks: make(map[string]*configLock)}

// WithConfigLock serializes every writer targeting the same node and file.
// The lock is process-wide so Manager, AutoConfig and alert-rule sync share it.
func WithConfigLock(clusterID uint, file string, fn func() error) error {
	_ = clusterID // retained in the API to make call-site scope explicit
	key := filepath.Clean(file)
	publicationLocks.mu.Lock()
	entry := publicationLocks.locks[key]
	if entry == nil {
		entry = &configLock{}
		publicationLocks.locks[key] = entry
	}
	entry.refs++
	publicationLocks.mu.Unlock()

	entry.mu.Lock()
	defer func() {
		entry.mu.Unlock()
		publicationLocks.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(publicationLocks.locks, key)
		}
		publicationLocks.mu.Unlock()
	}()
	return fn()
}

// ValidationResult distinguishes a semantic tool check from the YAML-only
// fallback used when promtool/amtool is not installed.
type ValidationResult struct {
	Level   string `json:"level"` // full or partial
	Tool    string `json:"tool,omitempty"`
	Message string `json:"message,omitempty"`
}

// ValidateConfigArtifact validates YAML and, for Prometheus/Alertmanager
// artifacts, invokes the native checker when it is available. A missing tool
// is explicitly partial rather than being reported as fully valid.
func ValidateConfigArtifact(kind, path string) (ValidationResult, error) {
	return validateConfigArtifact(kind, path, "")
}

// ValidateConfigArtifactWithBinary requires the native checker at binaryPath.
// Managed configuration changes must never fall back to YAML-only validation.
func ValidateConfigArtifactWithBinary(kind, path, binaryPath string) (ValidationResult, error) {
	if strings.TrimSpace(binaryPath) == "" {
		return ValidationResult{}, errors.New("native config validation tool is required")
	}
	return validateConfigArtifact(kind, path, binaryPath)
}

func validateConfigArtifact(kind, path, binaryPath string) (ValidationResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ValidationResult{}, err
	}
	if err := ValidateYAML(data); err != nil {
		return ValidationResult{}, fmt.Errorf("YAML 格式无效，请修正后重试: %w", err)
	}

	var tool string
	var args []string
	switch strings.ToLower(kind) {
	case "prometheus", "prometheus-config":
		tool, args = "promtool", []string{"check", "config", path}
	case "prometheus-rules":
		tool, args = "promtool", []string{"check", "rules", path}
	case "alertmanager":
		tool, args = "amtool", []string{"check-config", path}
	default:
		return ValidationResult{Level: "full", Message: "YAML 结构有效"}, nil
	}

	bin := binaryPath
	if bin == "" {
		bin, err = exec.LookPath(tool)
		if errors.Is(err, exec.ErrNotFound) {
			return ValidationResult{
				Level:   "partial",
				Tool:    tool,
				Message: "未找到 " + tool + "，仅完成 YAML 校验；请安装校验工具后再次发布",
			}, nil
		}
		if err != nil {
			return ValidationResult{}, fmt.Errorf("查找校验工具失败，请检查 PATH 后重试: %w", err)
		}
	}
	out, err := exec.Command(bin, args...).CombinedOutput()
	if err != nil {
		const maxOutput = 4096
		if len(out) > maxOutput {
			out = out[:maxOutput]
		}
		return ValidationResult{}, fmt.Errorf("%s 校验失败，请修正配置后重试: %s", tool, strings.TrimSpace(string(out)))
	}
	return ValidationResult{Level: "full", Tool: tool, Message: "已通过 " + tool + " 校验"}, nil
}

// ValidatePrometheusConfigWithBinary starts an isolated Prometheus process
// against the candidate configuration. A valid server remains running until
// the validation deadline; syntax or semantic failures exit immediately.
func ValidatePrometheusConfigWithBinary(path, binaryPath string) (ValidationResult, error) {
	dataDir, err := os.MkdirTemp("", "prometheus-config-validation-*")
	if err != nil {
		return ValidationResult{}, err
	}
	defer os.RemoveAll(dataDir)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	output, runErr := exec.CommandContext(
		ctx,
		binaryPath,
		"--config.file="+path,
		"--storage.tsdb.path="+dataDir,
		"--storage.tsdb.retention.time=1h",
		"--web.listen-address=127.0.0.1:0",
		"--log.level=error",
	).CombinedOutput()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return ValidationResult{Level: "full", Tool: filepath.Base(binaryPath), Message: "已通过隔离 Prometheus 启动校验"}, nil
	}
	const maxOutput = 4096
	if len(output) > maxOutput {
		output = output[:maxOutput]
	}
	if runErr != nil {
		return ValidationResult{}, fmt.Errorf("Prometheus 隔离启动校验失败，请修正配置后重试: %s", strings.TrimSpace(string(output)))
	}
	return ValidationResult{}, errors.New("Prometheus 校验进程意外提前退出")
}

// PublishValidatedLocal performs the common generate-to-temp, semantic check,
// atomic replace and reload sequence. If reload fails the previous bytes are
// restored before the error is returned.
func PublishValidatedLocal(clusterID uint, kind, path string, data []byte, mode os.FileMode, reload func() error) (ValidationResult, error) {
	var result ValidationResult
	err := WithConfigLock(clusterID, path, func() error {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return fmt.Errorf("创建配置目录失败，请检查目录权限: %w", err)
		}
		tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".validate-*")
		if err != nil {
			return fmt.Errorf("创建临时配置失败，请检查磁盘空间和目录权限: %w", err)
		}
		tmpPath := tmp.Name()
		defer os.Remove(tmpPath)
		if _, err = tmp.Write(data); err == nil {
			err = tmp.Chmod(mode)
		}
		if err == nil {
			err = tmp.Sync()
		}
		closeErr := tmp.Close()
		if err != nil {
			return fmt.Errorf("写入临时配置失败，请检查磁盘空间: %w", err)
		}
		if closeErr != nil {
			return fmt.Errorf("关闭临时配置失败，请重试: %w", closeErr)
		}

		result, err = ValidateConfigArtifact(kind, tmpPath)
		if err != nil {
			return err
		}

		old, readErr := os.ReadFile(path)
		hadOld := readErr == nil
		oldMode := mode
		if info, statErr := os.Stat(path); statErr == nil {
			oldMode = info.Mode().Perm()
		} else if !os.IsNotExist(statErr) {
			return fmt.Errorf("读取现有配置状态失败，请检查文件权限: %w", statErr)
		}
		if err := os.Rename(tmpPath, path); err != nil {
			return fmt.Errorf("原子替换配置失败，线上配置未变更: %w", err)
		}
		if reload == nil {
			return nil
		}
		if err := reload(); err != nil {
			if hadOld {
				if restoreErr := atomicReplaceBytes(path, old, oldMode); restoreErr != nil {
					return fmt.Errorf("重载失败且恢复旧配置失败，请立即检查服务: %v（恢复错误: %w）", err, restoreErr)
				}
			} else if removeErr := os.Remove(path); removeErr != nil && !os.IsNotExist(removeErr) {
				return fmt.Errorf("重载失败且清理新配置失败，请立即检查服务: %v（清理错误: %w）", err, removeErr)
			}
			return fmt.Errorf("重载失败，已恢复旧配置；请检查组件状态后重试: %w", err)
		}
		return nil
	})
	return result, err
}

func atomicReplaceBytes(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".restore-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Chmod(mode)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func NewManager(basePath string) *Manager {
	if basePath == "" {
		basePath = "/opt/promlifecycle/configs"
	}
	return &Manager{configDir: basePath}
}

func (m *Manager) Init() error {
	return os.MkdirAll(m.configDir, 0755)
}

func (m *Manager) GetComponentConfigDir(clusterName, componentName string) string {
	return filepath.Join(m.configDir, clusterName, componentName)
}

// SaveConfig takes only the per-file publication lock. Managed monitoring
// callers must hold WithMonitoringLock across this save, live publication and
// the ComponentConfig ownership write; acquiring it here would both leave that
// ownership transition unprotected and recursively lock managed change flows.
func (m *Manager) SaveConfig(clusterName, componentName, configName string, content []byte) (string, error) {
	if !safeSegment(clusterName) || !safeSegment(componentName) || !safeSegment(configName) {
		return "", fmt.Errorf("配置路径无效，请仅使用普通文件名")
	}
	dir := m.GetComponentConfigDir(clusterName, componentName)
	path := filepath.Join(dir, configName)
	kind := ""
	switch strings.ToLower(configName) {
	case "prometheus.yml", "prometheus.yaml":
		kind = "prometheus"
	case "alertmanager.yml", "alertmanager.yaml":
		kind = "alertmanager"
	}
	if _, err := PublishValidatedLocal(0, kind, path, content, 0644, nil); err != nil {
		return "", fmt.Errorf("配置校验或写入失败，请修正内容并检查目录权限后重试")
	}
	return path, nil
}

func (m *Manager) LoadConfig(clusterName, componentName, configName string) ([]byte, error) {
	if !safeSegment(clusterName) || !safeSegment(componentName) || !safeSegment(configName) {
		return nil, fmt.Errorf("unsafe config path segment")
	}
	path := filepath.Join(m.GetComponentConfigDir(clusterName, componentName), configName)
	return os.ReadFile(path)
}

// safeSegment reports whether s is safe to use as a single path segment.
func safeSegment(s string) bool {
	if s == "" || s == "." || s == ".." || len(s) > 128 {
		return false
	}
	return !strings.ContainsAny(s, "/\\\x00")
}

// GeneratePrometheusConfig generates Prometheus configuration YAML
func GeneratePrometheusConfig(cfg map[string]interface{}) ([]byte, error) {
	return yaml.Marshal(cfg)
}

// GenerateAlertmanagerConfig generates Alertmanager configuration YAML
func GenerateAlertmanagerConfig(cfg map[string]interface{}) ([]byte, error) {
	return yaml.Marshal(cfg)
}

// GenerateBlackboxConfig generates Blackbox Exporter configuration YAML
func GenerateBlackboxConfig(cfg map[string]interface{}) ([]byte, error) {
	return yaml.Marshal(cfg)
}

// GenerateLokiConfig generates Loki configuration YAML
func GenerateLokiConfig(cfg map[string]interface{}) ([]byte, error) {
	return yaml.Marshal(cfg)
}

// GeneratePromtailConfig generates Promtail configuration YAML
func GeneratePromtailConfig(cfg map[string]interface{}) ([]byte, error) {
	return yaml.Marshal(cfg)
}

// ValidateYAML validates if content is valid YAML
func ValidateYAML(content []byte) error {
	var result interface{}
	return yaml.Unmarshal(content, &result)
}

// ValidateJSON validates if content is valid JSON
func ValidateJSON(content []byte) error {
	var result interface{}
	return json.Unmarshal(content, &result)
}

// ToYAML converts a Go struct to YAML bytes
func ToYAML(v interface{}) ([]byte, error) {
	return yaml.Marshal(v)
}

// ToJSON converts a Go struct to JSON bytes
func ToJSON(v interface{}) ([]byte, error) {
	return json.MarshalIndent(v, "", "  ")
}

// DefaultPrometheusConfig returns a default Prometheus configuration
func DefaultPrometheusConfig() map[string]interface{} {
	return map[string]interface{}{
		"global": map[string]interface{}{
			"scrape_interval":     "15s",
			"evaluation_interval": "15s",
			"scrape_timeout":      "10s",
		},
		"alerting": map[string]interface{}{
			"alertmanagers": []map[string]interface{}{
				{
					"static_configs": []map[string]interface{}{
						{
							"targets": []string{"alertmanager:9093"},
						},
					},
				},
			},
		},
		"rule_files": []string{"/etc/prometheus/rules/*.yml"},
		"scrape_configs": []map[string]interface{}{
			{
				"job_name": "prometheus",
				"static_configs": []map[string]interface{}{
					{
						"targets": []string{"localhost:9090"},
					},
				},
			},
		},
	}
}

func PrometheusGlobalIntervals(componentConfig string) (scrapeInterval, evaluationInterval, scrapeTimeout string) {
	scrapeInterval, evaluationInterval, scrapeTimeout = "15s", "15s", "10s"
	parameters, err := models.DeploymentParametersFromConfig(componentConfig)
	if err != nil {
		return
	}
	if value, ok := parameters["scrape_interval"].(string); ok && value != "" {
		scrapeInterval = value
	}
	if value, ok := parameters["evaluation_interval"].(string); ok && value != "" {
		evaluationInterval = value
	}
	if value, ok := parameters["scrape_timeout"].(string); ok && value != "" {
		scrapeTimeout = value
	}
	return
}

func AlertmanagerConfigIntervals(componentConfig string) (resolveTimeout, groupWait, groupInterval, repeatInterval string) {
	resolveTimeout, groupWait, groupInterval, repeatInterval = "5m", "30s", "5m", "4h"
	parameters, err := models.DeploymentParametersFromConfig(componentConfig)
	if err != nil {
		return
	}
	values := map[string]*string{
		"resolve_timeout": &resolveTimeout,
		"group_wait":      &groupWait,
		"group_interval":  &groupInterval,
		"repeat_interval": &repeatInterval,
	}
	for field, destination := range values {
		if value, ok := parameters[field].(string); ok && value != "" {
			*destination = value
		}
	}
	return
}

// DefaultAlertmanagerConfig returns a default Alertmanager configuration
func DefaultAlertmanagerConfig() map[string]interface{} {
	return map[string]interface{}{
		"global": map[string]interface{}{
			"resolve_timeout": "5m",
		},
		"route": map[string]interface{}{
			"receiver":        "default",
			"group_by":        []string{"alertname", "cluster"},
			"group_wait":      "30s",
			"group_interval":  "5m",
			"repeat_interval": "4h",
		},
		"receivers": []map[string]interface{}{
			{
				"name": "default",
			},
		},
	}
}

// DefaultBlackboxConfig returns a default Blackbox Exporter configuration
func DefaultBlackboxConfig() map[string]interface{} {
	return map[string]interface{}{
		"modules": map[string]interface{}{
			"http_2xx": map[string]interface{}{
				"prober": "http",
				"http": map[string]interface{}{
					"valid_http_versions": []string{"HTTP/1.1", "HTTP/2.0"},
					"valid_status_codes":  []int{200, 301, 302},
				},
			},
			"http_post_2xx": map[string]interface{}{
				"prober": "http",
				"http": map[string]interface{}{
					"method":              "POST",
					"valid_http_versions": []string{"HTTP/1.1", "HTTP/2.0"},
					"valid_status_codes":  []int{200, 302},
				},
			},
			"tcp_connect": map[string]interface{}{
				"prober": "tcp",
			},
			"icmp": map[string]interface{}{
				"prober": "icmp",
			},
			"dns": map[string]interface{}{
				"prober": "dns",
				"dns": map[string]interface{}{
					"query_name": "example.com",
					"query_type": "A",
				},
			},
		},
	}
}

func BlackboxDeploymentConfig(componentConfig string) map[string]interface{} {
	parameters, _ := models.DeploymentParametersFromConfig(componentConfig)
	stringValue := func(field, fallback string) string {
		if value, ok := parameters[field].(string); ok && value != "" {
			return value
		}
		return fallback
	}
	boolValue := func(field string, fallback bool) bool {
		if value, ok := parameters[field].(bool); ok {
			return value
		}
		return fallback
	}
	intValue := func(field string, fallback int) int {
		if value, ok := parameters[field].(float64); ok {
			return int(value)
		}
		if value, ok := parameters[field].(int); ok {
			return value
		}
		return fallback
	}
	stringSlice := func(field string, fallback []string) []string {
		switch values := parameters[field].(type) {
		case []string:
			return values
		case []interface{}:
			result := make([]string, 0, len(values))
			for _, value := range values {
				if text, ok := value.(string); ok {
					result = append(result, text)
				}
			}
			if len(result) > 0 {
				return result
			}
		}
		return fallback
	}
	statusCodes := make([]int, 0)
	for _, status := range stringSlice("http_valid_status_codes", []string{"200", "301", "302"}) {
		if value, err := strconv.Atoi(status); err == nil {
			statusCodes = append(statusCodes, value)
		}
	}
	timeout := stringValue("probe_timeout", "5s")
	ipProtocol := stringValue("preferred_ip_protocol", "ip4")
	httpConfig := func(method string) map[string]interface{} {
		return map[string]interface{}{
			"valid_http_versions":   stringSlice("http_valid_versions", []string{"HTTP/1.1", "HTTP/2.0"}),
			"valid_status_codes":    statusCodes,
			"method":                method,
			"follow_redirects":      boolValue("http_follow_redirects", true),
			"preferred_ip_protocol": ipProtocol,
			"fail_if_ssl":           boolValue("http_fail_if_ssl", false),
			"fail_if_not_ssl":       boolValue("http_fail_if_not_ssl", false),
		}
	}
	return map[string]interface{}{
		"modules": map[string]interface{}{
			"http_2xx": map[string]interface{}{
				"prober": "http", "timeout": timeout,
				"http": httpConfig(stringValue("http_method", "GET")),
			},
			"http_post_2xx": map[string]interface{}{
				"prober": "http", "timeout": timeout, "http": httpConfig("POST"),
			},
			"tcp_connect": map[string]interface{}{
				"prober": "tcp", "timeout": timeout,
				"tcp": map[string]interface{}{"preferred_ip_protocol": ipProtocol},
			},
			"icmp": map[string]interface{}{
				"prober": "icmp", "timeout": timeout,
				"icmp": map[string]interface{}{"preferred_ip_protocol": ipProtocol, "payload_size": intValue("icmp_payload_size", 32)},
			},
			"dns": map[string]interface{}{
				"prober": "dns", "timeout": timeout,
				"dns": map[string]interface{}{
					"query_name":            stringValue("dns_query_name", "example.com"),
					"query_type":            stringValue("dns_query_type", "A"),
					"valid_rcodes":          stringSlice("dns_valid_rcodes", []string{"NOERROR"}),
					"preferred_ip_protocol": ipProtocol,
				},
			},
		},
	}
}
func LokiDeploymentConfig(dataDir string, port int, componentConfig string) map[string]interface{} {
	if dataDir == "" {
		dataDir = "/var/lib/promlifecycle/loki"
	}
	if port == 0 {
		port = 3100
	}
	parameters, _ := models.DeploymentParametersFromConfig(componentConfig)
	stringValue := func(field, fallback string) string {
		if value, ok := parameters[field].(string); ok && value != "" {
			return value
		}
		return fallback
	}
	boolValue := func(field string, fallback bool) bool {
		if value, ok := parameters[field].(bool); ok {
			return value
		}
		return fallback
	}
	intValue := func(field string, fallback int) int {
		if value, ok := parameters[field].(float64); ok {
			return int(value)
		}
		if value, ok := parameters[field].(int); ok {
			return value
		}
		return fallback
	}
	return map[string]interface{}{
		"auth_enabled": false,
		"server": map[string]interface{}{
			"http_listen_port": port, "grpc_listen_port": 9096,
			"log_level": stringValue("log_level", "info"),
		},
		"common": map[string]interface{}{
			"instance_addr": "127.0.0.1", "path_prefix": dataDir,
			"storage": map[string]interface{}{
				"filesystem": map[string]interface{}{
					"chunks_directory": filepath.Join(dataDir, "chunks"),
					"rules_directory":  filepath.Join(dataDir, "rules"),
				},
			},
			"replication_factor": 1,
			"ring":               map[string]interface{}{"kvstore": map[string]interface{}{"store": "inmemory"}},
		},
		"query_range": map[string]interface{}{
			"results_cache": map[string]interface{}{
				"cache": map[string]interface{}{
					"embedded_cache": map[string]interface{}{
						"enabled": true, "max_size_mb": intValue("results_cache_max_size_mb", 100),
					},
				},
			},
		},
		"schema_config": map[string]interface{}{
			"configs": []map[string]interface{}{{
				"from": "2023-01-01", "store": "tsdb", "object_store": "filesystem", "schema": "v13",
				"index": map[string]interface{}{"prefix": "index_", "period": "24h"},
			}},
		},
		"storage_config": map[string]interface{}{
			"tsdb_shipper": map[string]interface{}{
				"active_index_directory": filepath.Join(dataDir, "tsdb-index"),
				"cache_location":         filepath.Join(dataDir, "tsdb-cache"),
				"cache_ttl":              "24h",
			},
		},
		"compactor": map[string]interface{}{
			"working_directory":             filepath.Join(dataDir, "compactor"),
			"compaction_interval":           stringValue("compactor_interval", "10m"),
			"retention_enabled":             true,
			"retention_delete_delay":        stringValue("retention_delete_delay", "2h"),
			"retention_delete_worker_count": intValue("retention_delete_worker_count", 20),
			"delete_request_store":          "filesystem",
		},
		"limits_config": map[string]interface{}{
			"retention_period":            stringValue("retention_period", "720h"),
			"max_query_length":            stringValue("max_query_length", "720h"),
			"reject_old_samples":          boolValue("reject_old_samples", true),
			"reject_old_samples_max_age":  stringValue("reject_old_samples_max_age", "168h"),
			"ingestion_rate_mb":           intValue("ingestion_rate_mb", 16),
			"ingestion_burst_size_mb":     intValue("ingestion_burst_size_mb", 24),
			"query_timeout":               stringValue("query_timeout", "1m"),
			"max_query_parallelism":       intValue("max_query_parallelism", 32),
			"per_stream_rate_limit":       stringValue("per_stream_rate_limit", "3MB"),
			"per_stream_rate_limit_burst": stringValue("per_stream_rate_limit_burst", "15MB"),
		},
		"ruler": map[string]interface{}{
			"alertmanager_url": stringValue("alertmanager_url", "http://127.0.0.1:9093"),
		},
		"frontend":  map[string]interface{}{"encoding": "protobuf"},
		"analytics": map[string]interface{}{"reporting_enabled": boolValue("analytics_reporting_enabled", false)},
	}
}

// DefaultLokiConfig returns a default Loki configuration
func DefaultLokiConfig() map[string]interface{} {
	return map[string]interface{}{
		"auth_enabled": false,
		"server": map[string]interface{}{
			"http_listen_port": 3100,
		},
		"common": map[string]interface{}{
			"instance_addr": "127.0.0.1",
			"path_prefix":   "/loki",
			"storage": map[string]interface{}{
				"type": "filesystem",
			},
			"replication_factor": 1,
		},
		"schema_config": map[string]interface{}{
			"configs": []map[string]interface{}{
				{
					"from":   "",
					"store":  "tsdb",
					"schema": "v13",
					"index": map[string]interface{}{
						"prefix": "index_",
						"period": "24h",
					},
				},
			},
		},
		"ruler": map[string]interface{}{
			"alertmanager_url": "http://alertmanager:9093",
		},
	}
}
func PromtailDeploymentConfig(dataDir string, port int, componentConfig string) map[string]interface{} {
	if dataDir == "" {
		dataDir = "/var/lib/promlifecycle/promtail"
	}
	if port == 0 {
		port = 9080
	}
	parameters, _ := models.DeploymentParametersFromConfig(componentConfig)
	stringValue := func(field, fallback string) string {
		if value, ok := parameters[field].(string); ok && value != "" {
			return value
		}
		return fallback
	}
	boolValue := func(field string, fallback bool) bool {
		if value, ok := parameters[field].(bool); ok {
			return value
		}
		return fallback
	}
	intValue := func(field string, fallback int) int {
		if value, ok := parameters[field].(float64); ok {
			return int(value)
		}
		if value, ok := parameters[field].(int); ok {
			return value
		}
		return fallback
	}
	client := map[string]interface{}{
		"url":                       stringValue("client_url", "http://127.0.0.1:3100/loki/api/v1/push"),
		"batchwait":                 stringValue("batch_wait", "1s"),
		"batchsize":                 intValue("batch_size_bytes", 1048576),
		"timeout":                   stringValue("client_timeout", "10s"),
		"drop_rate_limited_batches": boolValue("drop_rate_limited_batches", false),
		"backoff_config": map[string]interface{}{
			"min_period":  stringValue("backoff_min_period", "500ms"),
			"max_period":  stringValue("backoff_max_period", "5m"),
			"max_retries": intValue("backoff_max_retries", 10),
		},
	}
	if tenant := stringValue("tenant_id", ""); tenant != "" {
		client["tenant_id"] = tenant
	}
	scrapeConfigs := make([]map[string]interface{}, 0, 2)
	if boolValue("file_log_enabled", true) {
		scrapeConfigs = append(scrapeConfigs, map[string]interface{}{
			"job_name": stringValue("file_job_name", "varlogs"),
			"static_configs": []map[string]interface{}{{
				"targets": []string{"localhost"},
				"labels": map[string]interface{}{
					"job": stringValue("file_job_name", "varlogs"), "__path__": stringValue("file_glob", "/var/log/*log"),
				},
			}},
		})
	}
	if boolValue("journal_enabled", true) {
		scrapeConfigs = append(scrapeConfigs, map[string]interface{}{
			"job_name": stringValue("journal_job_name", "systemd-journal"),
			"journal": map[string]interface{}{
				"max_age": stringValue("journal_max_age", "12h"),
				"path":    stringValue("journal_path", "/var/log/journal"),
				"labels":  map[string]string{"job": stringValue("journal_job_name", "systemd-journal")},
			},
			"relabel_configs": []map[string]interface{}{
				{"source_labels": []string{"__journal__systemd_unit"}, "target_label": "unit"},
				{"source_labels": []string{"__journal__hostname"}, "target_label": "hostname"},
			},
		})
	}
	return map[string]interface{}{
		"server": map[string]interface{}{
			"http_listen_port": port, "grpc_listen_port": 0,
			"log_level": stringValue("log_level", "info"),
		},
		"positions": map[string]interface{}{
			"filename":            filepath.Join(dataDir, "positions.yaml"),
			"sync_period":         stringValue("positions_sync_period", "10s"),
			"ignore_invalid_yaml": boolValue("positions_ignore_invalid_yaml", false),
		},
		"clients":        []map[string]interface{}{client},
		"scrape_configs": scrapeConfigs,
	}
}

// DefaultPromtailConfig returns a default Promtail configuration
func DefaultPromtailConfig() map[string]interface{} {
	return map[string]interface{}{
		"server": map[string]interface{}{
			"http_listen_port": 9080,
		},
		"positions": map[string]interface{}{
			"filename": "/tmp/positions.yaml",
		},
		"clients": []map[string]interface{}{
			{
				"url": "http://loki:3100/loki/api/v1/push",
			},
		},
		"scrape_configs": []map[string]interface{}{
			{
				"job_name": "system",
				"static_configs": []map[string]interface{}{
					{
						"targets": []string{"localhost"},
						"labels": map[string]interface{}{
							"job":      "varlogs",
							"__path__": "/var/log/*log",
						},
					},
				},
			},
		},
	}
}
