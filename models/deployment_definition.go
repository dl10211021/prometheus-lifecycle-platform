package models

// DeploymentParameter describes one configuration-driven deployment form
// field. Field names are restricted to inputs already consumed by the current
// deployment executor. VersionRule exposes the same audited CLI rule catalog
// consumed by Prometheus parameter admission.
type DeploymentParameter struct {
	Field       string               `json:"field"`
	Label       string               `json:"label"`
	Type        string               `json:"type"` // package-version/number/path/text/textarea/password/boolean/select/multiselect/duration/size/url
	Required    bool                 `json:"required,omitempty"`
	Default     interface{}          `json:"default,omitempty"`
	Min         int                  `json:"min,omitempty"`
	Max         int                  `json:"max,omitempty"`
	Options     []string             `json:"options,omitempty"`
	Advanced    bool                 `json:"advanced,omitempty"`
	Placeholder string               `json:"placeholder,omitempty"`
	Help        string               `json:"help,omitempty"`
	VersionRule *ArgumentVersionRule `json:"version_rule,omitempty"`
}

// DeploymentDefinition is the catalog entry consumed by the deployment
// wizard. Built-in and custom components share this response contract.
type DeploymentDefinition struct {
	Type           string                `json:"type"`
	DisplayName    string                `json:"display_name"`
	Description    string                `json:"description"`
	Platform       string                `json:"platform"` // linux/windows
	DeployType     string                `json:"deploy_type"`
	ServiceManager string                `json:"service_manager"`
	HealthPath     string                `json:"health_path"`
	BuiltIn        bool                  `json:"built_in"`
	Parameters     []DeploymentParameter `json:"parameters"`
}
