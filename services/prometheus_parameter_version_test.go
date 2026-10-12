package services

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/promlifecycle/platform/models"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestPrometheusParameterVersionUpgradeRejectsBeforeWrites(t *testing.T) {
	for _, mode := range []string{"preflight", "execute", "apply"} {
		t.Run(mode, func(t *testing.T) {
			f := newCompatibilityFixture(t, models.ComponentPrometheus, "2.55.0", "0.39.2")
			log := f.binary(t, "3.1.0")
			args, _ := json.Marshal([]string{"--config.file=" + f.component.ConfigPath, "--storage.tsdb.path=" + f.component.DataPath, "--storage.tsdb.retention=45d"})
			f.component.ImportedArgs = string(args)
			if err := f.db.Omit("Host").Save(&f.component).Error; err != nil {
				t.Fatal(err)
			}
			plan := f.plan(t, "2.55.0", "3.1.0", models.ChangeStatusDraft)
			var before models.Component
			f.db.First(&before, f.component.ID)
			var beforePlan models.ChangePlan
			f.db.First(&beforePlan, plan.ID)
			counts := func() [2]int64 {
				var n [2]int64
				f.db.Model(&models.Backup{}).Count(&n[0])
				f.db.Model(&models.Operation{}).Count(&n[1])
				return n
			}
			beforeCounts := counts()
			var err error
			switch mode {
			case "preflight":
				_, _, err = f.service.preflightUpgrade(context.Background(), &f.component, upgradeChange{NewVersion: "3.1.0"})
			case "execute":
				_, err = f.service.Execute(context.Background(), plan.ID, ChangeActor{Role: "system_admin"})
			case "apply":
				err = f.service.apply(context.Background(), plan, &f.component)
			}
			if !errors.Is(err, ErrPrometheusMigrationAdmission) || !strings.Contains(err.Error(), "storage.tsdb.retention") {
				t.Errorf("expected before-mutation parameter rejection, got %v", err)
			}
			var after models.Component
			f.db.First(&after, f.component.ID)
			var afterPlan models.ChangePlan
			f.db.First(&afterPlan, plan.ID)
			if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(beforePlan, afterPlan) || counts() != beforeCounts {
				t.Error("parameter rejection mutated persistent state")
			}
			if len(f.commands) != 0 {
				t.Errorf("parameter rejection reached archive commands: %v", f.commands)
			}
			if raw, _ := os.ReadFile(log); len(raw) != 0 {
				t.Errorf("parameter rejection touched runtime: %s", raw)
			}
		})
	}
}

func TestPrometheusParameterVersionImportedBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, source, target string
		flags                []string
		want                 string
	}{
		{"legacy existing instance", "2.5.0", "2.6.1", []string{"--storage.tsdb.retention", "45d"}, ""},
		{"size before introduction", "2.5.0", "2.6.1", []string{"--storage.tsdb.retention.size=50GB"}, "2.7.0"},
		{"time first release", "2.6.1", "2.7.0", []string{"--storage.tsdb.retention.time=45d"}, ""},
		{"wal before introduction", "2.9.0", "2.10.0", []string{"--storage.tsdb.wal-compression"}, "2.11.0"},
		{"negative wal before introduction", "2.9.0", "2.10.0", []string{"--no-storage.tsdb.wal-compression"}, "2.11.0"},
		{"wal first release", "2.10.0", "2.11.0", []string{"--storage.tsdb.wal-compression"}, ""},
		{"legacy removed", "2.55.0", "3.0.0", []string{"--storage.tsdb.retention=45d"}, "storage.tsdb.retention.time"},
		{"mixed aliases", "2.54.0", "2.55.0", []string{"--storage.tsdb.retention=45d", "--storage.tsdb.retention.time=30d"}, "冲突"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"--config.file=/etc/prometheus.yml", "--storage.tsdb.path=/var/lib/prometheus"}, tc.flags...)
			raw, _ := json.Marshal(args)
			c := models.Component{Type: models.ComponentPrometheus, DeployType: "binary", Version: tc.source, ConfigPath: "/etc/prometheus.yml", DataPath: "/var/lib/prometheus", ImportedArgs: string(raw)}
			before := c
			_, err := checkPrometheusMigration(&c, tc.target)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want %q", err, tc.want)
			}
			if !reflect.DeepEqual(c, before) {
				t.Fatal("imported snapshot was rewritten")
			}
		})
	}
}
