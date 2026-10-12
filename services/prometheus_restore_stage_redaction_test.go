package services

import (
	"bytes"
	"context"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"log"
	"strings"
	"testing"
)

func TestPrometheusRestoreStagePrivateSQLLogging(t *testing.T) {
	f, _, prepared, cp := checkpointFixture(t)
	ready, err := cp.CreatePrometheusPairedRestoreCheckpoint(context.Background(), prepared.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Exec("CREATE TRIGGER stage_log_fault BEFORE UPDATE OF phase ON prometheus_restore_transactions WHEN NEW.phase='restore_stage_intent' BEGIN SELECT RAISE(ABORT,'private-stage-trigger'); END").Error; err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	f.s.db = f.db.Session(&gorm.Session{Logger: logger.New(log.New(&output, "", 0), logger.Config{LogLevel: logger.Info})})
	if _, err := f.s.StagePrometheusPairedRestore(context.Background(), ready.ID); err == nil || strings.Contains(err.Error(), "private-stage-trigger") {
		t.Fatal("trigger was not redacted", err)
	}
	if strings.Contains(output.String(), "private-stage-trigger") || strings.Contains(output.String(), "restore-stage-") || strings.Contains(output.String(), "AdmissionSHA256") {
		t.Fatal("private stage payload or SQL error leaked through GORM logger")
	}
}
