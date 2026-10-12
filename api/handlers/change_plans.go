package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"github.com/promlifecycle/platform/services"
	"gorm.io/gorm"
)

type ChangePlanHandler struct {
	service *services.ChangeService
}

func NewChangePlanHandler(service *services.ChangeService) *ChangePlanHandler {
	return &ChangePlanHandler{service: service}
}

// changeService gives legacy component endpoints the same durable workflow as
// the dedicated ChangePlan API without adding another mutable Handler field.
func (h *Handler) changeService() *services.ChangeService {
	return services.NewChangeService(h.db, h.deployerFactory, h.configManager, services.NewBackupService(h.db), h.basePath)
}

func changeActor(c *gin.Context) services.ChangeActor {
	id, _ := c.Get("user_id")
	userID, _ := id.(uint)
	return services.ChangeActor{ID: userID, Username: c.GetString("username"), Role: c.GetString("role")}
}

func (h *ChangePlanHandler) List(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	if limit < 1 || limit > 200 {
		limit = 50
	}
	if page < 1 {
		page = 1
	}
	offset := (page - 1) * limit
	if raw, present := c.GetQuery("offset"); present {
		var err error
		offset, err = strconv.Atoi(raw)
		if err != nil || offset < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "offset 必须是非负整数"})
			return
		}
		page = offset/limit + 1
	}
	if offset < 0 {
		offset = 0
	}
	plans, total, err := h.service.List(c.Query("status"), c.Query("kind"), offset, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取变更计划失败: " + err.Error()})
		return
	}
	for index := range plans {
		plans[index] = *publicChangePlan(&plans[index])
	}
	c.JSON(http.StatusOK, gin.H{"plans": plans, "total": total, "page": page, "offset": offset, "limit": limit})
}

func (h *ChangePlanHandler) Get(c *gin.Context) {
	raw, allowed := configRawRead(c)
	if !allowed {
		return
	}
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}
	plan, err := h.service.Get(id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			utils.HandleNotFoundError(c, "change plan")
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取变更计划失败: " + err.Error()})
		return
	}
	if raw {
		c.JSON(http.StatusOK, plan)
		return
	}
	c.JSON(http.StatusOK, publicChangePlan(plan))
}

func (h *ChangePlanHandler) Create(c *gin.Context) {
	var req services.ChangePlanRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "变更计划参数无效: " + err.Error()})
		return
	}
	if utils.HasRedactedSecrets(string(req.After)) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "不能保存脱敏预览；请通过授权的 raw=1 配置读取后编辑"})
		return
	}
	plan, err := h.service.Create(c.Request.Context(), req, changeActor(c))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"plan": publicChangePlan(plan)})
}

func (h *ChangePlanHandler) Update(c *gin.Context) {
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}
	var req services.ChangePlanRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "变更计划参数无效: " + err.Error()})
		return
	}
	if utils.HasRedactedSecrets(string(req.After)) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "不能保存脱敏预览；请通过授权的 raw=1 配置读取后编辑"})
		return
	}
	plan, err := h.service.UpdateDraft(c.Request.Context(), id, req, changeActor(c))
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"plan": publicChangePlan(plan)})
}

func (h *ChangePlanHandler) Delete(c *gin.Context) {
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}
	if err := h.service.DeleteDraft(id, changeActor(c)); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "草稿已删除"})
}

func (h *ChangePlanHandler) Execute(c *gin.Context) {
	h.transition(c, func(ctx context.Context, id uint, actor services.ChangeActor) (*models.ChangePlan, error) {
		execCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
		defer cancel()
		return h.service.Execute(execCtx, id, actor)
	})
}

func (h *ChangePlanHandler) Verify(c *gin.Context) {
	h.transition(c, func(ctx context.Context, id uint, actor services.ChangeActor) (*models.ChangePlan, error) {
		verifyCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		return h.service.Verify(verifyCtx, id)
	})
}

func (h *ChangePlanHandler) Rollback(c *gin.Context) {
	h.transition(c, func(ctx context.Context, id uint, actor services.ChangeActor) (*models.ChangePlan, error) {
		rollbackCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
		defer cancel()
		return h.service.Rollback(rollbackCtx, id, actor)
	})
}

func (h *ChangePlanHandler) transition(c *gin.Context, fn func(context.Context, uint, services.ChangeActor) (*models.ChangePlan, error)) {
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}
	plan, err := fn(c.Request.Context(), id, changeActor(c))
	if err != nil {
		status := http.StatusConflict
		if errors.Is(err, gorm.ErrRecordNotFound) {
			status = http.StatusNotFound
		}
		c.JSON(status, gin.H{"error": err.Error(), "plan": publicChangePlan(plan)})
		return
	}
	c.JSON(http.StatusOK, gin.H{"plan": publicChangePlan(plan)})
}

func publicChangePlan(plan *models.ChangePlan) *models.ChangePlan {
	if plan == nil {
		return nil
	}
	copy := *plan
	copy.Before = redactChangePlanJSON(plan.Before)
	copy.After = redactChangePlanJSON(plan.After)
	copy.Diff = "--- before (redacted)\n+++ after (redacted)\n" + string(copy.Before) + "\n" + string(copy.After)
	copy.Verification = utils.SanitizeConfigJSON(plan.Verification)
	copy.RollbackEvidence = utils.SanitizeConfigJSON(plan.RollbackEvidence)
	return &copy
}

func redactChangePlanJSON(raw json.RawMessage) json.RawMessage {
	return utils.SanitizeConfigJSON(raw)
}
