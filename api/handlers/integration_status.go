package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/core/deployer"
	"github.com/promlifecycle/platform/core/ssh"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/services"
)

type integrationCheck struct {
	Key      string `json:"key"`
	Name     string `json:"name"`
	Passed   bool   `json:"passed"`
	Evidence string `json:"evidence"`
}

func (h *Handler) GetMonitoringIntegrationStatus(c *gin.Context) {
	checks := h.monitoringIntegrationChecks(c.Request.Context())
	passed := len(checks) > 0
	for _, check := range checks {
		passed = passed && check.Passed
	}
	c.JSON(http.StatusOK, gin.H{"passed": passed, "checks": checks, "checked_at": time.Now()})
}

func (h *Handler) ReconcileMonitoringIntegration(c *gin.Context) {
	clusterID, err := services.SingleNodeID(h.db)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Minute)
	defer cancel()
	if err := h.ensureDefaultHostAlertRules(clusterID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建推荐告警失败: " + err.Error()})
		return
	}
	if err := h.autoConfig.SyncPrometheusConfig(clusterID, h.basePath); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "同步 Prometheus 失败: " + err.Error()})
		return
	}
	if err := h.alertSync.SyncAlertRules(clusterID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "同步告警规则失败: " + err.Error()})
		return
	}
	var alertmanager models.Component
	if h.db.Preload("Host").Where("cluster_id = ? AND type = ? AND status = ?", clusterID, models.ComponentAlertmanager, models.StatusRunning).First(&alertmanager).Error == nil {
		if err := h.autoConfig.SyncAlertmanagerConfig(clusterID, h.basePath); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "同步 Alertmanager 失败: " + err.Error()})
			return
		}
	}
	var grafana models.Component
	if h.db.Preload("Host").Where("cluster_id = ? AND type = ? AND status = ?", clusterID, models.ComponentGrafana, models.StatusRunning).First(&grafana).Error == nil {
		binaryDeployer := deployer.NewBinaryDeployer(h.db, h.basePath)
		if err := binaryDeployer.SyncGrafanaDatasource(ctx, &grafana); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "同步 Grafana 数据源失败: " + err.Error()})
			return
		}
		endpoint := componentHTTPURL(grafana, 3000) + "/api/health"
		for attempt := 0; attempt < 15; attempt++ {
			if httpStatus(ctx, endpoint) == http.StatusOK {
				break
			}
			select {
			case <-ctx.Done():
				break
			case <-time.After(time.Second):
			}
		}
	}
	checks := h.monitoringIntegrationChecks(ctx)
	passed := true
	for _, check := range checks {
		passed = passed && check.Passed
	}
	c.JSON(http.StatusOK, gin.H{"message": "组件推荐配置与联调已重新同步", "passed": passed, "checks": checks})
}

func (h *Handler) monitoringIntegrationChecks(ctx context.Context) []integrationCheck {
	checks := make([]integrationCheck, 0, 6)
	var prometheus models.Component
	if err := h.db.Preload("Host").Where("type = ? AND status = ?", models.ComponentPrometheus, models.StatusRunning).Limit(1).Find(&prometheus).Error; err != nil || prometheus.ID == 0 {
		return append(checks, integrationCheck{Key: "prometheus", Name: "Prometheus 运行状态", Passed: false, Evidence: "未找到运行中的 Prometheus"})
	}
	promEndpoint := componentManagedHTTPURL(prometheus, 9090)
	promReady := httpStatus(ctx, promEndpoint+"/-/ready") == http.StatusOK
	checks = append(checks, integrationCheck{Key: "prometheus", Name: "Prometheus 运行状态", Passed: promReady, Evidence: promEndpoint + "/-/ready"})
	configPath := prometheus.ConfigPath
	if configPath == "" {
		configPath = "/etc/prometheus/prometheus.yml"
	}
	configData, _ := readComponentFile(prometheus.Host, configPath)
	configText := string(configData)

	targetHealth := prometheusTargetHealth(ctx, promEndpoint)
	var exporters []models.Component
	h.db.Preload("Host").Where("type = ? AND status = ?", models.ComponentNodeExporter, models.StatusRunning).Find(&exporters)
	nodePassed := len(exporters) > 0
	nodeEvidence := make([]string, 0, len(exporters))
	for _, exporter := range exporters {
		address := net.JoinHostPort(exporter.Host.IP, strconv.Itoa(defaultPortValue(exporter.Port, 9100)))
		health := targetHealth[address]
		configured := strings.Contains(configText, address)
		nodePassed = nodePassed && configured && health == "up"
		nodeEvidence = append(nodeEvidence, fmt.Sprintf("%s config=%t health=%s", address, configured, health))
	}
	checks = append(checks, integrationCheck{Key: "node_exporter", Name: "Prometheus → Node Exporter", Passed: nodePassed, Evidence: strings.Join(nodeEvidence, "; ")})

	var blackbox models.Component
	blackboxFound := h.db.Preload("Host").Where("type = ? AND status = ?", models.ComponentBlackbox, models.StatusRunning).Limit(1).Find(&blackbox).Error == nil && blackbox.ID != 0
	blackboxConfigured := blackboxFound && strings.Contains(configText, "job_name: blackbox_http") && strings.Contains(configText, net.JoinHostPort(blackbox.Host.IP, strconv.Itoa(defaultPortValue(blackbox.Port, 9115))))
	checks = append(checks, integrationCheck{Key: "blackbox", Name: "Prometheus → Blackbox 探测", Passed: !blackboxFound || blackboxConfigured, Evidence: fmt.Sprintf("installed=%t configured=%t", blackboxFound, blackboxConfigured)})

	var alertmanager models.Component
	alertmanagerFound := h.db.Preload("Host").Where("type = ? AND status = ?", models.ComponentAlertmanager, models.StatusRunning).Limit(1).Find(&alertmanager).Error == nil && alertmanager.ID != 0
	alertAddress := ""
	alertPassed := !alertmanagerFound
	if alertmanagerFound {
		alertAddress = net.JoinHostPort(alertmanager.Host.IP, strconv.Itoa(defaultPortValue(alertmanager.Port, 9093)))
		alertPassed = strings.Contains(configText, alertAddress) && httpStatus(ctx, componentManagedHTTPURL(alertmanager, 9093)+"/-/ready") == http.StatusOK
	}
	checks = append(checks, integrationCheck{Key: "alertmanager", Name: "Prometheus → Alertmanager", Passed: alertPassed, Evidence: fmt.Sprintf("installed=%t target=%s", alertmanagerFound, alertAddress)})

	var grafana models.Component
	grafanaFound := h.db.Preload("Host").Where("type = ? AND status = ?", models.ComponentGrafana, models.StatusRunning).Limit(1).Find(&grafana).Error == nil && grafana.ID != 0
	grafanaPassed := !grafanaFound
	grafanaEvidence := "not installed"
	if grafanaFound {
		datasource, _ := readComponentFile(grafana.Host, "/etc/grafana/provisioning/datasources/promlifecycle.yml")
		grafanaEndpoint := componentManagedHTTPURL(grafana, 3000)
		grafanaPassed = strings.Contains(string(datasource), promEndpoint) && httpStatus(ctx, grafanaEndpoint+"/api/health") == http.StatusOK
		grafanaEvidence = fmt.Sprintf("datasource=%t health=%t", strings.Contains(string(datasource), promEndpoint), httpStatus(ctx, grafanaEndpoint+"/api/health") == http.StatusOK)
	}
	checks = append(checks, integrationCheck{Key: "grafana", Name: "Grafana → Prometheus 数据源", Passed: grafanaPassed, Evidence: grafanaEvidence})

	var loki models.Component
	lokiFound := h.db.Preload("Host").Where("type = ? AND status = ?", models.ComponentLoki, models.StatusRunning).Limit(1).Find(&loki).Error == nil && loki.ID != 0
	lokiPassed := !lokiFound
	lokiEvidence := "not installed"
	if lokiFound && grafanaFound {
		datasource, _ := readComponentFile(grafana.Host, "/etc/grafana/provisioning/datasources/promlifecycle.yml")
		lokiEndpoint := componentHTTPURL(loki, 3100)
		lokiPassed = strings.Contains(string(datasource), "uid: promlifecycle-loki") && strings.Contains(string(datasource), lokiEndpoint)
		lokiEvidence = fmt.Sprintf("datasource=%t endpoint=%s", lokiPassed, lokiEndpoint)
	} else if lokiFound {
		lokiPassed = false
		lokiEvidence = "Loki installed but Grafana missing"
	}
	checks = append(checks, integrationCheck{Key: "grafana_loki", Name: "Grafana → Loki 数据源", Passed: lokiPassed, Evidence: lokiEvidence})

	rulesHealthy, rulesEvidence := prometheusRulesHealth(ctx, promEndpoint)
	checks = append(checks, integrationCheck{Key: "alerts", Name: "Prometheus 告警规则", Passed: rulesHealthy, Evidence: rulesEvidence})
	return checks
}

func componentHTTPURL(component models.Component, fallbackPort int) string {
	if component.InternalURL != "" {
		return strings.TrimSuffix(component.InternalURL, "/")
	}
	host := component.Host.IP
	if ssh.IsLocalHost(host, component.Host.ConnectionType) {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(defaultPortValue(component.Port, fallbackPort)))
}

func componentManagedHTTPURL(component models.Component, fallbackPort int) string {
	return componentHTTPURL(component, fallbackPort) + models.ManagedRoutePrefix(&component)
}

func defaultPortValue(value, fallback int) int {
	if value > 0 {
		return value
	}
	return fallback
}

func httpStatus(ctx context.Context, url string) int {
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		return 0
	}
	defer response.Body.Close()
	return response.StatusCode
}

func prometheusTargetHealth(ctx context.Context, endpoint string) map[string]string {
	result := map[string]string{}
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/api/v1/targets", nil)
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		return result
	}
	defer response.Body.Close()
	var payload struct {
		Data struct {
			ActiveTargets []struct {
				Health string            `json:"health"`
				Labels map[string]string `json:"labels"`
			} `json:"activeTargets"`
		} `json:"data"`
	}
	if json.NewDecoder(response.Body).Decode(&payload) == nil {
		for _, target := range payload.Data.ActiveTargets {
			result[target.Labels["instance"]] = target.Health
		}
	}
	return result
}

func prometheusRulesHealth(ctx context.Context, endpoint string) (bool, string) {
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/api/v1/rules", nil)
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		return false, err.Error()
	}
	defer response.Body.Close()
	var payload struct {
		Data struct {
			Groups []struct {
				Rules []struct {
					Name   string `json:"name"`
					Health string `json:"health"`
				} `json:"rules"`
			} `json:"groups"`
		} `json:"data"`
	}
	if json.NewDecoder(response.Body).Decode(&payload) != nil {
		return false, "invalid Prometheus rules response"
	}
	count := 0
	for _, group := range payload.Data.Groups {
		for _, rule := range group.Rules {
			count++
			if rule.Health != "ok" {
				return false, rule.Name + " health=" + rule.Health
			}
		}
	}
	return count > 0, fmt.Sprintf("%d rules health=ok", count)
}
