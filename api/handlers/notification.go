package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/core/config"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"github.com/promlifecycle/platform/services"
	"gorm.io/gorm"
)

type NotificationHandler struct {
	db         *gorm.DB
	autoConfig *config.AutoConfig
	basePath   string
}

type notificationChannelResponse struct {
	ID                    uint      `json:"id"`
	Name                  string    `json:"name"`
	Type                  string    `json:"type"`
	Config                string    `json:"config"`
	Enabled               bool      `json:"enabled"`
	ApplyStatus           string    `json:"apply_status"`
	LastError             string    `json:"last_error,omitempty"`
	Warning               string    `json:"warning,omitempty"`
	InspectionSupported   bool      `json:"inspection_supported"`
	AlertmanagerSupported bool      `json:"alertmanager_supported"`
	CapabilityError       string    `json:"capability_error,omitempty"`
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`
}

func channelResponse(ch models.NotificationChannel) notificationChannelResponse {
	configJSON := utils.SanitizeConfig("notification", "json", ch.Config)
	if ch.Type == "email" {
		var email struct {
			To string `json:"to"`
		}
		_ = json.Unmarshal([]byte(ch.Config), &email)
		if encoded, err := json.Marshal(email); err == nil {
			configJSON = string(encoded)
		}
	}
	response := notificationChannelResponse{
		ID: ch.ID, Name: ch.Name, Type: ch.Type, Config: configJSON, Enabled: ch.Enabled,
		ApplyStatus: ch.ApplyStatus, LastError: ch.LastError,
		CreatedAt: ch.CreatedAt, UpdatedAt: ch.UpdatedAt,
		AlertmanagerSupported: ch.Type != "dingtalk",
	}
	if err := config.ValidateInspectionChannel(ch); err != nil {
		response.CapabilityError = err.Error()
	} else {
		response.InspectionSupported = true
	}
	return response
}

// Status exposes transport capabilities and acknowledgement state without credentials or payloads.
func (h *NotificationHandler) Status(c *gin.Context) {
	var channels []models.NotificationChannel
	if err := h.db.Order("id ASC").Find(&channels).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取通知能力失败"})
		return
	}
	capabilities := make([]gin.H, 0, len(channels))
	for _, channel := range channels {
		view := channelResponse(channel)
		capabilities = append(capabilities, gin.H{"channel_id": channel.ID, "name": channel.Name, "type": channel.Type, "enabled": channel.Enabled, "inspection_supported": view.InspectionSupported, "alertmanager_supported": view.AlertmanagerSupported, "capability_error": view.CapabilityError, "apply_status": channel.ApplyStatus})
	}
	var rows []models.InspectionDelivery
	if err := h.db.Order("id DESC").Limit(200).Find(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取通知投递状态失败"})
		return
	}
	deliveries := make([]gin.H, 0, len(rows))
	for _, row := range rows {
		deliveries = append(deliveries, gin.H{"id": row.ID, "transition_id": row.TransitionID, "channel_id": row.ChannelID, "status": row.Status, "attempts": row.Attempts, "next_attempt_at": row.NextAttemptAt, "acknowledged_at": row.AcknowledgedAt, "updated_at": row.UpdatedAt})
	}
	c.JSON(http.StatusOK, gin.H{"capabilities": capabilities, "deliveries": deliveries})
}

func validChannelName(name string) bool {
	name = strings.TrimSpace(name)
	return name != "" && len(name) <= 128 && !strings.ContainsAny(name, "\r\n\x00")
}

func validChannelType(channelType string) bool {
	switch channelType {
	case "email", "webhook", "dingtalk", "wechat", "slack":
		return true
	default:
		return false
	}
}

func NewNotificationHandler(db *gorm.DB, autoConfig *config.AutoConfig, basePath string) *NotificationHandler {
	return &NotificationHandler{db: db, autoConfig: autoConfig, basePath: basePath}
}

// syncAlertmanagerConfigs publishes the singleton Alertmanager configuration
// before a channel mutation is reported successful.
func (h *NotificationHandler) syncAlertmanagerConfigs(clusterID *uint) error {
	if h.autoConfig == nil {
		return fmt.Errorf("Alertmanager 配置服务不可用")
	}
	id := uint(0)
	if clusterID != nil {
		id = *clusterID
	}
	if id == 0 {
		var err error
		id, err = services.SingleNodeID(h.db)
		if err != nil {
			return err
		}
	}
	if _, err := h.autoConfig.FindAlertmanagerComponent(id); err != nil {
		return fmt.Errorf("未找到运行中的 Alertmanager")
	}
	return h.autoConfig.SyncAlertmanagerConfig(id, h.basePath)
}

func (h *NotificationHandler) publishChannel(ch *models.NotificationChannel) error {
	if ch.Type == "dingtalk" {
		if ch.ApplyStatus != "draft" && ch.ApplyStatus != "manual" {
			if err := h.syncAlertmanagerConfigs(ch.ClusterID); err != nil {
				ch.ApplyStatus, ch.LastError = "error", err.Error()
				if saveErr := h.db.Model(ch).Updates(map[string]interface{}{"apply_status": ch.ApplyStatus, "last_error": ch.LastError}).Error; saveErr != nil {
					return saveErr
				}
				return err
			}
		}
		ch.ApplyStatus = "manual"
		ch.LastError = "仅直接巡检通知；Alertmanager 告警需使用 webhook 适配器"
		return h.db.Model(ch).Updates(map[string]interface{}{"apply_status": ch.ApplyStatus, "last_error": ch.LastError}).Error
	}
	if !ch.Enabled && ch.ApplyStatus == "draft" {
		ch.ApplyStatus = "draft"
		ch.LastError = ""
		return h.db.Model(ch).Updates(map[string]interface{}{"apply_status": ch.ApplyStatus, "last_error": ""}).Error
	}
	ch.ApplyStatus = "pending"
	ch.LastError = ""
	if err := h.db.Model(ch).Updates(map[string]interface{}{"apply_status": ch.ApplyStatus, "last_error": ""}).Error; err != nil {
		return err
	}
	if err := h.syncAlertmanagerConfigs(ch.ClusterID); err != nil {
		ch.ApplyStatus = "error"
		ch.LastError = err.Error()
		_ = h.db.Model(ch).Updates(map[string]interface{}{"apply_status": ch.ApplyStatus, "last_error": ch.LastError}).Error
		return err
	}
	ch.ApplyStatus = "applied"
	if !ch.Enabled {
		ch.ApplyStatus = "draft"
	}
	ch.LastError = ""
	return h.db.Model(ch).Updates(map[string]interface{}{"apply_status": ch.ApplyStatus, "last_error": ""}).Error
}

// ListChannels lists notification channels
func (h *NotificationHandler) ListChannels(c *gin.Context) {
	nodeID, err := services.SingleNodeID(h.db)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取节点配置失败，请刷新页面后重试"})
		return
	}
	var channels []models.NotificationChannel
	if err := h.db.Where("cluster_id = ? OR cluster_id IS NULL", nodeID).Order("created_at DESC").Find(&channels).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取通知渠道失败，请稍后重试"})
		return
	}
	result := make([]notificationChannelResponse, 0, len(channels))
	for _, ch := range channels {
		result = append(result, channelResponse(ch))
	}
	c.JSON(http.StatusOK, gin.H{"channels": result})
}
func (h *NotificationHandler) CreateChannel(c *gin.Context) {
	var req struct {
		Name    string `json:"name"`
		Type    string `json:"type"`
		Config  string `json:"config"`
		Secret  string `json:"secret"`
		Enabled *bool  `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效请求"})
		return
	}
	nodeID, err := services.SingleNodeID(h.db)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取节点配置失败，请刷新页面后重试"})
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	ch := models.NotificationChannel{
		Name: req.Name, Type: req.Type, Config: req.Config, Secret: req.Secret,
		ClusterID: &nodeID, Enabled: enabled, ApplyStatus: "draft",
	}
	if utils.HasRedactedSecrets(ch.Config) || utils.HasRedactedSecrets(ch.Secret) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "不能保存脱敏占位符，请填写真实渠道配置"})
		return
	}
	if ch.Type == "email" {
		if _, _, err := config.ResolveEmailChannel(ch); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "操作失败: " + err.Error()})
			return
		}
	}
	if !validChannelName(ch.Name) || !validChannelType(ch.Type) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "通知渠道名称或类型无效，请修正后重试"})
		return
	}
	if err := h.db.Create(&ch).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建失败"})
		return
	}
	if err := h.publishChannel(&ch); err != nil {
		response := channelResponse(ch)
		response.Warning = "渠道已保存，但尚未发布到 Alertmanager；恢复组件后重试发布"
		c.JSON(http.StatusAccepted, response)
		return
	}
	c.JSON(http.StatusCreated, channelResponse(ch))
}

// UpdateChannel updates a channel
func (h *NotificationHandler) UpdateChannel(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "通知渠道 ID 无效，请刷新列表后重试"})
		return
	}
	var ch models.NotificationChannel
	if h.db.First(&ch, id).Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "未找到该通知渠道，请刷新列表"})
		return
	}
	var req struct {
		Name    string `json:"name"`
		Type    string `json:"type"`
		Config  string `json:"config"`
		Enabled *bool  `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "更新请求无效，请检查字段后重试"})
		return
	}
	if utils.HasRedactedSecrets(req.Config) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "不能保存脱敏占位符；保持配置不变时请省略 config，修改时请填写真实配置"})
		return
	}
	updates := map[string]interface{}{}
	if req.Name != "" {
		updates["name"] = req.Name
	}
	if req.Type != "" {
		updates["type"] = req.Type
	}
	if req.Config != "" {
		updates["config"] = req.Config
	}
	if req.Enabled != nil {
		updates["enabled"] = *req.Enabled
	}
	if len(updates) > 0 {
		candidate := ch
		if req.Type != "" {
			candidate.Type = req.Type
		}
		if req.Config != "" {
			candidate.Config = req.Config
		}
		if req.Name != "" {
			candidate.Name = req.Name
		}
		if !validChannelName(candidate.Name) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "通知渠道名称无效，请修正后重试"})
			return
		}
		if !validChannelType(candidate.Type) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "通知渠道类型无效，请选择受支持的类型"})
			return
		}
		if candidate.Type == "email" {
			if _, _, err := config.ResolveEmailChannel(candidate); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "操作失败: " + err.Error()})
				return
			}
		}
		if err := h.db.Model(&ch).Updates(updates).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "更新失败"})
			return
		}
	}
	if err := h.db.First(&ch, id).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "重新读取通知渠道失败"})
		return
	}
	if err := h.publishChannel(&ch); err != nil {
		response := channelResponse(ch)
		response.Warning = "设置已保存，但尚未发布到 Alertmanager；恢复组件后重试发布"
		c.JSON(http.StatusAccepted, response)
		return
	}
	c.JSON(http.StatusOK, channelResponse(ch))
}

// DeleteChannel deletes a channel
func (h *NotificationHandler) DeleteChannel(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "通知渠道 ID 无效，请刷新列表后重试"})
		return
	}
	var ch models.NotificationChannel
	blockedReason := ""
	err = h.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&ch, id).Error; err != nil {
			return err
		}
		var pending int64
		if err := tx.Model(&models.InspectionDelivery{}).Where("channel_id = ? AND status <> ?", ch.ID, "sent").Count(&pending).Error; err != nil {
			return err
		}
		if pending > 0 {
			blockedReason = "渠道仍有未确认巡检通知，请先恢复投递；不能丢失重试记录"
			return gorm.ErrInvalidData
		}
		var policies []models.InspectionNotificationPolicy
		if err := tx.Where("enabled = ?", true).Find(&policies).Error; err != nil {
			return err
		}
		for _, policy := range policies {
			for _, channelID := range policy.ChannelIDs {
				if channelID == ch.ID {
					blockedReason = "请先从启用的巡检通知策略中移除该渠道"
					return gorm.ErrInvalidData
				}
			}
		}
		return tx.Delete(&ch).Error
	})
	if err == gorm.ErrRecordNotFound {
		c.JSON(http.StatusNotFound, gin.H{"error": "未找到该通知渠道，请刷新列表"})
		return
	}
	if blockedReason != "" {
		c.JSON(http.StatusConflict, gin.H{"error": blockedReason})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "删除通知渠道失败，请稍后重试"})
		return
	}
	if (!ch.Enabled && ch.ApplyStatus == "draft") || (ch.Type == "dingtalk" && ch.ApplyStatus == "manual") {
		c.JSON(http.StatusOK, gin.H{"message": "通知渠道已删除"})
		return
	}
	if err := h.syncAlertmanagerConfigs(ch.ClusterID); err != nil {
		if restoreErr := h.db.Create(&ch).Error; restoreErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "通知渠道发布失败且恢复记录失败: " + restoreErr.Error()})
			return
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": "通知渠道删除已撤销；Alertmanager 恢复后重试"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "通知渠道已删除"})
}

// TestChannel publishes the current singleton configuration and sends a real Alertmanager test alert.
func (h *NotificationHandler) TestChannel(c *gin.Context) {
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}
	var ch models.NotificationChannel
	if h.db.First(&ch, id).Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "不存在"})
		return
	}

	var req struct {
		Message string `json:"message"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "测试请求无效，请检查字段后重试"})
		return
	}
	if !ch.Enabled {
		c.JSON(http.StatusBadRequest, gin.H{"error": "通知渠道已禁用，请启用后再测试"})
		return
	}
	if ch.Type == "dingtalk" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "钉钉机器人仅支持直接巡检通知；Alertmanager 测试请使用 webhook 适配器渠道"})
		return
	}
	msg := req.Message
	if msg == "" {
		msg = fmt.Sprintf("【测试消息】Prometheus 监控平台通知通道测试 - %s", time.Now().Format("15:04:05"))
	}
	clusterID, err := services.SingleNodeID(h.db)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取节点配置失败，请刷新页面后重试"})
		return
	}
	if h.autoConfig == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Alertmanager 配置服务不可用，请稍后重试"})
		return
	}
	if err := h.autoConfig.SyncAlertmanagerConfig(clusterID, h.basePath); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "发布 Alertmanager 配置失败，请检查渠道配置和组件状态后重试"})
		return
	}
	status, errStr := h.sendTestAlertViaAlertmanager(clusterID, ch.Name, msg)

	h.db.Create(&models.AlertNotification{
		AlertName:   "test_alert",
		Severity:    "info",
		ChannelID:   ch.ID,
		ChannelType: ch.Type,
		Message:     msg,
		Status:      status,
		Error:       errStr,
	})

	if status == "sent" {
		c.JSON(http.StatusOK, gin.H{"message": "发送成功"})
	} else {
		// Distinguish client-side config errors (400) from upstream
		// delivery failures (502 Bad Gateway). Both are expected outcomes
		// of a "test" action — 500 would imply a server bug.
		code := http.StatusBadGateway
		if strings.Contains(errStr, "为空") || strings.Contains(errStr, "不支持") ||
			strings.Contains(errStr, "不是有效 JSON") || strings.Contains(errStr, "未配置 SMTP") ||
			strings.Contains(errStr, "收件人") || strings.Contains(errStr, "未找到可用的 Alertmanager") ||
			strings.Contains(errStr, "拒绝访问") {
			code = http.StatusBadRequest
		}
		c.JSON(code, gin.H{"error": "发送失败: " + errStr})
	}
}

// Alert represents a single alert to be sent to Alertmanager
type Alert struct {
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations,omitempty"`
	StartsAt    time.Time         `json:"startsAt"`
	EndsAt      time.Time         `json:"endsAt,omitempty"`
}

// sendTestAlertViaAlertmanager sends a test alert to Alertmanager via POST /api/v1/alerts
func (h *NotificationHandler) sendTestAlertViaAlertmanager(clusterID uint, channelName string, msg string) (string, string) {
	var amComp models.Component
	if err := h.db.Where("cluster_id = ? AND type = ? AND status = ?", clusterID, models.ComponentAlertmanager, models.StatusRunning).
		Preload("Host").First(&amComp).Error; err != nil {
		return "failed", "未找到可用的 Alertmanager，请先启动组件后重试"
	}
	port := amComp.Port
	if port == 0 {
		port = 9093
	}
	endpoint := ""
	if amComp.ExternalURL != "" {
		endpoint = strings.TrimSuffix(amComp.ExternalURL, "/")
	} else if amComp.InternalURL != "" {
		endpoint = strings.TrimSuffix(amComp.InternalURL, "/")
	} else {
		endpoint = fmt.Sprintf("http://%s:%d", amComp.Host.IP, port)
	}
	endpoint += "/api/v2/alerts"

	// Create alert payload
	alert := Alert{
		Labels: map[string]string{
			"alertname": "TestAlert",
			"channel":   channelName,
			"severity":  "info",
		},
		Annotations: map[string]string{
			"message":  msg,
			"test":     "true",
			"platform": "promlifecycle",
		},
		StartsAt: time.Now(),
	}

	payload, err := json.Marshal([]Alert{alert})
	if err != nil {
		return "failed", "生成测试告警失败，请稍后重试"
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(endpoint, "application/json", bytes.NewReader(payload))
	if err != nil {
		return "failed", "Alertmanager 无法访问，请检查组件状态和网络后重试"
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "failed", fmt.Sprintf("Alertmanager 返回 HTTP %d，请检查组件日志后重试", resp.StatusCode)
	}

	return "sent", ""
}

// ListAlertNotifications lists sent notifications
func (h *NotificationHandler) ListAlertNotifications(c *gin.Context) {
	var notifications []models.AlertNotification
	var total int64
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > 200 {
		limit = 50
	}
	offset := (page - 1) * limit
	h.db.Model(&models.AlertNotification{}).Count(&total)
	h.db.Order("created_at DESC").Offset(offset).Limit(limit).Find(&notifications)
	c.JSON(http.StatusOK, gin.H{"notifications": notifications, "total": total})
}

// ResolveDelivery requires an operator decision; uncertain sends are never retried automatically.
func (h *NotificationHandler) ResolveDelivery(c *gin.Context) {
	if c.GetString("role") != string(models.RoleSystemAdmin) {
		c.JSON(http.StatusForbidden, gin.H{"error": "仅系统管理员可确认不确定的通知投递"})
		return
	}
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}
	var input struct {
		Resolution string `json:"resolution"`
		Reason     string `json:"reason"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10)
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "通知处理请求无效"})
		return
	}
	input.Reason = strings.TrimSpace(input.Reason)
	if (input.Resolution != "confirmed_sent" && input.Resolution != "retry") || input.Reason == "" || len(input.Reason) > 2000 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请选择 confirmed_sent 或 retry，并填写不超过2000字节的处理原因"})
		return
	}
	var delivery models.InspectionDelivery
	now := time.Now()
	status := "sent"
	warning := ""
	updates := map[string]interface{}{"last_error": "", "acknowledged_at": now}
	if input.Resolution == "retry" {
		status = "pending"
		updates["acknowledged_at"] = nil
		updates["next_attempt_at"] = now
		warning = "原投递结果不确定；再次投递可能重复。已仅重排所选通知，尚未发送。"
	}
	updates["status"] = status
	err := h.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&delivery, id).Error; err != nil {
			return err
		}
		result := tx.Model(&models.InspectionDelivery{}).Where("id = ? AND status = ?", id, "unknown").Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return gorm.ErrInvalidData
		}
		resolution := models.InspectionDeliveryResolution{DeliveryID: id, Resolution: input.Resolution, Reason: input.Reason, UserID: c.GetUint("user_id"), Username: c.GetString("username")}
		if err := tx.Create(&resolution).Error; err != nil {
			return err
		}
		return tx.First(&delivery, id).Error
	})
	if err == gorm.ErrRecordNotFound {
		c.JSON(http.StatusNotFound, gin.H{"error": "通知投递不存在"})
		return
	}
	if err == gorm.ErrInvalidData {
		c.JSON(http.StatusConflict, gin.H{"error": "仅 unknown 投递可处理；该通知状态已改变，请刷新"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存通知处理决定失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"delivery": gin.H{"id": delivery.ID, "transition_id": delivery.TransitionID, "channel_id": delivery.ChannelID, "status": delivery.Status, "attempts": delivery.Attempts, "next_attempt_at": delivery.NextAttemptAt, "acknowledged_at": delivery.AcknowledgedAt, "updated_at": delivery.UpdatedAt}, "warning": warning})
}
