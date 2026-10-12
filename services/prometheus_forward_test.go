package services

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/promlifecycle/platform/models"
	"gorm.io/gorm"
)

type migrationReadinessTransport struct{}

func (migrationReadinessTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	body := `{"status":"success","data":{"result":[{}],"activeTargets":[{"health":"up"}],"groups":[]}}`
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

func newPrometheusForward(t *testing.T) (*compatibilityFixture, *models.ChangePlan, string) {
	t.Helper()
	f := newCompatibilityFixture(t, models.ComponentPrometheus, "2.55.0", "0.39.2")
	log := f.binary(t, "3.12.0")
	f.service.client = &http.Client{Transport: migrationReadinessTransport{}}
	before, _ := json.Marshal(prometheusBefore{Component: &f.component, Version: f.component.Version, Image: f.component.Image, ImportedArgsSHA256: importedArgsHash(f.component.ImportedArgs)})
	plan := f.plan(t, "2.55.0", "3.12.0", models.ChangeStatusDraft)
	plan.Before = before
	if err := f.db.Model(plan).Update("before", before).Error; err != nil {
		t.Fatal(err)
	}
	return f, plan, log
}

func TestPrometheusPrepareHashesActualHistoricalArgv(t *testing.T) {
	for _, argv := range []string{"", `["--web.config.file=/private/secret.yml"]`} {
		t.Run("argv"+string(rune('0'+len(argv)%10)), func(t *testing.T) {
			f, _, _ := newPrometheusForward(t)
			if argv != "" {
				actual, _ := json.Marshal([]string{"--config.file", f.component.ConfigPath, "--storage.tsdb.path=" + f.component.DataPath, "--web.config.file=/private/secret.yml"})
				argv = string(actual)
			}
			f.component.ImportedArgs = argv
			if err := f.db.AutoMigrate(&models.Package{}); err != nil {
				t.Fatal(err)
			}
			if err := f.db.Create(&models.ComponentVersion{Type: models.ComponentPrometheus, Version: "3.0.0+build.1", Source: "registered"}).Error; err != nil {
				t.Fatal(err)
			}
			before, after, _, _, evidence, err := f.service.prepare(context.Background(), ChangePlanRequest{Kind: models.ChangeKindUpgrade, After: json.RawMessage(`{"new_version":" v3.0.0+build.1 "}`)}, &f.component)
			if err != nil {
				t.Fatal(err)
			}
			var snapshot prometheusBefore
			if err := json.Unmarshal(before, &snapshot); err != nil {
				t.Fatal(err)
			}
			if snapshot.ImportedArgsSHA256 != importedArgsHash(argv) {
				t.Error("snapshot hash not bound to actual imported args")
			}
			if argv != "" && strings.Contains(string(before), "/private/secret.yml") {
				t.Error("imported args copied in plaintext")
			}
			if !strings.Contains(string(after), `"new_version":"3.0.0+build.1"`) {
				t.Errorf("target not normalized: %s", after)
			}
			raw, _ := json.Marshal(evidence)
			if !strings.Contains(string(raw), `"decision":"eligible"`) || !strings.Contains(string(raw), "not certified") {
				t.Errorf("missing scoped migration evidence: %s", raw)
			}
		})
	}
}

func TestPrometheusForwardUpgradePostMutationFailureQuarantinesUnsafeHistory(t *testing.T) {
	f, plan, log := newPrometheusForward(t)
	var reached atomic.Bool
	// Fail only after successful binary replacement, readiness and postflight.
	if err := f.db.Callback().Update().Before("gorm:update").Register("test:forward-completed", func(tx *gorm.DB) {
		if tx.Statement.Table == "change_baselines" && !reached.Swap(true) {
			var stored models.ChangePlan
			if err := tx.Session(&gorm.Session{NewDB: true}).First(&stored, plan.ID).Error; err != nil {
				tx.AddError(err)
				return
			}
			var backup models.Backup
			if stored.BackupID == nil {
				tx.AddError(errors.New("forward backup missing"))
				return
			}
			if err := tx.Session(&gorm.Session{NewDB: true}).First(&backup, *stored.BackupID).Error; err != nil {
				tx.AddError(err)
				return
			}
			if err := os.WriteFile(backup.Path, []byte("archive damaged after forward mutation"), 0600); err != nil {
				tx.AddError(err)
				return
			}
			tx.AddError(errors.New("post-forward persistence failed"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	_, err := f.service.Execute(context.Background(), plan.ID, ChangeActor{Role: "system_admin"})
	if !reached.Load() || !errors.Is(err, ErrPrometheusRecoveryAdmission) {
		t.Fatalf("post-forward quarantine boundary not reached: %v", err)
	}
	var current models.Component
	f.db.First(&current, f.component.ID)
	if current.Version != "3.12.0" {
		t.Errorf("unsafe historical metadata restored: %s", current.Version)
	}
	if raw, _ := os.ReadFile(current.BinaryPath); string(raw) != "historical-binary" {
		t.Errorf("forward binary not retained: %s", raw)
	}
	if raw, _ := os.ReadFile(log); strings.Count(string(raw), "restart compatibility-fixture") != 1 {
		t.Errorf("unsafe compensation restarted: %s", raw)
	}
	assertPrometheusBlocked(t, f)
}

func TestPrometheusFinalAdmissionRejectsWithoutAutomaticCompensation(t *testing.T) {
	f, plan, log := newPrometheusForward(t)
	var loaded *models.Component
	f.service.backups.runnerFactory = func(c *models.Component) (hostCommandRunner, error) { loaded = c; return backupControlRunner{}, nil }
	if err := f.db.Callback().Create().After("gorm:create").Register("test:late-argv-drift", func(tx *gorm.DB) {
		if tx.Statement.Table == "operations" && loaded != nil {
			loaded.ImportedArgs = `["--unexpected"]`
		}
	}); err != nil {
		t.Fatal(err)
	}
	_, err := f.service.Execute(context.Background(), plan.ID, ChangeActor{Role: "system_admin"})
	if !errors.Is(err, ErrPrometheusMigrationAdmission) {
		t.Fatalf("final migration admission missed drift: %v", err)
	}
	if raw, _ := os.ReadFile(log); len(raw) != 0 {
		t.Errorf("before-mutation rejection dispatched runtime or compensation: %s", raw)
	}
	var pauses int64
	f.db.Model(&ComponentBackupPause{}).Where("status = ?", "termination_unconfirmed").Count(&pauses)
	if pauses != 0 {
		t.Error("untouched service quarantined")
	}
	var current models.Component
	f.db.First(&current, f.component.ID)
	if current.Version != "2.55.0" {
		t.Error("final rejection changed database version")
	}
}

func TestPrometheusMigrationFinalGuardExcludesConcurrentOperations(t *testing.T) {
	f, plan, _ := newPrometheusForward(t)
	// Direct apply runs under the same host/component claims as Execute. Hold
	// the last companion check, then expose a newer source to the final rule.
	ctx, release, err := ClaimComponentVersionChange(context.Background(), f.db, f.component.ID, "execute")
	if err != nil {
		t.Fatal(err)
	}
	ready, proceed := make(chan struct{}), make(chan struct{})
	var once atomic.Bool
	f.db.Callback().Query().After("gorm:query").Register("test:migration-final-boundary", func(tx *gorm.DB) {
		if _, ok := tx.Statement.Dest.(*[]models.Component); ok && !once.Swap(true) {
			close(ready)
			<-proceed
			f.component.Version = "3.13.0"
		}
	})
	done := make(chan error, 1)
	go func() { done <- f.service.apply(ctx, plan, &f.component) }()
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		close(proceed)
		release()
		t.Fatal("guard not reached")
	}
	for _, host := range []uint{7, 99} {
		_, other, e := ClaimHostOperation(context.Background(), f.db, host, "other")
		if other != nil {
			other()
		}
		if host == 7 && !errors.Is(e, ErrComponentBusy) {
			t.Errorf("same host admitted: %v", e)
		}
		if host == 99 && e != nil {
			t.Errorf("unrelated host rejected: %v", e)
		}
	}
	_, other, e := ClaimComponentOperation(context.Background(), f.db, f.component.ID, "other")
	if other != nil {
		other()
	}
	if !errors.Is(e, ErrComponentBusy) {
		t.Error("same component admitted")
	}
	close(proceed)
	err = <-done
	release()
	if !errors.Is(err, ErrPrometheusMigrationAdmission) || !strings.Contains(err.Error(), "requires_restore") {
		t.Fatalf("final version drift missed: %v", err)
	}
	_, release, err = ClaimComponentVersionChange(context.Background(), f.db, f.component.ID, "retry")
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestPrometheusGenericSameVersionAndStatefulScope(t *testing.T) {
	for _, scope := range []string{"config", "data", "full"} {
		t.Run(scope, func(t *testing.T) {
			s, _, _ := newBackupTestService(t)
			c := backupTestComponent(t)
			persistBackupRestoreComponent(t, s, c)
			backup, _, err := s.CreateComponentBackup(context.Background(), c, "scope", scope)
			if err != nil {
				t.Fatal(err)
			}
			c.Version = " v3.5.0 "
			err = s.RestoreComponentBackup(context.Background(), backup, c)
			if scope == "data" {
				if !errors.Is(err, ErrPrometheusRecoveryAdmission) {
					t.Fatalf("data-only stateful restore admitted: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			// Verify/Rehearse remain archive inspection and allow older/newer versions.
			c.Version = "2.55.0"
			if _, err = s.VerifyComponentBackup(context.Background(), backup, c); err != nil {
				t.Fatal(err)
			}
			if _, err = s.RehearseComponentBackup(context.Background(), backup, c); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPrometheusForwardFailureRestoresPairedHistory(t *testing.T) {
	f, plan, log := newPrometheusForward(t)
	if err := os.WriteFile(filepath.Join(f.service.factory.BasePath(), "bin", "prometheus-2.55.0"), []byte("current-binary"), 0755); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("post-forward baseline persistence failed")
	var injected atomic.Bool
	f.db.Callback().Update().Before("gorm:update").Register("test:recoverable-forward-failure", func(tx *gorm.DB) {
		if tx.Statement.Table == "change_baselines" && !injected.Swap(true) {
			tx.AddError(cause)
		}
	})
	result, err := f.service.Execute(context.Background(), plan.ID, ChangeActor{Role: "system_admin"})
	if !errors.Is(err, cause) || result == nil || result.Status != models.ChangeStatusRolledBack {
		t.Fatalf("recoverable forward failure did not restore: %+v %v", result, err)
	}
	var current models.Component
	f.db.First(&current, f.component.ID)
	if current.Version != "2.55.0" {
		t.Errorf("historical version not persisted: %s", current.Version)
	}
	if raw, _ := os.ReadFile(current.BinaryPath); string(raw) != "current-binary" {
		t.Errorf("historical binary not restored: %s", raw)
	}
	if raw, _ := os.ReadFile(log); strings.Count(string(raw), "restart compatibility-fixture") != 2 {
		t.Errorf("expected forward and historical restarts: %s", raw)
	}
	_, release, err := ClaimComponentVersionChange(context.Background(), f.db, f.component.ID, "after-successful-compensation")
	if err != nil {
		t.Fatal(err)
	}
	release()
}
