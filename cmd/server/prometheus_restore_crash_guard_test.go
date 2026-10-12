package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/promlifecycle/platform/models"
	platformsqlite "github.com/promlifecycle/platform/storage/sqlite"
)

func journalExpectStartupRejected(t *testing.T, root, data string) {
	t.Helper()
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
		t.Fatal("journal startup failed to reject")
	}
	output, _ := os.ReadFile(log)
	if command.ProcessState.Success() || !strings.Contains(string(output), "paired restore") {
		t.Fatalf("missing startup journal failure: %s", output)
	}
}

func TestStartupPrometheusRestoreJournalReclaimsOnlyAbandonedOwnerClaims(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "prepared", true: "corrupt"}[corrupt], func(t *testing.T) {
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
			defer conn.Close()
			other := models.Host{Name: "unrelated", IP: "192.0.2.25", ConnectionType: "ssh", Status: "pending"}
			cluster := models.Cluster{Name: "journal-claims"}
			if err := db.Create(&other).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Create(&cluster).Error; err != nil {
				t.Fatal(err)
			}
			for i, hid := range []uint{1, 1, other.ID} {
				c := models.Component{HostID: hid, ClusterID: cluster.ID, Name: []string{"owner", "sibling", "other"}[i], Type: models.ComponentPrometheus, Status: "removed", DeployType: "binary"}
				if err := db.Create(&c).Error; err != nil || c.ID != uint(i+1) {
					t.Fatal("fixture component identity", err)
				}
			}
			row := startupJournalRecord(t)
			if corrupt {
				row.Phase = "applying"
			}
			if err := db.Create(&row).Error; err != nil {
				t.Fatal(err)
			}
			for _, id := range []uint{1, other.ID} {
				if err := db.Create(&models.HostOperationClaim{HostID: id, Token: "abandoned-host", Kind: "restore"}).Error; err != nil {
					t.Fatal(err)
				}
			}
			for _, id := range []uint{1, 2, 3} {
				if err := db.Create(&models.ComponentOperationClaim{ComponentID: id, Token: "abandoned-component", Kind: "restore"}).Error; err != nil {
					t.Fatal(err)
				}
			}
			journalExpectStartupRejected(t, root, data)
			var hosts, components int64
			db.Model(&models.HostOperationClaim{}).Count(&hosts)
			db.Model(&models.ComponentOperationClaim{}).Count(&components)
			if corrupt {
				if hosts != 2 || components != 3 {
					t.Fatal("corrupt journal allowed claim cleanup")
				}
			} else {
				if hosts != 1 || components != 1 {
					t.Fatal("validated journal left abandoned claims", hosts, components)
				}
				var h models.HostOperationClaim
				var c models.ComponentOperationClaim
				if err := db.First(&h, "host_id = ?", other.ID).Error; err != nil {
					t.Fatal("unrelated host claim lost", err)
				}
				if err := db.First(&c, "component_id = ?", 3).Error; err != nil {
					t.Fatal("unrelated component claim lost", err)
				}
			}
			var retained models.PrometheusRestoreTransaction
			if err := db.First(&retained, row.ID).Error; err != nil || retained.Phase != row.Phase || retained.Token != row.Token || retained.Revision != 1 {
				t.Fatal("startup cleanup changed journal", err)
			}
			var unchanged models.Host
			if err := db.First(&unchanged, other.ID).Error; err != nil || unchanged.Status != "pending" {
				t.Fatal("startup cleanup ran metadata migrations", err)
			}
		})
	}
}

func TestStartupPrometheusRestoreJournalInterruptedDatabaseRestore(t *testing.T) {
	for _, mode := range []string{"candidate-prepared", "candidate-corrupt-journal", "candidate-corrupt-database", "original-prepared", "missing-original", "corrupt-original", "corrupt-original-journal"} {
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
			conn, _ := db.DB()
			candidate := filepath.Join(root, "candidate.db")
			row := startupJournalRecord(t)
			if strings.HasPrefix(mode, "candidate-") {
				if mode == "candidate-corrupt-journal" {
					row.Phase = "unknown"
				}
				if err := db.Create(&row).Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := platformsqlite.CreateSnapshot(db, candidate); err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(mode, "candidate-") {
				if err := db.Delete(&row).Error; err != nil {
					t.Fatal(err)
				}
			} else if mode == "original-prepared" || mode == "corrupt-original-journal" {
				if mode == "corrupt-original-journal" {
					row.Phase = "unknown"
				}
				if err := db.Create(&row).Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := platformsqlite.StageRestore(path, candidate); err != nil {
				t.Fatal(err)
			}
			// Leave the durable applying state exactly as a crash after database rename.
			if _, err := platformsqlite.ApplyPendingRestore(path); err != nil {
				t.Fatal(err)
			}
			if mode == "candidate-corrupt-database" {
				if err := os.WriteFile(path, []byte("corrupt interrupted candidate"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			original := path + ".restore-original"
			if mode == "missing-original" {
				if err := os.Remove(original); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "corrupt-original" {
				if err := os.WriteFile(original, []byte("not sqlite"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			savedOriginal, _ := os.ReadFile(original)
			savedCandidate, _ := os.ReadFile(path)
			savedState, _ := os.ReadFile(path + ".restore.json")
			if strings.HasPrefix(mode, "candidate-") {
				_, stop = startOwnedServer(t, root, data, startupTestPort(t))
				stop(false)
			} else {
				journalExpectStartupRejected(t, root, data)
			}
			gotOriginal, e := os.ReadFile(original)
			if mode == "missing-original" {
				if !os.IsNotExist(e) {
					t.Fatal("missing original recreated", e)
				}
			} else if e != nil || string(gotOriginal) != string(savedOriginal) {
				t.Fatal("authoritative original changed", e)
			}
			state, err := platformsqlite.RestoreStatus(path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(mode, "candidate-") || mode == "original-prepared" {
				if state.Status != "failed" {
					t.Fatal("interrupted restore did not roll back", state.Status)
				}
				reopened, err := platformsqlite.OpenDB(path)
				if err != nil {
					t.Fatal(err)
				}
				connection, _ := reopened.DB()
				defer connection.Close()
				var count int64
				if err := reopened.Model(&models.PrometheusRestoreTransaction{}).Where("phase = ?", "prepared").Count(&count).Error; err != nil {
					t.Fatal(err)
				}
				want := int64(0)
				if mode == "original-prepared" {
					want = 1
				}
				if count != want {
					t.Fatal("rollback lost authoritative journal identity", count, want)
				}
			} else {
				got, e := os.ReadFile(path)
				if e != nil || string(got) != string(savedCandidate) {
					t.Fatal("unsafe original replaced candidate", e)
				}
				got, e = os.ReadFile(path + ".restore.json")
				if e != nil || string(got) != string(savedState) {
					t.Fatal("unsafe original changed restore status", e)
				}
			}
		})
	}
}
