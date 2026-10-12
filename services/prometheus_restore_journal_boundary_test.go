package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promlifecycle/platform/models"
	platformsqlite "github.com/promlifecycle/platform/storage/sqlite"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestPrometheusRestoreJournalInventoryBoundsAndReadFailures(t *testing.T) {
	t.Run("over-limit", func(t *testing.T) {
		db, cs := journalGuardFixture(t)
		row := journalGuardRecord(t, db, cs[0])
		var batch []models.PrometheusRestoreTransaction
		for i := uint(2); i <= 1024; i++ {
			next := *row
			next.ID = 0
			next.HostID = i
			next.ComponentID = i
			hid, cid := i, i
			next.ActiveHostID = &hid
			next.ActiveComponentID = &cid
			next.Token = fmt.Sprintf("%064x", i)
			var payload prometheusRestoreJournalPayload
			if err := json.Unmarshal(row.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			payload.Current.HostID = i
			payload.Current.ComponentID = i
			next.Payload, _ = json.Marshal(payload)
			next.PayloadSHA256 = pointDigest(next.Payload)
			batch = append(batch, next)
		}
		if err := db.CreateInBatches(batch, 100).Error; err != nil {
			t.Fatal(err)
		}
		rows, err := pendingPairedRestoreJournals(context.Background(), db)
		if err != nil || len(rows) != 1024 {
			t.Fatal("supported inventory boundary rejected", err)
		}
		extra := *row
		extra.ID = 0
		id := uint(1025)
		extra.HostID = id
		extra.ComponentID = id
		extra.ActiveHostID = &id
		extra.ActiveComponentID = &id
		extra.Token = fmt.Sprintf("%064x", id)
		// Keep the overflow row valid so this tests the inventory bound,
		// rather than failing early while decoding a mismatched owner.
		var payload prometheusRestoreJournalPayload
		if err := json.Unmarshal(row.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		payload.Current.HostID = id
		payload.Current.ComponentID = id
		extra.Payload, _ = json.Marshal(payload)
		extra.PayloadSHA256 = pointDigest(extra.Payload)
		if err := db.Create(&extra).Error; err != nil {
			t.Fatal(err)
		}
		_, err = pendingPairedRestoreJournals(context.Background(), db)
		if !errors.Is(err, ErrPrometheusRecoveryAdmission) || !strings.Contains(err.Error(), "exceeds supported bound") {
			t.Fatal("oversized valid pending inventory did not fail", err)
		}
	})
	t.Run("legacy-and-failed-read", func(t *testing.T) {
		db, _ := journalGuardFixture(t)
		if err := db.Migrator().DropTable(&models.PrometheusRestoreTransaction{}); err != nil {
			t.Fatal(err)
		}
		rows, err := pendingPairedRestoreJournals(context.Background(), db)
		if err != nil || len(rows) != 0 {
			t.Fatal("legacy absent table rejected", err)
		}
		if err := db.Exec("CREATE TABLE prometheus_restore_transactions (id INTEGER PRIMARY KEY)").Error; err != nil {
			t.Fatal(err)
		}
		if _, err := pendingPairedRestoreJournals(context.Background(), db); !errors.Is(err, ErrPrometheusRecoveryAdmission) {
			t.Fatal("unreadable inventory treated as absent", err)
		}
		connection, _ := db.DB()
		if err := connection.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := pendingPairedRestoreJournals(context.Background(), db); !errors.Is(err, ErrPrometheusRecoveryAdmission) {
			t.Fatal("failed schema read treated as absent", err)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		db, _ := journalGuardFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := pendingPairedRestoreJournals(ctx, db); !errors.Is(err, context.Canceled) {
			t.Fatal("journal read lost cancellation", err)
		}
	})
}

func TestPrometheusRestoreJournalStrictPayloadAndIndependentUniqueKeys(t *testing.T) {
	for _, mode := range []string{"oversized", "unknown-field", "trailing", "host-unique", "component-unique", "moved-owner"} {
		t.Run(mode, func(t *testing.T) {
			db, cs := journalGuardFixture(t)
			row := journalGuardRecord(t, db, cs[0])
			switch mode {
			case "host-unique", "component-unique":
				next := *row
				next.ID = 0
				next.Token = strings.Repeat("b", 64)
				if mode == "host-unique" {
					id := cs[2].ID
					next.ActiveComponentID = &id
				} else {
					id := cs[2].HostID
					next.ActiveHostID = &id
				}
				if err := db.Create(&next).Error; err == nil {
					t.Fatal("independent active unique key not enforced")
				}
				return
			case "oversized":
				row.Payload = []byte(strings.Repeat(" ", 65537))
			case "unknown-field":
				var value map[string]any
				if err := json.Unmarshal(row.Payload, &value); err != nil {
					t.Fatal(err)
				}
				value["Unrecognized"] = true
				row.Payload, _ = json.Marshal(value)
			case "trailing":
				row.Payload = append(row.Payload, []byte(" {}")...)
			case "moved-owner":
				if err := db.Model(&models.Component{}).Where("id = ?", cs[0].ID).Update("host_id", cs[2].HostID).Error; err != nil {
					t.Fatal(err)
				}
				if _, release, err := ClaimComponentOperation(context.Background(), db, cs[0].ID, "restart"); !errors.Is(err, ErrPrometheusRecoveryAdmission) {
					if release != nil {
						release()
					}
					t.Fatal("moved owning component escaped fence", err)
				}
				return
			}
			row.PayloadSHA256 = pointDigest(row.Payload)
			if _, err := decodeRestoreJournal(row); !errors.Is(err, ErrPrometheusRecoveryAdmission) {
				t.Fatal("invalid payload accepted", err)
			}
		})
	}
}

func TestPrometheusRestoreJournalReadOnlyExistingDatabase(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "journal # space?.db")
	if err := CheckPrometheusRestoreJournalBeforeDatabaseRestore(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("preflight created missing database", err)
	}
	if err := ValidatePrometheusRestoreJournalForDatabaseRollback(context.Background(), path); err == nil {
		t.Fatal("rollback accepted missing original")
	}
	db, err := gorm.Open(sqlite.Open((&url.URL{Scheme: "file", Path: path}).String()+"?mode=rwc&_journal_mode=WAL"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	connection, _ := db.DB()
	defer connection.Close()
	if err := db.AutoMigrate(&models.PrometheusRestoreTransaction{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("PRAGMA wal_autocheckpoint=0").Error; err != nil {
		t.Fatal(err)
	}
	row := journalGuardRecord(t, db, &models.Component{ID: 1, HostID: 1})
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckPrometheusRestoreJournalBeforeDatabaseRestore(context.Background(), path); !errors.Is(err, ErrPrometheusRecoveryAdmission) {
		t.Fatal("read-only escaped WAL intent or special path", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatal("read-only preflight rewrote database", err)
	}
	var retained models.PrometheusRestoreTransaction
	if err := db.First(&retained, row.ID).Error; err != nil || retained.Token != row.Token {
		t.Fatal("preflight changed journal", err)
	}
	if err := ValidatePrometheusRestoreJournalForDatabaseRollback(context.Background(), path); err != nil {
		t.Fatal("valid original intent cannot return to live database", err)
	}
	bad := filepath.Join(root, "corrupt.db")
	if err := os.WriteFile(bad, []byte("not sqlite"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrometheusRestoreJournalBeforeDatabaseRestore(context.Background(), bad); !errors.Is(err, ErrPrometheusRecoveryAdmission) {
		t.Fatal("corrupt database treated as fresh", err)
	}
	alias := filepath.Join(root, "alias.db")
	if err := os.Symlink(path, alias); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrometheusRestoreJournalBeforeDatabaseRestore(context.Background(), alias); err == nil {
		t.Fatal("unresolved database symlink admitted")
	}
}

func TestPrometheusRestoreJournalStartupClaimCleanupIsAtomic(t *testing.T) {
	db, cs := journalGuardFixture(t)
	ctx, releaseHost, err := ClaimHostOperation(context.Background(), db, cs[0].HostID, "prepare")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseHost()
	_, releaseComponent, err := ClaimComponentOperation(ctx, db, cs[0].ID, "prepare")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseComponent()
	journalGuardRecord(t, db, cs[0])
	path, err := platformsqlite.DatabasePath(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TRIGGER journal_cleanup_fault BEFORE DELETE ON component_operation_claims BEGIN SELECT RAISE(ABORT, 'private-cleanup-fault'); END").Error; err != nil {
		t.Fatal(err)
	}
	if err := RecoverPreparedPrometheusRestoreClaimsForStartup(context.Background(), path); !errors.Is(err, ErrPrometheusRecoveryAdmission) || strings.Contains(err.Error(), "private-cleanup") {
		t.Fatal("cleanup fault was lost or leaked", err)
	}
	var count int64
	if err := db.Model(&models.HostOperationClaim{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatal("failed cleanup partially deleted host claim", err)
	}
	if err := db.Model(&models.ComponentOperationClaim{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatal("failed cleanup deleted component claim", err)
	}
	if err := db.Exec("DROP TRIGGER journal_cleanup_fault").Error; err != nil {
		t.Fatal(err)
	}
}

func TestPrometheusRestoreJournalClaimsMustStillBeOwned(t *testing.T) {
	for _, mode := range []string{"host-lost", "component-lost"} {
		t.Run(mode, func(t *testing.T) {
			db, cs := journalGuardFixture(t)
			ctx, releaseH, err := ClaimHostOperation(context.Background(), db, cs[0].HostID, "prepare")
			if err != nil {
				t.Fatal(err)
			}
			defer releaseH()
			ctx, releaseC, err := ClaimComponentOperation(ctx, db, cs[0].ID, "prepare")
			if err != nil {
				t.Fatal(err)
			}
			defer releaseC()
			row := journalGuardRecord(t, db, cs[0])
			if err := requireRestoreJournalClaims(ctx, db, row); err != nil {
				t.Fatal("valid claim ownership rejected", err)
			}
			if mode == "host-lost" {
				err = db.Where("host_id = ?", cs[0].HostID).Delete(&models.HostOperationClaim{}).Error
			} else {
				err = db.Where("component_id = ?", cs[0].ID).Delete(&models.ComponentOperationClaim{}).Error
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := requireRestoreJournalClaims(ctx, db, row); !errors.Is(err, ErrComponentBusy) {
				t.Fatal("lost durable claim admitted", err)
			}
		})
	}
}
