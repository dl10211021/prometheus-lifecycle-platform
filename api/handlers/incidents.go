package handlers

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/services"
	"gorm.io/gorm"
)

type IncidentsHandler struct {
	service *services.IncidentService
}

func NewIncidentsHandler(db *gorm.DB) *IncidentsHandler {
	return &IncidentsHandler{service: services.NewIncidentService(db)}
}

func (h *IncidentsHandler) List(c *gin.Context) {
	limit := queryInt(c, "limit", 50)
	if limit < 1 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	offset := queryInt(c, "offset", 0)
	if offset < 0 {
		offset = 0
	}
	var ownerID *uint
	if raw := strings.TrimSpace(c.Query("owner_id")); raw != "" {
		value, err := strconv.ParseUint(raw, 10, 64)
		if err != nil || value == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "owner_id 无效"})
			return
		}
		parsed := uint(value)
		ownerID = &parsed
	}
	incidents, total, err := h.service.List(strings.TrimSpace(c.Query("status")), strings.TrimSpace(c.Query("kind")), strings.TrimSpace(c.Query("severity")), ownerID, limit, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取事件失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"incidents": incidents, "total": total, "limit": limit, "offset": offset})
}

func (h *IncidentsHandler) Get(c *gin.Context) {
	id, ok := incidentID(c)
	if !ok {
		return
	}
	detail, err := h.service.Detail(c.Request.Context(), id)
	if err != nil {
		writeIncidentError(c, err)
		return
	}
	for index := range detail.RecentChanges {
		detail.RecentChanges[index] = *publicChangePlan(&detail.RecentChanges[index])
	}
	c.JSON(http.StatusOK, detail)
}

func (h *IncidentsHandler) Acknowledge(c *gin.Context) {
	id, ok := incidentID(c)
	if !ok {
		return
	}
	var req struct {
		Owner string `json:"owner"`
	}
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式无效"})
			return
		}
	}
	actorID, actor := incidentActor(c)
	owner := strings.TrimSpace(req.Owner)
	if owner == "" {
		owner = actor
	}
	incident, err := h.service.Acknowledge(id, actorID, actor, owner)
	if err != nil {
		writeIncidentError(c, err)
		return
	}
	c.JSON(http.StatusOK, incident)
}

func (h *IncidentsHandler) Comment(c *gin.Context) {
	id, ok := incidentID(c)
	if !ok {
		return
	}
	var req struct {
		Content string `json:"content"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式无效"})
		return
	}
	if strings.TrimSpace(req.Content) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "评论内容不能为空"})
		return
	}
	actorID, actor := incidentActor(c)
	event, err := h.service.Comment(id, actorID, actor, req.Content)
	if err != nil {
		writeIncidentError(c, err)
		return
	}
	c.JSON(http.StatusCreated, event)
}

func (h *IncidentsHandler) Resolve(c *gin.Context) {
	id, ok := incidentID(c)
	if !ok {
		return
	}
	var req struct {
		Verification string `json:"verification"`
		Evidence     any    `json:"evidence"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式无效"})
		return
	}
	if strings.TrimSpace(req.Verification) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "解决事件必须提交验证说明"})
		return
	}
	actorID, actor := incidentActor(c)
	incident, err := h.service.Resolve(id, actorID, actor, req.Verification, req.Evidence)
	if err != nil {
		writeIncidentError(c, err)
		return
	}
	c.JSON(http.StatusOK, incident)
}

func queryInt(c *gin.Context, name string, fallback int) int {
	value, err := strconv.Atoi(c.Query(name))
	if err != nil {
		return fallback
	}
	return value
}

func incidentID(c *gin.Context) (uint, bool) {
	value, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || value == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "事件 ID 无效"})
		return 0, false
	}
	return uint(value), true
}

func incidentActor(c *gin.Context) (*uint, string) {
	var actorID *uint
	if raw, ok := c.Get("user_id"); ok {
		if value, ok := raw.(uint); ok && value != 0 {
			actorID = &value
		}
	}
	actor := c.GetString("username")
	if actor == "" {
		actor = "unknown"
	}
	return actorID, actor
}

func writeIncidentError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "事件不存在"})
	case strings.Contains(err.Error(), "不能") || strings.Contains(err.Error(), "必须") || strings.Contains(err.Error(), "已经") || strings.Contains(err.Error(), "不能为空"):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "事件操作失败"})
	}
}
