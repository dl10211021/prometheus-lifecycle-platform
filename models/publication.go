package models

import "time"

// PublicationState distinguishes saved desired configuration from verified live configuration.
type PublicationState struct {
	ApplyStatus   string     `gorm:"size:32;default:pending" json:"apply_status"`
	LastError     string     `gorm:"type:text" json:"last_error"`
	AppliedAt     *time.Time `json:"applied_at,omitempty"`
	DeletePending bool       `gorm:"not null;default:false" json:"delete_pending"`
}
