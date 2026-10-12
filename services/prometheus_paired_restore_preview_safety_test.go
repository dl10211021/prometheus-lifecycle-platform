package services

import (
	"context"
	"errors"
	"github.com/promlifecycle/platform/models"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPrometheusPairedRestorePreviewRejectsSharedReferences(t *testing.T) {
	f, _, r := pairedPreviewFixture(t)
	original := r.refs
	for _, kind := range []string{"reload executable", "environment file", "canonical symlink", "registered binary", "registered data ancestor", "registered service identity", "registered alias identity", "registered uncertain path"} {
		t.Run(kind, func(t *testing.T) {
			r.refs = original
			r.calls = nil
			var other models.Component
			var alias string
			if strings.HasPrefix(kind, "registered") {
				other = models.Component{Name: "retained-other", HostID: f.c.HostID, ClusterID: f.c.ClusterID, Type: models.ComponentNodeExporter, DeployType: "binary", Version: "1.8.0", Status: models.StatusStopped, ServiceName: "other.service"}
				switch kind {
				case "registered binary":
					other.BinaryPath = f.c.BinaryPath
				case "registered data ancestor":
					other.DataPath = filepath.Dir(f.c.DataPath)
				case "registered service identity":
					other.ServiceName = f.c.ServiceName
				case "registered alias identity":
					other.ServiceName = "alias.service"
					r.refs = strings.Replace(original, "Names="+f.c.ServiceName, "Names="+f.c.ServiceName+" alias.service", 1)
				case "registered uncertain path":
					other.ConfigPath = "/etc/%i/ambiguous.yml"
				}
				if err := f.db.Create(&other).Error; err != nil {
					t.Fatal(err)
				}
				defer f.db.Delete(&other)
			} else {
				field := "ExecReload=" + pairedReferenceExec(f.c.BinaryPath)
				if kind == "environment file" {
					field = "EnvironmentFiles=" + f.runner.environment + " (ignore_errors=no)"
				}
				if kind == "canonical symlink" {
					alias = filepath.Join(f.root, "shared-alias")
					if err := os.Symlink(f.c.BinaryPath, alias); err != nil {
						t.Fatal(err)
					}
					defer os.Remove(alias)
					field = "ExecStart=" + pairedReferenceExec(alias)
				}
				r.refs = strings.Replace(original, "\nPROMLIFECYCLE_PROPERTIES_V1", "\nother.service\nPROMLIFECYCLE_PROPERTIES_V1", 1)
				r.refs = strings.Replace(r.refs, "\nPROMLIFECYCLE_END_V1", "\n"+pairedReferenceBlock("other.service", field+"\n")+"\nPROMLIFECYCLE_END_V1", 1)
			}
			defer func() { r.refs = original }()
			before := pairedPreviewFiles(t, f.root)
			state := pairedPreviewBusinessState(t, f)
			pid := f.runner.pid
			proof, err := f.s.PreviewPrometheusRecoveryPointRestore(context.Background(), f.plan.ID)
			if proof != nil || !errors.Is(err, ErrPrometheusRecoveryAdmission) {
				t.Fatalf("shared runtime admitted: %+v %v", proof, err)
			}
			if !reflect.DeepEqual(state, pairedPreviewBusinessState(t, f)) {
				t.Fatal("sharing rejection modified business state")
			}
			assertPairedPreviewNoMutation(t, f, r, before, pid)
		})
	}
	t.Run("other host path is independent", func(t *testing.T) {
		r.refs = original
		r.calls = nil
		host := models.Host{Name: "independent", IP: "127.0.0.2"}
		if err := f.db.Create(&host).Error; err != nil {
			t.Fatal(err)
		}
		defer f.db.Delete(&host)
		other := models.Component{Name: "other-host", HostID: host.ID, ClusterID: f.c.ClusterID, Type: models.ComponentNodeExporter, DeployType: "binary", Version: "1.8.0", Status: models.StatusStopped, BinaryPath: f.c.BinaryPath, ServiceName: f.c.ServiceName}
		if err := f.db.Create(&other).Error; err != nil {
			t.Fatal(err)
		}
		defer f.db.Delete(&other)
		before := pairedPreviewFiles(t, f.root)
		pid := f.runner.pid
		if _, err := f.s.PreviewPrometheusRecoveryPointRestore(context.Background(), f.plan.ID); err != nil {
			t.Fatal(err)
		}
		assertPairedPreviewNoMutation(t, f, r, before, pid)
	})
}
func TestPrometheusPairedRestorePreviewRejectsMidObservationDrift(t *testing.T) {
	f, _, r := pairedPreviewFixture(t)
	originalRefs := r.refs
	for _, kind := range []string{"process generation", "process PID", "startup content", "binary content", "unit reference inventory", "registered inventory", "pause state", "plan state", "plan kind", "plan resource type", "plan resource id", "component version"} {
		t.Run(kind, func(t *testing.T) {
			oldPID, oldGeneration := f.runner.pid, f.runner.generation
			r.calls = nil
			r.refs = originalRefs
			var undo func()
			var expectedFiles, expectedState map[string]string
			injected := false
			reads := 0
			r.before = func(_ context.Context, cmd string) error {
				if !strings.Contains(cmd, "# promlifecycle:paired-restore-unit-references") {
					return nil
				}
				reads++
				if reads != 2 {
					return nil
				}
				injected = true
				switch kind {
				case "process generation":
					f.runner.generation++
				case "process PID":
					f.runner.pid++
				case "startup content", "binary content":
					path := f.runner.environment
					if kind == "binary content" {
						path = f.c.BinaryPath
					}
					info, err := os.Stat(path)
					if err != nil {
						return err
					}
					raw, err := os.ReadFile(path)
					if err != nil {
						return err
					}
					if err := os.WriteFile(path, append(raw, []byte("\nchanged-during-observation")...), info.Mode().Perm()); err != nil {
						return err
					}
					undo = func() {
						if err := os.WriteFile(path, raw, info.Mode().Perm()); err != nil {
							t.Error(err)
						}
						if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
							t.Error(err)
						}
					}
				case "unit reference inventory":
					r.refs = strings.Replace(originalRefs, "\nPROMLIFECYCLE_PROPERTIES_V1", "\nnew.service\nPROMLIFECYCLE_PROPERTIES_V1", 1)
					r.refs = strings.Replace(r.refs, "\nPROMLIFECYCLE_END_V1", "\n"+pairedReferenceBlock("new.service", "FragmentPath=/etc/systemd/system/new.service\n")+"\nPROMLIFECYCLE_END_V1", 1)
				case "registered inventory":
					other := models.Component{Name: "new-retained", HostID: f.c.HostID, ClusterID: f.c.ClusterID, Type: models.ComponentNodeExporter, Status: models.StatusStopped, BinaryPath: "/opt/independent"}
					if err := f.db.Create(&other).Error; err != nil {
						return err
					}
					undo = func() { f.db.Delete(&other) }
				case "pause state":
					previewUpdate(t, f, "component_backup_pauses", "component_id", f.c.ID, "status", "termination_unconfirmed")
					undo = func() { previewUpdate(t, f, "component_backup_pauses", "component_id", f.c.ID, "status", "resumed") }
				case "plan state":
					previewUpdate(t, f, "change_plans", "id", f.plan.ID, "status", models.ChangeStatusExecuting)
					undo = func() { previewUpdate(t, f, "change_plans", "id", f.plan.ID, "status", models.ChangeStatusFailed) }
				case "plan kind":
					previewUpdate(t, f, "change_plans", "id", f.plan.ID, "kind", "config")
					undo = func() { previewUpdate(t, f, "change_plans", "id", f.plan.ID, "kind", models.ChangeKindUpgrade) }
				case "plan resource type":
					previewUpdate(t, f, "change_plans", "id", f.plan.ID, "resource_type", "host")
					undo = func() { previewUpdate(t, f, "change_plans", "id", f.plan.ID, "resource_type", "component") }
				case "plan resource id":
					previewUpdate(t, f, "change_plans", "id", f.plan.ID, "resource_id", f.c.ID+1)
					undo = func() { previewUpdate(t, f, "change_plans", "id", f.plan.ID, "resource_id", f.c.ID) }
				case "component version":
					previewUpdate(t, f, "components", "id", f.c.ID, "version", "2.56.0")
					undo = func() { previewUpdate(t, f, "components", "id", f.c.ID, "version", "2.55.0") }
				}
				expectedFiles = pairedPreviewFiles(t, f.root)
				expectedState = pairedPreviewBusinessState(t, f)
				return nil
			}
			defer func() {
				r.before = nil
				r.refs = originalRefs
				f.runner.pid = oldPID
				f.runner.generation = oldGeneration
				if undo != nil {
					undo()
				}
			}()
			proof, err := f.s.PreviewPrometheusRecoveryPointRestore(context.Background(), f.plan.ID)
			if !injected || proof != nil || !errors.Is(err, ErrPrometheusRecoveryAdmission) {
				t.Fatalf("state drift admitted/injection missed: %t %+v %v", injected, proof, err)
			}
			// The externally injected state belongs to that actor. A preview must
			// neither compensate it nor attempt to resume the service.
			if !reflect.DeepEqual(expectedState, pairedPreviewBusinessState(t, f)) {
				t.Fatal("preview compensated external durable change")
			}
			assertPairedPreviewNoMutation(t, f, r, expectedFiles, f.runner.pid)
		})
	}
}
func TestPrometheusPairedRestorePreviewRequiresFreshHistoricalArtifacts(t *testing.T) {
	f, point, r := pairedPreviewFixture(t)
	var backup models.Backup
	if err := f.db.First(&backup, point.BackupID).Error; err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(backup.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backup.Path, append(raw, []byte("corrupt-trailer")...), 0600); err != nil {
		t.Fatal(err)
	}
	before := pairedPreviewFiles(t, f.root)
	state := pairedPreviewBusinessState(t, f)
	pid := f.runner.pid
	r.calls = nil
	if proof, err := f.s.PreviewPrometheusRecoveryPointRestore(context.Background(), f.plan.ID); proof != nil || err == nil {
		t.Fatal("corrupt historical archive admitted")
	}
	if !reflect.DeepEqual(state, pairedPreviewBusinessState(t, f)) {
		t.Fatal("corrupt history changed metadata")
	}
	assertPairedPreviewNoMutation(t, f, r, before, pid)
}

func TestPrometheusPairedRestorePreviewRejectsLateDataAliases(t *testing.T) {
	f, _, r := pairedPreviewFixture(t)
	for _, kind := range []string{"root alias", "nested alias"} {
		t.Run(kind, func(t *testing.T) {
			reads := 0
			injected := false
			var expectedFiles, expectedState map[string]string
			var undo func()
			r.calls = nil
			r.before = func(_ context.Context, cmd string) error {
				if !strings.Contains(cmd, "# promlifecycle:paired-restore-unit-references") {
					return nil
				}
				reads++
				if reads != 2 {
					return nil
				}
				if kind == "root alias" {
					moved := f.c.DataPath + "-relocated"
					if err := os.Rename(f.c.DataPath, moved); err != nil {
						return err
					}
					undo = func() {
						os.Remove(f.c.DataPath)
						if err := os.Rename(moved, f.c.DataPath); err != nil {
							t.Error(err)
						}
					}
					if err := os.Symlink(moved, f.c.DataPath); err != nil {
						return err
					}
				} else {
					path := filepath.Join(f.c.DataPath, "external-alias")
					if err := os.Symlink(f.runner.environment, path); err != nil {
						return err
					}
					undo = func() {
						if err := os.Remove(path); err != nil {
							t.Error(err)
						}
					}
				}
				injected = true
				expectedFiles = pairedPreviewFiles(t, f.root)
				expectedState = pairedPreviewBusinessState(t, f)
				return nil
			}
			defer func() {
				r.before = nil
				if undo != nil {
					undo()
				}
			}()
			proof, err := f.s.PreviewPrometheusRecoveryPointRestore(context.Background(), f.plan.ID)
			if !injected || proof != nil || err == nil {
				t.Fatalf("late data alias admitted: injected=%t proof=%t err=%v", injected, proof != nil, err)
			}
			if !reflect.DeepEqual(expectedState, pairedPreviewBusinessState(t, f)) {
				t.Fatal("preview compensated injected alias")
			}
			assertPairedPreviewNoMutation(t, f, r, expectedFiles, f.runner.pid)
		})
	}
}
