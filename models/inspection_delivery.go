package models

import "time"

// InspectionClaim serializes manual and scheduled observations across workers.
type InspectionClaim struct {
	Resource  string    `gorm:"primaryKey;size:64" json:"-"`
	CreatedAt time.Time `json:"created_at"`
}

type InspectionTransition struct {
	ID                  uint      `gorm:"primaryKey" json:"id"`
	ComponentID         uint      `gorm:"index;uniqueIndex:inspection_transition" json:"component_id"`
	RunID               uint      `gorm:"uniqueIndex:inspection_transition" json:"run_id"`
	Event               string    `gorm:"size:32;uniqueIndex:inspection_transition" json:"event"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	Message             string    `gorm:"type:text" json:"message"`
	CreatedAt           time.Time `json:"created_at"`
}

// Acknowledgements belong to a transition AND channel, not to component health.
// Unknown means the process stopped during delivery; automatic resend is unsafe.
type InspectionDelivery struct {
	ID             uint       `gorm:"primaryKey" json:"id"`
	TransitionID   uint       `gorm:"index;uniqueIndex:inspection_delivery" json:"transition_id"`
	ChannelID      uint       `gorm:"uniqueIndex:inspection_delivery" json:"channel_id"`
	Status         string     `gorm:"size:32;index" json:"status"`
	Attempts       int        `json:"attempts"`
	LastError      string     `gorm:"type:text" json:"last_error"`
	NextAttemptAt  time.Time  `json:"next_attempt_at"`
	AcknowledgedAt *time.Time `json:"acknowledged_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// InspectionDeliveryResolution records explicit operator decisions for ambiguous sends.
type InspectionDeliveryResolution struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	DeliveryID uint      `gorm:"index;not null" json:"delivery_id"`
	Resolution string    `gorm:"size:32;not null" json:"resolution"`
	Reason     string    `gorm:"type:text;not null" json:"-"`
	UserID     uint      `json:"user_id"`
	Username   string    `gorm:"size:64" json:"username"`
	CreatedAt  time.Time `json:"created_at"`
}
