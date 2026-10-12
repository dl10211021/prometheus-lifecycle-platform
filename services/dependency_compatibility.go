package services

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/promlifecycle/platform/models"
	"gorm.io/gorm"
)

// Stable releases only; the scoped Sidecar rule is based on the Prometheus 3
// external-label fix in https://github.com/thanos-io/thanos/releases/tag/v0.37.0.
var stableDependencyVersion = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)

// ThanosPrometheusCompatibility evaluates the existing scoped Sidecar rule,
// not certification of an entire deployment or arbitrary future releases.
func ThanosPrometheusCompatibility(thanosVersion, prometheusVersion string) string {
	parse := func(version string) ([3]int, bool) {
		var result [3]int
		match := stableDependencyVersion.FindStringSubmatch(strings.TrimSpace(version))
		if match == nil {
			return result, false
		}
		for i := range result {
			number, err := strconv.Atoi(match[i+1])
			if err != nil {
				return result, false
			}
			result[i] = number
		}
		return result, true
	}
	thanos, thanosKnown := parse(thanosVersion)
	prometheus, prometheusKnown := parse(prometheusVersion)
	if !thanosKnown || !prometheusKnown || thanos[0] != 0 || (prometheus[0] != 2 && prometheus[0] != 3) {
		return "unknown"
	}
	if prometheus[0] == 3 && thanos[1] < 37 {
		return "incompatible"
	}
	return "compatible"
}

// ErrDependencyCompatibility means admission failed before a component was
// mutated. Execution must report failure without rolling back untouched state.
var ErrDependencyCompatibility = errors.New("依赖兼容性检查未通过")

type DependentCompatibility struct {
	DependencyID      uint                   `json:"dependency_id"`
	Name              string                 `json:"name"`
	Status            models.ComponentStatus `json:"status"`
	ThanosVersion     string                 `json:"thanos_version"`
	PrometheusVersion string                 `json:"prometheus_version"`
	Compatibility     string                 `json:"compatibility"`
}

// CheckPrometheusDependents follows the platform's current same-host dependency
// model. A stopped/failed retained instance can be restarted, so it still counts.
// Cross-host custom endpoints require explicit topology mapping separately.
func CheckPrometheusDependents(db *gorm.DB, hostID uint, targetVersion string) ([]DependentCompatibility, error) {
	var instances []models.Component
	if err := db.Where("host_id = ? AND type = ? AND (status IS NULL OR status <> ?) AND (deploy_type IS NULL OR deploy_type <> ?)", hostID, models.ComponentThanos, "removed", "target").Order("id ASC").Find(&instances).Error; err != nil {
		return nil, fmt.Errorf("%w：查询已有 Thanos 失败: %v", ErrDependencyCompatibility, err)
	}
	evidence := make([]DependentCompatibility, 0, len(instances))
	var problem error
	for _, instance := range instances {
		compatibility := ThanosPrometheusCompatibility(instance.Version, targetVersion)
		evidence = append(evidence, DependentCompatibility{DependencyID: instance.ID, Name: instance.Name, Status: instance.Status, ThanosVersion: instance.Version, PrometheusVersion: targetVersion, Compatibility: compatibility})
		if compatibility != "compatible" && problem == nil {
			if compatibility == "unknown" {
				problem = fmt.Errorf("%w：无法确认 Prometheus %s 与已有 Thanos %s（ID %d，版本 %q）的兼容性，请核对完整正式版本", ErrDependencyCompatibility, targetVersion, instance.Name, instance.ID, instance.Version)
			} else {
				problem = fmt.Errorf("%w：Prometheus %s 与已有 Thanos %s（ID %d，版本 %s）不兼容；请先将 Thanos 升级至支持目标版本的正式版本，再升级或重新部署 Prometheus", ErrDependencyCompatibility, targetVersion, instance.Name, instance.ID, instance.Version)
			}
		}
	}
	return evidence, problem
}

// CheckComponentTargetVersion validates the requested version against live retained
// companions under the caller's same-host version-change claim. It deliberately
// does not use versions promised by another pending plan.
func CheckComponentTargetVersion(db *gorm.DB, component *models.Component, targetVersion string) ([]DependentCompatibility, error) {
	switch component.Type {
	case models.ComponentPrometheus:
		return CheckPrometheusDependents(db, component.HostID, targetVersion)
	case models.ComponentThanos:
		return checkThanosTargetVersion(db, component.HostID, targetVersion)
	default:
		return nil, nil
	}
}

// checkThanosTargetVersion uses the same retained-instance boundary as the
// reverse check. Thanos requires at least one same-host Prometheus instance.
func checkThanosTargetVersion(db *gorm.DB, hostID uint, targetVersion string) ([]DependentCompatibility, error) {
	var instances []models.Component
	if err := db.Where("host_id = ? AND type = ? AND (status IS NULL OR status <> ?) AND (deploy_type IS NULL OR deploy_type <> ?)", hostID, models.ComponentPrometheus, "removed", "target").Order("id ASC").Find(&instances).Error; err != nil {
		return nil, fmt.Errorf("%w：查询已有 Prometheus 失败: %v", ErrDependencyCompatibility, err)
	}
	evidence := make([]DependentCompatibility, 0, len(instances))
	if len(instances) == 0 {
		return evidence, fmt.Errorf("%w：Thanos %s 缺少同主机保留的 Prometheus 实例，请先部署并确认依赖版本", ErrDependencyCompatibility, targetVersion)
	}
	var problem error
	for _, instance := range instances {
		compatibility := ThanosPrometheusCompatibility(targetVersion, instance.Version)
		evidence = append(evidence, DependentCompatibility{DependencyID: instance.ID, Name: instance.Name, Status: instance.Status, ThanosVersion: targetVersion, PrometheusVersion: instance.Version, Compatibility: compatibility})
		if compatibility != "compatible" && problem == nil {
			if compatibility == "unknown" {
				problem = fmt.Errorf("%w：无法确认 Thanos %s 与已有 Prometheus %s（ID %d，版本 %q）的兼容性，请核对完整正式版本", ErrDependencyCompatibility, targetVersion, instance.Name, instance.ID, instance.Version)
			} else {
				problem = fmt.Errorf("%w：Thanos %s 与已有 Prometheus %s（ID %d，版本 %s）不兼容；请选择支持所有保留 Prometheus 实例的 Thanos 正式版本", ErrDependencyCompatibility, targetVersion, instance.Name, instance.ID, instance.Version)
			}
		}
	}
	return evidence, problem
}

// ClaimComponentVersionChange shares deployment's host exclusion for the two
// version-coupled components. Claims are reentrant through the returned context.
func ClaimComponentVersionChange(ctx context.Context, db *gorm.DB, id uint, kind string) (context.Context, func(), error) {
	var component models.Component
	if err := db.WithContext(ctx).First(&component, id).Error; err != nil {
		return ctx, nil, err
	}
	if component.Type != models.ComponentPrometheus && component.Type != models.ComponentThanos {
		return ClaimComponentOperation(ctx, db, id, kind)
	}
	hostCtx, releaseHost, err := ClaimHostOperation(ctx, db, component.HostID, kind)
	if err != nil {
		return ctx, nil, err
	}
	claimed, releaseComponent, err := ClaimComponentOperation(hostCtx, db, id, kind)
	if err != nil {
		releaseHost()
		return ctx, nil, err
	}
	release := func() { releaseComponent(); releaseHost() }
	// A record may have moved between the initial lookup and taking its claim.
	var current models.Component
	if err := db.WithContext(claimed).First(&current, id).Error; err != nil {
		release()
		return ctx, nil, err
	}
	if current.HostID != component.HostID || current.Type != component.Type {
		release()
		return ctx, nil, ErrComponentBusy
	}
	return claimed, release, nil
}
