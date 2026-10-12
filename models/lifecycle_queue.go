package models

import "time"

// ComponentOperationClaim is a durable exclusion record, not a runtime status.
type ComponentOperationClaim struct {
	ComponentID uint   `gorm:"primaryKey;autoIncrement:false"`
	Token       string `gorm:"not null"`
	Kind        string
	CreatedAt   time.Time
}

type HostOperationClaim struct {
	HostID    uint   `gorm:"primaryKey;autoIncrement:false"`
	Token     string `gorm:"not null"`
	Kind      string
	CreatedAt time.Time
}

// ChangeBaseline binds a plan to the complete managed desired state at review time.
type ChangeBaseline struct {
	PlanID             uint   `gorm:"primaryKey;autoIncrement:false"`
	Fingerprint        string `gorm:"not null"`
	AppliedFingerprint string
}

type DeploymentStack struct {
	BatchID      *uint                 `gorm:"index" json:"batch_id,omitempty"`
	ID           uint                  `gorm:"primaryKey" json:"id"`
	Name         string                `json:"name"`
	HostID       uint                  `json:"host_id"`
	TemplateKey  string                `json:"template_key"`
	Status       string                `gorm:"index" json:"status"`
	CurrentIndex int                   `json:"current_index"`
	Total        int                   `json:"total"`
	LastError    string                `json:"last_error"`
	Steps        []DeploymentStackStep `gorm:"foreignKey:StackID" json:"steps"`
	CreatedAt    time.Time             `json:"created_at"`
	UpdatedAt    time.Time             `json:"updated_at"`
}

type DeploymentStackStep struct {
	Port              int    `json:"port,omitempty"`
	Ownership         string `json:"ownership,omitempty"`
	Result            string `json:"result,omitempty"`
	Action            string `json:"action,omitempty"`
	ChangePlanID      *uint  `json:"change_plan_id,omitempty"`
	ChangePlanHistory []uint `gorm:"serializer:json" json:"change_plan_history,omitempty"`
	FromVersion       string `json:"from_version,omitempty"`
	TargetVersion     string `json:"target_version,omitempty"`
	ID                uint   `gorm:"primaryKey" json:"-"`
	StackID           uint   `gorm:"uniqueIndex:stack_step" json:"-"`
	Position          int    `gorm:"uniqueIndex:stack_step" json:"-"`
	Type              string `json:"type"`
	Status            string `json:"status"`
	ComponentID       *uint  `json:"component_id"`
	OperationID       *uint  `json:"operation_id"`
	Error             string `json:"error"`
	EncryptedRequest  string `gorm:"type:text;not null" json:"-"`
}
