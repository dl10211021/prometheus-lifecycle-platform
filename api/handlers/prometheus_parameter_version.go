package handlers

import (
	"fmt"
	"github.com/promlifecycle/platform/core/deployer"
	"github.com/promlifecycle/platform/models"
	"gorm.io/gorm"
)

// CreateComponent replaces Config and CustomArgs but retains ImportedArgs.
// Validate that merged value without changing the retained database object.
func validatePrometheusDeploymentParameterVersion(request *componentCreateRequest, existing *models.Component) error {
	if request.Type != string(models.ComponentPrometheus) || request.DeployType != "binary" {
		return nil
	}
	candidate := models.Component{}
	if existing != nil {
		candidate = *existing
	}
	snapshot, err := models.EmbedDeploymentParameters("", request.StructuredParameters)
	if err != nil {
		return err
	}
	candidate.Type = models.ComponentPrometheus
	candidate.DeployType = request.DeployType
	candidate.Version = request.Version
	candidate.Config = snapshot
	candidate.CustomArgs = request.CustomArgs
	return deployer.ValidatePrometheusParameterVersion(&candidate, request.Version)
}

func (h *Handler) preflightPrometheusParameterVersion(request *componentCreateRequest, host *models.Host, checks *[]deploymentPreflightCheck) error {
	if request.Type != string(models.ComponentPrometheus) || request.DeployType != "binary" {
		return nil
	}
	var existing *models.Component
	var problem error
	if request.ReplaceExisting {
		var retained models.Component
		err := h.db.Where("host_id = ? AND type = ? AND deploy_type <> ? AND status <> ?", host.ID, request.Type, "target", "removed").First(&retained).Error
		if err == nil {
			existing = &retained
		} else if err != gorm.ErrRecordNotFound {
			problem = fmt.Errorf("读取保留实例参数失败: %w", err)
		}
	}
	if problem == nil {
		problem = validatePrometheusDeploymentParameterVersion(request, existing)
	}
	status, message := "pass", "已检查有效启动参数的已知版本规则；不代表所有参数、配置或运行行为均已认证"
	source := "platform_effective_arguments"
	if existing != nil && existing.ImportedArgs != "" {
		source = "imported_snapshot"
		message = "已按保留的导入启动参数检查已知版本规则；结构化参数不会改写该快照"
	}
	if problem != nil {
		status = "fail"
		message = problem.Error()
	}
	*checks = append(*checks, deploymentPreflightCheck{Key: "parameter_version", Name: "Prometheus 参数版本", Status: status, Blocking: true, Message: message, Evidence: map[string]interface{}{"target_version": request.Version, "arguments_source": source, "scope": "known_cli_parameter_version_rules_only"}})
	return problem
}
