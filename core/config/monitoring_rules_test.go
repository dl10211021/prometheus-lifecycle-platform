package config

import (
	"github.com/promlifecycle/platform/models"
	"gopkg.in/yaml.v3"
	"strings"
	"testing"
)

func TestRuleSnapshotRetainsAnnotationsAndRejectsMalformedLabels(t *testing.T) {
	rules := []models.AlertRule{{ID: 1, Group: "z", Name: "Down", Expression: "up == 0", Annotations: `{"summary":"Exporter down","description":"Investigate {{ $labels.instance }}"}`}, {ID: 2, Group: "a", Name: "Other", Expression: "up == 1"}}
	data, err := buildAlertRulesYAML(rules)
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Groups []struct {
			Name  string `yaml:"name"`
			Rules []struct {
				Annotations map[string]string `yaml:"annotations"`
			} `yaml:"rules"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal([]byte(data), &file); err != nil {
		t.Fatal(err)
	}
	if file.Groups[0].Name != "a" || file.Groups[1].Rules[0].Annotations["summary"] != "Exporter down" || !strings.Contains(file.Groups[1].Rules[0].Annotations["description"], "$labels.instance") {
		t.Fatalf("rule ordering or annotations lost: %s", data)
	}
	rules[0].Annotations = `{"summary":false}`
	if _, err := buildAlertRulesYAML(rules); err == nil {
		t.Fatal("invalid annotations silently dropped")
	}
	recordings := []models.RecordingRule{{ID: 1, Name: "job:up", Expression: "sum(up)", Labels: `{"job":false}`}}
	if _, err := buildRecordingRulesYAML(recordings); err == nil {
		t.Fatal("invalid recording labels silently dropped")
	}
}
