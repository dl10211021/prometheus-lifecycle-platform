package services

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
)

type executionRealStartFault struct {
	runner   hostCommandRunner
	injected bool
}

func (r *executionRealStartFault) Run(ctx context.Context, command string) (string, error) {
	if !r.injected && strings.Contains(command, "# promlifecycle:restore-execution-start") {
		r.injected = true
		return "", errors.New("owned fixture injected historical start failure before dispatch")
	}
	return r.runner.Run(ctx, command)
}

// Diagnostics contain only fixed action labels, phase names and elapsed time.
// Never print commands, remote stdout/stderr, private journal payload or paths.
type executionRealTrace struct {
	runner hostCommandRunner
	t      *testing.T
	s      *BackupService
	id     uint
	last   string
}

func (r *executionRealTrace) Run(ctx context.Context, command string) (string, error) {
	action := "host-evidence"
	for _, label := range []string{"restore-execution-replace", "restore-execution-start", "restore-execution-listener", "recovery-point-runtime", "recovery-point-version", "recovery-point-launch", "restore-stage-fence"} {
		if strings.Contains(command, "# promlifecycle:"+label) {
			action = label
			break
		}
	}
	if strings.HasPrefix(command, "curl -fsS") {
		action = "readiness-http"
	}
	r.last = action
	started := time.Now()
	out, err := r.runner.Run(ctx, command)
	if err != nil || strings.HasPrefix(action, "restore-execution-") || action == "readiness-http" {
		var row models.PrometheusRestoreTransaction
		phase := "unavailable"
		if r.s.db.Select("phase").First(&row, r.id).Error == nil {
			phase = row.Phase
		}
		r.t.Logf("execution host boundary: phase=%s action=%s success=%t elapsed=%s", phase, action, err == nil, time.Since(started).Round(time.Millisecond))
	}
	return out, err
}

func pointRealRestoreExecution(t *testing.T, ctx context.Context, s *BackupService, r hostCommandRunner, c *models.Component, planID uint, root, unit, sampleTime, historical string) {
	t.Helper()
	if os.Getenv("PROMLIFECYCLE_PAIRED_RESTORE_EXECUTION") != "1" {
		return
	}
	if !strings.HasPrefix(root, "/var/lib/plc-paired-real-") || filepath.Base(root)+".service" != unit {
		t.Fatal("execution requires exclusively owned real fixture")
	}
	if err := s.db.AutoMigrate(&models.PrometheusRestoreTransaction{}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Model(&models.ChangePlan{}).Where("id = ?", planID).Update("status", models.ChangeStatusFailed).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := snapshotRun(ctx, r, "set -eu\nprintf current-stopped-checkpoint > "+utils.QuoteShell(c.DataPath+"/checkpoint-current-marker")+"\nprintf 'PAIRED_TEST_SECRET=current-private-fixture\\n' > "+utils.QuoteShell(root+"/protected.env")); err != nil {
		t.Fatal(err)
	}
	currentTime := pointRealQueryWait(t, ctx, r, c.Port, `timestamp(up{job="paired-self"})`, "")
	currentSample := pointRealQueryWait(t, ctx, r, c.Port, `up{job="paired-self"}`, currentTime)
	row, err := s.PreparePrometheusPairedRestoreTransaction(ctx, planID)
	if err != nil {
		t.Fatal("execution prepare", err)
	}
	if _, err = s.CreatePrometheusPairedRestoreCheckpoint(ctx, row.ID); err != nil {
		t.Fatal("execution checkpoint", err)
	}
	row, err = s.StagePrometheusPairedRestore(ctx, row.ID)
	if err != nil {
		t.Fatal("execution stage", err)
	}
	staged := *row
	// The production protocol deliberately retains displaced roots. This fixture
	// owns exact token/revision/index directories; clean only those after the run.
	t.Cleanup(func() {
		clean, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		base, err := s.runnerFor(c)
		if err != nil {
			t.Error(err)
			return
		}
		runner, release, err := acquireUnitSnapshotRunner(clean, base)
		if err != nil {
			t.Error(err)
			return
		}
		defer release()
		var durable models.PrometheusRestoreTransaction
		if s.db.First(&durable, staged.ID).Error != nil {
			t.Error("fixture journal missing")
			return
		}
		proof, _, err := decodeRestoreExecution(&durable)
		if err != nil {
			t.Error("fixture execution cleanup evidence invalid")
			return
		}
		frozen := frozenExecutionStage(&durable, proof)
		admission, err := decodeRestoreJournal(&frozen)
		if err != nil {
			t.Error(err)
			return
		}
		cp, err := decodeRestoreCheckpoint(&frozen, admission)
		if err != nil {
			t.Error(err)
			return
		}
		manifest, err := verifyUnitSnapshot(clean, runner, filepath.Dir(cp.Evidence.Startup.Path), &cp.Evidence.Startup, cp.Evidence.Binding, false)
		if err != nil {
			t.Error(err)
			return
		}
		paths := []string{c.ConfigPath, c.DataPath, c.BinaryPath}
		for _, file := range manifest.Files {
			paths = append(paths, file.Path)
		}
		script := "set -eu\nsystemctl stop " + utils.QuoteShell(unit) + "\n"
		for i, phase := range proof.Steps {
			if phase != "restore_executing" && phase != "restore_compensating" {
				continue
			}
			revision := proof.StageRevision + uint64(i) + 1
			for index, target := range paths {
				holding := filepath.Join(filepath.Dir(target), fmt.Sprintf(".plc-restore-%s-%d-%d", durable.Token[:24], revision, index))
				script += "p=" + utils.QuoteShell(holding) + "\nif [ -e \"$p\" ]; then test \"$(readlink -m -- \"$p\")\" = \"$p\"; test -d \"$p\" && test ! -L \"$p\"; test \"$(stat -c '%u:%g:%a' -- \"$p\")\" = 0:0:700; rm -rf -- \"$p\"; fi\n"
			}
		}
		if _, err := snapshotRun(clean, runner, script); err != nil {
			t.Error("owned execution displaced-path cleanup", err)
		}
	})
	fail := os.Getenv("PROMLIFECYCLE_PAIRED_EXECUTION_FAIL_START") == "1"
	trace := &executionRealTrace{runner: r, t: t, s: s, id: row.ID}
	var executionRunner hostCommandRunner = trace
	if fail {
		executionRunner = &executionRealStartFault{runner: trace}
	}
	s = s.onPointRunner(executionRunner)
	result, err := s.ExecutePrometheusPairedRestore(ctx, row.ID)
	expected := "restore_completed"
	at, value := sampleTime, historical
	if fail {
		expected = "restore_compensated"
		at, value = currentTime, currentSample
	}
	if result == nil || result.Phase != expected || (err != nil) != fail {
		phase := "missing"
		if result != nil {
			phase = result.Phase
		}
		t.Fatalf("real matched execution outcome: phase=%s expected=%s last_action=%s error=%v", phase, expected, trace.last, err)
	}
	if got := pointRealQueryWait(t, ctx, r, c.Port, `up{job="paired-self"}`, at); got != value {
		t.Fatal("real execution lost fixed TSDB sample")
	}
	if err := stageRealProbeBuildInfo(ctx, r, c.Port, "2.55.0"); err != nil {
		t.Fatal("real executed runtime version", err)
	}
	markerCommand := "test ! -e " + utils.QuoteShell(c.DataPath+"/checkpoint-current-marker")
	if fail {
		markerCommand = "test \"$(cat -- " + utils.QuoteShell(c.DataPath+"/checkpoint-current-marker") + ")\" = current-stopped-checkpoint"
	}
	if _, err := snapshotRun(ctx, r, markerCommand); err != nil {
		t.Fatal("real current/historical dataset discrimination", err)
	}
	journalAssertClaimsEmpty(t, s.db)
	if err := CheckPrometheusRestoreJournalBeforeStartup(ctx, s.db); err != nil {
		t.Fatal("terminal real execution startup fence", err)
	}
	t.Logf("real paired execution: phase=%s fixed_sample=%s value=%s exact_runtime_and_matching_data_verified=true", result.Phase, at, value)
}
func TestPrometheusRestoreExecutionRealFixtureOptIn(t *testing.T) {
	t.Setenv("PROMLIFECYCLE_PAIRED_RESTORE_EXECUTION", "")
	pointRealRestoreExecution(t, context.Background(), nil, nil, nil, 0, "", "", "", "")
}
