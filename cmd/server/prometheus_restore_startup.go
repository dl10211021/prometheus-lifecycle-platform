package main

import (
	"context"
	"fmt"

	"github.com/promlifecycle/platform/services"
	"github.com/promlifecycle/platform/storage/sqlite"
)

// Called only after acquireDatabaseProcessLock, before database replacement,
// migrations, schedulers or HTTP. An applying candidate has never served work;
// its preserved original is authoritative until initialization commits restore.
func preparePairedRestoreStartup(ctx context.Context, dbPath string) error {
	state, err := sqlite.RestoreStatus(dbPath)
	if err != nil {
		return fmt.Errorf("paired restore startup status unavailable: %w", err)
	}
	if state.Status == "applying" {
		original := dbPath + ".restore-original"
		if err := sqlite.ValidateSnapshot(original); err != nil {
			return fmt.Errorf("paired restore original snapshot unavailable: %w", err)
		}
		if err := services.ValidatePrometheusRestoreJournalForDatabaseRollback(ctx, original); err != nil {
			return err
		}
		if _, err := sqlite.ApplyPendingRestore(dbPath); err != nil {
			return fmt.Errorf("paired restore interrupted database rollback: %w", err)
		}
	}
	return services.RecoverPreparedPrometheusRestoreClaimsForStartup(ctx, dbPath)
}
