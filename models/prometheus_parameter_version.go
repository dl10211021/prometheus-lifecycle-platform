package models

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ArgumentVersionRule is a scoped, evidenced CLI compatibility rule, not a
// certification of every option or behavior of a Prometheus release.
type ArgumentVersionRule struct {
	Argument       string `json:"argument"`
	MinimumVersion string `json:"minimum_version,omitempty"`
	RemovedVersion string `json:"removed_version,omitempty"`
	Replacement    string `json:"replacement,omitempty"`
	Evidence       string `json:"evidence"`
}

var ErrPrometheusParameterVersion = errors.New("Prometheus 参数版本检查未通过")

var prometheusArgumentRules = [...]ArgumentVersionRule{
	{Argument: "--storage.tsdb.retention.time", MinimumVersion: "2.7.0", Evidence: "https://github.com/prometheus/prometheus/blob/main/CHANGELOG.md#270--2019-01-28"},
	{Argument: "--storage.tsdb.retention.size", MinimumVersion: "2.7.0", Evidence: "https://github.com/prometheus/prometheus/blob/main/CHANGELOG.md#270--2019-01-28"},
	{Argument: "--storage.tsdb.wal-compression", MinimumVersion: "2.11.0", Evidence: "https://github.com/prometheus/prometheus/blob/main/CHANGELOG.md#2110--2019-07-09"},
	{Argument: "--no-storage.tsdb.wal-compression", MinimumVersion: "2.11.0", Evidence: "https://github.com/prometheus/prometheus/blob/main/CHANGELOG.md#2110--2019-07-09"},
	{Argument: "--storage.tsdb.retention", RemovedVersion: "3.0.0", Replacement: "--storage.tsdb.retention.time", Evidence: "https://prometheus.io/docs/prometheus/3.0/migration/"},
}

// Return a copy so a catalog consumer cannot change execution admission.
func PrometheusArgumentVersionRule(argument string) *ArgumentVersionRule {
	for _, rule := range prometheusArgumentRules {
		if rule.Argument == argument {
			return &rule
		}
	}
	return nil
}

var stableParameterVersionPattern = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)

func stableParameterVersion(raw string) ([3]int, bool) {
	var version [3]int
	match := stableParameterVersionPattern.FindStringSubmatch(strings.TrimSpace(raw))
	if match == nil {
		return version, false
	}
	for i := range version {
		n, err := strconv.Atoi(match[i+1])
		if err != nil {
			return version, false
		}
		version[i] = n
	}
	return version, version[0] == 2 || version[0] == 3
}

func parameterVersionBefore(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// ValidatePrometheusArgumentVersion inspects the actual argument list without
// altering flags or values. Unknown flags are outside this deliberately narrow
// rule set; passing it does not replace native config validation or runtime tests.
func ValidatePrometheusArgumentVersion(target string, args []string) error {
	version, known := stableParameterVersion(target)
	if !known {
		return fmt.Errorf("%w：请提供完整的 Prometheus 2.x/3.x 正式版本；未知版本、预发布和未来主版本需要单独验证", ErrPrometheusParameterVersion)
	}
	flags := make(map[string]bool)
	for _, arg := range args {
		key, _, _ := strings.Cut(arg, "=")
		flags[key] = true
	}
	for _, rule := range prometheusArgumentRules {
		if !flags[rule.Argument] {
			continue
		}
		if rule.MinimumVersion != "" {
			minimum, _ := stableParameterVersion(rule.MinimumVersion)
			if parameterVersionBefore(version, minimum) {
				return fmt.Errorf("%w：%s 要求版本 >= %s，目标为 %s；保留原参数并选择兼容版本，或完成参数迁移后重新预检。依据：%s", ErrPrometheusParameterVersion, rule.Argument, rule.MinimumVersion, target, rule.Evidence)
			}
		}
		if rule.RemovedVersion != "" {
			removed, _ := stableParameterVersion(rule.RemovedVersion)
			if !parameterVersionBefore(version, removed) {
				return fmt.Errorf("%w：%s 自 %s 移除，目标为 %s；请显式迁移到 %s 并重新预检，平台不会自动改写原参数。依据：%s", ErrPrometheusParameterVersion, rule.Argument, rule.RemovedVersion, target, rule.Replacement, rule.Evidence)
			}
		}
	}
	if flags["--storage.tsdb.retention"] && flags["--storage.tsdb.retention.time"] {
		return fmt.Errorf("%w：--storage.tsdb.retention 与 --storage.tsdb.retention.time 冲突；请显式选择一个保留时间参数，避免已有值被另一参数覆盖", ErrPrometheusParameterVersion)
	}
	return nil
}
