package handlers

import (
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/promlifecycle/platform/models"
)

type componentCreateRequest struct {
	Type                 string                 `json:"type" binding:"required"`
	Name                 string                 `json:"name" binding:"required"`
	HostID               *uint                  `json:"host_id"`
	Version              string                 `json:"version"`
	Port                 int                    `json:"port"`
	DeployType           string                 `json:"deploy_type"`
	Config               map[string]interface{} `json:"config"`
	Parameters           map[string]interface{} `json:"parameters"`
	ResourceLimits       string                 `json:"resource_limits"`
	CustomEnv            string                 `json:"custom_env"`
	ExtraVolumes         string                 `json:"extra_volumes"`
	DataPathOverride     string                 `json:"data_path_override"`
	PreflightToken       string                 `json:"preflight_token"`
	InstallPath          string                 `json:"install_path"`
	CustomArgs           string                 `json:"custom_args"`
	PlannedComponents    []string               `json:"planned_components,omitempty"`
	PlannedVersions      map[string]string      `json:"planned_versions,omitempty"`
	RequiredComponents   []string               `json:"required_components,omitempty"`
	InitialPassword      string                 `json:"initial_password"`
	ReplaceExisting      bool                   `json:"replace_existing"`
	StructuredParameters map[string]interface{} `json:"-"`
}

func (h *Handler) normalizeDeploymentRequest(request *componentCreateRequest, custom *models.ComponentDefinition) (models.DeploymentDefinition, error) {
	if request.StructuredParameters == nil {
		request.StructuredParameters = make(map[string]interface{})
	}
	definition, err := deploymentDefinitionForRequest(request.Type, custom)
	if err != nil {
		return models.DeploymentDefinition{}, err
	}
	if request.DeployType == "" {
		request.DeployType = definition.DeployType
	}
	if request.DeployType == "target" {
		if request.Type != string(models.ComponentNodeExporter) && request.Type != string(models.ComponentBlackbox) {
			return definition, fmt.Errorf("target 模式仅支持 node_exporter 或 blackbox_exporter")
		}
		if request.Port < 1 || request.Port > 65535 {
			return definition, fmt.Errorf("target 模式端口必须在 1 到 65535 之间")
		}
		return definition, nil
	}
	if request.DeployType != definition.DeployType {
		return definition, fmt.Errorf("组件 %s 必须使用 %s 部署方式", request.Type, definition.DeployType)
	}

	allowed := make(map[string]models.DeploymentParameter, len(definition.Parameters))
	for _, parameter := range definition.Parameters {
		allowed[parameter.Field] = parameter
	}
	for field, value := range request.Parameters {
		parameter, exists := allowed[field]
		if !exists {
			return definition, fmt.Errorf("部署参数 %q 不属于组件 %s 的定义", field, request.Type)
		}
		if err := applyDeploymentParameter(request, parameter, value); err != nil {
			return definition, err
		}
	}
	for _, parameter := range definition.Parameters {
		value, exists := deploymentRequestValue(request, parameter.Field)
		if (!exists || deploymentValueEmpty(value)) && parameter.Default != nil {
			if err := applyDeploymentParameter(request, parameter, parameter.Default); err != nil {
				return definition, err
			}
			value, exists = deploymentRequestValue(request, parameter.Field)
		}
		conditionallyOptional := request.Type == string(models.ComponentGrafana) &&
			request.ReplaceExisting && parameter.Field == "initial_password"
		if parameter.Required && !conditionallyOptional && (!exists || deploymentValueEmpty(value)) {
			return definition, fmt.Errorf("部署参数 %q（%s）不能为空", parameter.Field, parameter.Label)
		}
		if exists {
			if err := validateDeploymentParameterValue(parameter, value); err != nil {
				return definition, err
			}
		}
	}
	if request.Type == string(models.ComponentGrafana) && request.InitialPassword != "" {
		if err := applyGrafanaInitialPassword(request); err != nil {
			return definition, err
		}
	}
	delete(request.StructuredParameters, "initial_password")
	delete(request.StructuredParameters, "custom_env")
	for _, parameter := range definition.Parameters {
		if parameter.Type == "password" || parameter.Field == "custom_env" {
			continue
		}
		if value, exists := deploymentRequestValue(request, parameter.Field); exists {
			request.StructuredParameters[parameter.Field] = value
		}
	}
	if request.Type == string(models.ComponentPrometheus) {
		if err := validatePrometheusDeploymentSemantics(request.StructuredParameters); err != nil {
			return definition, err
		}
		if err := models.ValidatePrometheusArgumentVersion(request.Version, nil); err != nil {
			return definition, err
		}
		// The retained instance is resolved later under its host/component
		// claim. Its imported argv replaces platform defaults, so do not
		// validate a fictitious default argv for a re-deploy here.
		if !request.ReplaceExisting {
			if err := validatePrometheusDeploymentParameterVersion(request, nil); err != nil {
				return definition, err
			}
		}
	}
	if request.Type == string(models.ComponentAlertmanager) {
		if err := validateAlertmanagerDeploymentSemantics(request.StructuredParameters); err != nil {
			return definition, err
		}
	}
	if request.Type == string(models.ComponentGrafana) {
		if err := validateGrafanaDeploymentSemantics(request.StructuredParameters); err != nil {
			return definition, err
		}
	}
	if request.Type == string(models.ComponentNodeExporter) {
		if err := validateNodeExporterDeploymentSemantics(request.StructuredParameters); err != nil {
			return definition, err
		}
	}
	if request.Type == string(models.ComponentBlackbox) {
		if err := validateBlackboxDeploymentSemantics(request.StructuredParameters); err != nil {
			return definition, err
		}
	}
	if request.Type == string(models.ComponentPushgateway) {
		if err := validatePushgatewayDeploymentSemantics(request.StructuredParameters); err != nil {
			return definition, err
		}
	}
	if request.Type == string(models.ComponentLoki) {
		if err := validateLokiDeploymentSemantics(request.StructuredParameters); err != nil {
			return definition, err
		}
	}
	if request.Type == string(models.ComponentPromtail) {
		if err := validatePromtailDeploymentSemantics(request.StructuredParameters); err != nil {
			return definition, err
		}
	}
	if request.Type == string(models.ComponentThanos) {
		if err := validateThanosDeploymentSemantics(request.StructuredParameters); err != nil {
			return definition, err
		}
	}
	return definition, nil
}

func deploymentDefinitionForRequest(componentType string, custom *models.ComponentDefinition) (models.DeploymentDefinition, error) {
	for _, definition := range builtInDeploymentDefinitions() {
		if definition.Type == componentType {
			return definition, nil
		}
	}
	if custom != nil && custom.Type == componentType {
		return models.DeploymentDefinition{
			Type: custom.Type, DisplayName: custom.DisplayName,
			Platform: "linux", DeployType: "binary", ServiceManager: "systemd",
			HealthPath: custom.HealthPath, BuiltIn: false,
			Parameters: customDefinitionParameters(*custom),
		}, nil
	}
	return models.DeploymentDefinition{}, fmt.Errorf("组件 %s 没有可用部署定义", componentType)
}

func applyDeploymentParameter(request *componentCreateRequest, parameter models.DeploymentParameter, value interface{}) error {
	if request.StructuredParameters == nil {
		request.StructuredParameters = make(map[string]interface{})
	}
	switch parameter.Type {
	case "number":
		number, ok := deploymentInteger(value)
		if !ok {
			return fmt.Errorf("部署参数 %q 必须是整数", parameter.Field)
		}
		if parameter.Min > 0 && number < parameter.Min || parameter.Max > 0 && number > parameter.Max {
			return fmt.Errorf("部署参数 %q 必须在 %d 到 %d 之间", parameter.Field, parameter.Min, parameter.Max)
		}
		request.StructuredParameters[parameter.Field] = number
		if parameter.Field == "port" {
			request.Port = number
		}
	case "multiselect":
		values, ok := deploymentStrings(value)
		if !ok {
			return fmt.Errorf("部署参数 %q 必须是字符串数组", parameter.Field)
		}
		if len(parameter.Options) > 0 {
			allowed := make(map[string]bool, len(parameter.Options))
			for _, option := range parameter.Options {
				allowed[option] = true
			}
			for _, item := range values {
				if !allowed[item] {
					return fmt.Errorf("部署参数 %q 包含不支持的选项 %q", parameter.Field, item)
				}
			}
		}
		request.StructuredParameters[parameter.Field] = values
		if parameter.Field == "collectors" {
			if request.Config == nil {
				request.Config = make(map[string]interface{})
			}
			request.Config["collectors"] = values
		}
	case "boolean":
		boolean, ok := value.(bool)
		if !ok {
			return fmt.Errorf("部署参数 %q 必须是布尔值", parameter.Field)
		}
		request.StructuredParameters[parameter.Field] = boolean
	default:
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("部署参数 %q 必须是字符串", parameter.Field)
		}
		request.StructuredParameters[parameter.Field] = text
		switch parameter.Field {
		case "version":
			request.Version = text
		case "install_path":
			request.InstallPath = text
		case "data_path_override":
			request.DataPathOverride = text
		case "custom_args":
			request.CustomArgs = text
		case "custom_env":
			request.CustomEnv = text
		case "resource_limits":
			request.ResourceLimits = text
		case "initial_password":
			request.InitialPassword = text
		}
	}
	return nil
}

func validateDeploymentHost(definition models.DeploymentDefinition, request *componentCreateRequest, host *models.Host) error {
	if request == nil || host == nil || request.DeployType == "target" {
		return nil
	}
	if definition.Platform == "windows" && (host.ConnectionType != "winrm" || host.OS != "windows") {
		return fmt.Errorf("部署平台不匹配：该组件需要 Windows WinRM 主机")
	}
	if definition.Platform == "linux" && host.ConnectionType == "winrm" {
		return fmt.Errorf("部署平台不匹配：该组件需要 Linux 本机或 SSH 主机")
	}
	return nil
}

func deploymentRequestValue(request *componentCreateRequest, field string) (interface{}, bool) {
	switch field {
	case "version":
		return request.Version, request.Version != ""
	case "port":
		return request.Port, request.Port != 0
	case "install_path":
		return request.InstallPath, request.InstallPath != ""
	case "data_path_override":
		return request.DataPathOverride, request.DataPathOverride != ""
	case "custom_args":
		return request.CustomArgs, request.CustomArgs != ""
	case "custom_env":
		return request.CustomEnv, request.CustomEnv != ""
	case "resource_limits":
		return request.ResourceLimits, request.ResourceLimits != ""
	case "initial_password":
		if request.InitialPassword != "" {
			return request.InitialPassword, true
		}
		var environment map[string]string
		if json.Unmarshal([]byte(request.CustomEnv), &environment) == nil && environment["GF_SECURITY_ADMIN_PASSWORD"] != "" {
			return environment["GF_SECURITY_ADMIN_PASSWORD"], true
		}
		return "", false
	case "collectors":
		if request.Config == nil {
			return nil, false
		}
		value, exists := request.Config["collectors"]
		return value, exists
	default:
		value, exists := request.StructuredParameters[field]
		return value, exists
	}
}

func validateDeploymentParameterValue(parameter models.DeploymentParameter, value interface{}) error {
	switch parameter.Type {
	case "number":
		number, ok := deploymentInteger(value)
		if !ok {
			return fmt.Errorf("部署参数 %q 必须是整数", parameter.Field)
		}
		if parameter.Min > 0 && number < parameter.Min || parameter.Max > 0 && number > parameter.Max {
			return fmt.Errorf("部署参数 %q 必须在 %d 到 %d 之间", parameter.Field, parameter.Min, parameter.Max)
		}
	case "multiselect":
		values, ok := deploymentStrings(value)
		if !ok {
			return fmt.Errorf("部署参数 %q 必须是字符串数组", parameter.Field)
		}
		if parameter.Required && len(values) == 0 {
			return fmt.Errorf("部署参数 %q 至少选择一项", parameter.Field)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("部署参数 %q 必须是布尔值", parameter.Field)
		}
	case "select":
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("部署参数 %q 必须是字符串选项", parameter.Field)
		}
		if len(parameter.Options) > 0 {
			allowed := false
			for _, option := range parameter.Options {
				if text == option {
					allowed = true
					break
				}
			}
			if !allowed {
				return fmt.Errorf("部署参数 %q 不支持选项 %q", parameter.Field, text)
			}
		}
	case "duration":
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("部署参数 %q 必须是时长字符串", parameter.Field)
		}
		if _, err := parsePrometheusDuration(text); err != nil {
			return fmt.Errorf("部署参数 %q 无效: %w", parameter.Field, err)
		}
	case "size":
		text, ok := value.(string)
		if !ok || text != "" && !prometheusSizePattern.MatchString(text) {
			return fmt.Errorf("部署参数 %q 必须是容量值，例如 50GB 或 10GiB", parameter.Field)
		}
	case "url":
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("部署参数 %q 必须是 URL", parameter.Field)
		}
		if text != "" {
			parsed, err := url.ParseRequestURI(text)
			if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Scheme != "http" && parsed.Scheme != "https" {
				return fmt.Errorf("部署参数 %q 必须是无凭据的 HTTP/HTTPS URL", parameter.Field)
			}
		}
	case "regex":
		text, ok := value.(string)
		if !ok || len(text) > 1024 || strings.ContainsAny(text, "\r\n") {
			return fmt.Errorf("部署参数 %q 必须是单行且不超过 1024 字节的正则表达式", parameter.Field)
		}
		if text != "" {
			if _, err := regexp.Compile(text); err != nil {
				return fmt.Errorf("部署参数 %q 不是有效正则表达式: %w", parameter.Field, err)
			}
		}
	default:
		if _, ok := value.(string); !ok {
			return fmt.Errorf("部署参数 %q 必须是字符串", parameter.Field)
		}
	}
	return nil
}

func deploymentInteger(value interface{}) (int, bool) {
	switch number := value.(type) {
	case int:
		return number, true
	case float64:
		integer := int(number)
		return integer, number == float64(integer)
	case json.Number:
		integer, err := number.Int64()
		return int(integer), err == nil
	default:
		return 0, false
	}
}

func deploymentStrings(value interface{}) ([]string, bool) {
	switch values := value.(type) {
	case []string:
		return values, true
	case []interface{}:
		result := make([]string, 0, len(values))
		for _, value := range values {
			text, ok := value.(string)
			if !ok || strings.TrimSpace(text) == "" {
				return nil, false
			}
			result = append(result, text)
		}
		return result, true
	default:
		return nil, false
	}
}

func deploymentValueEmpty(value interface{}) bool {
	switch item := value.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(item) == ""
	case []string:
		return len(item) == 0
	case []interface{}:
		return len(item) == 0
	default:
		return false
	}
}

var prometheusDurationPattern = regexp.MustCompile(`([0-9]+)(ms|s|m|h|d|w|y)`)
var prometheusSizePattern = regexp.MustCompile(`^[1-9][0-9]*(B|KB|MB|GB|TB|PB|EB|KiB|MiB|GiB|TiB|PiB|EiB)$`)

func parsePrometheusDuration(value string) (int64, error) {
	matches := prometheusDurationPattern.FindAllStringSubmatch(value, -1)
	consumed := 0
	total := int64(0)
	multipliers := map[string]int64{
		"ms": 1, "s": 1000, "m": 60 * 1000, "h": 60 * 60 * 1000,
		"d": 24 * 60 * 60 * 1000, "w": 7 * 24 * 60 * 60 * 1000,
		"y": 365 * 24 * 60 * 60 * 1000,
	}
	for _, match := range matches {
		consumed += len(match[0])
		number, err := strconv.ParseInt(match[1], 10, 64)
		multiplier := multipliers[match[2]]
		if err != nil || number > math.MaxInt64/multiplier || total > math.MaxInt64-number*multiplier {
			return 0, fmt.Errorf("时长超出范围")
		}
		total += number * multiplier
	}
	if consumed != len(value) || total <= 0 {
		return 0, fmt.Errorf("必须是正数时长，例如 15s、2m 或 30d")
	}
	return total, nil
}

func validatePrometheusDeploymentSemantics(parameters map[string]interface{}) error {
	scrapeInterval, scrapeErr := parsePrometheusDuration(fmt.Sprint(parameters["scrape_interval"]))
	if scrapeErr != nil {
		return fmt.Errorf("部署参数 %q 无效: %w", "scrape_interval", scrapeErr)
	}
	scrapeTimeout, timeoutErr := parsePrometheusDuration(fmt.Sprint(parameters["scrape_timeout"]))
	if timeoutErr != nil {
		return fmt.Errorf("部署参数 %q 无效: %w", "scrape_timeout", timeoutErr)
	}
	if scrapeTimeout > scrapeInterval {
		return fmt.Errorf("部署参数 %q 不能大于 %q", "scrape_timeout", "scrape_interval")
	}
	if enabled, exists := parameters["enable_lifecycle"]; !exists || enabled != true {
		return fmt.Errorf("部署参数 %q 必须启用，平台配置同步依赖 /-/reload", "enable_lifecycle")
	}
	return nil
}

func validateAlertmanagerDeploymentSemantics(parameters map[string]interface{}) error {
	groupWait, waitErr := parsePrometheusDuration(fmt.Sprint(parameters["group_wait"]))
	if waitErr != nil {
		return fmt.Errorf("部署参数 %q 无效: %w", "group_wait", waitErr)
	}
	groupInterval, intervalErr := parsePrometheusDuration(fmt.Sprint(parameters["group_interval"]))
	if intervalErr != nil {
		return fmt.Errorf("部署参数 %q 无效: %w", "group_interval", intervalErr)
	}
	repeatInterval, repeatErr := parsePrometheusDuration(fmt.Sprint(parameters["repeat_interval"]))
	if repeatErr != nil {
		return fmt.Errorf("部署参数 %q 无效: %w", "repeat_interval", repeatErr)
	}
	if groupWait > groupInterval {
		return fmt.Errorf("部署参数 %q 不能大于 %q", "group_wait", "group_interval")
	}
	if groupInterval > repeatInterval {
		return fmt.Errorf("部署参数 %q 不能大于 %q", "group_interval", "repeat_interval")
	}
	routePrefix, _ := parameters["route_prefix"].(string)
	if !strings.HasPrefix(routePrefix, "/") || strings.ContainsAny(routePrefix, "?#\\") {
		return fmt.Errorf("部署参数 %q 必须是以 / 开头且不含查询参数、片段或反斜杠的路径", "route_prefix")
	}
	for _, segment := range strings.Split(routePrefix, "/") {
		if segment == "." || segment == ".." || strings.TrimSpace(segment) != segment {
			return fmt.Errorf("部署参数 %q 包含不安全路径段", "route_prefix")
		}
	}
	if concurrency, ok := deploymentInteger(parameters["web_get_concurrency"]); !ok || concurrency < 0 {
		return fmt.Errorf("部署参数 %q 必须是不小于 0 的整数", "web_get_concurrency")
	}
	return nil
}

var grafanaHostnameLabelPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)
var grafanaTimezonePattern = regexp.MustCompile(`^(?:browser|utc|[A-Za-z0-9_+-]+(?:/[A-Za-z0-9_+-]+)+)$`)

func validateGrafanaDeploymentSemantics(parameters map[string]interface{}) error {
	domain, _ := parameters["domain"].(string)
	if len(domain) > 253 && net.ParseIP(strings.Trim(domain, "[]")) == nil {
		return fmt.Errorf("部署参数 %q 不是有效的主机名或 IP", "domain")
	}
	if net.ParseIP(strings.Trim(domain, "[]")) == nil {
		for _, label := range strings.Split(domain, ".") {
			if !grafanaHostnameLabelPattern.MatchString(label) {
				return fmt.Errorf("部署参数 %q 不是有效的主机名或 IP", "domain")
			}
		}
	}
	timezone, _ := parameters["default_timezone"].(string)
	if !grafanaTimezonePattern.MatchString(timezone) {
		return fmt.Errorf("部署参数 %q 必须是 browser、utc 或安全的 IANA 时区", "default_timezone")
	}
	rootURL, _ := parameters["root_url"].(string)
	var parsedRootURL *url.URL
	if rootURL != "" {
		parsedRootURL, _ = url.ParseRequestURI(rootURL)
		if parsedRootURL.RawQuery != "" || parsedRootURL.Fragment != "" {
			return fmt.Errorf("部署参数 %q 不能包含查询参数或片段", "root_url")
		}
		if parsedRootURL.Path != "" && parsedRootURL.Path != "/" && !strings.HasSuffix(parsedRootURL.Path, "/") {
			return fmt.Errorf("部署参数 %q 的子路径必须以 / 结尾", "root_url")
		}
		if parsedRootURL.Path != "" && parsedRootURL.Path != "/" && parameters["serve_from_sub_path"] != true {
			return fmt.Errorf("外部访问地址包含子路径时，部署参数 %q 必须启用", "serve_from_sub_path")
		}
	}
	if parameters["cookie_secure"] == true && (parsedRootURL == nil || parsedRootURL.Scheme != "https") {
		return fmt.Errorf("部署参数 %q 启用时必须配置 HTTPS 外部访问地址", "cookie_secure")
	}
	return nil
}

func applyGrafanaInitialPassword(request *componentCreateRequest) error {
	environment := map[string]string{}
	if strings.TrimSpace(request.CustomEnv) != "" {
		if err := json.Unmarshal([]byte(request.CustomEnv), &environment); err != nil {
			return fmt.Errorf("Grafana 环境变量无效: %w", err)
		}
	}
	environment["GF_SECURITY_ADMIN_USER"] = "admin"
	environment["GF_SECURITY_ADMIN_PASSWORD"] = request.InitialPassword
	encoded, err := json.Marshal(environment)
	if err != nil {
		return err
	}
	request.CustomEnv = string(encoded)
	return nil
}

func validateNodeExporterDeploymentSemantics(parameters map[string]interface{}) error {
	disabled, _ := deploymentStrings(parameters["disabled_collectors"])
	enabled, _ := deploymentStrings(parameters["enabled_collectors"])
	disabledSet := make(map[string]bool, len(disabled))
	for _, collector := range disabled {
		disabledSet[collector] = true
	}
	for _, collector := range []string{"cpu", "filesystem", "meminfo"} {
		if disabledSet[collector] {
			return fmt.Errorf("部署参数 %q 不能禁用平台默认主机告警依赖的 %s 采集器", "disabled_collectors", collector)
		}
	}
	for _, collector := range enabled {
		if disabledSet[collector] {
			return fmt.Errorf("采集器 %q 不能同时启用和禁用", collector)
		}
	}
	if maximum, ok := deploymentInteger(parameters["web_max_requests"]); !ok || maximum < 0 {
		return fmt.Errorf("部署参数 %q 必须是不小于 0 的整数", "web_max_requests")
	}
	return nil
}

var blackboxDNSNamePattern = regexp.MustCompile(`^[A-Za-z0-9_](?:[A-Za-z0-9_.-]{0,251}[A-Za-z0-9_.])?$`)

func validateBlackboxDeploymentSemantics(parameters map[string]interface{}) error {
	if parameters["http_fail_if_ssl"] == true && parameters["http_fail_if_not_ssl"] == true {
		return fmt.Errorf("部署参数 %q 与 %q 不能同时启用", "http_fail_if_ssl", "http_fail_if_not_ssl")
	}
	dnsName, _ := parameters["dns_query_name"].(string)
	if !blackboxDNSNamePattern.MatchString(dnsName) || strings.Contains(dnsName, "..") {
		return fmt.Errorf("部署参数 %q 必须是安全的 DNS 查询名称", "dns_query_name")
	}
	for _, field := range []string{"history_limit", "icmp_payload_size"} {
		if value, ok := deploymentInteger(parameters[field]); !ok || value < 0 {
			return fmt.Errorf("部署参数 %q 必须是不小于 0 的整数", field)
		}
	}
	if externalURL, _ := parameters["external_url"].(string); externalURL != "" {
		parsed, _ := url.ParseRequestURI(externalURL)
		if parsed.Path != "" && parsed.Path != "/" {
			return fmt.Errorf("部署参数 %q 暂不支持子路径，以保持健康检查与探测端点一致", "external_url")
		}
	}
	return nil
}

func validatePushgatewayDeploymentSemantics(parameters map[string]interface{}) error {
	interval, err := parsePrometheusDuration(fmt.Sprint(parameters["persistence_interval"]))
	if err != nil {
		return fmt.Errorf("部署参数 %q 无效: %w", "persistence_interval", err)
	}
	if interval < 1000 || interval > 24*60*60*1000 {
		return fmt.Errorf("部署参数 %q 必须在 1s 到 24h 之间", "persistence_interval")
	}
	if externalURL, _ := parameters["external_url"].(string); externalURL != "" {
		parsed, _ := url.ParseRequestURI(externalURL)
		if parsed.Path != "" && parsed.Path != "/" {
			return fmt.Errorf("部署参数 %q 暂不支持子路径，以保持健康检查与指标推送端点一致", "external_url")
		}
	}
	return nil
}

func validateLokiDeploymentSemantics(parameters map[string]interface{}) error {
	retention, retentionErr := parsePrometheusDuration(fmt.Sprint(parameters["retention_period"]))
	if retentionErr != nil {
		return fmt.Errorf("部署参数 %q 无效: %w", "retention_period", retentionErr)
	}
	if retention < 24*60*60*1000 {
		return fmt.Errorf("部署参数 %q 不能小于 24h", "retention_period")
	}
	oldSampleAge, ageErr := parsePrometheusDuration(fmt.Sprint(parameters["reject_old_samples_max_age"]))
	if ageErr != nil {
		return fmt.Errorf("部署参数 %q 无效: %w", "reject_old_samples_max_age", ageErr)
	}
	if parameters["reject_old_samples"] == true && oldSampleAge > retention {
		return fmt.Errorf("部署参数 %q 不能大于 %q", "reject_old_samples_max_age", "retention_period")
	}
	rate, rateOK := deploymentInteger(parameters["ingestion_rate_mb"])
	burst, burstOK := deploymentInteger(parameters["ingestion_burst_size_mb"])
	if !rateOK || !burstOK || burst < rate {
		return fmt.Errorf("部署参数 %q 不能小于 %q", "ingestion_burst_size_mb", "ingestion_rate_mb")
	}
	compactorInterval, compactorErr := parsePrometheusDuration(fmt.Sprint(parameters["compactor_interval"]))
	if compactorErr != nil {
		return fmt.Errorf("部署参数 %q 无效: %w", "compactor_interval", compactorErr)
	}
	if compactorInterval < 60*1000 || compactorInterval > 60*60*1000 {
		return fmt.Errorf("部署参数 %q 必须在 1m 到 1h 之间", "compactor_interval")
	}
	return nil
}

var promtailNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

func validatePromtailDeploymentSemantics(parameters map[string]interface{}) error {
	if parameters["file_log_enabled"] != true && parameters["journal_enabled"] != true {
		return fmt.Errorf("文件日志与 systemd Journal 至少启用一种采集源")
	}
	clientURL, _ := parameters["client_url"].(string)
	parsedClient, _ := url.ParseRequestURI(clientURL)
	if !strings.HasSuffix(parsedClient.Path, "/loki/api/v1/push") {
		return fmt.Errorf("部署参数 %q 必须指向 /loki/api/v1/push", "client_url")
	}
	fileGlob, _ := parameters["file_glob"].(string)
	if !strings.HasPrefix(fileGlob, "/") || strings.Contains(fileGlob, "..") ||
		strings.ContainsAny(fileGlob, "\r\n\"'") {
		return fmt.Errorf("部署参数 %q 必须是安全的绝对路径匹配", "file_glob")
	}
	journalPath, _ := parameters["journal_path"].(string)
	if !strings.HasPrefix(journalPath, "/") || strings.Contains(journalPath, "..") ||
		strings.ContainsAny(journalPath, "\r\n*?[]{}\"'") {
		return fmt.Errorf("部署参数 %q 必须是安全的绝对目录", "journal_path")
	}
	for _, field := range []string{"file_job_name", "journal_job_name"} {
		if !promtailNamePattern.MatchString(fmt.Sprint(parameters[field])) {
			return fmt.Errorf("部署参数 %q 必须是安全名称", field)
		}
	}
	if tenant, _ := parameters["tenant_id"].(string); tenant != "" && !promtailNamePattern.MatchString(tenant) {
		return fmt.Errorf("部署参数 %q 必须是安全名称", "tenant_id")
	}
	syncPeriod, _ := parsePrometheusDuration(fmt.Sprint(parameters["positions_sync_period"]))
	if syncPeriod < 1000 || syncPeriod > 5*60*1000 {
		return fmt.Errorf("部署参数 %q 必须在 1s 到 5m 之间", "positions_sync_period")
	}
	batchWait, _ := parsePrometheusDuration(fmt.Sprint(parameters["batch_wait"]))
	clientTimeout, _ := parsePrometheusDuration(fmt.Sprint(parameters["client_timeout"]))
	if batchWait > clientTimeout {
		return fmt.Errorf("部署参数 %q 不能大于 %q", "batch_wait", "client_timeout")
	}
	minBackoff, _ := parsePrometheusDuration(fmt.Sprint(parameters["backoff_min_period"]))
	maxBackoff, _ := parsePrometheusDuration(fmt.Sprint(parameters["backoff_max_period"]))
	if minBackoff > maxBackoff {
		return fmt.Errorf("部署参数 %q 不能大于 %q", "backoff_min_period", "backoff_max_period")
	}
	if retries, ok := deploymentInteger(parameters["backoff_max_retries"]); !ok || retries < 0 {
		return fmt.Errorf("部署参数 %q 必须是不小于 0 的整数", "backoff_max_retries")
	}
	return nil
}

func validateThanosDeploymentSemantics(parameters map[string]interface{}) error {
	for _, field := range []string{"tsdb_path", "reloader_config_file", "reloader_rule_dir"} {
		value, _ := parameters[field].(string)
		if !strings.HasPrefix(value, "/") || strings.Contains(value, "..") ||
			strings.ContainsAny(value, "\r\n*?[]{}\"'") {
			return fmt.Errorf("部署参数 %q 必须是安全的绝对路径", field)
		}
	}
	configInterval, _ := parsePrometheusDuration(fmt.Sprint(parameters["prometheus_get_config_interval"]))
	configTimeout, _ := parsePrometheusDuration(fmt.Sprint(parameters["prometheus_get_config_timeout"]))
	if configTimeout > configInterval {
		return fmt.Errorf("部署参数 %q 不能大于 %q", "prometheus_get_config_timeout", "prometheus_get_config_interval")
	}
	watchInterval, _ := parsePrometheusDuration(fmt.Sprint(parameters["reloader_watch_interval"]))
	retryInterval, _ := parsePrometheusDuration(fmt.Sprint(parameters["reloader_retry_interval"]))
	if retryInterval > watchInterval {
		return fmt.Errorf("部署参数 %q 不能大于 %q", "reloader_retry_interval", "reloader_watch_interval")
	}
	return nil
}
