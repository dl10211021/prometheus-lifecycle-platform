package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/services"
	platformsqlite "github.com/promlifecycle/platform/storage/sqlite"
)

// Run the real main in a disposable child rather than simulating ownership.
func TestStartupProcessHelper(t *testing.T) {
	encoded := os.Getenv("PROMLIFECYCLE_TEST_SERVER_ARGS")
	if encoded == "" {
		return
	}
	var args []string
	if err := json.Unmarshal([]byte(encoded), &args); err != nil {
		panic(err)
	}
	flag.CommandLine = flag.NewFlagSet("server", flag.ExitOnError)
	os.Args = append([]string{"server"}, args...)
	main()
	os.Exit(0)
}

func startupTestPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return port
}

func startupTestCommand(t *testing.T, root, data string, port int) (*exec.Cmd, string) {
	t.Helper()
	args, _ := json.Marshal([]string{"--data-dir", data, "--base-path", filepath.Join(root, "base"), "--listen-address", "127.0.0.1", "--port", strconv.Itoa(port)})
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestStartupProcessHelper$")
	command.Env = append(os.Environ(), "PROMLIFECYCLE_TEST_SERVER_ARGS="+string(args), "PROMLIFECYCLE_OFFLINE=1", "PROMLIFECYCLE_ADMIN_PASSWORD=fixture-Aa123456", "PROMLIFECYCLE_JWT_SECRET=fixture-jwt-key-for-process-ownership-testing")
	log, err := os.CreateTemp(root, "startup-*.log")
	if err != nil {
		t.Fatal(err)
	}
	command.Stdout, command.Stderr = log, log
	t.Cleanup(func() { log.Close() })
	return command, log.Name()
}

func startOwnedServer(t *testing.T, root, data string, port int) (*exec.Cmd, func(bool)) {
	t.Helper()
	command, log := startupTestCommand(t, root, data, port)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	stopped := false
	stop := func(crash bool) {
		if stopped {
			return
		}
		stopped = true
		if crash {
			command.Process.Kill()
		} else {
			command.Process.Signal(syscall.SIGTERM)
		}
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			command.Process.Kill()
			<-done
			t.Error("server failed to stop")
		}
	}
	t.Cleanup(func() { stop(true) })
	client := &http.Client{Timeout: 200 * time.Millisecond}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			stopped = true
			content, _ := os.ReadFile(log)
			t.Fatalf("server exited: %v\n%s", err, content)
		default:
		}
		response, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/health", port))
		if err == nil {
			response.Body.Close()
			if response.StatusCode == 200 {
				return command, stop
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	content, _ := os.ReadFile(log)
	t.Fatalf("server not healthy\n%s", content)
	return nil, nil
}

func TestStartupProcessLockRejectsDuplicateWithoutMutatingRecovery(t *testing.T) {
	for _, variant := range []string{"same-port", "different-port", "symlink-data-dir", "symlink-database", "pending-restore"} {
		t.Run(variant, func(t *testing.T) {
			root := t.TempDir()
			data := filepath.Join(root, "data")
			port := startupTestPort(t)
			_, stop := startOwnedServer(t, root, data, port)
			defer stop(false)
			db, err := platformsqlite.OpenDB(filepath.Join(data, "promlifecycle.db"))
			if err != nil {
				t.Fatal(err)
			}
			sqlDB, _ := db.DB()
			defer sqlDB.Close()
			operation := models.Operation{Type: "upgrade", Status: "running", Message: "live owner marker"}
			claim := models.ComponentOperationClaim{ComponentID: 998, Token: "live-owner-token", Kind: "upgrade", CreatedAt: time.Now()}
			checkpoint := services.ComponentRestoreCheckpoint{ComponentID: 998, Path: filepath.Join(root, "live-checkpoint"), Status: "running"}
			for _, value := range []any{&operation, &claim, &checkpoint} {
				if err := db.Create(value).Error; err != nil {
					t.Fatal(err)
				}
			}
			otherPort := port
			if variant != "same-port" {
				otherPort = startupTestPort(t)
			}
			duplicateData := data
			if variant == "symlink-data-dir" {
				duplicateData = filepath.Join(root, "data-alias")
				if err := os.Symlink(data, duplicateData); err != nil {
					t.Fatal(err)
				}
			}
			if variant == "symlink-database" {
				duplicateData = filepath.Join(root, "other-data")
				os.MkdirAll(duplicateData, 0700)
				if err := os.Symlink(filepath.Join(data, "promlifecycle.db"), filepath.Join(duplicateData, "promlifecycle.db")); err != nil {
					t.Fatal(err)
				}
			}
			if variant == "pending-restore" {
				// Invalid pending input proves ownership is checked before restore parsing.
				if err := os.WriteFile(filepath.Join(data, "promlifecycle.db.restore.json"), []byte("pending restore must remain untouched"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			command, log := startupTestCommand(t, root, duplicateData, otherPort)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- command.Wait() }()
			select {
			case <-done:
			case <-ctx.Done():
				command.Process.Kill()
				<-done
				t.Fatal("duplicate startup did not reject promptly")
			}
			var gotOperation models.Operation
			var gotClaim models.ComponentOperationClaim
			var gotCheckpoint services.ComponentRestoreCheckpoint
			if err := db.First(&gotOperation, operation.ID).Error; err != nil {
				t.Fatal(err)
			}
			if gotOperation.Status != "running" || gotOperation.Message != "live owner marker" {
				t.Fatalf("duplicate startup modified active operation: %+v", gotOperation)
			}
			if err := db.First(&gotClaim, "component_id = ?", claim.ComponentID).Error; err != nil || gotClaim.Token != "live-owner-token" {
				t.Fatalf("duplicate startup removed live claim: %+v %v", gotClaim, err)
			}
			if err := db.First(&gotCheckpoint, checkpoint.ID).Error; err != nil || gotCheckpoint.Status != "running" {
				t.Fatalf("duplicate startup modified recovery record: %+v %v", gotCheckpoint, err)
			}
			if variant == "pending-restore" {
				content, err := os.ReadFile(filepath.Join(data, "promlifecycle.db.restore.json"))
				if err != nil || string(content) != "pending restore must remain untouched" {
					t.Fatalf("duplicate changed pending restore: %q %v", content, err)
				}
			}
			output, _ := os.ReadFile(log)
			if command.ProcessState.Success() || !strings.Contains(string(output), "already in use") {
				t.Fatalf("duplicate lacked ownership error: %s", output)
			}
		})
	}
}

func TestStartupProcessLockReacquiresAfterExitAndCrash(t *testing.T) {
	for _, crash := range []bool{false, true} {
		t.Run(fmt.Sprint(crash), func(t *testing.T) {
			root := t.TempDir()
			data := filepath.Join(root, "data")
			_, stop := startOwnedServer(t, root, data, startupTestPort(t))
			stop(crash)
			_, nextStop := startOwnedServer(t, root, data, startupTestPort(t))
			nextStop(false)
		})
	}
}
