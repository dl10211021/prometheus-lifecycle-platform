package services

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promlifecycle/platform/core/deployer"
	"github.com/promlifecycle/platform/models"
)

func TestPrometheusDockerHistoricalRecoveryPreservesExactImage(t *testing.T) {
	for _, mode := range []string{"v tag", "digest", "runtime failure", "stop failure", "remove failure"} {
		t.Run(mode, func(t *testing.T) {
			f := newCompatibilityFixture(t, models.ComponentPrometheus, "2.55.0", "0.39.2")
			dir := runtimeFixtureDir(t)
			commands := filepath.Join(dir, "commands")
			if err := os.Mkdir(commands, 0755); err != nil {
				t.Fatal(err)
			}
			log := filepath.Join(dir, "docker.log")
			script := "#!/bin/sh\nprintf '%s\n' \"$*\" >> \"$MIGRATION_DOCKER_LOG\"\nif [ \"$1\" = inspect ]; then printf true; fi\nif [ \"$MIGRATION_DOCKER_FAIL\" = \"$1\" ]; then exit 9; fi\nexit 0\n"
			if err := os.WriteFile(filepath.Join(commands, "docker"), []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", commands+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("MIGRATION_DOCKER_LOG", log)
			image := "prom/prometheus:v2.55.0"
			if mode == "digest" {
				image = "prom/prometheus@sha256:" + strings.Repeat("a", 64)
			}
			f.component.DeployType = "docker"
			f.component.Image = image
			f.component.ServiceName = "historical-prometheus"
			f.component.ConfigPath = filepath.Join(dir, "config")
			f.component.DataPath = filepath.Join(dir, "data")
			for _, path := range []string{f.component.ConfigPath, f.component.DataPath} {
				if err := os.MkdirAll(path, 0755); err != nil {
					t.Fatal(err)
				}
			}
			config := filepath.Join(f.component.ConfigPath, "prometheus.yml")
			data := filepath.Join(f.component.DataPath, "block")
			for path, contents := range map[string]string{config: "historical-config", data: "historical-data"} {
				if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.db.Omit("Host").Save(&f.component).Error; err != nil {
				t.Fatal(err)
			}
			before, _ := json.Marshal(prometheusBefore{Component: &f.component, Version: f.component.Version, Image: f.component.Image, ImportedArgsSHA256: importedArgsHash("")})
			var err error
			f.backup, _, err = f.service.backups.CreateComponentBackup(context.Background(), &f.component, "docker-history", "full")
			if err != nil {
				t.Fatal(err)
			}
			f.component.Version = "3.12.0"
			f.component.Image = "prom/prometheus:3.12.0"
			if err := f.db.Omit("Host").Save(&f.component).Error; err != nil {
				t.Fatal(err)
			}
			os.WriteFile(config, []byte("current-config"), 0600)
			os.WriteFile(data, []byte("current-data"), 0600)
			os.WriteFile(log, nil, 0600)
			plan := f.plan(t, "2.55.0", "3.12.0", models.ChangeStatusVerified)
			plan.Before = before
			f.db.Model(plan).Update("before", before)
			f.service.factory = deployer.NewFactory(dir)
			f.service.client = &http.Client{Transport: migrationReadinessTransport{}}
			f.service.backups.runnerFactory = func(*models.Component) (hostCommandRunner, error) { return prometheusDockerBackupRunner{}, nil }
			if mode == "runtime failure" {
				t.Setenv("MIGRATION_DOCKER_FAIL", "run")
			}
			if mode == "stop failure" {
				t.Setenv("MIGRATION_DOCKER_FAIL", "stop")
			}
			if mode == "remove failure" {
				t.Setenv("MIGRATION_DOCKER_FAIL", "rm")
			}
			_, err = f.service.Rollback(context.Background(), plan.ID, ChangeActor{Role: "system_admin"})
			if strings.Contains(mode, "failure") {
				if !errors.Is(err, deployer.ErrBinaryRecoveryUnconfirmed) {
					t.Fatalf("Docker failure lost quarantine: %v", err)
				}
				assertPrometheusBlocked(t, f)
			} else {
				if err != nil {
					t.Fatal(err)
				}
				var current models.Component
				f.db.First(&current, f.component.ID)
				if current.Image != image || current.Version != "2.55.0" {
					t.Errorf("historical image/version not preserved: %s %s", current.Image, current.Version)
				}
			}
			raw, _ := os.ReadFile(log)
			if mode == "stop failure" {
				if strings.Contains(string(raw), "rm ") || strings.Contains(string(raw), "run ") {
					t.Errorf("stop failure continued runtime commands: %s", raw)
				}
			} else if mode == "remove failure" {
				if strings.Contains(string(raw), "run ") {
					t.Errorf("remove failure continued runtime commands: %s", raw)
				}
			} else if !strings.Contains(string(raw), " "+image+" --config.file=") {
				t.Errorf("runtime did not use exact historical image: %s", raw)
			}
			for path, want := range map[string]string{config: "historical-config", data: "historical-data"} {
				if raw, _ := os.ReadFile(path); string(raw) != want {
					t.Errorf("Docker recovery lost data pairing: %s", raw)
				}
			}
		})
	}
}

// BackupService has already confirmed its own quiescence. Keep its transport
// distinct so stop/rm fault injection exercises the historical runtime callback
// after real historical data restoration, rather than the initial backup stop.
type prometheusDockerBackupRunner struct{}

func (prometheusDockerBackupRunner) Run(ctx context.Context, command string) (string, error) {
	if strings.HasPrefix(command, "docker inspect") {
		return "true", nil
	}
	if strings.HasPrefix(command, "docker stop") || strings.HasPrefix(command, "docker restart") || strings.Contains(command, "docker inspect -f") {
		return "", nil
	}
	return (backupControlRunner{}).Run(ctx, command)
}

func TestPrometheusDockerRecoveryRejectsMissingHistoricalImageBeforeCommands(t *testing.T) {
	f, plan, _ := newPrometheusHistory(t, "2.55.0", "")
	f.component.DeployType = "docker"
	var before prometheusBefore
	json.Unmarshal(plan.Before, &before)
	before.Component.DeployType = "docker"
	before.Component.Image = ""
	before.Image = ""
	plan.Before, _ = json.Marshal(before)
	err := f.service.rollback(context.Background(), plan, f.backup, &f.component)
	if !errors.Is(err, ErrPrometheusRecoveryAdmission) {
		t.Fatalf("missing historical image admitted: %v", err)
	}
	if len(f.commands) != 0 {
		t.Error("missing image reached host commands")
	}
}
