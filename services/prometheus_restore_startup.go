package services

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/promlifecycle/platform/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Read-only mode intentionally includes WAL; immutable=1 would miss live intent.
// mode=rw is reserved for abandoned-claim cleanup under the server process lock.
func withRestoreJournalStartupDB(ctx context.Context, path, mode string, required bool, fn func(*gorm.DB) error) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return restoreJournalReadError(ctx, "startup check cancelled", err)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return restoreJournalReject("existing database path unavailable")
	}
	info, err := os.Lstat(absolute)
	if errors.Is(err, os.ErrNotExist) && !required {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return restoreJournalReject("existing database is not a readable regular file")
	}
	uri := url.URL{Scheme: "file", Path: absolute}
	query := url.Values{"mode": {mode}, "_busy_timeout": {"5000"}}
	if mode == "ro" {
		query.Set("_query_only", "1")
	}
	uri.RawQuery = query.Encode()
	connection, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		return restoreJournalReadError(ctx, "existing database unavailable", err)
	}
	defer connection.Close()
	connection.SetMaxOpenConns(1)
	db, err := gorm.Open(sqlite.Dialector{Conn: connection}, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return restoreJournalReadError(ctx, "existing database unavailable", err)
	}
	return fn(db.WithContext(ctx))
}

// CheckPrometheusRestoreJournalBeforeDatabaseRestore is a read-only fence.
func CheckPrometheusRestoreJournalBeforeDatabaseRestore(ctx context.Context, path string) error {
	return withRestoreJournalStartupDB(ctx, path, "ro", false, func(db *gorm.DB) error {
		return rejectPendingPairedRestoreJournals(db.Statement.Context, db)
	})
}

// ValidatePrometheusRestoreJournalForDatabaseRollback validates the preserved
// original before an interrupted whole-database restore may copy it back.
// Valid pending intent may return to the live database, where startup fences it.
func ValidatePrometheusRestoreJournalForDatabaseRollback(ctx context.Context, path string) error {
	return withRestoreJournalStartupDB(ctx, path, "ro", true, func(db *gorm.DB) error {
		_, err := pendingPairedRestoreJournals(db.Statement.Context, db)
		return err
	})
}

// RecoverPreparedPrometheusRestoreClaimsForStartup requires exclusive ownership
// of the database process lock. It only releases abandoned transient claims for
// validated pending hosts, then still refuses startup. Runtime Abort must never
// steal these claims from a possibly live owner.
func RecoverPreparedPrometheusRestoreClaimsForStartup(ctx context.Context, path string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var inventory []models.PrometheusRestoreTransaction
	if err := withRestoreJournalStartupDB(ctx, path, "ro", false, func(db *gorm.DB) error {
		var err error
		inventory, err = pendingPairedRestoreJournals(ctx, db)
		return err
	}); err != nil {
		return err
	}
	if len(inventory) == 0 {
		return nil
	}
	if err := withRestoreJournalStartupDB(ctx, path, "rw", true, func(db *gorm.DB) error {
		return db.Transaction(func(tx *gorm.DB) error {
			rows, err := pendingPairedRestoreJournals(ctx, tx)
			if err != nil {
				return err
			}
			if len(rows) != len(inventory) {
				return restoreJournalReject("startup journal inventory changed")
			}
			var hosts, components []uint
			for i, row := range rows {
				if row.Phase != "prepared" || row.ID != inventory[i].ID || row.Token != inventory[i].Token || row.PayloadSHA256 != inventory[i].PayloadSHA256 {
					return restoreJournalReject("startup journal identity changed")
				}
				hosts = append(hosts, row.HostID)
				components = append(components, row.ComponentID)
			}
			if err := tx.Where("host_id IN ?", hosts).Delete(&models.HostOperationClaim{}).Error; err != nil {
				return restoreJournalReadError(ctx, "cannot release abandoned host claims", err)
			}
			// A sibling INSERT may have committed just before its post-acquire fence
			// check was interrupted by the same process crash.
			if err := tx.Where("component_id IN ? OR component_id IN (SELECT id FROM components WHERE host_id IN ?)", components, hosts).
				Delete(&models.ComponentOperationClaim{}).Error; err != nil {
				return restoreJournalReadError(ctx, "cannot release abandoned component claims", err)
			}
			return nil
		})
	}); err != nil {
		return err
	}
	return restoreJournalReject("pending recovery must be resolved before startup")
}

// CheckPrometheusRestoreJournalBeforeStartup protects a newly restored database
// before metadata migration, reconciliation or any work dispatch.
func CheckPrometheusRestoreJournalBeforeStartup(ctx context.Context, db *gorm.DB) error {
	return rejectPendingPairedRestoreJournals(ctx, db)
}
