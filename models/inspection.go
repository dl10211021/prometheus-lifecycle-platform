package models

import "time"

// InspectionRun groups one explicit full-platform inspection. Status describes
// execution completion; Result describes the observed platform health.
type InspectionRun struct {
	ID                    uint              `gorm:"primaryKey" json:"id"`
	Status                string            `gorm:"size:32;index" json:"status"` // running/completed/failed
	Result                string            `gorm:"size:32;index" json:"result"` // healthy/issues/unknown
	TriggeredBy           string            `gorm:"size:128" json:"triggered_by"`
	ScheduledInspectionID *uint             `gorm:"index" json:"scheduled_inspection_id,omitempty"`
	Total                 int               `json:"total"`
	Passed                int               `json:"passed"`
	Failed                int               `json:"failed"`
	DurationMS            int64             `json:"duration_ms"`
	StartedAt             time.Time         `json:"started_at"`
	CompletedAt           *time.Time        `json:"completed_at,omitempty"`
	Error                 string            `gorm:"type:text" json:"error,omitempty"`
	CreatedAt             time.Time         `json:"created_at"`
	UpdatedAt             time.Time         `json:"updated_at"`
	Checks                []InspectionCheck `gorm:"foreignKey:RunID" json:"checks,omitempty"`
}

// InspectionCheck is the immutable evidence for one component in an
// InspectionRun. OperationID links to the detailed task-stage history.
type InspectionCheck struct {
	ID            uint      `gorm:"primaryKey" json:"id"`
	RunID         uint      `gorm:"index;not null" json:"run_id"`
	OperationID   uint      `gorm:"index" json:"operation_id"`
	ComponentID   uint      `gorm:"index" json:"component_id"`
	Component     string    `gorm:"size:128" json:"component"`
	ComponentType string    `gorm:"size:64" json:"component_type"`
	Host          string    `gorm:"size:128" json:"host"`
	DeployType    string    `gorm:"size:32" json:"deploy_type"`
	Status        string    `gorm:"size:32;index" json:"status"`
	Message       string    `gorm:"type:text" json:"message"`
	Endpoint      string    `gorm:"size:512" json:"endpoint,omitempty"`
	DurationMS    int64     `json:"duration_ms"`
	CheckedAt     time.Time `json:"checked_at"`
	CreatedAt     time.Time `json:"created_at"`
}

// ScheduledInspection defines a minute-level cron schedule for full-platform
// component inspections.
type ScheduledInspection struct {
	ID        uint       `gorm:"primaryKey" json:"id"`
	Name      string     `gorm:"size:128;not null" json:"name"`
	Cron      string     `gorm:"size:64;not null" json:"cron"`
	KeepCount int        `gorm:"default:30" json:"keep_count"`
	Enabled   bool       `gorm:"index" json:"enabled"`
	LastRun   *time.Time `json:"last_run,omitempty"`
	CreatedBy string     `gorm:"size:128" json:"created_by"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// ScheduledInspectionStatus is the current execution evidence for one plan.
type ScheduledInspectionStatus struct {
	ID                    uint                `gorm:"primaryKey" json:"id"`
	ScheduledInspectionID uint                `gorm:"uniqueIndex;not null" json:"scheduled_inspection_id"`
	ScheduledInspection   ScheduledInspection `gorm:"foreignKey:ScheduledInspectionID;constraint:OnDelete:CASCADE" json:"-"`
	LastStatus            string              `gorm:"size:32;not null" json:"last_status"`
	LastResult            string              `gorm:"size:32" json:"last_result"`
	LastError             string              `gorm:"type:text" json:"last_error,omitempty"`
	DurationMS            int64               `json:"duration_ms"`
	InspectionRunID       *uint               `gorm:"index" json:"inspection_run_id,omitempty"`
	LastRun               time.Time           `json:"last_run"`
	CreatedAt             time.Time           `json:"created_at"`
	UpdatedAt             time.Time           `json:"updated_at"`
}

// InspectionNotificationPolicy controls state-change notifications for
// component inspections. A single row is used by the single-node platform.
type InspectionNotificationPolicy struct {
	ID                  uint      `gorm:"primaryKey" json:"id"`
	Enabled             bool      `gorm:"index" json:"enabled"`
	ConsecutiveFailures int       `gorm:"default:2" json:"consecutive_failures"`
	NotifyRecovery      bool      `json:"notify_recovery"`
	ChannelIDs          []uint    `gorm:"serializer:json;type:text" json:"channel_ids"`
	UpdatedBy           string    `gorm:"size:128" json:"updated_by"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

// InspectionNotificationState is the deduplication state for one component.
// Alerting transitions happen only after the configured consecutive failures.
type InspectionNotificationState struct {
	ID                  uint       `gorm:"primaryKey" json:"id"`
	ComponentID         uint       `gorm:"uniqueIndex;not null" json:"component_id"`
	State               string     `gorm:"size:32;index" json:"state"` // healthy/pending_failure/alerting
	LastStatus          string     `gorm:"size:32" json:"last_status"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	LastInspectionRunID uint       `gorm:"index" json:"last_inspection_run_id"`
	OutageRunID         uint       `gorm:"index" json:"outage_run_id"`
	LastMessage         string     `gorm:"type:text" json:"last_message"`
	LastNotifiedAt      *time.Time `json:"last_notified_at,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
}
