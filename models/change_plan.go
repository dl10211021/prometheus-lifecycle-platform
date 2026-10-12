package models

import (
	"encoding/json"
	"time"
)

// ChangeKind is the supported class of managed component mutation.
type ChangeKind string

const (
	ChangeKindConfig  ChangeKind = "config"
	ChangeKindUpgrade ChangeKind = "upgrade"
	ChangeKindRemove  ChangeKind = "remove"
)

const (
	ChangeRiskLow    = "low"
	ChangeRiskMedium = "medium"
	ChangeRiskHigh   = "high"
)

// ChangeStatus is the durable execution state of a change record.
type ChangeStatus string

const (
	ChangeStatusDraft ChangeStatus = "draft"
	// Pending and approved are retained only so existing databases can execute
	// records created by older releases. New changes go directly draft → executing.
	ChangeStatusPending    ChangeStatus = "pending"
	ChangeStatusApproved   ChangeStatus = "approved"
	ChangeStatusExecuting  ChangeStatus = "executing"
	ChangeStatusVerified   ChangeStatus = "verified"
	ChangeStatusFailed     ChangeStatus = "failed"
	ChangeStatusRolledBack ChangeStatus = "rolled_back"
)

// ChangePlan records intent, recovery point, execution and evidence for every
// configuration, upgrade and removal mutation.
type ChangePlan struct {
	QueueManaged          bool            `gorm:"not null;default:false" json:"queue_managed,omitempty"`
	ID                    uint            `gorm:"primaryKey" json:"id"`
	Kind                  ChangeKind      `gorm:"size:16;index;not null" json:"kind"`
	ResourceType          string          `gorm:"size:32;index;not null" json:"resource_type"`
	ResourceID            uint            `gorm:"index;not null" json:"resource_id"`
	ResourceName          string          `gorm:"size:256" json:"resource_name"`
	RequestedByID         uint            `gorm:"index;not null" json:"requested_by_id"`
	RequestedBy           string          `gorm:"size:128;not null" json:"requested_by"`
	ApprovedByID          *uint           `gorm:"index" json:"approved_by_id,omitempty"`
	ApprovedBy            string          `gorm:"size:128" json:"approved_by,omitempty"`
	Status                ChangeStatus    `gorm:"size:24;index;not null" json:"status"`
	Reason                string          `gorm:"type:text;not null" json:"reason"`
	Before                json.RawMessage `gorm:"type:text;serializer:json;not null" json:"before"`
	After                 json.RawMessage `gorm:"type:text;serializer:json;not null" json:"after"`
	Diff                  string          `gorm:"type:text;not null" json:"diff"`
	Risk                  string          `gorm:"size:16;index;not null" json:"risk"`
	BackupID              *uint           `gorm:"index" json:"backup_id,omitempty"`
	PairedRecoveryPointID *uint           `gorm:"index" json:"paired_recovery_point_id,omitempty"`
	OperationID           *uint           `gorm:"index" json:"operation_id,omitempty"`
	Verification          json.RawMessage `gorm:"type:text;serializer:json" json:"verification,omitempty"`
	RollbackEvidence      json.RawMessage `gorm:"type:text;serializer:json" json:"rollback_evidence,omitempty"`
	RequestedAt           time.Time       `gorm:"index;not null" json:"requested_at"`
	ApprovedAt            *time.Time      `json:"approved_at,omitempty"`
	ExecutedAt            *time.Time      `json:"executed_at,omitempty"`
	VerifiedAt            *time.Time      `json:"verified_at,omitempty"`
	RolledBackAt          *time.Time      `json:"rolled_back_at,omitempty"`
	CreatedAt             time.Time       `json:"created_at"`
	UpdatedAt             time.Time       `json:"updated_at"`
}
