package services

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func pairedReferenceFrame(names, properties string) string {
	return "PROMLIFECYCLE_UNITS_V1\n" + names + "\nPROMLIFECYCLE_PROPERTIES_V1\n" + properties + "\nPROMLIFECYCLE_END_V1\n"
}
func pairedReferenceBlock(id, paths string) string {
	return "Id=" + id + "\nNames=" + id + "\nLoadState=loaded\n" + paths
}
func TestPairedRestoreReferencesCompleteInventory(t *testing.T) {
	raw := pairedReferenceFrame("alpha.service\nother.service", pairedReferenceBlock("alpha.service", "FragmentPath=/etc/systemd/system/alpha.service\nEnvironmentFiles=/etc/alpha.env (ignore_errors=yes)\nExecStart={ path=/opt/alpha/prometheus ; argv[]=/opt/alpha/prometheus --config.file=/etc/alpha.yml ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0 }\n")+"\n"+pairedReferenceBlock("other.service", "ExecStartPre={ path=/opt/helper ; argv[]=/opt/helper ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0 }\n"))
	refs, err := parsePairedRestoreUnitReferences(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 || refs[0].ID != "alpha.service" || strings.Join(refs[0].Paths, ",") != "/etc/alpha.env,/etc/systemd/system/alpha.service,/opt/alpha/prometheus" || strings.Join(refs[1].Paths, ",") != "/opt/helper" {
		t.Fatalf("incomplete direct references: %+v", refs)
	}
}
func TestPairedRestoreReferencesRejectIncompleteOrAmbiguous(t *testing.T) {
	valid := pairedReferenceFrame("alpha.service", pairedReferenceBlock("alpha.service", "FragmentPath=/etc/systemd/system/alpha.service\n"))
	cases := map[string]string{
		"missing end":        strings.TrimSuffix(valid, "PROMLIFECYCLE_END_V1\n"),
		"trailing data":      valid + "secret-argv",
		"unreported unit":    strings.Replace(valid, "alpha.service\nPROMLIFECYCLE_PROPERTIES", "alpha.service\nmissing.service\nPROMLIFECYCLE_PROPERTIES", 1),
		"duplicate identity": strings.Replace(valid, "Names=alpha.service", "Id=alpha.service\nNames=alpha.service", 1),
		"missing names":      strings.Replace(valid, "Names=alpha.service\n", "", 1),
		"bad load":           strings.Replace(valid, "LoadState=loaded", "LoadState=not-found", 1),
		"unknown property":   strings.Replace(valid, "LoadState=loaded", "LoadState=loaded\nFutureProperty=unsafe", 1),
		"dynamic path":       strings.Replace(valid, "/etc/systemd/system/alpha.service", "/etc/%i/alpha.service", 1),
		"escaped path":       strings.Replace(valid, "/etc/systemd/system/alpha.service", `/etc/alpha\x20.service`, 1),
		"glob path":          strings.Replace(valid, "/etc/systemd/system/alpha.service", "/etc/*.service", 1),
		"relative path":      strings.Replace(valid, "/etc/systemd/system/alpha.service", "etc/alpha.service", 1),
		"malformed exec":     strings.Replace(valid, "FragmentPath=/etc/systemd/system/alpha.service", "ExecStart=unparsed secret-argv", 1),
		"incomplete env":     strings.Replace(valid, "FragmentPath=/etc/systemd/system/alpha.service", "EnvironmentFiles=/etc/alpha.env", 1),
		"oversized":          strings.Repeat("x", 3<<20+1),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parsePairedRestoreUnitReferences(raw); err == nil || strings.Contains(err.Error(), "secret-argv") {
				t.Fatalf("invalid inventory accepted or leaked: %v", err)
			}
		})
	}
}
func TestPairedRestoreReferencesAliasesAccountForEveryName(t *testing.T) {
	raw := pairedReferenceFrame("alias.service\nreal.service", "Id=real.service\nNames=real.service alias.service\nLoadState=loaded\nFragmentPath=/etc/systemd/system/real.service\n")
	refs, err := parsePairedRestoreUnitReferences(raw)
	if err != nil || len(refs) != 1 || strings.Join(refs[0].Names, ",") != "alias.service,real.service" {
		t.Fatalf("alias identity: %+v %v", refs, err)
	}
}

type pairedReferenceShell struct{ dir string }

func (r pairedReferenceShell) Run(ctx context.Context, command string) (string, error) {
	return localBackupRunner{}.Run(ctx, "export PATH="+r.dir+`:"$PATH"`+"\n"+command)
}
func TestPairedRestoreReferencesCollectorRunsAndResolvesAliases(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "actual")
	if err := os.WriteFile(target, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/bash\nset -eu\ncase \"$1\" in\nlist-unit-files) printf 'alpha.service enabled\\nfuture@.service disabled\\n';;\nlist-units) printf 'alpha.service loaded active running Alpha\\n';;\nshow) printf 'Id=alpha.service\\nNames=alpha.service\\nLoadState=loaded\\nFragmentPath=" + alias + "\\n';;\n*) exit 9;;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	refs, err := readPairedRestoreUnitReferences(context.Background(), pairedReferenceShell{dir})
	if err != nil || len(refs) != 1 || len(refs[0].Paths) != 1 || refs[0].Paths[0] != target {
		t.Fatalf("collector/canonical reference: %+v %v", refs, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte("#!/bin/bash\necho private-stderr >&2\nexit 7\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := readPairedRestoreUnitReferences(context.Background(), pairedReferenceShell{dir}); err == nil || strings.Contains(err.Error(), "private-stderr") {
		t.Fatalf("failed enumeration accepted or leaked: %v", err)
	}
}

func TestPairedRestoreReferencesOneEnumerationFailureCannotDisappear(t *testing.T) {
	for _, failed := range []string{"list-unit-files", "list-units"} {
		t.Run(failed, func(t *testing.T) {
			dir := t.TempDir()
			script := "#!/bin/bash\ncase \"$1\" in\n" + failed + ") exit 7;;\nlist-unit-files|list-units) printf 'alpha.service loaded active running Alpha\\n';;\nshow) printf 'Id=alpha.service\\nNames=alpha.service\\nLoadState=loaded\\n';;\nesac\n"
			if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			if _, err := readPairedRestoreUnitReferences(context.Background(), pairedReferenceShell{dir}); err == nil {
				t.Fatal("incomplete enumeration was accepted")
			}
		})
	}
}

func pairedReferenceExec(path string) string {
	return "{ path=" + path + " ; argv[]=" + path + " ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0 }"
}
func TestPairedRestoreReferencesReviewRegressions(t *testing.T) {
	t.Run("reload and repeated hooks", func(t *testing.T) {
		raw := pairedReferenceFrame("alpha.service", pairedReferenceBlock("alpha.service", "ExecReload="+pairedReferenceExec("/opt/reload")+"\nExecStartPre="+pairedReferenceExec("/opt/pre1")+"\nExecStartPre="+pairedReferenceExec("/opt/pre2")+"\n"))
		refs, err := parsePairedRestoreUnitReferences(raw)
		if err != nil || len(refs) != 1 || strings.Join(refs[0].Paths, ",") != "/opt/pre1,/opt/pre2,/opt/reload" {
			t.Fatalf("complete hook references: %+v %v", refs, err)
		}
	})
	for name, value := range map[string]string{
		"missing fields":               "{ path=/bin/true ;",
		"unfinished extra":             pairedReferenceExec("/bin/true") + " ; { path=/protected/prometheus",
		"argv impersonates executable": "{ path=/bad;path ; argv[]=/bad;path " + pairedReferenceExec("/bin/true"),
		"extra trailing bytes":         pairedReferenceExec("/bin/true") + " garbage",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parsePairedRestoreUnitReferences(pairedReferenceFrame("alpha.service", pairedReferenceBlock("alpha.service", "ExecStart="+value+"\n"))); err == nil {
				t.Fatal("ambiguous executable accepted")
			}
		})
	}
	t.Run("conflicting alias owners", func(t *testing.T) {
		raw := pairedReferenceFrame("alpha.service\nbeta.service\nshared.service", "Id=alpha.service\nNames=alpha.service shared.service\nLoadState=loaded\n\nId=beta.service\nNames=beta.service shared.service\nLoadState=loaded\n")
		if _, err := parsePairedRestoreUnitReferences(raw); err == nil {
			t.Fatal("conflicting alias ownership accepted")
		}
	})
}
func TestPairedRestoreReferencesExactByteBounds(t *testing.T) {
	for _, kind := range []string{"list-unit-files", "list-units", "show"} {
		for _, tail := range []bool{false, true} {
			name := kind
			if tail {
				name += "-omitted-tail"
			}
			t.Run(name, func(t *testing.T) {
				dir := t.TempDir()
				limit := 524288
				raw := "alpha.service loaded active running Alpha\n"
				suffix := "hidden.service loaded active running Hidden\n"
				if kind == "show" {
					limit = 2097152
					raw = "Id=alpha.service\nNames=alpha.service\nLoadState=loaded\n"
					suffix = "ExecStartPost=" + pairedReferenceExec("/protected/prometheus") + "\n"
				}
				// A newline is precisely the max+1 byte; command substitution must not
				// erase that byte before the cap is enforced.
				payload := raw + strings.Repeat("\n", limit+1-len(raw))
				if tail {
					payload += suffix
				}
				if err := os.WriteFile(filepath.Join(dir, "payload"), []byte(payload), 0600); err != nil {
					t.Fatal(err)
				}
				script := "#!/bin/bash\ncase \"$1\" in\n" + kind + ") cat '" + filepath.Join(dir, "payload") + "';;\nlist-unit-files|list-units) printf 'alpha.service loaded active running Alpha\\n';;\nshow) printf 'Id=alpha.service\\nNames=alpha.service\\nLoadState=loaded\\n';;\nesac\n"
				if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
				if _, err := readPairedRestoreUnitReferences(context.Background(), pairedReferenceShell{dir}); err == nil {
					t.Fatal("oversized inventory accepted")
				}
			})
		}
	}
}

func TestPairedRestoreReferencesAbsentDependencyIdentity(t *testing.T) {
	absent := "Id=absent.service\nNames=absent.service\nLoadState=not-found\nActiveState=inactive\nSubState=dead\nMainPID=0\nControlPID=0\n"
	refs, err := parsePairedRestoreUnitReferences(pairedReferenceFrame("absent.service", absent))
	if err != nil || len(refs) != 1 || len(refs[0].Paths) != 0 {
		t.Fatalf("empty absent dependency rejected: %+v %v", refs, err)
	}
	for _, bad := range []string{
		strings.Replace(absent, "MainPID=0", "MainPID=1", 1),
		strings.Replace(absent, "ControlPID=0", "ControlPID=1", 1),
		strings.Replace(absent, "ActiveState=inactive", "ActiveState=active", 1),
		strings.Replace(absent, "SubState=dead", "SubState=failed", 1),
		strings.Replace(absent, "MainPID=0\n", "", 1),
		absent + "FragmentPath=/protected/prometheus\n",
		absent + "ExecReload=" + pairedReferenceExec("/protected/prometheus") + "\n",
	} {
		if _, err := parsePairedRestoreUnitReferences(pairedReferenceFrame("absent.service", bad)); err == nil {
			t.Fatal("inconsistent absent dependency accepted")
		}
	}
}

func TestPairedRestoreReferencesAliasLifecycleKeepsCompleteOwnership(t *testing.T) {
	first := "Id=real.service\nNames=real.service\nLoadState=loaded\nFragmentPath=/etc/systemd/system/real.service\n"
	second := "Id=real.service\nNames=real.service alias.service\nLoadState=loaded\nFragmentPath=/etc/systemd/system/real.service\n"
	for _, props := range []string{first + "\n" + second, second + "\n" + first} {
		refs, err := parsePairedRestoreUnitReferences(pairedReferenceFrame("real.service\nalias.service", props))
		if err != nil || len(refs) != 1 || strings.Join(refs[0].Names, ",") != "alias.service,real.service" {
			t.Fatalf("alias lifecycle lost ownership: %+v %v", refs, err)
		}
	}
	if _, err := parsePairedRestoreUnitReferences(pairedReferenceFrame("real.service\nalias.service", first+"\n"+strings.Replace(second, "/etc/systemd/system/real.service", "/etc/systemd/system/replaced.service", 1))); err == nil {
		t.Fatal("alias path drift accepted")
	}
}

func TestPairedRestoreReferencesTemplateFragmentIsLiteral(t *testing.T) {
	refs, err := parsePairedRestoreUnitReferences(pairedReferenceFrame("getty@tty1.service", pairedReferenceBlock("getty@tty1.service", "FragmentPath=/usr/lib/systemd/system/getty@.service\n")))
	if err != nil || len(refs) != 1 || strings.Join(refs[0].Paths, ",") != "/usr/lib/systemd/system/getty@.service" {
		t.Fatalf("literal template fragment rejected: %+v %v", refs, err)
	}
}

func TestPairedRestoreReferencesResolveDotSegmentsAfterSymlinks(t *testing.T) {
	dir := t.TempDir()
	actual := filepath.Join(dir, "actual")
	if err := os.MkdirAll(filepath.Join(actual, "child"), 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(actual, "target")
	if err := os.WriteFile(target, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(filepath.Join(actual, "child"), alias); err != nil {
		t.Fatal(err)
	}
	rawPath := alias + "/../target" // filepath.Clean would incorrectly point to dir/target.
	script := "#!/bin/bash\ncase \"$1\" in\nlist-unit-files|list-units) printf 'alpha.service loaded active running Alpha\\n';;\nshow) printf 'Id=alpha.service\\nNames=alpha.service\\nLoadState=loaded\\nFragmentPath=" + rawPath + "\\n';;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	refs, err := readPairedRestoreUnitReferences(context.Background(), pairedReferenceShell{dir})
	if err != nil || len(refs) != 1 || strings.Join(refs[0].Paths, ",") != target {
		t.Fatalf("host path semantics lost: %+v %v", refs, err)
	}
}

func TestPairedRestoreReferencesRejectFilesystemRemapping(t *testing.T) {
	// A service-local absolute path is not necessarily a host absolute path.
	for _, key := range []string{"RootDirectory", "RootImage", "BindPaths", "BindReadOnlyPaths", "TemporaryFileSystem", "MountImages", "ExtensionImages", "ExtensionDirectories"} {
		t.Run(key, func(t *testing.T) {
			base := pairedReferenceBlock("other.service", "ExecStart="+pairedReferenceExec("/prometheus")+"\n")
			if _, err := parsePairedRestoreUnitReferences(pairedReferenceFrame("other.service", base+key+"=\n")); err != nil {
				t.Fatalf("empty namespace property rejected: %v", err)
			}
			if _, err := parsePairedRestoreUnitReferences(pairedReferenceFrame("other.service", base+key+"=/private/remap\n")); err == nil {
				t.Fatal("remapped executable treated as host path")
			}
		})
	}
}

func TestPairedRestoreReferencesCollectorRequestsFilesystemRemapping(t *testing.T) {
	for _, key := range []string{"RootDirectory", "RootImage", "BindPaths", "BindReadOnlyPaths", "TemporaryFileSystem", "MountImages", "ExtensionImages", "ExtensionDirectories"} {
		t.Run(key, func(t *testing.T) {
			dir := t.TempDir()
			script := "#!/bin/bash\ncase \"$1\" in\nlist-unit-files|list-units) printf 'other.service loaded active running Other\\n';;\nshow) printf 'Id=other.service\\nNames=other.service\\nLoadState=loaded\\n'; if [[ \"$*\" == *" + key + "* ]]; then printf '" + key + "=/private/remap\\n'; fi;;\nesac\n"
			if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			if _, err := readPairedRestoreUnitReferences(context.Background(), pairedReferenceShell{dir}); err == nil {
				t.Fatal("collector omitted filesystem remapping property")
			}
		})
	}
}

func TestPairedRestoreReferencesLiteralExecutableWithEnvironmentArgv(t *testing.T) {
	exec := strings.Replace(pairedReferenceExec("/usr/bin/postgres"), "argv[]=/usr/bin/postgres ;", "argv[]=/usr/bin/postgres -D ${PGDATA} ${EXTRA_OPTIONS} ;", 1)
	refs, err := parsePairedRestoreUnitReferences(pairedReferenceFrame("other.service", pairedReferenceBlock("other.service", "ExecStart="+exec+"\n")))
	if err != nil || len(refs) != 1 || strings.Join(refs[0].Paths, ",") != "/usr/bin/postgres" {
		t.Fatalf("literal executable lost behind argv variables: %+v %v", refs, err)
	}
}

func TestPairedRestoreReferencesVerifyUnprintableArraysWithTypedValues(t *testing.T) {
	for _, mode := range []string{"empty", "nonempty", "failed", "truncated", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			systemctl := "#!/bin/bash\ncase \"$1\" in\nlist-unit-files|list-units) printf 'alpha.service loaded active running Alpha\\n';;\nshow) printf 'Id=alpha.service\\nNames=alpha.service\\nLoadState=loaded\\nBindPaths=[unprintable]\\nBindReadOnlyPaths=[unprintable]\\nTemporaryFileSystem=[unprintable]\\n';;\nesac\n"
			bus := "#!/bin/bash\n[[ \"$*\" == *BindPaths* && \"$*\" == *BindReadOnlyPaths* && \"$*\" == *TemporaryFileSystem* ]] || exit 9\n"
			switch mode {
			case "empty":
				bus += "printf 'a(ssbt) 0\\na(ssbt) 0\\na(ss) 0\\n'\n"
			case "nonempty":
				bus += "printf 'a(ssbt) 1 \"/shared\" \"/prometheus\" false 0\\na(ssbt) 0\\na(ss) 0\\n'\n"
			case "failed":
				bus += "echo private-bus-output >&2\nexit 7\n"
			case "truncated":
				bus += "printf 'a(ssbt) 0\\na(ssbt) 0\\n'\n"
			case "oversized":
				bus += "printf '%4096s\\n' ''\nprintf 'a(ssbt) 0\\na(ssbt) 0\\na(ss) 0\\n'\n"
			}
			for name, script := range map[string]string{"systemctl": systemctl, "busctl": bus} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
			}
			refs, err := readPairedRestoreUnitReferences(context.Background(), pairedReferenceShell{dir})
			if mode == "empty" {
				if err != nil || len(refs) != 1 {
					t.Fatalf("typed empty mapping rejected: %+v %v", refs, err)
				}
			} else if err == nil || strings.Contains(err.Error(), "private-bus-output") {
				t.Fatalf("unproven mapping admitted or leaked: %v", err)
			}
		})
	}
}
