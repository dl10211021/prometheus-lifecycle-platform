package models

import "time"

// DeploymentBatch groups durable, independent per-host stacks.
type DeploymentBatch struct {
	Kind          string                 `gorm:"size:16" json:"kind,omitempty"`
	ID            uint                   `gorm:"primaryKey" json:"id"`
	Name          string                 `gorm:"size:256;not null" json:"name"`
	RequestID     string                 `gorm:"size:128;uniqueIndex;not null" json:"request_id"`
	RequestDigest string                 `gorm:"size:64;not null" json:"-"`
	Stacks        []DeploymentStack      `gorm:"-" json:"stacks"`
	Status        string                 `gorm:"-" json:"status"`
	Summary       DeploymentBatchSummary `gorm:"-" json:"summary"`
	CreatedAt     time.Time              `json:"created_at"`
	UpdatedAt     time.Time              `json:"updated_at"`
}
type DeploymentBatchSummary struct {
	Total          int `json:"total"`
	Pending        int `json:"pending"`
	Running        int `json:"running"`
	Success        int `json:"success"`
	Failed         int `json:"failed"`
	Interrupted    int `json:"interrupted"`
	Unknown        int `json:"unknown"`
	CompletedSteps int `json:"completed_steps"`
	TotalSteps     int `json:"total_steps"`
}
