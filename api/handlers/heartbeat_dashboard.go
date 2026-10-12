package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
)

type heartbeatTarget struct {
	ID           string   `json:"id"`
	Status       string   `json:"status"`
	LastCheck    string   `json:"last_check,omitempty"`
	Address      string   `json:"address"`
	Job          string   `json:"job"`
	Component    string   `json:"component,omitempty"`
	Description  string   `json:"description,omitempty"`
	Interval     string   `json:"interval"`
	LastError    string   `json:"last_error,omitempty"`
	Receivers    []string `json:"receivers"`
	HasHistory   bool     `json:"has_history"`
	ScrapeURL    string   `json:"scrape_url,omitempty"`
	DurationSecs float64  `json:"duration_seconds,omitempty"`
}

func (h *Handler) GetHeartbeatDashboard(c *gin.Context) {
	var prometheus models.Component
	if err := h.db.Preload("Host").Where("type = ? AND status = ?", models.ComponentPrometheus, models.StatusRunning).First(&prometheus).Error; err != nil {
		c.JSON(http.StatusOK, gin.H{"summary": gin.H{"total": 0, "ok": 0, "late": 0, "missing": 0}, "targets": []heartbeatTarget{}, "message": "部署 Prometheus 后自动显示监控状态"})
		return
	}
	var channels []models.NotificationChannel
	h.db.Where("enabled = ?", true).Find(&channels)
	receivers := make([]string, 0, len(channels))
	for _, channel := range channels {
		receivers = append(receivers, channel.Name)
	}
	var historyCount int64
	h.db.Model(&models.AlertHistory{}).Count(&historyCount)

	ctx, cancel := context.WithTimeout(c.Request.Context(), 8*time.Second)
	defer cancel()
	endpoint := componentHTTPURL(prometheus, 9090)
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/api/v1/targets", nil)
	response, err := (&http.Client{Timeout: 8 * time.Second}).Do(request)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"summary": gin.H{"total": 1, "ok": 0, "late": 0, "missing": 1}, "targets": []heartbeatTarget{{ID: "prometheus", Status: "missing", Address: endpoint, Job: "prometheus", Interval: "15s", LastError: err.Error()}}, "message": "Prometheus targets API 不可达"})
		return
	}
	defer response.Body.Close()
	var payload struct {
		Data struct {
			ActiveTargets []struct {
				DiscoveredLabels map[string]string `json:"discoveredLabels"`
				Labels           map[string]string `json:"labels"`
				ScrapePool       string            `json:"scrapePool"`
				ScrapeURL        string            `json:"scrapeUrl"`
				LastError        string            `json:"lastError"`
				LastScrape       time.Time         `json:"lastScrape"`
				LastDuration     float64           `json:"lastScrapeDuration"`
				Health           string            `json:"health"`
			} `json:"activeTargets"`
		} `json:"data"`
	}
	if json.NewDecoder(response.Body).Decode(&payload) != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Prometheus targets 返回无效"})
		return
	}
	targets := make([]heartbeatTarget, 0, len(payload.Data.ActiveTargets))
	summary := map[string]int{"total": 0, "ok": 0, "late": 0, "missing": 0}
	for _, active := range payload.Data.ActiveTargets {
		status := "late"
		switch active.Health {
		case "up":
			status = "ok"
		case "down":
			status = "missing"
		}
		summary["total"]++
		summary[status]++
		instance := active.Labels["instance"]
		job := active.Labels["job"]
		name := active.Labels["name"]
		if name == "" {
			name = strings.TrimPrefix(job, "node_exporter_")
		}
		if name == "" || name == job {
			name = instance
		}
		lastCheck := ""
		if !active.LastScrape.IsZero() {
			lastCheck = active.LastScrape.Format(time.RFC3339)
		}
		targets = append(targets, heartbeatTarget{
			ID: name, Status: status, LastCheck: lastCheck, Address: instance, Job: job,
			Component: active.Labels["component"], Description: active.Labels["host"],
			Interval: "15s", LastError: active.LastError, Receivers: receivers,
			HasHistory: historyCount > 0 || active.LastError != "", ScrapeURL: active.ScrapeURL,
			DurationSecs: active.LastDuration,
		})
	}
	c.JSON(http.StatusOK, gin.H{"summary": summary, "targets": targets, "updated_at": time.Now(), "prometheus": endpoint})
}
