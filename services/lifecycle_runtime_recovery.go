package services

import (
	"context"
	"github.com/promlifecycle/platform/core/deployer"
	"github.com/promlifecycle/platform/models"
	"gorm.io/gorm"
	"time"
)

// ReconcileInterruptedRuntime records health evidence only. A ready service
// does not make a partially applied change/deployment operation successful.
func ReconcileInterruptedRuntime(ctx context.Context, db *gorm.DB, basePath string) error {
	var components []models.Component
	if err := db.Preload("Host").Where("status <> ? AND id IN (?)", "removed", db.Model(&models.Operation{}).Select("component_id").Where("status = ? AND component_id IS NOT NULL", "interrupted")).Find(&components).Error; err != nil {
		return err
	}
	for i := range components {
		component := &components[i]
		if ctx.Err() != nil {
			return ctx.Err()
		}
		probe, cancel := context.WithTimeout(ctx, 15*time.Second)
		var healthErr error
		switch component.DeployType {
		case "binary":
			healthErr = deployer.NewBinaryDeployer(db, basePath).HealthCheck(probe, component)
		case "windows":
			healthErr = deployer.NewWindowsDeployer(db, basePath).HealthCheck(probe, component)
		default:
			cancel()
			continue
		}
		cancel()
		status := models.StatusError
		message := "Interrupted operation: runtime readiness could not be verified; explicit recovery or retry required"
		if healthErr == nil {
			status = models.StatusRunning
			message = "Interrupted operation: runtime is ready, but lifecycle/integration completion is unverified; explicit retry required"
		}
		if err := db.Model(component).Updates(map[string]interface{}{"status": status, "error": message}).Error; err != nil {
			return err
		}
		if err := db.Model(&models.Operation{}).Where("component_id = ? AND status = ?", component.ID, "interrupted").Update("message", message).Error; err != nil {
			return err
		}
	}
	return nil
}
