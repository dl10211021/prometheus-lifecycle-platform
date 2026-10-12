package services

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestPrometheusRestoreStageRevisionMustBindGeneration(t *testing.T) {
	f, _, prepared, cp := checkpointFixture(t)
	ready, err := cp.CreatePrometheusPairedRestoreCheckpoint(context.Background(), prepared.ID)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("a", 64)
	p := prometheusRestoreStagePayload{Schema: 1, TransactionID: ready.ID, AdmissionSHA256: ready.PayloadSHA256, CheckpointSHA256: ready.CheckpointSHA256, Fence: restoreStageFence{BackupRoot: f.s.backupRoot, Root: restoreStageRoot(f.s.backupRoot, ready), Generation: token, RootIdentity: "1:2", LockIdentity: "1:3"}, Generations: []string{token}, Claims: restoreStageClaims{strings.Repeat("a", 48), strings.Repeat("b", 48)}, StartedAt: f.s.now().UTC(), CompletedAt: f.s.now().UTC()}
	row := *ready
	row.Phase = "restore_staged"
	row.Revision = 7
	row.Stage, _ = json.Marshal(p)
	row.StageSHA256 = pointDigest(row.Stage)
	if _, err := decodeRestoreJournal(&row); err != nil {
		t.Fatal("valid stage shape rejected", err)
	}
	row.Revision++
	if _, err := decodeRestoreJournal(&row); err == nil {
		t.Fatal("impossible first-generation completed revision accepted")
	}
}
