package services

import (
	"context"
	"errors"
	"testing"

	"github.com/promlifecycle/platform/models"
	"gorm.io/gorm"
)

func TestPrometheusRestoreJournalPublicationDuringClaimAcquisition(t *testing.T) {
	for _, hostClaim := range []bool{false, true} {
		name := "component"
		if hostClaim {
			name = "host"
		}
		t.Run(name, func(t *testing.T) {
			db, cs := journalGuardFixture(t)
			table := "component_operation_claims"
			if hostClaim {
				table = "host_operation_claims"
			}
			injected := false
			callback := "journal-publication-during-claim"
			if err := db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Schema == nil || tx.Statement.Schema.Table != table || injected {
					return
				}
				injected = true
				// A second DB connection commits while the original claim is
				// between its initial journal read and its first INSERT.
				journalGuardRecord(t, db, cs[0])
			}); err != nil {
				t.Fatal(err)
			}
			defer db.Callback().Create().Remove(callback)
			var release func()
			var err error
			if hostClaim {
				_, release, err = ClaimHostOperation(context.Background(), db, cs[0].HostID, "restart")
			} else {
				_, release, err = ClaimComponentOperation(context.Background(), db, cs[1].ID, "restart")
			}
			if release != nil {
				release()
			}
			if !injected {
				t.Fatal("race fixture did not publish journal")
			}
			if !errors.Is(err, ErrPrometheusRecoveryAdmission) {
				t.Fatalf("claim admitted after journal committed: %v", err)
			}
			journalAssertClaimsEmpty(t, db)
			var journals int64
			if err := db.Model(&models.PrometheusRestoreTransaction{}).Where("phase = ?", "prepared").Count(&journals).Error; err != nil || journals != 1 {
				t.Fatal("claim rejection lost committed journal", err)
			}
		})
	}
}
