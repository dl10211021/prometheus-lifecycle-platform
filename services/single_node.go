package services

import (
	"errors"
	"fmt"

	"github.com/promlifecycle/platform/models"
	"gorm.io/gorm"
)

const singleNodeName = "single-node"

// EnsureSingleNode returns the hidden Cluster row used as the platform's
// internal foreign-key scope. Existing installations reuse their oldest
// cluster so its ID and node settings remain stable across the migration.
func EnsureSingleNode(db *gorm.DB) (*models.Cluster, error) {
	var node models.Cluster
	if err := db.Where("name = ?", singleNodeName).First(&node).Error; err == nil {
		return &node, nil
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("find single node: %w", err)
	}

	if err := db.Order("id ASC").First(&node).Error; err == nil {
		if err := db.Model(&node).Update("name", singleNodeName).Error; err != nil {
			return nil, fmt.Errorf("reuse cluster as single node: %w", err)
		}
		node.Name = singleNodeName
		return &node, nil
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("find cluster for single node: %w", err)
	}

	node = models.Cluster{
		Name:           singleNodeName,
		Environment:    "local",
		DeploymentType: "single-node",
		Status:         "active",
	}
	if err := db.Create(&node).Error; err != nil {
		// A concurrent initializer may have won the unique-name race.
		if findErr := db.Where("name = ?", singleNodeName).First(&node).Error; findErr == nil {
			return &node, nil
		}
		return nil, fmt.Errorf("create single node: %w", err)
	}
	return &node, nil
}

// SingleNodeID resolves the internal singleton scope ID.
func SingleNodeID(db *gorm.DB) (uint, error) {
	node, err := EnsureSingleNode(db)
	if err != nil {
		return 0, err
	}
	return node.ID, nil
}

// MigrateToSingleNode atomically moves every cluster-scoped row to the hidden
// singleton before deleting obsolete Cluster rows. It is safe to run after
// every AutoMigrate and leaves already-migrated databases unchanged.
func MigrateToSingleNode(db *gorm.DB) error {
	return db.Transaction(func(tx *gorm.DB) error {
		node, err := EnsureSingleNode(tx)
		if err != nil {
			return err
		}

		modelsWithRequiredScope := []interface{}{
			&models.Component{},
			&models.AlertRule{},
			&models.AlertHistory{},
			&models.ScrapeConfigModel{},
			&models.RecordingRule{},
			&models.RemoteWriteModel{},
			&models.AlertmanagerRoute{},
			&models.AlertSilence{},
			&models.Dashboard{},
			&models.AlertmanagerSettings{},
		}
		for _, model := range modelsWithRequiredScope {
			if err := tx.Model(model).Where("cluster_id <> ? OR cluster_id IS NULL", node.ID).
				Update("cluster_id", node.ID).Error; err != nil {
				return fmt.Errorf("migrate %T to single node: %w", model, err)
			}
		}

		modelsWithOptionalScope := []interface{}{
			&models.Operation{},
			&models.Backup{},
			&models.NotificationChannel{},
			&models.ScheduledBackup{},
			&models.MaintenanceWindow{},
		}
		for _, model := range modelsWithOptionalScope {
			if err := tx.Model(model).Where("cluster_id IS NOT NULL AND cluster_id <> ?", node.ID).
				Update("cluster_id", node.ID).Error; err != nil {
				return fmt.Errorf("migrate optional %T to single node: %w", model, err)
			}
		}

		// TopologyEdge and MaintenanceSilence inherit their scope through
		// Component IDs and MaintenanceWindow IDs rather than a cluster_id
		// column. Those IDs are preserved above, so neither model needs a
		// rewrite and all rows remain attached to the migrated node.
		if err := tx.Where("id <> ?", node.ID).Delete(&models.Cluster{}).Error; err != nil {
			return fmt.Errorf("delete obsolete clusters: %w", err)
		}
		return nil
	})
}
