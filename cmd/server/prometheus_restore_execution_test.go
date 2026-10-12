package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPrometheusRestoreExecutionRecoveryCommandFailsBeforeStartup(t *testing.T) {
	for _, mode := range []string{"missing", "corrupt", "held", "whole-db", "empty-id"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			data := filepath.Join(root, "data")
			base := filepath.Join(root, "never-create")
			path := filepath.Join(data, "promlifecycle.db")
			if mode != "missing" {
				os.Mkdir(data, 0700)
				os.WriteFile(path, []byte("not sqlite"), 0600)
			}
			if mode == "held" {
				_, lock, err := acquireDatabaseProcessLock(path)
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
			}
			if mode == "whole-db" {
				os.WriteFile(path+".restore.json", []byte("{\"status\":\"applying\"}"), 0600)
			}
			id := "1"
			if mode == "empty-id" {
				id = ""
			}
			args, _ := json.Marshal([]string{"--recover-prometheus-restore-execution=" + id, "--restore-backup-root=" + filepath.Join(root, "artifacts"), "--data-dir=" + data, "--base-path=" + base, "--listen-address=0.0.0.0"})
			executable, _ := os.Executable()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestStartupProcessHelper$")
			cmd.Env = append(os.Environ(), "PROMLIFECYCLE_TEST_SERVER_ARGS="+string(args))
			output, err := cmd.CombinedOutput()
			if err == nil || ctx.Err() != nil || !strings.Contains(string(output), "paired restore execution recovery rejected") || strings.Contains(string(output), "flag provided but not defined") {
				t.Fatalf("recovery command not handled before startup: %v %s", err, output)
			}
			if strings.Contains(string(output), root) || strings.Contains(string(output), "not sqlite") {
				t.Fatal("private recovery detail leaked")
			}
			if _, err := os.Stat(base); !os.IsNotExist(err) {
				t.Fatal("recovery created runtime base")
			}
			if mode == "missing" {
				if _, err := os.Stat(data); !os.IsNotExist(err) {
					t.Fatal("recovery created missing database directory")
				}
			}
		})
	}
}
