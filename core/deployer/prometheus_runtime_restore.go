package deployer

import (
	"context"
	"errors"
	"fmt"

	"github.com/promlifecycle/platform/models"
)

// RestorePrometheusImage restarts the exact historical image against data
// already restored by ChangeService. Unlike legacy generic Rollback it must
// preserve every stop/remove failure, including unknown remote termination.
// The caller owns readiness verification and durable recovery quarantine.
func (d *DockerDeployer) RestorePrometheusImage(ctx context.Context, component *models.Component) error {
	if component == nil || component.Type != models.ComponentPrometheus || component.Image == "" || component.ServiceName == "" {
		return errors.New("historical Prometheus Docker image/service identity is missing")
	}
	for _, action := range []string{"stop", "rm"} {
		if _, err := d.runHostCommand(ctx, component, "docker", action, component.ServiceName); err != nil {
			return fmt.Errorf("historical Docker %s failed: %w", action, err)
		}
	}
	if _, err := d.runContainer(ctx, component, component.Image); err != nil {
		return fmt.Errorf("historical Docker run failed: %w", err)
	}
	return nil
}
