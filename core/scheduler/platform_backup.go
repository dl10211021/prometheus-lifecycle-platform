package scheduler

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/promlifecycle/platform/storage/sqlite"
	"gorm.io/gorm"
)

const (
	platformBackupKeep  = 7
	platformBackupEvery = time.Hour
)

// runPlatformDBBackup snapshots the platform's own SQLite database (host
// credentials, rules, audit history - everything) into {basePath}/backups so
// a reinstall does not lose the whole platform state. Runs on startup (when
// the last snapshot is not from today) and then hourly, keeping the newest
// platformBackupKeep copies.
func (s *Scheduler) runPlatformDBBackup(ctx context.Context) {
	defer s.wg.Done()

	ticker := time.NewTicker(platformBackupEvery)
	defer ticker.Stop()

	backupDir := filepath.Join(s.basePath, "backups")
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		log.Printf("[Backup] platform DB backup dir %s: %v", backupDir, err)
		observeLoop("platform_backup", err)
		return
	}

	lastDate := ""
	_, err := snapshotPlatformDBAtResult(s.db, backupDir, &lastDate, time.Now())
	observeLoop("platform_backup", err)
	observeLatestPlatformBackup(backupDir)

	for {
		select {
		case <-ctx.Done():
			log.Printf("[Backup] platform DB backup stopped")
			return
		case <-ticker.C:
			_, err := snapshotPlatformDBAtResult(s.db, backupDir, &lastDate, time.Now())
			observeLoop("platform_backup", err)
			observeLatestPlatformBackup(backupDir)
		}
	}
}

// snapshotPlatformDB preserves the legacy call shape while delegating all date
// decisions to the injectable-time helper.
func snapshotPlatformDB(db *gorm.DB, backupDir string, lastDate *string, _ bool) {
	snapshotPlatformDBAt(db, backupDir, lastDate, time.Now())
}

// snapshotPlatformDBAt writes at most one snapshot per calendar day. Supplying
// now makes the cross-midnight tick and retention behavior deterministic in
// focused tests without replacing the process clock.
func snapshotPlatformDBAt(db *gorm.DB, backupDir string, lastDate *string, now time.Time) bool {
	created, _ := snapshotPlatformDBAtResult(db, backupDir, lastDate, now)
	return created
}

func snapshotPlatformDBAtResult(db *gorm.DB, backupDir string, lastDate *string, now time.Time) (bool, error) {
	today := now.Format("2006-01-02")
	if *lastDate == today {
		return false, nil
	}
	if newest := newestPlatformBackup(backupDir); strings.HasPrefix(newest, "platform-db-"+today) {
		*lastDate = today
		return false, nil
	}

	dst := filepath.Join(backupDir, "platform-db-"+now.Format("2006-01-02-150405")+".db")
	if err := sqlite.CreateSnapshot(db, dst); err != nil {
		log.Printf("[Backup] platform DB snapshot failed: %v", err)
		return false, err
	}
	*lastDate = today
	log.Printf("[Backup] platform DB snapshot written: %s", dst)

	// Lexicographic order equals chronological order for the timestamp format.
	files, _ := filepath.Glob(filepath.Join(backupDir, "platform-db-*.db"))
	sort.Sort(sort.Reverse(sort.StringSlice(files)))
	for i := platformBackupKeep; i < len(files); i++ {
		if err := os.Remove(files[i]); err == nil {
			log.Printf("[Backup] rotated out old platform snapshot %s", files[i])
		}
	}
	return true, nil
}

func newestPlatformBackup(backupDir string) string {
	files, _ := filepath.Glob(filepath.Join(backupDir, "platform-db-*.db"))
	sort.Strings(files)
	if len(files) == 0 {
		return ""
	}
	return filepath.Base(files[len(files)-1])
}

func observeLatestPlatformBackup(backupDir string) {
	name := newestPlatformBackup(backupDir)
	if name == "" {
		return
	}
	if info, err := os.Stat(filepath.Join(backupDir, name)); err == nil {
		observeBackupSuccess(info.ModTime())
	}
}
