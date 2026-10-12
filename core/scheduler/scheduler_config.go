package scheduler

import (
	"context"
	"github.com/promlifecycle/platform/core/config"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/services"
	"log"
	"time"
)

// refreshVersionsLoop periodically (daily) fetches the latest official
// component versions and auto-downloads the newest package per type, so the
// platform stays current with upstream releases without manual action.
func (s *Scheduler) refreshVersionsLoop(ctx context.Context) {
	defer s.wg.Done()

	timer := time.NewTimer(2 * time.Minute) // first run shortly after startup
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			svc := services.NewVersionService(s.db)
			summary := svc.RefreshAllAndAutoDownload(ctx, s.basePath, nil)
			observeLoop("version_refresh", summary.Err())
			log.Printf("[Scheduler] %s", summary.Detail())
			for _, result := range summary.Results {
				if result.Status != "success" || result.DownloadStatus == "failed" || result.DownloadStatus == "canceled" {
					log.Printf("[Scheduler] Version refresh %s: %s", result.Type, result.Detail())
				}
			}
			timer.Reset(24 * time.Hour)
		}
	}
}

// syncConfig runs every 2 minutes to sync Prometheus scrape configs and alert rules for clusters with Prometheus.
func (s *Scheduler) syncConfig(ctx context.Context) {
	defer s.wg.Done()

	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()

	log.Printf("[Scheduler] Config sync started, running every 2 minutes")

	// Run immediately on start.
	observeLoop("config_sync", s.executeConfigSync(ctx))

	for {
		select {
		case <-ctx.Done():
			log.Printf("[Scheduler] Config sync stopped")
			return
		case <-ticker.C:
			observeLoop("config_sync", s.executeConfigSync(ctx))
		}
	}
}

// executeConfigSync performs a full config sync for all clusters with running Prometheus instances.
// Includes config validation, Prometheus config, alert rules, alertmanager config, and Grafana dashboards.
func (s *Scheduler) executeConfigSync(ctx context.Context) error {
	var promComponents []models.Component
	if err := s.db.Where("type IN ? AND status = ?", []models.ComponentType{models.ComponentPrometheus, models.ComponentGrafana}, models.StatusRunning).
		Preload("Host").
		Preload("Cluster").
		Find(&promComponents).Error; err != nil {
		log.Printf("[Scheduler] Failed to fetch prometheus components: %v", err)
		return err
	}
	if len(promComponents) == 0 {
		return nil
	}
	var firstErr error

	// Track processed cluster IDs to avoid duplicates
	processed := make(map[uint]bool)

	// Create services
	base := s.basePath
	if base == "" {
		base = "/opt/promlifecycle"
	}
	autoConfig := config.NewAutoConfig(s.db, base)

	for _, prom := range promComponents {
		clusterID := prom.ClusterID
		if processed[clusterID] {
			continue
		}
		processed[clusterID] = true

		clusterName := "unknown"
		if prom.Cluster.Name != "" {
			clusterName = prom.Cluster.Name
		}

		log.Printf("[Scheduler] Syncing config for cluster %d (%s)", clusterID, clusterName)

		// Sync scrape configs and prometheus.yml
		if err := autoConfig.SyncPrometheusConfig(clusterID, base); err != nil {
			log.Printf("[Scheduler] Failed to sync prometheus config for cluster %d: %v", clusterID, err)
			observeConfigReloadFailure()
			if firstErr == nil {
				firstErr = err
			}
		}

		// Validate and sync alertmanager config if alertmanager exists in cluster
		if err := autoConfig.ValidateAlertmanagerConfig(clusterID); err == nil {
			if err := autoConfig.SyncAlertmanagerConfig(clusterID, base); err != nil {
				log.Printf("[Scheduler] Alertmanager config sync for cluster %d: %v", clusterID, err)
				observeConfigReloadFailure()
				if firstErr == nil {
					firstErr = err
				}
			}
		} else {
			log.Printf("[Scheduler] Alertmanager config validation failed for cluster %d: %v", clusterID, err)
			if firstErr == nil {
				firstErr = err
			}
		}

		// Provision Grafana dashboards for clusters with Grafana
		if err := s.provisionGrafanaDashboards(clusterID); err != nil {
			// Non-fatal: may not have Grafana in cluster
			log.Printf("[Scheduler] Grafana dashboard provisioning for cluster %d: %v", clusterID, err)
		}
	}
	return firstErr
}
