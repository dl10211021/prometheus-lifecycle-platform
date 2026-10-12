package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/services"
	"gorm.io/gorm"
)

// runMaintenanceWatcher reconciles every maintenance window against every
// running Alertmanager instance. Per-instance rows make global and HA windows
// safe: a failure on one instance never discards another instance's silence.
func (s *Scheduler) runMaintenanceWatcher(ctx context.Context) {
	defer s.wg.Done()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	log.Printf("[Scheduler] Maintenance window watcher started, running every 30 seconds")
	observeLoop("maintenance", s.syncMaintenanceWindows())
	for {
		select {
		case <-ctx.Done():
			log.Printf("[Scheduler] Maintenance window watcher stopped")
			return
		case <-ticker.C:
			observeLoop("maintenance", s.syncMaintenanceWindows())
		}
	}
}

func (s *Scheduler) syncMaintenanceWindows() error {
	var windows []models.MaintenanceWindow
	if err := s.db.Find(&windows).Error; err != nil {
		return err
	}
	var alertmanagers []models.Component
	if err := s.db.Preload("Host").Where("type = ? AND status = ?", models.ComponentAlertmanager, models.StatusRunning).
		Order("id ASC").Find(&alertmanagers).Error; err != nil {
		return err
	}

	var reconcileErrors []error
	now := time.Now()
	for i := range windows {
		w := &windows[i]
		var refs []models.MaintenanceSilence
		if err := s.db.Where("maintenance_window_id = ?", w.ID).Find(&refs).Error; err != nil {
			continue
		}
		_, scopeErr := services.MaintenanceMatchers(s.db, w)

		// Adopt the legacy single silence reference so upgrades do not leak it.
		if len(refs) == 0 && w.SilenceID != "" {
			for j := range alertmanagers {
				if scopeErr != nil || w.ClusterID == nil || alertmanagers[j].ClusterID == *w.ClusterID {
					ref := models.MaintenanceSilence{
						MaintenanceWindowID:     w.ID,
						AlertmanagerComponentID: alertmanagers[j].ID,
						SilenceID:               w.SilenceID,
						Status:                  "pending",
					}
					if s.db.Create(&ref).Error == nil {
						refs = append(refs, ref)
						s.db.Model(w).Update("silence_id", "")
					}
					break
				}
			}
		}
		if scopeErr != nil {
			reconcileErrors = append(reconcileErrors, fmt.Errorf("maintenance window %d: %w", w.ID, scopeErr))
			for j := range refs {
				var am models.Component
				if err := s.db.Preload("Host").First(&am, refs[j].AlertmanagerComponentID).Error; err != nil {
					s.updateSilenceError(&refs[j], scopeErr.Error())
					continue
				}
				s.expireWindowSilence(&am, &refs[j])
				s.updateSilenceError(&refs[j], scopeErr.Error())
			}
			continue
		}

		active := !now.Before(w.StartAt) && now.Before(w.EndAt)
		deleting := false
		for j := range refs {
			if refs[j].DeleteRequested {
				deleting = true
				break
			}
		}

		byAlertmanager := make(map[uint]*models.MaintenanceSilence, len(refs))
		for j := range refs {
			byAlertmanager[refs[j].AlertmanagerComponentID] = &refs[j]
		}

		if active && !deleting {
			for j := range alertmanagers {
				am := &alertmanagers[j]
				if w.ClusterID != nil && am.ClusterID != *w.ClusterID {
					continue
				}
				ref := byAlertmanager[am.ID]
				if ref == nil {
					ref = &models.MaintenanceSilence{
						MaintenanceWindowID:     w.ID,
						AlertmanagerComponentID: am.ID,
						Status:                  "pending",
					}
					if err := s.db.Create(ref).Error; err != nil {
						continue
					}
					refs = append(refs, *ref)
				}
				s.ensureWindowSilence(w, am, ref)
			}
		} else {
			for j := range refs {
				ref := &refs[j]
				if ref.Status == "expired" {
					continue
				}
				var am models.Component
				if err := s.db.Preload("Host").First(&am, ref.AlertmanagerComponentID).Error; err != nil {
					s.updateSilenceError(ref, "Alertmanager component unavailable: "+err.Error())
					continue
				}
				s.expireWindowSilence(&am, ref)
			}
		}

		if deleting {
			var remaining int64
			s.db.Model(&models.MaintenanceSilence{}).
				Where("maintenance_window_id = ? AND status <> ?", w.ID, "expired").
				Count(&remaining)
			if remaining == 0 {
				_ = s.db.Transaction(func(tx *gorm.DB) error {
					if err := tx.Where("maintenance_window_id = ?", w.ID).Delete(&models.MaintenanceSilence{}).Error; err != nil {
						return err
					}
					return tx.Delete(&models.MaintenanceWindow{}, w.ID).Error
				})
			}
		}
	}
	return errors.Join(reconcileErrors...)
}

func alertmanagerEndpoint(comp *models.Component) string {
	endpoint := comp.ExternalURL
	if endpoint == "" {
		endpoint = comp.InternalURL
	}
	if endpoint == "" {
		host := comp.Host.IP
		if host == "" {
			host = "localhost"
		}
		port := comp.Port
		if port == 0 {
			port = 9093
		}
		endpoint = "http://" + net.JoinHostPort(host, strconv.Itoa(port))
	}
	return strings.TrimRight(endpoint, "/")
}

func (s *Scheduler) ensureWindowSilence(w *models.MaintenanceWindow, am *models.Component, ref *models.MaintenanceSilence) {
	matchers, scopeErr := services.MaintenanceMatchers(s.db, w)
	if scopeErr != nil {
		if ref.SilenceID != "" {
			s.expireWindowSilence(am, ref)
		}
		s.updateSilenceError(ref, scopeErr.Error())
		return
	}
	if ref.DeleteRequested {
		return
	}
	if ref.SilenceID != "" {
		state, err := fetchSilenceState(am, ref.SilenceID)
		if err != nil {
			s.updateSilenceError(ref, err.Error())
			return
		}
		now := time.Now()
		if state == "active" || state == "pending" {
			s.db.Model(ref).Updates(map[string]interface{}{"status": state, "last_error": "", "last_checked_at": now})
			return
		}
		s.db.Model(ref).Updates(map[string]interface{}{"status": "expired", "last_error": "", "last_checked_at": now})
		ref.Status = "expired"
	}

	comment := fmt.Sprintf("维护窗口：%s", w.Name)
	if w.Reason != "" {
		comment += "（" + w.Reason + "）"
	}
	body, _ := json.Marshal(map[string]interface{}{
		"matchers": matchers, "startsAt": w.StartAt.Format(time.RFC3339),
		"endsAt": w.EndAt.Format(time.RFC3339), "comment": comment,
		"createdBy": "promlifecycle-maintenance",
	})
	req, err := http.NewRequest(http.MethodPost, alertmanagerEndpoint(am)+"/api/v2/silences", bytes.NewReader(body))
	if err != nil {
		s.updateSilenceError(ref, err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := maintenanceHTTPClient.Do(req)
	if err != nil {
		s.updateSilenceError(ref, err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		s.updateSilenceError(ref, fmt.Sprintf("create silence HTTP %d", resp.StatusCode))
		return
	}
	var out struct {
		SilenceID string `json:"silenceId"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil || out.SilenceID == "" {
		s.updateSilenceError(ref, "create silence returned no silenceId")
		return
	}
	now := time.Now()
	status := "pending"
	lastError := ""
	if state, stateErr := fetchSilenceState(am, out.SilenceID); stateErr == nil && state != "" {
		status = state
	} else if stateErr != nil {
		lastError = stateErr.Error()
	}
	s.db.Model(ref).Updates(map[string]interface{}{
		"silence_id": out.SilenceID, "status": status, "last_error": lastError, "last_checked_at": now,
	})
}

func (s *Scheduler) expireWindowSilence(am *models.Component, ref *models.MaintenanceSilence) {
	if ref.SilenceID == "" {
		s.db.Model(ref).Updates(map[string]interface{}{"status": "expired", "last_error": "", "last_checked_at": time.Now()})
		return
	}
	req, err := http.NewRequest(http.MethodDelete,
		alertmanagerEndpoint(am)+"/api/v2/silence/"+url.PathEscape(ref.SilenceID), nil)
	if err != nil {
		s.updateSilenceError(ref, err.Error())
		return
	}
	resp, err := maintenanceHTTPClient.Do(req)
	if err != nil {
		s.updateSilenceError(ref, err.Error())
		return
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if (resp.StatusCode < 200 || resp.StatusCode >= 300) && resp.StatusCode != http.StatusNotFound {
		s.db.Model(ref).Updates(map[string]interface{}{
			"status": "pending_delete", "last_error": fmt.Sprintf("expire silence HTTP %d", resp.StatusCode),
			"last_checked_at": time.Now(),
		})
		return
	}
	s.db.Model(ref).Updates(map[string]interface{}{"status": "expired", "last_error": "", "last_checked_at": time.Now()})
}

func (s *Scheduler) updateSilenceError(ref *models.MaintenanceSilence, message string) {
	status := ref.Status
	if status == "" || status == "expired" {
		status = "pending"
	}
	if ref.DeleteRequested {
		status = "pending_delete"
	}
	s.db.Model(ref).Updates(map[string]interface{}{"status": status, "last_error": message, "last_checked_at": time.Now()})
}

func fetchSilenceState(am *models.Component, silenceID string) (string, error) {
	req, err := http.NewRequest(http.MethodGet,
		alertmanagerEndpoint(am)+"/api/v2/silence/"+url.PathEscape(silenceID), nil)
	if err != nil {
		return "", err
	}
	resp, err := maintenanceHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "expired", nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("get silence HTTP %d", resp.StatusCode)
	}
	var out struct {
		Status struct {
			State string `json:"state"`
		} `json:"status"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return "", err
	}
	if out.Status.State == "" {
		return "", fmt.Errorf("silence state missing")
	}
	return out.Status.State, nil
}

var maintenanceHTTPClient = &http.Client{Timeout: 15 * time.Second}
