package services

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/promlifecycle/platform/models"
	platformsqlite "github.com/promlifecycle/platform/storage/sqlite"
)

type readyObservationBoundaryRunner struct {
	hostCommandRunner
	fences int
	after  func()
}

func (r *readyObservationBoundaryRunner) Run(ctx context.Context, cmd string) (string, error) {
	out, err := r.hostCommandRunner.Run(ctx, cmd)
	if err == nil && strings.Contains(cmd, "# promlifecycle:restore-stage-fence") {
		r.fences++
		if r.fences == 2 {
			r.after()
		}
	}
	return out, err
}
func TestPrometheusRestoreStageReadyObservationOwnershipDrift(t *testing.T) {
	for _, method := range []string{"stage", "verify"} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()
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
			proof, err := decodeRestoreStage(&initial)
			if err != nil {
				t.Fatal(err)
			}
			// Mutations originate on another actual connection, not a shared transaction snapshot.
			db, err := platformsqlite.OpenDB(f.dbPath)
			if err != nil {
				t.Fatal(err)
			}
			conn, _ := db.DB()
			defer conn.Close()
			op := f.s.StagePrometheusPairedRestore
			if method == "verify" {
				op = f.s.VerifyPrometheusPairedRestoreStage
			}
			if (restoreStageAccess{initial}).allows(&initial, initial.HostID, 0) {
				t.Fatal("ready row mints writer access")
			}
			if _, release, err := ClaimHostOperation(context.Background(), db, initial.HostID, "business-writer"); err == nil {
				release()
				t.Fatal("journal did not fence host writer")
			}
			if _, release, err := ClaimComponentOperation(context.Background(), db, initial.ComponentID, "business-writer"); err == nil {
				release()
				t.Fatal("journal did not fence component writer")
			}
			journalAssertClaimsEmpty(t, db)
			t.Run("outer-transaction", func(t *testing.T) {
				tx := db.Begin()
				if tx.Error != nil {
					t.Fatal(tx.Error)
				}
				defer tx.Rollback()
				scoped := *f.s
				scoped.db = tx
				call := scoped.StagePrometheusPairedRestore
				if method == "verify" {
					call = scoped.VerifyPrometheusPairedRestoreStage
				}
				r.calls = nil
				if _, err := call(context.Background(), initial.ID); err == nil {
					t.Fatal("transaction snapshot admitted")
				}
				after := checkpointJournal(t, tx, initial.ID)
				if len(r.calls) != 0 || !sameRestoreStageRow(&initial, &after) || !initial.UpdatedAt.Equal(after.UpdatedAt) {
					t.Fatal("transaction observer acted")
				}
				journalAssertClaimsEmpty(t, tx)
			})
			foreign := strings.Repeat("f", 48)
			sibling := models.Component{ClusterID: f.c.ClusterID, HostID: initial.HostID, Type: "node_exporter", Name: "ready-observer-sibling"}
			if err := db.Create(&sibling).Error; err != nil {
				t.Fatal(err)
			}
			for _, kind := range []string{"host", "component", "sibling"} {
				t.Run("foreign-"+kind, func(t *testing.T) {
					componentID := initial.ComponentID
					if kind == "sibling" {
						componentID = sibling.ID
					}
					if kind == "host" {
						if err := db.Create(&models.HostOperationClaim{HostID: initial.HostID, Token: foreign, Kind: "foreign", CreatedAt: time.Now()}).Error; err != nil {
							t.Fatal(err)
						}
						defer db.Where("host_id = ? AND token = ?", initial.HostID, foreign).Delete(&models.HostOperationClaim{})
					} else {
						if err := db.Create(&models.ComponentOperationClaim{ComponentID: componentID, Token: foreign, Kind: "foreign", CreatedAt: time.Now()}).Error; err != nil {
							t.Fatal(err)
						}
						defer db.Where("component_id = ? AND token = ?", componentID, foreign).Delete(&models.ComponentOperationClaim{})
					}
					r.calls = nil
					if _, err := op(context.Background(), initial.ID); err == nil {
						t.Fatal("foreign ownership accepted")
					}
					after := checkpointJournal(t, db, initial.ID)
					if len(r.calls) != 0 || !sameRestoreStageRow(&initial, &after) {
						t.Fatal("foreign observation acted")
					}
				})
			}
			journalAssertClaimsEmpty(t, db)
			for _, kind := range []string{"timestamp", "late-foreign", "pinned-release", "takeover"} {
				t.Run(kind, func(t *testing.T) {
					if kind == "pinned-release" {
						if err := db.Create(&models.HostOperationClaim{HostID: initial.HostID, Token: proof.Claims.Host, Kind: "paired-restore-stage", CreatedAt: time.Now()}).Error; err != nil {
							t.Fatal(err)
						}
					}
					fired := false
					wrapper := &readyObservationBoundaryRunner{hostCommandRunner: r}
					wrapper.after = func() {
						fired = true
						switch kind {
						case "timestamp":
							if err := db.Model(&models.PrometheusRestoreTransaction{}).Where("id = ?", initial.ID).UpdateColumn("updated_at", initial.UpdatedAt.Add(time.Second)).Error; err != nil {
								t.Fatal(err)
							}
						case "late-foreign":
							if err := db.Create(&models.HostOperationClaim{HostID: initial.HostID, Token: foreign, Kind: "foreign", CreatedAt: time.Now()}).Error; err != nil {
								t.Fatal(err)
							}
						case "pinned-release":
							if result := db.Where("host_id = ? AND token = ?", initial.HostID, proof.Claims.Host).Delete(&models.HostOperationClaim{}); result.Error != nil || result.RowsAffected != 1 {
								t.Fatal("exact pinned release failed", result.Error)
							}
						case "takeover":
							_, lock, err := platformsqlite.AcquireDatabaseProcessLock(f.dbPath)
							if err != nil {
								t.Fatal(err)
							}
							defer lock.Close()
							other := NewBackupServiceWithRoot(db, f.s.backupRoot)
							other.runnerFactory = func(*models.Component) (hostCommandRunner, error) { return r, nil }
							got, err := other.RecoverPrometheusPairedRestoreStageForStartup(context.Background(), initial.ID, lock)
							if err != nil || got.Phase != "restore_staged" {
								t.Fatal("concurrent takeover failed", err)
							}
						}
					}
					f.s.runnerFactory = func(*models.Component) (hostCommandRunner, error) { return wrapper, nil }
					if _, err := op(context.Background(), initial.ID); err == nil {
						t.Fatal("changed observation accepted", kind)
					}
					if !fired {
						t.Fatal("read boundary not reached", kind)
					}
					f.s.runnerFactory = func(*models.Component) (hostCommandRunner, error) { return r, nil }
					if kind == "timestamp" {
						if err := db.Model(&models.PrometheusRestoreTransaction{}).Where("id = ?", initial.ID).UpdateColumn("updated_at", initial.UpdatedAt).Error; err != nil {
							t.Fatal(err)
						}
					}
					if kind == "late-foreign" {
						if err := db.Where("host_id = ? AND token = ?", initial.HostID, foreign).Delete(&models.HostOperationClaim{}).Error; err != nil {
							t.Fatal(err)
						}
					}
					journalAssertClaimsEmpty(t, db)
				})
			}
			after := checkpointJournal(t, db, initial.ID)
			next, err := decodeRestoreStage(&after)
			if err != nil || len(next.Generations) != 2 || next.Fence.Generation == proof.Fence.Generation || string(after.Payload) != string(initial.Payload) || string(after.Checkpoint) != string(initial.Checkpoint) || f.runner.running {
				t.Fatal("takeover did not retain original evidence and stopped baseline", err)
			}

		})
	}
}
