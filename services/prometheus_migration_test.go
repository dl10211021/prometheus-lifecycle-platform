package services

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/promlifecycle/platform/models"
)

func TestPrometheusMigrationRejectsBeforeWrites(t *testing.T) {
	for _, mode := range []string{"preflight", "execute", "apply"} {
		t.Run(mode, func(t *testing.T) {
			f := newCompatibilityFixture(t, models.ComponentPrometheus, "3.11.0", "0.39.2")
			log := f.binary(t, "3.10.0")
			plan := f.plan(t, "3.11.0", "3.10.0", models.ChangeStatusDraft)
			var old models.Component
			f.db.First(&old, f.component.ID)
			var oldPlan models.ChangePlan
			f.db.First(&oldPlan, plan.ID)
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
				_, _, err = f.service.preflightUpgrade(context.Background(), &f.component, upgradeChange{NewVersion: "3.10.0"})
			case "execute":
				_, err = f.service.Execute(context.Background(), plan.ID, ChangeActor{Role: "system_admin"})
			case "apply":
				err = f.service.apply(context.Background(), plan, &f.component)
			}
			if err == nil || !strings.Contains(err.Error(), "requires_restore") {
				t.Errorf("direct downgrade must give historical recovery guidance, got %v", err)
			}
			var current models.Component
			f.db.First(&current, f.component.ID)
			var currentPlan models.ChangePlan
			f.db.First(&currentPlan, plan.ID)
			if !reflect.DeepEqual(old, current) || !reflect.DeepEqual(oldPlan, currentPlan) || counts() != beforeCounts {
				t.Error("early rejection mutated component, plan, backup or operations")
			}
			if len(f.commands) != 0 {
				t.Errorf("early rejection ran archive commands: %v", f.commands)
			}
			if raw, _ := os.ReadFile(log); len(raw) != 0 {
				t.Errorf("early rejection touched runtime: %s", raw)
			}
			_, release, claimErr := ClaimComponentVersionChange(context.Background(), f.db, f.component.ID, "retry")
			if claimErr != nil {
				t.Fatal(claimErr)
			}
			release()
		})
	}
}

func TestPrometheusGenericRestoreRejectsDifferentVersion(t *testing.T) {
	s, _, _ := newBackupTestService(t)
	c := backupTestComponent(t)
	backup, _, err := s.CreateComponentBackup(context.Background(), c, "historical", "full")
	if err != nil {
		t.Fatal(err)
	}
	c.Version = "2.55.0"
	persistBackupRestoreComponent(t, s, c)
	block := filepath.Join(c.DataPath, "block")
	if err := os.WriteFile(block, []byte("current-data"), 0600); err != nil {
		t.Fatal(err)
	}
	var commands []string
	s.runnerFactory = func(*models.Component) (hostCommandRunner, error) {
		return backupControlRunner{before: func(_ context.Context, command string) error { commands = append(commands, command); return nil }}, nil
	}
	err = s.RestoreComponentBackup(context.Background(), backup, c)
	if err == nil {
		t.Error("generic restore admitted cross-version archive")
	}
	if raw, _ := os.ReadFile(block); string(raw) != "current-data" {
		t.Errorf("mismatched archive wrote data: %s", raw)
	}
	for _, command := range commands {
		if strings.Contains(command, "systemctl") || strings.Contains(command, ".restore-") {
			t.Errorf("unsafe restore dispatched %s", command)
		}
	}
}

func TestPrometheusHistoricalRestoreRequiresSnapshot(t *testing.T) {
	f := newCompatibilityFixture(t, models.ComponentPrometheus, "3.0.0", "0.39.2")
	log := f.binary(t, "2.55.0")
	plan := f.plan(t, "2.55.0", "3.0.0", models.ChangeStatusVerified)
	_, err := f.service.Rollback(context.Background(), plan.ID, ChangeActor{Role: "system_admin"})
	if err == nil {
		t.Error("missing historical component snapshot admitted")
	}
	if f.component.Version != "3.0.0" {
		t.Error("unsafe in-memory version mutation")
	}
	if raw, _ := os.ReadFile(log); len(raw) != 0 {
		t.Errorf("unsafe runtime mutation: %s", raw)
	}
}

func TestPrometheusMigrationVersionMatrix(t *testing.T) {
	for _, tc := range []struct{ source, target, decision, direction string }{
		{"2.54.9", "3.0.0", "requires_intermediate", "upgrade"}, {"2.0.0", "3.12.0", "requires_intermediate", "upgrade"},
		{"2.53.5", "3.0.0", "requires_intermediate", "upgrade"}, {"2.55.0", "3.0.0", "eligible", "upgrade"},
		{"2.55.99", "3.12.0", "eligible", "upgrade"}, {"2.54.9", "2.55.1", "eligible", "upgrade"},
		{"3.9.9", "3.10.0", "eligible", "upgrade"}, {"3.10.0", "3.9.9", "requires_restore", "downgrade"},
		{"3.0.0", "2.55.0", "requires_restore", "downgrade"}, {"2.55.1", "2.55.0", "requires_restore", "downgrade"},
		{" v2.55.0+build.1 ", "2.55.0+other", "no_upgrade", "equal"}, {"v2.55.0", " 3.0.0+build ", "eligible", "upgrade"},
		{"3.0.0", "3.0.0", "no_upgrade", "equal"}, {"unknown", "3.0.0", "unknown", "unknown"},
		{"2.55.0", "latest", "unknown", "unknown"}, {"", "3.0.0", "unknown", "unknown"},
		{"2.55.0-rc.1", "3.0.0", "unknown", "unknown"}, {"2.55.0", "3.0.0-rc.1", "unknown", "unknown"},
		{"2.55", "3.0.0", "unknown", "unknown"}, {"02.55.0", "3.0.0", "unknown", "unknown"},
		{"2.055.0", "3.0.0", "unknown", "unknown"}, {"2.55.0", "4.0.0", "unknown", "unknown"},
		{"4.0.0", "3.0.0", "unknown", "unknown"}, {"2.55.0", "3.0.0+", "unknown", "unknown"},
		{"2.55.0", "3.0.99999999999999999999999999999", "unknown", "unknown"},
		{"2.99999999999999999999999999999.0", "3.0.0", "unknown", "unknown"},
	} {
		t.Run(tc.source+"_to_"+tc.target, func(t *testing.T) {
			c := models.Component{Type: models.ComponentPrometheus, DeployType: "binary", Version: tc.source, ConfigPath: "/etc/prom.yml", DataPath: "/var/lib/prom"}
			e, err := checkPrometheusMigration(&c, tc.target)
			if e.Decision != tc.decision || e.Direction != tc.direction || errors.Is(err, ErrPrometheusMigrationAdmission) != (tc.decision != "eligible") {
				t.Fatalf("decision=%+v err=%v", e, err)
			}
			if e.Scope == "" || e.Rule == "" || len(e.Provenance) == 0 || e.Reason == "" {
				t.Fatal("missing rule evidence")
			}
		})
	}
	for _, c := range []models.Component{{Type: models.ComponentPrometheus, DeployType: "target", Version: "3.0.0"}, {Type: models.ComponentThanos, DeployType: "binary", Version: "0.39.0"}} {
		e, err := checkPrometheusMigration(&c, "unknown")
		if err != nil || e.Decision != "not_applicable" {
			t.Fatalf("out-of-scope component blocked: %+v %v", e, err)
		}
	}
}

func TestPrometheusMigrationScopeAndLegacyPlanAdmission(t *testing.T) {
	for _, mode := range []string{"no paths", "config only", "data only", "relative", "missing snapshot", "hidden argv"} {
		t.Run(mode, func(t *testing.T) {
			f := newCompatibilityFixture(t, models.ComponentPrometheus, "2.55.0", "0.39.2")
			f.component.DeployType = "binary"
			f.component.DataPath = "/var/lib/prom"
			switch mode {
			case "no paths":
				f.component.ConfigPath = ""
				f.component.DataPath = ""
			case "config only":
				f.component.DataPath = ""
			case "data only":
				f.component.ConfigPath = ""
			case "relative":
				f.component.DataPath = "data"
			case "hidden argv":
				f.component.ImportedArgs = "sensitive-token"
			}
			if err := f.db.Omit("Host").Save(&f.component).Error; err != nil {
				t.Fatal(err)
			}
			plan := f.plan(t, "2.55.0", "3.0.0", models.ChangeStatusDraft)
			if mode == "hidden argv" {
				raw, _ := json.Marshal(prometheusBefore{Component: &f.component, Version: f.component.Version, Image: f.component.Image})
				plan.Before = raw
				f.db.Model(plan).Update("before", raw)
			}
			_, err := f.service.Execute(context.Background(), plan.ID, ChangeActor{Role: "system_admin"})
			if !errors.Is(err, ErrPrometheusMigrationAdmission) {
				t.Fatalf("unsafe forward admitted: %v", err)
			}
			if strings.Contains(err.Error(), "sensitive-token") {
				t.Fatal("secret leaked")
			}
			var current models.ChangePlan
			f.db.First(&current, plan.ID)
			if current.Status != models.ChangeStatusDraft || len(f.commands) != 0 {
				t.Error("early rejection mutated state")
			}
		})
	}
}
