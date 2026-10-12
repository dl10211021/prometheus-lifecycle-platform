package models

import "time"

// ComponentAdoption records how an existing process is owned by the platform.
// Observe ownership is intentionally non-mutating; managed ownership is only
// granted after a successful preflight and a takeover snapshot.
type ComponentAdoption struct {
	ID               uint      `gorm:"primaryKey" json:"id"`
	ComponentID      uint      `gorm:"uniqueIndex;not null" json:"component_id"`
	HostID           uint      `gorm:"index;not null" json:"host_id"`
	Ownership        string    `gorm:"size:16;index;not null" json:"ownership"`
	SnapshotPath     string    `gorm:"size:512" json:"snapshot_path,omitempty"`
	SnapshotChecksum string    `gorm:"size:64" json:"snapshot_checksum,omitempty"`
	Discovery        string    `gorm:"type:text" json:"discovery,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// OnboardingCheckRun stores evidence from an acceptance run. Status pages may
// reuse a recent notification result, but targets, rules and backups are always
// probed again so object existence can never masquerade as acceptance.
type OnboardingCheckRun struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Category  string    `gorm:"size:32;index;not null" json:"category"`
	Status    string    `gorm:"size:16;index;not null" json:"status"`
	Reason    string    `gorm:"type:text" json:"reason"`
	Evidence  string    `gorm:"type:text" json:"evidence,omitempty"`
	CheckedAt time.Time `gorm:"index;not null" json:"checked_at"`
}
