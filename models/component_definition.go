package models

import (
	"encoding/json"
	"fmt"
	"time"
)

// ComponentDefinition describes a user-defined single-binary component type.
// Secrets and credentials deliberately do not belong in reusable definitions.
type ComponentDefinition struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	Type        string    `gorm:"size:32;uniqueIndex;not null" json:"type"`
	DisplayName string    `gorm:"size:128;not null" json:"display_name"`
	BinName     string    `gorm:"size:128;not null" json:"bin_name"`
	DefaultPort int       `gorm:"not null" json:"default_port"`
	ConfigDir   string    `gorm:"size:512" json:"config_dir"`
	DataDir     string    `gorm:"size:512" json:"data_dir"`
	HealthPath  string    `gorm:"size:256" json:"health_path"`
	Command     string    `gorm:"type:text" json:"command"`
	PackageURL  string    `gorm:"size:2048;not null" json:"package_url"`
	ParamSchema string    `gorm:"type:text" json:"param_schema,omitempty"` // JSON []DeploymentParameter
	Enabled     bool      `gorm:"not null" json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// ComponentDefinitionSnapshot is persisted inside Component.Config when a
// custom component is created. Deployments only consume this immutable copy,
// never the mutable ComponentDefinition row.
type ComponentDefinitionSnapshot struct {
	Type        string `json:"type"`
	DisplayName string `json:"display_name"`
	BinName     string `json:"bin_name"`
	DefaultPort int    `json:"default_port"`
	ConfigDir   string `json:"config_dir,omitempty"`
	DataDir     string `json:"data_dir,omitempty"`
	HealthPath  string `json:"health_path,omitempty"`
	Command     string `json:"command,omitempty"`
	PackageURL  string `json:"package_url"`
	ParamSchema string `json:"param_schema,omitempty"`
}

const ComponentDefinitionSnapshotKey = "_component_definition"

func (d ComponentDefinition) Snapshot() ComponentDefinitionSnapshot {
	return ComponentDefinitionSnapshot{
		Type: d.Type, DisplayName: d.DisplayName, BinName: d.BinName,
		DefaultPort: d.DefaultPort, ConfigDir: d.ConfigDir, DataDir: d.DataDir,
		HealthPath: d.HealthPath, Command: d.Command, PackageURL: d.PackageURL,
		ParamSchema: d.ParamSchema,
	}
}

// EmbedComponentDefinitionSnapshot preserves existing JSON configuration and
// adds the immutable definition used by binary deployment.
func EmbedComponentDefinitionSnapshot(config string, d ComponentDefinition) (string, error) {
	values := make(map[string]json.RawMessage)
	if config != "" {
		if err := json.Unmarshal([]byte(config), &values); err != nil {
			return "", fmt.Errorf("component config must be a JSON object: %w", err)
		}
		if values == nil {
			return "", fmt.Errorf("component config must be a JSON object")
		}
	}
	snapshot, err := json.Marshal(d.Snapshot())
	if err != nil {
		return "", err
	}
	values[ComponentDefinitionSnapshotKey] = snapshot
	encoded, err := json.Marshal(values)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// ComponentDefinitionSnapshotFromConfig extracts the deployment snapshot from
// Component.Config. found=false means the component is not a custom definition.
func ComponentDefinitionSnapshotFromConfig(config string) (snapshot ComponentDefinitionSnapshot, found bool, err error) {
	if config == "" {
		return snapshot, false, nil
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal([]byte(config), &values); err != nil {
		return snapshot, false, fmt.Errorf("invalid component config JSON: %w", err)
	}
	if values == nil {
		return snapshot, false, fmt.Errorf("invalid component config JSON object")
	}
	raw, ok := values[ComponentDefinitionSnapshotKey]
	if !ok {
		return snapshot, false, nil
	}
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return snapshot, true, fmt.Errorf("invalid component definition snapshot: %w", err)
	}
	return snapshot, true, nil
}
