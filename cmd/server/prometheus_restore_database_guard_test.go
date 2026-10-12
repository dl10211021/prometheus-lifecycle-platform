package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/promlifecycle/platform/models"
	platformsqlite "github.com/promlifecycle/platform/storage/sqlite"
)

func startupJournalRecord(t *testing.T) models.PrometheusRestoreTransaction {
	t.Helper()
	digest := strings.Repeat("1", 64)
	payload, err := json.Marshal(map[string]any{
		"Schema": 1, "PlanID": 7, "PointID": 8, "BackupID": 9,
		"Current":     map[string]any{"ComponentID": 1, "HostID": 1, "Unit": "journal-test.service", "RuntimeSHA256": digest, "HostSHA256": digest},
		"PointSHA256": digest, "BeforeSHA256": digest, "AfterSHA256": digest,
		"PlanStatus": "failed", "CurrentVersion": "2.55.0", "HistoricalVersion": "2.55.0", "ObservedAt": time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(payload)
	cid, hid := uint(1), uint(1)
	return models.PrometheusRestoreTransaction{PlanID: 7, PointID: 8, BackupID: 9, ComponentID: cid, HostID: hid, ActiveComponentID: &cid, ActiveHostID: &hid,
		Token: strings.Repeat("a", 64), Phase: "prepared", Revision: 1, Payload: payload, PayloadSHA256: hex.EncodeToString(hash[:])}
}

func TestStartupPrometheusRestoreJournalBeforeDatabaseReplacement(t *testing.T) {
	for _, mode := range []string{"metadata", "staged-WAL", "legacy-staged"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			data := filepath.Join(root, "data")
			path := filepath.Join(data, "promlifecycle.db")
			_, stop := startOwnedServer(t, root, data, startupTestPort(t))
			stop(false)
			db, err := platformsqlite.OpenDB(path)
			if err != nil {
				t.Fatal(err)
			}
			connection, _ := db.DB()
			defer connection.Close()
			snapshot := filepath.Join(root, "older.db")
			if err := platformsqlite.CreateSnapshot(db, snapshot); err != nil {
				t.Fatal(err)
			}
			if err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)").Error; err != nil {
				t.Fatal(err)
			}
			// Keep this connection open: the newly committed intent exists in WAL.
			if err := db.Exec("PRAGMA wal_autocheckpoint=0").Error; err != nil {
				t.Fatal(err)
			}
			row := startupJournalRecord(t)
			if err := db.Create(&row).Error; err != nil {
				t.Fatal(err)
			}
			marker := models.Host{Name: "journal-startup-marker", IP: "192.0.2.13", ConnectionType: "ssh", Status: "pending"}
			if err := db.Create(&marker).Error; err != nil {
				t.Fatal(err)
			}
			var staged, state []byte
			legacy := filepath.Join(root, "base", "backups", "promlifecycle.db.restore")
			if mode == "staged-WAL" {
				if _, err := platformsqlite.StageRestore(path, snapshot); err != nil {
					t.Fatal(err)
				}
				staged, err = os.ReadFile(path + ".restore")
				if err != nil {
					t.Fatal(err)
				}
				state, err = os.ReadFile(path + ".restore.json")
				if err != nil {
					t.Fatal(err)
				}
			} else if mode == "legacy-staged" {
				if err := os.MkdirAll(filepath.Dir(legacy), 0700); err != nil {
					t.Fatal(err)
				}
				staged, err = os.ReadFile(snapshot)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(legacy, staged, 0600); err != nil {
					t.Fatal(err)
				}
			}
			command, log := startupTestCommand(t, root, data, startupTestPort(t))
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- command.Wait() }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				command.Process.Kill()
				<-done
				t.Fatal("startup served or stalled instead of rejecting pending journal")
			}
			output, _ := os.ReadFile(log)
			if command.ProcessState.Success() || !strings.Contains(string(output), "paired restore transaction") {
				t.Fatalf("startup did not reject paired journal: %s", output)
			}
			var retained models.PrometheusRestoreTransaction
			if err := db.First(&retained, row.ID).Error; err != nil || retained.Token != row.Token || retained.Phase != "prepared" {
				t.Fatal("startup lost or mutated durable journal", err)
			}
			var host models.Host
			if err := db.First(&host, marker.ID).Error; err != nil || host.Status != "pending" {
				t.Fatal("startup migrated bound metadata before fencing", err)
			}
			if mode == "staged-WAL" {
				got, e := os.ReadFile(path + ".restore")
				if e != nil || string(got) != string(staged) {
					t.Fatal("startup replaced or changed staged snapshot", e)
				}
				got, e = os.ReadFile(path + ".restore.json")
				if e != nil || string(got) != string(state) {
					t.Fatal("startup changed staged restore status", e)
				}
			} else if mode == "legacy-staged" {
				got, e := os.ReadFile(legacy)
				if e != nil || string(got) != string(staged) {
					t.Fatal("startup imported legacy snapshot before fencing", e)
				}
				if _, e := os.Stat(path + ".restore.json"); !os.IsNotExist(e) {
					t.Fatal("startup created restore state before fencing", e)
				}
			}
		})
	}
}

func TestStartupPrometheusRestoreJournalAbortedHistoryPermitsStartup(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "data")
	_, stop := startOwnedServer(t, root, data, startupTestPort(t))
	stop(false)
	db, err := platformsqlite.OpenDB(filepath.Join(data, "promlifecycle.db"))
	if err != nil {
		t.Fatal(err)
	}
	row := startupJournalRecord(t)
	now := time.Now().UTC()
	row.Phase = "aborted"
	row.Revision = 2
	row.ActiveComponentID = nil
	row.ActiveHostID = nil
	row.FinishedAt = &now
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	connection, _ := db.DB()
	connection.Close()
	_, stop = startOwnedServer(t, root, data, startupTestPort(t))
	stop(false)
}
