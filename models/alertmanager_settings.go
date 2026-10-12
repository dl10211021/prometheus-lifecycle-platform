package models

import "time"

// AlertmanagerSettings stores the singleton node's generated Alertmanager options.
// JSON-valued fields are validated by the handler before they are persisted.
type AlertmanagerSettings struct {
	ID             uint      `gorm:"primaryKey" json:"id"`
	ClusterID      uint      `gorm:"uniqueIndex;not null" json:"cluster_id"`
	ResolveTimeout string    `gorm:"size:32;not null;default:5m" json:"resolve_timeout"`
	InhibitRules   string    `gorm:"type:text;not null;default:'[]'" json:"-"`
	Templates      string    `gorm:"type:text;not null;default:'[]'" json:"-"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}
