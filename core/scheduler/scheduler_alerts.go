package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/services"
	"gorm.io/gorm"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// collectAlerts queries Prometheus APIs every 30 seconds for firing alerts.
func (s *Scheduler) collectAlerts(ctx context.Context) {
	defer s.wg.Done()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			observeLoop("alert_collection", s.fetchAlertsFromPrometheus(ctx))
		}
	}
}

func (s *Scheduler) fetchAlertsFromPrometheus(ctx context.Context) error {
	var components []models.Component
	if err := s.db.Where("type = ? AND status = ?", models.ComponentPrometheus, models.StatusRunning).Find(&components).Error; err != nil {
		return err
	}
	for i := range components {
		s.processPrometheusAlerts(ctx, &components[i])
	}
	return nil
}

var (
	alertErrMu   sync.Mutex
	alertErrLast = map[string]time.Time{}
)

// logAlertCollectError logs alert collection failures at most once per
// 5 minutes per target, so a persistently unreachable Prometheus does not
// flood the log while the failure stays visible.
func logAlertCollectError(comp *models.Component, err error) {
	if err == nil {
		return
	}
	observeLoop("alert_collection", err)
	key := fmt.Sprintf("%d|%s", comp.ID, comp.Name)
	alertErrMu.Lock()
	defer alertErrMu.Unlock()
	now := time.Now()
	if last, ok := alertErrLast[key]; ok && now.Sub(last) < 5*time.Minute {
		return
	}
	alertErrLast[key] = now
	log.Printf("[Scheduler] alert collection failed for %s (cluster %d): %v", comp.Name, comp.ClusterID, err)
}

// processPrometheusAlerts fetches alerts from a single Prometheus instance and updates AlertHistory.
func (s *Scheduler) processPrometheusAlerts(ctx context.Context, prom *models.Component) {
	var url string
	if prom.ExternalURL != "" {
		url = prom.ExternalURL
	} else if prom.InternalURL != "" {
		url = prom.InternalURL
	} else {
		url = fmt.Sprintf("http://localhost:%d", prom.Port)
	}
	url = strings.TrimSuffix(url, "/")
	apiURL := fmt.Sprintf("%s/api/v1/alerts?active=true", url)
	incidentService := services.NewIncidentService(s.db)
	recordSourceError := func(err error) {
		logAlertCollectError(prom, err)
		if recordErr := incidentService.RecordSourceError(models.IncidentKindAlert, "component", prom.ID, err, time.Now()); recordErr != nil {
			log.Printf("[Scheduler] failed to record alert source error for component %d: %v", prom.ID, recordErr)
		}
	}

	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, apiURL, nil)
	if err != nil {
		recordSourceError(err)
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		recordSourceError(err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		recordSourceError(fmt.Errorf("HTTP %d", resp.StatusCode))
		return
	}

	var result struct {
		Status string `json:"status"`
		Data   struct {
			Alerts []struct {
				Labels      map[string]string `json:"labels"`
				Annotations map[string]string `json:"annotations"`
				State       string            `json:"state"`
				ActiveAt    string            `json:"activeAt"`
				Value       string            `json:"value"`
			} `json:"alerts"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		recordSourceError(fmt.Errorf("decode Prometheus alerts: %w", err))
		return
	}
	if result.Status != "success" {
		recordSourceError(fmt.Errorf("Prometheus alerts API status %q", result.Status))
		return
	}

	observedAt := time.Now()
	currentFiring := make(map[string]struct{})
	for _, alert := range result.Data.Alerts {
		if alert.State != "firing" {
			continue
		}
		labelsJSON, _ := json.Marshal(alert.Labels)
		alertName := alert.Labels["alertname"]
		alertKey := alertName + "|" + string(labelsJSON)
		currentFiring[alertKey] = struct{}{}
		severity := alert.Labels["severity"]
		if severity == "" {
			severity = "warning"
		}
		startedAt := observedAt
		if parsed, err := time.Parse(time.RFC3339, alert.ActiveAt); err == nil {
			startedAt = parsed
		}

		var existing models.AlertHistory
		err := s.db.Where("alert_name = ? AND labels = ? AND cluster_id = ? AND status = 'firing' AND ended_at IS NULL",
			alertName, string(labelsJSON), prom.ClusterID).First(&existing).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			history := models.AlertHistory{
				ClusterID: prom.ClusterID, AlertName: alertName, Severity: severity,
				Status: "firing", Value: alert.Value, Labels: string(labelsJSON),
				Summary: alert.Annotations["summary"], StartedAt: startedAt,
			}
			if err := s.db.Create(&history).Error; err != nil {
				log.Printf("[Scheduler] failed to record alert history: %v", err)
			}
		} else if err != nil {
			log.Printf("[Scheduler] failed to query alert history: %v", err)
		}

		if _, err := incidentService.Observe(services.IncidentObservation{
			Kind: models.IncidentKindAlert, Severity: severity, SourceKey: alertKey,
			ResourceType: "component", ResourceID: prom.ID, ResourceName: alertName,
			ClusterID: &prom.ClusterID, ComponentID: &prom.ID,
			Title: alertName, Summary: alert.Annotations["summary"],
			Evidence: map[string]any{
				"labels": alert.Labels, "annotations": alert.Annotations, "value": alert.Value,
				"started_at": startedAt, "prometheus_component_id": prom.ID,
				"collected_at": observedAt, "source": apiURL,
			},
			ObservedAt: observedAt,
		}); err != nil {
			log.Printf("[Scheduler] failed to upsert alert incident: %v", err)
		}
	}

	var firing []models.AlertHistory
	s.db.Where("cluster_id = ? AND status = 'firing' AND ended_at IS NULL", prom.ClusterID).Find(&firing)
	for i := range firing {
		history := &firing[i]
		if _, still := currentFiring[history.AlertName+"|"+history.Labels]; still {
			continue
		}
		history.Status = "resolved"
		history.EndedAt = &observedAt
		s.db.Save(history)
	}
	if err := incidentService.ResolveMissing(models.IncidentKindAlert, "component", prom.ID, currentFiring, observedAt, "Prometheus 最新采集已确认告警不再 firing"); err != nil {
		log.Printf("[Scheduler] failed to resolve recovered alert incidents: %v", err)
	}
}
