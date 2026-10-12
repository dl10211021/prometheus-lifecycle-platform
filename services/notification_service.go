package services

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/smtp"
	"strings"
	"time"

	"github.com/promlifecycle/platform/core/config"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"gorm.io/gorm"
)

// NotificationService delivers component health transitions to enabled
// notification channels. Callers decide whether a transition is meaningful;
// this service performs one delivery attempt and records its outcome.
type NotificationService struct {
	db     *gorm.DB
	client *http.Client
}

func NewNotificationService(db *gorm.DB) *NotificationService {
	return &NotificationService{
		db: db,
		client: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment, DialContext: utils.SafeDialContext,
		}},
	}
}

func (s *NotificationService) SendHealthTransition(component *models.Component, from, to models.ComponentStatus) {
	if s == nil || s.db == nil || component == nil || from == to {
		return
	}
	var channels []models.NotificationChannel
	if err := s.db.Where("enabled = ? AND (cluster_id IS NULL OR cluster_id = ?)", true, component.ClusterID).
		Order("id ASC").Find(&channels).Error; err != nil {
		return
	}
	severity := "warning"
	if to == models.StatusError {
		severity = "critical"
	} else if to == models.StatusRunning {
		severity = "info"
	}
	message := fmt.Sprintf("组件 %s（%s）健康状态从 %s 变为 %s", component.Name, component.Type, from, to)
	for i := range channels {
		status, errorMessage := s.deliver(&channels[i], "ComponentHealthTransition", message, severity)
		s.db.Create(&models.AlertNotification{
			AlertName: "ComponentHealthTransition", Severity: severity,
			ChannelID: channels[i].ID, ChannelType: channels[i].Type,
			Message: message, Status: status, Error: errorMessage, CreatedAt: time.Now(),
		})
	}
}

type NotificationDelivery struct {
	ChannelID   uint   `json:"channel_id"`
	ChannelName string `json:"channel_name"`
	ChannelType string `json:"channel_type"`
	Status      string `json:"status"`
	Error       string `json:"error,omitempty"`
}

func (s *NotificationService) deliver(channel *models.NotificationChannel, event, message, severity string) (string, string) {
	if strings.EqualFold(channel.Type, "email") {
		settings, recipient, err := config.ResolveEmailChannel(*channel)
		if err != nil {
			return "failed", err.Error()
		}
		body := "Content-Type: text/plain; charset=UTF-8\r\n" +
			"Subject: 【Prometheus 监控平台】组件健康状态变化\r\n" +
			"From: " + settings.From + "\r\nTo: " + recipient + "\r\n\r\n" + message
		auth := smtp.PlainAuth("", settings.Username, settings.Password, settings.Host)
		if err := smtp.SendMail(settings.Smarthost(), auth, settings.From, []string{recipient}, []byte(body)); err != nil {
			return "failed", "SMTP 发送失败"
		}
		return "sent", ""
	}
	endpoint, endpointErr := config.DirectNotificationEndpoint(*channel)
	if endpointErr != nil {
		return "failed", endpointErr.Error()
	}
	var payload interface{}
	switch strings.ToLower(channel.Type) {
	case "dingtalk":
		payload = map[string]interface{}{"msgtype": "text", "text": map[string]string{"content": message}}
	case "slack":
		payload = map[string]interface{}{"blocks": []map[string]interface{}{{"type": "section", "text": map[string]string{"type": "mrkdwn", "text": message}}}}
	case "webhook":
		payload = map[string]string{"text": message, "alert": event, "severity": severity}
	default:
		return "failed", "不支持直接发送的渠道类型: " + channel.Type
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "failed", err.Error()
	}
	resp, err := s.client.Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return "failed", "HTTP 通知请求失败"
	}
	defer func() {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "failed", fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	if strings.EqualFold(channel.Type, "dingtalk") {
		var result struct {
			ErrCode *int `json:"errcode"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&result); err != nil {
			return "failed", "钉钉响应无法解析"
		}
		if result.ErrCode == nil {
			return "failed", "钉钉响应缺少确认状态"
		}
		if *result.ErrCode != 0 {
			return "failed", fmt.Sprintf("钉钉拒绝消息 (%d)", *result.ErrCode)
		}
	}
	return "sent", ""
}
