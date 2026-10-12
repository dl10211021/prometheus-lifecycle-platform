package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/credentials"
	"github.com/promlifecycle/platform/pkg/utils"
	"gorm.io/gorm"
)

type queuedPower struct {
	ActorID     uint   `json:"actor_id"`
	ComponentID uint   `json:"component_id"`
	Action      string `json:"action"`
	Fingerprint string `json:"fingerprint"`
	Reason      string `json:"reason"`
}

func (h *Handler) powerActor(id uint) (*models.User, error) {
	var user models.User
	if id == 0 || h.db.First(&user, id).Error != nil || !user.IsActive || (user.Role != "change_admin" && user.Role != "system_admin") {
		return nil, errors.New("启停发起人已无变更权限")
	}
	return &user, nil
}
func (h *Handler) powerTarget(id uint) (*models.Component, error) {
	var c models.Component
	if id == 0 || h.db.Preload("Host").First(&c, id).Error != nil || c.Host.ID == 0 {
		return nil, errors.New("组件或主机不存在")
	}
	if c.Status == "removed" || componentBusy(&c) {
		return nil, errors.New("组件已移除或正在部署/升级")
	}
	if c.DeployType != "binary" && c.DeployType != "windows" {
		return nil, errors.New("仅受管 Linux binary 或 Windows 服务支持启停")
	}
	var adoption models.ComponentAdoption
	err := h.db.Where("component_id = ?", id).First(&adoption).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, errors.New("读取组件所有权失败")
	}
	if err == nil && adoption.Ownership != "managed" {
		return nil, errors.New("观察模式实例不能执行启停")
	}
	return &c, nil
}

// Runtime status is intentionally excluded: start/stop changes it. Bind the
// resource and host address so a queued action cannot follow an edited target.
func powerIdentity(c *models.Component) string {
	raw, _ := json.Marshal([]interface{}{c.ID, c.ClusterID, c.HostID, c.Type, c.DeployType, c.Port, c.Version, c.ServiceName, c.BinaryPath, c.InstallPath, c.ConfigPath, c.DataPath, c.Config, c.CustomArgs, c.ImportedArgs, c.Host.IP, c.Host.Port, c.Host.ConnectionType, c.Host.OS, c.Host.WinRMPort, c.Host.WinRMScheme, c.Host.WinRMSkipVerify})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func (h *Handler) createPowerBatch(c *gin.Context, req deploymentBatchRequest) {
	req.Name = strings.TrimSpace(req.Name)
	req.Reason = strings.TrimSpace(req.Reason)
	if (req.Kind != "start" && req.Kind != "stop") || req.Mode != "" || len(req.Stacks) > 0 || len(req.Upgrades) > 0 || len(req.Adoptions) > 0 || len(req.ComponentIDs) < 1 || len(req.ComponentIDs) > 200 || req.Name == "" || len(req.Name) > 256 || req.Reason == "" || len(req.Reason) > 2000 || len(req.RequestID) < 8 || len(req.RequestID) > 128 || !utils.SafePathSegment(req.RequestID) {
		c.JSON(400, gin.H{"error": "请填写批次名称、操作原因和有效提交标识，并选择 1–200 个受管实例（最多 50 台主机）"})
		return
	}
	actor, err := h.powerActor(changeActor(c).ID)
	if err != nil {
		c.JSON(403, gin.H{"error": err.Error()})
		return
	}
	raw, _ := json.Marshal(req)
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	if h.existingDeploymentBatch(c, req.RequestID, digest) {
		return
	}
	batch := models.DeploymentBatch{Kind: req.Kind, Name: req.Name, RequestID: req.RequestID, RequestDigest: digest}
	positions := map[uint]int{}
	seen := map[uint]bool{}
	for _, id := range req.ComponentIDs {
		if id == 0 || seen[id] {
			c.JSON(400, gin.H{"error": "组件不能重复且必须有效"})
			return
		}
		seen[id] = true
		component, err := h.powerTarget(id)
		if err != nil {
			c.JSON(409, gin.H{"error": fmt.Sprintf("组件 %d：%s", id, err)})
			return
		}
		pos, exists := positions[component.HostID]
		if !exists {
			if len(positions) >= 50 {
				c.JSON(400, gin.H{"error": "一个批次最多包含 50 台主机"})
				return
			}
			pos = len(batch.Stacks)
			positions[component.HostID] = pos
			batch.Stacks = append(batch.Stacks, models.DeploymentStack{HostID: component.HostID, Name: req.Name, TemplateKey: req.Kind, Status: "pending"})
		}
		payload, _ := json.Marshal(queuedPower{ActorID: actor.ID, ComponentID: id, Action: req.Kind, Fingerprint: powerIdentity(component), Reason: req.Reason})
		encrypted, err := credentials.Encrypt(string(payload))
		if err != nil {
			c.JSON(500, gin.H{"error": "无法保护启停队列请求"})
			return
		}
		stack := &batch.Stacks[pos]
		componentID := id
		stack.Steps = append(stack.Steps, models.DeploymentStackStep{Action: req.Kind, Position: len(stack.Steps), Type: string(component.Type), Port: component.Port, Status: "pending", ComponentID: &componentID, EncryptedRequest: encrypted})
		stack.Total = len(stack.Steps)
	}
	err = h.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Omit("Stacks").Create(&batch).Error; err != nil {
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
		c.JSON(503, gin.H{"error": "启停批次未确认入库，请使用相同提交标识查询或重试"})
		return
	}
	summarizeDeploymentBatch(&batch)
	c.JSON(202, gin.H{"batch": batch})
}
