package handlers

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/core/scheduler"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/services"
	"gorm.io/gorm"
)

type scheduledInspectionInput struct {
	Name      string `json:"name"`
	Cron      string `json:"cron"`
	KeepCount int    `json:"keep_count"`
	Enabled   *bool  `json:"enabled"`
}

func (h *Handler) ListScheduledInspections(c *gin.Context) {
	var schedules []models.ScheduledInspection
	if err := h.db.Order("created_at DESC").Find(&schedules).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取定时巡检计划失败"})
		return
	}
	var statuses []models.ScheduledInspectionStatus
	if err := h.db.Order("last_run DESC").Find(&statuses).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取定时巡检状态失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"schedules": schedules, "statuses": statuses})
}

func (h *Handler) CreateScheduledInspection(c *gin.Context) {
	var input scheduledInspectionInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "定时巡检输入无效: " + err.Error()})
		return
	}
	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	if input.KeepCount == 0 {
		input.KeepCount = 30
	}
	schedule := models.ScheduledInspection{
		Name: strings.TrimSpace(input.Name), Cron: strings.TrimSpace(input.Cron),
		KeepCount: input.KeepCount, Enabled: enabled, CreatedBy: inspectionActor(c),
	}
	if err := scheduler.ValidateInspectionSchedule(&schedule); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.db.Create(&schedule).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建定时巡检计划失败"})
		return
	}
	c.JSON(http.StatusCreated, schedule)
}

func (h *Handler) UpdateScheduledInspection(c *gin.Context) {
	id, ok := parseInspectionScheduleID(c)
	if !ok {
		return
	}
	var schedule models.ScheduledInspection
	if err := h.db.First(&schedule, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "定时巡检计划不存在"})
		return
	}
	var input scheduledInspectionInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "定时巡检输入无效: " + err.Error()})
		return
	}
	if strings.TrimSpace(input.Name) != "" {
		schedule.Name = strings.TrimSpace(input.Name)
	}
	if strings.TrimSpace(input.Cron) != "" {
		schedule.Cron = strings.TrimSpace(input.Cron)
	}
	if input.KeepCount != 0 {
		schedule.KeepCount = input.KeepCount
	}
	if input.Enabled != nil {
		schedule.Enabled = *input.Enabled
	}
	if err := scheduler.ValidateInspectionSchedule(&schedule); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.db.Save(&schedule).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "更新定时巡检计划失败"})
		return
	}
	c.JSON(http.StatusOK, schedule)
}

func (h *Handler) DeleteScheduledInspection(c *gin.Context) {
	id, ok := parseInspectionScheduleID(c)
	if !ok {
		return
	}
	_, release, claimErr := services.ClaimInspection(c.Request.Context(), h.db)
	if claimErr != nil {
		c.JSON(http.StatusConflict, gin.H{"error": claimErr.Error()})
		return
	}
	defer release()
	var status models.ScheduledInspectionStatus
	if err := h.db.Where("scheduled_inspection_id = ?", id).First(&status).Error; err == nil && status.LastStatus == "running" {
		c.JSON(http.StatusConflict, gin.H{"error": "定时巡检正在执行，不能删除"})
		return
	}
	result := h.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("scheduled_inspection_id = ?", id).Delete(&models.ScheduledInspectionStatus{}).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.InspectionRun{}).Where("scheduled_inspection_id = ?", id).Update("scheduled_inspection_id", nil).Error; err != nil {
			return err
		}
		result := tx.Delete(&models.ScheduledInspection{}, id)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		return nil
	})
	if result == gorm.ErrRecordNotFound {
		c.JSON(http.StatusNotFound, gin.H{"error": "定时巡检计划不存在"})
		return
	}
	if result != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "删除定时巡检计划失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "定时巡检计划已删除"})
}

func (h *Handler) RunScheduledInspectionNow(c *gin.Context) {
	id, ok := parseInspectionScheduleID(c)
	if !ok {
		return
	}
	var schedule models.ScheduledInspection
	if err := h.db.First(&schedule, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "定时巡检计划不存在"})
		return
	}
	var status models.ScheduledInspectionStatus
	if err := h.db.Where("scheduled_inspection_id = ?", id).First(&status).Error; err == nil && status.LastStatus == "running" {
		c.JSON(http.StatusConflict, gin.H{"error": "定时巡检已经在执行"})
		return
	}
	execution, err := scheduler.NewScheduler(h.db, h.basePath).ExecuteInspectionSchedule(
		c.Request.Context(), &schedule, inspectionActor(c), time.Now(),
	)
	if err != nil {
		if errors.Is(err, services.ErrInspectionBusy) {
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "执行定时巡检失败: " + err.Error(), "run": execution.Run})
		return
	}
	response := inspectionRunResponse(execution.Run, execution.Checks)
	response["notifications"] = execution.Notifications
	if execution.NotificationError != "" {
		response["notification_error"] = execution.NotificationError
	}
	c.JSON(http.StatusOK, response)
}

func inspectionActor(c *gin.Context) string {
	if username, exists := c.Get("username"); exists {
		if value, ok := username.(string); ok && strings.TrimSpace(value) != "" {
			return value
		}
	}
	return "api"
}

func parseInspectionScheduleID(c *gin.Context) (uint, bool) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "定时巡检计划 ID 无效"})
		return 0, false
	}
	return uint(id), true
}
