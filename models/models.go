package models

import (
	"errors"
	"time"

	"github.com/promlifecycle/platform/pkg/credentials"
	"gorm.io/gorm"
)

// ComponentType represents a Prometheus ecosystem component
type ComponentType string

const (
	ComponentPrometheus      ComponentType = "prometheus"
	ComponentGrafana         ComponentType = "grafana"
	ComponentAlertmanager    ComponentType = "alertmanager"
	ComponentLoki            ComponentType = "loki"
	ComponentPromtail        ComponentType = "promtail"
	ComponentNodeExporter    ComponentType = "node_exporter"
	ComponentWindowsExporter ComponentType = "windows_exporter"
	ComponentPushgateway     ComponentType = "pushgateway"
	ComponentBlackbox        ComponentType = "blackbox_exporter"
	ComponentThanos          ComponentType = "thanos"
)

// ComponentStatus represents the lifecycle state
type ComponentStatus string

const (
	StatusPending     ComponentStatus = "pending"
	StatusDeploying   ComponentStatus = "deploying"
	StatusRunning     ComponentStatus = "running"
	StatusDegraded    ComponentStatus = "degraded"
	StatusStopping    ComponentStatus = "stopping"
	StatusStopped     ComponentStatus = "stopped"
	StatusError       ComponentStatus = "error"
	StatusUpgrading   ComponentStatus = "upgrading"
	StatusRollingBack ComponentStatus = "rolling_back"
	StatusConfiguring ComponentStatus = "configuring"
)

// Host represents a physical or virtual server
type Host struct {
	ID              uint        `gorm:"primaryKey" json:"id"`
	Name            string      `gorm:"size:128;uniqueIndex" json:"name"`
	IP              string      `gorm:"size:64;uniqueIndex" json:"ip"`
	OS              string      `gorm:"size:32" json:"os"`              // linux/windows
	ConnectionType  string      `gorm:"size:16" json:"connection_type"` // ssh/winrm
	Port            int         `gorm:"default:22" json:"port"`
	Username        string      `gorm:"size:64" json:"username"`
	Password        string      `gorm:"size:256" json:"-"`
	PrivateKey      string      `gorm:"type:text" json:"-"`
	Status          string      `gorm:"size:32;default:unknown" json:"status"` // online/offline/unknown (not yet verified)
	LastError       string      `gorm:"type:text" json:"last_error,omitempty"`
	LastCheckedAt   *time.Time  `json:"last_checked_at,omitempty"`
	IsWindows       bool        `json:"is_windows"`
	WinRMPort       int         `gorm:"default:5986" json:"winrm_port"`
	WinRMScheme     string      `gorm:"size:8;default:https" json:"winrm_scheme"` // http/https
	WinRMSkipVerify bool        `gorm:"default:false" json:"winrm_skip_verify"`
	HasDocker       bool        `json:"has_docker"`
	DockerVersion   string      `gorm:"size:64" json:"docker_version,omitempty"`
	OSVersion       string      `gorm:"size:128" json:"os_version,omitempty"`
	Role            string      `gorm:"size:32;default:auto" json:"role"` // monitor/target/auto
	Description     string      `gorm:"type:text" json:"description"`
	CreatedAt       time.Time   `json:"created_at"`
	UpdatedAt       time.Time   `json:"updated_at"`
	LastOnline      *time.Time  `json:"last_online,omitempty"`
	Components      []Component `gorm:"foreignKey:HostID" json:"components,omitempty"`
}

// BeforeSave encrypts host credentials at rest. Idempotent: values already
// carrying the encryption prefix (e.g. a no-op update after a failed save)
// are left untouched.
func (h *Host) BeforeSave(tx *gorm.DB) error {
	var err error
	if h.Password, err = credentials.Encrypt(h.Password); err != nil {
		return err
	}
	if h.PrivateKey, err = credentials.Encrypt(h.PrivateKey); err != nil {
		return err
	}
	return nil
}

// AfterFind decrypts host credentials for in-memory use. Legacy plaintext
// rows (written before encryption existed) pass through unchanged.
func (h *Host) AfterFind(tx *gorm.DB) error {
	var err error
	if h.Password, err = credentials.Decrypt(h.Password); err != nil {
		return err
	}
	if h.PrivateKey, err = credentials.Decrypt(h.PrivateKey); err != nil {
		return err
	}
	return nil
}

// Cluster represents a logical grouping of monitoring components (e.g., prod, test)
type Cluster struct {
	ID             uint        `gorm:"primaryKey" json:"id"`
	Name           string      `gorm:"size:128;uniqueIndex" json:"name"`
	Environment    string      `gorm:"size:32" json:"environment"`     // prod/test/dev/staging
	DeploymentType string      `gorm:"size:32" json:"deployment_type"` // docker/k8s
	Host           string      `gorm:"size:256" json:"host"`
	Description    string      `gorm:"type:text" json:"description"`
	Status         string      `gorm:"size:32;default:'active'" json:"status"`
	CreatedAt      time.Time   `json:"created_at"`
	UpdatedAt      time.Time   `json:"updated_at"`
	Components     []Component `gorm:"foreignKey:ClusterID" json:"components,omitempty"`
}

// Component represents a deployed Prometheus ecosystem component
type Component struct {
	ID         uint            `gorm:"primaryKey" json:"id"`
	ClusterID  uint            `gorm:"index" json:"cluster_id"`
	Cluster    Cluster         `gorm:"foreignKey:ClusterID" json:"-"`
	HostID     uint            `gorm:"index" json:"host_id"`
	Host       Host            `gorm:"foreignKey:HostID" json:"host,omitempty"`
	Type       ComponentType   `gorm:"size:32;index" json:"type"`
	Name       string          `gorm:"size:128" json:"name"`
	Version    string          `gorm:"size:64" json:"version"`
	Image      string          `gorm:"size:256" json:"image"`
	Port       int             `gorm:"default:0" json:"port"`
	DeployType string          `gorm:"size:16;default:binary" json:"deploy_type"` // binary | target; docker is legacy-read-only
	Status     ComponentStatus `gorm:"size:32;index;default:pending" json:"status"`
	Error      string          `gorm:"type:text" json:"error,omitempty"`

	// Deployment paths (on target host)
	ComposePath  string `gorm:"size:256" json:"compose_path,omitempty"`
	ConfigPath   string `gorm:"size:256" json:"config_path,omitempty"`
	DataPath     string `gorm:"size:256" json:"data_path,omitempty"`
	ServiceName  string `gorm:"size:128" json:"service_name,omitempty"`
	BinaryPath   string `gorm:"size:512" json:"binary_path,omitempty"`
	ImportedArgs string `gorm:"type:text" json:"-"`

	// URLs
	InternalURL string `gorm:"size:256" json:"internal_url,omitempty"`
	ExternalURL string `gorm:"size:256" json:"external_url,omitempty"`

	// Config
	Config string `gorm:"type:text" json:"config,omitempty"` // JSON config

	// Custom paths and resources
	ResourceLimits   string `gorm:"type:text" json:"resource_limits,omitempty"`   // JSON: {cpu: "0.5", memory: "512Mi"}
	CustomEnv        string `gorm:"type:text" json:"custom_env,omitempty"`        // JSON map of env vars
	ExtraVolumes     string `gorm:"type:text" json:"extra_volumes,omitempty"`     // JSON array: [{src, dst}]
	DataPathOverride string `gorm:"size:256" json:"data_path_override,omitempty"` // Override default data path
	InstallPath      string `gorm:"size:256" json:"install_path,omitempty"`       // Override binary install dir (binary deploys)
	CustomArgs       string `gorm:"type:text" json:"custom_args,omitempty"`       // Extra binary launch args, one per line (binary deploys)

	// Timestamps
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	LastDeployedAt *time.Time `json:"last_deployed_at,omitempty"`

	// Relations
	Configs    []ComponentConfig `gorm:"foreignKey:ComponentID" json:"configs,omitempty"`
	Operations []Operation       `gorm:"foreignKey:ComponentID" json:"operations,omitempty"`
}

// ComponentConfig represents a configuration file for a component
type ComponentConfig struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	ComponentID uint      `gorm:"index" json:"component_id"`
	Component   Component `gorm:"foreignKey:ComponentID" json:"component,omitempty"`
	Name        string    `gorm:"size:128" json:"name"`        // prometheus.yml, alertmanager.yml
	ContentType string    `gorm:"size:32" json:"content_type"` // yaml/json
	Content     string    `gorm:"type:text" json:"content"`    // YAML/JSON content
	Version     int       `gorm:"default:1" json:"version"`    // version number for rollback
	IsActive    bool      `gorm:"default:true" json:"is_active"`
	FilePath    string    `gorm:"size:256" json:"file_path,omitempty"` // actual path on host
	Description string    `gorm:"type:text" json:"description"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type OperationStageEvent struct {
	Stage     string    `json:"stage"`
	Message   string    `json:"message"`
	Progress  int       `json:"progress"`
	CreatedAt time.Time `json:"created_at"`
}

// Operation tracks deployment/upgrade/rollback operations
type Operation struct {
	ID           uint       `gorm:"primaryKey" json:"id"`
	ComponentID  *uint      `json:"component_id"`
	ClusterID    *uint      `json:"cluster_id"`
	Type         string     `gorm:"size:32" json:"type"`   // deploy/upgrade/rollback/restart/config-update
	Status       string     `gorm:"size:32" json:"status"` // pending/running/success/failed
	Message      string     `gorm:"type:text" json:"message"`
	Input        string     `gorm:"type:text" json:"input,omitempty"`  // JSON input
	Output       string     `gorm:"type:text" json:"output,omitempty"` // JSON output
	Stage        string     `gorm:"size:64" json:"stage,omitempty"`
	Progress     int        `gorm:"default:0" json:"progress"`
	StageHistory string     `gorm:"type:text" json:"stage_history,omitempty"` // JSON []OperationStageEvent
	StartedAt    *time.Time `json:"started_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
}

// Backup represents a configuration backup
type Backup struct {
	ID                uint      `gorm:"primaryKey" json:"id"`
	ScheduledBackupID *uint     `gorm:"index" json:"scheduled_backup_id,omitempty"`
	ComponentID       *uint     `json:"component_id"`
	ClusterID         *uint     `json:"cluster_id"`
	Name              string    `gorm:"size:128" json:"name"`
	Path              string    `gorm:"size:256" json:"path"`
	Size              int64     `json:"size"`
	Type              string    `gorm:"size:32" json:"type"` // config/data/full
	CreatedAt         time.Time `json:"created_at"`
}

// UserRole is a product capability role. Roles are intentionally explicit:
// authorization is assigned per route rather than inferred from a numeric rank.
type UserRole string

const (
	RoleViewer      UserRole = "viewer"
	RoleOperator    UserRole = "operator"
	RoleChangeAdmin UserRole = "change_admin"
	RoleSystemAdmin UserRole = "system_admin"
	RoleAuditor     UserRole = "auditor"
)

// IsValidUserRole is the single allow-list used by authentication and user
// persistence. Legacy roles are migrated at startup, never accepted as aliases.
func IsValidUserRole(role string) bool {
	switch UserRole(role) {
	case RoleViewer, RoleOperator, RoleChangeAdmin, RoleSystemAdmin, RoleAuditor:
		return true
	default:
		return false
	}
}

// User represents a platform user.
type User struct {
	ID                 uint      `gorm:"primaryKey" json:"id"`
	Username           string    `gorm:"size:64;uniqueIndex" json:"username"`
	Password           string    `gorm:"size:128" json:"-"`
	Email              string    `gorm:"size:128" json:"email"`
	Role               string    `gorm:"size:32;default:viewer" json:"role"`
	IsActive           bool      `gorm:"not null" json:"is_active"`
	MustChangePassword bool      `gorm:"default:false" json:"must_change_password"`
	SessionVersion     int64     `gorm:"not null;default:0" json:"-"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

// BeforeSave prevents user CRUD from persisting unknown or legacy roles.
func (u *User) BeforeSave(_ *gorm.DB) error {
	if !IsValidUserRole(u.Role) {
		return errors.New("invalid user role")
	}
	return nil
}

// ComponentVersion tracks available versions
type ComponentVersion struct {
	ID          uint          `gorm:"primaryKey" json:"id"`
	Type        ComponentType `gorm:"size:32;index" json:"type"`
	Version     string        `gorm:"size:64;index" json:"version"`
	Image       string        `gorm:"size:256" json:"image"`
	Source      string        `gorm:"size:32" json:"source"` // official/custom
	Checksum    string        `gorm:"size:128" json:"checksum,omitempty"`
	Notes       string        `gorm:"type:text" json:"notes"`
	ComponentID *uint         `gorm:"index" json:"component_id,omitempty"` // set for upgrade-history records
	CreatedAt   time.Time     `json:"created_at"`
}

// HealthStatus represents component health
type HealthStatus struct {
	Status      string                 `json:"status"`
	ComponentID uint                   `json:"component_id"`
	Component   string                 `json:"component"`
	Type        ComponentType          `json:"type"`
	Message     string                 `json:"message,omitempty"`
	Details     map[string]interface{} `json:"details,omitempty"`
	Timestamp   time.Time              `json:"timestamp"`
}

// OperationType represents the type of lifecycle operation
type OperationType string

const (
	OpDeploy       OperationType = "deploy"
	OpUpgrade      OperationType = "upgrade"
	OpRollback     OperationType = "rollback"
	OpRestart      OperationType = "restart"
	OpStart        OperationType = "start"
	OpStop         OperationType = "stop"
	OpConfigure    OperationType = "configure"
	OpScale        OperationType = "scale"
	OpConfigUpdate OperationType = "config_update"
	OpDelete       OperationType = "delete"
	OpBackup       OperationType = "backup"
	OpRestore      OperationType = "restore"
	OpHealthCheck  OperationType = "health_check"
)

// OperationLog tracks user actions for audit trail. It stores request outcome
// metadata only; request bodies and secrets are never persisted.
type OperationLog struct {
	ID            uint      `gorm:"primaryKey" json:"id"`
	UserID        uint      `gorm:"index" json:"user_id"`
	Username      string    `gorm:"size:64" json:"username"`
	Role          string    `gorm:"size:32;index" json:"role"`
	Action        string    `gorm:"size:64;index" json:"action"`
	Resource      string    `gorm:"size:64;index" json:"resource"`
	ResourceID    uint      `gorm:"index" json:"resource_id"`
	Description   string    `gorm:"type:text" json:"description"`
	Result        string    `gorm:"size:16;index" json:"result"`
	StatusCode    int       `json:"status_code"`
	DurationMS    int64     `json:"duration_ms"`
	CorrelationID string    `gorm:"size:32;index" json:"correlation_id,omitempty"`
	IPAddress     string    `gorm:"size:64" json:"ip_address"`
	UserAgent     string    `gorm:"size:256" json:"user_agent"`
	CreatedAt     time.Time `json:"created_at"`
}

// AlertRule represents an alerting rule
type AlertRule struct {
	PublicationState
	ID          uint      `gorm:"primaryKey" json:"id"`
	ClusterID   uint      `gorm:"index" json:"cluster_id"`
	Name        string    `gorm:"size:256" json:"name"`
	Expression  string    `gorm:"type:text" json:"expression"`             // PromQL expression
	Duration    string    `gorm:"size:32;default:5m" json:"duration"`      // FOR duration (e.g., 5m, 1h)
	Severity    string    `gorm:"size:32;default:warning" json:"severity"` // critical/warning/info
	Labels      string    `gorm:"type:text" json:"labels"`                 // JSON labels
	Annotations string    `gorm:"type:text" json:"annotations"`            // JSON annotations (summary, description)
	Enabled     bool      `gorm:"default:true" json:"enabled"`
	Group       string    `gorm:"size:128" json:"group"` // rule group name
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// AlertHistory records triggered alerts
type AlertHistory struct {
	ID        uint       `gorm:"primaryKey" json:"id"`
	RuleID    uint       `gorm:"index" json:"rule_id"`
	ClusterID uint       `gorm:"index" json:"cluster_id"`
	AlertName string     `gorm:"size:256;index" json:"alert_name"`
	Severity  string     `gorm:"size:32" json:"severity"`
	Status    string     `gorm:"size:32" json:"status"`   // firing/resolved/unknown
	Value     string     `gorm:"size:128" json:"value"`   // metric value when triggered
	Labels    string     `gorm:"type:text" json:"labels"` // JSON labels
	Summary   string     `gorm:"type:text" json:"summary"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

// PlatformStats holds platform statistics
type PlatformStats struct {
	TotalHosts        int64  `json:"total_hosts"`
	OnlineHosts       int64  `json:"online_hosts"`
	DockerHosts       int64  `json:"docker_hosts"`
	TotalComponents   int64  `json:"total_components"`
	RunningComponents int64  `json:"running_components"`
	TotalAlertRules   int64  `json:"total_alert_rules"`
	ActiveAlerts      int64  `json:"active_alerts"`
	TotalOperations   int64  `json:"total_operations"`
	PlatformVersion   string `json:"platform_version"`
	Uptime            string `json:"uptime"`
}

// NotificationChannel represents an alert notification destination
type NotificationChannel struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	Name        string    `gorm:"size:128" json:"name"`
	Type        string    `gorm:"size:32" json:"type"`     // email/webhook/dingtalk/wechat/slack
	Config      string    `gorm:"type:text" json:"config"` // JSON config
	ClusterID   *uint     `json:"cluster_id"`
	Enabled     bool      `json:"enabled"`
	Secret      string    `gorm:"size:256" json:"-"`                         // webhook secret/token
	ApplyStatus string    `gorm:"size:32;default:draft" json:"apply_status"` // draft/pending/applied/error
	LastError   string    `gorm:"type:text" json:"last_error,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// AlertNotification records sent alert notifications
type AlertNotification struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	AlertName   string    `gorm:"size:256" json:"alert_name"`
	Severity    string    `gorm:"size:32" json:"severity"`
	ChannelID   uint      `json:"channel_id"`
	ChannelType string    `gorm:"size:32" json:"channel_type"`
	Message     string    `gorm:"type:text" json:"message"`
	Status      string    `gorm:"size:32" json:"status"` // sent/failed
	Error       string    `gorm:"type:text" json:"error,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// ScheduledBackup represents a cron-based backup task
type ScheduledBackup struct {
	ID          uint       `gorm:"primaryKey" json:"id"`
	Name        string     `gorm:"size:128" json:"name"`
	Cron        string     `gorm:"size:64" json:"cron"` // cron expression
	ClusterID   *uint      `json:"cluster_id"`
	ComponentID *uint      `json:"component_id"`
	Type        string     `gorm:"size:32;default:config" json:"type"` // config/data/full
	KeepCount   int        `gorm:"default:7" json:"keep_count"`        // keep N backups
	Enabled     bool       `gorm:"default:true" json:"enabled"`
	LastRun     *time.Time `json:"last_run,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// ResourceUsage tracks component resource metrics
type ResourceUsage struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	ComponentID uint      `gorm:"index" json:"component_id"`
	CPUUsage    float64   `json:"cpu_usage"` // percentage
	MemoryMB    float64   `json:"memory_mb"`
	DiskMB      float64   `json:"disk_mb"`
	DiskUsage   float64   `json:"disk_usage"` // percentage
	Timestamp   time.Time `gorm:"index" json:"timestamp"`
}

// MaintenanceWindow defines periods when alerts are silenced
type MaintenanceWindow struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	Name        string    `gorm:"size:128" json:"name"`
	ClusterID   *uint     `json:"cluster_id"`
	ComponentID *uint     `json:"component_id"`
	StartAt     time.Time `json:"start_at"`
	EndAt       time.Time `json:"end_at"`
	Reason      string    `gorm:"type:text" json:"reason"`
	SilenceID   string    `gorm:"size:128" json:"silence_id"` // Alertmanager silence created by the scheduler
	CreatedBy   uint      `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
}

// Certificate represents TLS certificate for components
type Certificate struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	Name        string    `gorm:"size:128" json:"name"`
	ComponentID *uint     `json:"component_id"`
	Domain      string    `gorm:"size:256" json:"domain"`
	CertPath    string    `gorm:"size:256" json:"cert_path"`
	KeyPath     string    `gorm:"size:256" json:"key_path"`
	ExpiryDate  time.Time `json:"expiry_date"`
	Type        string    `gorm:"size:32" json:"type"` // self-signed/letsencrypt/custom
	CreatedAt   time.Time `json:"created_at"`
}

// TopologyEdge represents connection between components
type TopologyEdge struct {
	ID          uint   `gorm:"primaryKey" json:"id"`
	SourceID    uint   `gorm:"index" json:"source_id"`
	TargetID    uint   `gorm:"index" json:"target_id"`
	Type        string `gorm:"size:32" json:"type"` // scrape/notify/log/export
	Description string `gorm:"size:256" json:"description"`
}

// ConfigTemplate represents a reusable configuration template for components
type ConfigTemplate struct {
	ID            uint      `gorm:"primaryKey" json:"id"`
	Name          string    `gorm:"size:256;index" json:"name"`
	Description   string    `gorm:"size:512" json:"description"`
	ComponentType string    `gorm:"size:64;index" json:"component_type"`
	Content       string    `gorm:"type:text" json:"content"`
	Version       int       `gorm:"default:1" json:"version"`
	IsActive      bool      `gorm:"default:true;index" json:"is_active"`
	IsDefault     bool      `gorm:"default:false;index" json:"is_default"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Package represents a stored Docker image or binary package
type Package struct {
	ID          uint          `gorm:"primaryKey" json:"id"`
	Name        string        `gorm:"size:256;index" json:"name"`
	Type        ComponentType `gorm:"size:64" json:"type"`
	Version     string        `gorm:"size:64" json:"version"`
	Format      string        `gorm:"size:16" json:"format"` // docker/zip/binary
	Size        int64         `json:"size"`
	Checksum    string        `gorm:"size:64" json:"checksum"` // SHA256
	StoragePath string        `gorm:"size:512" json:"-"`
	DownloadURL string        `gorm:"size:512" json:"download_url"`
	Description string        `gorm:"type:text" json:"description"`
	CreatedAt   time.Time     `json:"created_at"`
	UpdatedAt   time.Time     `json:"updated_at"`
}

// ScrapeConfigModel represents a Prometheus scrape configuration entry managed by the platform.
type ScrapeConfigModel struct {
	PublicationState
	ID             uint      `gorm:"primaryKey" json:"id"`
	ClusterID      uint      `gorm:"index" json:"cluster_id"`
	Name           string    `gorm:"size:256" json:"name"`           // human-readable name
	JobName        string    `gorm:"size:256;index" json:"job_name"` // Prometheus job_name
	ScrapeInterval string    `gorm:"size:32;default:15s" json:"scrape_interval"`
	ScrapeTimeout  string    `gorm:"size:32;default:10s" json:"scrape_timeout"`
	Targets        string    `gorm:"type:text" json:"targets"`         // JSON array of strings
	Labels         string    `gorm:"type:text" json:"labels"`          // JSON map
	FileSDConfigs  string    `gorm:"type:text" json:"file_sd_configs"` // JSON array
	HTTPSDConfigs  string    `gorm:"type:text" json:"http_sd_configs"` // JSON array
	RelabelConfigs string    `gorm:"type:text" json:"relabel_configs"` // JSON array
	Enabled        bool      `gorm:"default:true" json:"enabled"`
	Type           string    `gorm:"size:32;default:static" json:"type"` // static/file_sd/http_sd/kubernetes_sd
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// RecordingRule represents a Prometheus recording rule.
type RecordingRule struct {
	PublicationState
	ID         uint      `gorm:"primaryKey" json:"id"`
	ClusterID  uint      `gorm:"index" json:"cluster_id"`
	Group      string    `gorm:"size:128;index" json:"group"` // rule group name
	Name       string    `gorm:"size:256" json:"name"`        // recording rule name
	Expression string    `gorm:"type:text" json:"expression"` // PromQL expression
	Labels     string    `gorm:"type:text" json:"labels"`     // JSON map
	Enabled    bool      `gorm:"default:true" json:"enabled"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// RemoteWriteModel represents a Prometheus remote write configuration stored in the database.
type RemoteWriteModel struct {
	PublicationState
	ID                uint      `gorm:"primaryKey" json:"id"`
	ClusterID         uint      `gorm:"index" json:"cluster_id"`
	Name              string    `gorm:"size:256" json:"name"`
	URL               string    `gorm:"size:512" json:"url"`
	RemoteTimeout     string    `gorm:"size:32;default:30s" json:"remote_timeout"`
	BasicAuthUser     string    `gorm:"size:256" json:"basic_auth_user"`
	BasicAuthPassword string    `gorm:"size:512" json:"-"` // never expose in JSON
	BearerToken       string    `gorm:"size:512" json:"-"` // never expose in JSON
	Enabled           bool      `gorm:"default:true" json:"enabled"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// AlertmanagerRoute represents a custom routing rule for Alertmanager
type AlertmanagerRoute struct {
	ID             uint      `gorm:"primaryKey" json:"id"`
	ClusterID      uint      `gorm:"index" json:"cluster_id"`
	Name           string    `gorm:"size:128" json:"name"`
	Receiver       string    `gorm:"size:128" json:"receiver"`
	Match          string    `gorm:"type:text" json:"match,omitempty"`    // JSON: {"severity": "critical"}
	MatchRE        string    `gorm:"type:text" json:"match_re,omitempty"` // JSON: {"alertname": ".*Error.*"}
	GroupBy        string    `gorm:"type:text" json:"group_by,omitempty"` // JSON: ["alertname", "cluster_id"]
	GroupWait      string    `gorm:"size:32" json:"group_wait,omitempty"`
	GroupInterval  string    `gorm:"size:32" json:"group_interval,omitempty"`
	RepeatInterval string    `gorm:"size:32" json:"repeat_interval,omitempty"`
	ParentID       *uint     `json:"parent_id"` // For nested routes
	Enabled        bool      `json:"enabled"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// AlertSilence represents a silence in Alertmanager
type AlertSilence struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	ClusterID  uint      `gorm:"index" json:"cluster_id"`
	ExternalID string    `gorm:"size:128;uniqueIndex" json:"external_id"` // Alertmanager silence ID
	Matchers   string    `gorm:"type:text" json:"matchers"`               // JSON: [{"name": "alertname", "value": "Down", "isRegex": false}]
	Comment    string    `gorm:"type:text" json:"comment"`
	CreatedBy  string    `gorm:"size:128" json:"created_by"`
	StartsAt   time.Time `json:"starts_at"`
	EndsAt     time.Time `json:"ends_at"`
	Status     string    `gorm:"size:32" json:"status"` // pending/active/expired
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Dashboard represents a Grafana dashboard managed by the platform.
type Dashboard struct {
	PublicationState
	ID            uint      `gorm:"primaryKey" json:"id"`
	ClusterID     uint      `gorm:"index" json:"cluster_id"`
	Name          string    `gorm:"size:256;index" json:"name"`
	UID           string    `gorm:"size:128" json:"uid"`
	Source        string    `gorm:"size:32" json:"source"` // url/json/manual
	URL           string    `gorm:"size:512" json:"url"`
	Content       string    `gorm:"type:text" json:"content"`                 // JSON dashboard content
	ProvisionPath string    `gorm:"size:512" json:"provision_path,omitempty"` // path on host for provisioning
	Version       int       `gorm:"default:1" json:"version"`
	Enabled       bool      `gorm:"default:true;index" json:"enabled"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}
