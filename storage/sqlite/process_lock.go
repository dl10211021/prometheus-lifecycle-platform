package sqlite

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DatabaseProcessLock cannot be forged through a pathname or a boolean.
// Close is serialized with the complete ownership callback, including host I/O.
type DatabaseProcessLock struct {
	mu     sync.Mutex
	file   *os.File
	path   string
	closed bool
}

func canonicalDatabaseProcessPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if _, err = os.Lstat(absolute); err == nil {
		return filepath.EvalSymlinks(absolute)
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(absolute)), nil
}
func AcquireDatabaseProcessLock(path string) (string, *DatabaseProcessLock, error) {
	canonical, err := canonicalDatabaseProcessPath(path)
	if err != nil {
		return "", nil, fmt.Errorf("resolve database ownership path: %w", err)
	}
	if info, e := os.Lstat(canonical); e == nil {
		if !regularSingleLink(info) {
			return "", nil, errors.New("database ownership requires an ordinary single-link file")
		}
	} else if !os.IsNotExist(e) {
		return "", nil, e
	}
	file, err := os.OpenFile(canonical+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return "", nil, err
	}
	l := &DatabaseProcessLock{file: file, path: canonical}
	if err = l.validateLock(); err == nil {
		err = lockDatabaseProcessFile(file)
	}
	if err != nil {
		file.Close()
		return "", nil, fmt.Errorf("database is already in use or cannot be exclusively locked; stop the existing platform process before retrying: %w", err)
	}
	return canonical, l, nil
}
func (l *DatabaseProcessLock) validateLock() error {
	if l == nil || l.closed || l.file == nil || l.path == "" {
		return errors.New("database ownership capability unavailable")
	}
	fd, err := l.file.Stat()
	if err != nil {
		return err
	}
	path, err := os.Lstat(l.path + ".lock")
	if err != nil {
		return err
	}
	if !regularSingleLink(fd) || !regularSingleLink(path) || !os.SameFile(fd, path) {
		return errors.New("database ownership lock identity changed")
	}
	return nil
}
func (l *DatabaseProcessLock) WithOwnership(ctx context.Context, databasePath string, fn func() error) error {
	if l == nil || fn == nil {
		return errors.New("database ownership capability required")
	}
	// A context-aware wait avoids blocking cancellation behind an active callback.
	for !l.mu.TryLock() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
	defer l.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := l.validateLock(); err != nil {
		return err
	}
	canonical, err := canonicalDatabaseProcessPath(databasePath)
	if err != nil || canonical != l.path {
		return errors.New("database ownership does not match actual database")
	}
	info, err := os.Lstat(canonical)
	if err != nil || !regularSingleLink(info) {
		return errors.New("existing single-link database required")
	}
	if err := lockDatabaseProcessFile(l.file); err != nil {
		return errors.New("database exclusivity lost")
	}
	return fn()
}
func (l *DatabaseProcessLock) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	if l.file == nil {
		return nil
	}
	return l.file.Close()
}
