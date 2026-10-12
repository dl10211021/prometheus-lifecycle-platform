package handlers

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/services"
)

// ========== NODE ==========
// GetNode returns the user-visible settings for the platform's only node. The
// internal Cluster name is intentionally omitted.
func (h *Handler) GetNode(c *gin.Context) {
	node, err := services.EnsureSingleNode(h.db)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取节点失败: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"id":              node.ID,
		"host":            node.Host,
		"description":     node.Description,
		"status":          node.Status,
		"deployment_type": node.DeploymentType,
		"created_at":      node.CreatedAt,
		"updated_at":      node.UpdatedAt,
	})
}

// UpdateNode updates user-visible node settings without exposing or changing
// the hidden singleton Cluster identity.
func (h *Handler) UpdateNode(c *gin.Context) {
	node, err := services.EnsureSingleNode(h.db)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取节点失败: " + err.Error()})
		return
	}
	var req struct {
		Host           *string `json:"host"`
		Description    *string `json:"description"`
		Status         *string `json:"status"`
		DeploymentType *string `json:"deployment_type"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "操作失败: " + err.Error()})
		return
	}
	updates := make(map[string]interface{}, 4)
	if req.Host != nil {
		updates["host"] = strings.TrimSpace(*req.Host)
	}
	if req.Description != nil {
		updates["description"] = strings.TrimSpace(*req.Description)
	}
	if req.Status != nil {
		switch *req.Status {
		case "active", "inactive":
			updates["status"] = *req.Status
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "status 仅可为 active 或 inactive"})
			return
		}
	}
	if req.DeploymentType != nil {
		switch *req.DeploymentType {
		case "single-node", "binary":
			updates["deployment_type"] = *req.DeploymentType
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "deployment_type 无效，仅支持 single-node 或 binary"})
			return
		}
	}
	if len(updates) > 0 {
		if err := h.db.Model(node).Updates(updates).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "更新节点失败: " + err.Error()})
			return
		}
	}
	h.GetNode(c)
}
