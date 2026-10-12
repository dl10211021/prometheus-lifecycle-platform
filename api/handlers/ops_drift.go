package handlers

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/core/ssh"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"github.com/promlifecycle/platform/services"
)

// ===== Config Drift Detection =====

// CheckConfigDrift compares the managed version with the effective file on the
// component's deployment host. Target registrations are explicitly unsupported
// because the platform has no execution channel to their filesystem.
func (h *OpsHandler) CheckConfigDrift(c *gin.Context) {
	var components []models.Component
	if err := h.db.Where("status <> ?", "removed").Preload("Host").Preload("Configs").Find(&components).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取组件失败"})
		return
	}
	drifts := []gin.H{}
	incidentService := services.NewIncidentService(h.db)
	observedAt := time.Now()
	for i := range components {
		comp := &components[i]
		if comp.DeployType == "target" {
			sourceErr := fmt.Errorf("target 类型不支持配置漂移检测：没有可执行的主机连接")
			_ = incidentService.RecordSourceError(models.IncidentKindDrift, "component", comp.ID, sourceErr, observedAt)
			drifts = append(drifts, gin.H{
				"component": comp.Name, "component_id": comp.ID, "config_path": "", "status": "unsupported",
				"message": sourceErr.Error(), "collected_at": observedAt,
			})
			continue
		}
		active := latestActiveConfig(comp.Configs)
		if active == nil {
			continue
		}
		actual, source, err := readActualComponentConfig(comp, active)
		if err != nil {
			_ = incidentService.RecordSourceError(models.IncidentKindDrift, "component", comp.ID, err, observedAt)
			drifts = append(drifts, gin.H{
				"component": comp.Name, "component_id": comp.ID, "config_path": source, "status": "source_error",
				"message": err.Error(), "collected_at": observedAt,
			})
			continue
		}
		sourceKey := fmt.Sprintf("config-drift:%d:%d", comp.ID, active.ID)
		activeKeys := map[string]struct{}{}
		if strings.TrimSpace(active.Content) != strings.TrimSpace(actual) {
			activeKeys[sourceKey] = struct{}{}
			message := fmt.Sprintf("目标主机配置与平台记录不一致（版本 v%d）", active.Version)
			drifts = append(drifts, gin.H{
				"component": comp.Name, "component_id": comp.ID, "config_path": source, "status": "drift",
				"message": message, "collected_at": observedAt,
			})
			if _, err := incidentService.Observe(services.IncidentObservation{
				Kind: models.IncidentKindDrift, Severity: "warning", SourceKey: sourceKey,
				ResourceType: "component", ResourceID: comp.ID, ResourceName: comp.Name,
				ClusterID: &comp.ClusterID, ComponentID: &comp.ID,
				Title: comp.Name + " 配置发生漂移", Summary: message,
				Evidence: map[string]any{
					"component_id": comp.ID, "config_id": active.ID, "config_version": active.Version,
					"config_path": source, "managed_content_length": len(active.Content),
					"actual_content_length": len(actual), "collected_at": observedAt,
				},
				ObservedAt: observedAt,
			}); err != nil {
				drifts[len(drifts)-1]["incident_error"] = err.Error()
			}
		}
		if err := incidentService.ResolveMissing(models.IncidentKindDrift, "component", comp.ID, activeKeys, observedAt, "最新配置采集已确认实际内容与平台管理版本一致"); err != nil {
			drifts = append(drifts, gin.H{"component": comp.Name, "component_id": comp.ID, "status": "incident_error", "message": err.Error()})
		}
	}
	c.JSON(http.StatusOK, gin.H{"drifts": drifts, "collected_at": observedAt})
}

func latestActiveConfig(configs []models.ComponentConfig) *models.ComponentConfig {
	var latest *models.ComponentConfig
	for i := range configs {
		if configs[i].IsActive && (latest == nil || configs[i].Version > latest.Version) {
			latest = &configs[i]
		}
	}
	return latest
}

func readActualComponentConfig(comp *models.Component, managed *models.ComponentConfig) (string, string, error) {
	if comp.DeployType == "target" {
		return "", "", fmt.Errorf("target 类型不支持配置漂移检测：没有可执行的主机连接")
	}
	name := managed.Name
	if name == "" && managed.FilePath != "" {
		name = filepath.Base(managed.FilePath)
	}
	if name == "" {
		name = defaultOperationalConfigName(comp.Type)
	}
	if comp.DeployType == "docker" || comp.DeployType == "" {
		if comp.ServiceName == "" {
			return "", "", fmt.Errorf("Docker 容器名为空")
		}
		containerPath := filepath.Join("/etc", string(comp.Type), name)
		command := "docker exec " + utils.QuoteShell(comp.ServiceName) + " cat " + utils.QuoteShell(containerPath)
		output, err := runHostCommand(&comp.Host, command)
		if err != nil {
			return "", "docker:" + containerPath, fmt.Errorf("无法从实际 Docker 容器读取配置: %w", err)
		}
		return output, "docker:" + containerPath, nil
	}

	path := managed.FilePath
	if path == "" {
		path = comp.ConfigPath
	}
	if path == "" {
		return "", "", fmt.Errorf("binary 组件未记录实际配置路径")
	}
	if filepath.Ext(path) == "" {
		path = filepath.Join(path, name)
	}
	if ssh.IsLocalHost(comp.Host.IP, comp.Host.ConnectionType) {
		content, err := os.ReadFile(path)
		if err != nil {
			return "", path, fmt.Errorf("无法读取本地 binary 配置: %w", err)
		}
		return string(content), path, nil
	}
	output, err := runHostCommand(&comp.Host, "cat "+utils.QuoteShell(path))
	if err != nil {
		return "", path, fmt.Errorf("无法通过 SSH 读取远程 binary 配置: %w", err)
	}
	return output, path, nil
}

func defaultOperationalConfigName(componentType models.ComponentType) string {
	switch componentType {
	case models.ComponentPrometheus:
		return "prometheus.yml"
	case models.ComponentAlertmanager:
		return "alertmanager.yml"
	case models.ComponentGrafana:
		return "grafana.ini"
	case models.ComponentLoki:
		return "loki.yml"
	case models.ComponentPromtail:
		return "promtail.yml"
	case models.ComponentBlackbox:
		return "blackbox.yml"
	default:
		return string(componentType) + ".yml"
	}
}
