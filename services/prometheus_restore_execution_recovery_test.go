package services

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	coressh "github.com/promlifecycle/platform/core/ssh"
	"github.com/promlifecycle/platform/models"
	platformsqlite "github.com/promlifecycle/platform/storage/sqlite"
)

type pairedExecutionRecoveryAPI interface {
	RecoverPrometheusPairedRestoreExecutionForStartup(context.Context, uint, *platformsqlite.DatabaseProcessLock) (*models.PrometheusRestoreTransaction, error)
}

func TestPrometheusRestoreExecutionInterruptedRecovery(t *testing.T) {
	t.Parallel()
	api, ok := any(NewBackupService(nil)).(pairedExecutionRecoveryAPI)
	if !ok {
		t.Fatal("exclusive interrupted execution compensation is not implemented")
	}
	_ = api
	f, r, staged := executionFixture(t)
	fired := false
	r.before = func(_ context.Context, command string) error {
		if strings.Contains(command, "# promlifecycle:restore-execution-start") {
			fired = true
			return coressh.ErrRemoteTerminationUnconfirmed
		}
		return nil
	}
	row, err := f.s.ExecutePrometheusPairedRestore(context.Background(), staged.ID)
	if !fired || !errors.Is(err, coressh.ErrRemoteTerminationUnconfirmed) || row == nil || row.Phase != "restore_starting" {
		t.Fatal("uncertain writer did not remain fenced", err)
	}
	old, _, err := decodeRestoreExecution(row)
	if err != nil {
		t.Fatal(err)
	}
	r.before = func(context.Context, string) error { t.Fatal("no ownership must reject before host"); return nil }
	api = any(f.s).(pairedExecutionRecoveryAPI)
	if _, err = api.RecoverPrometheusPairedRestoreExecutionForStartup(context.Background(), row.ID, nil); err == nil {
		t.Fatal("missing ownership admitted")
	}
	r.before = nil
	// Exact abandoned claims can be retired; unrelated owners cannot.
	foreign := models.HostOperationClaim{HostID: row.HostID, Token: strings.Repeat("e", 48), Kind: "foreign", CreatedAt: time.Now()}
	if err = f.db.Create(&foreign).Error; err != nil {
		t.Fatal(err)
	}
	_, lock, err := platformsqlite.AcquireDatabaseProcessLock(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	r.before = func(context.Context, string) error { t.Fatal("foreign ownership must reject before host"); return nil }
	if _, err = api.RecoverPrometheusPairedRestoreExecutionForStartup(context.Background(), row.ID, lock); err == nil {
		t.Fatal("foreign host claim adopted")
	}
	r.before = nil
	f.db.Where("host_id = ?", row.HostID).Delete(&models.HostOperationClaim{})
	for _, claim := range []any{&models.HostOperationClaim{HostID: row.HostID, Token: old.Claims.Host, Kind: "abandoned", CreatedAt: time.Now()}, &models.ComponentOperationClaim{ComponentID: row.ComponentID, Token: old.Claims.Component, Kind: "abandoned", CreatedAt: time.Now()}} {
		if err = f.db.Create(claim).Error; err != nil {
			t.Fatal(err)
		}
	}
	result, err := api.RecoverPrometheusPairedRestoreExecutionForStartup(context.Background(), row.ID, lock)
	if err != nil || result == nil || result.Phase != "restore_compensated" {
		t.Fatal("exclusive recovery failed", err)
	}
	proof, _, err := decodeRestoreExecution(result)
	if err != nil {
		t.Fatal(err)
	}
	if proof.Fence.Generation == old.Fence.Generation || proof.Claims == old.Claims {
		t.Fatal("recovery failed to retire old writer")
	}
	if err = runRestoreStageFenced(context.Background(), localBackupRunner{}, old.Fence, "true"); err == nil {
		t.Fatal("old host generation still authorizes writes")
	}
	assertExecutionFiles(t, f, false)
	journalAssertClaimsEmpty(t, f.db)
}
