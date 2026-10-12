package models

import "time"

// IncidentKind identifies the platform signal that opened an incident.
type IncidentKind string

const (
	IncidentKindAlert  IncidentKind = "alert"
	IncidentKindDisk   IncidentKind = "disk"
	IncidentKindDrift  IncidentKind = "drift"
	IncidentKindChange IncidentKind = "change"
)

// IncidentStatus is the operator lifecycle of an incident.
type IncidentStatus string

const (
	IncidentStatusOpen         IncidentStatus = "open"
	IncidentStatusAcknowledged IncidentStatus = "acknowledged"
	IncidentStatusResolved     IncidentStatus = "resolved"
)

// Incident is the durable work item created from an operational signal.
// SourceKey identifies one signal occurrence while it remains active; a later
// recurrence creates a new incident so the previous review remains immutable.
type Incident struct {
	ID              uint            `gorm:"primaryKey" json:"id"`
	Kind            IncidentKind    `gorm:"size:24;index;not null" json:"kind"`
	Severity        string          `gorm:"size:24;index;not null" json:"severity"`
	Status          IncidentStatus  `gorm:"size:24;index;not null" json:"status"`
	SourceKey       string          `gorm:"size:512;index;not null" json:"-"`
	ActiveKey       *string         `gorm:"size:64;uniqueIndex" json:"-"`
	ResourceType    string          `gorm:"size:32;index;not null" json:"resource_type"`
	ResourceID      uint            `gorm:"index;not null" json:"resource_id"`
	ResourceName    string          `gorm:"size:256" json:"resource_name"`
	ClusterID       *uint           `gorm:"index" json:"cluster_id,omitempty"`
	ComponentID     *uint           `gorm:"index" json:"component_id,omitempty"`
	OwnerID         *uint           `gorm:"index" json:"owner_id,omitempty"`
	Owner           string          `gorm:"size:64" json:"owner,omitempty"`
	Title           string          `gorm:"size:512;not null" json:"title"`
	Summary         string          `gorm:"type:text" json:"summary"`
	Evidence        string          `gorm:"type:text" json:"-"`
	SourceError     string          `gorm:"type:text" json:"source_error,omitempty"`
	OpenedAt        time.Time       `gorm:"index;not null" json:"opened_at"`
	AcknowledgedAt  *time.Time      `json:"acknowledged_at,omitempty"`
	ResolvedAt      *time.Time      `json:"resolved_at,omitempty"`
	ChangePlanID    *uint           `gorm:"index" json:"change_plan_id,omitempty"`
	LastObservedAt  time.Time       `gorm:"index;not null" json:"last_observed_at"`
	SourceCheckedAt time.Time       `gorm:"index;not null" json:"source_checked_at"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
	Events          []IncidentEvent `gorm:"foreignKey:IncidentID" json:"timeline,omitempty"`
}

// IncidentEvent is an append-only timeline entry. Verification entries are
// required for manual resolution and preserve the evidence used to close work.
type IncidentEvent struct {
	ID             uint      `gorm:"primaryKey" json:"id"`
	IncidentID     uint      `gorm:"index;not null" json:"incident_id"`
	Type           string    `gorm:"size:24;index;not null" json:"type"` // comment/action/verification
	ActorID        *uint     `gorm:"index" json:"actor_id,omitempty"`
	Actor          string    `gorm:"size:64" json:"actor,omitempty"`
	Content        string    `gorm:"type:text;not null" json:"content"`
	Evidence       string    `gorm:"type:text" json:"-"`
	EvidenceObject any       `gorm:"-" json:"evidence,omitempty"`
	CreatedAt      time.Time `gorm:"index" json:"created_at"`
}
