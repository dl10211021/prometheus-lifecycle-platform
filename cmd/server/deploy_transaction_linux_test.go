package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

type installerFixture struct {
	root, cwd, installer, binary string
	env                          []string
}

func newInstallerFixture(t *testing.T, mode string) installerFixture {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("installer integration requires root to exercise its permission checks")
	}
	for _, tool := range []string{"bash", "python3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s unavailable: %v", tool, err)
		}
	}
	// /tmp is traversable by the original service UID in the rollback probe.
	root, err := os.MkdirTemp("", "promlifecycle-installer-test-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	f := installerFixture{root: root, cwd: filepath.Join(root, "release"),
		installer: filepath.Join(root, "deploy.sh"), binary: filepath.Join(root, "candidate")}
	for _, dir := range []string{f.cwd, filepath.Join(root, "bin")} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	fixture, err := os.ReadFile("testdata/installer-fixture.py")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{f.binary, filepath.Join(root, "bin/systemctl")} {
		if err := os.WriteFile(file, fixture, 0755); err != nil {
			t.Fatal(err)
		}
	}
	script, err := os.ReadFile("../../deploy.sh")
	if err != nil {
		t.Fatal(err)
	}
	// Relocate only the host unit boundary; run all installer phases unchanged.
	const hostUnit = "SERVICE_FILE=/etc/systemd/system/$SERVICE"
	if strings.Count(string(script), hostUnit) != 1 {
		t.Fatal("installer service file boundary changed")
	}
	script = []byte(strings.Replace(string(script), hostUnit, "SERVICE_FILE=\"$INSTALLER_TEST_ROOT/promlifecycle-server.service\"", 1))
	if err := os.WriteFile(f.installer, script, 0700); err != nil {
		t.Fatal(err)
	}
	f.env = append(os.Environ(), "PATH="+filepath.Join(root, "bin")+":"+os.Getenv("PATH"),
		"INSTALLER_TEST_ROOT="+root, "INSTALLER_TEST_MODE="+mode,
		"BASE_DIR=", "DATA_DIR=", "JWT_SECRET_FILE=", "ADMIN_PASSWORD_FILE=", "PORT=", "LISTEN_ADDRESS=",
		"PROMLIFECYCLE_ADMIN_PASSWORD_FILE=", "PROMLIFECYCLE_SNAPSHOT_RESERVE_BYTES=0")
	t.Cleanup(func() {
		stop := exec.Command(filepath.Join(root, "bin/systemctl"), "stop", "isolated.service")
		stop.Env = f.env
		if output, err := stop.CombinedOutput(); err != nil {
			t.Errorf("fixture cleanup: %v: %s", err, output)
		}
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("fixture cleanup: %v", err)
		}
	})
	return f
}

func (f installerFixture) deploy(args ...string) ([]byte, error) {
	command := exec.Command("bash", append([]string{f.installer, "--binary", f.binary}, args...)...)
	command.Dir, command.Env = f.cwd, f.env
	return command.CombinedOutput()
}

// A raw relative path reaches initialization from the caller's directory but
// reaches service startup from WorkingDirectory. Both must open the same DB.
func TestInstallerNewInstallResolvesPathsFromInvocationDirectory(t *testing.T) {
	for _, mode := range []string{"relative-data", "all-relative", "relative-base-defaults", "absolute"} {
		t.Run(mode, func(t *testing.T) {
			f := newInstallerFixture(t, "install")
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			port := listener.Addr().(*net.TCPAddr).Port
			listener.Close()
			base := filepath.Join(f.root, "installed")
			data := filepath.Join(f.cwd, "custom-data")
			jwt, admin := filepath.Join(base, "jwt_secret"), filepath.Join(base, "admin_initial_password")
			args := []string{"--base", base, "--data", "custom-data"}
			switch mode {
			case "all-relative":
				base = filepath.Join(f.cwd, "installed")
				jwt, admin = filepath.Join(f.cwd, "secrets/jwt"), filepath.Join(f.cwd, "secrets/admin")
				args = []string{"--base", "installed", "--data", "custom-data",
					"--jwt-secret-file", "secrets/jwt", "--admin-password-file", "secrets/admin"}
			case "relative-base-defaults":
				base = filepath.Join(f.cwd, "installed")
				data = filepath.Join(base, "data")
				jwt, admin = filepath.Join(base, "jwt_secret"), filepath.Join(base, "admin_initial_password")
				args = []string{"--base", "installed"}
			case "absolute":
				args = []string{"--base", base, "--data", data}
			}
			args = append(args, "--port", fmt.Sprint(port))
			output, err := f.deploy(args...)
			if err != nil {
				t.Fatalf("new install: %v: %s", err, output)
			}
			observation, err := os.ReadFile(filepath.Join(f.root, "service-observation.json"))
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]string
			if err := json.Unmarshal(observation, &got); err != nil {
				t.Fatal(err)
			}
			for key, want := range map[string]string{"database": filepath.Join(data, "promlifecycle.db"),
				"marker": "initialized", "base": base, "jwt": jwt, "listen": "0.0.0.0"} {
				if got[key] != want {
					t.Errorf("running service %s=%q, want %q", key, got[key], want)
				}
			}
			if !strings.Contains(string(output), "Initial admin password: "+admin+"\n") {
				t.Errorf("installer did not report canonical password file %q: %s", admin, output)
			}
			if _, err := os.Stat(admin); err != nil {
				t.Errorf("initial password file: %v", err)
			}
		})
	}
}

// A candidate can change both contents and metadata before failing; rollback
// must restore the original account's ability to open and write its database.
func TestInstallerFailedUpgradeRestoresDatabaseAccessMetadata(t *testing.T) {
	f := newInstallerFixture(t, "rollback")
	base, data := filepath.Join(f.root, "installed"), filepath.Join(f.root, "data")
	for _, dir := range []string{base, data} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	executable := filepath.Join(base, "promlifecycle-server")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n# installer-original\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	unit := fmt.Sprintf("[Service]\nUser=65534\nWorkingDirectory=%s\nExecStart=%s --base-path %s --data-dir %s --listen-address 127.0.0.1\n", base, executable, base, data)
	if err := os.WriteFile(filepath.Join(f.root, "promlifecycle-server.service"), []byte(unit), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "service-state.json"), []byte("{\"active\":true,\"enabled\":true,\"pid\":0}"), 0600); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(data, "promlifecycle.db")
	create := exec.Command("python3", "-c", "import sqlite3,sys; db=sqlite3.connect(sys.argv[1]); db.execute('CREATE TABLE marker(value TEXT)'); db.execute(\"INSERT INTO marker VALUES ('original')\"); db.commit(); db.close()", db)
	if output, err := create.CombinedOutput(); err != nil {
		t.Fatalf("create database: %v: %s", err, output)
	}
	if err := os.Chown(data, 65534, 65534); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(db, 65534, 65534); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(db, 0640); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Setxattr(db, "user.installer-test", []byte("original"), 0); err != nil {
		t.Fatal(err)
	}
	output, err := f.deploy("--listen-address", "0.0.0.0")
	if err == nil {
		t.Fatal("candidate failure unexpectedly succeeded")
	}
	info, err := os.Stat(db)
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	if stat.Uid != 65534 || stat.Gid != 65534 || info.Mode().Perm() != 0640 {
		t.Errorf("restored DB owner/mode=%d:%d %04o, want 65534:65534 0640", stat.Uid, stat.Gid, info.Mode().Perm())
	}
	value := make([]byte, 32)
	n, err := syscall.Getxattr(db, "user.installer-test", value)
	if n < 0 {
		n = 0
	}
	if err != nil || string(value[:n]) != "original" {
		t.Errorf("restored DB extended attribute=%q err=%v, want original", value[:n], err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "original-user-reopened")); err != nil {
		t.Errorf("original non-root service could not reopen restored DB: %v: %s", err, output)
	}
	if strings.Contains(string(output), "ROLLBACK FAILED") {
		t.Errorf("rollback failed: %s", output)
	}
	check := exec.Command("python3", "-c", "import sqlite3,sys; db=sqlite3.connect(sys.argv[1]); assert db.execute('PRAGMA integrity_check').fetchone()[0]=='ok'; assert db.execute('SELECT value FROM marker').fetchone()[0]=='original'", db)
	if output, err := check.CombinedOutput(); err != nil {
		t.Errorf("restored database contents/integrity: %v: %s", err, output)
	}
	restored, err := os.ReadFile(filepath.Join(f.root, "promlifecycle-server.service"))
	if err != nil || string(restored) != unit {
		t.Errorf("original non-root unit not preserved: %v", err)
	}
}

// New installs need a loopback-only unit from their very first service start.
func TestInstallerNewInstallHonorsListenAddress(t *testing.T) {
	f := newInstallerFixture(t, "install")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	base := filepath.Join(f.root, "installed")
	output, err := f.deploy("--base", base, "--port", fmt.Sprint(port), "--listen-address", "127.0.0.1")
	if err != nil {
		t.Fatalf("loopback install: %v: %s", err, output)
	}
	observation, err := os.ReadFile(filepath.Join(f.root, "service-observation.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal(observation, &got); err != nil {
		t.Fatal(err)
	}
	if got["listen"] != "127.0.0.1" {
		t.Errorf("running service listen address=%q, want 127.0.0.1", got["listen"])
	}
}

func TestInstallerRejectsInvalidListenAddressBeforeMutation(t *testing.T) {
	for _, address := range []string{"localhost:8080", "127.0.0.1\nEnvironment=INJECTED=true", "::1%scope"} {
		t.Run(address, func(t *testing.T) {
			f := newInstallerFixture(t, "install")
			base := filepath.Join(f.root, "installed")
			if output, err := f.deploy("--base", base, "--listen-address", address); err == nil {
				t.Fatalf("invalid listen address accepted: %s", output)
			}
			for _, path := range []string{base, filepath.Join(f.root, "promlifecycle-server.service")} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Errorf("invalid address mutated %s: %v", path, err)
				}
			}
		})
	}
}
