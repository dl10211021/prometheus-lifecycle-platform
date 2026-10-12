//go:build !linux

package sqlite

import (
	"errors"
	"os"
)

func lockDatabaseProcessFile(*os.File) error {
	return errors.New("exclusive database ownership unsupported on this platform")
}
func regularSingleLink(os.FileInfo) bool { return false }
