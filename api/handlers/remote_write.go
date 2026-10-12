package handlers

import (
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/core/config"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"gorm.io/gorm"
)

// RemoteWriteHandler handles CRUD operations for Prometheus remote write configurations.
type RemoteWriteHandler struct {
	db         *gorm.DB
	autoConfig *config.AutoConfig
	basePath   string
}

// NewRemoteWriteHandler creates a new RemoteWriteHandler instance.
func NewRemoteWriteHandler(db *gorm.DB, autoConfig *config.AutoConfig, basePath string) *RemoteWriteHandler {
	return &RemoteWriteHandler{db: db, autoConfig: autoConfig, basePath: basePath}
}

// syncPrometheusConfig triggers an async Prometheus config regeneration so the
// remote-write change lands in prometheus.yml without waiting for the scheduler.
func (h *RemoteWriteHandler) syncPrometheusConfig(clusterID uint) {
	if h.autoConfig == nil || clusterID == 0 {
		return
	}
	go func() {
		if err := h.autoConfig.SyncPrometheusConfig(clusterID, h.basePath); err != nil {
			log.Printf("[RemoteWrite] Failed to sync prometheus config for cluster %d: %v", clusterID, err)
		}
	}()
}

// List returns remote write configs, optionally filtered by cluster_id.
func (h *RemoteWriteHandler) List(c *gin.Context) {
	var items []models.RemoteWriteModel
	clusterID := c.Query("cluster_id")
	query := h.db.Order("created_at DESC")
	if clusterID != "" {
		query = query.Where("cluster_id = ?", clusterID)
	}
	query.Find(&items)
	responses := make([]remoteWriteResponse, 0, len(items))
	for _, item := range items {
		responses = append(responses, remoteWriteDTO(item))
	}
	c.JSON(http.StatusOK, gin.H{"remote_writes": responses})
}

// Get returns a single remote write config by ID.
func (h *RemoteWriteHandler) Get(c *gin.Context) {
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}
	var item models.RemoteWriteModel
	if err := h.db.First(&item, id).Error; err != nil {
		utils.HandleNotFoundError(c, "remote_write")
		return
	}
	c.JSON(http.StatusOK, remoteWriteDTO(item))
}

// Create creates a new remote write config.
func (h *RemoteWriteHandler) Create(c *gin.Context) {
	var req struct {
		ClusterID         uint   `json:"cluster_id"`
		Name              string `json:"name" binding:"required"`
		URL               string `json:"url" binding:"required"`
		RemoteTimeout     string `json:"remote_timeout"`
		BasicAuthUser     string `json:"basic_auth_user"`
		BasicAuthPassword string `json:"basic_auth_password"`
		BearerToken       string `json:"bearer_token"`
		Enabled           *bool  `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "操作失败: " + err.Error()})
		return
	}
	if req.RemoteTimeout == "" {
		req.RemoteTimeout = "30s"
	}
	item := models.RemoteWriteModel{
		ClusterID:         req.ClusterID,
		Name:              req.Name,
		URL:               req.URL,
		RemoteTimeout:     req.RemoteTimeout,
		BasicAuthUser:     req.BasicAuthUser,
		BasicAuthPassword: req.BasicAuthPassword,
		BearerToken:       req.BearerToken,
		Enabled:           req.Enabled == nil || *req.Enabled,
	}
	if err := createMonitoring(h.db, &item); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建失败"})
		return
	}
	h.syncPrometheusConfig(item.ClusterID)
	c.JSON(http.StatusAccepted, remoteWriteDTO(item))
}

// Update updates an existing remote write config.
func (h *RemoteWriteHandler) Update(c *gin.Context) {
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}
	var item models.RemoteWriteModel
	if err := h.db.First(&item, id).Error; err != nil {
		utils.HandleNotFoundError(c, "remote_write")
		return
	}
	var req struct {
		Name              *string `json:"name"`
		URL               *string `json:"url"`
		RemoteTimeout     *string `json:"remote_timeout"`
		BasicAuthUser     *string `json:"basic_auth_user"`
		BasicAuthPassword *string `json:"basic_auth_password"`
		BearerToken       *string `json:"bearer_token"`
		Enabled           *bool   `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "操作失败: " + err.Error()})
		return
	}
	updates := map[string]interface{}{}
	if req.Name != nil {
		updates["name"] = *req.Name
	}
	if req.URL != nil {
		updates["url"] = *req.URL
	}
	if req.RemoteTimeout != nil {
		updates["remote_timeout"] = *req.RemoteTimeout
	}
	if req.BasicAuthUser != nil {
		updates["basic_auth_user"] = *req.BasicAuthUser
	}
	if req.BasicAuthPassword != nil {
		updates["basic_auth_password"] = *req.BasicAuthPassword
	}
	if req.BearerToken != nil {
		updates["bearer_token"] = *req.BearerToken
	}
	if req.Enabled != nil {
		updates["enabled"] = *req.Enabled
	}
	if len(updates) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "没有可更新的字段"})
		return
	}
	if err := updateMonitoring(h.db, &item, updates); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "更新失败"})
		return
	}
	h.syncPrometheusConfig(item.ClusterID)
	h.db.First(&item, id)
	c.JSON(http.StatusAccepted, remoteWriteDTO(item))
}

// Delete removes a remote write config.
func (h *RemoteWriteHandler) Delete(c *gin.Context) {
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}
	var item models.RemoteWriteModel
	if err := h.db.First(&item, id).Error; err != nil {
		utils.HandleNotFoundError(c, "remote_write")
		return
	}
	clusterID := item.ClusterID
	if err := deleteMonitoring(h.db, &item); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "删除失败"})
		return
	}
	h.syncPrometheusConfig(clusterID)
	c.JSON(http.StatusAccepted, gin.H{"message": "删除已排队", "apply_status": "pending", "delete_pending": true})
}
