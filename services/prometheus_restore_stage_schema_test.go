package services

import (
	"context"
	"github.com/promlifecycle/platform/models"
	"testing"
)

func TestPrometheusRestoreStageOldSchemaCompatibility(t *testing.T) {
	db, cs := journalGuardFixture(t)
	row := journalGuardRecord(t, db, cs[0])
	for _, column := range []string{"stage", "stage_sha256"} {
		if err := db.Exec("ALTER TABLE prometheus_restore_transactions DROP COLUMN " + column).Error; err != nil {
			t.Fatal(err)
		}
	}
	pending, err := pendingPairedRestoreJournals(context.Background(), db)
	if err != nil || len(pending) != 1 || pending[0].ID != row.ID {
		t.Fatal("old schema no longer decodes", err)
	}
	if err := db.AutoMigrate(&models.PrometheusRestoreTransaction{}); err != nil {
		t.Fatal(err)
	}
	var nulls int
	if err := db.Raw("SELECT (stage IS NULL AND stage_sha256 IS NULL) FROM prometheus_restore_transactions WHERE id = ?", row.ID).Scan(&nulls).Error; err != nil || nulls != 1 {
		t.Fatal("new evidence columns are not nullable", err)
	}
	pending, err = pendingPairedRestoreJournals(context.Background(), db)
	if err != nil || len(pending) != 1 || len(pending[0].Stage) != 0 || pending[0].StageSHA256 != "" {
		t.Fatal("migration changed old evidence", err)
	}
}
