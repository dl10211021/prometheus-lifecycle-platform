package handlers

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"github.com/promlifecycle/platform/services"
)

// ===== Maintenance Windows =====

// ListMaintenanceWindows returns the scheduler's last observed state for every
// per-Alertmanager silence, rather than inferring state from the time window.
func (h *OpsHandler) ListMaintenanceWindows(c *gin.Context) {
	var windows []models.MaintenanceWindow
	h.db.Where("end_at > ?", time.Now().UTC()).Order("start_at ASC").Find(&windows)
	result := make([]gin.H, 0, len(windows))
	for i := range windows {
		var silences []models.MaintenanceSilence
		h.db.Where("maintenance_window_id = ?", windows[i].ID).
			Order("alertmanager_component_id ASC").Find(&silences)
		state := "pending"
		if len(silences) > 0 {
			state = "active"
			for j := range silences {
				if silences[j].DeleteRequested || silences[j].Status == "pending_delete" {
					state = "pending_delete"
					break
				}
				if silences[j].Status != "active" {
					state = silences[j].Status
				}
			}
		}
		result = append(result, gin.H{
			"id": windows[i].ID, "name": windows[i].Name,
			"cluster_id": windows[i].ClusterID, "component_id": windows[i].ComponentID,
			"start_at": windows[i].StartAt, "end_at": windows[i].EndAt,
			"reason": windows[i].Reason, "created_by": windows[i].CreatedBy,
			"created_at": windows[i].CreatedAt, "status": state, "silences": silences,
		})
	}
	c.JSON(http.StatusOK, gin.H{"windows": result})
}

// CreateMaintenanceWindow creates a maintenance window
func (h *OpsHandler) CreateMaintenanceWindow(c *gin.Context) {
	var mw models.MaintenanceWindow
	if err := c.ShouldBindJSON(&mw); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效请求"})
		return
	}
	if mw.Name == "" || mw.StartAt.IsZero() || mw.EndAt.IsZero() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "名称、开始时间和结束时间不能为空"})
		return
	}
	if !mw.EndAt.After(mw.StartAt) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "结束时间必须晚于开始时间"})
		return
	}
	if _, err := services.MaintenanceMatchers(h.db, &mw); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	mw.ID = 0
	mw.SilenceID = ""
	// middleware stores user_id in context (not the full user object)
	if v, ok := c.Get("user_id"); ok {
		if id, ok2 := v.(uint); ok2 {
			mw.CreatedBy = id
		}
	}
	if err := h.db.Create(&mw).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建失败"})
		return
	}
	c.JSON(http.StatusCreated, mw)
}

// DeleteMaintenanceWindow requests expiry on every Alertmanager. The window
// row remains until all instances confirm success (or 404); transport errors
// and 5xx responses keep their references for scheduler retries.
func (h *OpsHandler) DeleteMaintenanceWindow(c *gin.Context) {
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}
	var mw models.MaintenanceWindow
	if err := h.db.First(&mw, id).Error; err != nil {
		utils.HandleNotFoundError(c, "maintenance window")
		return
	}
	var count int64
	h.db.Model(&models.MaintenanceSilence{}).Where("maintenance_window_id = ?", mw.ID).Count(&count)
	if count == 0 {
		if mw.SilenceID == "" {
			if err := h.db.Delete(&mw).Error; err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "删除失败"})
				return
			}
			c.JSON(http.StatusOK, gin.H{"message": "已删除"})
			return
		}
		var am models.Component
		q := h.db.Where("type = ?", models.ComponentAlertmanager).Order("id ASC")
		if mw.ClusterID != nil {
			q = q.Where("cluster_id = ?", *mw.ClusterID)
		}
		if err := q.First(&am).Error; err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "无法定位旧静默所属的 Alertmanager，已保留维护窗口以避免丢失引用"})
			return
		}
		if err := h.db.Create(&models.MaintenanceSilence{
			MaintenanceWindowID: mw.ID, AlertmanagerComponentID: am.ID,
			SilenceID: mw.SilenceID, Status: "pending_delete", DeleteRequested: true,
		}).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "迁移旧静默引用失败"})
			return
		}
	}
	if err := h.db.Model(&models.MaintenanceSilence{}).
		Where("maintenance_window_id = ?", mw.ID).
		Updates(map[string]interface{}{"delete_requested": true, "status": "pending_delete"}).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "删除请求保存失败"})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"message": "正在关闭各 Alertmanager 的维护静默"})
}

// IsMaintenance is true only when the scheduler has observed an actually
// active Alertmanager silence for a currently active window.
func (h *OpsHandler) IsMaintenance(c *gin.Context) {
	componentID := c.Query("component_id")
	clusterID := c.Query("cluster_id")
	now := time.Now().UTC()
	var count int64
	q := h.db.Table("maintenance_windows").
		Joins("JOIN maintenance_silences ON maintenance_silences.maintenance_window_id = maintenance_windows.id").
		Where("maintenance_windows.start_at <= ? AND maintenance_windows.end_at > ?", now, now).
		Where("maintenance_silences.status = ? AND maintenance_silences.delete_requested = ?", "active", false)
	if componentID != "" {
		var component models.Component
		if h.db.First(&component, componentID).Error != nil {
			c.JSON(http.StatusOK, gin.H{"in_maintenance": false})
			return
		}
		q = q.Where("(maintenance_windows.component_id = ?) OR (maintenance_windows.component_id IS NULL AND maintenance_windows.cluster_id = ?)",
			componentID, component.ClusterID)
	} else if clusterID != "" {
		q = q.Where("maintenance_windows.component_id IS NULL AND maintenance_windows.cluster_id = ?", clusterID)
	} else {
		c.JSON(http.StatusBadRequest, gin.H{"error": "必须指定组件或节点"})
		return
	}
	q.Distinct("maintenance_windows.id").Count(&count)
	c.JSON(http.StatusOK, gin.H{"in_maintenance": count > 0})
}
