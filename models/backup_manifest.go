package models

import "time"

// BackupManifest is the integrity and provenance record for one backup artifact.
// A Backup row is not considered restorable unless its manifest exists and the
// artifact matches SHA256, Size, Scope, and Format.
type BackupManifest struct {
	ID               uint          `gorm:"primaryKey" json:"id"`
	BackupID         uint          `gorm:"uniqueIndex;not null" json:"backup_id"`
	Backup           Backup        `gorm:"foreignKey:BackupID;constraint:OnDelete:CASCADE" json:"-"`
	Scope            string        `gorm:"size:16;not null" json:"scope"`
	Format           string        `gorm:"size:32;not null" json:"format"`
	SourcePaths      []string      `gorm:"serializer:json;type:text" json:"source_paths"`
	ComponentType    ComponentType `gorm:"size:32" json:"component_type"`
	ComponentVersion string        `gorm:"size:64" json:"component_version"`
	SHA256           string        `gorm:"size:64;not null" json:"sha256"`
	Size             int64         `gorm:"not null" json:"size"`
	Consistency      string        `gorm:"size:32" json:"consistency"`
	ScopeLimit       string        `gorm:"type:text" json:"scope_limit"`
	CreatedAt        time.Time     `json:"created_at"`
	VerifiedAt       *time.Time    `json:"verified_at,omitempty"`
}

// ScheduledBackupStatus captures execution evidence without changing the
// legacy ScheduledBackup schema. There is one current status row per schedule.
type ScheduledBackupStatus struct {
	ID                uint            `gorm:"primaryKey" json:"id"`
	ScheduledBackupID uint            `gorm:"uniqueIndex;not null" json:"scheduled_backup_id"`
	ScheduledBackup   ScheduledBackup `gorm:"foreignKey:ScheduledBackupID;constraint:OnDelete:CASCADE" json:"-"`
	LastStatus        string          `gorm:"size:16;not null" json:"last_status"`
	LastError         string          `gorm:"type:text" json:"last_error,omitempty"`
	DurationMS        int64           `gorm:"not null" json:"duration_ms"`
	BackupID          *uint           `gorm:"index" json:"backup_id,omitempty"`
	LastRun           time.Time       `json:"last_run"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
}
