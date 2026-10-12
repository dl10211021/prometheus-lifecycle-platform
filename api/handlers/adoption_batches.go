package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/credentials"
	"github.com/promlifecycle/platform/pkg/utils"
	"github.com/promlifecycle/platform/services"
	"gorm.io/gorm"
	"net/http"
	"strings"
	"time"
)

type adoptionRequest struct {
	ClusterID   uint   `json:"cluster_id"`
	Type        string `json:"type" binding:"required"`
	Port        int    `json:"port" binding:"required"`
	Name        string `json:"name"`
	Ownership   string `json:"ownership"`
	Fingerprint string `json:"fingerprint"`
}
type batchAdoptionRequest struct {
	HostID uint `json:"host_id"`
	adoptionRequest
}
type queuedAdoption struct {
	ActorID uint                 `json:"actor_id"`
	Item    batchAdoptionRequest `json:"item"`
}

func (h *Handler) adoptionActor(id uint) (*models.User, error) {
	var user models.User
	if id == 0 || h.db.First(&user, id).Error != nil || !user.IsActive || (user.Role != "change_admin" && user.Role != "system_admin") {
		return nil, errors.New("接管发起人已无变更权限")
	}
	return &user, nil
}
func (h *Handler) createAdoptionBatch(c *gin.Context, req deploymentBatchRequest) {
	req.Name = strings.TrimSpace(req.Name)
	if req.Kind != "adopt" || req.Mode != "" || len(req.Stacks) > 0 || len(req.Upgrades) > 0 || len(req.Adoptions) < 1 || len(req.Adoptions) > 200 || req.Name == "" || len(req.Name) > 256 || len(req.RequestID) < 8 || len(req.RequestID) > 128 || !utils.SafePathSegment(req.RequestID) {
		c.JSON(400, gin.H{"error": "请填写批次名称、有效提交标识，并选择 1–200 个已有实例（最多 50 台主机）"})
		return
	}
	actor, err := h.adoptionActor(changeActor(c).ID)
	if err != nil {
		c.JSON(403, gin.H{"error": err.Error()})
		return
	}
	// Normalize before hashing, and replay before checking mutable host state.
	for i := range req.Adoptions {
		if req.Adoptions[i].Ownership == "" {
			req.Adoptions[i].Ownership = "observe"
		}
	}
	raw, _ := json.Marshal(req)
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	if h.existingDeploymentBatch(c, req.RequestID, digest) {
		return
	}
	batch := models.DeploymentBatch{Kind: "adopt", Name: req.Name, RequestID: req.RequestID, RequestDigest: digest}
	positions := map[uint]int{}
	seen := map[string]bool{}
	for _, item := range req.Adoptions {
		key := fmt.Sprintf("%d:%d", item.HostID, item.Port)
		fingerprint, decodeErr := hex.DecodeString(item.Fingerprint)
		if item.HostID == 0 || seen[key] || !utils.IsValidComponentType(item.Type) || item.Port < 1 || item.Port > 65535 || decodeErr != nil || len(fingerprint) != 32 || len(item.Name) > 256 || (item.Ownership != "managed" && item.Ownership != "observe") {
			c.JSON(400, gin.H{"error": "实例主机、类型、端口、进程指纹或所有权无效；同主机端口不能重复"})
			return
		}
		seen[key] = true
		pos, ok := positions[item.HostID]
		if !ok {
			var host models.Host
			if h.db.First(&host, item.HostID).Error != nil || host.ConnectionType == "winrm" {
				c.JSON(400, gin.H{"error": "请选择存在的 Linux 主机", "host_id": item.HostID})
				return
			}
			if len(positions) >= 50 {
				c.JSON(400, gin.H{"error": "一个批次最多包含 50 台主机"})
				return
			}
			pos = len(batch.Stacks)
			positions[item.HostID] = pos
			batch.Stacks = append(batch.Stacks, models.DeploymentStack{HostID: item.HostID, Name: req.Name, TemplateKey: "adopt", Status: "pending"})
		}
		payload, _ := json.Marshal(queuedAdoption{ActorID: actor.ID, Item: item})
		encrypted, err := credentials.Encrypt(string(payload))
		if err != nil {
			c.JSON(500, gin.H{"error": "无法保护接管队列请求"})
			return
		}
		stack := &batch.Stacks[pos]
		stack.Steps = append(stack.Steps, models.DeploymentStackStep{Action: "adopt", Position: len(stack.Steps), Type: item.Type, Status: "pending", Port: item.Port, Ownership: item.Ownership, EncryptedRequest: encrypted})
		stack.Total = len(stack.Steps)
	}
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
		if h.existingDeploymentBatch(c, req.RequestID, digest) {
			return
		}
		c.JSON(503, gin.H{"error": "接管批次未确认入库，请使用相同提交标识查询或重试"})
		return
	}
	summarizeDeploymentBatch(&batch)
	c.JSON(202, gin.H{"batch": batch})
}

// A committed component link is the durable receipt. It is persisted in the
// same transaction as adoption, so interruption cannot create a second record.
func linkAdoptionStep(ctx context.Context, tx *gorm.DB, componentID uint, result, ownership string) error {
	id, _ := ctx.Value(adoptionStepContextKey{}).(uint)
	if id == 0 {
		return nil
	}
	updated := tx.Model(&models.DeploymentStackStep{}).Where("id = ? AND action = ? AND status = ? AND component_id IS NULL", id, "adopt", "running").Updates(map[string]interface{}{"component_id": componentID, "result": result, "ownership": ownership})
	if updated.Error != nil {
		return updated.Error
	}
	if updated.RowsAffected != 1 {
		return errors.New("adoption queue step changed")
	}
	return nil
}

type adoptionStepContextKey struct{}

// Skip only after matching the live fingerprint and recorded service identity.
func (h *Handler) skipOwnedAdoption(c *gin.Context, host models.Host, existing models.Component, req adoptionRequest) {
	items, err := h.discoverHost(host)
	if err != nil {
		c.JSON(502, gin.H{"error": "无法复核已有实例"})
		return
	}
	matched := false
	for _, item := range items {
		if item.component.Type == req.Type && item.component.Port == req.Port && item.component.Fingerprint == req.Fingerprint &&
			sameDiscoveredIdentity(existing, item.component) {
			matched = true
			break
		}
	}
	if !matched {
		c.JSON(409, gin.H{"error": "已记录组件与所选进程身份不一致，请重新扫描核对"})
		return
	}
	ownership := "platform"
	var adoption models.ComponentAdoption
	if err := h.db.Where("component_id = ?", existing.ID).First(&adoption).Error; err == nil {
		ownership = adoption.Ownership
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(500, gin.H{"error": "读取所有权失败"})
		return
	}
	if err := h.db.Transaction(func(tx *gorm.DB) error {
		return linkAdoptionStep(c.Request.Context(), tx, existing.ID, "already_owned", ownership)
	}); err != nil {
		c.JSON(500, gin.H{"error": "保存接管结果失败"})
		return
	}
	h.logOp(c, "adopt_skip", "component", existing.ID, "批量接管：身份一致，保留已有所有权 "+ownership)
	c.JSON(200, gin.H{"component": existing, "skipped": true, "ownership": ownership})
}

func (h *Handler) runAdoptionStackStep(ctx context.Context, step *models.DeploymentStackStep) error {
	plain, err := credentials.Decrypt(step.EncryptedRequest)
	if err != nil {
		return err
	}
	var queued queuedAdoption
	if err := json.Unmarshal([]byte(plain), &queued); err != nil {
		return err
	}
	var stack models.DeploymentStack
	if err := h.db.First(&stack, step.StackID).Error; err != nil {
		return err
	}
	if stack.HostID != queued.Item.HostID || step.Type != queued.Item.Type || step.Port != queued.Item.Port {
		return errors.New("queued adoption identity changed")
	}
	if step.ComponentID != nil {
		var comp models.Component
		if step.Result != "adopted" && step.Result != "already_owned" {
			return errors.New("invalid adoption receipt")
		}
		if err := h.db.First(&comp, *step.ComponentID).Error; err != nil {
			return err
		}
		if comp.HostID != queued.Item.HostID || comp.Port != queued.Item.Port || string(comp.Type) != queued.Item.Type || comp.Status == "removed" {
			return errors.New("adopted component was removed or changed")
		}
		return nil
	}
	user, err := h.adoptionActor(queued.ActorID)
	if err != nil {
		return &services.StackPreflightFailure{Checks: []string{"接管发起人已无变更权限"}}
	}
	executeCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	executeCtx = context.WithValue(executeCtx, adoptionStepContextKey{}, step.ID)
	raw, _ := json.Marshal(queued.Item.adoptionRequest)
	req, err := http.NewRequestWithContext(executeCtx, http.MethodPost, fmt.Sprintf("http://internal/hosts/%d/adopt", queued.Item.HostID), bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	response := &deploymentResponse{header: make(http.Header)}
	engine := gin.New()
	engine.POST("/hosts/:id/adopt", func(c *gin.Context) {
		c.Set("user_id", user.ID)
		c.Set("username", user.Username)
		c.Set("role", user.Role)
		h.AdoptComponent(c)
	})
	engine.ServeHTTP(response, req)
	if response.status < 200 || response.status >= 300 {
		check := map[int]string{400: "接管参数无效", 404: "主机或环境已不存在", 409: "主机忙或进程身份变化，请重新扫描核对", 422: "原服务不满足所选接管模式，请核对进程、协议和服务配置", 502: "无法连接主机或读取运行进程", 500: "接管记录未保存，请检查平台存储"}[response.status]
		if check == "" {
			check = "接管未完成，请核对原服务"
		}
		return &services.StackPreflightFailure{Checks: []string{check}}
	}
	var receipt models.DeploymentStackStep
	if err := h.db.First(&receipt, step.ID).Error; err != nil {
		return err
	}
	if receipt.ComponentID == nil || (receipt.Result != "adopted" && receipt.Result != "already_owned") {
		return errors.New("adoption receipt missing")
	}
	step.ComponentID = receipt.ComponentID
	step.Result = receipt.Result
	step.Ownership = receipt.Ownership
	return nil
}
