package deployer

import (
	"encoding/json"
	"github.com/promlifecycle/platform/models"
	"reflect"
	"strings"
	"testing"
)

func TestParameterMigrationPreviewUsesEffectiveImportedOverrides(t *testing.T) {
	c := models.Component{Type: models.ComponentPrometheus, DeployType: "binary", Version: "2.55.0", ImportedArgs: `["--storage.tsdb.retention","30d","--private=hidden-value"]`, CustomArgs: "--storage.tsdb.retention=045d", Config: `{"retention_time":"99d"}`}
	before := c
	p := PreviewPrometheusParameterMigration(&c, "3.1.0")
	if p.Status != "available" || p.ArgumentsSource != "imported_snapshot" || len(p.Changes) != 1 || p.Changes[0].Value != "045d" {
		t.Fatalf("%+v", p)
	}
	if !reflect.DeepEqual(c, before) {
		t.Fatal("preview mutated component")
	}
	raw, _ := json.Marshal(p)
	if strings.Contains(string(raw), "hidden-value") {
		t.Fatal("private argument leaked")
	}
	c.ImportedArgs = ""
	c.CustomArgs = ""
	p = PreviewPrometheusParameterMigration(&c, "3.1.0")
	if p.Status != "not_needed" || p.ArgumentsSource != "platform_effective_arguments" {
		t.Fatalf("%+v", p)
	}
}
func TestParameterMigrationPreviewRejectsMalformedSavedArguments(t *testing.T) {
	for _, c := range []models.Component{
		{Type: models.ComponentPrometheus, DeployType: "binary", Version: "2.55.0", ImportedArgs: "invalid-private-secret"},
		{Type: models.ComponentPrometheus, DeployType: "binary", Version: "2.55.0", ImportedArgs: "null"},
		{Type: models.ComponentPrometheus, DeployType: "binary", Version: "2.55.0", CustomArgs: "--storage.tsdb.retention=30d;private-secret"},
	} {
		p := PreviewPrometheusParameterMigration(&c, "3.1.0")
		if p.Status != "blocked" || p.Arguments != nil || p.SourceCompatible || p.CurrentCompatible {
			t.Fatalf("%+v", p)
		}
		raw, _ := json.Marshal(p)
		if strings.Contains(string(raw), "private-secret") {
			t.Fatal("malformed saved arguments leaked")
		}
	}
	for _, c := range []*models.Component{nil, {Type: models.ComponentThanos, DeployType: "binary"}, {Type: models.ComponentPrometheus, DeployType: "docker"}} {
		if p := PreviewPrometheusParameterMigration(c, "3.1.0"); p.Status != "not_applicable" {
			t.Fatalf("%+v", p)
		}
	}
}
