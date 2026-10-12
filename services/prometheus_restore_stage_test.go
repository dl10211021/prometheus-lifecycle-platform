package services

import (
	"context"
	"encoding/json"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	platformsqlite "github.com/promlifecycle/platform/storage/sqlite"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type restoreStageAPI interface {
	StagePrometheusPairedRestore(context.Context, uint) (*models.PrometheusRestoreTransaction, error)
	VerifyPrometheusPairedRestoreStage(context.Context, uint) (*models.PrometheusRestoreTransaction, error)
}

func stageAPI(t *testing.T, s *BackupService) restoreStageAPI {
	t.Helper()
	api, ok := any(s).(restoreStageAPI)
	if !ok {
		t.Fatal("BackupService lacks fenced paired stage API")
	}
	return api
}
func TestPrometheusRestoreStageCopiesAndReadOnlyRepeat(t *testing.T) {
	f, r, prepared, checkpoint := checkpointFixture(t)
	api := stageAPI(t, f.s)
	if err := os.WriteFile(f.c.BinaryPath, []byte(strings.Repeat("distinct-current-executable", 10000)), 0755); err != nil {
		t.Fatal(err)
	}
	ready, err := checkpoint.CreatePrometheusPairedRestoreCheckpoint(context.Background(), prepared.ID)
	if err != nil {
		t.Fatal(err)
	}
	before := pairedPreviewFiles(t, f.root)
	staged, err := api.StagePrometheusPairedRestore(context.Background(), ready.ID)
	if err != nil {
		t.Fatal(err)
	}
	if staged.Phase != "restore_staged" || staged.Revision != 7 || staged.ActiveHostID == nil || staged.ActiveComponentID == nil || staged.FinishedAt != nil || f.runner.running {
		t.Fatal("stage lost durable stopped fence")
	}
	if string(staged.Payload) != string(ready.Payload) || string(staged.Checkpoint) != string(ready.Checkpoint) {
		t.Fatal("stage changed immutable evidence")
	}
	after := pairedPreviewFiles(t, f.root)
	for p, v := range before {
		if after[p] != v {
			t.Fatal("original changed", p)
		}
	}
	proof, err := decodeRestoreStage(staged)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(staged)
	for _, secret := range []string{proof.Fence.Root, proof.Fence.Generation, proof.Claims.Host, proof.Claims.Component, string(staged.Stage), f.c.ImportedArgs} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("private stage evidence exposed")
		}
	}
	var admission prometheusRestoreJournalPayload
	json.Unmarshal(staged.Payload, &admission)
	cpProof, err := decodeRestoreCheckpoint(staged, &admission)
	if err != nil {
		t.Fatal(err)
	}
	var point models.PrometheusRecoveryPoint
	if err := f.db.First(&point, staged.PointID).Error; err != nil {
		t.Fatal(err)
	}
	historical, err := decodeRecoveryPointPayload(point.Payload)
	if err != nil {
		t.Fatal(err)
	}
	for i, source := range []*prometheusRecoveryPayload{cpProof.Evidence, historical} {
		root := filepath.Join(proof.Fence.Root, "g-"+proof.Fence.Generation, []string{"current", "historical"}[i])
		copy := derivedRestoreStageSet(root, source)
		for _, pair := range [][2]string{{source.Archive.Path, copy.Archive.Path}, {filepath.Join(source.Binary.Directory, "prometheus"), filepath.Join(copy.Binary.Directory, "prometheus")}, {filepath.Join(source.Startup.Path, "manifest.json"), filepath.Join(copy.Startup.Path, "manifest.json")}} {
			old, _ := os.ReadFile(pair[0])
			new, _ := os.ReadFile(pair[1])
			a, _ := os.Stat(pair[0])
			b, _ := os.Stat(pair[1])
			if string(old) != string(new) || os.SameFile(a, b) || b.Mode().Perm() != 0600 {
				t.Fatal("copy bytes/metadata/independence mismatch")
			}
		}
		sample, err := (localBackupRunner{}).Run(context.Background(), "tar -xOzf "+utils.QuoteShell(copy.Archive.Path)+" ./data/data/sample")
		if err != nil || sample != []string{"current-checkpoint-sample", "historical-sample"}[i] {
			t.Fatal("wrong copied archive content", err)
		}
	}
	reopened, err := platformsqlite.OpenDB(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	connection, _ := reopened.DB()
	defer connection.Close()
	fresh := NewBackupServiceWithRoot(reopened, f.s.backupRoot)
	fresh.runnerFactory = f.s.runnerFactory
	api = stageAPI(t, fresh)
	if string(raw) == "" {
		t.Fatal("empty public record")
	}
	r.calls = nil
	for _, op := range []func(context.Context, uint) (*models.PrometheusRestoreTransaction, error){api.StagePrometheusPairedRestore, api.VerifyPrometheusPairedRestoreStage} {
		got, err := op(context.Background(), staged.ID)
		if err != nil || got.Revision != staged.Revision || !got.UpdatedAt.Equal(staged.UpdatedAt) {
			t.Fatal("read-only repeat", err)
		}
	}
	if !reflect.DeepEqual(after, pairedPreviewFiles(t, f.root)) {
		t.Fatal("ready verification wrote files")
	}
	var reads []string
	for _, call := range r.calls {
		if strings.Contains(call, "# promlifecycle:restore-stage-fence") {
			marker := strings.Trim(utils.QuoteShell("operation='verify'\nbootstrap=0\n"), "'")
			if !strings.Contains(call, marker) || strings.Contains(call, "# promlifecycle:restore-stage-copy") {
				t.Fatal("ready call used non-verify fence mode")
			}
		} else {
			reads = append(reads, call)
		}
	}
	checkpointAssertReadOnly(t, reads)
	journalAssertClaimsEmpty(t, f.db)
	// Each complete set must contain physically independent startup, binary and archive copies.
	count := 0
	filepath.Walk(f.s.backupRoot, func(p string, info os.FileInfo, e error) error {
		if e == nil && info.Mode().IsRegular() && filepath.Base(p) == "payload.tar.gz" {
			count++
		}
		return e
	})
	if count != 4 {
		t.Fatalf("expected two original and two copied full archives, got %d", count)
	}
}
