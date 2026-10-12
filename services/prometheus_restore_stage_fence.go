package services

import (
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/promlifecycle/platform/pkg/utils"
)

// restoreStageFence is private host evidence, persisted before protected work.
// Empty identities are permitted only for a committed pre-pin intent. Root is
// one direct child of BackupRoot; the caller reserves that path durably first.
type restoreStageFence struct {
	BackupRoot, Root, Generation string
	RootIdentity, LockIdentity   string
}

var errRestoreStageFenceBusy = errors.New("restore stage host fence busy")
var restoreStageFenceIdentity = regexp.MustCompile(`^[0-9]+:[1-9][0-9]*$`)

// initializeRestoreStageFence may complete only pre-pin control-file bootstrap;
// it never adopts payloads. Pinned requests verify without missing-state repair.
func initializeRestoreStageFence(ctx context.Context, r hostCommandRunner, f restoreStageFence) (restoreStageFence, error) {
	return executeRestoreStageFence(ctx, r, f, "initialize", f.Generation, "")
}

// verifyRestoreStageFence acquires the existing lock read-only. It does not
// create, sync, repair or remove anything.
func verifyRestoreStageFence(ctx context.Context, r hostCommandRunner, f restoreStageFence) error {
	_, err := executeRestoreStageFence(ctx, r, f, "verify", f.Generation, "")
	return err
}

// rotateRestoreStageFence implements expected -> desired, or confirms already
// desired durably. Every other generation rejects, including delayed rotations.
// Only an unpinned committed initialization/reclaim intent may repair absence.
func rotateRestoreStageFence(ctx context.Context, r hostCommandRunner, f restoreStageFence, desired string) (restoreStageFence, error) {
	return executeRestoreStageFence(ctx, r, f, "rotate", desired, "")
}

// runRestoreStageFenced is solely for package-authored bounded foreground
// copy/sync work. Never pass caller command text. FD 9 is inherited by every
// child, retaining the fence even if the parent dies before its child finishes.
// Commands must not close/unlock FD 9, background work or mutate control files.
// The host timeout also bounds work when a controller transport disappears.
func runRestoreStageFenced(ctx context.Context, r hostCommandRunner, f restoreStageFence, command string) error {
	_, err := executeRestoreStageFence(ctx, r, f, "run", f.Generation, command)
	return err
}

func executeRestoreStageFence(ctx context.Context, r hostCommandRunner, f restoreStageFence, operation, desired, command string) (restoreStageFence, error) {
	if r == nil || !validUnitSnapshotPath(f.BackupRoot) || !validUnitSnapshotPath(f.Root) || filepath.Dir(f.Root) != f.BackupRoot || !unitSnapshotHash.MatchString(f.Generation) || !unitSnapshotHash.MatchString(desired) {
		return restoreStageFence{}, snapshotReject("invalid restore stage fence request")
	}
	pinned := f.RootIdentity != "" || f.LockIdentity != ""
	if pinned && (!restoreStageFenceIdentity.MatchString(f.RootIdentity) || !restoreStageFenceIdentity.MatchString(f.LockIdentity)) || !pinned && (operation == "verify" || operation == "run") {
		return restoreStageFence{}, snapshotReject("missing or invalid restore stage fence identity")
	}
	if operation != "initialize" && operation != "verify" && operation != "rotate" && operation != "run" || operation == "run" && (command == "" || len(command) > 256<<10 || strings.ContainsRune(command, 0)) {
		return restoreStageFence{}, snapshotReject("invalid restore stage fence operation")
	}
	bootstrap := "0"
	if !pinned {
		bootstrap = "1"
	}
	script := "# promlifecycle:restore-stage-fence\nset -euo pipefail\numask 077\n" +
		"root=" + utils.QuoteShell(f.Root) + "\nbackup=" + utils.QuoteShell(f.BackupRoot) +
		"\nexpected=" + utils.QuoteShell(f.Generation) + "\ndesired=" + utils.QuoteShell(desired) +
		"\nroot_pin=" + utils.QuoteShell(f.RootIdentity) + "\nlock_pin=" + utils.QuoteShell(f.LockIdentity) +
		"\noperation=" + utils.QuoteShell(operation) + "\nbootstrap=" + bootstrap + "\n" + restoreStageFenceHostScript
	if operation == "run" {
		script += "\ntimeout --foreground --signal=TERM --kill-after=5s 600s bash -o pipefail -c " + utils.QuoteShell(command) + " >/dev/null 2>&1\n" +
			"check_identity\nread_generation\ntest \"$generation\" = \"$expected\"\nsync -f -- \"$root\"\n"
	}
	script += "\nprintf 'stage-fence-v1\\n%s\\n%s\\n%s\\n' \"$root_identity\" \"$lock_identity\" \"$generation\"\n"
	out, err := snapshotRun(ctx, r, "bash -o pipefail -c "+utils.QuoteShell(script))
	if err != nil {
		return restoreStageFence{}, err
	}
	if out == "stage-fence-busy\n" {
		return restoreStageFence{}, errors.Join(ErrUnitSnapshot, errRestoreStageFenceBusy)
	}
	lines := strings.Split(out, "\n")
	if len(lines) != 5 || lines[0] != "stage-fence-v1" || lines[4] != "" || !restoreStageFenceIdentity.MatchString(lines[1]) || !restoreStageFenceIdentity.MatchString(lines[2]) || lines[3] != desired || pinned && (lines[1] != f.RootIdentity || lines[2] != f.LockIdentity) {
		return restoreStageFence{}, snapshotReject("invalid restore stage fence frame")
	}
	f.RootIdentity, f.LockIdentity, f.Generation = lines[1], lines[2], lines[3]
	return f, nil
}

// Control grammar: lock (empty), generation (64 lowercase hex plus newline),
// .generation.[A-Za-z0-9]{10} (retained publication temporary, at most 65 bytes),
// and, only with pinned identities, g-[0-9a-f]{64} protected directories.
// Temporaries are never trusted generation state and are never removed.
// Every ancestor is canonical, root-owned and not group/world writable.
const restoreStageFenceHostScript = `
test "$(id -u)" = 0
protected_ancestor() {
 test -d "$1" && test ! -L "$1"
 test "$(stat -c %u -- "$1")" = 0
 test -z "$(find "$1" -maxdepth 0 \( -perm /022 -o -perm /7000 \) -print)"
}
check_ancestors() {
 test "$(readlink -e -- "$backup")" = "$backup"
 ancestor="$backup"
 while :; do
  protected_ancestor "$ancestor"
  [ "$ancestor" != / ] || break
  ancestor=$(dirname -- "$ancestor")
 done
}
check_ancestors
test "$(readlink -m -- "$root")" = "$root"
if [ ! -e "$root" ] && [ ! -L "$root" ]; then
 test "$bootstrap" = 1
 mkdir -m 0700 -- "$root" 2>/dev/null || test -d "$root"
fi
private_directory() {
 test -d "$1" && test ! -L "$1"
 test "$(stat -c '%u:%g:%a' -- "$1")" = 0:0:700
}
private_file() {
 test -f "$1" && test ! -L "$1"
 test "$(stat -c '%u:%g:%a:%h' -- "$1")" = 0:0:600:1
}
private_directory "$root"
exec 8<"$root"
root_identity=$(stat -Lc '%d:%i' -- "/proc/$$/fd/8")
test -z "$root_pin" || test "$root_identity" = "$root_pin"
# No lock is ever created when generation or payload evidence already exists.
# Two empty-root initializers use noclobber, then converge on the winning inode.
if [ ! -e "$root/lock" ] && [ ! -L "$root/lock" ]; then
 test "$bootstrap" = 1
 test -z "$(find "$root" -mindepth 1 -maxdepth 1 -print -quit)"
 (set -C; : >"$root/lock") 2>/dev/null || test -f "$root/lock"
fi
private_file "$root/lock"
test "$(stat -c %s -- "$root/lock")" = 0
exec 9<"$root/lock"
if ! flock -n 9; then printf 'stage-fence-busy\n'; exit 0; fi
check_identity() {
 check_ancestors
 private_directory "$root"
 private_file "$root/lock"
 test "$(stat -c %s -- "$root/lock")" = 0
 test "$(readlink -e -- "$root")" = "$root"
 test "$(stat -c '%d:%i' -- "$root")" = "$root_identity"
 test "$(stat -Lc '%d:%i' -- "/proc/$$/fd/8")" = "$root_identity"
 lock_identity=$(stat -Lc '%d:%i' -- "/proc/$$/fd/9")
 test "$(stat -c '%d:%i' -- "$root/lock")" = "$lock_identity"
 test -z "$lock_pin" || test "$lock_identity" = "$lock_pin"
 test -z "$root_pin" || test "$root_identity" = "$root_pin"
}
check_identity
shopt -s nullglob dotglob
for entry in "$root"/*; do
 name="${entry##*/}"
 case "$name" in
 lock|generation) private_file "$entry";;
 .generation.*)
  [[ "$name" =~ ^\.generation\.[A-Za-z0-9]{10}$ ]]
  private_file "$entry"
  test "$(stat -c %s -- "$entry")" -le 65;;
 g-*)
  test "$bootstrap" = 0
  [[ "$name" =~ ^g-[0-9a-f]{64}$ ]]
  private_directory "$entry";;
 *) exit 1;;
 esac
done
generation=''
read_generation() {
 private_file "$root/generation"
 test "$(stat -c %s -- "$root/generation")" = 65
 exec 10<"$root/generation"
 generation_identity=$(stat -Lc '%d:%i' -- "/proc/$$/fd/10")
 test "$(stat -c '%d:%i' -- "$root/generation")" = "$generation_identity"
 IFS= read -r generation <&10
 [[ "$generation" =~ ^[0-9a-f]{64}$ ]]
 test "$(stat -c '%d:%i' -- "$root/generation")" = "$generation_identity"
 exec 10<&-
}
if [ -e "$root/generation" ] || [ -L "$root/generation" ]; then
 read_generation
 if [ "$operation" = rotate ]; then
  test "$generation" = "$expected" || test "$generation" = "$desired"
 else
  test "$generation" = "$expected"
 fi
else
 test "$bootstrap" = 1
fi
if [ "$operation" = initialize ] || [ "$operation" = rotate ]; then
 # Sync even an idempotent retry: a prior controller could have died between
 # rename and directory sync or between durability and DB publication.
 sync -f -- "$root/lock"
 sync -f -- "$root"
 sync -f -- "$backup"
 if [ "$generation" != "$desired" ]; then
  temporary=$(mktemp -- "$root/.generation.XXXXXXXXXX")
  printf '%s\n' "$desired" >"$temporary"
  sync -f -- "$temporary"
  check_identity
  mv -T -- "$temporary" "$root/generation"
 fi
 sync -f -- "$root/generation"
 sync -f -- "$root"
 sync -f -- "$backup"
 read_generation
 test "$generation" = "$desired"
fi
check_identity
`
