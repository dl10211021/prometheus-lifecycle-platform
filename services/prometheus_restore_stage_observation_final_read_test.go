package services

import (
	"context"
	"testing"

	"github.com/promlifecycle/platform/models"
	platformsqlite "github.com/promlifecycle/platform/storage/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// A journal mutation between the first owner read and its trailing fresh read
// must reject before opening a runner, even when phase/digests are unchanged.
func TestPrometheusRestoreStageReadyObservationFinalReadRequiresActiveFence(t *testing.T) {
	f, r, prepared, cp := checkpointFixture(t)
	ready, err := cp.CreatePrometheusPairedRestoreCheckpoint(context.Background(), prepared.ID)
	if err != nil {
		t.Fatal(err)
	}
	staged, err := f.s.StagePrometheusPairedRestore(context.Background(), ready.ID)
	if err != nil {
		t.Fatal(err)
	}
	initial := checkpointJournal(t, f.db, staged.ID)
	db, err := platformsqlite.OpenDB(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	conn, _ := db.DB()
	defer conn.Close()
	f.db.Logger = logger.Default.LogMode(logger.Silent)
	for _, method := range []string{"stage", "verify"} {
		t.Run(method, func(t *testing.T) {
			reads := 0
			fired := false
			if err := f.db.Callback().Query().Before("gorm:query").Register("ready-final-row-fence", func(tx *gorm.DB) {
				if tx.Statement.Table != "prometheus_restore_transactions" {
					return
				}
				reads++
				if reads == 3 {
					fired = true
					if err := db.Model(&models.PrometheusRestoreTransaction{}).Where("id = ?", initial.ID).UpdateColumn("active_host_id", nil).Error; err != nil {
						t.Fatal(err)
					}
				}
			}); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := f.db.Callback().Query().Remove("ready-final-row-fence"); err != nil {
					t.Error(err)
				}
				if err := db.Model(&models.PrometheusRestoreTransaction{}).Where("id = ?", initial.ID).UpdateColumn("active_host_id", initial.HostID).Error; err != nil {
					t.Error(err)
				}
			}()
			r.calls = nil
			op := f.s.StagePrometheusPairedRestore
			if method == "verify" {
				op = f.s.VerifyPrometheusPairedRestoreStage
			}
			if _, err := op(context.Background(), initial.ID); err == nil {
				t.Fatal("missing active journal fence admitted")
			}
			if !fired || len(r.calls) != 0 {
				t.Fatalf("final fresh row was not validated before host observation: fired=%v host=%d", fired, len(r.calls))
			}
			after := checkpointJournal(t, db, initial.ID)
			if !sameRestoreStageRow(&initial, &after) || !after.UpdatedAt.Equal(initial.UpdatedAt) || after.ActiveHostID != nil {
				t.Fatal("observer wrote journal")
			}
			journalAssertClaimsEmpty(t, db)
		})
	}
}
