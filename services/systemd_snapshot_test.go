package services

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func snapshotProperties(unit, fragment, dropins, env string) string {
	return fmt.Sprintf("Id=%s\nLoadState=loaded\nFragmentPath=%s\nDropInPaths=%s\nEnvironmentFiles=%s\nUnitFileState=disabled\nActiveState=active\nSubState=running\nMainPID=123\nControlPID=0\nNeedDaemonReload=no\nTransient=no\n", unit, fragment, dropins, env)
}

func TestUnitSnapshotProperties(t *testing.T) {
	base := snapshotProperties("prom-test.service", "/etc/systemd/system/prom-test.service", "/etc/systemd/system/prom-test.service.d/limits.conf", "/etc/prom/env (ignore_errors=no) /etc/prom/optional (ignore_errors=yes)")
	state, files, err := parseUnitSnapshotProperties("prom-test.service", base)
	if err != nil || len(files) != 4 || state["MainPID"] != "123" || !files[3].Optional {
		t.Fatalf("valid inventory: %v %v %v", state, files, err)
	}
	tests := []struct{ name, from, to string }{
		{"alias", "Id=prom-test.service", "Id=other.service"},
		{"reload", "NeedDaemonReload=no", "NeedDaemonReload=yes"},
		{"transient", "Transient=no", "Transient=yes"},
		{"masked", "UnitFileState=disabled", "UnitFileState=masked"},
		{"unloaded", "LoadState=loaded", "LoadState=not-found"},
		{"transition", "ActiveState=active", "ActiveState=activating"},
		{"control-process", "ControlPID=0", "ControlPID=12"},
		{"missing-pid", "MainPID=123", "MainPID=0"},
		{"bad-pid", "MainPID=123", "MainPID=-1"},
		{"wildcard", "/etc/prom/env", "/etc/prom/*.env"},
		{"traversal", "/etc/prom/env", "/etc/prom/../env"},
		{"escaped", "/etc/prom/env", `/etc/prom/a\x20b`},
		{"missing", "Transient=no\n", ""},
		{"duplicate", "Transient=no\n", "Transient=no\nTransient=no\n"},
		{"extra", "Transient=no\n", "Transient=no\nSecret=value\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := parseUnitSnapshotProperties("prom-test.service", strings.Replace(base, tt.from, tt.to, 1)); err == nil {
				t.Fatal("unsafe inventory admitted")
			}
		})
	}
}

type snapshotPropertyRunner struct {
	properties, object, environmentFiles string
	objectErr, environmentFilesErr       error
	commands                             []string
}

func (r *snapshotPropertyRunner) Run(_ context.Context, cmd string) (string, error) {
	r.commands = append(r.commands, cmd)
	switch {
	case strings.HasPrefix(cmd, "systemctl show "):
		return r.properties, nil
	case strings.HasPrefix(cmd, "busctl call "):
		return r.object, r.objectErr
	case strings.HasPrefix(cmd, "busctl get-property "):
		return r.environmentFiles, r.environmentFilesErr
	default:
		return "", fmt.Errorf("unexpected command: %s", cmd)
	}
}

func TestReadUnitSnapshotStateRequiresTypedEmptyEnvironmentFilesFallback(t *testing.T) {
	unit := "prom-test.service"
	complete := snapshotProperties(unit, "/etc/systemd/system/prom-test.service", "", "")
	missingEnvironmentFiles := strings.Replace(complete, "EnvironmentFiles=\n", "", 1)
	object := "o \"/org/freedesktop/systemd1/unit/prom_2dtest_2eservice\"\n"

	t.Run("typed empty array", func(t *testing.T) {
		runner := &snapshotPropertyRunner{properties: missingEnvironmentFiles, object: object, environmentFiles: "a(sb) 0\n"}
		state, _, err := readUnitSnapshotState(context.Background(), runner, unit)
		if err != nil || state["EnvironmentFiles"] != "" {
			t.Fatalf("typed empty EnvironmentFiles was not recovered: state=%v err=%v", state, err)
		}
		if len(runner.commands) != 3 || !strings.Contains(runner.commands[0], " --all ") {
			t.Fatalf("unexpected compatibility command sequence: %#v", runner.commands)
		}
	})

	t.Run("complete output", func(t *testing.T) {
		runner := &snapshotPropertyRunner{properties: complete}
		if _, _, err := readUnitSnapshotState(context.Background(), runner, unit); err != nil {
			t.Fatal(err)
		}
		if len(runner.commands) != 1 {
			t.Fatalf("complete systemctl output triggered fallback: %#v", runner.commands)
		}
	})

	for _, tc := range []struct {
		name, properties, object, environmentFiles string
		objectErr, environmentFilesErr             error
	}{
		{"nonempty array", missingEnvironmentFiles, object, "a(sb) 1 \"/etc/prom/env\" false\n", nil, nil},
		{"malformed array", missingEnvironmentFiles, object, "a(sb)\n", nil, nil},
		{"typed trailing data", missingEnvironmentFiles, object, "a(sb) 0\nextra\n", nil, nil},
		{"foreign object", missingEnvironmentFiles, "o \"/org/example/foreign\"\n", "a(sb) 0\n", nil, nil},
		{"nested unit object", missingEnvironmentFiles, "o \"/org/freedesktop/systemd1/unit/nested/path\"\n", "a(sb) 0\n", nil, nil},
		{"GetUnit failure", missingEnvironmentFiles, "", "", errors.New("dbus unavailable"), nil},
		{"get-property failure", missingEnvironmentFiles, object, "", nil, errors.New("property unavailable")},
		{"other field missing", strings.Replace(missingEnvironmentFiles, "Transient=no\n", "", 1), object, "a(sb) 0\n", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &snapshotPropertyRunner{properties: tc.properties, object: tc.object,
				environmentFiles: tc.environmentFiles, objectErr: tc.objectErr,
				environmentFilesErr: tc.environmentFilesErr}
			if _, _, err := readUnitSnapshotState(context.Background(), runner, unit); err == nil {
				t.Fatal("unsupported or incomplete property evidence was admitted")
			}
		})
	}
}

type snapshotFixtureRunner struct {
	properties    string
	showCount     int
	beforeCommand func(string)
	failManifest  bool
}

func (r *snapshotFixtureRunner) Run(ctx context.Context, cmd string) (string, error) {
	if r.beforeCommand != nil {
		r.beforeCommand(cmd)
	}
	if strings.HasPrefix(cmd, "systemctl show ") {
		r.showCount++
		return r.properties, nil
	}
	if r.failManifest && strings.Contains(cmd, "base64 -d") {
		return "", errors.New("secret-sentinel from remote stderr")
	}
	return (localBackupRunner{}).Run(ctx, cmd)
}
func snapshotFixture(t *testing.T) (*snapshotFixtureRunner, string, unitSnapshotBinding, []string) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("private host snapshot filesystem tests require root; parser tests run unprivileged")
	}
	root := t.TempDir()
	files := []string{filepath.Join(root, "prom-test.service"), filepath.Join(root, "limit.conf"), filepath.Join(root, "env"), filepath.Join(root, "missing")}
	for i, p := range files[:3] {
		if err := os.WriteFile(p, []byte(fmt.Sprintf("secret-sentinel-%d\n", i)), 0640); err != nil {
			t.Fatal(err)
		}
	}
	runner := &snapshotFixtureRunner{properties: snapshotProperties("prom-test.service", files[0], files[1], files[2]+" (ignore_errors=no) "+files[3]+" (ignore_errors=yes)")}
	binding := unitSnapshotBinding{ComponentID: 42, HostID: 7, Unit: "prom-test.service", RuntimeSHA256: strings.Repeat("a", 64), HostSHA256: strings.Repeat("b", 64)}
	return runner, filepath.Join(root, "snapshots"), binding, files
}

func TestUnitSnapshotDurableCaptureAndVerify(t *testing.T) {
	r, root, b, paths := snapshotFixture(t)
	ref, err := captureUnitSnapshot(context.Background(), r, root, b)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := verifyUnitSnapshot(context.Background(), r, root, ref, b, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Files) != 4 || manifest.Files[3].Present {
		t.Fatal("optional missing file lost")
	}
	for i, f := range manifest.Files[:3] {
		got, err := os.ReadFile(filepath.Join(ref.Path, f.Payload))
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(paths[i])
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) || f.Mode != 0640 || f.UID != 0 {
			t.Fatalf("source content/metadata lost: %v", f)
		}
		st, _ := os.Stat(filepath.Join(ref.Path, f.Payload))
		if st.Mode().Perm() != 0600 {
			t.Fatal("payload not private")
		}
	}
	st, _ := os.Stat(ref.Path)
	if st.Mode().Perm() != 0700 {
		t.Fatal("directory not private")
	}
	st, _ = os.Stat(filepath.Join(ref.Path, "manifest.json"))
	if st.Mode().Perm() != 0600 {
		t.Fatal("manifest not private")
	}
	fresh := &snapshotFixtureRunner{properties: r.properties}
	if _, err := verifyUnitSnapshot(context.Background(), fresh, root, ref, b, false); err != nil {
		t.Fatalf("durable reopen: %v", err)
	}
	if err := os.WriteFile(paths[2], []byte("new-secret"), 0640); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyUnitSnapshot(context.Background(), fresh, root, ref, b, false); err != nil {
		t.Fatal("historical artifact must survive live change", err)
	}
	if _, err := verifyUnitSnapshot(context.Background(), fresh, root, ref, b, true); err == nil {
		t.Fatal("live drift admitted")
	}
}

func TestUnitSnapshotRejectsUnsafeFiles(t *testing.T) {
	for _, name := range []string{"symlink", "hardlink", "directory", "writable", "oversize", "required-missing", "parent-symlink"} {
		t.Run(name, func(t *testing.T) {
			r, root, b, paths := snapshotFixture(t)
			switch name {
			case "symlink":
				os.Remove(paths[2])
				os.Symlink(paths[1], paths[2])
			case "hardlink":
				os.Remove(paths[2])
				os.Link(paths[1], paths[2])
			case "directory":
				os.Remove(paths[2])
				os.Mkdir(paths[2], 0700)
			case "writable":
				os.Chmod(paths[2], 0666)
			case "oversize":
				f, _ := os.OpenFile(paths[2], os.O_WRONLY, 0)
				f.Truncate((1 << 20) + 1)
				f.Close()
			case "required-missing":
				os.Remove(paths[2])
			case "parent-symlink":
				alias := filepath.Join(filepath.Dir(root), "alias")
				os.Symlink(filepath.Dir(root), alias)
				r.properties = strings.Replace(r.properties, paths[2], filepath.Join(alias, "env"), 1)
			}
			ref, err := captureUnitSnapshot(context.Background(), r, root, b)
			if err == nil || ref != nil {
				t.Fatal("unsafe source published")
			}
			if strings.Contains(err.Error(), "secret-sentinel") {
				t.Fatal("secret leaked in failure")
			}
		})
	}
}

func TestUnitSnapshotRejectsCorruptionAndUnsafeReference(t *testing.T) {
	for _, name := range []string{"payload", "manifest", "permissions", "extra-file", "missing-payload", "wrong-component", "wrong-host", "wrong-runtime", "untrusted-path", "pin"} {
		t.Run(name, func(t *testing.T) {
			r, root, b, _ := snapshotFixture(t)
			ref, err := captureUnitSnapshot(context.Background(), r, root, b)
			if err != nil {
				t.Fatal(err)
			}
			m, err := verifyUnitSnapshot(context.Background(), r, root, ref, b, false)
			if err != nil {
				t.Fatal(err)
			}
			switch name {
			case "payload":
				os.WriteFile(filepath.Join(ref.Path, m.Files[0].Payload), []byte("corrupt"), 0600)
			case "manifest":
				os.WriteFile(filepath.Join(ref.Path, "manifest.json"), []byte("{}"), 0600)
			case "permissions":
				os.Chmod(ref.Path, 0755)
			case "extra-file":
				os.WriteFile(filepath.Join(ref.Path, "extra"), []byte("x"), 0600)
			case "missing-payload":
				os.Remove(filepath.Join(ref.Path, m.Files[0].Payload))
			case "wrong-component":
				b.ComponentID++
			case "wrong-host":
				b.HostID++
			case "wrong-runtime":
				b.RuntimeSHA256 = strings.Repeat("c", 64)
			case "untrusted-path":
				ref.Path = filepath.Dir(ref.Path)
			case "pin":
				ref.SHA256 = strings.Repeat("0", 64)
			}
			if _, err := verifyUnitSnapshot(context.Background(), r, root, ref, b, false); err == nil {
				t.Fatal("corrupt/unbound artifact admitted")
			}
		})
	}
}

func TestUnitSnapshotFailureNeverPublishesReference(t *testing.T) {
	for _, name := range []string{"cancel", "manifest-error", "source-race", "state-race"} {
		t.Run(name, func(t *testing.T) {
			r, root, b, paths := snapshotFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch name {
			case "cancel":
				cancel()
			case "manifest-error":
				r.failManifest = true
			case "source-race":
				r.beforeCommand = func(cmd string) {
					if strings.HasPrefix(cmd, "systemctl show ") && r.showCount == 1 {
						os.WriteFile(paths[2], []byte("changed"), 0640)
					}
				}
			case "state-race":
				r.beforeCommand = func(cmd string) {
					if strings.HasPrefix(cmd, "systemctl show ") && r.showCount == 1 {
						r.properties = strings.Replace(r.properties, "MainPID=123", "MainPID=124", 1)
					}
				}
			}
			ref, err := captureUnitSnapshot(ctx, r, root, b)
			if err == nil || ref != nil {
				t.Fatal("failure published trusted reference")
			}
			if strings.Contains(err.Error(), "secret-sentinel") {
				t.Fatal("raw runner output leaked")
			}
		})
	}
}

func TestUnitSnapshotRepeatedEnvironmentFiles(t *testing.T) {
	out := snapshotProperties("prom-test.service", "/etc/systemd/system/prom-test.service", "", "/etc/prom/one (ignore_errors=no)")
	out += "EnvironmentFiles=/etc/prom/two (ignore_errors=yes)\n"
	_, files, err := parseUnitSnapshotProperties("prom-test.service", out)
	if err != nil || len(files) != 3 || files[1].Path != "/etc/prom/one" || files[2].Path != "/etc/prom/two" {
		t.Fatalf("real systemctl repeated environment rows rejected: %v", err)
	}
}

func TestUnitSnapshotGrowingSourceBounded(t *testing.T) {
	r, root, b, paths := snapshotFixture(t)
	grown := false
	r.beforeCommand = func(cmd string) {
		if !grown && strings.Contains(cmd, "\ndst=") {
			grown = true
			f, err := os.OpenFile(paths[0], os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.Truncate(4 << 20); err != nil {
				t.Fatal(err)
			}
			f.Close()
		}
	}
	ref, err := captureUnitSnapshot(context.Background(), r, root, b)
	if err == nil || ref != nil {
		t.Fatal("growing source published")
	}
	if !grown {
		t.Fatal("copy race fixture did not execute")
	}
	err = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() && info.Size() > (1<<20)+1 {
			t.Errorf("unbounded retained file: %d bytes", info.Size())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestUnitSnapshotInventoryLimits(t *testing.T) {
	out := snapshotProperties("prom-test.service", "/etc/systemd/system/prom-test.service", "", "")
	var env strings.Builder
	for i := 0; i < 64; i++ {
		fmt.Fprintf(&env, "EnvironmentFiles=/etc/prom/env%d (ignore_errors=yes)\n", i)
	}
	if _, _, err := parseUnitSnapshotProperties("prom-test.service", strings.Replace(out, "EnvironmentFiles=\n", "", 1)+env.String()); err == nil {
		t.Fatal("65-file inventory admitted")
	}
	for _, state := range []struct{ active, sub, pid string }{{"inactive", "dead", "0"}, {"failed", "failed", "0"}} {
		changed := strings.NewReplacer("ActiveState=active", "ActiveState="+state.active, "SubState=running", "SubState="+state.sub, "MainPID=123", "MainPID="+state.pid).Replace(out)
		if _, _, err := parseUnitSnapshotProperties("prom-test.service", changed); err != nil {
			t.Fatal("stable stopped state rejected", err)
		}
	}
}
func TestUnitSnapshotTotalLimitBeforePublication(t *testing.T) {
	r, root, b, paths := snapshotFixture(t)
	var env strings.Builder
	for i := 0; i < 9; i++ {
		p := filepath.Join(filepath.Dir(paths[0]), fmt.Sprintf("large-%d", i))
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(1 << 20); err != nil {
			t.Fatal(err)
		}
		f.Close()
		fmt.Fprintf(&env, "EnvironmentFiles=%s (ignore_errors=no)\n", p)
	}
	r.properties = snapshotProperties(b.Unit, paths[0], "", "")
	r.properties = strings.Replace(r.properties, "EnvironmentFiles=\n", "", 1) + env.String()
	if ref, err := captureUnitSnapshot(context.Background(), r, root, b); err == nil || ref != nil {
		t.Fatal("oversized total published")
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("unsupported inventory created artifact root")
	}
}
