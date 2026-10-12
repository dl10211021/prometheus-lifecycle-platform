package models

import "time"

// MaintenanceSilence tracks the silence created for one maintenance window on
// one Alertmanager instance. A separate row is required for HA deployments and
// for global windows that span multiple Alertmanager instances.
type MaintenanceSilence struct {
	ID                      uint      `gorm:"primaryKey" json:"id"`
	MaintenanceWindowID     uint      `gorm:"uniqueIndex:idx_window_alertmanager;index" json:"maintenance_window_id"`
	AlertmanagerComponentID uint      `gorm:"uniqueIndex:idx_window_alertmanager;index" json:"alertmanager_component_id"`
	SilenceID               string    `gorm:"size:128;index" json:"silence_id"`
	Status                  string    `gorm:"size:32;index" json:"status"` // active | pending | pending_delete | expired
	DeleteRequested         bool      `gorm:"index;default:false" json:"delete_requested"`
	LastError               string    `gorm:"type:text" json:"last_error,omitempty"`
	LastCheckedAt           time.Time `json:"last_checked_at"`
	CreatedAt               time.Time `json:"created_at"`
	UpdatedAt               time.Time `json:"updated_at"`
}
