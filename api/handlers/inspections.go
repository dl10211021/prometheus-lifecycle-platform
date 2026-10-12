package handlers

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/services"
)

// RunComponentInspection performs one explicit full-platform inspection. The
// same service is used by the scheduler, so manual and scheduled runs produce
// identical history and task evidence.
func (h *Handler) RunComponentInspection(c *gin.Context) {
	triggeredBy := "api"
	if username, exists := c.Get("username"); exists {
		if value, ok := username.(string); ok && strings.TrimSpace(value) != "" {
			triggeredBy = value
		}
	}
	execution, err := services.NewInspectionService(h.db, h.basePath).Run(
		c.Request.Context(), services.InspectionRequest{TriggeredBy: triggeredBy},
	)
	if err != nil {
		if errors.Is(err, services.ErrInspectionBusy) {
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "执行巡检失败: " + err.Error(), "run": execution.Run})
		return
	}
	response := inspectionRunResponse(execution.Run, execution.Checks)
	response["notifications"] = execution.Notifications
	if execution.NotificationError != "" {
		response["notification_error"] = execution.NotificationError
	}
	c.JSON(http.StatusOK, response)
}

func inspectionRunResponse(run models.InspectionRun, checks []models.InspectionCheck) gin.H {
	return gin.H{
		"run": run,
		"summary": gin.H{
			"total": run.Total, "passed": run.Passed, "failed": run.Failed,
			"duration_ms": run.DurationMS,
		},
		"checks": checks, "checked_at": run.CompletedAt,
	}
}

func (h *Handler) ListInspectionRuns(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	if limit < 1 {
		limit = 1
	}
	if limit > 200 {
		limit = 200
	}
	var runs []models.InspectionRun
	if err := h.db.Order("created_at DESC").Limit(limit).Find(&runs).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取巡检历史失败: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"runs": runs})
}

func (h *Handler) GetInspectionRun(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "巡检记录 ID 无效"})
		return
	}
	var run models.InspectionRun
	if err := h.db.First(&run, uint(id)).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "巡检记录不存在"})
		return
	}
	var checks []models.InspectionCheck
	if err := h.db.Where("run_id = ?", run.ID).Order("component_id ASC, id ASC").Find(&checks).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取巡检检查证据失败: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, inspectionRunResponse(run, checks))
}
