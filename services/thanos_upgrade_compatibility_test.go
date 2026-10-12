package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/promlifecycle/platform/core/deployer"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/promlifecycle/platform/models"
	"gorm.io/gorm"
)

// Removing the target guard admits unknown/old Thanos versions and misses
// retained Prometheus instances after the first compatible one.
func TestThanosUpgradePreflightCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name, target, version, status, deploy string
		host                                  uint
		omit, queryFail, block                bool
	}{
		{name: "compatible", target: "0.37.0", version: "3.0.0", status: "running", deploy: "binary", host: 7},
		{name: "old target", target: "0.36.1", version: "3.0.0", status: "running", deploy: "binary", host: 7, block: true},
		{name: "unknown target", target: "latest", version: "2.55.0", status: "running", deploy: "binary", host: 7, block: true},
		{name: "empty target", version: "2.55.0", status: "running", deploy: "binary", host: 7, block: true},
		{name: "prerelease target", target: "0.37.0-rc.0", version: "2.55.0", status: "running", deploy: "binary", host: 7, block: true},
		{name: "unknown dependency", target: "0.37.0", version: "unknown", status: "running", deploy: "binary", host: 7, block: true},
		{name: "stopped second", target: "0.36.1", version: "3.0.0", status: "stopped", deploy: "binary", host: 7, block: true},
		{name: "failed second", target: "0.36.1", version: "3.0.0", status: "failed", deploy: "binary", host: 7, block: true},
		{name: "unknown status", target: "0.36.1", version: "3.0.0", status: "unknown", deploy: "binary", host: 7, block: true},
		{name: "removed", target: "0.36.1", version: "3.0.0", status: "removed", deploy: "binary", host: 7},
		{name: "observed target", target: "0.36.1", version: "3.0.0", status: "running", deploy: "target", host: 7},
		{name: "other host", target: "0.36.1", version: "3.0.0", status: "running", deploy: "binary", host: 8},
		{name: "missing dependency", target: "0.37.0", omit: true, block: true},
		{name: "query failure", target: "0.37.0", queryFail: true, block: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := lifecycleQueueDB(t)
			if !tc.omit {
				for _, c := range []models.Component{
					{Name: "first-prom", Type: models.ComponentPrometheus, HostID: 7, Version: "2.55.0", Status: models.StatusRunning, DeployType: "binary"},
					{Name: "second-prom", Type: models.ComponentPrometheus, HostID: tc.host, Version: tc.version, Status: models.ComponentStatus(tc.status), DeployType: tc.deploy},
				} {
					if err := db.Create(&c).Error; err != nil {
						t.Fatal(err)
					}
				}
			}
			if tc.queryFail {
				if err := db.Migrator().DropTable(&models.Component{}); err != nil {
					t.Fatal(err)
				}
			}
			component := models.Component{Name: "sidecar", Type: models.ComponentThanos, HostID: 7, Version: "0.39.2", Status: models.StatusRunning, DeployType: "binary"}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			evidence, risk, err := NewChangeService(db, nil, nil, nil, t.TempDir()).preflightUpgrade(ctx, &component, upgradeChange{NewVersion: tc.target})
			if errors.Is(err, ErrDependencyCompatibility) != tc.block {
				t.Fatalf("compatibility block=%v want %v: %v", errors.Is(err, ErrDependencyCompatibility), tc.block, err)
			}
			raw, _ := json.Marshal(evidence)
			if evidence["dependency_scope"] != "same_host" || !strings.Contains(string(raw), "dependent_compatibility") {
				t.Errorf("missing scope/evidence: %s", raw)
			}
			if tc.block && risk != models.ChangeRiskHigh {
				t.Errorf("risk=%s", risk)
			}
			if tc.block && err != nil && !strings.Contains(err.Error(), "Prometheus") {
				t.Errorf("missing actionable companion: %v", err)
			}
			if !tc.omit && !tc.queryFail && !strings.Contains(string(raw), "first-prom") {
				t.Errorf("missing retained-instance evidence: %s", raw)
			}
		})
	}
}

type compatibilityFixture struct {
	db        *gorm.DB
	service   *ChangeService
	component models.Component
	companion models.Component
	backup    *models.Backup
	commands  []string
}

func newCompatibilityFixture(t *testing.T, kind models.ComponentType, version, companionVersion string) *compatibilityFixture {
	t.Helper()
	db := lifecycleQueueDB(t)
	if err := db.AutoMigrate(&models.Host{}, &models.Cluster{}, &models.Backup{}, &models.BackupManifest{}, &models.ComponentConfig{}, &models.ComponentVersion{}, &ComponentRestoreCheckpoint{}); err != nil {
		t.Fatal(err)
	}
	f := &compatibilityFixture{db: db}
	host := models.Host{ID: 7, IP: "127.0.0.1", ConnectionType: "local"}
	if err := db.Create(&host).Error; err != nil {
		t.Fatal(err)
	}
	dir := runtimeFixtureDir(t)
	f.component = models.Component{Name: "changing", Type: kind, HostID: 7, Host: host, Version: version, Image: "current-image", Status: models.StatusRunning, DeployType: "target", ConfigPath: filepath.Join(dir, "config")}
	if err := os.WriteFile(f.component.ConfigPath, []byte("before-config"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := db.Omit("Host").Create(&f.component).Error; err != nil {
		t.Fatal(err)
	}
	companionType := models.ComponentPrometheus
	if kind == models.ComponentPrometheus {
		companionType = models.ComponentThanos
	}
	f.companion = models.Component{Name: "live-companion", Type: companionType, HostID: 7, Version: companionVersion, Status: models.StatusStopped, DeployType: "binary"}
	if err := db.Create(&f.companion).Error; err != nil {
		t.Fatal(err)
	}
	backups := NewBackupServiceWithRoot(db, filepath.Join(dir, "backups"))
	backups.runnerFactory = func(*models.Component) (hostCommandRunner, error) {
		return backupControlRunner{before: func(_ context.Context, command string) error { f.commands = append(f.commands, command); return nil }}, nil
	}
	var err error
	f.backup, _, err = backups.CreateComponentBackup(context.Background(), &f.component, "historical", "config")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.component.ConfigPath, []byte("current-config"), 0600); err != nil {
		t.Fatal(err)
	}
	f.service = NewChangeService(db, nil, nil, backups, dir)
	f.service.incidents = nil
	f.commands = nil
	return f
}

func (f *compatibilityFixture) plan(t *testing.T, before, target string, status models.ChangeStatus) *models.ChangePlan {
	t.Helper()
	previous, _ := json.Marshal(map[string]string{"version": before, "image": "historical-image"})
	after, _ := json.Marshal(upgradeChange{NewVersion: target})
	plan := &models.ChangePlan{Kind: models.ChangeKindUpgrade, ResourceType: "component", ResourceID: f.component.ID, Status: status, Before: previous, After: after, BackupID: &f.backup.ID}
	if err := f.db.Create(plan).Error; err != nil {
		t.Fatal(err)
	}
	baseline := models.ChangeBaseline{PlanID: plan.ID, Fingerprint: componentFingerprint(&f.component), AppliedFingerprint: componentFingerprint(&f.component)}
	if err := f.db.Create(&baseline).Error; err != nil {
		t.Fatal(err)
	}
	return plan
}

func (f *compatibilityFixture) unchanged(t *testing.T, plan *models.ChangePlan, action func() error) {
	t.Helper()
	var priorPlan models.ChangePlan
	if err := f.db.First(&priorPlan, plan.ID).Error; err != nil {
		t.Fatal(err)
	}
	var prior models.Component
	if err := f.db.First(&prior, f.component.ID).Error; err != nil {
		t.Fatal(err)
	}
	memoryVersion, memoryImage := f.component.Version, f.component.Image
	artifact, err := os.ReadFile(f.backup.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range []any{&models.Operation{}, &models.Backup{}} {
		var count int64
		if err := f.db.Model(model).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		defer func(model any, count int64) {
			var after int64
			f.db.Model(model).Count(&after)
			if after != count {
				t.Errorf("%T count changed %d -> %d", model, count, after)
			}
		}(model, count)
	}
	if err := action(); !errors.Is(err, ErrDependencyCompatibility) {
		t.Errorf("wanted compatibility rejection: %v", err)
	}
	var current models.Component
	f.db.First(&current, prior.ID)
	var currentPlan models.ChangePlan
	f.db.First(&currentPlan, priorPlan.ID)
	if !reflect.DeepEqual(prior, current) {
		t.Errorf("component changed: before=%+v after=%+v", prior, current)
	}
	if !reflect.DeepEqual(priorPlan, currentPlan) {
		t.Errorf("plan changed: before=%+v after=%+v", priorPlan, currentPlan)
	}
	if f.component.Version != memoryVersion || f.component.Image != memoryImage {
		t.Errorf("in-memory version/image mutated")
	}
	if raw, _ := os.ReadFile(f.backup.Path); !bytes.Equal(raw, artifact) {
		t.Error("backup content changed")
	}
	if raw, _ := os.ReadFile(f.component.ConfigPath); string(raw) != "current-config" {
		t.Errorf("live config changed: %s", raw)
	}
	if len(f.commands) != 0 {
		t.Errorf("rejected operation dispatched backup/runtime commands: %v", f.commands)
	}
	var pauses int64
	f.db.Model(&ComponentBackupPause{}).Where("status = ?", "termination_unconfirmed").Count(&pauses)
	if pauses != 0 {
		t.Error("manual rejection quarantined untouched component")
	}
	_, release, err := ClaimComponentVersionChange(context.Background(), f.db, f.component.ID, "after-rejection")
	if err != nil {
		t.Errorf("claim leaked: %v", err)
	} else {
		release()
	}
}

func TestThanosUpgradeExecutionRejectsHistoricalPlanDependencyDrift(t *testing.T) {
	f := newCompatibilityFixture(t, models.ComponentThanos, "0.39.2", "2.55.0")
	plan := f.plan(t, "0.39.2", "0.36.1", models.ChangeStatusDraft)
	if err := f.db.Model(&f.companion).Update("version", "3.0.0").Error; err != nil {
		t.Fatal(err)
	}
	f.unchanged(t, plan, func() error {
		_, err := f.service.Execute(context.Background(), plan.ID, ChangeActor{Role: "system_admin"})
		return err
	})
}

func TestThanosUpgradeApplyRejectsBeforeMutation(t *testing.T) {
	f := newCompatibilityFixture(t, models.ComponentThanos, "0.39.2", "3.0.0")
	plan := f.plan(t, "0.39.2", "0.36.1", models.ChangeStatusDraft)
	f.unchanged(t, plan, func() error { return f.service.apply(context.Background(), plan, &f.component) })
}

func TestHistoricalUpgradeRollbackRejectsDependencyDrift(t *testing.T) {
	for _, tc := range []struct {
		kind                            models.ComponentType
		current, before, original, live string
	}{
		{models.ComponentThanos, "0.39.2", "0.36.1", "2.55.0", "3.0.0"},
		{models.ComponentPrometheus, "2.55.0", "3.0.0", "0.37.0", "0.36.1"},
		{models.ComponentThanos, "0.39.2", "unknown", "2.55.0", "2.55.0"},
		{models.ComponentPrometheus, "2.55.0", "unknown", "0.37.0", "0.37.0"},
	} {
		t.Run(fmt.Sprint(tc.kind)+tc.before, func(t *testing.T) {
			f := newCompatibilityFixture(t, tc.kind, tc.current, tc.original)
			plan := f.plan(t, tc.before, tc.current, models.ChangeStatusVerified)
			if err := f.db.Model(&f.companion).Update("version", tc.live).Error; err != nil {
				t.Fatal(err)
			}
			f.unchanged(t, plan, func() error {
				_, err := f.service.Rollback(context.Background(), plan.ID, ChangeActor{Role: "system_admin"})
				return err
			})
		})
	}
}

func TestSharedUpgradeRollbackRejectsBeforeInMemoryMutation(t *testing.T) {
	f := newCompatibilityFixture(t, models.ComponentThanos, "0.39.2", "3.0.0")
	plan := f.plan(t, "0.36.1", "0.39.2", models.ChangeStatusVerified)
	f.unchanged(t, plan, func() error { return f.service.rollback(context.Background(), plan, f.backup, &f.component) })
}

func TestAutomaticUpgradeRollbackQuarantinesIncompatibleHistory(t *testing.T) {
	f := newCompatibilityFixture(t, models.ComponentThanos, "0.39.2", "3.0.0")
	plan := f.plan(t, "0.36.1", "0.39.2", models.ChangeStatusExecuting)
	op := models.Operation{ComponentID: &f.component.ID, Status: "running", Type: "change-UPGRADE"}
	if err := f.db.Create(&op).Error; err != nil {
		t.Fatal(err)
	}
	ctx, release, err := ClaimComponentVersionChange(context.Background(), f.db, f.component.ID, "execute")
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.service.rollbackAfterFailure(ctx, plan, f.backup, &f.component, &op, "verify", errors.New("forward verification failed"))
	release()
	if !errors.Is(err, ErrDependencyCompatibility) {
		t.Errorf("lost incompatible recovery error: %v", err)
	}
	var current models.Component
	f.db.First(&current, f.component.ID)
	if current.Version != "0.39.2" || f.component.Version != "0.39.2" {
		t.Errorf("unsafe historical version restored: %s/%s", current.Version, f.component.Version)
	}
	if raw, _ := os.ReadFile(current.ConfigPath); string(raw) != "current-config" {
		t.Errorf("unsafe backup restored: %s", raw)
	}
	if len(f.commands) != 0 {
		t.Errorf("unsafe compensation dispatched commands: %v", f.commands)
	}
	f.db.First(plan, plan.ID)
	if plan.Status != models.ChangeStatusFailed || !strings.Contains(string(plan.RollbackEvidence), "live-companion") {
		t.Errorf("missing failed recovery evidence: %+v", plan)
	}
	var pause ComponentBackupPause
	if err := f.db.First(&pause, "component_id = ?", current.ID).Error; err != nil || !strings.Contains(pause.Error, "live-companion") {
		t.Errorf("missing durable recovery block: %+v %v", pause, err)
	}
	if err := RecoverInterruptedLifecycle(f.db); err != nil {
		t.Fatal(err)
	}
	if _, release, err := ClaimComponentVersionChange(context.Background(), f.db, current.ID, "after-restart"); err == nil {
		release()
		t.Error("unsafe later operation admitted")
	}
}

// The command transport is replaced, while real local binary files, SQLite,
// archive creation/restoration and HTTP readiness exercise the production flow.
func (f *compatibilityFixture) binary(t *testing.T, historical string) string {
	t.Helper()
	dir := runtimeFixtureDir(t)
	commandLog := filepath.Join(dir, "runtime.log")
	commands := filepath.Join(dir, "commands")
	if err := os.MkdirAll(commands, 0755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s\n' \"$*\" >> \"$COMPATIBILITY_RUNTIME_LOG\"\nexit 0\n"
	if err := os.WriteFile(filepath.Join(commands, "systemctl"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMPATIBILITY_RUNTIME_LOG", commandLog)
	t.Setenv("PATH", commands+string(os.PathListSeparator)+os.Getenv("PATH"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	t.Cleanup(server.Close)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	f.component.Port, _ = strconv.Atoi(port)
	f.component.DeployType = "binary"
	f.component.ServiceName = "compatibility-fixture"
	f.component.InstallPath = filepath.Join(dir, "install")
	f.component.BinaryPath = filepath.Join(f.component.InstallPath, string(f.component.Type))
	f.component.DataPath = filepath.Join(dir, "data")
	for _, path := range []string{f.component.InstallPath, f.component.DataPath, filepath.Join(dir, "bin")} {
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatal(err)
		}
	}
	for path, contents := range map[string]string{f.component.BinaryPath: "current-binary", filepath.Join(f.component.DataPath, "block"): "before-data", filepath.Join(dir, "bin", string(f.component.Type)+"-"+historical): "historical-binary"} {
		if err := os.WriteFile(path, []byte(contents), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.db.Omit("Host").Save(&f.component).Error; err != nil {
		t.Fatal(err)
	}
	var err error
	f.backup, _, err = f.service.backups.CreateComponentBackup(context.Background(), &f.component, "full-history", "full")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.component.DataPath, "block"), []byte("current-data"), 0600); err != nil {
		t.Fatal(err)
	}
	f.service.factory = deployer.NewFactory(dir)
	f.commands = nil
	if err := os.WriteFile(commandLog, nil, 0600); err != nil {
		t.Fatal(err)
	}
	return commandLog
}

func TestHistoricalUpgradeBinaryRollbackCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		kind                       models.ComponentType
		current, before, companion string
		block                      bool
	}{
		{"thanos compatible", models.ComponentThanos, "0.39.2", "0.37.0", "3.0.0", false},
		{"thanos incompatible", models.ComponentThanos, "0.39.2", "0.36.1", "3.0.0", true},
		{"prometheus compatible", models.ComponentPrometheus, "3.0.0", "2.55.0", "0.37.0", false},
		{"prometheus incompatible", models.ComponentPrometheus, "2.55.0", "3.0.0", "0.36.1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Capture the backup and snapshot while this really is the historical
			// version; only then advance the live metadata to the current version.
			f := newCompatibilityFixture(t, tc.kind, tc.before, tc.companion)
			f.component.Image = "historical-image"
			commandLog := f.binary(t, tc.before)
			historical, _ := json.Marshal(prometheusBefore{Component: &f.component, Version: f.component.Version, Image: f.component.Image})
			f.component.Version, f.component.Image = tc.current, "current-image"
			if err := f.db.Omit("Host").Save(&f.component).Error; err != nil {
				t.Fatal(err)
			}
			plan := f.plan(t, tc.before, tc.current, models.ChangeStatusVerified)
			plan.Before = historical
			if err := f.db.Model(plan).Update("before", historical).Error; err != nil {
				t.Fatal(err)
			}
			action := func() error {
				_, err := f.service.Rollback(context.Background(), plan.ID, ChangeActor{Role: "system_admin"})
				return err
			}
			if tc.block {
				f.unchanged(t, plan, action)
				if raw, _ := os.ReadFile(commandLog); len(raw) != 0 {
					t.Errorf("rejected rollback touched runtime: %s", raw)
				}
				if raw, _ := os.ReadFile(f.component.BinaryPath); string(raw) != "current-binary" {
					t.Errorf("rejected rollback replaced binary: %s", raw)
				}
				if raw, _ := os.ReadFile(filepath.Join(f.component.DataPath, "block")); string(raw) != "current-data" {
					t.Errorf("rejected rollback restored data: %s", raw)
				}
			} else {
				if err := action(); err != nil {
					t.Fatal(err)
				}
				var current models.Component
				f.db.First(&current, f.component.ID)
				if current.Version != tc.before || current.Status != models.StatusRunning {
					t.Errorf("compatible rollback not applied: %s/%s", current.Version, current.Status)
				}
				if raw, _ := os.ReadFile(f.component.BinaryPath); string(raw) != "historical-binary" {
					t.Errorf("old executable not installed: %s", raw)
				}
				if raw, _ := os.ReadFile(filepath.Join(f.component.DataPath, "block")); string(raw) != "before-data" {
					t.Errorf("old data not restored: %s", raw)
				}
				if raw, _ := os.ReadFile(commandLog); !strings.Contains(string(raw), "restart compatibility-fixture") {
					t.Errorf("compatible runtime never restarted: %s", raw)
				}
				f.db.First(plan, plan.ID)
				if plan.Status != models.ChangeStatusRolledBack {
					t.Errorf("plan status=%s", plan.Status)
				}
			}
		})
	}
}

func TestThanosUpgradeCompatibleHistoricalExecution(t *testing.T) {
	f := newCompatibilityFixture(t, models.ComponentThanos, "0.36.1", "3.0.0")
	plan := f.plan(t, "0.36.1", "0.37.0", models.ChangeStatusDraft)
	result, err := f.service.Execute(context.Background(), plan.ID, ChangeActor{Role: "system_admin"})
	if err != nil {
		t.Fatal(err)
	}
	f.db.First(&f.component, f.component.ID)
	if f.component.Version != "0.37.0" || result.Status != models.ChangeStatusVerified {
		t.Fatalf("compatible repair failed: %s/%s", f.component.Version, result.Status)
	}
}

func TestHistoricalUpgradeRollbackMissingOrUnreadableCompanion(t *testing.T) {
	for _, failure := range []string{"missing", "query"} {
		t.Run(failure, func(t *testing.T) {
			f := newCompatibilityFixture(t, models.ComponentThanos, "0.39.2", "3.0.0")
			plan := f.plan(t, "0.37.0", "0.39.2", models.ChangeStatusVerified)
			if failure == "missing" {
				if err := f.db.Delete(&f.companion).Error; err != nil {
					t.Fatal(err)
				}
			} else {
				if err := f.db.Callback().Query().Before("gorm:query").Register("test:dependency-query-failure", func(tx *gorm.DB) {
					if _, ok := tx.Statement.Dest.(*[]models.Component); ok {
						tx.AddError(errors.New("dependency query unavailable"))
					}
				}); err != nil {
					t.Fatal(err)
				}
			}
			f.unchanged(t, plan, func() error {
				_, err := f.service.Rollback(context.Background(), plan.ID, ChangeActor{Role: "system_admin"})
				return err
			})
		})
	}
}

func TestUpgradeCompatibilityHostExclusionAtFinalGuard(t *testing.T) {
	for _, mode := range []string{"thanos execution", "thanos rollback", "prometheus rollback"} {
		t.Run(mode, func(t *testing.T) {
			kind, current, previous, companion := models.ComponentThanos, "0.39.2", "0.37.0", "3.0.0"
			if mode == "prometheus rollback" {
				kind, current, previous, companion = models.ComponentPrometheus, "3.0.0", "2.55.0", "0.37.0"
			}
			f := newCompatibilityFixture(t, kind, current, companion)
			status := models.ChangeStatusVerified
			if mode == "thanos execution" {
				status = models.ChangeStatusDraft
			}
			plan := f.plan(t, previous, current, status)
			other := models.ChangePlan{Kind: models.ChangeKindUpgrade, ResourceID: f.companion.ID, Status: models.ChangeStatusDraft}
			if err := f.db.Create(&other).Error; err != nil {
				t.Fatal(err)
			}
			ready, proceed := make(chan struct{}), make(chan struct{})
			var queries atomic.Int32
			threshold := int32(1)
			if mode == "thanos execution" {
				threshold = 2
			}
			if err := f.db.Callback().Query().After("gorm:query").Register("test:compatibility-boundary", func(tx *gorm.DB) {
				if _, ok := tx.Statement.Dest.(*[]models.Component); ok && queries.Add(1) == threshold {
					close(ready)
					<-proceed
					tx.AddError(errors.New("final check query failed"))
				}
			}); err != nil {
				t.Fatal(err)
			}
			actor := ChangeActor{Role: "system_admin"}
			done := make(chan error, 1)
			go func() {
				var err error
				if mode == "thanos execution" {
					_, err = f.service.Execute(context.Background(), plan.ID, actor)
				} else {
					_, err = f.service.Rollback(context.Background(), plan.ID, actor)
				}
				done <- err
			}()
			select {
			case <-ready:
			case err := <-done:
				t.Fatalf("guard not reached: %v", err)
			case <-time.After(10 * time.Second):
				close(proceed)
				t.Fatal("guard barrier timed out")
			}
			_, release, deployErr := ClaimHostOperation(context.Background(), f.db, 7, "deploy")
			if release != nil {
				release()
			}
			_, executeErr := f.service.Execute(context.Background(), other.ID, actor)
			_, rollbackErr := f.service.Rollback(context.Background(), other.ID, actor)
			_, releaseOther, otherErr := ClaimHostOperation(context.Background(), f.db, 8, "unrelated-deploy")
			if releaseOther != nil {
				releaseOther()
			}
			close(proceed)
			select {
			case err := <-done:
				if !errors.Is(err, ErrDependencyCompatibility) {
					t.Errorf("lost final rejection: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("worker failed to finish")
			}
			for name, err := range map[string]error{"deploy": deployErr, "upgrade": executeErr, "rollback": rollbackErr} {
				if !errors.Is(err, ErrComponentBusy) {
					t.Errorf("same-host %s not excluded: %v", name, err)
				}
			}
			if otherErr != nil {
				t.Errorf("different host blocked: %v", otherErr)
			}
			_, release, err := ClaimComponentVersionChange(context.Background(), f.db, f.component.ID, "after-rejected-guard")
			if err != nil {
				t.Fatal(err)
			}
			release()
		})
	}
}

func TestAutomaticUpgradeBinaryFailureBlocksUnsafeRuntimeCompensation(t *testing.T) {
	f := newCompatibilityFixture(t, models.ComponentThanos, "0.36.1", "3.0.0")
	commandLog := f.binary(t, "0.39.2")
	if err := os.WriteFile(filepath.Join(f.service.factory.BasePath(), "bin", "thanos-0.36.1"), []byte("incompatible-old-binary"), 0755); err != nil {
		t.Fatal(err)
	}
	plan := f.plan(t, "0.36.1", "0.39.2", models.ChangeStatusDraft)
	var reached atomic.Bool
	if err := f.db.Callback().Update().Before("gorm:update").Register("test:post-mutation-failure", func(tx *gorm.DB) {
		if tx.Statement.Table == "change_baselines" && !reached.Swap(true) {
			tx.AddError(errors.New("record applied baseline failed"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	_, err := f.service.Execute(context.Background(), plan.ID, ChangeActor{Role: "system_admin"})
	if !reached.Load() {
		t.Fatalf("forward binary mutation did not finish: %v", err)
	}
	if !errors.Is(err, ErrDependencyCompatibility) {
		t.Errorf("expected incompatible automatic compensation: %v", err)
	}
	var current models.Component
	if err := f.db.First(&current, f.component.ID).Error; err != nil {
		t.Fatal(err)
	}
	if current.Version != "0.39.2" {
		t.Errorf("unsafe metadata restored: %s", current.Version)
	}
	if raw, _ := os.ReadFile(current.BinaryPath); string(raw) != "historical-binary" {
		t.Errorf("unsafe old executable restored: %s", raw)
	}
	if raw, _ := os.ReadFile(filepath.Join(current.DataPath, "block")); string(raw) != "current-data" {
		t.Errorf("unexpected rollback data: %s", raw)
	}
	raw, readErr := os.ReadFile(commandLog)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Count(string(raw), "restart compatibility-fixture") != 1 {
		t.Errorf("expected only the forward restart, got: %s", raw)
	}
	for _, command := range f.commands {
		if strings.Contains(command, ".restore-full-") {
			t.Errorf("unsafe full backup compensation dispatched: %s", command)
		}
	}
	f.db.First(plan, plan.ID)
	if plan.Status != models.ChangeStatusFailed || !strings.Contains(string(plan.RollbackEvidence), "live-companion") {
		t.Errorf("missing failure evidence: %s %s", plan.Status, plan.RollbackEvidence)
	}
	var pause ComponentBackupPause
	if err := f.db.First(&pause, "component_id = ?", current.ID).Error; err != nil || !strings.Contains(pause.Error, "live-companion") {
		t.Errorf("missing recovery quarantine: %+v %v", pause, err)
	}
	if err := RecoverInterruptedLifecycle(f.db); err != nil {
		t.Fatal(err)
	}
	if _, release, err := ClaimComponentVersionChange(context.Background(), f.db, current.ID, "unsafe-retry"); err == nil {
		release()
		t.Error("later operation admitted after unsafe compensation")
	}
}

func TestThanosUpgradeExecutionRejectsUnconfirmedDependencies(t *testing.T) {
	for _, failure := range []string{"missing", "query", "unknown target"} {
		t.Run(failure, func(t *testing.T) {
			f := newCompatibilityFixture(t, models.ComponentThanos, "0.39.2", "3.0.0")
			target := "0.37.0"
			if failure == "unknown target" {
				target = "latest"
			}
			plan := f.plan(t, "0.39.2", target, models.ChangeStatusDraft)
			if failure == "missing" {
				if err := f.db.Delete(&f.companion).Error; err != nil {
					t.Fatal(err)
				}
			}
			if failure == "query" {
				if err := f.db.Callback().Query().Before("gorm:query").Register("test:unavailable-live-companions", func(tx *gorm.DB) {
					if _, ok := tx.Statement.Dest.(*[]models.Component); ok {
						tx.AddError(errors.New("dependency query unavailable"))
					}
				}); err != nil {
					t.Fatal(err)
				}
			}
			f.unchanged(t, plan, func() error {
				_, err := f.service.Execute(context.Background(), plan.ID, ChangeActor{Role: "system_admin"})
				return err
			})
		})
	}
}

func TestThanosUpgradeNullRetainedMetadataStillBlocks(t *testing.T) {
	f := newCompatibilityFixture(t, models.ComponentThanos, "0.39.2", "3.0.0")
	if err := f.db.Model(&f.companion).Updates(map[string]any{"status": nil, "deploy_type": nil}).Error; err != nil {
		t.Fatal(err)
	}
	plan := f.plan(t, "0.39.2", "0.36.1", models.ChangeStatusDraft)
	f.unchanged(t, plan, func() error {
		_, err := f.service.Execute(context.Background(), plan.ID, ChangeActor{Role: "system_admin"})
		return err
	})
}
