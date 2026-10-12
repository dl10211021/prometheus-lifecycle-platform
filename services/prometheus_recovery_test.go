package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/promlifecycle/platform/core/deployer"
	"github.com/promlifecycle/platform/models"
	"gorm.io/gorm"
)

// The backup is made at the historical version, with historical config/data.
// Advance the live state only AFTER capturing its actual runtime identity.
func newPrometheusHistory(t *testing.T, version, argv string) (*compatibilityFixture, *models.ChangePlan, string) {
	t.Helper()
	f := newCompatibilityFixture(t, models.ComponentPrometheus, version, "0.39.2")
	f.component.Image = "historical-image"
	f.component.ImportedArgs = argv
	if err := os.WriteFile(f.component.ConfigPath, []byte("historical-config"), 0600); err != nil {
		t.Fatal(err)
	}
	log := f.binary(t, strings.TrimPrefix(strings.TrimSpace(version), "v"))
	if argv != "" {
		actual, _ := json.Marshal([]string{"--config.file=" + f.component.ConfigPath, "--storage.tsdb.path", f.component.DataPath, "--web.enable-lifecycle"})
		f.component.ImportedArgs = string(actual)
		argv = f.component.ImportedArgs
		if err := os.WriteFile(filepath.Join(f.component.DataPath, "block"), []byte("before-data"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := f.db.Omit("Host").Save(&f.component).Error; err != nil {
			t.Fatal(err)
		}
		var err error
		f.backup, _, err = f.service.backups.CreateComponentBackup(context.Background(), &f.component, "full-history-with-actual-argv", "full")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(f.component.DataPath, "block"), []byte("current-data"), 0600); err != nil {
			t.Fatal(err)
		}
		f.commands = nil
	}
	historical := f.component
	before, _ := json.Marshal(prometheusBefore{Component: &historical, Version: historical.Version, Image: historical.Image, ImportedArgsSHA256: importedArgsHash(argv)})
	f.component.Version = "3.12.0"
	f.component.Image = "current-image"
	if err := f.db.Omit("Host").Save(&f.component).Error; err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.component.ConfigPath, []byte("current-config"), 0600); err != nil {
		t.Fatal(err)
	}
	plan := f.plan(t, version, "3.12.0", models.ChangeStatusVerified)
	plan.Before = before
	if err := f.db.Model(plan).Update("before", before).Error; err != nil {
		t.Fatal(err)
	}
	return f, plan, log
}

func assertPrometheusBlocked(t *testing.T, f *compatibilityFixture) {
	t.Helper()
	var pause ComponentBackupPause
	if err := f.db.First(&pause, "component_id = ?", f.component.ID).Error; err != nil || pause.Status != "termination_unconfirmed" {
		t.Fatalf("missing durable quarantine: %+v %v", pause, err)
	}
	if err := RecoverInterruptedLifecycle(f.db); err != nil {
		t.Fatal(err)
	}
	if err := RecoverInterruptedComponentRestores(context.Background(), f.db); err == nil {
		t.Error("startup accepted unsafe recovery")
	}
	_, release, err := ClaimComponentVersionChange(context.Background(), f.db, f.component.ID, "after-restart")
	if release != nil {
		release()
	}
	if err == nil {
		t.Error("later lifecycle operation admitted")
	}
}

func TestPrometheusHistoricalRecoveryBindingMatrix(t *testing.T) {
	for _, mode := range []string{"nil backup", "no association", "wrong association", "missing backup", "missing manifest", "wrong component", "wrong type", "wrong version", "unknown version", "manifest scope", "config only", "data only", "consistency", "format", "paths", "checksum", "size", "missing archive", "corrupt archive", "malformed snapshot", "missing snapshot", "scalar version", "scalar image", "snapshot id", "hidden argv", "changed argv", "database query"} {
		t.Run(mode, func(t *testing.T) {
			f, plan, log := newPrometheusHistory(t, "2.55.0", "")
			backup := f.backup
			var before prometheusBefore
			json.Unmarshal(plan.Before, &before)
			manifest := func(updates map[string]any) {
				if err := f.db.Model(&models.BackupManifest{}).Where("backup_id = ?", f.backup.ID).Updates(updates).Error; err != nil {
					t.Fatal(err)
				}
			}
			switch mode {
			case "nil backup":
				backup = nil
			case "no association":
				plan.BackupID = nil
			case "wrong association":
				id := f.backup.ID + 100
				plan.BackupID = &id
			case "missing backup":
				f.db.Delete(f.backup)
			case "missing manifest":
				f.db.Where("backup_id = ?", f.backup.ID).Delete(&models.BackupManifest{})
			case "wrong component":
				id := f.component.ID + 100
				backup.ComponentID = &id
			case "wrong type":
				manifest(map[string]any{"component_type": models.ComponentThanos})
			case "wrong version":
				manifest(map[string]any{"component_version": "3.0.0"})
			case "unknown version":
				manifest(map[string]any{"component_version": ""})
			case "manifest scope":
				manifest(map[string]any{"scope": "data"})
			case "config only", "data only":
				scope := strings.Split(mode, " ")[0]
				backup.Type = scope
				f.db.Model(backup).Update("type", scope)
				manifest(map[string]any{"scope": scope})
			case "consistency":
				manifest(map[string]any{"consistency": "filesystem_copy"})
			case "format":
				manifest(map[string]any{"format": "file"})
			case "paths":
				manifest(map[string]any{"source_paths": `["/wrong/config","/wrong/data"]`})
			case "checksum":
				manifest(map[string]any{"sha256": strings.Repeat("0", 64)})
			case "size":
				manifest(map[string]any{"size": backup.Size + 1})
			case "missing archive":
				os.Remove(backup.Path)
			case "corrupt archive":
				os.WriteFile(backup.Path, []byte("bad archive"), 0600)
			case "malformed snapshot":
				plan.Before = json.RawMessage(`{broken`)
			case "missing snapshot":
				before.Component = nil
			case "scalar version":
				before.Version = "2.54.0"
			case "scalar image":
				before.Image = "different"
			case "snapshot id":
				before.Component.ID += 100
			case "hidden argv":
				before.ImportedArgsSHA256 = ""
				f.component.ImportedArgs = "private-sensitive-argv"
			case "changed argv":
				f.component.ImportedArgs = "private-sensitive-argv"
			case "database query":
				f.db.Callback().Query().Before("gorm:query").Register("test:manifest-unavailable", func(tx *gorm.DB) {
					if _, ok := tx.Statement.Dest.(*models.BackupManifest); ok {
						tx.AddError(errors.New("manifest query unavailable"))
					}
				})
			}
			if mode != "malformed snapshot" {
				plan.Before, _ = json.Marshal(before)
			}
			var prior models.Component
			f.db.First(&prior, f.component.ID)
			memory := f.component
			err := f.service.rollback(context.Background(), plan, backup, &f.component)
			if !errors.Is(err, ErrPrometheusRecoveryAdmission) {
				t.Fatalf("unsafe history must reject, got %v", err)
			}
			if strings.Contains(err.Error(), "private-sensitive-argv") {
				t.Error("secret leaked")
			}
			var current models.Component
			f.db.First(&current, f.component.ID)
			if !reflect.DeepEqual(prior, current) || !reflect.DeepEqual(memory, f.component) {
				t.Error("rejection mutated component identity")
			}
			if raw, _ := os.ReadFile(log); len(raw) != 0 {
				t.Errorf("rejection dispatched runtime: %s", raw)
			}
			for path, want := range map[string]string{f.component.ConfigPath: "current-config", filepath.Join(f.component.DataPath, "block"): "current-data", f.component.BinaryPath: "current-binary"} {
				if raw, _ := os.ReadFile(path); string(raw) != want {
					t.Errorf("live path changed: %s", path)
				}
			}
			archiveRead := mode == "checksum" || mode == "missing archive" || mode == "corrupt archive"
			if !archiveRead && len(f.commands) != 0 {
				t.Errorf("metadata rejection dispatched runner: %v", f.commands)
			}
			for _, cmd := range f.commands {
				if strings.Contains(cmd, "systemctl") || strings.Contains(cmd, ".restore-") || strings.Contains(cmd, ".checkpoint-") {
					t.Errorf("archive rejection wrote runtime/data: %s", cmd)
				}
			}
			var pauses int64
			f.db.Model(&ComponentBackupPause{}).Where("status = ?", "termination_unconfirmed").Count(&pauses)
			if pauses != 0 {
				t.Error("manual rejection quarantined untouched service")
			}
		})
	}
}

func TestPrometheusHistoricalRuntimeIdentityDrift(t *testing.T) {
	for _, field := range []string{"Name", "HostID", "ClusterID", "DeployType", "Port", "Config", "ConfigPath", "DataPath", "ServiceName", "InstallPath", "BinaryPath", "ComposePath", "CustomArgs", "CustomEnv", "ResourceLimits", "ExtraVolumes", "DataPathOverride"} {
		t.Run(field, func(t *testing.T) {
			f, plan, _ := newPrometheusHistory(t, "2.55.0", "")
			value := reflect.ValueOf(&f.component).Elem().FieldByName(field)
			switch value.Kind() {
			case reflect.String:
				value.SetString(value.String() + "-changed")
			case reflect.Uint:
				value.SetUint(value.Uint() + 1)
			case reflect.Int:
				value.SetInt(value.Int() + 1)
			}
			err := f.service.rollback(context.Background(), plan, f.backup, &f.component)
			// Host drift may fail the companion guard first, both are before mutation.
			if !errors.Is(err, ErrPrometheusRecoveryAdmission) && !errors.Is(err, ErrDependencyCompatibility) {
				t.Fatalf("identity drift admitted: %v", err)
			}
			if len(f.commands) != 0 {
				t.Error("identity drift reached runner")
			}
		})
	}
}

func TestPrometheusHistoricalRecoverySuccessAndMetadataOrdering(t *testing.T) {
	for _, mode := range []string{"historical pre-2.55", "aliases", "legacy empty argv", "proven argv"} {
		t.Run(mode, func(t *testing.T) {
			version, argv := "2.55.0", ""
			if mode == "historical pre-2.55" {
				version = "2.53.5"
			}
			if mode == "aliases" {
				version = "v2.55.0+historical"
			}
			if mode == "proven argv" {
				argv = `["--web.listen-address=:9090"]`
			}
			f, plan, log := newPrometheusHistory(t, version, argv)
			if mode == "legacy empty argv" {
				var before prometheusBefore
				json.Unmarshal(plan.Before, &before)
				before.ImportedArgsSHA256 = ""
				plan.Before, _ = json.Marshal(before)
				f.db.Model(plan).Update("before", plan.Before)
			}
			var ordered atomic.Bool
			if err := f.db.Callback().Update().Before("gorm:update").Register("test:version-before-unpause", func(tx *gorm.DB) {
				if tx.Statement.Table != "component_backup_pauses" {
					return
				}
				updates, ok := tx.Statement.Dest.(map[string]interface{})
				if !ok || updates["status"] != "resumed" {
					return
				}
				var current models.Component
				if err := tx.Session(&gorm.Session{NewDB: true}).First(&current, f.component.ID).Error; err != nil {
					tx.AddError(err)
					return
				}
				if !samePrometheusVersion(current.Version, version) || current.Image != "historical-image" {
					tx.AddError(fmt.Errorf("unpaused before metadata: %s %s", current.Version, current.Image))
					return
				}
				ordered.Store(true)
			}); err != nil {
				t.Fatal(err)
			}
			_, err := f.service.Rollback(context.Background(), plan.ID, ChangeActor{Role: "system_admin"})
			if err != nil {
				t.Fatal(err)
			}
			if !ordered.Load() {
				t.Error("metadata/unpause boundary not observed")
			}
			for path, want := range map[string]string{f.component.ConfigPath: "historical-config", filepath.Join(f.component.DataPath, "block"): "before-data", f.component.BinaryPath: "historical-binary"} {
				if raw, _ := os.ReadFile(path); string(raw) != want {
					t.Errorf("historical pairing missing %s: %s", path, raw)
				}
			}
			if raw, _ := os.ReadFile(log); strings.Count(string(raw), "restart compatibility-fixture") != 1 {
				t.Errorf("unexpected restarts: %s", raw)
			}
			if strings.Contains(string(plan.Before), argv) && argv != "" {
				t.Error("new snapshot leaked imported arguments")
			}
		})
	}
}

func TestPrometheusRuntimeRecoveryFailureQuarantinesData(t *testing.T) {
	for _, mode := range []string{"runtime error", "metadata write error"} {
		t.Run(mode, func(t *testing.T) {
			f, plan, log := newPrometheusHistory(t, "2.55.0", "")
			cause := errors.New("injected historical metadata unavailable")
			if mode == "runtime error" {
				// Real binary install succeeds, but a controlled systemctl restart fails.
				commands := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))[0]
				script := "#!/bin/sh\nprintf '%s\n' \"$*\" >> \"$COMPATIBILITY_RUNTIME_LOG\"\nif [ \"$1\" = restart ]; then exit 9; fi\nexit 0\n"
				if err := os.WriteFile(filepath.Join(commands, "systemctl"), []byte(script), 0755); err != nil {
					t.Fatal(err)
				}
			} else {
				f.db.Callback().Update().Before("gorm:update").Register("test:historical-metadata-failure", func(tx *gorm.DB) {
					if tx.Statement.Table == "components" {
						if updates, ok := tx.Statement.Dest.(map[string]interface{}); ok && updates["version"] == "2.55.0" {
							tx.AddError(cause)
						}
					}
				})
			}
			_, err := f.service.Rollback(context.Background(), plan.ID, ChangeActor{Role: "system_admin"})
			if !errors.Is(err, deployer.ErrBinaryRecoveryUnconfirmed) {
				t.Fatalf("lost unconfirmed boundary: %v", err)
			}
			if mode == "metadata write error" && !errors.Is(err, cause) {
				t.Errorf("lost original cause: %v", err)
			}
			if raw, _ := os.ReadFile(filepath.Join(f.component.DataPath, "block")); string(raw) != "before-data" {
				t.Errorf("checkpoint replayed conflicting current data: %s", raw)
			}
			if raw, _ := os.ReadFile(log); strings.Count(string(raw), "restart compatibility-fixture") != 1 {
				t.Errorf("unexpected runtime replay: %s", raw)
			}
			var checkpoint ComponentRestoreCheckpoint
			f.db.First(&checkpoint, "component_id = ?", f.component.ID)
			if checkpoint.Status != "termination_unconfirmed" {
				t.Errorf("checkpoint status=%s", checkpoint.Status)
			}
			assertPrometheusBlocked(t, f)
		})
	}
}

func TestPrometheusAutomaticUnsafeRecoveryIsDurablyBlocked(t *testing.T) {
	for _, mode := range []string{"malformed snapshot", "mismatched backup", "missing backup", "corrupt archive"} {
		t.Run(mode, func(t *testing.T) {
			f, plan, _ := newPrometheusHistory(t, "2.55.0", "")
			switch mode {
			case "malformed snapshot":
				plan.Before = json.RawMessage(`{bad`)
			case "mismatched backup":
				f.db.Model(&models.BackupManifest{}).Where("backup_id = ?", f.backup.ID).Update("component_version", "3.0.0")
			case "missing backup":
				plan.BackupID = nil
			case "corrupt archive":
				os.WriteFile(f.backup.Path, []byte("broken"), 0600)
			}
			op := models.Operation{ComponentID: &f.component.ID, Type: "change-UPGRADE", Status: "running"}
			if err := f.db.Create(&op).Error; err != nil {
				t.Fatal(err)
			}
			ctx, release, err := ClaimComponentVersionChange(context.Background(), f.db, f.component.ID, "execute")
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.service.rollbackAfterFailure(ctx, plan, f.backup, &f.component, &op, "verify", errors.New("forward runtime mutated then failed"))
			release()
			if !errors.Is(err, ErrPrometheusRecoveryAdmission) {
				t.Fatalf("missing recovery cause: %v", err)
			}
			f.db.First(plan, plan.ID)
			if plan.Status != models.ChangeStatusFailed || !strings.Contains(string(plan.RollbackEvidence), "rollback_error") {
				t.Error("missing durable diagnosis")
			}
			assertPrometheusBlocked(t, f)
		})
	}
}

func TestPrometheusRecoveryCriticalIntervalExcludesHostAndComponent(t *testing.T) {
	f, plan, _ := newPrometheusHistory(t, "2.55.0", "")
	ready, proceed := make(chan struct{}), make(chan struct{})
	var entered atomic.Bool
	f.service.backups.runnerFactory = func(*models.Component) (hostCommandRunner, error) {
		return backupControlRunner{before: func(_ context.Context, command string) error {
			if strings.Contains(command, ".restore-full-") && !entered.Swap(true) {
				close(ready)
				<-proceed
			}
			return nil
		}}, nil
	}
	done := make(chan error, 1)
	go func() {
		_, err := f.service.Rollback(context.Background(), plan.ID, ChangeActor{Role: "system_admin"})
		done <- err
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("restore boundary not reached: %v", err)
	case <-time.After(15 * time.Second):
		close(proceed)
		t.Fatal("restore barrier timeout")
	}
	for _, kind := range []string{"host", "component"} {
		var release func()
		var err error
		if kind == "host" {
			_, release, err = ClaimHostOperation(context.Background(), f.db, f.component.HostID, "other")
		} else {
			_, release, err = ClaimComponentOperation(context.Background(), f.db, f.component.ID, "other")
		}
		if release != nil {
			release()
		}
		if !errors.Is(err, ErrComponentBusy) {
			t.Errorf("%s not excluded: %v", kind, err)
		}
	}
	_, release, err := ClaimHostOperation(context.Background(), f.db, 99, "independent")
	if err != nil {
		t.Error(err)
	} else {
		release()
	}
	// This models startup during the already durable runtime transition boundary.
	if err := RecoverInterruptedComponentRestores(context.Background(), f.db); err == nil {
		t.Error("startup accepted in-progress historical transition")
	}
	close(proceed)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("restore did not finish")
	}
	_, release, err = ClaimComponentVersionChange(context.Background(), f.db, f.component.ID, "after")
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func persistBackupRestoreComponent(t *testing.T, s *BackupService, c *models.Component) {
	t.Helper()
	if err := s.db.AutoMigrate(&models.Component{}, &models.Host{}, &models.Cluster{}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Omit("Host", "Cluster").Save(c).Error; err != nil {
		t.Fatal(err)
	}
}

func TestPrometheusGenericRestoreRejectsStaleComponentSnapshot(t *testing.T) {
	for _, mode := range []string{"version", "argv", "binary path", "database missing"} {
		t.Run(mode, func(t *testing.T) {
			f, _, _ := newPrometheusHistory(t, "2.55.0", "")
			// The API has read this v3 pointer. Another operation completes a v2
			// restoration before this request acquires its claim.
			var backupErr error
			f.backup, _, backupErr = f.service.backups.CreateComponentBackup(context.Background(), &f.component, "stale-api-version", "full")
			if backupErr != nil {
				t.Fatal(backupErr)
			}
			f.commands = nil
			stale := f.component
			switch mode {
			case "version":
				f.db.Model(&f.component).Update("version", "2.55.0")
			case "argv":
				f.db.Model(&f.component).Update("imported_args", "sensitive-new-argv")
			case "binary path":
				f.db.Model(&f.component).Update("binary_path", "/different/prometheus")
			case "database missing":
				f.db.Delete(&f.component)
			}
			err := f.service.backups.RestoreComponentBackup(context.Background(), f.backup, &stale)
			if !errors.Is(err, ErrPrometheusRecoveryAdmission) {
				t.Fatalf("stale caller identity accepted: %v", err)
			}
			if len(f.commands) != 0 {
				t.Error("stale caller reached host commands")
			}
		})
	}
}

func TestPrometheusHistoricalArchiveRequiresBothRoots(t *testing.T) {
	for _, root := range []string{"config", "data"} {
		t.Run("missing-"+root, func(t *testing.T) {
			f, plan, log := newPrometheusHistory(t, "2.55.0", "")
			dir := t.TempDir()
			present, name := "config", filepath.Base(f.component.ConfigPath)
			if root == "config" {
				present, name = "data", filepath.Base(f.component.DataPath)
			}
			if err := os.MkdirAll(filepath.Join(dir, present, name), 0755); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command("tar", "-czf", f.backup.Path, "-C", dir, ".").CombinedOutput(); err != nil {
				t.Fatalf("build structurally incomplete archive: %s %v", out, err)
			}
			content, err := os.ReadFile(f.backup.Path)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(content)
			size := int64(len(content))
			// Valid integrity metadata must not disguise missing scope branches.
			f.backup.Size = size
			f.db.Model(f.backup).Update("size", size)
			f.db.Model(&models.BackupManifest{}).Where("backup_id = ?", f.backup.ID).Updates(map[string]any{"size": size, "sha256": hex.EncodeToString(sum[:])})
			err = f.service.rollback(context.Background(), plan, f.backup, &f.component)
			if !errors.Is(err, ErrPrometheusRecoveryAdmission) {
				t.Fatalf("incomplete archive accepted: %v", err)
			}
			if raw, _ := os.ReadFile(log); len(raw) != 0 {
				t.Error("invalid archive reached runtime")
			}
			if raw, _ := os.ReadFile(filepath.Join(f.component.DataPath, "block")); string(raw) != "current-data" {
				t.Error("invalid archive wrote data")
			}
		})
	}
}

func TestPrometheusRuntimeCallbackInterruptionKeepsStartupBlocked(t *testing.T) {
	f, plan, _ := newPrometheusHistory(t, "2.55.0", "")
	var historical prometheusBefore
	json.Unmarshal(plan.Before, &historical)
	target := f.component
	target.Version = historical.Version
	target.Image = historical.Image
	ready, proceed := make(chan struct{}), make(chan struct{})
	callback := f.service.prometheusRuntimeRecovery(&target, func(context.Context) error { close(ready); <-proceed; return errors.New("interrupted callback") })
	done := make(chan error, 1)
	go func() {
		done <- f.service.backups.restoreComponentBackup(context.Background(), f.backup, &target, callback)
	}()
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		close(proceed)
		t.Fatal("callback not reached")
	}
	if err := RecoverInterruptedComponentRestores(context.Background(), f.db); err == nil {
		t.Error("startup replayed during interrupted callback")
	}
	var current models.Component
	f.db.First(&current, f.component.ID)
	if current.Version != "3.12.0" {
		t.Error("metadata changed before confirmed callback")
	}
	close(proceed)
	if err := <-done; !errors.Is(err, deployer.ErrBinaryRecoveryUnconfirmed) {
		t.Fatalf("callback error lost quarantine: %v", err)
	}
	assertPrometheusBlocked(t, f)
}

func TestPrometheusHistoricalMetadataPersistenceIntervalBlocksStartup(t *testing.T) {
	f, plan, _ := newPrometheusHistory(t, "2.55.0", "")
	ready, proceed := make(chan struct{}), make(chan struct{})
	var entered atomic.Bool
	f.db.Callback().Update().Before("gorm:update").Register("test:metadata-persistence-barrier", func(tx *gorm.DB) {
		if tx.Statement.Table != "components" {
			return
		}
		updates, ok := tx.Statement.Dest.(map[string]interface{})
		if ok && updates["version"] == "2.55.0" && !entered.Swap(true) {
			close(ready)
			<-proceed
		}
	})
	done := make(chan error, 1)
	go func() {
		_, err := f.service.Rollback(context.Background(), plan.ID, ChangeActor{Role: "system_admin"})
		done <- err
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("persistence not reached: %v", err)
	case <-time.After(20 * time.Second):
		close(proceed)
		t.Fatal("persistence barrier timeout")
	}
	if err := RecoverInterruptedComponentRestores(context.Background(), f.db); err == nil {
		t.Error("startup allowed unmatched DB/runtime before metadata commit")
	}
	var current models.Component
	f.db.First(&current, f.component.ID)
	if current.Version != "3.12.0" {
		t.Error("barrier is not before metadata commit")
	}
	if raw, _ := os.ReadFile(current.BinaryPath); string(raw) != "historical-binary" {
		t.Error("barrier did not exercise switched binary")
	}
	if raw, _ := os.ReadFile(filepath.Join(current.DataPath, "block")); string(raw) != "before-data" {
		t.Error("barrier did not exercise historical data")
	}
	close(proceed)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestPrometheusRuntimeImageDriftCannotClearRecoveryPause(t *testing.T) {
	f, plan, _ := newPrometheusHistory(t, "2.55.0", "")
	var before prometheusBefore
	json.Unmarshal(plan.Before, &before)
	target := f.component
	target.Version = before.Version
	target.Image = before.Image
	restore := f.service.prometheusRuntimeRecovery(&target, func(context.Context) error {
		target.Image = "repository:rewritten-tag"
		return os.WriteFile(target.BinaryPath, []byte("runtime-with-unconfirmed-image"), 0755)
	})
	err := f.service.backups.restoreComponentBackup(context.Background(), f.backup, &target, restore)
	if !errors.Is(err, deployer.ErrBinaryRecoveryUnconfirmed) {
		t.Fatalf("image drift cleared pause: %v", err)
	}
	var current models.Component
	f.db.First(&current, f.component.ID)
	if current.Version != "3.12.0" || current.Image != "current-image" {
		t.Error("unconfirmed metadata persisted")
	}
	assertPrometheusBlocked(t, f)
}
