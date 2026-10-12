package utils

import (
	"encoding/json"
	"net/url"
	"strings"

	"gopkg.in/ini.v1"
	"gopkg.in/yaml.v3"
)

const RedactedSecret = "[REDACTED]"

func CanReadConfigSecrets(role string) bool {
	return role == "system_admin" || role == "change_admin"
}

// SanitizeConfig is a fail-closed preview, never an editable source. Unknown
// configuration fields are omitted unless their values have a known safe schema.
// Callers must use an explicitly authorized raw read for editing.
func SanitizeConfig(componentType, contentType, content string) string {
	if strings.TrimSpace(content) == "" {
		return content
	}
	if strings.EqualFold(contentType, "ini") || componentType == "grafana" {
		cfg, err := ini.Load([]byte(content))
		if err != nil {
			return RedactedSecret
		}
		var out strings.Builder
		for _, section := range cfg.Sections() {
			if section.Name() != "DEFAULT" {
				out.WriteString("[" + section.Name() + "]\n")
			}
			for _, key := range section.Keys() {
				out.WriteString(key.Name() + " = ")
				value := sanitizeConfigField(key.Name(), key.Value())
				text, ok := value.(string)
				if !ok {
					text = RedactedSecret
				}
				out.WriteString(text + "\n")
			}
		}
		return out.String()
	}
	if contentType != "" && contentType != "yaml" && contentType != "yml" && contentType != "json" {
		return RedactedSecret
	}
	var value any
	if err := yaml.Unmarshal([]byte(content), &value); err != nil {
		return RedactedSecret
	}
	switch value.(type) {
	case map[string]any, []any:
	default:
		return RedactedSecret
	}
	value = sanitizeConfigTree(value)
	if contentType == "json" {
		data, err := json.Marshal(value)
		if err != nil {
			return RedactedSecret
		}
		return string(data)
	}
	data, err := yaml.Marshal(value)
	if err != nil {
		return RedactedSecret
	}
	return string(data)
}

func sensitiveConfigKey(key string) bool {
	key = strings.ToLower(strings.ReplaceAll(key, "-", "_"))
	for _, part := range []string{"password", "passwd", "token", "secret", "credential", "auth", "cookie", "api_key", "apikey", "private_key", "client_key", "access_key", "signing_key", "webhook", "custom_env", "http_headers", "headers", "identity"} {
		if strings.Contains(key, part) {
			return true
		}
	}
	return key == "auth" || key == "basic_auth" || key == "oauth2" || key == "username" || key == "user" || key == "key" || key == "key_file" || key == "custom_args" || key == "extra_args" || key == "args" || key == "env" || key == "environment" || key == "command"
}

// Safe scalar fields are shared by built-in Prometheus, Alertmanager, Loki,
// Promtail, Blackbox and Grafana configs. Unknown extension values fail closed.
func safeConfigScalar(key string) bool {
	switch strings.ToLower(key) {
	case "name", "job_name", "group_name", "alert", "record", "expr", "for", "keep_firing_for", "interval", "scrape_interval", "scrape_timeout", "evaluation_interval", "timeout", "refresh_interval", "remote_timeout", "path", "metrics_path", "scheme", "type", "version", "notes", "enabled", "disabled", "honor_labels", "honor_timestamps", "follow_redirects", "enable_http2", "insecure_skip_verify", "min_version", "max_version", "server_name", "ca_file", "cert_file", "listen_address", "http_addr", "http_port", "http_listen_port", "grpc_listen_port", "log_level", "log_format", "protocol", "prober", "method", "valid_status_codes", "valid_http_versions", "preferred_ip_protocol", "ip_protocol_fallback", "targets", "files", "rule_files", "source_labels", "target_label", "action", "regex", "replacement", "separator", "modulus", "receiver", "group_by", "group_wait", "group_interval", "repeat_interval", "continue", "match", "match_re", "matchers", "source_matchers", "target_matchers", "equal", "send_resolved", "url", "proxy_url", "external_url", "root_url", "datasource_uid", "uid", "title", "description", "max_samples_per_send", "capacity", "min_shards", "max_shards", "batch_send_deadline", "min_backoff", "max_backoff", "retry_on_http_429":
		return true
	}
	return false
}

func SanitizeConfigURL(value string) string {
	if !strings.Contains(value, "://") && !strings.HasPrefix(value, "//") {
		return value
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return RedactedSecret
	}
	if parsed.Scheme == "" && parsed.Host == "" {
		return value
	}
	if parsed.User != nil {
		parsed.User = url.User(RedactedSecret)
	}
	if parsed.RawQuery != "" {
		parsed.RawQuery = "redacted=" + url.QueryEscape(RedactedSecret)
	}
	if parsed.Fragment != "" {
		parsed.Fragment = RedactedSecret
		parsed.RawFragment = ""
	}
	return parsed.String()
}

func safeConfigContainer(key string) bool {
	switch strings.ToLower(key) {
	case "global", "scrape_configs", "remote_write", "remote_read", "alerting", "alertmanagers", "static_configs", "file_sd_configs", "http_sd_configs", "dns_sd_configs", "kubernetes_sd_configs", "consul_sd_configs", "ec2_sd_configs", "tls_config", "relabel_configs", "metric_relabel_configs", "write_relabel_configs", "queue_config", "metadata_config", "external_labels", "labels", "params", "groups", "rules", "annotations", "route", "routes", "receivers", "inhibit_rules", "mute_time_intervals", "time_intervals", "email_configs", "slack_configs", "pagerduty_configs", "opsgenie_configs", "wechat_configs", "http_config", "modules", "http", "tcp", "icmp", "dns", "server", "common", "storage_config", "schema_config", "configs", "limits_config", "ruler", "clients", "positions", "pipeline_stages":
		return true
	}
	return false
}

func sanitizeConfigField(key string, value any) any {
	if sensitiveConfigKey(key) {
		return RedactedSecret
	}
	switch item := value.(type) {
	case map[string]any:
		if !safeConfigContainer(key) {
			return RedactedSecret
		}
		return sanitizeConfigTree(item)
	case []any:
		out := make([]any, len(item))
		for i, v := range item {
			out[i] = sanitizeConfigField(key, v)
		}
		return out
	case string:
		if !safeConfigScalar(key) {
			return RedactedSecret
		}
		return SanitizeConfigURL(item)
	default:
		if !safeConfigScalar(key) {
			return RedactedSecret
		}
		return value
	}
}

func sanitizeConfigTree(value any) any {
	switch item := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(item))
		for key, value := range item {
			out[key] = sanitizeConfigField(key, value)
		}
		return out
	case []any:
		out := make([]any, len(item))
		for i, v := range item {
			out[i] = sanitizeConfigTree(v)
		}
		return out
	default:
		return RedactedSecret
	}
}

// SanitizeConfigJSON handles snapshots with embedded config strings as well as
// nested credential objects. Diffs cannot be parsed safely; regenerate from the
// sanitized snapshots instead of forwarding their original text.
func SanitizeConfigJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return json.RawMessage(`"[REDACTED]"`)
	}
	encoded, err := json.Marshal(sanitizeSnapshot(value))
	if err != nil {
		return json.RawMessage(`"[REDACTED]"`)
	}
	return encoded
}

func sanitizeSnapshot(value any) any {
	switch item := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(item))
		for key, value := range item {
			if sensitiveConfigKey(key) {
				out[key] = RedactedSecret
				continue
			}
			switch strings.ToLower(key) {
			case "content", "config", "config_content", "default_config":
				if text, ok := value.(string); ok {
					format, _ := item["content_type"].(string)
					kind, _ := item["type"].(string)
					out[key] = SanitizeConfig(kind, format, text)
				} else {
					out[key] = sanitizeConfigTree(value)
				}
			case "diff", "error", "last_error", "output", "logs", "component_logs", "summary", "message":
				out[key] = RedactedSecret
			default:
				out[key] = sanitizeSnapshot(value)
			}
		}
		return out
	case []any:
		out := make([]any, len(item))
		for i, v := range item {
			out[i] = sanitizeSnapshot(v)
		}
		return out
	case string:
		var embedded any
		if json.Unmarshal([]byte(item), &embedded) == nil {
			switch embedded.(type) {
			case map[string]any, []any:
				encoded, err := json.Marshal(sanitizeSnapshot(embedded))
				if err != nil {
					return RedactedSecret
				}
				return string(encoded)
			}
		}
		return SanitizeConfigURL(item)
	default:
		return value
	}
}

func HasRedactedSecrets(content string) bool {
	if strings.Contains(content, RedactedSecret) || strings.Contains(strings.ToUpper(content), "%5BREDACTED%5D") {
		return true
	}
	var value any
	if json.Unmarshal([]byte(content), &value) != nil {
		return false
	}
	var contains func(any) bool
	contains = func(v any) bool {
		switch item := v.(type) {
		case string:
			return HasRedactedSecrets(item)
		case map[string]any:
			for _, child := range item {
				if contains(child) {
					return true
				}
			}
		case []any:
			for _, child := range item {
				if contains(child) {
					return true
				}
			}
		}
		return false
	}
	return contains(value)
}
