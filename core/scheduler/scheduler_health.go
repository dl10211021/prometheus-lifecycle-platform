package scheduler

import (
	"context"
	"fmt"
	"github.com/promlifecycle/platform/core/deployer"
	"github.com/promlifecycle/platform/core/ssh"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"os/exec"
	"strings"
	"time"
)

// healthPoll checks every 60 seconds that operational and failed components
// reflect the state observed on their actual deployment host.
func (s *Scheduler) healthPoll(ctx context.Context) {
	defer s.wg.Done()
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			observeLoop("health", s.checkComponentHealth(ctx))
		}
	}
}

// checkComponentHealth emits a notification only after a persisted transition.
func (s *Scheduler) checkComponentHealth(ctx context.Context) error {
	var components []models.Component
	if err := s.db.Preload("Host").
		Where("status IN ?", []models.ComponentStatus{models.StatusRunning, models.StatusDegraded, models.StatusError}).
		Find(&components).Error; err != nil {
		return err
	}
	for i := range components {
		comp := &components[i]
		if comp.DeployType != "target" && comp.ServiceName == "" {
			continue
		}
		var alive bool
		switch comp.DeployType {
		case "binary":
			alive = s.isServiceRunning(ctx, comp)
		case "windows":
			alive = deployer.NewWindowsDeployer(s.db, s.basePath).HealthCheck(ctx, comp) == nil
		case "target":
			alive = true
		default:
			alive = s.isContainerRunning(ctx, comp)
		}

		oldStatus := comp.Status
		newStatus := oldStatus
		updates := map[string]interface{}{}
		if !alive && oldStatus != models.StatusError {
			newStatus = models.StatusError
			updates["status"] = newStatus
			updates["error"] = "not running (detected by health poll)"
		} else if alive && oldStatus == models.StatusError {
			newStatus = models.StatusRunning
			updates["status"] = newStatus
			updates["error"] = ""
		}
		if newStatus != oldStatus {
			if err := s.persistObservedHealth(comp, newStatus, updates); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Scheduler) persistObservedHealth(comp *models.Component, newStatus models.ComponentStatus, updates map[string]interface{}) error {
	result := s.db.Model(&models.Component{}).Where("id = ? AND status = ? AND updated_at = ?", comp.ID, comp.Status, comp.UpdatedAt).Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return nil
	}
	s.notifier.SendHealthTransition(comp, comp.Status, newStatus)
	return nil
}

func (s *Scheduler) isServiceRunning(ctx context.Context, comp *models.Component) bool {
	serviceName := comp.ServiceName
	if ssh.IsLocalHost(comp.Host.IP, comp.Host.ConnectionType) {
		out, err := exec.CommandContext(ctx, "systemctl", "is-active", serviceName).Output()
		return err == nil && strings.TrimSpace(string(out)) == "active"
	}
	client, err := ssh.NewClient(comp.Host.IP, comp.Host.Port, comp.Host.Username,
		comp.Host.Password, comp.Host.PrivateKey)
	if err != nil {
		return false
	}
	defer client.Close()
	_, _, err = client.RunCommand("systemctl is-active --quiet " + utils.QuoteShell(serviceName))
	return err == nil
}

func (s *Scheduler) isContainerRunning(ctx context.Context, comp *models.Component) bool {
	output, err := s.runDockerCommand(ctx, comp, "ps",
		"--filter", fmt.Sprintf("name=^/%s$", comp.ServiceName),
		"--filter", "status=running", "--format", "{{.Names}}")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if strings.TrimSpace(line) == comp.ServiceName {
			return true
		}
	}
	return false
}

func (s *Scheduler) runDockerCommand(ctx context.Context, comp *models.Component, args ...string) (string, error) {
	if ssh.IsLocalHost(comp.Host.IP, comp.Host.ConnectionType) {
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			return string(out), fmt.Errorf("docker %s: %w", strings.Join(args, " "), err)
		}
		return string(out), nil
	}
	client, err := ssh.NewClient(comp.Host.IP, comp.Host.Port, comp.Host.Username,
		comp.Host.Password, comp.Host.PrivateKey)
	if err != nil {
		return "", err
	}
	defer client.Close()
	quoted := make([]string, 0, len(args)+1)
	quoted = append(quoted, "docker")
	for _, arg := range args {
		quoted = append(quoted, utils.QuoteShell(arg))
	}
	stdout, stderr, err := client.RunCommand(strings.Join(quoted, " "))
	if err != nil {
		return stdout, fmt.Errorf("remote docker: %s: %w", strings.TrimSpace(stderr), err)
	}
	return stdout, nil
}
