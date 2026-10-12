package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
)

func (h *Handler) ListVersions(c *gin.Context) {
	var versions []models.ComponentVersion
	h.db.Find(&versions)
	c.JSON(http.StatusOK, gin.H{"versions": versions})
}

func (h *Handler) GetVersion(c *gin.Context) {
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}
	var version models.ComponentVersion
	if err := h.db.First(&version, id).Error; err != nil {
		utils.HandleNotFoundError(c, "version")
		return
	}
	c.JSON(http.StatusOK, version)
}

func (h *Handler) CreateVersion(c *gin.Context) {
	var req struct {
		Type     string `json:"type" binding:"required"`
		Version  string `json:"version" binding:"required"`
		Image    string `json:"image" binding:"required"`
		Source   string `json:"source"`
		Checksum string `json:"checksum"`
		Notes    string `json:"notes"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "操作失败: " + err.Error()})
		return
	}
	if req.Source == "" {
		req.Source = "custom"
	}
	version := models.ComponentVersion{
		Type:     models.ComponentType(req.Type),
		Version:  req.Version,
		Image:    req.Image,
		Source:   req.Source,
		Checksum: req.Checksum,
		Notes:    req.Notes,
	}
	if err := h.db.Create(&version).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建版本失败"})
		return
	}
	c.JSON(http.StatusCreated, version)
}

func (h *Handler) UpdateVersion(c *gin.Context) {
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}
	var req struct {
		Type     string `json:"type"`
		Version  string `json:"version"`
		Image    string `json:"image"`
		Source   string `json:"source"`
		Checksum string `json:"checksum"`
		Notes    string `json:"notes"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "操作失败: " + err.Error()})
		return
	}
	var version models.ComponentVersion
	if err := h.db.First(&version, id).Error; err != nil {
		utils.HandleNotFoundError(c, "version")
		return
	}
	if req.Type != "" {
		version.Type = models.ComponentType(req.Type)
	}
	if req.Version != "" {
		version.Version = req.Version
	}
	if req.Image != "" {
		version.Image = req.Image
	}
	if req.Source != "" {
		version.Source = req.Source
	}
	if req.Checksum != "" {
		version.Checksum = req.Checksum
	}
	if req.Notes != "" {
		version.Notes = req.Notes
	}
	if err := h.db.Save(&version).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "更新版本失败"})
		return
	}
	c.JSON(http.StatusOK, version)
}

func (h *Handler) DeleteVersion(c *gin.Context) {
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}
	result := h.db.Delete(&models.ComponentVersion{}, id)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "删除版本失败"})
		return
	}
	if result.RowsAffected == 0 {
		utils.HandleNotFoundError(c, "version")
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "已删除"})
}
