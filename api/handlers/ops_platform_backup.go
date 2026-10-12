package handlers

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/core/scheduler"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"github.com/promlifecycle/platform/storage/sqlite"
)

// ===== Platform DB Backups =====

// restoreStageMu serializes platform DB restore staging: two concurrent
// restores must not interleave their temp-file copies into the pending path.
var restoreStageMu sync.Mutex
var platformSnapshotMu sync.Mutex

// ListPlatformBackups lists the platform's own SQLite snapshots written by the
// scheduler ({basePath}/backups/platform-db-*.db).
func (h *OpsHandler) ListPlatformBackups(c *gin.Context) {
	dir := filepath.Join(h.basePath, "backups")
	files, err := filepath.Glob(filepath.Join(dir, "platform-db-*.db"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取备份目录失败"})
		return
	}
	backups := make([]gin.H, 0, len(files))
	dbPath, err := sqlite.DatabasePath(h.db)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	restore, err := sqlite.RestoreStatus(dbPath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	for _, f := range files {
		fi, err := os.Stat(f)
		if err != nil {
			continue
		}
		eligibilityError := ""
		if err := sqlite.RestoreEligibility(dbPath, f); err != nil {
			eligibilityError = err.Error()
		}
		backups = append(backups, gin.H{
			"name":             filepath.Base(f),
			"size":             fi.Size(),
			"modified_at":      fi.ModTime().Format(time.RFC3339),
			"restore_eligible": eligibilityError == "",
			"restore_error":    eligibilityError,
		})
	}
	// Newest first
	sort.Slice(backups, func(i, j int) bool {
		return backups[i]["modified_at"].(string) > backups[j]["modified_at"].(string)
	})
	c.JSON(http.StatusOK, gin.H{"backups": backups, "restore": restore})
}

func (h *OpsHandler) CreatePlatformBackup(c *gin.Context) {
	platformSnapshotMu.Lock()
	defer platformSnapshotMu.Unlock()
	dir := filepath.Join(h.basePath, "backups")
	if err := os.MkdirAll(dir, 0700); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建平台备份目录失败"})
		return
	}
	name := "platform-db-" + time.Now().Format("2006-01-02-150405.000000000") + ".db"
	path := filepath.Join(dir, name)
	if err := sqlite.CreateSnapshot(h.db, path); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建平台数据库快照失败: " + err.Error()})
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "平台快照已创建但无法读取"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"backup": gin.H{"name": name, "size": info.Size(), "modified_at": info.ModTime()}})
}

// DownloadPlatformBackup streams a platform snapshot. Name is validated
// against the backup directory so arbitrary file reads are impossible.
func (h *OpsHandler) DownloadPlatformBackup(c *gin.Context) {
	name := filepath.Base(c.Query("name"))
	if name == "" || name == "." || name == ".." ||
		!strings.HasPrefix(name, "platform-db-") || !strings.HasSuffix(name, ".db") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的备份文件名"})
		return
	}
	path := filepath.Join(h.basePath, "backups", name)
	if _, err := os.Stat(path); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "备份文件不存在"})
		return
	}
	c.Header("Content-Disposition", "attachment; filename="+name)
	c.File(path)
}

// RestorePlatformBackup validates and stages a candidate on the DB filesystem.
// Startup retains the original until migrations and initialization succeed.
func (h *OpsHandler) RestorePlatformBackup(c *gin.Context) {
	name := filepath.Base(c.Query("name"))
	if name == "" || name == "." || name == ".." ||
		!strings.HasPrefix(name, "platform-db-") || !strings.HasSuffix(name, ".db") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的备份文件名"})
		return
	}
	src := filepath.Join(h.basePath, "backups", name)
	if _, err := os.Stat(src); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "备份文件不存在"})
		return
	}
	restoreStageMu.Lock()
	defer restoreStageMu.Unlock()
	dbPath, err := sqlite.DatabasePath(h.db)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	state, err := sqlite.StageRestore(dbPath, src)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "restore": state})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"message": "恢复已暂存，重启服务并完成初始化后生效", "restore": state})
}

// ===== Scheduled Backups =====

// ListScheduledBackups lists scheduled backup tasks
func (h *OpsHandler) ListScheduledBackups(c *gin.Context) {
	var backups []models.ScheduledBackup
	h.db.Order("created_at DESC").Find(&backups)
	c.JSON(http.StatusOK, gin.H{"backups": backups})
}

func (h *OpsHandler) validateScheduledBackup(backup *models.ScheduledBackup) error {
	backup.Name = strings.TrimSpace(backup.Name)
	backup.Cron = strings.TrimSpace(backup.Cron)
	backup.Type = strings.TrimSpace(backup.Type)
	if backup.Type == "" {
		backup.Type = "config"
	}
	if backup.Name == "" {
		return fmt.Errorf("名称不能为空")
	}
	if !scheduler.ValidCronExpression(backup.Cron) {
		return fmt.Errorf("Cron 必须是有效的五段表达式")
	}
	if backup.KeepCount < 1 || backup.KeepCount > 365 {
		return fmt.Errorf("保留数量必须在 1 到 365 之间")
	}
	if backup.ComponentID == nil || *backup.ComponentID == 0 {
		return fmt.Errorf("定时备份必须指定具体组件")
	}
	var component models.Component
	if err := h.db.First(&component, *backup.ComponentID).Error; err != nil {
		return fmt.Errorf("备份组件不存在")
	}
	if message := backupTargetError(&component, backup.Type); message != "" {
		return fmt.Errorf("%s", message)
	}
	clusterID := component.ClusterID
	backup.ClusterID = &clusterID
	return nil
}

// CreateScheduledBackup creates a scheduled backup
func (h *OpsHandler) CreateScheduledBackup(c *gin.Context) {
	var backup models.ScheduledBackup
	if err := c.ShouldBindJSON(&backup); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效请求"})
		return
	}
	if backup.KeepCount <= 0 {
		backup.KeepCount = 7
	}
	if err := h.validateScheduledBackup(&backup); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.db.Create(&backup).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建失败"})
		return
	}
	c.JSON(http.StatusCreated, backup)
}

// UpdateScheduledBackup updates a scheduled backup
func (h *OpsHandler) UpdateScheduledBackup(c *gin.Context) {
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}
	var backup models.ScheduledBackup
	if h.db.First(&backup, id).Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "不存在"})
		return
	}
	var req struct {
		Name        *string `json:"name"`
		Cron        *string `json:"cron"`
		ClusterID   *uint   `json:"cluster_id"`
		ComponentID *uint   `json:"component_id"`
		Type        *string `json:"type"`
		KeepCount   *int    `json:"keep_count"`
		Enabled     *bool   `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "操作失败: " + err.Error()})
		return
	}
	if req.Name != nil {
		backup.Name = *req.Name
	}
	if req.Cron != nil {
		backup.Cron = *req.Cron
	}
	if req.ClusterID != nil {
		backup.ClusterID = req.ClusterID
	}
	if req.ComponentID != nil {
		backup.ComponentID = req.ComponentID
	}
	if req.Type != nil {
		backup.Type = *req.Type
	}
	if req.KeepCount != nil {
		backup.KeepCount = *req.KeepCount
	}
	if req.Enabled != nil {
		backup.Enabled = *req.Enabled
	}
	if err := h.validateScheduledBackup(&backup); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.db.Save(&backup).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "更新失败"})
		return
	}
	c.JSON(http.StatusOK, backup)
}

// DeleteScheduledBackup deletes a scheduled backup
func (h *OpsHandler) DeleteScheduledBackup(c *gin.Context) {
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}
	result := h.db.Delete(&models.ScheduledBackup{}, id)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "删除失败"})
		return
	}
	if result.RowsAffected == 0 {
		utils.HandleNotFoundError(c, "scheduled backup")
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "已删除"})
}
