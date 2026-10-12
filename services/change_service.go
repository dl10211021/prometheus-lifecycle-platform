package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/promlifecycle/platform/core/config"
	"github.com/promlifecycle/platform/core/deployer"
	"github.com/promlifecycle/platform/core/ssh"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"gorm.io/gorm"
)

const maxVerificationBody = 64 << 10

// ChangeActor is the authenticated identity responsible for a managed change.
// Roles still control who may execute, but there is no approval workflow.
type ChangeActor struct {
	ID       uint
	Username string
	Role     string
}

// ChangePlanRequest is the common input used by lifecycle APIs. Before state
// is always loaded by the service rather than trusted from the caller.
type ChangePlanRequest struct {
	Kind         models.ChangeKind `json:"kind" yaml:"kind"`
	ResourceType string            `json:"resource_type" yaml:"resource_type"`
	ResourceID   uint              `json:"resource_id" yaml:"resource_id"`
	Reason       string            `json:"reason" yaml:"reason"`
	Risk         string            `json:"risk,omitempty" yaml:"risk,omitempty"`
	After        json.RawMessage   `json:"after" yaml:"-"`
	Draft        bool              `json:"draft,omitempty" yaml:"draft,omitempty"`
}

type configChange struct {
	Name        string `json:"name" yaml:"name"`
	Content     string `json:"content" yaml:"content"`
	ContentType string `json:"content_type" yaml:"content_type"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
}

type upgradeChange struct {
	NewVersion     string `json:"new_version" yaml:"new_version"`
	TargetImage    string `json:"target_image,omitempty" yaml:"target_image,omitempty"`
	PairedRecovery bool   `json:"paired_recovery,omitempty" yaml:"paired_recovery,omitempty"`
}

func validatePairedRecoveryUpgrade(component *models.Component, desired upgradeChange) error {
	if desired.PairedRecovery && (component == nil || component.Type != models.ComponentPrometheus || component.DeployType != "binary") {
		return errors.New("paired recovery is supported only for binary Prometheus upgrades")
	}
	return nil
}

type removeChange struct {
	Mode string `json:"mode" yaml:"mode"`
}

type componentRemovalSnapshot struct {
	Component models.Component          `json:"component"`
	Configs   []models.ComponentConfig  `json:"configs,omitempty"`
	Versions  []models.ComponentVersion `json:"versions,omitempty"`
}

// ChangeService is the only writer for managed config, upgrade and removal
// mutations. It keeps recovery, execution and evidence atomic.
type ChangeService struct {
	db        *gorm.DB
	factory   *deployer.Factory
	configs   *config.Manager
	backups   *BackupService
	basePath  string
	incidents *IncidentService
	client    *http.Client
}

func NewChangeService(db *gorm.DB, factory *deployer.Factory, configs *config.Manager, backups *BackupService, basePath string) *ChangeService {
	if backups == nil {
		backups = NewBackupService(db)
	}
	return &ChangeService{
		db: db, factory: factory, configs: configs, backups: backups, basePath: basePath,
		client: &http.Client{Timeout: 8 * time.Second}, incidents: NewIncidentService(db),
	}
}

func (s *ChangeService) List(status, kind string, offset, limit int) ([]models.ChangePlan, int64, error) {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := s.db.Model(&models.ChangePlan{})
	if status != "" {
		q = q.Where("status = ?", status)
	}
	if kind != "" {
		q = q.Where("kind = ?", kind)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var plans []models.ChangePlan
	err := q.Order("created_at DESC, id DESC").Offset(offset).Limit(limit).Find(&plans).Error
	return plans, total, err
}

func (s *ChangeService) Get(id uint) (*models.ChangePlan, error) {
	var plan models.ChangePlan
	if err := s.db.First(&plan, id).Error; err != nil {
		return nil, err
	}
	return &plan, nil
}

func (s *ChangeService) Create(ctx context.Context, req ChangePlanRequest, actor ChangeActor) (*models.ChangePlan, error) {
	if strings.TrimSpace(req.Reason) == "" {
		return nil, errors.New("变更原因不能为空")
	}
	if actor.ID == 0 || strings.TrimSpace(actor.Username) == "" {
		return nil, errors.New("缺少变更申请人身份")
	}
	if req.ResourceType == "" {
		req.ResourceType = "component"
	}
	if req.ResourceType != "component" || req.ResourceID == 0 {
		return nil, errors.New("目前仅支持明确指定的组件资源")
	}
	ctx, release, claimErr := ClaimComponentOperation(ctx, s.db, req.ResourceID, "change-plan")
	if claimErr != nil {
		return nil, claimErr
	}
	defer release()
	var component models.Component
	if err := s.db.Preload("Host").Preload("Cluster").First(&component, req.ResourceID).Error; err != nil {
		return nil, fmt.Errorf("加载变更组件: %w", err)
	}

	before, after, diff, assessedRisk, preflight, err := s.prepare(ctx, req, &component)
	if err != nil {
		return nil, err
	}
	risk, err := elevateRisk(assessedRisk, req.Risk)
	if err != nil {
		return nil, err
	}
	verification, _ := json.Marshal(map[string]interface{}{"preflight": preflight})
	status := models.ChangeStatusDraft
	requestedAt := time.Now()
	plan := &models.ChangePlan{
		Kind: req.Kind, ResourceType: req.ResourceType, ResourceID: component.ID, ResourceName: component.Name,
		RequestedByID: actor.ID, RequestedBy: actor.Username, RequestedAt: requestedAt,
		Status: status, Reason: strings.TrimSpace(req.Reason),
		Before: before, After: after, Diff: diff, Risk: risk, Verification: verification,
	}
	if err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(plan).Error; err != nil {
			return err
		}
		return tx.Create(&models.ChangeBaseline{PlanID: plan.ID, Fingerprint: componentFingerprint(&component)}).Error
	}); err != nil {
		return nil, fmt.Errorf("保存变更计划: %w", err)
	}
	return plan, nil
}

func (s *ChangeService) UpdateDraft(ctx context.Context, id uint, req ChangePlanRequest, actor ChangeActor) (*models.ChangePlan, error) {
	current, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	if current.QueueManaged {
		return nil, errors.New("批次生成的计划不能单独编辑，请在批次中查看进度或重试失败项")
	}
	if current.Status != models.ChangeStatusDraft || current.RequestedByID != actor.ID {
		return nil, errors.New("只有申请人可以修改自己的草稿")
	}
	req.Kind = current.Kind
	req.ResourceType = current.ResourceType
	req.ResourceID = current.ResourceID
	created, err := s.Create(ctx, req, actor)
	if err != nil {
		return nil, err
	}
	updates := map[string]interface{}{
		"reason": created.Reason, "before": created.Before, "after": created.After, "diff": created.Diff,
		"risk": created.Risk, "verification": created.Verification, "status": created.Status,
	}
	if err := s.db.Transaction(func(tx *gorm.DB) error {
		var baseline models.ChangeBaseline
		if err := tx.First(&baseline, "plan_id = ?", created.ID).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.ChangeBaseline{}).Where("plan_id = ?", current.ID).Update("fingerprint", baseline.Fingerprint).Error; err != nil {
			return err
		}
		return tx.Delete(&baseline).Error
	}); err != nil {
		return nil, err
	}
	if err := s.db.Delete(created).Error; err != nil {
		return nil, err
	}
	if err := s.db.Model(current).Updates(updates).Error; err != nil {
		return nil, err
	}
	return s.Get(id)
}

func (s *ChangeService) DeleteDraft(id uint, actor ChangeActor) error {
	result := s.db.Where("id = ? AND status = ? AND requested_by_id = ? AND queue_managed = ?", id, models.ChangeStatusDraft, actor.ID, false).Delete(&models.ChangePlan{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errors.New("只有申请人可以删除自己的草稿")
	}
	return nil
}

func recoveryScopeForPlan(plan *models.ChangePlan, component *models.Component) (string, bool, error) {
	switch plan.Kind {
	case models.ChangeKindConfig:
		return "config", true, nil
	case models.ChangeKindUpgrade:
		switch {
		case component.ConfigPath != "" && component.DataPath != "":
			return "full", true, nil
		case component.ConfigPath != "":
			return "config", true, nil
		case component.DataPath != "":
			return "data", true, nil
		default:
			if component.DeployType == "binary" || component.DeployType == "docker" || component.DeployType == "windows" {
				return "", false, nil
			}
			return "", false, errors.New("该部署方式没有可验证的升级恢复机制")
		}
	case models.ChangeKindRemove:
		var desired removeChange
		if err := json.Unmarshal(plan.After, &desired); err != nil {
			return "", false, err
		}
		if component.DeployType == "target" || desired.Mode == "keep_service" {
			return "", false, nil
		}
		if desired.Mode == "keep_data" {
			if component.ConfigPath != "" {
				return "config", true, nil
			}
			if component.DeployType == "binary" || component.DeployType == "docker" || component.DeployType == "windows" {
				return "", false, nil
			}
			return "", false, errors.New("该部署方式没有可验证的服务恢复机制")
		}
		switch {
		case component.ConfigPath != "" && component.DataPath != "":
			return "full", true, nil
		case component.ConfigPath != "":
			return "config", true, nil
		case component.DataPath != "":
			return "data", true, nil
		default:
			if component.DeployType == "binary" || component.DeployType == "docker" || component.DeployType == "windows" {
				return "", false, nil
			}
			return "", false, errors.New("组件没有可备份的配置或数据路径，不能完整删除")
		}
	default:
		return "", false, errors.New("不支持的变更类型")
	}
}

func rollbackWithoutArtifactAllowed(plan *models.ChangePlan, component *models.Component) bool {
	if plan.Kind == models.ChangeKindUpgrade {
		return component.DeployType == "binary" || component.DeployType == "docker" || component.DeployType == "windows"
	}
	if plan.Kind != models.ChangeKindRemove {
		return false
	}
	var desired removeChange
	if json.Unmarshal(plan.After, &desired) != nil {
		return false
	}
	if component.DeployType == "target" || desired.Mode == "keep_service" {
		return true
	}
	return (component.DeployType == "binary" || component.DeployType == "docker" || component.DeployType == "windows") &&
		((desired.Mode == "keep_data" && component.ConfigPath == "") ||
			(desired.Mode == "full_delete" && component.ConfigPath == "" && component.DataPath == ""))
}

func (s *ChangeService) Execute(ctx context.Context, id uint, actor ChangeActor) (*models.ChangePlan, error) {
	plan, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	if plan.QueueManaged {
		return nil, errors.New("该计划由批次执行，请在批次中查看进度或重试失败项")
	}
	if actor.Role != "change_admin" && actor.Role != "system_admin" {
		return nil, errors.New("执行变更需要 change_admin 或 system_admin 角色")
	}
	claimOperation := ClaimComponentOperation
	if plan.Kind == models.ChangeKindUpgrade {
		claimOperation = ClaimComponentVersionChange
	}
	ctx, release, claimErr := claimOperation(ctx, s.db, plan.ResourceID, "change-execute")
	if claimErr != nil {
		return nil, claimErr
	}
	defer release()
	// Lock order: component claim, monitoring publication, then per-file locks.
	// Keep the same boundary through verification and automatic compensation so
	// generated snapshots cannot observe ownership between file and DB writes.
	if plan.Kind != models.ChangeKindConfig {
		return s.executeClaimed(ctx, plan)
	}
	var result *models.ChangePlan
	err = config.WithMonitoringLock(func() error {
		result, err = s.executeClaimed(ctx, plan)
		return err
	})
	return result, err
}

// executeClaimed requires the component claim and, for config changes, the
// monitoring lock. Lower-level apply/save/rollback helpers must not reacquire it.
func (s *ChangeService) executeClaimed(ctx context.Context, plan *models.ChangePlan) (*models.ChangePlan, error) {
	executableStatuses := []models.ChangeStatus{
		models.ChangeStatusDraft,
		models.ChangeStatusPending,
		models.ChangeStatusApproved,
	}
	var baselineComponent models.Component
	if err := s.db.Preload("Host").First(&baselineComponent, plan.ResourceID).Error; err != nil {
		return nil, err
	}
	if err := validateQueuedUpgradeTarget(ctx, &baselineComponent); err != nil {
		return nil, err
	}
	if err := s.validateBaseline(ctx, plan, &baselineComponent); err != nil {
		return nil, err
	}
	pairedRecovery := false
	if plan.Kind == models.ChangeKindUpgrade {
		var desired upgradeChange
		if err := json.Unmarshal(plan.After, &desired); err != nil {
			return nil, err
		}
		if err := validatePairedRecoveryUpgrade(&baselineComponent, desired); err != nil {
			return nil, err
		}
		pairedRecovery = desired.PairedRecovery
		if _, err := CheckComponentTargetVersion(s.db, &baselineComponent, desired.NewVersion); err != nil {
			return nil, err
		}
		if err := admitPrometheusPlan(plan, &baselineComponent, desired.NewVersion); err != nil {
			return nil, err
		}
		if managedPrometheus(&baselineComponent) {
			if err := s.backups.verifyPrometheusResolvedRoots(ctx, &baselineComponent); err != nil {
				return nil, errors.Join(ErrPrometheusMigrationAdmission, err)
			}
		}
	}
	claim := s.db.Model(&models.ChangePlan{}).
		Where("id = ? AND status IN ?", plan.ID, executableStatuses).
		Update("status", models.ChangeStatusExecuting)
	if claim.Error != nil {
		return nil, claim.Error
	}
	if claim.RowsAffected != 1 {
		return nil, errors.New("计划已被其他执行者领取")
	}

	var component models.Component
	if err := s.db.Preload("Host").Preload("Cluster").First(&component, plan.ResourceID).Error; err != nil {
		s.markFailed(plan.ID, err)
		s.observeChangeFailure(plan, nil, "load_resource", err, nil)
		failed, _ := s.Get(plan.ID)
		return failed, err
	}
	if err := validateQueuedUpgradeTarget(ctx, &component); err != nil {
		s.markFailed(plan.ID, err)
		s.observeChangeFailure(plan, &component, "target_binding", err, nil)
		failed, _ := s.Get(plan.ID)
		return failed, err
	}
	if plan.Kind == models.ChangeKindRemove && component.DeployType == "binary" {
		var desired removeChange
		preflightErr := json.Unmarshal(plan.After, &desired)
		if preflightErr == nil && desired.Mode == "keep_data" {
			preflightErr = deployer.NewBinaryDeployer(s.db, s.factory.BasePath()).ValidateRemoveKeepData(ctx, &component)
		}
		if preflightErr != nil {
			s.markFailed(plan.ID, preflightErr)
			s.observeChangeFailure(plan, &component, "removal_preflight", preflightErr, nil)
			failed, _ := s.Get(plan.ID)
			return failed, preflightErr
		}
	}
	if pairedRecovery {
		if _, captureErr := s.backups.CreatePrometheusRecoveryPoint(ctx, plan.ID); captureErr != nil {
			failure := fmt.Errorf("create paired recovery point: %w", captureErr)
			s.markFailed(plan.ID, failure)
			s.observeChangeFailure(plan, &component, "paired_recovery_point", failure, nil)
			failed, _ := s.Get(plan.ID)
			return failed, failure
		}
		reloaded, reloadErr := s.Get(plan.ID)
		if reloadErr != nil {
			failure := fmt.Errorf("reload paired recovery plan: %w", reloadErr)
			s.markFailed(plan.ID, failure)
			s.observeChangeFailure(plan, &component, "paired_recovery_point", failure, nil)
			failed, _ := s.Get(plan.ID)
			return failed, failure
		}
		plan = reloaded
	}
	scope, backupRequired, recoveryErr := recoveryScopeForPlan(plan, &component)
	if recoveryErr != nil {
		failure := fmt.Errorf("准备恢复点: %w", recoveryErr)
		s.markFailed(plan.ID, failure)
		s.observeChangeFailure(plan, &component, "backup", failure, nil)
		failed, _ := s.Get(plan.ID)
		return failed, failure
	}
	var backup *models.Backup
	backupID := uint(0)
	if backupRequired {
		createdBackup, _, createErr := s.backups.CreateComponentBackup(ctx, &component, fmt.Sprintf("change-%d-%s", plan.ID, time.Now().Format("20060102-150405")), scope)
		if createErr != nil {
			failure := fmt.Errorf("创建恢复点: %w", createErr)
			s.markFailed(plan.ID, failure)
			s.observeChangeFailure(plan, &component, "backup", failure, map[string]interface{}{"scope": scope})
			failed, _ := s.Get(plan.ID)
			return failed, failure
		}
		backup = createdBackup
		if _, verifyErr := s.backups.VerifyComponentBackup(ctx, backup, &component); verifyErr != nil {
			failure := fmt.Errorf("验证恢复点: %w", verifyErr)
			s.markFailed(plan.ID, failure)
			s.observeChangeFailure(plan, &component, "backup_verification", failure, map[string]interface{}{"backup_id": backup.ID})
			failed, _ := s.Get(plan.ID)
			return failed, failure
		}
		backupID = backup.ID
		plan.BackupID = &backup.ID
		if err := s.db.Model(plan).Update("backup_id", backup.ID).Error; err != nil {
			return nil, err
		}
	}

	op := &models.Operation{ComponentID: &component.ID, ClusterID: &component.ClusterID, Type: "change-" + string(plan.Kind), Status: "running"}
	op.Input = string(plan.After)
	if err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(op).Error; err != nil {
			return err
		}
		linked := tx.Model(&models.ChangePlan{}).Where("id = ? AND status = ?", plan.ID, models.ChangeStatusExecuting).Update("operation_id", op.ID)
		if linked.Error != nil {
			return linked.Error
		}
		if linked.RowsAffected != 1 {
			return errors.New("变更计划状态已改变，未开始执行")
		}
		return nil
	}); err != nil {
		s.markFailed(plan.ID, err)
		s.observeChangeFailure(plan, &component, "operation_record", err, map[string]interface{}{"backup_id": backupID})
		failed, _ := s.Get(plan.ID)
		return failed, err
	}
	plan.OperationID = &op.ID

	failureStage := "apply"
	applyErr := s.apply(ctx, plan, &component)
	if applyErr == nil {
		failureStage = "verify"
		var evidence map[string]interface{}
		evidence, applyErr = s.verify(ctx, plan, &component)
		data, recordErr := mergeVerification(plan.Verification, "postflight", evidence, applyErr)
		if recordErr == nil {
			record := s.db.Model(plan).Update("verification", data)
			if plan.Kind == models.ChangeKindUpgrade && managedPrometheus(&component) {
				recordErr = prometheusTransitionRecordError(record, "postflight evidence")
			}
		}
		if recordErr != nil && plan.Kind == models.ChangeKindUpgrade && managedPrometheus(&component) {
			failureStage = "record_postflight"
			applyErr = errors.Join(applyErr, recordErr)
		}
	}
	if applyErr != nil {
		if errors.Is(applyErr, ErrDependencyCompatibility) || errors.Is(applyErr, ErrPrometheusMigrationAdmission) || errors.Is(applyErr, deployer.ErrKeepDataUnsafe) {
			// The final admission check ran before any component mutation. Do not
			// restart/rewrite an untouched service through automatic compensation.
			now := time.Now()
			_ = s.db.Model(op).Updates(map[string]interface{}{"status": "failed", "message": applyErr.Error(), "completed_at": &now}).Error
			s.markFailed(plan.ID, applyErr)
			stage := "dependency_preflight"
			if errors.Is(applyErr, deployer.ErrKeepDataUnsafe) {
				stage = "removal_preflight"
			}
			s.observeChangeFailure(plan, &component, stage, applyErr, nil)
			failed, _ := s.Get(plan.ID)
			return failed, applyErr
		}
		return s.rollbackAfterFailure(ctx, plan, backup, &component, op, failureStage, applyErr)
	}

	var appliedComponent models.Component
	if err := s.db.First(&appliedComponent, component.ID).Error; err != nil {
		return s.rollbackAfterFailure(ctx, plan, backup, &component, op, "record_baseline", err)
	}
	baselineRecord := s.db.Model(&models.ChangeBaseline{}).Where("plan_id = ?", plan.ID).Update("applied_fingerprint", componentFingerprint(&appliedComponent))
	baselineErr := baselineRecord.Error
	if plan.Kind == models.ChangeKindUpgrade && managedPrometheus(&component) {
		baselineErr = prometheusTransitionRecordError(baselineRecord, "applied baseline")
	}
	if baselineErr != nil {
		return s.rollbackAfterFailure(ctx, plan, backup, &component, op, "record_baseline", baselineErr)
	}
	now := time.Now()
	opRecord := s.db.Model(op).Updates(map[string]interface{}{"status": "success", "message": "变更已验证", "completed_at": &now})
	managedTransition := plan.Kind == models.ChangeKindUpgrade && managedPrometheus(&component)
	if managedTransition {
		if err := prometheusTransitionRecordError(opRecord, "operation"); err != nil {
			return nil, s.recordPrometheusTransitionFailure(plan, op, err)
		}
	}
	planRecord := s.db.Model(plan).Updates(map[string]interface{}{"status": models.ChangeStatusVerified, "executed_at": &now, "verified_at": &now})
	if managedTransition {
		if err := prometheusTransitionRecordError(planRecord, "plan"); err != nil {
			return nil, s.recordPrometheusTransitionFailure(plan, op, err)
		}
		if err := s.finishPrometheusTransition(plan); err != nil {
			return nil, s.recordPrometheusTransitionFailure(plan, op, err)
		}
	} else if planRecord.Error != nil {
		return nil, planRecord.Error
	}

	s.resolveChangeIncident(plan, "变更已执行并验证")
	return s.Get(plan.ID)
}

func (s *ChangeService) Verify(ctx context.Context, id uint) (*models.ChangePlan, error) {
	plan, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	ctx, release, claimErr := ClaimComponentOperation(ctx, s.db, plan.ResourceID, "change-verify")
	if claimErr != nil {
		return nil, claimErr
	}
	defer release()
	if plan.Status != models.ChangeStatusExecuting && plan.Status != models.ChangeStatusVerified {
		return nil, errors.New("只有执行中或已验证的计划可以再次验证")
	}
	var component models.Component
	if plan.Kind == models.ChangeKindRemove {
		var snapshot componentRemovalSnapshot
		if err := json.Unmarshal(plan.Before, &snapshot); err != nil {
			return nil, err
		}
		component = snapshot.Component
	} else if err := s.db.Preload("Host").First(&component, plan.ResourceID).Error; err != nil {
		return nil, err
	}
	var reviewed models.ChangeBaseline
	var current models.Component
	if err := s.db.First(&reviewed, "plan_id = ?", plan.ID).Error; err != nil {
		return nil, ErrStaleChangePlan
	}
	if err := s.db.First(&current, plan.ResourceID).Error; err != nil {
		return nil, err
	}
	if reviewed.AppliedFingerprint == "" || reviewed.AppliedFingerprint != componentFingerprint(&current) {
		return nil, ErrStaleChangePlan
	}
	evidence, verifyErr := s.verify(ctx, plan, &component)
	data, _ := mergeVerification(plan.Verification, "manual", evidence, verifyErr)
	updates := map[string]interface{}{"verification": data}
	if verifyErr == nil {
		now := time.Now()
		updates["status"] = models.ChangeStatusVerified
		updates["verified_at"] = &now
	}
	if err := s.db.Model(plan).Updates(updates).Error; err != nil {
		return nil, err
	}
	if verifyErr != nil {
		s.observeChangeFailure(plan, &component, "verify", verifyErr, evidence)
		updated, _ := s.Get(id)
		return updated, verifyErr
	}
	s.resolveChangeIncident(plan, "变更已再次验证成功")
	return s.Get(id)
}

func (s *ChangeService) Rollback(ctx context.Context, id uint, actor ChangeActor) (*models.ChangePlan, error) {
	plan, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	if actor.Role != "change_admin" && actor.Role != "system_admin" {
		return nil, errors.New("回滚需要 change_admin 或 system_admin 角色")
	}
	claimOperation := ClaimComponentOperation
	if plan.Kind == models.ChangeKindUpgrade {
		claimOperation = ClaimComponentVersionChange
	}
	ctx, release, claimErr := claimOperation(ctx, s.db, plan.ResourceID, "change-rollback")
	if claimErr != nil {
		return nil, claimErr
	}
	defer release()
	if plan.Kind != models.ChangeKindConfig {
		return s.rollbackClaimed(ctx, plan)
	}
	var result *models.ChangePlan
	err = config.WithMonitoringLock(func() error {
		result, err = s.rollbackClaimed(ctx, plan)
		return err
	})
	return result, err
}

// rollbackClaimed uses the same claim-before-monitoring order as Execute and
// holds publication exclusion from the live baseline check through restoration
// and reactivation of the previous manual config owner.
func (s *ChangeService) rollbackClaimed(ctx context.Context, plan *models.ChangePlan) (*models.ChangePlan, error) {
	if plan.Status != models.ChangeStatusVerified && plan.Status != models.ChangeStatusFailed && plan.Status != models.ChangeStatusRolledBack {
		return nil, errors.New("只有完成或失败的计划可以回滚")
	}
	if plan.Status == models.ChangeStatusRolledBack {
		return plan, nil
	}
	var newer int64
	if err := s.db.Model(&models.ChangePlan{}).Where("resource_type = ? AND resource_id = ? AND id > ? AND operation_id IS NOT NULL", plan.ResourceType, plan.ResourceID, plan.ID).Count(&newer).Error; err != nil {
		return nil, err
	}
	if newer != 0 {
		return nil, ErrStaleChangePlan
	}
	if plan.Status == models.ChangeStatusVerified {
		var baseline models.ChangeBaseline
		var current models.Component
		if err := s.db.First(&baseline, "plan_id = ?", plan.ID).Error; err != nil {
			return nil, ErrStaleChangePlan
		}
		if err := s.db.Preload("Host").First(&current, plan.ResourceID).Error; err != nil {
			return nil, err
		}
		if baseline.AppliedFingerprint == "" || baseline.AppliedFingerprint != componentFingerprint(&current) {
			return nil, ErrStaleChangePlan
		}
		if plan.Kind == models.ChangeKindConfig {
			var desired configChange
			if err := json.Unmarshal(plan.After, &desired); err != nil {
				return nil, err
			}
			_, content, err := s.readLiveConfig(ctx, &current, desired.Name)
			if err != nil {
				return nil, err
			}
			if content != desired.Content {
				return nil, ErrStaleChangePlan
			}
		}
	}
	var backup *models.Backup
	backupID := uint(0)
	if plan.BackupID != nil {
		var stored models.Backup
		if err := s.db.First(&stored, *plan.BackupID).Error; err != nil {
			failure := fmt.Errorf("加载恢复点: %w", err)
			s.observeChangeFailure(plan, nil, "rollback", failure, map[string]interface{}{"backup_id": plan.BackupID})
			return nil, failure
		}
		backup = &stored
		backupID = stored.ID
	}
	var component models.Component
	if err := s.restoreComponentRecord(plan, &component); err != nil {
		s.observeChangeFailure(plan, nil, "rollback", err, map[string]interface{}{"backup_id": backupID})
		return nil, err
	}
	if backup == nil && !rollbackWithoutArtifactAllowed(plan, &component) {
		failure := errors.New("计划没有恢复点，无法安全回滚")
		s.observeChangeFailure(plan, &component, "rollback", failure, nil)
		return nil, failure
	}
	if err := s.rollback(ctx, plan, backup, &component); err != nil {
		s.observeChangeFailure(plan, &component, "rollback", err, map[string]interface{}{"backup_id": backupID, "manual": true})
		return nil, err
	}
	evidence := map[string]interface{}{"manual": true, "backup_id": backupID, "restored_at": time.Now().UTC()}
	data, _ := json.Marshal(evidence)
	now := time.Now()
	if err := s.db.Model(plan).Updates(map[string]interface{}{
		"status": models.ChangeStatusRolledBack, "rollback_evidence": data, "rolled_back_at": &now,
	}).Error; err != nil {
		return nil, err
	}
	s.resolveChangeIncident(plan, "回滚已完成并验证")
	return s.Get(plan.ID)
}

func (s *ChangeService) prepare(ctx context.Context, req ChangePlanRequest, component *models.Component) (json.RawMessage, json.RawMessage, string, string, map[string]interface{}, error) {
	switch req.Kind {
	case models.ChangeKindConfig:
		if component.DeployType == "target" {
			return nil, nil, "", "", nil, errors.New("仅注册监控目标没有可管理配置，不能创建配置变更")
		}
		if strings.TrimSpace(component.ConfigPath) == "" {
			return nil, nil, "", "", nil, errors.New("组件未记录配置路径，不能创建配置变更")
		}
		var desired configChange
		if err := json.Unmarshal(req.After, &desired); err != nil {
			return nil, nil, "", "", nil, errors.New("配置变更 after 无效")
		}
		if !utils.SafePathSegment(desired.Name) || strings.TrimSpace(desired.Content) == "" {
			return nil, nil, "", "", nil, errors.New("配置名或内容无效")
		}
		if utils.HasRedactedSecrets(desired.Content) {
			return nil, nil, "", "", nil, errors.New("脱敏配置不能执行变更；请读取授权原文并重新输入密钥")
		}
		if len(desired.Content) > 4<<20 {
			return nil, nil, "", "", nil, errors.New("配置内容超过 4 MiB 安全上限")
		}
		if desired.ContentType == "" {
			desired.ContentType = "yaml"
		}
		semantic, err := s.validateConfigContent(ctx, component, desired)
		if err != nil {
			return nil, nil, "", "", nil, err
		}
		var active models.ComponentConfig
		_ = s.db.Where("component_id = ? AND name = ? AND is_active = ?", component.ID, desired.Name, true).Order("version DESC").First(&active).Error
		if active.ID == 0 {
			livePath, liveContent, readErr := s.readLiveConfig(ctx, component, desired.Name)
			if readErr != nil {
				return nil, nil, "", "", nil, fmt.Errorf("读取首次配置变更基线失败: %w", readErr)
			}
			active = models.ComponentConfig{
				ComponentID: component.ID, Name: desired.Name, Content: liveContent,
				ContentType: desired.ContentType, Version: 1, FilePath: livePath,
				Description: "首次受管配置变更前自动捕获的运行基线",
			}
		}
		beforeState := configChange{Name: desired.Name, Content: active.Content, ContentType: desired.ContentType, Description: active.Description}
		before, _ := json.Marshal(beforeState)
		after, _ := json.Marshal(desired)
		diff := unifiedDiff(active.Content, desired.Content, "before/"+desired.Name, "after/"+desired.Name)
		risk := models.ChangeRiskMedium
		changes := strings.Count(diff, "\n+") + strings.Count(diff, "\n-")
		if changes <= 10 {
			risk = models.ChangeRiskLow
		} else if changes > 100 {
			risk = models.ChangeRiskHigh
		}
		return before, after, diff, risk, map[string]interface{}{"semantic_validation": semantic}, nil
	case models.ChangeKindUpgrade:
		var desired upgradeChange
		if err := json.Unmarshal(req.After, &desired); err != nil {
			return nil, nil, "", "", nil, errors.New("升级目标版本无效")
		}
		if err := validatePairedRecoveryUpgrade(component, desired); err != nil {
			return nil, nil, "", "", nil, err
		}
		if !managedPrometheus(component) && !utils.ValidVersion(desired.NewVersion) {
			return nil, nil, "", "", nil, errors.New("升级目标版本无效")
		}
		if !managedPrometheus(component) && desired.NewVersion == component.Version {
			return nil, nil, "", "", nil, errors.New("升级目标版本与当前版本相同")
		}
		preflight, risk, err := s.preflightUpgrade(ctx, component, desired)
		if err != nil {
			return nil, nil, "", "", preflight, err
		}
		if managedPrometheus(component) {
			desired.NewVersion, _ = prometheusArtifactVersion(desired.NewVersion)
		}
		snapshot := prometheusBefore{Component: component, Version: component.Version, Image: component.Image}
		if managedPrometheus(component) {
			snapshot.ImportedArgsSHA256 = importedArgsHash(component.ImportedArgs)
		}
		before, _ := json.Marshal(snapshot)
		after, _ := json.Marshal(desired)
		diff := unifiedDiff(component.Version+"\n"+component.Image, desired.NewVersion+"\n"+desired.TargetImage, "before/version", "after/version")
		return before, after, diff, risk, preflight, nil
	case models.ChangeKindRemove:
		var desired removeChange
		if err := json.Unmarshal(req.After, &desired); err != nil {
			return nil, nil, "", "", nil, errors.New("移除选项无效")
		}
		if desired.Mode != "keep_service" && desired.Mode != "keep_data" && desired.Mode != "full_delete" {
			return nil, nil, "", "", nil, errors.New("移除必须明确选择 keep_service、keep_data 或 full_delete")
		}
		var configs []models.ComponentConfig
		if err := s.db.Where("component_id = ?", component.ID).Order("version ASC").Find(&configs).Error; err != nil {
			return nil, nil, "", "", nil, err
		}
		var versions []models.ComponentVersion
		if err := s.db.Where("component_id = ?", component.ID).Order("created_at ASC").Find(&versions).Error; err != nil {
			return nil, nil, "", "", nil, err
		}
		before, _ := json.Marshal(componentRemovalSnapshot{Component: *component, Configs: configs, Versions: versions})
		after, _ := json.Marshal(desired)
		risk := models.ChangeRiskMedium
		if desired.Mode == "full_delete" {
			risk = models.ChangeRiskHigh
		}
		diff := fmt.Sprintf("--- before/component-%d\n+++ after/component-%d\n@@ removal @@\n-%s %s\n+removal_mode=%s\n", component.ID, component.ID, component.Type, component.Name, desired.Mode)
		return before, after, diff, risk, map[string]interface{}{"removal_mode": desired.Mode, "recovery_scope": "full"}, nil
	default:
		return nil, nil, "", "", nil, errors.New("kind 仅支持 config、upgrade、remove")
	}
}

func (s *ChangeService) validateConfigContent(ctx context.Context, component *models.Component, desired configChange) (map[string]interface{}, error) {
	switch desired.ContentType {
	case "yaml", "yml":
		if err := config.ValidateYAML([]byte(desired.Content)); err != nil {
			return nil, fmt.Errorf("YAML 配置校验失败: %w", err)
		}
	case "json":
		if err := config.ValidateJSON([]byte(desired.Content)); err != nil {
			return nil, fmt.Errorf("JSON 配置校验失败: %w", err)
		}
	default:
		return nil, errors.New("content_type 仅支持 yaml 或 json")
	}
	result := map[string]interface{}{"level": "full", "message": "结构校验通过"}
	if desired.ContentType == "yaml" || desired.ContentType == "yml" {
		tmp, err := os.CreateTemp("", "change-config-*")
		if err != nil {
			return nil, err
		}
		name := tmp.Name()
		defer os.Remove(name)
		if _, err = tmp.WriteString(desired.Content); err == nil {
			err = tmp.Close()
		} else {
			_ = tmp.Close()
		}
		if err != nil {
			return nil, err
		}
		var validation config.ValidationResult
		switch component.Type {
		case models.ComponentPrometheus:
			toolPath, toolErr := deployer.NewBinaryDeployer(s.db, s.factory.BasePath()).ValidationTool(ctx, component)
			if toolErr != nil {
				return nil, fmt.Errorf("准备 Prometheus 配置校验器失败: %w", toolErr)
			}
			if filepath.Base(toolPath) == "promtool" || strings.HasPrefix(filepath.Base(toolPath), "promtool-") {
				validation, err = config.ValidateConfigArtifactWithBinary(string(component.Type), name, toolPath)
			} else {
				validation, err = config.ValidatePrometheusConfigWithBinary(name, toolPath)
			}
		case models.ComponentAlertmanager:
			toolPath, toolErr := deployer.NewBinaryDeployer(s.db, s.factory.BasePath()).ValidationTool(ctx, component)
			if toolErr != nil {
				return nil, fmt.Errorf("准备 Alertmanager 配置校验器失败: %w", toolErr)
			}
			validation, err = config.ValidateConfigArtifactWithBinary(string(component.Type), name, toolPath)
		default:
			validation, err = config.ValidateConfigArtifact(string(component.Type), name)
		}
		if err != nil {
			return nil, err
		}
		result = map[string]interface{}{"level": validation.Level, "tool": validation.Tool, "message": validation.Message}
	}
	return result, nil
}

func versionCandidates(version string) []string {
	trimmed := strings.TrimSpace(version)
	normalized := strings.TrimPrefix(trimmed, "v")
	candidates := []string{trimmed}
	if normalized != trimmed {
		candidates = append(candidates, normalized)
	} else if normalized != "" {
		candidates = append(candidates, "v"+normalized)
	}
	return candidates
}

func (s *ChangeService) preflightUpgrade(ctx context.Context, component *models.Component, desired upgradeChange) (map[string]interface{}, string, error) {
	evidence := map[string]interface{}{
		"target_version": desired.NewVersion, "current_version": component.Version, "port": component.Port,
		"health": component.Status, "rollback_version": component.Version, "rollback_image": component.Image,
	}
	if component.Type == models.ComponentPrometheus && component.DeployType == "binary" {
		evidence["parameter_migration"] = deployer.PreviewPrometheusParameterMigration(component, desired.NewVersion)
	}
	if component.Type == models.ComponentPrometheus || component.Type == models.ComponentThanos {
		dependents, err := CheckComponentTargetVersion(s.db, component, desired.NewVersion)
		evidence["dependent_compatibility"] = dependents
		evidence["dependency_scope"] = "same_host"
		if err != nil {
			return evidence, models.ChangeRiskHigh, err
		}
	}
	migration, migrationErr := checkPrometheusMigration(component, desired.NewVersion)
	evidence["migration"] = migration
	if migrationErr != nil {
		return evidence, models.ChangeRiskHigh, migrationErr
	}
	if managedPrometheus(component) {
		if err := s.backups.verifyPrometheusResolvedRoots(ctx, component); err != nil {
			return evidence, models.ChangeRiskHigh, errors.Join(ErrPrometheusMigrationAdmission, err)
		}
	}
	if component.Status != models.StatusRunning && component.DeployType != "target" {
		return evidence, models.ChangeRiskMedium, errors.New("组件当前未运行，不能安全升级")
	}
	healthEvidence, healthErr := s.verify(ctx, &models.ChangePlan{Kind: models.ChangeKindUpgrade}, component)
	evidence["health_check"] = healthEvidence
	if healthErr != nil {
		return evidence, models.ChangeRiskHigh, fmt.Errorf("升级前健康检查失败: %w", healthErr)
	}
	if component.Version == "" || (component.Image == "" && component.DeployType == "docker") {
		return evidence, models.ChangeRiskHigh, errors.New("缺少明确的回滚版本或镜像")
	}
	lookupVersion := desired.NewVersion
	if managedPrometheus(component) {
		lookupVersion, _ = prometheusArtifactVersion(lookupVersion)
	}
	candidates := versionCandidates(lookupVersion)
	var pkg models.Package
	pkgErr := s.db.Where("type = ? AND version IN ?", component.Type, candidates).Order("created_at DESC").First(&pkg).Error
	if pkgErr != nil {
		var available models.ComponentVersion
		if err := s.db.Where("type = ? AND version IN ?", component.Type, candidates).Order("created_at DESC").First(&available).Error; err != nil {
			return evidence, models.ChangeRiskMedium, errors.New("目标包/版本尚未登记，请先下载或登记该版本")
		}
		evidence["package_source"] = available.Source
		evidence["package_image"] = available.Image
	} else {
		evidence["package_id"] = pkg.ID
		evidence["package_path"] = pkg.StoragePath
		evidence["package_checksum"] = pkg.Checksum
		evidence["package_size"] = pkg.Size
	}
	var conflict int64
	if component.Port > 0 {
		s.db.Model(&models.Component{}).Where("host_id = ? AND port = ? AND id <> ? AND status <> ?", component.HostID, component.Port, component.ID, "removed").Count(&conflict)
	}
	if conflict > 0 {
		return evidence, models.ChangeRiskHigh, fmt.Errorf("目标端口 %d 已被其他组件占用", component.Port)
	}
	evidence["port_available"] = true

	path := component.InstallPath
	if path == "" {
		path = component.DataPath
	}
	if path == "" {
		path = s.basePath
	}
	if path == "" && s.factory != nil {
		path = s.factory.BasePath()
	}
	if path == "" {
		path = "/"
	}
	free, err := availableBytes(ctx, component, path)
	if err != nil {
		return evidence, models.ChangeRiskMedium, fmt.Errorf("检查目标主机可用空间: %w", err)
	}
	evidence["space_path"] = path
	evidence["space_host"] = component.Host.IP
	required := pkg.Size * 2
	if required < 256<<20 {
		required = 256 << 20
	}
	evidence["free_bytes"] = free
	evidence["required_bytes"] = required
	if free < required {
		return evidence, models.ChangeRiskHigh, errors.New("目标空间不足，无法保留升级包和回滚副本")
	}

	risk := models.ChangeRiskMedium
	if majorVersion(component.Version) != "" && majorVersion(component.Version) != majorVersion(desired.NewVersion) {
		risk = models.ChangeRiskHigh
	}
	evidence["risk_basis"] = map[bool]string{true: "major_version_change", false: "same_major_upgrade"}[risk == models.ChangeRiskHigh]
	return evidence, risk, nil
}

func componentLiveConfigPath(component *models.Component, name string) (string, error) {
	base := strings.TrimSpace(component.ConfigPath)
	if base == "" {
		return "", errors.New("组件未记录实际配置路径")
	}
	if component.DeployType == "docker" {
		return filepath.Join(base, name), nil
	}
	return base, nil
}

func (s *ChangeService) writeLiveConfig(ctx context.Context, component *models.Component, name, content string) (string, error) {
	target, err := componentLiveConfigPath(component, name)
	if err != nil {
		return "", err
	}
	if ssh.IsLocalHost(component.Host.IP, component.Host.ConnectionType) {
		if err := utils.AtomicWriteFile(target, []byte(content), 0644); err != nil {
			return "", fmt.Errorf("原子写入实际配置: %w", err)
		}
	} else {
		client, err := ssh.NewClient(component.Host.IP, component.Host.Port, component.Host.Username, component.Host.Password, component.Host.PrivateKey)
		if err != nil {
			return "", fmt.Errorf("连接配置目标主机: %w", err)
		}
		defer client.Close()
		if _, _, err := client.RunCommand("mkdir -p " + utils.QuoteShell(filepath.Dir(target))); err != nil {
			return "", fmt.Errorf("创建远程配置目录: %w", err)
		}
		if err := client.CopyFileContext(ctx, strings.NewReader(content), target, 0644, int64(len(content))); err != nil {
			return "", fmt.Errorf("原子写入远程实际配置: %w", err)
		}
	}
	if component.Status != models.StatusRunning {
		return target, nil
	}
	if component.DeployType == "binary" {
		if err := deployer.NewBinaryDeployer(s.db, s.factory.BasePath()).Restart(ctx, component); err != nil {
			return "", err
		}
	} else if component.DeployType == "docker" {
		if err := s.factory.Deployer().Restart(ctx, component); err != nil {
			return "", err
		}
	}
	return target, nil
}

func (s *ChangeService) readLiveConfig(ctx context.Context, component *models.Component, name string) (string, string, error) {
	target, err := componentLiveConfigPath(component, name)
	if err != nil {
		return "", "", err
	}
	if ssh.IsLocalHost(component.Host.IP, component.Host.ConnectionType) {
		content, err := os.ReadFile(target)
		if err != nil {
			return target, "", err
		}
		return target, string(content), nil
	}
	client, err := ssh.NewClient(component.Host.IP, component.Host.Port, component.Host.Username, component.Host.Password, component.Host.PrivateKey)
	if err != nil {
		return target, "", err
	}
	defer client.Close()
	content, _, err := client.RunCommand("cat -- " + utils.QuoteShell(target))
	return target, content, err
}

func (s *ChangeService) apply(ctx context.Context, plan *models.ChangePlan, component *models.Component) error {
	switch plan.Kind {
	case models.ChangeKindConfig:
		var desired configChange
		if err := json.Unmarshal(plan.After, &desired); err != nil {
			return err
		}
		if _, err := s.configs.SaveConfig(component.Cluster.Name, component.Name, desired.Name, []byte(desired.Content)); err != nil {
			return err
		}
		livePath, err := s.writeLiveConfig(ctx, component, desired.Name, desired.Content)
		if err != nil {
			return err
		}
		var maxVersion int
		if err := s.db.Model(&models.ComponentConfig{}).Where("component_id = ? AND name = ?", component.ID, desired.Name).
			Select("COALESCE(MAX(version), 0)").Scan(&maxVersion).Error; err != nil {
			return err
		}
		var previous configChange
		if err := json.Unmarshal(plan.Before, &previous); err != nil {
			return err
		}
		return s.db.Transaction(func(tx *gorm.DB) error {
			if maxVersion == 0 && previous.Content != "" {
				baseline := models.ComponentConfig{
					ComponentID: component.ID, Name: previous.Name, Content: previous.Content,
					ContentType: previous.ContentType, Version: 1, IsActive: false,
					FilePath: livePath, Description: previous.Description,
				}
				if err := tx.Create(&baseline).Error; err != nil {
					return err
				}
				maxVersion = 1
			}
			if err := tx.Model(&models.ComponentConfig{}).Where("component_id = ? AND name = ?", component.ID, desired.Name).Update("is_active", false).Error; err != nil {
				return err
			}
			cfg := models.ComponentConfig{
				ComponentID: component.ID, Name: desired.Name, Content: desired.Content, ContentType: desired.ContentType,
				Version: maxVersion + 1, IsActive: true, FilePath: livePath, Description: desired.Description,
			}
			return tx.Create(&cfg).Error
		})
	case models.ChangeKindUpgrade:
		var desired upgradeChange
		if err := json.Unmarshal(plan.After, &desired); err != nil {
			return err
		}
		if _, err := CheckComponentTargetVersion(s.db, component, desired.NewVersion); err != nil {
			return err
		}
		if err := admitPrometheusPlan(plan, component, desired.NewVersion); err != nil {
			return err
		}
		if managedPrometheus(component) {
			if err := s.backups.verifyPrometheusResolvedRoots(ctx, component); err != nil {
				return errors.Join(ErrPrometheusMigrationAdmission, err)
			}
		}
		if managedPrometheus(component) {
			desired.NewVersion, _ = prometheusArtifactVersion(desired.NewVersion)
		}
		if managedPrometheus(component) {
			if err := s.beginPrometheusTransition(ctx, plan, component); err != nil {
				return err
			}
		}
		oldVersion, oldImage := component.Version, component.Image
		component.Version = desired.NewVersion
		if desired.TargetImage != "" {
			component.Image = desired.TargetImage
		}
		if err := s.db.Model(component).Updates(map[string]interface{}{"status": models.StatusUpgrading, "version": component.Version, "image": component.Image, "error": ""}).Error; err != nil {
			return err
		}
		componentID := component.ID
		if err := s.db.Create(&models.ComponentVersion{Type: component.Type, Version: oldVersion, Image: oldImage, Source: "change_plan", ComponentID: &componentID,
			Notes: fmt.Sprintf("Recovery target for change plan %d", plan.ID)}).Error; err != nil {
			return err
		}
		var err error
		switch component.DeployType {
		case "binary":
			err = deployer.NewBinaryDeployer(s.db, s.factory.BasePath()).Upgrade(ctx, component, desired.NewVersion)
		case "windows":
			err = deployer.NewWindowsDeployer(s.db, s.factory.BasePath()).Deploy(ctx, component)
		case "target":
		default:
			err = s.factory.Deployer().Upgrade(ctx, component, desired.NewVersion)
		}
		if err != nil {
			return err
		}
		return s.db.Model(component).Updates(map[string]interface{}{"status": models.StatusRunning, "version": component.Version, "image": component.Image, "error": ""}).Error
	case models.ChangeKindRemove:
		var desired removeChange
		if err := json.Unmarshal(plan.After, &desired); err != nil {
			return err
		}
		if desired.Mode != "keep_service" {
			switch component.DeployType {
			case "binary":
				binaryDeployer := deployer.NewBinaryDeployer(s.db, s.factory.BasePath())
				var removeErr error
				if desired.Mode == "keep_data" {
					removeErr = binaryDeployer.RemoveKeepData(ctx, component)
				} else {
					removeErr = binaryDeployer.Remove(ctx, component)
				}
				if removeErr != nil {
					return removeErr
				}
			case "windows":
				if err := deployer.NewWindowsDeployer(s.db, s.factory.BasePath()).Remove(ctx, component); err != nil {
					return err
				}
			case "target":
			default:
				if err := s.factory.Deployer().Delete(ctx, component); err != nil {
					return err
				}
			}
		}
		if desired.Mode == "full_delete" && component.DataPath != "" {
			if err := removeComponentData(ctx, component); err != nil {
				return err
			}
		}
		return s.deleteComponentRecords(component.ID)
	default:
		return errors.New("不支持的变更类型")
	}
}

func changeReadinessPath(component *models.Component) string {
	return models.ManagedHealthPath(component)
}

func (s *ChangeService) verify(ctx context.Context, plan *models.ChangePlan, component *models.Component) (map[string]interface{}, error) {
	evidence := map[string]interface{}{"checked_at": time.Now().UTC(), "kind": plan.Kind}
	if plan.Kind == models.ChangeKindConfig {
		var desired configChange
		if err := json.Unmarshal(plan.After, &desired); err != nil {
			return evidence, err
		}
		target, actual, err := s.readLiveConfig(ctx, component, desired.Name)
		evidence["config_file"] = map[string]interface{}{"path": target, "matches": err == nil && strings.TrimSpace(actual) == strings.TrimSpace(desired.Content)}
		if err != nil {
			return evidence, fmt.Errorf("读取实际配置进行验收: %w", err)
		}
		if strings.TrimSpace(actual) != strings.TrimSpace(desired.Content) {
			return evidence, errors.New("实际配置内容与变更计划不一致")
		}
		if component.Status != models.StatusRunning {
			evidence["readiness"] = "not_applicable_component_not_running"
			return evidence, nil
		}
	}
	if plan.Kind == models.ChangeKindRemove {
		var count int64
		if err := s.db.Model(&models.Component{}).Where("id = ? AND status <> ?", plan.ResourceID, "removed").Count(&count).Error; err != nil {
			return evidence, err
		}
		evidence["component_retired"] = count == 0
		if count != 0 {
			return evidence, errors.New("组件记录仍然存在")
		}
		var desired removeChange
		if err := json.Unmarshal(plan.After, &desired); err != nil {
			return evidence, err
		}
		if component.DeployType == "target" || component.Port <= 0 {
			evidence["service_check"] = "not_applicable"
			return evidence, nil
		}
		host := component.Host.IP
		if host == "" || host == "0.0.0.0" {
			host = "127.0.0.1"
		}
		address := net.JoinHostPort(host, strconv.Itoa(component.Port))
		connection, dialErr := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp", address)
		if connection != nil {
			_ = connection.Close()
		}
		running := dialErr == nil
		evidence["service_check"] = map[string]interface{}{"address": address, "reachable": running, "mode": desired.Mode}
		if desired.Mode == "keep_service" && !running {
			return evidence, errors.New("keep_service 验证失败：原服务不可达")
		}
		if desired.Mode != "keep_service" && running {
			return evidence, errors.New("移除验证失败：服务端口仍可连接")
		}
		return evidence, nil
	}
	if component.DeployType == "target" {
		evidence["readiness"] = "not_applicable_target_mode"
		return evidence, nil
	}
	base := ""
	if ssh.IsLocalHost(component.Host.IP, component.Host.ConnectionType) {
		base = "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(component.Port))
	} else {
		base = strings.TrimRight(component.InternalURL, "/")
		if base == "" {
			host := component.Host.IP
			if host == "" || host == "0.0.0.0" {
				host = "127.0.0.1"
			}
			base = "http://" + net.JoinHostPort(host, strconv.Itoa(component.Port))
		}
	}
	readinessPath := changeReadinessPath(component)
	status, body, err := s.getWithRetry(ctx, base+readinessPath, 20, 500*time.Millisecond)
	evidence["readiness"] = map[string]interface{}{"url": base + readinessPath, "status": status, "body": body}
	if err != nil || status < 200 || status >= 300 {
		if err == nil {
			err = fmt.Errorf("readiness 返回 HTTP %d", status)
		}
		return evidence, err
	}
	if component.Type == models.ComponentPrometheus {
		checks := map[string]string{
			"critical_query": "/api/v1/query?query=up", "targets": "/api/v1/targets", "rules": "/api/v1/rules",
		}
		for name, path := range checks {
			var code int
			var response string
			var checkErr error
			ready := false
			for attempt := 0; attempt < 90; attempt++ {
				code, response, checkErr = s.get(ctx, base+path)
				if checkErr == nil && code >= 200 && code < 300 {
					ready, checkErr = prometheusVerificationReady(name, response)
					if checkErr == nil && ready {
						break
					}
				}
				select {
				case <-ctx.Done():
					return evidence, ctx.Err()
				case <-time.After(500 * time.Millisecond):
				}
			}
			evidence[name] = map[string]interface{}{"url": base + path, "status": code, "body": response, "ready": ready}
			if checkErr != nil {
				return evidence, fmt.Errorf("%s 验证失败: %w", name, checkErr)
			}
			if !ready {
				return evidence, fmt.Errorf("%s 未在 45 秒内恢复有效运行数据", name)
			}
		}
	}
	return evidence, nil
}

func prometheusVerificationReady(name, response string) (bool, error) {
	var envelope struct {
		Status string          `json:"status"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(response), &envelope); err != nil {
		return false, err
	}
	if envelope.Status != "success" {
		return false, errors.New("Prometheus API status is not success")
	}
	switch name {
	case "critical_query":
		var data struct {
			Result []json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(envelope.Data, &data); err != nil {
			return false, err
		}
		return len(data.Result) > 0, nil
	case "targets":
		var data struct {
			ActiveTargets []struct {
				Health string `json:"health"`
			} `json:"activeTargets"`
		}
		if err := json.Unmarshal(envelope.Data, &data); err != nil {
			return false, err
		}
		if len(data.ActiveTargets) == 0 {
			return false, nil
		}
		for _, target := range data.ActiveTargets {
			if target.Health == "unknown" {
				return false, nil
			}
		}
		return true, nil
	case "rules":
		var data struct {
			Groups []struct {
				Rules []struct {
					Health string `json:"health"`
				} `json:"rules"`
			} `json:"groups"`
		}
		if err := json.Unmarshal(envelope.Data, &data); err != nil {
			return false, err
		}
		for _, group := range data.Groups {
			for _, rule := range group.Rules {
				if rule.Health != "ok" {
					return false, nil
				}
			}
		}
		return true, nil
	default:
		return false, fmt.Errorf("unknown Prometheus verification %q", name)
	}
}

func (s *ChangeService) get(ctx context.Context, rawURL string) (int, string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return 0, "", errors.New("组件验证 URL 无效")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return 0, "", err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxVerificationBody))
	return resp.StatusCode, string(body), err
}

func (s *ChangeService) getWithRetry(ctx context.Context, rawURL string, attempts int, delay time.Duration) (int, string, error) {
	if attempts < 1 {
		attempts = 1
	}
	var status int
	var body string
	var lastErr error
	for range attempts {
		status, body, lastErr = s.get(ctx, rawURL)
		if lastErr == nil && status >= 200 && status < 300 {
			return status, body, nil
		}
		select {
		case <-ctx.Done():
			return status, body, ctx.Err()
		case <-time.After(delay):
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("readiness 返回 HTTP %d", status)
	}
	return status, body, lastErr
}

func (s *ChangeService) rollbackAfterFailure(ctx context.Context, plan *models.ChangePlan, backup *models.Backup, component *models.Component, op *models.Operation, stage string, cause error) (*models.ChangePlan, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
	defer cancel()
	unconfirmed := errors.Is(cause, deployer.ErrBinaryRecoveryUnconfirmed) || errors.Is(cause, ssh.ErrRemoteTerminationUnconfirmed)
	if !unconfirmed && plan.Kind == models.ChangeKindRemove {
		if err := s.restoreComponentRecord(plan, component); err != nil {
			s.observeChangeFailure(plan, component, "rollback", err, map[string]interface{}{"automatic": true})
			return nil, fmt.Errorf("恢复移除前组件记录: %w", err)
		}
	}
	var rollbackErr error
	if unconfirmed {
		rollbackErr = errors.Join(cause, s.backups.blockComponentRecovery(component.ID, cause))
	} else {
		rollbackErr = s.rollback(ctx, plan, backup, component)
		if errors.Is(rollbackErr, ErrDependencyCompatibility) || (rollbackErr != nil && plan.Kind == models.ChangeKindUpgrade && managedPrometheus(component)) {
			// Forward mutation already failed. Unsafe compensation must not leave
			// the component available for another operation after claims release.
			rollbackErr = errors.Join(rollbackErr, s.backups.blockComponentRecovery(component.ID, rollbackErr))
		}
	}
	backupID := uint(0)
	if backup != nil {
		backupID = backup.ID
	}
	evidence := map[string]interface{}{"automatic": true, "stage": stage, "cause": cause.Error(), "backup_id": backupID, "rolled_back_at": time.Now().UTC()}
	if rollbackErr != nil {
		evidence["rollback_error"] = rollbackErr.Error()
	}
	data, _ := json.Marshal(evidence)
	now := time.Now()
	status := models.ChangeStatusRolledBack
	if rollbackErr != nil {
		status = models.ChangeStatusFailed
	}
	message := cause.Error()
	if rollbackErr != nil {
		message += "; rollback: " + rollbackErr.Error()
	}
	opRecord := s.db.Model(op).Updates(map[string]interface{}{"status": "failed", "message": message, "completed_at": &now, "output": string(data)})
	planRecord := s.db.Model(plan).Updates(map[string]interface{}{
		"status": status, "executed_at": &now, "rolled_back_at": &now, "rollback_evidence": data,
	})
	if plan.Kind == models.ChangeKindUpgrade && managedPrometheus(component) {
		rollbackErr = errors.Join(rollbackErr, prometheusTransitionRecordError(opRecord, "rollback operation"), prometheusTransitionRecordError(planRecord, "rollback plan"))
		if rollbackErr == nil {
			rollbackErr = s.finishPrometheusTransition(plan)
		}
		if rollbackErr != nil {
			rollbackErr = s.recordPrometheusTransitionFailure(plan, op, rollbackErr)
		}
	}
	incidentStage := stage
	incidentCause := cause
	if rollbackErr != nil {
		incidentStage = "rollback"
		incidentCause = fmt.Errorf("%w；自动回滚失败: %v", cause, rollbackErr)
	}
	s.observeChangeFailure(plan, component, incidentStage, incidentCause, evidence)
	if rollbackErr == nil {
		s.resolveChangeIncident(plan, "自动回滚已完成并验证")
	}
	updated, _ := s.Get(plan.ID)
	if rollbackErr != nil {
		return updated, fmt.Errorf("变更执行失败，且自动回滚失败: %v；回滚错误: %w", cause, rollbackErr)
	}
	return updated, fmt.Errorf("变更执行失败，已自动回滚: %w", cause)
}

func (s *ChangeService) rollback(ctx context.Context, plan *models.ChangePlan, backup *models.Backup, component *models.Component) error {
	backupRestored := false
	if plan.Kind == models.ChangeKindUpgrade {
		var before struct {
			Version string `json:"version"`
			Image   string `json:"image"`
		}
		if err := json.Unmarshal(plan.Before, &before); err != nil {
			if managedPrometheus(component) {
				return errors.Join(ErrPrometheusRecoveryAdmission, err)
			}
			return err
		}
		// Both historical manual plans and automatic compensation must recheck
		// live companions before changing metadata, restoring data or runtime.
		if _, err := CheckComponentTargetVersion(s.db, component, before.Version); err != nil {
			return err
		}
		if managedPrometheus(component) {
			if err := s.admitPrometheusRecovery(ctx, plan, backup, component); err != nil {
				return err
			}
		}
		runtimeVersion := before.Version
		if managedPrometheus(component) {
			runtimeVersion, _ = prometheusArtifactVersion(before.Version)
		}
		component.Version = before.Version
		component.Image = before.Image
		restoreRuntime := func(ctx context.Context) error {
			switch component.DeployType {
			case "binary":
				return deployer.NewBinaryDeployer(s.db, s.factory.BasePath()).Upgrade(ctx, component, runtimeVersion)
			case "windows":
				return deployer.NewWindowsDeployer(s.db, s.factory.BasePath()).Deploy(ctx, component)
			case "target":
				return nil
			default:
				if managedPrometheus(component) {
					// Docker's Upgrade retags repository:version; historical
					// recovery must retain the exact recorded image reference.
					if err := s.factory.Deployer().RestorePrometheusImage(ctx, component); err != nil {
						return err
					}
					_, err := s.verify(ctx, plan, component)
					return err
				}
				return s.factory.Deployer().Upgrade(ctx, component, runtimeVersion)
			}
		}
		if managedPrometheus(component) {
			restoreRuntime = s.prometheusRuntimeRecovery(component, restoreRuntime)
		}
		if backup != nil && (backup.Type == "full" || backup.Type == "data") {
			if err := s.backups.restoreComponentBackup(ctx, backup, component, restoreRuntime); err != nil {
				return err
			}
			backupRestored = true
		} else if err := restoreRuntime(ctx); err != nil {
			return err
		}
	}
	if plan.Kind == models.ChangeKindRemove {
		var desired removeChange
		if err := json.Unmarshal(plan.After, &desired); err != nil {
			return err
		}
		if desired.Mode != "keep_service" {
			switch component.DeployType {
			case "binary":
				if err := deployer.NewBinaryDeployer(s.db, s.factory.BasePath()).Deploy(ctx, component); err != nil {
					return fmt.Errorf("重新部署二进制服务: %w", err)
				}
			case "windows":
				if err := deployer.NewWindowsDeployer(s.db, s.factory.BasePath()).Deploy(ctx, component); err != nil {
					return fmt.Errorf("重新部署 Windows 服务: %w", err)
				}
			case "docker":
				if err := s.factory.Deployer().Deploy(ctx, component, nil); err != nil {
					return fmt.Errorf("重新部署容器服务: %w", err)
				}
			}
			component.Status = models.StatusRunning
			component.Error = ""
			if err := s.db.Omit("Host", "Cluster", "Configs", "Operations").Save(component).Error; err != nil {
				return fmt.Errorf("保存重新部署后的组件状态: %w", err)
			}
		}
	}
	if backup != nil && !backupRestored {
		if err := s.backups.RestoreComponentBackup(ctx, backup, component); err != nil {
			return err
		}
	} else if backup == nil && !rollbackWithoutArtifactAllowed(plan, component) {
		return errors.New("计划没有可用恢复点，无法安全回滚")
	}
	if plan.Kind == models.ChangeKindConfig {
		var previous configChange
		if err := json.Unmarshal(plan.Before, &previous); err != nil {
			return err
		}
		var desired configChange
		if err := json.Unmarshal(plan.After, &desired); err != nil {
			return err
		}
		if err := s.db.Where(
			"component_id = ? AND name = ? AND content = ? AND is_active = ?",
			component.ID, desired.Name, desired.Content, true,
		).Delete(&models.ComponentConfig{}).Error; err != nil {
			return err
		}
		if err := s.db.Model(&models.ComponentConfig{}).Where("component_id = ? AND name = ?", component.ID, previous.Name).Update("is_active", false).Error; err != nil {
			return err
		}
		if previous.Content != "" {
			var old models.ComponentConfig
			if err := s.db.Where("component_id = ? AND name = ? AND content = ?", component.ID, previous.Name, previous.Content).Order("version DESC").First(&old).Error; err == nil {
				_ = s.db.Model(&old).Update("is_active", true).Error
			}
		}
		if component.Status == models.StatusRunning {
			switch component.DeployType {
			case "binary":
				if err := deployer.NewBinaryDeployer(s.db, s.factory.BasePath()).Restart(ctx, component); err != nil {
					return fmt.Errorf("重启恢复配置后的二进制服务: %w", err)
				}
			case "docker":
				if err := s.factory.Deployer().Restart(ctx, component); err != nil {
					return fmt.Errorf("重启恢复配置后的容器服务: %w", err)
				}
			}
		}
	}
	if plan.Kind == models.ChangeKindUpgrade {
		var before struct {
			Version string `json:"version"`
			Image   string `json:"image"`
		}
		_ = json.Unmarshal(plan.Before, &before)
		return s.db.Model(&models.Component{}).Where("id = ?", component.ID).Updates(map[string]interface{}{
			"version": before.Version, "image": before.Image, "status": models.StatusRunning, "error": "",
		}).Error
	}
	return nil
}

func (s *ChangeService) restoreComponentRecord(plan *models.ChangePlan, component *models.Component) error {
	err := s.db.Preload("Host").Preload("Cluster").First(component, plan.ResourceID).Error
	if err == nil {
		if component.Status == models.ComponentStatus("removed") && plan.Kind == models.ChangeKindRemove {
			var snapshot componentRemovalSnapshot
			if err := json.Unmarshal(plan.Before, &snapshot); err != nil {
				return err
			}
			component.Status = snapshot.Component.Status
			if err := s.db.Model(component).Update("status", component.Status).Error; err != nil {
				return err
			}
		}
		return nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) || plan.Kind != models.ChangeKindRemove {
		return err
	}
	var snapshot componentRemovalSnapshot
	if err := json.Unmarshal(plan.Before, &snapshot); err != nil {
		return err
	}
	*component = snapshot.Component
	component.Configs = nil
	component.Operations = nil
	if err := s.db.Omit("Host", "Cluster", "Configs", "Operations").Create(component).Error; err != nil {
		return fmt.Errorf("恢复组件记录: %w", err)
	}
	for i := range snapshot.Configs {
		snapshot.Configs[i].Component = models.Component{}
		if err := s.db.Omit("Component").Create(&snapshot.Configs[i]).Error; err != nil {
			return fmt.Errorf("恢复组件配置历史: %w", err)
		}
	}
	for i := range snapshot.Versions {
		if err := s.db.Create(&snapshot.Versions[i]).Error; err != nil {
			return fmt.Errorf("恢复组件版本历史: %w", err)
		}
	}
	return s.db.Preload("Host").Preload("Cluster").First(component, component.ID).Error
}

func (s *ChangeService) deleteComponentRecords(id uint) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&models.ScheduledBackup{}).Where("component_id = ?", id).Update("enabled", false).Error; err != nil {
			return err
		}
		if err := tx.Where("source_id = ? OR target_id = ?", id, id).Delete(&models.TopologyEdge{}).Error; err != nil {
			return err
		}
		return tx.Model(&models.Component{}).Where("id = ?", id).Updates(map[string]interface{}{"status": "removed", "error": ""}).Error
	})
}

func removeComponentData(ctx context.Context, component *models.Component) error {
	candidate := filepath.Clean(strings.TrimSpace(component.DataPath))
	if candidate == "" || candidate == "." || !filepath.IsAbs(candidate) {
		return errors.New("数据路径必须是明确的绝对路径")
	}
	protected := map[string]bool{
		"/": true, "/bin": true, "/boot": true, "/dev": true, "/etc": true, "/home": true,
		"/opt": true, "/proc": true, "/root": true, "/run": true, "/srv": true, "/sys": true,
		"/tmp": true, "/usr": true, "/var": true,
	}
	if protected[candidate] || len(strings.Split(strings.Trim(candidate, string(filepath.Separator)), string(filepath.Separator))) < 2 {
		return errors.New("拒绝删除根级或系统级数据路径")
	}
	if ssh.IsLocalHost(component.Host.IP, component.Host.ConnectionType) {
		if info, err := os.Lstat(candidate); err == nil && info.Mode()&os.ModeSymlink != 0 {
			return errors.New("拒绝删除符号链接数据路径")
		}
		return os.RemoveAll(candidate)

	}
	client, err := ssh.NewClientContext(ctx, component.Host.IP, component.Host.Port, component.Host.Username, component.Host.Password, component.Host.PrivateKey)
	if err != nil {
		return fmt.Errorf("连接目标主机删除数据: %w", err)
	}
	defer client.Close()
	if _, _, err := client.RunCommandContext(ctx, "rm -rf -- "+utils.QuoteShell(candidate)); err != nil {
		return fmt.Errorf("删除远程数据路径: %w", err)
	}
	return nil
}
func (s *ChangeService) observeChangeFailure(plan *models.ChangePlan, component *models.Component, stage string, cause error, details map[string]interface{}) {
	if plan == nil || cause == nil || s.incidents == nil {
		return
	}
	severity := "warning"
	if plan.Risk == models.ChangeRiskHigh || stage == "rollback" {
		severity = "critical"
	}
	evidence := map[string]interface{}{
		"change_plan_id": plan.ID, "kind": plan.Kind, "risk": plan.Risk, "stage": stage,
		"error": cause.Error(), "reason": plan.Reason, "backup_id": plan.BackupID,
		"operation_id": plan.OperationID, "observed_at": time.Now().UTC(),
	}
	for key, value := range details {
		evidence[key] = value
	}
	componentID := plan.ResourceID
	var clusterID *uint
	if component != nil && component.ClusterID != 0 {
		cluster := component.ClusterID
		clusterID = &cluster
	}
	_, _ = s.incidents.Observe(IncidentObservation{
		Kind: models.IncidentKindChange, Severity: severity, SourceKey: changeIncidentSourceKey(plan.ID),
		ResourceType: plan.ResourceType, ResourceID: plan.ResourceID, ResourceName: plan.ResourceName,
		ClusterID: clusterID, ComponentID: &componentID, ChangePlanID: &plan.ID,
		Title: fmt.Sprintf("变更 %s 执行失败", plan.ResourceName), Summary: cause.Error(),
		Evidence: evidence, ObservedAt: time.Now(),
	})
}

func (s *ChangeService) resolveChangeIncident(plan *models.ChangePlan, reason string) {
	if plan == nil || s.incidents == nil {
		return
	}
	_ = s.incidents.ResolveSource(models.IncidentKindChange, plan.ResourceType, plan.ResourceID, changeIncidentSourceKey(plan.ID), time.Now(), reason)
}

func changeIncidentSourceKey(planID uint) string {
	return fmt.Sprintf("change-plan:%d", planID)
}

func (s *ChangeService) markFailed(id uint, failure error) {
	var plan models.ChangePlan
	if err := s.db.First(&plan, id).Error; err != nil {
		return
	}
	data, _ := mergeVerification(plan.Verification, "execution_failure", map[string]interface{}{"error": failure.Error()}, failure)
	_ = s.db.Model(&plan).Updates(map[string]interface{}{
		"status": models.ChangeStatusFailed, "verification": data,
	}).Error
}

func availableBytes(ctx context.Context, component *models.Component, path string) (int64, error) {
	if ssh.IsLocalHost(component.Host.IP, component.Host.ConnectionType) {
		var stat syscall.Statfs_t
		if err := syscall.Statfs(path, &stat); err != nil {
			return 0, err
		}
		return int64(stat.Bavail) * int64(stat.Bsize), nil
	}
	client, err := ssh.NewClient(component.Host.IP, component.Host.Port, component.Host.Username, component.Host.Password, component.Host.PrivateKey)
	if err != nil {
		return 0, err
	}
	defer client.Close()
	output, _, err := client.RunCommand("df -Pk -- " + utils.QuoteShell(path))
	if err != nil {
		return 0, err
	}
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) < 2 {
		return 0, errors.New("目标主机 df 输出无效")
	}
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) < 4 {
		return 0, errors.New("目标主机 df 输出缺少可用空间")
	}
	kilobytes, err := strconv.ParseInt(fields[3], 10, 64)
	if err != nil {
		return 0, err
	}
	return kilobytes * 1024, nil
}

func elevateRisk(assessed, requested string) (string, error) {
	rank := map[string]int{models.ChangeRiskLow: 1, models.ChangeRiskMedium: 2, models.ChangeRiskHigh: 3}
	if _, ok := rank[assessed]; !ok {
		return "", errors.New("内部风险评估无效")
	}
	if requested == "" {
		return assessed, nil
	}
	if _, ok := rank[requested]; !ok {
		return "", errors.New("risk 仅支持 low、medium、high")
	}
	if rank[requested] > rank[assessed] {
		return requested, nil
	}
	return assessed, nil
}

func majorVersion(version string) string {
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	if index := strings.IndexByte(version, '.'); index >= 0 {
		version = version[:index]
	}
	if _, err := strconv.Atoi(version); err != nil {
		return ""
	}
	return version
}

func unifiedDiff(before, after, beforeName, afterName string) string {
	if before == after {
		return "--- " + beforeName + "\n+++ " + afterName + "\n"
	}
	a := strings.Split(strings.ReplaceAll(before, "\r\n", "\n"), "\n")
	b := strings.Split(strings.ReplaceAll(after, "\r\n", "\n"), "\n")
	// Bound memory for very large generated configs while retaining a unified,
	// reviewable representation.
	if len(a)*len(b) > 1_000_000 {
		return fmt.Sprintf("--- %s\n+++ %s\n@@ full replacement: %d -> %d lines @@\n-%s\n+%s\n", beforeName, afterName, len(a), len(b), before, after)
	}
	dp := make([][]int, len(a)+1)
	for i := range dp {
		dp[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n@@ -1,%d +1,%d @@\n", beforeName, afterName, len(a), len(b))
	for i, j := 0, 0; i < len(a) || j < len(b); {
		if i < len(a) && j < len(b) && a[i] == b[j] {
			out.WriteString(" " + a[i] + "\n")
			i++
			j++
		} else if j < len(b) && (i == len(a) || dp[i][j+1] > dp[i+1][j]) {
			out.WriteString("+" + b[j] + "\n")
			j++
		} else {
			out.WriteString("-" + a[i] + "\n")
			i++
		}
	}
	return out.String()
}

func mergeVerification(existing json.RawMessage, phase string, evidence map[string]interface{}, phaseErr error) (json.RawMessage, error) {
	combined := map[string]interface{}{}
	if len(existing) > 0 {
		_ = json.Unmarshal(existing, &combined)
	}
	entry := map[string]interface{}{"evidence": evidence, "passed": phaseErr == nil, "at": time.Now().UTC()}
	if phaseErr != nil {
		entry["error"] = phaseErr.Error()
	}
	combined[phase] = entry
	return json.Marshal(combined)
}
