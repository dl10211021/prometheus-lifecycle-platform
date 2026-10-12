package services

import (
	"context"
	"encoding/json"
	"errors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPrometheusRestoreStageCommitBoundaries(t *testing.T) {
	for stage, phase := range []string{"restore_stage_intent", "restore_stage_copying", "restore_staged"} {
		for _, mode := range []string{"error", "zero", "commit", "cancel"} {
			t.Run(phase+"/"+mode, func(t *testing.T) {
				t.Parallel()
				f, r, prepared, cp := checkpointFixture(t)
				ready, err := cp.CreatePrometheusPairedRestoreCheckpoint(context.Background(), prepared.ID)
				if err != nil {
					t.Fatal(err)
				}
				before := pairedPreviewFiles(t, f.root)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if mode == "commit" {
					journalDeferredCommitFault(t, f.db)
				}
				fired := false
				f.db.Callback().Update().Before("gorm:update").Register("stage-fault", func(tx *gorm.DB) {
					values, ok := tx.Statement.Dest.(map[string]any)
					if !ok || tx.Statement.Table != "prometheus_restore_transactions" || values["phase"] != phase {
						return
					}
					fired = true
					switch mode {
					case "error":
						tx.AddError(errors.New("private-stage-sql"))
					case "zero":
						tx.Statement.AddClause(clause.Where{Exprs: []clause.Expression{clause.Expr{SQL: "1=0"}}})
					case "cancel":
						cancel()
					case "commit":
						tx.AddError(tx.Exec("INSERT INTO journal_fault_child(parent_id) VALUES (999)").Error)
					}
				})
				_, err = f.s.StagePrometheusPairedRestore(ctx, ready.ID)
				f.db.Callback().Update().Remove("stage-fault")
				if !fired || err == nil || !errors.Is(err, ErrPrometheusRecoveryAdmission) || strings.Contains(err.Error(), "private-stage") {
					t.Fatal("phase fault admitted/leaked", err)
				}
				if errors.Is(err, context.Canceled) != (mode == "cancel") {
					t.Fatal("cancellation identity", err)
				}
				row := checkpointJournal(t, f.db, ready.ID)
				if row.Phase != []string{"checkpoint_ready", "restore_stage_intent", "restore_stage_copying"}[stage] || row.Revision != uint64(4+stage) {
					t.Fatal("uncommitted phase escaped", row.Phase, row.Revision)
				}
				if _, err := decodeRestoreJournal(&row); err != nil {
					t.Fatal("partial durable evidence invalid", err)
				}
				after := pairedPreviewFiles(t, f.root)
				for p, v := range before {
					if after[p] != v {
						t.Fatal("changed original", p)
					}
				}
				root := restoreStageRoot(f.s.backupRoot, ready)
				if stage == 0 {
					if _, err := os.Stat(root); !os.IsNotExist(err) {
						t.Fatal("host init preceded intent COMMIT")
					}
				}
				if stage == 1 {
					entries, _ := os.ReadDir(root)
					for _, e := range entries {
						if strings.HasPrefix(e.Name(), "g-") {
							t.Fatal("copy preceded copying COMMIT")
						}
					}
				}
				if f.runner.running {
					t.Fatal("failure restarted original")
				}
				r.before = nil
				journalAssertClaimsEmpty(t, f.db)
			})
		}
	}
}
func TestPrometheusRestoreStageStrictEvidenceAndReadOnlyDrift(t *testing.T) {
	f, r, prepared, cp := checkpointFixture(t)
	ready, err := cp.CreatePrometheusPairedRestoreCheckpoint(context.Background(), prepared.ID)
	if err != nil {
		t.Fatal(err)
	}
	staged, err := f.s.StagePrometheusPairedRestore(context.Background(), ready.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"digest", "trailing", "unknown", "revision", "root", "claims", "generations", "identity", "premature", "checkpoint", "reclaim-missing-origin"} {
		t.Run(mode, func(t *testing.T) {
			row := *staged
			var p prometheusRestoreStagePayload
			json.Unmarshal(row.Stage, &p)
			switch mode {
			case "root":
				p.Fence.Root += "-other"
			case "claims":
				p.Claims.Host = "forged"
			case "generations":
				p.Generations = make([]string, 33)
			case "identity":
				p.Fence.LockIdentity = ""
			case "premature":
				row.Phase = "restore_stage_copying"
			case "revision":
				row.Revision = ^uint64(0)
			case "checkpoint":
				row.Checkpoint = nil
				row.CheckpointSHA256 = ""
			case "reclaim-missing-origin":
				row.Phase = "restore_stage_reclaim"
				row.Revision++
				p.CompletedAt = time.Time{}
				p.Fence.RootIdentity = ""
				p.Fence.LockIdentity = ""
				p.Reclaim = &restoreStageReclaim{Desired: strings.Repeat("a", 64), Replacement: restoreStageClaims{strings.Repeat("a", 48), strings.Repeat("b", 48)}, StartedAt: time.Now()}
			}
			row.Stage, _ = json.Marshal(p)
			if mode == "unknown" {
				row.Stage = append(row.Stage[:len(row.Stage)-1], []byte(",\"Extra\":true}")...)
			}
			if mode == "trailing" {
				row.Stage = append(row.Stage, []byte("{}")...)
			}
			row.StageSHA256 = pointDigest(row.Stage)
			if mode == "digest" {
				row.StageSHA256 = strings.Repeat("c", 64)
			}
			if _, err := decodeRestoreJournal(&row); err == nil {
				t.Fatal("malformed phase evidence admitted", mode)
			}
		})
	}
	p, _ := decodeRestoreStage(staged)
	for _, mode := range []string{"current-archive", "historical-binary", "current-startup", "generation", "lock", "root-extra", "generation-extra", "restarted"} {
		t.Run(mode, func(t *testing.T) {
			gen := filepath.Join(p.Fence.Root, "g-"+p.Fence.Generation)
			var undo func()
			switch mode {
			case "restarted":
				f.runner.running = true
				undo = func() { f.runner.running = false }
			case "root-extra", "generation-extra":
				path := filepath.Join(p.Fence.Root, "unexpected")
				if mode == "generation-extra" {
					path = filepath.Join(gen, "unexpected")
				}
				os.WriteFile(path, []byte("untrusted"), 0600)
				undo = func() { os.Remove(path) }
			default:
				path := filepath.Join(p.Fence.Root, "generation")
				if mode == "lock" {
					path = filepath.Join(p.Fence.Root, "lock")
				}
				if mode == "current-archive" || mode == "historical-binary" || mode == "current-startup" {
					side, base := "current", "payload.tar.gz"
					if mode == "historical-binary" {
						side, base = "historical", "prometheus"
					}
					if mode == "current-startup" {
						base = "manifest.json"
					}
					filepath.Walk(filepath.Join(gen, side), func(candidate string, info os.FileInfo, e error) error {
						if e == nil && info.Mode().IsRegular() && info.Name() == base {
							path = candidate
						}
						return e
					})
				}
				raw, e := os.ReadFile(path)
				if e != nil {
					t.Fatal(e)
				}
				os.WriteFile(path, []byte("private-stage-corruption"), 0600)
				undo = func() { os.WriteFile(path, raw, 0600) }
			}
			defer undo()
			before := pairedPreviewFiles(t, f.root)
			r.calls = nil
			if _, err := f.s.VerifyPrometheusPairedRestoreStage(context.Background(), staged.ID); err == nil || strings.Contains(err.Error(), "private-stage") {
				t.Fatal("corruption admitted/leaked", err)
			}
			if !reflect.DeepEqual(before, pairedPreviewFiles(t, f.root)) {
				t.Fatal("read-only verification repaired artifacts")
			}
			row := checkpointJournal(t, f.db, staged.ID)
			if !sameRestoreStageRow(staged, &row) {
				t.Fatal("verification changed evidence")
			}
			journalAssertClaimsEmpty(t, f.db)
		})
	}
}
