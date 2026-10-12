package handlers

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/core/ssh"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"github.com/promlifecycle/platform/services"
)

func (h *Handler) ListComponentConfigs(c *gin.Context) {
	raw, allowed := configRawRead(c)
	if !allowed {
		return
	}
	compID, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}

	var configs []models.ComponentConfig
	h.db.Where("component_id = ?", compID).Order("version desc").Find(&configs)
	if !raw {
		for i := range configs {
			configs[i] = publicComponentConfig(configs[i])
		}
	}
	c.JSON(http.StatusOK, gin.H{"configs": configs})
}

func (h *Handler) GetComponentConfig(c *gin.Context) {
	raw, allowed := configRawRead(c)
	if !allowed {
		return
	}
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}

	var cfg models.ComponentConfig
	if err := h.db.First(&cfg, id).Error; err != nil {
		utils.HandleNotFoundError(c, "config")
		return
	}

	if !raw {
		cfg = publicComponentConfig(cfg)
	}
	c.JSON(http.StatusOK, cfg)
}

// GetCurrentComponentConfig returns the effective config file content for a
// component by reading the actual file on disk (locally, or over SSH for
// remote binary hosts). It falls back to the platform's managed config
// directory and is meant for the config editor when no version history has
// been saved through the platform yet.
func (h *Handler) GetCurrentComponentConfig(c *gin.Context) {
	raw, allowed := configRawRead(c)
	if !allowed {
		return
	}
	c.Header("Cache-Control", "no-store")
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}

	var component models.Component
	if err := h.db.Preload("Host").Preload("Cluster").First(&component, id).Error; err != nil {
		utils.HandleNotFoundError(c, "component")
		return
	}

	configNames := map[models.ComponentType]string{models.ComponentPrometheus: "prometheus.yml", models.ComponentAlertmanager: "alertmanager.yml", models.ComponentBlackbox: "blackbox.yml", models.ComponentGrafana: "grafana.ini", models.ComponentLoki: "config.yaml", models.ComponentPromtail: "config.yaml"}
	name := configNames[component.Type]
	if name == "" {
		name = string(component.Type) + ".yml"
	}
	contentType := "yaml"
	if component.Type == models.ComponentGrafana {
		contentType = "ini"
	}
	readFromPath := func(path string) ([]byte, error) {
		if ssh.IsLocalHost(component.Host.IP, component.Host.ConnectionType) {
			return os.ReadFile(path)
		}
		client, err := ssh.NewClient(component.Host.IP, component.Host.Port, component.Host.Username,
			component.Host.Password, component.Host.PrivateKey)
		if err != nil {
			return nil, err
		}
		defer client.Close()
		out, _, err := client.RunCommand("cat " + utils.QuoteShell(path))
		if err != nil {
			return nil, err
		}
		return []byte(out), nil
	}

	// 1. The component's own config path (binary: /etc/prometheus/prometheus.yml;
	// docker: mounted config directory).
	if component.ConfigPath != "" {
		path := component.ConfigPath
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			path = filepath.Join(path, name)
		}
		if data, err := readFromPath(path); err == nil && len(data) > 0 {
			c.JSON(http.StatusOK, gin.H{
				"name":         name,
				"content":      configResponseContent(raw, string(component.Type), contentType, string(data)),
				"content_type": contentType,
				"source":       path,
			})
			return
		}
	}

	// 2. The platform's managed config directory.
	if component.Cluster.Name != "" {
		if data, err := h.configManager.LoadConfig(component.Cluster.Name, component.Name, name); err == nil && len(data) > 0 {
			c.JSON(http.StatusOK, gin.H{
				"name":         name,
				"content":      configResponseContent(raw, string(component.Type), contentType, string(data)),
				"content_type": contentType,
				"source":       "managed",
			})
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{"name": name, "content": "", "content_type": contentType, "source": ""})
}

func (h *Handler) UpdateComponentConfig(c *gin.Context) {
	compID, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}
	var req struct {
		Name        string `json:"name" binding:"required"`
		Content     string `json:"content" binding:"required"`
		ContentType string `json:"content_type"`
		Description string `json:"description"`
		Reason      string `json:"reason" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "配置变更必须包含 name、content 和 reason: " + err.Error()})
		return
	}
	if utils.HasRedactedSecrets(req.Content) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "不能保存脱敏预览；请通过授权的 raw=1 配置读取后编辑"})
		return
	}
	after, _ := json.Marshal(gin.H{
		"name": req.Name, "content": req.Content, "content_type": req.ContentType, "description": req.Description,
	})
	svc := h.changeService()
	plan, err := svc.Create(c.Request.Context(), services.ChangePlanRequest{
		Kind: models.ChangeKindConfig, ResourceType: "component", ResourceID: compID,
		Reason: req.Reason, After: after,
	}, changeActor(c))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	plan, err = svc.Execute(c.Request.Context(), plan.ID, changeActor(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error(), "plan": publicChangePlan(plan)})
		return
	}
	var cfg models.ComponentConfig
	h.db.Where("component_id = ? AND name = ? AND is_active = ?", compID, req.Name, true).
		Order("version DESC").First(&cfg)
	c.JSON(http.StatusOK, gin.H{"config": publicComponentConfig(cfg), "plan": publicChangePlan(plan), "message": "配置变更已执行并验证"})
}

func (h *Handler) RollbackConfig(c *gin.Context) {
	compID, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}
	var req struct {
		ConfigName string `json:"config_name" binding:"required"`
		Version    int    `json:"version" binding:"required"`
		Reason     string `json:"reason" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "配置回滚必须包含 config_name、version 和 reason: " + err.Error()})
		return
	}
	var cfg models.ComponentConfig
	if err := h.db.Where("component_id = ? AND name = ? AND version = ?", compID, req.ConfigName, req.Version).First(&cfg).Error; err != nil {
		utils.HandleNotFoundError(c, "config version")
		return
	}
	after, _ := json.Marshal(gin.H{
		"name": cfg.Name, "content": cfg.Content, "content_type": cfg.ContentType,
		"description": "Rollback to version " + req.ConfigName,
	})
	svc := h.changeService()
	plan, err := svc.Create(c.Request.Context(), services.ChangePlanRequest{
		Kind: models.ChangeKindConfig, ResourceType: "component", ResourceID: compID,
		Reason: req.Reason, After: after,
	}, changeActor(c))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	plan, err = svc.Execute(c.Request.Context(), plan.ID, changeActor(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error(), "plan": publicChangePlan(plan)})
		return
	}
	var active models.ComponentConfig
	if err := h.db.Where("component_id = ? AND name = ? AND is_active = ?", compID, req.ConfigName, true).
		Order("version DESC").First(&active).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "配置已回滚，但读取新活动版本失败", "plan": publicChangePlan(plan)})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"message": "配置已回滚并验证", "version": active.Version,
		"source_version": req.Version, "config": publicComponentConfig(active), "plan": publicChangePlan(plan),
	})
}

// ========== HELPER ==========

// Raw reads are explicit, authorized, and never cached. Preview responses must
// not be used as mutation input because redaction can hide unknown extensions.
func configRawRead(c *gin.Context) (bool, bool) {
	c.Header("Cache-Control", "no-store")
	raw := c.Query("raw") == "1"
	if raw && !utils.CanReadConfigSecrets(c.GetString("role")) {
		c.JSON(http.StatusForbidden, gin.H{"error": "仅变更管理员或系统管理员可读取原始配置"})
		return false, false
	}
	return raw, true
}

func configResponseContent(raw bool, kind, format, content string) string {
	if raw {
		return content
	}
	return utils.SanitizeConfig(kind, format, content)
}

func publicComponentConfig(cfg models.ComponentConfig) models.ComponentConfig {
	cfg.Content = utils.SanitizeConfig("", cfg.ContentType, cfg.Content)
	return cfg
}
