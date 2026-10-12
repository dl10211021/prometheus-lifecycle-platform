package handlers

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/pkg/utils"
	"github.com/promlifecycle/platform/services"
)

func (h *Handler) GetDeploymentStackAcceptance(c *gin.Context) {
	stackID, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}
	prometheusID, ok := optionalAcceptanceID(c, "prometheus_id")
	if !ok {
		return
	}
	report, err := services.NewDeploymentAcceptanceService(h.db, nil).EvaluateStack(c.Request.Context(), stackID, prometheusID)
	if err != nil {
		writeAcceptanceError(c, err)
		return
	}
	c.JSON(http.StatusOK, report)
}

func (h *Handler) getHostMonitoringStatus(c *gin.Context) {
	hostID, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}
	prometheusID, ok := optionalAcceptanceID(c, "prometheus_id")
	if !ok {
		return
	}
	componentID, ok := optionalAcceptanceID(c, "component_id")
	if !ok {
		return
	}
	report, err := services.NewDeploymentAcceptanceService(h.db, nil).EvaluateHost(c.Request.Context(), hostID, prometheusID, componentID)
	if err != nil {
		writeAcceptanceError(c, err)
		return
	}
	c.JSON(http.StatusOK, report)
}

func optionalAcceptanceID(c *gin.Context, name string) (uint, bool) {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return 0, true
	}
	id, err := strconv.ParseUint(raw, 10, 32)
	if err != nil || id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid " + name})
		return 0, false
	}
	return uint(id), true
}

func writeAcceptanceError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, services.ErrAcceptanceNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
	case errors.Is(err, services.ErrAcceptanceConflict):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "运行验收读取失败"})
	}
}
