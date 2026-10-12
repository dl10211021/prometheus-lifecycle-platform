package services

import (
	"context"
	"errors"
	coressh "github.com/promlifecycle/platform/core/ssh"
	"github.com/promlifecycle/platform/models"
	platformsqlite "github.com/promlifecycle/platform/storage/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
	"strings"
	"testing"
	"time"
)

type stageUncertainRotationRunner struct {
	inner hostCommandRunner
	fired bool
}

func (r *stageUncertainRotationRunner) Run(ctx context.Context, cmd string) (string, error) {
	out, err := r.inner.Run(ctx, cmd)
	if !r.fired && strings.Contains(cmd, "# promlifecycle:restore-stage-fence") {
		r.fired = true
		if err == nil {
			return "", coressh.ErrRemoteTerminationUnconfirmed
		}
	}
	return out, err
}
func TestPrometheusRestoreStageUncertainRotationAndHandoffFailures(t *testing.T) {
	f, r, old := interruptedStageFixture(t)
	f.db.Logger = logger.Default.LogMode(logger.Silent)
	p, _ := decodeRestoreStage(old)
	host := models.HostOperationClaim{HostID: old.HostID, Token: p.Claims.Host, Kind: "abandoned", CreatedAt: time.Now()}
	component := models.ComponentOperationClaim{ComponentID: old.ComponentID, Token: p.Claims.Component, Kind: "abandoned", CreatedAt: time.Now()}
	if err := f.db.Create(&host).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Create(&component).Error; err != nil {
		t.Fatal(err)
	}
	_, lock, err := platformsqlite.AcquireDatabaseProcessLock(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	// A failed reclaim-intent statement must precede every host contact.
	for _, mode := range []string{"error", "zero", "commit"} {
		t.Run("intent-"+mode, func(t *testing.T) {
			if mode == "commit" {
				journalDeferredCommitFault(t, f.db)
			}
			f.db.Callback().Update().Before("gorm:update").Register("reclaim-intent-fault", func(tx *gorm.DB) {
				values, ok := tx.Statement.Dest.(map[string]any)
				if !ok || values["phase"] != "restore_stage_reclaim" {
					return
				}
				if mode == "error" {
					tx.AddError(errors.New("private intent fault"))
				}
				if mode == "zero" {
					tx.Statement.AddClause(clause.Where{Exprs: []clause.Expression{clause.Expr{SQL: "1=0"}}})
				}
				if mode == "commit" {
					tx.AddError(tx.Exec("INSERT INTO journal_fault_child(parent_id) VALUES (999)").Error)
				}
			})
			r.calls = nil
			_, err := f.s.RecoverPrometheusPairedRestoreStageForStartup(context.Background(), old.ID, lock)
			f.db.Callback().Update().Remove("reclaim-intent-fault")
			got := checkpointJournal(t, f.db, old.ID)
			if err == nil || len(r.calls) != 0 || !sameRestoreStageRow(old, &got) {
				t.Fatal("failed intent contacted host/changed row", err)
			}
		})
	}
	uncertain := &stageUncertainRotationRunner{inner: r}
	f.s.runnerFactory = func(*models.Component) (hostCommandRunner, error) { return uncertain, nil }
	_, err = f.s.RecoverPrometheusPairedRestoreStageForStartup(context.Background(), old.ID, lock)
	if !errors.Is(err, coressh.ErrRemoteTerminationUnconfirmed) || !uncertain.fired {
		t.Fatal("uncertain rotation not retained", err)
	}
	reclaim := checkpointJournal(t, f.db, old.ID)
	rp, err := decodeRestoreStage(&reclaim)
	if err != nil || reclaim.Phase != "restore_stage_reclaim" {
		t.Fatal("uncertain rotation lost intent", err)
	}
	rotated, err := rotateRestoreStageFence(context.Background(), localBackupRunner{}, rp.Fence, rp.Reclaim.Desired)
	if err != nil {
		t.Fatal(err)
	}
	assertUnchanged := func() {
		row := checkpointJournal(t, f.db, old.ID)
		if !sameRestoreStageRow(&reclaim, &row) {
			t.Fatal("failed handoff published phase")
		}
		var gotHost models.HostOperationClaim
		var gotComponent models.ComponentOperationClaim
		if err := f.db.First(&gotHost, "host_id = ?", old.HostID).Error; err != nil || gotHost.Token != host.Token || gotHost.Kind != host.Kind || !gotHost.CreatedAt.Equal(host.CreatedAt) {
			t.Fatal("failed handoff changed old host", err)
		}
		if err := f.db.First(&gotComponent, "component_id = ?", old.ComponentID).Error; err != nil || gotComponent.Token != component.Token || gotComponent.Kind != component.Kind || !gotComponent.CreatedAt.Equal(component.CreatedAt) {
			t.Fatal("failed handoff changed old component", err)
		}
	}
	assertUnchanged()
	for _, operation := range []string{"delete-host", "delete-component", "create-host", "create-component", "publish"} {
		for _, mode := range []string{"error", "zero"} {
			t.Run(operation+"/"+mode, func(t *testing.T) {
				fired := false
				callback := func(tx *gorm.DB) {
					target := "host_operation_claims"
					if strings.HasSuffix(operation, "component") {
						target = "component_operation_claims"
					}
					if operation == "publish" {
						target = "prometheus_restore_transactions"
					}
					if tx.Statement.Table != target {
						return
					}
					fired = true
					if mode == "error" {
						tx.AddError(errors.New("private handoff fault"))
						return
					}
					if strings.HasPrefix(operation, "create") {
						// Override the actual INSERT result to zero and require transaction rollback.
						tx.RowsAffected = 0
					} else {
						tx.Statement.AddClause(clause.Where{Exprs: []clause.Expression{clause.Expr{SQL: "1=0"}}})
					}
				}
				name := "handoff-" + operation + "-" + mode
				if strings.HasPrefix(operation, "delete") {
					if err := f.db.Callback().Delete().Before("gorm:delete").Register(name, callback); err != nil {
						t.Fatal(err)
					}
				}
				if strings.HasPrefix(operation, "create") {
					if mode == "zero" {
						if err := f.db.Callback().Create().After("gorm:create").Register(name, callback); err != nil {
							t.Fatal(err)
						}
					} else {
						if err := f.db.Callback().Create().Before("gorm:create").Register(name, callback); err != nil {
							t.Fatal(err)
						}
					}
				}
				if operation == "publish" {
					if err := f.db.Callback().Update().Before("gorm:update").Register(name, callback); err != nil {
						t.Fatal(err)
					}
				}
				next, _, release, err := f.s.quietRestoreStageService().publishRestoreStageTakeover(context.Background(), &reclaim, rotated)
				if strings.HasPrefix(operation, "delete") {
					if err := f.db.Callback().Delete().Remove(name); err != nil {
						t.Fatal(err)
					}
				}
				if strings.HasPrefix(operation, "create") {
					if err := f.db.Callback().Create().Remove(name); err != nil {
						t.Fatal(err)
					}
				}
				if operation == "publish" {
					if err := f.db.Callback().Update().Remove(name); err != nil {
						t.Fatal(err)
					}
				}
				if !fired {
					t.Fatal("target handoff injection was not observed")
				}
				if err == nil || next != nil || release != nil {
					if release != nil {
						release()
					}
					t.Fatal("handoff fault returned authority", err)
				}
				assertUnchanged()
			})
		}
	}
	f.s.runnerFactory = func(*models.Component) (hostCommandRunner, error) { return r, nil }
	final, err := f.s.RecoverPrometheusPairedRestoreStageForStartup(context.Background(), old.ID, lock)
	if err != nil || final.Phase != "restore_staged" {
		t.Fatal("uncertain intent retry failed", err)
	}
	journalAssertClaimsEmpty(t, f.db)
}
