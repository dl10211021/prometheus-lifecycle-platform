package sqlite

import (
	"log"
	"fmt"
	"os"
	"path/filepath"

	_ "github.com/mattn/go-sqlite3"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func OpenDB(path string) (*gorm.DB, error) {
	if path == "" {
		return nil, fmt.Errorf("database path is required")
	}

	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}

	db, err := gorm.Open(sqlite.Open(path+"?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on"), &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("init gorm: %w", err)
	}

	// The database holds host credentials (encrypted since the credentials
	// work, but the file must not be world/group readable either way).
	if err := os.Chmod(path, 0600); err != nil {
		log.Printf("[SQLite] WARNING: could not chmod %s to 0600: %v", path, err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Chmod(path+suffix, 0600); err != nil {
			// WAL/SHM files may not exist yet; ignore.
			continue
		}
	}

	sqlDb, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("get sql db: %w", err)
	}

	sqlDb.SetMaxIdleConns(10)
	sqlDb.SetMaxOpenConns(100)

	return db, nil
}

// NewStateStore creates a new state store instance
func NewStateStore(db *gorm.DB) *gorm.DB {
	return db
}
