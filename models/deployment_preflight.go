package models

import "time"

// DeploymentPreflight is a short-lived, single-use attestation that binds a
// successful dry run to the exact normalized deployment request.
type DeploymentPreflight struct {
	Token          string     `gorm:"primaryKey;size:64" json:"token"`
	RequestHash    string     `gorm:"size:64;index;not null" json:"request_hash"`
	ComponentType  string     `gorm:"size:64;index;not null" json:"component_type"`
	DefinitionHash string     `gorm:"size:64;not null" json:"definition_hash"`
	HostID         uint       `gorm:"index;not null" json:"host_id"`
	Port           int        `json:"port"`
	CreatedBy      string     `gorm:"size:128" json:"created_by"`
	Evidence       string     `gorm:"type:text" json:"evidence,omitempty"`
	ExpiresAt      time.Time  `gorm:"index;not null" json:"expires_at"`
	ConsumedAt     *time.Time `gorm:"index" json:"consumed_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
}
