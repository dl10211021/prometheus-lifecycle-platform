package handlers

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/core/config"
	"github.com/promlifecycle/platform/models"
	"gorm.io/gorm"
)

// ScrapeConfigsHandler handles CRUD operations for Prometheus scrape configurations.
type ScrapeConfigsHandler struct {
	db         *gorm.DB
	autoConfig *config.AutoConfig
	basePath   string
}

// NewScrapeConfigsHandler creates a new ScrapeConfigsHandler instance.
func NewScrapeConfigsHandler(db *gorm.DB, autoConfig *config.AutoConfig, basePath string) *ScrapeConfigsHandler {
	return &ScrapeConfigsHandler{db: db, autoConfig: autoConfig, basePath: basePath}
}

// ListScrapeConfigs lists scrape configurations, optionally filtered by cluster_id.
func (h *ScrapeConfigsHandler) ListScrapeConfigs(c *gin.Context) {
	raw, allowed := configRawRead(c)
	if !allowed {
		return
	}
	var configs []models.ScrapeConfigModel
	clusterID := c.Query("cluster_id")

	query := h.db.Order("created_at DESC")
	if clusterID != "" {
		if id, err := strconv.Atoi(clusterID); err == nil {
			query = query.Where("cluster_id = ?", id)
		}
	}

	query.Find(&configs)
	if !raw {
		for i := range configs {
			configs[i] = scrapeConfigDTO(configs[i])
		}
	}
	c.JSON(http.StatusOK, gin.H{"configs": configs})
}

// GetScrapeConfig returns a single scrape configuration by ID.
func (h *ScrapeConfigsHandler) GetScrapeConfig(c *gin.Context) {
	raw, allowed := configRawRead(c)
	if !allowed {
		return
	}
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效ID"})
		return
	}

	var cfg models.ScrapeConfigModel
	if err := h.db.First(&cfg, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "采集配置不存在"})
		return
	}

	if !raw {
		cfg = scrapeConfigDTO(cfg)
	}
	c.JSON(http.StatusOK, cfg)
}

// CreateScrapeConfig creates a new scrape configuration and triggers Prometheus config sync.
func (h *ScrapeConfigsHandler) CreateScrapeConfig(c *gin.Context) {
	var req struct {
		ClusterID      uint            `json:"cluster_id"`
		Name           string          `json:"name"`
		JobName        string          `json:"job_name"`
		ScrapeInterval string          `json:"scrape_interval"`
		ScrapeTimeout  string          `json:"scrape_timeout"`
		Targets        json.RawMessage `json:"targets"` // accept array or string
		Labels         json.RawMessage `json:"labels"`
		FileSDConfigs  json.RawMessage `json:"file_sd_configs"`
		HTTPSDConfigs  json.RawMessage `json:"http_sd_configs"`
		RelabelConfigs json.RawMessage `json:"relabel_configs"`
		Type           string          `json:"type"`
		Enabled        *bool           `json:"enabled"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效请求参数"})
		return
	}

	if req.Name == "" || req.JobName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "名称和 job_name 不能为空"})
		return
	}
	fields, err := normalizeScrapeJSONFields(map[string]json.RawMessage{
		"targets": req.Targets, "labels": req.Labels, "file_sd_configs": req.FileSDConfigs,
		"http_sd_configs": req.HTTPSDConfigs, "relabel_configs": req.RelabelConfigs,
	})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	cfg := models.ScrapeConfigModel{
		ClusterID:      req.ClusterID,
		Name:           req.Name,
		JobName:        req.JobName,
		ScrapeInterval: req.ScrapeInterval,
		ScrapeTimeout:  req.ScrapeTimeout,
		Targets:        fields["targets"],
		Labels:         fields["labels"],
		FileSDConfigs:  fields["file_sd_configs"],
		HTTPSDConfigs:  fields["http_sd_configs"],
		RelabelConfigs: fields["relabel_configs"],
		Type:           req.Type,
		Enabled:        true,
	}
	if cfg.ScrapeInterval == "" {
		cfg.ScrapeInterval = "15s"
	}
	if cfg.ScrapeTimeout == "" {
		cfg.ScrapeTimeout = "10s"
	}
	if cfg.Type == "" {
		cfg.Type = "static"
	}
	if req.Enabled != nil {
		cfg.Enabled = *req.Enabled
	}

	if err := createMonitoring(h.db, &cfg); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建采集配置失败: " + err.Error()})
		return
	}

	// Trigger Prometheus config sync
	go func(clusterID uint) {
		if err := h.autoConfig.SyncPrometheusConfig(clusterID, h.basePath); err != nil {
			log.Printf("[ScrapeConfigs] Failed to sync prometheus config after create: %v", err)
		}
	}(cfg.ClusterID)

	c.JSON(http.StatusAccepted, scrapeConfigDTO(cfg))
}

// UpdateScrapeConfig updates an existing scrape configuration and triggers Prometheus config sync.
func (h *ScrapeConfigsHandler) UpdateScrapeConfig(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效ID"})
		return
	}

	var cfg models.ScrapeConfigModel
	if err := h.db.First(&cfg, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "采集配置不存在"})
		return
	}

	var req struct {
		Name           *string         `json:"name"`
		JobName        *string         `json:"job_name"`
		ScrapeInterval *string         `json:"scrape_interval"`
		ScrapeTimeout  *string         `json:"scrape_timeout"`
		Targets        json.RawMessage `json:"targets"`
		Labels         json.RawMessage `json:"labels"`
		FileSDConfigs  json.RawMessage `json:"file_sd_configs"`
		HTTPSDConfigs  json.RawMessage `json:"http_sd_configs"`
		RelabelConfigs json.RawMessage `json:"relabel_configs"`
		Enabled        *bool           `json:"enabled"`
		Type           *string         `json:"type"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效请求参数"})
		return
	}

	clusterID := cfg.ClusterID

	updates := map[string]interface{}{}
	if req.Name != nil {
		updates["name"] = *req.Name
	}
	if req.JobName != nil {
		updates["job_name"] = *req.JobName
	}
	if req.ScrapeInterval != nil {
		updates["scrape_interval"] = *req.ScrapeInterval
	}
	if req.ScrapeTimeout != nil {
		updates["scrape_timeout"] = *req.ScrapeTimeout
	}
	fields, err := normalizeScrapeJSONFields(map[string]json.RawMessage{
		"targets": req.Targets, "labels": req.Labels, "file_sd_configs": req.FileSDConfigs,
		"http_sd_configs": req.HTTPSDConfigs, "relabel_configs": req.RelabelConfigs,
	})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	for name, value := range fields {
		updates[name] = value
	}
	if req.Enabled != nil {
		updates["enabled"] = *req.Enabled
	}
	if req.Type != nil {
		updates["type"] = *req.Type
	}
	if len(updates) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "没有可更新的字段"})
		return
	}

	if err := updateMonitoring(h.db, &cfg, updates); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "更新采集配置失败"})
		return
	}

	h.db.First(&cfg, id)

	// Trigger Prometheus config sync
	go func(cid uint) {
		if err := h.autoConfig.SyncPrometheusConfig(cid, h.basePath); err != nil {
			log.Printf("[ScrapeConfigs] Failed to sync prometheus config after update: %v", err)
		}
	}(clusterID)

	c.JSON(http.StatusAccepted, scrapeConfigDTO(cfg))
}

// DeleteScrapeConfig deletes a scrape configuration and triggers Prometheus config sync.
func (h *ScrapeConfigsHandler) DeleteScrapeConfig(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效ID"})
		return
	}

	var cfg models.ScrapeConfigModel
	if err := h.db.First(&cfg, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "采集配置不存在"})
		return
	}

	clusterID := cfg.ClusterID

	if err := deleteMonitoring(h.db, &cfg); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "删除采集配置失败"})
		return
	}

	// Trigger Prometheus config sync
	go func(cid uint) {
		if err := h.autoConfig.SyncPrometheusConfig(cid, h.basePath); err != nil {
			log.Printf("[ScrapeConfigs] Failed to sync prometheus config after delete: %v", err)
		}
	}(clusterID)

	c.JSON(http.StatusAccepted, gin.H{"message": "删除已排队", "apply_status": "pending", "delete_pending": true})
}

// SyncPrometheusConfig manually triggers a Prometheus config sync for a cluster.
func (h *ScrapeConfigsHandler) SyncPrometheusConfig(c *gin.Context) {
	clusterID := 0
	if raw := c.Param("cluster_id"); raw != "" {
		id, err := strconv.Atoi(raw)
		if err != nil || id < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "无效的集群ID"})
			return
		}
		clusterID = id
	}

	if err := h.autoConfig.SyncPrometheusConfig(uint(clusterID), h.basePath); err != nil {
		status, state := http.StatusInternalServerError, "error"
		if errors.Is(err, config.ErrManualPrometheusConfig) {
			status, state = http.StatusConflict, "manual"
		}
		c.JSON(status, gin.H{"error": "同步 prometheus 配置失败: " + err.Error(), "apply_status": state, "last_error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Prometheus 配置已同步", "apply_status": "applied"})
}
