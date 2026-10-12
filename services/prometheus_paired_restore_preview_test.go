package services

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/promlifecycle/platform/models"
)

type pairedPreviewRunner struct {
	fixture    *pointFixtureRunner
	refs       string
	version    string
	before     func(context.Context, string) error
	calls      []string
	properties func() string
	override   func(string) (string, bool)
}

func (r *pairedPreviewRunner) Run(ctx context.Context, command string) (string, error) {
	r.calls = append(r.calls, command)
	if r.before != nil {
		if err := r.before(ctx, command); err != nil {
			return "", err
		}
	}
	if r.override != nil {
		if out, ok := r.override(command); ok {
			return out, nil
		}
	}
	if strings.Contains(command, "# promlifecycle:paired-restore-unit-references") {
		return r.refs, nil
	}
	if strings.Contains(command, "# promlifecycle:recovery-point-version") && r.version != "" {
		return "prometheus, version " + r.version + " (fixture)\n", nil
	}
	if r.properties != nil && strings.HasPrefix(command, "systemctl show --no-pager") {
		return r.properties(), nil
	}
	return r.fixture.Run(ctx, command)
}
func pairedPreviewFixture(t *testing.T) (*pointFixture, *models.PrometheusRecoveryPoint, *pairedPreviewRunner) {
	t.Helper()
	f := newPointFixture(t)
	point, err := f.s.CreatePrometheusRecoveryPoint(context.Background(), f.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Model(f.plan).Update("status", models.ChangeStatusFailed).Error; err != nil {
		t.Fatal(err)
	}
	props := pairedReferenceBlock(f.c.ServiceName, "FragmentPath="+f.runner.fragment+"\nEnvironmentFiles="+f.runner.environment+" (ignore_errors=no)\nExecStart={ path="+f.c.BinaryPath+" ; argv[]="+f.c.BinaryPath+" ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0 }\n")
	runner := &pairedPreviewRunner{fixture: f.runner, refs: pairedReferenceFrame(f.c.ServiceName, props)}
	f.s.runnerFactory = func(*models.Component) (hostCommandRunner, error) { return runner, nil }
	return f, point, runner
}
func pairedPreviewFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			result[path] = info.Mode().String()
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			result[path] = fmt.Sprintf("%s:%d:link:%s", info.Mode(), info.ModTime().UnixNano(), target)
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		result[path] = fmt.Sprintf("%s:%d:%s", info.Mode(), info.ModTime().UnixNano(), pointDigest(raw))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func assertPairedPreviewNoMutation(t *testing.T, f *pointFixture, r *pairedPreviewRunner, before map[string]string, pid int) {
	t.Helper()
	if !reflect.DeepEqual(before, pairedPreviewFiles(t, f.root)) || !f.runner.running || f.runner.pid != pid {
		t.Fatal("preview mutated live or historical files/runtime")
	}
	for _, command := range r.calls {
		for _, forbidden := range []string{"systemctl stop ", "systemctl restart ", "systemctl start ", "systemctl daemon-reload", "systemctl enable ", "systemctl disable "} {
			if strings.Contains(command, forbidden) {
				t.Fatal("preview issued lifecycle mutation")
			}
		}
	}
	for _, model := range []any{&models.ComponentOperationClaim{}, &models.HostOperationClaim{}} {
		var count int64
		if err := f.db.Model(model).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("preview leaked claims: %d %v", count, err)
		}
	}
}
func TestPrometheusPairedRestorePreviewPreservesLiveAndHistoricalState(t *testing.T) {
	f, point, runner := pairedPreviewFixture(t)
	if err := os.WriteFile(f.runner.environment, []byte("TOKEN=changed-current-private\n"), 0600); err != nil {
		t.Fatal(err)
	}
	before := pairedPreviewFiles(t, f.root)
	pid := f.runner.pid
	proof, err := f.s.PreviewPrometheusRecoveryPointRestore(context.Background(), f.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if proof == nil || proof.PointID != point.ID || proof.PlanID != f.plan.ID || proof.BackupID != point.BackupID || proof.CurrentVersion != "2.55.0" || proof.HistoricalVersion != "2.55.0" || proof.Scope != "paired_restore_admission_preview_only" || proof.RestoreAuthorized || !proof.RequiresDowntime || !proof.RequiresCurrentCheckpoint {
		t.Fatalf("wrong preview contract: %+v", proof)
	}
	if len(proof.Replacements) != 5 {
		t.Fatalf("binary, two startup files and two archive roots required: %+v", proof.Replacements)
	}
	changed := false
	for _, file := range proof.Replacements {
		if file.Path == f.runner.environment {
			changed = file.ContentChanged && file.Action == "replace"
		}
	}
	if !changed {
		t.Fatal("changed startup content not previewed")
	}
	raw, _ := json.Marshal(proof)
	for _, secret := range []string{"changed-current-private", "private-env-sentinel", f.c.ImportedArgs} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("preview leaked private startup data")
		}
	}
	assertPairedPreviewNoMutation(t, f, runner, before, pid)
}
