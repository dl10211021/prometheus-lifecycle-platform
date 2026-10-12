package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"
)

type ConfigValidateHandler struct{}

// ValidatePrometheusConfig uses promtool, falling back to basic checks only when it is absent.
func (h *ConfigValidateHandler) ValidatePrometheusConfig(c *gin.Context) {
	var input struct {
		Content string `json:"content"`
	}

	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效请求参数"})
		return
	}

	result := struct {
		Valid   bool   `json:"valid"`
		Message string `json:"message"`
		Error   string `json:"error,omitempty"`
		Method  string `json:"method"`
	}{Valid: false}

	// Method 1: Try using promtool binary
	result.Method = "promtool"
	if err := h.validateWithPromtool(input.Content); err == nil {
		result.Valid = true
		result.Message = "Prometheus configuration is valid"
		c.JSON(http.StatusOK, result)
		return
	} else if !errors.Is(err, exec.ErrNotFound) {
		result.Error = err.Error()
		c.JSON(http.StatusConflict, result)
		return
	}

	// Basic YAML and structure validation cannot establish full semantic validity.
	result.Method = "basic"
	if err := h.validatePrometheusStructure(input.Content); err != nil {
		result.Error = err.Error()
		c.JSON(http.StatusConflict, result)
		return
	}
	result.Valid = true
	result.Message = "Prometheus configuration passed basic validation (promtool not available)"
	c.JSON(http.StatusOK, result)
}

// ValidateAlertmanagerConfig uses amtool, falling back to basic checks only when it is absent.
func (h *ConfigValidateHandler) ValidateAlertmanagerConfig(c *gin.Context) {
	var input struct {
		Content string `json:"content"`
	}

	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效请求参数"})
		return
	}

	result := struct {
		Valid   bool   `json:"valid"`
		Message string `json:"message"`
		Error   string `json:"error,omitempty"`
		Method  string `json:"method"`
	}{Valid: false}

	// Method 1: Try using amtool binary
	result.Method = "amtool"
	if err := h.validateWithAmtool(input.Content); err == nil {
		result.Valid = true
		result.Message = "Alertmanager configuration is valid"
		c.JSON(http.StatusOK, result)
		return
	} else if !errors.Is(err, exec.ErrNotFound) {
		result.Error = err.Error()
		c.JSON(http.StatusConflict, result)
		return
	}

	// Basic YAML and structure validation cannot establish full semantic validity.
	result.Method = "basic"
	if err := h.validateAlertmanagerStructure(input.Content); err != nil {
		result.Error = err.Error()
		c.JSON(http.StatusConflict, result)
		return
	}
	result.Valid = true
	result.Message = "Alertmanager configuration passed basic validation (amtool not available)"
	c.JSON(http.StatusOK, result)
}

// validateWithPromtool uses promtool check config
func (h *ConfigValidateHandler) validateWithPromtool(content string) error {
	if content == "" {
		return fmt.Errorf("empty configuration")
	}

	// Write to temp file (promtool doesn't support stdin)
	tmpFile, err := os.CreateTemp("", "prometheus-config-*.yml")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(content); err != nil {
		tmpFile.Close()
		return fmt.Errorf("failed to write config: %w", err)
	}
	tmpFile.Close()

	cmd := exec.Command("promtool", "check", "config", tmpFile.Name())
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("promtool validation failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// validatePrometheusStructure checks selected YAML shapes when promtool is absent.
// It does not validate all Prometheus fields or replace native semantic validation.
func (h *ConfigValidateHandler) validatePrometheusStructure(content string) error {
	if content == "" {
		return fmt.Errorf("empty configuration")
	}

	var cfg map[string]interface{}
	if err := yaml.Unmarshal([]byte(content), &cfg); err != nil {
		return fmt.Errorf("invalid YAML: %w", err)
	}

	if g, ok := cfg["global"]; ok {
		if _, ok := g.(map[string]interface{}); !ok {
			return fmt.Errorf("'global' 必须是键值映射")
		}
	}

	scs, ok := cfg["scrape_configs"]
	if !ok || scs == nil {
		return fmt.Errorf("缺少必需的 'scrape_configs' 段")
	}
	scList, ok := scs.([]interface{})
	if !ok {
		return fmt.Errorf("'scrape_configs' 必须是数组")
	}
	if len(scList) == 0 {
		return fmt.Errorf("'scrape_configs' 不能为空")
	}
	for i, item := range scList {
		sc, ok := item.(map[string]interface{})
		if !ok {
			return fmt.Errorf("scrape_configs[%d] 必须是映射", i)
		}
		job, _ := sc["job_name"].(string)
		if job == "" {
			return fmt.Errorf("scrape_configs[%d] 缺少 job_name", i)
		}
		if st, ok := sc["static_configs"]; ok {
			stList, ok := st.([]interface{})
			if !ok {
				return fmt.Errorf("scrape_configs[%d].static_configs 必须是数组", i)
			}
			for _, s := range stList {
				sm, ok := s.(map[string]interface{})
				if !ok {
					return fmt.Errorf("scrape_configs[%d].static_configs 中的每项必须是映射", i)
				}
				if t, ok := sm["targets"]; ok {
					if _, ok := t.([]interface{}); !ok {
						return fmt.Errorf("scrape_configs[%d].static_configs.targets 必须是数组", i)
					}
				}
			}
		}
	}

	if rw, ok := cfg["remote_write"]; ok {
		rwList, ok := rw.([]interface{})
		if !ok {
			return fmt.Errorf("'remote_write' 必须是数组")
		}
		for i, item := range rwList {
			rm, ok := item.(map[string]interface{})
			if !ok {
				return fmt.Errorf("remote_write[%d] 必须是映射", i)
			}
			if u, _ := rm["url"].(string); u == "" {
				return fmt.Errorf("remote_write[%d] 缺少 url", i)
			}
		}
	}

	if rf, ok := cfg["rule_files"]; ok {
		if _, ok := rf.([]interface{}); !ok {
			return fmt.Errorf("'rule_files' 必须是数组")
		}
	}

	return nil
}

// validateWithAmtool uses amtool check-config
func (h *ConfigValidateHandler) validateWithAmtool(content string) error {
	if content == "" {
		return fmt.Errorf("empty configuration")
	}

	// Write to temp file (amtool doesn't support stdin for check-config)
	tmpFile, err := os.CreateTemp("", "alertmanager-config-*.yml")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(content); err != nil {
		tmpFile.Close()
		return fmt.Errorf("failed to write config: %w", err)
	}
	tmpFile.Close()

	cmd := exec.Command("amtool", "check-config", tmpFile.Name())
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("amtool validation failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// validateAlertmanagerStructure does basic YAML and structure validation
func (h *ConfigValidateHandler) validateAlertmanagerStructure(content string) error {
	if content == "" {
		return fmt.Errorf("empty configuration")
	}

	var cfg map[string]interface{}
	if err := yaml.Unmarshal([]byte(content), &cfg); err != nil {
		return fmt.Errorf("invalid YAML: %w", err)
	}

	route, ok := cfg["route"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("alertmanager config requires a 'route' mapping")
	}
	receiver, _ := route["receiver"].(string)
	if receiver == "" {
		return fmt.Errorf("alertmanager root route requires a receiver")
	}
	receivers, ok := cfg["receivers"].([]interface{})
	if !ok {
		return fmt.Errorf("alertmanager config requires a 'receivers' array")
	}
	found := false
	for i, item := range receivers {
		entry, ok := item.(map[string]interface{})
		if !ok {
			return fmt.Errorf("receivers[%d] must be a mapping", i)
		}
		name, _ := entry["name"].(string)
		if name == "" {
			return fmt.Errorf("receivers[%d] requires a name", i)
		}
		if name == receiver {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("route receiver %q not found in receivers", receiver)
	}

	return nil
}
