package scheduler

import (
	"context"
	"fmt"
	"github.com/promlifecycle/platform/core/ssh"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"github.com/promlifecycle/platform/services"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// collectResourceUsage polls real resource usage for running components every 60s
// and writes it into the resource_usages table (docker stats / systemd memory).
func (s *Scheduler) collectResourceUsage(ctx context.Context) {
	defer s.wg.Done()

	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	log.Printf("[Scheduler] Resource usage collection started, running every 60 seconds")

	for {
		select {
		case <-ctx.Done():
			log.Printf("[Scheduler] Resource usage collection stopped")
			return
		case <-ticker.C:
			observeLoop("resource_usage", s.collectUsageOnce(ctx))
		}
	}
}

func (s *Scheduler) collectUsageOnce(ctx context.Context) error {
	var components []models.Component
	if err := s.db.Where("status = ?", models.StatusRunning).Preload("Host").Find(&components).Error; err != nil {
		log.Printf("[Scheduler] Failed to fetch running components: %v", err)
		return err
	}
	now := time.Now()
	if err := s.pruneResourceUsage(now); err != nil {
		return err
	}
	incidentService := services.NewIncidentService(s.db)
	for i := range components {
		comp := &components[i]
		cpu, memMB, diskMB, diskPct := s.usageFor(ctx, comp)
		usage := models.ResourceUsage{
			ComponentID: comp.ID, Timestamp: now, CPUUsage: cpu,
			MemoryMB: memMB, DiskMB: diskMB, DiskUsage: diskPct,
		}
		if err := s.db.Create(&usage).Error; err != nil {
			log.Printf("[Scheduler] Failed to record usage for component %d: %v", comp.ID, err)
			return err
		}

		sourceKey := fmt.Sprintf("disk-capacity:%d", comp.ID)
		active := map[string]struct{}{}
		if diskPct >= 80 {
			active[sourceKey] = struct{}{}
			severity := "warning"
			if diskPct >= 90 {
				severity = "critical"
			}
			if _, err := incidentService.Observe(services.IncidentObservation{
				Kind: models.IncidentKindDisk, Severity: severity, SourceKey: sourceKey,
				ResourceType: "component", ResourceID: comp.ID, ResourceName: comp.Name,
				ClusterID: &comp.ClusterID, ComponentID: &comp.ID,
				Title:   fmt.Sprintf("%s 磁盘使用率 %.1f%%", comp.Name, diskPct),
				Summary: "组件数据盘使用率达到事件阈值（warning 80%，critical 90%）",
				Evidence: map[string]any{
					"disk_usage_percent": diskPct, "disk_used_mb": diskMB,
					"component_id": comp.ID, "data_path": comp.DataPath, "collected_at": now,
				},
				ObservedAt: now,
			}); err != nil {
				log.Printf("[Scheduler] failed to upsert disk incident for component %d: %v", comp.ID, err)
			}
		}
		if diskPct > 0 {
			if err := incidentService.ResolveMissing(models.IncidentKindDisk, "component", comp.ID, active, now, "最新磁盘采集已确认使用率低于 80% 阈值"); err != nil {
				log.Printf("[Scheduler] failed to resolve disk incident for component %d: %v", comp.ID, err)
			}
		} else if comp.DataPath != "" || comp.ServiceName != "" {
			_ = incidentService.RecordSourceError(models.IncidentKindDisk, "component", comp.ID, fmt.Errorf("未能采集磁盘使用率"), now)
		}
	}
	return nil
}

func (s *Scheduler) pruneResourceUsage(now time.Time) error {
	today := now.Format("2006-01-02")
	if s.lastResourcePrune == today {
		return nil
	}
	days := 30
	if value := strings.TrimSpace(os.Getenv("PROMLIFECYCLE_RESOURCE_RETENTION_DAYS")); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil {
			days = parsed
		}
	}
	if days > 0 {
		if err := s.db.Where("timestamp < ?", now.AddDate(0, 0, -days)).Delete(&models.ResourceUsage{}).Error; err != nil {
			log.Printf("[Scheduler] Failed to prune resource usage: %v", err)
			return err
		}
	}
	s.lastResourcePrune = today
	return nil
}

func (s *Scheduler) usageFor(ctx context.Context, comp *models.Component) (cpu, memMB, diskMB, diskPct float64) {
	if comp.DeployType == "binary" {
		binName := comp.ServiceName
		if binName == "" {
			binName = comp.Name
		}
		if ssh.IsLocalHost(comp.Host.IP, comp.Host.ConnectionType) {
			return s.usageForBinaryLocal(ctx, comp.Type, binName, comp.DataPath)
		}
		return s.usageForBinaryRemote(ctx, comp, binName, comp.DataPath)
	}
	if comp.DeployType == "target" || comp.ServiceName == "" {
		return 0, 0, 0, 0
	}
	out, err := s.runDockerCommand(ctx, comp, "stats", "--no-stream",
		"--format", "{{.CPUPerc}}|{{.MemUsage}}", comp.ServiceName)
	if err == nil {
		fields := strings.Split(strings.TrimSpace(out), "|")
		if len(fields) == 2 {
			cpu, _ = strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(fields[0]), "%"), 64)
			memText := strings.TrimSpace(fields[1])
			if idx := strings.Index(memText, "/"); idx >= 0 {
				memText = strings.TrimSpace(memText[:idx])
			}
			memMB, _, _ = parseMem(memText)
		}
	}

	// Resolve the source actually mounted as the component data volume. This
	// avoids reporting the platform server's filesystem for remote Docker.
	format := fmt.Sprintf(`{{range .Mounts}}{{if eq .Destination %q}}{{.Source}}{{end}}{{end}}`,
		containerDataPathForUsage(comp.Type))
	source, inspectErr := s.runDockerCommand(ctx, comp, "inspect", "--format", format, comp.ServiceName)
	source = strings.TrimSpace(source)
	if inspectErr != nil || source == "" {
		source = comp.DataPath
	}
	if source != "" {
		diskMB, diskPct = s.diskUsageOnHost(ctx, comp, source)
	}
	return cpu, memMB, diskMB, diskPct
}

func containerDataPathForUsage(componentType models.ComponentType) string {
	return "/var/lib/" + string(componentType)
}

func (s *Scheduler) diskUsageOnHost(ctx context.Context, comp *models.Component, path string) (diskMB, diskPct float64) {
	command := "du -sm " + utils.QuoteShell(path) + " 2>/dev/null | cut -f1; df -kP " +
		utils.QuoteShell(path) + " 2>/dev/null | tail -1"
	var output string
	if ssh.IsLocalHost(comp.Host.IP, comp.Host.ConnectionType) {
		out, _ := exec.CommandContext(ctx, "sh", "-c", command).CombinedOutput()
		output = string(out)
	} else {
		client, err := ssh.NewClient(comp.Host.IP, comp.Host.Port, comp.Host.Username,
			comp.Host.Password, comp.Host.PrivateKey)
		if err != nil {
			return 0, 0
		}
		defer client.Close()
		output, _, _ = client.RunCommand(command)
	}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 1 {
			if value, err := strconv.ParseFloat(fields[0], 64); err == nil {
				diskMB = value
			}
			continue
		}
		if len(fields) >= 5 && strings.HasSuffix(fields[4], "%") {
			diskPct, _ = strconv.ParseFloat(strings.TrimSuffix(fields[4], "%"), 64)
		}
	}
	return diskMB, diskPct
}

// processNameFor maps a component type to its executable name (ps -C matches
// the process comm, not the systemd unit name).
func processNameFor(compType models.ComponentType) string {
	switch compType {
	case models.ComponentPrometheus:
		return "prometheus"
	case models.ComponentGrafana:
		return "grafana"
	case models.ComponentAlertmanager:
		return "alertmanager"
	case models.ComponentLoki:
		return "loki"
	case models.ComponentPromtail:
		return "promtail"
	case models.ComponentNodeExporter:
		return "node_exporter"
	case models.ComponentPushgateway:
		return "pushgateway"
	case models.ComponentBlackbox:
		return "blackbox_exporter"
	default:
		return string(compType)
	}
}

// usageForBinaryLocal reads systemd memory + ps CPU + data dir disk usage for
// a local binary service.
func (s *Scheduler) usageForBinaryLocal(ctx context.Context, compType models.ComponentType, binName, dataPath string) (cpu, memMB, diskMB, diskPct float64) {
	if out, err := exec.CommandContext(ctx, "systemctl", "show", binName, "-p", "MemoryCurrent").Output(); err == nil {
		fields := strings.Split(strings.TrimSpace(string(out)), "=")
		if len(fields) == 2 {
			if v, err := strconv.ParseFloat(fields[1], 64); err == nil && v > 0 {
				memMB = v / 1024 / 1024
			}
		}
	}
	if out, err := exec.CommandContext(ctx, "ps", "-C", processNameFor(compType), "-o", "pcpu=").Output(); err == nil {
		if v, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64); err == nil {
			cpu = v
		}
	}
	if dataPath != "" {
		diskMB, diskPct = diskUsageOf(ctx, exec.CommandContext(ctx, "df", "-kP", dataPath))
	}
	return cpu, memMB, diskMB, diskPct
}

// diskUsageOf parses `df -kP <path>` output into used MB and used percent.
func diskUsageOf(ctx context.Context, cmd *exec.Cmd) (diskMB, diskPct float64) {
	out, err := cmd.Output()
	if err != nil {
		return 0, 0
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return 0, 0
	}
	fields := strings.Fields(lines[1])
	// 1K-blocks Used Available Use% Mounted on
	if len(fields) >= 5 {
		if used, err := strconv.ParseFloat(fields[2], 64); err == nil {
			diskMB = used / 1024
		}
		if pct := strings.TrimSuffix(fields[4], "%"); pct != "" {
			if v, err := strconv.ParseFloat(pct, 64); err == nil {
				diskPct = v
			}
		}
	}
	return diskMB, diskPct
}

// usageForBinaryRemote reads systemd memory + ps CPU + data dir disk usage
// for a remote binary service over SSH.
func (s *Scheduler) usageForBinaryRemote(ctx context.Context, comp *models.Component, binName, dataPath string) (cpu, memMB, diskMB, diskPct float64) {
	procName := processNameFor(comp.Type)
	client, err := ssh.NewClient(comp.Host.IP, comp.Host.Port, comp.Host.Username,
		comp.Host.Password, comp.Host.PrivateKey)
	if err != nil {
		return 0, 0, 0, 0
	}
	defer client.Close()

	if out, _, err := client.RunCommand("systemctl show " + utils.QuoteShell(binName) + " -p MemoryCurrent"); err == nil {
		fields := strings.Split(strings.TrimSpace(out), "=")
		if len(fields) == 2 {
			if v, err := strconv.ParseFloat(fields[1], 64); err == nil && v > 0 {
				memMB = v / 1024 / 1024
			}
		}
	}
	if out, _, err := client.RunCommand("ps -C " + procName + " -o pcpu= --no-headers"); err == nil {
		if v, err := strconv.ParseFloat(strings.TrimSpace(out), 64); err == nil {
			cpu = v
		}
	}
	if dataPath != "" {
		if out, _, err := client.RunCommand("df -kP " + utils.QuoteShell(dataPath)); err == nil {
			lines := strings.Split(strings.TrimSpace(out), "\n")
			if len(lines) >= 2 {
				fields := strings.Fields(lines[1])
				if len(fields) >= 5 {
					if used, err := strconv.ParseFloat(fields[2], 64); err == nil {
						diskMB = used / 1024
					}
					if pct := strings.TrimSuffix(fields[4], "%"); pct != "" {
						if v, err := strconv.ParseFloat(pct, 64); err == nil {
							diskPct = v
						}
					}
				}
			}
		}
	}
	return cpu, memMB, diskMB, diskPct
}

// parseMem converts docker memory strings (MiB/GiB/KiB) to MB.
func parseMem(s string) (float64, string, bool) {
	s = strings.TrimSpace(s)
	for _, suffix := range []string{"MiB", "GiB", "KiB", "MB", "GB", "KB"} {
		if strings.HasSuffix(s, suffix) {
			v, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(s, suffix)), 64)
			if err != nil {
				return 0, suffix, false
			}
			switch suffix {
			case "GiB", "GB":
				return v * 1024, suffix, true
			case "KiB", "KB":
				return v / 1024, suffix, true
			default:
				return v, suffix, true
			}
		}
	}
	return 0, "", false
}
