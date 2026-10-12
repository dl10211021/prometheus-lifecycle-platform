package services

import (
	"context"
	"crypto/rand"
	"fmt"
	"github.com/promlifecycle/platform/models"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Opt-in because this fixture creates, starts and removes its own systemd unit.
// It never operates on a preexisting unit, environment file or TSDB directory.
func TestRealSystemdUnitSnapshot(t *testing.T) {
	if os.Getenv("PROMLIFECYCLE_REAL_SYSTEMD_SNAPSHOT") != "1" {
		t.Skip("requires explicit owned-systemd acceptance opt-in")
	}
	if os.Geteuid() != 0 {
		t.Fatal("owned systemd acceptance requires root")
	}
	binary := os.Getenv("PROMLIFECYCLE_SNAPSHOT_PROMETHEUS_BINARY")
	if !filepath.IsAbs(binary) || !validUnitSnapshotPath(binary) {
		t.Fatal("absolute verified Prometheus fixture binary required")
	}
	version, err := exec.Command(binary, "--version").CombinedOutput()
	if err != nil || !strings.Contains(string(version), "version 2.55.0") {
		t.Fatal("fixture binary must be Prometheus 2.55.0")
	}
	var token [8]byte
	if _, err := rand.Read(token[:]); err != nil {
		t.Fatal(err)
	}
	unit := fmt.Sprintf("plc-snapshot-%x.service", token[:])
	unitPath := filepath.Join("/etc/systemd/system", unit)
	dropDir := unitPath + ".d"
	root, err := os.MkdirTemp("/var/lib", "plc-snapshot-")
	if err != nil {
		t.Fatal(err)
	}
	createdUnit, createdDrop := false, false
	t.Cleanup(func() {
		if createdUnit {
			if out, err := exec.Command("systemctl", "stop", unit).CombinedOutput(); err != nil {
				t.Errorf("owned unit cleanup stop: %s %v", out, err)
				return
			}
			out, err := exec.Command("systemctl", "show", "--property=MainPID", "--value", unit).Output()
			if err != nil || strings.TrimSpace(string(out)) != "0" {
				t.Errorf("owned PID cleanup unconfirmed")
				return
			}
		}
		if createdDrop {
			if err := os.Remove(filepath.Join(dropDir, "limits.conf")); err != nil {
				t.Error(err)
			}
			if err := os.Remove(dropDir); err != nil {
				t.Error(err)
			}
		}
		if createdUnit {
			if err := os.Remove(unitPath); err != nil {
				t.Error(err)
			}
		}
		if out, err := exec.Command("systemctl", "daemon-reload").CombinedOutput(); err != nil {
			t.Errorf("cleanup daemon reload: %s %v", out, err)
			return
		}
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	config := filepath.Join(root, "prometheus.yml")
	env := filepath.Join(root, "protected.env")
	missing := filepath.Join(root, "optional.env")
	for p, content := range map[string]string{config: "global:\n  scrape_interval: 1s\nscrape_configs: []\n", env: "SNAPSHOT_TEST_SECRET=private-systemd-sentinel\n"} {
		if err := os.WriteFile(p, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	content := fmt.Sprintf("[Unit]\nDescription=Owned startup snapshot acceptance\n[Service]\nType=simple\nEnvironmentFile=%s\nEnvironmentFile=-%s\nExecStart=%s --config.file=%s --storage.tsdb.path=%s --web.listen-address=127.0.0.1:%d\n[Install]\nWantedBy=multi-user.target\n", env, missing, binary, config, filepath.Join(root, "data"), port)
	f, err := os.OpenFile(unitPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	createdUnit = true
	if _, err = f.WriteString(content); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(dropDir, 0755); err != nil {
		t.Fatal(err)
	}
	createdDrop = true
	if err = os.WriteFile(filepath.Join(dropDir, "limits.conf"), []byte("[Service]\nLimitNOFILE=65536\n"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"daemon-reload"}, {"start", unit}} {
		if out, err := exec.Command("systemctl", args...).CombinedOutput(); err != nil {
			t.Fatalf("fixture start: %s %v", out, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	client := &http.Client{Timeout: time.Second}
	for {
		resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/-/ready", port))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("fixture readiness timeout")
		case <-time.After(100 * time.Millisecond):
		}
	}
	before, _, err := snapshotInventory(ctx, localBackupRunner{}, unit)
	if err != nil {
		t.Fatal(err)
	}
	s, c, _ := snapshotServiceFixture(t)
	s.backupRoot = filepath.Join(root, "snapshots")
	s.runnerFactory = s.runnerFor
	if err := s.db.Model(c).Updates(map[string]any{"service_name": unit, "imported_args": "", "config_path": config, "data_path": filepath.Join(root, "data"), "port": port}).Error; err != nil {
		t.Fatal(err)
	}
	ref, err := s.CaptureComponentUnitSnapshot(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	fresh := NewBackupServiceWithRoot(s.db, s.backupRoot)
	proof, err := fresh.VerifyComponentUnitSnapshot(ctx, ref, true)
	if err != nil {
		t.Fatal(err)
	}
	if proof.Files != 4 || proof.PresentFiles != 3 {
		t.Fatalf("real inventory incomplete: %+v", proof)
	}
	after, _, err := snapshotInventory(ctx, localBackupRunner{}, unit)
	if err != nil {
		t.Fatal(err)
	}
	if before["MainPID"] != after["MainPID"] || before["ActiveState"] != after["ActiveState"] || before["UnitFileState"] != after["UnitFileState"] {
		t.Fatal("capture changed process or enablement")
	}
	got, err := os.ReadFile(unitPath)
	if err != nil || string(got) != content {
		t.Fatal("capture rewrote unit")
	}
	// An edit on disk without daemon-reload must block a new baseline capture.
	f, err = os.OpenFile(unitPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("\n# external edit\n")
	f.Close()
	if bad, err := fresh.CaptureComponentUnitSnapshot(ctx, c.ID); bad != nil || err == nil {
		t.Fatal("pending daemon reload admitted")
	}
	if _, err := fresh.VerifyComponentUnitSnapshot(ctx, ref, false); err != nil {
		t.Fatal("historical artifact lost after live edit", err)
	}
	if _, err := fresh.VerifyComponentUnitSnapshot(ctx, ref, true); err == nil {
		t.Fatal("live edit not detected")
	}
	t.Logf("owned real Prometheus systemd acceptance: files=%d present=%d bytes=%d PID unchanged; live drift rejected", proof.Files, proof.PresentFiles, proof.Bytes)
	// Confirm component status/identity was not rewritten by snapshot operations.
	var current models.Component
	if err := s.db.First(&current, c.ID).Error; err != nil {
		t.Fatal(err)
	}
	if current.Version != "2.55.0" {
		t.Fatal("snapshot changed version")
	}
}
