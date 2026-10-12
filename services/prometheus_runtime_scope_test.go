package services

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promlifecycle/platform/models"
)

func TestPrometheusFullRecoveryRejectsEffectivePathMismatch(t *testing.T) {
	for _, mode := range []string{"data override", "imported config equals", "imported data split", "imported missing paths", "docker shadow mount", "unsupported deployment"} {
		t.Run(mode, func(t *testing.T) {
			c := models.Component{Type: models.ComponentPrometheus, DeployType: "binary", Version: "2.55.0", Image: "prom/prometheus:2.55.0", ConfigPath: "/etc/owned/prometheus.yml", DataPath: "/srv/owned/data"}
			switch mode {
			case "data override":
				c.DataPathOverride = "/unbacked/data"
			case "imported config equals":
				c.ImportedArgs = `["--config.file=/unbacked/config.yml","--storage.tsdb.path=/srv/owned/data"]`
			case "imported data split":
				c.ImportedArgs = `["--config.file","/etc/owned/prometheus.yml","--storage.tsdb.path","/unbacked/data"]`
			case "imported missing paths":
				c.ImportedArgs = `["--web.enable-lifecycle"]`
			case "docker shadow mount":
				c.DeployType = "docker"
				c.ExtraVolumes = `[{"src":"/unbacked/data","dst":"/var/lib/prometheus"}]`
			case "unsupported deployment":
				c.DeployType = "windows"
			}
			if _, err := checkPrometheusMigration(&c, "3.0.0"); !errors.Is(err, ErrPrometheusMigrationAdmission) {
				t.Fatalf("unbacked effective runtime scope admitted: %v", err)
			}
		})
	}
}

func TestPrometheusHistoricalRecoveryRejectsUnbackedActualPaths(t *testing.T) {
	f, plan, _ := newPrometheusHistory(t, "2.55.0", "")
	// Both snapshots agree, yet the effective TSDB path is outside the archive.
	f.component.DataPathOverride = "/unbacked/data"
	var before prometheusBefore
	json.Unmarshal(plan.Before, &before)
	before.Component.DataPathOverride = f.component.DataPathOverride
	plan.Before, _ = json.Marshal(before)
	err := f.service.rollback(context.Background(), plan, f.backup, &f.component)
	if !errors.Is(err, ErrPrometheusRecoveryAdmission) {
		t.Fatalf("matching identity hid wrong effective data scope: %v", err)
	}
	if len(f.commands) != 0 {
		t.Error("bad runtime scope reached host commands")
	}
}

func TestPrometheusFullRecoveryRejectsSymlinkNamespaceAlias(t *testing.T) {
	f, plan, _ := newPrometheusHistory(t, "2.55.0", "")
	alias := filepath.Join(runtimeFixtureDir(t), "data-alias")
	if err := os.Symlink(f.component.DataPath, alias); err != nil {
		t.Fatal(err)
	}
	// A stable alias in both snapshots is still not a canonical data root.
	f.component.DataPath = alias
	var before prometheusBefore
	json.Unmarshal(plan.Before, &before)
	before.Component.DataPath = alias
	plan.Before, _ = json.Marshal(before)
	// Keep manifest binding honest: a distinct real backup was captured using
	// that declared alias while the component really held the historical version.
	historical := *before.Component
	if err := f.db.Omit("Host").Save(&historical).Error; err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(alias, "block"), []byte("before-data"), 0600); err != nil {
		t.Fatal(err)
	}
	var err error
	f.backup, _, err = f.service.backups.CreateComponentBackup(context.Background(), &historical, "symlink-history", "full")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Omit("Host").Save(&f.component).Error; err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(alias, "block"), []byte("current-data"), 0600); err != nil {
		t.Fatal(err)
	}
	plan.BackupID = &f.backup.ID
	f.db.Model(plan).Update("backup_id", f.backup.ID)
	f.commands = nil
	err = f.service.rollback(context.Background(), plan, f.backup, &f.component)
	if !errors.Is(err, ErrPrometheusRecoveryAdmission) {
		t.Fatalf("symlink scope accepted: %v", err)
	}
	for _, cmd := range f.commands {
		if !strings.HasPrefix(cmd, "readlink -m") && !strings.HasPrefix(cmd, "if [ -e") {
			t.Errorf("unproven alias reached archive or runtime: %s", cmd)
		}
	}
}

func TestPrometheusFullRecoveryRejectsNestedSymlinkData(t *testing.T) {
	f, plan, _ := newPrometheusHistory(t, "2.55.0", "")
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "wal-segment"), []byte("host-only-WAL"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(f.component.DataPath, "wal")); err != nil {
		t.Fatal(err)
	}
	f.commands = nil
	err := f.service.rollback(context.Background(), plan, f.backup, &f.component)
	if !errors.Is(err, ErrPrometheusRecoveryAdmission) {
		t.Fatalf("nested link could bind host-only WAL: %v", err)
	}
	for _, cmd := range f.commands {
		if !strings.HasPrefix(cmd, "readlink -m") && !strings.HasPrefix(cmd, "if [ -e") {
			t.Errorf("nested link reached archive/runtime command: %s", cmd)
		}
	}
	if raw, _ := os.ReadFile(filepath.Join(outside, "wal-segment")); string(raw) != "host-only-WAL" {
		t.Error("nested target changed")
	}
}
