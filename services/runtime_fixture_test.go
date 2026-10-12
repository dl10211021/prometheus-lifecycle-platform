package services

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// systemd-managed Prometheus roots cannot live in PrivateTmp. Keep real
// runtime fixtures in a unique protected cache directory so plain go test
// exercises a valid production scope without global TMPDIR requirements.
func runtimeFixtureDir(t *testing.T) string {
	t.Helper()
	temp, resolveErr := filepath.EvalSymlinks(os.TempDir())
	private := func(path string) bool {
		path = filepath.Clean(path)
		return path == "/tmp" || strings.HasPrefix(path, "/tmp/") || path == "/var/tmp" || strings.HasPrefix(path, "/var/tmp/")
	}
	if resolveErr == nil && !private(temp) {
		return t.TempDir()
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cache, 0700); err != nil {
		t.Fatal(err)
	}
	cache, err = filepath.EvalSymlinks(cache)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(cache, "promlifecycle-runtime-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		absolute, err := filepath.Abs(dir)
		if err != nil {
			t.Error(err)
			return
		}
		parent, err := filepath.Abs(cache)
		if err != nil {
			t.Error(err)
			return
		}
		if filepath.Dir(absolute) != parent || !strings.HasPrefix(filepath.Base(absolute), "promlifecycle-runtime-") {
			t.Errorf("unsafe fixture cleanup target: %s", absolute)
			return
		}
		if err := os.RemoveAll(absolute); err != nil {
			t.Error(err)
		}
	})
	return dir
}
