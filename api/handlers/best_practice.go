package handlers

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/core/deployer"
	"github.com/promlifecycle/platform/core/ssh"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
)

func (h *Handler) GetComponentBestPractice(c *gin.Context) {
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}
	var component models.Component
	if err := h.db.Preload("Host").First(&component, id).Error; err != nil {
		utils.HandleNotFoundError(c, "component")
		return
	}
	content := ""
	if component.ConfigPath != "" {
		if data, err := readComponentFile(component.Host, component.ConfigPath); err == nil {
			content = string(data)
		}
	}
	markers := bestPracticeMarkers(component.Type)
	missing := make([]string, 0)
	for _, marker := range markers {
		if !strings.Contains(content, marker) {
			missing = append(missing, marker)
		}
	}
	expectedLimits := deployer.BestPracticeResourceLimits(component.Type)
	customized := strings.TrimSpace(component.CustomArgs) != "" || (expectedLimits != "" && component.ResourceLimits != expectedLimits)
	compliant := !customized && len(missing) == 0
	c.JSON(http.StatusOK, gin.H{
		"component_id": component.ID, "type": component.Type, "compliant": compliant,
		"customized": customized, "missing_markers": missing,
		"recommended_resource_limits": expectedLimits,
		"summary":                     bestPracticeSummary(component.Type),
	})
}

func (h *Handler) ApplyComponentBestPractice(c *gin.Context) {
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}
	var component models.Component
	if err := h.db.Preload("Host").First(&component, id).Error; err != nil {
		utils.HandleNotFoundError(c, "component")
		return
	}
	component.CustomArgs = ""
	component.ResourceLimits = deployer.BestPracticeResourceLimits(component.Type)
	if err := deployer.ValidatePrometheusParameterVersion(&component, component.Version); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.db.Save(&component).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存推荐参数失败"})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	binaryDeployer := deployer.NewBinaryDeployer(h.db, h.basePath)

	var err error
	switch component.Type {
	case models.ComponentPrometheus:
		h.db.Model(&models.ComponentConfig{}).Where("component_id = ? AND name = ?", component.ID, "prometheus.yml").Update("is_active", false)
		err = h.autoConfig.SyncPrometheusConfig(component.ClusterID, h.basePath)
	case models.ComponentAlertmanager:
		h.db.Model(&models.ComponentConfig{}).Where("component_id = ? AND name = ?", component.ID, "alertmanager.yml").Update("is_active", false)
		err = h.autoConfig.SyncAlertmanagerConfig(component.ClusterID, h.basePath)
	case models.ComponentBlackbox, models.ComponentGrafana, models.ComponentLoki, models.ComponentPromtail:
		content := deployer.BestPracticeConfig(&component)
		if content != "" {
			err = binaryDeployer.UploadConfig(ctx, &component, content)
		}
	}
	if err == nil {
		err = binaryDeployer.ApplyUnit(ctx, &component)
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "应用推荐配置失败: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "推荐配置已应用并通过 systemd 与健康验证", "component": component})
}

func (h *Handler) ResetGrafanaAdminPassword(c *gin.Context) {
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}
	var component models.Component
	if err := h.db.Preload("Host").First(&component, id).Error; err != nil {
		utils.HandleNotFoundError(c, "component")
		return
	}
	if component.Type != models.ComponentGrafana || component.DeployType != "binary" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "仅支持重置平台 binary 部署的 Grafana"})
		return
	}
	var request struct {
		Password string `json:"password"`
	}
	_ = c.ShouldBindJSON(&request)
	password := request.Password
	if password == "" {
		password = randomGrafanaPassword()
	}
	if len(password) < 12 || strings.ContainsAny(password, "\r\n\x00") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Grafana 新密码至少 12 位且不能包含控制字符"})
		return
	}
	installPath := component.InstallPath
	configPath := component.ConfigPath
	if configPath == "" {
		configPath = "/etc/grafana/grafana.ini"
	}
	dataPath := component.DataPath
	if dataPath == "" {
		dataPath = component.DataPathOverride
	}
	if dataPath == "" {
		dataPath = "/var/lib/promlifecycle/grafana"
	}
	binaryPath := filepath.Join(installPath, "bin", "grafana")
	serviceName := component.ServiceName
	if serviceName == "" {
		serviceName = "promlifecycle-grafana"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cliArgs := []string{"cli", "--homepath", installPath, "--config", configPath, "--configOverrides", "cfg:default.paths.data=" + dataPath, "admin", "reset-admin-password", password}
	if ssh.IsLocalHost(component.Host.IP, component.Host.ConnectionType) {
		if output, err := exec.CommandContext(ctx, "systemctl", "stop", serviceName).CombinedOutput(); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("停止 Grafana 失败: %s", strings.TrimSpace(string(output)))})
			return
		}
		runuserArgs := append([]string{"-u", "nobody", "--", binaryPath}, cliArgs...)
		command := exec.CommandContext(ctx, "runuser", runuserArgs...)
		command.Dir = "/tmp"
		output, err := command.CombinedOutput()
		if err != nil {
			_ = exec.CommandContext(ctx, "systemctl", "start", serviceName).Run()
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Grafana 密码重置失败: %s", strings.TrimSpace(string(output)))})
			return
		}
	} else {
		client, err := ssh.NewClient(component.Host.IP, component.Host.Port, component.Host.Username, component.Host.Password, component.Host.PrivateKey)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "连接 Grafana 主机失败: " + err.Error()})
			return
		}
		defer client.Close()
		if _, _, err := client.RunCommand("systemctl stop " + utils.QuoteShell(serviceName)); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "停止远程 Grafana 失败: " + err.Error()})
			return
		}
		command := "cd /tmp && runuser -u nobody -- " + utils.QuoteShell(binaryPath)
		for _, argument := range cliArgs {
			command += " " + utils.QuoteShell(argument)
		}
		if output, _, err := client.RunCommand(command); err != nil {
			_, _, _ = client.RunCommand("systemctl start " + utils.QuoteShell(serviceName))
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Grafana 密码重置失败: " + strings.TrimSpace(output)})
			return
		}
	}
	environment := map[string]string{}
	_ = json.Unmarshal([]byte(component.CustomEnv), &environment)
	environment["GF_SECURITY_ADMIN_USER"] = "admin"
	environment["GF_SECURITY_ADMIN_PASSWORD"] = password
	encoded, _ := json.Marshal(environment)
	component.CustomEnv = string(encoded)
	_ = h.db.Model(&component).Update("custom_env", component.CustomEnv).Error
	binaryDeployer := deployer.NewBinaryDeployer(h.db, h.basePath)
	if err := binaryDeployer.ApplyUnit(ctx, &component); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "密码已写入实际 Grafana 数据库，但启动或环境文件更新失败: " + err.Error()})
		return
	}
	endpoint := componentHTTPURL(component, 3000) + "/login"
	verified := false
	var lastStatus int
	loginPayload, _ := json.Marshal(gin.H{"user": "admin", "password": password})
	for attempt := 0; attempt < 20; attempt++ {
		request, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(loginPayload))
		request.Header.Set("Content-Type", "application/json")
		response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
		if err == nil {
			lastStatus = response.StatusCode
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				verified = true
				break
			}
		}
		select {
		case <-ctx.Done():
			break
		case <-time.After(time.Second):
		}
	}
	if !verified {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Grafana CLI 已执行，但新密码登录验证失败（HTTP %d）；未返回成功凭据", lastStatus)})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Grafana 管理员密码已重置并通过实际登录验证", "username": "admin", "password": password, "verified": true})
}

func randomGrafanaPassword() string {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789!@#_-"
	buffer := make([]byte, 18)
	_, _ = rand.Read(buffer)
	result := "Gf9!"
	for _, value := range buffer {
		result += string(alphabet[int(value)%len(alphabet)])
	}
	return result
}

func bestPracticeMarkers(componentType models.ComponentType) []string {
	return map[models.ComponentType][]string{
		models.ComponentPrometheus:   {"scrape_interval: 15s", "scrape_timeout: 10s", "rule_files:"},
		models.ComponentAlertmanager: {"resolve_timeout: 5m", "group_wait: 30s", "group_interval: 5m", "repeat_interval: 4h"},
		models.ComponentBlackbox:     {"http_2xx:", "tcp_connect:", "icmp:", "dns:"},
		models.ComponentGrafana:      {"default_language = zh-Hans", "default_timezone = browser", "[unified_alerting]"},
		models.ComponentLoki:         {"schema: v13", "retention_period: 720h"},
		models.ComponentPromtail:     {"positions:", "systemd-journal"},
	}[componentType]
}

func bestPracticeSummary(componentType models.ComponentType) string {
	return map[models.ComponentType]string{
		models.ComponentPrometheus:   "15s 采集、10s 超时、30 天保留、WAL 压缩、规则与 Alertmanager 联调",
		models.ComponentAlertmanager: "5m resolve、30s group wait、5m group interval、4h repeat",
		models.ComponentBlackbox:     "HTTP/TCP/ICMP/DNS 模块与 CAP_NET_RAW",
		models.ComponentGrafana:      "简体中文、浏览器时区、Unified Alerting、Prometheus 数据源",
		models.ComponentNodeExporter: "标准采集器、最小资源限制、systemd enabled+active",
	}[componentType]
}
