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

// RecordingRulesHandler handles CRUD operations for Prometheus recording rules.
type RecordingRulesHandler struct {
	db        *gorm.DB
	alertSync *services.AlertSyncService
}

// NewRecordingRulesHandler creates a new RecordingRulesHandler instance.
func NewRecordingRulesHandler(db *gorm.DB, alertSync *services.AlertSyncService) *RecordingRulesHandler {
	return &RecordingRulesHandler{db: db, alertSync: alertSync}
}

// ListRecordingRules lists recording rules, optionally filtered by cluster_id.
func (h *RecordingRulesHandler) ListRecordingRules(c *gin.Context) {
	var rules []models.RecordingRule
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

// GetRecordingRule returns a single recording rule by ID.
func (h *RecordingRulesHandler) GetRecordingRule(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效ID"})
		return
	}

	var rule models.RecordingRule
	if err := h.db.First(&rule, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "记录规则不存在"})
		return
	}

	c.JSON(http.StatusOK, rule)
}

// CreateRecordingRule creates a new recording rule and triggers rules sync.
func (h *RecordingRulesHandler) CreateRecordingRule(c *gin.Context) {
	rule := models.RecordingRule{Enabled: true}

	if err := c.ShouldBindJSON(&rule); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效请求参数"})
		return
	}

	if rule.Name == "" || rule.Expression == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "名称和表达式不能为空"})
		return
	}

	if rule.Group == "" {
		rule.Group = "recording_rules"
	}

	if err := createMonitoring(h.db, &rule); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建记录规则失败: " + err.Error()})
		return
	}

	// Trigger rules sync (includes both alert rules and recording rules)
	go func(clusterID uint) {
		if err := h.alertSync.SyncAlertRules(clusterID); err != nil {
			log.Printf("[RecordingRules] Failed to sync rules after create: %v", err)
		}
	}(rule.ClusterID)

	c.JSON(http.StatusAccepted, rule)
}

// UpdateRecordingRule updates an existing recording rule and triggers rules sync.
func (h *RecordingRulesHandler) UpdateRecordingRule(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效ID"})
		return
	}

	var rule models.RecordingRule
	if err := h.db.First(&rule, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "记录规则不存在"})
		return
	}

	var req struct {
		Group      *string `json:"group"`
		Name       *string `json:"name"`
		Expression *string `json:"expression"`
		Labels     *string `json:"labels"`
		Enabled    *bool   `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效请求参数"})
		return
	}

	clusterID := rule.ClusterID

	updates := map[string]interface{}{}
	if req.Group != nil {
		updates["group"] = *req.Group
	}
	if req.Name != nil {
		updates["name"] = *req.Name
	}
	if req.Expression != nil {
		updates["expression"] = *req.Expression
	}
	if req.Labels != nil {
		updates["labels"] = *req.Labels
	}
	if req.Enabled != nil {
		updates["enabled"] = *req.Enabled
	}
	if len(updates) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "没有可更新的字段"})
		return
	}

	if err := updateMonitoring(h.db, &rule, updates); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "更新记录规则失败"})
		return
	}

	h.db.First(&rule, id)

	// Trigger rules sync
	go func(cid uint) {
		if err := h.alertSync.SyncAlertRules(cid); err != nil {
			log.Printf("[RecordingRules] Failed to sync rules after update: %v", err)
		}
	}(clusterID)

	c.JSON(http.StatusAccepted, rule)
}

// DeleteRecordingRule deletes a recording rule and triggers rules sync.
func (h *RecordingRulesHandler) DeleteRecordingRule(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效ID"})
		return
	}

	var rule models.RecordingRule
	if err := h.db.First(&rule, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "记录规则不存在"})
		return
	}

	clusterID := rule.ClusterID

	if err := deleteMonitoring(h.db, &rule); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "删除记录规则失败"})
		return
	}

	// Trigger rules sync
	go func(cid uint) {
		if err := h.alertSync.SyncAlertRules(cid); err != nil {
			log.Printf("[RecordingRules] Failed to sync rules after delete: %v", err)
		}
	}(clusterID)

	c.JSON(http.StatusAccepted, gin.H{"message": "删除已排队", "apply_status": "pending", "delete_pending": true})
}
