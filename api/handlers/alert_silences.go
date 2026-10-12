package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"github.com/promlifecycle/platform/services"
	"gorm.io/gorm"
)

// AlertSilenceHandler handles silence CRUD operations
type AlertSilenceHandler struct {
	db *gorm.DB
}

// NewAlertSilenceHandler creates a new AlertSilenceHandler
func NewAlertSilenceHandler(db *gorm.DB) *AlertSilenceHandler {
	return &AlertSilenceHandler{db: db}
}

func (h *AlertSilenceHandler) resolveClusterID(c *gin.Context) (uint, bool) {
	if c.Param("cluster_id") != "" {
		return utils.ParseUintParam(c, "cluster_id")
	}
	id, err := services.SingleNodeID(h.db)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取节点配置失败，请刷新页面后重试"})
		return 0, false
	}
	return id, true
}

// findAlertmanagerEndpoint returns the Alertmanager HTTP endpoint for the given cluster
func (h *AlertSilenceHandler) findAlertmanagerEndpoint(clusterID uint) (string, error) {
	var amComp models.Component
	if err := h.db.Preload("Host").Where("cluster_id = ? AND type = ? AND status = ?",
		clusterID, models.ComponentAlertmanager, models.StatusRunning).
		First(&amComp).Error; err != nil {
		return "", fmt.Errorf("未找到运行中的 Alertmanager")
	}

	port := amComp.Port
	if port == 0 {
		port = 9093
	}

	if amComp.ExternalURL != "" {
		return strings.TrimSuffix(amComp.ExternalURL, "/"), nil
	}
	if amComp.InternalURL != "" {
		return strings.TrimSuffix(amComp.InternalURL, "/"), nil
	}

	hostIP := amComp.Host.IP
	return fmt.Sprintf("http://%s:%d", hostIP, port), nil
}

// SilenceMatcher represents a matcher for silence creation
type SilenceMatcher struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	IsRegex bool   `json:"isRegex"`
}

// SilenceRequest represents a request to create a silence
type SilenceRequest struct {
	Matchers []SilenceMatcher `json:"matchers" binding:"required"`
	Comment  string           `json:"comment"`
	Expires  string           `json:"expires"` // duration string like "1h", "24h", "7d"
	StartsAt *time.Time       `json:"starts_at"`
	EndsAt   *time.Time       `json:"ends_at"`
}

// SilenceResponse represents Alertmanager silence response
type SilenceResponse struct {
	Status string `json:"status"`
	Data   struct {
		ID string `json:"id"`
	} `json:"data"`
}

type silenceListItem struct {
	ID             uint             `json:"id"`
	ExternalID     string           `json:"external_id"`
	Matchers       string           `json:"matchers"`
	ParsedMatchers []SilenceMatcher `json:"parsed_matchers"`
	Comment        string           `json:"comment"`
	CreatedBy      string           `json:"created_by"`
	StartsAt       time.Time        `json:"starts_at"`
	EndsAt         time.Time        `json:"ends_at"`
	Status         string           `json:"status"`
	CreatedAt      time.Time        `json:"created_at"`
	UpdatedAt      time.Time        `json:"updated_at"`
}

func silenceListResponse(silences []models.AlertSilence) []silenceListItem {
	result := make([]silenceListItem, 0, len(silences))
	for _, silence := range silences {
		matchers := []SilenceMatcher{}
		_ = json.Unmarshal([]byte(silence.Matchers), &matchers)
		result = append(result, silenceListItem{
			ID: silence.ID, ExternalID: silence.ExternalID, Matchers: silence.Matchers,
			ParsedMatchers: matchers, Comment: silence.Comment, CreatedBy: silence.CreatedBy,
			StartsAt: silence.StartsAt, EndsAt: silence.EndsAt, Status: silence.Status,
			CreatedAt: silence.CreatedAt, UpdatedAt: silence.UpdatedAt,
		})
	}
	return result
}

// CreateSilence creates a new silence in Alertmanager
func (h *AlertSilenceHandler) CreateSilence(c *gin.Context) {
	clusterID, ok := h.resolveClusterID(c)
	if !ok {
		return
	}

	var req SilenceRequest
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "静默请求无效或超过 1MB，请检查 matcher、时间和备注后重试"})
		return
	}
	if len(req.Matchers) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "至少需要一个 matcher，请填写后重试"})
		return
	}
	for _, matcher := range req.Matchers {
		if !alertLabelNamePattern.MatchString(matcher.Name) || matcher.Name == "cluster_id" || strings.ContainsAny(matcher.Value, "\r\n\x00") {
			c.JSON(http.StatusBadRequest, gin.H{"error": "matcher 无效，请使用合法标签名和值后重试"})
			return
		}
		if matcher.IsRegex {
			if _, err := regexp.Compile(matcher.Value); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "matcher 正则表达式无效，请修正后重试"})
				return
			}
		}
	}

	endpoint, err := h.findAlertmanagerEndpoint(clusterID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "未找到可用的 Alertmanager，请先启动组件后重试"})
		return
	}

	// Build Alertmanager silence payload
	startsAt := time.Now()
	if req.StartsAt != nil {
		startsAt = *req.StartsAt
	}

	endsAt := time.Now().Add(time.Hour * 24) // default 24 hours
	if req.EndsAt != nil {
		endsAt = *req.EndsAt
	} else if req.Expires != "" {
		// Parse duration string
		duration, err := parseDuration(req.Expires)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "静默时长无效，请使用 30m、1h、7d 等格式"})
			return
		}
		endsAt = time.Now().Add(duration)
	}
	if !endsAt.After(startsAt) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "静默结束时间必须晚于开始时间，请修正后重试"})
		return
	}

	// Build matchers for Alertmanager API
	createdBy := c.GetString("username")
	if createdBy == "" {
		createdBy = "promlifecycle"
	}
	matchers := make([]map[string]interface{}, 0, len(req.Matchers))
	for _, m := range req.Matchers {
		matcher := map[string]interface{}{
			"name":    m.Name,
			"value":   m.Value,
			"isRegex": m.IsRegex,
			"isEqual": true,
		}
		matchers = append(matchers, matcher)
	}

	payload := map[string]interface{}{
		"matchers":  matchers,
		"startsAt":  startsAt.Format(time.RFC3339),
		"endsAt":    endsAt.Format(time.RFC3339),
		"comment":   req.Comment,
		"createdBy": createdBy,
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "生成静默请求失败，请稍后重试"})
		return
	}

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Post(endpoint+"/api/v2/silences", "application/json", bytes.NewReader(payloadBytes))
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Alertmanager 无法访问，请检查组件状态和网络后重试"})
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "读取 Alertmanager 响应失败，请稍后重试"})
		return
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Alertmanager 拒绝创建静默，请检查 matcher 和时间后重试", "upstream_status": resp.StatusCode})
		return
	}

	// Parse response to get silence ID. The Alertmanager v2 API returns
	// {"silenceID":"..."}, while older versions wrap it in data.id.
	var amResp struct {
		SilenceID string `json:"silenceID"`
		Status    string `json:"status"`
		Data      struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &amResp); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Alertmanager 响应格式异常，请检查组件版本和日志后重试"})
		return
	}
	externalID := amResp.SilenceID
	if externalID == "" {
		externalID = amResp.Data.ID
	}
	if externalID == "" {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Alertmanager 未返回静默 ID，请检查组件日志后重试"})
		return
	}

	// Store silence in local database
	createdAtBy := c.GetString("username")

	matchersJSON, _ := json.Marshal(req.Matchers)
	silence := models.AlertSilence{
		ClusterID:  clusterID,
		ExternalID: externalID,
		Matchers:   string(matchersJSON),
		Comment:    req.Comment,
		CreatedBy:  createdAtBy,
		StartsAt:   startsAt,
		EndsAt:     endsAt,
		Status:     "pending",
	}
	if err := h.db.Create(&silence).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "静默已在 Alertmanager 创建，但本地记录失败，请刷新静默列表"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"id":          silence.ID,
		"external_id": externalID,
		"status":      silence.Status,
		"starts_at":   silence.StartsAt,
		"ends_at":     silence.EndsAt,
		"matchers":    req.Matchers,
		"comment":     silence.Comment,
	})
}

// ListSilences lists all silences for a cluster
func (h *AlertSilenceHandler) ListSilences(c *gin.Context) {
	clusterID, ok := h.resolveClusterID(c)
	if !ok {
		return
	}
	var silences []models.AlertSilence

	// Fetch from local DB for quick listing, but also sync with Alertmanager
	if err := h.db.Where("cluster_id = ? AND status != ?", clusterID, "expired").
		Order("created_at DESC").Find(&silences).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取静默列表失败，请稍后重试"})
		return
	}

	refreshError := ""
	endpoint, err := h.findAlertmanagerEndpoint(clusterID)
	if err != nil {
		refreshError = err.Error()
	} else {
		client := &http.Client{Timeout: 10 * time.Second}
		resp, fetchErr := client.Get(endpoint + "/api/v2/silences")
		if fetchErr != nil {
			refreshError = "Alertmanager 静默刷新失败"
		} else {
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				refreshError = fmt.Sprintf("Alertmanager 静默刷新 HTTP %d", resp.StatusCode)
			} else {
				var remote []struct {
					ID     string `json:"id"`
					Status struct {
						State string `json:"state"`
					} `json:"status"`
				}
				if err := json.NewDecoder(io.LimitReader(resp.Body, 10<<20)).Decode(&remote); err != nil {
					refreshError = "Alertmanager v2 静默响应格式无效"
				} else {
					err := h.db.Transaction(func(tx *gorm.DB) error {
						for _, item := range remote {
							if item.ID == "" || (item.Status.State != "active" && item.Status.State != "pending" && item.Status.State != "expired") {
								return fmt.Errorf("Alertmanager 静默状态无效")
							}
							if err := tx.Model(&models.AlertSilence{}).Where("cluster_id = ? AND external_id = ? AND status != ?", clusterID, item.ID, "pending_delete").Update("status", item.Status.State).Error; err != nil {
								return err
							}
						}
						return nil
					})
					if err != nil {
						refreshError = "保存静默状态失败: " + err.Error()
					}
				}
			}
		}
	}
	if refreshError == "" {
		if err := h.db.Where("cluster_id = ? AND status != ?", clusterID, "expired").Order("created_at DESC").Find(&silences).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "读取刷新后的静默列表失败"})
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"silences": silenceListResponse(silences), "refresh_error": refreshError, "stale": refreshError != ""})
}

// GetSilence gets a single silence by ID
func (h *AlertSilenceHandler) GetSilence(c *gin.Context) {
	clusterID, ok := h.resolveClusterID(c)
	if !ok {
		return
	}

	silenceID, _ := strconv.Atoi(c.Param("id"))
	var silence models.AlertSilence
	if err := h.db.Where("cluster_id = ? AND id = ?", clusterID, silenceID).First(&silence).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "未找到该静默，请刷新列表"})
		return
	}

	var matchers []SilenceMatcher
	json.Unmarshal([]byte(silence.Matchers), &matchers)

	c.JSON(http.StatusOK, gin.H{
		"id":          silence.ID,
		"external_id": silence.ExternalID,
		"matchers":    matchers,
		"comment":     silence.Comment,
		"created_by":  silence.CreatedBy,
		"starts_at":   silence.StartsAt,
		"ends_at":     silence.EndsAt,
		"status":      silence.Status,
		"created_at":  silence.CreatedAt,
		"updated_at":  silence.UpdatedAt,
	})
}

// DeleteSilence expires a silence in Alertmanager
func (h *AlertSilenceHandler) DeleteSilence(c *gin.Context) {
	clusterID, ok := h.resolveClusterID(c)
	if !ok {
		return
	}

	silenceID, err := strconv.Atoi(c.Param("id"))
	if err != nil || silenceID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "静默 ID 无效，请刷新列表后重试"})
		return
	}
	var silence models.AlertSilence
	if err := h.db.Where("cluster_id = ? AND id = ?", clusterID, silenceID).First(&silence).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "未找到该静默，请刷新列表"})
		return
	}
	if err := h.db.Model(&silence).Update("status", "pending_delete").Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "记录删除状态失败，请稍后重试"})
		return
	}

	endpoint, err := h.findAlertmanagerEndpoint(clusterID)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{
			"error":  "Alertmanager 当前不可用，删除请求已保留，请恢复组件后重试",
			"status": "pending_delete",
		})
		return
	}
	client := &http.Client{Timeout: 10 * time.Second}
	requestURL := endpoint + "/api/v2/silence/" + url.PathEscape(silence.ExternalID)
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodDelete, requestURL, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建删除请求失败，请稍后重试", "status": "pending_delete"})
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{
			"error":  "Alertmanager 无法访问，删除请求已保留，请检查网络后重试",
			"status": "pending_delete",
		})
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.JSON(http.StatusBadGateway, gin.H{
			"error":           "Alertmanager 拒绝删除，删除请求已保留，请检查组件日志后重试",
			"status":          "pending_delete",
			"upstream_status": resp.StatusCode,
		})
		return
	}
	if err := h.db.Model(&silence).Update("status", "expired").Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Alertmanager 已删除静默，但本地状态更新失败，请刷新列表", "status": "pending_delete"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "静默已删除", "status": "expired"})
}

// parseDuration parses duration strings like "1h", "24h", "7d", "30m"
func parseDuration(s string) (time.Duration, error) {
	// Try standard duration first
	d, err := time.ParseDuration(s)
	if err == nil {
		return d, nil
	}

	// Handle "d" for days
	if len(s) > 0 && s[len(s)-1] == 'd' {
		n := s[:len(s)-1]
		if num, err := strconv.Atoi(n); err == nil {
			return time.Duration(num) * 24 * time.Hour, nil
		}
	}

	return 0, fmt.Errorf("时长格式无效，请使用 30m、1h、7d 等格式: %s", s)
}
