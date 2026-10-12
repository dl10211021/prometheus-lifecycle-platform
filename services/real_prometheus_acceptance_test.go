//go:build linux

package services

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	realPrometheusOptInEnv  = "PROMETHEUS_REAL_ACCEPTANCE"
	realPrometheusRootEnv   = "PROMETHEUS_REAL_ACCEPTANCE_ROOT"
	realPrometheusNSenv     = "PROMETHEUS_REAL_ACCEPTANCE_NAMESPACE"
	realPrometheus255Env    = "PROMETHEUS_255_ARCHIVE"
	realPrometheus310Env    = "PROMETHEUS_310_ARCHIVE"
	realPrometheusOwnerFile = ".promlifecycle-real-acceptance-owner.json"
	realPrometheusCreatedBy = "TestRealPrometheusMigrationAndRecovery"
	realPrometheus255SHA256 = "7a6b6d5ea003e8d59def294392c64e28338da627bf760cf268e788d6a8832a23"
	realPrometheus310SHA256 = "9a9d1e115d1745826b13aec3f1409780b9fcf1d4206746cb4faee46ca5add70c"
)

var realPrometheusNamespacePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{7,47}$`)
var realPrometheusRunPattern = regexp.MustCompile(`^run-([0-9a-f]{12})$`)

type realPrometheusOwner struct {
	Schema    int    `json:"schema"`
	Namespace string `json:"namespace"`
	Unit      string `json:"unit"`
	RunRoot   string `json:"run_root"`
	CreatedBy string `json:"created_by"`
}

func TestRealPrometheusAcceptanceOwnershipGuard(t *testing.T) {
	root := t.TempDir()
	runRoot := filepath.Join(root, "run-0123456789ab")
	if err := os.Mkdir(runRoot, 0700); err != nil {
		t.Fatal(err)
	}
	owner := realPrometheusOwner{
		Schema: 1, Namespace: "plc-real-acceptance", Unit: "plc-real-acceptance-0123456789ab",
		RunRoot: runRoot, CreatedBy: realPrometheusCreatedBy,
	}
	if err := writeRealPrometheusOwner(runRoot, owner); err != nil {
		t.Fatal(err)
	}
	if err := validateRealPrometheusOwnership(root, runRoot, owner.Namespace, owner.Unit); err != nil {
		t.Fatalf("valid ownership rejected: %v", err)
	}

	tests := []struct {
		name, namespace, unit, child string
	}{
		{"namespace mismatch", "plc-real-other", owner.Unit, runRoot},
		{"unit mismatch", owner.Namespace, "plc-real-acceptance-fedcba987654", runRoot},
		{"unsafe unit", owner.Namespace, "prometheus", runRoot},
		{"non-child root", owner.Namespace, owner.Unit, filepath.Join(runRoot, "nested")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateRealPrometheusOwnership(root, tc.child, tc.namespace, tc.unit); err == nil {
				t.Fatal("unsafe ownership accepted")
			}
		})
	}
}

func TestRealPrometheusAcceptanceOwnerMarkerTamperFailsClosed(t *testing.T) {
	root := t.TempDir()
	runRoot := filepath.Join(root, "run-0123456789ab")
	if err := os.Mkdir(runRoot, 0700); err != nil {
		t.Fatal(err)
	}
	owner := realPrometheusOwner{Schema: 1, Namespace: "plc-real-acceptance", Unit: "plc-real-acceptance-0123456789ab", RunRoot: runRoot, CreatedBy: realPrometheusCreatedBy}
	if err := writeRealPrometheusOwner(runRoot, owner); err != nil {
		t.Fatal(err)
	}
	owner.RunRoot = filepath.Join(root, "somewhere-else")
	if err := writeRealPrometheusOwner(runRoot, owner); err != nil {
		t.Fatal(err)
	}
	if err := validateRealPrometheusOwnership(root, runRoot, owner.Namespace, owner.Unit); err == nil {
		t.Fatal("tampered ownership marker accepted")
	}
}

func newRealPrometheusRunToken() (string, error) {
	var raw [6]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func validateRealPrometheusEnvironmentRoot(root string) (string, error) {
	if os.Geteuid() != 0 {
		return "", errors.New("real Prometheus acceptance requires root")
	}
	if err := validateRealPrometheusRootSyntax(root); err != nil {
		return "", err
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || root == string(filepath.Separator) {
		return "", errors.New("acceptance root must be a clean private absolute path")
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve acceptance root: %w", err)
	}
	if resolved != root {
		return "", errors.New("acceptance root must be canonical and contain no symlink aliases")
	}
	for _, private := range []string{"/tmp", "/var/tmp"} {
		if resolved == private || strings.HasPrefix(resolved, private+string(filepath.Separator)) {
			return "", errors.New("acceptance root must be outside systemd PrivateTmp paths")
		}
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || info.Mode().Perm() != 0700 {
		return "", errors.New("acceptance root must be root-owned mode 0700")
	}
	return resolved, nil
}

func validateRealPrometheusNamespace(namespace string) error {
	if !realPrometheusNamespacePattern.MatchString(namespace) || namespace == "prometheus" || namespace == "promlifecycle" {
		return errors.New("acceptance namespace must be a unique lowercase token of 8-48 characters")
	}
	return nil
}

func writeRealPrometheusOwner(runRoot string, owner realPrometheusOwner) error {
	data, err := json.MarshalIndent(owner, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(filepath.Join(runRoot, realPrometheusOwnerFile), data, 0600)
}

func validateRealPrometheusOwnership(parentRoot, runRoot, namespace, unit string) error {
	if err := validateRealPrometheusNamespace(namespace); err != nil {
		return err
	}
	parent, err := filepath.EvalSymlinks(parentRoot)
	if err != nil {
		return fmt.Errorf("resolve parent root: %w", err)
	}
	run, err := filepath.EvalSymlinks(runRoot)
	if err != nil {
		return fmt.Errorf("resolve run root: %w", err)
	}
	match := realPrometheusRunPattern.FindStringSubmatch(filepath.Base(run))
	if filepath.Dir(run) != parent || len(match) != 2 {
		return errors.New("run root is not a canonical direct owned child")
	}
	wantUnit := namespace + "-" + match[1]
	if unit != wantUnit || !strings.HasPrefix(unit, namespace+"-") {
		return errors.New("unit identity is not tied to the owned run token")
	}
	data, err := os.ReadFile(filepath.Join(run, realPrometheusOwnerFile))
	if err != nil {
		return fmt.Errorf("read ownership marker: %w", err)
	}
	var owner realPrometheusOwner
	if err := json.Unmarshal(data, &owner); err != nil {
		return fmt.Errorf("decode ownership marker: %w", err)
	}
	if owner.Schema != 1 || owner.Namespace != namespace || owner.Unit != unit || owner.RunRoot != run || owner.CreatedBy != realPrometheusCreatedBy {
		return errors.New("ownership marker does not exactly match requested cleanup target")
	}
	return nil
}
func TestRealPrometheusCleanupBoundaryRejectsUncertainStop(t *testing.T) {
	valid := realPrometheusStopObservation{
		InitialMainPID: 101, ActiveState: "inactive", FinalMainPID: 0, TrackedPIDExited: true,
	}
	if err := validateRealPrometheusStopped(valid); err != nil {
		t.Fatalf("valid stopped state rejected: %v", err)
	}
	tests := []struct {
		name        string
		observation realPrometheusStopObservation
	}{
		{"failed PID lookup", realPrometheusStopObservation{InitialMainPIDErr: errors.New("lookup failed")}},
		{"timed-out stop", realPrometheusStopObservation{InitialMainPID: 101, StopErr: context.DeadlineExceeded}},
		{"still-active unit", realPrometheusStopObservation{InitialMainPID: 101, ActiveState: "active", TrackedPIDExited: true}},
		{"nonzero final MainPID", realPrometheusStopObservation{InitialMainPID: 101, ActiveState: "inactive", FinalMainPID: 102, TrackedPIDExited: true}},
		{"still-running tracked PID", realPrometheusStopObservation{InitialMainPID: 101, ActiveState: "inactive", FinalMainPID: 0}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateRealPrometheusStopped(tc.observation); err == nil {
				t.Fatal("uncertain stop state accepted")
			}
		})
	}
}

func TestRealPrometheusAtomicCheckpointPreservesPreviousRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evidence.json")
	if err := os.WriteFile(path, []byte("previous\n"), 0600); err != nil {
		t.Fatal(err)
	}
	renameErr := errors.New("injected rename failure")
	err := writeRealPrometheusAtomicFileWithRename(path, []byte("replacement\n"), 0600, func(string, string) error {
		return renameErr
	})
	if !errors.Is(err, renameErr) {
		t.Fatalf("atomic write error=%v, want injected rename failure", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "previous\n" {
		t.Fatalf("failed checkpoint replaced previous record: %q", got)
	}
	missingParent := filepath.Join(t.TempDir(), "missing", "evidence.json")
	if err := writeRealPrometheusAtomicFile(missingParent, []byte("{}\n"), 0600); err == nil {
		t.Fatal("unwritable checkpoint destination accepted")
	}
}

func TestRealPrometheusEmergencyRestorationEvidence(t *testing.T) {
	restoreErr := errors.New("injected restore failure")
	failed := captureRealPrometheusEmergencyRestoration(
		func() error { return restoreErr },
		func() (string, error) { return "", errors.New("hash must not run") },
		"original",
	)
	if failed["attempted"] != true || failed["restored"] != false || failed["error"] != restoreErr.Error() {
		t.Fatalf("restore failure evidence=%v", failed)
	}

	mismatch := captureRealPrometheusEmergencyRestoration(
		func() error { return nil },
		func() (string, error) { return "different", nil },
		"original",
	)
	if mismatch["restored"] != false || mismatch["restored_sha256"] != "different" || mismatch["error"] == "" {
		t.Fatalf("hash mismatch evidence=%v", mismatch)
	}

	restored := captureRealPrometheusEmergencyRestoration(
		func() error { return nil },
		func() (string, error) { return "original", nil },
		"original",
	)
	if restored["attempted"] != true || restored["restored"] != true || restored["restored_sha256"] != "original" {
		t.Fatalf("successful restoration evidence=%v", restored)
	}
}

func TestRealPrometheusCleanupFinalStatusIsAtomic(t *testing.T) {
	evidencePath := filepath.Join(t.TempDir(), "evidence.json")
	f := &realPrometheusFixture{
		evidencePath: evidencePath,
		evidence: realPrometheusEvidence{
			Schema: 1, Test: realPrometheusCreatedBy, RunID: "0123456789ab", Status: "running",
		},
	}
	cleanup := map[string]any{"runtime_removed": false, "unit_file_absent": true}
	updateRealPrometheusCleanupEvidence(f, cleanup, nil, false, false)
	if err := f.checkpoint(); err != nil {
		t.Fatal(err)
	}

	cleanupErrors := []string{"remove runtime: injected failure"}
	updateRealPrometheusCleanupEvidence(f, cleanup, cleanupErrors, false, true)
	finalWriteErr := errors.New("injected final checkpoint failure")
	err := f.checkpointWithWriter(func(string, []byte, os.FileMode) error { return finalWriteErr })
	if !errors.Is(err, finalWriteErr) {
		t.Fatalf("final checkpoint error=%v, want injected failure", err)
	}

	data, err := os.ReadFile(evidencePath)
	if err != nil {
		t.Fatal(err)
	}
	var durable realPrometheusEvidence
	if err := json.Unmarshal(data, &durable); err != nil {
		t.Fatal(err)
	}
	if durable.Status != "cleanup_pending" {
		t.Fatalf("last durable status=%q, want cleanup_pending", durable.Status)
	}
	if removed, _ := durable.Cleanup["runtime_removed"].(bool); removed {
		t.Fatal("last durable checkpoint claims runtime removal")
	}
}

func TestRealPrometheusCleanupPublicationsShareCompletionTime(t *testing.T) {
	root := t.TempDir()
	logsPath := filepath.Join(root, "logs")
	if err := os.Mkdir(logsPath, 0700); err != nil {
		t.Fatal(err)
	}
	f := &realPrometheusFixture{
		evidencePath: filepath.Join(root, "evidence.json"),
		evidence: realPrometheusEvidence{
			Schema: 1, Test: realPrometheusCreatedBy, RunID: "0123456789ab", Status: "running",
		},
	}
	cleanup := map[string]any{"runtime_removed": true, "unit_file_absent": true}
	updateRealPrometheusCleanupEvidence(f, cleanup, nil, false, false)
	if _, completed := cleanup["completed_at"]; completed {
		t.Error("pre-removal cleanup record claims completion")
	}

	updateRealPrometheusCleanupEvidence(f, cleanup, nil, false, true)
	logData, err := json.MarshalIndent(cleanup, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(logsPath, "cleanup.json")
	if err := writeRealPrometheusAtomicFile(logPath, append(logData, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	updateRealPrometheusCleanupEvidence(f, cleanup, nil, false, true)
	if err := f.checkpoint(); err != nil {
		t.Fatal(err)
	}

	retainedData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	var retained map[string]any
	if err := json.Unmarshal(retainedData, &retained); err != nil {
		t.Fatal(err)
	}
	mainData, err := os.ReadFile(f.evidencePath)
	if err != nil {
		t.Fatal(err)
	}
	var mainEvidence realPrometheusEvidence
	if err := json.Unmarshal(mainData, &mainEvidence); err != nil {
		t.Fatal(err)
	}
	retainedCanonical, _ := json.Marshal(retained)
	mainCanonical, _ := json.Marshal(mainEvidence.Cleanup)
	if string(retainedCanonical) != string(mainCanonical) {
		t.Fatalf("retained cleanup differs from main evidence:\nlog=%s\nmain=%s", retainedCanonical, mainCanonical)
	}
}

func TestRealPrometheusFailedUnitResetIsExact(t *testing.T) {
	unit := "plc-real-acceptance-0123456789ab"
	calls := 0
	if err := resetRealPrometheusFailedUnit("inactive", unit, func(string) error {
		calls++
		return nil
	}); err != nil || calls != 0 {
		t.Fatalf("inactive reset result: calls=%d err=%v", calls, err)
	}
	wantErr := errors.New("injected reset failure")
	err := resetRealPrometheusFailedUnit("failed", unit, func(got string) error {
		calls++
		if got != unit {
			t.Fatalf("reset unit=%q, want %q", got, unit)
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) || calls != 1 {
		t.Fatalf("failed reset result: calls=%d err=%v", calls, err)
	}
}

func TestRealPrometheusProvisionalUnitCleanup(t *testing.T) {
	t.Run("write failure removes exact created fragment", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "owned.service")
		f := &realPrometheusFixture{unitPath: path}
		writeErr := errors.New("injected write failure")
		err := f.writeUnitFileExclusive("partial", func(*os.File, string) (int, error) {
			return 0, writeErr
		}, func(file *os.File) error { return file.Close() })
		if !errors.Is(err, writeErr) {
			t.Fatalf("write error=%v, want injected failure", err)
		}
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("failed write fragment remains: %v", err)
		}
	})

	t.Run("close failure removes exact created fragment", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "owned.service")
		f := &realPrometheusFixture{unitPath: path}
		closeErr := errors.New("injected close failure")
		err := f.writeUnitFileExclusive("complete", func(file *os.File, value string) (int, error) {
			return io.WriteString(file, value)
		}, func(file *os.File) error {
			if err := file.Close(); err != nil {
				return err
			}
			return closeErr
		})
		if !errors.Is(err, closeErr) {
			t.Fatalf("close error=%v, want injected failure", err)
		}
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("failed close fragment remains: %v", err)
		}
	})

	t.Run("replacement is preserved", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "owned.service")
		f := &realPrometheusFixture{unitPath: path}
		writeErr := errors.New("injected write failure after replacement")
		err := f.writeUnitFileExclusive("owned", func(*os.File, string) (int, error) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("foreign"), 0600); err != nil {
				t.Fatal(err)
			}
			return 0, writeErr
		}, func(file *os.File) error { return file.Close() })
		if !errors.Is(err, writeErr) || !strings.Contains(err.Error(), "replaced") {
			t.Fatalf("replacement error=%v", err)
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil || string(data) != "foreign" {
			t.Fatalf("foreign replacement changed: data=%q err=%v", data, readErr)
		}
		if err := f.removeProvisionalUnitFile(); err == nil {
			t.Fatal("foreign replacement accepted for cleanup")
		}
	})
}

func TestRealPrometheusEffectiveIdentityRejectsPrefixMatches(t *testing.T) {
	binary := "/srv/owned/install/prometheus"
	if !realPrometheusEffectiveExecStartMatches("{ path="+binary+" ; argv[]="+binary+" --config.file=/srv/owned/config ; ignore_errors=no ; }", binary) {
		t.Fatal("exact effective ExecStart rejected")
	}
	for _, value := range []string{
		"{ path=/srv/evil/prometheus ; argv[]=/srv/evil/prometheus " + binary + " ; ignore_errors=no ; }",
		"{ path=" + binary + "-other ; argv[]=" + binary + "-other ; ignore_errors=no ; }",
	} {
		if realPrometheusEffectiveExecStartMatches(value, binary) {
			t.Fatalf("prefix/argument ExecStart match accepted: %s", value)
		}
	}
	unit := "plc-real-example-0123456789ab"
	if !realPrometheusPIDInExactUnitCgroup("0::/system.slice/"+unit+".service\n", unit) {
		t.Fatal("exact owned cgroup rejected")
	}
	for _, value := range []string{
		"0::/system.slice/" + unit + ".service.scope\n",
		"0::/system.slice/" + unit + ".service-other\n",
	} {
		if realPrometheusPIDInExactUnitCgroup(value, unit) {
			t.Fatalf("prefix cgroup match accepted: %q", value)
		}
	}
}

func TestRealPrometheusRootSyntaxRejectsUnitInjection(t *testing.T) {
	if err := validateRealPrometheusRootSyntax("/srv/promlifecycle-real/run_01"); err != nil {
		t.Fatalf("safe root syntax rejected: %v", err)
	}
	for _, root := range []string{
		"/srv/prom lifecycle/run",
		"/srv/prom\nExecStart=/bin/false",
		"/srv/prom\troot",
		"/srv/prom;false",
	} {
		if err := validateRealPrometheusRootSyntax(root); err == nil {
			t.Fatalf("unsafe root syntax accepted: %q", root)
		}
	}
}
