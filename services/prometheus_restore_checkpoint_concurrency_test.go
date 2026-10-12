package services

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/promlifecycle/platform/models"
)

type checkpointConcurrentCallerKey struct{}
type checkpointConcurrentRunner struct {
	base     hostCommandRunner
	first    [2]sync.Once
	arrivals atomic.Int32
	gate     chan struct{}
	mu       sync.Mutex
}

func (r *checkpointConcurrentRunner) Run(ctx context.Context, command string) (string, error) {
	id := ctx.Value(checkpointConcurrentCallerKey{}).(int)
	r.first[id].Do(func() {
		if r.arrivals.Add(1) == 2 {
			close(r.gate)
		}
	})
	select {
	case <-r.gate:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.base.Run(ctx, command)
}
func TestPrometheusRestoreCheckpointSameReentrantContextHasOneWinner(t *testing.T) {
	f, r, row, api := checkpointFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	ctx = context.WithValue(ctx, restoreJournalAccessKey{}, restoreJournalAccess{ID: row.ID, Token: row.Token, HostID: row.HostID, ComponentID: row.ComponentID})
	ctx, releaseH, err := ClaimHostOperation(ctx, f.db, row.HostID, "prepared-abort")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseH()
	ctx, releaseC, err := ClaimComponentOperation(ctx, f.db, row.ComponentID, "prepared-abort")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseC()
	runner := &checkpointConcurrentRunner{base: r, gate: make(chan struct{})}
	f.s.runnerFactory = func(*models.Component) (hostCommandRunner, error) { return runner, nil }
	type outcome struct {
		row *models.PrometheusRestoreTransaction
		err error
	}
	results := make(chan outcome, 2)
	for id := 0; id < 2; id++ {
		go func(id int) {
			got, err := api.CreatePrometheusPairedRestoreCheckpoint(context.WithValue(ctx, checkpointConcurrentCallerKey{}, id), row.ID)
			results <- outcome{got, err}
		}(id)
	}
	successes := 0
	for i := 0; i < 2; i++ {
		result := <-results
		if result.err == nil {
			if result.row == nil || result.row.Phase != "checkpoint_ready" {
				t.Fatal("concurrent success lacks stopped checkpoint")
			}
			successes++
		} else if result.row != nil || !errors.Is(result.err, ErrPrometheusRecoveryAdmission) {
			t.Fatal("concurrent loser lost admission error", result.err)
		}
	}
	if successes != 1 || runner.arrivals.Load() != 2 {
		t.Fatal("concurrent reentry did not have exactly one winner", successes)
	}
	stop, root := 0, 0
	for _, command := range r.calls {
		if strings.HasPrefix(command, "systemctl stop ") {
			stop++
		}
		if strings.Contains(command, "# promlifecycle:restore-checkpoint-root") {
			root++
		}
	}
	if stop != 1 || root != 1 || f.runner.running {
		t.Fatal("concurrent reentry repeated mutation", stop, root)
	}
	retained := checkpointJournal(t, f.db, row.ID)
	if retained.Phase != "checkpoint_ready" || retained.Revision != 4 {
		t.Fatal("concurrent reentry lost durable winner")
	}
	releaseC()
	releaseH()
	journalAssertClaimsEmpty(t, f.db)
}
