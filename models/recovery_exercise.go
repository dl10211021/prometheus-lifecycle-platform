package models

import "time"

// RecoveryExercise records a non-disruptive restore rehearsal. The backup is
// unpacked and validated in an isolated temporary location; live paths are
// never modified.
type RecoveryExercise struct {
	ID          uint       `gorm:"primaryKey" json:"id"`
	Name        string     `gorm:"size:128;not null" json:"name"`
	BackupID    uint       `gorm:"index;not null" json:"backup_id"`
	ComponentID uint       `gorm:"index;not null" json:"component_id"`
	Scope       string     `gorm:"size:16" json:"scope"`
	Status      string     `gorm:"size:16;index;not null" json:"status"`
	Result      string     `gorm:"type:text" json:"result,omitempty"`
	DurationMS  int64      `json:"duration_ms"`
	Error       string     `gorm:"type:text" json:"error,omitempty"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}
