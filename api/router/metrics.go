package router

import (
	"fmt"
	"net/http"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/core/scheduler"
	"github.com/promlifecycle/platform/models"
	"gorm.io/gorm"
)

// MetricsHandler exposes the platform's own health counters in Prometheus text
// exposition format so the platform can be monitored by the very Prometheus
// instances it manages. Zero dependencies (no client library) to keep the
// offline single-binary deployment model intact. Exposed without auth: the
// payload contains counts only, no credentials or topology data.
func MetricsHandler(db *gorm.DB) gin.HandlerFunc {
	started := time.Now()
	type dbGauges struct {
		hosts, components, rules, firing, operations, audits, packages int64
		windows, componentDown                                         int64
		backupLastSuccess                                              int64
	}
	var cache struct {
		sync.Mutex
		at     time.Time
		gauges dbGauges
	}
	refresh := func(now time.Time) dbGauges {
		cache.Lock()
		defer cache.Unlock()
		if !cache.at.IsZero() && now.Sub(cache.at) < 15*time.Second {
			return cache.gauges
		}
		var gauges dbGauges
		db.Model(&models.Host{}).Count(&gauges.hosts)
		db.Model(&models.Component{}).Count(&gauges.components)
		db.Model(&models.AlertRule{}).Count(&gauges.rules)
		db.Model(&models.AlertHistory{}).Where("status = ?", "firing").Count(&gauges.firing)
		db.Model(&models.OperationLog{}).Count(&gauges.audits)
		db.Model(&models.Operation{}).Count(&gauges.operations)
		db.Model(&models.Package{}).Count(&gauges.packages)
		db.Model(&models.Component{}).
			Where("status IN ?", []models.ComponentStatus{models.StatusError, models.StatusDegraded}).
			Count(&gauges.componentDown)
		db.Table("maintenance_silences").
			Where("status = ? AND delete_requested = ?", "active", false).
			Distinct("maintenance_window_id").Count(&gauges.windows)
		var latest models.Backup
		if db.Order("created_at DESC").First(&latest).Error == nil {
			gauges.backupLastSuccess = latest.CreatedAt.Unix()
		}
		cache.gauges = gauges
		cache.at = now
		return gauges
	}

	return func(c *gin.Context) {
		now := time.Now()
		gauges := refresh(now)
		health := scheduler.SchedulerMetricsSnapshot()
		backupLastSuccess := gauges.backupLastSuccess
		if health.BackupLastSuccess > backupLastSuccess {
			backupLastSuccess = health.BackupLastSuccess
		}
		var memory runtime.MemStats
		runtime.ReadMemStats(&memory)

		var body strings.Builder
		fmt.Fprintf(&body, `# HELP go_goroutines Number of goroutines that currently exist.
# TYPE go_goroutines gauge
go_goroutines %d
# HELP go_memstats_alloc_bytes Number of bytes allocated and still in use.
# TYPE go_memstats_alloc_bytes gauge
go_memstats_alloc_bytes %d
# HELP process_start_time_seconds Start time of the process since unix epoch in seconds.
# TYPE process_start_time_seconds gauge
process_start_time_seconds %d
# HELP promlifecycle_uptime_seconds Platform uptime.
# TYPE promlifecycle_uptime_seconds gauge
promlifecycle_uptime_seconds %.0f
# HELP promlifecycle_hosts Number of managed hosts.
# TYPE promlifecycle_hosts gauge
promlifecycle_hosts %d
# HELP promlifecycle_components Number of managed components.
# TYPE promlifecycle_components gauge
promlifecycle_components %d
# HELP promlifecycle_component_down Components currently error or degraded.
# TYPE promlifecycle_component_down gauge
promlifecycle_component_down %d
# HELP promlifecycle_alert_rules Number of alert rules.
# TYPE promlifecycle_alert_rules gauge
promlifecycle_alert_rules %d
# HELP promlifecycle_alerts_firing Number of firing alert history entries.
# TYPE promlifecycle_alerts_firing gauge
promlifecycle_alerts_firing %d
# HELP promlifecycle_operations_total Total operation records.
# TYPE promlifecycle_operations_total gauge
promlifecycle_operations_total %d
# HELP promlifecycle_audit_logs_total Total audit log entries.
# TYPE promlifecycle_audit_logs_total gauge
promlifecycle_audit_logs_total %d
# HELP promlifecycle_packages_total Total stored packages.
# TYPE promlifecycle_packages_total gauge
promlifecycle_packages_total %d
# HELP promlifecycle_maintenance_windows_active Windows with an observed active Alertmanager silence.
# TYPE promlifecycle_maintenance_windows_active gauge
promlifecycle_maintenance_windows_active %d
# HELP promlifecycle_backup_last_success_timestamp_seconds Most recent successful scheduled backup.
# TYPE promlifecycle_backup_last_success_timestamp_seconds gauge
promlifecycle_backup_last_success_timestamp_seconds %d
# HELP promlifecycle_backup_age_seconds Age of the most recent successful scheduled backup.
# TYPE promlifecycle_backup_age_seconds gauge
promlifecycle_backup_age_seconds %.0f
# HELP promlifecycle_config_reload_failures_total Config synchronization/reload failures.
# TYPE promlifecycle_config_reload_failures_total counter
promlifecycle_config_reload_failures_total %d
`, runtime.NumGoroutine(), memory.Alloc, started.Unix(), now.Sub(started).Seconds(),
			gauges.hosts, gauges.components, gauges.componentDown, gauges.rules,
			gauges.firing, gauges.operations, gauges.audits, gauges.packages,
			gauges.windows, backupLastSuccess, backupAge(now, backupLastSuccess),
			health.ConfigReloadFailures)

		body.WriteString("# HELP promlifecycle_scheduler_loop_last_success_timestamp_seconds Last successful scheduler iteration.\n")
		body.WriteString("# TYPE promlifecycle_scheduler_loop_last_success_timestamp_seconds gauge\n")
		body.WriteString("# HELP promlifecycle_scheduler_loop_failures_total Failed scheduler iterations.\n")
		body.WriteString("# TYPE promlifecycle_scheduler_loop_failures_total counter\n")
		names := []string{"alert_collection", "config_sync", "health", "maintenance", "platform_backup", "resource_usage", "scheduled_backup", "version_refresh"}
		for name := range health.Loops {
			found := false
			for _, known := range names {
				if known == name {
					found = true
					break
				}
			}
			if !found {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		for _, name := range names {
			metric := health.Loops[name]
			fmt.Fprintf(&body, "promlifecycle_scheduler_loop_last_success_timestamp_seconds{loop=%q} %d\n", name, metric.LastSuccessUnix)
			fmt.Fprintf(&body, "promlifecycle_scheduler_loop_failures_total{loop=%q} %d\n", name, metric.Failures)
		}

		c.Header("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		c.String(http.StatusOK, body.String())
	}
}

func backupAge(now time.Time, lastSuccess int64) float64 {
	if lastSuccess <= 0 {
		return 0
	}
	age := now.Sub(time.Unix(lastSuccess, 0)).Seconds()
	if age < 0 {
		return 0
	}
	return age
}
