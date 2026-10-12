package config

import (
	"encoding/json"
	"fmt"
	"github.com/promlifecycle/platform/models"
	"net/url"
	"strings"
)

// DirectNotificationEndpoint shares Alertmanager's canonical channel keys.
func DirectNotificationEndpoint(channel models.NotificationChannel) (string, error) {
	var cfg map[string]interface{}
	if strings.TrimSpace(channel.Config) != "" {
		if err := json.Unmarshal([]byte(channel.Config), &cfg); err != nil {
			return "", fmt.Errorf("通知配置不是有效 JSON")
		}
	}
	key := "url"
	switch channel.Type {
	case "slack":
		key = "api_url"
	case "dingtalk":
		key = "webhook_url"
	case "webhook":
	case "wechat":
		return "", fmt.Errorf("企业微信应用使用 Alertmanager 企业微信 API；不支持直接巡检通知，请选择 webhook、Slack、钉钉或邮件")
	default:
		return "", fmt.Errorf("不支持直接发送的渠道类型: %s", channel.Type)
	}
	endpoint, _ := cfg[key].(string)
	if endpoint == "" && channel.Type == "webhook" {
		endpoint, _ = cfg["webhook"].(string)
	}
	if endpoint == "" {
		endpoint = channel.Secret
	}
	endpoint = strings.TrimSpace(endpoint)
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil {
		return "", fmt.Errorf("%s 必须是有效 HTTP(S) URL", key)
	}
	return endpoint, nil
}

func ValidateInspectionChannel(channel models.NotificationChannel) error {
	if channel.Type == "email" {
		_, _, err := ResolveEmailChannel(channel)
		return err
	}
	_, err := DirectNotificationEndpoint(channel)
	return err
}
