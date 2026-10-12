package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise only the rollback function against disposable paths and a fake
// systemctl. The installer itself is never sourced or executed.
func TestInstallerRollbackRestoresArtifactsAndRefusesLiveDatabaseSwap(t *testing.T) {
	script, err := os.ReadFile("../../deploy.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	start := strings.Index(text, "rollback() {")
	end := strings.Index(text, "\ncleanup() {")
	if start < 0 || end < start {
		t.Fatal("rollback function not found")
	}
	rollback := text[start:end]
	for _, stopFails := range []bool{false, true} {
		name := "restores-original"
		if stopFails {
			name = "stop-failure-preserves-live"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			for _, dir := range []string{"bin", "exec-backup", "unit-backup", "db-backup/restore"} {
				if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
					t.Fatal(err)
				}
			}
			artifacts := map[string]string{"executable": "new executable", "unit": "new unit", "live.db": "new database", "live.db-wal": "candidate WAL", "exec-backup/original": "original executable", "unit-backup/original": "original unit", "db-backup/original.db": "consistent original database", "db-backup/restore/live.db.restore.json": "pending restore"}
			for path, data := range artifacts {
				if err := os.WriteFile(filepath.Join(root, path), []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			systemctl := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$CALLS\"\nif [ \"$1\" = stop ] && [ \"$STOP_FAILS\" = 1 ]; then exit 1; fi\n"
			if err := os.WriteFile(filepath.Join(root, "bin/systemctl"), []byte(systemctl), 0700); err != nil {
				t.Fatal(err)
			}
			command := exec.Command("bash", "-c", rollback+"\nrollback")
			fail := "0"
			if stopFails {
				fail = "1"
			}
			command.Env = append(os.Environ(), "PATH="+filepath.Join(root, "bin")+":"+os.Getenv("PATH"), "CALLS="+filepath.Join(root, "calls"), "STOP_FAILS="+fail,
				"SERVICE=isolated.service", "SERVICE_FILE="+filepath.Join(root, "unit"), "EXECUTABLE="+filepath.Join(root, "executable"), "DATA_DIR="+root, "DB_FILE="+filepath.Join(root, "live.db"), "EXEC_BACKUP="+filepath.Join(root, "exec-backup"), "UNIT_BACKUP="+filepath.Join(root, "unit-backup"), "DB_BACKUP="+filepath.Join(root, "db-backup"), "HAD_EXEC=1", "HAD_UNIT=1", "HAD_DB=1", "WAS_ACTIVE=1", "WAS_ENABLED=1", "INIT_PID=")
			output, err := command.CombinedOutput()
			if stopFails && err == nil {
				t.Fatal("rollback ignored active writer stop failure")
			}
			if !stopFails && err != nil {
				t.Fatalf("rollback: %v: %s", err, output)
			}
			want := map[string]string{"executable": "original executable", "unit": "original unit", "live.db": "consistent original database", "live.db.restore.json": "pending restore"}
			if stopFails {
				want = map[string]string{"executable": "new executable", "unit": "new unit", "live.db": "new database", "live.db-wal": "candidate WAL"}
			}
			for path, expected := range want {
				data, err := os.ReadFile(filepath.Join(root, path))
				if err != nil || string(data) != expected {
					t.Fatalf("%s=%q error=%v want=%q", path, data, err, expected)
				}
			}
			calls, err := os.ReadFile(filepath.Join(root, "calls"))
			if err != nil {
				t.Fatal(err)
			}
			if stopFails && strings.Contains(string(calls), "start ") {
				t.Fatal("restarted service after unsafe rollback")
			}
			if !stopFails && !strings.Contains(string(calls), "start isolated.service") {
				t.Fatal("did not restart original service")
			}
		})
	}
}
