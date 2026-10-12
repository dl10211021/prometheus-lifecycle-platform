package config

import (
	"encoding/json"
	"fmt"
	"net"
	"net/mail"
	"os"
	"strconv"
	"strings"

	"github.com/promlifecycle/platform/models"
)

// SMTPSettings is the single source of truth used by generated Alertmanager
// configuration and the notification-channel delivery test.
type SMTPSettings struct {
	Host       string
	Port       int
	Username   string
	Password   string
	From       string
	RequireTLS bool
}

func (s SMTPSettings) Smarthost() string {
	return net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
}

// SMTPSettingsFromEnv parses and validates PROMLIFECYCLE_SMTP_*.
func SMTPSettingsFromEnv() (SMTPSettings, error) {
	s := SMTPSettings{
		Host:       strings.TrimSpace(os.Getenv("PROMLIFECYCLE_SMTP_HOST")),
		Username:   os.Getenv("PROMLIFECYCLE_SMTP_USERNAME"),
		Password:   os.Getenv("PROMLIFECYCLE_SMTP_PASSWORD"),
		From:       strings.TrimSpace(os.Getenv("PROMLIFECYCLE_SMTP_FROM")),
		RequireTLS: true,
	}
	if s.Host == "" {
		return s, fmt.Errorf("未配置 SMTP 主机，请设置 PROMLIFECYCLE_SMTP_HOST 后重试")
	}
	if strings.ContainsAny(s.Host, "\r\n\x00") {
		return s, fmt.Errorf("SMTP 主机格式无效，请检查 PROMLIFECYCLE_SMTP_HOST")
	}
	portText := strings.TrimSpace(os.Getenv("PROMLIFECYCLE_SMTP_PORT"))
	if portText == "" {
		portText = "587"
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return s, fmt.Errorf("SMTP 端口无效，请将 PROMLIFECYCLE_SMTP_PORT 设置为 1-65535")
	}
	s.Port = port
	if s.From == "" {
		s.From = s.Username
	}
	if _, err := mail.ParseAddress(s.From); err != nil {
		return s, fmt.Errorf("SMTP 发件人无效，请检查 PROMLIFECYCLE_SMTP_FROM: %w", err)
	}
	if raw := strings.TrimSpace(os.Getenv("PROMLIFECYCLE_SMTP_REQUIRE_TLS")); raw != "" {
		requireTLS, err := strconv.ParseBool(raw)
		if err != nil {
			return s, fmt.Errorf("SMTP TLS 配置无效，请将 PROMLIFECYCLE_SMTP_REQUIRE_TLS 设置为 true 或 false")
		}
		s.RequireTLS = requireTLS
	}
	if s.Username == "" && s.Password != "" {
		return s, fmt.Errorf("SMTP 用户名为空，请设置 PROMLIFECYCLE_SMTP_USERNAME 后重试")
	}
	return s, nil
}

// OptionalSMTPSettingsFromEnv returns nil when SMTP is intentionally absent.
// Once any SMTP host is configured the complete settings are validated.
func OptionalSMTPSettingsFromEnv() (*SMTPSettings, error) {
	if strings.TrimSpace(os.Getenv("PROMLIFECYCLE_SMTP_HOST")) == "" {
		return nil, nil
	}
	settings, err := SMTPSettingsFromEnv()
	if err != nil {
		return nil, err
	}
	return &settings, nil
}

// ResolveEmailChannel validates the channel recipient and returns the exact
// SMTP settings represented in alertmanager.yml.
func ResolveEmailChannel(ch models.NotificationChannel) (SMTPSettings, string, error) {
	settings, err := SMTPSettingsFromEnv()
	if err != nil {
		return settings, "", err
	}
	var cfg struct {
		To string `json:"to"`
	}
	if err := json.Unmarshal([]byte(ch.Config), &cfg); err != nil {
		return settings, "", fmt.Errorf("邮件渠道配置不是有效 JSON，请修正后重试")
	}
	cfg.To = strings.TrimSpace(cfg.To)
	if cfg.To == "" {
		return settings, "", fmt.Errorf("邮件渠道未配置收件人，请填写 to 后重试")
	}
	addresses, err := mail.ParseAddressList(cfg.To)
	if err != nil || len(addresses) == 0 {
		return settings, "", fmt.Errorf("邮件收件人格式无效，请修正 to 后重试")
	}
	return settings, cfg.To, nil
}
