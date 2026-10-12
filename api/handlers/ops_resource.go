package handlers

import (
	"fmt"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/core/ssh"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
)

// ===== Resource Monitoring =====

// GetResourceUsage gets resource usage for a component
func (h *OpsHandler) GetResourceUsage(c *gin.Context) {
	id, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}
	var component models.Component
	if h.db.Preload("Host").First(&component, id).Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "组件不存在"})
		return
	}

	// For binary components, query the real process on its host (local or
	// remote via SSH) instead of returning stale/zero scheduler values.
	if component.DeployType == "binary" && component.Host.ID > 0 {
		cpu, memMB, diskMB, diskPct := h.probeBinaryResource(&component)
		if cpu >= 0 {
			c.JSON(http.StatusOK, gin.H{
				"component":  component.Name,
				"name":       component.Name,
				"type":       component.Type,
				"cpu_usage":  cpu,
				"memory_mb":  memMB,
				"disk_mb":    diskMB,
				"disk_usage": diskPct,
				"timestamp":  time.Now(),
				"status":     component.Status,
				"source":     "host",
			})
			return
		}
	}

	// Fallback: latest stored resource usage
	var usage models.ResourceUsage
	err := h.db.Where("component_id = ?", id).Order("timestamp DESC").First(&usage).Error
	if err != nil {
		// No data yet, return zeros
		usage = models.ResourceUsage{ComponentID: uint(id), Timestamp: time.Now()}
	}

	diskUsage := usage.DiskUsage

	c.JSON(http.StatusOK, gin.H{
		"component":  component.Name,
		"name":       component.Name,
		"source":     "collector",
		"type":       component.Type,
		"cpu_usage":  usage.CPUUsage,
		"memory_mb":  usage.MemoryMB,
		"disk_mb":    usage.DiskMB,
		"disk_usage": diskUsage,
		"timestamp":  usage.Timestamp,
		"status":     component.Status,
	})
}

// probeBinaryResource returns (cpu%, memoryMB, diskMB). cpu<0 means probe failed.
func (h *OpsHandler) probeBinaryResource(component *models.Component) (float64, float64, float64, float64) {
	if ssh.IsLocalHost(component.Host.IP, component.Host.ConnectionType) {
		return probeLocalBinaryResource(component)
	}
	unit := strings.TrimSpace(component.ServiceName)
	typeName := string(component.Type)
	dataPath := component.DataPath
	if dataPath == "" {
		dataPath = fmt.Sprintf("/var/lib/%s", typeName)
	}
	cmd := fmt.Sprintf(
		`pid=$(systemctl show -p MainPID --value %s 2>/dev/null); if [ -z "$pid" ] || [ "$pid" = "0" ]; then pid=$(pgrep -f %s | head -1); fi; if [ -n "$pid" ]; then ps -o %%cpu=,rss= -p "$pid" 2>/dev/null; fi; du -sm %s 2>/dev/null | cut -f1; df -kP %s 2>/dev/null | tail -1 | awk '{print $5}'`,
		utils.QuoteShell(unit), utils.QuoteShell(typeName), utils.QuoteShell(dataPath), utils.QuoteShell(dataPath))
	out, err := runHostCommand(&component.Host, cmd)
	if err != nil && strings.TrimSpace(out) == "" {
		return -1, 0, 0, 0
	}
	return parseResourceProbe(out)
}

func probeLocalBinaryResource(component *models.Component) (float64, float64, float64, float64) {
	unit := strings.TrimSpace(component.ServiceName)
	typeName := string(component.Type)
	dataPath := component.DataPath
	if dataPath == "" {
		dataPath = fmt.Sprintf("/var/lib/%s", typeName)
	}
	cmd := fmt.Sprintf(
		`pid=$(systemctl show -p MainPID --value %s 2>/dev/null); if [ -z "$pid" ] || [ "$pid" = "0" ]; then pid=$(pgrep -f %s | head -1); fi; if [ -n "$pid" ]; then ps -o %%cpu=,rss= -p "$pid" 2>/dev/null; fi; du -sm %s 2>/dev/null | cut -f1; df -kP %s 2>/dev/null | tail -1 | awk '{print $5}'`,
		utils.QuoteShell(unit), utils.QuoteShell(typeName), utils.QuoteShell(dataPath), utils.QuoteShell(dataPath))
	out, err := exec.Command("sh", "-c", cmd).CombinedOutput()
	if err != nil && strings.TrimSpace(string(out)) == "" {
		return -1, 0, 0, 0
	}
	return parseResourceProbe(string(out))
}

func parseResourceProbe(out string) (float64, float64, float64, float64) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var cpu, memMB, diskMB, diskPct float64
	gotCPU := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasSuffix(line, "%") {
			if v, err := strconv.ParseFloat(strings.TrimSuffix(line, "%"), 64); err == nil {
				diskPct = v
			}
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 2 && !gotCPU {
			cpuVal, err1 := strconv.ParseFloat(fields[0], 64)
			rssKB, err2 := strconv.ParseFloat(fields[1], 64)
			if err1 == nil && err2 == nil {
				cpu = cpuVal
				memMB = rssKB / 1024.0
				gotCPU = true
			}
			continue
		}
		if len(fields) == 1 && gotCPU {
			if v, err := strconv.ParseFloat(fields[0], 64); err == nil {
				diskMB = v
			}
		}
	}
	if !gotCPU {
		return -1, 0, 0, 0
	}
	return cpu, memMB, diskMB, diskPct
}

// GetClusterResourceUsage gets resource usage summary for a cluster
func (h *OpsHandler) GetClusterResourceUsage(c *gin.Context) {
	clusterID, ok := utils.ParseUintParam(c, "id")
	if !ok {
		return
	}
	var components []models.Component
	h.db.Where("cluster_id = ? AND status <> ?", clusterID, "removed").Find(&components)

	total := gin.H{"cpu_usage": 0.0, "memory_mb": 0.0, "disk_mb": 0.0}
	componentsList := []gin.H{}

	// Load the latest usage row per component in a single query.
	componentIDs := make([]uint, 0, len(components))
	for _, comp := range components {
		componentIDs = append(componentIDs, comp.ID)
	}
	latestByComponent := map[uint]models.ResourceUsage{}
	if len(componentIDs) > 0 {
		var usages []models.ResourceUsage
		h.db.Where("component_id IN ?", componentIDs).
			Order("timestamp DESC").
			Find(&usages)
		seen := map[uint]bool{}
		for _, u := range usages {
			if !seen[u.ComponentID] {
				latestByComponent[u.ComponentID] = u
				seen[u.ComponentID] = true
			}
		}
	}

	for _, comp := range components {
		usage := latestByComponent[comp.ID]

		compData := gin.H{
			"name":       comp.Name,
			"type":       comp.Type,
			"cpu_usage":  usage.CPUUsage,
			"memory_mb":  usage.MemoryMB,
			"disk_mb":    usage.DiskMB,
			"disk_usage": usage.DiskUsage,
			"status":     comp.Status,
		}
		componentsList = append(componentsList, compData)
		total["cpu_usage"] = total["cpu_usage"].(float64) + usage.CPUUsage
		total["memory_mb"] = total["memory_mb"].(float64) + usage.MemoryMB
		total["disk_mb"] = total["disk_mb"].(float64) + usage.DiskMB
	}

	c.JSON(http.StatusOK, gin.H{"total": total, "components": componentsList})
}

// ===== Disk Alerts =====

// CheckDiskUsage evaluates only the newest sample for each component.
func (h *OpsHandler) CheckDiskUsage(c *gin.Context) {
	var components []models.Component
	h.db.Where("status <> ?", "removed").Find(&components)
	var usages []models.ResourceUsage
	h.db.Table("resource_usages AS ru").Select("ru.*").
		Joins("JOIN (SELECT component_id, MAX(timestamp) AS timestamp FROM resource_usages GROUP BY component_id) latest ON latest.component_id = ru.component_id AND latest.timestamp = ru.timestamp").
		Joins("JOIN components ON components.id = ru.component_id AND components.status <> ?", "removed").
		Scan(&usages)
	latest := make(map[uint]models.ResourceUsage, len(components))
	for i := range usages {
		if _, exists := latest[usages[i].ComponentID]; !exists {
			latest[usages[i].ComponentID] = usages[i]
		}
	}

	results := make([]gin.H, 0, len(components))
	alerts := []gin.H{}
	for i := range components {
		comp := &components[i]
		usage := latest[comp.ID]
		results = append(results, gin.H{
			"component": comp.Name, "type": comp.Type, "data_path": comp.DataPath,
			"disk_mb": usage.DiskMB, "disk_usage": usage.DiskUsage, "timestamp": usage.Timestamp,
		})
		if usage.DiskUsage > 80 {
			severity := "warning"
			if usage.DiskUsage > 90 {
				severity = "critical"
			}
			alerts = append(alerts, gin.H{
				"component": comp.Name, "severity": severity,
				"message": fmt.Sprintf("磁盘使用率 %.1f%% 超过阈值", usage.DiskUsage),
			})
		}
	}
	c.JSON(http.StatusOK, gin.H{"components": results, "alerts": alerts})
}

// ===== Enhanced Stats =====

// GetEnhancedStats returns enhanced platform statistics
func (h *OpsHandler) GetEnhancedStats(c *gin.Context) {
	var stats models.PlatformStats

	h.db.Model(&models.Host{}).Count(&stats.TotalHosts)
	h.db.Model(&models.Host{}).Where("status = ?", "online").Count(&stats.OnlineHosts)
	h.db.Model(&models.Host{}).Where("has_docker = ?", true).Count(&stats.DockerHosts)
	h.db.Model(&models.Component{}).Where("status <> ?", "removed").Count(&stats.TotalComponents)
	h.db.Model(&models.Component{}).Where("status = ?", "running").Count(&stats.RunningComponents)
	h.db.Model(&models.AlertRule{}).Count(&stats.TotalAlertRules)
	h.db.Model(&models.AlertHistory{}).Where("status = ?", "firing").Count(&stats.ActiveAlerts)
	h.db.Model(&models.Operation{}).Count(&stats.TotalOperations)

	// Count threshold breaches from the newest sample per component only.
	var diskAlerts int64
	h.db.Table("resource_usages AS ru").
		Joins("JOIN (SELECT component_id, MAX(timestamp) AS timestamp FROM resource_usages GROUP BY component_id) latest ON latest.component_id = ru.component_id AND latest.timestamp = ru.timestamp").
		Joins("JOIN components ON components.id = ru.component_id AND components.status <> ?", "removed").
		Where("ru.disk_usage > ?", 80).Distinct("ru.component_id").Count(&diskAlerts)

	var maintWindows int64
	h.db.Model(&models.MaintenanceWindow{}).Where("end_at > ?", time.Now().UTC()).Count(&maintWindows)

	stats.PlatformVersion = "1.1"
	stats.Uptime = "running"

	c.JSON(http.StatusOK, gin.H{
		"stats":               stats,
		"disk_alerts":         diskAlerts,
		"maintenance_windows": maintWindows,
	})
}
