package models

import (
	"encoding/json"
	"time"
)

// PrometheusRecoveryPoint pins historical evidence, not a client-supplied
// restore instruction. Only capture state changes after payload publication.
type PrometheusRecoveryPoint struct {
	ID          uint            `gorm:"primaryKey" json:"id"`
	PlanID      uint            `gorm:"uniqueIndex;not null" json:"plan_id"`
	ComponentID uint            `gorm:"index;not null" json:"component_id"`
	HostID      uint            `gorm:"not null" json:"host_id"`
	BackupID    uint            `gorm:"uniqueIndex;not null" json:"backup_id"`
	State       string          `gorm:"size:24;not null" json:"state"`
	Payload     json.RawMessage `gorm:"serializer:json;type:text;not null" json:"-"`
	CreatedAt   time.Time       `json:"created_at"`
}
