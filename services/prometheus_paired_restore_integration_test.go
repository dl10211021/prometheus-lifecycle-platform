package services

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func pointRealRestorePreview(t *testing.T, ctx context.Context, s *BackupService, r hostCommandRunner, c *models.Component, planID uint, root, unit string, port int, sampleTime, historical string) {
	t.Helper()
	if err := s.db.Model(&models.ChangePlan{}).Where("id = ?", planID).Update("status", models.ChangeStatusFailed).Error; err != nil {
		t.Fatal(err)
	}
	before, files, err := snapshotInventory(ctx, r, unit)
	if err != nil {
		t.Fatal(err)
	}
	checkUnchanged := func() {
		t.Helper()
		after, afterFiles, err := snapshotInventory(ctx, r, unit)
		if err != nil || before["MainPID"] != after["MainPID"] || before["UnitFileState"] != after["UnitFileState"] || !reflect.DeepEqual(files, afterFiles) {
			t.Fatal("restore preview changed actual process/startup", err)
		}
		if got := pointRealQueryWait(t, ctx, r, port, `up{job="paired-self"}`, sampleTime); got != historical {
			t.Fatal("restore preview changed historical sample")
		}
	}
	proof, err := s.PreviewPrometheusRecoveryPointRestore(ctx, planID)
	if os.Getenv("PROMLIFECYCLE_PAIRED_EXPECT_UNSUPPORTED_REFERENCES") == "1" {
		if proof != nil || !errors.Is(err, ErrPrometheusRecoveryAdmission) || !strings.Contains(err.Error(), "unsupported direct executable path") {
			t.Fatal("unsupported host did not reject the expected reference boundary", err)
		}
		if os.Getenv("PROMLIFECYCLE_PAIRED_RESTORE_JOURNAL") == "1" {
			pointRealRestoreJournal(t, ctx, s, planID, true, checkUnchanged)
		}
		checkUnchanged()
		t.Log("real paired restore reference boundary: non-absolute executable rejected; process PID, startup metadata and fixed TSDB sample unchanged")
		return
	}
	if err != nil || proof == nil {
		t.Fatal("actual paired restore admission", err)
	}
	if proof.RestoreAuthorized || !proof.RequiresCurrentCheckpoint || !proof.RequiresDowntime || proof.Scope != "paired_restore_admission_preview_only" || len(proof.Replacements) != 7 {
		t.Fatalf("actual preview scope: %+v", proof)
	}
	raw, _ := json.Marshal(proof)
	if strings.Contains(string(raw), "private-acceptance-sentinel") {
		t.Fatal("preview leaked environment")
	}
	checkUnchanged()
	// The second unit is never started. Its reload executable alone must still
	// prevent replacement of the running fixture's shared executable.
	other := strings.TrimSuffix(unit, ".service") + "-shared.service"
	path := "/etc/systemd/system/" + other
	content := "[Service]\nType=oneshot\nExecStart=/bin/true\nExecReload=" + c.BinaryPath + " --version\n"
	setup := "set -eu\numask 077\ntest ! -e " + utils.QuoteShell(path) + " && test ! -L " + utils.QuoteShell(path) + "\nset -C\nprintf %s " + utils.QuoteShell(base64.StdEncoding.EncodeToString([]byte(content))) + " | base64 -d > " + utils.QuoteShell(path)
	if _, err := snapshotRun(ctx, r, setup); err != nil {
		t.Fatal("exclusive shared unit fixture", err)
	}
	removed := false
	cleanup := func() error {
		clean, done := context.WithTimeout(context.Background(), 45*time.Second)
		defer done()
		base, err := s.runnerFor(c)
		if err != nil {
			return err
		}
		cr, release, err := acquireUnitSnapshotRunner(clean, base)
		if err != nil {
			return err
		}
		defer release()
		_, err = snapshotRun(clean, cr, "set -eu\ntest \"$(systemctl show --property=MainPID --value "+utils.QuoteShell(other)+")\" = 0\nrm -- "+utils.QuoteShell(path)+"\nsystemctl daemon-reload\ntest ! -e "+utils.QuoteShell(path)+"\ntest \"$(systemctl show --property=LoadState --value "+utils.QuoteShell(other)+")\" = not-found")
		return err
	}
	t.Cleanup(func() {
		if !removed {
			if err := cleanup(); err != nil {
				t.Error("shared unit cleanup", err)
			}
		}
	})
	if _, err := snapshotRun(ctx, r, "systemctl daemon-reload"); err != nil {
		t.Fatal(err)
	}
	if proof, err := s.PreviewPrometheusRecoveryPointRestore(ctx, planID); err == nil || proof != nil {
		t.Fatal("actual shared reload executable admitted")
	}
	checkUnchanged()
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	removed = true
	if _, err := s.PreviewPrometheusRecoveryPointRestore(ctx, planID); err != nil {
		t.Fatal("preview after owned sharing fixture removal", err)
	}
	checkUnchanged()
	if os.Getenv("PROMLIFECYCLE_PAIRED_RESTORE_JOURNAL") == "1" {
		pointRealRestoreJournal(t, ctx, s, planID, false, checkUnchanged)
	}
	t.Log("real paired restore preview: historical artifacts verified; process PID, startup metadata and fixed TSDB sample unchanged; shared ExecReload rejected; owned shared unit cleanup verified")
	if os.Getenv("PROMLIFECYCLE_PAIRED_RESTORE_CHECKPOINT") == "1" {
		pointRealRestoreCheckpoint(t, ctx, s, r, c, planID, root, unit, sampleTime, historical)
	}
}
