package utils

import (
	"path/filepath"
	"testing"
)

func TestPathWithinBase(t *testing.T) {
	t.Parallel()
	base := filepath.Join(string(filepath.Separator), "opt", "promlifecycle", "packages")
	if !PathWithinBase(base, filepath.Join(base, "prometheus-2.53.0.tar.gz")) {
		t.Fatal("valid package path rejected")
	}
	if PathWithinBase(base, filepath.Join(base, "..", "secrets")) {
		t.Fatal("escaping package path accepted")
	}
	if PathWithinBase(base, base) {
		t.Fatal("package directory itself accepted as a file")
	}
}

func TestValidVersionRejectsPathTraversal(t *testing.T) {
	t.Parallel()
	for _, version := range []string{"../../etc/passwd", "v1.2.3/evil", "latest", "", "1.2.3\\evil"} {
		if ValidVersion(version) {
			t.Fatalf("unsafe version accepted: %q", version)
		}
	}
	for _, version := range []string{"1.2.3", "v2.53.0", "1.0.0-rc.1"} {
		if !ValidVersion(version) {
			t.Fatalf("valid version rejected: %q", version)
		}
	}
}
