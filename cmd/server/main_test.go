package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/promlifecycle/platform/models"
	platformsqlite "github.com/promlifecycle/platform/storage/sqlite"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestValidateServerTransport(t *testing.T) {
	t.Parallel()

	if err := validateServerTransport("127.0.0.1", "", "", "", 0, false); err != nil {
		t.Fatalf("loopback HTTP rejected: %v", err)
	}
	if err := validateServerTransport("0.0.0.0", "", "", "", 0, false); err == nil {
		t.Fatal("public plaintext HTTP accepted without explicit opt-in")
	}
	if err := validateServerTransport("0.0.0.0", "", "", "", 0, true); err != nil {
		t.Fatalf("explicit insecure HTTP opt-in rejected: %v", err)
	}
	if err := validateServerTransport("0.0.0.0", "cert.pem", "", "", 0, false); err == nil {
		t.Fatal("unpaired TLS certificate accepted")
	}
	if err := validateServerTransport("0.0.0.0", "cert.pem", "key.pem", "monitor.example.com", 80, false); err != nil {
		t.Fatalf("valid TLS redirect configuration rejected: %v", err)
	}
}

func TestHTTPSRedirectUsesConfiguredHost(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest("GET", "http://attacker.invalid/packages?q=1", nil)
	location := httpsRedirectURL("monitor.example.com", 8443, req)
	if location != "https://monitor.example.com:8443/packages?q=1" {
		t.Fatalf("unexpected redirect location: %s", location)
	}
	if strings.Contains(location, "attacker.invalid") {
		t.Fatalf("request Host leaked into redirect: %s", location)
	}
}

func TestMigrateLegacyUserRoles(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:role-migration?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	if err := db.AutoMigrate(&models.User{}); err != nil {
		t.Fatalf("migrate users: %v", err)
	}
	legacyUsers := []struct {
		username string
		role     string
	}{
		{username: "legacy-admin", role: "admin"},
		{username: "legacy-user", role: "user"},
		{username: "viewer", role: string(models.RoleViewer)},
	}
	for i := range legacyUsers {
		if err := db.Exec("INSERT INTO users(username,password,role,is_active) VALUES(?,?,?,?)",
			legacyUsers[i].username, "x", legacyUsers[i].role, true).Error; err != nil {
			t.Fatalf("create legacy user %s: %v", legacyUsers[i].username, err)
		}
	}
	if err := migrateLegacyUserRoles(db); err != nil {
		t.Fatalf("migrate roles: %v", err)
	}
	if err := migrateLegacyUserRoles(db); err != nil {
		t.Fatalf("repeat migration: %v", err)
	}
	want := map[string]string{
		"legacy-admin": string(models.RoleSystemAdmin),
		"legacy-user":  string(models.RoleChangeAdmin),
		"viewer":       string(models.RoleViewer),
	}
	var migrated []models.User
	if err := db.Order("id").Find(&migrated).Error; err != nil {
		t.Fatalf("load users: %v", err)
	}
	for i := range migrated {
		if got := migrated[i].Role; got != want[migrated[i].Username] {
			t.Fatalf("%s role=%q, want %q", migrated[i].Username, got, want[migrated[i].Username])
		}
	}
}

func TestMigrateLegacyHostStatusPreservesEvidence(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "host-migration.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	if err := db.AutoMigrate(&models.Host{}); err != nil {
		t.Fatal(err)
	}
	checked := time.Now().UTC().Truncate(time.Second)
	hosts := []models.Host{
		{Name: "legacy", IP: "192.0.2.1", Status: "pending"},
		{Name: "offline", IP: "192.0.2.2", Status: "offline", LastError: "refused", LastCheckedAt: &checked},
		{Name: "online", IP: "192.0.2.3", Status: "online", LastOnline: &checked, LastCheckedAt: &checked},
	}
	if err := db.Create(&hosts).Error; err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrateLegacyHostStatus(db); err != nil {
			t.Fatal(err)
		}
	}
	var migrated []models.Host
	if err := db.Order("ip").Find(&migrated).Error; err != nil {
		t.Fatal(err)
	}
	if len(migrated) != 3 {
		t.Fatalf("migration lost hosts: %+v", migrated)
	}
	if migrated[0].Status != "unknown" || migrated[0].LastCheckedAt != nil || migrated[0].LastOnline != nil {
		t.Fatalf("migration invented probe evidence: %+v", migrated[0])
	}
	if migrated[1].Status != "offline" || migrated[1].LastError != "refused" || migrated[1].LastCheckedAt == nil || !migrated[1].LastCheckedAt.Equal(checked) {
		t.Fatalf("migration lost failed check: %+v", migrated[1])
	}
	if migrated[2].Status != "online" || migrated[2].LastOnline == nil || !migrated[2].LastOnline.Equal(checked) {
		t.Fatalf("migration lost successful check: %+v", migrated[2])
	}
}

func TestCheckpointSQLiteForArchiveIncludesWALWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "live.db")
	db, err := gorm.Open(sqlite.Open(path+"?_journal_mode=WAL"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TABLE markers(value TEXT)").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO markers(value) VALUES(?)", "wal-marker").Error; err != nil {
		t.Fatal(err)
	}
	if err := platformsqlite.Checkpoint(path); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "archive.db")
	if err := os.WriteFile(archive, content, 0600); err != nil {
		t.Fatal(err)
	}
	archived, err := gorm.Open(sqlite.Open(archive), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	var value string
	if err := archived.Raw("SELECT value FROM markers").Scan(&value).Error; err != nil {
		t.Fatal(err)
	}
	if value != "wal-marker" {
		t.Fatalf("archived marker=%q", value)
	}
}
