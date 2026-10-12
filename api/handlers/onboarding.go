package handlers

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"github.com/promlifecycle/platform/services"
)

func (h *Handler) onboardingService() *services.OnboardingService {
	return services.NewOnboardingService(h.db, h.basePath)
}

// GetOnboardingStatus evaluates the five first-run outcomes. Runtime outcomes
// are probed live; a row existing in the database is never enough to complete a
// step.
func (h *Handler) GetOnboardingStatus(c *gin.Context) {
	status, err := h.onboardingService().Status()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取首次向导状态失败: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, status)
}

func (h *Handler) GetOnboardingPreflight(c *gin.Context) {
	report, err := h.onboardingService().Preflight()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "本机预检失败: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, report)
}

func (h *Handler) GetOnboardingDiscovery(c *gin.Context) {
	components, err := h.onboardingService().DiscoverLocal()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "扫描本机组件失败: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"host": "127.0.0.1", "components": components})
}

func (h *Handler) DeleteOnboardingDiscovery(c *gin.Context) {
	hostID, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}
	var req struct {
		Fingerprint  string `json:"fingerprint" binding:"required"`
		Confirmation string `json:"confirmation" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Confirmation != "DELETE_DISCOVERED" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "删除发现项必须提供最新 fingerprint 并确认 DELETE_DISCOVERED"})
		return
	}
	result, err := h.onboardingService().DeleteDiscovered(c.Request.Context(), hostID, req.Fingerprint)
	if err != nil {
		switch {
		case errors.Is(err, services.ErrDiscoveredNotFound):
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		case errors.Is(err, services.ErrDiscoveredManaged), errors.Is(err, services.ErrDiscoveredSharedArtifact):
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "发现项已停止、禁用自启动、卸载并验证；配置和数据路径保持不变", "result": result})
}

// RunOnboardingAcceptance actively checks Prometheus health, target health,
// rule evaluation, a real notification delivery and readable backup contents.
func (h *Handler) RunOnboardingAcceptance(c *gin.Context) {
	var req struct {
		NotificationChannelID uint `json:"notification_channel_id"`
	}
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "验收请求无效，请检查通知渠道 ID"})
			return
		}
	}
	report, err := h.onboardingService().Acceptance(req.NotificationChannelID, true)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "执行实时验收失败: " + err.Error()})
		return
	}
	code := http.StatusOK
	if !report.Passed {
		code = http.StatusUnprocessableEntity
	}
	c.JSON(code, report)
}

func (h *Handler) ListStackTemplates(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"templates": services.StackTemplates()})
}

func (h *Handler) GetComponentAdoption(c *gin.Context) {
	componentID, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}
	var adoption models.ComponentAdoption
	if err := h.db.Where("component_id = ?", componentID).First(&adoption).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "该组件不是通过接管创建的"})
		return
	}
	c.JSON(http.StatusOK, adoption)
}
