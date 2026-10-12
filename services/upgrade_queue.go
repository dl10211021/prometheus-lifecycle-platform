package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"gorm.io/gorm"
	"strings"
	"time"
)

// PreparedUpgrade has no persisted work. Save its plan and baseline in the queue
// transaction; Execute revalidates the component before modifying it.
type PreparedUpgrade struct {
	Plan              models.ChangePlan
	Baseline          models.ChangeBaseline
	Component         models.Component
	TargetFingerprint string
}

func (s *ChangeService) PrepareUpgradePlan(ctx context.Context, id uint, currentVersion, targetVersion, reason string, actor ChangeActor) (*PreparedUpgrade, error) {
	if actor.ID == 0 || actor.Username == "" || (actor.Role != "change_admin" && actor.Role != "system_admin") {
		return nil, errors.New("批量升级需要有效的变更管理员身份")
	}
	if id == 0 || strings.TrimSpace(reason) == "" || currentVersion == "" || !utils.ValidVersion(targetVersion) {
		return nil, errors.New("组件、当前版本、目标版本及升级原因不能为空或无效")
	}
	ctx, release, err := ClaimComponentOperation(ctx, s.db, id, "upgrade-preview")
	if err != nil {
		return nil, err
	}
	defer release()
	var component models.Component
	if err := s.db.Preload("Host").Preload("Cluster").First(&component, id).Error; err != nil {
		return nil, errors.New("组件不存在，请刷新列表")
	}
	if err := validateQueuedUpgradeTarget(ctx, &component); err != nil {
		return nil, err
	}
	if component.DeployType != "binary" || !utils.IsValidComponentType(string(component.Type)) {
		return nil, errors.New("请先将已有 Linux 组件托管接管；仅观察的实例不能批量升级")
	}
	var adoption models.ComponentAdoption
	if err := s.db.Where("component_id = ?", id).First(&adoption).Error; err == nil {
		if adoption.Ownership != "managed" {
			return nil, errors.New("该实例仅观察，请先完成托管接管")
		}
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	if strings.TrimPrefix(component.Version, "v") != strings.TrimPrefix(currentVersion, "v") {
		return nil, ErrStaleChangePlan
	}
	if strings.TrimPrefix(component.Version, "v") == strings.TrimPrefix(targetVersion, "v") {
		return nil, errors.New("目标版本与当前版本相同")
	}
	after, _ := json.Marshal(upgradeChange{NewVersion: targetVersion})
	request := ChangePlanRequest{Kind: models.ChangeKindUpgrade, ResourceType: "component", ResourceID: id, Reason: reason, After: after}
	before, after, diff, risk, preflight, err := s.prepare(ctx, request, &component)
	if err != nil {
		return nil, err
	}
	verification, _ := json.Marshal(map[string]interface{}{"preflight": preflight})
	return &PreparedUpgrade{
		Plan:     models.ChangePlan{QueueManaged: true, Kind: models.ChangeKindUpgrade, ResourceType: "component", ResourceID: id, ResourceName: component.Name, RequestedByID: actor.ID, RequestedBy: actor.Username, RequestedAt: time.Now(), Status: models.ChangeStatusDraft, Reason: strings.TrimSpace(reason), Before: before, After: after, Diff: diff, Risk: risk, Verification: verification},
		Baseline: models.ChangeBaseline{Fingerprint: componentFingerprint(&component)}, Component: component,
		TargetFingerprint: upgradeTargetFingerprint(&component),
	}, nil
}

// upgradeTargetFingerprint binds the reviewed instance and connection endpoint.
// Credentials and polling metadata are deliberately excluded: rotating a secret
// must not authorize a different target or invalidate an unchanged target.
func upgradeTargetFingerprint(component *models.Component) string {
	host := component.Host
	data, _ := json.Marshal([]interface{}{
		component.ID, componentFingerprint(component), component.BinaryPath,
		component.ImportedArgs, component.ComposePath, component.InternalURL,
		host.ID, host.IP, host.Port, host.Username, host.ConnectionType,
		host.OS, host.IsWindows, host.WinRMPort, host.WinRMScheme, host.WinRMSkipVerify,
	})
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

type queuedUpgradeTargetContextKey struct{}

func queuedUpgradeTargetContext(ctx context.Context, fingerprint string) (context.Context, error) {
	if fingerprint == "" {
		return ctx, fmt.Errorf("%w: 缺少原始升级目标绑定，请重新创建升级批次", ErrStaleChangePlan)
	}
	return context.WithValue(ctx, queuedUpgradeTargetContextKey{}, fingerprint), nil
}

// validateQueuedUpgradeTarget runs on the exact snapshot used by preflight or
// execution, before either boundary can touch the reviewed remote instance.
func validateQueuedUpgradeTarget(ctx context.Context, component *models.Component) error {
	fingerprint, bound := ctx.Value(queuedUpgradeTargetContextKey{}).(string)
	if bound && (component.Host.ID == 0 || upgradeTargetFingerprint(component) != fingerprint) {
		return ErrStaleChangePlan
	}
	return nil
}

// PrepareQueuedUpgradeRetry preserves the original target binding while making
// a fresh plan after a safe failure or verified rollback. Drift is checked before
// PrepareUpgradePlan performs any remote preflight or returns persistable work.
func (s *ChangeService) PrepareQueuedUpgradeRetry(ctx context.Context, id uint, fingerprint, currentVersion, targetVersion, reason string, actor ChangeActor) (*PreparedUpgrade, error) {
	ctx, err := queuedUpgradeTargetContext(ctx, fingerprint)
	if err != nil {
		return nil, err
	}
	return s.PrepareUpgradePlan(ctx, id, currentVersion, targetVersion, reason, actor)
}

// UpgradePlanDigest binds a queued step to immutable intent, excluding progress.
func UpgradePlanDigest(plan *models.ChangePlan) string {
	data, _ := json.Marshal([]interface{}{plan.Kind, plan.ResourceType, plan.ResourceID, plan.RequestedByID, plan.Reason, plan.Before, plan.After})
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

// ExecuteQueuedUpgrade holds the same lifecycle claim through the final
// preflight, baseline validation, execution and compensation. It executes the
// verified plan snapshot; public draft endpoints cannot alter queued plans.
func (s *ChangeService) ExecuteQueuedUpgrade(ctx context.Context, id uint, digest, fingerprint, targetFingerprint, currentVersion, targetVersion string) (*models.ChangePlan, error) {
	ctx, err := queuedUpgradeTargetContext(ctx, targetFingerprint)
	if err != nil {
		return nil, err
	}
	plan, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	ctx, release, err := ClaimComponentVersionChange(ctx, s.db, plan.ResourceID, "batch-upgrade")
	if err != nil {
		return nil, err
	}
	defer release()
	plan, err = s.Get(id)
	if err != nil {
		return nil, err
	}
	if !plan.QueueManaged || plan.Kind != models.ChangeKindUpgrade || plan.Status != models.ChangeStatusDraft || UpgradePlanDigest(plan) != digest {
		return nil, errors.New("批次计划已发生变化，请重新创建升级批次")
	}
	var user models.User
	if err := s.db.First(&user, plan.RequestedByID).Error; err != nil || !user.IsActive {
		return nil, errors.New("升级申请人不存在或已停用")
	}
	actor := ChangeActor{ID: user.ID, Username: user.Username, Role: user.Role}
	prepared, err := s.PrepareUpgradePlan(ctx, plan.ResourceID, currentVersion, targetVersion, plan.Reason, actor)
	if err != nil {
		return nil, err
	}
	if prepared.Baseline.Fingerprint != fingerprint || !bytes.Equal(prepared.Plan.After, plan.After) {
		return nil, ErrStaleChangePlan
	}
	return s.executeClaimed(ctx, plan)
}
