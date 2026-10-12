package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	platformsqlite "github.com/promlifecycle/platform/storage/sqlite"
	"gorm.io/gorm"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type restoreStageRecoveryAPI interface {
	RecoverPrometheusPairedRestoreStageForStartup(context.Context, uint, *platformsqlite.DatabaseProcessLock) (*models.PrometheusRestoreTransaction, error)
}

func stageRecoveryAPI(t *testing.T, s *BackupService) restoreStageRecoveryAPI {
	t.Helper()
	api, ok := any(s).(restoreStageRecoveryAPI)
	if !ok {
		t.Fatal("BackupService lacks capability-gated takeover")
	}
	return api
}
func TestPrometheusRestoreStageRecoveryCopiesNewGeneration(t *testing.T) {
	f, r, prepared, checkpoint := checkpointFixture(t)
	api := stageRecoveryAPI(t, f.s)
	ready, err := checkpoint.CreatePrometheusPairedRestoreCheckpoint(context.Background(), prepared.ID)
	if err != nil {
		t.Fatal(err)
	}
	fired := false
	r.before = func(ctx context.Context, cmd string) error {
		if strings.Contains(cmd, "# promlifecycle:restore-stage-copy") {
			fired = true
			row := checkpointJournal(t, f.db, ready.ID)
			p, _ := decodeRestoreStage(&row)
			gen := filepath.Join(p.Fence.Root, "g-"+p.Fence.Generation)
			if err := runRestoreStageFenced(ctx, localBackupRunner{}, p.Fence, "mkdir -m 0700 -- "+utils.QuoteShell(gen)+"; printf forensic > "+utils.QuoteShell(filepath.Join(gen, "partial"))+"; sync -f -- "+utils.QuoteShell(gen)); err != nil {
				t.Fatal(err)
			}
			return errors.New("private-failed-copy")
		}
		return nil
	}
	if _, err := stageAPI(t, f.s).StagePrometheusPairedRestore(context.Background(), ready.ID); err == nil || !fired {
		t.Fatal("fault did not stop copy", err)
	}
	old := checkpointJournal(t, f.db, ready.ID)
	if old.Phase != "restore_stage_copying" {
		t.Fatal(old.Phase)
	}
	oldProof, err := decodeRestoreStage(&old)
	if err != nil {
		t.Fatal(err)
	}
	r.before = nil
	r.calls = nil
	if _, err := api.RecoverPrometheusPairedRestoreStageForStartup(context.Background(), ready.ID, nil); err == nil || len(r.calls) != 0 {
		t.Fatal("nil capability contacted host", err)
	}
	_, ownership, err := platformsqlite.AcquireDatabaseProcessLock(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ownership.Close()
	got, err := api.RecoverPrometheusPairedRestoreStageForStartup(context.Background(), ready.ID, ownership)
	if err != nil {
		t.Fatal(err)
	}
	p, err := decodeRestoreStage(got)
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != "restore_staged" || got.Revision != old.Revision+3 || len(p.Generations) != 2 || p.Fence.Generation == oldProof.Fence.Generation || p.Claims == oldProof.Claims || p.Fence.RootIdentity != oldProof.Fence.RootIdentity || p.Fence.LockIdentity != oldProof.Fence.LockIdentity || f.runner.running {
		t.Fatal("takeover did not preserve fence and isolate writer")
	}
	stalePath := filepath.Join(oldProof.Fence.Root, "g-"+oldProof.Fence.Generation, "stale-write")
	if err := runRestoreStageFenced(context.Background(), localBackupRunner{}, oldProof.Fence, "printf stale > "+utils.QuoteShell(stalePath)); err == nil {
		t.Fatal("delayed old writer accepted")
	}
	if _, err := rotateRestoreStageFence(context.Background(), localBackupRunner{}, oldProof.Fence, strings.Repeat("f", 64)); err == nil {
		t.Fatal("delayed old rotation accepted")
	}
	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Fatal("stale writer changed previous generation")
	}
	if raw, err := os.ReadFile(filepath.Join(oldProof.Fence.Root, "g-"+oldProof.Fence.Generation, "partial")); err != nil || string(raw) != "forensic" {
		t.Fatal("old partial generation not retained", err)
	}
	journalAssertClaimsEmpty(t, f.db)
}

func interruptedStageFixture(t *testing.T) (*pointFixture, *pairedPreviewRunner, *models.PrometheusRestoreTransaction) {
	t.Helper()
	f, r, prepared, cp := checkpointFixture(t)
	ready, err := cp.CreatePrometheusPairedRestoreCheckpoint(context.Background(), prepared.ID)
	if err != nil {
		t.Fatal(err)
	}
	r.before = func(_ context.Context, cmd string) error {
		if strings.Contains(cmd, "# promlifecycle:restore-stage-copy") {
			return errors.New("fixture interrupted copy")
		}
		return nil
	}
	if _, err := f.s.StagePrometheusPairedRestore(context.Background(), ready.ID); err == nil {
		t.Fatal("copy fault absent")
	}
	r.before = nil
	r.calls = nil
	row := checkpointJournal(t, f.db, ready.ID)
	if row.Phase != "restore_stage_copying" {
		t.Fatal(row.Phase)
	}
	return f, r, &row
}
func TestPrometheusRestoreStageRecoveryAtomicClaimsAndCommitRetry(t *testing.T) {
	f, r, old := interruptedStageFixture(t)
	p, _ := decodeRestoreStage(old)
	for _, claim := range []any{&models.HostOperationClaim{HostID: old.HostID, Token: p.Claims.Host, Kind: "abandoned", CreatedAt: time.Now()}, &models.ComponentOperationClaim{ComponentID: old.ComponentID, Token: p.Claims.Component, Kind: "abandoned", CreatedAt: time.Now()}} {
		if err := f.db.Create(claim).Error; err != nil {
			t.Fatal(err)
		}
	}
	_, lock, err := platformsqlite.AcquireDatabaseProcessLock(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	journalDeferredCommitFault(t, f.db)
	fired := false
	f.db.Callback().Update().Before("gorm:update").Register("takeover-commit", func(tx *gorm.DB) {
		values, ok := tx.Statement.Dest.(map[string]any)
		if !ok || values["phase"] != "restore_stage_copying" {
			return
		}
		fired = true
		tx.AddError(tx.Exec("INSERT INTO journal_fault_child(parent_id) VALUES (999)").Error)
	})
	rotated := false
	r.before = func(_ context.Context, cmd string) error {
		if strings.Contains(cmd, "# promlifecycle:restore-stage-fence") {
			row := checkpointJournal(t, f.db, old.ID)
			if row.Phase != "restore_stage_reclaim" {
				t.Fatal("rotation preceded reclaim COMMIT")
			}
			proof, _ := decodeRestoreStage(&row)
			if proof.Reclaim.Captured != p.Claims {
				t.Fatal("intent failed to pin exact old claims")
			}
			// Simulate delayed old release after durable reclaim intent, before handoff.
			if !rotated {
				if err := f.db.Where("host_id = ? AND token = ?", old.HostID, p.Claims.Host).Delete(&models.HostOperationClaim{}).Error; err != nil {
					t.Fatal(err)
				}
				rotated = true
			}
		}
		return nil
	}
	if _, err := f.s.RecoverPrometheusPairedRestoreStageForStartup(context.Background(), old.ID, lock); err == nil || !fired || !rotated {
		t.Fatal("deferred handoff COMMIT fault absent", err)
	}
	f.db.Callback().Update().Remove("takeover-commit")
	reclaim := checkpointJournal(t, f.db, old.ID)
	rp, _ := decodeRestoreStage(&reclaim)
	if reclaim.Phase != "restore_stage_reclaim" {
		t.Fatal("failed handoff published phase")
	}
	generation, err := os.ReadFile(filepath.Join(p.Fence.Root, "generation"))
	if err != nil || string(generation) != rp.Reclaim.Desired+"\n" {
		t.Fatal("rotation did not precede failed handoff", err)
	}
	var h models.HostOperationClaim
	var c models.ComponentOperationClaim
	if f.db.Where("host_id = ?", old.HostID).Find(&h).RowsAffected != 0 {
		t.Fatal("failed COMMIT created host claim")
	}
	if err := f.db.First(&c, "component_id = ?", old.ComponentID).Error; err != nil || c.Token != p.Claims.Component {
		t.Fatal("failed COMMIT retired component", err)
	}
	checkedNew := false
	r.before = func(_ context.Context, cmd string) error {
		if strings.Contains(cmd, "# promlifecycle:restore-stage-copy") {
			row := checkpointJournal(t, f.db, old.ID)
			proof, _ := decodeRestoreStage(&row)
			if row.Phase != "restore_stage_copying" || proof.Fence.Generation != rp.Reclaim.Desired || proof.Claims != rp.Reclaim.Replacement {
				t.Fatal("retry did not reuse pinned takeover")
			}
			for _, del := range []*gorm.DB{f.db.Where("host_id = ? AND token = ?", old.HostID, p.Claims.Host).Delete(&models.HostOperationClaim{}), f.db.Where("component_id = ? AND token = ?", old.ComponentID, p.Claims.Component).Delete(&models.ComponentOperationClaim{})} {
				if del.Error != nil || del.RowsAffected != 0 {
					t.Fatal("delayed cleanup removed replacement")
				}
			}
			if err := f.db.First(&h, "host_id = ?", old.HostID).Error; err != nil || h.Token != proof.Claims.Host {
				t.Fatal("fresh host absent", err)
			}
			if err := f.db.First(&c, "component_id = ?", old.ComponentID).Error; err != nil || c.Token != proof.Claims.Component {
				t.Fatal("fresh component absent", err)
			}
			checkedNew = true
		}
		return nil
	}
	got, err := f.s.RecoverPrometheusPairedRestoreStageForStartup(context.Background(), old.ID, lock)
	if err != nil || !checkedNew || got.Phase != "restore_staged" {
		t.Fatal("same-intent retry failed", err)
	}
	journalAssertClaimsEmpty(t, f.db)
}
func TestPrometheusRestoreStageRecoveryRejectsForeignOwnershipBeforeHost(t *testing.T) {
	f, r, row := interruptedStageFixture(t)
	p, _ := decodeRestoreStage(row)
	_, lock, err := platformsqlite.AcquireDatabaseProcessLock(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	for _, mode := range []string{"host-replaced", "component-replaced", "sibling", "whole-db-pending", "whole-db-malformed", "max-generations", "hardlink-db", "old-checkpoint", "pause", "version"} {
		t.Run(mode, func(t *testing.T) {
			undo := func() {}
			switch mode {
			case "host-replaced":
				claim := models.HostOperationClaim{HostID: row.HostID, Token: strings.Repeat("e", 48), Kind: "foreign", CreatedAt: time.Now()}
				f.db.Create(&claim)
				undo = func() { f.db.Where("host_id = ?", row.HostID).Delete(&claim) }
			case "component-replaced", "sibling":
				id := row.ComponentID
				if mode == "sibling" {
					sibling := models.Component{Name: "stage-sibling", HostID: row.HostID, ClusterID: f.c.ClusterID, Type: models.ComponentPrometheus, DeployType: "binary", Version: f.c.Version, Status: models.StatusRunning}
					if err := f.db.Create(&sibling).Error; err != nil {
						t.Fatal(err)
					}
					id = sibling.ID
				}
				claim := models.ComponentOperationClaim{ComponentID: id, Token: strings.Repeat("e", 48), Kind: "foreign", CreatedAt: time.Now()}
				f.db.Create(&claim)
				undo = func() {
					f.db.Where("component_id = ?", id).Delete(&claim)
					if mode == "sibling" {
						f.db.Delete(&models.Component{}, id)
					}
				}
			case "whole-db-pending", "whole-db-malformed":
				raw := []byte("{\"status\":\"pending\"}")
				if mode == "whole-db-malformed" {
					raw = []byte("{\"status\":\"applied\",\"unknown\":true}")
				}
				os.WriteFile(f.dbPath+".restore.json", raw, 0600)
				undo = func() { os.Remove(f.dbPath + ".restore.json") }
			case "max-generations":
				copy := *p
				copy.Generations = nil
				for i := 1; i <= 32; i++ {
					copy.Generations = append(copy.Generations, fmt.Sprintf("%064x", i))
				}
				copy.Fence.Generation = copy.Generations[31]
				raw, _ := json.Marshal(copy)
				f.db.Model(&models.PrometheusRestoreTransaction{}).Where("id = ?", row.ID).UpdateColumns(map[string]any{"stage": string(raw), "stage_sha256": pointDigest(raw), "revision": uint64(98)})
				undo = func() {
					f.db.Model(&models.PrometheusRestoreTransaction{}).Where("id = ?", row.ID).UpdateColumns(map[string]any{"stage": string(row.Stage), "stage_sha256": row.StageSHA256, "revision": row.Revision})
				}
			case "hardlink-db":
				os.Link(f.dbPath, f.dbPath+".alias")
				undo = func() { os.Remove(f.dbPath + ".alias") }
			case "old-checkpoint":
				f.db.Model(&models.PrometheusRestoreTransaction{}).Where("id = ?", row.ID).UpdateColumns(map[string]any{"phase": "checkpoint_ready", "revision": 4, "stage": nil, "stage_sha256": nil})
				undo = func() {
					f.db.Model(&models.PrometheusRestoreTransaction{}).Where("id = ?", row.ID).UpdateColumns(map[string]any{"phase": row.Phase, "revision": row.Revision, "stage": string(row.Stage), "stage_sha256": row.StageSHA256})
				}
			case "pause", "version":
				pause := ComponentBackupPause{ComponentID: row.ComponentID, Status: "paused"}
				if mode == "version" {
					pause.Status = "resumed"
					pause.VersionTransitionPlanID = row.PlanID
				}
				var original ComponentBackupPause
				if err := f.db.First(&original, "component_id = ?", row.ComponentID).Error; err != nil {
					t.Fatal(err)
				}
				if err := f.db.Save(&pause).Error; err != nil {
					t.Fatal(err)
				}
				undo = func() {
					if err := f.db.Save(&original).Error; err != nil {
						t.Fatal(err)
					}
				}
			}
			defer undo()
			r.calls = nil
			before := checkpointJournal(t, f.db, row.ID)
			files := pairedPreviewFiles(t, f.root)
			if _, err := f.s.RecoverPrometheusPairedRestoreStageForStartup(context.Background(), row.ID, lock); err == nil {
				t.Fatal("foreign state admitted", mode)
			}
			after := checkpointJournal(t, f.db, row.ID)
			if !sameRestoreStageRow(&before, &after) || len(r.calls) != 0 || !reflect.DeepEqual(files, pairedPreviewFiles(t, f.root)) {
				t.Fatal("rejected recovery changed evidence/contacted host", mode)
			}
		})
	}
}

func TestPrometheusRestoreStageInitializedMissingControlsNeverRepair(t *testing.T) {
	f, r, row := interruptedStageFixture(t)
	p, _ := decodeRestoreStage(row)
	_, lock, err := platformsqlite.AcquireDatabaseProcessLock(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	for _, name := range []string{"root", "lock", "generation"} {
		t.Run(name, func(t *testing.T) {
			path := p.Fence.Root
			if name != "root" {
				path = filepath.Join(path, name)
			}
			away := filepath.Join(f.root, "held-"+name)
			if err := os.Rename(path, away); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := os.Rename(away, path); err != nil {
					t.Fatal(err)
				}
				if err := f.db.Model(&models.PrometheusRestoreTransaction{}).Where("id = ?", row.ID).UpdateColumns(map[string]any{"phase": row.Phase, "revision": row.Revision, "stage": string(row.Stage), "stage_sha256": row.StageSHA256}).Error; err != nil {
					t.Fatal(err)
				}
			}()
			r.calls = nil
			if _, err := f.s.RecoverPrometheusPairedRestoreStageForStartup(context.Background(), row.ID, lock); err == nil {
				t.Fatal("missing initialized control repaired")
			}
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatal("missing control recreated")
			}
			retained := checkpointJournal(t, f.db, row.ID)
			if retained.Phase != "restore_stage_reclaim" {
				t.Fatal("missing control lost reclaim intent")
			}
			journalAssertClaimsEmpty(t, f.db)
		})
	}
}
