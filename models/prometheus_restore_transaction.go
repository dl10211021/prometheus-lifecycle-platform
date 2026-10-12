package models

import (
	"encoding/json"
	"time"
)

// PrometheusRestoreTransaction owns a durable host/component fence. This
// protocol preserves admission and private, phase-bound checkpoint evidence.
type PrometheusRestoreTransaction struct {
	ID                uint            `gorm:"primaryKey" json:"id"`
	PlanID            uint            `gorm:"index;not null" json:"plan_id"`
	PointID           uint            `gorm:"index;not null" json:"point_id"`
	BackupID          uint            `gorm:"not null" json:"backup_id"`
	ComponentID       uint            `gorm:"index;not null" json:"component_id"`
	HostID            uint            `gorm:"index;not null" json:"host_id"`
	ActiveComponentID *uint           `gorm:"uniqueIndex" json:"-"`
	ActiveHostID      *uint           `gorm:"uniqueIndex" json:"-"`
	Token             string          `gorm:"size:64;uniqueIndex;not null" json:"-"`
	Phase             string          `gorm:"size:32;index;not null" json:"phase"`
	Revision          uint64          `gorm:"not null" json:"revision"`
	Payload           json.RawMessage `gorm:"serializer:json;type:text;not null" json:"-"`
	PayloadSHA256     string          `gorm:"size:64;not null" json:"-"`
	Checkpoint        json.RawMessage `gorm:"serializer:json;type:text" json:"-"`
	CheckpointSHA256  string          `gorm:"size:64" json:"-"`
	Stage             json.RawMessage `gorm:"serializer:json;type:text" json:"-"`
	StageSHA256       string          `gorm:"size:64" json:"-"`
	Execution         json.RawMessage `gorm:"serializer:json;type:text" json:"-"`
	ExecutionSHA256   string          `gorm:"size:64" json:"-"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
	FinishedAt        *time.Time      `json:"finished_at,omitempty"`
}
