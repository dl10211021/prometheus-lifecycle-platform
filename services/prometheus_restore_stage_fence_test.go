package services

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func stageFenceFixture(t *testing.T) restoreStageFence {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("root-owned host fence requires root")
	}
	base, err := os.MkdirTemp("/root", "stage-fence-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	return restoreStageFence{BackupRoot: base, Root: filepath.Join(base, "stage"), Generation: strings.Repeat("a", 64)}
}
func stageFenceInit(t *testing.T) restoreStageFence {
	t.Helper()
	f, e := initializeRestoreStageFence(context.Background(), localBackupRunner{}, stageFenceFixture(t))
	if e != nil {
		t.Fatal(e)
	}
	return f
}
func stageFenceWrite(t *testing.T, path, value string) {
	t.Helper()
	if e := os.WriteFile(path, []byte(value), 0600); e != nil {
		t.Fatal(e)
	}
}
func stageFenceWait(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, e := os.Stat(path); e == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("handshake not reached")
}
func TestRestoreStageFenceInitializeRotateAndStaleWriter(t *testing.T) {
	ctx := context.Background()
	f := stageFenceInit(t)
	if f.RootIdentity == "" || f.LockIdentity == "" {
		t.Fatal("identities missing")
	}
	if e := verifyRestoreStageFence(ctx, localBackupRunner{}, f); e != nil {
		t.Fatal(e)
	}
	rootInfo, _ := os.Stat(f.Root)
	lockInfo, _ := os.Stat(f.Root + "/lock")
	next, e := rotateRestoreStageFence(ctx, localBackupRunner{}, f, strings.Repeat("b", 64))
	if e != nil {
		t.Fatal(e)
	}
	if next.RootIdentity != f.RootIdentity || next.LockIdentity != f.LockIdentity {
		t.Fatal("permanent identities changed")
	}
	// A publication failure retries the exact saved old -> desired intent.
	if _, e = rotateRestoreStageFence(ctx, localBackupRunner{}, f, next.Generation); e != nil {
		t.Fatal(e)
	}
	if _, e = initializeRestoreStageFence(ctx, localBackupRunner{}, restoreStageFence{BackupRoot: f.BackupRoot, Root: f.Root, Generation: f.Generation}); e == nil {
		t.Fatal("late initializer reset generation")
	}
	if e = runRestoreStageFenced(ctx, localBackupRunner{}, f, "touch "+f.Root+"/stale"); e == nil {
		t.Fatal("stale writer accepted")
	}
	if _, e = os.Stat(f.Root + "/stale"); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("stale command executed")
	}
	third, e := rotateRestoreStageFence(ctx, localBackupRunner{}, next, strings.Repeat("c", 64))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = rotateRestoreStageFence(ctx, localBackupRunner{}, f, next.Generation); e == nil {
		t.Fatal("reordered rotation accepted")
	}
	if e = verifyRestoreStageFence(ctx, localBackupRunner{}, third); e != nil {
		t.Fatal(e)
	}
	rootAfter, _ := os.Stat(f.Root)
	lockAfter, _ := os.Stat(f.Root + "/lock")
	if !os.SameFile(rootInfo, rootAfter) || !os.SameFile(lockInfo, lockAfter) {
		t.Fatal("root or lock replaced")
	}
}
func TestRestoreStageFenceInitializationMatrix(t *testing.T) {
	for _, state := range []string{"missing", "root", "lock", "generation", "temporary"} {
		t.Run(state, func(t *testing.T) {
			f := stageFenceFixture(t)
			if state != "missing" {
				if e := os.Mkdir(f.Root, 0700); e != nil {
					t.Fatal(e)
				}
			}
			if state == "lock" || state == "generation" || state == "temporary" {
				stageFenceWrite(t, f.Root+"/lock", "")
			}
			if state == "generation" {
				stageFenceWrite(t, f.Root+"/generation", f.Generation+"\n")
			}
			if state == "temporary" {
				stageFenceWrite(t, f.Root+"/.generation.ABC123xyz0", "partial")
			}
			got, e := initializeRestoreStageFence(context.Background(), localBackupRunner{}, f)
			if e != nil {
				t.Fatal(e)
			}
			if e = verifyRestoreStageFence(context.Background(), localBackupRunner{}, got); e != nil {
				t.Fatal(e)
			}
			if state == "temporary" {
				b, e := os.ReadFile(f.Root + "/.generation.ABC123xyz0")
				if e != nil || string(b) != "partial" {
					t.Fatal("forensic temp changed")
				}
			}
		})
	}
	for _, state := range []string{"generation-no-lock", "unknown-entry", "wrong-generation", "symlink-root", "unsafe-root", "symlink-lock", "hardlink-lock"} {
		t.Run(state, func(t *testing.T) {
			f := stageFenceFixture(t)
			if e := os.Mkdir(f.Root, 0700); e != nil {
				t.Fatal(e)
			}
			switch state {
			case "generation-no-lock":
				stageFenceWrite(t, f.Root+"/generation", f.Generation+"\n")
			case "unknown-entry":
				stageFenceWrite(t, f.Root+"/evidence", "keep")
			case "wrong-generation":
				stageFenceWrite(t, f.Root+"/lock", "")
				stageFenceWrite(t, f.Root+"/generation", strings.Repeat("b", 64)+"\n")
			case "symlink-root":
				os.Rename(f.Root, f.Root+"-real")
				os.Symlink(f.Root+"-real", f.Root)
			case "unsafe-root":
				os.Chmod(f.Root, 0777)
			case "symlink-lock":
				stageFenceWrite(t, f.Root+"/target", "keep")
				os.Symlink(f.Root+"/target", f.Root+"/lock")
			case "hardlink-lock":
				stageFenceWrite(t, f.Root+"/lock", "keep")
				os.Link(f.Root+"/lock", f.BackupRoot+"/other")
			}
			if _, e := initializeRestoreStageFence(context.Background(), localBackupRunner{}, f); e == nil {
				t.Fatal("unsafe bootstrap accepted")
			}
			if state == "unknown-entry" {
				b, _ := os.ReadFile(f.Root + "/evidence")
				if string(b) != "keep" {
					t.Fatal("evidence altered")
				}
			}
		})
	}
}
func TestRestoreStageFencePinnedCorruptionNeverRepairs(t *testing.T) {
	for _, state := range []string{"missing-root", "missing-lock", "missing-generation", "wrong-root", "wrong-lock", "wrong-token", "symlink-generation", "hardlink-generation", "unsafe-generation", "incomplete-generation", "extra-generation-line", "unsafe-parent", "noncanonical"} {
		t.Run(state, func(t *testing.T) {
			f := stageFenceInit(t)
			switch state {
			case "missing-root":
				os.Rename(f.Root, f.Root+"-saved")
			case "missing-lock":
				os.Rename(f.Root+"/lock", f.BackupRoot+"/saved-lock")
			case "missing-generation":
				os.Rename(f.Root+"/generation", f.BackupRoot+"/saved-generation")
			case "wrong-root":
				os.Rename(f.Root, f.Root+"-saved")
				os.Mkdir(f.Root, 0700)
				stageFenceWrite(t, f.Root+"/lock", "")
				stageFenceWrite(t, f.Root+"/generation", f.Generation+"\n")
			case "wrong-lock":
				os.Rename(f.Root+"/lock", f.BackupRoot+"/saved-lock")
				stageFenceWrite(t, f.Root+"/lock", "")
			case "wrong-token":
				f.Generation = strings.Repeat("b", 64)
			case "symlink-generation":
				os.Rename(f.Root+"/generation", f.BackupRoot+"/saved-generation")
				os.Symlink(f.BackupRoot+"/saved-generation", f.Root+"/generation")
			case "hardlink-generation":
				os.Link(f.Root+"/generation", f.BackupRoot+"/linked-generation")
			case "unsafe-generation":
				os.Chmod(f.Root+"/generation", 0644)
			case "incomplete-generation":
				stageFenceWrite(t, f.Root+"/generation", f.Generation)
			case "extra-generation-line":
				stageFenceWrite(t, f.Root+"/generation", f.Generation+"\n\n")
			case "unsafe-parent":
				os.Chmod(f.BackupRoot, 0777)
			case "noncanonical":
				f.Root = f.BackupRoot + "/../" + filepath.Base(f.BackupRoot) + "/stage"
			}
			if e := verifyRestoreStageFence(context.Background(), localBackupRunner{}, f); e == nil {
				t.Fatal("corruption accepted by verify")
			}
			if _, e := initializeRestoreStageFence(context.Background(), localBackupRunner{}, f); e == nil {
				t.Fatal("pinned initializer repaired corruption")
			}
			if _, e := rotateRestoreStageFence(context.Background(), localBackupRunner{}, f, strings.Repeat("c", 64)); e == nil {
				t.Fatal("pinned rotation repaired corruption")
			}
			if e := runRestoreStageFenced(context.Background(), localBackupRunner{}, f, "true"); e == nil {
				t.Fatal("corrupt writer accepted")
			}
			if state == "missing-lock" {
				if _, e := os.Lstat(f.Root + "/lock"); !errors.Is(e, os.ErrNotExist) {
					t.Fatal("missing lock recreated")
				}
			}
			if state == "missing-generation" {
				if _, e := os.Lstat(f.Root + "/generation"); !errors.Is(e, os.ErrNotExist) {
					t.Fatal("missing generation recreated")
				}
			}
		})
	}
}
func TestRestoreStageFenceBusyForegroundAndCancellation(t *testing.T) {
	f := stageFenceInit(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := f.BackupRoot + "/ready"
	release := f.BackupRoot + "/release"
	done := make(chan error, 1)
	go func() {
		done <- runRestoreStageFenced(ctx, localBackupRunner{}, f, "touch "+ready+"; while [ ! -e "+release+" ]; do sleep .01; done; touch "+f.BackupRoot+"/done")
	}()
	stageFenceWait(t, ready)
	if _, e := rotateRestoreStageFence(context.Background(), localBackupRunner{}, f, strings.Repeat("b", 64)); e == nil {
		t.Fatal("rotation ignored busy writer")
	}
	cancel()
	select {
	case e := <-done:
		if !errors.Is(e, context.Canceled) {
			t.Fatalf("lost cancellation: %v", e)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("writer did not cancel")
	}
	if _, e := rotateRestoreStageFence(context.Background(), localBackupRunner{}, f, strings.Repeat("b", 64)); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(f.BackupRoot + "/done"); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("cancelled writer finished")
	}
}
func TestRestoreStageFenceChildRetainsLockAfterParentExit(t *testing.T) {
	if os.Getenv(stageFenceChildHelperMode) == "" {
		stageFenceRunChildFixtureProcess(t, "normal")
		return
	}
	// This isolated process owns and can reap the deliberately orphaned child.
	if _, _, errno := syscall.Syscall6(syscall.SYS_PRCTL, 36, 1, 0, 0, 0, 0); errno != 0 {
		t.Fatal(errno)
	}
	f := stageFenceInit(t)
	ready := f.BackupRoot + "/ready"
	release := f.BackupRoot + "/release"
	done := f.BackupRoot + "/done"
	// The shell exits without waiting, simulating a crashed host controller.
	// Its child's inherited lock must still prevent generation rotation.
	pidPath := f.BackupRoot + "/child.pid"
	stageFenceRegisterChildCleanup(t, release, pidPath)
	condition := "[ ! -e " + release + " ]"
	if os.Getenv(stageFenceChildHelperMode) == "fail-ignore-release" {
		condition = ":"
	}
	command := "( touch " + ready + "; while " + condition + "; do sleep .01; done; touch " + done + " ) >/dev/null 2>&1 & child=$!; printf '%s\n' \"$child\" >" + pidPath + "; exit 1"
	if e := runRestoreStageFenced(context.Background(), localBackupRunner{}, f, command); e == nil {
		t.Fatal("failed foreground accepted")
	}
	data, e := os.ReadFile(pidPath)
	if e != nil {
		t.Fatal(e)
	}
	pid, e := strconv.Atoi(strings.TrimSpace(string(data)))
	if e != nil {
		t.Fatal(e)
	}
	start, e := stageFenceProcessStartTime(pid)
	if e != nil {
		t.Fatal(e)
	}
	record, e := json.Marshal(stageFenceChildRecord{PID: pid, Root: f.BackupRoot, StartTime: start})
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(os.Getenv(stageFenceChildHelperRecord), record, 0600); e != nil {
		t.Fatal(e)
	}
	if os.Getenv(stageFenceChildHelperMode) == "fail-before-ready" {
		t.Fatal("forced early fixture failure")
	}
	stageFenceWait(t, ready)
	if os.Getenv(stageFenceChildHelperMode) != "normal" {
		t.Fatal("forced early fixture failure")
	}
	if _, e := rotateRestoreStageFence(context.Background(), localBackupRunner{}, f, strings.Repeat("b", 64)); !errors.Is(e, errRestoreStageFenceBusy) {
		t.Fatalf("child lost inherited lock: %v", e)
	}
	stageFenceWrite(t, release, "")
	stageFenceWait(t, done)
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, e := rotateRestoreStageFence(context.Background(), localBackupRunner{}, f, strings.Repeat("b", 64)); e == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child lock never released")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
func TestRestoreStageFenceReadOnlyAndRedaction(t *testing.T) {
	f := stageFenceInit(t)
	before, _ := os.Stat(f.Root + "/generation")
	rootBefore, _ := os.Stat(f.Root)
	if e := verifyRestoreStageFence(context.Background(), localBackupRunner{}, f); e != nil {
		t.Fatal(e)
	}
	after, _ := os.Stat(f.Root + "/generation")
	rootAfter, _ := os.Stat(f.Root)
	if before.ModTime() != after.ModTime() || rootBefore.ModTime() != rootAfter.ModTime() {
		t.Fatal("verification wrote filesystem")
	}
	secret := "private-credential-should-not-leak"
	if e := runRestoreStageFenced(context.Background(), localBackupRunner{}, f, "echo "+secret+"; echo "+secret+" >&2; exit 1"); e == nil || strings.Contains(e.Error(), secret) || strings.Contains(e.Error(), f.Generation) {
		t.Fatalf("unsafe error %v", e)
	}
}
func TestRestoreStageFenceExternalBusyLock(t *testing.T) {
	f := stageFenceInit(t)
	file, e := os.OpenFile(f.Root+"/lock", os.O_RDONLY, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer file.Close()
	if e = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		t.Fatal(e)
	}
	if e = verifyRestoreStageFence(context.Background(), localBackupRunner{}, f); e == nil {
		t.Fatal("busy verify accepted")
	}
}

type stageFencePathRunner struct{ path string }

func (r stageFencePathRunner) Run(ctx context.Context, command string) (string, error) {
	return (localBackupRunner{}).Run(ctx, "PATH="+r.path+":\"$PATH\" "+command)
}

// Interpose a real executable boundary, wait for a filesystem handshake, then
// cancel the actual process tree. No production hooks or source assertions.
func stageFenceCrashRunner(t *testing.T, f restoreStageFence, utility, phase, match string) (hostCommandRunner, string) {
	t.Helper()
	bin := f.BackupRoot + "/bin"
	if e := os.Mkdir(bin, 0700); e != nil {
		t.Fatal(e)
	}
	real, e := exec.LookPath(utility)
	if e != nil {
		t.Fatal(e)
	}
	ready := f.BackupRoot + "/crash-ready"
	script := "#!/bin/bash\nset -eu\nlast=\"${!#}\"\ngate() { touch " + ready + "; while :; do /bin/sleep .01; done; }\n"
	script += "match=0\ncase \"$last\" in " + match + ") match=1;; esac\n"
	if phase == "before" {
		script += "if [ \"$match\" = 1 ]; then gate; fi\n"
	}
	script += "result=$(" + real + " \"$@\")\n"
	if phase == "after" {
		script += "if [ \"$match\" = 1 ]; then gate; fi\n"
	}
	script += "printf '%s' \"$result\"\n"
	if e = os.WriteFile(bin+"/"+utility, []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	return stageFencePathRunner{path: bin}, ready
}
func TestRestoreStageFenceInitializationCrashBoundaries(t *testing.T) {
	for _, boundary := range []struct{ name, utility, match string }{
		{"root", "mkdir", "*/stage"}, {"lock", "flock", "9"}, {"temporary", "mktemp", "*/.generation.*"},
		{"temporary-sync", "sync", "*/.generation.*"}, {"publish", "mv", "*/generation"}, {"generation-sync", "sync", "*/generation"},
	} {
		for _, phase := range []string{"before", "after"} {
			t.Run(boundary.name+"-"+phase, func(t *testing.T) {
				f := stageFenceFixture(t)
				r, ready := stageFenceCrashRunner(t, f, boundary.utility, phase, boundary.match)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				finished := make(chan error, 1)
				go func() { _, e := initializeRestoreStageFence(ctx, r, f); finished <- e }()
				stageFenceWait(t, ready)
				var rootBefore, lockBefore os.FileInfo
				rootBefore, _ = os.Stat(f.Root)
				lockBefore, _ = os.Stat(f.Root + "/lock")
				cancel()
				select {
				case e := <-finished:
					if !errors.Is(e, context.Canceled) {
						t.Fatalf("lost cancellation: %v", e)
					}
				case <-time.After(7 * time.Second):
					t.Fatal("crashed initializer did not return")
				}
				temps, _ := filepath.Glob(f.Root + "/.generation.*")
				got, e := initializeRestoreStageFence(context.Background(), localBackupRunner{}, f)
				if e != nil {
					t.Fatal(e)
				}
				rootAfter, _ := os.Stat(f.Root)
				lockAfter, _ := os.Stat(f.Root + "/lock")
				if rootBefore != nil && !os.SameFile(rootBefore, rootAfter) {
					t.Fatal("bootstrap replaced root")
				}
				if lockBefore != nil && !os.SameFile(lockBefore, lockAfter) {
					t.Fatal("bootstrap replaced lock")
				}
				for _, temp := range temps {
					if _, e := os.Lstat(temp); e != nil {
						t.Fatal("bootstrap removed forensic temporary")
					}
				}
				if e = verifyRestoreStageFence(context.Background(), localBackupRunner{}, got); e != nil {
					t.Fatal(e)
				}
			})
		}
	}
}
func TestRestoreStageFenceRotationCrashAfterPublication(t *testing.T) {
	f := stageFenceInit(t)
	r, ready := stageFenceCrashRunner(t, f, "mv", "after", "*/generation")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	desired := strings.Repeat("b", 64)
	go func() { _, e := rotateRestoreStageFence(ctx, r, f, desired); finished <- e }()
	stageFenceWait(t, ready)
	bytes, e := os.ReadFile(f.Root + "/generation")
	if e != nil || string(bytes) != desired+"\n" {
		t.Fatal("generation was not switched at handshake")
	}
	cancel()
	if e = <-finished; !errors.Is(e, context.Canceled) {
		t.Fatalf("unexpected crash result %v", e)
	}
	// Controller state still holds old, desired. Reuse exactly that intent.
	got, e := rotateRestoreStageFence(context.Background(), localBackupRunner{}, f, desired)
	if e != nil {
		t.Fatal(e)
	}
	if e = verifyRestoreStageFence(context.Background(), localBackupRunner{}, got); e != nil {
		t.Fatal(e)
	}
	if e = runRestoreStageFenced(context.Background(), localBackupRunner{}, f, "touch "+f.BackupRoot+"/stale"); e == nil {
		t.Fatal("old writer survived switch")
	}
}
func TestRestoreStageFenceRacingInitializersKeepOneLock(t *testing.T) {
	f := stageFenceFixture(t)
	start := make(chan struct{})
	results := make(chan struct {
		f restoreStageFence
		e error
	}, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			got, e := initializeRestoreStageFence(context.Background(), localBackupRunner{}, f)
			results <- struct {
				f restoreStageFence
				e error
			}{got, e}
		}()
	}
	close(start)
	var successful restoreStageFence
	for i := 0; i < 2; i++ {
		result := <-results
		if result.e == nil {
			if successful.LockIdentity != "" && successful.LockIdentity != result.f.LockIdentity {
				t.Fatal("racing initializer replaced lock")
			}
			successful = result.f
		}
	}
	if successful.LockIdentity == "" {
		t.Fatal("neither initializer completed")
	}
	retry, e := initializeRestoreStageFence(context.Background(), localBackupRunner{}, f)
	if e != nil || retry.LockIdentity != successful.LockIdentity {
		t.Fatal("retry did not converge")
	}
}
func TestRestoreStageFenceDelayedCommandChecksGenerationAfterLock(t *testing.T) {
	f := stageFenceInit(t)
	r, ready := stageFenceCrashRunner(t, f, "flock", "before", "9")
	// Replace the crashing gate with a release handshake in this test fixture.
	wrapper := f.BackupRoot + "/bin/flock"
	script, e := os.ReadFile(wrapper)
	if e != nil {
		t.Fatal(e)
	}
	release := f.BackupRoot + "/release"
	script = []byte(strings.ReplaceAll(string(script), "while :; do", "while [ ! -e "+release+" ]; do"))
	if e = os.WriteFile(wrapper, script, 0700); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- runRestoreStageFenced(ctx, r, f, "touch "+f.BackupRoot+"/stale") }()
	stageFenceWait(t, ready)
	if _, e = rotateRestoreStageFence(context.Background(), localBackupRunner{}, f, strings.Repeat("b", 64)); e != nil {
		t.Fatal(e)
	}
	stageFenceWrite(t, release, "")
	if e = <-finished; e == nil {
		t.Fatal("delayed stale command ran")
	}
	if _, e = os.Stat(f.BackupRoot + "/stale"); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("stale marker exists")
	}
}
func TestRestoreStageFenceGenerationDirectoriesAndInvalidFrames(t *testing.T) {
	f := stageFenceInit(t)
	generationRoot := f.Root + "/g-" + f.Generation
	if e := os.Mkdir(generationRoot, 0700); e != nil {
		t.Fatal(e)
	}
	stageFenceWrite(t, generationRoot+"/partial", "forensic")
	if e := verifyRestoreStageFence(context.Background(), localBackupRunner{}, f); e != nil {
		t.Fatal(e)
	}
	if e := runRestoreStageFenced(context.Background(), localBackupRunner{}, f, "printf finished >"+generationRoot+"/copied"); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(generationRoot + "/copied")
	if e != nil || string(b) != "finished" {
		t.Fatal("valid fenced writer did not run")
	}
	unpinned := f
	unpinned.RootIdentity = ""
	unpinned.LockIdentity = ""
	if _, e = initializeRestoreStageFence(context.Background(), localBackupRunner{}, unpinned); e == nil {
		t.Fatal("bootstrap adopted payloads")
	}
	for _, mode := range []string{"truncated", "extra", "wrong-inode", "wrong-generation"} {
		t.Run(mode, func(t *testing.T) {
			if e := verifyRestoreStageFence(context.Background(), stageFenceFrameRunner{mode}, f); e == nil {
				t.Fatal("invalid response frame accepted")
			}
		})
	}
}

type stageFenceFrameRunner struct{ mode string }

func (r stageFenceFrameRunner) Run(ctx context.Context, command string) (string, error) {
	out, e := (localBackupRunner{}).Run(ctx, command)
	if e != nil {
		return out, e
	}
	switch r.mode {
	case "truncated":
		return strings.TrimSuffix(out, "\n"), nil
	case "extra":
		return out + "unexpected\n", nil
	case "wrong-inode":
		parts := strings.Split(out, "\n")
		parts[2] = "0:1"
		return strings.Join(parts, "\n"), nil
	case "wrong-generation":
		parts := strings.Split(out, "\n")
		parts[3] = strings.Repeat("f", 64)
		return strings.Join(parts, "\n"), nil
	}
	return out, nil
}

func TestRestoreStageFenceRechecksAncestorAfterLockAcquisition(t *testing.T) {
	f := stageFenceInit(t)
	r, ready := stageFenceCrashRunner(t, f, "flock", "before", "9")
	wrapper := f.BackupRoot + "/bin/flock"
	script, e := os.ReadFile(wrapper)
	if e != nil {
		t.Fatal(e)
	}
	release := f.BackupRoot + "/release"
	script = []byte(strings.ReplaceAll(string(script), "while :; do", "while [ ! -e "+release+" ]; do"))
	if e = os.WriteFile(wrapper, script, 0700); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- runRestoreStageFenced(ctx, r, f, "touch "+f.BackupRoot+"/unsafe-write") }()
	stageFenceWait(t, ready)
	if e = os.Chmod(f.BackupRoot, 0777); e != nil {
		t.Fatal(e)
	}
	stageFenceWrite(t, release, "")
	if e = <-finished; e == nil {
		t.Fatal("ancestor became writable before flock, but writer executed")
	}
	if _, e = os.Stat(f.BackupRoot + "/unsafe-write"); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("unsafe write occurred")
	}
}

func TestRestoreStageFenceDeadlineAndBusyErrorKinds(t *testing.T) {
	f := stageFenceInit(t)
	file, e := os.Open(f.Root + "/lock")
	if e != nil {
		t.Fatal(e)
	}
	if e = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		t.Fatal(e)
	}
	e = verifyRestoreStageFence(context.Background(), localBackupRunner{}, f)
	if !errors.Is(e, errRestoreStageFenceBusy) || !errors.Is(e, ErrUnitSnapshot) {
		t.Fatalf("busy kind lost: %v", e)
	}
	file.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	e = runRestoreStageFenced(ctx, localBackupRunner{}, f, "sleep 5")
	if !errors.Is(e, context.DeadlineExceeded) {
		t.Fatalf("deadline kind lost: %v", e)
	}
}

const stageFenceChildHelperMode = "PROMLIFECYCLE_STAGE_FENCE_CHILD_HELPER"
const stageFenceChildHelperRecord = "PROMLIFECYCLE_STAGE_FENCE_CHILD_RECORD"

type stageFenceChildRecord struct {
	PID                            int
	Root, StartTime                string
	ChildWaitStatus                int
	ChildReaped, DescendantsReaped bool
}

func stageFenceProcessStartTime(pid int) (string, error) {
	data, e := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if e != nil {
		return "", e
	}
	closing := strings.LastIndexByte(string(data), ')')
	if closing < 0 {
		return "", errors.New("invalid owned process identity")
	}
	fields := strings.Fields(string(data)[closing+1:])
	if len(fields) < 20 {
		return "", errors.New("incomplete owned process identity")
	}
	return fields[19], nil
}
func stageFenceReadChildRecord(path string) (stageFenceChildRecord, error) {
	var record stageFenceChildRecord
	data, e := os.ReadFile(path)
	if e != nil {
		return record, e
	}
	if e = json.Unmarshal(data, &record); e != nil {
		return record, e
	}
	if record.PID <= 1 || record.StartTime == "" || filepath.Dir(record.Root) != "/root" || !strings.HasPrefix(filepath.Base(record.Root), "stage-fence-test-") {
		return record, errors.New("invalid owned child record")
	}
	return record, nil
}
func stageFenceRunChildFixtureProcess(t *testing.T, mode string) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("root-owned host fence requires root")
	}
	observer, e := os.MkdirTemp("/root", "stage-fence-observer-")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.RemoveAll(observer) })
	recordPath := observer + "/child.json"
	// Independently contain the intentionally failing regression even before
	// its fixture cleanup is corrected. A start-time match prevents PID reuse.
	t.Cleanup(func() {
		record, e := stageFenceReadChildRecord(recordPath)
		if e != nil {
			return
		}
		if current, e := stageFenceProcessStartTime(record.PID); e == nil && current == record.StartTime {
			_ = syscall.Kill(record.PID, syscall.SIGKILL)
		}
		_ = os.RemoveAll(record.Root)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRestoreStageFenceChildRetainsLockAfterParentExit$", "-test.count=1")
	cmd.Env = append(os.Environ(), stageFenceChildHelperMode+"="+mode, stageFenceChildHelperRecord+"="+recordPath)
	output, e := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("child fixture timed out: %v", ctx.Err())
	}
	if mode == "normal" && e != nil {
		t.Fatalf("child fixture failed: %v\n%s", e, output)
	}
	if mode != "normal" && (e == nil || !strings.Contains(string(output), "forced early fixture failure")) {
		t.Fatalf("missing intentional failure: %v\n%s", e, output)
	}
	record, e := stageFenceReadChildRecord(recordPath)
	if e != nil {
		t.Fatal(e)
	}
	_, pidErr := os.Lstat("/proc/" + strconv.Itoa(record.PID))
	_, rootErr := os.Lstat(record.Root)
	pidAbsent, rootAbsent := errors.Is(pidErr, os.ErrNotExist), errors.Is(rootErr, os.ErrNotExist)
	t.Logf("mode=%s helper_exit=%d owned_pid=%d child_wait_status=%d child_reaped=%t descendants_reaped=%t pid_absent=%t fixture_absent=%t fixture=%s", mode, cmd.ProcessState.ExitCode(), record.PID, record.ChildWaitStatus, record.ChildReaped, record.DescendantsReaped, pidAbsent, rootAbsent, record.Root)
	if !record.ChildReaped || !record.DescendantsReaped {
		t.Error("owned children were not fully reaped")
	}
	if !pidAbsent {
		t.Errorf("owned detached child remains after fixture cleanup: %v", pidErr)
	}
	if !rootAbsent {
		t.Errorf("fixture path remains after cleanup: %v", rootErr)
	}
	status := syscall.WaitStatus(record.ChildWaitStatus)
	if mode == "fail-ignore-release" {
		if !status.Signaled() || status.Signal() != syscall.SIGKILL {
			t.Errorf("fallback did not terminate child: %v", status)
		}
	} else if !status.Exited() || status.ExitStatus() != 0 {
		t.Errorf("released child did not exit cleanly: %v", status)
	}
}
func TestRestoreStageFenceChildCleanupAfterEarlyFailure(t *testing.T) {
	for _, mode := range []string{"fail-before-ready", "fail-after-ready", "fail-ignore-release"} {
		t.Run(mode, func(t *testing.T) { stageFenceRunChildFixtureProcess(t, mode) })
	}
}

func stageFenceRegisterChildCleanup(t *testing.T, release, pidPath string) {
	t.Helper()
	// Registered after the root cleanup and before launch: Go cleanup LIFO
	// guarantees release, termination and wait all finish before root removal.
	t.Cleanup(func() {
		childReaped, descendantsReaped := false, false
		var childStatus syscall.WaitStatus
		defer func() {
			record, e := stageFenceReadChildRecord(os.Getenv(stageFenceChildHelperRecord))
			if e != nil {
				t.Errorf("read cleanup evidence: %v", e)
				return
			}
			record.ChildReaped, record.DescendantsReaped, record.ChildWaitStatus = childReaped, descendantsReaped, int(childStatus)
			data, e := json.Marshal(record)
			if e != nil {
				t.Errorf("encode cleanup evidence: %v", e)
				return
			}
			if e = os.WriteFile(os.Getenv(stageFenceChildHelperRecord), data, 0600); e != nil {
				t.Errorf("write cleanup evidence: %v", e)
			}
		}()
		if e := os.WriteFile(release, nil, 0600); e != nil {
			t.Errorf("release owned child: %v", e)
		}
		data, e := os.ReadFile(pidPath)
		if errors.Is(e, os.ErrNotExist) {
			var status syscall.WaitStatus
			if _, e = syscall.Wait4(-1, &status, syscall.WNOHANG, nil); errors.Is(e, syscall.ECHILD) {
				return
			}
			t.Errorf("owned child PID was not recorded")
			return
		}
		if e != nil {
			t.Errorf("read owned child PID: %v", e)
			return
		}
		pid, e := strconv.Atoi(strings.TrimSpace(string(data)))
		if e != nil || pid <= 1 {
			t.Errorf("invalid owned child PID")
			return
		}
		wait := func(limit time.Duration) bool {
			deadline := time.Now().Add(limit)
			for {
				var status syscall.WaitStatus
				got, e := syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
				if got == pid {
					childReaped, childStatus = true, status
					return true
				}
				if errors.Is(e, syscall.EINTR) {
					continue
				}
				if e != nil {
					t.Errorf("wait for owned child: %v", e)
					return false
				}
				if time.Now().After(deadline) {
					return false
				}
				time.Sleep(5 * time.Millisecond)
			}
		}
		if !wait(time.Second) {
			// Wait4 returning zero proves this is still our unreaped child. Recheck
			// that ownership before signalling; its PID cannot be recycled until reaped.
			var status syscall.WaitStatus
			got, e := syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
			if got == 0 && e == nil {
				if e = syscall.Kill(pid, syscall.SIGKILL); e != nil && !errors.Is(e, syscall.ESRCH) {
					t.Errorf("terminate owned child: %v", e)
				}
				if !wait(time.Second) {
					t.Errorf("owned child did not terminate")
				}
			} else if got == pid {
				childReaped, childStatus = true, status
			} else if e != nil {
				t.Errorf("verify owned child before termination: %v", e)
			}
		}
		// Killing a shell can adopt its final short sleep. This isolated subreaper
		// has no unrelated children, and must reap those descendants as well.
		deadline := time.Now().Add(time.Second)
		for {
			var status syscall.WaitStatus
			_, e := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
			if errors.Is(e, syscall.ECHILD) {
				descendantsReaped = true
				return
			}
			if e != nil && !errors.Is(e, syscall.EINTR) {
				t.Errorf("reap owned descendants: %v", e)
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("owned descendants did not terminate")
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	})
}
