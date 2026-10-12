package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/promlifecycle/platform/models"
	platformsqlite "github.com/promlifecycle/platform/storage/sqlite"
	"gorm.io/gorm"
)

// Structural inventory fixtures intentionally name absent host artifacts:
// startup must validate the journal without attempting SSH or filesystem repair.
func startupCheckpointRecord(t *testing.T, phase string) models.PrometheusRestoreTransaction {
	t.Helper()
	row := startupJournalRecord(t)
	row.ID = 1
	var admission map[string]any
	if err := json.Unmarshal(row.Payload, &admission); err != nil {
		t.Fatal(err)
	}
	root := fmt.Sprintf("/fixture/backups/restore-%d-%s", row.ID, row.Token[:32])
	now := time.Now().UTC()
	hash := strings.Repeat("1", 64)
	proof := map[string]any{"Schema": 1, "TransactionID": row.ID, "AdmissionSHA256": row.PayloadSHA256, "Root": root, "StartedAt": now, "StoppedAt": time.Time{}, "Evidence": nil}
	row.Phase = phase
	row.Revision = 2
	if phase != "checkpointing" {
		point := root + "/point-1-" + strings.Repeat("b", 32)
		evidence := map[string]any{
			"Schema": 1, "BeforeSHA256": hash, "AfterSHA256": hash, "Binding": admission["Current"],
			"Startup": map[string]any{"component_id": 1, "host_id": 1, "path": root + "/systemd-1-" + strings.Repeat("c", 32), "sha256": hash},
			"Binary":  map[string]any{"Directory": point, "File": map[string]any{"Path": "/fixture/prometheus", "Role": "binary", "Present": true, "Optional": false, "Mode": 0755, "UID": 0, "GID": 0, "Size": 1024, "SHA256": hash, "Stamp": "fixture-stamp", "Payload": ""}},
			"Runtime": map[string]any{"PID": "1234", "StartTime": "100", "ArgvSHA256": hash, "ConfigIdentity": "1:2", "DataIdentity": "1:3"},
			"Archive": map[string]any{},
		}
		proof["Evidence"] = evidence
		row.Revision = 3
		if phase == "checkpoint_ready" {
			row.Revision = 4
			proof["StoppedAt"] = now.Add(time.Second)
			evidence["Archive"] = map[string]any{"Path": point + "/payload.tar.gz", "SHA256": hash, "Size": 512, "Version": "2.55.0", "Sources": []string{"/fixture/prometheus.yml", "/fixture/data"}}
		}
	}
	var err error
	row.Checkpoint, err = json.Marshal(proof)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(row.Checkpoint)
	row.CheckpointSHA256 = hex.EncodeToString(sum[:])
	return row
}
func checkpointStartupOpen(t *testing.T) (string, string, string, *gorm.DB) {
	t.Helper()
	root := t.TempDir()
	data := filepath.Join(root, "data")
	path := filepath.Join(data, "promlifecycle.db")
	_, stop := startOwnedServer(t, root, data, startupTestPort(t))
	stop(false)
	db, err := platformsqlite.OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	conn, _ := db.DB()
	t.Cleanup(func() { conn.Close() })
	return root, data, path, db
}
func TestStartupPrometheusRestoreCheckpointOldColumns(t *testing.T) {
	for _, phase := range []string{"prepared", "aborted"} {
		t.Run(phase, func(t *testing.T) {
			root, data, _, db := checkpointStartupOpen(t)
			row := startupJournalRecord(t)
			if phase == "aborted" {
				now := time.Now().UTC()
				row.Phase = phase
				row.Revision = 2
				row.ActiveHostID = nil
				row.ActiveComponentID = nil
				row.FinishedAt = &now
			}
			if err := db.Create(&row).Error; err != nil {
				t.Fatal(err)
			}
			for _, column := range []string{"checkpoint", "checkpoint_sha256"} {
				if err := db.Exec("ALTER TABLE prometheus_restore_transactions DROP COLUMN " + column).Error; err != nil {
					t.Fatal(err)
				}
				if db.Migrator().HasColumn(&models.PrometheusRestoreTransaction{}, column) {
					t.Fatal("old-column fixture still has new evidence column")
				}
			}
			if phase == "prepared" {
				journalExpectStartupRejected(t, root, data)
			} else {
				_, stop := startOwnedServer(t, root, data, startupTestPort(t))
				stop(false)
				for _, column := range []string{"checkpoint", "checkpoint_sha256"} {
					if !db.Migrator().HasColumn(&models.PrometheusRestoreTransaction{}, column) {
						t.Fatal("actual server did not migrate nullable checkpoint evidence", column)
					}
				}
			}
			var got models.PrometheusRestoreTransaction
			if err := db.First(&got, row.ID).Error; err != nil || got.Phase != phase || got.Token != row.Token || string(got.Payload) != string(row.Payload) || len(got.Checkpoint) != 0 || got.CheckpointSHA256 != "" {
				t.Fatal("old journal migration changed admission/history", err)
			}
		})
	}
}
func TestStartupPrometheusRestoreCheckpointRetainsOwner(t *testing.T) {
	for _, phase := range []string{"checkpointing", "checkpoint_stopping", "checkpoint_ready"} {
		t.Run(phase, func(t *testing.T) {
			root, data, _, db := checkpointStartupOpen(t)
			cluster := models.Cluster{Name: "checkpoint-startup"}
			if err := db.Create(&cluster).Error; err != nil {
				t.Fatal(err)
			}
			c := models.Component{HostID: 1, ClusterID: cluster.ID, Name: "checkpoint-owner", Type: models.ComponentPrometheus, DeployType: "binary", Status: "removed"}
			if err := db.Create(&c).Error; err != nil || c.ID != 1 {
				t.Fatal("checkpoint component fixture", err)
			}
			row := startupCheckpointRecord(t, phase)
			if err := db.Create(&row).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Create(&models.HostOperationClaim{HostID: 1, Token: "checkpoint-host", Kind: "paired-restore-checkpoint"}).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Create(&models.ComponentOperationClaim{ComponentID: 1, Token: "checkpoint-component", Kind: "paired-restore-checkpoint"}).Error; err != nil {
				t.Fatal(err)
			}
			journalExpectStartupRejected(t, root, data)
			for _, model := range []any{&models.HostOperationClaim{}, &models.ComponentOperationClaim{}} {
				var count int64
				if err := db.Model(model).Count(&count).Error; err != nil || count != 1 {
					t.Fatal("server reclaimed possibly stopped checkpoint owner", err)
				}
			}
			var got models.PrometheusRestoreTransaction
			if err := db.First(&got, row.ID).Error; err != nil || got.Phase != phase || got.CheckpointSHA256 != row.CheckpointSHA256 || string(got.Checkpoint) != string(row.Checkpoint) {
				t.Fatal("startup altered checkpoint evidence", err)
			}
		})
	}
}
func TestStartupPrometheusRestoreCheckpointInterruptedDatabaseRollback(t *testing.T) {
	for _, phase := range []string{"checkpointing", "checkpoint_stopping", "checkpoint_ready"} {
		for _, corrupt := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/corrupt=%t", phase, corrupt), func(t *testing.T) {
				root, data, path, db := checkpointStartupOpen(t)
				candidate := filepath.Join(root, "before-checkpoint.db")
				if err := platformsqlite.CreateSnapshot(db, candidate); err != nil {
					t.Fatal(err)
				}
				row := startupCheckpointRecord(t, phase)
				if corrupt {
					row.CheckpointSHA256 = strings.Repeat("f", 64)
				}
				if err := db.Create(&row).Error; err != nil {
					t.Fatal(err)
				}
				conn, _ := db.DB()
				if err := conn.Close(); err != nil {
					t.Fatal(err)
				}
				if _, err := platformsqlite.StageRestore(path, candidate); err != nil {
					t.Fatal(err)
				}
				if _, err := platformsqlite.ApplyPendingRestore(path); err != nil {
					t.Fatal(err)
				}
				original, err := os.ReadFile(path + ".restore-original")
				if err != nil {
					t.Fatal(err)
				}
				beforeCandidate, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				beforeState, err := os.ReadFile(path + ".restore.json")
				if err != nil {
					t.Fatal(err)
				}
				journalExpectStartupRejected(t, root, data)
				afterOriginal, err := os.ReadFile(path + ".restore-original")
				if err != nil || string(afterOriginal) != string(original) {
					t.Fatal("startup changed authoritative checkpoint database", err)
				}
				state, err := platformsqlite.RestoreStatus(path)
				if err != nil {
					t.Fatal(err)
				}
				if corrupt {
					if state.Status != "applying" {
						t.Fatal("corrupt checkpoint original allowed rollback", state.Status)
					}
					after, e := os.ReadFile(path)
					if e != nil || string(after) != string(beforeCandidate) {
						t.Fatal("corrupt original replaced candidate", e)
					}
					after, e = os.ReadFile(path + ".restore.json")
					if e != nil || string(after) != string(beforeState) {
						t.Fatal("corrupt original changed restore state", e)
					}
				} else {
					if state.Status != "failed" {
						t.Fatal("valid interrupted checkpoint blocked required database rollback", state.Status)
					}
					reopened, err := platformsqlite.OpenDB(path)
					if err != nil {
						t.Fatal(err)
					}
					connection, _ := reopened.DB()
					defer connection.Close()
					var got models.PrometheusRestoreTransaction
					if err := reopened.First(&got, row.ID).Error; err != nil || got.Phase != row.Phase || got.CheckpointSHA256 != row.CheckpointSHA256 || string(got.Checkpoint) != string(row.Checkpoint) {
						t.Fatal("database rollback lost interrupted checkpoint", err)
					}
				}
			})
		}
	}
}

func TestStartupPrometheusRestoreCheckpointMixedInventoryIsAtomic(t *testing.T) {
	root, data, _, db := checkpointStartupOpen(t)
	other := models.Host{Name: "prepared-mixed-host", IP: "192.0.2.88", ConnectionType: "ssh"}
	cluster := models.Cluster{Name: "mixed-checkpoint"}
	if err := db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&cluster).Error; err != nil {
		t.Fatal(err)
	}
	for _, host := range []uint{1, other.ID} {
		c := models.Component{HostID: host, ClusterID: cluster.ID, Name: fmt.Sprintf("mixed-%d", host), Type: models.ComponentPrometheus, DeployType: "binary", Status: "removed"}
		if err := db.Create(&c).Error; err != nil {
			t.Fatal(err)
		}
	}
	prepared := startupJournalRecord(t)
	prepared.ID = 1
	prepared.HostID, prepared.ComponentID = other.ID, 2
	prepared.ActiveHostID, prepared.ActiveComponentID = &prepared.HostID, &prepared.ComponentID
	prepared.Token = strings.Repeat("d", 64)
	var admission map[string]any
	if err := json.Unmarshal(prepared.Payload, &admission); err != nil {
		t.Fatal(err)
	}
	current := admission["Current"].(map[string]any)
	current["HostID"] = prepared.HostID
	current["ComponentID"] = prepared.ComponentID
	prepared.Payload, _ = json.Marshal(admission)
	sum := sha256.Sum256(prepared.Payload)
	prepared.PayloadSHA256 = hex.EncodeToString(sum[:])
	if err := db.Create(&prepared).Error; err != nil {
		t.Fatal(err)
	}
	checkpoint := startupCheckpointRecord(t, "checkpoint_stopping")
	checkpoint.ID = 2
	var proof map[string]any
	if err := json.Unmarshal(checkpoint.Checkpoint, &proof); err != nil {
		t.Fatal(err)
	}
	proof["TransactionID"] = checkpoint.ID
	raw, _ := json.Marshal(proof)
	checkpoint.Checkpoint = []byte(strings.ReplaceAll(string(raw), "/restore-1-", "/restore-2-"))
	sum = sha256.Sum256(checkpoint.Checkpoint)
	checkpoint.CheckpointSHA256 = hex.EncodeToString(sum[:])
	if err := db.Create(&checkpoint).Error; err != nil {
		t.Fatal(err)
	}
	for _, row := range []models.PrometheusRestoreTransaction{prepared, checkpoint} {
		if err := db.Create(&models.HostOperationClaim{HostID: row.HostID, Token: row.Token, Kind: "restore"}).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&models.ComponentOperationClaim{ComponentID: row.ComponentID, Token: row.Token, Kind: "restore"}).Error; err != nil {
			t.Fatal(err)
		}
	}
	journalExpectStartupRejected(t, root, data)
	for _, model := range []any{&models.HostOperationClaim{}, &models.ComponentOperationClaim{}} {
		var count int64
		if err := db.Model(model).Count(&count).Error; err != nil || count != 2 {
			t.Fatal("mixed inventory partially reclaimed prepared owner before seeing checkpoint", count, err)
		}
	}
	for _, original := range []models.PrometheusRestoreTransaction{prepared, checkpoint} {
		var row models.PrometheusRestoreTransaction
		if err := db.First(&row, original.ID).Error; err != nil || row.Phase != original.Phase || row.Token != original.Token || string(row.Checkpoint) != string(original.Checkpoint) {
			t.Fatal("mixed startup altered durable journal", err)
		}
	}
}
