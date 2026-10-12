package handlers

import (
	"errors"
	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/core/deployer"
	"github.com/promlifecycle/platform/models"
	"gorm.io/gorm"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// PreviewPrometheusParameterMigration reads saved metadata only. No client
// argv, plan, preflight token or execution permission is accepted or produced.
func (h *Handler) PreviewPrometheusParameterMigration(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	id, err := strconv.ParseUint(c.Param("id"), 10, strconv.IntSize)
	if err != nil || id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "组件 ID 无效"})
		return
	}
	query, err := url.ParseQuery(c.Request.URL.RawQuery)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "预览查询参数无效"})
		return
	}
	for key, values := range query {
		if key == "target_version" {
			continue
		}
		// SingleNodeScope replaces this field with its authoritative node ID.
		nodeID, ok := c.Get("single_node_id")
		node, typed := nodeID.(uint)
		if key != "cluster_id" || !ok || !typed || len(values) != 1 || values[0] != strconv.FormatUint(uint64(node), 10) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "预览只接受目标版本参数"})
			return
		}
	}
	versions := query["target_version"]
	if len(versions) != 1 || len(versions[0]) > 128 || strings.TrimSpace(versions[0]) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请提供唯一的目标版本"})
		return
	}
	var component models.Component
	if err := h.db.First(&component, uint(id)).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "组件不存在"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "读取组件失败"})
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"component_id": component.ID, "preview": deployer.PreviewPrometheusParameterMigration(&component, strings.TrimSpace(versions[0]))})
}
