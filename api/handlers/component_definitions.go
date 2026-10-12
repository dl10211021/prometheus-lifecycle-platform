package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"gorm.io/gorm"
)

type ComponentDefinitionHandler struct {
	db *gorm.DB
}

func NewComponentDefinitionHandler(db *gorm.DB) *ComponentDefinitionHandler {
	return &ComponentDefinitionHandler{db: db}
}

func (h *ComponentDefinitionHandler) ListComponentDefinitions(c *gin.Context) {
	var definitions []models.ComponentDefinition
	query := h.db.Order("type ASC")
	if enabled := c.Query("enabled"); enabled == "true" || enabled == "false" {
		query = query.Where("enabled = ?", enabled == "true")
	}
	if err := query.Find(&definitions).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取组件定义列表失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"component_definitions": definitions})
}

func (h *ComponentDefinitionHandler) GetComponentDefinition(c *gin.Context) {
	var definition models.ComponentDefinition
	if err := h.db.First(&definition, c.Param("id")).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "组件定义不存在"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "读取组件定义失败"})
		}
		return
	}
	c.JSON(http.StatusOK, definition)
}

type componentDefinitionInput struct {
	Type        string          `json:"type"`
	DisplayName string          `json:"display_name"`
	BinName     string          `json:"bin_name"`
	DefaultPort int             `json:"default_port"`
	ConfigDir   string          `json:"config_dir"`
	DataDir     string          `json:"data_dir"`
	HealthPath  string          `json:"health_path"`
	Command     string          `json:"command"`
	PackageURL  string          `json:"package_url"`
	ParamSchema json.RawMessage `json:"param_schema"`
	Enabled     *bool           `json:"enabled"`
}

func (in componentDefinitionInput) model() models.ComponentDefinition {
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	return models.ComponentDefinition{
		Type: strings.TrimSpace(in.Type), DisplayName: strings.TrimSpace(in.DisplayName),
		BinName: strings.TrimSpace(in.BinName), DefaultPort: in.DefaultPort,
		ConfigDir: strings.TrimSpace(in.ConfigDir), DataDir: strings.TrimSpace(in.DataDir),
		HealthPath: strings.TrimSpace(in.HealthPath), Command: strings.TrimSpace(in.Command),
		PackageURL: strings.TrimSpace(in.PackageURL), Enabled: enabled,
	}
}

func (h *ComponentDefinitionHandler) CreateComponentDefinition(c *gin.Context) {
	var input componentDefinitionInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "输入无效: " + err.Error()})
		return
	}
	definition := input.model()
	paramSchema, err := normalizeCustomDeploymentParameterSchema(input.ParamSchema)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "操作失败: " + err.Error()})
		return
	}
	definition.ParamSchema = paramSchema
	if err := ValidateComponentDefinition(&definition); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "操作失败: " + err.Error()})
		return
	}
	if err := h.db.Create(&definition).Error; err != nil {
		if utils.IsUniqueConstraintError(err) {
			c.JSON(http.StatusConflict, gin.H{"error": "该组件定义类型已存在"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "创建组件定义失败"})
		}
		return
	}
	c.JSON(http.StatusCreated, definition)
}

func (h *ComponentDefinitionHandler) UpdateComponentDefinition(c *gin.Context) {
	var definition models.ComponentDefinition
	if err := h.db.First(&definition, c.Param("id")).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "组件定义不存在"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "读取组件定义失败"})
		}
		return
	}
	var input componentDefinitionInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "输入无效: " + err.Error()})
		return
	}
	updated := input.model()
	if len(input.ParamSchema) == 0 {
		updated.ParamSchema = definition.ParamSchema
	} else {
		paramSchema, err := normalizeCustomDeploymentParameterSchema(input.ParamSchema)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "操作失败: " + err.Error()})
			return
		}
		updated.ParamSchema = paramSchema
	}
	if updated.Type == "" {
		updated.Type = definition.Type
	}
	if updated.Type != definition.Type {
		c.JSON(http.StatusBadRequest, gin.H{"error": "组件定义的 type 不可修改"})
		return
	}
	updated.ID = definition.ID
	updated.CreatedAt = definition.CreatedAt
	if input.Enabled == nil {
		updated.Enabled = definition.Enabled
	}
	if err := ValidateComponentDefinition(&updated); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "操作失败: " + err.Error()})
		return
	}
	if err := h.db.Model(&definition).Select("display_name", "bin_name", "default_port", "config_dir", "data_dir", "health_path", "command", "package_url", "param_schema", "enabled").Updates(updated).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "更新组件定义失败"})
		return
	}
	if err := h.db.First(&definition, definition.ID).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "重新读取组件定义失败"})
		return
	}
	c.JSON(http.StatusOK, definition)
}

func (h *ComponentDefinitionHandler) DeleteComponentDefinition(c *gin.Context) {
	result := h.db.Delete(&models.ComponentDefinition{}, c.Param("id"))
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "删除组件定义失败"})
		return
	}
	if result.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "组件定义不存在"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "组件定义已删除"})
}

// FindEnabledComponentDefinition resolves a type for CreateComponent before the
// definition snapshot is embedded into Component.Config.
func FindEnabledComponentDefinition(db *gorm.DB, componentType string) (models.ComponentDefinition, error) {
	var definition models.ComponentDefinition
	if err := db.Where("type = ? AND enabled = ?", componentType, true).First(&definition).Error; err != nil {
		return definition, err
	}
	return definition, nil
}

// ValidateComponentDefinition enforces values that are later used in package
// paths, systemd units and HTTP health checks.
func ValidateComponentDefinition(d *models.ComponentDefinition) error {
	if d == nil {
		return fmt.Errorf("必须提供组件定义")
	}
	if !validDefinitionType(d.Type) {
		return fmt.Errorf("type 必须匹配 [a-z][a-z0-9_]{1,31}")
	}
	if utils.IsValidComponentType(d.Type) {
		return fmt.Errorf("type 与内置组件类型冲突")
	}
	if d.DisplayName == "" || len(d.DisplayName) > 128 || hasControl(d.DisplayName) {
		return fmt.Errorf("display_name 为必填项且长度不能超过 128 个字符")
	}
	if !validExecutableName(d.BinName) {
		return fmt.Errorf("bin_name 必须是安全的可执行文件名")
	}
	if d.DefaultPort < 1 || d.DefaultPort > 65535 {
		return fmt.Errorf("default_port 必须在 1 到 65535 之间")
	}
	if !validDefinitionPath(d.ConfigDir) || !validDefinitionPath(d.DataDir) {
		return fmt.Errorf("config_dir 和 data_dir 必须是安全的绝对路径")
	}
	if !validHealthPath(d.HealthPath) {
		return fmt.Errorf("health_path 必须是不含查询参数和路径穿越的绝对 URL 路径")
	}
	if !validDefinitionCommand(d.Command, d.BinName) {
		return fmt.Errorf("command 包含不支持或不安全的参数")
	}
	if err := validatePackageURL(d.PackageURL); err != nil {
		return err
	}
	if err := validateCustomDeploymentParameterSchema(d.ParamSchema); err != nil {
		return err
	}
	return nil
}

func validDefinitionType(value string) bool {
	if len(value) < 2 || len(value) > 32 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for i := 1; i < len(value); i++ {
		c := value[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

func validExecutableName(value string) bool {
	if !utils.SafePathSegment(value) || len(value) > 128 || value[0] == '-' {
		return false
	}
	for _, r := range value {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-' && r != '.' {
			return false
		}
	}
	return true
}

func validDefinitionPath(value string) bool {
	if value == "" {
		return true
	}
	if len(value) > 512 || !filepath.IsAbs(value) || filepath.Clean(value) != value || value == "/" {
		return false
	}
	for _, part := range strings.Split(strings.TrimPrefix(value, "/"), "/") {
		if !utils.SafePathSegment(part) {
			return false
		}
		for _, r := range part {
			if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-' && r != '.' {
				return false
			}
		}
	}
	return true
}

func validHealthPath(value string) bool {
	if value == "" {
		return true
	}
	if len(value) > 256 || !strings.HasPrefix(value, "/") || strings.ContainsAny(value, "?#\\\r\n\x00") {
		return false
	}
	cleaned := filepath.ToSlash(filepath.Clean(value))
	return cleaned == value && !strings.Contains(value, "..")
}

func validDefinitionCommand(command, binName string) bool {
	if len(command) > 2048 || hasControl(command) {
		return false
	}
	fields := strings.Fields(command)
	if len(fields) > 0 && fields[0] == binName {
		fields = fields[1:]
	}
	for _, field := range fields {
		if len(field) > 512 {
			return false
		}
		for _, r := range field {
			switch {
			case unicode.IsLetter(r), unicode.IsDigit(r):
			case strings.ContainsRune("-_=.:/,{}+@", r):
			default:
				return false
			}
		}
		if strings.Contains(field, "{") && !strings.Contains(field, "{port}") {
			return false
		}
	}
	return true
}

func validatePackageURL(raw string) error {
	if raw == "" || len(raw) > 2048 || hasControl(raw) {
		return fmt.Errorf("package_url 为必填项")
	}
	candidate := strings.ReplaceAll(raw, "{version}", "1.0.0")
	parsed, err := url.Parse(candidate)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return fmt.Errorf("package_url 必须是不含凭据和片段的 HTTPS 绝对地址")
	}
	if strings.Contains(raw, "{") && !strings.Contains(raw, "{version}") {
		return fmt.Errorf("package_url 仅支持 {version} 占位符")
	}
	return nil
}

func hasControl(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}
