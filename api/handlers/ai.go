package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/core/deployer"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"github.com/promlifecycle/platform/services"
	"gorm.io/gorm"
)

const (
	maxAIPromptBytes  = 8 << 10
	maxAIContextBytes = 24 << 10
	maxAIConfigBody   = 32 << 10
	maxAIChatBody     = 16 << 10
)

type AIHandler struct {
	db      *gorm.DB
	service *services.AIService
}

func NewAIHandler(db *gorm.DB) *AIHandler {
	return &AIHandler{db: db, service: services.NewAIService()}
}

func NewAIHandlerWithService(db *gorm.DB, service *services.AIService) *AIHandler {
	if service == nil {
		service = services.NewAIService()
	}
	return &AIHandler{db: db, service: service}
}

type aiConfigResponse struct {
	Provider         string    `json:"provider"`
	BaseURL          string    `json:"base_url"`
	Model            string    `json:"model"`
	APIKeyConfigured bool      `json:"api_key_configured"`
	SystemPrompt     string    `json:"system_prompt"`
	Timeout          int       `json:"timeout"`
	Enabled          bool      `json:"enabled"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

func (h *AIHandler) GetConfig(c *gin.Context) {
	cfg, found, err := h.loadConfig()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取 AI 配置失败"})
		return
	}
	if !found {
		cfg = defaultAIConfig()
	}
	c.JSON(http.StatusOK, configResponse(cfg))
}

type aiConfigRequest struct {
	Provider     string `json:"provider"`
	BaseURL      string `json:"base_url"`
	Model        string `json:"model"`
	APIKey       string `json:"api_key"`
	SystemPrompt string `json:"system_prompt"`
	Timeout      int    `json:"timeout"`
	Enabled      bool   `json:"enabled"`
}

func (h *AIHandler) PutConfig(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxAIConfigBody)
	var req aiConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式无效"})
		return
	}
	normalizedURL, err := services.NormalizeAIBaseURL(req.BaseURL)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	req.Provider = strings.TrimSpace(req.Provider)
	req.Model = strings.TrimSpace(req.Model)
	if req.Provider == "" || len(req.Provider) > 64 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "provider 不能为空且长度不能超过 64"})
		return
	}
	if req.Model == "" || len(req.Model) > 128 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model 不能为空且长度不能超过 128"})
		return
	}
	if req.Timeout <= 0 || req.Timeout > 300 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "timeout 必须在 1 到 300 秒之间"})
		return
	}
	if len(req.APIKey) > 16<<10 || len(req.SystemPrompt) > 16<<10 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "AI 配置字段超过大小限制"})
		return
	}

	var saved models.AIConfig
	err = h.db.Transaction(func(tx *gorm.DB) error {
		var cfg models.AIConfig
		result := tx.First(&cfg, models.AIConfigSingletonID)
		if result.Error != nil && !errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return result.Error
		}
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			cfg = models.AIConfig{ID: models.AIConfigSingletonID}
		}
		if strings.TrimSpace(req.APIKey) != "" {
			cfg.APIKey = req.APIKey
		}
		cfg.Provider = req.Provider
		cfg.BaseURL = normalizedURL
		cfg.Model = req.Model
		cfg.SystemPrompt = strings.TrimSpace(req.SystemPrompt)
		if cfg.SystemPrompt == "" {
			cfg.SystemPrompt = services.DefaultAISystemPrompt
		}
		cfg.Timeout = req.Timeout
		cfg.Enabled = req.Enabled
		if err := tx.Where("id <> ?", models.AIConfigSingletonID).Delete(&models.AIConfig{}).Error; err != nil {
			return err
		}
		if err := tx.Save(&cfg).Error; err != nil {
			return err
		}
		saved = cfg
		return nil
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存 AI 配置失败"})
		return
	}
	c.JSON(http.StatusOK, configResponse(saved))
}

type aiChatRequest struct {
	Prompt      string `json:"prompt"`
	ContextType string `json:"context_type"`
	ResourceID  *uint  `json:"resource_id"`
}

func (h *AIHandler) Test(c *gin.Context) {
	started := time.Now()
	cfg, found, err := h.loadConfig()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取 AI 配置失败"})
		return
	}
	if !found {
		h.recordInteraction("test", nil, cfg, "setup_required", started, "请仅回复：连接成功")
		writeAISetupRequired(c)
		return
	}
	result, err := h.service.Chat(c.Request.Context(), cfg, "请仅回复：连接成功", "")
	if err != nil {
		h.recordInteraction("test", nil, cfg, "failed", started, "请仅回复：连接成功")
		h.writeServiceError(c, err)
		return
	}
	cfg.Provider, cfg.Model = result.Provider, result.Model
	h.recordInteraction("test", nil, cfg, "success", started, "请仅回复：连接成功")
	c.JSON(http.StatusOK, result)
}

func (h *AIHandler) Chat(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxAIChatBody)
	var req aiChatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式无效"})
		return
	}
	req.Prompt = strings.TrimSpace(req.Prompt)
	req.ContextType = strings.TrimSpace(req.ContextType)
	if req.Prompt == "" || len(req.Prompt) > maxAIPromptBytes {
		c.JSON(http.StatusBadRequest, gin.H{"error": "prompt 不能为空且不能超过 8 KiB"})
		return
	}
	if !validAIContextType(req.ContextType) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "context_type 必须是 general、alerts、component、config、logs、incident 或 change"})
		return
	}
	if contextRequiresResource(req.ContextType) && (req.ResourceID == nil || *req.ResourceID == 0) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "该 context_type 需要有效的 resource_id"})
		return
	}

	started := time.Now()
	cfg, found, err := h.loadConfig()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取 AI 配置失败"})
		return
	}
	if !found {
		h.recordInteraction(req.ContextType, req.ResourceID, cfg, "setup_required", started, req.Prompt)
		writeAISetupRequired(c)
		return
	}
	if !cfg.Enabled {
		h.recordInteraction(req.ContextType, req.ResourceID, cfg, "disabled", started, req.Prompt)
		h.writeServiceError(c, services.ErrAIDisabled)
		return
	}
	trustedContext, evidenceRefs, err := h.buildTrustedContextWithEvidence(c.Request.Context(), req.ContextType, req.ResourceID)
	if err != nil {
		h.recordInteraction(req.ContextType, req.ResourceID, cfg, "context_error", started, req.Prompt)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "未找到上下文资源"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "加载 AI 上下文失败"})
		}
		return
	}
	result, err := h.service.Chat(c.Request.Context(), cfg, req.Prompt, trustedContext)
	if err != nil {
		h.recordInteraction(req.ContextType, req.ResourceID, cfg, "failed", started, req.Prompt)
		h.writeServiceError(c, err)
		return
	}
	result.EvidenceRefs = evidenceRefs
	cfg.Provider, cfg.Model = result.Provider, result.Model
	h.recordInteraction(req.ContextType, req.ResourceID, cfg, "success", started, req.Prompt)
	setAIProductMetadata(c, req.ContextType, len(evidenceRefs), len(evidenceRefs) > 0 && len(result.Analysis.Facts) > 0)
	c.JSON(http.StatusOK, result)
}

func (h *AIHandler) loadConfig() (models.AIConfig, bool, error) {
	var cfg models.AIConfig
	err := h.db.First(&cfg, models.AIConfigSingletonID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return models.AIConfig{}, false, nil
	}
	return cfg, err == nil, err
}

func (h *AIHandler) buildTrustedContext(contextType string, resourceID *uint) (string, error) {
	contextJSON, _, err := h.buildTrustedContextWithEvidence(context.Background(), contextType, resourceID)
	return contextJSON, err
}

func (h *AIHandler) buildTrustedContextWithEvidence(ctx context.Context, contextType string, resourceID *uint) (string, []services.AIEvidenceRef, error) {
	var value any
	refs := []services.AIEvidenceRef{}
	now := time.Now()
	ref := func(resource, evidenceType string, id uint, timestamp time.Time) {
		refs = append(refs, services.AIEvidenceRef{
			Resource: resource, Type: evidenceType,
			ID: strconv.FormatUint(uint64(id), 10), Timestamp: timestamp,
		})
	}
	switch contextType {
	case "general":
		var hosts, components, running, firing int64
		if err := h.db.Model(&models.Host{}).Count(&hosts).Error; err != nil {
			return "", nil, err
		}
		if err := h.db.Model(&models.Component{}).Where("status <> ?", "removed").Count(&components).Error; err != nil {
			return "", nil, err
		}
		if err := h.db.Model(&models.Component{}).Where("status = ?", models.StatusRunning).Count(&running).Error; err != nil {
			return "", nil, err
		}
		if err := h.db.Model(&models.AlertHistory{}).Where("status = ?", "firing").Count(&firing).Error; err != nil {
			return "", nil, err
		}
		value = map[string]any{
			"hosts": hosts, "components": components, "running_components": running,
			"firing_alerts": firing, "collected_at": now,
		}
		refs = append(refs, services.AIEvidenceRef{Resource: "platform", Type: "aggregate", ID: "current", Timestamp: now})
	case "alerts":
		var alerts []models.AlertHistory
		query := h.db.Where("status = ?", "firing").Order("created_at DESC").Limit(50)
		if resourceID != nil && *resourceID != 0 {
			query = query.Where("rule_id = ?", *resourceID)
		}
		if err := query.Find(&alerts).Error; err != nil {
			return "", nil, err
		}
		safeAlerts := make([]map[string]any, 0, len(alerts))
		for i := range alerts {
			labels := map[string]any{}
			if alerts[i].Labels != "" {
				_ = json.Unmarshal([]byte(alerts[i].Labels), &labels)
			}
			safeAlerts = append(safeAlerts, map[string]any{
				"id": alerts[i].ID, "rule_id": alerts[i].RuleID,
				"alert_name": alerts[i].AlertName, "severity": alerts[i].Severity,
				"status": alerts[i].Status, "value": alerts[i].Value, "labels": labels,
				"summary": redactConfigSecrets(alerts[i].Summary), "started_at": alerts[i].StartedAt,
			})
			ref(alerts[i].AlertName, "alert_history", alerts[i].ID, alerts[i].CreatedAt)
		}
		value = safeAlerts
	case "component":
		var component models.Component
		if err := h.db.Select("id", "host_id", "type", "name", "version", "port", "deploy_type", "status", "error", "updated_at").First(&component, *resourceID).Error; err != nil {
			return "", nil, err
		}
		value = map[string]any{
			"id": component.ID, "host_id": component.HostID, "type": component.Type,
			"name": component.Name, "version": component.Version, "port": component.Port,
			"deploy_type": component.DeployType, "status": component.Status,
			"error": redactConfigSecrets(component.Error), "updated_at": component.UpdatedAt,
		}
		ref(component.Name, "component_health", component.ID, component.UpdatedAt)
	case "config":
		var configs []models.ComponentConfig
		query := h.db.Select("id", "component_id", "name", "content_type", "content", "version", "is_active", "updated_at").Order("updated_at DESC").Limit(10)
		if resourceID != nil && *resourceID != 0 {
			query = query.Where("component_id = ?", *resourceID)
		} else {
			query = query.Where("is_active = ?", true)
		}
		if err := query.Find(&configs).Error; err != nil {
			return "", nil, err
		}
		safeConfigs := make([]map[string]any, 0, len(configs))
		for i := range configs {
			safeConfigs = append(safeConfigs, map[string]any{
				"id": configs[i].ID, "component_id": configs[i].ComponentID,
				"name": configs[i].Name, "content_type": configs[i].ContentType,
				"content": utils.SanitizeConfig("", configs[i].ContentType, configs[i].Content),
				"version": configs[i].Version, "is_active": configs[i].IsActive,
				"updated_at": configs[i].UpdatedAt,
			})
			ref(configs[i].Name, "component_config", configs[i].ID, configs[i].UpdatedAt)
		}
		value = safeConfigs
	case "logs":
		var component models.Component
		if err := h.db.Preload("Host").First(&component, *resourceID).Error; err != nil {
			return "", nil, err
		}
		logs, err := h.recentComponentLogs(ctx, &component)
		if err != nil {
			return "", nil, err
		}
		safeLogs := make([]string, 0, len(logs))
		for i := range logs {
			safeLogs = append(safeLogs, redactConfigSecrets(logs[i]))
		}
		value = map[string]any{
			"component_id": component.ID, "component": component.Name,
			"source": componentLogSource(&component), "collected_at": now, "lines": safeLogs,
		}
		ref(component.Name, "component_log", component.ID, now)
	case "incident":
		detail, err := services.NewIncidentService(h.db).Detail(ctx, *resourceID)
		if err != nil {
			return "", nil, err
		}
		value = detail
		ref(detail.ResourceName, "incident", detail.ID, detail.UpdatedAt)
		for i := range detail.Alerts {
			ref(detail.Alerts[i].AlertName, "alert_history", detail.Alerts[i].ID, detail.Alerts[i].CreatedAt)
		}
		if detail.ComponentHealth != nil {
			ref(detail.ComponentHealth.Name, "component_health", detail.ComponentHealth.ID, detail.ComponentHealth.UpdatedAt)
			if len(detail.ComponentLogs) > 0 {
				ref(detail.ComponentHealth.Name, "component_log", detail.ComponentHealth.ID, now)
			}
		}
		for i := range detail.RecentChanges {
			ref(detail.RecentChanges[i].ResourceName, "change_plan", detail.RecentChanges[i].ID, detail.RecentChanges[i].UpdatedAt)
		}
		for i := range detail.Maintenance {
			ref(detail.Maintenance[i].Name, "maintenance_window", detail.Maintenance[i].ID, detail.Maintenance[i].CreatedAt)
		}
		for i := range detail.Notifications {
			ref(detail.Notifications[i].AlertName, "alert_notification", detail.Notifications[i].ID, detail.Notifications[i].CreatedAt)
		}
	case "change":
		var change models.ChangePlan
		if err := h.db.First(&change, *resourceID).Error; err != nil {
			return "", nil, err
		}
		value = map[string]any{
			"id": change.ID, "kind": change.Kind, "resource_type": change.ResourceType,
			"resource_id": change.ResourceID, "resource_name": change.ResourceName,
			"status": change.Status, "reason": change.Reason, "before": change.Before,
			"after": change.After, "diff": change.Diff, "risk": change.Risk,
			"verification": change.Verification, "rollback_evidence": change.RollbackEvidence,
			"created_at": change.CreatedAt, "updated_at": change.UpdatedAt,
			"executed_at": change.ExecutedAt, "verified_at": change.VerifiedAt,
		}
		ref(change.ResourceName, "change_plan", change.ID, change.UpdatedAt)
	default:
		return "", nil, errors.New("unsupported AI context type")
	}
	envelope := map[string]any{"data": value, "evidence_refs": refs}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return "", nil, err
	}
	return boundedAIContext(utils.SanitizeConfigJSON(encoded)), refs, nil
}

func (h *AIHandler) recentComponentLogs(ctx context.Context, component *models.Component) ([]string, error) {
	const tailLines = 100
	switch component.DeployType {
	case "binary":
		return deployer.NewBinaryDeployer(h.db, "").GetLogs(ctx, component, tailLines)
	case "target":
		return nil, errors.New("target 类型没有可读取的 journal 或容器日志")
	default:
		target := component
		if component.ServiceName == "" {
			copy := *component
			copy.ServiceName = component.Name
			target = &copy
		}
		if target.ServiceName == "" {
			return nil, errors.New("组件未记录容器名称，无法读取 Docker 日志")
		}
		return deployer.NewDockerDeployer("").GetLogs(ctx, target, tailLines)
	}
}

func componentLogSource(component *models.Component) string {
	if component.DeployType == "binary" {
		return "journal"
	}
	return "docker"
}

func (h *AIHandler) writeServiceError(c *gin.Context, err error) {
	status := http.StatusBadGateway
	switch {
	case errors.Is(err, services.ErrAIDisabled):
		status = http.StatusServiceUnavailable
	case errors.Is(err, services.ErrAIResponseTooBig):
		status = http.StatusBadGateway
	case strings.Contains(err.Error(), "超时"):
		status = http.StatusGatewayTimeout
	}
	c.JSON(status, gin.H{"error": err.Error()})
}

func validAIContextType(value string) bool {
	switch value {
	case "general", "alerts", "component", "config", "logs", "incident", "change":
		return true
	default:
		return false
	}
}

func contextRequiresResource(value string) bool {
	switch value {
	case "component", "logs", "incident", "change":
		return true
	default:
		return false
	}
}

func writeAISetupRequired(c *gin.Context) {
	c.JSON(http.StatusServiceUnavailable, gin.H{
		"error": "AI 尚未配置，请先在设置中配置并启用 AI 提供商",
		"code":  "setup_required", "setup_required": true,
	})
}

func (h *AIHandler) recordInteraction(contextType string, resourceID *uint, cfg models.AIConfig, status string, started time.Time, prompt string) {
	hash := sha256.Sum256([]byte(prompt))
	interaction := models.AIInteraction{
		ContextType: contextType, ResourceID: resourceID,
		Provider: strings.TrimSpace(cfg.Provider), Model: strings.TrimSpace(cfg.Model),
		Status: status, DurationMS: time.Since(started).Milliseconds(),
		PromptHash: hex.EncodeToString(hash[:]),
	}
	_ = h.db.Create(&interaction).Error
}

func setAIProductMetadata(c *gin.Context, contextType string, evidenceCount int, verified bool) {
	metadata, _ := json.Marshal(map[string]any{
		"context_type": contextType, "evidence_count": evidenceCount, "verified": verified,
	})
	c.Set("product_event_metadata", string(metadata))
}

func configResponse(cfg models.AIConfig) aiConfigResponse {
	return aiConfigResponse{Provider: cfg.Provider, BaseURL: cfg.BaseURL, Model: cfg.Model, APIKeyConfigured: cfg.APIKey != "", SystemPrompt: defaultIfEmpty(cfg.SystemPrompt, services.DefaultAISystemPrompt), Timeout: cfg.Timeout, Enabled: cfg.Enabled, CreatedAt: cfg.CreatedAt, UpdatedAt: cfg.UpdatedAt}
}

func defaultAIConfig() models.AIConfig {
	return models.AIConfig{Provider: "openai-compatible", BaseURL: "https://api.openai.com/v1", SystemPrompt: services.DefaultAISystemPrompt, Timeout: 60}
}

func defaultIfEmpty(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
func redactConfigSecrets(content string) string {
	// Free-form logs and error text have no safe credential schema.
	if strings.TrimSpace(content) == "" {
		return content
	}
	return utils.RedactedSecret
}
func truncateUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return fmt.Sprintf("%s\n[上下文已截断，原始数据超过 %d 字节]", value, limit)
}

// Truncate individual strings, not encoded JSON: the provider boundary must
// still be able to parse and sanitize a context that exceeded the byte budget.
func boundedAIContext(encoded json.RawMessage) string {
	if len(encoded) <= maxAIContextBytes {
		return string(encoded)
	}
	var value any
	if json.Unmarshal(encoded, &value) != nil {
		return `{"data":"[REDACTED]"}`
	}
	for limit := maxAIContextBytes / 2; limit >= 64; limit /= 2 {
		var shrink func(any) any
		shrink = func(v any) any {
			switch item := v.(type) {
			case string:
				return truncateUTF8(item, limit)
			case map[string]any:
				for k, child := range item {
					item[k] = shrink(child)
				}
			case []any:
				for i, child := range item {
					item[i] = shrink(child)
				}
			}
			return v
		}
		value = shrink(value)
		data, err := json.Marshal(value)
		if err != nil {
			return `{"data":"[REDACTED]"}`
		}
		if len(data) <= maxAIContextBytes {
			return string(data)
		}
	}
	return `{"data":"[REDACTED]","truncated":"上下文已截断"}`
}
