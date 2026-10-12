package main

import "github.com/promlifecycle/platform/storage/sqlite"

// Startup and explicit recovery always use the same canonical lock capability.
func acquireDatabaseProcessLock(path string) (string, *sqlite.DatabaseProcessLock, error) {
	return sqlite.AcquireDatabaseProcessLock(path)
}
