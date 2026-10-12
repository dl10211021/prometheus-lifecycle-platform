package handlers

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/services"
	"gorm.io/gorm"
)

type OperationsHandler struct {
	db      *gorm.DB
	backups *services.BackupService
}

func NewOperationsHandler(db *gorm.DB) *OperationsHandler {
	return &OperationsHandler{db: db, backups: services.NewBackupService(db)}
}

func backupTargetError(component *models.Component, scope string) string {
	if component == nil || component.ID == 0 {
		return "请选择具体组件"
	}
	if component.DeployType == "target" {
		return "target 组件仅登记外部服务，平台不管理其文件，不能创建备份"
	}
	switch scope {
	case "config":
		if strings.TrimSpace(component.ConfigPath) == "" {
			return "该组件没有可备份的配置路径"
		}
	case "data":
		if strings.TrimSpace(component.DataPath) == "" {
			return "该组件没有可备份的数据路径"
		}
	case "full":
		if strings.TrimSpace(component.ConfigPath) == "" || strings.TrimSpace(component.DataPath) == "" {
			return "完整备份要求组件同时配置配置路径和数据路径"
		}
	default:
		return "备份范围仅支持 config、data 或 full"
	}
	return ""
}

// CreateOperationLog creates an audit log entry
func (h *OperationsHandler) CreateOperationLog(userID uint, username, action, resource string, resourceID uint, description, ipAddr, userAgent string) {
	if userID == 0 || username == "" || action == "" {
		return
	}
	h.db.Create(&models.OperationLog{
		UserID:      userID,
		Username:    username,
		Action:      action,
		Resource:    resource,
		ResourceID:  resourceID,
		Description: description,
		IPAddress:   ipAddr,
		UserAgent:   userAgent,
	})
}

// ListOperationLogs lists operation logs with pagination and filtering
func (h *OperationsHandler) ListOperationLogs(c *gin.Context) {
	var logs []models.OperationLog
	var total int64

	// Pagination
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > 200 {
		limit = 50
	}
	offset := (page - 1) * limit
	if _, supplied := c.GetQuery("offset"); supplied {
		offset, _ = strconv.Atoi(c.Query("offset"))
		if offset < 0 {
			offset = 0
		}
		page = offset/limit + 1
	}

	// Filters
	userID := c.Query("user_id")
	action := c.Query("action")
	resource := c.Query("resource")
	startDate := c.Query("start_date")
	endDate := c.Query("end_date")

	query := h.db.Model(&models.OperationLog{}).Order("created_at DESC")

	if userID != "" {
		if id, err := strconv.Atoi(userID); err == nil {
			query = query.Where("user_id = ?", id)
		}
	}
	if action != "" {
		query = query.Where("action LIKE ?", "%"+action+"%")
	}
	if resource != "" {
		query = query.Where("resource = ?", resource)
	}
	if startDate != "" {
		query = query.Where("created_at >= ?", startDate)
	}
	if endDate != "" {
		query = query.Where("created_at <= ?", endDate)
	}

	if err := query.Count(&total).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取操作日志总数失败"})
		return
	}
	if err := query.Offset(offset).Limit(limit).Find(&logs).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取操作日志失败"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"logs":   logs,
		"total":  total,
		"page":   page,
		"limit":  limit,
		"offset": offset,
	})
}

// GetPlatformStats returns platform statistics
func (h *OperationsHandler) GetPlatformStats(c *gin.Context) {
	var totalHosts, onlineHosts, dockerHosts, totalComponents, runningComponents int64
	var totalAlertRules, activeAlerts, totalOperations int64
	h.db.Model(&models.Host{}).Count(&totalHosts)
	h.db.Model(&models.Host{}).Where("status = ?", "online").Count(&onlineHosts)
	h.db.Model(&models.Host{}).Where("has_docker = ?", true).Count(&dockerHosts)
	h.db.Model(&models.Component{}).Where("status <> ?", "removed").Count(&totalComponents)
	h.db.Model(&models.Component{}).Where("status = ?", "running").Count(&runningComponents)
	h.db.Model(&models.AlertRule{}).Count(&totalAlertRules)
	h.db.Model(&models.AlertHistory{}).Where("status = ?", "firing").Count(&activeAlerts)
	h.db.Model(&models.Operation{}).Count(&totalOperations)

	var protectedBackups, verifiedBackups, failedSchedules, successfulExercises, failedExercises int64
	h.db.Model(&models.BackupManifest{}).Count(&protectedBackups)
	h.db.Model(&models.BackupManifest{}).Where("verified_at IS NOT NULL").Count(&verifiedBackups)
	h.db.Model(&models.ScheduledBackupStatus{}).Where("last_status = ?", "failed").Count(&failedSchedules)
	h.db.Model(&models.RecoveryExercise{}).Where("status = ?", "success").Count(&successfulExercises)
	h.db.Model(&models.RecoveryExercise{}).Where("status = ?", "failed").Count(&failedExercises)

	c.JSON(http.StatusOK, gin.H{
		"total_hosts": totalHosts, "online_hosts": onlineHosts, "docker_hosts": dockerHosts,
		"total_components": totalComponents, "running_components": runningComponents,
		"total_alert_rules": totalAlertRules, "active_alerts": activeAlerts,
		"total_operations": totalOperations, "platform_version": "1.0", "uptime": "running",
		"workspace_protection": gin.H{
			"protected_backups": protectedBackups, "verified_backups": verifiedBackups,
			"failed_schedules": failedSchedules, "successful_exercises": successfulExercises,
			"failed_exercises": failedExercises,
		},
	})
}

type operationResponse struct {
	models.Operation
	ComponentName string `json:"component_name,omitempty"`
	ComponentType string `json:"component_type,omitempty"`
	HostName      string `json:"host_name,omitempty"`
	HostIP        string `json:"host_ip,omitempty"`
	DurationMS    int64  `json:"duration_ms"`
}

func (h *OperationsHandler) operationResponses(operations []models.Operation) []operationResponse {
	componentIDs := make([]uint, 0, len(operations))
	for _, operation := range operations {
		if operation.ComponentID != nil {
			componentIDs = append(componentIDs, *operation.ComponentID)
		}
	}
	components := make(map[uint]models.Component, len(componentIDs))
	if len(componentIDs) > 0 {
		var records []models.Component
		if h.db.Preload("Host").Where("id IN ?", componentIDs).Find(&records).Error == nil {
			for _, component := range records {
				components[component.ID] = component
			}
		}
	}
	now := time.Now()
	result := make([]operationResponse, 0, len(operations))
	for _, operation := range operations {
		if operation.Stage == "" {
			operation.Stage = operation.Status
		}
		if operation.Progress == 0 && (operation.Status == "success" || operation.Status == "failed") {
			operation.Progress = 100
		}
		startedAt := operation.CreatedAt
		if operation.StartedAt != nil {
			startedAt = *operation.StartedAt
		}
		finishedAt := now
		if operation.CompletedAt != nil {
			finishedAt = *operation.CompletedAt
		}
		view := operationResponse{
			Operation:  operation,
			DurationMS: max(finishedAt.Sub(startedAt).Milliseconds(), 0),
		}
		if operation.ComponentID != nil {
			if component, exists := components[*operation.ComponentID]; exists {
				view.ComponentName = component.Name
				view.ComponentType = string(component.Type)
				view.HostName = component.Host.Name
				view.HostIP = component.Host.IP
			}
		}
		result = append(result, view)
	}
	return result
}

// GetRecentOperations returns recent operations
func (h *OperationsHandler) GetRecentOperations(c *gin.Context) {
	var operations []models.Operation
	offset, limit := operationPage(c)
	var total int64
	query := h.db.Model(&models.Operation{})
	if err := query.Count(&total).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取操作总数失败"})
		return
	}
	if err := query.Order("created_at DESC, id DESC").Offset(offset).Limit(limit).Find(&operations).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取操作失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"operations": h.operationResponses(operations), "offset": offset, "limit": limit, "total": total})
}

func operationPage(c *gin.Context) (int, int) {
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "100"))
	if offset < 0 {
		offset = 0
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 500 {
		limit = 500
	}
	return offset, limit
}

// GetOperation returns a single operation by ID
func (h *OperationsHandler) GetOperation(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ID 无效"})
		return
	}

	var operation models.Operation
	if err := h.db.First(&operation, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "操作记录不存在"})
		return
	}
	responses := h.operationResponses([]models.Operation{operation})
	c.JSON(http.StatusOK, responses[0])
}

// ListBackups lists backups
func (h *OperationsHandler) ListBackups(c *gin.Context) {
	var backups []models.Backup
	clusterID := c.Query("cluster_id")

	offset, limit := operationPage(c)
	query := h.db.Model(&models.Backup{})
	if clusterID != "" {
		if id, err := strconv.Atoi(clusterID); err == nil {
			query = query.Where("cluster_id = ?", id)
		}
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取备份总数失败"})
		return
	}
	if err := query.Order("created_at DESC, id DESC").Offset(offset).Limit(limit).Find(&backups).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取备份失败"})
		return
	}
	backupIDs := make([]uint, 0, len(backups))
	for i := range backups {
		backupIDs = append(backupIDs, backups[i].ID)
	}
	var manifests []models.BackupManifest
	if len(backupIDs) > 0 {
		if err := h.db.Where("backup_id IN ?", backupIDs).Find(&manifests).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "读取备份清单失败"})
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"backups": backups, "manifests": manifests, "offset": offset, "limit": limit, "total": total})
}

// ListScheduledBackupStatuses exposes execution evidence independently from
// the legacy schedule definition response.
func (h *OperationsHandler) ListScheduledBackupStatuses(c *gin.Context) {
	var statuses []models.ScheduledBackupStatus
	offset, limit := operationPage(c)
	var total int64
	query := h.db.Model(&models.ScheduledBackupStatus{})
	if err := query.Count(&total).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取定时备份状态总数失败"})
		return
	}
	if err := query.Order("last_run DESC, id DESC").Offset(offset).Limit(limit).Find(&statuses).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取定时备份状态失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"statuses": statuses, "offset": offset, "limit": limit, "total": total})
}

// CreateBackup creates a new backup record
func (h *OperationsHandler) CreateBackup(c *gin.Context) {
	var req struct {
		Name        string `json:"name"`
		ClusterID   *uint  `json:"cluster_id"`
		ComponentID *uint  `json:"component_id"`
		Type        string `json:"type"` // config/data/full
		Path        string `json:"path"`
		Size        int64  `json:"size"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效请求参数"})
		return
	}

	if req.Name == "" {
		req.Name = "backup-" + time.Now().Format("20060102-150405")
	}
	if req.Type == "" {
		req.Type = "config"
	}

	// Resolve the component to back up: either an explicit component, a
	// cluster with exactly one component, or an explicit cluster + component.
	var comp models.Component
	if req.ComponentID != nil {
		if err := h.db.Preload("Host").First(&comp, *req.ComponentID).Error; err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "组件不存在"})
			return
		}
		if req.ClusterID == nil {
			req.ClusterID = &comp.ClusterID
		}
	} else if req.ClusterID != nil {
		var comps []models.Component
		h.db.Where("cluster_id = ?", *req.ClusterID).Find(&comps)
		if len(comps) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "该集群下没有组件，无法备份"})
			return
		}
		if len(comps) > 1 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "该集群下有多个组件，请指定具体组件"})
			return
		}
		comp = comps[0]
		if err := h.db.Preload("Host").First(&comp, comp.ID).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "加载组件失败"})
			return
		}
	} else {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请选择集群或组件后再备份"})
		return
	}
	if message := backupTargetError(&comp, req.Type); message != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": message})
		return
	}

	backup, manifest, err := h.backups.CreateComponentBackup(c.Request.Context(), &comp, req.Name, req.Type)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "path") || strings.Contains(msg, "scope") || strings.Contains(msg, "component") {
			c.JSON(http.StatusBadRequest, gin.H{"error": msg})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "备份执行失败: " + msg})
		}
		return
	}

	c.JSON(http.StatusCreated, gin.H{"backup": backup, "manifest": manifest, "message": "备份已校验并完成"})
}

// RestoreBackup restores a backup archive to its component's data/config
// directory and restarts the service (binary: systemctl, docker: docker restart).
func (h *OperationsHandler) RestoreBackup(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效ID"})
		return
	}
	var backup models.Backup
	if err := h.db.First(&backup, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "备份不存在"})
		return
	}
	if backup.ComponentID == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "该备份没有关联组件，无法恢复"})
		return
	}
	var comp models.Component
	if err := h.db.Preload("Host").First(&comp, *backup.ComponentID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "关联组件不存在"})
		return
	}
	if err := h.backups.RestoreComponentBackup(c.Request.Context(), &backup, &comp); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "恢复失败: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "恢复完成，校验、预恢复点、重启与就绪检查均已通过"})
}

// VerifyBackup performs an explicit manifest, checksum, size, scope, and
// format verification without changing the component.
func (h *OperationsHandler) VerifyBackup(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效ID"})
		return
	}
	var backup models.Backup
	if err := h.db.First(&backup, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "备份不存在"})
		return
	}
	if backup.ComponentID == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "该接口仅校验组件备份"})
		return
	}
	var component models.Component
	if err := h.db.Preload("Host").First(&component, *backup.ComponentID).Error; err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "关联组件不存在"})
		return
	}
	manifest, err := h.backups.VerifyComponentBackup(c.Request.Context(), &backup, &component)
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"verified": false, "error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"verified": true, "manifest": manifest})
}

// DeleteBackup deletes a backup
func (h *OperationsHandler) DeleteBackup(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效ID"})
		return
	}

	var backup models.Backup
	if err := h.db.First(&backup, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "备份不存在"})
		return
	}

	var component *models.Component
	if backup.ComponentID != nil {
		var comp models.Component
		if err := h.db.Preload("Host").First(&comp, *backup.ComponentID).Error; err != nil {
			c.JSON(http.StatusConflict, gin.H{"error": "无法解析备份所属主机，文件和记录均已保留"})
			return
		}
		component = &comp
	}
	if err := h.backups.DeleteBackupArtifact(c.Request.Context(), &backup, component); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "删除备份文件失败，记录已保留: " + err.Error()})
		return
	}
	if err := h.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("backup_id = ?", id).Delete(&models.BackupManifest{}).Error; err != nil {
			return err
		}
		return tx.Delete(&models.Backup{}, id).Error
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "删除备份记录失败"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "备份已删除"})
}

// removeBackupArtifact is retained for component/host cascade cleanup. The
// service understands both local/remote hosts and file/directory artifacts.
func (h *OperationsHandler) removeBackupArtifact(backup *models.Backup) {
	var component *models.Component
	if backup != nil && backup.ComponentID != nil {
		var comp models.Component
		if err := h.db.Preload("Host").First(&comp, *backup.ComponentID).Error; err == nil {
			component = &comp
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_ = h.backups.DeleteBackupArtifact(ctx, backup, component)
}
