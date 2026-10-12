package models

import (
	"regexp"
	"strconv"
	"strings"
)

// PrometheusParameterMigrationPreview is advisory evidence, never execution
// admission. Only the evidenced retention alias is rewritten; unknown flags,
// version upgrade paths, config and runtime compatibility require other checks.
type PrometheusParameterMigrationPreview struct {
	PreviewOnly         bool                                  `json:"preview_only"`
	Scope               string                                `json:"scope"`
	Rule                string                                `json:"rule"`
	SourceVersion       string                                `json:"source_version"`
	TargetVersion       string                                `json:"target_version"`
	ArgumentsSource     string                                `json:"arguments_source,omitempty"`
	Status              string                                `json:"status"`
	Reason              string                                `json:"reason"`
	SourceCompatible    bool                                  `json:"source_compatible"`
	CurrentCompatible   bool                                  `json:"current_compatible"`
	ConvertedCompatible bool                                  `json:"converted_compatible"`
	Changes             []PrometheusParameterMigrationChange  `json:"changes"`
	Blockers            []PrometheusParameterMigrationBlocker `json:"blockers"`
	// Never serialize argv: unknown options may contain credentials.
	Arguments []string `json:"-"`
}
type PrometheusParameterMigrationChange struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Value    string `json:"value"`
	Required bool   `json:"required"`
	Evidence string `json:"evidence"`
}
type PrometheusParameterMigrationBlocker struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func PreviewPrometheusArguments(source, target string, args []string) PrometheusParameterMigrationPreview {
	p := PrometheusParameterMigrationPreview{
		PreviewOnly: true, Scope: "known_cli_parameter_rules_only", Rule: "prometheus-retention-rename-v1",
		Status: "blocked", Changes: []PrometheusParameterMigrationChange{}, Blockers: []PrometheusParameterMigrationBlocker{},
	}
	block := func(code, message string) {
		p.Blockers = append(p.Blockers, PrometheusParameterMigrationBlocker{Code: code, Message: message})
	}
	sourceVersion, sourceOK := stableParameterVersion(source)
	targetVersion, targetOK := stableParameterVersion(target)
	if sourceOK {
		p.SourceVersion = strings.TrimSpace(source)
	}
	if targetOK {
		p.TargetVersion = strings.TrimSpace(target)
	}
	if !sourceOK || !targetOK {
		block("unsupported_version", "源版本和目标版本必须是完整的 Prometheus 2.x/3.x 正式版本。")
		p.Reason = "版本信息不足，不能生成迁移建议。"
		return p
	}
	// Bound imported metadata before scanning, copying or displaying it.
	total := 0
	for _, arg := range args {
		if len(args) > 4096 || len(arg) > 1024*1024-total {
			block("arguments_too_large", "保存的启动参数过大，需要人工核查。")
			p.Reason = "启动参数超出预览范围。"
			return p
		}
		total += len(arg)
	}
	p.SourceCompatible = ValidatePrometheusArgumentVersion(source, args) == nil
	p.CurrentCompatible = ValidatePrometheusArgumentVersion(target, args) == nil
	const oldFlag = "--storage.tsdb.retention"
	const newFlag = "--storage.tsdb.retention.time"
	oldCount, newCount, index := 0, 0, -1
	value, equals := "", false
	for i, arg := range args {
		if arg == "--" {
			block("ambiguous_arguments", "启动参数包含选项终止符，需人工核查参数边界。")
		}
		key, v, eq := strings.Cut(arg, "=")
		if key == newFlag {
			newCount++
		}
		if key != oldFlag {
			continue
		}
		oldCount++
		index = i
		value = v
		equals = eq
		if !eq && i+1 < len(args) {
			value = args[i+1]
		}
	}
	if oldCount > 1 {
		block("duplicate_retention", "旧保留时间参数重复，必须先明确唯一值。")
	}
	if oldCount > 0 && newCount > 0 {
		block("conflicting_retention", "新旧保留时间参数同时存在，不能推断应保留哪一个值。")
	}
	if oldCount > 0 && !validMigrationRetentionDuration(value) {
		block("invalid_retention_duration", "旧保留时间参数缺少有效时长；支持按顺序排列的 y、w、d、h、m、s、ms 和单独的 0，且不得溢出。")
	}
	// Older releases differ in duration grammar and zero-retention semantics.
	// 2.55 is a verified baseline, not a claimed feature introduction version.
	if oldCount > 0 && validMigrationRetentionDuration(value) &&
		(parameterVersionBefore(sourceVersion, [3]int{2, 55, 0}) || parameterVersionBefore(targetVersion, [3]int{2, 55, 0})) &&
		!legacyMigrationRetentionPattern.MatchString(value) {
		block("unverified_duration_semantics", "较早版本的复合时长语法或零值语义不在已验证范围，需要人工核查；不会替换为默认值。")
	}
	if !p.SourceCompatible {
		block("source_incompatible", "源版本与已知参数规则不兼容，需先核查保存的启动参数。")
	}
	candidate := append([]string(nil), args...)
	rename := oldCount == 1 && !parameterVersionBefore(targetVersion, [3]int{2, 7, 0})
	if len(p.Blockers) == 0 && rename {
		candidate[index] = newFlag
		if equals {
			candidate[index] += "=" + value
		}
	}
	if len(p.Blockers) == 0 && ValidatePrometheusArgumentVersion(target, candidate) != nil {
		block("target_incompatible", "转换后仍有参数不受目标版本支持；不会删除参数来强行通过检查。")
	}
	if len(p.Blockers) > 0 {
		p.Reason = "需要先解决阻塞项；未生成可用的转换结果。"
		return p
	}
	p.ConvertedCompatible = true
	p.Arguments = candidate
	p.Status = "not_needed"
	p.Reason = "已知规则范围内无需转换；仍需完整升级预检和原生验证。"
	if rename {
		p.Status = "available"
		p.Reason = "可逐字保留时长值并替换参数名；尚未应用，仍需完整升级预检和原生验证。"
		p.Changes = append(p.Changes, PrometheusParameterMigrationChange{From: oldFlag, To: newFlag, Value: value, Required: !parameterVersionBefore(targetVersion, [3]int{3, 0, 0}), Evidence: PrometheusArgumentVersionRule(oldFlag).Evidence})
	}
	return p
}

// This deliberately small grammar follows Prometheus common/model duration:
// https://github.com/prometheus/common/blob/v0.59.1/model/time.go
// Preserve the original spelling; check ordered unique integer units and signed
// nanosecond overflow. No float conversion or default-value substitution.
var legacyMigrationRetentionPattern = regexp.MustCompile(`^[0-9]*[1-9][0-9]*(y|w|d|h|m|s|ms)$`)

func validMigrationRetentionDuration(value string) bool {
	if value == "0" {
		return true
	}
	if value == "" || len(value) > 128 {
		return false
	}
	units := []string{"y", "w", "d", "h", "m", "s", "ms"}
	scales := []uint64{31536000000000000, 604800000000000, 86400000000000, 3600000000000, 60000000000, 1000000000, 1000000}
	const limit = uint64(1<<63 - 1)
	last, total := -1, uint64(0)
	for pos := 0; pos < len(value); {
		start := pos
		for pos < len(value) && value[pos] >= '0' && value[pos] <= '9' {
			pos++
		}
		if pos == start {
			return false
		}
		number, err := strconv.ParseUint(value[start:pos], 10, 64)
		if err != nil {
			return false
		}
		start = pos
		for pos < len(value) && value[pos] >= 'a' && value[pos] <= 'z' {
			pos++
		}
		unit := value[start:pos]
		rank := -1
		for i, u := range units {
			if unit == u {
				rank = i
				break
			}
		}
		if rank <= last || rank < 0 || number > (limit-total)/scales[rank] {
			return false
		}
		total += number * scales[rank]
		last = rank
	}
	return true
}
