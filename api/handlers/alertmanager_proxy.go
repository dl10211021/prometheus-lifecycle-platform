package handlers

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"gorm.io/gorm"
)

// AlertmanagerProxyHandler handles proxy requests to Alertmanager API
type AlertmanagerProxyHandler struct {
	db *gorm.DB
}

// NewAlertmanagerProxyHandler creates a new AlertmanagerProxyHandler
func NewAlertmanagerProxyHandler(db *gorm.DB) *AlertmanagerProxyHandler {
	return &AlertmanagerProxyHandler{db: db}
}

// findAlertmanagerEndpoint returns the Alertmanager HTTP endpoint for the given cluster
func (h *AlertmanagerProxyHandler) findAlertmanagerEndpoint(clusterID uint) (string, error) {
	var amComp models.Component
	if err := h.db.Preload("Host").Where("cluster_id = ? AND type = ? AND status = ?",
		clusterID, models.ComponentAlertmanager, models.StatusRunning).
		First(&amComp).Error; err != nil {
		return "", fmt.Errorf("alertmanager not found for cluster %d", clusterID)
	}

	port := amComp.Port
	if port == 0 {
		port = 9093
	}

	// Prefer external URL if available
	if amComp.ExternalURL != "" {
		return amComp.ExternalURL, nil
	}

	hostIP := amComp.Host.IP
	return fmt.Sprintf("http://%s:%d", hostIP, port), nil
}

// Proxy forwards requests to Alertmanager API
// Supports: /api/v1/alertmanager/:cluster_id/alerts, /silences, /status, /receivers
func (h *AlertmanagerProxyHandler) Proxy(c *gin.Context) {
	clusterID, ok := utils.ParseUintParam(c, "cluster_id")
	if !ok {
		return
	}

	// Get Alertmanager endpoint
	endpoint, err := h.findAlertmanagerEndpoint(clusterID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "操作失败: " + err.Error()})
		return
	}

	// Build target URL by appending the remaining path
	originalPath := c.Request.URL.Path
	// Strip the prefix "/api/v1/alertmanager/:cluster_id"
	prefix := fmt.Sprintf("/api/v1/alertmanager/%d", clusterID)
	remainingPath := strings.TrimPrefix(originalPath, prefix)
	if remainingPath == "" {
		remainingPath = "/"
	}
	// Map legacy relative paths to the current Alertmanager v2 API
	if remainingPath == "/silences" || strings.HasPrefix(remainingPath, "/silences/") {
		remainingPath = strings.Replace(remainingPath, "/silences", "/api/v2/silences", 1)
	}

	targetURL := endpoint + remainingPath

	// Copy query parameters
	if c.Request.URL.RawQuery != "" {
		targetURL += "?" + c.Request.URL.RawQuery
	}

	// Read request body (bounded: a crafted multi-GB body would otherwise be
	// buffered fully into memory by an authenticated caller)
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "读取请求内容失败，请确认内容不超过 1MB"})
		return
	}

	// Create proxied request
	var reqBody io.Reader
	if len(body) > 0 {
		reqBody = bytes.NewReader(body)
	}
	proxiedReq, err := http.NewRequestWithContext(c.Request.Context(), c.Request.Method, targetURL, reqBody)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "创建代理请求失败: " + err.Error()})
		return
	}

	copyAlertmanagerRequestHeaders(proxiedReq.Header, c.Request.Header)

	// Execute proxied request
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(proxiedReq)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Alertmanager 上游请求失败，请检查组件状态和网络: " + err.Error()})
		return
	}
	defer resp.Body.Close()

	// Read response body
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "读取 Alertmanager 上游响应失败，请检查组件日志"})
		return
	}

	// Set response status code
	c.Status(resp.StatusCode)

	// Copy response headers
	for key, values := range resp.Header {
		if key == "Content-Type" || key == "Retry-After" {
			for _, value := range values {
				c.Header(key, value)
			}
		}
	}

	// Write response body
	c.Writer.Write(respBody)
}

func copyAlertmanagerRequestHeaders(dst, src http.Header) {
	// Authentication belongs to the platform, not to the managed upstream.
	for _, key := range []string{"Accept", "Content-Type"} {
		for _, value := range src.Values(key) {
			dst.Add(key, value)
		}
	}
}

// GetAlerts queries active alerts from Alertmanager
func (h *AlertmanagerProxyHandler) GetAlerts(c *gin.Context) {
	clusterID, ok := utils.ParseUintParam(c, "cluster_id")
	if !ok {
		return
	}

	endpoint, err := h.findAlertmanagerEndpoint(clusterID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "操作失败: " + err.Error()})
		return
	}

	url := endpoint + "/api/v2/alerts"

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Alertmanager 上游请求失败，请检查组件状态和网络: " + err.Error()})
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "读取 Alertmanager 响应失败，请检查组件日志"})
		return
	}

	c.Data(resp.StatusCode, "application/json", body)
}

// GetStatus returns Alertmanager cluster status
func (h *AlertmanagerProxyHandler) GetStatus(c *gin.Context) {
	clusterID, ok := utils.ParseUintParam(c, "cluster_id")
	if !ok {
		return
	}

	endpoint, err := h.findAlertmanagerEndpoint(clusterID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "操作失败: " + err.Error()})
		return
	}

	url := endpoint + "/api/v2/status"

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Alertmanager 上游请求失败，请检查组件状态和网络: " + err.Error()})
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "读取 Alertmanager 响应失败，请检查组件日志"})
		return
	}

	c.Data(resp.StatusCode, "application/json", body)
}

// GetReceivers returns configured receivers
func (h *AlertmanagerProxyHandler) GetReceivers(c *gin.Context) {
	clusterID, ok := utils.ParseUintParam(c, "cluster_id")
	if !ok {
		return
	}

	endpoint, err := h.findAlertmanagerEndpoint(clusterID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "操作失败: " + err.Error()})
		return
	}

	url := endpoint + "/api/v2/receivers"

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Alertmanager 上游请求失败，请检查组件状态和网络: " + err.Error()})
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "读取 Alertmanager 响应失败，请检查组件日志"})
		return
	}

	c.Data(resp.StatusCode, "application/json", body)
}

// PostAlerts posts alerts directly to Alertmanager
func (h *AlertmanagerProxyHandler) PostAlerts(c *gin.Context) {
	clusterID, ok := utils.ParseUintParam(c, "cluster_id")
	if !ok {
		return
	}

	endpoint, err := h.findAlertmanagerEndpoint(clusterID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "操作失败: " + err.Error()})
		return
	}

	url := endpoint + "/api/v2/alerts"

	// Read request body before creating new request (body can only be read once)
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4<<20)
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "读取请求内容失败，请确认内容不超过 4MB"})
		return
	}

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Post(url, c.ContentType(), bytes.NewReader(body))
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Alertmanager 上游请求失败，请检查组件状态和网络: " + err.Error()})
		return
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "读取 Alertmanager 响应失败，请检查组件日志"})
		return
	}

	c.Data(resp.StatusCode, "application/json", respBody)
}
