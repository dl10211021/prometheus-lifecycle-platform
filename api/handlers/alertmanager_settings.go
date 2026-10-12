package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/core/config"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/services"
	"gorm.io/gorm"
)

var alertLabelNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var alertReceiverPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type AlertmanagerSettingsHandler struct {
	db         *gorm.DB
	autoConfig *config.AutoConfig
	basePath   string
}

func NewAlertmanagerSettingsHandler(db *gorm.DB, autoConfig *config.AutoConfig, basePath string) *AlertmanagerSettingsHandler {
	return &AlertmanagerSettingsHandler{db: db, autoConfig: autoConfig, basePath: basePath}
}

type alertmanagerRouteRequest struct {
	Name           string            `json:"name"`
	Receiver       string            `json:"receiver"`
	Match          map[string]string `json:"match"`
	MatchRE        map[string]string `json:"match_re"`
	GroupBy        []string          `json:"group_by"`
	GroupWait      string            `json:"group_wait"`
	GroupInterval  string            `json:"group_interval"`
	RepeatInterval string            `json:"repeat_interval"`
	ParentID       *uint             `json:"parent_id"`
	Enabled        *bool             `json:"enabled"`
}

type alertmanagerRouteResponse struct {
	ID             uint              `json:"id"`
	Name           string            `json:"name"`
	Receiver       string            `json:"receiver"`
	Match          map[string]string `json:"match"`
	MatchRE        map[string]string `json:"match_re"`
	GroupBy        []string          `json:"group_by"`
	GroupWait      string            `json:"group_wait"`
	GroupInterval  string            `json:"group_interval"`
	RepeatInterval string            `json:"repeat_interval"`
	ParentID       *uint             `json:"parent_id"`
	Enabled        bool              `json:"enabled"`
	CreatedAt      time.Time         `json:"created_at"`
	UpdatedAt      time.Time         `json:"updated_at"`
}

func (h *AlertmanagerSettingsHandler) nodeID(c *gin.Context) (uint, bool) {
	id, err := services.SingleNodeID(h.db)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取节点配置失败，请刷新页面后重试"})
		return 0, false
	}
	return id, true
}

func (h *AlertmanagerSettingsHandler) sync(clusterID uint) error {
	if h.autoConfig == nil {
		return fmt.Errorf("Alertmanager 配置服务不可用，请稍后重试")
	}
	if _, err := h.autoConfig.FindAlertmanagerComponent(clusterID); err != nil {
		return fmt.Errorf("未找到运行中的 Alertmanager，请先启动组件后重试")
	}
	return h.autoConfig.SyncAlertmanagerConfig(clusterID, h.basePath)
}

func (h *AlertmanagerSettingsHandler) ListAlertmanagerRoutes(c *gin.Context) {
	clusterID, ok := h.nodeID(c)
	if !ok {
		return
	}
	var routes []models.AlertmanagerRoute
	if err := h.db.Where("cluster_id = ?", clusterID).Order("created_at ASC").Find(&routes).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取路由失败，请稍后重试"})
		return
	}
	result := make([]alertmanagerRouteResponse, 0, len(routes))
	for _, route := range routes {
		result = append(result, routeResponse(route))
	}
	c.JSON(http.StatusOK, gin.H{"routes": result})
}

func (h *AlertmanagerSettingsHandler) CreateAlertmanagerRoute(c *gin.Context) {
	clusterID, ok := h.nodeID(c)
	if !ok {
		return
	}
	var req alertmanagerRouteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "路由请求无效，请检查 JSON 字段后重试"})
		return
	}
	normalizedReceiver, err := h.normalizeReceiver(clusterID, req.Receiver)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "操作失败: " + err.Error()})
		return
	}
	req.Receiver = normalizedReceiver
	if err := h.validateRoute(clusterID, 0, req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "操作失败: " + err.Error()})
		return
	}
	route, err := requestRoute(clusterID, req)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "路由字段无法编码，请检查后重试"})
		return
	}
	if err := h.db.Create(&route).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建路由失败，请稍后重试"})
		return
	}
	if err := h.sync(clusterID); err != nil {
		if rollbackErr := h.db.Delete(&route).Error; rollbackErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "路由发布失败且回滚失败: " + rollbackErr.Error()})
			return
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": "路由发布失败，已撤销变更；请检查 Alertmanager 状态后重试"})
		return
	}
	c.JSON(http.StatusCreated, routeResponse(route))
}

func (h *AlertmanagerSettingsHandler) UpdateAlertmanagerRoute(c *gin.Context) {
	clusterID, ok := h.nodeID(c)
	if !ok {
		return
	}
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "路由 ID 无效，请刷新列表后重试"})
		return
	}
	var previous models.AlertmanagerRoute
	if err := h.db.Where("cluster_id = ? AND id = ?", clusterID, uint(id)).First(&previous).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "未找到该路由，请刷新列表"})
		return
	}
	var req alertmanagerRouteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "路由请求无效，请检查 JSON 字段后重试"})
		return
	}
	normalizedReceiver, err := h.normalizeReceiver(clusterID, req.Receiver)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "操作失败: " + err.Error()})
		return
	}
	req.Receiver = normalizedReceiver
	if err := h.validateRoute(clusterID, previous.ID, req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "操作失败: " + err.Error()})
		return
	}
	next, err := requestRoute(clusterID, req)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "路由字段无法编码，请检查后重试"})
		return
	}
	next.ID, next.CreatedAt = previous.ID, previous.CreatedAt
	if err := h.db.Model(&models.AlertmanagerRoute{}).Where("id = ?", previous.ID).Updates(map[string]interface{}{
		"name": next.Name, "receiver": next.Receiver, "match": next.Match, "match_re": next.MatchRE,
		"group_by": next.GroupBy, "group_wait": next.GroupWait, "group_interval": next.GroupInterval,
		"repeat_interval": next.RepeatInterval, "parent_id": next.ParentID, "enabled": next.Enabled,
	}).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "更新路由失败，请稍后重试"})
		return
	}
	if err := h.sync(clusterID); err != nil {
		if rollbackErr := h.db.Save(&previous).Error; rollbackErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "路由发布失败且回滚失败: " + rollbackErr.Error()})
			return
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": "路由发布失败，已恢复旧设置；请检查 Alertmanager 状态后重试"})
		return
	}
	_ = h.db.First(&next, previous.ID).Error
	c.JSON(http.StatusOK, routeResponse(next))
}

func (h *AlertmanagerSettingsHandler) DeleteAlertmanagerRoute(c *gin.Context) {
	clusterID, ok := h.nodeID(c)
	if !ok {
		return
	}
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "路由 ID 无效，请刷新列表后重试"})
		return
	}
	var route models.AlertmanagerRoute
	if err := h.db.Where("cluster_id = ? AND id = ?", clusterID, uint(id)).First(&route).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "未找到该路由，请刷新列表"})
		return
	}
	var childCount int64
	h.db.Model(&models.AlertmanagerRoute{}).Where("cluster_id = ? AND parent_id = ?", clusterID, route.ID).Count(&childCount)
	if childCount > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "该路由仍有子路由，请先删除或移动子路由"})
		return
	}
	if err := h.db.Delete(&route).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "删除路由失败，请稍后重试"})
		return
	}
	if err := h.sync(clusterID); err != nil {
		route.ID = uint(id)
		if rollbackErr := h.db.Create(&route).Error; rollbackErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "路由发布失败且恢复失败: " + rollbackErr.Error()})
			return
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": "路由发布失败，已恢复原路由；请检查 Alertmanager 状态后重试"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "路由已删除"})
}

type alertmanagerSettingsRequest struct {
	ResolveTimeout string               `json:"resolve_timeout"`
	InhibitRules   []config.InhibitRule `json:"inhibit_rules"`
	Templates      []string             `json:"templates"`
}

func (h *AlertmanagerSettingsHandler) GetAlertmanagerSettings(c *gin.Context) {
	clusterID, ok := h.nodeID(c)
	if !ok {
		return
	}
	var settings models.AlertmanagerSettings
	err := h.db.Where("cluster_id = ?", clusterID).First(&settings).Error
	if err != nil && err != gorm.ErrRecordNotFound {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取 Alertmanager 设置失败，请稍后重试"})
		return
	}
	response := alertmanagerSettingsRequest{ResolveTimeout: "5m", InhibitRules: []config.InhibitRule{}, Templates: []string{}}
	if err == nil {
		if settings.ResolveTimeout != "" {
			response.ResolveTimeout = settings.ResolveTimeout
		}
		if err := json.Unmarshal([]byte(settings.InhibitRules), &response.InhibitRules); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "抑制规则存储格式无效，请重新保存 Alertmanager 设置"})
			return
		}
		if err := json.Unmarshal([]byte(settings.Templates), &response.Templates); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "模板路径存储格式无效，请重新保存 Alertmanager 设置"})
			return
		}
		if response.InhibitRules == nil {
			response.InhibitRules = []config.InhibitRule{}
		}
		if response.Templates == nil {
			response.Templates = []string{}
		}
	}
	c.JSON(http.StatusOK, response)
}

func (h *AlertmanagerSettingsHandler) UpdateAlertmanagerSettings(c *gin.Context) {
	clusterID, ok := h.nodeID(c)
	if !ok {
		return
	}
	var req alertmanagerSettingsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "设置请求无效，请检查 JSON 字段后重试"})
		return
	}
	if err := validateSettings(req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "操作失败: " + err.Error()})
		return
	}
	if req.InhibitRules == nil {
		req.InhibitRules = []config.InhibitRule{}
	}
	if req.Templates == nil {
		req.Templates = []string{}
	}
	inhibitJSON, _ := json.Marshal(req.InhibitRules)
	templatesJSON, _ := json.Marshal(req.Templates)
	var previous models.AlertmanagerSettings
	findErr := h.db.Where("cluster_id = ?", clusterID).First(&previous).Error
	next := models.AlertmanagerSettings{ClusterID: clusterID, ResolveTimeout: req.ResolveTimeout, InhibitRules: string(inhibitJSON), Templates: string(templatesJSON)}
	if findErr == nil {
		next.ID, next.CreatedAt = previous.ID, previous.CreatedAt
	}
	if findErr != nil && findErr != gorm.ErrRecordNotFound {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取原设置失败，请稍后重试"})
		return
	}
	if err := h.db.Save(&next).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存设置失败，请稍后重试"})
		return
	}
	if err := h.sync(clusterID); err != nil {
		if findErr == nil {
			_ = h.db.Save(&previous).Error
		} else {
			_ = h.db.Delete(&next).Error
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": "设置发布失败，已恢复旧配置；请检查模板路径和 Alertmanager 状态后重试"})
		return
	}
	c.JSON(http.StatusOK, req)
}

func (h *AlertmanagerSettingsHandler) normalizeReceiver(clusterID uint, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	var channels []models.NotificationChannel
	if err := h.db.Where("enabled = ? AND type <> ? AND (cluster_id = ? OR cluster_id IS NULL)", true, "dingtalk", clusterID).Find(&channels).Error; err != nil {
		return "", fmt.Errorf("读取通知渠道失败，请稍后重试")
	}
	for _, channel := range channels {
		normalized := alertmanagerReceiverName(channel.Name)
		if requested == channel.Name || requested == normalized {
			return normalized, nil
		}
	}
	return "", fmt.Errorf("接收器不存在或未启用，请先配置通知渠道")
}

func (h *AlertmanagerSettingsHandler) validateRoute(clusterID, routeID uint, req alertmanagerRouteRequest) error {
	if strings.TrimSpace(req.Name) == "" || len(req.Name) > 128 {
		return fmt.Errorf("路由名称不能为空且不得超过 128 个字符，请修正后重试")
	}
	if !alertReceiverPattern.MatchString(req.Receiver) {
		return fmt.Errorf("接收器名称无效，请选择现有通知渠道")
	}
	var channels []models.NotificationChannel
	if err := h.db.Where("enabled = ? AND type <> ? AND (cluster_id = ? OR cluster_id IS NULL)", true, "dingtalk", clusterID).Find(&channels).Error; err != nil {
		return fmt.Errorf("读取通知渠道失败，请稍后重试")
	}
	foundReceiver := false
	for _, channel := range channels {
		if alertmanagerReceiverName(channel.Name) == req.Receiver {
			foundReceiver = true
			break
		}
	}
	if !foundReceiver {
		return fmt.Errorf("接收器不存在或未启用，请先配置通知渠道")
	}
	if err := validateMatchers(req.Match, false); err != nil {
		return err
	}
	if err := validateMatchers(req.MatchRE, true); err != nil {
		return err
	}
	for _, label := range req.GroupBy {
		if !alertLabelNamePattern.MatchString(label) || label == "cluster_id" {
			return fmt.Errorf("group_by 标签 %q 无效，请使用合法标签名且不要使用 cluster_id", label)
		}
	}
	for name, duration := range map[string]string{"group_wait": req.GroupWait, "group_interval": req.GroupInterval, "repeat_interval": req.RepeatInterval} {
		if duration != "" {
			parsed, err := time.ParseDuration(duration)
			if err != nil || parsed <= 0 {
				return fmt.Errorf("%s 时长无效，请使用 30s、5m 或 1h 等格式", name)
			}
		}
	}
	if req.ParentID != nil {
		if *req.ParentID == routeID && routeID != 0 {
			return fmt.Errorf("路由不能将自身设为父路由，请重新选择")
		}
		var parent models.AlertmanagerRoute
		if err := h.db.Where("cluster_id = ? AND id = ?", clusterID, *req.ParentID).First(&parent).Error; err != nil {
			return fmt.Errorf("父路由不存在，请刷新列表后重试")
		}
		for parent.ParentID != nil {
			if *parent.ParentID == routeID && routeID != 0 {
				return fmt.Errorf("父路由会形成循环，请重新选择")
			}
			if err := h.db.Where("cluster_id = ? AND id = ?", clusterID, *parent.ParentID).First(&parent).Error; err != nil {
				return fmt.Errorf("父路由链无效，请修复路由结构后重试")
			}
		}
	}
	return nil
}

func validateSettings(req alertmanagerSettingsRequest) error {
	parsed, err := time.ParseDuration(req.ResolveTimeout)
	if err != nil || parsed <= 0 {
		return fmt.Errorf("resolve_timeout 无效，请使用 5m、1h 等正时长")
	}
	for i, rule := range req.InhibitRules {
		if len(rule.SourceMatch)+len(rule.SourceMatchRE) == 0 || len(rule.TargetMatch)+len(rule.TargetMatchRE) == 0 {
			return fmt.Errorf("第 %d 条抑制规则必须同时配置来源和目标 matcher", i+1)
		}
		for _, item := range []struct {
			values map[string]string
			regex  bool
		}{{rule.SourceMatch, false}, {rule.SourceMatchRE, true}, {rule.TargetMatch, false}, {rule.TargetMatchRE, true}} {
			if err := validateMatchers(item.values, item.regex); err != nil {
				return fmt.Errorf("第 %d 条抑制规则无效: %w", i+1, err)
			}
		}
		for _, label := range rule.Equal {
			if !alertLabelNamePattern.MatchString(label) || label == "cluster_id" {
				return fmt.Errorf("第 %d 条抑制规则的 equal 标签 %q 无效", i+1, label)
			}
		}
	}
	for _, templatePath := range req.Templates {
		if err := validateTemplatePath(templatePath); err != nil {
			return err
		}
	}
	return nil
}

func validateMatchers(matchers map[string]string, regexValues bool) error {
	for name, value := range matchers {
		if !alertLabelNamePattern.MatchString(name) || name == "cluster_id" {
			return fmt.Errorf("matcher 标签 %q 无效，请使用合法标签名且不要使用 cluster_id", name)
		}
		if strings.ContainsAny(value, "\r\n\x00") {
			return fmt.Errorf("matcher %q 的值包含非法控制字符，请修正后重试", name)
		}
		if regexValues {
			if _, err := regexp.Compile(value); err != nil {
				return fmt.Errorf("matcher %q 的正则表达式无效，请修正后重试", name)
			}
		}
	}
	return nil
}

func validateTemplatePath(path string) error {
	if path == "" || strings.ContainsAny(path, "\r\n\x00") || !filepath.IsAbs(path) {
		return fmt.Errorf("模板路径 %q 无效，请使用 /etc/alertmanager/templates 下的绝对 .tmpl 路径", path)
	}
	clean := filepath.Clean(path)
	rel, err := filepath.Rel("/etc/alertmanager/templates", clean)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.Ext(clean) != ".tmpl" {
		return fmt.Errorf("模板路径 %q 无效，请使用 /etc/alertmanager/templates 下的 .tmpl 文件或通配模式", path)
	}
	return nil
}

func requestRoute(clusterID uint, req alertmanagerRouteRequest) (models.AlertmanagerRoute, error) {
	if req.Match == nil {
		req.Match = map[string]string{}
	}
	if req.MatchRE == nil {
		req.MatchRE = map[string]string{}
	}
	if req.GroupBy == nil {
		req.GroupBy = []string{}
	}
	matchJSON, err := json.Marshal(req.Match)
	if err != nil {
		return models.AlertmanagerRoute{}, err
	}
	matchREJSON, err := json.Marshal(req.MatchRE)
	if err != nil {
		return models.AlertmanagerRoute{}, err
	}
	groupByJSON, err := json.Marshal(req.GroupBy)
	if err != nil {
		return models.AlertmanagerRoute{}, err
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	return models.AlertmanagerRoute{
		ClusterID: clusterID, Name: strings.TrimSpace(req.Name), Receiver: req.Receiver,
		Match: string(matchJSON), MatchRE: string(matchREJSON), GroupBy: string(groupByJSON),
		GroupWait: req.GroupWait, GroupInterval: req.GroupInterval, RepeatInterval: req.RepeatInterval,
		ParentID: req.ParentID, Enabled: enabled,
	}, nil
}

func routeResponse(route models.AlertmanagerRoute) alertmanagerRouteResponse {
	response := alertmanagerRouteResponse{
		ID: route.ID, Name: route.Name, Receiver: route.Receiver, GroupWait: route.GroupWait,
		GroupInterval: route.GroupInterval, RepeatInterval: route.RepeatInterval, ParentID: route.ParentID,
		Enabled: route.Enabled, CreatedAt: route.CreatedAt, UpdatedAt: route.UpdatedAt,
		Match: map[string]string{}, MatchRE: map[string]string{}, GroupBy: []string{},
	}
	_ = json.Unmarshal([]byte(route.Match), &response.Match)
	_ = json.Unmarshal([]byte(route.MatchRE), &response.MatchRE)
	_ = json.Unmarshal([]byte(route.GroupBy), &response.GroupBy)
	if response.Match == nil {
		response.Match = map[string]string{}
	}
	if response.MatchRE == nil {
		response.MatchRE = map[string]string{}
	}
	if response.GroupBy == nil {
		response.GroupBy = []string{}
	}
	return response
}

func alertmanagerReceiverName(name string) string {
	var builder strings.Builder
	for i, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			builder.WriteRune(r)
		} else if i > 0 {
			builder.WriteByte('_')
		}
	}
	result := builder.String()
	if result == "" {
		result = "receiver"
	}
	if result[0] >= '0' && result[0] <= '9' {
		result = "r_" + result
	}
	return result
}
