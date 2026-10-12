package services

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promlifecycle/platform/pkg/utils"
)

func TestPrometheusRestoreCheckpointProbeCleanupRespectsOwnership(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root-only owned probe cleanup")
	}
	for _, name := range []string{"preexisting", "symlink", "hardlink", "owned"} {
		foreign := name != "owned"
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			bin := filepath.Join(root, "bin")
			path := filepath.Join(root, "probe.service")
			calls := filepath.Join(root, "systemctl.calls")
			if err := os.Mkdir(bin, 0700); err != nil {
				t.Fatal(err)
			}
			script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + utils.QuoteShell(calls) + "\ncase \"$1\" in show) case \"$*\" in *MainPID*) printf '0\\n';; *) printf 'not-found\\n';; esac;; esac\n"
			if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			wanted := "[Service]\nExecStart=/exclusive/current-fixture/prometheus\n"
			actual := wanted
			if name == "preexisting" {
				actual = "[Service]\nExecStart=/preexisting/service\n"
			}
			if err := os.WriteFile(path, []byte(actual), 0644); err != nil {
				t.Fatal(err)
			}
			if name == "symlink" || name == "hardlink" {
				target := path + ".target"
				if err := os.Rename(path, target); err != nil {
					t.Fatal(err)
				}
				var err error
				if name == "symlink" {
					err = os.Symlink(target, path)
				} else {
					err = os.Link(target, path)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			command := "export PATH=" + utils.QuoteShell(bin) + ":/usr/bin:/bin\n" + checkpointProbeCleanupCommand(path, "probe.service", pointDigest([]byte(wanted)))
			_, err := (localBackupRunner{}).Run(context.Background(), command)
			if foreign {
				after, readErr := os.ReadFile(path)
				if err == nil || readErr != nil || string(after) != actual {
					t.Fatal("cleanup took over a preexisting probe unit", err, readErr)
				}
				if _, readErr := os.Stat(calls); !os.IsNotExist(readErr) {
					t.Fatal("foreign probe cleanup contacted systemd")
				}
			} else {
				if err != nil {
					t.Fatal("owned probe cleanup rejected", err)
				}
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("owned probe unit remained")
				}
				raw, err := os.ReadFile(calls)
				if err != nil || !strings.Contains(string(raw), "stop probe.service") {
					t.Fatal("owned cleanup did not stop its unit", err)
				}
			}
		})
	}
}
