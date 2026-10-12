package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/promlifecycle/platform/core/ssh"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"gorm.io/gorm"
)

func (ac *AutoConfig) FindGrafanaComponent(clusterID uint) (*models.Component, error) {
	var component models.Component
	err := ac.db.Preload("Host").
		Where("cluster_id = ? AND type = ? AND status = ?", clusterID, models.ComponentGrafana, models.StatusRunning).
		Limit(1).Find(&component).Error
	if err != nil {
		return nil, err
	}
	if component.ID == 0 {
		return nil, gorm.ErrRecordNotFound
	}
	return &component, nil
}

// GetGrafanaEndpoint returns the HTTP endpoint for the grafana instance in the given cluster.
func (ac *AutoConfig) GetGrafanaEndpoint(clusterID uint) (string, error) {
	comp, err := ac.FindGrafanaComponent(clusterID)
	if err != nil {
		return "", fmt.Errorf("no running grafana component found in cluster %d: %w", clusterID, err)
	}
	endpoint := comp.ExternalURL
	if endpoint == "" {
		endpoint = comp.InternalURL
	}
	if endpoint == "" {
		port := comp.Port
		if port == 0 {
			port = 3000
		}
		endpoint = fmt.Sprintf("http://%s:%d", comp.Host.IP, port)
	}
	return strings.TrimSuffix(endpoint, "/"), nil
}

// GrafanaDashboardDir is the watched directory installed by the binary deployer.
const GrafanaDashboardDir = "/etc/grafana/provisioning/dashboards/promlifecycle"

func GrafanaDashboardProvider() string {
	return "apiVersion: 1\nproviders:\n  - name: PromLifecycle\n    orgId: 1\n    folder: PromLifecycle\n    type: file\n    disableDeletion: false\n    updateIntervalSeconds: 30\n    allowUiUpdates: true\n    options:\n      path: " + GrafanaDashboardDir + "\n"
}

// ProvisionGrafanaDashboards reconciles only platform-owned dashboard-ID files.
// Deployment installs the provider; reconciliation never restarts Grafana or
// overwrites its built-in overview and user-managed provisioning files.
func (ac *AutoConfig) ProvisionGrafanaDashboards(clusterID uint, basePath string) error {
	return WithMonitoringLock(func() error {
		err := ac.provisionGrafanaDashboardsLocked(clusterID, basePath)
		return finishPublication(ac.db, clusterID, []interface{}{&models.Dashboard{}}, err)
	})
}

func (ac *AutoConfig) provisionGrafanaDashboardsLocked(clusterID uint, basePath string) error {
	lockPath := filepath.Join(basePath, "grafana", strconv.FormatUint(uint64(clusterID), 10))
	return WithConfigLock(clusterID, lockPath, func() error {
		component, err := ac.FindGrafanaComponent(clusterID)
		if err != nil {
			return fmt.Errorf("find managed Grafana: %w", err)
		}
		if component.DeployType != "binary" {
			return fmt.Errorf("dashboard publication requires a managed binary Grafana component")
		}
		dashboardDir := grafanaDashboardDir(component)
		var dashboards []models.Dashboard
		if err := ac.db.Where("cluster_id = ?", clusterID).Find(&dashboards).Error; err != nil {
			return err
		}
		files := make(map[string][]byte, len(dashboards))
		for i := range dashboards {
			d := &dashboards[i]
			if !d.Enabled {
				continue
			}
			var content map[string]interface{}
			if d.Content == "" {
				return fmt.Errorf("enabled dashboard %d has no content", d.ID)
			}
			if err := json.Unmarshal([]byte(d.Content), &content); err != nil || content == nil {
				return fmt.Errorf("dashboard %d must contain a JSON object", d.ID)
			}
			if d.UID == "" {
				d.UID, _ = content["uid"].(string)
				if d.UID == "" {
					d.UID = fmt.Sprintf("promlifecycle-%d", d.ID)
				}
			}
			content["uid"] = d.UID
			delete(content, "id")
			data, err := json.Marshal(content)
			if err != nil {
				return err
			}
			files[fmt.Sprintf("dashboard-%d.json", d.ID)] = data
		}
		if ssh.IsLocalHost(component.Host.IP, component.Host.ConnectionType) {
			if err := reconcileGrafanaDashboardFiles(dashboardDir, files); err != nil {
				return err
			}
		} else {
			if err := reconcileRemoteGrafanaDashboards(component, files); err != nil {
				return err
			}
		}
		legacyDir := filepath.Join(lockPath, "provisioning", "dashboards", "dashboards")
		for _, d := range dashboards {
			path := ""
			if _, enabled := files[fmt.Sprintf("dashboard-%d.json", d.ID)]; enabled {
				path = filepath.Join(dashboardDir, fmt.Sprintf("dashboard-%d.json", d.ID))
			}
			if d.ProvisionPath != "" && filepath.Dir(d.ProvisionPath) == legacyDir {
				if err := os.Remove(d.ProvisionPath); err != nil && !os.IsNotExist(err) {
					return err
				}
			}
			if err := ac.db.Model(&models.Dashboard{}).Where("id = ?", d.ID).
				Updates(map[string]interface{}{"provision_path": path, "uid": d.UID}).Error; err != nil {
				return err
			}
		}
		return ac.verifyGrafanaDashboards(component, dashboards, files)
	})
}

func ownedGrafanaDashboardFile(name string) bool {
	if !strings.HasPrefix(name, "dashboard-") || !strings.HasSuffix(name, ".json") {
		return false
	}
	id := strings.TrimSuffix(strings.TrimPrefix(name, "dashboard-"), ".json")
	value, err := strconv.ParseUint(id, 10, 64)
	return err == nil && value > 0 && strconv.FormatUint(value, 10) == id
}

func reconcileGrafanaDashboardFiles(dir string, files map[string][]byte) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	for name, data := range files {
		if err := utils.AtomicWriteFile(filepath.Join(dir, name), data, 0644); err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if _, keep := files[entry.Name()]; !keep && ownedGrafanaDashboardFile(entry.Name()) && !entry.IsDir() {
			if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func grafanaDashboardDir(component *models.Component) string {
	if component.ConfigPath == "" {
		return GrafanaDashboardDir
	}
	return filepath.Join(filepath.Dir(component.ConfigPath), "provisioning", "dashboards", "promlifecycle")
}

func reconcileRemoteGrafanaDashboards(component *models.Component, files map[string][]byte) error {
	host := component.Host
	dir := grafanaDashboardDir(component)
	client, err := ssh.NewClient(host.IP, host.Port, host.Username, host.Password, host.PrivateKey)
	if err != nil {
		return err
	}
	defer client.Close()
	if _, _, err := client.RunCommand("mkdir -p " + utils.QuoteShell(dir)); err != nil {
		return err
	}
	for name, data := range files {
		if err := client.CopyFile(bytes.NewReader(data), filepath.Join(dir, name), 0644, int64(len(data))); err != nil {
			return err
		}
	}
	output, _, err := client.RunCommand("printf '%s\\n' " + utils.QuoteShell(dir) + "/dashboard-*.json")
	if err != nil {
		return err
	}
	for _, path := range strings.Split(strings.TrimSpace(output), "\n") {
		name := filepath.Base(path)
		if _, keep := files[name]; !keep && ownedGrafanaDashboardFile(name) && filepath.Dir(path) == dir {
			if _, _, err := client.RunCommand("rm -f " + utils.QuoteShell(path)); err != nil {
				return err
			}
		}
	}
	return nil
}
