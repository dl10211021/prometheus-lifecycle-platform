package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/promlifecycle/platform/models"
	"gorm.io/gorm"
)

// PreparedRemoval is a read-only, reviewed plan until the owning batch is
// committed. The ordinary change engine owns execution, verification and
// compensation; the queue only binds an instance to that plan.
type PreparedRemoval struct {
	Plan        models.ChangePlan
	Baseline    models.ChangeBaseline
	Component   models.Component
	Fingerprint string
}

func RemovalPlanDigest(plan *models.ChangePlan) string {
	return UpgradePlanDigest(plan)
}

func removalTargetFingerprint(component *models.Component, ownership string) string {
	host := component.Host
	data, _ := json.Marshal([]interface{}{
		component.ID, componentFingerprint(component), component.BinaryPath,
		component.ImportedArgs, component.ComposePath, component.InternalURL,
		host.ID, host.IP, host.Port, host.Username, host.ConnectionType,
		host.OS, host.WinRMPort, host.WinRMScheme, host.WinRMSkipVerify,
		ownership,
	})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (s *ChangeService) PrepareRemovalPlan(ctx context.Context, id uint, mode, reason string, actor ChangeActor) (*PreparedRemoval, error) {
	if actor.ID == 0 || actor.Username == "" || (actor.Role != "change_admin" && actor.Role != "system_admin") {
		return nil, errors.New("批量移除需要有效的变更管理员身份")
	}
	if id == 0 || strings.TrimSpace(reason) == "" || (mode != "keep_service" && mode != "keep_data") {
		return nil, errors.New("组件、移除方式和原因不能为空或无效")
	}
	ctx, release, err := ClaimComponentOperation(ctx, s.db, id, "remove-preview")
	if err != nil {
		return nil, err
	}
	defer release()
	var component models.Component
	if err := s.db.Preload("Host").Preload("Cluster").First(&component, id).Error; err != nil || component.Host.ID == 0 {
		return nil, errors.New("组件或主机不存在，请刷新列表")
	}
	if component.Status == "removed" || component.Status == "deploying" || component.Status == "upgrading" {
		return nil, errors.New("组件已移除或正在部署/升级")
	}
	if component.DeployType != "binary" && component.DeployType != "windows" {
		return nil, errors.New("仅受管 Linux binary 或 Windows 服务支持批量移除")
	}
	ownership := "platform"
	var adoption models.ComponentAdoption
	if err := s.db.Where("component_id = ?", id).First(&adoption).Error; err == nil {
		if adoption.Ownership != "managed" {
			return nil, errors.New("观察模式实例不能移除服务，请先完成托管接管")
		}
		ownership = "managed"
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("读取组件所有权: %w", err)
	}
	after, _ := json.Marshal(removeChange{Mode: mode})
	request := ChangePlanRequest{Kind: models.ChangeKindRemove, ResourceType: "component", ResourceID: id, Reason: reason, After: after}
	before, after, diff, risk, preflight, err := s.prepare(ctx, request, &component)
	if err != nil {
		return nil, err
	}
	verification, _ := json.Marshal(map[string]interface{}{"preflight": preflight})
	return &PreparedRemoval{
		Plan:        models.ChangePlan{QueueManaged: true, Kind: models.ChangeKindRemove, ResourceType: "component", ResourceID: id, ResourceName: component.Name, RequestedByID: actor.ID, RequestedBy: actor.Username, RequestedAt: time.Now(), Status: models.ChangeStatusDraft, Reason: strings.TrimSpace(reason), Before: before, After: after, Diff: diff, Risk: risk, Verification: verification},
		Baseline:    models.ChangeBaseline{Fingerprint: componentFingerprint(&component)},
		Component:   component,
		Fingerprint: removalTargetFingerprint(&component, ownership),
	}, nil
}

// ExecuteQueuedRemoval rechecks actor, ownership, component and host while the
// component claim is held, then invokes the original change execution engine.
func (s *ChangeService) ExecuteQueuedRemoval(ctx context.Context, id uint, digest, fingerprint, mode string) (*models.ChangePlan, error) {
	plan, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	ctx, release, err := ClaimComponentOperation(ctx, s.db, plan.ResourceID, "batch-remove")
	if err != nil {
		return nil, err
	}
	defer release()
	plan, err = s.Get(id)
	if err != nil {
		return nil, err
	}
	if !plan.QueueManaged || plan.Kind != models.ChangeKindRemove || plan.Status != models.ChangeStatusDraft || RemovalPlanDigest(plan) != digest {
		return nil, errors.New("批次移除计划已改变，请重新创建批次")
	}
	var user models.User
	if err := s.db.First(&user, plan.RequestedByID).Error; err != nil || !user.IsActive || (user.Role != "change_admin" && user.Role != "system_admin") {
		return nil, errors.New("移除申请人不存在、已停用或已失去变更权限")
	}
	actor := ChangeActor{ID: user.ID, Username: user.Username, Role: user.Role}
	prepared, err := s.PrepareRemovalPlan(ctx, plan.ResourceID, mode, plan.Reason, actor)
	if err != nil {
		return nil, err
	}
	var baseline models.ChangeBaseline
	if err := s.db.First(&baseline, "plan_id = ?", plan.ID).Error; err != nil {
		return nil, fmt.Errorf("读取移除计划基线: %w", err)
	}
	if prepared.Fingerprint != fingerprint || prepared.Baseline.Fingerprint != baseline.Fingerprint ||
		!bytes.Equal(prepared.Plan.After, plan.After) {
		return nil, ErrStaleChangePlan
	}
	return s.executeClaimed(ctx, plan)
}
