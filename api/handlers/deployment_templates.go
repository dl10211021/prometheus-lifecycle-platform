package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
)

// Deployment templates are reusable settings, never credential containers.
// Keep harmless environment/argument values but omit secrets rather than passing
// redaction placeholders to a deployment worker.
func sanitizeDeploymentTemplate(content string) (string, error) {
	var input struct {
		ComponentOrder []string                          `json:"component_order"`
		Settings       map[string]map[string]interface{} `json:"settings"`
	}
	if err := json.Unmarshal([]byte(content), &input); err != nil || len(input.ComponentOrder) == 0 || input.Settings == nil {
		return "", fmt.Errorf("整栈模板需要合法 component_order 和 settings")
	}
	clean := make(map[string]map[string]interface{}, len(input.Settings))
	for _, componentType := range input.ComponentOrder {
		if strings.TrimSpace(componentType) == "" {
			return "", fmt.Errorf("组件类型不能为空")
		}
		settings := make(map[string]interface{})
		for key, value := range input.Settings[componentType] {
			switch key {
			case "initial_password", "host_id", "preflight_token", "planned_components":
				continue
			case "custom_env":
				raw, ok := value.(string)
				if !ok {
					return "", fmt.Errorf("custom_env 必须为 JSON 字符串")
				}
				var environment map[string]interface{}
				if err := json.Unmarshal([]byte(raw), &environment); raw != "" && err != nil {
					return "", fmt.Errorf("custom_env 不是合法 JSON")
				}
				safeEnvironment := make(map[string]interface{})
				for name, setting := range environment {
					candidate, _ := json.Marshal(map[string]interface{}{name: setting})
					safe := utils.SanitizeConfigJSON(candidate)
					if !utils.HasRedactedSecrets(string(safe)) {
						safeEnvironment[name] = setting
					}
				}
				encoded, _ := json.Marshal(safeEnvironment)
				settings[key] = string(encoded)
			case "custom_args":
				text, ok := value.(string)
				if !ok {
					return "", fmt.Errorf("custom_args 必须为字符串")
				}
				lines := make([]string, 0)
				for _, line := range strings.Split(text, "\n") {
					line = strings.TrimSpace(line)
					if line == "" {
						continue
					}
					parts := strings.SplitN(strings.TrimLeft(line, "-"), "=", 2)
					if len(parts) == 1 {
						parts = strings.SplitN(parts[0], " ", 2)
					}
					argument := ""
					if len(parts) > 1 {
						argument = parts[1]
					}
					candidate, _ := json.Marshal(map[string]interface{}{parts[0]: argument})
					if !utils.HasRedactedSecrets(string(utils.SanitizeConfigJSON(candidate))) {
						lines = append(lines, line)
					}
				}
				settings[key] = strings.Join(lines, "\n")
			default:
				candidate, err := json.Marshal(map[string]interface{}{key: value})
				if err != nil {
					return "", err
				}
				safe := utils.SanitizeConfigJSON(candidate)
				if utils.HasRedactedSecrets(string(safe)) {
					continue
				}
				var decoded map[string]interface{}
				if err := json.Unmarshal(safe, &decoded); err != nil {
					return "", err
				}
				settings[key] = decoded[key]
			}
		}
		clean[componentType] = settings
	}
	encoded, err := json.Marshal(map[string]interface{}{"component_order": input.ComponentOrder, "settings": clean})
	return string(encoded), err
}

func (h *TemplateHandler) ListDeploymentTemplates(c *gin.Context) {
	var templates []models.ConfigTemplate
	if err := h.db.Where("component_type = ?", "_deployment_stack").Order("created_at DESC").Find(&templates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取整栈模板失败"})
		return
	}
	result := make([]models.ConfigTemplate, 0, len(templates))
	for _, item := range templates {
		content, err := sanitizeDeploymentTemplate(item.Content)
		if err != nil {
			continue
		}
		item.Content = content
		result = append(result, item)
	}
	c.JSON(http.StatusOK, gin.H{"templates": result})
}
