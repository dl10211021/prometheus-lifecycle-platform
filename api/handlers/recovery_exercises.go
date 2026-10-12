package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/services"
	"gorm.io/gorm"
)

type RecoveryExercisesHandler struct {
	db      *gorm.DB
	backups *services.BackupService
}

func NewRecoveryExercisesHandler(db *gorm.DB) *RecoveryExercisesHandler {
	return &RecoveryExercisesHandler{db: db, backups: services.NewBackupService(db)}
}

func (h *RecoveryExercisesHandler) List(c *gin.Context) {
	offset, limit := operationPage(c)
	query := h.db.Model(&models.RecoveryExercise{})
	if status := strings.TrimSpace(c.Query("status")); status != "" {
		query = query.Where("status = ?", status)
	}
	if backupID := strings.TrimSpace(c.Query("backup_id")); backupID != "" {
		query = query.Where("backup_id = ?", backupID)
	}
	var exercises []models.RecoveryExercise
	var total int64
	if err := query.Count(&total).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取演练总数失败"})
		return
	}
	if err := query.Order("created_at DESC, id DESC").Offset(offset).Limit(limit).Find(&exercises).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取恢复演练失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"recovery_exercises": exercises, "offset": offset, "limit": limit, "total": total})
}

func (h *RecoveryExercisesHandler) Get(c *gin.Context) {
	exercise, ok := h.loadExercise(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"recovery_exercise": exercise})
}

func (h *RecoveryExercisesHandler) Create(c *gin.Context) {
	var request struct {
		Name     string `json:"name"`
		BackupID uint   `json:"backup_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&request); err != nil || request.BackupID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "backup_id 为必填项"})
		return
	}
	var backup models.Backup
	if err := h.db.First(&backup, request.BackupID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "备份不存在"})
		return
	}
	if backup.ComponentID == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "平台数据库备份不支持组件恢复演练"})
		return
	}
	var manifest models.BackupManifest
	if err := h.db.Where("backup_id = ?", backup.ID).First(&manifest).Error; err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "备份缺少完整性清单，不能创建演练"})
		return
	}
	name := strings.TrimSpace(request.Name)
	if name == "" {
		name = "恢复演练-" + time.Now().Format("20060102-150405")
	}
	exercise := models.RecoveryExercise{
		Name: name, BackupID: backup.ID, ComponentID: *backup.ComponentID,
		Scope: manifest.Scope, Status: "pending",
	}
	if err := h.db.Create(&exercise).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建恢复演练失败: " + err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"recovery_exercise": exercise})
}

func (h *RecoveryExercisesHandler) Update(c *gin.Context) {
	exercise, ok := h.loadExercise(c)
	if !ok {
		return
	}
	if exercise.Status == "running" {
		c.JSON(http.StatusConflict, gin.H{"error": "运行中的演练不能编辑"})
		return
	}
	var request struct {
		Name string `json:"name" binding:"required"`
	}
	if err := c.ShouldBindJSON(&request); err != nil || strings.TrimSpace(request.Name) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name 为必填项"})
		return
	}
	update := h.db.Model(&exercise).Where("status <> ?", "running").Update("name", strings.TrimSpace(request.Name))
	if update.Error != nil || update.RowsAffected != 1 {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "更新恢复演练失败"})
		return
	}
	exercise.Name = strings.TrimSpace(request.Name)
	c.JSON(http.StatusOK, gin.H{"recovery_exercise": exercise})
}

func (h *RecoveryExercisesHandler) Delete(c *gin.Context) {
	exercise, ok := h.loadExercise(c)
	if !ok {
		return
	}
	if exercise.Status == "running" {
		c.JSON(http.StatusConflict, gin.H{"error": "运行中的演练不能删除"})
		return
	}
	deleted := h.db.Where("status <> ?", "running").Delete(&exercise)
	if deleted.Error != nil || deleted.RowsAffected != 1 {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "删除恢复演练失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "恢复演练已删除"})
}

func (h *RecoveryExercisesHandler) Run(c *gin.Context) {
	exercise, ok := h.loadExercise(c)
	if !ok {
		return
	}
	if exercise.Status == "running" {
		c.JSON(http.StatusConflict, gin.H{"error": "恢复演练正在运行"})
		return
	}
	var backup models.Backup
	if err := h.db.First(&backup, exercise.BackupID).Error; err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "演练关联备份已不存在"})
		return
	}
	var component models.Component
	if err := h.db.Preload("Host").First(&component, exercise.ComponentID).Error; err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "演练关联组件已不存在"})
		return
	}

	startedAt := time.Now()
	claim := h.db.Model(&exercise).Where("status <> ?", "running").Updates(map[string]any{
		"status": "running", "started_at": startedAt, "completed_at": nil,
		"duration_ms": 0, "result": "", "error": "",
	})
	if claim.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "无法启动恢复演练"})
		return
	}
	if claim.RowsAffected != 1 {
		c.JSON(http.StatusConflict, gin.H{"error": "恢复演练正在运行"})
		return
	}
	result, runErr := h.backups.RehearseComponentBackup(c.Request.Context(), &backup, &component)
	completedAt := time.Now()
	duration := completedAt.Sub(startedAt).Milliseconds()
	updates := map[string]any{"completed_at": completedAt, "duration_ms": duration}
	if runErr != nil {
		updates["status"] = "failed"
		updates["error"] = runErr.Error()
		if c.Request.Context().Err() != nil {
			updates["status"] = "interrupted"
		}
		if err := h.db.Model(&exercise).Updates(updates).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "记录演练失败状态失败: " + err.Error()})
			return
		}
		exercise.Status, exercise.Error = updates["status"].(string), runErr.Error()
		exercise.StartedAt, exercise.CompletedAt = &startedAt, &completedAt
		exercise.DurationMS = duration
		c.JSON(http.StatusUnprocessableEntity, gin.H{"recovery_exercise": exercise, "error": runErr.Error()})
		return
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		updates["status"], updates["error"] = "failed", err.Error()
		if persistErr := h.db.Model(&exercise).Updates(updates).Error; persistErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "记录演练失败状态失败: " + persistErr.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "序列化演练证据失败"})
		return
	}
	updates["status"], updates["result"], updates["error"] = "success", string(encoded), ""
	if err := h.db.Model(&exercise).Updates(updates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "记录演练结果失败"})
		return
	}
	exercise.Status, exercise.Result, exercise.Error = "success", string(encoded), ""
	exercise.StartedAt, exercise.CompletedAt = &startedAt, &completedAt
	exercise.DurationMS = duration
	c.JSON(http.StatusOK, gin.H{"recovery_exercise": exercise, "result": result})
}

func (h *RecoveryExercisesHandler) loadExercise(c *gin.Context) (models.RecoveryExercise, bool) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效ID"})
		return models.RecoveryExercise{}, false
	}
	var exercise models.RecoveryExercise
	if err := h.db.First(&exercise, uint(id)).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "恢复演练不存在"})
		return models.RecoveryExercise{}, false
	}
	return exercise, true
}

// RecoverInterruptedExercises makes abandoned runs explicitly retryable; an
// archive rehearsal must never be promoted to successful application recovery.
func RecoverInterruptedExercises(db *gorm.DB) error {
	return db.Model(&models.RecoveryExercise{}).Where("status = ?", "running").Updates(map[string]any{
		"status": "interrupted", "completed_at": time.Now(), "result": "",
		"error": "Server stopped during archive validation; retry the rehearsal. Application recovery was not verified.",
	}).Error
}
