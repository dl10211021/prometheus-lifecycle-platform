package deployer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promlifecycle/platform/models"
)

// Only systemctl is substituted: removal and symlink resolution operate on
// owned temporary files, and no real service is touched.
func keepDataFixture(t *testing.T) (*BinaryDeployer, *models.Component, string, string) {
	t.Helper()
	root := t.TempDir()
	commands := filepath.Join(root, "commands")
	if err := os.Mkdir(commands, 0700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(root, "systemctl.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$KEEP_DATA_COMMAND_LOG\"\ncase \"$1\" in is-active|is-enabled) exit 1;; esac\n"
	if err := os.WriteFile(filepath.Join(commands, "systemctl"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KEEP_DATA_COMMAND_LOG", log)
	t.Setenv("PATH", commands+":"+os.Getenv("PATH"))
	install := filepath.Join(root, "grafana")
	if err := os.MkdirAll(filepath.Join(install, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(install, "bin", "grafana"), []byte("fixture binary"), 0700); err != nil {
		t.Fatal(err)
	}
	c := &models.Component{Type: models.ComponentGrafana, DeployType: "binary", Host: models.Host{IP: "127.0.0.1", ConnectionType: "local"}, InstallPath: install, ConfigPath: filepath.Join(root, "config", "grafana.ini"), ServiceName: "plc-keep-data-" + filepath.Base(root) + "-" + filepath.Base(filepath.Dir(root))}
	return NewBinaryDeployer(nil, root), c, root, log
}

func TestRemoveKeepDataRejectsOverlappingDataBeforeMutation(t *testing.T) {
	for _, layout := range []string{"nested", "same_directory", "data_parent", "data_symlink", "install_parent_symlink", "recorded_data_also_preserved"} {
		t.Run(layout, func(t *testing.T) {
			d, c, root, log := keepDataFixture(t)
			actual := filepath.Join(c.InstallPath, "data")
			if layout == "same_directory" {
				actual = c.InstallPath
			}
			if layout == "data_parent" {
				actual = root
			}
			if err := os.MkdirAll(actual, 0700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(actual, "customer.db")
			if err := os.WriteFile(marker, []byte("retained data"), 0600); err != nil {
				t.Fatal(err)
			}
			c.DataPath = actual
			if layout == "data_symlink" {
				c.DataPath = filepath.Join(root, "data-alias")
				if err := os.Symlink(actual, c.DataPath); err != nil {
					t.Fatal(err)
				}
			}
			if layout == "install_parent_symlink" {
				alias := filepath.Join(root, "install-alias")
				if err := os.Symlink(c.InstallPath, alias); err != nil {
					t.Fatal(err)
				}
				c.InstallPath = filepath.Join(alias, "child", "..")
			}
			if layout == "recorded_data_also_preserved" {
				c.DataPathOverride = filepath.Join(root, "new-data")
				if err := os.Mkdir(c.DataPathOverride, 0700); err != nil {
					t.Fatal(err)
				}
			}
			err := d.RemoveKeepData(context.Background(), c)
			if err == nil || !strings.Contains(err.Error(), "keep_data") {
				t.Errorf("unsafe removal was not rejected with keep_data guidance: %v", err)
			}
			if data, readErr := os.ReadFile(marker); readErr != nil || string(data) != "retained data" {
				t.Errorf("retained data changed: %q, %v", data, readErr)
			}
			if commands, _ := os.ReadFile(log); len(commands) != 0 {
				t.Errorf("service mutation before rejection: %s", commands)
			}
			if _, statErr := os.Stat(filepath.Join(root, "grafana", "bin", "grafana")); statErr != nil {
				t.Errorf("installation changed before rejection: %v", statErr)
			}
		})
	}
}

func TestRemoveKeepDataAllowsSeparatedData(t *testing.T) {
	d, c, root, log := keepDataFixture(t)
	c.DataPath = filepath.Join(root, "grafana-data")
	if err := os.Mkdir(c.DataPath, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(c.DataPath, "customer.db")
	if err := os.WriteFile(marker, []byte("retained data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := d.RemoveKeepData(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "retained data" {
		t.Fatalf("retained data changed: %q %v", data, err)
	}
	if _, err := os.Stat(c.InstallPath); !os.IsNotExist(err) {
		t.Fatalf("installation not removed: %v", err)
	}
	if commands, _ := os.ReadFile(log); !strings.Contains(string(commands), "stop "+c.ServiceName) {
		t.Fatalf("removal did not stop fixture service: %s", commands)
	}
}

func TestRemoveFullDeleteStillRemovesNestedData(t *testing.T) {
	d, c, _, _ := keepDataFixture(t)
	c.DataPath = filepath.Join(c.InstallPath, "data")
	if err := os.Mkdir(c.DataPath, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.DataPath, "customer.db"), []byte("explicit full delete"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := d.Remove(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(c.InstallPath); !os.IsNotExist(err) {
		t.Fatalf("full-delete installation not removed: %v", err)
	}
}

func TestRemoveKeepDataRejectsUnresolvableDataBeforeMutation(t *testing.T) {
	d, c, root, log := keepDataFixture(t)
	c.DataPath = filepath.Join(root, "loop")
	if err := os.Symlink("loop", c.DataPath); err != nil {
		t.Fatal(err)
	}
	err := d.RemoveKeepData(context.Background(), c)
	if err == nil || !strings.Contains(err.Error(), "keep_data") {
		t.Errorf("unresolved data was not rejected: %v", err)
	}
	if commands, _ := os.ReadFile(log); len(commands) != 0 {
		t.Errorf("service mutation before rejection: %s", commands)
	}
	if _, err := os.Stat(c.InstallPath); err != nil {
		t.Errorf("installation changed: %v", err)
	}
}

func TestRemoveKeepDataRejectsOwnedConfigDataOnSSH(t *testing.T) {
	host, commands := binaryMutationPeer(t, "never-fail", 0)
	c := &models.Component{ID: 927361, Type: models.ComponentGrafana, DeployType: "binary", Host: host, DataPath: "/etc/promlifecycle-components/grafana-927361/data"}
	err := NewBinaryDeployer(nil, t.TempDir()).RemoveKeepData(context.Background(), c)
	if err == nil || !strings.Contains(err.Error(), "keep_data") {
		t.Errorf("owned config data overlap was not rejected: %v", err)
	}
	for _, cmd := range commands() {
		if strings.Contains(cmd, "systemctl stop") || strings.Contains(cmd, "rm -") {
			t.Errorf("remote mutation before rejection: %s", cmd)
		}
	}
}
