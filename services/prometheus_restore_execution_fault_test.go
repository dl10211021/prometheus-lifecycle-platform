package services

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	coressh "github.com/promlifecycle/platform/core/ssh"
	"github.com/promlifecycle/platform/models"
	"gorm.io/gorm"
)

func TestPrometheusRestoreExecutionFailureFences(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"health", "compensation", "commit-intent", "commit-terminal", "cancel", "lost-claim"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			f, r, staged := executionFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "health" {
				r.failHealth = true
			}
			if mode == "compensation" {
				r.failHistorical = true
				r.failCurrent = true
			}
			fired := false
			if strings.HasPrefix(mode, "commit-") {
				journalDeferredCommitFault(t, f.db)
				phase := "restore_executing"
				if mode == "commit-terminal" {
					phase = "restore_completed"
				}
				name := "execution-commit"
				if err := f.db.Callback().Update().Before("gorm:update").Register(name, func(tx *gorm.DB) {
					values, ok := tx.Statement.Dest.(map[string]any)
					if !ok || values["phase"] != phase {
						return
					}
					fired = true
					if mode == "commit-terminal" {
						var candidate models.Component
						if err := tx.Session(&gorm.Session{NewDB: true}).First(&candidate, f.c.ID).Error; err != nil {
							t.Fatal(err)
						}
						if candidate.Version != "2.55.0" || candidate.Image != "historical-image-reference" {
							t.Fatal("terminal transaction did not include matched historical metadata")
						}
					}
					tx.AddError(tx.Exec("INSERT INTO journal_fault_child(parent_id) VALUES (999)").Error)
				}); err != nil {
					t.Fatal(err)
				}
				defer f.db.Callback().Update().Remove(name)
			}
			r.before = func(_ context.Context, command string) error {
				if strings.Contains(command, "# promlifecycle:restore-execution-replace") {
					durable := checkpointJournal(t, f.db, staged.ID)
					if durable.Phase != "restore_executing" && durable.Phase != "restore_compensating" {
						t.Fatal("host replacement before committed intent")
					}
					if mode == "commit-intent" {
						t.Fatal("host replacement escaped failed COMMIT")
					}
					if mode == "commit-terminal" && durable.Phase == "restore_compensating" {
						var candidate models.Component
						var plan models.ChangePlan
						if err := f.db.First(&candidate, f.c.ID).Error; err != nil {
							t.Fatal(err)
						}
						if err := f.db.First(&plan, f.plan.ID).Error; err != nil {
							t.Fatal(err)
						}
						if candidate.Version != "3.1.0" || candidate.Image != "current-image-reference" || plan.Status != models.ChangeStatusFailed || len(plan.RollbackEvidence) != 0 {
							t.Fatal("rejected terminal commit leaked historical metadata or plan outcome")
						}
						if durable.ActiveHostID == nil || durable.ActiveComponentID == nil {
							t.Fatal("rejected terminal commit released durable ownership")
						}
					}
				}
				if !fired && strings.Contains(command, "# promlifecycle:restore-execution-start") {
					if mode == "cancel" {
						fired = true
						cancel()
						return context.Canceled
					}
					if mode == "lost-claim" {
						fired = true
						f.db.Where("host_id = ?", staged.HostID).Delete(&models.HostOperationClaim{})
						return errors.New("claim lost")
					}
				}
				return nil
			}
			row, err := f.s.ExecutePrometheusPairedRestore(ctx, staged.ID)
			if err == nil || row == nil {
				t.Fatal("injected execution fault not returned", mode, err)
			}
			if strings.HasPrefix(mode, "commit-") && !fired {
				t.Fatal("commit fault did not fire")
			}
			switch mode {
			case "health", "commit-terminal", "cancel":
				if row.Phase != "restore_compensated" {
					t.Fatal("confirmed failure did not compensate", row.Phase, err)
				}
				assertExecutionFiles(t, f, false)
			case "commit-intent":
				if row.Phase != "restore_staged" || f.runner.running {
					t.Fatal("failed intent advanced host or journal")
				}
			case "compensation":
				if row.Phase != "restore_compensation_starting" || row.ActiveHostID == nil || row.ActiveComponentID == nil {
					t.Fatal("failed compensation cleared fence", row.Phase)
				}
			case "lost-claim":
				if row.Phase != "restore_starting" || row.ActiveHostID == nil {
					t.Fatal("lost claim was stolen for compensation")
				}
			}
		})
	}
}
func TestPrometheusRestoreExecutionRejectsDuplicateForeignCorruptAndOuterTransaction(t *testing.T) {
	t.Parallel()
	f, r, staged := executionFixture(t)
	for _, kind := range []string{"host", "component", "outer", "corrupt"} {
		t.Run(kind, func(t *testing.T) {
			noHost := func(context.Context, string) error { t.Fatal("rejected authority contacted host"); return nil }
			r.before = noHost
			defer func() { r.before = nil }()
			var undo func()
			switch kind {
			case "host":
				c := models.HostOperationClaim{HostID: staged.HostID, Token: strings.Repeat("e", 48), Kind: "foreign", CreatedAt: time.Now()}
				if err := f.db.Create(&c).Error; err != nil {
					t.Fatal(err)
				}
				undo = func() { f.db.Where("host_id = ?", staged.HostID).Delete(&models.HostOperationClaim{}) }
			case "component":
				c := models.ComponentOperationClaim{ComponentID: staged.ComponentID, Token: strings.Repeat("e", 48), Kind: "foreign", CreatedAt: time.Now()}
				if err := f.db.Create(&c).Error; err != nil {
					t.Fatal(err)
				}
				undo = func() { f.db.Where("component_id = ?", staged.ComponentID).Delete(&models.ComponentOperationClaim{}) }
			case "outer":
				err := f.db.Transaction(func(tx *gorm.DB) error {
					scoped := *f.s
					scoped.db = tx
					if _, err := scoped.ExecutePrometheusPairedRestore(context.Background(), staged.ID); err == nil {
						t.Fatal("SAVEPOINT admitted as commit")
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				return
			case "corrupt":
				f.db.Model(&models.PrometheusRestoreTransaction{}).Where("id = ?", staged.ID).UpdateColumn("stage_sha256", strings.Repeat("0", 64))
				undo = func() {
					f.db.Model(&models.PrometheusRestoreTransaction{}).Where("id = ?", staged.ID).UpdateColumn("stage_sha256", staged.StageSHA256)
				}
			}
			if undo != nil {
				defer undo()
			}
			if _, err := f.s.ExecutePrometheusPairedRestore(context.Background(), staged.ID); err == nil {
				t.Fatal("invalid execution accepted", kind)
			}
		})
	}
	r.before = func(_ context.Context, command string) error {
		if strings.Contains(command, "# promlifecycle:restore-execution-start") {
			return coressh.ErrRemoteTerminationUnconfirmed
		}
		return nil
	}
	row, err := f.s.ExecutePrometheusPairedRestore(context.Background(), staged.ID)
	if err == nil || row == nil || row.Phase != "restore_starting" {
		t.Fatal("failed to create interrupted fixture", err)
	}
	r.before = func(context.Context, string) error { t.Fatal("pending POST replayed host writes"); return nil }
	if got, err := f.s.RestorePrometheusRecoveryPoint(context.Background(), f.plan.ID); err == nil || got == nil || got.ID != row.ID {
		t.Fatal("pending plan not returned/rejected", err)
	}
	if got, err := f.s.ExecutePrometheusPairedRestore(context.Background(), row.ID); err == nil || got == nil || got.ID != row.ID {
		t.Fatal("pending transaction replayed", err)
	}
}
func TestPrometheusRestoreExecutionTerminalEvidenceCannotHideCorruption(t *testing.T) {
	t.Parallel()
	f, _, staged := executionFixture(t)
	row, err := f.s.ExecutePrometheusPairedRestore(context.Background(), staged.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"argv", "finish-before-create", "verified-before-stage", "active-key", "historical-digest", "unknown-field"} {
		t.Run(kind, func(t *testing.T) {
			copy := *row
			var p restoreExecutionPayload
			if err := json.Unmarshal(copy.Execution, &p); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "argv":
				p.Runtime.ArgvSHA256 = strings.Repeat("f", 64)
			case "finish-before-create":
				before := row.CreatedAt.Add(-time.Hour)
				copy.FinishedAt = &before
			case "verified-before-stage":
				p.StartedAt = row.CreatedAt.Add(-2 * time.Hour)
				p.VerifiedAt = row.CreatedAt.Add(-time.Hour)
			case "active-key":
				copy.ActiveHostID = &copy.HostID
			case "historical-digest":
				p.Historical = []byte(strings.Replace(string(p.Historical), `2.55.0`, `2.55.1`, 1))
			}
			copy.Execution, _ = json.Marshal(p)
			if kind == "unknown-field" {
				copy.Execution = append(copy.Execution[:len(copy.Execution)-1], []byte(",\"Untrusted\":true}")...)
			}
			copy.ExecutionSHA256 = pointDigest(copy.Execution)
			if _, err := decodeRestoreJournal(&copy); err == nil {
				t.Fatal("malformed terminal accepted", kind)
			}
		})
	}
	// A structurally named terminal with invalid evidence must still block startup.
	if err := f.db.Model(row).UpdateColumn("execution_sha256", strings.Repeat("f", 64)).Error; err != nil {
		t.Fatal(err)
	}
	if err := CheckPrometheusRestoreJournalBeforeStartup(context.Background(), f.db); err == nil {
		t.Fatal("malformed terminal disappeared from startup inventory")
	}
	if got, err := f.s.GetPrometheusPairedRestoreTransaction(context.Background(), f.plan.ID); err == nil || got != nil {
		t.Fatal("malformed terminal exposed by read-only lookup")
	}
}
func TestPrometheusRestoreExecutionCorruptCopiedArtifactNeverSwitches(t *testing.T) {
	t.Parallel()
	f, r, row := executionFixture(t)
	p, err := decodeRestoreStage(row)
	if err != nil {
		t.Fatal(err)
	}
	var point models.PrometheusRecoveryPoint
	f.db.First(&point, row.PointID)
	historical, err := decodeRecoveryPointPayload(point.Payload)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(p.Fence.Root, "g-"+p.Fence.Generation, "historical", filepath.Base(historical.Binary.Directory), "prometheus")
	if err := os.WriteFile(path, []byte("corrupt-private-artifact"), 0600); err != nil {
		t.Fatal(err)
	}
	before := pairedPreviewFiles(t, f.root)
	r.before = func(_ context.Context, command string) error {
		if strings.Contains(command, "# promlifecycle:restore-execution-") {
			t.Fatal("corrupt artifact authorized replacement")
		}
		return nil
	}
	got, err := f.s.ExecutePrometheusPairedRestore(context.Background(), row.ID)
	if err == nil || got == nil || got.Phase != "restore_staged" || strings.Contains(err.Error(), "corrupt-private") {
		t.Fatal("corrupt set admitted or leaked", err)
	}
	if !equalJournalFiles(before, pairedPreviewFiles(t, f.root)) {
		t.Fatal("rejection mutated filesystem")
	}
}

func TestPrometheusRestoreExecutionZeroIDJournalCannotDisappear(t *testing.T) {
	db, components := journalGuardFixture(t)
	row := journalGuardRecord(t, db, components[0])
	if err := db.Model(&models.PrometheusRestoreTransaction{}).Where("id = ?", row.ID).UpdateColumns(map[string]any{"id": 0, "phase": "restore_completed", "active_host_id": nil, "active_component_id": nil, "finished_at": time.Now()}).Error; err != nil {
		t.Fatal(err)
	}
	if err := CheckPrometheusRestoreJournalBeforeStartup(context.Background(), db); err == nil {
		t.Fatal("malformed zero-ID terminal disappeared from startup inventory")
	}
}
