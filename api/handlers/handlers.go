package handlers

import (
	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/core/config"
	"github.com/promlifecycle/platform/core/deployer"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/services"
	"gorm.io/gorm"
)

type Handler struct {
	opsHandler      *OperationsHandler
	db              *gorm.DB
	deployerFactory *deployer.Factory
	configManager   *config.Manager
	alertSync       *services.AlertSyncService
	versionSvc      *services.VersionService
	autoConfig      *config.AutoConfig
	basePath        string
}

func NewHandler(db *gorm.DB, deployerFactory *deployer.Factory, configManager *config.Manager, alertSync *services.AlertSyncService, autoConfig *config.AutoConfig, basePath string) *Handler {
	h := &Handler{
		db:              db,
		deployerFactory: deployerFactory,
		configManager:   configManager,
		alertSync:       alertSync,
		versionSvc:      services.NewVersionService(db),
		autoConfig:      autoConfig,
		basePath:        basePath,
	}

	h.opsHandler = NewOperationsHandler(db)
	return h
}

// Helper: log operation
// componentBusy reports whether a component is mid-lifecycle (deploying or
// upgrading). Concurrent uninstall/delete/edit/restart during those phases
// would race the deployer goroutine and leave orphaned services or a half
// swapped binary.
func componentBusy(component *models.Component) bool {
	return component.Status == models.StatusDeploying || component.Status == models.StatusUpgrading
}

func (h *Handler) logOp(c *gin.Context, action, resource string, resourceID uint, description string) {
	// middleware stores user_id/username in context (not the full user object)
	userID, ok1 := c.Get("user_id")
	username, ok2 := c.Get("username")
	if !ok1 || !ok2 {
		return
	}
	uid, okU := userID.(uint)
	uname, okN := username.(string)
	if !okU || !okN {
		return
	}
	h.opsHandler.CreateOperationLog(uid, uname, action, resource, resourceID, description, c.ClientIP(), c.Request.UserAgent())
}

func (h *Handler) createOperation(componentID uint, clusterID *uint, opType, status, source string, input interface{}) *models.Operation {
	return services.NewOperationService(h.db).Create(componentID, clusterID, opType, status, input)
}

func (h *Handler) updateOperationProgress(id uint, stage, message string, progress int) {
	services.NewOperationService(h.db).Progress(id, stage, message, progress)
}

func (h *Handler) updateOperation(id uint, success bool, message string, err error) {
	services.NewOperationService(h.db).Complete(id, success, message, err)
}

func (h *Handler) claimComponentRequest(c *gin.Context, id uint, kind string) (func(), bool) {
	claimOperation := services.ClaimComponentOperation
	if kind == "upgrade" || kind == "rollback" {
		claimOperation = services.ClaimComponentVersionChange
	}
	ctx, release, err := claimOperation(c.Request.Context(), h.db, id, kind)
	if err != nil {
		c.JSON(409, gin.H{"error": err.Error()})
		return nil, false
	}
	var retired int64
	if err := h.db.Model(&models.Component{}).Where("id = ? AND status = ?", id, "removed").Count(&retired).Error; err != nil {
		release()
		c.JSON(500, gin.H{"error": err.Error()})
		return nil, false
	}
	if retired != 0 {
		release()
		c.JSON(409, gin.H{"error": "组件已移除；请使用备份恢复，而不是修改退休记录"})
		return nil, false
	}
	c.Request = c.Request.WithContext(ctx)
	return release, true
}
