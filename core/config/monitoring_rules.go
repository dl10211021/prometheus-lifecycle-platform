package config

import (
	"encoding/json"
	"fmt"
	"github.com/promlifecycle/platform/models"
	"gopkg.in/yaml.v3"
	"sort"
)

// buildAlertRulesYAML generates properly formatted Prometheus alert rule group YAML.
func buildAlertRulesYAML(rules []models.AlertRule) (string, error) {
	groups := groupRules(rules)

	type alertRule struct {
		Alert       string            `yaml:"alert"`
		Expr        string            `yaml:"expr"`
		For         string            `yaml:"for,omitempty"`
		Labels      map[string]string `yaml:"labels,omitempty"`
		Annotations map[string]string `yaml:"annotations,omitempty"`
	}
	type alertGroup struct {
		Name  string      `yaml:"name"`
		Rules []alertRule `yaml:"rules"`
	}
	type ruleFile struct {
		Groups []alertGroup `yaml:"groups"`
	}

	file := ruleFile{Groups: make([]alertGroup, 0, len(groups))}
	for _, group := range groups {
		g := alertGroup{Name: group.Name, Rules: make([]alertRule, 0, len(group.Rules))}
		for _, rule := range group.Rules {
			labels := make(map[string]string)
			if rule.Severity != "" {
				labels["severity"] = rule.Severity
			}
			if rule.Labels != "" {
				var extra map[string]string
				if err := json.Unmarshal([]byte(rule.Labels), &extra); err != nil {
					return "", fmt.Errorf("invalid labels for rule %d: %w", rule.ID, err)
				} else {
					for k, v := range extra {
						labels[k] = v
					}
				}
			}
			annotations := make(map[string]string)
			if rule.Annotations != "" {
				if err := json.Unmarshal([]byte(rule.Annotations), &annotations); err != nil {
					return "", fmt.Errorf("invalid annotations for rule %d: %w", rule.ID, err)
				}
			}
			g.Rules = append(g.Rules, alertRule{
				Alert:       rule.Name,
				Expr:        rule.Expression,
				For:         rule.Duration,
				Labels:      labels,
				Annotations: annotations,
			})
		}
		file.Groups = append(file.Groups, g)
	}

	data, err := yaml.Marshal(file)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// buildRecordingRulesYAML generates properly formatted Prometheus recording rule group YAML.
func buildRecordingRulesYAML(rules []models.RecordingRule) (string, error) {
	groups := groupRecordingRules(rules)

	type recordingRule struct {
		Record string            `yaml:"record"`
		Expr   string            `yaml:"expr"`
		Labels map[string]string `yaml:"labels,omitempty"`
	}
	type recordingGroup struct {
		Name  string          `yaml:"name"`
		Rules []recordingRule `yaml:"rules"`
	}
	type recordingFile struct {
		Groups []recordingGroup `yaml:"groups"`
	}

	file := recordingFile{Groups: make([]recordingGroup, 0, len(groups))}
	for _, group := range groups {
		g := recordingGroup{Name: group.Name, Rules: make([]recordingRule, 0, len(group.Rules))}
		for _, rule := range group.Rules {
			labels := make(map[string]string)
			if rule.Labels != "" {
				if err := json.Unmarshal([]byte(rule.Labels), &labels); err != nil {
					return "", fmt.Errorf("invalid labels for rule %d: %w", rule.ID, err)
				}
			}
			g.Rules = append(g.Rules, recordingRule{Record: rule.Name, Expr: rule.Expression, Labels: labels})
		}
		file.Groups = append(file.Groups, g)
	}

	data, err := yaml.Marshal(file)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// RecordingRuleGroup groups recording rules by group name.
type RecordingRuleGroup struct {
	Name  string
	Rules []models.RecordingRule
}

// groupRecordingRules groups recording rules by their group name.
func groupRecordingRules(rules []models.RecordingRule) []RecordingRuleGroup {
	groups := make(map[string][]models.RecordingRule)

	for _, rule := range rules {
		if rule.Group == "" {
			rule.Group = "recording_rules"
		}
		groups[rule.Group] = append(groups[rule.Group], rule)
	}

	var result []RecordingRuleGroup
	for name, rules := range groups {
		result = append(result, RecordingRuleGroup{Name: name, Rules: rules})
	}

	// Sort for deterministic output
	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})

	return result
}

type RuleGroup struct {
	Name  string
	Rules []models.AlertRule
}

func groupRules(rules []models.AlertRule) []RuleGroup {
	groups := make(map[string]*RuleGroup)
	for _, rule := range rules {
		gname := rule.Group
		if gname == "" {
			gname = "default"
		}
		if groups[gname] == nil {
			groups[gname] = &RuleGroup{Name: gname}
		}
		groups[gname].Rules = append(groups[gname].Rules, rule)
	}
	result := make([]RuleGroup, 0, len(groups))
	for _, g := range groups {
		result = append(result, *g)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}
