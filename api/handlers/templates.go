package handlers

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/gin-gonic/gin"
	"github.com/promlifecycle/platform/core/config"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"gorm.io/gorm"
)

type TemplateHandler struct {
	db *gorm.DB
}

func NewTemplateHandler(db *gorm.DB) *TemplateHandler {
	h := &TemplateHandler{db: db}
	h.preloadTemplates()
	return h
}

// ListTemplates returns all templates, optionally filtered by component type
func (h *TemplateHandler) ListTemplates(c *gin.Context) {
	var templates []models.ConfigTemplate
	query := h.db.Model(&models.ConfigTemplate{})

	if compType := c.Query("component_type"); compType != "" {
		query = query.Where("component_type = ?", compType)
	}
	if defaultOnly := c.Query("default_only"); defaultOnly == "true" {
		query = query.Where("is_default = ?", true)
	}

	if err := query.Order("created_at DESC").Find(&templates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取模板列表失败"})
		return
	}

	c.JSON(http.StatusOK, templates)
}

// GetTemplate returns a single template by ID
func (h *TemplateHandler) GetTemplate(c *gin.Context) {
	id := c.Param("id")
	var tmpl models.ConfigTemplate
	if err := h.db.First(&tmpl, id).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "模板不存在"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "读取模板失败"})
		}
		return
	}
	c.JSON(http.StatusOK, tmpl)
}

// CreateTemplate creates a new configuration template
func (h *TemplateHandler) CreateTemplate(c *gin.Context) {
	var input struct {
		Name          string `json:"name" binding:"required"`
		ComponentType string `json:"component_type" binding:"required"`
		Content       string `json:"content" binding:"required"`
		Description   string `json:"description"`
	}

	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "输入无效: " + err.Error()})
		return
	}
	if input.ComponentType == "_deployment_stack" {
		content, err := sanitizeDeploymentTemplate(input.Content)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		input.Content = content
	}

	// Validate YAML content if it looks like YAML
	if isYAMLContent(input.Content) {
		if err := validateYAML(input.Content); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "模板 YAML 校验失败，请修正后重试: " + err.Error()})
			return
		}
	}

	tmpl := models.ConfigTemplate{
		Name:          input.Name,
		ComponentType: input.ComponentType,
		Content:       input.Content,
		IsDefault:     false,
		Description:   input.Description,
	}

	if err := h.db.Create(&tmpl).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建模板失败"})
		return
	}

	c.JSON(http.StatusCreated, tmpl)
}

// UpdateTemplate updates an existing template (increments version)
func (h *TemplateHandler) UpdateTemplate(c *gin.Context) {
	id := c.Param("id")
	var tmpl models.ConfigTemplate
	if err := h.db.First(&tmpl, id).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "模板不存在"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "读取模板失败"})
		}
		return
	}

	var input struct {
		Name          string `json:"name"`
		ComponentType string `json:"component_type"`
		Content       string `json:"content"`
		Description   string `json:"description"`
		IsDefault     *bool  `json:"is_default"`
	}

	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "输入无效: " + err.Error()})
		return
	}
	componentType := tmpl.ComponentType
	if input.ComponentType != "" {
		componentType = input.ComponentType
	}
	if componentType == "_deployment_stack" {
		content := input.Content
		if content == "" {
			content = tmpl.Content
		}
		safe, err := sanitizeDeploymentTemplate(content)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		input.Content = safe
	}

	// Apply updates
	updates := make(map[string]interface{})
	if input.Name != "" {
		updates["name"] = input.Name
	}
	if input.ComponentType != "" {
		updates["component_type"] = input.ComponentType
	}
	if input.Content != "" {
		// Validate YAML if it looks like YAML
		if isYAMLContent(input.Content) {
			if err := validateYAML(input.Content); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "模板 YAML 校验失败，请修正后重试: " + err.Error()})
				return
			}
		}
		updates["content"] = input.Content
	}
	if input.Description != "" {
		updates["description"] = input.Description
	}
	if input.IsDefault != nil {
		updates["is_default"] = *input.IsDefault
	}

	if err := h.db.Model(&tmpl).Updates(updates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "更新模板失败"})
		return
	}

	// Reload to return updated data
	h.db.First(&tmpl, id)
	c.JSON(http.StatusOK, tmpl)
}

// DeleteTemplate deletes a template
func (h *TemplateHandler) DeleteTemplate(c *gin.Context) {
	id := c.Param("id")
	if err := h.db.Delete(&models.ConfigTemplate{}, id).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "模板不存在"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "删除模板失败"})
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "模板已删除"})
}

// DownloadTemplate serves the template content as a downloadable YAML file
func (h *TemplateHandler) DownloadTemplate(c *gin.Context) {
	id := c.Param("id")
	var tmpl models.ConfigTemplate
	if err := h.db.First(&tmpl, id).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "模板不存在"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "读取模板失败"})
		}
		return
	}

	filename := sanitizeFilename(tmpl.Name) + ".yaml"
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%s", filename))
	c.Header("Content-Type", "text/x-yaml")
	c.String(http.StatusOK, tmpl.Content)
}

// UploadTemplate accepts a YAML file and creates a template from it
func (h *TemplateHandler) UploadTemplate(c *gin.Context) {
	name := c.PostForm("name")
	compType := c.PostForm("component_type")
	description := c.PostForm("description")

	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name 不能为空"})
		return
	}
	if !utils.SafePathSegment(name) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "名称不能包含路径分隔符或特殊字符"})
		return
	}

	file, header, err := c.Request.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "读取上传文件失败，请重新选择文件: " + err.Error()})
		return
	}
	defer file.Close()

	// Read file content (limit to 10MB)
	maxSize := int64(10 * 1024 * 1024)
	limited := &limitedReader{file, maxSize}
	content, err := readAll(limited)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "操作失败: " + err.Error()})
		return
	}

	// Validate YAML
	if err := validateYAML(string(content)); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "模板 YAML 校验失败，请修正后重试: " + err.Error()})
		return
	}

	// Detect component type from filename if not provided
	if compType == "" {
		compType = detectComponentTypeFromFilename(header.Filename)
	}

	tmpl := models.ConfigTemplate{
		Name:          name,
		ComponentType: compType,
		Content:       string(content),
		IsDefault:     false,
		Description:   description,
	}

	if err := h.db.Create(&tmpl).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存模板失败"})
		return
	}

	c.JSON(http.StatusCreated, tmpl)
}

// preLoadTemplates creates default templates if none exist
func (h *TemplateHandler) preloadTemplates() {
	// Try to load templates from templates directory first
	h.loadTemplatesFromDisk()

	// Call LoadDefaultTemplates() to ensure defaults exist if no templates found
	if err := config.LoadDefaultTemplates(h.db); err != nil {
		fmt.Printf("warning: failed to load default templates: %v\n", err)
	}
}

// EnsureDefaults is a public wrapper for preloadTemplates, called by router
func (h *TemplateHandler) EnsureDefaults() error {
	h.preloadTemplates()
	return nil
}

// GetDefaultTemplateByType returns the first active template for a given component type
func (h *TemplateHandler) GetDefaultTemplateByType(c *gin.Context) {
	compType := c.Param("type")
	var tmpl models.ConfigTemplate
	if err := h.db.Where("component_type = ? AND is_default = ?", compType, true).
		Order("created_at ASC").First(&tmpl).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "未找到该组件类型的默认模板: " + compType})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "读取模板失败"})
		}
		return
	}
	c.JSON(http.StatusOK, tmpl)
}

func (h *TemplateHandler) loadTemplatesFromDisk() {
	// Check templates directory in the project root
	templatesDirs := []string{
		"./templates",
		filepath.Join(os.Getenv("HOME"), "promlifecycle", "templates"),
	}

	for _, dir := range templatesDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}

		for _, entry := range entries {
			if entry.IsDir() {
				h.loadTemplateDir(filepath.Join(dir, entry.Name()))
			} else if strings.HasSuffix(entry.Name(), ".yaml") || strings.HasSuffix(entry.Name(), ".yml") {
				h.loadTemplateFile(filepath.Join(dir, entry.Name()))
			}
		}
	}
}

func (h *TemplateHandler) loadTemplateDir(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if strings.HasSuffix(entry.Name(), ".yaml") || strings.HasSuffix(entry.Name(), ".yml") {
			h.loadTemplateFile(filepath.Join(dir, entry.Name()))
		}
	}
}

func (h *TemplateHandler) loadTemplateFile(path string) {
	content, err := os.ReadFile(path)
	if err != nil {
		return
	}

	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	compType := detectComponentTypeFromFilename(filepath.Base(path))

	tmpl := models.ConfigTemplate{
		Name:          name,
		ComponentType: compType,
		Content:       string(content),
		IsDefault:     false,
		Description:   "Loaded from " + path,
	}

	h.db.Create(&tmpl)
}

// Helper functions

type limitedReader struct {
	r io.Reader
	n int64
}

func (lr *limitedReader) Close() error {
	if closer, ok := lr.r.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

func (lr *limitedReader) Read(p []byte) (int, error) {
	if lr.n <= 0 {
		return 0, fmt.Errorf("file too large, max 10MB")
	}
	if int64(len(p)) > lr.n {
		p = p[:lr.n]
	}
	n, err := lr.r.Read(p)
	lr.n -= int64(n)
	return n, err
}

func readAll(r io.Reader) ([]byte, error) {
	buf := &bytes.Buffer{}
	_, err := buf.ReadFrom(r)
	return buf.Bytes(), err
}

func isYAMLContent(content string) bool {
	content = strings.TrimSpace(content)
	return strings.HasPrefix(content, "---") ||
		strings.Contains(content, ": ") && !strings.Contains(content, "func(") &&
			!strings.HasPrefix(content, "<")
}

func validateYAML(content string) error {
	// Basic YAML validation - check for common issues
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		// Check for tabs (YAML doesn't allow tabs for indentation)
		if strings.Contains(line, "\t") && !strings.HasPrefix(trimmed, "- ") {
			return fmt.Errorf("line %d: tabs not allowed in YAML indentation", i+1)
		}
	}
	return nil
}

func detectComponentTypeFromFilename(filename string) string {
	name := strings.ToLower(filename)
	switch {
	case strings.Contains(name, "prometheus"):
		return string(models.ComponentPrometheus)
	case strings.Contains(name, "alertmanager"):
		return string(models.ComponentAlertmanager)
	case strings.Contains(name, "grafana"):
		return string(models.ComponentGrafana)
	case strings.Contains(name, "loki"):
		return string(models.ComponentLoki)
	case strings.Contains(name, "promtail"):
		return string(models.ComponentPromtail)
	case strings.Contains(name, "blackbox"):
		return string(models.ComponentBlackbox)
	default:
		return "unknown"
	}
}

// RenderTemplate renders a template with given data using Go text/template
func (h *TemplateHandler) RenderTemplate(c *gin.Context) {
	id := c.Param("id")
	var tmpl models.ConfigTemplate
	if err := h.db.First(&tmpl, id).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "模板不存在"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "读取模板失败"})
		}
		return
	}

	var data map[string]interface{}
	if err := c.ShouldBindJSON(&data); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "模板渲染数据无效，请检查字段后重试"})
		return
	}

	tmplObj, err := template.New("config").Parse(tmpl.Content)
	if err != nil {
		// If not a Go template, return raw content
		c.JSON(http.StatusOK, gin.H{"content": tmpl.Content, "rendered": false})
		return
	}

	var buf bytes.Buffer
	if err := tmplObj.Execute(&buf, data); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "模板渲染失败，请检查模板变量: " + err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"content": buf.String(), "rendered": true})
}
