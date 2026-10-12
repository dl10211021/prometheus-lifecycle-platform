package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/promlifecycle/platform/models"
	platformsqlite "github.com/promlifecycle/platform/storage/sqlite"
)

func TestStartupPrometheusRestoreJournalMigration(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "fresh"
		if legacy {
			name = "previous-schema"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			data := filepath.Join(root, "data")
			dbPath := filepath.Join(data, "promlifecycle.db")
			var retainedID uint
			if legacy {
				if err := os.MkdirAll(data, 0700); err != nil {
					t.Fatal(err)
				}
				db, err := platformsqlite.OpenDB(dbPath)
				if err != nil {
					t.Fatal(err)
				}
				if err := db.AutoMigrate(&models.Component{}); err != nil {
					t.Fatal(err)
				}
				host := models.Host{Name: "legacy-journal-host", IP: "127.0.0.1", ConnectionType: "local"}
				cluster := models.Cluster{Name: "legacy-journal-cluster"}
				if err := db.Create(&host).Error; err != nil {
					t.Fatal(err)
				}
				if err := db.Create(&cluster).Error; err != nil {
					t.Fatal(err)
				}
				retained := models.Component{HostID: host.ID, ClusterID: cluster.ID, Name: "retained-journal-migration", Type: models.ComponentPrometheus, DeployType: "binary", Version: "2.55.0", Status: "removed"}
				if err := db.Create(&retained).Error; err != nil {
					t.Fatal(err)
				}
				retainedID = retained.ID
				if db.Migrator().HasTable("prometheus_restore_transactions") {
					t.Fatal("fixture is not a previous schema")
				}
				connection, _ := db.DB()
				if err := connection.Close(); err != nil {
					t.Fatal(err)
				}
			}
			_, stop := startOwnedServer(t, root, data, startupTestPort(t))
			stop(false)
			db, err := platformsqlite.OpenDB(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			connection, _ := db.DB()
			defer connection.Close()
			if !db.Migrator().HasTable("prometheus_restore_transactions") {
				t.Fatal("server startup did not migrate durable paired restore journal")
			}
			if legacy {
				var retained models.Component
				if err := db.First(&retained, retainedID).Error; err != nil {
					t.Fatal(err)
				}
				if retained.Name != "retained-journal-migration" || retained.Version != "2.55.0" {
					t.Fatal("journal migration changed retained component identity")
				}
			}
			var integrity string
			if err := db.Raw("PRAGMA integrity_check").Scan(&integrity).Error; err != nil || integrity != "ok" {
				t.Fatalf("migration database integrity: %q %v", integrity, err)
			}
		})
	}
}
