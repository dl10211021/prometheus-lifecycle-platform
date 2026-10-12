package services

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
)

type checkpointRealObserver struct {
	runner hostCommandRunner
	calls  []string
}

func (r *checkpointRealObserver) Run(ctx context.Context, command string) (string, error) {
	r.calls = append(r.calls, command)
	return r.runner.Run(ctx, command)
}

// This helper is called only by the existing explicitly owned real-host fixture.
// Its isolated archive probe is acceptance machinery, never restore service code.
func pointRealRestoreCheckpoint(t *testing.T, ctx context.Context, s *BackupService, r hostCommandRunner, c *models.Component, planID uint, root, unit, sampleTime, historical string) {
	t.Helper()
	if err := s.db.AutoMigrate(&models.PrometheusRestoreTransaction{}); err != nil {
		t.Fatal(err)
	}
	marker := c.DataPath + "/checkpoint-current-marker"
	if _, err := snapshotRun(ctx, r, "set -eu\numask 077\nset -C\nprintf '%s' 'current-stopped-checkpoint' > "+utils.QuoteShell(marker)); err != nil {
		t.Fatal("exclusive current marker", err)
	}
	business := transactionBusinessState(t, s.db)
	row, err := s.PreparePrometheusPairedRestoreTransaction(ctx, planID)
	if err != nil {
		t.Fatal("real checkpoint prepare", err)
	}
	ready, err := s.CreatePrometheusPairedRestoreCheckpoint(ctx, row.ID)
	if err != nil {
		t.Fatal("real stopped checkpoint", err)
	}
	if ready.Phase != "checkpoint_ready" || ready.Revision != 4 || ready.FinishedAt != nil || string(ready.Payload) != string(row.Payload) {
		t.Fatal("real checkpoint did not retain admission/fence")
	}
	proof, err := decodeRestoreJournal(ready)
	if err != nil {
		t.Fatal(err)
	}
	stopped, err := decodeRestoreCheckpoint(ready, proof)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compareRecoveryPointBaseline(ctx, r, c, stopped.Evidence, true); err != nil {
		t.Fatal("original service not durably stopped", err)
	}
	archived, err := snapshotRun(ctx, r, "tar -xOzf "+utils.QuoteShell(stopped.Evidence.Archive.Path)+" ./data/data/checkpoint-current-marker")
	if err != nil || archived != "current-stopped-checkpoint" {
		t.Fatal("real checkpoint substituted historical archive", err)
	}
	observer := &checkpointRealObserver{runner: r}
	fresh := NewBackupServiceWithRoot(s.db, s.backupRoot)
	fresh.runnerFactory = func(*models.Component) (hostCommandRunner, error) { return observer, nil }
	for _, op := range []func(context.Context, uint) (*models.PrometheusRestoreTransaction, error){fresh.VerifyPrometheusPairedRestoreCheckpoint, fresh.CreatePrometheusPairedRestoreCheckpoint} {
		got, err := op(ctx, ready.ID)
		if err != nil || got == nil || got.Revision != 4 || !got.UpdatedAt.Equal(ready.UpdatedAt) {
			t.Fatal("real ready verification/idempotence", err)
		}
	}
	checkpointAssertReadOnly(t, observer.calls)
	if got, err := fresh.AbortPreparedPrometheusPairedRestoreTransaction(ctx, ready.ID); got != nil || !errors.Is(err, ErrPrometheusRecoveryAdmission) {
		t.Fatal("real prepared abort cleared stopped fence", err)
	}
	if business != transactionBusinessState(t, s.db) {
		t.Fatal("real checkpoint changed business/history rows")
	}
	journalAssertClaimsEmpty(t, s.db)
	public, _ := json.Marshal(ready)
	if strings.Contains(string(public), stopped.Root) || strings.Contains(string(public), ready.Token) {
		t.Fatal("real checkpoint exposed private evidence")
	}

	// Start a separate owned Prometheus using the pinned binary and extracted
	// archive. The original live roots and original unit remain stopped.
	probe := root + "/checkpoint-archive-probe"
	probeUnit := strings.TrimSuffix(unit, ".service") + "-checkpoint-probe.service"
	probePath := "/etc/systemd/system/" + probeUnit
	out, err := snapshotRun(ctx, r, "python3 -c "+utils.QuoteShell("import socket; s=socket.socket(); s.bind(('127.0.0.1',0)); print(s.getsockname()[1]); s.close()"))
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil || port <= 0 {
		t.Fatal("invalid archive probe port")
	}
	content := fmt.Sprintf("[Service]\nType=simple\nExecStart=%s/prometheus --config.file=%s/config/prometheus.yml --storage.tsdb.path=%s/data/data --web.listen-address=127.0.0.1:%d\n", probe, probe, probe, port)
	cleaned := false
	cleanup := func() error {
		clean, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		base, err := s.runnerFor(c)
		if err != nil {
			return err
		}
		runner, release, err := acquireUnitSnapshotRunner(clean, base)
		if err != nil {
			return err
		}
		defer release()
		command := checkpointProbeCleanupCommand(probePath, probeUnit, pointDigest([]byte(content)))
		_, err = snapshotRun(clean, runner, command)
		return err
	}
	t.Cleanup(func() {
		if !cleaned {
			if err := cleanup(); err != nil {
				t.Error("checkpoint probe cleanup", err)
			}
		}
	})
	setup := "set -eu\numask 077\ntest ! -e " + utils.QuoteShell(probePath) + " && test ! -L " + utils.QuoteShell(probePath) + "\nmkdir -m 0700 -- " + utils.QuoteShell(probe) +
		"\ntar -xzf " + utils.QuoteShell(stopped.Evidence.Archive.Path) + " -C " + utils.QuoteShell(probe) +
		"\ncp -- " + utils.QuoteShell(stopped.Evidence.Binary.Directory+"/prometheus") + " " + utils.QuoteShell(probe+"/prometheus") +
		"\nchmod 0755 -- " + utils.QuoteShell(probe+"/prometheus") +
		"\nset -C\nprintf %s " + utils.QuoteShell(base64.StdEncoding.EncodeToString([]byte(content))) + " | base64 -d > " + utils.QuoteShell(probePath) +
		"\nchmod 0644 -- " + utils.QuoteShell(probePath) + "\nsystemctl daemon-reload\nsystemctl start " + utils.QuoteShell(probeUnit)
	if _, err := snapshotRun(ctx, r, setup); err != nil {
		t.Fatal("start isolated checkpoint archive probe", err)
	}
	if got := pointRealQueryWait(t, ctx, r, port, `up{job="paired-self"}`, sampleTime); got != historical {
		t.Fatal("checkpoint archive lost fixed TSDB sample")
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	cleaned = true
	if _, err := compareRecoveryPointBaseline(ctx, r, c, stopped.Evidence, true); err != nil {
		t.Fatal("archive probe changed original stopped baseline", err)
	}
	if business != transactionBusinessState(t, s.db) {
		t.Fatal("archive probe changed business/history rows")
	}
	t.Logf("real paired restore checkpoint: ready revision=4; original stopped; current marker archived; fresh read-only verification/idempotence; prepared abort fenced; isolated archive TSDB fixed_sample=%s value=%s; checkpoint/history retained; probe unit cleanup verified", sampleTime, historical)
	pointRealRestoreStage(t, ctx, s, r, c, ready, root, unit, sampleTime, historical)
}

// A failed exclusive setup cannot authorize cleanup of an unrelated unit.
// The exact content binds this unit to the already exclusively owned fixture
// root; canonical single-link root-owned files are required before systemd.
func checkpointProbeCleanupCommand(probePath, probeUnit, expectedSHA256 string) string {
	return "set -eu\npath=" + utils.QuoteShell(probePath) + "\nunit=" + utils.QuoteShell(probeUnit) + "\nexpected=" + utils.QuoteShell(expectedSHA256) + `
if test -e "$path" || test -L "$path"; then
 test "$(readlink -m -- "$path")" = "$path"
 test -f "$path" && test ! -L "$path"
 test "$(stat -c '%u:%h' -- "$path")" = '0:1'
 test "$(sha256sum -- "$path" | cut -d ' ' -f 1)" = "$expected"
 systemctl stop "$unit"
 test "$(systemctl show --property=MainPID --value "$unit")" = 0
 rm -- "$path"
 systemctl daemon-reload
fi
test ! -e "$path" && test ! -L "$path"
test "$(systemctl show --property=LoadState --value "$unit")" = not-found
`
}
