package services

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/promlifecycle/platform/core/deployer"
	"github.com/promlifecycle/platform/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// IncidentObservation is a normalized signal emitted by a collector.
type IncidentObservation struct {
	Kind         models.IncidentKind
	Severity     string
	SourceKey    string
	ResourceType string
	ResourceID   uint
	ResourceName string
	ClusterID    *uint
	ComponentID  *uint
	ChangePlanID *uint
	Title        string
	Summary      string
	Evidence     any
	ObservedAt   time.Time
	SourceError  string
}

type IncidentService struct {
	db *gorm.DB
}

func NewIncidentService(db *gorm.DB) *IncidentService {
	return &IncidentService{db: db}
}

// Observe creates one incident for a newly active source or refreshes its
// evidence and collection freshness while it remains unresolved.
func (s *IncidentService) Observe(observation IncidentObservation) (*models.Incident, error) {
	if observation.Kind == "" || strings.TrimSpace(observation.SourceKey) == "" || strings.TrimSpace(observation.Title) == "" {
		return nil, errors.New("事件观察缺少 kind、source_key 或 title")
	}
	if observation.ObservedAt.IsZero() {
		observation.ObservedAt = time.Now()
	}
	if observation.Severity == "" {
		observation.Severity = "warning"
	}
	activeKey := incidentActiveKey(observation)
	evidence, err := marshalIncidentEvidence(observation.Evidence)
	if err != nil {
		return nil, err
	}

	var incident models.Incident
	err = s.db.Transaction(func(tx *gorm.DB) error {
		result := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("active_key = ?", activeKey).First(&incident)
		if result.Error != nil && !errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return result.Error
		}
		if result.Error == nil {
			updates := map[string]any{
				"severity": observation.Severity, "resource_name": observation.ResourceName,
				"title": observation.Title, "summary": observation.Summary, "evidence": evidence,
				"last_observed_at": observation.ObservedAt, "source_checked_at": observation.ObservedAt,
				"source_error": observation.SourceError,
			}
			if observation.ClusterID != nil {
				updates["cluster_id"] = observation.ClusterID
			}
			if observation.ComponentID != nil {
				updates["component_id"] = observation.ComponentID
			}
			if observation.ChangePlanID != nil {
				updates["change_plan_id"] = observation.ChangePlanID
			}
			if err := tx.Model(&incident).Updates(updates).Error; err != nil {
				return err
			}
			return tx.First(&incident, incident.ID).Error
		}

		incident = models.Incident{
			Kind: observation.Kind, Severity: observation.Severity, Status: models.IncidentStatusOpen,
			SourceKey: observation.SourceKey, ActiveKey: &activeKey, ResourceType: observation.ResourceType,
			ResourceID: observation.ResourceID, ResourceName: observation.ResourceName,
			ClusterID: observation.ClusterID, ComponentID: observation.ComponentID, ChangePlanID: observation.ChangePlanID,
			Title: observation.Title, Summary: observation.Summary, Evidence: evidence,
			SourceError: observation.SourceError, OpenedAt: observation.ObservedAt,
			LastObservedAt: observation.ObservedAt, SourceCheckedAt: observation.ObservedAt,
		}
		if err := tx.Create(&incident).Error; err != nil {
			return err
		}
		return tx.Create(&models.IncidentEvent{
			IncidentID: incident.ID, Type: "action", Actor: "system",
			Content: "平台检测到新的运行事件", Evidence: evidence, CreatedAt: observation.ObservedAt,
		}).Error
	})
	return &incident, err
}

// RecordSourceError retains collector failures on active incidents without
// pretending that an absent sample means the underlying signal recovered.
func (s *IncidentService) RecordSourceError(kind models.IncidentKind, resourceType string, resourceID uint, sourceErr error, observedAt time.Time) error {
	if sourceErr == nil {
		return nil
	}
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	return s.db.Model(&models.Incident{}).
		Where("kind = ? AND resource_type = ? AND resource_id = ? AND status <> ?", kind, resourceType, resourceID, models.IncidentStatusResolved).
		Updates(map[string]any{"source_error": sourceErr.Error(), "source_checked_at": observedAt}).Error
}

// ResolveMissing resolves active incidents from a collector only after a
// successful collection proves that their source key is no longer present.
func (s *IncidentService) ResolveMissing(kind models.IncidentKind, resourceType string, resourceID uint, activeSourceKeys map[string]struct{}, observedAt time.Time, reason string) error {
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	var incidents []models.Incident
	if err := s.db.Where("kind = ? AND resource_type = ? AND resource_id = ? AND status <> ?", kind, resourceType, resourceID, models.IncidentStatusResolved).Find(&incidents).Error; err != nil {
		return err
	}
	for i := range incidents {
		if _, active := activeSourceKeys[incidents[i].SourceKey]; active {
			continue
		}
		incident := &incidents[i]
		if err := s.db.Transaction(func(tx *gorm.DB) error {
			updates := map[string]any{
				"status": models.IncidentStatusResolved, "resolved_at": observedAt,
				"last_observed_at": observedAt, "source_checked_at": observedAt,
				"source_error": "", "active_key": nil,
			}
			if err := tx.Model(incident).Updates(updates).Error; err != nil {
				return err
			}
			return tx.Create(&models.IncidentEvent{IncidentID: incident.ID, Type: "verification", Actor: "system", Content: reason, CreatedAt: observedAt}).Error
		}); err != nil {
			return err
		}
	}
	return nil
}

// ResolveSource resolves exactly one source without affecting another active
// signal on the same resource (for example, concurrent change plans).
func (s *IncidentService) ResolveSource(kind models.IncidentKind, resourceType string, resourceID uint, sourceKey string, observedAt time.Time, reason string) error {
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	activeKey := incidentActiveKey(IncidentObservation{
		Kind: kind, ResourceType: resourceType, ResourceID: resourceID, SourceKey: sourceKey,
	})
	var incident models.Incident
	err := s.db.Where("active_key = ?", activeKey).First(&incident).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&incident).Updates(map[string]any{
			"status": models.IncidentStatusResolved, "resolved_at": observedAt,
			"last_observed_at": observedAt, "source_checked_at": observedAt,
			"source_error": "", "active_key": nil,
		}).Error; err != nil {
			return err
		}
		return tx.Create(&models.IncidentEvent{
			IncidentID: incident.ID, Type: "verification", Actor: "system",
			Content: reason, CreatedAt: observedAt,
		}).Error
	})
}

func (s *IncidentService) List(status, kind, severity string, ownerID *uint, limit, offset int) ([]models.Incident, int64, error) {
	query := s.db.Model(&models.Incident{})
	if status != "" {
		query = query.Where("status = ?", status)
	}
	if kind != "" {
		query = query.Where("kind = ?", kind)
	}
	if severity != "" {
		query = query.Where("severity = ?", severity)
	}
	if ownerID != nil {
		query = query.Where("owner_id = ?", *ownerID)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var incidents []models.Incident
	if err := query.Order("CASE WHEN status = 'resolved' THEN 1 ELSE 0 END, opened_at DESC").Limit(limit).Offset(offset).Find(&incidents).Error; err != nil {
		return nil, 0, err
	}
	return incidents, total, nil
}

type AlertHistoryEvidence struct {
	ID        uint           `json:"id"`
	RuleID    uint           `json:"rule_id"`
	ClusterID uint           `json:"cluster_id"`
	AlertName string         `json:"alert_name"`
	Severity  string         `json:"severity"`
	Status    string         `json:"status"`
	Value     string         `json:"value"`
	Labels    map[string]any `json:"labels"`
	Summary   string         `json:"summary"`
	StartedAt time.Time      `json:"started_at"`
	EndedAt   *time.Time     `json:"ended_at,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
}
type ComponentHealthEvidence struct {
	ID         uint                   `json:"id"`
	HostID     uint                   `json:"host_id"`
	ClusterID  uint                   `json:"cluster_id"`
	Type       models.ComponentType   `json:"type"`
	Name       string                 `json:"name"`
	Version    string                 `json:"version"`
	DeployType string                 `json:"deploy_type"`
	Status     models.ComponentStatus `json:"status"`
	Error      string                 `json:"error,omitempty"`
	UpdatedAt  time.Time              `json:"updated_at"`
}

type IncidentDetail struct {
	models.Incident
	EvidenceObject     any                        `json:"evidence"`
	Alerts             []AlertHistoryEvidence     `json:"alert_history"`
	ComponentHealth    *ComponentHealthEvidence   `json:"component_health,omitempty"`
	ComponentLogs      []string                   `json:"component_logs"`
	ComponentLogsError string                     `json:"component_logs_error,omitempty"`
	RecentChanges      []models.ChangePlan        `json:"recent_change_plans"`
	Maintenance        []models.MaintenanceWindow `json:"maintenance"`
	Notifications      []models.AlertNotification `json:"notifications"`
}

func (s *IncidentService) Detail(ctx context.Context, id uint) (*IncidentDetail, error) {
	var incident models.Incident
	if err := s.db.Preload("Events", func(db *gorm.DB) *gorm.DB { return db.Order("created_at ASC") }).First(&incident, id).Error; err != nil {
		return nil, err
	}
	detail := &IncidentDetail{Incident: incident, ComponentLogs: []string{}, Alerts: []AlertHistoryEvidence{}, RecentChanges: []models.ChangePlan{}, Maintenance: []models.MaintenanceWindow{}, Notifications: []models.AlertNotification{}}
	for i := range detail.Events {
		if detail.Events[i].Evidence == "" {
			continue
		}
		if err := json.Unmarshal([]byte(detail.Events[i].Evidence), &detail.Events[i].EvidenceObject); err != nil {
			detail.Events[i].EvidenceObject = detail.Events[i].Evidence
		}
	}
	if incident.Evidence != "" {
		if err := json.Unmarshal([]byte(incident.Evidence), &detail.EvidenceObject); err != nil {
			detail.EvidenceObject = incident.Evidence
		}
	}

	if incident.ClusterID != nil || incident.Kind == models.IncidentKindAlert {
		alertQuery := s.db.Order("created_at DESC").Limit(50)
		if incident.ClusterID != nil {
			alertQuery = alertQuery.Where("cluster_id = ?", *incident.ClusterID)
		}
		if incident.Kind == models.IncidentKindAlert {
			alertQuery = alertQuery.Where("alert_name = ?", incident.ResourceName)
		}
		var alerts []models.AlertHistory
		if err := alertQuery.Find(&alerts).Error; err != nil {
			return nil, err
		}
		for i := range alerts {
			labels := map[string]any{}
			if alerts[i].Labels != "" {
				_ = json.Unmarshal([]byte(alerts[i].Labels), &labels)
			}
			detail.Alerts = append(detail.Alerts, AlertHistoryEvidence{
				ID: alerts[i].ID, RuleID: alerts[i].RuleID, ClusterID: alerts[i].ClusterID,
				AlertName: alerts[i].AlertName, Severity: alerts[i].Severity,
				Status: alerts[i].Status, Value: alerts[i].Value, Labels: labels,
				Summary: alerts[i].Summary, StartedAt: alerts[i].StartedAt,
				EndedAt: alerts[i].EndedAt, CreatedAt: alerts[i].CreatedAt,
			})
		}
	}

	if incident.ComponentID != nil {
		var component models.Component
		if err := s.db.Preload("Host").First(&component, *incident.ComponentID).Error; err == nil {
			detail.ComponentHealth = &ComponentHealthEvidence{
				ID: component.ID, HostID: component.HostID, ClusterID: component.ClusterID,
				Type: component.Type, Name: component.Name, Version: component.Version,
				DeployType: component.DeployType, Status: component.Status,
				Error: component.Error, UpdatedAt: component.UpdatedAt,
			}
			detail.ComponentLogs, detail.ComponentLogsError = s.componentLogs(ctx, &component)
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, err
		}
	}

	changeQuery := s.db.Order("created_at DESC").Limit(10)
	if incident.ChangePlanID != nil {
		changeQuery = changeQuery.Where("id = ?", *incident.ChangePlanID)
	} else {
		changeQuery = changeQuery.Where("resource_type = ? AND resource_id = ?", incident.ResourceType, incident.ResourceID)
	}
	if err := changeQuery.Find(&detail.RecentChanges).Error; err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	maintenanceQuery := s.db.Where("start_at <= ? AND end_at >= ?", now, now).Order("start_at DESC")
	if incident.ComponentID != nil && incident.ClusterID != nil {
		maintenanceQuery = maintenanceQuery.Where("component_id = ? OR (component_id IS NULL AND cluster_id = ?) OR (component_id IS NULL AND cluster_id IS NULL)", *incident.ComponentID, *incident.ClusterID)
	} else if incident.ClusterID != nil {
		maintenanceQuery = maintenanceQuery.Where("cluster_id = ? OR cluster_id IS NULL", *incident.ClusterID)
	}
	if err := maintenanceQuery.Find(&detail.Maintenance).Error; err != nil {
		return nil, err
	}
	if incident.Kind == models.IncidentKindAlert {
		if err := s.db.Where("alert_name = ?", incident.ResourceName).Order("created_at DESC").Limit(50).Find(&detail.Notifications).Error; err != nil {
			return nil, err
		}
	}
	return detail, nil
}

func (s *IncidentService) Acknowledge(id uint, actorID *uint, actor, owner string) (*models.Incident, error) {
	var incident models.Incident
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&incident, id).Error; err != nil {
			return err
		}
		if incident.Status == models.IncidentStatusResolved {
			return errors.New("已解决事件不能认领")
		}
		now := time.Now()
		updates := map[string]any{"status": models.IncidentStatusAcknowledged, "acknowledged_at": now, "owner_id": actorID, "owner": owner}
		if err := tx.Model(&incident).Updates(updates).Error; err != nil {
			return err
		}
		return tx.Create(&models.IncidentEvent{IncidentID: id, Type: "action", ActorID: actorID, Actor: actor, Content: "已认领事件", CreatedAt: now}).Error
	})
	if err != nil {
		return nil, err
	}
	return &incident, s.db.First(&incident, id).Error
}

func (s *IncidentService) Comment(id uint, actorID *uint, actor, content string) (*models.IncidentEvent, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, errors.New("评论内容不能为空")
	}
	var count int64
	if err := s.db.Model(&models.Incident{}).Where("id = ?", id).Count(&count).Error; err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, gorm.ErrRecordNotFound
	}
	event := models.IncidentEvent{IncidentID: id, Type: "comment", ActorID: actorID, Actor: actor, Content: content, CreatedAt: time.Now()}
	return &event, s.db.Create(&event).Error
}

func (s *IncidentService) Resolve(id uint, actorID *uint, actor, verification string, evidence any) (*models.Incident, error) {
	verification = strings.TrimSpace(verification)
	if verification == "" {
		return nil, errors.New("解决事件必须提交验证说明")
	}
	evidenceJSON, err := marshalIncidentEvidence(evidence)
	if err != nil {
		return nil, err
	}
	var incident models.Incident
	err = s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&incident, id).Error; err != nil {
			return err
		}
		if incident.Status == models.IncidentStatusResolved {
			return errors.New("事件已经解决")
		}
		now := time.Now()
		if err := tx.Model(&incident).Updates(map[string]any{
			"status": models.IncidentStatusResolved, "resolved_at": now, "active_key": nil,
		}).Error; err != nil {
			return err
		}
		return tx.Create(&models.IncidentEvent{IncidentID: id, Type: "verification", ActorID: actorID, Actor: actor, Content: verification, Evidence: evidenceJSON, CreatedAt: now}).Error
	})
	if err != nil {
		return nil, err
	}
	return &incident, s.db.First(&incident, id).Error
}

func (s *IncidentService) componentLogs(ctx context.Context, component *models.Component) ([]string, string) {
	const lines = 100
	var logs []string
	var err error
	switch component.DeployType {
	case "binary":
		logs, err = deployer.NewBinaryDeployer(s.db, "").GetLogs(ctx, component, lines)
	case "target":
		return []string{}, "target 类型没有可读取的 journal 或容器日志"
	default:
		target := component
		if component.ServiceName == "" {
			copy := *component
			copy.ServiceName = component.Name
			target = &copy
		}
		if target.ServiceName == "" {
			return []string{}, "组件未记录容器名称，无法读取 Docker 日志"
		}
		logs, err = deployer.NewDockerDeployer("").GetLogs(ctx, target, lines)
	}
	if err != nil {
		return []string{}, err.Error()
	}
	return logs, ""
}

func incidentActiveKey(observation IncidentObservation) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d|%s", observation.Kind, observation.ResourceType, observation.ResourceID, observation.SourceKey)))
	return fmt.Sprintf("%x", sum[:])
}

func marshalIncidentEvidence(value any) (string, error) {
	if value == nil {
		return "", nil
	}
	if text, ok := value.(string); ok {
		if strings.TrimSpace(text) == "" {
			return "", nil
		}
		if json.Valid([]byte(text)) {
			return text, nil
		}
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("编码事件证据失败: %w", err)
	}
	return string(encoded), nil
}
