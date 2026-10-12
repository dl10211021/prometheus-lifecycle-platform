package deployer

import (
	"fmt"
	"github.com/promlifecycle/platform/models"
)

// ValidatePrometheusParameterVersion checks the same effective argv used to
// generate the unit. An upgrade keeps that unit, so inspect the current argv
// against targetVersion; never pretend the target's defaults replaced it.
func ValidatePrometheusParameterVersion(component *models.Component, targetVersion string) error {
	if component == nil {
		return fmt.Errorf("%w：缺少组件", models.ErrPrometheusParameterVersion)
	}
	if component.Type != models.ComponentPrometheus || component.DeployType != "binary" {
		return nil
	}
	spec, err := binarySpecForComponent(component)
	if err != nil {
		return fmt.Errorf("%w：%v", models.ErrPrometheusParameterVersion, err)
	}
	return models.ValidatePrometheusArgumentVersion(targetVersion, effectiveArgs(spec, component))
}
