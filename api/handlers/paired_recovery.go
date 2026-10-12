package handlers

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"github.com/promlifecycle/platform/services"
	"gorm.io/gorm"
)

type pairedRecoveryService interface {
	GetPrometheusPairedRestoreTransaction(context.Context, uint) (*models.PrometheusRestoreTransaction, error)
	PreviewPrometheusRecoveryPointRestore(context.Context, uint) (*services.PrometheusPairedRestorePreview, error)
	RestorePrometheusRecoveryPoint(context.Context, uint) (*models.PrometheusRestoreTransaction, error)
}

type PairedRecoveryHandler struct {
	service pairedRecoveryService
}

func NewPairedRecoveryHandler(service pairedRecoveryService) *PairedRecoveryHandler {
	return &PairedRecoveryHandler{service: service}
}

func RegisterPairedRecoveryRoutes(read, write gin.IRoutes, handler *PairedRecoveryHandler) {
	read.GET("/change-plans/:id/paired-recovery", handler.Get)
	write.POST("/change-plans/:id/paired-recovery", handler.Restore)
}

type publicPairedRecoveryTransaction struct {
	ID          uint       `json:"id"`
	PlanID      uint       `json:"plan_id"`
	PointID     uint       `json:"point_id"`
	ComponentID uint       `json:"component_id"`
	HostID      uint       `json:"host_id"`
	Phase       string     `json:"phase"`
	Revision    uint64     `json:"revision"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
}

type publicPairedRecoveryPreview struct {
	PointID                   uint                              `json:"point_id"`
	PlanID                    uint                              `json:"plan_id"`
	ComponentID               uint                              `json:"component_id"`
	HostID                    uint                              `json:"host_id"`
	CurrentVersion            string                            `json:"current_version"`
	HistoricalVersion         string                            `json:"historical_version"`
	Scope                     string                            `json:"scope"`
	RestoreAuthorized         bool                              `json:"restore_authorized"`
	RequiresDowntime          bool                              `json:"requires_downtime"`
	RequiresCurrentCheckpoint bool                              `json:"requires_current_checkpoint"`
	ReplacementCount          int                               `json:"replacement_count"`
	Dependencies              []services.DependentCompatibility `json:"dependencies"`
	Checks                    []string                          `json:"checks"`
	ObservedAt                time.Time                         `json:"observed_at"`
}

func publicPairedTransaction(transaction *models.PrometheusRestoreTransaction) *publicPairedRecoveryTransaction {
	if transaction == nil {
		return nil
	}
	return &publicPairedRecoveryTransaction{
		ID: transaction.ID, PlanID: transaction.PlanID, PointID: transaction.PointID,
		ComponentID: transaction.ComponentID, HostID: transaction.HostID,
		Phase: transaction.Phase, Revision: transaction.Revision,
		CreatedAt: transaction.CreatedAt, UpdatedAt: transaction.UpdatedAt, FinishedAt: transaction.FinishedAt,
	}
}

func publicPairedPreview(preview *services.PrometheusPairedRestorePreview) *publicPairedRecoveryPreview {
	if preview == nil {
		return nil
	}
	return &publicPairedRecoveryPreview{
		PointID: preview.PointID, PlanID: preview.PlanID,
		ComponentID: preview.ComponentID, HostID: preview.HostID,
		CurrentVersion: preview.CurrentVersion, HistoricalVersion: preview.HistoricalVersion,
		Scope: preview.Scope, RestoreAuthorized: preview.RestoreAuthorized,
		RequiresDowntime:          preview.RequiresDowntime,
		RequiresCurrentCheckpoint: preview.RequiresCurrentCheckpoint,
		ReplacementCount:          len(preview.Replacements),
		Dependencies:              preview.Dependencies, Checks: preview.Checks, ObservedAt: preview.ObservedAt,
	}
}

func pairedRecoveryPlanID(c *gin.Context) (uint, bool) {
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return 0, false
	}
	if id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id: 0"})
		return 0, false
	}
	return id, true
}

func (h *PairedRecoveryHandler) Get(c *gin.Context) {
	id, ok := pairedRecoveryPlanID(c)
	if !ok {
		return
	}
	transaction, err := h.service.GetPrometheusPairedRestoreTransaction(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			utils.HandleNotFoundError(c, "change plan")
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "paired recovery status unavailable"})
		return
	}
	if transaction != nil {
		c.JSON(http.StatusOK, gin.H{"eligible": true, "transaction": publicPairedTransaction(transaction)})
		return
	}
	preview, err := h.service.PreviewPrometheusRecoveryPointRestore(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, services.ErrPrometheusPairedRecoveryPointNotFound) {
			c.JSON(http.StatusOK, gin.H{"eligible": false})
			return
		}
		if errors.Is(err, gorm.ErrRecordNotFound) {
			utils.HandleNotFoundError(c, "change plan")
			return
		}
		c.JSON(http.StatusOK, gin.H{"eligible": false})
		return
	}
	if preview == nil {
		c.JSON(http.StatusOK, gin.H{"eligible": false})
		return
	}
	c.JSON(http.StatusOK, gin.H{"eligible": true, "preview": publicPairedPreview(preview)})
}

func (h *PairedRecoveryHandler) Restore(c *gin.Context) {
	id, ok := pairedRecoveryPlanID(c)
	if !ok {
		return
	}
	transaction, err := h.service.RestorePrometheusRecoveryPoint(c.Request.Context(), id)
	if err == nil {
		c.JSON(http.StatusOK, gin.H{"transaction": publicPairedTransaction(transaction)})
		return
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		utils.HandleNotFoundError(c, "change plan")
		return
	}
	message := "paired recovery did not complete; refresh the durable transaction before any further action"
	if transaction != nil && transaction.Phase == "restore_compensated" {
		message = "historical restore failed; the previous current state was recovered"
	}
	c.JSON(http.StatusConflict, gin.H{"error": message, "transaction": publicPairedTransaction(transaction)})
}
