package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/promlifecycle/platform/models"
	"gorm.io/gorm"
)

const (
	acceptanceHTTPTimeout = 5 * time.Second
	acceptanceMaxBody     = 1 << 20
	acceptanceFreshness   = 2 * time.Minute
)

var (
	ErrAcceptanceNotFound = errors.New("acceptance scope not found")
	ErrAcceptanceConflict = errors.New("acceptance scope conflict")
)

type DeploymentAcceptanceScope struct {
	Kind         string `json:"kind"`
	StackID      uint   `json:"stack_id,omitempty"`
	HostID       uint   `json:"host_id,omitempty"`
	PrometheusID uint   `json:"prometheus_id,omitempty"`
}

type DeploymentAcceptanceCheck struct {
	Key    string `json:"key"`
	Status string `json:"status"`
	Reason string `json:"reason"`
}

type DeploymentAcceptanceTarget struct {
	ComponentID uint                 `json:"component_id"`
	HostID      uint                 `json:"host_id"`
	Type        models.ComponentType `json:"type"`
	Address     string               `json:"address"`
	Status      string               `json:"status"`
	Reason      string               `json:"reason"`
	LastScrape  *time.Time           `json:"last_scrape"`
}

type DeploymentAcceptanceReport struct {
	CheckedAt time.Time                    `json:"checked_at"`
	Passed    bool                         `json:"passed"`
	Message   string                       `json:"message"`
	Scope     DeploymentAcceptanceScope    `json:"scope"`
	Checks    []DeploymentAcceptanceCheck  `json:"checks"`
	Targets   []DeploymentAcceptanceTarget `json:"targets"`
}

type DeploymentAcceptanceService struct {
	db     *gorm.DB
	client *http.Client
	now    func() time.Time
}

func NewDeploymentAcceptanceService(db *gorm.DB, client *http.Client) *DeploymentAcceptanceService {
	if client == nil {
		client = &http.Client{}
	}
	copyClient := *client
	copyClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	if copyClient.Timeout == 0 || copyClient.Timeout > acceptanceHTTPTimeout {
		copyClient.Timeout = acceptanceHTTPTimeout
	}
	return &DeploymentAcceptanceService{db: db, client: &copyClient, now: time.Now}
}

func (s *DeploymentAcceptanceService) EvaluateStack(ctx context.Context, stackID, prometheusID uint) (DeploymentAcceptanceReport, error) {
	now := s.now().UTC()
	report := DeploymentAcceptanceReport{
		CheckedAt: now,
		Scope:     DeploymentAcceptanceScope{Kind: "stack", StackID: stackID},
		Checks:    make([]DeploymentAcceptanceCheck, 0),
		Targets:   make([]DeploymentAcceptanceTarget, 0),
	}
	if s.db == nil {
		return report, errors.New("acceptance database is not configured")
	}

	var stack models.DeploymentStack
	if err := s.db.Preload("Steps", func(tx *gorm.DB) *gorm.DB { return tx.Order("position ASC") }).First(&stack, stackID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return report, fmt.Errorf("%w: deployment stack %d", ErrAcceptanceNotFound, stackID)
		}
		return report, fmt.Errorf("load deployment stack: %w", err)
	}
	report.Scope.HostID = stack.HostID
	if stack.Status != "success" || len(stack.Steps) == 0 || stack.Total != len(stack.Steps) {
		return report, fmt.Errorf("%w: deployment stack %d is not completely successful", ErrAcceptanceConflict, stackID)
	}

	components := make([]models.Component, 0, len(stack.Steps))
	seen := make(map[uint]struct{}, len(stack.Steps))
	var clusterID uint
	var boundPrometheus *models.Component
	for _, step := range stack.Steps {
		if step.Status != "success" || step.ComponentID == nil || *step.ComponentID == 0 {
			return report, fmt.Errorf("%w: deployment stack step %d is not successfully bound", ErrAcceptanceConflict, step.Position)
		}
		if _, duplicate := seen[*step.ComponentID]; duplicate {
			return report, fmt.Errorf("%w: component %d is bound to multiple stack steps", ErrAcceptanceConflict, *step.ComponentID)
		}
		seen[*step.ComponentID] = struct{}{}
		var component models.Component
		if err := s.db.Preload("Host").First(&component, *step.ComponentID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return report, fmt.Errorf("%w: linked component %d no longer exists", ErrAcceptanceConflict, *step.ComponentID)
			}
			return report, fmt.Errorf("load linked component %d: %w", *step.ComponentID, err)
		}
		if component.HostID != stack.HostID || string(component.Type) != step.Type || component.Status == "removed" {
			return report, fmt.Errorf("%w: linked component %d identity changed", ErrAcceptanceConflict, component.ID)
		}
		if len(components) == 0 {
			clusterID = component.ClusterID
		} else if component.ClusterID != clusterID {
			return report, fmt.Errorf("%w: stack components no longer share one cluster", ErrAcceptanceConflict)
		}
		components = append(components, component)
		if component.Type == models.ComponentPrometheus {
			if boundPrometheus != nil {
				return report, fmt.Errorf("%w: stack has multiple bound Prometheus instances", ErrAcceptanceConflict)
			}
			copyComponent := component
			boundPrometheus = &copyComponent
		}
	}

	report.Checks = append(report.Checks, DeploymentAcceptanceCheck{Key: "stack_state", Status: "pass", Reason: "任务及全部步骤均为 success，组件绑定未变化"})
	for i := range components {
		report.Checks = append(report.Checks, componentStateCheck(&components[i]))
	}

	selected := boundPrometheus
	if boundPrometheus != nil {
		if prometheusID != 0 && prometheusID != boundPrometheus.ID {
			return report, fmt.Errorf("%w: stack is bound to Prometheus %d", ErrAcceptanceConflict, boundPrometheus.ID)
		}
	} else {
		var err error
		selected, err = s.selectPrometheus(clusterID, prometheusID)
		if err != nil {
			return report, err
		}
	}
	if selected == nil {
		report.Checks = append(report.Checks, DeploymentAcceptanceCheck{Key: "prometheus_scope", Status: "fail", Reason: "任务集群没有可验收 Prometheus"})
		report.Message = "部署任务运行验收未通过：没有可验收 Prometheus"
		return report, nil
	}
	report.Scope.PrometheusID = selected.ID
	if boundPrometheus == nil {
		report.Checks = append(report.Checks, componentStateCheck(selected))
	}

	expected := make([]expectedTarget, 0, len(components))
	for i := range components {
		component := &components[i]
		if !componentHasScrapeTarget(component.Type) {
			continue
		}
		address, err := componentMetricsURL(&component.Host, component)
		if err != nil {
			report.Checks = append(report.Checks, failedCheck("target_identity", fmt.Sprintf("组件 %d 保存地址无效", component.ID)))
			report.Targets = append(report.Targets, DeploymentAcceptanceTarget{ComponentID: component.ID, HostID: component.HostID, Type: component.Type, Status: "unknown", Reason: "组件保存地址无效"})
			report.Message = "部署任务运行验收未通过：组件保存地址无效"
			return report, nil
		}
		expected = append(expected, expectedTarget{component: *component, host: component.Host, address: address})
	}
	if len(expected) == 0 {
		report.Checks = append(report.Checks, failedCheck("targets", "任务没有适用的 Prometheus 采集目标"))
		report.Message = "部署任务运行验收未通过：任务没有适用的 Prometheus 采集目标"
		return report, nil
	}

	readyCheck, targets, targetsCheck := s.probePrometheus(ctx, selected, expected, now)
	report.Checks = append(report.Checks, readyCheck, targetsCheck)
	report.Targets = targets
	report.Passed = checksPassed(report.Checks) && targetsPassed(report.Targets)
	if report.Passed {
		report.Message = "部署任务运行验收通过"
	} else {
		report.Message = "部署任务运行验收未通过"
	}
	return report, nil
}

func (s *DeploymentAcceptanceService) EvaluateHost(ctx context.Context, hostID, prometheusID, exporterID uint) (DeploymentAcceptanceReport, error) {
	now := s.now().UTC()
	report := DeploymentAcceptanceReport{
		CheckedAt: now,
		Scope:     DeploymentAcceptanceScope{Kind: "host", HostID: hostID},
		Checks:    make([]DeploymentAcceptanceCheck, 0, 4),
		Targets:   make([]DeploymentAcceptanceTarget, 0, 1),
	}
	if s.db == nil {
		return report, errors.New("acceptance database is not configured")
	}

	var host models.Host
	if err := s.db.First(&host, hostID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return report, fmt.Errorf("%w: host %d", ErrAcceptanceNotFound, hostID)
		}
		return report, fmt.Errorf("load host: %w", err)
	}
	exporterType := models.ComponentNodeExporter
	if host.ConnectionType == "winrm" || strings.EqualFold(host.OS, "windows") || host.IsWindows {
		exporterType = models.ComponentWindowsExporter
	}

	exporter, err := s.selectExporter(host.ID, exporterType, exporterID)
	if err != nil {
		return report, err
	}
	if exporter == nil {
		report.Checks = append(report.Checks, DeploymentAcceptanceCheck{Key: "component_state", Status: "fail", Reason: "未找到该主机的可验收 Exporter"})
		report.Message = "主机监控运行验收未通过：没有可验收 Exporter"
		return report, nil
	}

	prometheus, err := s.selectPrometheus(exporter.ClusterID, prometheusID)
	if err != nil {
		return report, err
	}
	if prometheus == nil {
		report.Checks = append(report.Checks,
			componentStateCheck(exporter),
			DeploymentAcceptanceCheck{Key: "prometheus_scope", Status: "fail", Reason: "该集群没有可验收 Prometheus"},
		)
		report.Message = "主机监控运行验收未通过：没有可验收 Prometheus"
		return report, nil
	}
	report.Scope.PrometheusID = prometheus.ID
	report.Checks = append(report.Checks, componentStateCheck(exporter), componentStateCheck(prometheus))

	expected, err := componentMetricsURL(&host, exporter)
	if err != nil {
		report.Checks = append(report.Checks, DeploymentAcceptanceCheck{Key: "target_identity", Status: "fail", Reason: "Exporter 保存地址无效"})
		report.Targets = append(report.Targets, DeploymentAcceptanceTarget{ComponentID: exporter.ID, HostID: host.ID, Type: exporter.Type, Status: "unknown", Reason: "Exporter 保存地址无效"})
		report.Message = "主机监控运行验收未通过：Exporter 保存地址无效"
		return report, nil
	}

	readyCheck, targets, targetsCheck := s.probePrometheus(ctx, prometheus, []expectedTarget{{component: *exporter, host: host, address: expected}}, now)
	report.Checks = append(report.Checks, readyCheck, targetsCheck)
	report.Targets = targets
	report.Passed = checksPassed(report.Checks) && targetsPassed(report.Targets)
	if report.Passed {
		report.Message = "主机监控运行验收通过"
	} else {
		report.Message = "主机监控运行验收未通过"
	}
	return report, nil
}

func (s *DeploymentAcceptanceService) selectExporter(hostID uint, kind models.ComponentType, id uint) (*models.Component, error) {
	if id != 0 {
		var component models.Component
		if err := s.db.First(&component, id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, fmt.Errorf("%w: exporter %d", ErrAcceptanceNotFound, id)
			}
			return nil, fmt.Errorf("load exporter: %w", err)
		}
		if component.HostID != hostID || component.Type != kind || component.Status == "removed" {
			return nil, fmt.Errorf("%w: exporter %d is outside the host scope", ErrAcceptanceConflict, id)
		}
		return &component, nil
	}
	var components []models.Component
	if err := s.db.Where("host_id = ? AND type = ? AND status <> ?", hostID, kind, "removed").Order("id ASC").Find(&components).Error; err != nil {
		return nil, fmt.Errorf("load exporters: %w", err)
	}
	if len(components) == 0 {
		return nil, nil
	}
	if len(components) > 1 {
		return nil, fmt.Errorf("%w: multiple exporters require component_id", ErrAcceptanceConflict)
	}
	return &components[0], nil
}

func (s *DeploymentAcceptanceService) selectPrometheus(clusterID, id uint) (*models.Component, error) {
	if id != 0 {
		var component models.Component
		if err := s.db.Preload("Host").First(&component, id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, fmt.Errorf("%w: prometheus %d", ErrAcceptanceNotFound, id)
			}
			return nil, fmt.Errorf("load Prometheus: %w", err)
		}
		if component.Type != models.ComponentPrometheus || component.ClusterID != clusterID || component.Status == "removed" {
			return nil, fmt.Errorf("%w: Prometheus %d is outside the component cluster", ErrAcceptanceConflict, id)
		}
		return &component, nil
	}
	var components []models.Component
	if err := s.db.Preload("Host").Where("cluster_id = ? AND type = ? AND status <> ?", clusterID, models.ComponentPrometheus, "removed").Order("id ASC").Find(&components).Error; err != nil {
		return nil, fmt.Errorf("load Prometheus instances: %w", err)
	}
	if len(components) == 0 {
		return nil, nil
	}
	if len(components) > 1 {
		return nil, fmt.Errorf("%w: multiple Prometheus instances require prometheus_id", ErrAcceptanceConflict)
	}
	return &components[0], nil
}

func componentStateCheck(component *models.Component) DeploymentAcceptanceCheck {
	if component.Status == models.StatusRunning {
		return DeploymentAcceptanceCheck{Key: "component_state", Status: "pass", Reason: fmt.Sprintf("组件 %d（%s）为 running", component.ID, component.Type)}
	}
	return DeploymentAcceptanceCheck{Key: "component_state", Status: "fail", Reason: fmt.Sprintf("组件 %d（%s）状态为 %s", component.ID, component.Type, component.Status)}
}

type expectedTarget struct {
	component models.Component
	host      models.Host
	address   string
}

type prometheusTarget struct {
	ScrapeURL  string    `json:"scrapeUrl"`
	Health     string    `json:"health"`
	LastScrape time.Time `json:"lastScrape"`
}

func (s *DeploymentAcceptanceService) probePrometheus(ctx context.Context, prometheus *models.Component, expected []expectedTarget, now time.Time) (DeploymentAcceptanceCheck, []DeploymentAcceptanceTarget, DeploymentAcceptanceCheck) {
	base, err := componentBaseURL(&prometheus.Host, prometheus, 9090)
	if err != nil {
		reason := "Prometheus 保存地址无效"
		return failedCheck("prometheus_ready", reason), unknownTargets(expected, reason), failedCheck("targets", reason)
	}
	prefix := models.ManagedRoutePrefix(prometheus)
	readyURL := endpointURL(base, prefix, "/-/ready")
	readyStatus, _, readyErr := s.get(ctx, readyURL, 64<<10)
	ready := DeploymentAcceptanceCheck{Key: "prometheus_ready", Status: "pass", Reason: fmt.Sprintf("Prometheus %d ready 返回 HTTP %d", prometheus.ID, readyStatus)}
	if readyErr != nil || readyStatus < 200 || readyStatus >= 300 {
		ready = failedCheck("prometheus_ready", safeHTTPReason("Prometheus ready 探测失败", readyStatus, readyErr))
	}

	targetsURL := endpointURL(base, prefix, "/api/v1/targets")
	status, body, requestErr := s.get(ctx, targetsURL, acceptanceMaxBody)
	if requestErr != nil || status < 200 || status >= 300 {
		reason := safeHTTPReason("读取 Prometheus targets 失败", status, requestErr)
		return ready, unknownTargets(expected, reason), failedCheck("targets", reason)
	}
	var payload struct {
		Status string `json:"status"`
		Data   *struct {
			ActiveTargets *[]prometheusTarget `json:"activeTargets"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.Status != "success" || payload.Data == nil || payload.Data.ActiveTargets == nil {
		reason := "Prometheus targets 响应不是有效的 success JSON"
		return ready, unknownTargets(expected, reason), failedCheck("targets", reason)
	}

	results := make([]DeploymentAcceptanceTarget, 0, len(expected))
	for _, item := range expected {
		matches := make([]prometheusTarget, 0, 1)
		for _, active := range *payload.Data.ActiveTargets {
			if scrapeURLsEqual(active.ScrapeURL, item.address) {
				matches = append(matches, active)
			}
		}
		result := DeploymentAcceptanceTarget{ComponentID: item.component.ID, HostID: item.host.ID, Type: item.component.Type, Address: item.address, Status: "missing", Reason: "Prometheus active targets 中缺少精确端点"}
		switch len(matches) {
		case 0:
		case 1:
			match := matches[0]
			if !match.LastScrape.IsZero() {
				last := match.LastScrape.UTC()
				result.LastScrape = &last
			}
			if match.Health != "up" {
				result.Status, result.Reason = "down", "精确目标实时状态不是 up"
			} else if match.LastScrape.IsZero() || now.Sub(match.LastScrape) > acceptanceFreshness || match.LastScrape.After(now.Add(time.Minute)) {
				result.Status, result.Reason = "unknown", "精确目标 lastScrape 缺失或已过期"
			} else {
				result.Status, result.Reason = "up", "精确目标实时为 up 且 lastScrape 新鲜"
			}
		default:
			result.Status, result.Reason = "unknown", "Prometheus 返回多个相同 scrape URL，无法确定唯一目标"
		}
		results = append(results, result)
	}
	if targetsPassed(results) {
		return ready, results, DeploymentAcceptanceCheck{Key: "targets", Status: "pass", Reason: "全部期望目标均精确匹配且实时为 up"}
	}
	return ready, results, failedCheck("targets", "至少一个期望目标未通过精确实时采集验收")
}

func componentMetricsURL(host *models.Host, component *models.Component) (string, error) {
	return componentURLWithPath(host, component, defaultComponentPort(component.Type), "/metrics")
}

func componentBaseURL(host *models.Host, component *models.Component, fallbackPort int) (*url.URL, error) {
	raw, err := componentURLWithPath(host, component, fallbackPort, "")
	if err != nil {
		return nil, err
	}
	return url.Parse(raw)
}

func componentURLWithPath(host *models.Host, component *models.Component, fallbackPort int, endpointPath string) (string, error) {
	hostname := strings.TrimSpace(host.IP)
	if strings.HasPrefix(hostname, "[") && strings.HasSuffix(hostname, "]") {
		hostname = strings.TrimSuffix(strings.TrimPrefix(hostname, "["), "]")
	}
	if hostname == "" || strings.ContainsAny(hostname, "/?#@") {
		return "", errors.New("invalid component host")
	}
	port := component.Port
	if port == 0 {
		port = fallbackPort
	}
	if port < 1 || port > 65535 {
		return "", errors.New("invalid component port")
	}
	value := &url.URL{Scheme: "http", Host: net.JoinHostPort(hostname, strconv.Itoa(port)), Path: endpointPath}
	return value.String(), nil
}

func defaultComponentPort(kind models.ComponentType) int {
	switch kind {
	case models.ComponentPrometheus:
		return 9090
	case models.ComponentAlertmanager:
		return 9093
	case models.ComponentGrafana:
		return 3000
	case models.ComponentNodeExporter:
		return 9100
	case models.ComponentWindowsExporter:
		return 9182
	case models.ComponentPushgateway:
		return 9091
	case models.ComponentBlackbox:
		return 9115
	case models.ComponentLoki:
		return 3100
	case models.ComponentPromtail:
		return 9080
	default:
		return 0
	}
}

func componentHasScrapeTarget(kind models.ComponentType) bool {
	switch kind {
	case models.ComponentPrometheus,
		models.ComponentAlertmanager,
		models.ComponentNodeExporter,
		models.ComponentWindowsExporter,
		models.ComponentPushgateway,
		models.ComponentBlackbox,
		models.ComponentLoki,
		models.ComponentPromtail,
		models.ComponentThanos:
		return true
	default:
		return false
	}
}

func endpointURL(base *url.URL, prefix, suffix string) string {
	copyURL := *base
	copyURL.Path = "/" + strings.TrimPrefix(path.Join(prefix, suffix), "/")
	copyURL.RawQuery = ""
	copyURL.User = nil
	return copyURL.String()
}

func (s *DeploymentAcceptanceService) get(ctx context.Context, address string, limit int64) (int, []byte, error) {
	requestCtx, cancel := context.WithTimeout(ctx, acceptanceHTTPTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, address, nil)
	if err != nil {
		return 0, nil, err
	}
	response, err := s.client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return response.StatusCode, nil, err
	}
	if int64(len(body)) > limit {
		return response.StatusCode, nil, errors.New("response exceeds size limit")
	}
	return response.StatusCode, body, nil
}

func scrapeURLsEqual(actual, expected string) bool {
	actualURL, actualErr := url.Parse(actual)
	expectedURL, expectedErr := url.Parse(expected)
	if actualErr != nil || expectedErr != nil || actualURL.User != nil || actualURL.RawQuery != "" || actualURL.Fragment != "" {
		return false
	}
	return strings.EqualFold(actualURL.Scheme, expectedURL.Scheme) && strings.EqualFold(actualURL.Hostname(), expectedURL.Hostname()) && actualURL.Port() == expectedURL.Port() && actualURL.EscapedPath() == expectedURL.EscapedPath()
}

func unknownTargets(expected []expectedTarget, reason string) []DeploymentAcceptanceTarget {
	results := make([]DeploymentAcceptanceTarget, 0, len(expected))
	for _, item := range expected {
		results = append(results, DeploymentAcceptanceTarget{ComponentID: item.component.ID, HostID: item.host.ID, Type: item.component.Type, Address: item.address, Status: "unknown", Reason: reason})
	}
	return results
}

func failedCheck(key, reason string) DeploymentAcceptanceCheck {
	return DeploymentAcceptanceCheck{Key: key, Status: "fail", Reason: reason}
}

func safeHTTPReason(prefix string, status int, err error) string {
	if err != nil {
		return prefix + "：网络或响应错误"
	}
	return fmt.Sprintf("%s：HTTP %d", prefix, status)
}

func checksPassed(checks []DeploymentAcceptanceCheck) bool {
	if len(checks) == 0 {
		return false
	}
	for _, check := range checks {
		if check.Status != "pass" {
			return false
		}
	}
	return true
}

func targetsPassed(targets []DeploymentAcceptanceTarget) bool {
	if len(targets) == 0 {
		return false
	}
	for _, target := range targets {
		if target.Status != "up" {
			return false
		}
	}
	return true
}
