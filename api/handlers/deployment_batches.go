package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"gorm.io/gorm"
)

type deploymentBatchRequest struct {
	Mode         string                   `json:"mode,omitempty"`
	ComponentIDs []uint                   `json:"component_ids,omitempty"`
	Adoptions    []batchAdoptionRequest   `json:"adoptions,omitempty"`
	Name         string                   `json:"name"`
	RequestID    string                   `json:"request_id"`
	Stacks       []deploymentStackRequest `json:"stacks"`
	Kind         string                   `json:"kind,omitempty"`
	Reason       string                   `json:"reason,omitempty"`
	Upgrades     []batchUpgradeRequest    `json:"upgrades,omitempty"`
}

func (h *Handler) readDeploymentBatch(id uint) (*models.DeploymentBatch, error) {
	var batch models.DeploymentBatch
	err := h.db.First(&batch, id).Error
	if err != nil {
		return nil, err
	}
	if err := h.db.Preload("Steps", func(tx *gorm.DB) *gorm.DB { return tx.Order("position ASC") }).
		Where("batch_id = ?", id).Order("id ASC").Find(&batch.Stacks).Error; err != nil {
		return nil, err
	}
	summarizeDeploymentBatch(&batch)
	return &batch, nil
}

func summarizeDeploymentBatch(batch *models.DeploymentBatch) {
	summary := models.DeploymentBatchSummary{Total: len(batch.Stacks)}
	for _, stack := range batch.Stacks {
		switch stack.Status {
		case "pending":
			summary.Pending++
		case "running":
			summary.Running++
		case "success":
			summary.Success++
		case "failed":
			summary.Failed++
		case "interrupted":
			summary.Interrupted++
		default:
			summary.Unknown++
		}
		summary.TotalSteps += len(stack.Steps)
		for _, step := range stack.Steps {
			if step.Status == "success" {
				summary.CompletedSteps++
			}
		}
	}
	batch.Summary = summary
	switch {
	case summary.Total == 0 || summary.Unknown > 0:
		batch.Status = "unknown"
	case summary.Running > 0:
		batch.Status = "running"
	case summary.Pending == summary.Total:
		batch.Status = "pending"
	case summary.Pending > 0:
		batch.Status = "running"
	case summary.Success == summary.Total:
		batch.Status = "success"
	case summary.Success > 0:
		batch.Status = "partial"
	case summary.Failed > 0:
		batch.Status = "failed"
	default:
		batch.Status = "interrupted"
	}
}

func (h *Handler) existingDeploymentBatch(c *gin.Context, key, digest string) bool {
	var existing models.DeploymentBatch
	if err := h.db.Where("request_id = ?", key).First(&existing).Error; err != nil {
		return false
	}
	if existing.RequestDigest != digest {
		c.JSON(http.StatusConflict, gin.H{"error": "该提交标识已用于其他部署内容，请重新生成批次"})
		return true
	}
	batch, err := h.readDeploymentBatch(existing.ID)
	if err != nil {
		c.JSON(500, gin.H{"error": "读取已保存批次失败，请刷新任务列表"})
		return true
	}
	c.JSON(http.StatusOK, gin.H{"batch": batch, "replayed": true})
	return true
}

func (h *Handler) CreateDeploymentBatch(c *gin.Context) {
	var req deploymentBatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": "批次请求格式无效"})
		return
	}
	if req.Kind == "start" || req.Kind == "stop" {
		h.createPowerBatch(c, req)
		return
	}
	if req.Kind == "remove" {
		h.createRemovalBatch(c, req)
		return
	}
	if len(req.ComponentIDs) > 0 {
		c.JSON(400, gin.H{"error": "批次类型与实例列表不匹配"})
		return
	}
	if req.Kind == "adopt" {
		h.createAdoptionBatch(c, req)
		return
	}
	if req.Kind == "upgrade" {
		h.createUpgradeBatch(c, req)
		return
	}
	if req.Kind != "" || req.Mode != "" || len(req.Upgrades) != 0 || len(req.Adoptions) != 0 {
		c.JSON(400, gin.H{"error": "批次类型无效"})
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 256 || len(req.RequestID) < 8 || len(req.RequestID) > 128 || !utils.SafePathSegment(req.RequestID) || len(req.Stacks) < 1 || len(req.Stacks) > 50 {
		c.JSON(400, gin.H{"error": "请填写批次名称、有效提交标识，并选择 1–50 台主机"})
		return
	}
	encoded, err := json.Marshal(req)
	if err != nil {
		c.JSON(400, gin.H{"error": "批次请求无法编码"})
		return
	}
	digestBytes := sha256.Sum256(encoded)
	digest := hex.EncodeToString(digestBytes[:])
	if h.existingDeploymentBatch(c, req.RequestID, digest) {
		return
	}
	hosts := make(map[uint]bool, len(req.Stacks))
	batch := models.DeploymentBatch{Name: req.Name, RequestID: req.RequestID, RequestDigest: digest}
	for index, request := range req.Stacks {
		if request.HostID == 0 || hosts[request.HostID] {
			c.JSON(400, gin.H{"error": "批次主机不能为空或重复", "index": index})
			return
		}
		hosts[request.HostID] = true
		stack, err := h.prepareDeploymentStack(request)
		if err != nil {
			c.JSON(err.Status, gin.H{"error": err.Message, "host_id": request.HostID, "index": index, "checks": err.Checks})
			return
		}
		batch.Stacks = append(batch.Stacks, *stack)
	}
	// The batch and every host task become visible together. Execution still uses
	// the existing durable worker and repeats live preflight before each step.
	err = h.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&batch).Error; err != nil {
			return err
		}
		for i := range batch.Stacks {
			batch.Stacks[i].BatchID = &batch.ID
		}
		return tx.Create(&batch.Stacks).Error
	})
	if err != nil {
		// A concurrent replay may have won the unique request_id constraint.
		if h.existingDeploymentBatch(c, req.RequestID, digest) {
			return
		}
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "批次未确认入库，请使用相同提交标识重试"})
		return
	}
	summarizeDeploymentBatch(&batch)
	c.JSON(http.StatusAccepted, gin.H{"batch": batch})
}

func (h *Handler) GetDeploymentBatch(c *gin.Context) {
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}
	batch, err := h.readDeploymentBatch(id)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(404, gin.H{"error": "批次不存在"})
		return
	}
	if err != nil {
		c.JSON(500, gin.H{"error": "读取批次失败"})
		return
	}
	c.JSON(200, gin.H{"batch": batch})
}

func (h *Handler) ListDeploymentBatches(c *gin.Context) {
	var batches []models.DeploymentBatch
	query := h.db.Order("id DESC").Limit(50)
	if requestID := c.Query("request_id"); requestID != "" {
		query = query.Where("request_id = ?", requestID)
	}
	if err := query.Find(&batches).Error; err != nil {
		c.JSON(500, gin.H{"error": "读取批次列表失败"})
		return
	}
	if len(batches) > 0 {
		ids := make([]uint, 0, len(batches))
		positions := make(map[uint]int, len(batches))
		for i := range batches {
			ids = append(ids, batches[i].ID)
			positions[batches[i].ID] = i
			batches[i].Stacks = []models.DeploymentStack{}
		}
		var stacks []models.DeploymentStack
		if err := h.db.Preload("Steps", func(tx *gorm.DB) *gorm.DB { return tx.Order("position ASC") }).
			Where("batch_id IN ?", ids).Order("id ASC").Find(&stacks).Error; err != nil {
			c.JSON(500, gin.H{"error": "读取批次主机任务失败"})
			return
		}
		for _, stack := range stacks {
			if stack.BatchID != nil {
				if index, ok := positions[*stack.BatchID]; ok {
					batches[index].Stacks = append(batches[index].Stacks, stack)
				}
			}
		}
	}
	for i := range batches {
		summarizeDeploymentBatch(&batches[i])
	}
	c.JSON(200, gin.H{"batches": batches})
}

func (h *Handler) RetryDeploymentBatch(c *gin.Context) {
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}
	var retried []uint
	err := h.db.Transaction(func(tx *gorm.DB) error {
		var batch models.DeploymentBatch
		if err := tx.First(&batch, id).Error; err != nil {
			return err
		}
		var stacks []models.DeploymentStack
		if err := tx.Where("batch_id = ? AND status IN ?", id, []string{"failed", "interrupted"}).Order("id").Find(&stacks).Error; err != nil {
			return err
		}
		if len(stacks) == 0 {
			return errors.New("no retryable host tasks")
		}
		for _, stack := range stacks {
			if err := retryDeploymentStack(tx, stack.ID); err != nil {
				return err
			}
			retried = append(retried, stack.ID)
		}
		return nil
	})
	if errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(404, gin.H{"error": "批次不存在"})
		return
	}
	if err != nil {
		c.JSON(409, gin.H{"error": "没有可重试任务，或任务状态已变化；请刷新批次"})
		return
	}
	batch, err := h.readDeploymentBatch(id)
	if err != nil {
		c.JSON(500, gin.H{"error": "重试已提交，读取状态失败；请刷新批次"})
		return
	}
	c.JSON(202, gin.H{"batch": batch, "retried_stack_ids": retried})
}
