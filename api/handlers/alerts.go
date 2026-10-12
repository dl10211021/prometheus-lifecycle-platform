package handlers

import (
	"log"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/services"
	"gorm.io/gorm"
)

type AlertsHandler struct {
	db        *gorm.DB
	alertSync *services.AlertSyncService
}

func NewAlertsHandler(db *gorm.DB, alertSync *services.AlertSyncService) *AlertsHandler {
	return &AlertsHandler{db: db, alertSync: alertSync}
}

// ListAlertRules lists alert rules
func (h *AlertsHandler) ListAlertRules(c *gin.Context) {
	var rules []models.AlertRule
	clusterID := c.Query("cluster_id")

	query := h.db.Order("created_at DESC")
	if clusterID != "" {
		if id, err := strconv.Atoi(clusterID); err == nil {
			query = query.Where("cluster_id = ?", id)
		}
	}

	query.Find(&rules)
	c.JSON(http.StatusOK, gin.H{"rules": rules})
}

// CreateAlertRule creates a new alert rule
func (h *AlertsHandler) CreateAlertRule(c *gin.Context) {
	rule := models.AlertRule{Enabled: true}

	if err := c.ShouldBindJSON(&rule); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效请求参数"})
		return
	}

	if rule.Name == "" || rule.Expression == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "规则名称和表达式不能为空"})
		return
	}

	if rule.Duration == "" {
		rule.Duration = "5m"
	}
	if rule.Severity == "" {
		rule.Severity = "warning"
	}
	if rule.Group == "" {
		rule.Group = "default"
	}

	if err := createMonitoring(h.db, &rule); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建规则失败: " + err.Error()})
		return
	}

	// Trigger alert rules sync
	if rule.ClusterID > 0 {
		go func(clusterID uint) {
			if err := h.alertSync.SyncAlertRules(clusterID); err != nil {
				log.Printf("[Alerts] Failed to sync rules after create: %v", err)
			}
		}(rule.ClusterID)
	}

	c.JSON(http.StatusAccepted, rule)
}

// UpdateAlertRule updates an alert rule
func (h *AlertsHandler) UpdateAlertRule(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效ID"})
		return
	}

	var rule models.AlertRule
	if err := h.db.First(&rule, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "规则不存在"})
		return
	}

	var req struct {
		Name        *string `json:"name"`
		Expression  *string `json:"expression"`
		Duration    *string `json:"duration"`
		Severity    *string `json:"severity"`
		Labels      *string `json:"labels"`
		Annotations *string `json:"annotations"`
		Enabled     *bool   `json:"enabled"`
		Group       *string `json:"group"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效请求参数"})
		return
	}

	updates := map[string]interface{}{}
	if req.Name != nil {
		updates["name"] = *req.Name
	}
	if req.Expression != nil {
		updates["expression"] = *req.Expression
	}
	if req.Duration != nil {
		updates["duration"] = *req.Duration
	}
	if req.Severity != nil {
		updates["severity"] = *req.Severity
	}
	if req.Labels != nil {
		updates["labels"] = *req.Labels
	}
	if req.Annotations != nil {
		updates["annotations"] = *req.Annotations
	}
	if req.Enabled != nil {
		updates["enabled"] = *req.Enabled
	}
	if req.Group != nil {
		updates["group"] = *req.Group
	}
	if len(updates) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "没有可更新的字段"})
		return
	}

	if err := updateMonitoring(h.db, &rule, updates); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "更新规则失败"})
		return
	}

	h.db.First(&rule, id)

	// Trigger alert rules sync
	if rule.ClusterID > 0 {
		go func(clusterID uint) {
			if err := h.alertSync.SyncAlertRules(clusterID); err != nil {
				log.Printf("[Alerts] Failed to sync rules after update: %v", err)
			}
		}(rule.ClusterID)
	}

	c.JSON(http.StatusAccepted, rule)
}

// DeleteAlertRule deletes an alert rule
func (h *AlertsHandler) DeleteAlertRule(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效ID"})
		return
	}

	var rule models.AlertRule
	if err := h.db.First(&rule, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "规则不存在"})
		return
	}

	clusterID := rule.ClusterID

	if err := deleteMonitoring(h.db, &rule); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "删除规则失败"})
		return
	}

	// Trigger alert rules sync
	if clusterID > 0 {
		go func(cid uint) {
			if err := h.alertSync.SyncAlertRules(cid); err != nil {
				log.Printf("[Alerts] Failed to sync rules after delete: %v", err)
			}
		}(clusterID)
	}

	c.JSON(http.StatusAccepted, gin.H{"message": "删除已排队", "apply_status": "pending", "delete_pending": true})
}

// ListAlertHistory lists alert history
func (h *AlertsHandler) ListAlertHistory(c *gin.Context) {
	var alerts []models.AlertHistory
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

	clusterID := c.Query("cluster_id")
	status := c.Query("status")
	severity := c.Query("severity")

	query := h.db.Model(&models.AlertHistory{}).Order("started_at DESC")

	if clusterID != "" {
		if id, err := strconv.Atoi(clusterID); err == nil {
			query = query.Where("cluster_id = ?", id)
		}
	}
	if status != "" {
		query = query.Where("status = ?", status)
	}
	if severity != "" {
		query = query.Where("severity = ?", severity)
	}

	query.Count(&total)
	query.Offset(offset).Limit(limit).Find(&alerts)

	c.JSON(http.StatusOK, gin.H{
		"alerts": alerts,
		"total":  total,
		"page":   page,
		"limit":  limit,
	})
}

// GetActiveAlerts returns currently firing alerts
func (h *AlertsHandler) GetActiveAlerts(c *gin.Context) {
	var alerts []models.AlertHistory
	h.db.Where("status = ?", "firing").Order("started_at DESC").Find(&alerts)
	c.JSON(http.StatusOK, gin.H{"alerts": alerts, "count": len(alerts)})
}

// GetAlertRule returns a single alert rule by ID.
func (h *AlertsHandler) GetAlertRule(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效ID"})
		return
	}
	var rule models.AlertRule
	if err := h.db.First(&rule, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "规则不存在"})
		return
	}
	c.JSON(http.StatusOK, rule)
}
