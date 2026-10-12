package models

import "time"

// AIInteraction stores non-content metadata for an AI request. Prompts,
// answers, trusted context and credentials are intentionally never persisted.
type AIInteraction struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	ContextType string    `gorm:"size:24;index;not null" json:"context_type"`
	ResourceID  *uint     `gorm:"index" json:"resource_id,omitempty"`
	Provider    string    `gorm:"size:64;index" json:"provider"`
	Model       string    `gorm:"size:128" json:"model"`
	Status      string    `gorm:"size:24;index;not null" json:"status"`
	DurationMS  int64     `json:"duration_ms"`
	PromptHash  string    `gorm:"size:64;index;not null" json:"prompt_hash"`
	CreatedAt   time.Time `gorm:"index" json:"created_at"`
}
