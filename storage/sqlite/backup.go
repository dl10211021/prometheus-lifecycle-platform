package sqlite

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gorm.io/gorm"
)

// Snapshot limits apply equally to creation and restore. Zero means no size cap;
// available disk space (plus the reserve) is always enforced.
func snapshotBudget(size int64, dir string, copies int64) error {
	limit, err := envBytes("PROMLIFECYCLE_SNAPSHOT_MAX_BYTES", 0)
	if err != nil {
		return err
	}
	if limit > 0 && size > limit {
		return fmt.Errorf("snapshot size %d exceeds configured limit %d", size, limit)
	}
	reserve, err := envBytes("PROMLIFECYCLE_SNAPSHOT_RESERVE_BYTES", 64<<20)
	if err != nil {
		return err
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return err
	}
	available := uint64(st.Bavail) * uint64(st.Bsize)
	if size < 0 || uint64(size) > (^uint64(0)-uint64(reserve))/uint64(copies) || uint64(size)*uint64(copies)+uint64(reserve) > available {
		return fmt.Errorf("insufficient free disk space for snapshot and %d-byte reserve", reserve)
	}
	return nil
}

func envBytes(name string, fallback int64) (int64, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a nonnegative byte count", name)
	}
	return n, nil
}

// ValidateSnapshot never creates a file, changes journal mode, or writes sidecars.
// Core table columns distinguish platform snapshots from arbitrary SQLite files;
// older platform versions may omit newer tables which migrations will add.
func ValidateSnapshot(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("snapshot must be a regular file")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	uri := (&url.URL{Scheme: "file", Path: absolute}).String() + "?mode=ro&immutable=1"
	db, err := sql.Open("sqlite3", uri)
	if err != nil {
		return err
	}
	defer db.Close()
	var integrity string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
		return fmt.Errorf("snapshot integrity: %w", err)
	}
	if integrity != "ok" {
		return fmt.Errorf("snapshot integrity: %s", integrity)
	}
	for _, query := range []string{
		"SELECT id, username, password, role FROM users LIMIT 0",
		"SELECT id, name FROM hosts LIMIT 0",
		"SELECT id, host_id, type FROM components LIMIT 0",
	} {
		rows, err := db.Query(query)
		if err != nil {
			return fmt.Errorf("not a platform snapshot: %w", err)
		}
		rows.Close()
	}
	return nil
}

func DatabasePath(db *gorm.DB) (string, error) {
	var databases []struct {
		Seq  int
		Name string
		File string
	}
	if err := db.Raw("PRAGMA database_list").Scan(&databases).Error; err != nil {
		return "", err
	}
	for _, database := range databases {
		if database.Name == "main" && database.File != "" {
			return database.File, nil
		}
	}
	return "", fmt.Errorf("platform database must be file-backed")
}

func CreateSnapshot(db *gorm.DB, destination string) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	var pages, pageSize int64
	if err := db.Raw("PRAGMA page_count").Scan(&pages).Error; err != nil {
		return err
	}
	if err := db.Raw("PRAGMA page_size").Scan(&pageSize).Error; err != nil {
		return err
	}
	if err := snapshotBudget(pages*pageSize, filepath.Dir(destination), 1); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(destination), ".snapshot-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	tmp.Close()
	defer os.Remove(name)
	if err := db.Exec("VACUUM INTO '" + strings.ReplaceAll(name, "'", "''") + "'").Error; err != nil {
		return err
	}
	if err := ValidateSnapshot(name); err != nil {
		return err
	}
	if err := os.Chmod(name, 0600); err != nil {
		return err
	}
	if err := syncFile(name); err != nil {
		return err
	}
	if err := os.Rename(name, destination); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(destination))
}

type RestoreState struct {
	Status    string    `json:"status"`
	Name      string    `json:"name,omitempty"`
	LastError string    `json:"last_error,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

func RestoreStatus(dbPath string) (RestoreState, error) {
	var state RestoreState
	data, err := os.ReadFile(dbPath + ".restore.json")
	if os.IsNotExist(err) {
		return RestoreState{Status: "none"}, nil
	}
	if err != nil {
		return state, err
	}
	err = json.Unmarshal(data, &state)
	return state, err
}

func saveRestoreState(dbPath string, state RestoreState) error {
	state.UpdatedAt = time.Now().UTC()
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dbPath), ".restore-state-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), dbPath+".restore.json"); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(dbPath))
}

func RestoreEligibility(dbPath, source string) error {
	if err := ValidateSnapshot(source); err != nil {
		return err
	}
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	original, err := os.Stat(dbPath)
	if err != nil {
		return err
	}
	if err := snapshotBudget(info.Size(), filepath.Dir(dbPath), 2); err != nil {
		return err
	}
	// The original snapshot may exceed the configured candidate limit after growth.
	var st syscall.Statfs_t
	if err := syscall.Statfs(filepath.Dir(dbPath), &st); err != nil {
		return err
	}
	reserve, err := envBytes("PROMLIFECYCLE_SNAPSHOT_RESERVE_BYTES", 64<<20)
	if err != nil {
		return err
	}
	needed := uint64(info.Size())*2 + uint64(original.Size()) + uint64(reserve)
	if needed > st.Bavail*uint64(st.Bsize) {
		return fmt.Errorf("insufficient space for candidate and reversible original")
	}
	return nil
}

func StageRestore(dbPath, source string) (RestoreState, error) {
	state, err := RestoreStatus(dbPath)
	if err != nil {
		return state, err
	}
	if state.Status == "pending" || state.Status == "applying" {
		return state, fmt.Errorf("a platform restore is already %s", state.Status)
	}
	if err := RestoreEligibility(dbPath, source); err != nil {
		return state, err
	}
	if err := atomicCopy(source, dbPath+".restore"); err != nil {
		return state, err
	}
	if err := ValidateSnapshot(dbPath + ".restore"); err != nil {
		return state, err
	}
	state = RestoreState{Status: "pending", Name: filepath.Base(source), UpdatedAt: time.Now().UTC()}
	return state, saveRestoreState(dbPath, state)
}

// MigrateLegacyRestore imports the old base-directory pending file once. A bad
// legacy candidate becomes a visible failure without preventing the original DB
// from starting; it is never mistaken for an applied restore.
func MigrateLegacyRestore(dbPath, legacyPath string) error {
	state, err := RestoreStatus(dbPath)
	if err != nil {
		return err
	}
	if state.Status != "none" {
		return nil
	}
	if _, err := os.Stat(legacyPath); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if _, err := StageRestore(dbPath, legacyPath); err != nil {
		return saveRestoreState(dbPath, RestoreState{Status: "failed", Name: filepath.Base(legacyPath), LastError: err.Error()})
	}
	legacyInfo, err := os.Stat(legacyPath)
	if err != nil {
		return err
	}
	stagedInfo, err := os.Stat(dbPath + ".restore")
	if err != nil {
		return err
	}
	if os.SameFile(legacyInfo, stagedInfo) {
		return nil
	}
	if err := os.Remove(legacyPath); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(legacyPath))
}

// ApplyPendingRestore holds a reversible original on the database filesystem.
// The caller must close the candidate DB before Rollback, and Commit only after
// all startup migrations and initialization have succeeded.
type RestoreTransaction struct {
	path   string
	state  RestoreState
	active bool
}

func ApplyPendingRestore(dbPath string) (*RestoreTransaction, error) {
	state, err := RestoreStatus(dbPath)
	if err != nil {
		return nil, err
	}
	tx := &RestoreTransaction{path: dbPath, state: state}
	if state.Status == "applying" {
		tx.active = true
		if err := tx.Rollback(fmt.Errorf("restore interrupted before initialization completed")); err != nil {
			return nil, err
		}
		return tx, nil
	}
	if state.Status != "pending" {
		return tx, nil
	}
	fail := func(cause error) (*RestoreTransaction, error) {
		tx.state.Status = "failed"
		tx.state.LastError = cause.Error()
		if err := saveRestoreState(dbPath, tx.state); err != nil {
			return nil, err
		}
		return tx, nil // Original is untouched; the API must remain available to retry.
	}
	if err := RestoreEligibility(dbPath, dbPath+".restore"); err != nil {
		return fail(err)
	}
	if err := Checkpoint(dbPath); err != nil {
		return fail(err)
	}
	if err := atomicCopy(dbPath, dbPath+".restore-original"); err != nil {
		return fail(err)
	}
	tx.state.Status = "applying"
	if err := saveRestoreState(dbPath, tx.state); err != nil {
		return nil, err
	}
	tx.active = true
	if err := removeSidecars(dbPath); err != nil {
		if rollbackErr := tx.Rollback(err); rollbackErr != nil {
			return nil, rollbackErr
		}
		return tx, nil
	}
	if err := os.Rename(dbPath+".restore", dbPath); err != nil {
		if rollbackErr := tx.Rollback(err); rollbackErr != nil {
			return nil, rollbackErr
		}
		return tx, nil
	}
	if err := syncDirectory(filepath.Dir(dbPath)); err != nil {
		if rollbackErr := tx.Rollback(err); rollbackErr != nil {
			return nil, rollbackErr
		}
		return tx, nil
	}
	return tx, nil
}

func (tx *RestoreTransaction) Rollback(cause error) error {
	if !tx.active {
		return nil
	}
	if err := removeSidecars(tx.path); err != nil {
		return err
	}
	if err := atomicCopy(tx.path+".restore-original", tx.path); err != nil {
		return fmt.Errorf("restore rollback failed: %w (startup error: %v)", err, cause)
	}
	tx.state.Status = "failed"
	tx.state.LastError = cause.Error()
	if err := saveRestoreState(tx.path, tx.state); err != nil {
		return err
	}
	tx.active = false
	return nil
}

func (tx *RestoreTransaction) Commit() error {
	if !tx.active {
		return nil
	}
	tx.state.Status = "applied"
	tx.state.LastError = ""
	if err := saveRestoreState(tx.path, tx.state); err != nil {
		return err
	}
	tx.active = false
	return nil
}

func Checkpoint(path string) error {
	db, err := sql.Open("sqlite3", (&url.URL{Scheme: "file", Path: path}).String()+"?mode=rw&_busy_timeout=5000")
	if err != nil {
		return err
	}
	defer db.Close()
	var busy, logged, checkpointed int
	if err := db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logged, &checkpointed); err != nil {
		return err
	}
	if busy != 0 {
		return fmt.Errorf("database checkpoint is busy")
	}
	return nil
}

func removeSidecars(path string) error {
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func atomicCopy(source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.CreateTemp(filepath.Dir(destination), ".restore-copy-*")
	if err != nil {
		return err
	}
	defer os.Remove(out.Name())
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Rename(out.Name(), destination); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(destination))
}

func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
