package handlers

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/promlifecycle/platform/core/config"
	"github.com/promlifecycle/platform/pkg/utils"
)

// ========== CONFIG MANAGEMENT ==========

func (h *Handler) buildConfig(compType string, userConfig map[string]interface{}) map[string]interface{} {
	var cfg map[string]interface{}

	switch compType {
	case "prometheus":
		cfg = config.DefaultPrometheusConfig()
	case "alertmanager":
		cfg = config.DefaultAlertmanagerConfig()
	case "blackbox_exporter":
		cfg = config.DefaultBlackboxConfig()
	case "loki":
		cfg = config.DefaultLokiConfig()
	case "promtail":
		cfg = config.DefaultPromtailConfig()
	default:
		cfg = make(map[string]interface{})
	}

	// Override with user config
	if userConfig != nil {
		mergeConfigs(cfg, userConfig)
	}

	return cfg
}

func validateCustomEnvironment(raw string) error {
	if raw == "" {
		return nil
	}
	var env map[string]string
	if err := json.Unmarshal([]byte(raw), &env); err != nil || env == nil {
		return fmt.Errorf("环境变量必须是 JSON 字符串对象")
	}
	for name, value := range env {
		if !utils.ValidEnvironmentName(name) {
			return fmt.Errorf("环境变量名 %q 无效（仅允许字母、数字和下划线，且不能以数字开头）", name)
		}
		if strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("环境变量 %q 的值不能包含 NUL 字符", name)
		}
	}
	return nil
}

func mergeConfigs(base, override map[string]interface{}) {
	for k, v := range override {
		if ov, ok := v.(map[string]interface{}); ok {
			if bv, bok := base[k].(map[string]interface{}); bok {
				mergeConfigs(bv, ov)
				continue
			}
		}
		base[k] = v
	}
}
