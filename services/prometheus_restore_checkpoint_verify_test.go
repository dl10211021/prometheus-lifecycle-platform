package services

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/promlifecycle/platform/models"
)

func TestPrometheusRestoreCheckpointVerificationRejectsChangedEvidence(t *testing.T) {
	f, r, prepared, api := checkpointFixture(t)
	ready, err := api.CreatePrometheusPairedRestoreCheckpoint(context.Background(), prepared.ID)
	if err != nil {
		t.Fatal(err)
	}
	proof, _ := checkpointEvidence(t, f.db, ready.ID)
	for _, mode := range []string{"archive-corrupt", "archive-missing", "archive-hardlink", "binary-corrupt", "binary-missing", "startup-corrupt", "startup-missing", "root-entry", "permissions", "restarted", "payload-digest", "checkpoint-digest", "wrong-root"} {
		t.Run(mode, func(t *testing.T) {
			var undo func()
			switch mode {
			case "archive-corrupt", "binary-corrupt", "startup-corrupt":
				path := proof.Evidence.Archive.Path
				if mode == "binary-corrupt" {
					path = filepath.Join(proof.Evidence.Binary.Directory, "prometheus")
				}
				if mode == "startup-corrupt" {
					path = filepath.Join(proof.Evidence.Startup.Path, "manifest.json")
				}
				original, e := os.ReadFile(path)
				if e != nil {
					t.Fatal(e)
				}
				if e := os.WriteFile(path, []byte("private-corrupted-checkpoint"), 0600); e != nil {
					t.Fatal(e)
				}
				undo = func() {
					if e := os.WriteFile(path, original, 0600); e != nil {
						t.Fatal(e)
					}
				}
			case "archive-missing", "binary-missing", "startup-missing":
				path := proof.Evidence.Archive.Path
				if mode == "binary-missing" {
					path = filepath.Join(proof.Evidence.Binary.Directory, "prometheus")
				}
				if mode == "startup-missing" {
					path = filepath.Join(proof.Evidence.Startup.Path, "manifest.json")
				}
				away := filepath.Join(f.root, "temporarily-missing-checkpoint")
				if e := os.Rename(path, away); e != nil {
					t.Fatal(e)
				}
				undo = func() {
					if e := os.Rename(away, path); e != nil {
						t.Fatal(e)
					}
				}
			case "archive-hardlink":
				path := filepath.Join(f.root, "checkpoint-extra-link")
				if e := os.Link(proof.Evidence.Archive.Path, path); e != nil {
					t.Fatal(e)
				}
				undo = func() {
					if e := os.Remove(path); e != nil {
						t.Fatal(e)
					}
				}
			case "root-entry":
				path := filepath.Join(proof.Root, "unexpected")
				if e := os.WriteFile(path, []byte("unexpected"), 0600); e != nil {
					t.Fatal(e)
				}
				undo = func() {
					if e := os.Remove(path); e != nil {
						t.Fatal(e)
					}
				}
			case "permissions":
				if e := os.Chmod(proof.Evidence.Archive.Path, 0644); e != nil {
					t.Fatal(e)
				}
				undo = func() {
					if e := os.Chmod(proof.Evidence.Archive.Path, 0600); e != nil {
						t.Fatal(e)
					}
				}
			case "restarted":
				f.runner.running = true
				undo = func() { f.runner.running = false }
			case "payload-digest", "checkpoint-digest":
				column, value := "payload_sha256", ready.PayloadSHA256
				if mode == "checkpoint-digest" {
					column, value = "checkpoint_sha256", ready.CheckpointSHA256
				}
				if e := f.db.Model(&models.PrometheusRestoreTransaction{}).Where("id = ?", ready.ID).UpdateColumn(column, strings.Repeat("f", 64)).Error; e != nil {
					t.Fatal(e)
				}
				undo = func() {
					if e := f.db.Model(&models.PrometheusRestoreTransaction{}).Where("id = ?", ready.ID).UpdateColumn(column, value).Error; e != nil {
						t.Fatal(e)
					}
				}
			case "wrong-root":
				old := f.s.backupRoot
				f.s.backupRoot = filepath.Join(f.root, "other-backups")
				undo = func() { f.s.backupRoot = old }
			}
			defer undo()
			before := pairedPreviewFiles(t, f.root)
			r.calls = nil
			got, e := api.VerifyPrometheusPairedRestoreCheckpoint(context.Background(), ready.ID)
			if got != nil || !errors.Is(e, ErrPrometheusRecoveryAdmission) || strings.Contains(e.Error(), "private-corrupted") {
				t.Fatal("corrupt checkpoint accepted or leaked", e)
			}
			checkpointAssertReadOnly(t, r.calls)
			if !reflect.DeepEqual(before, pairedPreviewFiles(t, f.root)) {
				t.Fatal("verification repaired/removed evidence")
			}
			retained := checkpointJournal(t, f.db, ready.ID)
			if retained.Phase != "checkpoint_ready" || retained.Revision != 4 || !retained.UpdatedAt.Equal(ready.UpdatedAt) {
				t.Fatal("verification rewrote journal")
			}
			journalAssertClaimsEmpty(t, f.db)
		})
	}
	if got, err := api.VerifyPrometheusPairedRestoreCheckpoint(context.Background(), ready.ID); err != nil || got == nil {
		t.Fatal("fixture did not recover after explicit test undo", err)
	}
}
func TestPrometheusRestoreCheckpointStructuralPhasesAndCapability(t *testing.T) {
	_, _, prepared, api := checkpointFixture(t)
	row, err := api.CreatePrometheusPairedRestoreCheckpoint(context.Background(), prepared.ID)
	if err != nil {
		t.Fatal(err)
	}
	admission, err := decodeRestoreJournal(row)
	if err != nil {
		t.Fatal(err)
	}
	var valid prometheusRestoreCheckpointPayload
	if err := json.Unmarshal(row.Checkpoint, &valid); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"checkpointing", "checkpoint_stopping", "checkpoint_ready"} {
		t.Run(phase, func(t *testing.T) {
			copy := *row
			proof := valid
			if phase != "checkpoint_ready" {
				proof.StoppedAt = time.Time{}
			}
			if phase == "checkpointing" {
				proof.Evidence = nil
				copy.Revision = 2
			}
			if phase == "checkpoint_stopping" {
				evidence := *proof.Evidence
				evidence.Archive = recoveryPointArchive{}
				proof.Evidence = &evidence
				copy.Revision = 3
			}
			copy.Phase = phase
			copy.Checkpoint, _ = json.Marshal(proof)
			copy.CheckpointSHA256 = pointDigest(copy.Checkpoint)
			if _, err := decodeRestoreJournal(&copy); err != nil {
				t.Fatal("valid interrupted phase failed structural decoding", err)
			}
			if restoreJournalAccessAllows(context.WithValue(context.Background(), restoreJournalAccessKey{}, restoreJournalAccess{ID: copy.ID, Token: copy.Token, HostID: copy.HostID, ComponentID: copy.ComponentID}), &copy, copy.HostID, copy.ComponentID) {
				t.Fatal("prepared-only capability escaped into checkpoint")
			}
		})
	}
	for _, mode := range []string{"schema", "owner", "root", "started", "stopped", "binding", "pid", "start", "binary", "startup", "archive", "sources", "digest", "unknown", "trailing", "revision"} {
		t.Run(mode, func(t *testing.T) {
			copy := *row
			var p prometheusRestoreCheckpointPayload
			if err := json.Unmarshal(row.Checkpoint, &p); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "schema":
				p.Schema++
			case "owner":
				p.TransactionID++
			case "root":
				p.Root += "-wrong"
			case "started":
				p.StartedAt = time.Time{}
			case "stopped":
				p.StoppedAt = p.StartedAt.Add(-1)
			case "binding":
				p.Evidence.Binding.HostID++
			case "pid":
				p.Evidence.Runtime.PID = "0"
			case "start":
				p.Evidence.Runtime.StartTime = "0"
			case "binary":
				p.Evidence.Binary.Directory = "/outside/point-1-" + strings.Repeat("a", 32)
			case "startup":
				p.Evidence.Startup.Path = p.Evidence.Binary.Directory
			case "archive":
				p.Evidence.Archive.SHA256 = "bad"
			case "sources":
				p.Evidence.Archive.Sources[1] = p.Root
			case "revision":
				copy.Revision++
			}
			copy.Checkpoint, _ = json.Marshal(p)
			if mode == "unknown" {
				copy.Checkpoint = append(copy.Checkpoint[:len(copy.Checkpoint)-1], []byte(",\"Unexpected\":true}")...)
			}
			if mode == "trailing" {
				copy.Checkpoint = append(copy.Checkpoint, []byte("{}")...)
			}
			copy.CheckpointSHA256 = pointDigest(copy.Checkpoint)
			if mode == "digest" {
				copy.CheckpointSHA256 = strings.Repeat("b", 64)
			}
			if _, err := decodeRestoreJournal(&copy); err == nil {
				t.Fatal("malformed checkpoint accepted", mode)
			}
		})
	}
	access := restoreCheckpointAccess{ID: row.ID, Token: row.Token, HostID: row.HostID, ComponentID: row.ComponentID, Phase: row.Phase, Revision: row.Revision, AdmissionSHA256: row.PayloadSHA256, CheckpointSHA256: row.CheckpointSHA256}
	if !access.allows(row, row.HostID, row.ComponentID) {
		t.Fatal("exact ready access rejected")
	}
	for _, mode := range []string{"id", "token", "host", "component", "phase", "revision", "admission", "checkpoint", "neighbor", "other-host"} {
		copy := access
		hostID, componentID := row.HostID, row.ComponentID
		switch mode {
		case "id":
			copy.ID++
		case "token":
			copy.Token = strings.Repeat("c", 64)
		case "host":
			copy.HostID++
		case "component":
			copy.ComponentID++
		case "phase":
			copy.Phase = "prepared"
		case "revision":
			copy.Revision++
		case "admission":
			copy.AdmissionSHA256 = strings.Repeat("d", 64)
		case "checkpoint":
			copy.CheckpointSHA256 = strings.Repeat("e", 64)
		case "neighbor":
			componentID++
		case "other-host":
			componentID = 0
			hostID++
		}
		if copy.allows(row, hostID, componentID) {
			t.Fatal("checkpoint capability escaped exact owner", mode)
		}
	}
	if admission.Current.ComponentID != row.ComponentID {
		t.Fatal("lost original identity")
	}
}
