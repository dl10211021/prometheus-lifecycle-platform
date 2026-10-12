package services

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRecoveryPointBoundedFile(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root-only protected filesystem evidence")
	}
	root := t.TempDir()
	path := filepath.Join(root, "binary")
	content := make([]byte, (1<<20)+17)
	for i := range content {
		content[i] = byte(i % 251)
	}
	if err := os.WriteFile(path, content, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectUnitSnapshotFile(context.Background(), localBackupRunner{}, unitSnapshotFile{Path: path}); err == nil {
		t.Fatal("unit-file limit was relaxed")
	}
	info, err := inspectBoundedSnapshotFile(context.Background(), localBackupRunner{}, unitSnapshotFile{Path: path}, 2<<20)
	if err != nil || info.Size != int64(len(content)) || info.Mode != 0755 || len(info.SHA256) != 64 {
		t.Fatalf("bounded binary evidence: %+v %v", info, err)
	}
	if _, err := inspectBoundedSnapshotFile(context.Background(), localBackupRunner{}, unitSnapshotFile{Path: path}, int64(len(content)-1)); err == nil {
		t.Fatal("over-limit binary admitted")
	}
	for _, limit := range []int64{0, -1, (512 << 20) + 1} {
		if _, err := inspectBoundedSnapshotFile(context.Background(), localBackupRunner{}, unitSnapshotFile{Path: path}, limit); err == nil {
			t.Fatal("invalid inspector limit admitted")
		}
	}
	if err := os.Chmod(path, 0777); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectBoundedSnapshotFile(context.Background(), localBackupRunner{}, unitSnapshotFile{Path: path}, 2<<20); err == nil {
		t.Fatal("writable binary admitted")
	}
	if err := os.Chmod(path, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(path, filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectBoundedSnapshotFile(context.Background(), localBackupRunner{}, unitSnapshotFile{Path: path}, 2<<20); err == nil {
		t.Fatal("hard-linked binary admitted")
	}
}
