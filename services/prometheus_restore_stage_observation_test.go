package services

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/promlifecycle/platform/models"
	platformsqlite "github.com/promlifecycle/platform/storage/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestPrometheusRestoreStageReadyObservationCreatesNoClaims(t *testing.T) {
	for _, method := range []string{"stage", "verify"} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			f, _, prepared, cp := checkpointFixture(t)
			ready, err := cp.CreatePrometheusPairedRestoreCheckpoint(context.Background(), prepared.ID)
			if err != nil {
				t.Fatal(err)
			}
			staged, err := f.s.StagePrometheusPairedRestore(context.Background(), ready.ID)
			if err != nil {
				t.Fatal(err)
			}
			journalAssertClaimsEmpty(t, f.db)
			before := checkpointJournal(t, f.db, staged.ID)
			files := pairedPreviewFiles(t, f.root)
			f.db.Logger = logger.Default.LogMode(logger.Silent)
			creates, deletes, updates := 0, 0, 0
			claimTable := func(tx *gorm.DB) bool {
				return tx.Statement.Table == "host_operation_claims" || tx.Statement.Table == "component_operation_claims"
			}
			if err := f.db.Callback().Create().Before("gorm:create").Register("observe-claim-create", func(tx *gorm.DB) {
				if claimTable(tx) {
					creates++
				}
			}); err != nil {
				t.Fatal(err)
			}
			if err := f.db.Callback().Delete().Before("gorm:delete").Register("observe-claim-delete", func(tx *gorm.DB) {
				if claimTable(tx) {
					deletes++
					// Stage models a surviving host-only release; Verify models both releases failing.
					if method == "verify" || tx.Statement.Table == "host_operation_claims" {
						tx.AddError(errors.New("exact-token release failed"))
					}
				}
			}); err != nil {
				t.Fatal(err)
			}
			if err := f.db.Callback().Update().Before("gorm:update").Register("observe-journal-update", func(tx *gorm.DB) {
				if tx.Statement.Table == "prometheus_restore_transactions" {
					updates++
					tx.AddError(errors.New("observation must not update journal"))
				}
			}); err != nil {
				t.Fatal(err)
			}
			removed := false
			remove := func() {
				if removed {
					return
				}
				removed = true
				if err := f.db.Callback().Create().Remove("observe-claim-create"); err != nil {
					t.Error(err)
				}
				if err := f.db.Callback().Delete().Remove("observe-claim-delete"); err != nil {
					t.Error(err)
				}
				if err := f.db.Callback().Update().Remove("observe-journal-update"); err != nil {
					t.Error(err)
				}
			}
			defer remove()
			op := f.s.StagePrometheusPairedRestore
			if method == "verify" {
				op = f.s.VerifyPrometheusPairedRestoreStage
			}
			got, err := op(context.Background(), staged.ID)
			if err != nil {
				t.Fatal("ready observation failed", err)
			}
			after := checkpointJournal(t, f.db, staged.ID)
			if !reflect.DeepEqual(before, after) || !sameRestoreStageRow(got, &before) || !reflect.DeepEqual(files, pairedPreviewFiles(t, f.root)) {
				t.Fatal("observation changed evidence or files")
			}
			var hosts []models.HostOperationClaim
			var components []models.ComponentOperationClaim
			if err := f.db.Find(&hosts).Error; err != nil {
				t.Fatal(err)
			}
			if err := f.db.Find(&components).Error; err != nil {
				t.Fatal(err)
			}
			if creates != 0 || deletes != 0 || updates != 0 || len(hosts) != 0 || len(components) != 0 {
				t.Fatalf("ready observation creates unpinned durable claims: insert=%d delete=%d update=%d surviving host=%d component=%d", creates, deletes, updates, len(hosts), len(components))
			}
			// Hooks would fail cleanup but are never reached. Explicit recovery still recognizes the unchanged evidence.
			remove()
			_, lock, err := platformsqlite.AcquireDatabaseProcessLock(f.dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			recovered, err := f.s.RecoverPrometheusPairedRestoreStageForStartup(context.Background(), staged.ID, lock)
			if err != nil {
				t.Fatal("observation stranded recovery", err)
			}
			proof, err := decodeRestoreStage(recovered)
			old, _ := decodeRestoreStage(staged)
			if err != nil || recovered.Phase != "restore_staged" || proof.Fence.Generation == old.Fence.Generation || len(proof.Generations) != 2 {
				t.Fatal("recovery did not publish fresh generation", err)
			}
			journalAssertClaimsEmpty(t, f.db)
		})
	}
}
