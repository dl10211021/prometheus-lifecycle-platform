package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	neturl "net/url"
	"os"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/core/config"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"github.com/promlifecycle/platform/services"
	"gorm.io/gorm"
)

type DashboardHandler struct {
	db             *gorm.DB
	autoConfig     *config.AutoConfig
	basePath       string
	maxUploadBytes int64
}

func NewDashboardHandler(db *gorm.DB, autoConfig *config.AutoConfig, basePath string) *DashboardHandler {
	return &DashboardHandler{
		db: db, autoConfig: autoConfig, basePath: basePath,
		maxUploadBytes: dashboardUploadLimit(),
	}
}

// ListDashboards returns dashboards for the platform's internal single-node scope.
func (h *DashboardHandler) ListDashboards(c *gin.Context) {
	var dashboards []models.Dashboard
	enabled := c.Query("enabled")
	nodeID, err := services.SingleNodeID(h.db)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "无法解析单节点作用域"})
		return
	}

	query := h.db.Where("cluster_id = ?", nodeID).Order("created_at DESC")
	if enabled != "" {
		if enabled == "true" {
			query = query.Where("enabled = ?", true)
		} else if enabled == "false" {
			query = query.Where("enabled = ?", false)
		}
	}

	query.Find(&dashboards)
	c.JSON(http.StatusOK, gin.H{"dashboards": dashboards})
}

// GetDashboard returns a single dashboard by ID
func (h *DashboardHandler) GetDashboard(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效ID"})
		return
	}

	var dashboard models.Dashboard
	if err := h.db.First(&dashboard, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "仪表板不存在"})
		return
	}

	c.JSON(http.StatusOK, dashboard)
}

// CreateDashboard creates a new dashboard
func (h *DashboardHandler) CreateDashboard(c *gin.Context) {
	dashboard := models.Dashboard{Enabled: true}

	if err := c.ShouldBindJSON(&dashboard); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效请求参数: " + err.Error()})
		return
	}

	if dashboard.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "仪表板名称不能为空"})
		return
	}

	nodeID, err := services.SingleNodeID(h.db)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "无法解析单节点作用域"})
		return
	}
	dashboard.ClusterID = nodeID

	if dashboard.Source == "" {
		dashboard.Source = "manual"
	}

	// Validate content is valid JSON if provided
	if dashboard.Content != "" {
		var parsed map[string]interface{}
		if err := json.Unmarshal([]byte(dashboard.Content), &parsed); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "仪表板内容不是有效的JSON: " + err.Error()})
			return
		}
		// Extract UID from content if not set
		if dashboard.UID == "" {
			if uid, ok := parsed["uid"].(string); ok && uid != "" {
				dashboard.UID = uid
			}
		}
	}

	if err := createMonitoring(h.db, &dashboard); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建仪表板失败: " + err.Error()})
		return
	}

	go h.provisionDashboardAsync(dashboard)

	c.JSON(http.StatusAccepted, dashboard)
}

// UpdateDashboard updates an existing dashboard
func (h *DashboardHandler) UpdateDashboard(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效ID"})
		return
	}

	var dashboard models.Dashboard
	if err := h.db.First(&dashboard, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "仪表板不存在"})
		return
	}

	var req struct {
		Name    *string `json:"name"`
		Source  *string `json:"source"`
		URL     *string `json:"url"`
		Content *string `json:"content"`
		Enabled *bool   `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效请求参数"})
		return
	}

	updates := map[string]interface{}{}
	if req.Name != nil {
		updates["name"] = *req.Name
	}
	if req.Source != nil {
		updates["source"] = *req.Source
	}
	if req.URL != nil {
		updates["url"] = *req.URL
	}
	if req.Content != nil {
		if *req.Content != "" {
			var parsed map[string]interface{}
			if err := json.Unmarshal([]byte(*req.Content), &parsed); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "仪表板内容不是有效的JSON"})
				return
			}
		}
		updates["content"] = *req.Content
	}
	if req.Enabled != nil {
		updates["enabled"] = *req.Enabled
	}
	if len(updates) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "没有可更新的字段"})
		return
	}

	if err := updateMonitoring(h.db, &dashboard, updates); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "更新仪表板失败"})
		return
	}

	h.db.First(&dashboard, id)

	// Disabling must remove the previously published dashboard as well.
	go h.provisionDashboardAsync(dashboard)

	c.JSON(http.StatusAccepted, dashboard)
}

// DeleteDashboard deletes a dashboard
func (h *DashboardHandler) DeleteDashboard(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效ID"})
		return
	}

	var dashboard models.Dashboard
	if err := h.db.First(&dashboard, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "仪表板不存在"})
		return
	}

	if err := deleteMonitoring(h.db, &dashboard); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "删除仪表板失败"})
		return
	}

	go h.provisionDashboardAsync(dashboard)

	c.JSON(http.StatusAccepted, gin.H{"message": "删除已排队", "apply_status": "pending", "delete_pending": true})
}

// ImportDashboard imports a dashboard from a URL (e.g., Grafana.com)
func (h *DashboardHandler) ImportDashboard(c *gin.Context) {
	url := c.Query("url")
	if url == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "URL参数不能为空"})
		return
	}
	nodeID, err := services.SingleNodeID(h.db)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "无法解析单节点作用域"})
		return
	}

	parsed, err := neturl.Parse(url)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "URL 必须是合法的 http/https 地址"})
		return
	}

	// Fetch dashboard JSON from URL with a timeout and a size limit so a slow
	// or oversized upstream cannot hang the request or exhaust memory.
	// The custom dialer refuses private/loopback destinations (SSRF guard),
	// and redirects are not followed to a different host.
	client := &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			Proxy:       http.ProxyFromEnvironment,
			DialContext: utils.SafeDialContext,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			if len(via) > 0 && req.URL.Host != via[0].URL.Host {
				return fmt.Errorf("redirect to a different host is not allowed")
			}
			return nil
		},
	}
	resp, err := client.Get(parsed.String())
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "无法从URL获取仪表板: " + err.Error()})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.JSON(http.StatusBadGateway, gin.H{"error": "URL返回错误状态码: " + strconv.Itoa(resp.StatusCode)})
		return
	}

	dashboardJSON, err := decodeDashboardJSON(resp.Body, h.maxUploadBytes)
	if errors.Is(err, errDashboardTooLarge) {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "仪表板JSON超过大小限制"})
		return
	}
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "URL返回的内容不是有效的仪表板JSON"})
		return
	}

	// Handle Grafana.com format (wraps dashboard in API response)
	if apiResp, ok := dashboardJSON["dashboard"]; ok {
		if d, ok := apiResp.(map[string]interface{}); ok {
			dashboardJSON = d
		}
	}

	name, _ := dashboardJSON["title"].(string)
	if name == "" {
		name = "imported_dashboard"
	}

	uid, _ := dashboardJSON["uid"].(string)

	contentStr, err := json.Marshal(dashboardJSON)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "处理仪表板JSON失败"})
		return
	}

	dashboard := models.Dashboard{
		ClusterID: nodeID,
		Name:      name,
		UID:       uid,
		Source:    "url",
		URL:       url,
		Content:   string(contentStr),
		Enabled:   true,
		Version:   1,
	}

	if err := createMonitoring(h.db, &dashboard); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建仪表板失败: " + err.Error()})
		return
	}

	// Trigger provisioning
	go h.provisionDashboardAsync(dashboard)

	c.JSON(http.StatusAccepted, dashboard)
}

// UploadDashboard uploads a dashboard from a JSON file
func (h *DashboardHandler) UploadDashboard(c *gin.Context) {
	// Bound the complete multipart request as well as the file stream. The
	// allowance covers multipart headers without weakening the file limit.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.maxUploadBytes+(1<<20))
	file, err := c.FormFile("file")
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "仪表板文件超过大小限制"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "请上传仪表板JSON文件"})
		return
	}
	if file.Size > h.maxUploadBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "仪表板文件超过大小限制"})
		return
	}

	nodeID, err := services.SingleNodeID(h.db)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "无法解析单节点作用域"})
		return
	}

	src, err := file.Open()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "打开文件失败"})
		return
	}
	defer src.Close()

	dashboardJSON, err := decodeDashboardJSON(src, h.maxUploadBytes)
	if errors.Is(err, errDashboardTooLarge) {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "仪表板文件超过大小限制"})
		return
	}
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "文件内容不是有效的仪表板JSON"})
		return
	}

	name, _ := dashboardJSON["title"].(string)
	if name == "" {
		name = "uploaded_dashboard"
	}

	uid, _ := dashboardJSON["uid"].(string)

	contentStr, err := json.Marshal(dashboardJSON)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "处理仪表板JSON失败"})
		return
	}

	dashboard := models.Dashboard{
		ClusterID: nodeID,
		Name:      name,
		UID:       uid,
		Source:    "json",
		URL:       file.Filename,
		Content:   string(contentStr),
		Enabled:   true,
		Version:   1,
	}

	if err := createMonitoring(h.db, &dashboard); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建仪表板失败: " + err.Error()})
		return
	}

	// Trigger provisioning
	go h.provisionDashboardAsync(dashboard)

	c.JSON(http.StatusAccepted, dashboard)
}

// ExportDashboard exports a dashboard as JSON file
func (h *DashboardHandler) ExportDashboard(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效ID"})
		return
	}

	var dashboard models.Dashboard
	if err := h.db.First(&dashboard, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "仪表板不存在"})
		return
	}

	if dashboard.Content == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "仪表板内容为空，无法导出"})
		return
	}

	// Pretty-print JSON
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(dashboard.Content), &parsed); err == nil {
		pretty, _ := json.MarshalIndent(parsed, "", "  ")
		dashboard.Content = string(pretty)
	}

	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="dashboard-%s.json"`, sanitizeDashboardFilename(dashboard.Name)))
	c.Header("Content-Type", "application/json")
	c.String(http.StatusOK, dashboard.Content)
}

// provisionDashboardAsync reconciles current database state, not a stale row snapshot.
func (h *DashboardHandler) provisionDashboardAsync(dashboard models.Dashboard) {
	if err := h.autoConfig.ProvisionGrafanaDashboards(dashboard.ClusterID, h.basePath); err != nil {
		log.Printf("[Dashboard] Publication failed for node %d: %v", dashboard.ClusterID, err)
	}
}

func sanitizeDashboardFilename(name string) string {
	result := ""
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			result += string(r)
		} else {
			result += "_"
		}
	}
	if result == "" {
		result = "dashboard"
	}
	return result
}

const defaultDashboardUploadBytes int64 = 10 << 20

var errDashboardTooLarge = errors.New("dashboard JSON exceeds size limit")

func dashboardUploadLimit() int64 {
	raw := os.Getenv("PROMLIFECYCLE_DASHBOARD_MAX_UPLOAD_BYTES")
	if raw == "" {
		return defaultDashboardUploadBytes
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 1024 || value > 1<<30 {
		return defaultDashboardUploadBytes
	}
	return value
}

func decodeDashboardJSON(reader io.Reader, maxBytes int64) (map[string]interface{}, error) {
	limited := &io.LimitedReader{R: reader, N: maxBytes + 1}
	decoder := json.NewDecoder(limited)
	var dashboard map[string]interface{}
	if err := decoder.Decode(&dashboard); err != nil {
		if limited.N == 0 {
			return nil, errDashboardTooLarge
		}
		return nil, err
	}
	var trailing interface{}
	err := decoder.Decode(&trailing)
	if limited.N == 0 {
		return nil, errDashboardTooLarge
	}
	if err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("multiple JSON values are not allowed")
		}
		return nil, err
	}
	if dashboard == nil {
		return nil, fmt.Errorf("dashboard JSON must be an object")
	}
	return dashboard, nil
}
