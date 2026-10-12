package deployer

import (
	"encoding/json"
	"github.com/promlifecycle/platform/models"
	"strings"
)

// PreviewPrometheusParameterMigration uses the same saved effective arguments
// as binary execution. It never reads a live host, persists state or authorizes
// execution. Imported snapshots and custom overrides retain their precedence.
func PreviewPrometheusParameterMigration(component *models.Component, target string) models.PrometheusParameterMigrationPreview {
	source := ""
	if component != nil {
		source = component.Version
	}
	p := models.PreviewPrometheusArguments(source, target, nil)
	reject := func(code, message string) models.PrometheusParameterMigrationPreview {
		p.Status = "blocked"
		p.Reason = message
		p.SourceCompatible = false
		p.CurrentCompatible = false
		p.ConvertedCompatible = false
		p.Arguments = nil
		p.Blockers = []models.PrometheusParameterMigrationBlocker{{Code: code, Message: message}}
		return p
	}
	if component == nil || component.Type != models.ComponentPrometheus || component.DeployType != "binary" {
		p.Status = "not_applicable"
		p.Reason = "仅支持 Linux 二进制 Prometheus 的保存参数预览。"
		p.Blockers = []models.PrometheusParameterMigrationBlocker{}
		p.Arguments = nil
		p.SourceCompatible = false
		p.CurrentCompatible = false
		p.ConvertedCompatible = false
		return p
	}
	basis := "platform_effective_arguments"
	if component.ImportedArgs != "" {
		basis = "imported_snapshot"
	}
	p.ArgumentsSource = basis
	if len(component.ImportedArgs) > 2*1024*1024 || len(component.CustomArgs) > 1024*1024 {
		return reject("arguments_too_large", "保存的启动参数过大，需要人工核查。")
	}
	if component.ImportedArgs != "" {
		var args []string
		if err := json.Unmarshal([]byte(component.ImportedArgs), &args); err != nil || args == nil {
			return reject("invalid_saved_arguments", "导入的启动参数快照不是有效的参数数组，需要重新核查。")
		}
	}
	for _, line := range strings.Split(component.CustomArgs, "\n") {
		if arg := strings.TrimSpace(line); arg != "" && !validCustomArg(arg) {
			// effectiveArgs logs and skips malformed custom args. A preview must instead
			// fail visibly without passing raw potentially secret values to that logger.
			return reject("invalid_saved_arguments", "自定义启动参数包含无效行，需要先修正。")
		}
	}
	spec, err := binarySpecForComponent(component)
	if err != nil {
		return reject("invalid_saved_arguments", "无法读取保存的启动参数，需要重新核查组件配置。")
	}
	p = models.PreviewPrometheusArguments(source, target, effectiveArgs(spec, component))
	p.ArgumentsSource = basis
	return p
}
