package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/promlifecycle/platform/models"
)

var ErrStaleChangePlan = errors.New("change plan baseline is stale; create a fresh plan")

func componentFingerprint(component *models.Component) string {
	// Only managed desired/runtime identity participates: polling timestamps and
	// health state must not invalidate an otherwise unchanged reviewed plan.
	state := []interface{}{component.Type, component.Name, component.HostID, component.ClusterID, component.Version, component.Image, component.DeployType, component.Port, component.Config, component.ConfigPath, component.DataPath, component.InstallPath, component.ServiceName, component.CustomArgs, component.CustomEnv, component.ResourceLimits, component.ExtraVolumes, component.DataPathOverride}
	data, _ := json.Marshal(state)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (s *ChangeService) validateBaseline(ctx context.Context, plan *models.ChangePlan, component *models.Component) error {
	var baseline models.ChangeBaseline
	if err := s.db.First(&baseline, "plan_id = ?", plan.ID).Error; err != nil {
		return fmt.Errorf("%w: missing reviewed baseline", ErrStaleChangePlan)
	}
	if baseline.Fingerprint != componentFingerprint(component) {
		return ErrStaleChangePlan
	}
	if plan.Kind == models.ChangeKindConfig {
		var before configChange
		if err := json.Unmarshal(plan.Before, &before); err != nil {
			return err
		}
		if err := s.db.Preload("Host").First(component, component.ID).Error; err != nil {
			return err
		}
		_, content, err := s.readLiveConfig(ctx, component, before.Name)
		if err != nil {
			return err
		}
		if content != before.Content {
			return ErrStaleChangePlan
		}
	}
	return nil
}
