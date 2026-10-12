package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/promlifecycle/platform/core/ssh"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"gopkg.in/yaml.v3"
	"gorm.io/gorm"
)

func (ac *AutoConfig) GenerateAlertmanagerConfig(clusterID uint) (*AlertmanagerConfig, error) {
	clusterID, err := ac.resolveClusterScope(clusterID)
	if err != nil {
		return nil, err
	}
	resolveTimeout, groupWait, groupInterval, repeatInterval := AlertmanagerConfigIntervals("")
	var alertmanagerComponent models.Component
	if findErr := ac.db.Where("cluster_id = ? AND type = ? AND status = ?", clusterID, models.ComponentAlertmanager, models.StatusRunning).
		First(&alertmanagerComponent).Error; findErr == nil {
		resolveTimeout, groupWait, groupInterval, repeatInterval = AlertmanagerConfigIntervals(alertmanagerComponent.Config)
	}
	var channels []models.NotificationChannel

	// Query channels for this cluster, or global channels if ClusterID is nil
	query := ac.db.Where("enabled = ?", true)
	if clusterID > 0 {
		query = query.Where("(cluster_id = ? OR cluster_id IS NULL)", clusterID)
	}

	if err := query.Find(&channels).Error; err != nil {
		return nil, fmt.Errorf("读取通知渠道失败，请检查数据库后重试: %w", err)
	}

	receivers := make([]Receiver, 0, len(channels))
	var smtpSettings *SMTPSettings
	channelReceivers := make(map[string]string, len(channels))
	channelOrder := make([]string, 0, len(channels))
	receiverNames := make(map[string]struct{}, len(channels))

	for _, ch := range channels {
		receiverName := sanitizeReceiverName(ch.Name)
		receiver := Receiver{Name: receiverName}

		switch ch.Type {
		case "email":
			settings, to, err := ResolveEmailChannel(ch)
			if err != nil {
				return nil, fmt.Errorf("邮件渠道 %q 无法生成配置，请修正渠道或 SMTP 设置: %w", ch.Name, err)
			}
			smtpSettings = &settings
			receiver.EmailConfigs = []EmailConfig{{To: to}}

		case "slack":
			var sc SlackConfig
			if ch.Config != "" {
				var raw map[string]string
				if err := utils.JSONBytesToMap([]byte(ch.Config), &raw); err == nil {
					sc.Channel = raw["channel"]
					sc.Title = raw["title"]
					sc.Text = raw["text"]
					sc.Username = raw["username"]
					sc.IconEmoji = raw["icon_emoji"]
				}
			}
			endpoint, err := DirectNotificationEndpoint(ch)
			if err != nil {
				return nil, fmt.Errorf("Slack 渠道 %q: %w", ch.Name, err)
			}
			sc.APIURL = endpoint
			receiver.SlackConfigs = []SlackConfig{sc}

		case "webhook":
			endpoint, err := DirectNotificationEndpoint(ch)
			if err != nil {
				return nil, fmt.Errorf("Webhook 渠道 %q: %w", ch.Name, err)
			}
			receiver.WebhookConfigs = []WebhookConfig{{URL: endpoint}}

		case "wechat":
			var wc WeChatConfig
			if ch.Config != "" {
				var raw map[string]string
				if err := utils.JSONBytesToMap([]byte(ch.Config), &raw); err == nil {
					wc.CorpID = raw["corp_id"]
					wc.ToUser = raw["to_user"]
					wc.ToParty = raw["to_party"]
					wc.ToTag = raw["to_tag"]
					wc.AgentID = raw["agent_id"]
					wc.MsgType = raw["msg_type"]
					wc.Content = raw["content"]
					wc.Title = raw["title"]
				}
			}
			wc.APISecret = ch.Secret
			receiver.WeChatConfigs = []WeChatConfig{wc}

		case "dingtalk":
			// Standard Alertmanager has no DingTalk receiver. This channel is
			// explicitly direct-inspection-only; adapters use type webhook.
			continue

		default:
			return nil, fmt.Errorf("不支持的通知渠道类型: %s", ch.Type)
		}

		if _, exists := receiverNames[receiverName]; exists {
			return nil, fmt.Errorf("通知渠道名称 %q 与其他渠道生成相同接收器名，请重命名后重试", ch.Name)
		}
		receiverNames[receiverName] = struct{}{}
		receivers = append(receivers, receiver)
		channelReceivers[ch.Name] = receiverName
		channelOrder = append(channelOrder, ch.Name)
	}

	// Ensure at least a default receiver
	if len(receivers) == 0 {
		receivers = []Receiver{{Name: "default-receiver"}}
	}

	// Build route with custom and auto-generated sub-routes
	route := AlertmanagerRoute{
		Receiver:       receivers[0].Name,
		GroupBy:        []string{"alertname"},
		GroupWait:      groupWait,
		GroupInterval:  groupInterval,
		RepeatInterval: repeatInterval,
	}

	// Query for custom routes defined via AlertmanagerRoute model
	var customRoutes []models.AlertmanagerRoute
	ac.db.Where("cluster_id = ? AND enabled = ?", clusterID, true).
		Where("parent_id IS NULL").
		Order("created_at ASC").
		Find(&customRoutes)

	subRoutes := make([]AlertmanagerRoute, 0, len(channelReceivers)+2)
	for _, channelName := range channelOrder {
		subRoutes = append(subRoutes, AlertmanagerRoute{
			Match:    map[string]string{"channel": channelName},
			Receiver: channelReceivers[channelName],
		})
	}
	if len(customRoutes) > 0 {
		// Use custom routes
		for _, cr := range customRoutes {
			subRoute := ac.buildCustomRoute(cr, clusterID)
			subRoutes = append(subRoutes, subRoute)
		}
	} else {
		// Auto-generate severity-based routing
		criticalReceiver := receivers[0].Name
		warningReceiver := receivers[0].Name
		for _, r := range receivers {
			lower := strings.ToLower(r.Name)
			if strings.Contains(lower, "critical") {
				criticalReceiver = r.Name
			}
			if strings.Contains(lower, "warning") {
				warningReceiver = r.Name
			}
		}

		subRoutes = append(subRoutes,
			AlertmanagerRoute{
				Match:    map[string]string{"severity": "critical"},
				Receiver: criticalReceiver,
			},
			AlertmanagerRoute{
				Match:    map[string]string{"severity": "warning"},
				Receiver: warningReceiver,
			},
		)
	}

	route.Routes = subRoutes
	if smtpSettings == nil {
		var err error
		smtpSettings, err = OptionalSMTPSettingsFromEnv()
		if err != nil {
			return nil, fmt.Errorf("SMTP 环境配置无效，请修正后重试: %w", err)
		}
	}

	global := AlertmanagerGlobal{ResolveTimeout: resolveTimeout}
	if smtpSettings != nil {
		requireTLS := smtpSettings.RequireTLS
		global.SMTPFrom = smtpSettings.From
		global.SMTPSmarthost = smtpSettings.Smarthost()
		global.SMTPAuthUsername = smtpSettings.Username
		global.SMTPAuthPassword = smtpSettings.Password
		global.SMTPRequireTLS = &requireTLS
	}

	var inhibitRules []InhibitRule
	var templates []string
	var settings models.AlertmanagerSettings
	err = ac.db.Where("cluster_id = ?", clusterID).Limit(1).Find(&settings).Error
	if err == nil && settings.ID != 0 {
		if settings.ResolveTimeout != "" {
			global.ResolveTimeout = settings.ResolveTimeout
		}
		if settings.InhibitRules != "" {
			if err := json.Unmarshal([]byte(settings.InhibitRules), &inhibitRules); err != nil {
				return nil, fmt.Errorf("抑制规则存储格式无效，请在 Alertmanager 设置中重新保存: %w", err)
			}
		}
		if settings.Templates != "" {
			if err := json.Unmarshal([]byte(settings.Templates), &templates); err != nil {
				return nil, fmt.Errorf("模板路径存储格式无效，请在 Alertmanager 设置中重新保存: %w", err)
			}
		}
	} else if err != nil {
		return nil, fmt.Errorf("读取 Alertmanager 设置失败，请检查数据库后重试: %w", err)
	}

	amCfg := &AlertmanagerConfig{
		Global:       global,
		Route:        route,
		Receivers:    receivers,
		InhibitRules: inhibitRules,
		Templates:    templates,
	}

	return amCfg, nil
}

// buildCustomRoute converts a database AlertmanagerRoute model to a YAML AlertmanagerRoute struct
func (ac *AutoConfig) buildCustomRoute(cr models.AlertmanagerRoute, clusterID uint) AlertmanagerRoute {
	route := AlertmanagerRoute{
		Receiver:       cr.Receiver,
		GroupWait:      cr.GroupWait,
		GroupInterval:  cr.GroupInterval,
		RepeatInterval: cr.RepeatInterval,
	}

	// Parse match map
	if cr.Match != "" {
		var matchMap map[string]string
		if err := json.Unmarshal([]byte(cr.Match), &matchMap); err == nil {
			route.Match = matchMap
		}
	}

	// Parse match_re map
	if cr.MatchRE != "" {
		var matchREMap map[string]string
		if err := json.Unmarshal([]byte(cr.MatchRE), &matchREMap); err == nil {
			route.MatchRE = matchREMap
		}
	}

	// Parse group_by array
	if cr.GroupBy != "" {
		var groupBy []string
		if err := json.Unmarshal([]byte(cr.GroupBy), &groupBy); err == nil {
			route.GroupBy = groupBy
		}
	}

	// Build nested child routes
	var childRoutes []models.AlertmanagerRoute
	if cr.ParentID != nil {
		ac.db.Where("cluster_id = ? AND enabled = ? AND parent_id = ?", clusterID, true, cr.ID).
			Find(&childRoutes)
	} else {
		ac.db.Where("cluster_id = ? AND enabled = ? AND parent_id = ?", clusterID, true, cr.ID).
			Find(&childRoutes)
	}

	if len(childRoutes) > 0 {
		route.Routes = make([]AlertmanagerRoute, 0, len(childRoutes))
		for _, child := range childRoutes {
			route.Routes = append(route.Routes, ac.buildCustomRoute(child, clusterID))
		}
	}

	return route
}

// SyncAlertmanagerConfig generates alertmanager.yml from NotificationChannel records and reloads Alertmanager.
func (ac *AutoConfig) SyncAlertmanagerConfig(clusterID uint, basePath string) error {
	clusterID, err := ac.resolveClusterScope(clusterID)
	if err != nil {
		return err
	}
	amComp, err := ac.FindAlertmanagerComponent(clusterID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// No alertmanager in this cluster — skip sync silently (not an error).
			return nil
		}
		log.Printf("[AutoConfig] Skipping alertmanager config sync for cluster %d: %v", clusterID, err)
		return err
	}
	// Imported and manually saved configuration belongs to the operator until
	// the existing best-practice action explicitly releases manual ownership.
	var manualCount int64
	if err := ac.db.Model(&models.ComponentConfig{}).Where("component_id = ? AND name = ? AND is_active = ?",
		amComp.ID, "alertmanager.yml", true).Count(&manualCount).Error; err != nil {
		return err
	}
	if manualCount > 0 {
		return fmt.Errorf("alertmanager.yml 由手动配置管理；请明确应用推荐配置后再启用自动同步")
	}
	log.Printf("[AutoConfig] Syncing alertmanager config for cluster %d using alertmanager %s (host: %s)", clusterID, amComp.Name, amComp.Host.IP)
	// Generate config
	amCfg, err := ac.GenerateAlertmanagerConfig(clusterID)
	if err != nil {
		log.Printf("[AutoConfig] Alertmanager generation failed: %v", err)
		return fmt.Errorf("Alertmanager 配置生成失败，请检查渠道、路由和模板设置后重试")
	}

	// Marshal to YAML
	data, err := yaml.Marshal(amCfg)
	if err != nil {
		return fmt.Errorf("生成 Alertmanager YAML 失败，请修正配置后重试: %w", err)
	}

	configPath := filepath.Join(basePath, "clusters", fmt.Sprintf("%d", clusterID), "alertmanager", "alertmanager.yml")
	validation, err := PublishValidatedLocal(clusterID, "alertmanager", configPath, data, 0644, func() error {
		return ac.publishComponentFile(amComp, "alertmanager.yml", data, 0644, func() error {
			return ac.reloadAlertmanager(clusterID)
		})
	})
	if err != nil {
		log.Printf("[AutoConfig] Alertmanager publish failed: %v", err)
		return fmt.Errorf("Alertmanager 配置发布失败，请检查渠道、路由、模板和组件状态后重试")
	}
	if validation.Level == "partial" {
		log.Printf("[AutoConfig] Alertmanager config validation partial: %s", validation.Message)
	}
	log.Printf("[AutoConfig] Published alertmanager config for node with %d receivers -> %s", len(amCfg.Receivers), configPath)
	return nil
}

// publishComponentFile atomically updates the component's live file and
// restores its previous contents when reload fails.
func (ac *AutoConfig) publishComponentFile(comp *models.Component, relName string, data []byte, mode os.FileMode, reload func() error) error {
	if comp.ConfigPath == "" {
		return reload()
	}
	dst := comp.ConfigPath
	if info, err := os.Stat(dst); err == nil && info.IsDir() {
		dst = filepath.Join(dst, relName)
	}

	if comp.Host.IP == "127.0.0.1" || comp.Host.IP == "localhost" || comp.Host.ConnectionType == "local" {
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			return fmt.Errorf("创建组件配置目录失败，请检查权限: %w", err)
		}
		old, readErr := os.ReadFile(dst)
		hadOld := readErr == nil
		if err := utils.AtomicWriteFile(dst, data, mode); err != nil {
			return fmt.Errorf("原子写入组件配置失败，线上配置未变更: %w", err)
		}
		if err := reload(); err != nil {
			if hadOld {
				if restoreErr := utils.AtomicWriteFile(dst, old, mode); restoreErr != nil {
					return fmt.Errorf("组件重载失败且旧配置恢复失败，请立即检查服务: %v（恢复错误: %w）", err, restoreErr)
				}
			} else {
				_ = os.Remove(dst)
			}
			_ = reload()
			return fmt.Errorf("组件重载失败，已恢复旧配置；请检查组件状态后重试: %w", err)
		}
		return nil
	}

	client, err := ssh.NewClient(comp.Host.IP, comp.Host.Port, comp.Host.Username, comp.Host.Password, comp.Host.PrivateKey)
	if err != nil {
		return fmt.Errorf("连接配置目标失败，请检查 SSH 设置和网络: %w", err)
	}
	defer client.Close()
	backup := dst + fmt.Sprintf(".promlifecycle-backup-%d", time.Now().UnixNano())
	prepare := fmt.Sprintf("mkdir -p %s && if [ -f %s ]; then cp -p %s %s; fi",
		utils.QuoteShell(filepath.Dir(dst)), utils.QuoteShell(dst), utils.QuoteShell(dst), utils.QuoteShell(backup))
	if _, _, err := client.RunCommand(prepare); err != nil {
		return fmt.Errorf("准备远程临时配置失败，请检查目录权限: %w", err)
	}
	if err := client.CopyFile(bytes.NewReader(data), dst, int64(mode), int64(len(data))); err != nil {
		_, _, _ = client.RunCommand("rm -f " + utils.QuoteShell(backup))
		return fmt.Errorf("原子上传远程配置失败，线上配置未变更: %w", err)
	}
	if err := reload(); err != nil {
		restore := fmt.Sprintf("if [ -f %s ]; then mv -f %s %s; else rm -f %s; fi",
			utils.QuoteShell(backup), utils.QuoteShell(backup), utils.QuoteShell(dst), utils.QuoteShell(dst))
		if _, _, restoreErr := client.RunCommand(restore); restoreErr != nil {
			return fmt.Errorf("组件重载失败且远程旧配置恢复失败，请立即检查服务: %v（恢复错误: %w）", err, restoreErr)
		}
		_ = reload()
		return fmt.Errorf("组件重载失败，已恢复远程旧配置；请检查组件状态后重试: %w", err)
	}
	_, _, _ = client.RunCommand("rm -f " + utils.QuoteShell(backup))
	return nil
}

// ValidateAlertmanagerConfig validates the generated alertmanager config before applying.
// Returns nil if valid, error otherwise.
func (ac *AutoConfig) ValidateAlertmanagerConfig(clusterID uint) error {
	amCfg, err := ac.GenerateAlertmanagerConfig(clusterID)
	if err != nil {
		return err
	}

	// Basic validation: ensure route and receivers exist
	if amCfg.Route.Receiver == "" {
		return fmt.Errorf("Alertmanager 路由缺少接收器，请配置接收器后重试")
	}

	if len(amCfg.Receivers) == 0 {
		return fmt.Errorf("Alertmanager 没有可用接收器，请先配置通知渠道")
	}

	// Verify route receiver exists in receivers
	receiverNames := make(map[string]bool)
	for _, r := range amCfg.Receivers {
		receiverNames[r.Name] = true
	}

	// Check all referenced receivers exist
	if err := ac.validateRouteReceivers(&amCfg.Route, receiverNames); err != nil {
		return err
	}

	// Try to marshal to YAML - if this fails, config is invalid
	if _, err := yaml.Marshal(amCfg); err != nil {
		return fmt.Errorf("生成 Alertmanager YAML 失败，请修正配置后重试: %w", err)
	}

	return nil
}

// validateRouteReceivers recursively checks that all receivers referenced in routes exist
func (ac *AutoConfig) validateRouteReceivers(route *AlertmanagerRoute, validReceivers map[string]bool) error {
	if route.Receiver != "" && !validReceivers[route.Receiver] {
		return fmt.Errorf("路由引用了不存在的接收器 %q，请重新选择通知渠道", route.Receiver)
	}
	for i := range route.Routes {
		if err := ac.validateRouteReceivers(&route.Routes[i], validReceivers); err != nil {
			return err
		}
	}
	return nil
}

// ValidatePrometheusConfig validates the generated prometheus config before applying.
func (ac *AutoConfig) ValidatePrometheusConfig(clusterID uint) error {
	// Generate scrape configs and check basic validity
	scrapeConfigs, err := ac.GenerateScrapeConfigs(clusterID)
	if err != nil {
		return fmt.Errorf("生成抓取配置失败，请修正组件或抓取设置后重试: %w", err)
	}

	// Basic validation: at least one scrape config should exist
	if len(scrapeConfigs) == 0 {
		log.Printf("[AutoConfig] Warning: no scrape configs for cluster %d", clusterID)
		// Not an error - Prometheus can run without scrape targets
	}

	// Validate each scrape config has required fields
	for i, sc := range scrapeConfigs {
		if sc.JobName == "" {
			return fmt.Errorf("scrape config[%d] missing job_name", i)
		}
	}

	// If Prometheus is running, optionally validate config format
	endpoint, err := ac.GetPrometheusEndpoint(clusterID)
	if err == nil {
		// Use Prometheus config validation via /api/v1/status/config
		url := endpoint + "/api/v1/status/config"
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Get(url)
		if err != nil {
			log.Printf("[AutoConfig] Could not validate prometheus config: %v", err)
		} else {
			defer resp.Body.Close()
			if resp.StatusCode >= 400 {
				return fmt.Errorf("Prometheus 配置状态返回 HTTP %d，请检查组件日志后重试", resp.StatusCode)
			}
		}
	}

	return nil
}

// ProxyAlertmanagerSilence forwards silence-related requests to Alertmanager
func (ac *AutoConfig) ProxyAlertmanagerSilence(clusterID uint, method, path string, body []byte) (int, []byte, error) {
	endpoint, err := ac.GetAlertmanagerEndpoint(clusterID)
	if err != nil {
		return 0, nil, fmt.Errorf("未找到运行中的 Alertmanager，请先启动组件后重试: %w", err)
	}

	url := endpoint + path
	client := &http.Client{Timeout: 15 * time.Second}

	var req *http.Request
	if body != nil && (method == "POST" || method == "PUT") {
		req, _ = http.NewRequest(method, url, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req, _ = http.NewRequest(method, url, nil)
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}

	return resp.StatusCode, respBody, nil
}

// ReloadPrometheusComponent asks a Prometheus component to reload its config.
func ReloadPrometheusComponent(comp *models.Component) error {
	return reloadComponent(comp, 9090, "Prometheus")
}

// ReloadAlertmanagerComponent asks an Alertmanager component to reload its config.
func ReloadAlertmanagerComponent(comp *models.Component) error {
	return reloadComponent(comp, 9093, "Alertmanager")
}

func reloadComponent(comp *models.Component, defaultPort int, componentName string) error {
	if comp == nil {
		return fmt.Errorf("%s 组件不存在，请刷新页面后重试", componentName)
	}
	endpoint := ""
	if comp.ExternalURL != "" {
		endpoint = strings.TrimSuffix(comp.ExternalURL, "/")
	} else if comp.InternalURL != "" {
		endpoint = strings.TrimSuffix(comp.InternalURL, "/")
	} else {
		port := comp.Port
		if port == 0 {
			port = defaultPort
		}
		hostIP := comp.Host.IP
		if hostIP == "" {
			hostIP = "127.0.0.1"
		}
		endpoint = fmt.Sprintf("http://%s:%d", hostIP, port)
	}
	if comp.ExternalURL == "" {
		endpoint += models.ManagedRoutePrefix(comp)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Post(endpoint+"/-/reload", "", nil)
	if err != nil {
		return fmt.Errorf("%s 重载请求失败，请检查组件状态和网络后重试: %w", componentName, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s 重载返回 HTTP %d，请检查组件日志后重试", componentName, resp.StatusCode)
	}
	return nil
}

func (ac *AutoConfig) reloadAlertmanager(clusterID uint) error {
	comp, err := ac.FindAlertmanagerComponent(clusterID)
	if err != nil {
		return err
	}
	return ReloadAlertmanagerComponent(comp)
}

// sanitizeReceiverName ensures the receiver name is valid for Alertmanager.
func sanitizeReceiverName(name string) string {
	// Alertmanager requires receiver names to be valid label names
	result := strings.Builder{}
	for i, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			result.WriteRune(r)
		} else if i > 0 {
			result.WriteRune('_')
		}
	}
	s := result.String()
	if s == "" {
		s = "receiver"
	}
	// Ensure it doesn't start with a digit
	if s[0] >= '0' && s[0] <= '9' {
		s = "r_" + s
	}
	return s
}

// Helper function to parse JSON string into map[string]string

// FindGrafanaComponent returns the running grafana component for a cluster.
